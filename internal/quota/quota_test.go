package quota

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
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
