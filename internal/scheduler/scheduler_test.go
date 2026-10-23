package scheduler

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/observability"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

type FixtureOrderingCase struct {
	TestID             string             `json:"test_id"`
	Strategy           string             `json:"strategy"`
	Now                float64            `json:"now"`
	Pool               *storage.Pool      `json:"pool"`
	Candidates         []*storage.Account `json:"candidates"`
	ExpectedOrderedIDs []string           `json:"expected_ordered_ids"`
}

func TestSchedulerOrderingGoldenCases(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "testdata", "scheduler", "ordering_cases.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("failed to read ordering_cases.json: %v", err)
	}

	var cases []FixtureOrderingCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("failed to parse ordering_cases.json: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.TestID, func(t *testing.T) {
			ordered := OrderCandidates(tc.Candidates, tc.Strategy, tc.Pool, tc.Now)
			var actualIDs []string
			for _, a := range ordered {
				actualIDs = append(actualIDs, a.ID)
			}

			if !reflect.DeepEqual(actualIDs, tc.ExpectedOrderedIDs) {
				t.Fatalf("ordering mismatch:\nGot:      %v\nExpected: %v", actualIDs, tc.ExpectedOrderedIDs)
			}
		})
	}
}

func setupTestStateDir(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	if err := config.ConfigureStateDir(tmpDir); err != nil {
		t.Fatalf("failed to configure state dir: %v", err)
	}
	t.Cleanup(config.ResetDataDir)
	return tmpDir
}

func floatPtr(f float64) *float64 {
	return &f
}

func int64Ptr(i int64) *int64 {
	return &i
}

func TestSchedulerSnapshotConsistent(t *testing.T) {
	setupTestStateDir(t)

	now := 1789560000.0
	p := storage.NewEmptyPool()
	p.Strategy = "max_quota"
	p.Accounts = append(p.Accounts, &storage.Account{
		ID:           "acc_1",
		Email:        "snap@example.com",
		RequestCount: 15,
		Status:       "active",
		LastQuota: &storage.QuotaState{
			RemainingFraction: floatPtr(0.75),
			UpdatedAt:         int64Ptr(int64(now - 30)),
		},
	})
	if err := storage.SavePool(p); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	snap, err := GetSchedulerSnapshot()
	if err != nil {
		t.Fatalf("GetSchedulerSnapshot failed: %v", err)
	}
	if snap == nil || len(snap.Accounts) != 1 {
		t.Fatalf("invalid snapshot: %+v", snap)
	}
	acc := snap.Accounts[0]
	if acc.ID != "acc_1" || acc.RequestCount != 15 || acc.LastQuota == nil || *acc.LastQuota.RemainingFraction != 0.75 {
		t.Fatalf("snapshot data inconsistent: %+v", acc)
	}
}

func makeAccountWithWindows(id string, q5, qw *float64, now float64) *storage.Account {
	lq := &storage.QuotaState{
		UpdatedAt: int64Ptr(int64(now - 10)),
	}
	if q5 != nil {
		lq.Gemini5H = &storage.QuotaWindow{
			Fraction:  q5,
			ResetTime: "2026-09-17T20:00:00Z",
		}
	}
	if qw != nil {
		lq.GeminiWeekly = &storage.QuotaWindow{
			Fraction:  qw,
			ResetTime: "2026-09-24T00:00:00Z",
		}
	}
	return &storage.Account{
		ID:        id,
		Status:    "active",
		LastQuota: lq,
	}
}

// Test A: Healthy beats Reserve
func TestMaxQuota_ReserveHeadroom_HealthyBeatsReserve(t *testing.T) {
	now := 1789560000.0
	mainAcc := makeAccountWithWindows("Main", floatPtr(0.10), floatPtr(0.50), now)
	backupAcc := makeAccountWithWindows("Backup", floatPtr(0.80), floatPtr(0.50), now)

	ordered := OrderCandidates([]*storage.Account{mainAcc, backupAcc}, config.StrategyMaxQuota, nil, now)
	if len(ordered) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(ordered))
	}
	if ordered[0].ID != "Backup" || ordered[1].ID != "Main" {
		t.Fatalf("expected [Backup, Main], got [%s, %s]", ordered[0].ID, ordered[1].ID)
	}
}

// Test B: Existing pace ranking preserved among Healthy
func TestMaxQuota_ReserveHeadroom_PaceRankingPreservedAmongHealthy(t *testing.T) {
	now := 1789560000.0
	acc1 := makeAccountWithWindows("acc_1", floatPtr(0.80), floatPtr(0.90), now)
	acc2 := makeAccountWithWindows("acc_2", floatPtr(0.70), floatPtr(0.85), now)

	ordered := OrderCandidates([]*storage.Account{acc2, acc1}, config.StrategyMaxQuota, nil, now)
	if len(ordered) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(ordered))
	}
	// acc1 has higher fractions and better pace, so acc1 must beat acc2
	if ordered[0].ID != "acc_1" || ordered[1].ID != "acc_2" {
		t.Fatalf("expected [acc_1, acc_2], got [%s, %s]", ordered[0].ID, ordered[1].ID)
	}
}

// Test C: Reserve fallback works when all candidates are below reserve threshold
func TestMaxQuota_ReserveHeadroom_ReserveFallbackWorks(t *testing.T) {
	now := 1789560000.0
	r1 := makeAccountWithWindows("res_1", floatPtr(0.12), floatPtr(0.80), now)
	r2 := makeAccountWithWindows("res_2", floatPtr(0.08), floatPtr(0.80), now)

	ordered := OrderCandidates([]*storage.Account{r2, r1}, config.StrategyMaxQuota, nil, now)
	if len(ordered) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(ordered))
	}
	// Both are Reserve; r1 has higher 5H fraction than r2, so r1 wins inside Reserve
	if ordered[0].ID != "res_1" || ordered[1].ID != "res_2" {
		t.Fatalf("expected [res_1, res_2], got [%s, %s]", ordered[0].ID, ordered[1].ID)
	}
}

// Test D: Depleted never wins over Reserve
func TestMaxQuota_ReserveHeadroom_DepletedNeverWinsOverReserve(t *testing.T) {
	now := 1789560000.0
	depAcc := makeAccountWithWindows("acc_depleted", floatPtr(0.004), floatPtr(0.50), now)
	resAcc := makeAccountWithWindows("acc_reserve", floatPtr(0.10), floatPtr(0.50), now)

	ordered := OrderCandidates([]*storage.Account{depAcc, resAcc}, config.StrategyMaxQuota, nil, now)
	if len(ordered) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(ordered))
	}
	if ordered[0].ID != "acc_reserve" || ordered[1].ID != "acc_depleted" {
		t.Fatalf("expected [acc_reserve, acc_depleted], got [%s, %s]", ordered[0].ID, ordered[1].ID)
	}
}

