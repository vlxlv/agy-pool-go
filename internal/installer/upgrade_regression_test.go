package installer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/diagnostics"
)

func fileHash(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s for hashing: %v", path, err)
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// TestUpgrade_ModelA_InPlaceManagedNativeSelfUpdate tests that when native agy
// updates its own binary in-place at ~/.local/libexec/agy-pool/agy-native,
// the managed shim remains untouched and integration continues seamlessly.
func TestUpgrade_ModelA_InPlaceManagedNativeSelfUpdate(t *testing.T) {
	tempDir, cleanup := setupInstallerTestEnv(t)
	defer cleanup()

	userHome := filepath.Join(tempDir, "user_home_a")
	targetBin := filepath.Join(userHome, ".local", "bin")
	libexecDir := filepath.Join(userHome, ".local", "libexec", "agy-pool")
	_ = os.MkdirAll(targetBin, 0755)

	t.Setenv("HOME", userHome)
	t.Setenv("PATH", targetBin)

	// Step 1: Initial install with v1 native binary
	v1Content := "#!/bin/sh\necho '1.2.3'\n"
	initialAgy := filepath.Join(targetBin, "agy")
	if err := os.WriteFile(initialAgy, []byte(v1Content), 0755); err != nil {
		t.Fatalf("failed to write initial agy: %v", err)
	}

	opts := Options{
		TargetDir: targetBin,
		HomeDir:   userHome,
	}
	if err := Install(opts); err != nil {
		t.Fatalf("initial install failed: %v", err)
	}

	managedNative := filepath.Join(libexecDir, "agy-native")
	shimPath := filepath.Join(targetBin, "agy")

	initialShimHash := fileHash(t, shimPath)

	// Step 2: In-place self-update of managed native binary to v2
	v2Content := "#!/bin/sh\necho '2.0.0'\n"
	if err := os.WriteFile(managedNative, []byte(v2Content), 0755); err != nil {
		t.Fatalf("failed to update managed native binary: %v", err)
	}

	// Managed shim must remain byte-for-byte untouched
	afterShimHash := fileHash(t, shimPath)
	if initialShimHash != afterShimHash {
		t.Errorf("managed shim was modified during in-place native self-update!")
	}

	// Resolver resolves managed native with v2
	resolved, src, err := diagnostics.ResolveNativeAgyBinary()
	if err != nil {
		t.Fatalf("failed to resolve native agy: %v", err)
	}
	if resolved != managedNative || src != diagnostics.SourceManaged {
		t.Errorf("expected resolver to return managed native %q (%q), got %q (%q)",
			managedNative, diagnostics.SourceManaged, resolved, src)
	}

	data, _ := os.ReadFile(resolved)
	if string(data) != v2Content {
		t.Errorf("resolved binary content mismatch: got %q, expected %q", string(data), v2Content)
	}

	// Doctor reports healthy layout without needing repair
	var buf bytes.Buffer
	diagnostics.SetAgyEntrypointFinder(func() string { return shimPath })
	defer diagnostics.SetAgyEntrypointFinder(nil)
	diagnostics.SetTLSProber(func(host string, timeout time.Duration) error { return nil })
	defer diagnostics.SetTLSProber(nil)

	ok := diagnostics.RunDoctor(&buf)
	if !ok {
		t.Fatalf("expected doctor to succeed on in-place updated native agy, got false. Output:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "managed shim ->") {
		t.Errorf("expected healthy managed shim message, got:\n%s", buf.String())
	}
}

// TestUpgrade_ModelB_OfficialUpdaterOverwritesShimAndRepairFlow tests that when an official
// updater overwrites ~/.local/bin/agy with a new native ELF, doctor flags it, repair preserves
// the new native ELF before restoring the shim, config/state remain unchanged, and repeated
// upgrades work recoverably.
func TestUpgrade_ModelB_OfficialUpdaterOverwritesShimAndRepairFlow(t *testing.T) {
	tempDir, cleanup := setupInstallerTestEnv(t)
	defer cleanup()

	userHome := filepath.Join(tempDir, "user_home_b")
	targetBin := filepath.Join(userHome, ".local", "bin")
	libexecDir := filepath.Join(userHome, ".local", "libexec", "agy-pool")
	cfgDir := filepath.Join(userHome, ".config", "agy-pool")
	dataDir := filepath.Join(userHome, ".local", "share", "agy-pool")

	_ = os.MkdirAll(targetBin, 0755)
	_ = os.MkdirAll(cfgDir, 0755)
	_ = os.MkdirAll(dataDir, 0755)

	t.Setenv("HOME", userHome)
	t.Setenv("PATH", targetBin)

	// Step 1: Initial install with v1 native binary
	v1Content := "#!/bin/sh\necho 'native agy v1.2.3'\n"
	targetAgy := filepath.Join(targetBin, "agy")
	_ = os.WriteFile(targetAgy, []byte(v1Content), 0755)

	// Pre-create unique custom config and synthetic state database
	customConfig := filepath.Join(cfgDir, "config.json")
	customConfigContent := `{"version":1,"server":{"listen":"127.0.0.1","port":9444},"scheduler":{"strategy":"max_quota"}}`
	_ = os.WriteFile(customConfig, []byte(customConfigContent), 0600)

	stateDB := filepath.Join(dataDir, "state.db")
	_ = os.WriteFile(stateDB, []byte("SQLite format 3\x00--dummy-state-db-data--"), 0600)

	opts := Options{
		TargetDir: targetBin,
		HomeDir:   userHome,
		ConfigDir: cfgDir,
		DataDir:   dataDir,
	}
	if err := Install(opts); err != nil {
		t.Fatalf("initial install failed: %v", err)
	}

	managedNative := filepath.Join(libexecDir, "agy-native")
	configHashBefore := fileHash(t, customConfig)
	stateDBHashBefore := fileHash(t, stateDB)

	// Step 2: Official updater overwrites ~/.local/bin/agy with new native ELF v2
	v2Content := "#!/bin/sh\necho 'official native agy v2.0.0 by Google'\n"
	_ = os.WriteFile(targetAgy, []byte(v2Content), 0755)

	// Step 3: agy-pool doctor detects and reports broken bypass state
	diagnostics.SetAgyEntrypointFinder(func() string { return targetAgy })
	defer diagnostics.SetAgyEntrypointFinder(nil)

	var docBuf bytes.Buffer
	doctorHealthy := diagnostics.RunDoctor(&docBuf)
	if doctorHealthy {
		t.Fatalf("expected doctor to report error when agy is a native binary bypassing agy-pool")
	}
	docOutput := docBuf.String()
	if !strings.Contains(docOutput, "is a native binary bypassing agy-pool") {
		t.Errorf("expected native binary bypass warning, got:\n%s", docOutput)
	}
	if !strings.Contains(docOutput, "agy-pool repair") {
		t.Errorf("expected doctor to suggest 'agy-pool repair', got:\n%s", docOutput)
	}

	// Step 4: Run Repair
	if err := Repair(opts); err != nil {
		t.Fatalf("Repair failed: %v", err)
	}

	// 1) Verify managed native now contains v2 content
	preservedV2, err := os.ReadFile(managedNative)
	if err != nil {
		t.Fatalf("failed to read managed native after repair: %v", err)
	}
	if string(preservedV2) != v2Content {
		t.Fatalf("expected managed native to be updated to v2.\nGot: %q\nExpected: %q",
			string(preservedV2), v2Content)
	}

	// 2) Verify targetDir/agy is restored to managed shim
	restoredShim, err := os.ReadFile(targetAgy)
	if err != nil {
		t.Fatalf("failed to read restored shim: %v", err)
	}
	if !strings.Contains(string(restoredShim), ManagedShimMarker) {
		t.Errorf("restored agy is not a managed shim: %s", string(restoredShim))
	}
	if !strings.Contains(string(restoredShim), "agy-pool run") {
		t.Errorf("restored agy does not invoke agy-pool run: %s", string(restoredShim))
	}

	// 3) Verify resolver now resolves to managed native v2
	resolvedAgy, src, err := diagnostics.ResolveNativeAgyBinary()
	if err != nil {
		t.Fatalf("failed to resolve native agy after repair: %v", err)
	}
	if resolvedAgy != managedNative || src != diagnostics.SourceManaged {
		t.Errorf("expected resolver to find managed native %q (%q), got %q (%q)",
			managedNative, diagnostics.SourceManaged, resolvedAgy, src)
	}

	// 4) Verify config.json and state.db remain completely unchanged
	configHashAfter := fileHash(t, customConfig)
	stateDBHashAfter := fileHash(t, stateDB)
	if configHashBefore != configHashAfter {
		t.Errorf("config.json was modified during repair!")
	}
	if stateDBHashBefore != stateDBHashAfter {
		t.Errorf("state.db was modified during repair!")
	}

	// 5) Verify doctor reports healthy after repair
	docBuf.Reset()
	if ok := diagnostics.RunDoctor(&docBuf); !ok {
		t.Fatalf("expected doctor to report healthy after repair, got false. Output:\n%s", docBuf.String())
	}

	// 6) Verify repair is idempotent
	if err := Repair(opts); err != nil {
		t.Fatalf("subsequent idempotent Repair failed: %v", err)
	}
	contentAfterIdempotent, _ := os.ReadFile(managedNative)
	if string(contentAfterIdempotent) != v2Content {
		t.Fatalf("idempotent repair altered managed native content")
	}

	// Step 5: Repeated official upgrade: updater overwrites with native ELF v3
	v3Content := "#!/bin/sh\necho 'official native agy v3.1.0 by Google'\n"
	_ = os.WriteFile(targetAgy, []byte(v3Content), 0755)

	docBuf.Reset()
	if ok := diagnostics.RunDoctor(&docBuf); ok {
		t.Fatalf("expected doctor to detect second official upgrade bypass")
	}

	if err := Repair(opts); err != nil {
		t.Fatalf("second Repair failed: %v", err)
	}

	preservedV3, _ := os.ReadFile(managedNative)
	if string(preservedV3) != v3Content {
		t.Fatalf("expected managed native to be updated to v3.\nGot: %q\nExpected: %q",
			string(preservedV3), v3Content)
	}

	shimV3, _ := os.ReadFile(targetAgy)
	if !strings.Contains(string(shimV3), ManagedShimMarker) {
		t.Fatalf("target agy was not restored to managed shim after second repair")
	}

	docBuf.Reset()
	if ok := diagnostics.RunDoctor(&docBuf); !ok {
		t.Fatalf("expected doctor to report healthy after second repair, got false. Output:\n%s", docBuf.String())
	}
}
