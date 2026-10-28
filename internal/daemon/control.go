package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
)

var (
	ErrNotRunning = errors.New("daemon is not running")
)

// stopWithFile stops the daemon instance using the specified PID file path.
func stopWithFile(ctx *InstanceContext, pidFile string, timeout time.Duration) error {
	if err := config.AssertSafeWritePath(pidFile); err != nil {
		return err
	}

	info, err := ReadPIDFile(pidFile)
	if err != nil {
		if errors.Is(err, ErrPIDNotFound) {
			return ErrNotRunning
		}
		// Malformed PID file: safely clean up if port is not listening
		if !IsPortListening(ctx.ListenPort) {
			_ = os.Remove(pidFile)
			return nil
		}
		return fmt.Errorf("cannot safely remove malformed PID file while port %d is listening: %w", ctx.ListenPort, ErrPIDMalformed)
	}

	pid := info.PID
	if !IsProcessAlive(pid) {
		// Process already dead: safely clean up stale PID file
		_ = os.Remove(pidFile)
		return nil
	}

	// 1. Validate process identity: NEVER signal foreign process!
	if !IsAgyPoolProcess(pid) {
		_ = os.Remove(pidFile)
		return ErrPIDReused
	}

	// 2. Validate data directory ownership: NEVER signal another instance!
	if info.DataDir != "" {
		canInfoData, _ := CanonicalizeDir(info.DataDir)
		if canInfoData != ctx.DataDir {
			return fmt.Errorf("%w: PID file contains foreign data dir %q (expected %q)", ErrPIDNotOwned, info.DataDir, ctx.DataDir)
		}
	} else {
		// Legacy metadata: check if process is owned by this dataDir
		if !isProcessOwnedByDataDir(pid, ctx.DataDir) {
			return fmt.Errorf("%w: cannot verify ownership of legacy PID %d for data dir %q", ErrPIDNotOwned, pid, ctx.DataDir)
		}
	}

	// 3. Port validation
	if info.Port > 0 && ctx.ListenPort > 0 && info.Port != ctx.ListenPort {
		return fmt.Errorf("%w: daemon running on port %d, requested %d", ErrPIDNotOwned, info.Port, ctx.ListenPort)
	}

	// 4. Config path validation (refuse stop if caller specified a different config file)
	if ctx.ConfigPath != "" && info.ConfigPath != "" {
		canInfoCfg, _ := CanonicalizeFile(info.ConfigPath)
		if canInfoCfg != ctx.ConfigPath {
			return fmt.Errorf("%w: daemon running with config %q, cannot stop with config %q", ErrConfigMismatch, info.ConfigPath, ctx.ConfigPath)
		}
	} else if ctx.ConfigPath != "" && info.ConfigPath == "" {
		return fmt.Errorf("%w: daemon running without config, cannot stop with config %q", ErrConfigMismatch, ctx.ConfigPath)
	}

	// 5. Stop through systemd when it supervises the daemon, otherwise send SIGTERM
	if scope := systemdScope(pid); scope != "" {
		if err := systemctl(scope, "stop"); err != nil {
			return err
		}
	} else if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("failed to send SIGTERM to PID %d: %w", pid, err)
	}

	// 6. Poll until process exits (up to timeout)
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

	// 7. Unlink PID file if owned
	_, _ = RemovePIDFileIfOwned(pidFile, pid)
	return nil
}

// StopInstance gracefully stops the daemon instance uniquely identified by ctx.
func StopInstance(ctx *InstanceContext, timeout time.Duration) error {
	if ctx == nil {
		return errors.New("instance context is required")
	}
	return stopWithFile(ctx, ctx.PIDFile(), timeout)
}

// StopDaemon stops the running proxy daemon gracefully with SIGTERM.
// Backwards-compatible wrapper.
func StopDaemon(pidFile string, port int, timeout time.Duration) error {
	if pidFile == "" {
		pidFile = PIDFileProvider()
	}
	dataDir := filepath.Dir(pidFile)
	configPath := ""
	if info := GetDaemonInfo(pidFile); info != nil {
		configPath = info.ConfigPath
	}
	ctx, err := ResolveInstanceContext(dataDir, configPath, port, "")
	if err != nil {
		return err
	}
	return stopWithFile(ctx, pidFile, timeout)
}

// RestartInstance stops the existing daemon for ctx, waits for port release, and starts the new daemon.
func RestartInstance(ctx *InstanceContext, opts LaunchOptions) (*StartResult, error) {
	if ctx == nil {
		return nil, errors.New("instance context is required")
	}

	pidFile := opts.PIDFile
	if pidFile == "" {
		pidFile = ctx.PIDFile()
	}

	status, _, msg := CheckStatusWithFile(ctx, pidFile)
	if status == StatusForeignInstance || status == StatusPortOccupiedForeign {
		return nil, fmt.Errorf("cannot restart instance: %s", msg)
	}

	// A systemd-supervised daemon is restarted by its unit; spawning our own would fight it for the PID lock.
	if info := GetDaemonInfo(pidFile); info != nil && IsProcessAlive(info.PID) && IsAgyPoolProcess(info.PID) {
		if scope := systemdScope(info.PID); scope != "" {
			if err := systemctl(scope, "restart"); err != nil {
				return nil, err
			}
			return waitRestarted(ctx, pidFile, info.PID, scope)
		}
	}

	// 1. Stop existing daemon
	_ = stopWithFile(ctx, pidFile, 5*time.Second)

	// 2. Bounded wait for port release
	for i := 0; i < 30; i++ {
		if !IsPortListening(ctx.ListenPort) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 3. Start new daemon with same instance configuration
	opts.StateDir = ctx.DataDir
	opts.ConfigFile = ctx.ConfigPath
	opts.Port = ctx.ListenPort
	opts.PIDFile = pidFile
	res, err := StartInstance(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to start daemon during restart: %w", err)
	}
	res.WasRestarted = true
	return res, nil
}

// waitRestarted waits for a daemon other than oldPID to own pidFile and listen on the instance port.
func waitRestarted(ctx *InstanceContext, pidFile string, oldPID int, scope string) (*StartResult, error) {
	for i := 0; i < 50; i++ {
		if info := GetDaemonInfo(pidFile); info != nil && info.PID != oldPID && IsProcessAlive(info.PID) && IsPortListening(ctx.ListenPort) {
			return &StartResult{PID: info.PID, WasRestarted: true}, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("%s restarted but the gateway is not listening on port %d; inspect `systemctl %s status %s` and %s",
		systemdUnitName, ctx.ListenPort, scope, systemdUnitName, ctx.LogFile())
}

// RestartDaemon stops the existing daemon, waits for port release, and starts the new daemon.
// Backwards-compatible wrapper.
func RestartDaemon(opts LaunchOptions) (*StartResult, error) {
	dataDir := opts.StateDir
	if dataDir == "" && opts.PIDFile != "" {
		dataDir = filepath.Dir(opts.PIDFile)
	}
	ctx, err := ResolveInstanceContext(dataDir, opts.ConfigFile, opts.Port, "")
	if err != nil {
		return nil, err
	}
	return RestartInstance(ctx, opts)
}