// Test E: Weekly protection: 5H high, weekly <= 10% -> Reserve
func TestMaxQuota_ReserveHeadroom_WeeklyProtection(t *testing.T) {
	now := 1789560000.0
	accWeeklyLow := makeAccountWithWindows("weekly_low", floatPtr(0.80), floatPtr(0.08), now)
	accHealthy := makeAccountWithWindows("healthy", floatPtr(0.60), floatPtr(0.50), now)

	if !IsReserveCandidate(accWeeklyLow, now) {
		t.Fatalf("expected accWeeklyLow to be classified as Reserve")
	}
	if IsReserveCandidate(accHealthy, now) {
		t.Fatalf("expected accHealthy to NOT be classified as Reserve")
	}

	ordered := OrderCandidates([]*storage.Account{accWeeklyLow, accHealthy}, config.StrategyMaxQuota, nil, now)
	if ordered[0].ID != "healthy" || ordered[1].ID != "weekly_low" {
		t.Fatalf("expected [healthy, weekly_low], got [%s, %s]", ordered[0].ID, ordered[1].ID)
	}
}

// Test DynamicWeeklyReserveFloor: when weekly reset is near (e.g. 22 hours away),
// 9% weekly quota is NOT reserve (it is healthy and drained), but drops to reserve below the dynamic floor.
func TestMaxQuota_DynamicWeeklyReserveFloor_NearReset(t *testing.T) {
	now := 1789560000.0
	// 22 hours remaining out of 7 days
	nearResetTime := now + 22*3600

	makeNearResetAcc := func(id string, q5, qw float64) *storage.Account {
		lq := &storage.QuotaState{
			UpdatedAt: int64Ptr(int64(now - 10)),
			Gemini5H: &storage.QuotaWindow{
				Fraction:  &q5,
				ResetTime: "2026-09-16T17:00:00Z", // 5h reset
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  &qw,
				ResetTime: nearResetTime,
			},
		}
		return &storage.Account{
			ID:        id,
			Status:    "active",
			LastQuota: lq,
		}
	}

	// Backup: 5H 100%, Weekly 9%, Weekly resets in 22h -> Healthy (not reserve)!
	backupAcc := makeNearResetAcc("backup", 1.0, 0.09)
	if IsReserveCandidate(backupAcc, now) {
		t.Fatalf("Backup (5H 100%%, WK 9%% with 22h to reset) should NOT be Reserve, dynamic floor must allow draining")
	}

	// If Weekly drops below dynamic floor (~1.3%), it enters reserve
	criticalAcc := makeNearResetAcc("critical", 1.0, 0.01)
	if !IsReserveCandidate(criticalAcc, now) {
		t.Fatalf("Critical (WK 1%% <= dynamic floor ~1.3%%) should be Reserve")
	}

	// Schedulers orders Backup as Healthy along with other healthy accounts
	otherHealthy := makeNearResetAcc("work", 0.80, 0.20)
	ordered := OrderCandidates([]*storage.Account{backupAcc, otherHealthy}, config.StrategyMaxQuota, nil, now)
	if len(ordered) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(ordered))
	}
	// Both are eligible and healthy
	for _, a := range ordered {
		if IsReserveCandidate(a, now) {
			t.Errorf("candidate %s should not be reserve", a.ID)
		}
	}
}

// Test F: 5H protection: 5H <= 15%, weekly healthy -> Reserve
func TestMaxQuota_ReserveHeadroom_FiveHourProtection(t *testing.T) {
	now := 1789560000.0
	acc5HLow := makeAccountWithWindows("5h_low", floatPtr(0.12), floatPtr(0.70), now)
	accHealthy := makeAccountWithWindows("healthy", floatPtr(0.60), floatPtr(0.70), now)

	if !IsReserveCandidate(acc5HLow, now) {
		t.Fatalf("expected acc5HLow to be classified as Reserve")
	}
	if IsReserveCandidate(accHealthy, now) {
		t.Fatalf("expected accHealthy to NOT be classified as Reserve")
	}

	ordered := OrderCandidates([]*storage.Account{acc5HLow, accHealthy}, config.StrategyMaxQuota, nil, now)
	if ordered[0].ID != "healthy" || ordered[1].ID != "5h_low" {
		t.Fatalf("expected [healthy, 5h_low], got [%s, %s]", ordered[0].ID, ordered[1].ID)
	}
}

// Test G, H, I, J: Exact boundary conditions
func TestMaxQuota_ReserveHeadroom_Boundaries(t *testing.T) {
	now := 1789560000.0

	// G. Boundary: exactly 15% 5H -> Reserve
	accExact15 := makeAccountWithWindows("exact_15", floatPtr(0.15), floatPtr(0.70), now)
	if !IsReserveCandidate(accExact15, now) {
		t.Fatalf("G failed: exactly 0.15 5H should be Reserve")
	}

	// H. Boundary: just above 15% 5H -> Healthy
	accAbove15 := makeAccountWithWindows("above_15", floatPtr(0.150001), floatPtr(0.70), now)
	if IsReserveCandidate(accAbove15, now) {
		t.Fatalf("H failed: 0.150001 5H should be Healthy")
	}

	// I. Boundary: exactly 10% weekly -> Reserve
	accExact10 := makeAccountWithWindows("exact_10", floatPtr(0.70), floatPtr(0.10), now)
	if !IsReserveCandidate(accExact10, now) {
		t.Fatalf("I failed: exactly 0.10 weekly should be Reserve")
	}

	// J. Boundary: just above 10% weekly -> Healthy
	accAbove10 := makeAccountWithWindows("above_10", floatPtr(0.70), floatPtr(0.100001), now)
	if IsReserveCandidate(accAbove10, now) {
		t.Fatalf("J failed: 0.100001 weekly should be Healthy")
	}
}

// Test K: Unknown/missing window not automatically treated as zero/reserve
func TestMaxQuota_ReserveHeadroom_UnknownMissingWindow(t *testing.T) {
	now := 1789560000.0

	// Only 5H known and healthy; weekly is nil
	accOnly5H := makeAccountWithWindows("only_5h", floatPtr(0.80), nil, now)
	if IsReserveCandidate(accOnly5H, now) {
		t.Fatalf("missing weekly window should not trigger Reserve")
	}

	// Only weekly known and healthy; 5H is nil
	accOnlyWeekly := makeAccountWithWindows("only_weekly", nil, floatPtr(0.80), now)
	if IsReserveCandidate(accOnlyWeekly, now) {
		t.Fatalf("missing 5H window should not trigger Reserve")
	}

	// Neither window known
	accNoWindows := &storage.Account{ID: "no_windows", Status: "active"}
	if IsReserveCandidate(accNoWindows, now) {
		t.Fatalf("completely unknown account should not trigger Reserve")
	}
}

