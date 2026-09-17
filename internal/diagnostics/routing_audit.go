package diagnostics

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// RoutingAuditEvent contains non-sensitive metadata for a single generation routing attempt.
type RoutingAuditEvent struct {
	Timestamp      time.Time `json:"timestamp"`
	RequestID      string    `json:"request_id,omitempty"`
	Strategy       string    `json:"strategy"`
	Account        string    `json:"account"`
	AccountID      string    `json:"account_id,omitempty"`
	Classification string    `json:"classification"`

	FiveHourKnown      bool       `json:"five_hour_known,omitempty"`
	FiveHourRemaining  *float64   `json:"five_hour_remaining"`
	FiveHourResetAt    *time.Time `json:"five_hour_reset_at,omitempty"`
	FiveHourResetRatio *float64   `json:"five_hour_reset_ratio,omitempty"`
	FiveHourPace       *float64   `json:"five_hour_pace,omitempty"`

	WeeklyKnown      bool       `json:"weekly_known,omitempty"`
	WeeklyRemaining  *float64   `json:"weekly_remaining"`
	WeeklyResetAt    *time.Time `json:"weekly_reset_at,omitempty"`
	WeeklyResetRatio *float64   `json:"weekly_reset_ratio,omitempty"`
	WeeklyPace       *float64   `json:"weekly_pace,omitempty"`

	KnownWindowCount int      `json:"known_window_count"`
	FreshnessRank    *int     `json:"freshness_rank,omitempty"`
	WorstPace        *float64 `json:"worst_pace,omitempty"`
	TotalPace        *float64 `json:"total_pace,omitempty"`
	RawFloor         *float64 `json:"raw_floor,omitempty"`

	HealthyCount      int      `json:"healthy_count"`
	ReserveCount      int      `json:"reserve_count"`
	HealthyCandidates []string `json:"healthy_candidates,omitempty"`
	ReserveCandidates []string `json:"reserve_candidates,omitempty"`

	Attempt        int    `json:"attempt"`
	Status         int    `json:"status"`
	FailoverReason string `json:"failover_reason,omitempty"`
	Verdict        string `json:"verdict"`
	VerdictDetails string `json:"verdict_details,omitempty"`
}

// AuditOptions configures the routing audit execution.
type AuditOptions struct {
	Duration       time.Duration
	JSONMode       bool
	ReportPath     string
	LogPath        string
	FromStart      bool
	Reader         io.Reader                 // Optional direct reader for testing
	EventChan      <-chan RoutingAuditEvent  // Optional direct event stream for testing
	ProxyEventChan <-chan proxy.RoutingEvent // Direct stream of frozen proxy routing events
}

