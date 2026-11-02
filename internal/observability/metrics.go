package observability

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Metrics holds low-overhead process-lifetime atomic counters for runtime observability.
type Metrics struct {
	GenerationRequests   atomic.Uint64
	GenerationSuccess    atomic.Uint64
	RoutingDecisions     atomic.Uint64
	FailoverAttempts     atomic.Uint64
	FailoverSuccess      atomic.Uint64
	RestrictedSkips      atomic.Uint64
	QuotaRefreshAttempts atomic.Uint64
	QuotaRefreshSuccess  atomic.Uint64
	QuotaRefreshFailure  atomic.Uint64
	AuthRefreshAttempts  atomic.Uint64
	AuthRefreshSuccess   atomic.Uint64
	AuthRefreshFailure   atomic.Uint64
	PersistenceErrors    atomic.Uint64
	NoReplayPrevented    atomic.Uint64
	LeaseFallbacks       atomic.Uint64
}

// Snapshot represents a point-in-time snapshot of process-lifetime counters.
type Snapshot struct {
	GenerationRequests   uint64 `json:"generation_requests_total"`
	GenerationSuccess    uint64 `json:"generation_success_total"`
	RoutingDecisions     uint64 `json:"routing_decisions_total"`
	FailoverAttempts     uint64 `json:"failover_attempts_total"`
	FailoverSuccess      uint64 `json:"failover_success_total"`
	RestrictedSkips      uint64 `json:"restricted_account_skips_total"`
	QuotaRefreshAttempts uint64 `json:"quota_refresh_attempts_total"`
	QuotaRefreshSuccess  uint64 `json:"quota_refresh_success_total"`
	QuotaRefreshFailure  uint64 `json:"quota_refresh_failure_total"`
	AuthRefreshAttempts  uint64 `json:"auth_refresh_attempts_total"`
	AuthRefreshSuccess   uint64 `json:"auth_refresh_success_total"`
	AuthRefreshFailure   uint64 `json:"auth_refresh_failure_total"`
	PersistenceErrors    uint64 `json:"persistence_errors_total"`
	NoReplayPrevented    uint64 `json:"no_replay_prevented_total"`
	LeaseFallbacks       uint64 `json:"lease_fallbacks_total"`
}

var globalMetrics Metrics

// Reset resets all process-lifetime counters to zero (used during startup or test setup).
func Reset() {
	globalMetrics.GenerationRequests.Store(0)
	globalMetrics.GenerationSuccess.Store(0)
	globalMetrics.RoutingDecisions.Store(0)
	globalMetrics.FailoverAttempts.Store(0)
	globalMetrics.FailoverSuccess.Store(0)
	globalMetrics.RestrictedSkips.Store(0)
	globalMetrics.QuotaRefreshAttempts.Store(0)
	globalMetrics.QuotaRefreshSuccess.Store(0)
	globalMetrics.QuotaRefreshFailure.Store(0)
	globalMetrics.AuthRefreshAttempts.Store(0)
	globalMetrics.AuthRefreshSuccess.Store(0)
	globalMetrics.AuthRefreshFailure.Store(0)
	globalMetrics.PersistenceErrors.Store(0)
	globalMetrics.NoReplayPrevented.Store(0)
	globalMetrics.LeaseFallbacks.Store(0)
	ResetInFlight()
}

// GetSnapshot returns a point-in-time snapshot of all observability counters.
func GetSnapshot() Snapshot {
	return Snapshot{
		GenerationRequests:   globalMetrics.GenerationRequests.Load(),
		GenerationSuccess:    globalMetrics.GenerationSuccess.Load(),
		RoutingDecisions:     globalMetrics.RoutingDecisions.Load(),
		FailoverAttempts:     globalMetrics.FailoverAttempts.Load(),
		FailoverSuccess:      globalMetrics.FailoverSuccess.Load(),
		RestrictedSkips:      globalMetrics.RestrictedSkips.Load(),
		QuotaRefreshAttempts: globalMetrics.QuotaRefreshAttempts.Load(),
		QuotaRefreshSuccess:  globalMetrics.QuotaRefreshSuccess.Load(),
		QuotaRefreshFailure:  globalMetrics.QuotaRefreshFailure.Load(),
		AuthRefreshAttempts:  globalMetrics.AuthRefreshAttempts.Load(),
		AuthRefreshSuccess:   globalMetrics.AuthRefreshSuccess.Load(),
		AuthRefreshFailure:   globalMetrics.AuthRefreshFailure.Load(),
		PersistenceErrors:    globalMetrics.PersistenceErrors.Load(),
		NoReplayPrevented:    globalMetrics.NoReplayPrevented.Load(),
		LeaseFallbacks:       globalMetrics.LeaseFallbacks.Load(),
	}
}