// Test L: restricted accounts are excluded under every strategy
func TestRestrictedAccountsExcludedFromAllStrategies(t *testing.T) {
	now := float64(1000)
	ready := &storage.Account{ID: "ready", LastQuota: &storage.QuotaState{RemainingFraction: floatPtr(0.9)}}
	reserve := &storage.Account{ID: "reserve", LastQuota: &storage.QuotaState{RemainingFraction: floatPtr(0.1)}}
	depleted := &storage.Account{ID: "depleted", LastQuota: &storage.QuotaState{RemainingFraction: floatPtr(0)}}
	cooldownUntil := now + 100
	cooldown := &storage.Account{ID: "cooldown", RateLimitedUntil: &cooldownUntil, LastQuota: &storage.QuotaState{RemainingFraction: floatPtr(0.9)}}
	validation := &storage.Account{ID: "validation", Status: "validation_required", LastQuota: &storage.QuotaState{RemainingFraction: floatPtr(1)}}
	authError := &storage.Account{ID: "auth", Status: "auth_error", LastQuota: &storage.QuotaState{RemainingFraction: floatPtr(1)}}
	disabled := &storage.Account{ID: "disabled", Status: "disabled", LastQuota: &storage.QuotaState{RemainingFraction: floatPtr(1)}}
	all := []*storage.Account{ready, reserve, depleted, cooldown, validation, authError, disabled}

	for _, strategy := range []string{config.StrategyMaxQuota, config.StrategySmart} {
		t.Run(strategy, func(t *testing.T) {
			pool := &storage.Pool{}
			ordered := OrderCandidates(all, strategy, pool, now)
			for _, acc := range ordered {
				if IsRestricted(acc) {
					t.Fatalf("restricted account %q returned for %s", acc.ID, strategy)
				}
			}

			eligible := []*storage.Account{ready, reserve, depleted, cooldown}
			want := OrderCandidates(eligible, strategy, pool, now)
			if len(ordered) != len(want) {
				t.Fatalf("got %d candidates, want %d", len(ordered), len(want))
			}
			for i := range want {
				if ordered[i].ID != want[i].ID {
					t.Fatalf("candidate %d = %q, want %q", i, ordered[i].ID, want[i].ID)
				}
			}
		})
	}
}

func TestReserveHeadroom_HealthyPreferredOverReserve(t *testing.T) {
	now := 1789560000.0

	// Main has low quota (Reserve) but low request count
	mainAcc := makeAccountWithWindows("Main", floatPtr(0.10), floatPtr(0.50), now)
	mainAcc.RequestCount = 2

	// Backup has high quota (Healthy) but higher request count
	backupAcc := makeAccountWithWindows("Backup", floatPtr(0.80), floatPtr(0.50), now)
	backupAcc.RequestCount = 20

	// Under max_quota: Backup wins because Main is in Reserve
	orderedMQ := OrderCandidates([]*storage.Account{mainAcc, backupAcc}, config.StrategyMaxQuota, nil, now)
	if orderedMQ[0].ID != "Backup" {
		t.Fatalf("max_quota expected Backup first, got %s", orderedMQ[0].ID)
	}

	// Under smart: Backup wins because Main is in Reserve and healthy accounts are strictly preferred
	orderedSmart := OrderCandidates([]*storage.Account{mainAcc, backupAcc}, config.StrategySmart, nil, now)
	if orderedSmart[0].ID != "Backup" {
		t.Fatalf("smart expected Backup first, got %s", orderedSmart[0].ID)
	}
}

// Section 13: Real observed production snapshot test
func TestMaxQuota_ReserveHeadroom_RealObservedSnapshot(t *testing.T) {
	now := 1789560000.0

	// Observed production shape:
	// Main: 5H 0.6%, Weekly 49.2%
	// Backup: 5H 100%, Weekly 67.1%
	// Work: 5H 99.9%, Weekly 67.1%
	mainAcc := makeAccountWithWindows("Main", floatPtr(0.006), floatPtr(0.492), now)
	backupAcc := makeAccountWithWindows("Backup", floatPtr(1.0), floatPtr(0.671), now)
	workAcc := makeAccountWithWindows("Work", floatPtr(0.999), floatPtr(0.671), now)

	// Verify classifications
	if !IsReserveCandidate(mainAcc, now) {
		t.Fatalf("Main (5H 0.6%%) should be Reserve")
	}
	if IsReserveCandidate(backupAcc, now) {
		t.Fatalf("Backup (5H 100%%) should be Healthy")
	}
	if IsReserveCandidate(workAcc, now) {
		t.Fatalf("Work (5H 99.9%%) should be Healthy")
	}

	ordered := OrderCandidates([]*storage.Account{mainAcc, backupAcc, workAcc}, config.StrategyMaxQuota, nil, now)
	if len(ordered) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(ordered))
	}

	// Main must NOT appear before Backup or Work
	if ordered[0].ID == "Main" || ordered[1].ID == "Main" {
		t.Fatalf("Main must not appear before Healthy candidates; got order: [%s, %s, %s]",
			ordered[0].ID, ordered[1].ID, ordered[2].ID)
	}
	if ordered[2].ID != "Main" {
		t.Fatalf("expected Main to be at index 2 (Reserve), got %s", ordered[2].ID)
	}

	// Deterministic pace comparison: Backup (1.0) has higher total pace than Work (0.999)
	if ordered[0].ID != "Backup" || ordered[1].ID != "Work" {
		t.Fatalf("expected [Backup, Work, Main], got [%s, %s, %s]",
			ordered[0].ID, ordered[1].ID, ordered[2].ID)
	}
}

// Legacy fractions have no window identity, so only depletion applies to them.
func TestMaxQuota_ReserveHeadroom_LegacyFraction(t *testing.T) {
	now := 1789560000.0
	for _, source := range []string{"remaining_fraction", "quota"} {
		t.Run(source, func(t *testing.T) {
			for _, fraction := range []float64{0.005, 0.006, 0.08, 0.10, 0.12, 0.15, 0.50} {
				legacy := &storage.Account{
					ID:        "legacy",
					LastQuota: &storage.QuotaState{UpdatedAt: int64Ptr(int64(now - 10))},
				}
				if source == "remaining_fraction" {
					legacy.LastQuota.RemainingFraction = floatPtr(fraction)
				} else {
					legacy.Quota = floatPtr(fraction)
				}
				if IsReserveCandidate(legacy, now) {
					t.Fatalf("legacy %g has no identified window and must not be Reserve", fraction)
				}
				capacity := quota.ComputeCapacityState(legacy, now)
				depleted := fraction <= config.DepletedThreshold
				if capacity.KnownWindowCount != 1 || capacity.IsDepleted != depleted {
					t.Fatalf("legacy %g lost existing capacity semantics: %+v", fraction, capacity)
				}

				reserve := makeAccountWithWindows("reserve", floatPtr(0.12), floatPtr(0.80), now)
				ordered := OrderCandidates([]*storage.Account{reserve, legacy}, config.StrategyMaxQuota, nil, now)
				wantFirst := legacy
				if depleted {
					wantFirst = reserve
				}
				if len(ordered) != 2 || ordered[0] != wantFirst {
					t.Fatalf("legacy %g: expected %s first, got %v", fraction, wantFirst.ID, ordered)
				}
			}
		})
	}

	// The same fraction has different reserve meaning in identified windows.
	fiveHour := makeAccountWithWindows("5h", floatPtr(0.12), nil, now)
	weekly := makeAccountWithWindows("weekly", nil, floatPtr(0.12), now)
	if !IsReserveCandidate(fiveHour, now) || IsReserveCandidate(weekly, now) {
		t.Fatal("12% must be Reserve for explicit 5H, but Healthy for explicit Weekly")
	}
}

