package cli

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func TestStrategyCommand(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	// 1. View strategy
	var stdout, stderr bytes.Buffer
	code := Main([]string{"strategy"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("strategy view expected 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "Current load balancing strategy:") {
		t.Errorf("expected strategy view output, got: %s", stdout.String())
	}

	// 2. Set invalid strategy -> exit 1
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"strategy", "invalid_choice_xyz"}, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1 on invalid strategy, got %d", code)
	}
	if !strings.Contains(stdout.String(), "[Error] Invalid strategy 'invalid_choice_xyz'") {
		t.Errorf("expected invalid strategy error on stdout, got: %s", stdout.String())
	}

	// 3. Set valid strategy -> exit 0
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"strategy", "least_used"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0 on setting valid strategy, got %d", code)
	}
	if !strings.Contains(stdout.String(), "✓ Load balancing strategy set to:") || !strings.Contains(stdout.String(), "least_used") {
		t.Errorf("expected success message, got: %s", stdout.String())
	}

	// Verify persistence in storage
	pool, _ := storage.LoadPool()
	if pool.Strategy != "least_used" {
		t.Errorf("expected strategy 'least_used' in pool, got %q", pool.Strategy)
	}
}

func TestStatusCmd_NoQuotaBarsAndDashboard(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	now := time.Now().Unix()
	frac := 0.85
	acc1 := &storage.Account{
		ID:           "acc_1",
		Email:        "secret1@example.com",
		Name:         "MainAccount",
		Status:       "ready",
		RequestCount: 15,
		CreatedAt:    &now,
		LastQuota: &storage.QuotaState{
			RemainingFraction: &frac,
			Gemini5H:          &storage.QuotaWindow{Fraction: &frac},
			GeminiWeekly:      &storage.QuotaWindow{Fraction: &frac},
		},
	}
	activeID := "acc_1"
	pool := &storage.Pool{
		Version:         1,
		ActiveAccountID: &activeID,
		Strategy:        config.StrategyMaxQuota,
		Accounts:        []*storage.Account{acc1},
	}
	_ = storage.SavePool(pool)

	var stdout, stderr bytes.Buffer
	code := Main([]string{"status", "-p", "19999"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on status, got %d", code)
	}
	out := stdout.String()

	// Must contain status summary fields
	for _, expected := range []string{
		"Gateway :",
		"STOPPED",
		"State   :",
		"Pool    : 1 accounts | CLI Base: MainAccount | 1 Ready | 0 Cooldown | 0 Restricted",
		"Strategy: max_quota",
		"Stopped",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("status output missing expected string %q\nGot: %s", expected, out)
		}
	}

	// Must NOT contain quota progress bars or account cards
	for _, prohibited := range []string{
		"Gemini 5-Hour:",
		"Gemini Weekly:",
		"Quota age:",
		"Hits:",
		"Antigravity Multi-Account Pool",
		"Base account for CLI/auth metadata",
		"Available in rotation pool",
		"[━━━━",
		"[░░░░",
	} {
		if strings.Contains(out, prohibited) {
			t.Errorf("status output must not contain full dashboard element %q\nGot: %s", prohibited, out)
		}
	}
}

func TestStatusCmd_RunningDaemonWithRuntimeStats(t *testing.T) {
	tmpDir, cleanup := setupCLITestEnv(t)
	defer cleanup()

	// Start a local listener to simulate running daemon port
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	ctx, err := daemon.ResolveInstanceContext(tmpDir, "", port, "")
	if err != nil {
		t.Fatalf("ResolveInstanceContext failed: %v", err)
	}

	// Write PID file
	currentPID := os.Getpid()
	pidInfo := daemon.DaemonInfo{
		PID:        currentPID,
		Port:       port,
		DataDir:    ctx.DataDir,
		Version:    ctx.Version,
		ListenHost: ctx.ListenHost,
		Listen:     ctx.ListenHost,
		ConfigPath: ctx.ConfigPath,
		ConfigHash: ctx.ConfigHash,
	}
	if err := daemon.WritePIDFile(ctx.PIDFile(), pidInfo); err != nil {
		t.Fatalf("WritePIDFile failed: %v", err)
	}

	// Write runtime stats with deterministic values
	startedAt := time.Now().Add(-1*time.Hour - 27*time.Minute).Truncate(time.Second)
	stats := daemon.RuntimeStats{
		PID:        currentPID,
		StartedAt:  startedAt,
		UpdatedAt:  time.Now(),
		Goroutines: 18,
		AllocBytes: uint64(124 * 1024 * 1024 / 10),
		HeapBytes:  uint64(187 * 1024 * 1024 / 10),
		SysBytes:   uint64(312 * 1024 * 1024 / 10),
		NumGC:      42,
		GoVersion:  "go1.26.7",
	}
	if err := daemon.WriteRuntimeStats(ctx.RuntimeFile(), stats); err != nil {
		t.Fatalf("WriteRuntimeStats failed: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Main([]string{"status", "-p", strconv.Itoa(port), "-D", tmpDir}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on status, got %d. stderr: %s", code, stderr.String())
	}
	out := stdout.String()

	// Assert required lines from visual shape
	for _, expected := range []string{
		"Gateway :",
		"RUNNING",
		fmt.Sprintf("PID %d", currentPID),
		fmt.Sprintf("127.0.0.1:%d", port),
		"Uptime  : 1h 27m",
		"Runtime : Go 1.26.7 | Goroutines: 18",
		"Memory  : Alloc 12.4 MiB | Heap 18.7 MiB | Sys 31.2 MiB",
		"GC      : 42 cycles",
		"State   :",
		"Pool    : 0 accounts (empty)",
		"Strategy: smart",
		"Healthy",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("status running output missing %q\nGot:\n%s", expected, out)
		}
	}
}

