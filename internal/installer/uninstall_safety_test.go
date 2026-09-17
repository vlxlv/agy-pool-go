package installer

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/diagnostics"
)

func TestUninstall_OwnershipEnforcement(t *testing.T) {
	tempDir, cleanup := setupInstallerTestEnv(t)
	defer cleanup()

	targetDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(targetDir, 0755)

	// 1. Target directory contains an unowned 'agy' binary (e.g. real native agy or user script)
	unownedAgy := filepath.Join(targetDir, "agy")
	_ = os.WriteFile(unownedAgy, []byte("#!/bin/sh\necho 'unowned original agy'\n"), 0755)

	// 2. Target directory contains an unowned 'agy-raw' script
	unownedRaw := filepath.Join(targetDir, "agy-raw")
	_ = os.WriteFile(unownedRaw, []byte("#!/bin/sh\necho 'unowned raw script'\n"), 0755)

	// 3. Target directory contains an 'agy-orig' symlink pointing to an unrelated file
	unrelatedFile := filepath.Join(tempDir, "unrelated_target")
	_ = os.WriteFile(unrelatedFile, []byte("important content"), 0644)
	unownedOrig := filepath.Join(targetDir, "agy-orig")
	_ = os.Symlink(unrelatedFile, unownedOrig)

	var buf bytes.Buffer
	opts := Options{
		TargetDir: targetDir,
		HomeDir:   tempDir,
		Stdout:    &buf,
	}

	if err := Uninstall(opts); err != nil {
		t.Fatalf("Uninstall returned error: %v", err)
	}

	// Verify unowned artifacts were NOT deleted
	if _, err := os.Stat(unownedAgy); os.IsNotExist(err) {
		t.Errorf("CRITICAL DEFECT: unowned 'agy' was deleted during uninstall!")
	}
	if _, err := os.Stat(unownedRaw); os.IsNotExist(err) {
		t.Errorf("CRITICAL DEFECT: unowned 'agy-raw' was deleted during uninstall!")
	}
	if _, err := os.Lstat(unownedOrig); os.IsNotExist(err) {
		t.Errorf("CRITICAL DEFECT: unowned 'agy-orig' symlink was deleted during uninstall!")
	}
	if _, err := os.Stat(unrelatedFile); os.IsNotExist(err) {
		t.Errorf("CRITICAL DEFECT: target of symlink was deleted!")
	}

	outStr := buf.String()
	if !strings.Contains(outStr, "missing agy-pool ownership marker") {
		t.Errorf("expected missing ownership marker report, got: %s", outStr)
	}
}

func TestUninstall_TOCTOUHardening_SymlinkSwapped(t *testing.T) {
	tempDir, cleanup := setupInstallerTestEnv(t)
	defer cleanup()

	targetDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(targetDir, 0755)

	// Victim file outside targetDir that must NEVER be unlinked or modified
	victimFile := filepath.Join(tempDir, "native_agy_victim")
	victimContent := []byte("#!/bin/sh\necho 'victim native agy'\n")
	_ = os.WriteFile(victimFile, victimContent, 0755)

	// Attacker sets targetDir/agy to be a symlink pointing to victimFile
	swappedShim := filepath.Join(targetDir, "agy")
	_ = os.Symlink(victimFile, swappedShim)

	var buf bytes.Buffer
	err := SafeRemoveOwnedEntry(targetDir, "agy", &buf)
	if err != nil {
		t.Fatalf("SafeRemoveOwnedEntry failed: %v", err)
	}

	// The victim file must be completely untouched!
	if _, err := os.Stat(victimFile); os.IsNotExist(err) {
		t.Fatalf("SECURITY VIOLATION: victim file was deleted via symlink traversal!")
	}
	curVictim, _ := os.ReadFile(victimFile)
	if !bytes.Equal(curVictim, victimContent) {
		t.Fatalf("SECURITY VIOLATION: victim file was altered!")
	}
}

func TestUninstall_TOCTOUHardening_HardLinkSwapped(t *testing.T) {
	tempDir, cleanup := setupInstallerTestEnv(t)
	defer cleanup()

	targetDir := filepath.Join(tempDir, "bin")
	_ = os.MkdirAll(targetDir, 0755)

	// Victim file outside targetDir
	victimFile := filepath.Join(tempDir, "native_agy_victim")
	victimContent := []byte("#!/bin/sh\necho 'victim native agy'\n")
	_ = os.WriteFile(victimFile, victimContent, 0755)

	// Attacker hard-links targetDir/agy to victimFile
	hardLinkShim := filepath.Join(targetDir, "agy")
	if err := os.Link(victimFile, hardLinkShim); err != nil {
		t.Skipf("hard links not supported on filesystem: %v", err)
	}

	var buf bytes.Buffer
	err := SafeRemoveOwnedEntry(targetDir, "agy", &buf)
	if err != nil {
		t.Fatalf("SafeRemoveOwnedEntry failed: %v", err)
	}

	// Because hardLinkShim lacks the ManagedShimMarker, it must be refused deletion!
	if _, err := os.Stat(hardLinkShim); os.IsNotExist(err) {
		t.Fatalf("CRITICAL DEFECT: hard link to native agy was unlinked!")
	}
	if _, err := os.Stat(victimFile); os.IsNotExist(err) {
		t.Fatalf("CRITICAL DEFECT: victim file was deleted!")
	}
}

