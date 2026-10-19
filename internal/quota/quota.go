package quota

import (
	"fmt"
	"math"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// ParseQuotaFraction returns a clamped [0.0, 1.0] fraction or nil if invalid.
func ParseQuotaFraction(v any) *float64 {
	if v == nil {
		return nil
	}
	var f float64
	switch val := v.(type) {
	case float64:
		f = val
	case float32:
		f = float64(val)
	case int:
		f = float64(val)
	case int64:
		f = float64(val)
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil {
			return nil
		}
		f = parsed
	default:
		return nil
	}

	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	if f < 0.0 {
		f = 0.0
	} else if f > 1.0 {
		f = 1.0
	}
	return &f
}

// ParseISOOrTimestamp converts an ISO 8601 string or epoch numeric timestamp to Unix seconds.
func ParseISOOrTimestamp(val any) *float64 {
	if val == nil {
		return nil
	}
	switch v := val.(type) {
	case float64:
		return &v
	case float32:
		f := float64(v)
		return &f
	case int:
		f := float64(v)
		return &f
	case int64:
		f := float64(v)
		return &f
	case string:
		str := strings.TrimSpace(v)
		if str == "" {
			return nil
		}
		upper := strings.ToUpper(str)
		if upper == "N/A" || upper == "NONE" || upper == "NULL" || upper == "READY" {
			return nil
		}
		// Attempt numeric parse first
		if num, err := strconv.ParseFloat(str, 64); err == nil {
			return &num
		}
		// Attempt ISO 8601 parsing
		clean := strings.Replace(str, "Z", "+00:00", 1)
		// Try RFC 3339
		t, err := time.Parse(time.RFC3339, str)
		if err == nil {
			sec := float64(t.Unix())
			return &sec
		}
		t, err = time.Parse(time.RFC3339Nano, str)
		if err == nil {
			sec := float64(t.UnixNano()) / 1e9
			return &sec
		}
		t, err = time.Parse("2006-01-02T15:04:05-07:00", clean)
		if err == nil {
			sec := float64(t.Unix())
			return &sec
		}
		t, err = time.Parse("2006-01-02T15:04:05", clean)
		if err == nil {
			sec := float64(t.Unix())
			return &sec
		}
		return nil
	default:
		return nil
	}
}

// FreshnessInfo describes the freshness classification of an account's quota.
type FreshnessInfo struct {
	Class string   `json:"class"` // "fresh", "aging", "stale", "unknown"
	Rank  int      `json:"rank"`  // 3 (fresh), 2 (aging), 1 (stale), 0 (unknown)
	Age   *float64 `json:"age"`
}

// Freshness classifies account quota freshness against a given reference time.
func Freshness(acc *storage.Account, now float64) FreshnessInfo {
	if acc == nil || acc.LastQuota == nil || acc.LastQuota.UpdatedAt == nil || *acc.LastQuota.UpdatedAt <= 0 {
		return FreshnessInfo{Class: "unknown", Rank: 0, Age: nil}
	}

	updated := float64(*acc.LastQuota.UpdatedAt)
	age := now - updated
	if age < 0 {
		age = 0
	}

	// Check if any window reset time has passed since the snapshot was taken. A reset already past at
	// snapshot time means the refresh did not renew that window; marking it stale would only starve the
	// account, since refreshing again returns the same reset.
	resetPassed := false
	checkWindow := func(w *storage.QuotaWindow) {
		if w != nil && w.ResetTime != nil {
			if ts := ParseISOOrTimestamp(w.ResetTime); ts != nil && *ts <= now && *ts > updated {
				resetPassed = true
			}
		}
	}
	checkWindow(acc.LastQuota.Gemini5H)
	checkWindow(acc.LastQuota.GeminiWeekly)

	var className string
	var rank int

	if resetPassed || age > config.QuotaAgingMaxAge {
		className, rank = "stale", 1
	} else if age > config.QuotaFreshMaxAge {
		className, rank = "aging", 2
	} else {
		className, rank = "fresh", 3
	}

	return FreshnessInfo{
		Class: className,
		Rank:  rank,
		Age:   &age,
	}
}

// GeminiQuotaNotRenewed reports whether the latest refresh left a Gemini window missing or still at a
// reset time from before that refresh, which means upstream stopped reporting it.
func GeminiQuotaNotRenewed(acc *storage.Account) bool {
	if acc == nil || acc.LastQuota == nil || acc.LastQuota.UpdatedAt == nil || *acc.LastQuota.UpdatedAt <= 0 {
		return false
	}
	updated := float64(*acc.LastQuota.UpdatedAt)
	for _, w := range []*storage.QuotaWindow{acc.LastQuota.Gemini5H, acc.LastQuota.GeminiWeekly} {
		if w == nil {
			return true
		}
		if ts := ParseISOOrTimestamp(w.ResetTime); ts != nil && *ts <= updated {
			return true
		}
	}
	return false
}

// FreshnessRank returns the scheduling confidence rank:
// fresh (2), aging (2), stale (1), unknown (0)
func FreshnessRank(acc *storage.Account, now float64) int {
	info := Freshness(acc, now)
	switch info.Class {
	case "fresh", "aging":
		return 2
	case "stale":
		return 1
	default:
		return 0
	}
}

// RefreshNeeded reports whether the quota snapshot should be refreshed asynchronously.
func RefreshNeeded(acc *storage.Account, now float64) bool {
	return Freshness(acc, now).Class != "fresh"
}

// FormatQuotaAge returns human-readable representation of quota snapshot age.
func FormatQuotaAge(acc *storage.Account, now float64) string {
	info := Freshness(acc, now)
	if info.Age == nil {
		return "unknown"
	}
	age := int(*info.Age)
	if info.Class == "stale" {
		return fmt.Sprintf("stale (%dm)", age/60)
	}
	if age >= 60 {
		return fmt.Sprintf("%dm%02ds", age/60, age%60)
	}
	return fmt.Sprintf("%ds", age)
}

// CapacityState contains the reset-aware capacity parameters calculated for an account.
type CapacityState struct {
	Q5                  *float64   `json:"q5"`
	Q7                  *float64   `json:"q7"`
	R5                  float64    `json:"r5"`
	R7                  float64    `json:"r7"`
	Pace5               *float64   `json:"pace5"`
	Pace7               *float64   `json:"pace7"`
	WorstPace           float64    `json:"worst_pace"`
	TotalPace           float64    `json:"total_pace"`
	RawFloor            float64    `json:"raw_floor"`
	Q5Known             bool       `json:"q5_known"`
	Q7Known             bool       `json:"q7_known"`
	KnownWindowCount    int        `json:"known_window_count"`
	LegacyFractionKnown bool       `json:"legacy_fraction_known"`
	IsDepleted          bool       `json:"is_depleted"`
	Reset5Time          *time.Time `json:"reset5_time,omitempty"`
	Reset7Time          *time.Time `json:"reset7_time,omitempty"`
}

// ComputeCapacityState computes reset-aware capacity without treating missing fractions as full quota.
func ComputeCapacityState(acc *storage.Account, now float64) CapacityState {
	if acc == nil {
		return CapacityState{
			WorstPace: math.Inf(-1),
			TotalPace: math.Inf(-1),
		}
	}

	lq := acc.LastQuota
	hasExplicitWindows := lq != nil && (lq.Gemini5H != nil || lq.GeminiWeekly != nil)

	var q5, q7 *float64
	q5Known := false
	q7Known := false
	legacyFractionKnown := false

	if lq != nil {
		if lq.Gemini5H != nil && lq.Gemini5H.Fraction != nil {
			q5 = ParseQuotaFraction(*lq.Gemini5H.Fraction)
			q5Known = q5 != nil
		}
		if lq.GeminiWeekly != nil && lq.GeminiWeekly.Fraction != nil {
			q7 = ParseQuotaFraction(*lq.GeminiWeekly.Fraction)
			q7Known = q7 != nil
		}
	}

	if !hasExplicitWindows {
		var legacy *float64
		if lq != nil && lq.RemainingFraction != nil {
			legacy = ParseQuotaFraction(*lq.RemainingFraction)
		}
		if legacy == nil && acc.Quota != nil {
			legacy = ParseQuotaFraction(*acc.Quota)
		}
		if legacy != nil {
			q5 = legacy
			q7 = legacy
			legacyFractionKnown = true
		} else {
			if acc.Gemini5HPct != nil {
				pct5 := *acc.Gemini5HPct / 100.0
				q5 = ParseQuotaFraction(pct5)
				q5Known = q5 != nil
			}
			if acc.GeminiWeeklyPct != nil {
				pct7 := *acc.GeminiWeeklyPct / 100.0
				q7 = ParseQuotaFraction(pct7)
				q7Known = q7 != nil
			}
		}
	}

	var reset5, reset7 any
	if lq != nil && lq.Gemini5H != nil {
		reset5 = lq.Gemini5H.ResetTime
	}
	if lq != nil && lq.GeminiWeekly != nil {
		reset7 = lq.GeminiWeekly.ResetTime
	}

	if legacyFractionKnown && lq != nil {
		reset5 = lq.ResetTime
		reset7 = lq.ResetTime
	} else if !hasExplicitWindows {
		reset5 = acc.Gemini5HReset
		reset7 = acc.GeminiWeeklyReset
	}

	// Absolute reset timestamps win; the legacy relative *_reset_sec fields are only a fallback,
	// because "now + sec" never counts down and would override fresh quota data.
	resetRatio := func(secVal *float64, reset any, windowSecs float64) float64 {
		var rem float64
		if ts := ParseISOOrTimestamp(reset); ts != nil {
			rem = *ts - now
		} else if secVal != nil {
			rem = *secVal
		} else {
			return 1.0
		}
		if rem <= 0 {
			return 1.0
		}
		ratio := rem / windowSecs
		if ratio > 1.0 {
			return 1.0
		}
		return ratio
	}

	r5 := resetRatio(acc.Gemini5HResetSec, reset5, config.Window5HSecs)
	r7 := resetRatio(acc.GeminiWeeklyResetSec, reset7, config.Window7DSecs)

	var pace5, pace7 *float64
	var knownPaces []float64
	var knownFractions []float64

	if q5 != nil {
		p5 := *q5 - r5
		pace5 = &p5
		knownPaces = append(knownPaces, p5)
		knownFractions = append(knownFractions, *q5)
	}
	if q7 != nil {
		p7 := *q7 - r7
		pace7 = &p7
		knownPaces = append(knownPaces, p7)
		knownFractions = append(knownFractions, *q7)
	}

	knownWindowCount := 0
	if legacyFractionKnown {
		knownWindowCount = 1
	} else {
		if q5Known {
			knownWindowCount++
		}
		if q7Known {
			knownWindowCount++
		}
	}

	worstPace := math.Inf(-1)
	totalPace := math.Inf(-1)
	if len(knownPaces) > 0 {
		minP := knownPaces[0]
		sumP := 0.0
		for _, p := range knownPaces {
			if p < minP {
				minP = p
			}
			sumP += p
		}
		worstPace = minP
		totalPace = sumP
	}

	rawFloor := 0.0
	if len(knownFractions) > 0 {
		minF := knownFractions[0]
		for _, f := range knownFractions {
			if f < minF {
				minF = f
			}
		}
		rawFloor = minF
	}

	isDepleted := len(knownFractions) > 0 && rawFloor <= config.DepletedThreshold

	var reset5Time, reset7Time *time.Time

	if ep := ParseISOOrTimestamp(reset5); ep != nil {
		tm := time.Unix(int64(*ep), 0).UTC()
		reset5Time = &tm
	} else if acc.Gemini5HResetSec != nil {
		tm := time.Unix(int64(now+*acc.Gemini5HResetSec), 0).UTC()
		reset5Time = &tm
	}

	if ep := ParseISOOrTimestamp(reset7); ep != nil {
		tm := time.Unix(int64(*ep), 0).UTC()
		reset7Time = &tm
	} else if acc.GeminiWeeklyResetSec != nil {
		tm := time.Unix(int64(now+*acc.GeminiWeeklyResetSec), 0).UTC()
		reset7Time = &tm
	}

	return CapacityState{
		Q5:                  q5,
		Q7:                  q7,
		R5:                  r5,
		R7:                  r7,
		Pace5:               pace5,
		Pace7:               pace7,
		WorstPace:           worstPace,
		TotalPace:           totalPace,
		RawFloor:            rawFloor,
		Q5Known:             q5Known,
		Q7Known:             q7Known,
		KnownWindowCount:    knownWindowCount,
		LegacyFractionKnown: legacyFractionKnown,
		IsDepleted:          isDepleted,
		Reset5Time:          reset5Time,
		Reset7Time:          reset7Time,
	}
}

// NextBackoffDelay calculates the exponential backoff interval for a given retry attempt.
func NextBackoffDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(config.QuotaRefreshBackoff) {
		return config.QuotaRefreshBackoff[len(config.QuotaRefreshBackoff)-1]
	}
	return config.QuotaRefreshBackoff[attempt]
}

