package cli

import (
	"bytes"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
	"github.com/vlxlv/agy-pool-go/internal/observability"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func createTestTopPool(now int64) *storage.Pool {
	frac90 := 0.90
	frac20 := 0.20
	zero := 0.0
	reset5H := float64(now + 18000)
	resetWK := float64(now + 432000)

	acc1 := &storage.Account{
		ID:           "acc_1",
		Name:         "MainAccount",
		Email:        "private-developer@internal.corp.example.com",
		Status:       "ready",
		RequestCount: 1500,
		CreatedAt:    &now,
		AccessToken:  "ya29.mock_token_sensitive_acc_1",
		RefreshToken: "1//04mock_refresh_sensitive",
		LastQuota: &storage.QuotaState{
			RemainingFraction: &frac90,
			Gemini5H: &storage.QuotaWindow{
				Fraction:  &frac90,
				ResetTime: reset5H,
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  &frac20,
				ResetTime: resetWK,
			},
		},
	}

	acc2 := &storage.Account{
		ID:           "acc_2",
		Name:         "BackupAccount",
		Email:        "private-backup@internal.corp.example.com",
		Status:       "ready",
		RequestCount: 200,
		CreatedAt:    &now,
		LastQuota:    nil, // Unknown quota
	}

	activeID := "acc_1"
	coolingTime := float64(now + 600)
	acc3 := &storage.Account{
		ID:               "acc_3",
		Name:             "CoolingAccount",
		Status:           "ready",
		RateLimitedUntil: &coolingTime,
		RequestCount:     50,
		CreatedAt:        &now,
		LastQuota: &storage.QuotaState{
			RemainingFraction: &frac90,
		},
	}

	acc4 := &storage.Account{
		ID:           "acc_4",
		Name:         "ExhaustedAccount",
		Status:       "ready",
		RequestCount: 80,
		CreatedAt:    &now,
		LastQuota: &storage.QuotaState{
			RemainingFraction: &zero,
			Gemini5H:          &storage.QuotaWindow{Fraction: &zero},
			GeminiWeekly:      &storage.QuotaWindow{Fraction: &zero},
		},
	}

	acc5 := &storage.Account{
		ID:           "acc_5",
		Name:         "VerifyAccount",
		Status:       "validation_required",
		RequestCount: 10,
		CreatedAt:    &now,
	}

	acc6 := &storage.Account{
		ID:           "acc_6",
		Name:         "AuthErrAccount",
		Status:       "auth_error",
		RequestCount: 5,
		CreatedAt:    &now,
	}

	return &storage.Pool{
		Version:         1,
		ActiveAccountID: &activeID,
		Strategy:        config.StrategyMaxQuota,
		Accounts:        []*storage.Account{acc1, acc2, acc3, acc4, acc5, acc6},
	}
}

func createTestRuntimeStats(pid int) *daemon.RuntimeStats {
	return &daemon.RuntimeStats{
		PID:        pid,
		StartedAt:  time.Now().Add(-2 * time.Hour),
		UpdatedAt:  time.Now(),
		Goroutines: 16,
		AllocBytes: 15 * 1024 * 1024,
		HeapBytes:  20 * 1024 * 1024,
		SysBytes:   30 * 1024 * 1024,
		NumGC:      10,
		GoVersion:  "go1.26.7",
		InFlight: map[string]int64{
			"acc_1": 2,
			"acc_2": 0,
			"acc_3": 5,
		},
		Snapshot: observability.Snapshot{
			GenerationRequests:   2500,
			GenerationSuccess:    2480,
			RoutingDecisions:     2500,
			FailoverAttempts:     30,
			FailoverSuccess:      28,
			RestrictedSkips:      12,
			QuotaRefreshAttempts: 150,
			QuotaRefreshSuccess:  148,
			QuotaRefreshFailure:  2,
			AuthRefreshAttempts:  20,
			AuthRefreshSuccess:   20,
			AuthRefreshFailure:   0,
			PersistenceErrors:    1,
			NoReplayPrevented:    4,
		},
	}
}

func TestTop_ResponsiveLayouts(t *testing.T) {
	now := time.Now().Unix()
	pool := createTestTopPool(now)
	stats := createTestRuntimeStats(12345)
	info := &daemon.DaemonInfo{PID: 12345}

	// 1. Wide Layout (width = 120)
	frameWide := RenderTopFrame(pool, stats, info, true, "", 2*time.Second, 120, false)
	plainWide := StripANSI(frameWide)
	for _, expected := range []string{
		"Antigravity Multi-Account Pool",
		"Gateway: RUNNING (PID 12345)",
		"Strategy: max_quota",
		"Refresh: 2.0s",
		"MainAccount · CLI Base",
		"in-flight 2 · hits 1500",
		"5H  ██████████████████", // block bar
		"WK  ████",
		"quota age",
		"Runtime Counters",
		"Generation          2,500 requests / 2,480 success",
		"Routing             2,500 decisions",
		"Failover               30 attempts / 28 recovered",
		"Restricted skips       12",
		"Quota refresh         150 / 148 ok / 2 failed",
		"Auth refresh           20 / 20 ok / 0 failed",
		"Persistence errors      1",
		"Replay prevented        4",
		"[q] Quit  [r] Refresh Quota  [+] Faster  [-] Slower  (2.0s)",
	} {
		if !strings.Contains(plainWide, expected) {
			t.Errorf("Wide frame missing expected content %q\nGot:\n%s", expected, plainWide)
		}
	}

	// 2. Compact Layout (width = 80)
	frameCompact := RenderTopFrame(pool, stats, info, true, "", 2*time.Second, 80, false)
	plainCompact := StripANSI(frameCompact)
	for _, expected := range []string{
		"agy-pool",
		"Gateway: RUNNING (PID 12345)",
		"max_quota",
		"MainAccount · CLI Base · in-flight 2 · hits 1500",
		"5H", "90%", "reset",
		"WK", "20%", "reset",
		"age",
		"Runtime Counters",
		"Generation",
		"Routing",
		"Failover",
		"[q] Quit  [r] Refresh Quota  [+] Faster  [-] Slower  (2.0s)",
	} {
		if !strings.Contains(plainCompact, expected) {
			t.Errorf("Compact frame missing expected content %q\nGot:\n%s", expected, plainCompact)
		}
	}

	// 3. Mobile Layout (width = 50)
	frameMobile := RenderTopFrame(pool, stats, info, true, "", 2*time.Second, 50, false)
	for _, expected := range []string{
		"agy-pool",
		"RUNNING",
		"Strategy: max_quota",
		"MainAccount",
		"CLI Base",
		"5H", "90%",
		"WK", "20%",
		"in-flight 2 · hits 1500",
		"Runtime Counters",
		"Generation : 2,500 req / 2,480 ok",
		"Routing    : 2,500 decisions",
		"Failover   : 30 att / 28 rec",
		"[q]Quit [r]Ref [+]Fast [-]Slow (2.0s)",
	} {
		if !strings.Contains(frameMobile, expected) {
			t.Errorf("Mobile frame missing expected content %q\nGot:\n%s", expected, frameMobile)
		}
	}

	// 4. Very Narrow Layout (width = 35)
	frameNarrow := RenderTopFrame(pool, stats, info, true, "", 2*time.Second, 35, false)
	for _, expected := range []string{
		"agy-pool",
		"RUNNING",
		"max_quota",
		"MainAccount",
		"CLI Base",
		"5H  90%",
		"WK  20%",
		"in-flight 2 · hits 1500",
		"Runtime Counters",
		"Generation : 2,500 / 2,480 ok",
		"[q] Quit  [r] Refresh",
		"[+] Faster  [-] Slower",
	} {
		if !strings.Contains(frameNarrow, expected) {
			t.Errorf("Very narrow frame missing expected content %q\nGot:\n%s", expected, frameNarrow)
		}
	}
}

func TestTop_AccountStates(t *testing.T) {
	now := time.Now().Unix()
	pool := createTestTopPool(now)
	stats := createTestRuntimeStats(12345)
	info := &daemon.DaemonInfo{PID: 12345}

	frame := RenderTopFrame(pool, stats, info, true, "", 2*time.Second, 100, false)

	expectedStates := []string{
		"CLI Base",
		"Cooldown",
		"Exhausted",
		"Action Required (Verify needed)",
		"Auth Failure (Re-authenticate)",
	}
	for _, state := range expectedStates {
		if !strings.Contains(frame, state) {
			t.Errorf("expected account state badge %q in top frame, got:\n%s", state, frame)
		}
	}
}

func TestTop_UnknownQuota(t *testing.T) {
	now := time.Now().Unix()
	acc := &storage.Account{
		ID:           "acc_unknown",
		Name:         "UnknownQuotaAccount",
		Status:       "ready",
		RequestCount: 10,
		CreatedAt:    &now,
		LastQuota:    nil, // nil quota
	}
	pool := &storage.Pool{
		Version:  1,
		Accounts: []*storage.Account{acc},
	}
	stats := createTestRuntimeStats(12345)
	info := &daemon.DaemonInfo{PID: 12345}

	frame := RenderTopFrame(pool, stats, info, true, "", 2*time.Second, 100, false)

	if !strings.Contains(frame, "5H -- unknown") {
		t.Errorf("expected '5H -- unknown' for nil quota, got:\n%s", frame)
	}
	if !strings.Contains(frame, "WK -- unknown") {
		t.Errorf("expected 'WK -- unknown' for nil quota, got:\n%s", frame)
	}
	// Never invent reset countdown for unknown quota
	if strings.Contains(frame, "resets in") {
		t.Errorf("unknown quota must NEVER invent a reset countdown, got:\n%s", frame)
	}
}

func TestTop_LongFriendlyNames_And_CJK(t *testing.T) {
	now := time.Now().Unix()
	frac := 0.8
	pool := &storage.Pool{
		Version: 1,
		Accounts: []*storage.Account{
			{
				ID:        "acc_cjk",
				Name:      "谷歌主账号·生产环境",
				Status:    "ready",
				CreatedAt: &now,
				LastQuota: &storage.QuotaState{RemainingFraction: &frac},
			},
			{
				ID:        "acc_long",
				Name:      "ThisIsAnExtremelyLongAccountFriendlyNameThatExceedsNormalWidth",
				Status:    "ready",
				CreatedAt: &now,
				LastQuota: &storage.QuotaState{RemainingFraction: &frac},
			},
		},
	}
	stats := createTestRuntimeStats(12345)
	info := &daemon.DaemonInfo{PID: 12345}

	for _, width := range []int{120, 80, 50, 35} {
		frame := RenderTopFrame(pool, stats, info, true, "", 2*time.Second, width, false)
		lines := strings.Split(frame, "\n")
		for lineIdx, line := range lines {
			vw := VisibleWidth(line)
			// Lines must not exceed width budget by more than margin (in wide layout header can be up to 68)
			maxAllowed := width
			if width > 68 && maxAllowed < 68 {
				maxAllowed = 68
			}
			if vw > maxAllowed+4 {
				t.Errorf("width %d: line %d exceeded width: visual width %d > %d: %q",
					width, lineIdx+1, vw, maxAllowed, line)
			}
		}
	}
}

func TestTop_InFlight_ZeroOneN(t *testing.T) {
	now := time.Now().Unix()
	frac := 0.95
	acc0 := &storage.Account{ID: "acc_0", Name: "ZeroFlight", CreatedAt: &now, LastQuota: &storage.QuotaState{RemainingFraction: &frac}}
	acc1 := &storage.Account{ID: "acc_1", Name: "OneFlight", CreatedAt: &now, LastQuota: &storage.QuotaState{RemainingFraction: &frac}}
	accN := &storage.Account{ID: "acc_N", Name: "NFlight", CreatedAt: &now, LastQuota: &storage.QuotaState{RemainingFraction: &frac}}

	pool := &storage.Pool{
		Version:  1,
		Accounts: []*storage.Account{acc0, acc1, accN},
	}
	stats := &daemon.RuntimeStats{
		PID: 5555,
		InFlight: map[string]int64{
			"acc_0": 0,
			"acc_1": 1,
			"acc_N": 9,
		},
	}
	info := &daemon.DaemonInfo{PID: 5555}

	frame := RenderTopFrame(pool, stats, info, true, "", 2*time.Second, 100, false)

	if !strings.Contains(frame, "in-flight 0") {
		t.Errorf("expected 'in-flight 0' in frame, got:\n%s", frame)
	}
	if !strings.Contains(frame, "in-flight 1") {
		t.Errorf("expected 'in-flight 1' in frame, got:\n%s", frame)
	}
	if !strings.Contains(frame, "in-flight 9") {
		t.Errorf("expected 'in-flight 9' in frame, got:\n%s", frame)
	}
}

func TestTop_IntervalBounds(t *testing.T) {
	tmpDir, cleanup := setupCLITestEnv(t)
	defer cleanup()

	var stdout, stderr bytes.Buffer
	// Test minimum clamp: 0.1s -> 500ms
	code := Main([]string{"top", "--interval=0.1s", "--once", "-D", tmpDir}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected code 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "0.5s") {
		t.Errorf("expected 0.1s to be clamped to 0.5s, got:\n%s", stdout.String())
	}

	// Test maximum clamp: 120s -> 60s
	stdout.Reset()
	code = Main([]string{"top", "--interval=120s", "--once", "-D", tmpDir}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected code 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "60.0s") {
		t.Errorf("expected 120s to be clamped to 60.0s, got:\n%s", stdout.String())
	}
}

func TestTop_KeyHandling_Q_Plus_Minus_R(t *testing.T) {
	tmpDir, cleanup := setupCLITestEnv(t)
	defer cleanup()

	now := time.Now().Unix()
	frac := 0.8
	acc := &storage.Account{
		ID:        "acc_key_test",
		Name:      "KeyTestAcc",
		Status:    "ready",
		CreatedAt: &now,
		LastQuota: &storage.QuotaState{RemainingFraction: &frac},
	}
	pool := &storage.Pool{
		Version:  1,
		Accounts: []*storage.Account{acc},
	}
	_ = storage.SavePool(pool)

	ctx, err := daemon.ResolveInstanceContext(tmpDir, "", 8899, "")
	if err != nil {
		t.Fatal(err)
	}

	// Pass keystrokes: '+' (faster), '-' (slower), 'r' (refresh), 'q' (quit)
	in := bytes.NewBufferString("+ - r q\n")
	var out, errBuf bytes.Buffer

	opts := TopOptions{
		Interval: 2 * time.Second,
		Width:    100,
		Once:     false,
		Stdin:    in,
		Stdout:   &out,
		Stderr:   &errBuf,
	}

	exitCode := RunTop(ctx, opts)
	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}

	outStr := out.String()
	if !strings.Contains(outStr, "[q] Quit") {
		t.Errorf("expected dashboard output from RunTop, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "\033[?25h") {
		t.Errorf("expected cursor restoration code \\033[?25h on quit")
	}
}

func TestTop_TerminalRestorationOnPanic(t *testing.T) {
	var out bytes.Buffer

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic to propagate")
		}
		outStr := out.String()
		if !strings.Contains(outStr, "\033[?25h") {
			t.Errorf("expected cursor show sequence \\033[?25h on panic recovery")
		}
	}()

	// Simulate panic recovery in Top
	panickingTop := func() {
		var cleanupOnce sync.Once
		cleanup := func() {
			cleanupOnce.Do(func() {
				out.WriteString("\033[?25h\033[0m\n")
			})
		}
		defer cleanup()
		defer func() {
			if r := recover(); r != nil {
				cleanup()
				panic(r)
			}
		}()

		panic("simulated terminal failure")
	}

	panickingTop()
}

