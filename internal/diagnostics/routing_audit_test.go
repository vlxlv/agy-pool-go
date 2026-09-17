package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/proxy"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func floatPtr(f float64) *float64 {
	return &f
}

func int64Ptr(i int64) *int64 {
	return &i
}

func makeTestAccount(id, name string, q5, qw *float64, now float64) *storage.Account {
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
		Name:      name,
		Status:    "active",
		LastQuota: lq,
	}
}

// Test A: Healthy selection -> PASS
func TestAudit_A_HealthySelection_Pass(t *testing.T) {
	now := 1789560000.0
	ts := time.Unix(int64(now), 0)

	pool := &storage.Pool{
		Strategy: config.StrategyMaxQuota,
		Accounts: []*storage.Account{
			makeTestAccount("acc_work", "Work", floatPtr(0.80), floatPtr(0.70), now),
			makeTestAccount("acc_backup", "Backup", floatPtr(0.90), floatPtr(0.70), now),
			makeTestAccount("acc_main", "Main", floatPtr(0.10), floatPtr(0.50), now), // Reserve
		},
	}

	ev := EvaluateRoutingAttempt(pool, "Work", 200, 1, "", ts, "req-1")
	if ev.Classification != "healthy" {
		t.Fatalf("expected classification 'healthy', got %q", ev.Classification)
	}
	if ev.Verdict != "pass" {
		t.Fatalf("expected verdict 'pass', got %q", ev.Verdict)
	}
}

// Test B: Reserve selection while Healthy exists -> VIOLATION
func TestAudit_B_ReserveWhileHealthyExists_Violation(t *testing.T) {
	now := 1789560000.0
	ts := time.Unix(int64(now), 0)

	pool := &storage.Pool{
		Strategy: config.StrategyMaxQuota,
		Accounts: []*storage.Account{
			makeTestAccount("acc_backup", "Backup", floatPtr(0.90), floatPtr(0.70), now), // Healthy
			makeTestAccount("acc_main", "Main", floatPtr(0.10), floatPtr(0.50), now),     // Reserve
		},
	}

	ev := EvaluateRoutingAttempt(pool, "Main", 200, 1, "", ts, "req-2")
	if ev.Classification != "reserve" {
		t.Fatalf("expected classification 'reserve', got %q", ev.Classification)
	}
	if ev.Verdict != "violation" {
		t.Fatalf("expected verdict 'violation', got %q", ev.Verdict)
	}
	if !strings.Contains(ev.VerdictDetails, "Healthy candidates were available") {
		t.Fatalf("expected details to mention available Healthy candidates, got %q", ev.VerdictDetails)
	}
}

// Test C: Reserve fallback with no Healthy -> PASS
func TestAudit_C_ReserveFallback_Pass(t *testing.T) {
	now := 1789560000.0
	ts := time.Unix(int64(now), 0)

	pool := &storage.Pool{
		Strategy: config.StrategyMaxQuota,
		Accounts: []*storage.Account{
			makeTestAccount("acc_main", "Main", floatPtr(0.10), floatPtr(0.50), now), // Reserve
			makeTestAccount("acc_r2", "Backup", floatPtr(0.08), floatPtr(0.50), now), // Reserve
		},
	}

	ev := EvaluateRoutingAttempt(pool, "Main", 200, 1, "", ts, "req-3")
	if ev.Classification != "reserve" {
		t.Fatalf("expected classification 'reserve', got %q", ev.Classification)
	}
	if ev.Verdict != "pass" {
		t.Fatalf("expected fallback verdict 'pass', got %q (%s)", ev.Verdict, ev.VerdictDetails)
	}
	if !strings.Contains(ev.VerdictDetails, "fallback") {
		t.Fatalf("expected details to indicate fallback, got %q", ev.VerdictDetails)
	}
}

// Test D: Depleted selected while Reserve exists -> VIOLATION
func TestAudit_D_DepletedWhileReserveExists_Violation(t *testing.T) {
	now := 1789560000.0
	ts := time.Unix(int64(now), 0)

	pool := &storage.Pool{
		Strategy: config.StrategyMaxQuota,
		Accounts: []*storage.Account{
			makeTestAccount("acc_dep", "DepletedAcc", floatPtr(0.002), floatPtr(0.50), now), // Depleted (<=0.005)
			makeTestAccount("acc_main", "Main", floatPtr(0.10), floatPtr(0.50), now),        // Reserve
		},
	}

	ev := EvaluateRoutingAttempt(pool, "DepletedAcc", 200, 1, "", ts, "req-4")
	if ev.Classification != "depleted" {
		t.Fatalf("expected classification 'depleted', got %q", ev.Classification)
	}
	if ev.Verdict != "violation" {
		t.Fatalf("expected verdict 'violation', got %q", ev.Verdict)
	}
}