// TestMaxQuota_AbundantWeeklyQuota_NoColdStartStarvation verifies that a newly active account
// with abundant weekly quota (>= 80%) is not starved by an early negative weekly pace
// when its 5H quota and available capacity are superior to mid-cycle accounts.
func TestMaxQuota_AbundantWeeklyQuota_NoColdStartStarvation(t *testing.T) {
	now := 1789560000.0

	// freshAcc: newly reset cycle (7 days left), 99% 5H, 99% weekly.
	// Raw weekly pace is -0.009 (because r7 ≈ 0.999), but 5H pace is +0.026.
	freshAcc := &storage.Account{
		ID:     "fresh_acc_4",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.99),
				ResetTime: "2026-09-16T20:50:00Z", // ~4.8h remaining (r5 ≈ 0.96)
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.99),
				ResetTime: "2026-09-23T16:00:00Z", // ~7d remaining (r7 ≈ 0.999)
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}

	// midCycleAcc: 3.8 days into week, 97% 5H, 63% weekly.
	// Mid-cycle surplus gives raw weekly pace +0.24, but lower overall quota.
	midCycleAcc := &storage.Account{
		ID:     "mid_cycle_acc_2",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.97),
				ResetTime: "2026-09-16T20:45:00Z", // ~4.75h remaining (r5 ≈ 0.95)
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.63),
				ResetTime: "2026-09-20T11:00:00Z", // ~3.8d remaining (r7 ≈ 0.54)
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}

	ordered := OrderCandidates([]*storage.Account{midCycleAcc, freshAcc}, config.StrategyMaxQuota, nil, now)
	if len(ordered) != 2 || ordered[0].ID != "fresh_acc_4" {
		t.Fatalf("expected fresh account with 99%% quota to be ordered first, got %s", ordered[0].ID)
	}
}

func TestSmartStrategy_RenewalOrder(t *testing.T) {
	now := 1789560000.0 // 2026-09-16T12:00:00Z
	accA := &storage.Account{
		ID:     "acc_soon",
		Status: "active",
		LastQuota: &storage.QuotaState{
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.50),
				ResetTime: "2026-09-17T18:00:00Z", // renews in ~30 hours
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}
	accB := &storage.Account{
		ID:     "acc_later",
		Status: "active",
		LastQuota: &storage.QuotaState{
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.90),
				ResetTime: "2026-09-21T18:00:00Z", // renews in ~5 days
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}

	ordered := OrderCandidates([]*storage.Account{accB, accA}, config.StrategySmart, nil, now)
	if len(ordered) != 2 || ordered[0].ID != "acc_soon" || ordered[1].ID != "acc_later" {
		t.Fatalf("expected [acc_soon, acc_later], got [%s, %s]", ordered[0].ID, ordered[1].ID)
	}
}

func TestSmartStrategy_HourTruncation(t *testing.T) {
	now := 1789560000.0
	// Both accounts renew in the same hour (10:xx).
	acc1 := &storage.Account{
		ID:           "acc_hits5",
		Status:       "active",
		RequestCount: 5,
		LastQuota: &storage.QuotaState{
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.70),
				ResetTime: "2026-09-18T10:15:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}
	acc2 := &storage.Account{
		ID:           "acc_hits2",
		Status:       "active",
		RequestCount: 2,
		LastQuota: &storage.QuotaState{
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.70),
				ResetTime: "2026-09-18T10:45:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}

	ordered := OrderCandidates([]*storage.Account{acc1, acc2}, config.StrategySmart, nil, now)
	if len(ordered) != 2 || ordered[0].ID != "acc_hits2" || ordered[1].ID != "acc_hits5" {
		t.Fatalf("expected [acc_hits2, acc_hits5] due to hour truncation and lower hits, got [%s, %s]",
			ordered[0].ID, ordered[1].ID)
	}
}

func TestSmartStrategy_HierarchicalWindows(t *testing.T) {
	now := 1789560000.0
	// Weekly renews in the same hour (10:xx), but acc2's 5H resets sooner (12:00 vs 15:00).
	acc1 := &storage.Account{
		ID:     "acc_5h_later",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.80),
				ResetTime: "2026-09-17T15:00:00Z",
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.70),
				ResetTime: "2026-09-18T10:10:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}
	acc2 := &storage.Account{
		ID:     "acc_5h_sooner",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.80),
				ResetTime: "2026-09-17T12:00:00Z",
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.70),
				ResetTime: "2026-09-18T10:50:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}

	ordered := OrderCandidates([]*storage.Account{acc1, acc2}, config.StrategySmart, nil, now)
	if len(ordered) != 2 || ordered[0].ID != "acc_5h_sooner" || ordered[1].ID != "acc_5h_later" {
		t.Fatalf("expected [acc_5h_sooner, acc_5h_later], got [%s, %s]", ordered[0].ID, ordered[1].ID)
	}
}

func TestSmartStrategy_FiveHourOnlyVsWeekly(t *testing.T) {
	now := 1789560000.0
	// 5H-only account renewing in 3 hours vs Weekly account renewing in 3 days.
	acc5HOnly := &storage.Account{
		ID:     "acc_5h_only",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.80),
				ResetTime: "2026-09-16T15:00:00Z", // renews in 3 hours
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}
	accWeekly := &storage.Account{
		ID:     "acc_weekly",
		Status: "active",
		LastQuota: &storage.QuotaState{
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.80),
				ResetTime: "2026-09-19T10:00:00Z", // renews in ~3 days
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}

	ordered := OrderCandidates([]*storage.Account{accWeekly, acc5HOnly}, config.StrategySmart, nil, now)
	if len(ordered) != 2 || ordered[0].ID != "acc_5h_only" || ordered[1].ID != "acc_weekly" {
		t.Fatalf("expected [acc_5h_only, acc_weekly], got [%s, %s]", ordered[0].ID, ordered[1].ID)
	}
}

func TestSmartStrategy_ReserveDeprioritization(t *testing.T) {
	now := 1789560000.0
	// Reserve account (5H at 10% <= 15% reserve floor) renewing in 1 hour vs Healthy account renewing in 2 days.
	accReserve := &storage.Account{
		ID:     "acc_reserve",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.10), // <= FiveHourReserveFloor (0.15)
				ResetTime: "2026-09-16T13:00:00Z",
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.50),
				ResetTime: "2026-09-16T13:00:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}
	accHealthy := &storage.Account{
		ID:     "acc_healthy",
		Status: "active",
		LastQuota: &storage.QuotaState{
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.80),
				ResetTime: "2026-09-18T12:00:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}

	ordered := OrderCandidates([]*storage.Account{accReserve, accHealthy}, config.StrategySmart, nil, now)
	if len(ordered) != 2 || ordered[0].ID != "acc_healthy" || ordered[1].ID != "acc_reserve" {
		t.Fatalf("expected [acc_healthy, acc_reserve], got [%s, %s]", ordered[0].ID, ordered[1].ID)
	}
}

