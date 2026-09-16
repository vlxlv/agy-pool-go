package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

var (
	ErrPIDNotFound          = errors.New("pid file not found")
	ErrPIDMalformed         = errors.New("pid file malformed")
	ErrProcessNotRunning    = errors.New("process not running")
	ErrPIDReused            = errors.New("pid reused by unrelated process")
	ErrPIDNotOwned          = errors.New("pid file not owned by current process")
	ErrDaemonAlreadyRunning = errors.New("daemon is already running")
	ErrForeignPortOccupied  = errors.New("port is occupied by another process")
)

// DaemonInfo represents the metadata written to agy-pool.pid matching Python v0.1.0-beta.2 schema.
type DaemonInfo struct {
	PID         int    `json:"pid"`
	Version     string `json:"version,omitempty"`
	ScriptMtime int64  `json:"script_mtime,omitempty"`
}

var (
	PIDFileProvider     = config.GetPIDFile
	LogFileProvider     = config.GetLogFile
	DefaultPortProvider = func() int { return config.DefaultPort }
	VersionProvider     = func() string { return config.Version }
	EntrypointProvider  = GetEntrypointPath
)

// GetEntrypointPath returns the canonical path of the agy-pool binary.
func GetEntrypointPath() string {
	exe, err := os.Executable()
	if err == nil {
		if eval, err := filepath.EvalSymlinks(exe); err == nil {
			return eval
		}
		return exe
	}
	if len(os.Args) > 0 && os.Args[0] != "" {
		if abs, err := filepath.Abs(os.Args[0]); err == nil {
			return abs
		}
	}
	return "agy-pool"
}

// WritePIDFile writes the PID metadata atomically with mode 0600.
func WritePIDFile(path string, info DaemonInfo) error {
	if err := config.AssertSafeWritePath(path); err != nil {
		return err
	}
	return storage.AtomicJSONWrite(path, info)
}

// ReadPIDFile reads the PID metadata, supporting both JSON and legacy integer format.
func ReadPIDFile(path string) (*DaemonInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrPIDNotFound
		}
		return nil, fmt.Errorf("failed to read pid file: %w", err)
	}

	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, ErrPIDMalformed
	}

	if strings.HasPrefix(trimmed, "{") {
		var info DaemonInfo
		if err := json.Unmarshal([]byte(trimmed), &info); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrPIDMalformed, err)
		}
		if info.PID <= 0 {
			return nil, fmt.Errorf("%w: invalid pid %d", ErrPIDMalformed, info.PID)
		}
		return &info, nil
	}

	// Legacy format: raw integer PID
	pid, err := strconv.Atoi(trimmed)
	if err != nil || pid <= 0 {
		return nil, fmt.Errorf("%w: invalid integer pid %q", ErrPIDMalformed, trimmed)
	}
	return &DaemonInfo{
		PID: pid,
	}, nil
}

// IsProcessAlive reports whether a process with the given PID is currently alive (and not a zombie).
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err != nil {
		// If permission is denied (EPERM), the process exists but is owned by another user
		if errors.Is(err, syscall.EPERM) {
			return true
		}
		return false
	}

	// On Linux, verify the process is not a zombie (terminated waiting for parent reap)
	statusPath := fmt.Sprintf("/proc/%d/status", pid)
	if data, err := os.ReadFile(statusPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "State:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 && fields[1] == "Z" {
					return false
				}
				break
			}
		}
	}

	return true
}

// IsAgyPoolProcess validates that a live PID belongs to agy-pool, avoiding false positives on PID reuse.
func IsAgyPoolProcess(pid int) bool {
	if !IsProcessAlive(pid) {
		return false
	}

	cmdlinePath := fmt.Sprintf("/proc/%d/cmdline", pid)
	data, err := os.ReadFile(cmdlinePath)
	if err != nil {
		// On non-Linux or systems where /proc is unavailable, assume valid if alive
		return true
	}

	cmd := string(bytes.ReplaceAll(data, []byte{0}, []byte(" ")))
	if strings.Contains(cmd, "agy-pool") || strings.Contains(cmd, "python") || strings.Contains(cmd, ".test") {
		return true
	}
	return false
}

// GetDaemonInfo returns validated DaemonInfo if a live agy-pool daemon is running.
// Returns nil if missing, stale, dead, or unrelated process.
func GetDaemonInfo(pidFile string) *DaemonInfo {
	if pidFile == "" {
		pidFile = PIDFileProvider()
	}

	info, err := ReadPIDFile(pidFile)
	if err != nil {
		return nil
	}

	if !IsProcessAlive(info.PID) {
		return nil
	}

	if !IsAgyPoolProcess(info.PID) {
		return nil
	}

	return info
}

// GetDaemonPID returns the verified daemon PID, or 0 if not running.
func GetDaemonPID() int {
	info := GetDaemonInfo("")
	if info != nil {
		return info.PID
	}
	return 0
}

// RemovePIDFileIfOwned unlinks the PID file ONLY if it currently contains expectedPID.
// Returns true if unlinked or already absent, false if not owned.
func RemovePIDFileIfOwned(path string, expectedPID int) (bool, error) {
	if err := config.AssertSafeWritePath(path); err != nil {
		return false, err
	}

	info, err := ReadPIDFile(path)
	if err != nil {
		if errors.Is(err, ErrPIDNotFound) {
			return true, nil
		}
		// If malformed, we only remove if expectedPID <= 0 (forced cleanup)
		if expectedPID <= 0 {
			_ = os.Remove(path)
			return true, nil
		}
		return false, err
	}

	if info.PID != expectedPID {
		return false, ErrPIDNotOwned
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("failed to unlink pid file: %w", err)
	}
	return true, nil
}

// TryAcquirePIDLock attempts to acquire an exclusive, non-blocking file lock on pidFile + ".lock".
// If acquired, returns an unlock function. If already locked, returns ErrDaemonAlreadyRunning.
func TryAcquirePIDLock(pidFile string) (func(), error) {
	if err := config.AssertSafeWritePath(pidFile); err != nil {
		return nil, err
	}

	lockPath := pidFile + ".lock"
	if err := config.AssertSafeWritePath(lockPath); err != nil {
		return nil, err
	}

	dir := filepath.Dir(lockPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create lock directory: %w", err)
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to open lock file %q: %w", lockPath, err)
	}
	_ = f.Chmod(0o600)

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrDaemonAlreadyRunning
		}
		return nil, fmt.Errorf("failed to acquire pid lock: %w", err)
	}

	unlock := func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
	return unlock, nil
}
