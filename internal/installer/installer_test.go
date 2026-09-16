package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/config"
)

func setupInstallerTestEnv(t *testing.T) (string, func()) {
	t.Helper()
	tempDir, err := os.MkdirTemp("", "agy_inst_test_*")
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
		_ = os.RemoveAll(tempDir)
	}

	return tempDir, cleanup
}

func TestDetermineTargetDir_Precedence(t *testing.T) {
	tempDir, cleanup := setupInstallerTestEnv(t)
	defer cleanup()

	fakeHome := filepath.Join(tempDir, "fakehome")
	fakePrefix := filepath.Join(tempDir, "fakeprefix")
	_ = os.MkdirAll(fakeHome, 0755)
	_ = os.MkdirAll(filepath.Join(fakePrefix, "bin"), 0755)

	// 1. Prefix with bin directory takes precedence
	target := DetermineTargetDir(fakePrefix, fakeHome, func() bool { return true })
	expected := filepath.Join(fakePrefix, "bin")
	if target != expected {
		t.Errorf("expected prefix target %q, got %q", expected, target)
	}

	// 2. Without prefix, writable /usr/local/bin takes precedence
	target = DetermineTargetDir("", fakeHome, func() bool { return true })
	if target != "/usr/local/bin" {
		t.Errorf("expected /usr/local/bin, got %q", target)
	}

	// 3. Without prefix and /usr/local/bin not writable, falls back to $HOME/.local/bin
	target = DetermineTargetDir("", fakeHome, func() bool { return false })
	expected = filepath.Join(fakeHome, ".local", "bin")
	if target != expected {
		t.Errorf("expected user bin %q, got %q", expected, target)
	}
}

func TestInstall_IdempotenceAndUninstallation(t *testing.T) {
	tempDir, cleanup := setupInstallerTestEnv(t)
	defer cleanup()

	fakeHome := filepath.Join(tempDir, "fakehome")
	targetDir := filepath.Join(tempDir, "install_bin")
	_ = os.MkdirAll(fakeHome, 0755)
	_ = os.MkdirAll(targetDir, 0755)

	// Create fake source binary
	srcBin := filepath.Join(tempDir, "built-agy-pool")
	if err := os.WriteFile(srcBin, []byte("#!/bin/sh\necho agy-pool v1\n"), 0755); err != nil {
		t.Fatalf("failed to create source binary: %v", err)
	}

	// Create fake .bashrc
	bashrc := filepath.Join(fakeHome, ".bashrc")
	_ = os.WriteFile(bashrc, []byte("# Existing bashrc content\nexport FOO=bar\n"), 0644)

	opts := Options{
		SourceBinary: srcBin,
		TargetDir:    targetDir,
		HomeDir:      fakeHome,
		SkipShellRC:  false,
	}

	// First install
	if err := Install(opts); err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	// Verify installed files
	installedPool := filepath.Join(targetDir, "agy-pool")
	installedRaw := filepath.Join(targetDir, "agy-raw")
	installedOrig := filepath.Join(targetDir, "agy-orig")

	if fi, err := os.Stat(installedPool); err != nil || fi.Mode()&0111 == 0 {
		t.Errorf("installed agy-pool missing or not executable: %v", err)
	}
	if fi, err := os.Stat(installedRaw); err != nil || fi.Mode()&0111 == 0 {
		t.Errorf("installed agy-raw missing or not executable: %v", err)
	}
	// Check agy-orig is a symlink pointing to agy-raw
	target, err := os.Readlink(installedOrig)
	if err != nil || target != installedRaw {
		t.Errorf("expected agy-orig symlink to %q, got %q (err: %v)", installedRaw, target, err)
	}

	// Verify .bashrc has alias block exactly once
	rcContent, _ := os.ReadFile(bashrc)
	if strings.Count(string(rcContent), MarkerStart) != 1 {
		t.Errorf("expected alias block count 1, got %d", strings.Count(string(rcContent), MarkerStart))
	}

	// Upgrade / Second Install (Idempotence)
	if err := Install(opts); err != nil {
		t.Fatalf("Second Install failed: %v", err)
	}
	rcContent, _ = os.ReadFile(bashrc)
	if strings.Count(string(rcContent), MarkerStart) != 1 {
		t.Errorf("expected alias block count to remain 1 after second install, got %d", strings.Count(string(rcContent), MarkerStart))
	}

	// Uninstallation
	if err := Uninstall(opts); err != nil {
		t.Fatalf("Uninstall failed: %v", err)
	}

	if _, err := os.Stat(installedPool); !os.IsNotExist(err) {
		t.Errorf("expected agy-pool to be removed")
	}
	if _, err := os.Stat(installedRaw); !os.IsNotExist(err) {
		t.Errorf("expected agy-raw to be removed")
	}
	if _, err := os.Lstat(installedOrig); !os.IsNotExist(err) {
		t.Errorf("expected agy-orig to be removed")
	}

	rcCleaned, _ := os.ReadFile(bashrc)
	if strings.Contains(string(rcCleaned), MarkerStart) {
		t.Errorf("alias block was not removed from rc file: %s", string(rcCleaned))
	}
	if !strings.Contains(string(rcCleaned), "export FOO=bar") {
		t.Errorf("original rc content was corrupted: %s", string(rcCleaned))
	}
}

func TestAssertSafeInstallTarget_FailClosed(t *testing.T) {
	_, cleanup := setupInstallerTestEnv(t)
	defer cleanup()

	// In test mode, /usr/local/bin must be rejected
	err := AssertSafeInstallTarget("/usr/local/bin")
	if err == nil {
		t.Fatalf("expected fail-closed error when installing to /usr/local/bin in test mode")
	}
	if !strings.Contains(err.Error(), "FAIL-CLOSED GUARD") {
		t.Errorf("expected FAIL-CLOSED GUARD message, got %v", err)
	}
}