// FormatPrometheusMetrics formats the current metrics snapshot in Prometheus text exposition format (version 0.0.4).
func FormatPrometheusMetrics() string {
	s := GetSnapshot()
	var b strings.Builder
	writeMetric := func(name, help, typ string, val uint64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", name, help, name, typ, name, val)
	}

	writeMetric("agy_generation_requests_total", "Total generation requests handled", "counter", s.GenerationRequests)
	writeMetric("agy_generation_success_total", "Total generation requests successfully finished", "counter", s.GenerationSuccess)
	writeMetric("agy_routing_decisions_total", "Total routing candidate decisions made", "counter", s.RoutingDecisions)
	writeMetric("agy_failover_attempts_total", "Total failover attempts to secondary candidates", "counter", s.FailoverAttempts)
	writeMetric("agy_failover_success_total", "Total successful failover dispatches", "counter", s.FailoverSuccess)
	writeMetric("agy_restricted_account_skips_total", "Total candidates skipped due to account restrictions", "counter", s.RestrictedSkips)
	writeMetric("agy_quota_refresh_attempts_total", "Total background quota refresh attempts", "counter", s.QuotaRefreshAttempts)
	writeMetric("agy_quota_refresh_success_total", "Total background quota refreshes succeeded", "counter", s.QuotaRefreshSuccess)
	writeMetric("agy_quota_refresh_failure_total", "Total background quota refreshes failed", "counter", s.QuotaRefreshFailure)
	writeMetric("agy_auth_refresh_attempts_total", "Total OAuth token refresh attempts", "counter", s.AuthRefreshAttempts)
	writeMetric("agy_auth_refresh_success_total", "Total OAuth token refreshes succeeded", "counter", s.AuthRefreshSuccess)
	writeMetric("agy_auth_refresh_failure_total", "Total OAuth token refreshes failed", "counter", s.AuthRefreshFailure)
	writeMetric("agy_persistence_errors_total", "Total state persistence errors", "counter", s.PersistenceErrors)
	writeMetric("agy_no_replay_prevented_total", "Total requests prevented from replaying once committed", "counter", s.NoReplayPrevented)
	writeMetric("agy_lease_fallbacks_total", "Total fallback reservations when DB lease failed", "counter", s.LeaseFallbacks)

	inFlightMu.RLock()
	totalInFlight := int64(0)
	for _, count := range inFlightCounts {
		totalInFlight += count
	}
	inFlightMu.RUnlock()
	fmt.Fprintf(&b, "# HELP agy_inflight_generations Current concurrent generation requests\n# TYPE agy_inflight_generations gauge\nagy_inflight_generations %d\n", totalInFlight)

	return b.String()
}

// RecordGenerationRequest increments generation_requests_total.
func RecordGenerationRequest() {
	globalMetrics.GenerationRequests.Add(1)
}

// RecordGenerationSuccess increments generation_success_total.
func RecordGenerationSuccess() {
	globalMetrics.GenerationSuccess.Add(1)
}

// RecordRoutingDecision increments routing_decisions_total.
func RecordRoutingDecision() {
	globalMetrics.RoutingDecisions.Add(1)
}

// RecordFailoverAttempt increments failover_attempts_total.
func RecordFailoverAttempt() {
	globalMetrics.FailoverAttempts.Add(1)
}

// RecordFailoverSuccess increments failover_success_total.
func RecordFailoverSuccess() {
	globalMetrics.FailoverSuccess.Add(1)
}

// RecordRestrictedSkip increments restricted_account_skips_total by 1.
func RecordRestrictedSkip() {
	globalMetrics.RestrictedSkips.Add(1)
}

// RecordQuotaRefreshAttempt increments quota_refresh_attempts_total.
func RecordQuotaRefreshAttempt() {
	globalMetrics.QuotaRefreshAttempts.Add(1)
}

// RecordQuotaRefreshSuccess increments quota_refresh_success_total.
func RecordQuotaRefreshSuccess() {
	globalMetrics.QuotaRefreshSuccess.Add(1)
}

