package proxy

import (
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/scheduler"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func TestModelSmoothInterval(t *testing.T) {
	if intvl := ModelSmoothInterval("gemini-2.5-pro"); intvl != 6*time.Second {
		t.Fatalf("expected 6s for gemini-2.5-pro, got %v", intvl)
	}
	if intvl := ModelSmoothInterval("models/gemini-pro"); intvl != 6*time.Second {
		t.Fatalf("expected 6s for models/gemini-pro, got %v", intvl)
	}
	if intvl := ModelSmoothInterval("gemini-2.5-flash"); intvl != 1*time.Second {
		t.Fatalf("expected 1s for gemini-2.5-flash, got %v", intvl)
	}
}

func TestSmoothModelRate_ReordersCoolingAccounts(t *testing.T) {
	ResetSmoothRate()
	t.Cleanup(ResetSmoothRate)

	frac := 0.9
	acc1 := &storage.Account{ID: "acc-1", LastQuota: &storage.QuotaState{RemainingFraction: &frac}}
	acc2 := &storage.Account{ID: "acc-2", LastQuota: &storage.QuotaState{RemainingFraction: &frac}}
	model := "gemini-2.5-pro"

	t0 := time.Now()
	// acc1 just served this model 1 second ago (needs 6s)
	RecordModelDispatch(acc1.ID, model, t0.Add(-1*time.Second))

	// Initial order: acc1, acc2
	cands := []*storage.Account{acc1, acc2}

	// At t0, acc1 is cooling, acc2 is ready
	reordered := SmoothModelRate(cands, model, t0)
	if len(reordered) != 2 || reordered[0].ID != "acc-2" || reordered[1].ID != "acc-1" {
		t.Fatalf("expected acc-2 to be prioritized ahead of cooling acc-1, got [%s, %s]", reordered[0].ID, reordered[1].ID)
	}

	// At t0 + 7s, acc1 is rested (> 6s)
	rested := SmoothModelRate(cands, model, t0.Add(7*time.Second))
	if rested[0].ID != "acc-1" {
		t.Fatalf("expected original order when both accounts are ready, got first=%s", rested[0].ID)
	}
}

func TestSmoothModelRate_AllCoolingPreservesOrder(t *testing.T) {
	ResetSmoothRate()
	t.Cleanup(ResetSmoothRate)

	frac := 0.9
	acc1 := &storage.Account{ID: "acc-1", LastQuota: &storage.QuotaState{RemainingFraction: &frac}}
	acc2 := &storage.Account{ID: "acc-2", LastQuota: &storage.QuotaState{RemainingFraction: &frac}}
	model := "gemini-2.5-pro"

	t0 := time.Now()
	RecordModelDispatch(acc1.ID, model, t0.Add(-1*time.Second))
	RecordModelDispatch(acc2.ID, model, t0.Add(-2*time.Second))

	cands := []*storage.Account{acc1, acc2}
	reordered := SmoothModelRate(cands, model, t0)
	if reordered[0].ID != "acc-1" || reordered[1].ID != "acc-2" {
		t.Fatalf("expected original order preserved when all candidates are cooling, got [%s, %s]", reordered[0].ID, reordered[1].ID)
	}
}

func TestSmoothModelRate_KeepsHealthyAheadOfRateLimited(t *testing.T) {
	now := time.Now()
	nowSecs := float64(now.Unix())
	futureRL := nowSecs + 3600
	frac := 0.9

	healthy := &storage.Account{
		ID:        "healthy",
		LastQuota: &storage.QuotaState{RemainingFraction: &frac},
	}
	limited := &storage.Account{
		ID:               "limited",
		RateLimitedUntil: &futureRL,
		LastQuota:        &storage.QuotaState{RemainingFraction: &frac},
	}

	pool := &storage.Pool{
		Accounts: []*storage.Account{healthy, limited},
	}

	cands := scheduler.OrderCandidates(pool.Accounts, config.StrategySmart, pool, nowSecs)
	ResetSmoothRate()
	t.Cleanup(ResetSmoothRate)

	RecordModelDispatch("healthy", "gemini-3.8-flash-high", now.Add(-500*time.Millisecond))

	smoothed := SmoothModelRate(cands, "gemini-3.8-flash-high", now)
	if len(smoothed) < 2 || smoothed[0].ID != "healthy" {
		t.Fatalf("expected healthy to stay first, got first=%s", smoothed[0].ID)
	}
}
