package diagnostics

import (
	"math"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/scheduler"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// AccountEffectiveness contains evaluation metrics for an individual account in the pool.
type AccountEffectiveness struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Status         string   `json:"status"`
	IsNext         bool     `json:"is_next"`
	WindowRequests int64    `json:"window_requests"`
	WindowPercent  float64  `json:"window_percent"`
	LifetimeHits   int64    `json:"lifetime_hits"`
	Quota5HPercent *float64 `json:"quota_5h_percent,omitempty"`
	Quota5HReset   string   `json:"quota_5h_reset,omitempty"`
	QuotaWkPercent *float64 `json:"quota_wk_percent,omitempty"`
	QuotaWkReset   string   `json:"quota_wk_reset,omitempty"`
	InFlight       int64    `json:"in_flight"`
	IsPaused       bool     `json:"is_paused"`
}

// GatewayEffectiveness contains real-time gateway process counters.
type GatewayEffectiveness struct {
	Status           string `json:"status"`
	PID              int    `json:"pid"`
	Uptime           string `json:"uptime"`
	UptimeSeconds    int64  `json:"uptime_seconds"`
	GenerationTotal  uint64 `json:"generation_total"`
	GenerationOk     uint64 `json:"generation_ok"`
	RoutingDecisions uint64 `json:"routing_decisions"`
	FailoversTotal   uint64 `json:"failovers_total"`
	FailoversOk      uint64 `json:"failovers_ok"`
	RestrictedSkips  uint64 `json:"restricted_skips"`
}

// AIVerdict provides a concise diagnostic analysis specifically formatted for AI reasoning.
type AIVerdict struct {
	Status          string   `json:"status"` // "OPTIMAL", "BALANCED", "SKEWED", "DEGRADED", "CRITICAL"
	Summary         string   `json:"summary"`
	Issues          []string `json:"issues"`
	Recommendations []string `json:"recommendations"`
}

// EffectivenessReport represents a comprehensive algorithm tracking snapshot.
type EffectivenessReport struct {
	Timestamp       string                 `json:"timestamp"`
	Strategy        string                 `json:"strategy"`
	Gateway         GatewayEffectiveness   `json:"gateway"`
	WindowSeconds   int64                  `json:"window_seconds"`
	WindowRequests  int64                  `json:"window_requests_total"`
	Accounts        []AccountEffectiveness `json:"accounts"`
	ActiveAccounts  int                    `json:"active_accounts"`
	BalanceIndex    float64                `json:"balance_index"`  // 0.0 to 1.0 (1.0 = ideal even distribution)
	BalanceStatus   string                 `json:"balance_status"` // "OPTIMAL", "BALANCED", "SKEWED", "MONOPOLIZED", "IDLE"
	DominantAccount string                 `json:"dominant_account,omitempty"`
	AIVerdict       AIVerdict              `json:"ai_verdict"`
}

