package daemon

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
)

var (
	ErrNotRunning = errors.New("daemon is not running")
)

// StopDaemon stops the running proxy daemon gracefully with SIGTERM.
// If the process is dead or malformed, it cleans up stale PID state safely without killing unrelated processes.
func StopDaemon(pidFile string, port int, timeout time.Duration) error {
	if pidFile == "" {
		pidFile = PIDFileProvider()
	}
	if err := config.AssertSafeWritePath(pidFile); err != nil {
		return err
	}

	info, err := ReadPIDFile(pidFile)
	if err != nil {
		if errors.Is(err, ErrPIDNotFound) {
			return ErrNotRunning
		}
		// Malformed PID file: safely clean up
		_ = os.Remove(pidFile)
		return nil
	}

	pid := info.PID
	if !IsProcessAlive(pid) {
		// Process already dead: clean up stale PID file
		_ = os.Remove(pidFile)
		return nil
	}

	// Validate process identity before sending signal!
	if !IsAgyPoolProcess(pid) {
		// Process is not an agy-pool process: do not signal foreign process!
		_ = os.Remove(pidFile)
		return ErrPIDReused
	}

	// Send SIGTERM
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("failed to send SIGTERM to PID %d: %w", pid, err)
	}

	// Poll until process exits (up to timeout)
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !IsProcessAlive(pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if IsProcessAlive(pid) {
		return fmt.Errorf("daemon (PID: %d) did not terminate within %v", pid, timeout)
	}

	// Unlink PID file if not already cleaned up
	if _, err := os.Stat(pidFile); err == nil {
		_ = os.Remove(pidFile)
	}

	return nil
}

// RestartDaemon stops the existing daemon, waits for port release, and starts the new daemon.
func RestartDaemon(opts LaunchOptions) (*StartResult, error) {
	pidFile := opts.PIDFile
	if pidFile == "" {
		pidFile = PIDFileProvider()
	}

	port := opts.Port
	if port <= 0 {
		port = DefaultPortProvider()
	}

	// 1. Stop existing daemon
	_ = StopDaemon(pidFile, port, 5*time.Second)

	// 2. Bounded wait for port release
	for i := 0; i < 30; i++ {
		if !IsPortListening(port) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 3. Start new daemon
	res, err := StartDaemon(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to start daemon during restart: %w", err)
	}
	res.WasRestarted = true
	return res, nil
}
