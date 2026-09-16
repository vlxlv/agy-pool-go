package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDestructiveGuard_ProtectsRealHostPathsInTestMode(t *testing.T) {
	origMode := IsTestMode()
	SetTestMode(true)
	defer SetTestMode(origMode)

	realHome := GetRealHostUserHome()
	if realHome == "" {
		t.Fatalf("real host user home is empty")
	}

	// 6. Real ~/.local/bin/agy-pool is rejected as a destructive target in test mode
	realLocalBinAgyPool := filepath.Join(realHome, ".local", "bin", "agy-pool")
	if err := AssertSafeDestructivePath(realLocalBinAgyPool); err == nil {
		t.Errorf("expected error protecting real local bin agy-pool %s, got nil", realLocalBinAgyPool)
	}

	realGeminiDir := filepath.Join(realHome, ".gemini")
	if err := AssertSafeDestructivePath(realGeminiDir); err == nil {
		t.Errorf("expected error protecting real ~/.gemini, got nil")
	}

	realBashrc := filepath.Join(realHome, ".bashrc")
	if err := AssertSafeDestructivePath(realBashrc); err == nil {
		t.Errorf("expected error protecting real ~/.bashrc, got nil")
	}

	// System paths
	for _, sysPath := range []string{"/usr/local/bin", "/usr/bin", "/etc/agy-pool", "/var/lib/agy-pool"} {
		if err := AssertSafeDestructivePath(sysPath); err == nil {
			t.Errorf("expected error protecting system path %s, got nil", sysPath)
		}
	}
}

func TestDestructiveGuard_SymlinkResolution(t *testing.T) {
	origMode := IsTestMode()
	SetTestMode(true)
	defer SetTestMode(origMode)

	tempDir := t.TempDir()
	resetSandbox := SetSyntheticSandboxRoot(tempDir)
	defer resetSandbox()

	realHome := GetRealHostUserHome()
	realLocalBin := filepath.Join(realHome, ".local", "bin")

	// 7. Symlink into real ~/.local/bin is rejected even if created inside tempDir
	symlinkToRealLocalBin := filepath.Join(tempDir, "fake_local_bin")
	if err := os.Symlink(realLocalBin, symlinkToRealLocalBin); err != nil {
		t.Fatalf("failed to create test symlink: %v", err)
	}

	targetInsideSymlink := filepath.Join(symlinkToRealLocalBin, "agy-pool")
	if err := AssertSafeDestructivePath(targetInsideSymlink); err == nil {
		t.Errorf("expected error for path traversing symlink to real ~/.local/bin: %s, got nil", targetInsideSymlink)
	}

	// Symlink pointing directly to real ~/.local/bin/agy-pool
	symlinkToFile := filepath.Join(tempDir, "symlink_to_entrypoint")
	_ = os.Symlink(filepath.Join(realLocalBin, "agy-pool"), symlinkToFile)
	if err := AssertSafeDestructivePath(symlinkToFile); err == nil {
		t.Errorf("expected error for symlink pointing to real entrypoint, got nil")
	}
}

func TestDestructiveGuard_HomeOverrideCannotRedirectSafetyClassification(t *testing.T) {
	origMode := IsTestMode()
	SetTestMode(true)
	defer SetTestMode(origMode)

	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)

	// 8. HOME override cannot redirect safety classification
	tempDir := t.TempDir()
	os.Setenv("HOME", tempDir)

	realHome := GetRealHostUserHome()
	realLocalBin := filepath.Join(realHome, ".local", "bin", "agy-pool")

	if err := AssertSafeDestructivePath(realLocalBin); err == nil {
		t.Errorf("expected AssertSafeDestructivePath to protect real home %s even when HOME=%s", realLocalBin, tempDir)
	}
}

func TestDestructiveGuard_RealHomeUsedOnlyForProtection(t *testing.T) {
	origMode := IsTestMode()
	SetTestMode(true)
	defer SetTestMode(origMode)

	realHome := GetRealHostUserHome()
	if realHome == "" {
		t.Fatalf("GetRealHostUserHome() returned empty string")
	}

	// 9. pwd/getuid real home is used only for protection, not test destination
	// Synthetic sandbox path should be permitted
	tempDir := t.TempDir()
	resetSandbox := SetSyntheticSandboxRoot(tempDir)
	defer resetSandbox()

	safePath := filepath.Join(tempDir, "safe", "path")
	if err := AssertSafeDestructivePath(safePath); err != nil {
		t.Errorf("expected safe sandbox path to pass, got err: %v", err)
	}
}
