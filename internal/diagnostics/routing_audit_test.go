package diagnostics

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/proxy"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/scheduler"
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
	blockGate := make(chan struct{})
	// Blocked observer that waits on gate
	slowObserver := func(accountName string, status int, attempt int, failoverReason string, path string) {
		atomic.AddInt64(&callCount, 1)
		<-blockGate // simulates blocked / slow auditor
	}

	proxy.SetAuditObserver(slowObserver)
	proxy.ResetDroppedAuditEvents()
	defer func() {
		close(blockGate)
		proxy.SetAuditObserver(nil)
	}()

	start := time.Now()
	// Emitting single event must return immediately without waiting for slowObserver
	ok := proxy.EmitRoutingEvent(proxy.RoutingEvent{
		AccountName: "MockAcc",
		Status:      200,
		Attempt:     1,
		Path:        "/v1internal:streamGenerateContent",
		Timestamp:   time.Now(),
	})
	elapsed := time.Since(start)

	if !ok {
		t.Fatalf("expected first event to be queued successfully")
	}
	if elapsed > 50*time.Millisecond {
		t.Fatalf("audit observer blocked generation path: took %v", elapsed)
	}

	// Overfill the buffer: Emit 1100 events while observer is completely blocked
	for i := 0; i < 1100; i++ {
		proxy.EmitRoutingEvent(proxy.RoutingEvent{
			AccountName: "MockAcc",
			Status:      200,
			Attempt:     1,
			Path:        "/v1internal:streamGenerateContent",
			Timestamp:   time.Now(),
		})
	}
	totalElapsed := time.Since(start)
	if totalElapsed > 100*time.Millisecond {
		t.Fatalf("overfilling bounded buffer blocked caller: took %v", totalElapsed)
	}

	dropped := proxy.DroppedAuditEvents()
	if dropped <= 0 {
		t.Fatalf("expected dropped events counter to be > 0 when buffer is full, got %d", dropped)
	}
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

// Regression Test A: Selection occurs while account is Healthy.
// State later changes to Reserve before worker consumes event.
// Audit must still report the original selection as PASS.
func TestTemporalAudit_A_HealthySelectionRemainsPassAfterStateDegrades(t *testing.T) {
	ev := proxy.RoutingEvent{
		Timestamp:             time.Now(),
		Strategy:              "max_quota",
		Account:               "Work",
		AccountName:           "Work",
		Classification:        "Healthy",
		HealthyCandidateCount: 1,
		HealthyCandidates:     []string{"Work"},
		ReserveCandidateCount: 0,
		FiveHourRemaining:     floatPtr(0.85),
		WeeklyRemaining:       floatPtr(0.90),
		Attempt:               1,
		Status:                200,
		Path:                  "/v1internal:streamGenerateContent",
	}

	// State in storage is later degraded to Reserve
	tmpDir := t.TempDir()
	_ = config.ConfigureDataDir(tmpDir)
	t.Cleanup(config.ResetDataDir)
	now := float64(time.Now().Unix())
	degradedAcc := makeTestAccount("acc_work", "Work", floatPtr(0.05), floatPtr(0.05), now) // Reserve
	_ = storage.SavePool(&storage.Pool{
		Accounts: []*storage.Account{degradedAcc},
		Strategy: "max_quota",
	})

	// Background worker evaluates frozen event
	auditEv := EvaluateFrozenRoutingEvent(ev)
	if auditEv.Verdict != "pass" {
		t.Fatalf("expected verdict 'pass', got %q (%s)", auditEv.Verdict, auditEv.VerdictDetails)
	}
}

// Regression Test B: Selection occurs while account is Reserve and Healthy alternative exists.
// State later changes before worker consumes event.
// Audit must still report VIOLATION.
func TestTemporalAudit_B_ReserveViolationRemainsViolationAfterStateChanges(t *testing.T) {
	ev := proxy.RoutingEvent{
		Timestamp:             time.Now(),
		Strategy:              "max_quota",
		Account:               "Work",
		AccountName:           "Work",
		Classification:        "Reserve",
		HealthyCandidateCount: 1,
		HealthyCandidates:     []string{"Backup"},
		ReserveCandidateCount: 1,
		FiveHourRemaining:     floatPtr(0.10),
		WeeklyRemaining:       floatPtr(0.50),
		Attempt:               1,
		Status:                200,
		Path:                  "/v1internal:streamGenerateContent",
	}

	// Later, state changes in storage so Backup is no longer healthy (all depleted)
	tmpDir := t.TempDir()
	_ = config.ConfigureDataDir(tmpDir)
	t.Cleanup(config.ResetDataDir)
	now := float64(time.Now().Unix())
	depletedAcc1 := makeTestAccount("acc_work", "Work", floatPtr(0.0), floatPtr(0.0), now)
	depletedAcc2 := makeTestAccount("acc_backup", "Backup", floatPtr(0.0), floatPtr(0.0), now)
	_ = storage.SavePool(&storage.Pool{
		Accounts: []*storage.Account{depletedAcc1, depletedAcc2},
		Strategy: "max_quota",
	})

	// Background worker evaluates frozen event
	auditEv := EvaluateFrozenRoutingEvent(ev)
	if auditEv.Verdict != "violation" {
		t.Fatalf("expected verdict 'violation', got %q (%s)", auditEv.Verdict, auditEv.VerdictDetails)
	}
}