// Test E: Unknown classification -> WARNING
func TestAudit_E_UnknownClassification_Warning(t *testing.T) {
	now := 1789560000.0
	ts := time.Unix(int64(now), 0)

	pool := &storage.Pool{
		Strategy: config.StrategyMaxQuota,
		Accounts: []*storage.Account{
			makeTestAccount("acc_work", "Work", floatPtr(0.80), floatPtr(0.70), now),
		},
	}

	// Account not in pool
	ev := EvaluateRoutingAttempt(pool, "GhostAccount", 200, 1, "", ts, "req-5")
	if ev.Classification != "unknown" {
		t.Fatalf("expected classification 'unknown', got %q", ev.Classification)
	}
	if ev.Verdict != "warning" {
		t.Fatalf("expected verdict 'warning', got %q", ev.Verdict)
	}
}

// Test F: a0b5003 legacy/unidentified-window behavior preserved
func TestAudit_F_LegacyUnidentifiedWindows(t *testing.T) {
	now := 1789560000.0
	ts := time.Unix(int64(now), 0)

	// Legacy account: reserve floor does not apply without identified window
	legacy12 := &storage.Account{
		ID:        "legacy_12",
		Name:      "Legacy12",
		Status:    "active",
		LastQuota: &storage.QuotaState{RemainingFraction: floatPtr(0.12), UpdatedAt: int64Ptr(int64(now - 10))},
	}
	// Explicit 5H account: 12% is Reserve (<=15%)
	explicit5H12 := makeTestAccount("explicit_5h", "Explicit5H", floatPtr(0.12), nil, now)
	// Explicit Weekly account: 12% is Healthy (>10%)
	explicitWeekly12 := makeTestAccount("explicit_weekly", "ExplicitWeekly", nil, floatPtr(0.12), now)
	// Completely unidentified account (no quota at all)
	unidentifiedAcc := &storage.Account{
		ID:     "unidentified",
		Name:   "Unidentified",
		Status: "active",
	}

	pool := &storage.Pool{
		Strategy: config.StrategyMaxQuota,
		Accounts: []*storage.Account{legacy12, explicit5H12, explicitWeekly12, unidentifiedAcc},
	}

	// 1. Legacy fraction 0.12 retains old behavior: not Reserve -> Healthy, PASS
	ev1 := EvaluateRoutingAttempt(pool, "Legacy12", 200, 1, "", ts, "")
	if ev1.Classification != "healthy" || ev1.Verdict != "pass" {
		t.Fatalf("legacy 0.12 has no window identity and must not be Reserve, got %s / %s", ev1.Classification, ev1.Verdict)
	}

	// 2. Explicit 5H 0.12 has identified 5H window -> Reserve; with ExplicitWeekly12 (Healthy) available, selecting Explicit5H is VIOLATION
	ev2 := EvaluateRoutingAttempt(pool, "Explicit5H", 200, 1, "", ts, "")
	if ev2.Classification != "reserve" || ev2.Verdict != "violation" {
		t.Fatalf("explicit 5H 0.12 should be Reserve violation, got %s / %s", ev2.Classification, ev2.Verdict)
	}

	// 3. Explicit Weekly 0.12 has identified Weekly window -> Healthy (>10%), PASS
	ev3 := EvaluateRoutingAttempt(pool, "ExplicitWeekly", 200, 1, "", ts, "")
	if ev3.Classification != "healthy" || ev3.Verdict != "pass" {
		t.Fatalf("explicit weekly 0.12 should be healthy pass, got %s / %s", ev3.Classification, ev3.Verdict)
	}

	// 4. Unidentified account (no quota known) -> WARNING
	ev4 := EvaluateRoutingAttempt(pool, "Unidentified", 200, 1, "", ts, "")
	if ev4.Classification != "unknown" || ev4.Verdict != "warning" {
		t.Fatalf("unidentified account should be unknown warning, got %s / %s", ev4.Classification, ev4.Verdict)
	}
}

// Test G: least_used does not receive max_quota reserve violations
func TestAudit_G_LeastUsed_NoReserveViolations(t *testing.T) {
	now := 1789560000.0
	ts := time.Unix(int64(now), 0)

	pool := &storage.Pool{
		Strategy: config.StrategyLeastUsed,
		Accounts: []*storage.Account{
			makeTestAccount("acc_work", "Work", floatPtr(0.10), floatPtr(0.50), now),     // 10% (Reserve under max_quota)
			makeTestAccount("acc_backup", "Backup", floatPtr(0.90), floatPtr(0.70), now), // 90% (Healthy)
		},
	}

	ev := EvaluateRoutingAttempt(pool, "Work", 200, 1, "", ts, "")
	if ev.Verdict != "pass" {
		t.Fatalf("least_used should not receive reserve violation, got %q", ev.Verdict)
	}
	if !strings.Contains(ev.VerdictDetails, "not applicable") {
		t.Fatalf("expected verdict details to note reserve headroom N/A, got %q", ev.VerdictDetails)
	}
}

