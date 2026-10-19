package quota

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func TestParseQuotaFraction(t *testing.T) {
	cases := []struct {
		input    any
		expected *float64
	}{
		{0.5, floatPtr(0.5)},
		{0.0, floatPtr(0.0)},
		{1.0, floatPtr(1.0)},
		{-0.1, floatPtr(0.0)},
		{1.5, floatPtr(1.0)},
		{"0.85", floatPtr(0.85)},
		{math.NaN(), nil},
		{math.Inf(1), nil},
		{math.Inf(-1), nil},
		{nil, nil},
		{"invalid", nil},
	}

	for _, c := range cases {
		res := ParseQuotaFraction(c.input)
		if c.expected == nil {
			if res != nil {
				t.Fatalf("expected nil for %v, got %v", c.input, *res)
			}
		} else {
			if res == nil || math.Abs(*res-*c.expected) > 1e-9 {
				t.Fatalf("expected %v for %v, got %v", *c.expected, c.input, res)
			}
		}
	}
}

func floatPtr(f float64) *float64 {
	return &f
}

func TestBackoffSequence(t *testing.T) {
	expected := []time.Duration{
		30 * time.Second,
		60 * time.Second,
		120 * time.Second,
		240 * time.Second,
		300 * time.Second,
		300 * time.Second,
		300 * time.Second,
	}

	for i, exp := range expected {
		actual := NextBackoffDelay(i)
		if actual != exp {
			t.Fatalf("attempt %d: expected %v, got %v", i, exp, actual)
		}
	}
}

type FixtureCapacityCase struct {
	TestID            string           `json:"test_id"`
	Account           *storage.Account `json:"account"`
	Now               float64          `json:"now"`
	ExpectedCapacity  CapacityState    `json:"expected_capacity"`
	ExpectedFreshness FreshnessInfo    `json:"expected_freshness"`
	ExpectedRank      int              `json:"expected_rank"`
	ExpectedNeeded    bool             `json:"expected_refresh_needed"`
}

func TestQuotaCapacityGoldenCases(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "testdata", "quota", "capacity_cases.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("failed to read capacity_cases.json: %v", err)
	}

	var cases []FixtureCapacityCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("failed to parse capacity_cases.json: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.TestID, func(t *testing.T) {
			actualCap := ComputeCapacityState(tc.Account, tc.Now)
			actualFresh := Freshness(tc.Account, tc.Now)
			actualRank := FreshnessRank(tc.Account, tc.Now)
			actualNeeded := RefreshNeeded(tc.Account, tc.Now)

			// Compare capacity metrics
			if actualCap.KnownWindowCount != tc.ExpectedCapacity.KnownWindowCount {
				t.Errorf("known_window_count mismatch: got %d, expected %d", actualCap.KnownWindowCount, tc.ExpectedCapacity.KnownWindowCount)
			}
			if actualCap.IsDepleted != tc.ExpectedCapacity.IsDepleted {
				t.Errorf("is_depleted mismatch: got %v, expected %v", actualCap.IsDepleted, tc.ExpectedCapacity.IsDepleted)
			}
			if math.Abs(actualCap.RawFloor-tc.ExpectedCapacity.RawFloor) > 1e-9 {
				t.Errorf("raw_floor mismatch: got %f, expected %f", actualCap.RawFloor, tc.ExpectedCapacity.RawFloor)
			}

			// Compare paces
			if math.IsInf(actualCap.WorstPace, -1) && math.IsInf(tc.ExpectedCapacity.WorstPace, -1) {
				// Both -Inf
			} else if math.Abs(actualCap.WorstPace-tc.ExpectedCapacity.WorstPace) > 1e-9 {
				t.Errorf("worst_pace mismatch: got %f, expected %f", actualCap.WorstPace, tc.ExpectedCapacity.WorstPace)
			}

			if math.IsInf(actualCap.TotalPace, -1) && math.IsInf(tc.ExpectedCapacity.TotalPace, -1) {
				// Both -Inf
			} else if math.Abs(actualCap.TotalPace-tc.ExpectedCapacity.TotalPace) > 1e-9 {
				t.Errorf("total_pace mismatch: got %f, expected %f", actualCap.TotalPace, tc.ExpectedCapacity.TotalPace)
			}

			// Compare freshness
			if actualFresh.Class != tc.ExpectedFreshness.Class {
				t.Errorf("freshness class mismatch: got %s, expected %s", actualFresh.Class, tc.ExpectedFreshness.Class)
			}
			if actualRank != tc.ExpectedRank {
				t.Errorf("freshness rank mismatch: got %d, expected %d", actualRank, tc.ExpectedRank)
			}
			if actualNeeded != tc.ExpectedNeeded {
				t.Errorf("refresh needed mismatch: got %v, expected %v", actualNeeded, tc.ExpectedNeeded)
			}
		})
	}
}