func TestSmartStrategy_CooldownSink(t *testing.T) {
	now := 1789560000.0
	accHealthy := &storage.Account{
		ID:     "healthy",
		Status: "active",
		LastQuota: &storage.QuotaState{
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.80),
				ResetTime: "2026-09-18T12:00:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}
	accDepleted := &storage.Account{
		ID:     "depleted",
		Status: "active",
		LastQuota: &storage.QuotaState{
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.001),
				ResetTime: "2026-09-17T12:00:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}
	cooldownUntil := now + 120
	accCooldown := &storage.Account{
		ID:               "cooldown",
		Status:           "active",
		RateLimitedUntil: &cooldownUntil,
		LastQuota: &storage.QuotaState{
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.95),
				ResetTime: "2026-09-16T13:00:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}

	ordered := OrderCandidates([]*storage.Account{accCooldown, accDepleted, accHealthy}, config.StrategySmart, nil, now)
	if len(ordered) != 3 || ordered[0].ID != "healthy" || ordered[1].ID != "depleted" || ordered[2].ID != "cooldown" {
		t.Fatalf("expected [healthy, depleted, cooldown], got [%s, %s, %s]", ordered[0].ID, ordered[1].ID, ordered[2].ID)
	}
}

func TestSmartStrategy_UserObservedSingleAccountDrainPrevented(t *testing.T) {
	now := 1789560000.0 // 2026-09-16T12:00:00Z

	// Real production bug snapshot:
	// Backup had sooner weekly reset (in 2 days) but only 23% weekly quota and 42% 5H quota.
	// Main, Work, Life had later weekly resets (in 6-7 days) but 97-99% quota.
	// The old smart algorithm hammered Backup because its weekly reset was sooner.
	// The redesigned smart algorithm MUST protect Backup and dispatch healthy high-headroom accounts!
	backup := &storage.Account{
		ID:     "Backup",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.42),
				ResetTime: "2026-09-16T12:34:00Z", // resets in 34m
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.23),         // constrained weekly budget!
				ResetTime: "2026-09-18T13:00:00Z", // resets in ~2 days
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}
	mainAcc := &storage.Account{
		ID:     "Main",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.97),
				ResetTime: "2026-09-16T16:54:00Z", // resets in ~5h
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.99),
				ResetTime: "2026-09-23T11:00:00Z", // resets in ~7 days
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}

	ordered := OrderCandidates([]*storage.Account{backup, mainAcc}, config.StrategySmart, nil, now)
	if len(ordered) != 2 || ordered[0].ID != "Main" {
		t.Fatalf("redesigned smart MUST prioritize Main (97%% headroom) over Backup (23%% headroom), got %s first", ordered[0].ID)
	}
}

func TestSmartStrategy_ExpiringQuotaHarvesting(t *testing.T) {
	now := 1789560000.0 // 2026-09-16T12:00:00Z

	// Account A has 80% 5H quota and resets in 25 minutes (expiring quota).
	// Account B has 95% 5H quota and resets in 4.5 hours.
	// Redesigned smart should harvest Account A before its reset so quota isn't wasted!
	accExpiring := &storage.Account{
		ID:     "acc_expiring",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.80),
				ResetTime: "2026-09-16T12:25:00Z", // resets in 25m <= 90m
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.90),
				ResetTime: "2026-09-22T12:00:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}
	accCruising := &storage.Account{
		ID:     "acc_cruising",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.95),
				ResetTime: "2026-09-16T16:30:00Z", // resets in 4.5h > 90m
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.95),
				ResetTime: "2026-09-22T12:00:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}

	ordered := OrderCandidates([]*storage.Account{accCruising, accExpiring}, config.StrategySmart, nil, now)
	if len(ordered) != 2 || ordered[0].ID != "acc_expiring" {
		t.Fatalf("expected expiring account to be harvested first, got %s", ordered[0].ID)
	}
}

func TestSmartStrategy_LowQuotaAccountProtectedFromHarvest(t *testing.T) {
	now := 1789560000.0 // 2026-09-16T12:00:00Z

	// Account Low has only 20% 5H quota and resets in 80 minutes.
	// The harvest floor at 80m is 25% * 80/90 ≈ 22%, so it is not yet harvested and must NOT be drained into 429!
	// Account Healthy (90% quota) should be chosen instead.
	accLow := &storage.Account{
		ID:     "acc_low",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.20), // below the 80m harvest floor
				ResetTime: "2026-09-16T13:20:00Z",
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.90),
				ResetTime: "2026-09-22T12:00:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}
	accHealthy := &storage.Account{
		ID:     "acc_healthy",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.90),
				ResetTime: "2026-09-16T16:30:00Z",
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.90),
				ResetTime: "2026-09-22T12:00:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}

	ordered := OrderCandidates([]*storage.Account{accLow, accHealthy}, config.StrategySmart, nil, now)
	if len(ordered) != 2 || ordered[0].ID != "acc_healthy" {
		t.Fatalf("expected healthy account to be preferred over low account, got %s", ordered[0].ID)
	}
}

func TestSmartStrategy_HarvestSurplusAbove25Percent(t *testing.T) {
	now := 1789560000.0 // 2026-09-16T12:00:00Z

	// Account Harvest has 27% headroom (>= 25%) and resets in 25 minutes.
	// It should qualify for harvest and take priority over a cruising account (90% quota, 4.5h reset).
	accHarvest := &storage.Account{
		ID:     "acc_harvest_27pct",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.27),
				ResetTime: "2026-09-16T12:25:00Z",
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.90),
				ResetTime: "2026-09-22T12:00:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}
	accCruising := &storage.Account{
		ID:     "acc_cruising",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.90),
				ResetTime: "2026-09-16T16:30:00Z",
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.90),
				ResetTime: "2026-09-22T12:00:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}

	ordered := OrderCandidates([]*storage.Account{accCruising, accHarvest}, config.StrategySmart, nil, now)
	if len(ordered) != 2 || ordered[0].ID != "acc_harvest_27pct" {
		t.Fatalf("expected 27%% harvest account to be preferred before reset, got %s", ordered[0].ID)
	}
}

func TestSmartStrategy_BalancedCruisingLoadDistribution(t *testing.T) {
	now := 1789560000.0 // 2026-09-16T12:00:00Z

	// Three accounts with ~96-97% quota and same 5h reset window.
	// Should balance load by request hits smoothly.
	acc1 := &storage.Account{
		ID:           "acc1",
		Status:       "active",
		RequestCount: 100,
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.97),
				ResetTime: "2026-09-16T16:50:00Z",
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.99),
				ResetTime: "2026-09-23T12:00:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}
	acc2 := &storage.Account{
		ID:           "acc2",
		Status:       "active",
		RequestCount: 10, // lowest hits
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.96),
				ResetTime: "2026-09-16T16:50:00Z",
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.99),
				ResetTime: "2026-09-23T12:00:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}

	ordered := OrderCandidates([]*storage.Account{acc1, acc2}, config.StrategySmart, nil, now)
	if len(ordered) != 2 || ordered[0].ID != "acc2" {
		t.Fatalf("expected acc2 (lowest hits: 10) to be ordered first, got %s", ordered[0].ID)
	}
}

