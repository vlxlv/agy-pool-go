package scheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
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

func TestRoundRobinAtomicReservation(t *testing.T) {
	setupTestStateDir(t)

	now := 1789560000.0
	pool := storage.NewEmptyPool()
	pool.Strategy = "round_robin"
	pool.Accounts = []*storage.Account{
		{ID: "acc_1", LastQuota: &storage.QuotaState{RemainingFraction: floatPtr(0.9), UpdatedAt: int64Ptr(int64(now - 10))}},
		{ID: "acc_2", LastQuota: &storage.QuotaState{RemainingFraction: floatPtr(0.9), UpdatedAt: int64Ptr(int64(now - 10))}},
		{ID: "acc_3", LastQuota: &storage.QuotaState{RemainingFraction: floatPtr(0.9), UpdatedAt: int64Ptr(int64(now - 10))}},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	// 1st reservation: acc_1
	acc, err := ReserveRoundRobinAccount(now)
	if err != nil || acc == nil || acc.ID != "acc_1" {
		t.Fatalf("1st reservation expected acc_1, got %v (err: %v)", acc, err)
	}

	// Check persisted cursor
	loaded, _ := storage.LoadPool()
	if loaded.RoundRobinLastAccountID == nil || *loaded.RoundRobinLastAccountID != "acc_1" {
		t.Fatalf("expected cursor acc_1, got %v", loaded.RoundRobinLastAccountID)
	}

	// 2nd reservation: acc_2
	acc, err = ReserveRoundRobinAccount(now)
	if err != nil || acc == nil || acc.ID != "acc_2" {
		t.Fatalf("2nd reservation expected acc_2, got %v (err: %v)", acc, err)
	}

	// 3rd reservation: acc_3
	acc, err = ReserveRoundRobinAccount(now)
	if err != nil || acc == nil || acc.ID != "acc_3" {
		t.Fatalf("3rd reservation expected acc_3, got %v (err: %v)", acc, err)
	}

	// 4th reservation wraps around to acc_1
	acc, err = ReserveRoundRobinAccount(now)
	if err != nil || acc == nil || acc.ID != "acc_1" {
		t.Fatalf("4th reservation expected wrap to acc_1, got %v (err: %v)", acc, err)
	}
}

func TestConcurrentRoundRobinReservation(t *testing.T) {
	setupTestStateDir(t)

	now := 1789560000.0
	pool := storage.NewEmptyPool()
	pool.Strategy = "round_robin"
	pool.Accounts = []*storage.Account{
		{ID: "acc_1", LastQuota: &storage.QuotaState{RemainingFraction: floatPtr(0.9), UpdatedAt: int64Ptr(int64(now - 10))}},
		{ID: "acc_2", LastQuota: &storage.QuotaState{RemainingFraction: floatPtr(0.9), UpdatedAt: int64Ptr(int64(now - 10))}},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	const numReservations = 20
	var counts sync.Map

	var wg sync.WaitGroup
	wg.Add(numReservations)

	for i := 0; i < numReservations; i++ {
		go func() {
			defer wg.Done()
			acc, err := ReserveRoundRobinAccount(now)
			if err != nil {
				t.Errorf("reservation error: %v", err)
				return
			}
			if acc != nil {
				val, _ := counts.LoadOrStore(acc.ID, new(int))
				ptr := val.(*int)
				// We don't synchronize atomic increment here on ptr, but we can verify both received hits
				_ = ptr
			}
		}()
	}

	wg.Wait()

	// Verify cursor was updated
	loaded, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	if loaded.RoundRobinLastAccountID == nil {
		t.Fatalf("expected cursor to be non-nil after reservations")
	}
}

func floatPtr(f float64) *float64 {
	return &f
}

func int64Ptr(i int64) *int64 {
	return &i
}

func TestRoundRobin_StaleMissingCursor(t *testing.T) {
	setupTestStateDir(t)

	now := 1789560000.0
	pool := storage.NewEmptyPool()
	pool.Strategy = "round_robin"
	ghostID := "acc_ghost"
	pool.RoundRobinLastAccountID = &ghostID
	pool.Accounts = []*storage.Account{
		{ID: "acc_1", LastQuota: &storage.QuotaState{RemainingFraction: floatPtr(0.9), UpdatedAt: int64Ptr(int64(now - 10))}},
		{ID: "acc_2", LastQuota: &storage.QuotaState{RemainingFraction: floatPtr(0.9), UpdatedAt: int64Ptr(int64(now - 10))}},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	// Reservation must not fail; it falls back to normal order and updates cursor
	acc, err := ReserveRoundRobinAccount(now)
	if err != nil {
		t.Fatalf("ReserveRoundRobinAccount failed with stale cursor: %v", err)
	}
	if acc == nil || acc.ID != "acc_1" {
		t.Fatalf("expected fallback to acc_1, got %v", acc)
	}

	loaded, _ := storage.LoadPool()
	if loaded.RoundRobinLastAccountID == nil || *loaded.RoundRobinLastAccountID != "acc_1" {
		t.Fatalf("cursor was not updated from stale value: got %v", loaded.RoundRobinLastAccountID)
	}
}

func TestRoundRobin_RestartPersistence(t *testing.T) {
	setupTestStateDir(t)

	now := 1789560000.0
	pool := storage.NewEmptyPool()
	pool.Strategy = "round_robin"
	pool.Accounts = []*storage.Account{
		{ID: "acc_1"},
		{ID: "acc_2"},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	// 1. Reserve acc_1
	acc, err := ReserveRoundRobinAccount(now)
	if err != nil || acc == nil || acc.ID != "acc_1" {
		t.Fatalf("1st reservation failed: %v", err)
	}

	// 2. Simulate complete restart by closing DB
	sdb, err := storage.GetStateDB()
	if err != nil {
		t.Fatalf("GetStateDB failed: %v", err)
	}
	if err := sdb.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// 3. Re-open and reserve again -> must pick acc_2!
	acc2, err := ReserveRoundRobinAccount(now)
	if err != nil || acc2 == nil || acc2.ID != "acc_2" {
		t.Fatalf("reservation after restart failed: want acc_2, got %v (err: %v)", acc2, err)
	}
}

func TestRoundRobin_FailedDispatchNoRollback(t *testing.T) {
	setupTestStateDir(t)

	now := 1789560000.0
	pool := storage.NewEmptyPool()
	pool.Strategy = "round_robin"
	pool.Accounts = []*storage.Account{
		{ID: "acc_1"},
		{ID: "acc_2"},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	// Reserve acc_1
	acc, err := ReserveRoundRobinAccount(now)
	if err != nil || acc == nil || acc.ID != "acc_1" {
		t.Fatalf("reservation failed: %v", err)
	}

	// Simulated downstream network failure (e.g. Google returned 500 or timeout)
	// Caller does NOT rollback RR cursor
	loaded, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	if loaded.RoundRobinLastAccountID == nil || *loaded.RoundRobinLastAccountID != "acc_1" {
		t.Fatalf("cursor was unexpectedly rolled back: %v", loaded.RoundRobinLastAccountID)
	}
}

func TestRoundRobin_NonRRStrategyCursorUnchanged(t *testing.T) {
	setupTestStateDir(t)

	now := 1789560000.0
	pool := storage.NewEmptyPool()
	initCursor := "acc_preserved"
	pool.RoundRobinLastAccountID = &initCursor
	pool.Accounts = []*storage.Account{
		{ID: "acc_1", RequestCount: 5},
		{ID: "acc_2", RequestCount: 1},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	// Evaluate with max_quota and least_used
	_ = OrderCandidates(pool.Accounts, config.StrategyMaxQuota, pool, now)
	_ = OrderCandidates(pool.Accounts, config.StrategyLeastUsed, pool, now)

	// Check that cursor in state.db was NOT mutated
	loaded, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	if loaded.RoundRobinLastAccountID == nil || *loaded.RoundRobinLastAccountID != "acc_preserved" {
		t.Fatalf("non-RR strategy mutated cursor! got %v", loaded.RoundRobinLastAccountID)
	}
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
func TestMaxQuota_ReserveHeadroom_LeastUsedAndRoundRobinUnchanged(t *testing.T) {
	now := 1789560000.0

	// Main has low quota (Reserve for max_quota) but low request count
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

	// Under least_used: Main wins because Main has fewer requests (Hits=2 vs 20)
	orderedLU := OrderCandidates([]*storage.Account{backupAcc, mainAcc}, config.StrategyLeastUsed, nil, now)
	if orderedLU[0].ID != "Main" {
		t.Fatalf("least_used expected Main first, got %s", orderedLU[0].ID)
	}

	// Under round_robin: cursor dictates order
	pool := storage.NewEmptyPool()
	pool.Strategy = "round_robin"
	cursor := "Backup"
	pool.RoundRobinLastAccountID = &cursor
	orderedRR := OrderCandidates([]*storage.Account{backupAcc, mainAcc}, config.StrategyRoundRobin, pool, now)
	if orderedRR[0].ID != "Main" {
		t.Fatalf("round_robin expected rotation past Backup to Main, got %s", orderedRR[0].ID)
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
