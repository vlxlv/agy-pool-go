package diagnostics

import (
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func TestComputeEffectivenessReport_EmptyPool(t *testing.T) {
	_, cleanup := setupDoctorTestEnv(t)
	defer cleanup()

	report, err := ComputeEffectivenessReport(nil, 4*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.ActiveAccounts != 0 {
		t.Fatalf("expected 0 active accounts, got %d", report.ActiveAccounts)
	}
	if report.BalanceStatus != "IDLE" {
		t.Fatalf("expected IDLE status, got %s", report.BalanceStatus)
	}
}

func TestComputeEffectivenessReport_BalancedDistribution(t *testing.T) {
	_, cleanup := setupDoctorTestEnv(t)
	defer cleanup()

	now := time.Now().Unix()
	frac := 0.90
	acc1 := &storage.Account{
		ID:     "acc1",
		Name:   "Account 1",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: &frac},
			GeminiWeekly: &storage.QuotaWindow{Fraction: &frac},
		},
	}
	acc2 := &storage.Account{
		ID:     "acc2",
		Name:   "Account 2",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: &frac},
			GeminiWeekly: &storage.QuotaWindow{Fraction: &frac},
		},
	}

	pool := &storage.Pool{
		Version:  1,
		Strategy: "smart",
		Accounts: []*storage.Account{acc1, acc2},
		GenerationEvents: []storage.GenerationEvent{
			{ID: "acc1", TS: now - 100},
			{ID: "acc1", TS: now - 80},
			{ID: "acc2", TS: now - 60},
			{ID: "acc2", TS: now - 40},
		},
	}
	_ = storage.SavePool(pool)

	report, err := ComputeEffectivenessReport(nil, 4*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if report.ActiveAccounts != 2 {
		t.Fatalf("expected 2 active accounts, got %d", report.ActiveAccounts)
	}
	if report.WindowRequests != 4 {
		t.Fatalf("expected 4 window requests, got %d", report.WindowRequests)
	}
	if report.BalanceIndex < 0.99 {
		t.Fatalf("expected perfectly balanced index ~1.0, got %f", report.BalanceIndex)
	}
	if report.BalanceStatus != "OPTIMAL" {
		t.Fatalf("expected OPTIMAL balance status, got %s", report.BalanceStatus)
	}
	if report.AIVerdict.Status != "DEGRADED" && report.AIVerdict.Status != "WARNING" && report.AIVerdict.Status != "HEALTHY" {
		t.Fatalf("invalid AI verdict status: %s", report.AIVerdict.Status)
	}
}

func TestComputeEffectivenessReport_SkewedDistribution(t *testing.T) {
	_, cleanup := setupDoctorTestEnv(t)
	defer cleanup()

	now := time.Now().Unix()
	frac := 0.90
	acc1 := &storage.Account{
		ID:     "acc1",
		Name:   "Account 1",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: &frac},
			GeminiWeekly: &storage.QuotaWindow{Fraction: &frac},
		},
	}
	acc2 := &storage.Account{
		ID:     "acc2",
		Name:   "Account 2",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: &frac},
			GeminiWeekly: &storage.QuotaWindow{Fraction: &frac},
		},
	}

	events := []storage.GenerationEvent{}
	for i := 0; i < 20; i++ {
		events = append(events, storage.GenerationEvent{ID: "acc1", TS: now - int64(i*10)})
	}
	events = append(events, storage.GenerationEvent{ID: "acc2", TS: now - 5})

	pool := &storage.Pool{
		Version:          1,
		Strategy:         "smart",
		Accounts:         []*storage.Account{acc1, acc2},
		GenerationEvents: events,
	}
	_ = storage.SavePool(pool)

	report, err := ComputeEffectivenessReport(nil, 4*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if report.DominantAccount != "acc1" {
		t.Fatalf("expected dominant account acc1, got %s", report.DominantAccount)
	}
	if report.BalanceStatus != "SKEWED" && report.BalanceStatus != "MONOPOLIZED" {
		t.Fatalf("expected SKEWED or MONOPOLIZED status, got %s", report.BalanceStatus)
	}
	if len(report.AIVerdict.Issues) == 0 {
		t.Fatalf("expected detected issues for skewed traffic")
	}
}