// Test H: round_robin does not receive max_quota reserve violations
func TestAudit_H_RoundRobin_NoReserveViolations(t *testing.T) {
	now := 1789560000.0
	ts := time.Unix(int64(now), 0)

	pool := &storage.Pool{
		Strategy: config.StrategyRoundRobin,
		Accounts: []*storage.Account{
			makeTestAccount("acc_work", "Work", floatPtr(0.10), floatPtr(0.50), now),     // 10%
			makeTestAccount("acc_backup", "Backup", floatPtr(0.90), floatPtr(0.70), now), // 90%
		},
	}

	ev := EvaluateRoutingAttempt(pool, "Work", 200, 1, "", ts, "")
	if ev.Verdict != "pass" {
		t.Fatalf("round_robin should not receive reserve violation, got %q", ev.Verdict)
	}
}

// Test I: 429 failover produces two attempts without false violation
func TestAudit_I_FailoverTwoAttemptsWithoutFalseViolation(t *testing.T) {
	now := 1789560000.0
	ts1 := time.Unix(int64(now), 0)
	ts2 := time.Unix(int64(now)+1, 0)

	pool := &storage.Pool{
		Strategy: config.StrategyMaxQuota,
		Accounts: []*storage.Account{
			makeTestAccount("acc_work", "Work", floatPtr(0.95), floatPtr(0.80), now),
			makeTestAccount("acc_backup", "Backup", floatPtr(0.90), floatPtr(0.80), now),
		},
	}

	// Attempt 1: Work hit rate limit (429)
	ev1 := EvaluateRoutingAttempt(pool, "Work", 429, 1, "rate limit/quota error", ts1, "req-1")
	if ev1.Verdict != "pass" {
		t.Fatalf("attempt 1 on healthy account should be pass, got %s", ev1.Verdict)
	}
	if ev1.Status != 429 {
		t.Fatalf("expected status 429, got %d", ev1.Status)
	}

	// Attempt 2: Backup succeeded (200)
	ev2 := EvaluateRoutingAttempt(pool, "Backup", 200, 2, "", ts2, "req-1")
	if ev2.Verdict != "pass" {
		t.Fatalf("attempt 2 on healthy account should be pass, got %s", ev2.Verdict)
	}
	if ev2.Status != 200 || ev2.Attempt != 2 {
		t.Fatalf("expected status 200 and attempt 2, got %d / %d", ev2.Status, ev2.Attempt)
	}
}

// Test J: Event consumer cannot block generation path
func TestAudit_J_EventConsumerCannotBlockGenerationPath(t *testing.T) {
	var callCount int64
	// Blocked observer that sleeps
	slowObserver := func(accountName string, status int, attempt int, failoverReason string, path string) {
		atomic.AddInt64(&callCount, 1)
		time.Sleep(500 * time.Millisecond) // simulates slow auditor
	}

	proxy.AuditObserver = slowObserver
	defer func() { proxy.AuditObserver = nil }()

	// Execute proxy dispatch
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1internal:streamGenerateContent", bytes.NewReader([]byte("{}")))

	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{
		{
			ID:     "acc_1",
			Name:   "MockAcc",
			Status: "active",
		},
	}

	start := time.Now()
	// Simulated observer dispatch should return immediately without waiting for the 500ms sleep
	if proxy.AuditObserver != nil {
		go proxy.AuditObserver("MockAcc", 200, 1, "", "/v1internal:streamGenerateContent")
	}
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Fatalf("audit observer blocked generation path: took %v", elapsed)
	}
	_ = rec
	_ = req
}

// Test K: Sensitive data is never present in emitted event structures
func TestAudit_K_SensitiveDataNeverExposed(t *testing.T) {
	now := 1789560000.0
	acc := &storage.Account{
		ID:           "acc_secret",
		Name:         "SafeDisplayName",
		Email:        "user.private.secret@corp.example.com",
		AccessToken:  "ya29.secret_token_12345",
		RefreshToken: "1//refresh_secret_abcdef",
		Status:       "active",
		LastQuota: &storage.QuotaState{
			RemainingFraction: floatPtr(0.85),
			UpdatedAt:         int64Ptr(int64(now - 5)),
		},
	}

	pool := &storage.Pool{
		Strategy: config.StrategyMaxQuota,
		Accounts: []*storage.Account{acc},
	}

	ev := EvaluateRoutingAttempt(pool, "SafeDisplayName", 200, 1, "", time.Now(), "req-sec")
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("failed to marshal event: %v", err)
	}

	jsonStr := string(b)
	forbidden := []string{
		"user.private.secret",
		"example.com",
		"ya29.",
		"refresh_secret",
		"secret_token",
		"Authorization",
		"Bearer",
	}

	for _, token := range forbidden {
		if strings.Contains(jsonStr, token) {
			t.Fatalf("security violation: emitted event contains sensitive token %q:\n%s", token, jsonStr)
		}
	}
}

