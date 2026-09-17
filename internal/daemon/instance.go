package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vlxlv/agy-pool-go/internal/config"
)

var (
	ErrConfigMismatch = errors.New("daemon configuration mismatch")
)

// MatchResult represents the outcome of comparing a PID metadata with an InstanceContext.
type MatchResult int

const (
	MatchExact MatchResult = iota
	MatchConfigMismatch
	MatchPortMismatch
	MatchVersionMismatch
	MatchForeign
	MatchStale
	MatchLegacy
)

// InstanceStatus represents the high-level status of an instance.
type InstanceStatus string

const (
	StatusRunningSameInstance InstanceStatus = "RUNNING_SAME_INSTANCE"
	StatusStopped             InstanceStatus = "STOPPED"
	StatusStalePID            InstanceStatus = "STALE_PID"
	StatusForeignInstance     InstanceStatus = "FOREIGN_INSTANCE"
	StatusConfigMismatch      InstanceStatus = "CONFIG_MISMATCH"
	StatusPortOccupiedForeign InstanceStatus = "PORT_OCCUPIED_FOREIGN"
	StatusOutdatedBinary      InstanceStatus = "OUTDATED_BINARY"
	StatusLegacyPID           InstanceStatus = "LEGACY_PID"
)

// InstanceContext uniquely identifies an agy-pool daemon instance.
type InstanceContext struct {
	DataDir    string `json:"data_dir"`
	ConfigPath string `json:"config_path,omitempty"`
	ConfigHash string `json:"config_hash,omitempty"`
	ListenHost string `json:"listen_host"`
	ListenPort int    `json:"listen_port"`
	BinaryPath string `json:"binary_path"`
	Version    string `json:"version"`
}

// PIDFile returns the canonical path to the instance PID file.
func (ctx *InstanceContext) PIDFile() string {
	return filepath.Join(ctx.DataDir, "agy-pool.pid")
}

// LogFile returns the canonical path to the instance log file.
func (ctx *InstanceContext) LogFile() string {
	return filepath.Join(ctx.DataDir, "agy-pool.log")
}

// StateDB returns the canonical path to the instance SQLite database.
func (ctx *InstanceContext) StateDB() string {
	return filepath.Join(ctx.DataDir, "state.db")
}

// LocksDir returns the canonical path to the instance locks directory.
func (ctx *InstanceContext) LocksDir() string {
	return filepath.Join(ctx.DataDir, "locks")
}

// LockFile returns the canonical path to a lock file in the instance locks directory.
func (ctx *InstanceContext) LockFile(name string) string {
	return filepath.Join(ctx.DataDir, "locks", name)
}

// CanonicalizeDir resolves symlinks, relative components, and user home directory for a directory path.
func CanonicalizeDir(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("empty directory path")
	}
	dir = config.ExpandUser(dir)
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve absolute directory path %q: %w", dir, err)
	}
	abs = filepath.Clean(abs)

	// If directory exists, evaluate symlinks directly
	if realPath, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(realPath), nil
	}

	// If directory doesn't exist yet, evaluate symlinks on deepest existing ancestor
	curr := abs
	var rest []string
	for {
		parent := filepath.Dir(curr)
		base := filepath.Base(curr)
		rest = append([]string{base}, rest...)
		if parent == curr {
			break
		}
		curr = parent
		if eval, err := filepath.EvalSymlinks(curr); err == nil {
			parts := append([]string{eval}, rest...)
			return filepath.Clean(filepath.Join(parts...)), nil
		}
	}
	return abs, nil
}

// CanonicalizeFile resolves symlinks, relative components, and user home directory for a file path.
func CanonicalizeFile(file string) (string, error) {
	if file == "" {
		return "", nil
	}
	file = config.ExpandUser(file)
	abs, err := filepath.Abs(file)
	if err != nil {
		return "", fmt.Errorf("failed to resolve absolute file path %q: %w", file, err)
	}
	abs = filepath.Clean(abs)
	if realPath, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(realPath), nil
	}
	return abs, nil
}

