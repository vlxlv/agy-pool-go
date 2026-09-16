package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/config"
)

func TestPIDFile_WriteAndReadRoundtrip(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "test.pid")

	info := DaemonInfo{
		PID:         os.Getpid(),
		Version:     "0.1.0-test",
		ScriptMtime: 1234567890,
	}

	if err := WritePIDFile(pidFile, info); err != nil {
		t.Fatalf("WritePIDFile failed: %v", err)
	}

	// Verify file permissions are 0600
	st, err := os.Stat(pidFile)
	if err != nil {
		t.Fatalf("os.Stat failed: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected permissions 0600, got %o", perm)
	}

	readInfo, err := ReadPIDFile(pidFile)
	if err != nil {
		t.Fatalf("ReadPIDFile failed: %v", err)
	}
	if readInfo.PID != info.PID || readInfo.Version != info.Version || readInfo.ScriptMtime != info.ScriptMtime {
		t.Fatalf("roundtrip mismatch: got %+v, want %+v", readInfo, info)
	}
}

func TestPIDFile_LegacyFormat(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "legacy.pid")

	if err := os.WriteFile(pidFile, []byte("98765\n"), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	info, err := ReadPIDFile(pidFile)
	if err != nil {
		t.Fatalf("ReadPIDFile failed: %v", err)
	}
	if info.PID != 98765 {
		t.Fatalf("expected PID 98765, got %d", info.PID)
	}
}

func TestPIDFile_MalformedAndMissing(t *testing.T) {
	tmpDir := t.TempDir()
	missingFile := filepath.Join(tmpDir, "missing.pid")

	_, err := ReadPIDFile(missingFile)
	if !errors.Is(err, ErrPIDNotFound) {
		t.Fatalf("expected ErrPIDNotFound, got %v", err)
	}

	malformedFile := filepath.Join(tmpDir, "malformed.pid")
	_ = os.WriteFile(malformedFile, []byte("{not json"), 0o600)
	_, err = ReadPIDFile(malformedFile)
	if !errors.Is(err, ErrPIDMalformed) {
		t.Fatalf("expected ErrPIDMalformed, got %v", err)
	}
}

func TestPIDFile_OwnershipSafeRemoval(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "ownership.pid")

	// Write PID file owned by foreign PID 999999
	_ = WritePIDFile(pidFile, DaemonInfo{PID: 999999, Version: "test"})

	// Attempt to remove with our PID should fail closed with ErrPIDNotOwned
	removed, err := RemovePIDFileIfOwned(pidFile, os.Getpid())
	if removed || !errors.Is(err, ErrPIDNotOwned) {
		t.Fatalf("expected RemovePIDFileIfOwned to fail with ErrPIDNotOwned, got removed=%v, err=%v", removed, err)
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("file should not have been removed when not owned")
	}

	// Removal with matching PID should succeed
	removed, err = RemovePIDFileIfOwned(pidFile, 999999)
	if !removed || err != nil {
		t.Fatalf("expected successful removal, got removed=%v, err=%v", removed, err)
	}
	if _, err := os.Stat(pidFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file should have been removed")
	}
}

func TestStalePIDDetection(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "stale.pid")

	// 1. Dead PID (very high PID that doesn't exist)
	deadPID := 4194303 // Max Linux PID limit is usually 4194304
	for IsProcessAlive(deadPID) {
		deadPID--
	}

	_ = WritePIDFile(pidFile, DaemonInfo{PID: deadPID, Version: "test"})
	if info := GetDaemonInfo(pidFile); info != nil {
		t.Fatalf("expected GetDaemonInfo to return nil for dead PID %d, got %+v", deadPID, info)
	}

	// 2. Live PID for current process (which is running the test binary)
	_ = WritePIDFile(pidFile, DaemonInfo{PID: os.Getpid(), Version: "test"})
	info := GetDaemonInfo(pidFile)
	if info == nil {
		t.Fatalf("expected GetDaemonInfo to return info for current process, got nil")
	}
	if info.PID != os.Getpid() {
		t.Fatalf("PID mismatch: got %d, want %d", info.PID, os.Getpid())
	}
}

func TestTryAcquirePIDLock(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "lock.pid")

	unlock1, err := TryAcquirePIDLock(pidFile)
	if err != nil {
		t.Fatalf("first TryAcquirePIDLock failed: %v", err)
	}
	defer unlock1()

	// Second acquisition while held must fail with ErrDaemonAlreadyRunning
	_, err = TryAcquirePIDLock(pidFile)
	if !errors.Is(err, ErrDaemonAlreadyRunning) {
		t.Fatalf("expected ErrDaemonAlreadyRunning, got %v", err)
	}

	// Release lock
	unlock1()

	// Third acquisition after release must succeed
	unlock2, err := TryAcquirePIDLock(pidFile)
	if err != nil {
		t.Fatalf("subsequent TryAcquirePIDLock failed: %v", err)
	}
	unlock2()
}

func TestFailClosedProductionPIDPath(t *testing.T) {
	prodDir := config.GetRealProductionGeminiDir()
	prodPIDFile := filepath.Join(prodDir, "test-should-fail.pid")

	// Attempting to write to real ~/.gemini must fail closed
	err := WritePIDFile(prodPIDFile, DaemonInfo{PID: os.Getpid()})
	if err == nil {
		t.Fatalf("expected WritePIDFile to fail-closed on production path %q", prodPIDFile)
	}

	_, err = TryAcquirePIDLock(prodPIDFile)
	if err == nil {
		t.Fatalf("expected TryAcquirePIDLock to fail-closed on production path %q", prodPIDFile)
	}

	_, err = RemovePIDFileIfOwned(prodPIDFile, os.Getpid())
	if err == nil {
		t.Fatalf("expected RemovePIDFileIfOwned to fail-closed on production path %q", prodPIDFile)
	}
}

func TestIsProcessAlive_ESRCH(t *testing.T) {
	// PID 0 or negative
	if IsProcessAlive(0) || IsProcessAlive(-1) {
		t.Fatalf("PID <= 0 must not be alive")
	}

	// PID 1 (init/systemd) is always alive on Unix
	if !IsProcessAlive(1) {
		t.Fatalf("PID 1 must be alive")
	}
}

func TestIsAgyPoolProcess_PID1(t *testing.T) {
	// PID 1 (systemd/init) is not agy-pool, should return false
	// unless /proc is not available
	if _, err := os.Stat("/proc/1/cmdline"); err == nil {
		if IsAgyPoolProcess(1) {
			t.Fatalf("PID 1 should not be classified as agy-pool")
		}
	}
}