// AuditSummary contains aggregated metrics for an audit session.
type AuditSummary struct {
	GenerationAttempts int64            `json:"generation_attempts"`
	AccountCounts      map[string]int64 `json:"account_counts"`
	Violations         int64            `json:"violations"`
	Warnings           int64            `json:"warnings"`
	DroppedEvents      int64            `json:"dropped_events"`
	FinalVerdict       string           `json:"final_verdict"`
	Strategy           string           `json:"strategy"`
	Duration           time.Duration    `json:"duration"`
}

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
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	now := float64(timestamp.Unix())

	strategy := config.StrategyMaxQuota
	if pool != nil && pool.Strategy != "" {
		strategy = pool.Strategy
	}

	event := RoutingAuditEvent{
		Timestamp:         timestamp,
		RequestID:         requestID,
		Strategy:          strategy,
		Account:           accountName,
		Attempt:           attempt,
		Status:            status,
		FailoverReason:    failoverReason,
		HealthyCandidates: []string{},
	}
	if event.Attempt <= 0 {
		event.Attempt = 1
	}

	if pool == nil || len(pool.Accounts) == 0 {
		event.Classification = "unknown"
		event.Verdict = "warning"
		event.VerdictDetails = "No pool accounts available in authoritative state"
		return event
	}

	// 1. Locate selected account in pool by friendly name, ID, or display name
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
		if cand.Status == "validation_required" || cand.Status == "auth_error" {
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
	if strategy == config.StrategyMaxQuota {
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
	if strategy == config.StrategyMaxQuota {
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

// ParsedLogEntry contains extracted fields from a daemon log line.
type ParsedLogEntry struct {
	Timestamp      time.Time
	Type           string // "PROXY", "PROXY ERROR", "FAILOVER"
	Path           string
	AccountName    string
	Status         int
	FailoverReason string
	IsGeneration   bool
	HasAuditMeta   bool
	Strategy       string
	Classification string
	HealthyCount   int
	ReserveCount   int
}

// ParseLogLine parses a log line produced by the agy-pool proxy.
func ParseLogLine(line string) (*ParsedLogEntry, bool) {
	line = strings.TrimSpace(line)
	if len(line) < 22 || line[0] != '[' || line[20] != ']' {
		return nil, false
	}

	ts, err := time.ParseInLocation("2006-01-02 15:04:05", line[1:20], time.Local)
	if err != nil {
		return nil, false
	}

	rest := strings.TrimSpace(line[21:])
	tagEnd := strings.Index(rest, "] ")
	if tagEnd == -1 || rest[0] != '[' {
		return nil, false
	}

	tag := rest[1:tagEnd]
	msg := strings.TrimSpace(rest[tagEnd+2:])

	switch tag {
	case "PROXY":
		// Format: METHOD PATH -> ACCOUNT (Status: STATUS) [audit: ...]
		arrowIdx := strings.Index(msg, " -> ")
		if arrowIdx == -1 {
			return nil, false
		}
		left := strings.TrimSpace(msg[:arrowIdx])
		right := strings.TrimSpace(msg[arrowIdx+4:])

		parts := strings.Fields(left)
		path := ""
		if len(parts) >= 2 {
			path = parts[1]
		} else if len(parts) == 1 {
			path = parts[0]
		}
		isGen := strings.Contains(strings.ToLower(path), "generatecontent")

		var hasAuditMeta bool
		var auditStrat, auditTier string
		var auditHealthy, auditReserve int
		auditIdx := strings.Index(right, " [audit: ")
		if auditIdx != -1 {
			metaStr := right[auditIdx+9:]
			right = right[:auditIdx]
			closeBracket := strings.Index(metaStr, "]")
			if closeBracket != -1 {
				metaStr = metaStr[:closeBracket]
			}
			hasAuditMeta = true
			for _, field := range strings.Fields(metaStr) {
				kv := strings.SplitN(field, "=", 2)
				if len(kv) == 2 {
					switch kv[0] {
					case "strategy":
						auditStrat = kv[1]
					case "tier":
						auditTier = kv[1]
					case "healthy":
						auditHealthy, _ = strconv.Atoi(kv[1])
					case "reserve":
						auditReserve, _ = strconv.Atoi(kv[1])
					}
				}
			}
		}

		// Parse right: ACCOUNT (Status: STATUS)
		statusIdx := strings.LastIndex(right, "(Status: ")
		if statusIdx == -1 {
			return nil, false
		}
		accName := strings.TrimSpace(right[:statusIdx])
		statusStr := strings.TrimSuffix(strings.TrimSpace(right[statusIdx+9:]), ")")
		status, _ := strconv.Atoi(statusStr)

		return &ParsedLogEntry{
			Timestamp:      ts,
			Type:           "PROXY",
			Path:           path,
			AccountName:    accName,
			Status:         status,
			IsGeneration:   isGen,
			HasAuditMeta:   hasAuditMeta,
			Strategy:       auditStrat,
			Classification: auditTier,
			HealthyCount:   auditHealthy,
			ReserveCount:   auditReserve,
		}, true

	case "PROXY ERROR":
		// Format: METHOD PATH -> ACCOUNT HTTP STATUS [audit: ...]
		arrowIdx := strings.Index(msg, " -> ")
		if arrowIdx == -1 {
			return nil, false
		}
		left := strings.TrimSpace(msg[:arrowIdx])
		right := strings.TrimSpace(msg[arrowIdx+4:])

		parts := strings.Fields(left)
		path := ""
		if len(parts) >= 2 {
			path = parts[1]
		} else if len(parts) == 1 {
			path = parts[0]
		}
		isGen := strings.Contains(strings.ToLower(path), "generatecontent")

		var hasAuditMeta bool
		var auditStrat, auditTier string
		var auditHealthy, auditReserve int
		auditIdx := strings.Index(right, " [audit: ")
		if auditIdx != -1 {
			metaStr := right[auditIdx+9:]
			right = right[:auditIdx]
			closeBracket := strings.Index(metaStr, "]")
			if closeBracket != -1 {
				metaStr = metaStr[:closeBracket]
			}
			hasAuditMeta = true
			for _, field := range strings.Fields(metaStr) {
				kv := strings.SplitN(field, "=", 2)
				if len(kv) == 2 {
					switch kv[0] {
					case "strategy":
						auditStrat = kv[1]
					case "tier":
						auditTier = kv[1]
					case "healthy":
						auditHealthy, _ = strconv.Atoi(kv[1])
					case "reserve":
						auditReserve, _ = strconv.Atoi(kv[1])
					}
				}
			}
		}

		httpIdx := strings.LastIndex(right, " HTTP ")
		if httpIdx == -1 {
			return nil, false
		}
		accName := strings.TrimSpace(right[:httpIdx])
		statusStr := strings.TrimSpace(right[httpIdx+6:])
		status, _ := strconv.Atoi(statusStr)

		return &ParsedLogEntry{
			Timestamp:      ts,
			Type:           "PROXY ERROR",
			Path:           path,
			AccountName:    accName,
			Status:         status,
			IsGeneration:   isGen,
			HasAuditMeta:   hasAuditMeta,
			Strategy:       auditStrat,
			Classification: auditTier,
			HealthyCount:   auditHealthy,
			ReserveCount:   auditReserve,
		}, true

	case "FAILOVER":
		// Format: Account ACCOUNT hit rate limit... or requires account verification...
		return &ParsedLogEntry{
			Timestamp:      ts,
			Type:           "FAILOVER",
			FailoverReason: msg,
		}, true

	default:
		return nil, false
	}
}

// Auditor coordinates routing observation, event evaluation, and report aggregation.
type Auditor struct {
	mu            sync.Mutex
	options       AuditOptions
	summary       AuditSummary
	events        []RoutingAuditEvent
	droppedEvents int64
	startTime     time.Time
}

// NewAuditor creates a new Auditor instance.
func NewAuditor(opts AuditOptions) *Auditor {
	return &Auditor{
		options: opts,
		summary: AuditSummary{
			AccountCounts: make(map[string]int64),
			FinalVerdict:  string(VerdictPass),
		},
		startTime: time.Now(),
	}
}

func formatResetCountdown(t *time.Time, now time.Time) string {
	if t == nil {
		return ""
	}
	d := t.Sub(now)
	if d <= 0 {
		return "reset now"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("reset in %dd%dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("reset in %dh%dm", hours, mins)
	}
	if mins > 0 {
		return fmt.Sprintf("reset in %dm", mins)
	}
	secs := int(d.Seconds()) % 60
	return fmt.Sprintf("reset in %ds", secs)
}

// RecordEvent records a verified routing event into summaries and logs.
func (a *Auditor) RecordEvent(event RoutingAuditEvent, out io.Writer) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.summary.GenerationAttempts++
	a.summary.AccountCounts[event.Account]++

	switch strings.ToUpper(event.Verdict) {
	case string(VerdictViolation):
		a.summary.Violations++
		a.summary.FinalVerdict = string(VerdictViolation)
	case string(VerdictWarning):
		a.summary.Warnings++
		if a.summary.FinalVerdict != string(VerdictViolation) {
			a.summary.FinalVerdict = string(VerdictWarning)
		}
	}

	if a.summary.Strategy == "" {
		a.summary.Strategy = event.Strategy
	}

	if a.options.ReportPath != "" {
		a.events = append(a.events, event)
	}

	// Format output to out
	if out != nil {
		if a.options.JSONMode {
			b, _ := json.Marshal(event)
			fmt.Fprintln(out, string(b))
		} else {
			attemptStr := ""
			if event.Attempt > 1 {
				attemptStr = fmt.Sprintf(" (Attempt %d)", event.Attempt)
			}
			tsStr := event.Timestamp.Format("15:04:05")

			fmt.Fprintf(out, "\n  [%s] %s%s\n", tsStr, event.Account, attemptStr)
			tierDisp := strings.Title(event.Classification)
			if tierDisp == "" {
				tierDisp = "Unknown"
			}
			fmt.Fprintf(out, "    Tier    : %s\n", tierDisp)

			if event.FiveHourRemaining != nil {
				resetInfo := ""
				if event.FiveHourResetAt != nil {
					resetInfo = fmt.Sprintf(" (%s)", formatResetCountdown(event.FiveHourResetAt, event.Timestamp))
				}
				fmt.Fprintf(out, "    5H      : %.1f%%%s\n", *event.FiveHourRemaining*100.0, resetInfo)
			} else {
				fmt.Fprintf(out, "    5H      : N/A\n")
			}

			if event.WeeklyRemaining != nil {
				resetInfo := ""
				if event.WeeklyResetAt != nil {
					resetInfo = fmt.Sprintf(" (%s)", formatResetCountdown(event.WeeklyResetAt, event.Timestamp))
				}
				fmt.Fprintf(out, "    Weekly  : %.1f%%%s\n", *event.WeeklyRemaining*100.0, resetInfo)
			} else {
				fmt.Fprintf(out, "    Weekly  : N/A\n")
			}

			if event.WorstPace != nil {
				fmt.Fprintf(out, "    WorstPace: %+.4f\n", *event.WorstPace)
			}

			fmt.Fprintf(out, "    Status  : %d\n", event.Status)

			verdictUpper := strings.ToUpper(event.Verdict)
			if verdictUpper == string(VerdictViolation) && event.VerdictDetails != "" {
				fmt.Fprintf(out, "    Verdict : %s (%s)\n", verdictUpper, event.VerdictDetails)
			} else if verdictUpper == string(VerdictWarning) && event.VerdictDetails != "" {
				fmt.Fprintf(out, "    Verdict : %s (%s)\n", verdictUpper, event.VerdictDetails)
			} else {
				fmt.Fprintf(out, "    Verdict : %s\n", verdictUpper)
			}

			if event.FailoverReason != "" {
				fmt.Fprintf(out, "    Failover: %s\n", event.FailoverReason)
			}
		}
	}
}

// PrintSummary prints the final summary to out.
func (a *Auditor) PrintSummary(out io.Writer) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.options.JSONMode {
		return
	}

	fmt.Fprintln(out, "\n  Summary")
	fmt.Fprintln(out, "  -------")
	fmt.Fprintf(out, "  Generation attempts : %d\n", a.summary.GenerationAttempts)

	// List known accounts from pool if available, plus any observed accounts
	accNames := make([]string, 0, len(a.summary.AccountCounts))
	for name := range a.summary.AccountCounts {
		accNames = append(accNames, name)
	}
	sort.Strings(accNames)

	for _, name := range accNames {
		fmt.Fprintf(out, "  %-20s: %d\n", name, a.summary.AccountCounts[name])
	}

	fmt.Fprintf(out, "  Violations          : %d\n", a.summary.Violations)
	fmt.Fprintf(out, "  Warnings            : %d\n", a.summary.Warnings)
	if a.droppedEvents > 0 {
		fmt.Fprintf(out, "  Dropped events      : %d\n", a.droppedEvents)
	}

	fmt.Fprintf(out, "\n  Final verdict: %s\n\n", a.summary.FinalVerdict)
}

// WriteReport writes audit results to report file if configured.
func (a *Auditor) WriteReport() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.options.ReportPath == "" {
		return nil
	}

	dir := filepath.Dir(a.options.ReportPath)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}

	f, err := os.Create(a.options.ReportPath)
	if err != nil {
		return err
	}
	defer f.Close()

	if a.options.JSONMode || strings.HasSuffix(a.options.ReportPath, ".json") {
		reportData := struct {
			Summary AuditSummary        `json:"summary"`
			Events  []RoutingAuditEvent `json:"events"`
		}{
			Summary: a.summary,
			Events:  a.events,
		}
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		return enc.Encode(reportData)
	}

	// Human readable report
	fmt.Fprintf(f, "agy-pool routing audit report\n")
	fmt.Fprintf(f, "Strategy : %s\n", a.summary.Strategy)
	fmt.Fprintf(f, "Duration : %s\n\n", a.summary.Duration)
	for _, ev := range a.events {
		b, _ := json.Marshal(ev)
		fmt.Fprintln(f, string(b))
	}
	fmt.Fprintln(f, "\nSummary")
	fmt.Fprintln(f, "-------")
	fmt.Fprintf(f, "Generation attempts : %d\n", a.summary.GenerationAttempts)
	for name, count := range a.summary.AccountCounts {
		fmt.Fprintf(f, "%-20s: %d\n", name, count)
	}
	fmt.Fprintf(f, "Violations          : %d\n", a.summary.Violations)
	fmt.Fprintf(f, "Warnings            : %d\n", a.summary.Warnings)
	fmt.Fprintf(f, "\nFinal verdict: %s\n", a.summary.FinalVerdict)
	return nil
}

