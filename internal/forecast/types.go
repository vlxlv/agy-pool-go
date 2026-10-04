package forecast

const (
	DefaultFractionPerHit5H     = 0.0050   // ~0.50% of 5h quota (~200 generations per 5h window)
	DefaultFractionPerHitWeekly = 0.00035  // ~0.035% of weekly quota (~2,850 generations per week)
	BenchmarkPaceGensPerHour    = 20.0     // Standard active pair-programming baseline (1 gen every 3 mins)
	TelemetryCap                = 1000     // Maximum rolling generation events stored
	Window5HSecs                = 18000.0  // 5 * 3600
	Window7DSecs                = 604800.0 // 7 * 86400
)

// BurnRateTelemetry contains rolling window inference metrics and estimated burn rate.
type BurnRateTelemetry struct {
	GensLast15M     int     `json:"gens_last_15m"`
	GensLast1H      int     `json:"gens_last_1h"`
	GensLast4H      int     `json:"gens_last_4h"`
	BurstRate15M    float64 `json:"burst_rate_15m"`
	SustainedRate1H float64 `json:"sustained_rate_1h"`
	SustainedRate4H float64 `json:"sustained_rate_4h"`
	GensPerHour     float64 `json:"gens_per_hour"`
	DailyPace       float64 `json:"daily_pace"`
	IsIdle          bool    `json:"is_idle"`
	WindowUsed      string  `json:"window_used"`
}

// SingleRunwayResult models time to first quota stall and weekly exhaustion under simulated load.
type SingleRunwayResult struct {
	FirstStallSec       *float64 `json:"first_stall_sec"`
	FirstStallCause     string   `json:"first_stall_cause,omitempty"`
	StallDurationSec    *float64 `json:"stall_duration_sec"`
	FirstEmptyAccID     string   `json:"first_empty_acc_id,omitempty"`
	FirstEmptySec       *float64 `json:"first_empty_sec"`
	WeeklyExhaustSec    *float64 `json:"weekly_exhaust_sec"`
	IsSustainable5H     bool     `json:"is_sustainable_5h"`
	IsSustainableWeekly bool     `json:"is_sustainable_weekly"`
}

// AccountForecast holds calculated per-account quota projection details.
type AccountForecast struct {
	ID           string  `json:"id"`
	DisplayName  string  `json:"display_name"`
	Q5Pct        float64 `json:"q5_pct"`
	QWPct        float64 `json:"qw_pct"`
	Reset5InSec  int64   `json:"reset5_in_sec"`
	ResetWInSec  int64   `json:"resetw_in_sec"`
	IsCooling    bool    `json:"is_cooling"`
	IsRestricted bool    `json:"is_restricted"`
}

// RunwayForecast represents the overall quota endurance prediction for the pool.
type RunwayForecast struct {
	Status             string             `json:"status"`
	HealthyCount       int                `json:"healthy_count"`
	CoolingCount       int                `json:"cooling_count"`
	RestrictedCount    int                `json:"restricted_count"`
	TotalAccounts      int                `json:"total_accounts"`
	Message            string             `json:"message,omitempty"`
	Strategy           string             `json:"strategy,omitempty"`
	TargetPace         float64            `json:"target_pace,omitempty"`
	TargetWeeklyPace   float64            `json:"target_weekly_pace,omitempty"`
	IsCustomPace       bool               `json:"is_custom_pace,omitempty"`
	IsIdle             bool               `json:"is_idle,omitempty"`
	Telemetry          *BurnRateTelemetry `json:"telemetry,omitempty"`
	ReplenishRatePctHr float64            `json:"replenish_rate_pct_hr,omitempty"`
	BurnRatePctHr      float64            `json:"burn_rate_pct_hr,omitempty"`
	NetRatePctHr       float64            `json:"net_rate_pct_hr,omitempty"`
	TotalQ5Pct         float64            `json:"total_q5_pct,omitempty"`
	TotalQWPct         float64            `json:"total_qw_pct,omitempty"`
	CurrentRunway      SingleRunwayResult `json:"current_runway,omitempty"`
	BenchmarkRunway    SingleRunwayResult `json:"benchmark_runway,omitempty"`
	AccountsForecast   []AccountForecast  `json:"accounts_forecast,omitempty"`
}

// SimAccount tracks internal mutable state for chronological minute-by-minute simulation.
type SimAccount struct {
	ID            string
	DisplayName   string
	IsRestricted  bool
	CooldownUntil float64
	Q5            float64
	QW            float64
	Reset5TS      float64
	ResetWTS      float64
	SimHits       float64
}
