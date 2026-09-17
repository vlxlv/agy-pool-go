package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func TestCanonicalizeDir(t *testing.T) {
	tmpDir := t.TempDir()
	cleanup := config.SetSyntheticSandboxRoot(tmpDir)
	defer cleanup()

	realDir := filepath.Join(tmpDir, "real-state")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatalf("failed to create realDir: %v", err)
	}

	symlinkDir := filepath.Join(tmpDir, "link-state")
	if err := os.Symlink(realDir, symlinkDir); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	canReal, err := CanonicalizeDir(realDir)
	if err != nil {
		t.Fatalf("CanonicalizeDir(realDir) failed: %v", err)
	}

	canLink, err := CanonicalizeDir(symlinkDir)
	if err != nil {
		t.Fatalf("CanonicalizeDir(symlinkDir) failed: %v", err)
	}

	if canLink != canReal {
		t.Fatalf("expected symlink canonical dir %q to match real canonical dir %q", canLink, canReal)
	}

	// Test non-existent subdirectory inside symlinked directory
	subLink := filepath.Join(symlinkDir, "sub", "child")
	canSubLink, err := CanonicalizeDir(subLink)
	if err != nil {
		t.Fatalf("CanonicalizeDir(subLink) failed: %v", err)
	}
	expectedSub := filepath.Join(canReal, "sub", "child")
	if canSubLink != expectedSub {
		t.Fatalf("expected %q, got %q", expectedSub, canSubLink)
	}
}

func TestResolveInstanceContext(t *testing.T) {
	tmpDir := t.TempDir()
	cleanup := config.SetSyntheticSandboxRoot(tmpDir)
	defer cleanup()

	dataDir := filepath.Join(tmpDir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("failed to create data dir: %v", err)
	}

	cfgPath := filepath.Join(tmpDir, "config.json")
	cfgContent := `{"version": 1, "server": {"port": 9199, "listen": "127.0.0.1"}}`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	ctx, err := ResolveInstanceContext(dataDir, cfgPath, 0, "")
	if err != nil {
		t.Fatalf("ResolveInstanceContext failed: %v", err)
	}

	canDataDir, _ := CanonicalizeDir(dataDir)
	if ctx.DataDir != canDataDir {
		t.Fatalf("expected DataDir %q, got %q", canDataDir, ctx.DataDir)
	}
	canCfgPath, _ := CanonicalizeFile(cfgPath)
	if ctx.ConfigPath != canCfgPath {
		t.Fatalf("expected ConfigPath %q, got %q", canCfgPath, ctx.ConfigPath)
	}
	if ctx.ConfigHash == "" {
		t.Fatal("expected non-empty ConfigHash")
	}
	if ctx.ListenPort != 9199 {
		t.Fatalf("expected ListenPort 9199, got %d", ctx.ListenPort)
	}
	if ctx.ListenHost != "127.0.0.1" {
		t.Fatalf("expected ListenHost 127.0.0.1, got %q", ctx.ListenHost)
	}
	if ctx.PIDFile() != filepath.Join(canDataDir, "agy-pool.pid") {
		t.Fatalf("unexpected PIDFile: %s", ctx.PIDFile())
	}
	if ctx.LogFile() != filepath.Join(canDataDir, "agy-pool.log") {
		t.Fatalf("unexpected LogFile: %s", ctx.LogFile())
	}
	if ctx.StateDB() != filepath.Join(canDataDir, "state.db") {
		t.Fatalf("unexpected StateDB: %s", ctx.StateDB())
	}
}

