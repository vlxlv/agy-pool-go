package installer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/diagnostics"
)

func setupInstallerTestEnv(t *testing.T) (string, func()) {
	t.Helper()
	tempDir := t.TempDir()

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

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			resetRoots()
			resetSandbox()
			if origState != "" {
				if _, err := os.Stat(origState); err == nil {
					_ = config.ConfigureStateDir(origState)
				} else {
					config.ResetDataDir()
				}
			} else {
				config.ResetDataDir()
			}
			config.SetTestMode(origTestMode)
		})
	}
	t.Cleanup(cleanup)

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

func TestInstallAndUninstall_NoShellRCModification(t *testing.T) {
	tempDir, cleanup := setupInstallerTestEnv(t)
	defer cleanup()

	fakeHome := filepath.Join(tempDir, "home")
	_ = os.MkdirAll(fakeHome, 0700)

	bashrcContent := []byte("# User personal bashrc\nexport MY_VAR=123\nalias ll='ls -la'\n# End of bashrc\n")
	zshrcContent := []byte("# User personal zshrc\nsetopt prompt_subst\nexport ZSH_THEME=agnoster\n")

	bashrc := filepath.Join(fakeHome, ".bashrc")
	zshrc := filepath.Join(fakeHome, ".zshrc")
	if err := os.WriteFile(bashrc, bashrcContent, 0644); err != nil {
		t.Fatalf("failed to write bashrc: %v", err)
	}
	if err := os.WriteFile(zshrc, zshrcContent, 0644); err != nil {
		t.Fatalf("failed to write zshrc: %v", err)
	}

	targetDir := filepath.Join(fakeHome, ".local", "bin")
	fakeNativeAgy := filepath.Join(tempDir, "fake_native_agy")
	_ = os.WriteFile(fakeNativeAgy, []byte("#!/bin/sh\necho native\n"), 0755)

	opts := Options{
		HomeDir:       fakeHome,
		TargetDir:     targetDir,
		NativeAgyPath: fakeNativeAgy,
		CleanLegacyRC: false,
	}

	// 1. Install must not touch bashrc or zshrc
	if err := Install(opts); err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	afterInstallBashrc, err := os.ReadFile(bashrc)
	if err != nil {
		t.Fatalf("failed to read bashrc: %v", err)
	}
	if !bytes.Equal(afterInstallBashrc, bashrcContent) {
		t.Fatalf("install modified .bashrc! Expected byte-for-byte identity.\nGot:\n%s\nExpected:\n%s", string(afterInstallBashrc), string(bashrcContent))
	}

	afterInstallZshrc, err := os.ReadFile(zshrc)
	if err != nil {
		t.Fatalf("failed to read zshrc: %v", err)
	}
	if !bytes.Equal(afterInstallZshrc, zshrcContent) {
		t.Fatalf("install modified .zshrc! Expected byte-for-byte identity.\nGot:\n%s\nExpected:\n%s", string(afterInstallZshrc), string(zshrcContent))
	}

	// 2. Default uninstall must not touch bashrc or zshrc
	if err := Uninstall(opts); err != nil {
		t.Fatalf("Uninstall failed: %v", err)
	}

	afterUninstallBashrc, err := os.ReadFile(bashrc)
	if err != nil {
		t.Fatalf("failed to read bashrc: %v", err)
	}
	if !bytes.Equal(afterUninstallBashrc, bashrcContent) {
		t.Fatalf("uninstall modified .bashrc! Expected byte-for-byte identity.\nGot:\n%s", string(afterUninstallBashrc))
	}

	afterUninstallZshrc, err := os.ReadFile(zshrc)
	if err != nil {
		t.Fatalf("failed to read zshrc: %v", err)
	}
	if !bytes.Equal(afterUninstallZshrc, zshrcContent) {
		t.Fatalf("uninstall modified .zshrc! Expected byte-for-byte identity.\nGot:\n%s", string(afterUninstallZshrc))
	}
}