func TestNativeAgyUpgradeAndMovement(t *testing.T) {
	tempDir, cleanup := setupInstallerTestEnv(t)
	defer cleanup()

	loc1Dir := filepath.Join(tempDir, "loc1")
	loc2Dir := filepath.Join(tempDir, "loc2")
	_ = os.MkdirAll(loc1Dir, 0755)
	_ = os.MkdirAll(loc2Dir, 0755)

	t.Setenv("HOME", tempDir)
	t.Setenv("PATH", loc1Dir)
	t.Setenv("AGY_BIN", "")
	t.Setenv("PREFIX", "")

	// Scenario A: Native agy replaced with newer executable at same path
	agyPath := filepath.Join(loc1Dir, "agy")
	_ = os.WriteFile(agyPath, []byte("#!/bin/sh\necho '1.0.0'\n"), 0755)

	resolved, _, err := diagnostics.ResolveNativeAgyBinary()
	if err != nil || resolved != agyPath {
		t.Fatalf("initial discovery failed: %v, got %s", err, resolved)
	}

	// Upgrade in place
	_ = os.WriteFile(agyPath, []byte("#!/bin/sh\necho '2.0.0'\n"), 0755)
	resolved2, _, err := diagnostics.ResolveNativeAgyBinary()
	if err != nil || resolved2 != agyPath {
		t.Fatalf("post-upgrade discovery failed: %v, got %s", err, resolved2)
	}

	// Scenario B: Native agy removed from old path and appears at later PATH entry
	_ = os.Remove(agyPath)
	agyPath2 := filepath.Join(loc2Dir, "agy")
	_ = os.WriteFile(agyPath2, []byte("#!/bin/sh\necho '2.1.0'\n"), 0755)
	t.Setenv("PATH", fmt.Sprintf("%s:%s", loc1Dir, loc2Dir))

	resolved3, src, err := diagnostics.ResolveNativeAgyBinary()
	if err != nil || resolved3 != agyPath2 || src != diagnostics.SourcePathEnv {
		t.Fatalf("moved discovery failed: %v, got %s (src: %s)", err, resolved3, src)
	}

	// Scenario C: AGY_BIN points to new executable
	loc3Dir := filepath.Join(tempDir, "loc3")
	_ = os.MkdirAll(loc3Dir, 0755)
	agyPath3 := filepath.Join(loc3Dir, "custom-agy")
	_ = os.WriteFile(agyPath3, []byte("#!/bin/sh\necho '3.0.0'\n"), 0755)
	t.Setenv("AGY_BIN", agyPath3)

	resolved4, src, err := diagnostics.ResolveNativeAgyBinary()
	if err != nil || resolved4 != agyPath3 || src != diagnostics.SourceEnvAgyBin {
		t.Fatalf("AGY_BIN override failed: %v, got %s (src: %s)", err, resolved4, src)
	}
}

func TestTokenPath_NoFictitiousPathRegression(t *testing.T) {
	tempDir := t.TempDir()
	cleanup := config.SetSyntheticSandboxRoot(tempDir)
	defer cleanup()

	// 1. Verify that the compatibility token file path derives strictly from antigravity-oauth-token
	tokenPath := config.GetAgyTokenFile()
	if !strings.HasSuffix(tokenPath, "antigravity-oauth-token") {
		t.Fatalf("expected token path to end with antigravity-oauth-token, got %s", tokenPath)
	}
	prohibitedName := "agy_" + "token_compat"
	if strings.Contains(tokenPath, prohibitedName) {
		t.Fatalf("fictitious path found in GetAgyTokenFile: %s", tokenPath)
	}

	// 2. Scan Go source tree to ensure no reference to prohibitedName exists anywhere
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}
	repoRoot := filepath.Clean(filepath.Join(wd, "..", ".."))

	err = filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "bin" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "uninstall_safety_test.go") {
			return nil
		}
		ext := filepath.Ext(path)
		if ext == ".go" || ext == ".sh" || ext == ".py" {
			data, err := os.ReadFile(path)
			if err == nil && strings.Contains(string(data), prohibitedName) {
				return fmt.Errorf("file %s contains prohibited fictitious token path %q", path, prohibitedName)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("regression check failed: %v", err)
	}
}

func TestInstall_NoGoStateInGemini(t *testing.T) {
	tempDir, cleanup := setupInstallerTestEnv(t)
	defer cleanup()

	fakeHome := filepath.Join(tempDir, "home_clean")
	_ = os.MkdirAll(fakeHome, 0700)

	targetDir := filepath.Join(fakeHome, ".local", "bin")
	fakeNativeAgy := filepath.Join(tempDir, "fake_native_agy")
	_ = os.WriteFile(fakeNativeAgy, []byte("#!/bin/sh\necho native\n"), 0755)

	opts := Options{
		HomeDir:       fakeHome,
		TargetDir:     targetDir,
		NativeAgyPath: fakeNativeAgy,
	}

	if err := Install(opts); err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	// Verify that under fakeHome/.gemini no state.db, *.pid, *.log, *.lock was created
	fakeGemini := filepath.Join(fakeHome, ".gemini")
	if fi, err := os.Stat(fakeGemini); err == nil && fi.IsDir() {
		entries, _ := os.ReadDir(fakeGemini)
		for _, e := range entries {
			name := e.Name()
			if strings.HasSuffix(name, ".db") ||
				strings.HasSuffix(name, ".pid") ||
				strings.HasSuffix(name, ".log") ||
				strings.HasSuffix(name, ".lock") ||
				name == "config.json" {
				t.Fatalf("PROHIBITED STATE in ~/.gemini: found %s", name)
			}
		}
	}
}
