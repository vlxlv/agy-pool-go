package diagnostics

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
	_ "modernc.org/sqlite"
)

func setupDoctorTestEnv(t *testing.T) (string, func()) {
	t.Helper()
	tempDir, err := os.MkdirTemp("", "agy_doc_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	stateDir := filepath.Join(tempDir, ".gemini")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatalf("failed to create state dir: %v", err)
	}

	origState := config.GetStateDir()
	origTestMode := config.IsTestMode()
	origGeminiDir := os.Getenv("AGY_GEMINI_DIR")
	config.SetTestMode(true)
	os.Setenv("AGY_GEMINI_DIR", stateDir)
	if err := config.ConfigureStateDir(stateDir); err != nil {
		t.Fatalf("ConfigureStateDir failed: %v", err)
	}

	cleanup := func() {
		_ = config.ConfigureStateDir(origState)
		config.SetTestMode(origTestMode)
		if origGeminiDir != "" {
			os.Setenv("AGY_GEMINI_DIR", origGeminiDir)
		} else {
			os.Unsetenv("AGY_GEMINI_DIR")
		}
		SetAgyBinaryFinder(nil)
		SetAgyVersionProvider(nil)
		SetBackendHostProvider(nil)
		SetTLSProber(nil)
		_ = os.RemoveAll(tempDir)
	}

	return tempDir, cleanup
}

func TestBinaryDiscoveryAndRecursionProtection(t *testing.T) {
	tempDir, cleanup := setupDoctorTestEnv(t)
	defer cleanup()

	// Create a fake agy-pool binary and a fake native agy binary
	binDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(binDir, 0755)

	selfBin := filepath.Join(binDir, "agy-pool")
	if err := os.WriteFile(selfBin, []byte("#!/bin/sh\necho agy-pool\n"), 0755); err != nil {
		t.Fatalf("failed to create fake self: %v", err)
	}

	realAgy := filepath.Join(binDir, "real-agy")
	if err := os.WriteFile(realAgy, []byte("#!/bin/sh\necho '1.2.3'\n"), 0755); err != nil {
		t.Fatalf("failed to create fake real agy: %v", err)
	}

	symlinkToSelf := filepath.Join(binDir, "agy")
	if err := os.Symlink(selfBin, symlinkToSelf); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	// 1. AGY_BIN points to self -> recursion protection skips it
	os.Setenv("AGY_BIN", symlinkToSelf)
	os.Setenv("AGY_SELF_PATH", selfBin)
	defer os.Unsetenv("AGY_BIN")
	defer os.Unsetenv("AGY_SELF_PATH")

	found := FindRealAgyBinary()
	if found == symlinkToSelf || found == selfBin {
		t.Fatalf("expected self to be skipped by recursion protection, got %q", found)
	}

	// 2. AGY_BIN points to real binary -> returns it
	os.Setenv("AGY_BIN", realAgy)
	found = FindRealAgyBinary()
	if found != realAgy {
		t.Fatalf("expected realAgy %q, got %q", realAgy, found)
	}

	// 3. Test GetInstalledAgyVersion
	v := GetInstalledAgyVersion()
	if v != "1.2.3" {
		t.Fatalf("expected version 1.2.3, got %q", v)
	}
}

func TestRunDoctor_EmptyEnvironment(t *testing.T) {
	_, cleanup := setupDoctorTestEnv(t)
	defer cleanup()

	// Mock native agy and TLS so test runs offline
	SetAgyBinaryFinder(func() string { return "/bin/true" })
	SetAgyVersionProvider(func() string { return "1.2.2" })
	SetTLSProber(func(host string, timeout time.Duration) error { return nil })

	var buf bytes.Buffer
	ok := RunDoctor(&buf)
	if !ok {
		t.Fatalf("expected RunDoctor to succeed on empty environment, got false")
	}

	out := buf.String()
	if !strings.Contains(out, "Antigravity System Doctor") {
		t.Errorf("missing title in doctor output")
	}
	if !strings.Contains(out, "Go Runtime:") {
		t.Errorf("missing Go Runtime in doctor output")
	}
	if !strings.Contains(out, "POSIX Concurrency:") {
		t.Errorf("missing Concurrency in doctor output")
	}
	if !strings.Contains(out, "No account pool file found yet") {
		t.Errorf("expected missing pool message, got: %s", out)
	}
	if !strings.Contains(out, "Cloud Code API: Reachable via TLS") {
		t.Errorf("expected TLS reachable message, got: %s", out)
	}
}