const (
	// BackendHost is the default Google Cloud Code PA host.
	BackendHost = "daily-cloudcode-pa.googleapis.com"

	// BackendURLBase is the default base URL for Google Cloud Code PA.
	BackendURLBase = "https://" + BackendHost

	// DefaultUA is the default user agent for proxy forwarding.
	DefaultUA = "antigravity/cli"
)

// AgyClientUA returns the User-Agent the agy CLI sends for the given agy version. Upstream reports
// Gemini quota buckets only to this versioned form; the bare DefaultUA gets the 3p buckets alone.
func AgyClientUA(version string) string {
	return fmt.Sprintf("antigravity/cli/%s (aidev_client; os_type=%s; arch=%s; auth_method=consumer)",
		version, runtime.GOOS, runtime.GOARCH)
}

// IsQuotaError reports whether an HTTP response status and body indicate quota exhaustion or rate limits.
func IsQuotaError(status int, body []byte) bool {
	if status == 429 {
		return true
	}
	if status != 403 {
		return false
	}
	lower := strings.ToLower(string(body))
	markers := []string{
		"resource_exhausted", "quota_exceeded", "rate_limit_exceeded",
		"quota exceeded", "quota exhausted", "exceeded your current quota",
		"insufficient quota",
	}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// ParseRetryAfter parses a Retry-After header string into delay seconds clamped between 5s and 86400s (24h).
func ParseRetryAfter(raw string, defaultSec int64) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultSec
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if seconds < 5 {
			return 5
		}
		if seconds > 86400 {
			return 86400
		}
		return seconds
	}
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC850, time.ANSIC} {
		if t, err := time.Parse(layout, raw); err == nil {
			seconds := int64(time.Until(t).Seconds())
			if seconds < 5 {
				return 5
			}
			if seconds > 86400 {
				return 86400
			}
			return seconds
		}
	}
	return defaultSec
}