func TestMatchesInstance(t *testing.T) {
	tmpDir := t.TempDir()
	cleanup := config.SetSyntheticSandboxRoot(tmpDir)
	defer cleanup()

	dataDirA := filepath.Join(tmpDir, "data-a")
	dataDirB := filepath.Join(tmpDir, "data-b")
	_ = os.MkdirAll(dataDirA, 0o700)
	_ = os.MkdirAll(dataDirB, 0o700)

	cfgA := filepath.Join(tmpDir, "cfg-a.json")
	cfgB := filepath.Join(tmpDir, "cfg-b.json")
	_ = os.WriteFile(cfgA, []byte(`{"version": 1, "server":{"port":9101}}`), 0o600)
	_ = os.WriteFile(cfgB, []byte(`{"version": 1, "server":{"port":9102}}`), 0o600)

	ctxA, err := ResolveInstanceContext(dataDirA, cfgA, 9101, "127.0.0.1")
	if err != nil {
		t.Fatalf("failed to resolve ctxA: %v", err)
	}

	currPID := os.Getpid()

	// 1. Exact match
	infoExact := &DaemonInfo{
		PID:        currPID,
		DataDir:    ctxA.DataDir,
		ConfigPath: ctxA.ConfigPath,
		ConfigHash: ctxA.ConfigHash,
		Port:       ctxA.ListenPort,
		ListenHost: ctxA.ListenHost,
		Version:    ctxA.Version,
	}
	if res := MatchesInstance(infoExact, ctxA); res != MatchExact {
		t.Fatalf("expected MatchExact, got %v", res)
	}

	// 2. Foreign data dir
	infoForeignDir := &DaemonInfo{
		PID:        currPID,
		DataDir:    dataDirB,
		ConfigPath: ctxA.ConfigPath,
		ConfigHash: ctxA.ConfigHash,
		Port:       ctxA.ListenPort,
		ListenHost: ctxA.ListenHost,
		Version:    ctxA.Version,
	}
	if res := MatchesInstance(infoForeignDir, ctxA); res != MatchForeign {
		t.Fatalf("expected MatchForeign for different data dir, got %v", res)
	}

	// 3. Port mismatch
	infoPortMismatch := &DaemonInfo{
		PID:        currPID,
		DataDir:    ctxA.DataDir,
		ConfigPath: ctxA.ConfigPath,
		ConfigHash: ctxA.ConfigHash,
		Port:       9102,
		ListenHost: ctxA.ListenHost,
		Version:    ctxA.Version,
	}
	if res := MatchesInstance(infoPortMismatch, ctxA); res != MatchPortMismatch {
		t.Fatalf("expected MatchPortMismatch, got %v", res)
	}

	// 4. Config path mismatch
	canCfgB, _ := CanonicalizeFile(cfgB)
	infoCfgPathMismatch := &DaemonInfo{
		PID:        currPID,
		DataDir:    ctxA.DataDir,
		ConfigPath: canCfgB,
		ConfigHash: ctxA.ConfigHash,
		Port:       ctxA.ListenPort,
		ListenHost: ctxA.ListenHost,
		Version:    ctxA.Version,
	}
	if res := MatchesInstance(infoCfgPathMismatch, ctxA); res != MatchConfigMismatch {
		t.Fatalf("expected MatchConfigMismatch, got %v", res)
	}

	// 5. Config hash mismatch (modified in place)
	infoHashMismatch := &DaemonInfo{
		PID:        currPID,
		DataDir:    ctxA.DataDir,
		ConfigPath: ctxA.ConfigPath,
		ConfigHash: "different-hash",
		Port:       ctxA.ListenPort,
		ListenHost: ctxA.ListenHost,
		Version:    ctxA.Version,
	}
	if res := MatchesInstance(infoHashMismatch, ctxA); res != MatchConfigMismatch {
		t.Fatalf("expected MatchConfigMismatch for modified hash, got %v", res)
	}

	// 6. Dead PID
	infoDead := &DaemonInfo{
		PID:        999999999, // unlikely to exist
		DataDir:    ctxA.DataDir,
		ConfigPath: ctxA.ConfigPath,
		ConfigHash: ctxA.ConfigHash,
		Port:       ctxA.ListenPort,
		ListenHost: ctxA.ListenHost,
		Version:    ctxA.Version,
	}
	if res := MatchesInstance(infoDead, ctxA); res != MatchStale {
		t.Fatalf("expected MatchStale for dead PID, got %v", res)
	}
}

func TestCheckInstanceStatus(t *testing.T) {
	tmpDir := t.TempDir()
	cleanup := config.SetSyntheticSandboxRoot(tmpDir)
	defer cleanup()

	dataDir := filepath.Join(tmpDir, "data")
	_ = os.MkdirAll(dataDir, 0o700)

	cfgPath := filepath.Join(tmpDir, "config.json")
	_ = os.WriteFile(cfgPath, []byte(`{"version": 1, "server":{"port":9201}}`), 0o600)

	ctx, err := ResolveInstanceContext(dataDir, cfgPath, 9201, "127.0.0.1")
	if err != nil {
		t.Fatalf("ResolveInstanceContext failed: %v", err)
	}

	// 1. Initial status: stopped (no PID file, port not listening)
	status, info, _ := CheckInstanceStatus(ctx)
	if status != StatusStopped || info != nil {
		t.Fatalf("expected StatusStopped, got %v (info: %v)", status, info)
	}

	// 2. Stale PID file (PID dead)
	staleInfo := DaemonInfo{
		PID:     999999999,
		DataDir: ctx.DataDir,
		Port:    9201,
	}
	_ = storage.AtomicJSONWrite(ctx.PIDFile(), staleInfo)

	status, _, _ = CheckInstanceStatus(ctx)
	if status != StatusStalePID {
		t.Fatalf("expected StatusStalePID, got %v", status)
	}

	// 3. Foreign DataDir in PID file (tampered)
	foreignInfo := DaemonInfo{
		PID:     os.Getpid(),
		DataDir: filepath.Join(tmpDir, "foreign-data"),
		Port:    9201,
	}
	_ = storage.AtomicJSONWrite(ctx.PIDFile(), foreignInfo)

	status, _, _ = CheckInstanceStatus(ctx)
	if status != StatusForeignInstance {
		t.Fatalf("expected StatusForeignInstance, got %v", status)
	}

	// 4. Config mismatch (hash changed)
	modifiedHashInfo := DaemonInfo{
		PID:        os.Getpid(),
		DataDir:    ctx.DataDir,
		ConfigPath: ctx.ConfigPath,
		ConfigHash: "old-sha256-hash",
		Port:       9201,
	}
	_ = storage.AtomicJSONWrite(ctx.PIDFile(), modifiedHashInfo)

	status, _, _ = CheckInstanceStatus(ctx)
	if status != StatusConfigMismatch {
		t.Fatalf("expected StatusConfigMismatch, got %v", status)
	}
}
