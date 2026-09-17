package daemon

import (
	"context"
	"errors"
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
	ConfigFile     string
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

// StartInstance starts the reverse proxy gateway for the given InstanceContext.
func StartInstance(ctx *InstanceContext, opts LaunchOptions) (*StartResult, error) {
	if ctx == nil {
		return nil, errors.New("instance context is required")
	}

	pidFile := opts.PIDFile
	if pidFile == "" {
		pidFile = ctx.PIDFile()
	}
	if err := config.AssertSafeWritePath(pidFile); err != nil {
		return nil, err
	}

	logFile := opts.LogFile
	if logFile == "" {
		logFile = ctx.LogFile()
	}
	if err := config.AssertSafeWritePath(logFile); err != nil {
		return nil, err
	}

	if opts.Foreground {
		err := RunForeground(context.Background(), ServerOptions{
			Port:     ctx.ListenPort,
			PIDFile:  pidFile,
			LogFile:  logFile,
			Instance: ctx,
		})
		return &StartResult{PID: os.Getpid()}, err
	}

	entrypoint := opts.EntrypointPath
	if entrypoint == "" {
		entrypoint = EntrypointProvider()
	}

	// 1. Check current instance status
	wasRestarted := false
	if IsDaemonRunning(pidFile, ctx.ListenPort) {
		if IsDaemonOutdated(pidFile, ctx.ListenPort) {
			wasRestarted = true
			if err := stopWithFile(ctx, pidFile, 5*time.Second); err != nil {
				return nil, fmt.Errorf("failed to stop outdated daemon: %w", err)
			}
			time.Sleep(300 * time.Millisecond)
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
	} else if IsPortListening(ctx.ListenPort) {
		return nil, fmt.Errorf("%w: port %d is occupied by another process or instance", ErrForeignPortOccupied, ctx.ListenPort)
	}

	// 2. Ensure data, log, and PID directories exist with 0700 permissions
	for _, f := range []string{pidFile, logFile} {
		dir := filepath.Dir(f)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("failed to create directory %q: %w", dir, err)
		}
	}
	_ = os.MkdirAll(ctx.LocksDir(), 0o700)

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
	} else {
		args = append(args, "-D", ctx.DataDir)
		if ctx.ConfigPath != "" {
			args = append(args, "-c", ctx.ConfigPath)
		}
	}
	args = append(args, "daemon-run")
	if ctx.ListenPort > 0 {
		args = append(args, "--port", strconv.Itoa(ctx.ListenPort))
	}

	cmd := exec.Command(entrypoint, args...)
	cmd.Stdout = logFD
	cmd.Stderr = logFD

	// Environment: propagate state directory and config file
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "AGY_DATA_DIR="+ctx.DataDir)
	cmd.Env = append(cmd.Env, "AGY_GEMINI_DIR="+ctx.DataDir)
	if ctx.ConfigPath != "" {
		cmd.Env = append(cmd.Env, "AGY_CONFIG_FILE="+ctx.ConfigPath)
	}
	if opts.PIDFile != "" {
		cmd.Env = append(cmd.Env, "AGY_TEST_PID_FILE="+opts.PIDFile)
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

	// 5. Readiness handshake: verify PID file exists, process is alive, and port is listening
	ready := false
	for i := 0; i < 20; i++ {
		curInfo := GetDaemonInfo(pidFile)
		if curInfo != nil && curInfo.PID > 0 && IsProcessAlive(curInfo.PID) {
			if IsPortListening(ctx.ListenPort) {
				ready = true
				break
			}
		}
		select {
		case <-childExited:
			i = 20
			break
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}

	curInfo := GetDaemonInfo(pidFile)
	if ready && curInfo != nil && curInfo.PID > 0 {
		return &StartResult{
			PID:          curInfo.PID,
			WasRestarted: wasRestarted,
		}, nil
	}

	return nil, fmt.Errorf("failed to start gateway daemon on port %d; inspect log file at %s", ctx.ListenPort, logFile)
}

// StartDaemon starts the reverse proxy gateway in the background (or foreground if requested).
// Backwards-compatible wrapper.
func StartDaemon(opts LaunchOptions) (*StartResult, error) {
	dataDir := opts.StateDir
	if dataDir == "" && opts.PIDFile != "" {
		dataDir = filepath.Dir(opts.PIDFile)
	}
	ctx, err := ResolveInstanceContext(dataDir, opts.ConfigFile, opts.Port, "")
	if err != nil {
		return nil, err
	}
	return StartInstance(ctx, opts)
}