// Regression Test C: Events emitted before audit session starts are not reported by the new session.
func TestTemporalAudit_C_PreSessionEventsNotReported(t *testing.T) {
	proxy.SetAuditObserver(nil)
	proxy.SetAuditObserverEvent(nil)
	proxy.ResetDroppedAuditEvents()

	// Emit event prior to session starting
	proxy.EmitRoutingEvent(proxy.RoutingEvent{
		Timestamp:      time.Now(),
		Strategy:       "max_quota",
		Account:        "OldAccount",
		Classification: "Healthy",
		Status:         200,
		Path:           "/v1internal:streamGenerateContent",
	})

	// Start a new audit session via SubscribeAudit
	subEvents, cancel := proxy.SubscribeAudit(100)
	defer cancel()

	// Emit new event after session starts
	proxy.EmitRoutingEvent(proxy.RoutingEvent{
		Timestamp:      time.Now(),
		Strategy:       "max_quota",
		Account:        "NewAccount",
		Classification: "Healthy",
		Status:         200,
		Path:           "/v1internal:streamGenerateContent",
	})

	select {
	case ev := <-subEvents:
		if ev.Account != "NewAccount" {
			t.Fatalf("expected NewAccount, got pre-session event %s", ev.Account)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timed out waiting for session event")
	}

	select {
	case ev := <-subEvents:
		t.Fatalf("unexpected additional event in session: %+v", ev)
	default:
		// Clean
	}
}

// Regression Test D: No active auditor => generation path does not fill audit queue.
func TestTemporalAudit_D_NoActiveAuditorDoesNotFillQueue(t *testing.T) {
	proxy.SetAuditObserver(nil)
	proxy.SetAuditObserverEvent(nil)
	proxy.ResetDroppedAuditEvents()

	if proxy.HasActiveAuditor() {
		t.Fatalf("expected HasActiveAuditor to be false when no observers or subscribers exist")
	}

	initialQueueLen := proxy.AuditQueueLen()

	for i := 0; i < 50; i++ {
		delivered := proxy.EmitRoutingEvent(proxy.RoutingEvent{
			Timestamp:      time.Now(),
			Strategy:       "max_quota",
			Account:        "MockAcc",
			Classification: "Healthy",
			Status:         200,
			Path:           "/v1internal:streamGenerateContent",
		})
		if delivered {
			t.Fatalf("expected EmitRoutingEvent to return false when no auditor is active")
		}
	}

	finalQueueLen := proxy.AuditQueueLen()
	if finalQueueLen != initialQueueLen {
		t.Fatalf("expected audit queue length to remain %d, got %d", initialQueueLen, finalQueueLen)
	}
	if proxy.DroppedAuditEvents() != 0 {
		t.Fatalf("expected dropped events counter to remain 0 when no auditor is active, got %d", proxy.DroppedAuditEvents())
	}
}

// Regression Test E: Blocked/slow consumer cannot delay generation.
func TestTemporalAudit_E_BlockedConsumerCannotDelayGeneration(t *testing.T) {
	gate := make(chan struct{})
	slowObs := func(ev proxy.RoutingEvent) {
		<-gate
	}

	proxy.SetAuditObserverEvent(slowObs)
	defer func() {
		close(gate)
		proxy.SetAuditObserverEvent(nil)
	}()

	start := time.Now()
	proxy.EmitRoutingEvent(proxy.RoutingEvent{
		Timestamp:      time.Now(),
		Strategy:       "max_quota",
		Account:        "Work",
		Classification: "Healthy",
		Status:         200,
		Path:           "/v1internal:streamGenerateContent",
	})
	elapsed := time.Since(start)

	if elapsed > 50*time.Millisecond {
		t.Fatalf("blocked consumer delayed generation path: took %v", elapsed)
	}
}

// Regression Test F: Queue overflow increments dropped-event count without blocking.
func TestTemporalAudit_F_QueueOverflowIncrementsDroppedCountWithoutBlocking(t *testing.T) {
	gate := make(chan struct{})
	slowObs := func(ev proxy.RoutingEvent) {
		<-gate
	}

	proxy.SetAuditObserverEvent(slowObs)
	proxy.ResetDroppedAuditEvents()
	defer func() {
		close(gate)
		proxy.SetAuditObserverEvent(nil)
	}()

	start := time.Now()
	for i := 0; i < 1200; i++ {
		proxy.EmitRoutingEvent(proxy.RoutingEvent{
			Timestamp:      time.Now(),
			Strategy:       "max_quota",
			Account:        "Work",
			Classification: "Healthy",
			Status:         200,
			Path:           "/v1internal:streamGenerateContent",
		})
	}
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Fatalf("overfilling queue blocked: took %v", elapsed)
	}

	dropped := proxy.DroppedAuditEvents()
	if dropped <= 0 {
		t.Fatalf("expected dropped audit events > 0, got %d", dropped)
	}
}

