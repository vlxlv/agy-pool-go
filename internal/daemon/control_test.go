package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRestartDaemon_Sequencing(t *testing.T) {
	if os.Getenv("AGY_HELPER_RESTART") == "1" {
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
	pidFile := filepath.Join(tmpDir, "restart.pid")
	logFile := filepath.Join(tmpDir, "restart.log")

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
		EntrypointPath: os.Args[0],
		CustomArgs:     []string{"-test.run=TestRestartDaemon_Sequencing"},
	}

	t.Setenv("AGY_HELPER_RESTART", "1")
	t.Setenv("AGY_TEST_PID_FILE", pidFile)
	t.Cleanup(func() {
		_ = StopDaemon(pidFile, port, 3*time.Second)
	})

	res1, err := StartDaemon(opts)
	if err != nil {
		t.Fatalf("initial StartDaemon failed: %v", err)
	}
	pid1 := res1.PID
	if pid1 <= 0 {
		t.Fatalf("invalid initial PID %d", pid1)
	}

	// Verify running
	if !IsDaemonRunning(pidFile, port) {
		t.Fatalf("expected daemon to be running")
	}

	// Restart
	res2, err := RestartDaemon(opts)
	if err != nil {
		t.Fatalf("RestartDaemon failed: %v", err)
	}
	pid2 := res2.PID
	if pid2 <= 0 {
		t.Fatalf("invalid restarted PID %d", pid2)
	}
	if pid1 == pid2 {
		t.Fatalf("expected new PID after restart, got same PID %d", pid1)
	}

	// Verify old PID is dead and new PID is running
	if IsProcessAlive(pid1) {
		t.Fatalf("expected old PID %d to be dead after restart", pid1)
	}
	if !IsDaemonRunning(pidFile, port) {
		t.Fatalf("expected restarted daemon to be running on port %d", port)
	}

	// Clean up
	_ = StopDaemon(pidFile, port, 3*time.Second)
}

func TestConcurrentStart_Protection(t *testing.T) {
	if os.Getenv("AGY_HELPER_CONCURRENT") == "1" {
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
	pidFile := filepath.Join(tmpDir, "concurrent.pid")
	logFile := filepath.Join(tmpDir, "concurrent.log")

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
		EntrypointPath: os.Args[0],
		CustomArgs:     []string{"-test.run=TestConcurrentStart_Protection"},
	}

	t.Setenv("AGY_HELPER_CONCURRENT", "1")
	t.Setenv("AGY_TEST_PID_FILE", pidFile)
	t.Cleanup(func() {
		_ = StopDaemon(pidFile, port, 3*time.Second)
	})

	var wg sync.WaitGroup
	var mu sync.Mutex
	var results []*StartResult
	var errs []error

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := StartDaemon(opts)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			} else {
				results = append(results, r)
			}
		}()
	}
	wg.Wait()

	// Exactly one or two successful results where the second notices AlreadyRun == true
	// or returns error, but there must be only ONE running process
	if len(results) == 0 {
		t.Fatalf("expected at least one StartDaemon to succeed, got none (errs: %v)", errs)
	}

	info := GetDaemonInfo(pidFile)
	if info == nil {
		t.Fatalf("expected daemon to be running after concurrent starts")
	}

	// Clean up
	_ = StopDaemon(pidFile, port, 3*time.Second)
}