// Absolute window reset timestamps must win over the legacy relative *_reset_sec fields,
// which are never refreshed and would otherwise keep the reset "N seconds from now" forever.
func TestComputeCapacityState_AbsoluteResetBeatsLegacySeconds(t *testing.T) {
	now := 1789560000.0 // 2026-09-16T12:00:00Z
	acc := &storage.Account{
		ID:                   "acc_legacy_sec",
		Gemini5HResetSec:     floatPtr(4 * 3600), // stale: claims 4h left
		GeminiWeeklyResetSec: floatPtr(6 * 24 * 3600),
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.5), ResetTime: "2026-09-16T12:30:00Z"},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.5), ResetTime: "2026-09-17T12:00:00Z"},
		},
	}
	c := ComputeCapacityState(acc, now)
	if want := time.Date(2026, 9, 16, 12, 30, 0, 0, time.UTC); c.Reset5Time == nil || !c.Reset5Time.Equal(want) {
		t.Errorf("Reset5Time = %v, want %v", c.Reset5Time, want)
	}
	if want := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC); c.Reset7Time == nil || !c.Reset7Time.Equal(want) {
		t.Errorf("Reset7Time = %v, want %v", c.Reset7Time, want)
	}
	if want := 1800.0 / 18000.0; math.Abs(c.R5-want) > 1e-9 {
		t.Errorf("R5 = %v, want %v", c.R5, want)
	}

	// Without an absolute timestamp, the legacy seconds field is still used.
	acc.LastQuota.Gemini5H.ResetTime = ""
	c = ComputeCapacityState(acc, now)
	if want := time.Unix(int64(now)+4*3600, 0).UTC(); c.Reset5Time == nil || !c.Reset5Time.Equal(want) {
		t.Errorf("fallback Reset5Time = %v, want %v", c.Reset5Time, want)
	}
}

func TestComputeCapacityState_MissingResetAndWeeklyOnly(t *testing.T) {
	now := 1789560000.0 // 2026-09-16T12:00:00Z

	// Fraction known but no reset time: treat the window as just started (r=1), never as resetting soon.
	noReset := &storage.Account{LastQuota: &storage.QuotaState{
		Gemini5H: &storage.QuotaWindow{Fraction: floatPtr(0.6)},
	}}
	c := ComputeCapacityState(noReset, now)
	if c.R5 != 1.0 || c.Reset5Time != nil || c.Pace5 == nil || math.Abs(*c.Pace5-(-0.4)) > 1e-9 {
		t.Errorf("missing reset: R5=%v Reset5Time=%v Pace5=%v, want R5=1 no reset pace=-0.4", c.R5, c.Reset5Time, c.Pace5)
	}

	// Only the weekly window reported: one known window, 5H unknown rather than zero.
	weeklyOnly := &storage.Account{LastQuota: &storage.QuotaState{
		GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.3), ResetTime: "2026-09-19T12:00:00Z"},
	}}
	c = ComputeCapacityState(weeklyOnly, now)
	if c.KnownWindowCount != 1 || c.Q5Known || !c.Q7Known || c.IsDepleted || math.Abs(c.RawFloor-0.3) > 1e-9 {
		t.Errorf("weekly only: known=%d q5Known=%v q7Known=%v depleted=%v floor=%v", c.KnownWindowCount, c.Q5Known, c.Q7Known, c.IsDepleted, c.RawFloor)
	}
}

func TestFreshness_ResetPassedOnlyAfterSnapshotIsStale(t *testing.T) {
	now := float64(time.Now().Unix())
	at := func(offset float64) string { return time.Unix(int64(now+offset), 0).UTC().Format(time.RFC3339) }
	acc := func(updatedAgo, weeklyReset float64) *storage.Account {
		return &storage.Account{LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.9), ResetTime: at(3600)},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.8), ResetTime: at(weeklyReset)},
			UpdatedAt:    int64Ptr(int64(now - updatedAgo)),
		}}
	}

	// Refreshed 10s ago, yet upstream still reports a weekly reset from an hour ago: not renewed, not stale.
	notRenewed := acc(10, -3600)
	if c := Freshness(notRenewed, now).Class; c != "fresh" {
		t.Errorf("reset already past at snapshot: class = %s, want fresh", c)
	}
	if !GeminiQuotaNotRenewed(notRenewed) {
		t.Errorf("expected GeminiQuotaNotRenewed for a window whose reset predates the refresh")
	}

	// Snapshot taken before a reset that has since passed: the data is outdated, so stale.
	if c := Freshness(acc(30, -10), now).Class; c != "stale" {
		t.Errorf("reset passed after snapshot: class = %s, want stale", c)
	}

	healthy := acc(10, 86400)
	if GeminiQuotaNotRenewed(healthy) {
		t.Errorf("did not expect GeminiQuotaNotRenewed for future resets")
	}
	healthy.LastQuota.GeminiWeekly = nil
	if !GeminiQuotaNotRenewed(healthy) {
		t.Errorf("expected GeminiQuotaNotRenewed when a Gemini window is missing")
	}
}

func TestAgyClientUA(t *testing.T) {
	want := "antigravity/cli/1.4.2 (aidev_client; os_type=" + runtime.GOOS + "; arch=" + runtime.GOARCH + "; auth_method=consumer)"
	if got := AgyClientUA("1.4.2"); got != want {
		t.Fatalf("AgyClientUA = %q, want %q", got, want)
	}
}
