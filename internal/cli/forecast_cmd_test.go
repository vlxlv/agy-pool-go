package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func TestForecastCmd_EmptyPool(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	// Plain text
	var buf bytes.Buffer
	err := ForecastCmd([]string{}, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "No accounts in pool yet") {
		t.Fatalf("expected empty pool warning, got: %s", buf.String())
	}

	// JSON output
	buf.Reset()
	err = ForecastCmd([]string{"--json"}, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if res["status"] != "no_accounts" {
		t.Fatalf("expected status no_accounts, got %v", res["status"])
	}
}

func TestForecastCmd_AllRestricted(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{
		{ID: "acc_1", Name: "Account 1", Status: storage.StatusValidationRequired},
		{ID: "acc_2", Name: "Account 2", Status: storage.StatusAuthError},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	var buf bytes.Buffer
	err := ForecastCmd([]string{}, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "All accounts are restricted") {
		t.Fatalf("expected all restricted warning, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "agy-pool verify") {
		t.Fatalf("expected agy-pool verify hint, got: %s", buf.String())
	}
}

func TestForecastCmd_NormalAccountsAndPace(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	now := float64(time.Now().Unix())
	pool := storage.NewEmptyPool()
	frac := 0.75
	reset5 := now + 7200
	resetW := now + 86400*3

	pool.Accounts = []*storage.Account{
		{
			ID:   "acc_1",
			Name: "Main",
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: &frac, ResetTime: reset5},
				GeminiWeekly: &storage.QuotaWindow{Fraction: &frac, ResetTime: resetW},
			},
		},
		{
			ID:   "acc_2",
			Name: "Backup",
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: &frac, ResetTime: reset5},
				GeminiWeekly: &storage.QuotaWindow{Fraction: &frac, ResetTime: resetW},
			},
		},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	// 1. Default idle / reference pace output
	var buf bytes.Buffer
	err := ForecastCmd([]string{}, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	plain := StripANSI(out)
	if !strings.Contains(plain, "Quota Runway & Endurance Forecast") {
		t.Fatalf("expected forecast header, got: %s", out)
	}
	if !strings.Contains(plain, "2/2 Accounts Active") {
		t.Fatalf("expected 2/2 Accounts Active, got: %s", plain)
	}
	if !strings.Contains(plain, "5-Hour Rolling Burst Runway") {
		t.Fatalf("expected 5h section, got: %s", out)
	}
	if !strings.Contains(plain, "Weekly Budget Runway") {
		t.Fatalf("expected weekly section, got: %s", out)
	}
	if !strings.Contains(plain, "Staggered Reset Timeline") {
		t.Fatalf("expected timeline section, got: %s", out)
	}

	// 2. Custom pace argument
	buf.Reset()
	err = ForecastCmd([]string{"--pace", "25.5"}, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	outPace := StripANSI(buf.String())
	if !strings.Contains(outPace, "25.5 generations/hour") {
		t.Fatalf("expected 25.5 pace in output, got: %s", outPace)
	}
	if !strings.Contains(outPace, "User Specified") {
		t.Fatalf("expected User Specified mode, got: %s", outPace)
	}

	// 3. JSON output
	buf.Reset()
	err = ForecastCmd([]string{"--json"}, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatalf("invalid json output: %v", err)
	}
	if res["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", res["status"])
	}
	if res["healthy_count"].(float64) != 2 {
		t.Fatalf("expected healthy_count 2, got %v", res["healthy_count"])
	}
}

func TestRunCLI_ForecastAliases(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	pool := storage.NewEmptyPool()
	frac := 0.9
	pool.Accounts = []*storage.Account{
		{
			ID:   "acc_1",
			Name: "TestAcc",
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: &frac},
				GeminiWeekly: &storage.QuotaWindow{Fraction: &frac},
			},
		},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	for _, cmdName := range []string{"forecast", "runway", "predict"} {
		var stdout, stderr bytes.Buffer
		code := Main([]string{cmdName, "--json"}, strings.NewReader(""), &stdout, &stderr)
		if code != 0 {
			t.Fatalf("%s failed with code %d: stderr=%s", cmdName, code, stderr.String())
		}
		var res map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
			t.Fatalf("%s returned invalid json: %v", cmdName, err)
		}
		if res["status"] != "ok" {
			t.Fatalf("%s expected status ok, got %v", cmdName, res["status"])
		}
	}
}
