package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
)

// LaunchOptions configures starting the background proxy daemon.
type LaunchOptions struct {
	Port           int
	StateDir       string
	PIDFile        string
	LogFile        string
	EntrypointPath string
	Foreground     bool
	CustomArgs     []string
}

// StartResult represents the outcome of a daemon start attempt.
type StartResult struct {
	PID          int
	AlreadyRun   bool
	WasRestarted bool
}

// StartDaemon starts the reverse proxy gateway in the background (or foreground if requested).
func StartDaemon(opts LaunchOptions) (*StartResult, error) {
	port := opts.Port
	if port <= 0 {
		port = DefaultPortProvider()
	}

	pidFile := opts.PIDFile
	if pidFile == "" {
		pidFile = PIDFileProvider()
	}
	if err := config.AssertSafeWritePath(pidFile); err != nil {
		return nil, err
	}

	logFile := opts.LogFile
	if logFile == "" {
		logFile = LogFileProvider()
	}
	if err := config.AssertSafeWritePath(logFile); err != nil {
		return nil, err
	}

	entrypoint := opts.EntrypointPath
	if entrypoint == "" {
		entrypoint = EntrypointProvider()
	}

	// 1. Check if already running
	wasRestarted := false
	if IsDaemonRunning(pidFile, port) {
		if IsDaemonOutdated(pidFile, port) {
			wasRestarted = true
			if err := StopDaemon(pidFile, port, 5*time.Second); err != nil {
				return nil, fmt.Errorf("failed to stop outdated daemon: %w", err)
			}
			time.Sleep(500 * time.Millisecond)
		} else {
			info := GetDaemonInfo(pidFile)
			pid := 0
			if info != nil {
				pid = info.PID
			}
			return &StartResult{
				PID:        pid,
				AlreadyRun: true,
			}, nil
		}
	}

	// 2. Ensure log and PID directories exist with 0700 permissions
	for _, f := range []string{pidFile, logFile} {
		dir := filepath.Dir(f)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("failed to create directory %q: %w", dir, err)
		}
	}

	// 3. Open log file with mode 0600
	logFD, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file %q: %w", logFile, err)
	}
	defer logFD.Close()
	_ = logFD.Chmod(0o600)

	// 4. Construct daemon child command
	var args []string
	if len(opts.CustomArgs) > 0 {
		args = append(args, opts.CustomArgs...)
	}
	args = append(args, "daemon-run")
	if opts.Port > 0 {
		args = append(args, "--port", strconv.Itoa(opts.Port))
	}

	cmd := exec.Command(entrypoint, args...)
	cmd.Stdout = logFD
	cmd.Stderr = logFD

	// Environment: propagate state directory if custom
	cmd.Env = os.Environ()
	if opts.StateDir != "" {
		cmd.Env = append(cmd.Env, "AGY_GEMINI_DIR="+opts.StateDir)
	}

	// Detached session: create a new session group
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to spawn daemon process: %w", err)
	}
	childExited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(childExited)
	}()

	// 5. Readiness handshake: poll until port is listening or child exits (up to 2 seconds)
	ready := false
	for i := 0; i < 20; i++ {
		if IsDaemonRunning(pidFile, port) {
			ready = true
			break
		}
		// Check if process terminated prematurely
		select {
		case <-childExited:
			i = 20
			break
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}

	info := GetDaemonInfo(pidFile)
	if ready && info != nil && info.PID > 0 {
		return &StartResult{
			PID:          info.PID,
			WasRestarted: wasRestarted,
		}, nil
	}

	return nil, fmt.Errorf("failed to start gateway daemon on port %d; inspect log file at %s", port, logFile)
}
