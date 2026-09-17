package daemon

import (
	"context"
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

func TestRunForeground_LifecycleAndGracefulShutdown(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "foreground.pid")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyChan := make(chan struct{})
	boundPortChan := make(chan int, 1)

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	serverErrChan := make(chan error, 1)
	go func() {
		serverErrChan <- RunForeground(ctx, ServerOptions{
			Port:                 0,
			EphemeralPort:        true,
			PIDFile:              pidFile,
			Handler:              testHandler,
			ReadyChan:            readyChan,
			BoundPortChan:        boundPortChan,
			QuotaRefreshInterval: 1 * time.Hour,
			DisableSignals:       true,
		})
	}()

	// Wait for server to bind and be ready
	select {
	case <-readyChan:
	case err := <-serverErrChan:
		t.Fatalf("RunForeground failed immediately: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for server ready")
	}

	boundPort := <-boundPortChan
	if boundPort <= 0 {
		t.Fatalf("invalid bound port %d", boundPort)
	}

	// 1. Verify port is listening
	if !IsPortListening(boundPort) {
		t.Fatalf("expected port %d to be listening", boundPort)
	}

	// 2. Verify PID file was written
	info, err := ReadPIDFile(pidFile)
	if err != nil {
		t.Fatalf("ReadPIDFile failed: %v", err)
	}
	if info.PID != os.Getpid() {
		t.Fatalf("expected PID %d, got %d", os.Getpid(), info.PID)
	}

	// 3. Verify concurrent start on same PID file fails
	concurrentErr := RunForeground(ctx, ServerOptions{
		Port:           0,
		EphemeralPort:  true,
		PIDFile:        pidFile,
		Handler:        testHandler,
		DisableSignals: true,
	})
	if !errors.Is(concurrentErr, ErrDaemonAlreadyRunning) {
		t.Fatalf("expected ErrDaemonAlreadyRunning, got %v", concurrentErr)
	}

	// 4. Trigger graceful shutdown
	cancel()

	select {
	case err := <-serverErrChan:
		if err != nil {
			t.Fatalf("RunForeground returned error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for graceful shutdown")
	}

	// 5. Verify listener is closed
	if IsPortListening(boundPort) {
		t.Fatalf("expected port %d to no longer be listening", boundPort)
	}

	// 6. Verify PID file was cleaned up
	if _, err := os.Stat(pidFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected PID file to be removed on clean exit")
	}
}

func TestIsDaemonOutdated(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "outdated.pid")

	origPIDProvider := PIDFileProvider
	PIDFileProvider = func() string { return pidFile }
	defer func() { PIDFileProvider = origPIDProvider }()

	// If daemon is not running / port not listening, IsDaemonOutdated returns false
	if IsDaemonOutdated(pidFile, 12345) {
		t.Fatalf("expected false when port not listening")
	}

	// Start a dummy HTTP listener on ephemeral port
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	boundPortChan := make(chan int, 1)
	readyChan := make(chan struct{})

	go func() {
		_ = RunForeground(ctx, ServerOptions{
			Port:           0,
			EphemeralPort:  true,
			PIDFile:        pidFile,
			ReadyChan:      readyChan,
			BoundPortChan:  boundPortChan,
			DisableSignals: true,
		})
	}()

	<-readyChan
	port := <-boundPortChan

	// Running current version: should not be outdated
	if IsDaemonOutdated(pidFile, port) {
		t.Fatalf("expected current daemon not to be outdated")
	}

	// Overwrite PID file with an older version string
	_ = WritePIDFile(pidFile, DaemonInfo{
		PID:         os.Getpid(),
		Version:     "0.0.1-old",
		ScriptMtime: time.Now().Unix(),
	})

	if !IsDaemonOutdated(pidFile, port) {
		t.Fatalf("expected daemon with older version to be marked outdated")
	}

	// Overwrite PID file with an older mtime (1 second in past)
	currExe := GetEntrypointPath()
	if st, err := os.Stat(currExe); err == nil {
		_ = WritePIDFile(pidFile, DaemonInfo{
			PID:         os.Getpid(),
			Version:     config.Version,
			ScriptMtime: st.ModTime().Unix() - 10,
		})
		if !IsDaemonOutdated(pidFile, port) {
			t.Fatalf("expected daemon with older script_mtime to be marked outdated")
		}
	}

	cancel()
}