func TestUninstall_LegacyRCOptInCleanup(t *testing.T) {
	tempDir, cleanup := setupInstallerTestEnv(t)
	defer cleanup()

	fakeHome := filepath.Join(tempDir, "home_legacy")
	_ = os.MkdirAll(fakeHome, 0700)

	surroundingPrefix := "# Pre-existing user settings\nexport PATH=/custom/bin:$PATH\n"
	surroundingSuffix := "\n# Post-existing user settings\nalias gs='git status'\n"
	fullLegacyContent := surroundingPrefix + AliasBlock + surroundingSuffix

	bashrc := filepath.Join(fakeHome, ".bashrc")
	if err := os.WriteFile(bashrc, []byte(fullLegacyContent), 0644); err != nil {
		t.Fatalf("failed to write bashrc: %v", err)
	}

	targetDir := filepath.Join(fakeHome, ".local", "bin")
	_ = os.MkdirAll(targetDir, 0755)

	opts := Options{
		HomeDir:       fakeHome,
		TargetDir:     targetDir,
		CleanLegacyRC: true,
	}

	if err := Uninstall(opts); err != nil {
		t.Fatalf("Uninstall failed: %v", err)
	}

	cleanedBashrc, err := os.ReadFile(bashrc)
	if err != nil {
		t.Fatalf("failed to read cleaned bashrc: %v", err)
	}

	expectedContent := surroundingPrefix + surroundingSuffix
	if !bytes.Equal(cleanedBashrc, []byte(expectedContent)) {
		t.Fatalf("expected exact byte-preserving cleanup.\nGot:\n%q\nExpected:\n%q", string(cleanedBashrc), expectedContent)
	}
}

func TestInstall_NativePreservationBeforeShimPublication(t *testing.T) {
	tempDir, cleanup := setupInstallerTestEnv(t)
	defer cleanup()

	fakeHome := filepath.Join(tempDir, "fakehome")
	targetDir := filepath.Join(fakeHome, ".local", "bin")
	libexecDir := filepath.Join(fakeHome, ".local", "libexec", "agy-pool")
	_ = os.MkdirAll(targetDir, 0755)

	t.Setenv("HOME", fakeHome)
	t.Setenv("PATH", targetDir)

	// Target directory initially contains the real native agy ELF executable
	initialTargetAgy := filepath.Join(targetDir, "agy")
	nativeContent := "#!/bin/sh\necho 'real native agy v1.2.3'\n"
	if err := os.WriteFile(initialTargetAgy, []byte(nativeContent), 0755); err != nil {
		t.Fatalf("failed to write initial target agy: %v", err)
	}

	srcBin := filepath.Join(tempDir, "built-agy-pool")
	_ = os.WriteFile(srcBin, []byte("#!/bin/sh\necho agy-pool\n"), 0755)

	opts := Options{
		SourceBinary: srcBin,
		TargetDir:    targetDir,
		HomeDir:      fakeHome,
	}

	// Install must discover target agy, preserve it to libexec, and then publish shim
	if err := Install(opts); err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	// 1. Managed native binary exists and contains the exact original content
	managedNative := filepath.Join(libexecDir, "agy-native")
	preservedContent, err := os.ReadFile(managedNative)
	if err != nil {
		t.Fatalf("expected managed native at %s, got error: %v", managedNative, err)
	}
	if string(preservedContent) != nativeContent {
		t.Fatalf("preserved native content mismatch.\nGot: %q\nExpected: %q", string(preservedContent), nativeContent)
	}

	// 2. Target agy is now the managed shim
	shimBytes, err := os.ReadFile(initialTargetAgy)
	if err != nil {
		t.Fatalf("failed to read installed agy shim: %v", err)
	}
	if !strings.Contains(string(shimBytes), ManagedShimMarker) {
		t.Errorf("installed agy is not a managed shim: %s", string(shimBytes))
	}
	if !strings.Contains(string(shimBytes), "agy-pool run") {
		t.Errorf("installed agy shim does not invoke agy-pool run: %s", string(shimBytes))
	}

	// 3. Managed native path is resolvable by diagnostics resolver
	resolved, src, err := diagnostics.ResolveNativeAgyBinary()
	if err != nil {
		t.Fatalf("failed to resolve native agy after install: %v", err)
	}
	if resolved != managedNative || src != diagnostics.SourceManaged {
		t.Errorf("expected resolver to find managed native %q with source %q, got %q (%q)",
			managedNative, diagnostics.SourceManaged, resolved, src)
	}

	// 4. agy shim is rejected as native candidate
	if err := diagnostics.ValidateNativeCandidate(initialTargetAgy); err == nil {
		t.Errorf("expected agy shim to be rejected by ValidateNativeCandidate, got nil")
	}
}

