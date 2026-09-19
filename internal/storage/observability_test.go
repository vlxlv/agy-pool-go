package storage

import (
	"errors"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/observability"
)

// Case L: Persistence failure increments persistence_errors_total exactly once
func TestObservability_CaseL_PersistenceFailure(t *testing.T) {
	setupTestStorage(t)
	observability.Reset()

	injectedErr := errors.New("simulated transaction failure")

	err := PoolTransaction(func(pool *Pool) error {
		return injectedErr
	})

	if !errors.Is(err, injectedErr) {
		t.Fatalf("expected injected error, got: %v", err)
	}

	snap := observability.GetSnapshot()
	if snap.PersistenceErrors != 1 {
		t.Errorf("persistence_errors_total = %d, want 1", snap.PersistenceErrors)
	}

	// Repeat once more to verify increment count
	_ = PoolTransaction(func(pool *Pool) error {
		return injectedErr
	})

	snap = observability.GetSnapshot()
	if snap.PersistenceErrors != 2 {
		t.Errorf("persistence_errors_total = %d, want 2", snap.PersistenceErrors)
	}
}
