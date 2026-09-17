package diagnostics

import "time"

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