func TestSmartStrategy_StrictWeakOrderingTransitivity(t *testing.T) {
	now := 1789560000.0 // 2026-09-16T12:00:00Z

	// Create diverse accounts across different headroom tiers, hits, and reset times
	accs := []*storage.Account{
		{
			ID: "acc_top_10hits", RequestCount: 10, Status: "active",
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.95), ResetTime: "2026-09-16T16:00:00Z"},
				GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.95), ResetTime: "2026-09-23T12:00:00Z"},
				UpdatedAt:    int64Ptr(int64(now - 10)),
			},
		},
		{
			ID: "acc_top_50hits", RequestCount: 50, Status: "active",
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.90), ResetTime: "2026-09-16T16:00:00Z"},
				GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.95), ResetTime: "2026-09-23T12:00:00Z"},
				UpdatedAt:    int64Ptr(int64(now - 10)),
			},
		},
		{
			ID: "acc_mid_5hits", RequestCount: 5, Status: "active",
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.65), ResetTime: "2026-09-16T16:00:00Z"},
				GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.70), ResetTime: "2026-09-23T12:00:00Z"},
				UpdatedAt:    int64Ptr(int64(now - 10)),
			},
		},
		{
			ID: "acc_mid_20hits", RequestCount: 20, Status: "active",
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.70), ResetTime: "2026-09-16T15:00:00Z"},
				GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.70), ResetTime: "2026-09-23T12:00:00Z"},
				UpdatedAt:    int64Ptr(int64(now - 10)),
			},
		},
		{
			ID: "acc_low_1hit", RequestCount: 1, Status: "active",
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.35), ResetTime: "2026-09-16T16:00:00Z"},
				GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.35), ResetTime: "2026-09-23T12:00:00Z"},
				UpdatedAt:    int64Ptr(int64(now - 10)),
			},
		},
		{
			ID: "acc_harvest_30m", RequestCount: 100, Status: "active",
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.85), ResetTime: "2026-09-16T12:30:00Z"}, // resets in 30m!
				GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.90), ResetTime: "2026-09-23T12:00:00Z"},
				UpdatedAt:    int64Ptr(int64(now - 10)),
			},
		},
	}

	ordered := OrderCandidates(accs, config.StrategySmart, nil, now)
	if len(ordered) != len(accs) {
		t.Fatalf("expected all %d accounts ordered, got %d", len(accs), len(ordered))
	}

	// Verify harvest account is ordered first despite 100 hits
	if ordered[0].ID != "acc_harvest_30m" {
		t.Fatalf("expected expiring harvest account to be first, got %s", ordered[0].ID)
	}

	// Verify top-tier accounts precede mid-tier and low-tier
	indices := make(map[string]int)
	for i, a := range ordered {
		indices[a.ID] = i
	}

	// Within top tier (>= 80%): 10 hits before 50 hits
	if indices["acc_top_10hits"] > indices["acc_top_50hits"] {
		t.Fatalf("expected acc_top_10hits before acc_top_50hits")
	}

	// Top tier before mid tier
	if indices["acc_top_50hits"] > indices["acc_mid_5hits"] {
		t.Fatalf("expected top tier before mid tier")
	}

	// Mid tier before low tier
	if indices["acc_mid_20hits"] > indices["acc_low_1hit"] {
		t.Fatalf("expected mid tier before low tier")
	}
}

func TestInFlightConcurrency_MaxQuotaAndSmart(t *testing.T) {
	observability.ResetInFlight()
	defer observability.ResetInFlight()

	now := 1700000000.0
	// accBusy has 100% quota, 0 hits
	accBusy := &storage.Account{
		ID:   "acc_busy",
		Name: "BusyAccount",
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(1.0), ResetTime: "2026-09-16T16:00:00Z"},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(1.0), ResetTime: "2026-09-23T12:00:00Z"},
			UpdatedAt:    int64Ptr(int64(now - 10)),
		},
	}
	// accIdle has 70% quota, 10 hits
	accIdle := &storage.Account{
		ID:           "acc_idle",
		Name:         "IdleAccount",
		RequestCount: 10,
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.7), ResetTime: "2026-09-16T16:00:00Z"},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.7), ResetTime: "2026-09-23T12:00:00Z"},
			UpdatedAt:    int64Ptr(int64(now - 10)),
		},
	}

	// When both are idle, accBusy wins under max_quota and smart (higher quota / tier)
	orderedMQ := OrderCandidates([]*storage.Account{accIdle, accBusy}, config.StrategyMaxQuota, nil, now)
	if orderedMQ[0].ID != "acc_busy" {
		t.Fatalf("expected acc_busy to win when both idle, got %s", orderedMQ[0].ID)
	}

	orderedSmart := OrderCandidates([]*storage.Account{accIdle, accBusy}, config.StrategySmart, nil, now)
	if orderedSmart[0].ID != "acc_busy" {
		t.Fatalf("expected acc_busy to win when both idle under smart, got %s", orderedSmart[0].ID)
	}

	// Mark accBusy as in-flight (1 active request)
	done := observability.StartInFlightGeneration("acc_busy")
	defer done()

	// Under new ordering, busy high-tier account outranks idle low-tier account
	orderedMQInFlight := OrderCandidates([]*storage.Account{accBusy, accIdle}, config.StrategyMaxQuota, nil, now)
	if orderedMQInFlight[0].ID != "acc_busy" {
		t.Fatalf("expected busy high-tier account to precede idle low-tier account under max_quota, got %s", orderedMQInFlight[0].ID)
	}

	orderedSmartInFlight := OrderCandidates([]*storage.Account{accBusy, accIdle}, config.StrategySmart, nil, now)
	if orderedSmartInFlight[0].ID != "acc_busy" {
		t.Fatalf("expected busy high-tier account to precede idle low-tier account under smart, got %s", orderedSmartInFlight[0].ID)
	}

	// accSameTierIdle is in the same tier/quota level (100% quota) with more hits (5 vs 0)
	accSameTierIdle := &storage.Account{
		ID:           "acc_same_tier_idle",
		Name:         "SameTierIdleAccount",
		RequestCount: 5,
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(1.0), ResetTime: "2026-09-16T16:00:00Z"},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(1.0), ResetTime: "2026-09-23T12:00:00Z"},
			UpdatedAt:    int64Ptr(int64(now - 10)),
		},
	}

	// Within the same tier, idle account outranks busy account despite higher hits
	orderedSmartSameTier := OrderCandidates([]*storage.Account{accBusy, accSameTierIdle}, config.StrategySmart, nil, now)
	if orderedSmartSameTier[0].ID != "acc_same_tier_idle" {
		t.Fatalf("expected idle same-tier account to precede busy same-tier account under smart, got %s", orderedSmartSameTier[0].ID)
	}

	orderedMQSameTier := OrderCandidates([]*storage.Account{accBusy, accSameTierIdle}, config.StrategyMaxQuota, nil, now)
	if orderedMQSameTier[0].ID != "acc_same_tier_idle" {
		t.Fatalf("expected idle same-tier account to precede busy same-tier account under max_quota, got %s", orderedMQSameTier[0].ID)
	}
}

