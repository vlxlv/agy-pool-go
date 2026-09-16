package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/diagnostics"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func setupCLITestEnv(t *testing.T) (string, func()) {
	t.Helper()
	tempDir, err := os.MkdirTemp("", "agy_cli_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	stateDir := filepath.Join(tempDir, ".gemini")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatalf("failed to create state dir: %v", err)
	}

	origState := config.GetStateDir()
	origTestMode := config.IsTestMode()
	config.SetTestMode(true)
	if err := config.ConfigureStateDir(stateDir); err != nil {
		t.Fatalf("ConfigureStateDir failed: %v", err)
	}

	origExec := ExecHandler

	cleanup := func() {
		ExecHandler = origExec
		_ = config.ConfigureStateDir(origState)
		config.SetTestMode(origTestMode)
		diagnostics.SetAgyBinaryFinder(nil)
		diagnostics.SetAgyVersionProvider(nil)
		diagnostics.SetTLSProber(nil)
		_ = os.RemoveAll(tempDir)
	}

	return tempDir, cleanup
}

func TestVersionAndHelp(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	for _, vFlag := range [][]string{{"--version"}, {"-v"}, {"version"}} {
		var stdout, stderr bytes.Buffer
		code := Main(vFlag, nil, &stdout, &stderr)
		if code != 0 {
			t.Errorf("%v expected exit 0, got %d", vFlag, code)
		}
		if !strings.HasPrefix(stdout.String(), "agy-pool ") {
			t.Errorf("%v expected 'agy-pool <ver>', got %q", vFlag, stdout.String())
		}
	}

	for _, hFlag := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		var stdout, stderr bytes.Buffer
		code := Main(hFlag, nil, &stdout, &stderr)
		if code != 0 {
			t.Errorf("%v expected exit 0, got %d", hFlag, code)
		}
		if !strings.Contains(stdout.String(), "usage: agy-pool") {
			t.Errorf("%v expected usage text, got %q", hFlag, stdout.String())
		}
	}
}

func TestUnknownCommandAndInvalidChoice(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	var stdout, stderr bytes.Buffer
	code := Main([]string{"nonexistent_command"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit code 2 on unknown command, got %d", code)
	}
	if !strings.Contains(stderr.String(), "invalid choice: 'nonexistent_command'") {
		t.Errorf("expected invalid choice error on stderr, got: %s", stderr.String())
	}
}

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

func TestRenameCommand(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	// 1. Missing arguments -> exit 2
	var stdout, stderr bytes.Buffer
	code := Main([]string{"rename"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2 on missing rename args, got %d", code)
	}

	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"rename", "1"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2 on missing rename target name, got %d", code)
	}

	// 2. Empty name -> exit 1
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"rename", "1", "   "}, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 on empty rename name, got %d", code)
	}
	if !strings.Contains(stdout.String(), "[Error] Account name cannot be empty.") {
		t.Errorf("expected empty name error, got: %s", stdout.String())
	}

	// 3. Target not found in empty pool -> exit 1
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"rename", "nonexistent", "NewName"}, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 on account not found, got %d", code)
	}
	if !strings.Contains(stdout.String(), "[Error] Account 'nonexistent' not found.") {
		t.Errorf("expected not found error, got: %s", stdout.String())
	}
}

func TestListAccounts_EmptyAndPopulated(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	// 1. Empty pool
	var stdout, stderr bytes.Buffer
	code := Main([]string{"list"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on empty list, got %d", code)
	}
	if !strings.Contains(stdout.String(), "No accounts in pool yet.") {
		t.Errorf("expected empty pool notice, got: %s", stdout.String())
	}

	// 2. Populate pool
	now := time.Now().Unix()
	frac := 0.75
	acc1 := &storage.Account{
		ID:           "acc_1",
		Email:        "user1@gmail.com",
		Name:         "Personal",
		Status:       "active",
		RequestCount: 42,
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

	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"list"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on populated list, got %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Personal") {
		t.Errorf("expected account name Personal, got: %s", out)
	}
	if !strings.Contains(out, "Hits: 42") {
		t.Errorf("expected Hits: 42, got: %s", out)
	}
	if !strings.Contains(out, "[* Active]") {
		t.Errorf("expected [* Active] badge, got: %s", out)
	}

	// 3. Target not found -> exit 0 matching Python
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"list", "nonexistent"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on list target not found, got %d", code)
	}
	if !strings.Contains(stdout.String(), "[Error] Account 'nonexistent' not found.") {
		t.Errorf("expected not found error on stdout, got: %s", stdout.String())
	}
}

func TestRunWrapperAndAgyDirect(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	diagnostics.SetAgyBinaryFinder(func() string { return "/fake/bin/agy" })

	var capturedBinary string
	var capturedArgs []string
	var capturedEnv []string

	ExecHandler = func(binary string, argv []string, env []string) error {
		capturedBinary = binary
		capturedArgs = argv
		capturedEnv = env
		return nil
	}

	// 1. Test "run"
	var stdout, stderr bytes.Buffer
	code := Main([]string{"run", "--some-flag", "val"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run expected 0, got %d", code)
	}
	if capturedBinary != "/fake/bin/agy" {
		t.Errorf("expected binary /fake/bin/agy, got %q", capturedBinary)
	}
	if len(capturedArgs) < 3 || capturedArgs[1] != "--some-flag" || capturedArgs[2] != "val" {
		t.Errorf("expected forwarded args, got %v", capturedArgs)
	}

	hasCloudCodeURL := false
	for _, e := range capturedEnv {
		if strings.HasPrefix(e, "CLOUD_CODE_URL=http://127.0.0.1:8899") {
			hasCloudCodeURL = true
			break
		}
	}
	if !hasCloudCodeURL {
		t.Errorf("expected CLOUD_CODE_URL in env for run")
	}

	// 2. Test "raw"
	capturedBinary = ""
	capturedArgs = nil
	capturedEnv = nil
	stdout.Reset()
	stderr.Reset()

	code = Main([]string{"raw", "--direct-flag"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("raw expected 0, got %d", code)
	}
	for _, e := range capturedEnv {
		if strings.HasPrefix(e, "CLOUD_CODE_URL=") {
			t.Errorf("raw mode must NOT set CLOUD_CODE_URL, found %s", e)
		}
	}
}

func TestDoctorCommand(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	diagnostics.SetAgyBinaryFinder(func() string { return "/bin/true" })
	diagnostics.SetTLSProber(func(host string, timeout time.Duration) error { return nil })

	var stdout, stderr bytes.Buffer
	code := Main([]string{"doctor"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doctor expected 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "Antigravity System Doctor") {
		t.Errorf("expected doctor header, got: %s", stdout.String())
	}
}

func TestLogsCommand(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	logFile := config.GetLogFile()
	_ = os.WriteFile(logFile, []byte("line1\nline2\nline3\n"), 0600)

	var stdout, stderr bytes.Buffer
	code := Main([]string{"logs", "-n", "2"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("logs expected 0, got %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Antigravity Gateway Log") {
		t.Errorf("expected log header, got: %s", out)
	}
	if !strings.Contains(out, "line2") || !strings.Contains(out, "line3") {
		t.Errorf("expected tail lines, got: %s", out)
	}

	// Clear logs
	stdout.Reset()
	code = Main([]string{"logs", "--clear"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("logs --clear expected 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "Successfully cleared gateway log") {
		t.Errorf("expected clear log success message, got: %s", stdout.String())
	}
}
