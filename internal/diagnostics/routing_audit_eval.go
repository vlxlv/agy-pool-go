package diagnostics

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/proxy"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/scheduler"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// AccountTier represents the headroom tier of an account candidate.
type AccountTier string

const (
	TierHealthy  AccountTier = "Healthy"
	TierReserve  AccountTier = "Reserve"
	TierDepleted AccountTier = "Depleted"
	TierUnknown  AccountTier = "Unknown"
)

// AuditVerdict represents the outcome classification of a routing attempt.
type AuditVerdict string

const (
	VerdictPass      AuditVerdict = "PASS"
	VerdictWarning   AuditVerdict = "WARNING"
	VerdictViolation AuditVerdict = "VIOLATION"
)

// EvaluateRoutingAttempt evaluates candidate selection against current pool state.
func EvaluateRoutingAttempt(
	pool *storage.Pool,
	accountName string,
	status int,
	attempt int,
	failoverReason string,
	timestamp time.Time,
	requestID string,
) RoutingAuditEvent {
	now := float64(timestamp.Unix())
	strategy := config.StrategyMaxQuota
	if pool != nil && pool.Strategy != "" {
		strategy = pool.Strategy
	}

	event := RoutingAuditEvent{
		Timestamp:      timestamp,
		RequestID:      requestID,
		Strategy:       strategy,
		Account:        accountName,
		Attempt:        attempt,
		Status:         status,
		FailoverReason: failoverReason,
	}
	if event.Attempt <= 0 {
		event.Attempt = 1
	}

	if pool == nil || len(pool.Accounts) == 0 {
		event.Classification = "unknown"
		event.Verdict = "warning"
		event.VerdictDetails = "Empty or uninitialized pool state"
		return event
	}

	// 1. Identify selected account
	var selectedAcc *storage.Account
	for _, acc := range pool.Accounts {
		disp := accounts.DisplayAccountName(acc)
		if strings.EqualFold(disp, accountName) ||
			strings.EqualFold(acc.ID, accountName) ||
			strings.EqualFold(acc.Name, accountName) {
			selectedAcc = acc
			break
		}
	}

	if selectedAcc == nil {
		event.Classification = "unknown"
		event.Verdict = "warning"
		event.VerdictDetails = fmt.Sprintf("Selected account %q not found in state.db", accountName)
		return event
	}

	// Always use safe friendly name (never email or secrets)
	event.Account = accounts.DisplayAccountName(selectedAcc)
	event.AccountID = selectedAcc.ID

	// 2. Classify selected account using authoritative scheduler and quota primitives
	capState := quota.ComputeCapacityState(selectedAcc, now)
	event.FiveHourKnown = capState.Q5Known
	event.FiveHourRemaining = capState.Q5
	event.FiveHourResetAt = capState.Reset5Time
	event.FiveHourPace = capState.Pace5
	if capState.Q5 != nil {
		r5 := capState.R5
		event.FiveHourResetRatio = &r5
	}

	event.WeeklyKnown = capState.Q7Known
	event.WeeklyRemaining = capState.Q7
	event.WeeklyResetAt = capState.Reset7Time
	event.WeeklyPace = capState.Pace7
	if capState.Q7 != nil {
		r7 := capState.R7
		event.WeeklyResetRatio = &r7
	}

	event.KnownWindowCount = capState.KnownWindowCount
	if !math.IsInf(capState.WorstPace, 0) {
		wp := capState.WorstPace
		event.WorstPace = &wp
	}
	if !math.IsInf(capState.TotalPace, 0) {
		tp := capState.TotalPace
		event.TotalPace = &tp
	}
	rf := capState.RawFloor
	event.RawFloor = &rf
	freshness := quota.FreshnessRank(selectedAcc, now)
	event.FreshnessRank = &freshness

	var classification AccountTier
	if capState.IsDepleted {
		classification = TierDepleted
	} else if scheduler.IsReserveCandidate(selectedAcc, now) {
		classification = TierReserve
	} else if capState.KnownWindowCount > 0 || capState.LegacyFractionKnown {
		classification = TierHealthy
	} else {
		classification = TierUnknown
	}
	event.Classification = strings.ToLower(string(classification))

	// 3. Find Healthy candidates available in pool at this reference time
	var healthyCandidates []string
	var reserveCandidates []string
	for _, cand := range pool.Accounts {
		if cand == nil {
			continue
		}
		if cand.Status == storage.StatusValidationRequired || cand.Status == storage.StatusVerificationPending || cand.Status == storage.StatusAuthError || cand.Status == storage.StatusDisabled {
			continue
		}
		if cand.RateLimitedUntil != nil && *cand.RateLimitedUntil > now {
			continue
		}
		candCap := quota.ComputeCapacityState(cand, now)
		if candCap.IsDepleted {
			continue
		}
		candDisp := accounts.DisplayAccountName(cand)
		if scheduler.IsReserveCapacity(candCap) {
			reserveCandidates = append(reserveCandidates, candDisp)
		} else if candCap.KnownWindowCount > 0 || candCap.LegacyFractionKnown {
			healthyCandidates = append(healthyCandidates, candDisp)
		}
	}
	event.HealthyCount = len(healthyCandidates)
	event.ReserveCount = len(reserveCandidates)
	event.HealthyCandidates = healthyCandidates
	event.ReserveCandidates = reserveCandidates

	// 4. Violation semantics (Section 2 & Section 11)
	if strategy == config.StrategyMaxQuota || strategy == config.StrategySmart {
		switch classification {
		case TierHealthy:
			event.Verdict = "pass"
		case TierReserve:
			hasOtherHealthy := false
			for _, hc := range healthyCandidates {
				if !strings.EqualFold(hc, event.Account) {
					hasOtherHealthy = true
					break
				}
			}
			if hasOtherHealthy {
				event.Verdict = "violation"
				event.VerdictDetails = fmt.Sprintf("Reserve account %s selected while Healthy candidates were available: %s",
					event.Account, strings.Join(healthyCandidates, ", "))
			} else {
				event.Verdict = "pass"
				event.VerdictDetails = "Reserve account selected as fallback (no Healthy candidates available)"
			}
		case TierDepleted:
			if len(healthyCandidates) > 0 || len(reserveCandidates) > 0 {
				event.Verdict = "violation"
				event.VerdictDetails = fmt.Sprintf("Depleted account %s selected while Healthy or Reserve candidates were available",
					event.Account)
			} else {
				event.Verdict = "pass"
				event.VerdictDetails = "Depleted account selected (all accounts depleted)"
			}
		default: // TierUnknown
			event.Verdict = "warning"
			event.VerdictDetails = "Selected account quota classification cannot be determined from authoritative state"
		}
	} else {
		// least_used or round_robin (Section 11)
		event.Verdict = "pass"
		event.VerdictDetails = fmt.Sprintf("Reserve headroom checks not applicable to strategy %s", strategy)
	}

	return event
}