func TestSmartHarvestCandidate_DecayingFloor(t *testing.T) {
	nowTime := time.Unix(1789560000, 0) // 2026-09-16T12:00:00Z
	in := func(d time.Duration) *time.Time { r := nowTime.Add(d); return &r }
	weeklyFar := in(5 * 24 * time.Hour)

	cases := []struct {
		name   string
		q5, q7 float64
		reset5 *time.Time
		want   bool
	}{
		{"5H leftover right before reset is harvested", 0.15, 0.90, in(10 * time.Minute), true},
		{"5H leftover too early in window is kept", 0.15, 0.90, in(80 * time.Minute), false},
		{"5H harvest never drains a low weekly window", 0.90, 0.15, in(10 * time.Minute), false},
		{"5H reset outside harvest window", 0.90, 0.90, in(3 * time.Hour), false},
	}
	for _, tc := range cases {
		q5, q7 := tc.q5, tc.q7
		capState := quota.CapacityState{
			Q5: &q5, Q7: &q7, Q5Known: true, Q7Known: true,
			RawFloor: math.Min(q5, q7), Reset5Time: tc.reset5, Reset7Time: weeklyFar,
		}
		if _, got := SmartHarvestCandidate(capState, nowTime); got != tc.want {
			t.Errorf("%s: got harvest=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSmartStrategy_OverWeeklyPaceHeldBack(t *testing.T) {
	now := 1789560000.0 // 2026-09-16T12:00:00Z

	// accFast would win on hits, but has spent 55% of its week with 6.5 days left (pace ≈ -0.48).
	accFast := &storage.Account{
		ID: "acc_fast", Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(1.0), ResetTime: "2026-09-16T16:00:00Z"},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.45), ResetTime: "2026-09-23T00:00:00Z"},
			UpdatedAt:    int64Ptr(int64(now - 10)),
		},
	}
	// accOnPace has more hits but is on weekly pace (3 days left, pace ≈ +0.07).
	accOnPace := &storage.Account{
		ID: "acc_on_pace", Status: "active", RequestCount: 50,
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.5), ResetTime: "2026-09-16T16:00:00Z"},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.5), ResetTime: "2026-09-19T12:00:00Z"},
			UpdatedAt:    int64Ptr(int64(now - 10)),
		},
	}

	for _, strategy := range []string{config.StrategySmart, config.StrategyMaxQuota} {
		ordered := OrderCandidates([]*storage.Account{accFast, accOnPace}, strategy, nil, now)
		if ordered[0].ID != "acc_on_pace" {
			t.Errorf("%s: expected on-pace account first, got %s", strategy, ordered[0].ID)
		}
	}
	if IsReserveCandidate(accFast, now) {
		t.Error("over-pace account must keep its healthy label; only routing order changes")
	}
}

func TestOrderCandidates_InFlight_TierPrecedence(t *testing.T) {
	observability.ResetInFlight()
	defer observability.ResetInFlight()

	now := 1700000000.0

	// (a) busy high-tier account outranks idle low-tier account
	t.Run("busy high-tier outranks idle low-tier", func(t *testing.T) {
		accHighBusy := &storage.Account{
			ID:     "acc_high_busy",
			Status: "active",
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.90), ResetTime: "2026-09-16T16:00:00Z"},
				GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.90), ResetTime: "2026-09-23T12:00:00Z"},
				UpdatedAt:    int64Ptr(int64(now - 10)),
			},
		}
		accLowIdle := &storage.Account{
			ID:     "acc_low_idle",
			Status: "active",
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.70), ResetTime: "2026-09-16T16:00:00Z"},
				GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.70), ResetTime: "2026-09-23T12:00:00Z"},
				UpdatedAt:    int64Ptr(int64(now - 10)),
			},
		}

		done := observability.StartInFlightGeneration("acc_high_busy")
		defer done()

		// Under Smart: Tier 4 (0.90) > Tier 3 (0.70)
		orderedSmart := OrderCandidates([]*storage.Account{accLowIdle, accHighBusy}, config.StrategySmart, nil, now)
		if len(orderedSmart) < 2 || orderedSmart[0].ID != "acc_high_busy" {
			t.Fatalf("expected busy high-tier account to outrank idle low-tier account under smart, got %s", orderedSmart[0].ID)
		}

		// Under MaxQuota: RawFloor 0.90 > 0.70
		orderedMQ := OrderCandidates([]*storage.Account{accLowIdle, accHighBusy}, config.StrategyMaxQuota, nil, now)
		if len(orderedMQ) < 2 || orderedMQ[0].ID != "acc_high_busy" {
			t.Fatalf("expected busy high-tier account to outrank idle low-tier account under max_quota, got %s", orderedMQ[0].ID)
		}
	})

	// (b) idle same-tier account outranks busy same-tier account
	t.Run("idle same-tier outranks busy same-tier cruising", func(t *testing.T) {
		accSameTierBusy := &storage.Account{
			ID:           "acc_same_busy",
			Status:       "active",
			RequestCount: 0,
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.85), ResetTime: "2026-09-16T16:00:00Z"},
				GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.85), ResetTime: "2026-09-23T12:00:00Z"},
				UpdatedAt:    int64Ptr(int64(now - 10)),
			},
		}
		accSameTierIdle := &storage.Account{
			ID:           "acc_same_idle",
			Status:       "active",
			RequestCount: 20, // more hits
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.85), ResetTime: "2026-09-16T16:00:00Z"},
				GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.85), ResetTime: "2026-09-23T12:00:00Z"},
				UpdatedAt:    int64Ptr(int64(now - 10)),
			},
		}

		done := observability.StartInFlightGeneration("acc_same_busy")
		defer done()

		// Under Smart
		orderedSmart := OrderCandidates([]*storage.Account{accSameTierBusy, accSameTierIdle}, config.StrategySmart, nil, now)
		if len(orderedSmart) < 2 || orderedSmart[0].ID != "acc_same_idle" {
			t.Fatalf("expected idle same-tier account to outrank busy same-tier account under smart, got %s", orderedSmart[0].ID)
		}

		// Under MaxQuota
		orderedMQ := OrderCandidates([]*storage.Account{accSameTierBusy, accSameTierIdle}, config.StrategyMaxQuota, nil, now)
		if len(orderedMQ) < 2 || orderedMQ[0].ID != "acc_same_idle" {
			t.Fatalf("expected idle same-tier account to outrank busy same-tier account under max_quota, got %s", orderedMQ[0].ID)
		}
	})

	t.Run("idle same-tier outranks busy same-tier harvest", func(t *testing.T) {
		// Both qualify for harvest with identical reset time (in 30 mins) and identical tier (RawFloor 0.50 -> Tier 2)
		nowTime := time.Unix(1789560000, 0)
		nowF := float64(nowTime.Unix())
		reset5 := nowTime.Add(30 * time.Minute).Format(time.RFC3339)
		reset7 := nowTime.Add(5 * 24 * time.Hour).Format(time.RFC3339)

		accHarvestBusy := &storage.Account{
			ID:           "acc_harvest_busy",
			Status:       "active",
			RequestCount: 0,
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.50), ResetTime: reset5},
				GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.50), ResetTime: reset7},
				UpdatedAt:    int64Ptr(int64(nowF - 10)),
			},
		}
		accHarvestIdle := &storage.Account{
			ID:           "acc_harvest_idle",
			Status:       "active",
			RequestCount: 20, // more hits
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.50), ResetTime: reset5},
				GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.50), ResetTime: reset7},
				UpdatedAt:    int64Ptr(int64(nowF - 10)),
			},
		}

		done := observability.StartInFlightGeneration("acc_harvest_busy")
		defer done()

		orderedSmart := OrderCandidates([]*storage.Account{accHarvestBusy, accHarvestIdle}, config.StrategySmart, nil, nowF)
		if len(orderedSmart) < 2 || orderedSmart[0].ID != "acc_harvest_idle" {
			t.Fatalf("expected idle harvest account to outrank busy harvest account under smart, got %s", orderedSmart[0].ID)
		}
	})
}

