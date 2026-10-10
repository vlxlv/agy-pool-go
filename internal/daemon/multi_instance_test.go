package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
)

// Helper process runner for multi-instance lifecycle tests
func TestHelperMultiProcess(t *testing.T) {
	if os.Getenv("AGY_HELPER_MULTI") != "1" {
		return
	}
	port := 0
	if pStr := os.Getenv(EnvDaemonPort); pStr != "" {
		_, _ = fmt.Sscanf(pStr, "%d", &port)
	}
	for i, arg := range os.Args {
		if arg == "--port" && i+1 < len(os.Args) {
			_, _ = fmt.Sscanf(os.Args[i+1], "%d", &port)
		}
	}
	pidFile := os.Getenv("AGY_TEST_PID_FILE")
	err := RunForeground(context.Background(), ServerOptions{
		Port:                 port,
		PIDFile:              pidFile,
		QuotaRefreshInterval: 1 * time.Hour,
		DisableSignals:       false,
	})
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func getFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func TestMultiInstance_ForegroundIsolation(t *testing.T) {
	tmpDir := t.TempDir()
	cleanup := config.SetSyntheticSandboxRoot(tmpDir)
	defer cleanup()

	portA := getFreePort(t)
	portB := getFreePort(t)

	dataDirA := filepath.Join(tmpDir, "data-a")
	dataDirB := filepath.Join(tmpDir, "data-b")
	_ = os.MkdirAll(dataDirA, 0o700)
	_ = os.MkdirAll(dataDirB, 0o700)

	cfgA := filepath.Join(tmpDir, "cfg-a.json")
	cfgB := filepath.Join(tmpDir, "cfg-b.json")
	_ = os.WriteFile(cfgA, []byte(fmt.Sprintf(`{"version": 1, "server":{"port":%d}}`, portA)), 0o600)
	_ = os.WriteFile(cfgB, []byte(fmt.Sprintf(`{"version": 1, "server":{"port":%d}}`, portB)), 0o600)

	ctxA, err := ResolveInstanceContext(dataDirA, cfgA, portA, "127.0.0.1")
	if err != nil {
		t.Fatalf("failed to resolve ctxA: %v", err)
	}
	ctxB, err := ResolveInstanceContext(dataDirB, cfgB, portB, "127.0.0.1")
	if err != nil {
		t.Fatalf("failed to resolve ctxB: %v", err)
	}

	// Verify paths are isolated
	if ctxA.PIDFile() == ctxB.PIDFile() {
		t.Fatal("PID files must be isolated")
	}
	if ctxA.LogFile() == ctxB.LogFile() {
		t.Fatal("Log files must be isolated")
	}
	if ctxA.StateDB() == ctxB.StateDB() {
		t.Fatal("State DBs must be isolated")
	}
	if ctxA.LocksDir() == ctxB.LocksDir() {
		t.Fatal("Locks dirs must be isolated")
	}

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctxSrvA, cancelA := context.WithCancel(context.Background())
	ctxSrvB, cancelB := context.WithCancel(context.Background())
	doneA := make(chan struct{})
	doneB := make(chan struct{})
	t.Cleanup(func() {
		cancelA()
		cancelB()
		<-doneA
		<-doneB
	})

	readyA := make(chan struct{})
	readyB := make(chan struct{})

	go func() {
		defer close(doneA)
		_ = RunForeground(ctxSrvA, ServerOptions{
			Port:           portA,
			Instance:       ctxA,
			Handler:        testHandler,
			ReadyChan:      readyA,
			DisableSignals: true,
		})
	}()

	go func() {
		defer close(doneB)
		_ = RunForeground(ctxSrvB, ServerOptions{
			Port:           portB,
			Instance:       ctxB,
			Handler:        testHandler,
			ReadyChan:      readyB,
			DisableSignals: true,
		})
	}()

	select {
	case <-readyA:
	case <-time.After(3 * time.Second):
		t.Fatalf("server A failed to become ready")
	}
	select {
	case <-readyB:
	case <-time.After(3 * time.Second):
		t.Fatalf("server B failed to become ready")
	}

	// Verify status for both
	statusA, infoA, _ := CheckInstanceStatus(ctxA)
	if statusA != StatusRunningSameInstance {
		t.Fatalf("expected statusA RUNNING_SAME_INSTANCE, got %s", statusA)
	}
	statusB, infoB, _ := CheckInstanceStatus(ctxB)
	if statusB != StatusRunningSameInstance {
		t.Fatalf("expected statusB RUNNING_SAME_INSTANCE, got %s", statusB)
	}

	// Cross-instance negative checks:
	// A info does not match B context
	if res := MatchesInstance(infoA, ctxB); res != MatchForeign {
		t.Fatalf("expected MatchForeign for infoA against ctxB, got %v", res)
	}
	if res := MatchesInstance(infoB, ctxA); res != MatchForeign {
		t.Fatalf("expected MatchForeign for infoB against ctxA, got %v", res)
	}

	// Stop A context targeting B PID file must fail with ErrPIDNotOwned
	err = stopWithFile(ctxA, ctxB.PIDFile(), 100*time.Millisecond)
	if !errors.Is(err, ErrPIDNotOwned) {
		t.Fatalf("expected ErrPIDNotOwned when stopping B from ctxA, got %v", err)
	}

	// Stop B context targeting A PID file must fail with ErrPIDNotOwned
	err = stopWithFile(ctxB, ctxA.PIDFile(), 100*time.Millisecond)
	if !errors.Is(err, ErrPIDNotOwned) {
		t.Fatalf("expected ErrPIDNotOwned when stopping A from ctxB, got %v", err)
	}

	// Stop A
	cancelA()
	for i := 0; i < 30; i++ {
		if !IsPortListening(portA) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify B remains alive and unharmed
	if !IsPortListening(portB) {
		t.Fatal("B must remain listening after A stopped")
	}
	statusBAfter, _, _ := CheckInstanceStatus(ctxB)
	if statusBAfter != StatusRunningSameInstance {
		t.Fatalf("B status must remain RUNNING_SAME_INSTANCE, got %s", statusBAfter)
	}

	// Stop B
	cancelB()
	for i := 0; i < 30; i++ {
		if !IsPortListening(portB) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSamePort_Collision(t *testing.T) {
	tmpDir := t.TempDir()
	cleanup := config.SetSyntheticSandboxRoot(tmpDir)
	defer cleanup()

	port := getFreePort(t)

	// Bind a listener on port to simulate Instance A running
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("failed to listen on port: %v", err)
	}
	defer l.Close()

	dataDirB := filepath.Join(tmpDir, "data-b")
	_ = os.MkdirAll(dataDirB, 0o700)
	cfgB := filepath.Join(tmpDir, "cfg-b.json")
	_ = os.WriteFile(cfgB, []byte(fmt.Sprintf(`{"version": 1, "server":{"port":%d}}`, port)), 0o600)

	ctxB, err := ResolveInstanceContext(dataDirB, cfgB, port, "127.0.0.1")
	if err != nil {
		t.Fatalf("failed to resolve ctxB: %v", err)
	}

	// Check status on B: port is occupied by foreign process, B has no PID file
	status, _, _ := CheckInstanceStatus(ctxB)
	if status != StatusPortOccupiedForeign {
		t.Fatalf("expected StatusPortOccupiedForeign, got %s", status)
	}

	// Attempt StartInstance on B: must fail immediately with ErrForeignPortOccupied
	_, err = StartInstance(ctxB, LaunchOptions{Port: port})
	if !errors.Is(err, ErrForeignPortOccupied) {
		t.Fatalf("expected ErrForeignPortOccupied, got %v", err)
	}

	// Port listener of A must remain alive and undisturbed
	if !IsPortListening(port) {
		t.Fatal("listener on port must remain alive after collision attempt")
	}
}

func TestConfigMismatch_And_Tampering(t *testing.T) {
	tmpDir := t.TempDir()
	cleanup := config.SetSyntheticSandboxRoot(tmpDir)
	defer cleanup()

	port := getFreePort(t)
	dataDir := filepath.Join(tmpDir, "data")
	_ = os.MkdirAll(dataDir, 0o700)

	cfgFile := filepath.Join(tmpDir, "config.json")
	_ = os.WriteFile(cfgFile, []byte(fmt.Sprintf(`{"version": 1, "server":{"port":%d,"listen":"127.0.0.1"}}`, port)), 0o600)

	ctxA, err := ResolveInstanceContext(dataDir, cfgFile, port, "127.0.0.1")
	if err != nil {
		t.Fatalf("ResolveInstanceContext failed: %v", err)
	}

	// Write PID file matching original hash
	pidInfo := DaemonInfo{
		PID:        os.Getpid(),
		Version:    ctxA.Version,
		ConfigPath: ctxA.ConfigPath,
		ConfigHash: ctxA.ConfigHash,
		DataDir:    ctxA.DataDir,
		Port:       port,
		ListenHost: "127.0.0.1",
	}
	_ = WritePIDFile(ctxA.PIDFile(), pidInfo)

	// Now modify config file on disk (hash changes)
	_ = os.WriteFile(cfgFile, []byte(fmt.Sprintf(`{"version": 1, "server":{"port":%d,"listen":"127.0.0.1"},"scheduler":{"strategy":"max_quota"}}`, port)), 0o600)

	// Re-resolve context with modified file
	ctxModified, err := ResolveInstanceContext(dataDir, cfgFile, port, "127.0.0.1")
	if err != nil {
		t.Fatalf("ResolveInstanceContext failed after edit: %v", err)
	}
	if ctxModified.ConfigHash == ctxA.ConfigHash {
		t.Fatal("expected different config hash after edit")
	}

	// Check status: must report CONFIG_MISMATCH
	status, _, msg := CheckInstanceStatus(ctxModified)
	if status != StatusConfigMismatch {
		t.Fatalf("expected StatusConfigMismatch, got %s (msg: %s)", status, msg)
	}

	// StopInstance: data dir and PID process match, so safe stop should still recognize ownership
	// and not treat it as foreign
	res := MatchesInstance(&pidInfo, ctxModified)
	if res != MatchConfigMismatch {
		t.Fatalf("expected MatchConfigMismatch, got %v", res)
	}
}

func TestSameDataDir_DifferentConfigFile_Rejected(t *testing.T) {
	tmpDir := t.TempDir()
	cleanup := config.SetSyntheticSandboxRoot(tmpDir)
	defer cleanup()

	port := getFreePort(t)
	dataDir := filepath.Join(tmpDir, "data")
	_ = os.MkdirAll(dataDir, 0o700)

	cfgA := filepath.Join(tmpDir, "config-a.json")
	cfgB := filepath.Join(tmpDir, "config-b.json")
	_ = os.WriteFile(cfgA, []byte(fmt.Sprintf(`{"version": 1, "server":{"port":%d}}`, port)), 0o600)
	_ = os.WriteFile(cfgB, []byte(fmt.Sprintf(`{"version": 1, "server":{"port":%d}}`, port)), 0o600)

	ctxA, _ := ResolveInstanceContext(dataDir, cfgA, port, "127.0.0.1")
	ctxB, _ := ResolveInstanceContext(dataDir, cfgB, port, "127.0.0.1")

	// Daemon started with config A
	pidInfo := DaemonInfo{
		PID:        os.Getpid(),
		Version:    ctxA.Version,
		ConfigPath: ctxA.ConfigPath,
		ConfigHash: ctxA.ConfigHash,
		DataDir:    ctxA.DataDir,
		Port:       port,
		ListenHost: "127.0.0.1",
	}
	_ = WritePIDFile(ctxA.PIDFile(), pidInfo)

	// Caller attempts to stop daemon with config B
	err := StopInstance(ctxB, 100*time.Millisecond)
	if !errors.Is(err, ErrConfigMismatch) {
		t.Fatalf("expected ErrConfigMismatch when stopping with different config file, got %v", err)
	}

	// PID file must remain intact
	if _, err := os.Stat(ctxA.PIDFile()); err != nil {
		t.Fatal("PID file must not be unlinked when config mismatch stops action")
	}
}

func TestDataDir_SymlinkCanonicalization(t *testing.T) {
	tmpDir := t.TempDir()
	cleanup := config.SetSyntheticSandboxRoot(tmpDir)
	defer cleanup()

	realDir := filepath.Join(tmpDir, "real-data")
	_ = os.MkdirAll(realDir, 0o700)

	symlinkDir := filepath.Join(tmpDir, "link-data")
	if err := os.Symlink(realDir, symlinkDir); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	ctxReal, err := ResolveInstanceContext(realDir, "", 8899, "")
	if err != nil {
		t.Fatalf("failed to resolve real ctx: %v", err)
	}
	ctxLink, err := ResolveInstanceContext(symlinkDir, "", 8899, "")
	if err != nil {
		t.Fatalf("failed to resolve link ctx: %v", err)
	}

	if ctxReal.DataDir != ctxLink.DataDir {
		t.Fatalf("expected identical canonical data dir: real=%q link=%q", ctxReal.DataDir, ctxLink.DataDir)
	}
	if ctxReal.PIDFile() != ctxLink.PIDFile() {
		t.Fatalf("expected identical PID file path: real=%q link=%q", ctxReal.PIDFile(), ctxLink.PIDFile())
	}
}

func TestPIDFileTampering_FailClosed(t *testing.T) {
	tmpDir := t.TempDir()
	cleanup := config.SetSyntheticSandboxRoot(tmpDir)
	defer cleanup()

	dataDir := filepath.Join(tmpDir, "data")
	_ = os.MkdirAll(dataDir, 0o700)

	ctx, err := ResolveInstanceContext(dataDir, "", 9301, "127.0.0.1")
	if err != nil {
		t.Fatalf("ResolveInstanceContext failed: %v", err)
	}

	// 1. Wrong data_dir in metadata
	tamperedDir := DaemonInfo{
		PID:     os.Getpid(),
		DataDir: filepath.Join(tmpDir, "other-data"),
		Port:    9301,
	}
	_ = WritePIDFile(ctx.PIDFile(), tamperedDir)
	err = StopInstance(ctx, 100*time.Millisecond)
	if !errors.Is(err, ErrPIDNotOwned) {
		t.Fatalf("expected ErrPIDNotOwned for tampered data_dir, got %v", err)
	}

	// 2. Wrong port in metadata
	tamperedPort := DaemonInfo{
		PID:     os.Getpid(),
		DataDir: ctx.DataDir,
		Port:    9999,
	}
	_ = WritePIDFile(ctx.PIDFile(), tamperedPort)
	err = StopInstance(ctx, 100*time.Millisecond)
	if !errors.Is(err, ErrPIDNotOwned) {
		t.Fatalf("expected ErrPIDNotOwned for tampered port, got %v", err)
	}

	// 3. Live unrelated PID (PID 1 / systemd)
	if _, err := os.Stat("/proc/1/cmdline"); err == nil {
		tamperedPID1 := DaemonInfo{
			PID:     1,
			DataDir: ctx.DataDir,
			Port:    9301,
		}
		_ = WritePIDFile(ctx.PIDFile(), tamperedPID1)
		err = StopInstance(ctx, 100*time.Millisecond)
		if !errors.Is(err, ErrPIDReused) {
			t.Fatalf("expected ErrPIDReused for PID 1, got %v", err)
		}
	}

	// 4. Legacy PID without data_dir proof
	legacyPID := DaemonInfo{
		PID: os.Getpid(),
	}
	_ = WritePIDFile(ctx.PIDFile(), legacyPID)
	status, _, _ := CheckInstanceStatus(ctx)
	if status != StatusLegacyPID {
		t.Fatalf("expected StatusLegacyPID, got %s", status)
	}
}

func TestPerInstance_PIDLocks(t *testing.T) {
	tmpDir := t.TempDir()
	cleanup := config.SetSyntheticSandboxRoot(tmpDir)
	defer cleanup()

	dataDirA := filepath.Join(tmpDir, "data-a")
	dataDirB := filepath.Join(tmpDir, "data-b")
	_ = os.MkdirAll(dataDirA, 0o700)
	_ = os.MkdirAll(dataDirB, 0o700)

	ctxA, _ := ResolveInstanceContext(dataDirA, "", 9401, "")
	ctxB, _ := ResolveInstanceContext(dataDirB, "", 9402, "")

	// 1. Acquire lock on A
	unlockA, err := TryAcquirePIDLock(ctxA.PIDFile())
	if err != nil {
		t.Fatalf("failed to acquire lock on A: %v", err)
	}

	// 2. Lock on B in separate dir must succeed (does NOT block)
	unlockB, err := TryAcquirePIDLock(ctxB.PIDFile())
	if err != nil {
		t.Fatalf("lock on B should succeed independently of A: %v", err)
	}

	// 3. Second lock on A must fail with ErrDaemonAlreadyRunning
	_, err = TryAcquirePIDLock(ctxA.PIDFile())
	if !errors.Is(err, ErrDaemonAlreadyRunning) {
		t.Fatalf("expected ErrDaemonAlreadyRunning for concurrent lock on A, got %v", err)
	}

	unlockA()
	unlockB()

	// 4. After unlock, lock on A succeeds again
	unlockA2, err := TryAcquirePIDLock(ctxA.PIDFile())
	if err != nil {
		t.Fatalf("failed to re-acquire lock on A after unlock: %v", err)
	}
	unlockA2()
}

func TestPerInstance_StateDB_And_Log_Isolation(t *testing.T) {
	tmpDir := t.TempDir()
	cleanup := config.SetSyntheticSandboxRoot(tmpDir)
	defer cleanup()

	dataDirA := filepath.Join(tmpDir, "data-a")
	dataDirB := filepath.Join(tmpDir, "data-b")
	_ = os.MkdirAll(dataDirA, 0o700)
	_ = os.MkdirAll(dataDirB, 0o700)

	ctxA, _ := ResolveInstanceContext(dataDirA, "", 9501, "")
	ctxB, _ := ResolveInstanceContext(dataDirB, "", 9502, "")

	dbA, err := sql.Open("sqlite", ctxA.StateDB())
	if err != nil {
		t.Fatalf("failed to open dbA: %v", err)
	}
	defer dbA.Close()

	_, _ = dbA.Exec("CREATE TABLE test_data (val TEXT);")
	_, _ = dbA.Exec("INSERT INTO test_data VALUES ('only-in-a');")

	dbB, err := sql.Open("sqlite", ctxB.StateDB())
	if err != nil {
		t.Fatalf("failed to open dbB: %v", err)
	}
	defer dbB.Close()

	// Verify table test_data does not exist in dbB
	var tableName string
	err = dbB.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='test_data'").Scan(&tableName)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected dbB to be isolated and not contain test_data table from dbA, got %v", err)
	}
}

func TestMultiInstance_SubprocessLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	cleanup := config.SetSyntheticSandboxRoot(tmpDir)
	defer cleanup()

	portA := getFreePort(t)
	portB := getFreePort(t)

	dataDirA := filepath.Join(tmpDir, "sub-data-a")
	dataDirB := filepath.Join(tmpDir, "sub-data-b")
	_ = os.MkdirAll(dataDirA, 0o700)
	_ = os.MkdirAll(dataDirB, 0o700)

	cfgA := filepath.Join(tmpDir, "sub-cfg-a.json")
	cfgB := filepath.Join(tmpDir, "sub-cfg-b.json")
	_ = os.WriteFile(cfgA, []byte(fmt.Sprintf(`{"version": 1, "server":{"port":%d}}`, portA)), 0o600)
	_ = os.WriteFile(cfgB, []byte(fmt.Sprintf(`{"version": 1, "server":{"port":%d}}`, portB)), 0o600)

	ctxA, _ := ResolveInstanceContext(dataDirA, cfgA, portA, "127.0.0.1")
	ctxB, _ := ResolveInstanceContext(dataDirB, cfgB, portB, "127.0.0.1")

	optsA := LaunchOptions{
		Port:           portA,
		StateDir:       dataDirA,
		ConfigFile:     cfgA,
		EntrypointPath: os.Args[0],
		CustomArgs:     []string{"-test.run=TestHelperMultiProcess"},
	}
	optsB := LaunchOptions{
		Port:           portB,
		StateDir:       dataDirB,
		ConfigFile:     cfgB,
		EntrypointPath: os.Args[0],
		CustomArgs:     []string{"-test.run=TestHelperMultiProcess"},
	}

	t.Setenv("AGY_HELPER_MULTI", "1")

	// 1. Start A
	t.Setenv("AGY_TEST_PID_FILE", ctxA.PIDFile())
	resA, err := StartDaemon(optsA)
	if err != nil {
		t.Fatalf("failed to start A: %v", err)
	}
	defer func() { _ = StopDaemon(ctxA.PIDFile(), portA, 3*time.Second) }()

	// 2. Start B
	t.Setenv("AGY_TEST_PID_FILE", ctxB.PIDFile())
	resB, err := StartDaemon(optsB)
	if err != nil {
		t.Fatalf("failed to start B: %v", err)
	}
	defer func() { _ = StopDaemon(ctxB.PIDFile(), portB, 3*time.Second) }()

	if resA.PID == resB.PID {
		t.Fatalf("expected different PIDs for A and B, got %d", resA.PID)
	}

	// 3. Verify both are running
	if !IsPortListening(portA) {
		t.Fatal("expected A to be listening")
	}
	if !IsPortListening(portB) {
		t.Fatal("expected B to be listening")
	}

	// 4. Stop A
	err = StopDaemon(ctxA.PIDFile(), portA, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to stop A: %v", err)
	}

	// 5. B must remain alive
	if !IsPortListening(portB) {
		t.Fatal("B must remain listening after A stopped")
	}
	if !IsProcessAlive(resB.PID) {
		t.Fatal("B process must remain alive after A stopped")
	}

	// 6. Restart B
	t.Setenv("AGY_TEST_PID_FILE", ctxB.PIDFile())
	resB2, err := RestartDaemon(optsB)
	if err != nil {
		t.Fatalf("failed to restart B: %v", err)
	}
	if resB2.PID == resB.PID {
		t.Fatalf("expected new PID after B restart, got %d", resB2.PID)
	}

	// A must remain stopped
	if IsPortListening(portA) {
		t.Fatal("A must remain stopped after B restart")
	}

	// 7. Start A again
	t.Setenv("AGY_TEST_PID_FILE", ctxA.PIDFile())
	resA2, err := StartDaemon(optsA)
	if err != nil {
		t.Fatalf("failed to start A again: %v", err)
	}
	if !IsPortListening(portA) || !IsPortListening(portB) {
		t.Fatal("both A and B must be running")
	}
	if !IsProcessAlive(resA2.PID) || !IsProcessAlive(resB2.PID) {
		t.Fatal("both A and B processes must be alive")
	}
}