// ResolveInstanceContext constructs an explicit, immutable InstanceContext for the given data directory and config.
func ResolveInstanceContext(dataDir string, configPath string, port int, listenHost string) (*InstanceContext, error) {
	if dataDir == "" {
		dataDir = config.GetDataDir()
	}
	canDataDir, err := CanonicalizeDir(dataDir)
	if err != nil {
		return nil, fmt.Errorf("failed to canonicalize data directory %q: %w", dataDir, err)
	}

	if config.IsProtectedProductionDir(canDataDir) {
		return nil, fmt.Errorf("[FAIL-CLOSED GUARD] refusing to use Python production state (%s) during Go development", canDataDir)
	}

	canConfigPath := ""
	cfgHash := ""
	var staticCfg *config.StaticConfig
	if configPath != "" {
		can, err := CanonicalizeFile(configPath)
		if err != nil {
			return nil, fmt.Errorf("failed to canonicalize config file %q: %w", configPath, err)
		}
		canConfigPath = can
		if h, err := config.ComputeConfigFileHash(can); err == nil {
			cfgHash = h
		}
		if c, err := config.LoadConfigFile(can); err == nil && c != nil {
			staticCfg = c
		}
	} else {
		if defPath, _ := config.ResolveDefaultConfigFile(); defPath != "" {
			if can, err := CanonicalizeFile(defPath); err == nil {
				canConfigPath = can
				if h, err := config.ComputeConfigFileHash(can); err == nil {
					cfgHash = h
				}
				if c, err := config.LoadConfigFile(can); err == nil && c != nil {
					staticCfg = c
				}
			}
		}
	}

	if staticCfg != nil {
		if port <= 0 && staticCfg.Server.Port > 0 {
			port = staticCfg.Server.Port
		}
		if listenHost == "" && staticCfg.Server.Listen != "" {
			listenHost = staticCfg.Server.Listen
		}
	}

	if port <= 0 {
		port = DefaultPortProvider()
	}
	if listenHost == "" {
		listenHost = "127.0.0.1"
	}

	return &InstanceContext{
		DataDir:    canDataDir,
		ConfigPath: canConfigPath,
		ConfigHash: cfgHash,
		ListenHost: listenHost,
		ListenPort: port,
		BinaryPath: EntrypointProvider(),
		Version:    VersionProvider(),
	}, nil
}

func isProcessOwnedByDataDir(pid int, dataDir string) bool {
	if pid <= 0 || dataDir == "" {
		return false
	}
	// On Linux, inspect /proc/<pid>/cmdline
	cmdlinePath := fmt.Sprintf("/proc/%d/cmdline", pid)
	if data, err := os.ReadFile(cmdlinePath); err == nil {
		cmd := string(data)
		if strings.Contains(cmd, dataDir) {
			return true
		}
	}
	// Also inspect /proc/<pid>/environ
	environPath := fmt.Sprintf("/proc/%d/environ", pid)
	if data, err := os.ReadFile(environPath); err == nil {
		env := string(data)
		if strings.Contains(env, "AGY_DATA_DIR="+dataDir) || strings.Contains(env, "AGY_GEMINI_DIR="+dataDir) {
			return true
		}
	}
	return false
}

// MatchesInstance checks whether a DaemonInfo matches the given InstanceContext.
func MatchesInstance(info *DaemonInfo, ctx *InstanceContext) MatchResult {
	if info == nil || info.PID <= 0 {
		return MatchStale
	}
	if !IsProcessAlive(info.PID) {
		return MatchStale
	}
	if !IsAgyPoolProcess(info.PID) {
		return MatchForeign
	}

	// 1. Data directory check
	if info.DataDir != "" {
		canInfoData, _ := CanonicalizeDir(info.DataDir)
		if canInfoData != ctx.DataDir {
			return MatchForeign
		}
	} else {
		// Legacy metadata: check if process is owned by this dataDir
		if !isProcessOwnedByDataDir(info.PID, ctx.DataDir) {
			return MatchLegacy
		}
	}

	// 2. Port check
	if info.Port > 0 && ctx.ListenPort > 0 && info.Port != ctx.ListenPort {
		return MatchPortMismatch
	}

	// 3. Config path check
	if info.ConfigPath != "" && ctx.ConfigPath != "" {
		canInfoCfg, _ := CanonicalizeFile(info.ConfigPath)
		if canInfoCfg != ctx.ConfigPath {
			return MatchConfigMismatch
		}
		// 4. Config hash check (in-place modification)
		if info.ConfigHash != "" && ctx.ConfigHash != "" && info.ConfigHash != ctx.ConfigHash {
			return MatchConfigMismatch
		}
	} else if (info.ConfigPath == "") != (ctx.ConfigPath == "") {
		// One specified a config path and the other didn't
		return MatchConfigMismatch
	}

	// 5. Version check
	if info.Version != "" && ctx.Version != "" && info.Version != ctx.Version {
		return MatchVersionMismatch
	}

	return MatchExact
}