func TestOrderCandidates_Smart_UnknownQuotaSortsAfterKnownQuota(t *testing.T) {
	now := 1789560000.0

	// (c) under smart, an unknown-quota account sorts after a known-quota one.
	accKnown := &storage.Account{
		ID:           "acc_known",
		Status:       "active",
		RequestCount: 100, // high hits
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.75), ResetTime: "2026-09-16T16:00:00Z"},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.75), ResetTime: "2026-09-23T12:00:00Z"},
			UpdatedAt:    int64Ptr(int64(now - 50)),
		},
	}

	// accUnknownFresh has 0 hits and very fresh probe timestamp (FreshnessRank 2), but KnownWindowCount is 0
	accUnknownFresh := &storage.Account{
		ID:           "acc_unknown_fresh",
		Status:       "active",
		RequestCount: 0,
		LastQuota: &storage.QuotaState{
			UpdatedAt: int64Ptr(int64(now - 1)),
		},
	}

	// accUnknownNil has nil LastQuota
	accUnknownNil := &storage.Account{
		ID:           "acc_unknown_nil",
		Status:       "active",
		RequestCount: 0,
	}

	// Known quota account must outrank fresh unknown account
	ordered1 := OrderCandidates([]*storage.Account{accUnknownFresh, accKnown}, config.StrategySmart, nil, now)
	if len(ordered1) < 2 || ordered1[0].ID != "acc_known" {
		t.Fatalf("expected known-quota account to outrank fresh unknown-quota account under smart, got %s", ordered1[0].ID)
	}

	// Known quota account must outrank nil-quota unknown account
	ordered2 := OrderCandidates([]*storage.Account{accUnknownNil, accKnown}, config.StrategySmart, nil, now)
	if len(ordered2) < 2 || ordered2[0].ID != "acc_known" {
		t.Fatalf("expected known-quota account to outrank nil-quota unknown account under smart, got %s", ordered2[0].ID)
	}
}

func TestOrderCandidates_Smart_InFlightSpillsToNextTier(t *testing.T) {
	observability.ResetInFlight()
	defer observability.ResetInFlight()

	now := 1700000000.0
	acc := func(id string, frac float64) *storage.Account {
		return &storage.Account{
			ID:     id,
			Status: "active",
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(frac), ResetTime: "2026-09-16T16:00:00Z"},
				GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(frac), ResetTime: "2026-09-23T12:00:00Z"},
				UpdatedAt:    int64Ptr(int64(now - 10)),
			},
		}
	}
	top, next := acc("acc_top", 0.85), acc("acc_next", 0.70)

	for i := 1; i <= smartInFlightPerTier; i++ {
		done := observability.StartInFlightGeneration("acc_top")
		defer done()
		want := "acc_top"
		if i == smartInFlightPerTier {
			want = "acc_next" // one tier of in-flight penalty ties the tiers; the idle account wins
		}
		ordered := OrderCandidates([]*storage.Account{next, top}, config.StrategySmart, nil, now)
		if ordered[0].ID != want {
			t.Fatalf("in-flight=%d: first = %s, want %s", i, ordered[0].ID, want)
		}
	}
}

func TestSmartStrategy_MidWeek5HNotStrandedByWeeklyTier(t *testing.T) {
	now := 1789560000.0 // 2026-09-16T12:00:00Z
	// accMidWeek: 3.5 days left in week (r7 = 0.50), Q7 = 0.65 (pace7 = +0.15 >= 0, ahead of schedule),
	// Q5 = 0.95 (resets in 2h). Its regenerable 5H quota is Tier 4 and resets sooner.
	accMidWeek := &storage.Account{
		ID:           "acc_mid_week_full_5h",
		Status:       "active",
		RequestCount: 50,
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.95), ResetTime: "2026-09-16T14:00:00Z"},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.65), ResetTime: "2026-09-20T00:00:00Z"},
			UpdatedAt:    int64Ptr(int64(now - 10)),
		},
	}
	// accFreshWeek: 6.5 days left in week, Q7 = 0.85, Q5 = 0.85 (resets in 4.5h).
	accFreshWeek := &storage.Account{
		ID:           "acc_fresh_week_later_5h",
		Status:       "active",
		RequestCount: 10,
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.85), ResetTime: "2026-09-16T16:30:00Z"},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.85), ResetTime: "2026-09-23T00:00:00Z"},
			UpdatedAt:    int64Ptr(int64(now - 10)),
		},
	}

	ordered := OrderCandidates([]*storage.Account{accFreshWeek, accMidWeek}, config.StrategySmart, nil, now)
	if len(ordered) != 2 || ordered[0].ID != "acc_mid_week_full_5h" {
		t.Fatalf("expected mid-week account with 95%% 5H quota (resets in 2h) to be used before 4.5h account, got %s", ordered[0].ID)
	}
}

func TestSmartStrategy_Harvest15MinBucketAvoidsClockSeam(t *testing.T) {
	// 2026-09-16T12:02:00Z: both accounts fall inside the 12:xx UTC hour,
	// but accImminent resets in 3 minutes (12:05) while accLaterHarvest resets in 56 minutes (12:58).
	now := 1789560120.0
	accImminent := &storage.Account{
		ID:     "acc_imminent_3m",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.45), ResetTime: "2026-09-16T12:05:00Z"},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.90), ResetTime: "2026-09-22T12:00:00Z"},
			UpdatedAt:    int64Ptr(int64(now - 10)),
		},
	}
	accLaterHarvest := &storage.Account{
		ID:     "acc_later_56m",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.85), ResetTime: "2026-09-16T12:58:00Z"},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.90), ResetTime: "2026-09-22T12:00:00Z"},
			UpdatedAt:    int64Ptr(int64(now - 10)),
		},
	}

	ordered := OrderCandidates([]*storage.Account{accLaterHarvest, accImminent}, config.StrategySmart, nil, now)
	if len(ordered) != 2 || ordered[0].ID != "acc_imminent_3m" {
		t.Fatalf("expected 3m-to-reset harvest account before 56m-to-reset account, got %s", ordered[0].ID)
	}
}