// ComputeEffectivenessReport compiles algorithm performance metrics from storage and daemon runtime.
func ComputeEffectivenessReport(ctx *daemon.InstanceContext, windowDuration time.Duration) (*EffectivenessReport, error) {
	pool, err := storage.LoadPool()
	if err != nil {
		return nil, err
	}
	if pool == nil {
		pool = storage.NewEmptyPool()
	}

	nowTime := time.Now()
	nowTS := nowTime.Unix()
	strat := pool.Strategy
	if strat == "" {
		strat = config.DefaultStrategy
	}

	if windowDuration <= 0 {
		windowDuration = 4 * time.Hour
	}
	cutoffTS := nowTS - int64(windowDuration.Seconds())

	// 1. Gather Gateway runtime metrics if running
	gwEff := GatewayEffectiveness{
		Status: "STOPPED",
	}
	var runtimeStats *daemon.RuntimeStats
	if ctx != nil {
		status, info, _ := daemon.CheckInstanceStatus(ctx)
		if (status == daemon.StatusRunningSameInstance || status == daemon.StatusOutdatedBinary) && info != nil && info.PID > 0 {
			gwEff.Status = "RUNNING"
			gwEff.PID = info.PID
			stats, err := ctx.GetRuntimeStats(info.PID)
			if err == nil && stats != nil {
				runtimeStats = stats
				gwEff.Uptime = daemon.FormatUptime(time.Since(stats.StartedAt))
				gwEff.UptimeSeconds = int64(time.Since(stats.StartedAt).Seconds())
				gwEff.GenerationTotal = stats.GenerationRequests
				gwEff.GenerationOk = stats.GenerationSuccess
				gwEff.RoutingDecisions = stats.RoutingDecisions
				gwEff.FailoversTotal = stats.FailoverAttempts
				gwEff.FailoversOk = stats.FailoverSuccess
				gwEff.RestrictedSkips = stats.RestrictedSkips
			}
		}
	}

	// 2. Count window requests per account from pool.GenerationEvents
	eventCounts := make(map[string]int64)
	var totalWindowEvents int64
	for _, e := range pool.GenerationEvents {
		if e.TS >= cutoffTS {
			eventCounts[e.ID]++
			totalWindowEvents++
		}
	}

	// 3. Determine current candidate order (who is Next ⚡)
	ordered := scheduler.OrderCandidates(pool.Accounts, strat, pool, float64(nowTS))
	if strat == config.StrategySmart {
		ordered = scheduler.RotateTopTier(ordered, scheduler.GetSmartRRCursor(), float64(nowTS))
	}
	var nextID string
	if len(ordered) > 0 && ordered[0] != nil {
		nextID = ordered[0].ID
	}

	// 4. Build per-account metrics
	accMetrics := make([]AccountEffectiveness, 0, len(pool.Accounts))
	var activeReadyCount int
	var activeReadyRequests []int64
	var activeReadyIDs []string

	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		eff := AccountEffectiveness{
			ID:           a.ID,
			Name:         accounts.DisplayAccountName(a),
			Status:       a.Status,
			IsNext:       a.ID == nextID,
			LifetimeHits: a.GetHits(),
		}

		if runtimeStats != nil && runtimeStats.InFlight != nil {
			eff.InFlight = runtimeStats.InFlight[a.ID]
		}
		if runtimeStats != nil && runtimeStats.ModelPauses != nil {
			if pauses, ok := runtimeStats.ModelPauses[a.ID]; ok && len(pauses) > 0 {
				eff.IsPaused = true
			}
		}

		// Quotas and capacity state
		capState := quota.ComputeCapacityState(a, float64(nowTS))
		q5Frac, gwFrac := quota.DisplayQuotaFractions(a.LastQuota)
		if q5Frac != nil {
			pct := math.Round(*q5Frac * 1000) / 10
			eff.Quota5HPercent = &pct
			if capState.Reset5Time != nil && !capState.Reset5Time.IsZero() {
				eff.Quota5HReset = quota.FormatCompactRemainingTime(capState.Reset5Time.Format(time.RFC3339))
			}
		}
		if gwFrac != nil {
			pct := math.Round(*gwFrac * 1000) / 10
			eff.QuotaWkPercent = &pct
			if capState.Reset7Time != nil && !capState.Reset7Time.IsZero() {
				eff.QuotaWkReset = quota.FormatCompactRemainingTime(capState.Reset7Time.Format(time.RFC3339))
			}
		}

		// Window requests
		reqs := eventCounts[a.ID]
		eff.WindowRequests = reqs
		if totalWindowEvents > 0 {
			eff.WindowPercent = math.Round(float64(reqs)*1000.0/float64(totalWindowEvents)) / 10.0
		}

		// Classify pool status
		if a.Status == storage.StatusDisabled {
			eff.Status = "Disabled"
		} else if scheduler.IsRestricted(a) {
			eff.Status = "Restricted"
		} else if a.RateLimitedUntil != nil && *a.RateLimitedUntil > float64(nowTS) {
			eff.Status = "Cooldown"
		} else if gwFrac != nil && *gwFrac <= config.DepletedThreshold {
			eff.Status = "Exhausted"
		} else {
			eff.Status = "Ready"
			activeReadyCount++
			activeReadyRequests = append(activeReadyRequests, reqs)
			activeReadyIDs = append(activeReadyIDs, a.ID)
		}

		accMetrics = append(accMetrics, eff)
	}

	// 5. Calculate Balance Index across active Ready accounts
	// Uses Normalized Relative Standard Deviation (RSD)
	balanceIndex := 1.0
	balanceStatus := "IDLE"
	var dominantAccount string
	var maxActiveReqs int64

	if activeReadyCount <= 1 {
		balanceIndex = 1.0
		if activeReadyCount == 1 {
			balanceStatus = "MONOPOLIZED"
			dominantAccount = activeReadyIDs[0]
		} else {
			balanceStatus = "IDLE"
		}
	} else if totalWindowEvents == 0 {
		balanceIndex = 1.0
		balanceStatus = "IDLE"
	} else {
		// Calculate mean and variance among active ready accounts
		var totalActiveReqs int64
		for i, reqs := range activeReadyRequests {
			totalActiveReqs += reqs
			if reqs > maxActiveReqs {
				maxActiveReqs = reqs
				dominantAccount = activeReadyIDs[i]
			}
		}

		if totalActiveReqs > 0 {
			mean := float64(totalActiveReqs) / float64(activeReadyCount)
			var sumSqDiff float64
			for _, reqs := range activeReadyRequests {
				diff := float64(reqs) - mean
				sumSqDiff += diff * diff
			}
			stdDev := math.Sqrt(sumSqDiff / float64(activeReadyCount))
			rsd := stdDev / mean
			// Max possible RSD when 1 account has all traffic is sqrt(N - 1)
			maxRSD := math.Sqrt(float64(activeReadyCount - 1))
			if maxRSD > 0 {
				normalizedSkew := math.Min(1.0, rsd/maxRSD)
				balanceIndex = math.Round((1.0-normalizedSkew)*100.0) / 100.0
			}

			switch {
			case balanceIndex >= 0.80:
				balanceStatus = "OPTIMAL"
			case balanceIndex >= 0.60:
				balanceStatus = "BALANCED"
			case balanceIndex >= 0.35:
				balanceStatus = "SKEWED"
			default:
				balanceStatus = "MONOPOLIZED"
			}
		}
	}

	// 6. Generate AI Verdict & Issues
	var issues []string
	var recommendations []string
	verdictStatus := "HEALTHY"

	if gwEff.Status != "RUNNING" {
		issues = append(issues, "Gateway proxy is currently STOPPED.")
		recommendations = append(recommendations, "Start gateway with 'agy-pool start'.")
		verdictStatus = "DEGRADED"
	}

	for _, acc := range accMetrics {
		if acc.Status == "Exhausted" {
			issues = append(issues, "Account "+acc.Name+" is Exhausted (Weekly quota depleted, all models blocked until reset).")
			verdictStatus = "DEGRADED"
		} else if acc.Status == "Restricted" {
			issues = append(issues, "Account "+acc.Name+" is Restricted (Requires re-verification with 'agy-pool verify').")
			if verdictStatus != "DEGRADED" {
				verdictStatus = "WARNING"
			}
		} else if acc.Status == "Cooldown" {
			issues = append(issues, "Account "+acc.Name+" is currently in Cooldown (429 rate limit backoff).")
			if verdictStatus != "DEGRADED" {
				verdictStatus = "WARNING"
			}
		}

		// Check if any active account is at weekly exhaustion risk
		if acc.Status == "Ready" && acc.QuotaWkPercent != nil && *acc.QuotaWkPercent < 15.0 {
			issues = append(issues, "Account "+acc.Name+" has low weekly quota ("+acc.QuotaWkReset+" until reset).")
			if verdictStatus != "DEGRADED" {
				verdictStatus = "WARNING"
			}
		}
	}

	// Traffic skew check
	if balanceStatus == "SKEWED" || balanceStatus == "MONOPOLIZED" {
		if totalWindowEvents >= int64(activeReadyCount*3) {
			issues = append(issues, "Traffic distribution is skewed (Dominant account: "+dominantAccount+").")
			if verdictStatus == "HEALTHY" {
				verdictStatus = "WARNING"
			}
			recommendations = append(recommendations, "Strategy is currently '"+strat+"'. Ensure cruising accounts rotate by Least-Hits.")
		}
	}

	if gwEff.FailoversTotal > 0 && gwEff.GenerationTotal > 0 {
		failRate := float64(gwEff.FailoversTotal) / float64(gwEff.GenerationTotal)
		if failRate > 0.05 {
			issues = append(issues, "High failover rate detected (failovers occur on >5% of generations).")
			if verdictStatus != "CRITICAL" {
				verdictStatus = "WARNING"
			}
		}
	}

	if len(issues) == 0 {
		recommendations = append(recommendations, "Strategy '"+strat+"' is operating smoothly with balanced account rotation and healthy quota margins.")
	} else if len(recommendations) == 0 {
		recommendations = append(recommendations, "Review account statuses and run 'agy-pool quota' to monitor replenishment timing.")
	}

	var summary string
	switch verdictStatus {
	case "HEALTHY":
		summary = "All active accounts are healthy, traffic is well-distributed, and quotas are sustainable."
	case "WARNING":
		summary = "System is operational, but some accounts have low quota or traffic is non-uniform."
	case "DEGRADED":
		summary = "One or more accounts are exhausted or restricted, reducing total pool concurrency."
	default:
		summary = "Critical condition detected in account pool or gateway proxy."
	}

	report := &EffectivenessReport{
		Timestamp:       nowTime.UTC().Format(time.RFC3339),
		Strategy:        strat,
		Gateway:         gwEff,
		WindowSeconds:   int64(windowDuration.Seconds()),
		WindowRequests:  totalWindowEvents,
		Accounts:        accMetrics,
		ActiveAccounts:  activeReadyCount,
		BalanceIndex:    balanceIndex,
		BalanceStatus:   balanceStatus,
		DominantAccount: dominantAccount,
		AIVerdict: AIVerdict{
			Status:          verdictStatus,
			Summary:         summary,
			Issues:          issues,
			Recommendations: recommendations,
		},
	}

	return report, nil
}