// CheckStatusWithFile inspects the status of the daemon for the given InstanceContext and specific PID file path.
func CheckStatusWithFile(ctx *InstanceContext, pidFile string) (InstanceStatus, *DaemonInfo, string) {
	if pidFile == "" {
		pidFile = ctx.PIDFile()
	}
	info, err := ReadPIDFile(pidFile)
	if err != nil {
		if errors.Is(err, ErrPIDNotFound) {
			if IsPortListening(ctx.ListenPort) {
				return StatusPortOccupiedForeign, nil, fmt.Sprintf("port %d occupied by another process, no PID file for this instance", ctx.ListenPort)
			}
			return StatusStopped, nil, "instance stopped"
		}
		if IsPortListening(ctx.ListenPort) {
			return StatusPortOccupiedForeign, nil, fmt.Sprintf("malformed PID file and port %d occupied", ctx.ListenPort)
		}
		return StatusStalePID, nil, fmt.Sprintf("malformed PID file: %v", err)
	}

	if !IsProcessAlive(info.PID) {
		if IsPortListening(ctx.ListenPort) {
			return StatusPortOccupiedForeign, info, fmt.Sprintf("daemon PID %d dead but port %d occupied by another process", info.PID, ctx.ListenPort)
		}
		return StatusStalePID, info, fmt.Sprintf("stale PID %d (process dead)", info.PID)
	}

	if !IsAgyPoolProcess(info.PID) {
		return StatusForeignInstance, info, fmt.Sprintf("PID %d is not an agy-pool process (PID reused)", info.PID)
	}

	res := MatchesInstance(info, ctx)
	switch res {
	case MatchForeign:
		return StatusForeignInstance, info, fmt.Sprintf("daemon running (PID %d) belongs to a different data directory %q", info.PID, info.DataDir)
	case MatchPortMismatch:
		return StatusPortOccupiedForeign, info, fmt.Sprintf("daemon running (PID %d) on port %d, requested %d", info.PID, info.Port, ctx.ListenPort)
	case MatchConfigMismatch:
		if info.ConfigPath != ctx.ConfigPath {
			return StatusConfigMismatch, info, fmt.Sprintf("daemon running (PID %d) with config %q, requested %q", info.PID, info.ConfigPath, ctx.ConfigPath)
		}
		return StatusConfigMismatch, info, fmt.Sprintf("config file %q modified since daemon start (hash mismatch)", ctx.ConfigPath)
	case MatchVersionMismatch:
		return StatusOutdatedBinary, info, fmt.Sprintf("daemon running (PID %d) with version %s, current binary is %s", info.PID, info.Version, ctx.Version)
	case MatchLegacy:
		return StatusLegacyPID, info, fmt.Sprintf("daemon running (PID %d) has legacy unverified metadata", info.PID)
	case MatchExact:
		if !IsPortListening(ctx.ListenPort) {
			return StatusStalePID, info, fmt.Sprintf("daemon PID %d is alive but port %d is not listening", info.PID, ctx.ListenPort)
		}
		if IsDaemonOutdated(pidFile, ctx.ListenPort) {
			return StatusOutdatedBinary, info, fmt.Sprintf("daemon running (PID %d) has outdated binary code", info.PID)
		}
		return StatusRunningSameInstance, info, fmt.Sprintf("daemon running (PID %d) on http://%s:%d", info.PID, ctx.ListenHost, ctx.ListenPort)
	default:
		return StatusForeignInstance, info, "unknown match state"
	}
}

// CheckInstanceStatus inspects the status of the daemon for the given InstanceContext.
func CheckInstanceStatus(ctx *InstanceContext) (InstanceStatus, *DaemonInfo, string) {
	return CheckStatusWithFile(ctx, ctx.PIDFile())
}