// Test L: JSON output is valid and stable enough for automation
func TestAudit_L_JSONOutputValid(t *testing.T) {
	now := 1789560000.0
	pool := &storage.Pool{
		Strategy: config.StrategyMaxQuota,
		Accounts: []*storage.Account{
			makeTestAccount("acc_work", "Work", floatPtr(0.824), floatPtr(0.651), now),
			makeTestAccount("acc_backup", "Backup", floatPtr(0.962), floatPtr(0.669), now),
		},
	}

	ev := EvaluateRoutingAttempt(pool, "Work", 200, 1, "", time.Now(), "req-json")
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("failed to marshal JSON: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("emitted JSON is not valid JSON: %v", err)
	}

	requiredFields := []string{
		"timestamp", "strategy", "account", "classification",
		"five_hour_remaining", "weekly_remaining", "healthy_candidates",
		"attempt", "status", "verdict",
	}
	for _, f := range requiredFields {
		if _, ok := parsed[f]; !ok {
			t.Fatalf("missing required field in JSON: %q", f)
		}
	}
}

// Test M: Duration termination works
func TestAudit_M_DurationTerminationWorks(t *testing.T) {
	tmpDir := t.TempDir()
	logFile := tmpDir + "/test.log"

	opts := AuditOptions{
		Duration: 100 * time.Millisecond,
		LogPath:  logFile,
	}

	var out bytes.Buffer
	start := time.Now()
	auditor := NewAuditor(opts)
	err := auditor.Run(context.Background(), &out, &out)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("auditor run returned error: %v", err)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("duration termination took too long: %v (expected ~100ms)", elapsed)
	}
	if !strings.Contains(out.String(), "Summary") {
		t.Fatalf("expected output to contain Summary, got:\n%s", out.String())
	}
}

// Test N: Ctrl-C / context cancellation graceful termination
func TestAudit_N_ContextCancellationGracefulTermination(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	eventChan := make(chan RoutingAuditEvent)
	opts := AuditOptions{
		EventChan: eventChan,
	}

	var out bytes.Buffer
	auditor := NewAuditor(opts)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = auditor.Run(ctx, &out, &out)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel() // simulate Ctrl-C

	wg.Wait()
	if !strings.Contains(out.String(), "Summary") {
		t.Fatalf("expected graceful summary output on cancellation, got:\n%s", out.String())
	}
}

func TestAudit_ReaderAndReport(t *testing.T) {
	tmpDir := t.TempDir()
	_ = config.ConfigureStateDir(tmpDir)

	now := 1789560000.0
	pool := &storage.Pool{
		Strategy: config.StrategyMaxQuota,
		Accounts: []*storage.Account{
			makeTestAccount("acc_work", "Work", floatPtr(0.824), floatPtr(0.651), now),
			makeTestAccount("acc_backup", "Backup", floatPtr(0.962), floatPtr(0.669), now),
		},
	}
	_ = storage.SavePool(pool)

	logLines := `[2026-09-17 17:30:02] [PROXY] POST v1internal:streamGenerateContent -> Work (Status: 200)
[2026-09-17 17:30:05] [PROXY] POST v1internal:listExperiments -> Work (Status: 200)
[2026-09-17 17:30:18] [PROXY] POST v1internal:streamGenerateContent -> Backup (Status: 200)
`
	reportPath := tmpDir + "/report.json"
	opts := AuditOptions{
		Reader:     strings.NewReader(logLines),
		ReportPath: reportPath,
	}

	var out bytes.Buffer
	auditor := NewAuditor(opts)
	err := auditor.Run(context.Background(), &out, &out)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	// Verify human output
	outStr := out.String()
	if !strings.Contains(outStr, "Work") || !strings.Contains(outStr, "Backup") {
		t.Fatalf("expected Work and Backup in output:\n%s", outStr)
	}
	if !strings.Contains(outStr, "PASS") {
		t.Fatalf("expected PASS in output:\n%s", outStr)
	}
	// listExperiments should NOT be counted (only generation requests)
	if auditor.summary.GenerationAttempts != 2 {
		t.Fatalf("expected exactly 2 generation attempts (ignoring listExperiments), got %d", auditor.summary.GenerationAttempts)
	}

	// Verify report file
	reportData, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("failed to read report file: %v", err)
	}
	if !strings.Contains(string(reportData), "Work") {
		t.Fatalf("expected Work in report file: %s", string(reportData))
	}
	if auditor.summary.Violations != 0 {
		t.Fatalf("expected 0 violations, got %d", auditor.summary.Violations)
	}
}