func TestStopDaemon_Scenarios(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "stop.pid")

	// 1. Stop nonexistent PID file
	err := StopDaemon(pidFile, 12345, 100*time.Millisecond)
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("expected ErrNotRunning, got %v", err)
	}

	// 2. Stop with dead PID: cleans up stale PID file
	deadPID := 4194300
	for IsProcessAlive(deadPID) {
		deadPID--
	}
	_ = WritePIDFile(pidFile, DaemonInfo{PID: deadPID})
	err = StopDaemon(pidFile, 12345, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("expected nil on stale dead PID cleanup, got %v", err)
	}
	if _, err := os.Stat(pidFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected stale PID file to be removed")
	}

	// 3. Stop with foreign PID (e.g. PID 1): refuses to kill, cleans up stale file
	if _, err := os.Stat("/proc/1/cmdline"); err == nil {
		_ = WritePIDFile(pidFile, DaemonInfo{PID: 1})
		err = StopDaemon(pidFile, 12345, 100*time.Millisecond)
		if !errors.Is(err, ErrPIDReused) {
			t.Fatalf("expected ErrPIDReused when PID file points to PID 1, got %v", err)
		}
		if _, err := os.Stat(pidFile); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected stale PID file to be cleaned up")
		}
	}
}

func TestHelperProcess_BackgroundDaemonSpawnAndStop(t *testing.T) {
	if os.Getenv("AGY_HELPER_PROCESS") == "1" {
		// Helper child process mode
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

	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "child.pid")
	logFile := filepath.Join(tmpDir, "child.log")

	// Pick an available ephemeral port
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get ephemeral port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	opts := LaunchOptions{
		Port:           port,
		PIDFile:        pidFile,
		LogFile:        logFile,
		EntrypointPath: os.Args[0], // Current test binary
		CustomArgs:     []string{"-test.run=TestHelperProcess_BackgroundDaemonSpawnAndStop"},
	}

	t.Setenv("AGY_HELPER_PROCESS", "1")
	t.Setenv("AGY_TEST_PID_FILE", pidFile)

	res, err := StartDaemon(opts)
	if err != nil {
		logContent, _ := os.ReadFile(logFile)
		t.Fatalf("StartDaemon failed: %v\nChild log:\n%s", err, string(logContent))
	}
	if res.PID <= 0 {
		t.Fatalf("invalid child PID %d", res.PID)
	}

	// Verify child is alive and port is listening
	if !IsProcessAlive(res.PID) {
		t.Fatalf("expected child PID %d to be alive", res.PID)
	}
	if !IsPortListening(port) {
		t.Fatalf("expected port %d to be listening", port)
	}

	// Verify log file was created with 0600 mode
	st, err := os.Stat(logFile)
	if err != nil {
		t.Fatalf("log file was not created: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected log file mode 0600, got %o", perm)
	}

	// Now stop the child daemon
	err = StopDaemon(pidFile, port, 3*time.Second)
	if err != nil {
		logContent, _ := os.ReadFile(logFile)
		t.Fatalf("StopDaemon failed: %v\nChild log:\n%s", err, string(logContent))
	}

	// Verify process is dead and port is closed
	if IsProcessAlive(res.PID) {
		t.Fatalf("expected child PID %d to be dead after StopDaemon", res.PID)
	}
	if IsPortListening(port) {
		t.Fatalf("expected port %d to no longer be listening", port)
	}
	if _, err := os.Stat(pidFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected PID file to be unlinked after StopDaemon")
	}
}