func TestStatusCmd_RunningDaemonStatsFromDaemonNotCLI(t *testing.T) {
	tmpDir, cleanup := setupCLITestEnv(t)
	defer cleanup()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	ctx, err := daemon.ResolveInstanceContext(tmpDir, "", port, "")
	if err != nil {
		t.Fatalf("ResolveInstanceContext failed: %v", err)
	}

	currentPID := os.Getpid()
	pidInfo := daemon.DaemonInfo{
		PID:        currentPID,
		Port:       port,
		DataDir:    ctx.DataDir,
		Version:    ctx.Version,
		ListenHost: ctx.ListenHost,
		Listen:     ctx.ListenHost,
		ConfigPath: ctx.ConfigPath,
		ConfigHash: ctx.ConfigHash,
	}
	if err := daemon.WritePIDFile(ctx.PIDFile(), pidInfo); err != nil {
		t.Fatalf("WritePIDFile failed: %v", err)
	}

	// Mock specific distinct metrics in daemon snapshot
	stats := daemon.RuntimeStats{
		PID:        currentPID,
		StartedAt:  time.Now().Add(-37 * time.Second),
		UpdatedAt:  time.Now(),
		Goroutines: 999, // Highly distinct number to prove it doesn't read CLI process goroutines
		AllocBytes: uint64(50.0 * 1024 * 1024),
		HeapBytes:  uint64(60.0 * 1024 * 1024),
		SysBytes:   uint64(80.0 * 1024 * 1024),
		NumGC:      777,
		GoVersion:  "go1.99.9",
	}
	if err := daemon.WriteRuntimeStats(ctx.RuntimeFile(), stats); err != nil {
		t.Fatalf("WriteRuntimeStats failed: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Main([]string{"status", "-p", strconv.Itoa(port), "-D", tmpDir}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	out := stdout.String()

	// Proves metrics came from daemon runtime snapshot, not CLI process
	if !strings.Contains(out, "Goroutines: 999") {
		t.Errorf("status must use daemon goroutine count (999)\nGot:\n%s", out)
	}
	if !strings.Contains(out, "GC      : 777 cycles") {
		t.Errorf("status must use daemon GC count (777)\nGot:\n%s", out)
	}
	if !strings.Contains(out, "Go 1.99.9") {
		t.Errorf("status must use daemon Go version\nGot:\n%s", out)
	}
	if !strings.Contains(out, "Uptime  : 37s") {
		t.Errorf("status must use daemon uptime (37s)\nGot:\n%s", out)
	}
}

func TestStatusCmd_RunningDaemonStatsUnavailable(t *testing.T) {
	tmpDir, cleanup := setupCLITestEnv(t)
	defer cleanup()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	ctx, err := daemon.ResolveInstanceContext(tmpDir, "", port, "")
	if err != nil {
		t.Fatalf("ResolveInstanceContext failed: %v", err)
	}

	currentPID := os.Getpid()
	pidInfo := daemon.DaemonInfo{
		PID:        currentPID,
		Port:       port,
		DataDir:    ctx.DataDir,
		Version:    ctx.Version,
		ListenHost: ctx.ListenHost,
		Listen:     ctx.ListenHost,
		ConfigPath: ctx.ConfigPath,
		ConfigHash: ctx.ConfigHash,
	}
	if err := daemon.WritePIDFile(ctx.PIDFile(), pidInfo); err != nil {
		t.Fatalf("WritePIDFile failed: %v", err)
	}
	// DO NOT write runtime stats file -> should report metrics unavailable gracefully

	var stdout, stderr bytes.Buffer
	code := Main([]string{"status", "-p", strconv.Itoa(port), "-D", tmpDir}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	out := stdout.String()

	if !strings.Contains(out, "Runtime : (metrics unavailable)") {
		t.Errorf("expected metrics unavailable message\nGot:\n%s", out)
	}
}

func TestListCmd_CompactAndPrivacy(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	now := time.Now().Unix()
	frac := 0.90
	acc1 := &storage.Account{
		ID:           "acc_1",
		Email:        "confidential-user@example.com",
		Name:         "WorkDev",
		Status:       "ready",
		RequestCount: 123,
		CreatedAt:    &now,
		LastQuota: &storage.QuotaState{
			RemainingFraction: &frac,
			Gemini5H:          &storage.QuotaWindow{Fraction: &frac},
			GeminiWeekly:      &storage.QuotaWindow{Fraction: &frac},
		},
	}
	acc2 := &storage.Account{
		ID:           "acc_2",
		Email:        "private-test@gmail.com",
		Status:       "ready", // No name -> should be Account 2
		RequestCount: 45,
		CreatedAt:    &now,
	}
	activeID := "acc_1"
	pool := &storage.Pool{
		Version:         1,
		ActiveAccountID: &activeID,
		Strategy:        config.StrategyMaxQuota,
		Accounts:        []*storage.Account{acc1, acc2},
	}
	_ = storage.SavePool(pool)

	var stdout, stderr bytes.Buffer
	code := Main([]string{"list"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on list, got %d", code)
	}
	out := stdout.String()

	// Table headers and columns
	if !strings.Contains(out, "#") || !strings.Contains(out, "ACCOUNT ID") || !strings.Contains(out, "STATUS") {
		t.Errorf("list output missing table headers\nGot: %s", out)
	}
	if !strings.Contains(out, "acc_1") || !strings.Contains(out, "WorkDev") {
		t.Errorf("list output missing acc_1 or WorkDev\nGot: %s", out)
	}
	if !strings.Contains(out, "acc_2") || !strings.Contains(out, "Account 2") {
		t.Errorf("list output missing acc_2 or Account 2 fallback\nGot: %s", out)
	}

	// PRIVACY: Never expose full email addresses
	for _, email := range []string{
		"confidential-user@example.com",
		"private-test@gmail.com",
		"@example.com",
		"@gmail.com",
	} {
		if strings.Contains(out, email) {
			t.Errorf("list output violates privacy by leaking email %q\nGot: %s", email, out)
		}
	}

	// Compact: should NOT contain progress bar cards
	if strings.Contains(out, "Gemini 5-Hour:") || strings.Contains(out, "[━━━━") {
		t.Errorf("list output should be compact without progress bar cards\nGot: %s", out)
	}
}

func TestQuotaUnknownIsExplicitAcrossLayouts(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{{ID: "acc_1", Name: "UnknownQuota", RequestCount: 1035}}
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}

	for _, width := range []int{120, 80, 50, 35} {
		var out bytes.Buffer
		ListAccountsWithWidth("", width, &out, &bytes.Buffer{})
		text := stripANSI(out.String())
		if strings.Contains(text, "100%") || strings.Contains(text, "████") || strings.Contains(text, "━━━━") {
			t.Fatalf("width %d rendered unknown quota as full: %q", width, text)
		}
		if !strings.Contains(text, "unknown") || !strings.Contains(text, "--") {
			t.Fatalf("width %d omitted unknown quota marker: %q", width, text)
		}
	}
}

func TestQuotaPartialWindowsDisplayIndependently(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()
	fiveHour, weekly := 0.72, 0.52
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{
		{ID: "acc_1", Name: "FiveHourOnly", LastQuota: &storage.QuotaState{Gemini5H: &storage.QuotaWindow{Fraction: &fiveHour}}},
		{ID: "acc_2", Name: "WeeklyOnly", LastQuota: &storage.QuotaState{GeminiWeekly: &storage.QuotaWindow{Fraction: &weekly}}},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	ListAccountsWithWidth("", 80, &out, &bytes.Buffer{})
	text := stripANSI(out.String())
	if !strings.Contains(text, "72%") || !strings.Contains(text, "52%") || strings.Count(text, "unknown") < 2 {
		t.Fatalf("partial windows were not displayed independently: %q", text)
	}
}

func TestQuotaCmd_ContainsProgressBars(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	now := time.Now().Unix()
	frac := 0.70
	acc := &storage.Account{
		ID:           "acc_1",
		Name:         "QuotaTest",
		Status:       "ready",
		RequestCount: 88,
		CreatedAt:    &now,
		LastQuota: &storage.QuotaState{
			RemainingFraction: &frac,
			Gemini5H:          &storage.QuotaWindow{Fraction: &frac},
			GeminiWeekly:      &storage.QuotaWindow{Fraction: &frac},
		},
	}
	activeID := "acc_1"
	pool := &storage.Pool{
		Version:         1,
		ActiveAccountID: &activeID,
		Strategy:        config.StrategyMaxQuota,
		Accounts:        []*storage.Account{acc},
	}
	_ = storage.SavePool(pool)

	var stdout, stderr bytes.Buffer
	code := Main([]string{"quota"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on quota, got %d", code)
	}
	cleanOut := stripANSI(stdout.String())

	// Must contain full quota dashboard elements
	for _, expected := range []string{
		"Antigravity Multi-Account Pool",
		"QuotaTest",
		"hits 88",
		"5H",
		"WK",
		"70%",
		"██████████████░░░░░░",
	} {
		if !strings.Contains(cleanOut, expected) {
			t.Errorf("quota output missing expected dashboard element %q\nGot: %s", expected, cleanOut)
		}
	}

	// Target filtering
	stdout.Reset()
	code = Main([]string{"quota", "1"}, nil, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "QuotaTest") {
		t.Errorf("quota target filtering failed\nGot: %s", stdout.String())
	}
}

func TestAuditRouting_RemovedFromCLI(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	for _, cmd := range []string{"audit-routing", "monitor-routing"} {
		var stdout, stderr bytes.Buffer
		code := Main([]string{cmd}, nil, &stdout, &stderr)
		if code != 2 {
			t.Fatalf("expected code 2 when invoking removed command %q, got %d", cmd, code)
		}
		if !strings.Contains(stderr.String(), fmt.Sprintf("invalid choice: '%s'", cmd)) {
			t.Fatalf("expected 'invalid choice: '%s'' in stderr, got: %s", cmd, stderr.String())
		}
		if strings.Contains(stderr.String(), cmd+",") {
			t.Fatalf("usage list in error output should not contain %q: %s", cmd, stderr.String())
		}
	}

	// Help output must not mention audit-routing or monitor-routing
	var stdout, stderr bytes.Buffer
	code := Main([]string{"--help"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected code 0 for --help, got %d", code)
	}
	for _, term := range []string{"audit-routing", "monitor-routing"} {
		if strings.Contains(stdout.String(), term) {
			t.Fatalf("help text must not mention %q, got: %s", term, stdout.String())
		}
	}
}

func TestListCmd_ShowEmail(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	now := time.Now().Unix()
	pool := &storage.Pool{
		Version: 1,
		Accounts: []*storage.Account{
			{
				ID:        "acc_1",
				Email:     "myuser@example.com",
				Name:      "DevAccount",
				Status:    "ready",
				CreatedAt: &now,
			},
		},
	}
	_ = storage.SavePool(pool)

	for _, flag := range []string{"-e", "--email", "--show-email"} {
		var stdout, stderr bytes.Buffer
		code := Main([]string{"list", flag}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected exit 0 on list %s, got %d", flag, code)
		}
		out := stdout.String()
		if !strings.Contains(out, "EMAIL") {
			t.Errorf("expected EMAIL column header for list %s, got: %s", flag, out)
		}
		if !strings.Contains(out, "myuser@example.com") {
			t.Errorf("expected email address in list %s, got: %s", flag, out)
		}
	}
}

func TestQuotaCmd_ShowEmail(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	now := time.Now().Unix()
	frac := 0.8
	pool := &storage.Pool{
		Version: 1,
		Accounts: []*storage.Account{
			{
				ID:        "acc_1",
				Email:     "dev@example.com",
				Name:      "DevAccount",
				Status:    "ready",
				CreatedAt: &now,
				LastQuota: &storage.QuotaState{RemainingFraction: &frac},
			},
		},
	}
	_ = storage.SavePool(pool)

	for _, flag := range []string{"-e", "--email", "--show-email"} {
		var stdout, stderr bytes.Buffer
		code := Main([]string{"quota", flag}, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected exit 0 on quota %s, got %d", flag, code)
		}
		out := stdout.String()
		if !strings.Contains(out, "Email: dev@example.com") {
			t.Errorf("expected 'Email: dev@example.com' in quota %s, got: %s", flag, out)
		}
	}

	// Without flag, email must NOT appear
	var stdout, stderr bytes.Buffer
	code := Main([]string{"quota"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on quota, got %d", code)
	}
	if strings.Contains(stdout.String(), "dev@example.com") {
		t.Errorf("quota without flag must not contain email, got: %s", stdout.String())
	}
}