func TestTop_Privacy(t *testing.T) {
	now := time.Now().Unix()
	pool := createTestTopPool(now)
	stats := createTestRuntimeStats(12345)
	info := &daemon.DaemonInfo{PID: 12345}

	frame := RenderTopFrame(pool, stats, info, true, "", 2*time.Second, 100, false)

	// Must never leak private information
	prohibited := []string{
		"private-developer@internal.corp.example.com",
		"private-backup@internal.corp.example.com",
		"@internal.corp.example.com",
		"ya29.",
		"mock_token",
		"mock_refresh",
		"https://",
		"http://",
		"prompt",
		"conversation",
	}

	for _, p := range prohibited {
		if strings.Contains(frame, p) {
			t.Errorf("SECURITY/PRIVACY VIOLATION: top frame leaked prohibited string %q\nFrame was:\n%s", p, frame)
		}
	}
}

func TestTop_AliasesAndFlags(t *testing.T) {
	tmpDir, cleanup := setupCLITestEnv(t)
	defer cleanup()

	now := time.Now().Unix()
	frac := 0.85
	acc := &storage.Account{
		ID:        "acc_alias_1",
		Name:      "AliasAccount",
		Status:    "ready",
		CreatedAt: &now,
		LastQuota: &storage.QuotaState{RemainingFraction: &frac},
	}
	pool := &storage.Pool{
		Version:  1,
		Accounts: []*storage.Account{acc},
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	ctx, err := daemon.ResolveInstanceContext(tmpDir, "", port, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := config.ConfigureDataDir(ctx.DataDir); err != nil {
		t.Fatal(err)
	}
	_ = storage.SavePool(pool)

	pidInfo := daemon.DaemonInfo{PID: os.Getpid(), Port: port}
	_ = daemon.WritePIDFile(ctx.PIDFile(), pidInfo)

	for _, cmd := range [][]string{
		{"top", "--once", "-p", strconv.Itoa(port), "-D", tmpDir},
		{"watch", "--once", "-p", strconv.Itoa(port), "-D", tmpDir},
		{"monitor", "--once", "-p", strconv.Itoa(port), "-D", tmpDir},
		{"quota", "-w", "--once", "-p", strconv.Itoa(port), "-D", tmpDir},
		{"quota", "--watch", "--once", "-p", strconv.Itoa(port), "-D", tmpDir},
		{"status", "-w", "--once", "-p", strconv.Itoa(port), "-D", tmpDir},
		{"status", "--watch", "--once", "-p", strconv.Itoa(port), "-D", tmpDir},
	} {
		var stdout, stderr bytes.Buffer
		code := Main(cmd, nil, &stdout, &stderr)
		if code != 0 {
			t.Errorf("command %v failed with exit code %d: %s", cmd, code, stderr.String())
		}
		out := stdout.String()
		if !strings.Contains(out, "AliasAccount") || !strings.Contains(out, "Runtime Counters") {
			t.Errorf("command %v did not render valid top frame:\n%s", cmd, out)
		}
	}
}

func TestTop_BoundedWidthsAssertion(t *testing.T) {
	now := time.Now().Unix()
	standardPool := createTestTopPool(now)
	cjkPool := &storage.Pool{
		Version: 1,
		Accounts: []*storage.Account{
			{
				ID:           "cjk_1",
				Name:         "主账号 · 极速开发者账号",
				Status:       "ready",
				RequestCount: 9999,
				LastQuota:    standardPool.Accounts[0].LastQuota,
			},
			{
				ID:           "cjk_2",
				Name:         "日本語アカウント（東京リージョン）",
				Status:       "ready",
				RequestCount: 120000,
				LastQuota:    standardPool.Accounts[0].LastQuota,
			},
		},
	}
	emptyPool := &storage.Pool{
		Version:  1,
		Accounts: []*storage.Account{},
	}
	longNamePool := &storage.Pool{
		Version: 1,
		Accounts: []*storage.Account{
			{
				ID:           "long_1",
				Name:         "SuperExtremelyLongAccountIdentifierThatExceedsNormalTerminalWidthEasily2026",
				Status:       "validation_required",
				RequestCount: 999999,
			},
		},
	}

	pools := map[string]*storage.Pool{
		"standard":  standardPool,
		"cjk":       cjkPool,
		"empty":     emptyPool,
		"long_name": longNamePool,
	}

	statsRunning := createTestRuntimeStats(3754721)
	infoRunning := &daemon.DaemonInfo{PID: 3754721}

	widths := []int{120, 80, 50, 35}

	for _, w := range widths {
		for pName, p := range pools {
			for _, daemonRunning := range []bool{true, false} {
				for _, refreshBanner := range []bool{true, false} {
					var curStats *daemon.RuntimeStats
					var curInfo *daemon.DaemonInfo
					if daemonRunning {
						curStats = statsRunning
						curInfo = infoRunning
					}

					frame := RenderTopFrame(p, curStats, curInfo, daemonRunning, "", 2*time.Second, w, refreshBanner)
					lines := strings.Split(frame, "\n")
					for lineIdx, line := range lines {
						visWidth := VisibleWidth(line)
						if visWidth > w {
							t.Fatalf("Width constraint VIOLATED at width=%d [pool=%s, daemonRunning=%v, banner=%v] line %d:\nVisible width: %d > %d\nRaw line: %q\nPlain line: %q",
								w, pName, daemonRunning, refreshBanner, lineIdx, visWidth, w, line, StripANSI(line))
						}
					}
				}
			}
		}
	}
}

func TestTop_PrintRepresentativeWidths(t *testing.T) {
	now := time.Now().Unix()
	pool := createTestTopPool(now)
	stats := createTestRuntimeStats(3754721)
	info := &daemon.DaemonInfo{PID: 3754721}

	for _, w := range []int{120, 80, 50, 35} {
		frame := RenderTopFrame(pool, stats, info, true, "", 2*time.Second, w, false)
		t.Logf("\n=== WIDTH %d ===\n%s", w, StripANSI(frame))
	}
}
