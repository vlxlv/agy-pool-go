package scheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/config"
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
			if tc.Strategy == "least_used" || tc.Strategy == "round_robin" {
				t.Skip("deprecated strategy: " + tc.Strategy)
				return
			}
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

// Test L: max_quota only; least_used and round_robin remain unchanged
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
				Fraction:  floatPtr(0.23), // constrained weekly budget!
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

	// Account Low has only 20% 5H quota and resets in 25 minutes.
	// Because headroom is < 30%, it has no surplus quota to harvest and must NOT be drained into 429!
	// Account Healthy (90% quota) should be chosen instead.
	accLow := &storage.Account{
		ID:     "acc_low",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.20), // low headroom!
				ResetTime: "2026-09-16T12:25:00Z",
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