// Test: Freeze identified 5H/Weekly reset timestamps and derived reset-aware ranking inputs
func TestFrozenResetTimestampsAndRankingInputs(t *testing.T) {
	now := float64(1789700000) // Fixed reference epoch
	reset5Str := "2026-09-17T20:00:00Z"
	reset7Str := "2026-09-24T00:00:00Z"
	q5 := 0.85
	qw := 0.92

	acc := &storage.Account{
		ID:     "acc_123",
		Name:   "Work",
		Status: "active",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  &q5,
				ResetTime: reset5Str,
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  &qw,
				ResetTime: reset7Str,
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}

	capState := quota.ComputeCapacityState(acc, now)
	if capState.Reset5Time == nil {
		t.Fatalf("expected non-nil Reset5Time")
	}
	if capState.Reset7Time == nil {
		t.Fatalf("expected non-nil Reset7Time")
	}
	if capState.Reset5Time.Format(time.RFC3339) != reset5Str {
		t.Fatalf("expected Reset5Time RFC3339 %q, got %q", reset5Str, capState.Reset5Time.Format(time.RFC3339))
	}
	if capState.Reset7Time.Format(time.RFC3339) != reset7Str {
		t.Fatalf("expected Reset7Time RFC3339 %q, got %q", reset7Str, capState.Reset7Time.Format(time.RFC3339))
	}
	if capState.Pace5 == nil || capState.Pace7 == nil {
		t.Fatalf("expected non-nil Pace5 and Pace7, got %v, %v", capState.Pace5, capState.Pace7)
	}

	freshness := quota.FreshnessRank(acc, now)
	wp := capState.WorstPace
	tp := capState.TotalPace
	rf := capState.RawFloor
	r5 := capState.R5
	r7 := capState.R7

	ev := proxy.RoutingEvent{
		Timestamp:             time.Unix(int64(now), 0),
		Strategy:              "max_quota",
		Account:               "Work",
		AccountName:           "Work",
		AccountID:             acc.ID,
		Classification:        "Healthy",
		HealthyCandidateCount: 1,
		HealthyCandidates:     []string{"Work"},
		FiveHourKnown:         capState.Q5Known,
		FiveHourRemaining:     capState.Q5,
		FiveHourResetAt:       capState.Reset5Time,
		FiveHourResetRatio:    &r5,
		FiveHourPace:          capState.Pace5,

		WeeklyKnown:      capState.Q7Known,
		WeeklyRemaining:  capState.Q7,
		WeeklyResetAt:    capState.Reset7Time,
		WeeklyResetRatio: &r7,
		WeeklyPace:       capState.Pace7,

		KnownWindowCount: capState.KnownWindowCount,
		FreshnessRank:    &freshness,
		WorstPace:        &wp,
		TotalPace:        &tp,
		RawFloor:         &rf,

		Attempt: 1,
		Status:  200,
		Path:    "/v1internal:streamGenerateContent",
	}

	// Evaluate frozen event
	auditEv := EvaluateFrozenRoutingEvent(ev)
	if auditEv.FiveHourResetAt == nil || auditEv.FiveHourResetAt.Format(time.RFC3339) != reset5Str {
		t.Fatalf("expected FiveHourResetAt %q, got %v", reset5Str, auditEv.FiveHourResetAt)
	}
	if auditEv.WeeklyResetAt == nil || auditEv.WeeklyResetAt.Format(time.RFC3339) != reset7Str {
		t.Fatalf("expected WeeklyResetAt %q, got %v", reset7Str, auditEv.WeeklyResetAt)
	}
	if auditEv.FiveHourPace == nil || *auditEv.FiveHourPace != *capState.Pace5 {
		t.Fatalf("expected FiveHourPace %v, got %v", capState.Pace5, auditEv.FiveHourPace)
	}
	if auditEv.WeeklyPace == nil || *auditEv.WeeklyPace != *capState.Pace7 {
		t.Fatalf("expected WeeklyPace %v, got %v", capState.Pace7, auditEv.WeeklyPace)
	}
	if auditEv.WorstPace == nil || *auditEv.WorstPace != wp {
		t.Fatalf("expected WorstPace %v, got %v", wp, auditEv.WorstPace)
	}
	if auditEv.TotalPace == nil || *auditEv.TotalPace != tp {
		t.Fatalf("expected TotalPace %v, got %v", tp, auditEv.TotalPace)
	}
	if auditEv.RawFloor == nil || *auditEv.RawFloor != rf {
		t.Fatalf("expected RawFloor %v, got %v", rf, auditEv.RawFloor)
	}
	if auditEv.FreshnessRank == nil || *auditEv.FreshnessRank != freshness {
		t.Fatalf("expected FreshnessRank %v, got %v", freshness, auditEv.FreshnessRank)
	}

	// Verify JSON serialization retains all frozen fields with exact names
	b, err := json.Marshal(auditEv)
	if err != nil {
		t.Fatalf("failed to marshal audit event: %v", err)
	}
	jsonStr := string(b)
	for _, key := range []string{
		"five_hour_reset_at",
		"weekly_reset_at",
		"five_hour_pace",
		"weekly_pace",
		"five_hour_reset_ratio",
		"weekly_reset_ratio",
		"worst_pace",
		"total_pace",
		"raw_floor",
		"freshness_rank",
		"healthy_count",
		"reserve_count",
	} {
		if !strings.Contains(jsonStr, key) {
			t.Fatalf("expected JSON to contain %q, got:\n%s", key, jsonStr)
		}
	}
}

