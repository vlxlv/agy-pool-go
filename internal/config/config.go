package config

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

const (
	Version = "0.1.0-alpha.1"

	// Strategy constants matching Python reference
	StrategyMaxQuota   = "max_quota"
	StrategyLeastUsed  = "least_used"
	StrategyRoundRobin = "round_robin"

	// Time window constants in seconds
	Window5HSecs = 18000.0  // 5 hours
	Window7DSecs = 604800.0 // 7 days

	// Quota freshness constants in seconds
	QuotaFreshMaxAge = 60.0  // fresh <= 60s
	QuotaAgingMaxAge = 300.0 // aging <= 300s, stale > 300s

	// Quota depletion threshold
	DepletedThreshold = 0.005 // raw floor <= 0.5% is depleted

	// Default network port
	DefaultPort = 8899

	// Log defaults
	DefaultMaxLogBytes    = 5 * 1024 * 1024 // 5 MB
	DefaultBackupLogCount = 1
)

// QuotaRefreshBackoff contains the retry intervals for failed quota refreshes
var QuotaRefreshBackoff = []time.Duration{
	30 * time.Second,
	60 * time.Second,
	120 * time.Second,
	240 * time.Second,
	300 * time.Second,
}

var (
	realProductionGeminiDir string
	configuredStateDir      string
	forbiddenWriteDirs      = make(map[string]bool)
	testMode                = false
)

func init() {
	realProductionGeminiDir = detectRealProductionGeminiDir()
	forbiddenWriteDirs[realProductionGeminiDir] = true
	forbiddenWriteDirs[filepath.Clean(realProductionGeminiDir)] = true

	if os.Getenv("AGY_TEST_MODE") == "1" {
		testMode = true
	}

	// Initialize state dir based on environment or default dev location
	initStateDir()
}

func detectRealProductionGeminiDir() string {
	if explicit := os.Getenv("AGY_REAL_GEMINI_DIR"); explicit != "" {
		abs, err := filepath.Abs(expandUser(explicit))
		if err == nil {
			eval, err := filepath.EvalSymlinks(abs)
			if err == nil {
				return eval
			}
			return abs
		}
	}

	// Detect real user home from OS user database independent of mutable $HOME
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		geminiPath := filepath.Join(u.HomeDir, ".gemini")
		eval, err := filepath.EvalSymlinks(geminiPath)
		if err == nil {
			return eval
		}
		return filepath.Clean(geminiPath)
	}

	// Fallback to ~ expansion
	home := os.Getenv("HOME")
	if home != "" {
		return filepath.Clean(filepath.Join(home, ".gemini"))
	}
	return "/tmp/.gemini"
}

func expandUser(path string) string {
	if strings.HasPrefix(path, "~/") || path == "~" {
		home := os.Getenv("HOME")
		if home == "" {
			if u, err := user.Current(); err == nil {
				home = u.HomeDir
			}
		}
		if path == "~" {
			return home
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

func initStateDir() {
	if envDir := os.Getenv("AGY_GEMINI_DIR"); envDir != "" {
		_ = ConfigureStateDir(envDir)
		return
	}

	// Default Go development state root is ~/.gemini-go-dev/
	home := os.Getenv("HOME")
	if home == "" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	defaultDevDir := filepath.Join(home, ".gemini-go-dev")
	_ = ConfigureStateDir(defaultDevDir)
}

// IsTestMode reports whether the runtime is in test mode.
func IsTestMode() bool {
	return testMode || os.Getenv("AGY_TEST_MODE") == "1"
}

// SetTestMode sets or unsets test mode.
func SetTestMode(enabled bool) {
	testMode = enabled
	if enabled {
		os.Setenv("AGY_TEST_MODE", "1")
	} else {
		os.Unsetenv("AGY_TEST_MODE")
	}
}

// GetRealProductionGeminiDir returns the detected real production state directory.
func GetRealProductionGeminiDir() string {
	return realProductionGeminiDir
}

// GetStateDir returns the currently configured state directory.
func GetStateDir() string {
	return configuredStateDir
}

// GetAccountsFile returns the path to the accounts pool JSON file.
func GetAccountsFile() string {
	return filepath.Join(configuredStateDir, "agy-pool-accounts.json")
}

// GetLockFile returns the path to the accounts lock file.
func GetLockFile() string {
	return filepath.Join(configuredStateDir, "agy-pool-accounts.json.lock")
}

// GetPIDFile returns the path to the daemon PID file.
func GetPIDFile() string {
	return filepath.Join(configuredStateDir, "agy-pool.pid")
}

// GetLogFile returns the path to the gateway log file.
func GetLogFile() string {
	return filepath.Join(configuredStateDir, "agy-pool.log")
}

// ConfigureStateDir configures the storage directory with fail-closed safety checks.
func ConfigureStateDir(dir string) error {
	abs, err := filepath.Abs(expandUser(dir))
	if err != nil {
		return fmt.Errorf("failed to resolve state directory %q: %w", dir, err)
	}

	// G1 HARD SAFETY DEFAULT: Refuse Python production state
	realTarget := abs
	if eval, err := filepath.EvalSymlinks(abs); err == nil {
		realTarget = eval
	}

	if isProtectedProductionDir(realTarget) {
		return fmt.Errorf("[FAIL-CLOSED GUARD] refusing to use Python production state (%s) during Go development", realTarget)
	}

	configuredStateDir = abs
	return nil
}

func isProtectedProductionDir(path string) bool {
	clean := filepath.Clean(path)
	for forbidden := range forbiddenWriteDirs {
		if clean == forbidden || strings.HasPrefix(clean, forbidden+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// AssertSafeWritePath ensures a target path never targets real production state.
func AssertSafeWritePath(path string) error {
	if path == "" {
		return nil
	}
	abs, err := filepath.Abs(expandUser(path))
	if err != nil {
		return fmt.Errorf("[FAIL-CLOSED GUARD] failed to resolve write path %q: %w", path, err)
	}

	realPath := abs
	if eval, err := filepath.EvalSymlinks(abs); err == nil {
		realPath = eval
	} else if parentEval, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		realPath = filepath.Join(parentEval, filepath.Base(abs))
	}

	if isProtectedProductionDir(realPath) {
		return fmt.Errorf("[FAIL-CLOSED GUARD] refusing write to protected production path: %s", realPath)
	}
	return nil
}
