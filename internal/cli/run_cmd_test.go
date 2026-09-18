package cli

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/diagnostics"
	"github.com/vlxlv/agy-pool-go/internal/storage"
	_ "modernc.org/sqlite"
)

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

func TestRunAgyWithLB_ArgumentPreservation(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	cfgFile := filepath.Join(tmpDir, "config.json")
	_ = os.WriteFile(cfgFile, []byte(`{"version":1,"server":{"listen":"127.0.0.1","port":9999}}`), 0o600)

	// Mock native agy binary
	fakeAgy := filepath.Join(tmpDir, "agy")
	_ = os.WriteFile(fakeAgy, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	diagnostics.SetAgyBinaryFinder(func() string {
		return fakeAgy
	})

	testCases := []struct {
		name         string
		args         []string
		expectedArgs []string
	}{
		{
			name:         "flag -c",
			args:         []string{"run", "-c", cfgFile, "-D", dataDir, "--", "-c"},
			expectedArgs: []string{fakeAgy, "-c"},
		},
		{
			name:         "conversation flag",
			args:         []string{"run", "-c", cfgFile, "-D", dataDir, "--", "--conversation=conv-1234"},
			expectedArgs: []string{fakeAgy, "--conversation=conv-1234"},
		},
		{
			name:         "prompt flag",
			args:         []string{"run", "-c", cfgFile, "-D", dataDir, "--", "-p", "hello"},
			expectedArgs: []string{fakeAgy, "-p", "hello"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var capturedBinary string
			var capturedArgv []string
			ExecHandler = func(binary string, argv []string, env []string) error {
				capturedBinary = binary
				capturedArgv = argv
				return nil
			}

			var stdout, stderr bytes.Buffer
			code := Main(tc.args, nil, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("expected code 0, got %d (stderr: %s)", code, stderr.String())
			}
			if capturedBinary != fakeAgy {
				t.Fatalf("expected exec binary %q, got %q", fakeAgy, capturedBinary)
			}
			if len(capturedArgv) != len(tc.expectedArgs) {
				t.Fatalf("expected argv %v, got %v", tc.expectedArgs, capturedArgv)
			}
			for i := range tc.expectedArgs {
				if capturedArgv[i] != tc.expectedArgs[i] {
					t.Fatalf("argv[%d] mismatch: expected %q, got %q", i, tc.expectedArgs[i], capturedArgv[i])
				}
			}
		})
	}
}

func TestRunAgyWithLB_ResolvesManagedNativeBinary(t *testing.T) {
	tempDir, cleanup := setupCLITestEnv(t)
	defer cleanup()

	fakeHome := filepath.Join(tempDir, "user_home")
	libexecDir := filepath.Join(fakeHome, ".local", "libexec", "agy-pool")
	_ = os.MkdirAll(libexecDir, 0755)
	managedAgy := filepath.Join(libexecDir, "agy-native")
	_ = os.WriteFile(managedAgy, []byte("#!/bin/sh\nexit 0\n"), 0755)

	t.Setenv("HOME", fakeHome)
	diagnostics.SetAgyBinaryFinder(nil)

	cfgFile := filepath.Join(fakeHome, "config.json")
	dataDir := filepath.Join(fakeHome, "data")
	_ = os.WriteFile(cfgFile, []byte(`{"version":1,"server":{"listen":"127.0.0.1","port":9876}}`), 0600)

	var capturedBinary string
	var capturedArgv []string
	var capturedEnv []string
	ExecHandler = func(binary string, argv []string, env []string) error {
		capturedBinary = binary
		capturedArgv = argv
		capturedEnv = env
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := Main([]string{"run", "-c", cfgFile, "-D", dataDir, "--", "--dangerously-skip-permissions", "-p", "hello"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected code 0, got %d (stderr: %s)", code, stderr.String())
	}

	if capturedBinary != managedAgy {
		t.Fatalf("expected exec binary %q, got %q", managedAgy, capturedBinary)
	}

	hasCloudCodeURL := false
	for _, e := range capturedEnv {
		if strings.HasPrefix(e, "CLOUD_CODE_URL=http://127.0.0.1:9876") {
			hasCloudCodeURL = true
			break
		}
	}
	if !hasCloudCodeURL {
		t.Errorf("expected CLOUD_CODE_URL to be injected in environment, got: %v", capturedEnv)
	}

	expectedArgs := []string{managedAgy, "--dangerously-skip-permissions", "-p", "hello"}
	if len(capturedArgv) != len(expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, capturedArgv)
	}
	for i := range expectedArgs {
		if capturedArgv[i] != expectedArgs[i] {
			t.Errorf("arg[%d] mismatch: expected %q, got %q", i, expectedArgs[i], capturedArgv[i])
		}
	}
}

func TestRunAgyDirect_ResolvesManagedNativeBinary(t *testing.T) {
	tempDir, cleanup := setupCLITestEnv(t)
	defer cleanup()

	fakeHome := filepath.Join(tempDir, "user_home")
	libexecDir := filepath.Join(fakeHome, ".local", "libexec", "agy-pool")
	_ = os.MkdirAll(libexecDir, 0755)
	managedAgy := filepath.Join(libexecDir, "agy-native")
	_ = os.WriteFile(managedAgy, []byte("#!/bin/sh\nexit 0\n"), 0755)

	t.Setenv("HOME", fakeHome)
	t.Setenv("CLOUD_CODE_URL", "http://127.0.0.1:9999")
	diagnostics.SetAgyBinaryFinder(nil)

	var capturedBinary string
	var capturedArgv []string
	var capturedEnv []string
	ExecHandler = func(binary string, argv []string, env []string) error {
		capturedBinary = binary
		capturedArgv = argv
		capturedEnv = env
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := Main([]string{"raw", "--", "--mode", "accept-edits"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected code 0, got %d (stderr: %s)", code, stderr.String())
	}

	if capturedBinary != managedAgy {
		t.Fatalf("expected exec binary %q, got %q", managedAgy, capturedBinary)
	}

	for _, e := range capturedEnv {
		if strings.HasPrefix(e, "CLOUD_CODE_URL=") {
			t.Errorf("expected CLOUD_CODE_URL to be stripped in raw bypass, but found: %s", e)
		}
	}

	expectedArgs := []string{managedAgy, "--mode", "accept-edits"}
	if len(capturedArgv) != len(expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, capturedArgv)
	}
	for i := range expectedArgs {
		if capturedArgv[i] != expectedArgs[i] {
			t.Errorf("arg[%d] mismatch: expected %q, got %q", i, expectedArgs[i], capturedArgv[i])
		}
	}
}

// F & H. RunAgyWithLB valid sync path executes native agy and preserves privacy
func TestRunAgyWithLB_ValidSyncExecutesAgy(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	pool := storage.NewEmptyPool()
	acc := &storage.Account{
		ID:          "acc_1",
		Name:        "Main",
		Email:       "user@example.com",
		AccessToken: "valid-access-token",
	}
	pool.Accounts = []*storage.Account{acc}
	pool.ActiveAccountID = &acc.ID
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}

	fakeAgy := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(fakeAgy, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	diagnostics.SetAgyBinaryFinder(func() string { return fakeAgy })
	defer diagnostics.SetAgyBinaryFinder(nil)

	var execCalled bool
	oldExec := ExecHandler
	defer func() { ExecHandler = oldExec }()
	ExecHandler = func(binary string, argv []string, env []string) error {
		execCalled = true
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := Main([]string{"run", "--", "-p", "hello"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected code 0 on valid sync, got %d (stderr: %s)", code, stderr.String())
	}
	if !execCalled {
		t.Fatalf("expected ExecHandler to be called on valid sync")
	}

	// Verify token file was written
	tokenFile := config.GetAgyTokenFile()
	if _, err := os.Stat(tokenFile); err != nil {
		t.Fatalf("expected token file %s to exist after sync: %v", tokenFile, err)
	}
}

// G & H. RunAgyWithLB failed sync path aborts without exec and preserves privacy
func TestRunAgyWithLB_SyncFailureAborts(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	secretToken := "confidential-token-secret-999"
	secretEmail := "super-confidential@secret-corp.internal"
	pool := storage.NewEmptyPool()
	acc := &storage.Account{
		ID:          "acc_1",
		Name:        "Main",
		Email:       secretEmail,
		AccessToken: secretToken,
	}
	pool.Accounts = []*storage.Account{acc}
	pool.ActiveAccountID = &acc.ID
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}

	// Make token path a directory so writing fails
	if err := os.MkdirAll(config.GetAgyTokenFile(), 0o700); err != nil {
		t.Fatal(err)
	}

	fakeAgy := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(fakeAgy, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	diagnostics.SetAgyBinaryFinder(func() string { return fakeAgy })
	defer diagnostics.SetAgyBinaryFinder(nil)

	var execCalled bool
	oldExec := ExecHandler
	defer func() { ExecHandler = oldExec }()
	ExecHandler = func(string, []string, []string) error {
		execCalled = true
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := Main([]string{"run", "--", "-p", "hello"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit code on sync failure, got 0")
	}
	if execCalled {
		t.Fatalf("CRITICAL: ExecHandler was called despite sync failure!")
	}

	stderrStr := stderr.String()
	if !strings.Contains(stderrStr, "Failed to synchronize native agy credentials") {
		t.Fatalf("expected sync failure error in stderr, got: %s", stderrStr)
	}
	if strings.Contains(stderrStr, secretToken) {
		t.Fatalf("token secret leaked in stderr: %s", stderrStr)
	}
	if strings.Contains(stderrStr, secretEmail) {
		t.Fatalf("email leaked in stderr: %s", stderrStr)
	}
}