func TestRunDoctor_PopulatedPoolAndPrivacy(t *testing.T) {
	_, cleanup := setupDoctorTestEnv(t)
	defer cleanup()

	// Mock native agy and TLS
	SetAgyBinaryFinder(func() string { return "/usr/local/bin/agy" })
	SetAgyVersionProvider(func() string { return "1.2.2" })
	SetTLSProber(func(host string, timeout time.Duration) error { return nil })

	// Populate pool with accounts
	secretEmail := "secret_user_email_12345@gmail.com"
	now := time.Now().Unix()
	activeID := "acc_1"
	acc1 := &storage.Account{
		ID:        "acc_1",
		Email:     secretEmail,
		Name:      "Work Account",
		Status:    "active",
		CreatedAt: &now,
	}
	acc2 := &storage.Account{
		ID:        "acc_2",
		Email:     "restricted_person@example.com",
		Status:    "validation_required",
		CreatedAt: &now,
	}

	pool := storage.Pool{
		Version:         1,
		ActiveAccountID: &activeID,
		Strategy:        config.StrategyMaxQuota,
		Accounts:        []*storage.Account{acc1, acc2},
	}

	if err := storage.SavePool(&pool); err != nil {
		t.Fatalf("failed to save pool: %v", err)
	}

	// Create session DB
	dbDir := config.GetAgyCliDir()
	_ = os.MkdirAll(dbDir, 0700)
	dbPath := filepath.Join(dbDir, "conversation_summaries.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	_, _ = db.Exec("CREATE TABLE conversation_summaries (conversation_id TEXT);")
	_, _ = db.Exec("INSERT INTO conversation_summaries VALUES ('c1'), ('c2'), ('c3');")
	db.Close()

	// Create log file
	logPath := config.GetLogFile()
	_ = os.WriteFile(logPath, []byte("2026-09-16 test log entry\n"), 0600)

	var buf bytes.Buffer
	ok := RunDoctor(&buf)
	out := buf.String()

	// Should succeed (validation_required is a warning, not fatal issue)
	if !ok {
		t.Fatalf("expected RunDoctor to return true (warnings only), got false. Output:\n%s", out)
	}

	// PRIVACY ASSERTION: user emails must never appear in doctor output
	if strings.Contains(out, secretEmail) {
		t.Errorf("PRIVACY VIOLATION: sensitive email %q found in doctor output!", secretEmail)
	}
	if strings.Contains(out, "restricted_person@example.com") {
		t.Errorf("PRIVACY VIOLATION: restricted email found in doctor output!")
	}

	// Friendly name or Account ID should appear
	if !strings.Contains(out, "Active: Work Account") {
		t.Errorf("expected active account display name 'Work Account', got:\n%s", out)
	}
	if !strings.Contains(out, "Session Continuity: Database active (3 conversation(s) recorded)") {
		t.Errorf("expected session DB count 3, got:\n%s", out)
	}
	if !strings.Contains(out, "Gateway Log:") {
		t.Errorf("expected gateway log line, got:\n%s", out)
	}
}

func TestRunDoctor_FatalErrorOnCorruptedPool(t *testing.T) {
	_, cleanup := setupDoctorTestEnv(t)
	defer cleanup()

	SetAgyBinaryFinder(func() string { return "/bin/true" })
	SetTLSProber(func(host string, timeout time.Duration) error { return nil })

	// Write invalid/corrupt JSON to accounts file
	_ = storage.EnsureDirs()
	poolFile := config.GetAccountsFile()
	_ = os.WriteFile(poolFile, []byte("{corrupt json data"), 0600)

	var buf bytes.Buffer
	ok := RunDoctor(&buf)
	if ok {
		t.Fatalf("expected RunDoctor to return false on corrupted pool file")
	}

	out := buf.String()
	if !strings.Contains(out, "Account Pool: Failed to read pool file") {
		t.Errorf("expected corrupt pool error message, got: %s", out)
	}
}

func TestRunDoctor_TLSFailureWarning(t *testing.T) {
	_, cleanup := setupDoctorTestEnv(t)
	defer cleanup()

	SetAgyBinaryFinder(func() string { return "/bin/true" })
	// Mock TLS probe failure
	SetTLSProber(func(host string, timeout time.Duration) error {
		return errors.New("network unreachable")
	})

	var buf bytes.Buffer
	ok := RunDoctor(&buf)
	// TLS failure should be a warning, NOT a fatal error, so RunDoctor returns true
	if !ok {
		t.Fatalf("expected RunDoctor to return true (warning only) on TLS failure, got false")
	}

	out := buf.String()
	if !strings.Contains(out, "Cloud Code API: Connection to cloudaicompanion.googleapis.com failed: network unreachable") {
		t.Errorf("expected TLS warning message, got: %s", out)
	}
}

func TestRunDoctor_StateDBInspectionAndCorruption(t *testing.T) {
	_, cleanup := setupDoctorTestEnv(t)
	defer cleanup()

	SetAgyBinaryFinder(func() string { return "/bin/true" })
	SetTLSProber(func(host string, timeout time.Duration) error { return nil })

	// 1. Healthy state.db
	secretEmail := "hidden_doctor_user@example.com"
	secretToken := "ya29.sensitive_secret_token_12345"
	now := time.Now().Unix()
	pool := &storage.Pool{
		Version: 1,
		Accounts: []*storage.Account{
			{
				ID:          "acc_db1",
				Name:        "Test SQLite User",
				Email:       secretEmail,
				AccessToken: secretToken,
				Status:      "active",
				CreatedAt:   &now,
			},
		},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("failed to save pool into state.db: %v", err)
	}

	var buf bytes.Buffer
	ok := RunDoctor(&buf)
	if !ok {
		t.Fatalf("expected RunDoctor to succeed on valid state.db")
	}
	out := buf.String()
	if !strings.Contains(out, "state.db: schema v1, readable") {
		t.Errorf("expected state.db schema and readability in doctor output, got:\n%s", out)
	}
	if !strings.Contains(out, "1 account(s) configured") {
		t.Errorf("expected account count 1 in doctor output, got:\n%s", out)
	}
	if strings.Contains(out, secretEmail) || strings.Contains(out, secretToken) {
		t.Errorf("PRIVACY VIOLATION: sensitive email or token leaked in doctor output:\n%s", out)
	}

	// 2. Corrupt state.db (0-byte file)
	stateDBFile := config.GetStateDBFile()
	if err := os.Truncate(stateDBFile, 0); err != nil {
		t.Fatalf("failed to truncate state.db: %v", err)
	}

	buf.Reset()
	ok = RunDoctor(&buf)
	if ok {
		t.Fatalf("expected RunDoctor to fail on 0-byte corrupt state.db")
	}
	out = buf.String()
	if !strings.Contains(out, "Failed to read pool file") {
		t.Errorf("expected error message on corrupt state.db, got:\n%s", out)
	}
}

func TestDiscovery_PrecedenceAndFallbacks(t *testing.T) {
	tempDir, cleanup := setupDoctorTestEnv(t)
	defer cleanup()

	// 1. Create native candidates in multiple locations
	prefixDir := filepath.Join(tempDir, "prefix")
	prefixBin := filepath.Join(prefixDir, "bin")
	_ = os.MkdirAll(prefixBin, 0755)
	prefixAgy := filepath.Join(prefixBin, "agy")
	_ = os.WriteFile(prefixAgy, []byte("#!/bin/sh\necho prefix\n"), 0755)

	userDir := filepath.Join(tempDir, "userhome")
	userBin := filepath.Join(userDir, ".local", "bin")
	_ = os.MkdirAll(userBin, 0755)
	userAgy := filepath.Join(userBin, "agy")
	_ = os.WriteFile(userAgy, []byte("#!/bin/sh\necho user\n"), 0755)

	pathDir := filepath.Join(tempDir, "pathbin")
	_ = os.MkdirAll(pathDir, 0755)
	pathAgy := filepath.Join(pathDir, "agy")
	_ = os.WriteFile(pathAgy, []byte("#!/bin/sh\necho path\n"), 0755)

	overrideDir := filepath.Join(tempDir, "override")
	_ = os.MkdirAll(overrideDir, 0755)
	overrideAgy := filepath.Join(overrideDir, "custom-agy")
	_ = os.WriteFile(overrideAgy, []byte("#!/bin/sh\necho override\n"), 0755)

	configAgy := filepath.Join(overrideDir, "config-agy")
	_ = os.WriteFile(configAgy, []byte("#!/bin/sh\necho config\n"), 0755)

	t.Setenv("HOME", userDir)
	t.Setenv("PATH", pathDir)
	t.Setenv("PREFIX", prefixDir)

	// A. When AGY_BIN is set, it wins with SourceEnvAgyBin
	t.Setenv("AGY_BIN", overrideAgy)
	p, src, err := ResolveNativeAgyBinary()
	if err != nil || p != overrideAgy || src != SourceEnvAgyBin {
		t.Fatalf("expected AGY_BIN override to win: p=%q, src=%q, err=%v", p, src, err)
	}

	// B. When AGY_BIN unset, config.native_agy.binary wins
	t.Setenv("AGY_BIN", "")
	origCfg := config.GetStaticConfig()
	mockCfg := *origCfg
	mockCfg.NativeAgy.Binary = &configAgy
	config.SetStaticConfig(&mockCfg)
	defer config.SetStaticConfig(origCfg)

	p, src, err = ResolveNativeAgyBinary()
	if err != nil || p != configAgy || src != SourceConfig {
		t.Fatalf("expected config native_agy.binary to win: p=%q, src=%q, err=%v", p, src, err)
	}

	// C. When config is nil/empty, PREFIX wins over user home and PATH
	mockCfg.NativeAgy.Binary = nil
	p, src, err = ResolveNativeAgyBinary()
	if err != nil || p != prefixAgy || src != SourcePrefix {
		t.Fatalf("expected PREFIX to win fallback precedence: p=%q, src=%q, err=%v", p, src, err)
	}

	// D. When PREFIX unset, ~/.local/bin/agy wins over PATH
	t.Setenv("PREFIX", "")
	p, src, err = ResolveNativeAgyBinary()
	if err != nil || p != userAgy || src != SourceUserLocal {
		t.Fatalf("expected ~/.local/bin to win over PATH: p=%q, src=%q, err=%v", p, src, err)
	}

	// E. When user home agy is a managed shim, it is skipped and PATH candidate wins
	shimContent := "#!/bin/sh\n# agy-pool managed shim v1\nexec agy-pool run -- \"$@\"\n"
	_ = os.WriteFile(userAgy, []byte(shimContent), 0755)
	p, src, err = ResolveNativeAgyBinary()
	if err != nil || p != pathAgy || src != SourcePathEnv {
		t.Fatalf("expected managed shim to be skipped and PATH to win: p=%q, src=%q, err=%v", p, src, err)
	}
}

func TestDiscovery_ExplicitOverrideFailure(t *testing.T) {
	tempDir, cleanup := setupDoctorTestEnv(t)
	defer cleanup()

	// 1. Missing AGY_BIN fails clearly without silent fallback
	t.Setenv("AGY_BIN", filepath.Join(tempDir, "nonexistent"))
	p, _, err := ResolveNativeAgyBinary()
	if err == nil {
		t.Fatalf("expected error on nonexistent AGY_BIN, got path %q", p)
	}
	if !strings.Contains(err.Error(), "explicit AGY_BIN override") {
		t.Errorf("expected explicit AGY_BIN error, got: %v", err)
	}

	// 2. Non-executable AGY_BIN fails clearly
	nonExec := filepath.Join(tempDir, "non_exec")
	_ = os.WriteFile(nonExec, []byte("echo hi"), 0644)
	t.Setenv("AGY_BIN", nonExec)
	_, _, err = ResolveNativeAgyBinary()
	if err == nil {
		t.Fatal("expected error on non-executable AGY_BIN")
	}

	// 3. Self AGY_BIN fails clearly
	t.Setenv("AGY_BIN", "")
	origCfg := config.GetStaticConfig()
	mockCfg := *origCfg
	badPath := filepath.Join(tempDir, "bad_cfg_bin")
	mockCfg.NativeAgy.Binary = &badPath
	config.SetStaticConfig(&mockCfg)
	defer config.SetStaticConfig(origCfg)

	_, _, err = ResolveNativeAgyBinary()
	if err == nil {
		t.Fatal("expected error on invalid config native_agy.binary")
	}
	if !strings.Contains(err.Error(), "explicit config native_agy.binary") {
		t.Errorf("expected explicit config error message, got: %v", err)
	}
}

func TestDiscovery_HardLinkProtection(t *testing.T) {
	tempDir, cleanup := setupDoctorTestEnv(t)
	defer cleanup()

	binDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(binDir, 0755)

	// Create fake agy-pool binary
	selfBin := filepath.Join(binDir, "agy-pool")
	_ = os.WriteFile(selfBin, []byte("#!/bin/sh\necho self\n"), 0755)
	t.Setenv("AGY_SELF_PATH", selfBin)

	// Create a hard link with completely unrelated name
	hardLinkAgy := filepath.Join(binDir, "some-other-name-agy")
	if err := os.Link(selfBin, hardLinkAgy); err != nil {
		t.Skipf("hard links not supported on this filesystem: %v", err)
	}

	// ValidateNativeCandidate must reject hard link as self-recursion
	err := ValidateNativeCandidate(hardLinkAgy)
	if err == nil {
		t.Fatalf("expected hard link to agy-pool to be rejected as self-recursion!")
	}
	if !strings.Contains(err.Error(), "recursion prevented") {
		t.Errorf("expected recursion prevented error, got %v", err)
	}
}

func TestDiscovery_SymlinkChainProtection(t *testing.T) {
	tempDir, cleanup := setupDoctorTestEnv(t)
	defer cleanup()

	binDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(binDir, 0755)

	selfBin := filepath.Join(binDir, "agy-pool")
	_ = os.WriteFile(selfBin, []byte("#!/bin/sh\necho self\n"), 0755)
	t.Setenv("AGY_SELF_PATH", selfBin)

	// Chain: link1 -> link2 -> link3 -> selfBin
	link3 := filepath.Join(binDir, "link3")
	link2 := filepath.Join(binDir, "link2")
	link1 := filepath.Join(binDir, "link1")
	_ = os.Symlink(selfBin, link3)
	_ = os.Symlink(link3, link2)
	_ = os.Symlink(link2, link1)

	err := ValidateNativeCandidate(link1)
	if err == nil {
		t.Fatalf("expected symlink chain resolving to self to be rejected!")
	}
}

func TestDiscovery_LegitimateScriptContent(t *testing.T) {
	tempDir, cleanup := setupDoctorTestEnv(t)
	defer cleanup()

	binDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(binDir, 0755)

	// Script mentions 'agy-pool', 'python', '.test' in comments or docstrings,
	// but is NOT an agy-pool managed shim
	scriptPath := filepath.Join(binDir, "real_custom_agy.sh")
	scriptBody := `#!/usr/bin/env bash
# This script interacts with python and agy-pool in some tests.
# It is a legitimate wrapper around upstream antigravity.
echo "native agy 2.5.0"
`
	if err := os.WriteFile(scriptPath, []byte(scriptBody), 0755); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	err := ValidateNativeCandidate(scriptPath)
	if err != nil {
		t.Fatalf("legitimate script was falsely rejected: %v", err)
	}
}
