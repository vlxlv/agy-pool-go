package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/config"
)

func TestInstallerIsolation_UserModeHermeticRoots(t *testing.T) {
	tempDir := t.TempDir()
	resetSandbox := config.SetSyntheticSandboxRoot(tempDir)
	defer resetSandbox()

	origTestMode := config.IsTestMode()
	config.SetTestMode(true)
	defer config.SetTestMode(origTestMode)

	testHome := filepath.Join(tempDir, "user_home")
	_ = os.MkdirAll(filepath.Join(testHome, ".local", "bin"), 0755)
	_ = os.MkdirAll(filepath.Join(testHome, ".config", "agy-pool"), 0755)
	_ = os.MkdirAll(filepath.Join(testHome, ".local", "share", "agy-pool"), 0755)

	resetRoots := SetTestInstallerRoots(&InstallerRoots{
		UserHome:   testHome,
		BinDir:     filepath.Join(testHome, ".local", "bin"),
		ConfigDir:  filepath.Join(testHome, ".config", "agy-pool"),
		DataDir:    filepath.Join(testHome, ".local", "share", "agy-pool"),
		SystemRoot: filepath.Join(tempDir, "sys_root"),
	})
	defer resetRoots()

	// Mock native agy
	mockAgy := filepath.Join(tempDir, "native_agy")
	if err := os.WriteFile(mockAgy, []byte("#!/bin/sh\necho mock native\n"), 0755); err != nil {
		t.Fatalf("failed to write mock native agy: %v", err)
	}

	roots := GetInstallerRoots()
	if !strings.HasPrefix(roots.BinDir, tempDir) {
		t.Fatalf("roots.BinDir %q is not inside tempDir %q", roots.BinDir, tempDir)
	}
	if !strings.HasPrefix(roots.ConfigDir, tempDir) {
		t.Fatalf("roots.ConfigDir %q is not inside tempDir %q", roots.ConfigDir, tempDir)
	}
	if !strings.HasPrefix(roots.DataDir, tempDir) {
		t.Fatalf("roots.DataDir %q is not inside tempDir %q", roots.DataDir, tempDir)
	}
	// Check whether real host entrypoint exists before test
	realHome := config.GetRealHostUserHome()
	realLocalBinAgyPool := filepath.Join(realHome, ".local", "bin", "agy-pool")
	_, errRealBefore := os.Lstat(realLocalBinAgyPool)
	realExistedBefore := errRealBefore == nil

	// 1. User install uses temp bin/config/data roots when flags omitted
	err := Install(Options{
		NativeAgyPath: mockAgy,
		SkipShellRC:   true,
	})
	if err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	installedAgy := filepath.Join(roots.BinDir, "agy")
	if _, err := os.Stat(installedAgy); err != nil {
		t.Errorf("expected shim at %s, got error: %v", installedAgy, err)
	}
	installedCfg := filepath.Join(roots.ConfigDir, "config.json")
	if _, err := os.Stat(installedCfg); err != nil {
		t.Errorf("expected config at %s, got error: %v", installedCfg, err)
	}

	// 2. User uninstall uses the same temp roots
	err = Uninstall(Options{})
	if err != nil {
		t.Fatalf("Uninstall failed: %v", err)
	}

	if _, err := os.Stat(installedAgy); !os.IsNotExist(err) {
		t.Errorf("expected shim %s to be removed by uninstall", installedAgy)
	}

	// Real host user home must be completely untouched
	if realExistedBefore {
		if _, err := os.Lstat(realLocalBinAgyPool); os.IsNotExist(err) {
			t.Fatalf("CRITICAL: real host entrypoint %s was deleted!", realLocalBinAgyPool)
		}
	}
}

func TestInstallerIsolation_SystemModeSyntheticRoot(t *testing.T) {
	tempDir := t.TempDir()
	resetSandbox := config.SetSyntheticSandboxRoot(tempDir)
	defer resetSandbox()

	origTestMode := config.IsTestMode()
	config.SetTestMode(true)
	defer config.SetTestMode(origTestMode)

	sysRoot := filepath.Join(tempDir, "sys_root")
	testHome := filepath.Join(tempDir, "user_home")
	_ = os.MkdirAll(filepath.Join(sysRoot, "usr", "local", "bin"), 0755)
	_ = os.MkdirAll(filepath.Join(sysRoot, "etc", "agy-pool"), 0755)
	_ = os.MkdirAll(filepath.Join(sysRoot, "var", "lib", "agy-pool"), 0755)

	resetRoots := SetTestInstallerRoots(&InstallerRoots{
		UserHome:   testHome,
		BinDir:     filepath.Join(testHome, ".local", "bin"),
		ConfigDir:  filepath.Join(testHome, ".config", "agy-pool"),
		DataDir:    filepath.Join(testHome, ".local", "share", "agy-pool"),
		SystemRoot: sysRoot,
	})
	defer resetRoots()

	mockAgy := filepath.Join(tempDir, "native_agy")
	if err := os.WriteFile(mockAgy, []byte("#!/bin/sh\necho mock native\n"), 0755); err != nil {
		t.Fatalf("failed to write mock native agy: %v", err)
	}

	// 3. System install uses synthetic system root
	err := Install(Options{
		IsSystem:      true,
		NativeAgyPath: mockAgy,
		SkipShellRC:   true,
	})
	if err != nil {
		t.Fatalf("System Install failed: %v", err)
	}

	sysShim := filepath.Join(sysRoot, "usr", "local", "bin", "agy")
	if _, err := os.Stat(sysShim); err != nil {
		t.Errorf("expected system shim at %s, got error: %v", sysShim, err)
	}
	sysCfg := filepath.Join(sysRoot, "etc", "agy-pool", "config.json")
	if _, err := os.Stat(sysCfg); err != nil {
		t.Errorf("expected system config at %s, got error: %v", sysCfg, err)
	}

	// 4. System uninstall uses synthetic system root
	err = Uninstall(Options{
		IsSystem: true,
	})
	if err != nil {
		t.Fatalf("System Uninstall failed: %v", err)
	}

	if _, err := os.Stat(sysShim); !os.IsNotExist(err) {
		t.Errorf("expected system shim %s to be removed by uninstall", sysShim)
	}
}

func TestInstallerIsolation_CleanupNeverResolvesToRealUserHome(t *testing.T) {
	origTestMode := config.IsTestMode()
	config.SetTestMode(true)
	defer config.SetTestMode(origTestMode)

	// 5. Test cleanup never resolves to real user home
	// Even if testInstallerRoots is nil, GetInstallerRoots() in test mode must use a temp sandbox
	resetRoots := SetTestInstallerRoots(nil)
	defer resetRoots()

	roots := GetInstallerRoots()
	realHome := config.GetRealHostUserHome()

	if roots.UserHome == realHome || strings.HasPrefix(roots.UserHome, realHome) {
		t.Fatalf("in test mode, GetInstallerRoots().UserHome resolved to real user home: %s", roots.UserHome)
	}
	if roots.BinDir == filepath.Join(realHome, ".local", "bin") {
		t.Fatalf("in test mode, GetInstallerRoots().BinDir resolved to real user bin: %s", roots.BinDir)
	}
}