// Run executes the audit until duration expires, ctx is cancelled, or input is exhausted.
func (a *Auditor) Run(ctx context.Context, stdout, stderr io.Writer) error {
	if a.options.Duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.options.Duration)
		defer cancel()
	}

	pool, _ := storage.LoadPool()
	strategy := config.StrategyMaxQuota
	if pool != nil && pool.Strategy != "" {
		strategy = pool.Strategy
	}
	a.summary.Strategy = strategy

	if !a.options.JSONMode {
		durStr := "manual (Ctrl-C)"
		if a.options.Duration > 0 {
			durStr = a.options.Duration.String()
		}
		stratNote := strategy
		if strategy != config.StrategyMaxQuota {
			stratNote = fmt.Sprintf("%s (reserve headroom checks N/A)", strategy)
		}

		fmt.Fprintln(stdout, "\n  agy-pool routing audit")
		fmt.Fprintf(stdout, "  Strategy : %s\n", stratNote)
		fmt.Fprintf(stdout, "  Duration : %s\n", durStr)
	}

	// 0. In-process proxy event channel mode (direct subscription with frozen RoutingEvent)
	if a.options.ProxyEventChan != nil {
		for {
			select {
			case <-ctx.Done():
				a.summary.Duration = time.Since(a.startTime)
				a.summary.DroppedEvents = proxy.DroppedAuditEvents()
				a.PrintSummary(stdout)
				_ = a.WriteReport()
				return nil
			case ev, ok := <-a.options.ProxyEventChan:
				if !ok {
					a.summary.Duration = time.Since(a.startTime)
					a.summary.DroppedEvents = proxy.DroppedAuditEvents()
					a.PrintSummary(stdout)
					_ = a.WriteReport()
					return nil
				}
				auditEv := EvaluateFrozenRoutingEvent(ev)
				a.RecordEvent(auditEv, stdout)
			}
		}
	}

	// 1. In-process event channel mode (testing or embedded)
	if a.options.EventChan != nil {
		for {
			select {
			case <-ctx.Done():
				a.summary.Duration = time.Since(a.startTime)
				a.PrintSummary(stdout)
				_ = a.WriteReport()
				return nil
			case ev, ok := <-a.options.EventChan:
				if !ok {
					a.summary.Duration = time.Since(a.startTime)
					a.PrintSummary(stdout)
					_ = a.WriteReport()
					return nil
				}
				a.RecordEvent(ev, stdout)
			}
		}
	}

	// 2. Direct reader mode (unit testing with log text)
	if a.options.Reader != nil {
		scanner := bufio.NewScanner(a.options.Reader)
		currentAttempt := 1
		lastFailoverReason := ""

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				a.summary.Duration = time.Since(a.startTime)
				a.PrintSummary(stdout)
				_ = a.WriteReport()
				return nil
			default:
			}

			line := scanner.Text()
			entry, ok := ParseLogLine(line)
			if !ok {
				continue
			}

			if entry.Type == "FAILOVER" {
				lastFailoverReason = entry.FailoverReason
				continue
			}

			if !entry.IsGeneration {
				continue
			}

			var ev RoutingAuditEvent
			if entry.HasAuditMeta {
				ev = EvaluateFrozenRoutingEvent(proxy.RoutingEvent{
					Timestamp:             entry.Timestamp,
					Strategy:              entry.Strategy,
					Account:               entry.AccountName,
					AccountName:           entry.AccountName,
					Classification:        entry.Classification,
					HealthyCandidateCount: entry.HealthyCount,
					ReserveCandidateCount: entry.ReserveCount,
					Attempt:               currentAttempt,
					Status:                entry.Status,
					FailoverReason:        lastFailoverReason,
					Path:                  entry.Path,
				})
			} else {
				currentPool, _ := storage.LoadPool()
				if currentPool == nil {
					currentPool = pool
				}
				ev = EvaluateRoutingAttempt(
					currentPool,
					entry.AccountName,
					entry.Status,
					currentAttempt,
					lastFailoverReason,
					entry.Timestamp,
					"",
				)
			}
			a.RecordEvent(ev, stdout)

			if entry.Status >= 400 && lastFailoverReason != "" {
				currentAttempt++
			} else {
				currentAttempt = 1
				lastFailoverReason = ""
			}
		}

		a.summary.Duration = time.Since(a.startTime)
		a.PrintSummary(stdout)
		_ = a.WriteReport()
		return nil
	}

	// 3. Live log follower mode (auditing live daemon from runtime log file)
	logFile := a.options.LogPath
	if logFile == "" {
		logFile = config.GetLogFile()
	}

	// Wait up to 2 seconds for log file to exist if not present yet
	for i := 0; i < 20; i++ {
		if _, err := os.Stat(logFile); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			a.summary.Duration = time.Since(a.startTime)
			a.PrintSummary(stdout)
			_ = a.WriteReport()
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}

	f, err := os.Open(logFile)
	if err != nil {
		if !a.options.JSONMode {
			fmt.Fprintf(stderr, "Log file does not exist yet: %s. Waiting for events...\n", logFile)
		}
	} else {
		defer f.Close()
		if !a.options.FromStart {
			_, _ = f.Seek(0, io.SeekEnd)
		}
	}

	currentAttempt := 1
	lastFailoverReason := ""

	var reader *bufio.Reader
	if f != nil {
		reader = bufio.NewReader(f)
	}

	for {
		select {
		case <-ctx.Done():
			a.summary.Duration = time.Since(a.startTime)
			a.PrintSummary(stdout)
			_ = a.WriteReport()
			return nil
		default:
		}

		if f == nil {
			newF, err := os.Open(logFile)
			if err == nil {
				f = newF
				defer f.Close()
				reader = bufio.NewReader(f)
			} else {
				time.Sleep(200 * time.Millisecond)
				continue
			}
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				// Check for log rotation or truncation
				if isRotatedOrTruncated(f, logFile) {
					newF, reopenErr := os.Open(logFile)
					if reopenErr == nil {
						_ = f.Close()
						f = newF
						reader = bufio.NewReader(f)
						continue
					}
				}
				time.Sleep(100 * time.Millisecond)
				continue
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}

		entry, ok := ParseLogLine(line)
		if !ok {
			continue
		}

		if entry.Type == "FAILOVER" {
			lastFailoverReason = entry.FailoverReason
			continue
		}

		if !entry.IsGeneration {
			continue
		}

		var ev RoutingAuditEvent
		if entry.HasAuditMeta {
			ev = EvaluateFrozenRoutingEvent(proxy.RoutingEvent{
				Timestamp:             entry.Timestamp,
				Strategy:              entry.Strategy,
				Account:               entry.AccountName,
				AccountName:           entry.AccountName,
				Classification:        entry.Classification,
				HealthyCandidateCount: entry.HealthyCount,
				ReserveCandidateCount: entry.ReserveCount,
				Attempt:               currentAttempt,
				Status:                entry.Status,
				FailoverReason:        lastFailoverReason,
				Path:                  entry.Path,
			})
		} else {
			currentPool, _ := storage.LoadPool()
			if currentPool == nil {
				currentPool = pool
			}
			ev = EvaluateRoutingAttempt(
				currentPool,
				entry.AccountName,
				entry.Status,
				currentAttempt,
				lastFailoverReason,
				entry.Timestamp,
				"",
			)
		}
		a.RecordEvent(ev, stdout)

		if entry.Status >= 400 && lastFailoverReason != "" {
			currentAttempt++
		} else {
			currentAttempt = 1
			lastFailoverReason = ""
		}
	}
}

func isRotatedOrTruncated(f *os.File, path string) bool {
	if f == nil {
		return true
	}
	fi, err := f.Stat()
	if err != nil {
		return true
	}
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !os.SameFile(fi, st) {
		return true
	}
	pos, err := f.Seek(0, io.SeekCurrent)
	if err == nil && st.Size() < pos {
		return true
	}
	return false
}

// RunAuditRouting executes the audit-routing command with given options.
func RunAuditRouting(ctx context.Context, opts AuditOptions, stdout, stderr io.Writer) int {
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
	}

	auditor := NewAuditor(opts)
	err := auditor.Run(ctx, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "audit-routing error: %v\n", err)
		return 1
	}

	if auditor.summary.Violations > 0 {
		return 1
	}
	return 0
}
