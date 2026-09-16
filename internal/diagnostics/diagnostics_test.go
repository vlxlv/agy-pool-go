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
	config.SetTestMode(true)
	if err := config.ConfigureStateDir(stateDir); err != nil {
		t.Fatalf("ConfigureStateDir failed: %v", err)
	}

	cleanup := func() {
		_ = config.ConfigureStateDir(origState)
		config.SetTestMode(origTestMode)
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
