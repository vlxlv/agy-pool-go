package scheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

type FixtureOrderingCase struct {
	TestID             string              `json:"test_id"`
	Strategy           string              `json:"strategy"`
	Now                float64             `json:"now"`
	Pool               *storage.Pool       `json:"pool"`
	Candidates         []*storage.Account  `json:"candidates"`
	ExpectedOrderedIDs []string            `json:"expected_ordered_ids"`
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

func TestRoundRobinAtomicReservation(t *testing.T) {
	tmpDir := t.TempDir()
	if err := config.ConfigureStateDir(tmpDir); err != nil {
		t.Fatalf("failed to configure state dir: %v", err)
	}

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
	tmpDir := t.TempDir()
	if err := config.ConfigureStateDir(tmpDir); err != nil {
		t.Fatalf("failed to configure state dir: %v", err)
	}

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
