package forecast

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func TestFormatDurationShort(t *testing.T) {
	if got := FormatDurationShort(nil); got != "N/A" {
		t.Fatalf("expected N/A, got %q", got)
	}

	sec30 := 30.0
	if got := FormatDurationShort(&sec30); got != "30s" {
		t.Fatalf("expected 30s, got %q", got)
	}

	sec45m := 2700.0
	if got := FormatDurationShort(&sec45m); got != "45m" {
		t.Fatalf("expected 45m, got %q", got)
	}

	sec4h50m := 4*3600.0 + 50*60.0
	if got := FormatDurationShort(&sec4h50m); got != "4h 50m" {
		t.Fatalf("expected 4h 50m, got %q", got)
	}

	sec6d22h := 6*86400.0 + 22*3600.0
	if got := FormatDurationShort(&sec6d22h); got != "6d 22h" {
		t.Fatalf("expected 6d 22h, got %q", got)
	}
}

func TestGetBurnRate(t *testing.T) {
	now := float64(1000000)

	// 1. Idle pool
	pool := storage.NewEmptyPool()
	telemetry := GetBurnRate(pool, now)
	if !telemetry.IsIdle || telemetry.GensPerHour != 0 || telemetry.WindowUsed != "idle" {
		t.Fatalf("expected idle telemetry, got %+v", telemetry)
	}

	// 2. 15m window (e.g. 3 events in last 10 minutes)
	pool.GenerationEvents = []storage.GenerationEvent{
		{TS: int64(now - 100), ID: "acc_1"},
		{TS: int64(now - 200), ID: "acc_1"},
		{TS: int64(now - 300), ID: "acc_2"},
	}
	telemetry = GetBurnRate(pool, now)
	if telemetry.GensLast15M != 3 || telemetry.GensPerHour != 12.0 || telemetry.WindowUsed != "15m" {
		t.Fatalf("expected 15m window with rate 12.0, got %+v", telemetry)
	}

	// 3. Sustained / smoothed window (>= 10 in 1h)
	pool.GenerationEvents = nil
	for i := 0; i < 15; i++ {
		pool.GenerationEvents = append(pool.GenerationEvents, storage.GenerationEvent{
			TS: int64(now - float64(i*100)),
			ID: "acc_1",
		})
	}
	telemetry = GetBurnRate(pool, now)
	if telemetry.WindowUsed != "smoothed" || telemetry.IsIdle {
		t.Fatalf("expected smoothed window, got %+v", telemetry)
	}
}

func TestSimulateQuotaRunwayEmptyOrRestricted(t *testing.T) {
	now := float64(1000000)

	// Empty pool
	empty := storage.NewEmptyPool()
	res := SimulateQuotaRunway(empty, nil, now)
	if res.Status != "no_accounts" {
		t.Fatalf("expected no_accounts status, got %s", res.Status)
	}

	// All restricted
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{
		{ID: "acc_1", Name: "A1", Status: storage.StatusValidationRequired},
		{ID: "acc_2", Name: "A2", Status: storage.StatusAuthError},
	}
	res = SimulateQuotaRunway(pool, nil, now)
	if res.Status != "all_restricted" || res.HealthyCount != 0 || res.RestrictedCount != 2 {
		t.Fatalf("expected all_restricted status, got %+v", res)
	}
}

func TestSimulateQuotaRunwayHealthy(t *testing.T) {
	now := float64(time.Now().Unix())
	pool := storage.NewEmptyPool()
	frac := 0.8
	pool.Accounts = []*storage.Account{
		{
			ID:   "acc_1",
			Name: "Account 1",
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: &frac},
				GeminiWeekly: &storage.QuotaWindow{Fraction: &frac},
			},
		},
		{
			ID:   "acc_2",
			Name: "Account 2",
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: &frac},
				GeminiWeekly: &storage.QuotaWindow{Fraction: &frac},
			},
		},
	}

	// 1. Idle simulation
	res := SimulateQuotaRunway(pool, nil, now)
	if res.Status != "ok" || res.HealthyCount != 2 || !res.IsIdle {
		t.Fatalf("expected ok idle status, got %+v", res)
	}
	if !res.BenchmarkRunway.IsSustainable5H || !res.BenchmarkRunway.IsSustainableWeekly {
		t.Fatalf("expected benchmark runway to be sustainable, got %+v", res.BenchmarkRunway)
	}

	// 2. Custom heavy pace simulation (e.g. 500 gens/hr)
	heavyPace := 500.0
	resHeavy := SimulateQuotaRunway(pool, &heavyPace, now)
	if resHeavy.Status != "ok" || !resHeavy.IsCustomPace {
		t.Fatalf("expected custom pace simulation, got %+v", resHeavy)
	}
	if resHeavy.CurrentRunway.IsSustainable5H {
		t.Fatalf("expected 5h runway to stall under 500 gens/hr, got %+v", resHeavy.CurrentRunway)
	}
	if resHeavy.CurrentRunway.FirstStallSec == nil || *resHeavy.CurrentRunway.FirstStallSec <= 0 {
		t.Fatalf("expected first stall sec to be positive, got %+v", resHeavy.CurrentRunway)
	}

	// 3. PrintRunwaySummary output check
	var buf bytes.Buffer
	PrintRunwaySummary(&buf, pool, now)
	out := buf.String()
	if !strings.Contains(out, "⚡ Quota Runway & Endurance Forecast:") {
		t.Fatalf("expected summary header, got %q", out)
	}
	if !strings.Contains(out, "2/2 Active") {
		t.Fatalf("expected 2/2 Active, got %q", out)
	}
}
