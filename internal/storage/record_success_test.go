package storage

import (
	"testing"
	"time"
)

func TestRecordSuccess(t *testing.T) {
	setupTestStorage(t)

	now := time.Now().Unix()
	pastCooldown := float64(now - 100)
	futureCooldown := float64(now + 3600)

	acc1 := &Account{
		ID:               "acc_1",
		Status:           StatusVerificationPending,
		RateLimitedUntil: &pastCooldown,
		GenCount:         nil,
	}
	acc2 := &Account{
		ID:               "acc_2",
		RateLimitedUntil: &futureCooldown,
	}

	activeID := "acc_2"
	pool := NewEmptyPool()
	pool.ActiveAccountID = &activeID
	pool.Accounts = []*Account{acc1, acc2}

	if err := SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	// 1. Generation success on acc_1: promotes to active, clears pending status and past cooldown
	if err := RecordSuccess(&Account{ID: "acc_1"}, true, now); err != nil {
		t.Fatalf("RecordSuccess failed: %v", err)
	}

	reloaded, err := LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}

	rAcc1 := FindAccount(reloaded, &Account{ID: "acc_1"})
	if rAcc1 == nil {
		t.Fatal("acc_1 not found in reloaded pool")
	}
	if rAcc1.RequestCount != 1 {
		t.Errorf("acc_1 RequestCount = %d, want 1", rAcc1.RequestCount)
	}
	if rAcc1.GenCount == nil || *rAcc1.GenCount != 1 {
		t.Errorf("acc_1 GenCount = %v, want 1", rAcc1.GenCount)
	}
	if rAcc1.LastUsedAt == nil || *rAcc1.LastUsedAt != now {
		t.Errorf("acc_1 LastUsedAt = %v, want %d", rAcc1.LastUsedAt, now)
	}
	if rAcc1.Status != "" {
		t.Errorf("acc_1 Status = %q, want empty string", rAcc1.Status)
	}
	if rAcc1.RateLimitedUntil != nil {
		t.Errorf("acc_1 RateLimitedUntil = %v, want nil", rAcc1.RateLimitedUntil)
	}

	if len(reloaded.GenerationEvents) != 1 {
		t.Fatalf("len(GenerationEvents) = %d, want 1", len(reloaded.GenerationEvents))
	}
	if reloaded.GenerationEvents[0].ID != "acc_1" {
		t.Errorf("GenerationEvent ID = %q, want acc_1", reloaded.GenerationEvents[0].ID)
	}

	if reloaded.ActiveAccountID == nil || *reloaded.ActiveAccountID != "acc_1" {
		t.Errorf("ActiveAccountID = %v, want acc_1", reloaded.ActiveAccountID)
	}

	rAcc2 := FindAccount(reloaded, &Account{ID: "acc_2"})
	if rAcc2 == nil {
		t.Fatal("acc_2 not found in reloaded pool")
	}
	if rAcc2.RateLimitedUntil == nil || *rAcc2.RateLimitedUntil != futureCooldown {
		t.Errorf("acc_2 RateLimitedUntil = %v, want %f", rAcc2.RateLimitedUntil, futureCooldown)
	}
	if rAcc2.RequestCount != 0 {
		t.Errorf("acc_2 RequestCount = %d, want 0", rAcc2.RequestCount)
	}

	// 2. Non-generation success on validation_required account: only bumps RequestCount
	validationAcc := &Account{
		ID:               "acc_3",
		Status:           StatusValidationRequired,
		RateLimitedUntil: &futureCooldown,
	}
	reloaded.Accounts = append(reloaded.Accounts, validationAcc)
	if err := SavePool(reloaded); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	if err := RecordSuccess(&Account{ID: "acc_3"}, false, now); err != nil {
		t.Fatalf("RecordSuccess non-gen failed: %v", err)
	}

	reloaded2, err := LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}

	rAcc3 := FindAccount(reloaded2, &Account{ID: "acc_3"})
	if rAcc3 == nil {
		t.Fatal("acc_3 not found in reloaded2 pool")
	}
	if rAcc3.RequestCount != 1 {
		t.Errorf("acc_3 RequestCount = %d, want 1", rAcc3.RequestCount)
	}
	if rAcc3.Status != StatusValidationRequired {
		t.Errorf("acc_3 Status = %q, want %q", rAcc3.Status, StatusValidationRequired)
	}
	if rAcc3.RateLimitedUntil == nil || *rAcc3.RateLimitedUntil != futureCooldown {
		t.Errorf("acc_3 RateLimitedUntil = %v, want %f", rAcc3.RateLimitedUntil, futureCooldown)
	}
	if rAcc3.GenCount != nil {
		t.Errorf("acc_3 GenCount = %v, want nil", rAcc3.GenCount)
	}
	if rAcc3.LastUsedAt != nil {
		t.Errorf("acc_3 LastUsedAt = %v, want nil", rAcc3.LastUsedAt)
	}
}
