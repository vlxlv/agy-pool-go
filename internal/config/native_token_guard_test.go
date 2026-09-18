package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A. Production-mode canonical token write succeeds
func TestAssertSafeNativeTokenWrite_ProductionMode_CanonicalSucceeds(t *testing.T) {
	origTestMode := IsTestMode()
	SetTestMode(false)
	defer SetTestMode(origTestMode)

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	canonicalToken := GetAgyTokenFile()
	expectedPrefix := filepath.Join(fakeHome, ".gemini", "antigravity-cli")
	if !strings.HasPrefix(canonicalToken, expectedPrefix) {
		t.Fatalf("expected canonical token file under %s, got %s", expectedPrefix, canonicalToken)
	}

	// 1. Canonical token file itself
	if err := AssertSafeNativeTokenWrite(canonicalToken); err != nil {
		t.Fatalf("expected canonical token file to be allowed in production mode: %v", err)
	}

	// 2. Canonical lock file
	lockFile := canonicalToken + ".lock"
	if err := AssertSafeNativeTokenWrite(lockFile); err != nil {
		t.Fatalf("expected canonical lock file to be allowed in production mode: %v", err)
	}

	// 3. Canonical atomic staging file
	stagingFile := filepath.Join(filepath.Dir(canonicalToken), "antigravity-oauth-token.tmp12345")
	if err := AssertSafeNativeTokenWrite(stagingFile); err != nil {
		t.Fatalf("expected canonical staging file to be allowed in production mode: %v", err)
	}
}

// B. Test-mode real-production-like path rejected
func TestAssertSafeNativeTokenWrite_TestMode_RealProductionRejected(t *testing.T) {
	origTestMode := IsTestMode()
	SetTestMode(true)
	defer SetTestMode(origTestMode)

	// Configure synthetic sandbox in a dedicated isolated directory
	sandboxDir := t.TempDir()
	resetSandbox := SetSyntheticSandboxRoot(sandboxDir)
	defer resetSandbox()

	// Configure a simulated real production home (NEVER actual /home/codex)
	fakeRealHome := t.TempDir()
	fakeRealGemini := filepath.Join(fakeRealHome, ".gemini")
	resetRealProd := SetRealProductionGeminiDir(fakeRealGemini)
	defer resetRealProd()

	targetRealToken := filepath.Join(fakeRealGemini, "antigravity-cli", "antigravity-oauth-token")

	err := AssertSafeNativeTokenWrite(targetRealToken)
	if err == nil {
		t.Fatalf("expected write to real-production-like path %s to fail closed in test mode", targetRealToken)
	}
	if !strings.Contains(err.Error(), "refusing write to protected production path in test mode") &&
		!strings.Contains(err.Error(), "protected production path") {
		t.Fatalf("expected fail-closed error message, got: %v", err)
	}
}

// C. Test-mode synthetic token path succeeds
func TestAssertSafeNativeTokenWrite_TestMode_SyntheticTokenSucceeds(t *testing.T) {
	origTestMode := IsTestMode()
	SetTestMode(true)
	defer SetTestMode(origTestMode)

	tempDir := t.TempDir()
	resetSandbox := SetSyntheticSandboxRoot(tempDir)
	defer resetSandbox()

	syntheticGemini := filepath.Join(tempDir, "synthetic_gemini")
	SetNativeAgyDir(syntheticGemini)
	defer SetNativeAgyDir("")

	canonicalToken := GetAgyTokenFile()
	if !strings.HasPrefix(canonicalToken, tempDir) {
		t.Fatalf("expected canonical token under %s, got %s", tempDir, canonicalToken)
	}

	// 1. Canonical token file in sandbox
	if err := AssertSafeNativeTokenWrite(canonicalToken); err != nil {
		t.Fatalf("expected synthetic token path in sandbox to succeed in test mode: %v", err)
	}

	// 2. Lock file in sandbox
	lockFile := canonicalToken + ".lock"
	if err := AssertSafeNativeTokenWrite(lockFile); err != nil {
		t.Fatalf("expected synthetic lock path in sandbox to succeed in test mode: %v", err)
	}

	// 3. Staging file in sandbox
	stagingFile := filepath.Join(filepath.Dir(canonicalToken), "antigravity-oauth-token.12345")
	if err := AssertSafeNativeTokenWrite(stagingFile); err != nil {
		t.Fatalf("expected synthetic staging path in sandbox to succeed in test mode: %v", err)
	}
}

// D. Production-mode arbitrary path rejected
func TestAssertSafeNativeTokenWrite_ProductionMode_ArbitraryPathsRejected(t *testing.T) {
	origTestMode := IsTestMode()
	SetTestMode(false)
	defer SetTestMode(origTestMode)

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	canonicalDir := filepath.Dir(GetAgyTokenFile())

	arbitraryPaths := []string{
		"/tmp/random-token",
		"/tmp/antigravity-oauth-token",
		filepath.Join(canonicalDir, "other_token.json"),
		filepath.Join(canonicalDir, "conversation_summaries.db"),
		filepath.Join(canonicalDir, "arbitrary_file.txt"),
		filepath.Join(fakeHome, ".bashrc"),
		filepath.Join(fakeHome, "notes.txt"),
		"",
	}

	for _, p := range arbitraryPaths {
		err := AssertSafeNativeTokenWrite(p)
		if err == nil {
			t.Errorf("expected arbitrary path %q to be rejected in production mode", p)
		}
	}
}

// E. Symlink/path traversal protection
func TestAssertSafeNativeTokenWrite_SymlinkAndTraversalProtection(t *testing.T) {
	origTestMode := IsTestMode()
	SetTestMode(false)
	defer SetTestMode(origTestMode)

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	canonicalToken := GetAgyTokenFile()
	canonicalDir := filepath.Dir(canonicalToken)
	if err := os.MkdirAll(canonicalDir, 0700); err != nil {
		t.Fatal(err)
	}

	// 1. Path traversal attempting to escape canonical directory
	traversalPath := filepath.Join(canonicalDir, "..", "other-token")
	if err := AssertSafeNativeTokenWrite(traversalPath); err == nil {
		t.Errorf("expected traversal path %q to be rejected", traversalPath)
	}

	// 2. Symlink attempting to redirect canonical token to sensitive file outside canonical dir
	sensitiveTarget := filepath.Join(fakeHome, "sensitive_file.txt")
	_ = os.WriteFile(sensitiveTarget, []byte("secret"), 0600)

	symlinkPath := filepath.Join(canonicalDir, "antigravity-oauth-token")
	if err := os.Symlink(sensitiveTarget, symlinkPath); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(symlinkPath)

	if err := AssertSafeNativeTokenWrite(symlinkPath); err == nil {
		t.Errorf("expected symlink redirecting out of canonical dir to %q to be rejected", sensitiveTarget)
	}

	// 3. Symlink to system directory
	sysSymlink := filepath.Join(canonicalDir, "antigravity-oauth-token.system")
	_ = os.Symlink("/etc/passwd", sysSymlink)
	defer os.Remove(sysSymlink)

	if err := AssertSafeNativeTokenWrite(sysSymlink); err == nil {
		t.Errorf("expected symlink to system directory to be rejected")
	}
}