// Regression Test I: Session stop unregisters observer cleanly, no stale events leak to next session.
func TestTemporalAudit_I_SessionStopUnregistersAndNoStaleLeak(t *testing.T) {
	proxy.SetAuditObserver(nil)
	proxy.SetAuditObserverEvent(nil)
	proxy.ResetDroppedAuditEvents()

	// Session 1
	sub1, cancel1 := proxy.SubscribeAudit(10)
	if !proxy.HasActiveAuditor() {
		t.Fatalf("expected HasActiveAuditor to be true during session 1")
	}

	proxy.EmitRoutingEvent(proxy.RoutingEvent{
		Timestamp:      time.Now(),
		Strategy:       "max_quota",
		Account:        "Session1Acc",
		Classification: "Healthy",
		Status:         200,
	})

	select {
	case ev := <-sub1:
		if ev.Account != "Session1Acc" {
			t.Fatalf("expected Session1Acc, got %s", ev.Account)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("session 1 timed out waiting for event")
	}

	// Stop session 1
	cancel1()

	if proxy.HasActiveAuditor() {
		t.Fatalf("expected HasActiveAuditor to be false after session 1 stopped")
	}

	// Emit events while stopped
	for i := 0; i < 5; i++ {
		proxy.EmitRoutingEvent(proxy.RoutingEvent{
			Timestamp:      time.Now(),
			Strategy:       "max_quota",
			Account:        "StaleAcc",
			Classification: "Healthy",
			Status:         200,
		})
	}

	// Session 2 starts
	sub2, cancel2 := proxy.SubscribeAudit(10)
	defer cancel2()

	if !proxy.HasActiveAuditor() {
		t.Fatalf("expected HasActiveAuditor to be true during session 2")
	}

	// Ensure no stale events are delivered to session 2
	select {
	case ev := <-sub2:
		t.Fatalf("unexpected stale event leaked to session 2: %+v", ev)
	case <-time.After(50 * time.Millisecond):
		// Clean
	}

	// Emit new event for session 2
	proxy.EmitRoutingEvent(proxy.RoutingEvent{
		Timestamp:      time.Now(),
		Strategy:       "max_quota",
		Account:        "Session2Acc",
		Classification: "Healthy",
		Status:         200,
	})

	select {
	case ev := <-sub2:
		if ev.Account != "Session2Acc" {
			t.Fatalf("expected Session2Acc, got %s", ev.Account)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("session 2 timed out waiting for event")
	}
}

// Regression Test J: Privacy: event structs/output contain no email/token/auth headers/request body/prompt/response.
func TestTemporalAudit_J_PrivacyNoSensitiveData(t *testing.T) {
	now := float64(time.Now().Unix())
	secretEmail := "developer-private@example.com"
	secretToken := "ya29.secret_token_12345"

	acc := &storage.Account{
		ID:           "acc_secret",
		Name:         "ProductionWork",
		Email:        secretEmail,
		AccessToken:  secretToken,
		RefreshToken: secretToken,
		LastQuota: &storage.QuotaState{
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}

	capState := quota.ComputeCapacityState(acc, now)
	disp := accounts.DisplayAccountName(acc)
	freshness := quota.FreshnessRank(acc, now)

	ev := proxy.RoutingEvent{
		Timestamp:             time.Now(),
		Strategy:              "max_quota",
		Account:               disp,
		AccountName:           disp,
		AccountID:             acc.ID,
		Classification:        "Healthy",
		HealthyCandidateCount: 1,
		HealthyCandidates:     []string{disp},
		KnownWindowCount:      capState.KnownWindowCount,
		FreshnessRank:         &freshness,
		Attempt:               1,
		Status:                200,
		Path:                  "/v1internal:streamGenerateContent",
	}

	auditEv := EvaluateFrozenRoutingEvent(ev)

	evJSON, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("failed to marshal RoutingEvent: %v", err)
	}
	auditEvJSON, err := json.Marshal(auditEv)
	if err != nil {
		t.Fatalf("failed to marshal RoutingAuditEvent: %v", err)
	}

	for _, sensitive := range []string{secretEmail, secretToken, "Authorization", "bearer", "prompt", "response"} {
		if strings.Contains(string(evJSON), sensitive) {
			t.Fatalf("RoutingEvent leaked sensitive data %q:\n%s", sensitive, string(evJSON))
		}
		if strings.Contains(string(auditEvJSON), sensitive) {
			t.Fatalf("RoutingAuditEvent leaked sensitive data %q:\n%s", sensitive, string(auditEvJSON))
		}
	}
}

// Regression Test K: a0b5003 legacy semantics remain intact:
// - 15% reserve floor only for positively identified 5H window
// - 10% reserve floor only for positively identified Weekly window
// - legacy/generic/unidentified values retain previous ranking/freshness/depletion behavior
func TestTemporalAudit_K_LegacySemanticsIntact(t *testing.T) {
	now := float64(time.Now().Unix())
	low5H := 0.15
	healthyWeekly := 0.80

	// Positively identified 5H at 15% -> Reserve
	acc5H := &storage.Account{
		ID:   "acc_5h",
		Name: "Identified5H",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  &low5H,
				ResetTime: "2026-09-17T20:00:00Z",
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  &healthyWeekly,
				ResetTime: "2026-09-24T00:00:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}
	cap5H := quota.ComputeCapacityState(acc5H, now)
	if !scheduler.IsReserveCapacity(cap5H) {
		t.Fatalf("expected identified 5H at 15%% to be Reserve candidate")
	}

	// Positively identified Weekly at 10% -> Reserve
	lowWk := 0.10
	accWk := &storage.Account{
		ID:   "acc_wk",
		Name: "IdentifiedWeekly",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  &healthyWeekly,
				ResetTime: "2026-09-17T20:00:00Z",
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  &lowWk,
				ResetTime: "2026-09-24T00:00:00Z",
			},
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}
	capWk := quota.ComputeCapacityState(accWk, now)
	if !scheduler.IsReserveCapacity(capWk) {
		t.Fatalf("expected identified Weekly at 10%% to be Reserve candidate")
	}

	// Legacy single fraction at 12% (no window identity) -> NOT reserve
	legacyFrac := 0.12
	accLegacy := &storage.Account{
		ID:   "acc_legacy",
		Name: "LegacyAccount",
		LastQuota: &storage.QuotaState{
			RemainingFraction: &legacyFrac,
			UpdatedAt:         int64Ptr(int64(now - 10)),
		},
	}
	capLegacy := quota.ComputeCapacityState(accLegacy, now)
	if scheduler.IsReserveCapacity(capLegacy) {
		t.Fatalf("expected legacy unidentified fraction at 12%% to NOT be Reserve candidate")
	}
	if !capLegacy.LegacyFractionKnown {
		t.Fatalf("expected LegacyFractionKnown to be true for legacy quota")
	}
}