// RecordQuotaRefreshFailure increments quota_refresh_failure_total.
func RecordQuotaRefreshFailure() {
	globalMetrics.QuotaRefreshFailure.Add(1)
}

// RecordAuthRefreshAttempt increments auth_refresh_attempts_total.
func RecordAuthRefreshAttempt() {
	globalMetrics.AuthRefreshAttempts.Add(1)
}

// RecordAuthRefreshSuccess increments auth_refresh_success_total.
func RecordAuthRefreshSuccess() {
	globalMetrics.AuthRefreshSuccess.Add(1)
}

// RecordAuthRefreshFailure increments auth_refresh_failure_total.
func RecordAuthRefreshFailure() {
	globalMetrics.AuthRefreshFailure.Add(1)
}

// RecordPersistenceError increments persistence_errors_total.
func RecordPersistenceError() {
	globalMetrics.PersistenceErrors.Add(1)
}

// RecordLeaseFallback increments lease_fallbacks_total: a routing pick that could not record its
// cross-process in-flight lease and was balanced on this process's counts alone.
func RecordLeaseFallback() {
	globalMetrics.LeaseFallbacks.Add(1)
}

// RecordNoReplayPrevented increments no_replay_prevented_total.
func RecordNoReplayPrevented() {
	globalMetrics.NoReplayPrevented.Add(1)
}

// FormatNumber formats a uint64 integer with thousands separators (e.g. 12,430).
func FormatNumber(n uint64) string {
	in := strconv.FormatUint(n, 10)
	if len(in) <= 3 {
		return in
	}
	var out []byte
	rem := len(in) % 3
	if rem > 0 {
		out = append(out, in[:rem]...)
	}
	for i := rem; i < len(in); i += 3 {
		if len(out) > 0 {
			out = append(out, ',')
		}
		out = append(out, in[i:i+3]...)
	}
	return string(out)
}

var (
	inFlightMu       sync.RWMutex
	inFlightCounts   = make(map[string]int64)
	remoteInFlight   map[string]int64
	onInFlightChange func()
)

// SetRemoteInFlight replaces the snapshot of in-flight counts held by other processes sharing the pool.
func SetRemoteInFlight(m map[string]int64) {
	inFlightMu.Lock()
	remoteInFlight = m
	inFlightMu.Unlock()
}

// SetOnInFlightChange registers a callback invoked whenever an in-flight generation starts or finishes.
func SetOnInFlightChange(fn func()) {
	inFlightMu.Lock()
	onInFlightChange = fn
	inFlightMu.Unlock()
}

// StartInFlightGeneration increments the in-flight count for accountID and returns a done function to decrement it.
// Decrementing via the returned function is idempotent (protected by sync.Once).
func StartInFlightGeneration(accountID string) func() {
	if accountID == "" {
		return func() {}
	}
	inFlightMu.Lock()
	inFlightCounts[accountID]++
	fn := onInFlightChange
	inFlightMu.Unlock()

	if fn != nil {
		fn()
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			inFlightMu.Lock()
			if val, ok := inFlightCounts[accountID]; ok {
				if val <= 1 {
					delete(inFlightCounts, accountID)
				} else {
					inFlightCounts[accountID] = val - 1
				}
			}
			fn2 := onInFlightChange
			inFlightMu.Unlock()

			if fn2 != nil {
				fn2()
			}
		})
	}
}

// GetInFlightGeneration returns the current in-flight generation request count for an account,
// including the last snapshot of other processes' requests.
func GetInFlightGeneration(accountID string) int64 {
	inFlightMu.RLock()
	defer inFlightMu.RUnlock()
	return inFlightCounts[accountID] + remoteInFlight[accountID]
}

// GetAllInFlightGenerations returns a map snapshot of all accounts with positive in-flight requests.
// Returns nil if no requests are in flight.
func GetAllInFlightGenerations() map[string]int64 {
	inFlightMu.RLock()
	defer inFlightMu.RUnlock()
	if len(inFlightCounts) == 0 {
		return nil
	}
	res := make(map[string]int64, len(inFlightCounts))
	for k, v := range inFlightCounts {
		if v > 0 {
			res[k] = v
		}
	}
	if len(res) == 0 {
		return nil
	}
	return res
}

// ResetInFlight resets all in-flight counts to zero.
func ResetInFlight() {
	inFlightMu.Lock()
	inFlightCounts = make(map[string]int64)
	remoteInFlight = nil
	inFlightMu.Unlock()
}