func TestInstall_ReinstallAndOfficialUpgradeRefresh(t *testing.T) {
	tempDir, cleanup := setupInstallerTestEnv(t)
	defer cleanup()

	fakeHome := filepath.Join(tempDir, "fakehome")
	targetDir := filepath.Join(fakeHome, ".local", "bin")
	libexecDir := filepath.Join(fakeHome, ".local", "libexec", "agy-pool")
	_ = os.MkdirAll(targetDir, 0755)

	t.Setenv("HOME", fakeHome)
	t.Setenv("PATH", targetDir)

	targetAgy := filepath.Join(targetDir, "agy")
	managedNative := filepath.Join(libexecDir, "agy-native")

	// Phase 1: First install preserves native agy v1
	v1Content := "#!/bin/sh\necho 'native agy v1'\n"
	_ = os.WriteFile(targetAgy, []byte(v1Content), 0755)

	srcBin := filepath.Join(tempDir, "built-agy-pool")
	_ = os.WriteFile(srcBin, []byte("#!/bin/sh\necho agy-pool\n"), 0755)

	opts := Options{
		SourceBinary: srcBin,
		TargetDir:    targetDir,
		HomeDir:      fakeHome,
	}

	if err := Install(opts); err != nil {
		t.Fatalf("Phase 1 Install failed: %v", err)
	}

	// Phase 2: Reinstall when agy is already shim retains existing managed native
	if err := Install(opts); err != nil {
		t.Fatalf("Phase 2 Reinstall failed: %v", err)
	}
	content, _ := os.ReadFile(managedNative)
	if string(content) != v1Content {
		t.Fatalf("reinstall corrupted managed native content: %s", string(content))
	}

	// Phase 3: Official agy installer overwrites targetDir/agy with new native ELF v2
	v2Content := "#!/bin/sh\necho 'native agy v2 - upgraded by Google'\n"
	_ = os.WriteFile(targetAgy, []byte(v2Content), 0755)

	// agy-pool install should detect new native ELF, refresh managed native, and restore shim
	if err := Install(opts); err != nil {
		t.Fatalf("Phase 3 Install failed: %v", err)
	}

	refreshedContent, err := os.ReadFile(managedNative)
	if err != nil {
		t.Fatalf("failed to read refreshed managed native: %v", err)
	}
	if string(refreshedContent) != v2Content {
		t.Fatalf("expected managed native to be refreshed to v2 content.\nGot: %q\nExpected: %q",
			string(refreshedContent), v2Content)
	}

	restoredShim, _ := os.ReadFile(targetAgy)
	if !strings.Contains(string(restoredShim), ManagedShimMarker) {
		t.Fatalf("target agy was not restored to managed shim after official upgrade repair")
	}
}

func TestUninstall_RemovesManagedNativeWithoutDeletingExternalBinary(t *testing.T) {
	tempDir, cleanup := setupInstallerTestEnv(t)
	defer cleanup()

	fakeHome := filepath.Join(tempDir, "fakehome")
	targetDir := filepath.Join(fakeHome, ".local", "bin")
	libexecDir := filepath.Join(fakeHome, ".local", "libexec", "agy-pool")
	_ = os.MkdirAll(targetDir, 0755)

	// User has an external native agy binary in custom dir
	externalDir := filepath.Join(tempDir, "external_bin")
	_ = os.MkdirAll(externalDir, 0755)
	externalAgy := filepath.Join(externalDir, "agy")
	externalContent := "#!/bin/sh\necho 'external native agy'\n"
	_ = os.WriteFile(externalAgy, []byte(externalContent), 0755)

	opts := Options{
		TargetDir:     targetDir,
		HomeDir:       fakeHome,
		NativeAgyPath: externalAgy,
	}

	if err := Install(opts); err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	managedNative := filepath.Join(libexecDir, "agy-native")
	if _, err := os.Stat(managedNative); err != nil {
		t.Fatalf("expected managed native at %s, got: %v", managedNative, err)
	}

	// Run Uninstall
	if err := Uninstall(opts); err != nil {
		t.Fatalf("Uninstall failed: %v", err)
	}

	// Managed native binary must be removed
	if _, err := os.Stat(managedNative); !os.IsNotExist(err) {
		t.Errorf("expected managed native %s to be removed on uninstall", managedNative)
	}

	// External native agy must remain completely untouched!
	extData, err := os.ReadFile(externalAgy)
	if err != nil {
		t.Fatalf("CRITICAL DEFECT: external native agy was deleted or became unreadable: %v", err)
	}
	if string(extData) != externalContent {
		t.Fatalf("CRITICAL DEFECT: external native agy was modified during uninstall")
	}
}
