package cli

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
	"github.com/vlxlv/agy-pool-go/internal/diagnostics"
	"github.com/vlxlv/agy-pool-go/internal/storage"
	_ "modernc.org/sqlite"
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
	config.SetNativeAgyDir(stateDir)
	if err := config.ConfigureStateDir(stateDir); err != nil {
		t.Fatalf("ConfigureStateDir failed: %v", err)
	}

	origExec := ExecHandler
	origEntrypoint := daemon.EntrypointProvider
	daemon.EntrypointProvider = func() string { return "/bin/true" }

	cleanup := func() {
		daemon.EntrypointProvider = origEntrypoint
		ExecHandler = origExec
		config.SetNativeAgyDir("")
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

func TestParseRunArgs(t *testing.T) {
	// Canonical collision test from specification:
	// agy-pool run -c cfg.json -D data -- -c foo -D bar --future-option
	cfg, data, native, err := ParseRunArgs([]string{"-c", "cfg.json", "-D", "data", "--", "-c", "foo", "-D", "bar", "--future-option"})
	if err != nil {
		t.Fatalf("ParseRunArgs error: %v", err)
	}
	if cfg != "cfg.json" {
		t.Errorf("expected cfg.json, got %q", cfg)
	}
	if data != "data" {
		t.Errorf("expected data, got %q", data)
	}
	expectedNative := []string{"-c", "foo", "-D", "bar", "--future-option"}
	if len(native) != len(expectedNative) {
		t.Fatalf("expected native args len %d, got %d: %v", len(expectedNative), len(native), native)
	}
	for i, v := range expectedNative {
		if native[i] != v {
			t.Errorf("native arg[%d] expected %q, got %q", i, v, native[i])
		}
	}

	// System layout example without native args:
	// agy-pool run -c /etc/agy-pool/config.json -D /var/lib/agy-pool
	cfg, data, native, err = ParseRunArgs([]string{"-c", "/etc/agy-pool/config.json", "-D", "/var/lib/agy-pool"})
	if err != nil {
		t.Fatalf("ParseRunArgs error: %v", err)
	}
	if cfg != "/etc/agy-pool/config.json" || data != "/var/lib/agy-pool" || len(native) != 0 {
		t.Errorf("unexpected parse result: cfg=%q data=%q native=%v", cfg, data, native)
	}

	// User-local layout with native args:
	// agy-pool run -c ~/.config/agy-pool/config.json -D ~/.local/share/agy-pool -- --conversation=abc123
	cfg, data, native, err = ParseRunArgs([]string{"-c", "~/.config/agy-pool/config.json", "-D", "~/.local/share/agy-pool", "--", "--conversation=abc123"})
	if err != nil {
		t.Fatalf("ParseRunArgs error: %v", err)
	}
	if cfg != "~/.config/agy-pool/config.json" || data != "~/.local/share/agy-pool" || len(native) != 1 || native[0] != "--conversation=abc123" {
		t.Errorf("unexpected parse result: cfg=%q data=%q native=%v", cfg, data, native)
	}

	// Missing arguments before --
	_, _, _, err = ParseRunArgs([]string{"-c"})
	if err == nil {
		t.Errorf("expected error for -c without argument, got nil")
	}
	_, _, _, err = ParseRunArgs([]string{"-D"})
	if err == nil {
		t.Errorf("expected error for -D without argument, got nil")
	}
	_, _, _, err = ParseRunArgs([]string{"-c", "cfg.json", "--unknown-flag", "--", "native"})
	if err == nil {
		t.Errorf("expected error for unknown option before --, got nil")
	}
}

func TestRunArgumentScopingAndDelimiter(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "config.json")
	cfgContent := `{"version": 1, "server": {"listen": "127.0.0.1", "port": 8899}, "scheduler": {"strategy": "max_quota"}, "native_agy": {"binary": null}, "logging": {"max_size_bytes": 5242880, "backup_count": 1}}`
	if err := os.WriteFile(cfgFile, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	dataDir := filepath.Join(tmpDir, "data")

	diagnostics.SetAgyBinaryFinder(func() string { return "/fake/bin/agy" })

	var capturedBinary string
	var capturedArgs []string

	ExecHandler = func(binary string, argv []string, env []string) error {
		capturedBinary = binary
		capturedArgs = argv
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := Main([]string{
		"run",
		"-c", cfgFile,
		"-D", dataDir,
		"--",
		"-c", "foo",
		"-D", "bar",
		"--future-option",
	}, nil, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run expected 0, got %d. stderr: %s", code, stderr.String())
	}
	if capturedBinary != "/fake/bin/agy" {
		t.Errorf("expected /fake/bin/agy, got %q", capturedBinary)
	}

	// Everything after "--" must be forwarded argument-for-argument to native agy
	expectedArgv := []string{"/fake/bin/agy", "-c", "foo", "-D", "bar", "--future-option"}
	if len(capturedArgs) != len(expectedArgv) {
		t.Fatalf("expected argv len %d, got %d: %v", len(expectedArgv), len(capturedArgs), capturedArgs)
	}
	for i, arg := range expectedArgv {
		if capturedArgs[i] != arg {
			t.Errorf("argv[%d] expected %q, got %q", i, arg, capturedArgs[i])
		}
	}

	// Verify -D configured the data directory without chdir
	if config.GetDataDir() != dataDir {
		t.Errorf("expected data dir %q, got %q", dataDir, config.GetDataDir())
	}
}

func TestRunTopLevelOptionsRejected(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	var stdout, stderr bytes.Buffer

	// -c FILE at top-level must be rejected (not global)
	code := Main([]string{"-c", "/etc/config.json", "list"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit code 2 for top-level -c, got %d", code)
	}
	if !strings.Contains(stderr.String(), "invalid choice: '-c'") {
		t.Errorf("expected invalid choice error for -c, got: %s", stderr.String())
	}

	// -D DIR at top-level must be rejected (not global)
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"-D", "/var/lib/data", "status"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit code 2 for top-level -D, got %d", code)
	}
	if !strings.Contains(stderr.String(), "invalid choice: '-D'") {
		t.Errorf("expected invalid choice error for -D, got: %s", stderr.String())
	}
}

func TestRunBareContinueResolvesConversation(t *testing.T) {
	tempDir, cleanup := setupCLITestEnv(t)
	defer cleanup()

	// 1. Setup mock conversation DB
	cliDir := config.GetAgyCliDir()
	_ = os.MkdirAll(cliDir, 0700)
	dbPath := filepath.Join(cliDir, "conversation_summaries.db")

	db, err := sql.Open("sqlite", dbPath)
	if err == nil {
		_, _ = db.Exec("CREATE TABLE conversation_summaries (conversation_id TEXT PRIMARY KEY, title TEXT, workspace_uris TEXT, last_modified_time INTEGER);")
		_, _ = db.Exec("INSERT INTO conversation_summaries VALUES ('conv_12345678', 'Test Session', '[\"file://" + tempDir + "\"]', 5000);")
		db.Close()
	}

	// 2. Setup config and data dir
	cfgFile := filepath.Join(tempDir, "config.json")
	dataDir := filepath.Join(tempDir, "state_run")
	cfgContent := `{"version": 1, "server": {"port": 9199}}`
	if err := os.WriteFile(cfgFile, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	diagnostics.SetAgyBinaryFinder(func() string { return "/fake/bin/agy" })

	var capturedBinary string
	var capturedArgs []string
	ExecHandler = func(binary string, argv []string, env []string) error {
		capturedBinary = binary
		capturedArgs = argv
		return nil
	}

	var stdout, stderr bytes.Buffer
	// Test running with bare "-c" after "--"
	code := Main([]string{
		"run",
		"-c", cfgFile,
		"-D", dataDir,
		"--",
		"-c",
	}, nil, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run expected 0, got %d. stderr: %s", code, stderr.String())
	}
	if capturedBinary != "/fake/bin/agy" {
		t.Errorf("expected /fake/bin/agy, got %q", capturedBinary)
	}
	if len(capturedArgs) == 0 {
		t.Fatalf("expected capturedArgs not to be empty")
	}

	// Verify -D configured the data directory without chdir
	if config.GetDataDir() != dataDir {
		t.Errorf("expected data dir %q, got %q", dataDir, config.GetDataDir())
	}
}

func TestFindRealAgyBinaryExcludesShims(t *testing.T) {
	tempDir, cleanup := setupCLITestEnv(t)
	defer cleanup()

	// Ensure no mock override is active
	diagnostics.SetAgyBinaryFinder(nil)

	shimDir := filepath.Join(tempDir, "shim_bin")
	realDir := filepath.Join(tempDir, "real_bin")
	if err := os.MkdirAll(shimDir, 0755); err != nil {
		t.Fatalf("failed to create shimDir: %v", err)
	}
	if err := os.MkdirAll(realDir, 0755); err != nil {
		t.Fatalf("failed to create realDir: %v", err)
	}

	// Create an agy-pool integration shim in shimDir
	shimFile := filepath.Join(shimDir, "agy")
	shimContent := `#!/bin/sh
# agy: agy-pool integration shim
exec agy-pool run "$@"
`
	if err := os.WriteFile(shimFile, []byte(shimContent), 0755); err != nil {
		t.Fatalf("failed to write shim: %v", err)
	}

	// Create a real mock agy executable in realDir
	realFile := filepath.Join(realDir, "agy")
	realContent := `#!/bin/sh
echo "real native agy v1.0.0"
`
	if err := os.WriteFile(realFile, []byte(realContent), 0755); err != nil {
		t.Fatalf("failed to write real mock: %v", err)
	}

	// Set PATH with shimDir FIRST, then realDir
	origPath := os.Getenv("PATH")
	defer os.Setenv("PATH", origPath)
	os.Setenv("PATH", shimDir+string(os.PathListSeparator)+realDir)

	// Isolate HOME, PREFIX, and AGY_BIN
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)
	os.Setenv("HOME", tempDir)

	origPrefix := os.Getenv("PREFIX")
	defer os.Setenv("PREFIX", origPrefix)
	os.Unsetenv("PREFIX")

	origAgyBin := os.Getenv("AGY_BIN")
	defer os.Setenv("AGY_BIN", origAgyBin)
	os.Unsetenv("AGY_BIN")

	found := diagnostics.FindRealAgyBinary()
	if found != realFile {
		t.Fatalf("expected FindRealAgyBinary to skip shim %s and find real binary %s, got: %s", shimFile, realFile, found)
	}
}

func TestMigrateLegacyCommand(t *testing.T) {
	tempDir, cleanup := setupCLITestEnv(t)
	defer cleanup()

	srcFile := filepath.Join(tempDir, "legacy_pool.json")
	targetDir := filepath.Join(tempDir, "target_data")
	content := `{
  "version": 1,
  "strategy": "max_quota",
  "accounts": [
    {"id": "acc_1", "email": "migrated@example.com", "access_token": "tok1"}
  ]
}`
	if err := os.WriteFile(srcFile, []byte(content), 0600); err != nil {
		t.Fatalf("failed to write legacy pool: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Main([]string{"migrate-legacy", "-s", srcFile, "-D", targetDir}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("migrate-legacy failed with code %d, stderr: %s", code, stderr.String())
	}

	if !strings.Contains(stdout.String(), "Successfully migrated 1 account") {
		t.Fatalf("unexpected stdout: %s", stdout.String())
	}

	// Verify target accounts.json exists
	targetAccounts := filepath.Join(targetDir, "accounts.json")
	if _, err := os.Stat(targetAccounts); err != nil {
		t.Fatalf("target accounts file was not created: %v", err)
	}

	// Verify source was untouched
	data, err := os.ReadFile(srcFile)
	if err != nil || string(data) != content {
		t.Fatalf("source file was modified or unreadable")
	}
}

func TestInstallCommand_PreconditionFailure(t *testing.T) {
	tempDir, cleanup := setupCLITestEnv(t)
	defer cleanup()

	targetDir := filepath.Join(tempDir, "bin")
	nonExistentAgy := filepath.Join(tempDir, "no_such_agy")

	var stdout, stderr bytes.Buffer
	code := Main([]string{"install", "--target", targetDir, "--native-agy", nonExistentAgy, "--skip-rc"}, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1 on missing native agy, got %d. stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Native agy was not found") {
		t.Errorf("expected error message about native agy, got: %s", stderr.String())
	}
}

func TestInstallAndUninstallCommand_UserMode(t *testing.T) {
	tempDir, cleanup := setupCLITestEnv(t)
	defer cleanup()

	targetBin := filepath.Join(tempDir, "usr_bin")
	configDir := filepath.Join(tempDir, "cfg")
	dataDir := filepath.Join(tempDir, "data")
	realAgyDir := filepath.Join(tempDir, "real_bin")
	if err := os.MkdirAll(realAgyDir, 0755); err != nil {
		t.Fatalf("failed to create realAgyDir: %v", err)
	}

	mockNativeAgy := filepath.Join(realAgyDir, "agy")
	if err := os.WriteFile(mockNativeAgy, []byte("#!/bin/sh\necho \"native agy\"\n"), 0755); err != nil {
		t.Fatalf("failed to write mock native agy: %v", err)
	}

	// 1. Install
	var stdout, stderr bytes.Buffer
	code := Main([]string{
		"install",
		"--user",
		"--target", targetBin,
		"--native-agy", mockNativeAgy,
		"--config-dir", configDir,
		"--data-dir", dataDir,
		"--skip-rc",
	}, nil, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("install failed with exit code %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Successfully installed agy-pool") {
		t.Errorf("expected success message in stdout, got: %s", stdout.String())
	}

	// Verify installed files
	shimPath := filepath.Join(targetBin, "agy")
	shimBytes, err := os.ReadFile(shimPath)
	if err != nil {
		t.Fatalf("failed to read installed shim: %v", err)
	}
	if !strings.Contains(string(shimBytes), "agy-pool run") {
		t.Errorf("expected shim to run agy-pool, got: %s", string(shimBytes))
	}

	rawPath := filepath.Join(targetBin, "agy-raw")
	if _, err := os.Stat(rawPath); err != nil {
		t.Fatalf("agy-raw was not created: %v", err)
	}

	cfgPath := filepath.Join(configDir, "config.json")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("config.json was not created: %v", err)
	}

	locksDir := filepath.Join(dataDir, "locks")
	if fi, err := os.Stat(locksDir); err != nil || !fi.IsDir() {
		t.Fatalf("data locks directory was not created: %v", err)
	}

	// 2. Uninstall
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"uninstall", "--target", targetBin}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("uninstall failed with exit code %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Successfully uninstalled agy-pool") {
		t.Errorf("expected success message in stdout, got: %s", stdout.String())
	}

	// Shim in targetBin should be removed
	if _, err := os.Stat(shimPath); !os.IsNotExist(err) {
		t.Errorf("shim at %s was not removed during uninstall", shimPath)
	}

	// Real native agy must still exist!
	if _, err := os.Stat(mockNativeAgy); err != nil {
		t.Errorf("CRITICAL: real native agy was deleted by uninstall!")
	}

	// Config and data dir must remain intact
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("user config.json was unexpectedly deleted by uninstall")
	}
	if _, err := os.Stat(locksDir); err != nil {
		t.Errorf("user data locks dir was unexpectedly deleted by uninstall")
	}
}

func TestInstallAndUninstall_InvalidArgs(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	var stdout, stderr bytes.Buffer

	// Missing flag argument
	code := Main([]string{"install", "--prefix"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Errorf("expected code 2 for install --prefix without value, got %d", code)
	}

	// Unknown argument
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"install", "--unknown-flag"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Errorf("expected code 2 for install --unknown-flag, got %d", code)
	}

	// Help flag
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"install", "--help"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected code 0 for install --help, got %d", code)
	}
	if !strings.Contains(stdout.String(), "usage: agy-pool install") {
		t.Errorf("expected usage in help output, got: %s", stdout.String())
	}

	// Uninstall missing flag argument
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"uninstall", "--target"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Errorf("expected code 2 for uninstall --target without value, got %d", code)
	}

	// Uninstall unknown argument
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"uninstall", "--bogus"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Errorf("expected code 2 for uninstall --bogus, got %d", code)
	}

	// Uninstall help flag
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"uninstall", "-h"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected code 0 for uninstall -h, got %d", code)
	}
	if !strings.Contains(stdout.String(), "usage: agy-pool uninstall") {
		t.Errorf("expected usage in help output, got: %s", stdout.String())
	}
}
