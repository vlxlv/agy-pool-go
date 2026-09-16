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
	resetSandbox := config.SetSyntheticSandboxRoot(tempDir)

	testHome := filepath.Join(tempDir, "test_home")
	_ = os.MkdirAll(filepath.Join(testHome, ".local", "bin"), 0755)
	_ = os.MkdirAll(filepath.Join(testHome, ".config", "agy-pool"), 0755)
	_ = os.MkdirAll(filepath.Join(testHome, ".local", "share", "agy-pool"), 0755)
	resetRoots := SetTestInstallerRoots(&InstallerRoots{
		UserHome:   testHome,
		BinDir:     filepath.Join(testHome, ".local", "bin"),
		ConfigDir:  filepath.Join(testHome, ".config", "agy-pool"),
		DataDir:    filepath.Join(testHome, ".local", "share", "agy-pool"),
		SystemRoot: filepath.Join(tempDir, "test_system"),
	})

	if err := config.ConfigureStateDir(stateDir); err != nil {
		t.Fatalf("ConfigureStateDir failed: %v", err)
	}

	cleanup := func() {
		resetRoots()
		resetSandbox()
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

	// Create fake native agy binary to satisfy installer precondition
	fakeNativeAgy := filepath.Join(tempDir, "fake_native_agy")
	if err := os.WriteFile(fakeNativeAgy, []byte("#!/bin/sh\necho native agy v1.2.2\n"), 0755); err != nil {
		t.Fatalf("failed to create fake native agy: %v", err)
	}

	// Create fake .bashrc
	bashrc := filepath.Join(fakeHome, ".bashrc")
	_ = os.WriteFile(bashrc, []byte("# Existing bashrc content\nexport FOO=bar\n"), 0644)

	opts := Options{
		SourceBinary:  srcBin,
		TargetDir:     targetDir,
		HomeDir:       fakeHome,
		NativeAgyPath: fakeNativeAgy,
		SkipShellRC:   false,
	}

	// First install
	if err := Install(opts); err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	// Verify installed files
	installedPool := filepath.Join(targetDir, "agy-pool")
	installedAgy := filepath.Join(targetDir, "agy")
	installedRaw := filepath.Join(targetDir, "agy-raw")
	installedOrig := filepath.Join(targetDir, "agy-orig")

	if fi, err := os.Stat(installedPool); err != nil || fi.Mode()&0111 == 0 {
		t.Errorf("installed agy-pool missing or not executable: %v", err)
	}
	if fi, err := os.Stat(installedAgy); err != nil || fi.Mode()&0111 == 0 {
		t.Errorf("installed agy shim missing or not executable: %v", err)
	}
	// Verify agy shim contains run command with -c and -D
	agyShimBytes, _ := os.ReadFile(installedAgy)
	if !strings.Contains(string(agyShimBytes), "agy-pool run") || !strings.Contains(string(agyShimBytes), "-c") {
		t.Errorf("installed agy shim does not invoke agy-pool run: %s", string(agyShimBytes))
	}

	if fi, err := os.Stat(installedRaw); err != nil || fi.Mode()&0111 == 0 {
		t.Errorf("installed agy-raw missing or not executable: %v", err)
	}
	rawShimBytes, _ := os.ReadFile(installedRaw)
	if !strings.Contains(string(rawShimBytes), "agy-pool raw") {
		t.Errorf("installed agy-raw shim does not invoke agy-pool raw: %s", string(rawShimBytes))
	}

	// Check agy-orig is a symlink pointing to agy-raw
	target, err := os.Readlink(installedOrig)
	if err != nil || (target != "agy-raw" && target != installedRaw) {
		t.Errorf("expected agy-orig symlink to agy-raw, got %q (err: %v)", target, err)
	}

	// Verify user config and data directories were initialized
	userCfg := filepath.Join(fakeHome, ".config", "agy-pool", "config.json")
	if _, err := os.Stat(userCfg); err != nil {
		t.Errorf("expected default config file at %s, got %v", userCfg, err)
	}
	userLocks := filepath.Join(fakeHome, ".local", "share", "agy-pool", "locks")
	if fi, err := os.Stat(userLocks); err != nil || !fi.IsDir() {
		t.Errorf("expected user data locks dir at %s, got %v", userLocks, err)
	}

	// Upgrade / Second Install (Idempotence)
	if err := Install(opts); err != nil {
		t.Fatalf("Second Install failed: %v", err)
	}

	// Uninstallation
	if err := Uninstall(opts); err != nil {
		t.Fatalf("Uninstall failed: %v", err)
	}

	if _, err := os.Stat(installedPool); !os.IsNotExist(err) {
		t.Errorf("expected agy-pool to be removed")
	}
	if _, err := os.Stat(installedAgy); !os.IsNotExist(err) {
		t.Errorf("expected agy shim to be removed")
	}
	if _, err := os.Stat(installedRaw); !os.IsNotExist(err) {
		t.Errorf("expected agy-raw to be removed")
	}
	if _, err := os.Lstat(installedOrig); !os.IsNotExist(err) {
		t.Errorf("expected agy-orig to be removed")
	}

	// Mutable data directory must be preserved after uninstall
	if _, err := os.Stat(userLocks); os.IsNotExist(err) {
		t.Errorf("mutable data directory was deleted during uninstall")
	}
}

func TestInstall_PreconditionFailsWhenNativeAgyMissing(t *testing.T) {
	tempDir, cleanup := setupInstallerTestEnv(t)
	defer cleanup()

	targetDir := filepath.Join(tempDir, "install_bin")
	_ = os.MkdirAll(targetDir, 0755)

	opts := Options{
		TargetDir:     targetDir,
		HomeDir:       tempDir,
		NativeAgyPath: "/nonexistent/path/to/agy",
	}

	err := Install(opts)
	if err == nil {
		t.Fatalf("expected Install to fail when native agy is missing, got nil")
	}
	if !strings.Contains(err.Error(), "Native agy was not found") {
		t.Fatalf("expected error containing 'Native agy was not found', got: %v", err)
	}
}

func TestInstall_SystemModeSyntheticRoot(t *testing.T) {
	tempDir, cleanup := setupInstallerTestEnv(t)
	defer cleanup()

	fakeNativeAgy := filepath.Join(tempDir, "fake_native_agy")
	_ = os.WriteFile(fakeNativeAgy, []byte("#!/bin/sh\necho native agy\n"), 0755)

	targetDir := filepath.Join(tempDir, "sys_bin")
	cfgDir := filepath.Join(tempDir, "sys_etc")
	dataDir := filepath.Join(tempDir, "sys_var")

	opts := Options{
		IsSystem:      true,
		TargetDir:     targetDir,
		ConfigDir:     cfgDir,
		DataDir:       dataDir,
		NativeAgyPath: fakeNativeAgy,
		SkipShellRC:   true,
	}

	if err := Install(opts); err != nil {
		t.Fatalf("System Install failed: %v", err)
	}

	// Verify system shim content
	agyShim := filepath.Join(targetDir, "agy")
	content, err := os.ReadFile(agyShim)
	if err != nil {
		t.Fatalf("failed to read installed agy shim: %v", err)
	}
	if !strings.Contains(string(content), "/etc/agy-pool/config.json") || !strings.Contains(string(content), "/var/lib/agy-pool") {
		t.Errorf("expected system config/data paths in agy shim: %s", string(content))
	}

	// Verify config and locks
	if _, err := os.Stat(filepath.Join(cfgDir, "config.json")); err != nil {
		t.Errorf("system config.json not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "locks")); err != nil {
		t.Errorf("system locks dir not created: %v", err)
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