// EvaluateFrozenRoutingEvent evaluates PASS / WARNING / VIOLATION purely from the frozen RoutingEvent.
// It DOES NOT read state.db or re-evaluate mutable quota state after dequeue.
func EvaluateFrozenRoutingEvent(ev proxy.RoutingEvent) RoutingAuditEvent {
	strategy := ev.Strategy
	if strategy == "" {
		strategy = config.StrategyMaxQuota
	}

	accName := ev.Account
	if accName == "" {
		accName = ev.AccountName
	}

	healthyCount := ev.HealthyCandidateCount
	if healthyCount == 0 && len(ev.HealthyCandidates) > 0 {
		healthyCount = len(ev.HealthyCandidates)
	}

	reserveCount := ev.ReserveCandidateCount
	if reserveCount == 0 && len(ev.ReserveCandidates) > 0 {
		reserveCount = len(ev.ReserveCandidates)
	}

	auditEv := RoutingAuditEvent{
		Timestamp:          ev.Timestamp,
		Strategy:           strategy,
		Account:            accName,
		AccountID:          ev.AccountID,
		Classification:     strings.ToLower(ev.Classification),
		FiveHourKnown:      ev.FiveHourKnown,
		FiveHourRemaining:  ev.FiveHourRemaining,
		FiveHourResetAt:    ev.FiveHourResetAt,
		FiveHourResetRatio: ev.FiveHourResetRatio,
		FiveHourPace:       ev.FiveHourPace,
		WeeklyKnown:        ev.WeeklyKnown,
		WeeklyRemaining:    ev.WeeklyRemaining,
		WeeklyResetAt:      ev.WeeklyResetAt,
		WeeklyResetRatio:   ev.WeeklyResetRatio,
		WeeklyPace:         ev.WeeklyPace,
		KnownWindowCount:   ev.KnownWindowCount,
		FreshnessRank:      ev.FreshnessRank,
		WorstPace:          ev.WorstPace,
		TotalPace:          ev.TotalPace,
		RawFloor:           ev.RawFloor,
		HealthyCount:       healthyCount,
		ReserveCount:       reserveCount,
		HealthyCandidates:  ev.HealthyCandidates,
		ReserveCandidates:  ev.ReserveCandidates,
		Attempt:            ev.Attempt,
		Status:             ev.Status,
		FailoverReason:     ev.FailoverReason,
	}
	if auditEv.Attempt <= 0 {
		auditEv.Attempt = 1
	}

	tier := strings.Title(strings.ToLower(ev.Classification))
	if strategy == config.StrategyMaxQuota || strategy == config.StrategySmart {
		switch tier {
		case "Healthy":
			auditEv.Verdict = "pass"
			auditEv.VerdictDetails = "Healthy account selected"
		case "Reserve":
			hasOtherHealthy := false
			for _, hc := range ev.HealthyCandidates {
				if !strings.EqualFold(hc, accName) {
					hasOtherHealthy = true
					break
				}
			}
			if !hasOtherHealthy && healthyCount > 0 {
				hasOtherHealthy = true
			}
			if hasOtherHealthy {
				auditEv.Verdict = "violation"
				healthyList := strings.Join(ev.HealthyCandidates, ", ")
				if healthyList == "" {
					healthyList = fmt.Sprintf("%d healthy candidates", healthyCount)
				}
				auditEv.VerdictDetails = fmt.Sprintf("Reserve account %s selected while Healthy candidates were available: %s",
					accName, healthyList)
			} else {
				auditEv.Verdict = "pass"
				auditEv.VerdictDetails = "Reserve account selected as fallback (no Healthy candidates available)"
			}
		case "Depleted":
			if healthyCount > 0 || len(ev.HealthyCandidates) > 0 || reserveCount > 0 || len(ev.ReserveCandidates) > 0 {
				auditEv.Verdict = "violation"
				auditEv.VerdictDetails = fmt.Sprintf("Depleted account %s selected while Healthy or Reserve candidates were available", accName)
			} else {
				auditEv.Verdict = "pass"
				auditEv.VerdictDetails = "Depleted account selected (all accounts depleted)"
			}
		default: // "Unknown"
			auditEv.Verdict = "warning"
			auditEv.VerdictDetails = "Selected account quota classification cannot be determined from selection-time metadata"
		}
	} else {
		auditEv.Verdict = "pass"
		auditEv.VerdictDetails = fmt.Sprintf("Reserve headroom checks not applicable to strategy %s", strategy)
	}

	return auditEv
}
