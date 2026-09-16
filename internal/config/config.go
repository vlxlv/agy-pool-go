package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
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

// StaticConfig defines the v1 static configuration schema.
type StaticConfig struct {
	Version   int             `json:"version"`
	Server    ServerConfig    `json:"server"`
	Scheduler SchedulerConfig `json:"scheduler"`
	NativeAgy NativeAgyConfig `json:"native_agy"`
	Logging   LoggingConfig   `json:"logging"`
}

type ServerConfig struct {
	Listen string `json:"listen"`
	Port   int    `json:"port"`
}

type SchedulerConfig struct {
	Strategy string `json:"strategy"`
}

type NativeAgyConfig struct {
	Binary *string `json:"binary"`
}

type LoggingConfig struct {
	MaxSizeBytes int64 `json:"max_size_bytes"`
	BackupCount  int   `json:"backup_count"`
}

var (
	realHostUserHome        string
	realProductionGeminiDir string
	configuredStateDir      string
	configuredDataDir       string
	dataDirMu               sync.RWMutex
	activeStaticConfig      = DefaultStaticConfig()
	activeConfigMu          sync.RWMutex
	forbiddenWriteDirs      = make(map[string]bool)
	forbiddenDirsMu         sync.RWMutex
	testMode                = false
	testModeMu              sync.RWMutex
	syntheticSandboxRoot    string
	syntheticSandboxMu      sync.RWMutex
)

func init() {
	realHostUserHome = detectRealHostUserHome()
	realProductionGeminiDir = detectRealProductionGeminiDir()

	initForbiddenPaths()

	if os.Getenv("AGY_TEST_MODE") == "1" {
		testMode = true
	}

	// Initialize state dir based on environment or default dev location
	initStateDir()
}

func detectRealHostUserHome() string {
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		eval, err := filepath.EvalSymlinks(u.HomeDir)
		if err == nil {
			return eval
		}
		return filepath.Clean(u.HomeDir)
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		eval, err := filepath.EvalSymlinks(h)
		if err == nil {
			return eval
		}
		return filepath.Clean(h)
	}
	home := os.Getenv("HOME")
	if home != "" {
		return filepath.Clean(home)
	}
	return "/home/codex"
}

// GetRealHostUserHome returns the detected real OS home directory for the host user.
func GetRealHostUserHome() string {
	return realHostUserHome
}

// SetSyntheticSandboxRoot sets the active test sandbox root.
// In test mode, paths strictly within this sandbox are permitted.
func SetSyntheticSandboxRoot(root string) func() {
	syntheticSandboxMu.Lock()
	orig := syntheticSandboxRoot
	if root != "" {
		abs, err := filepath.Abs(root)
		if err == nil {
			if eval, err := filepath.EvalSymlinks(abs); err == nil {
				syntheticSandboxRoot = filepath.Clean(eval)
			} else {
				syntheticSandboxRoot = filepath.Clean(abs)
			}
		} else {
			syntheticSandboxRoot = filepath.Clean(root)
		}
	} else {
		syntheticSandboxRoot = ""
	}
	syntheticSandboxMu.Unlock()

	return func() {
		syntheticSandboxMu.Lock()
		syntheticSandboxRoot = orig
		syntheticSandboxMu.Unlock()
	}
}

// GetSyntheticSandboxRoot returns the active test sandbox root.
func GetSyntheticSandboxRoot() string {
	syntheticSandboxMu.RLock()
	defer syntheticSandboxMu.RUnlock()
	return syntheticSandboxRoot
}

func initForbiddenPaths() {
	forbiddenDirsMu.Lock()
	defer forbiddenDirsMu.Unlock()

	paths := []string{
		realProductionGeminiDir,
		filepath.Join(realHostUserHome, ".gemini"),
		filepath.Join(realHostUserHome, ".local", "bin"),
		filepath.Join(realHostUserHome, ".config", "agy-pool"),
		filepath.Join(realHostUserHome, ".local", "share", "agy-pool"),
		filepath.Join(realHostUserHome, ".bashrc"),
		filepath.Join(realHostUserHome, ".zshrc"),
		filepath.Join(realHostUserHome, ".profile"),
		filepath.Join(realHostUserHome, ".bash_profile"),
		"/usr/local/bin",
		"/usr/bin",
		"/etc/agy-pool",
		"/var/lib/agy-pool",
	}

	for _, p := range paths {
		if p == "" {
			continue
		}
		clean := filepath.Clean(p)
		forbiddenWriteDirs[clean] = true
		if eval, err := filepath.EvalSymlinks(clean); err == nil {
			forbiddenWriteDirs[filepath.Clean(eval)] = true
		}
	}
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
	if realHostUserHome != "" {
		geminiPath := filepath.Join(realHostUserHome, ".gemini")
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
	if envDir := os.Getenv("AGY_DATA_DIR"); envDir != "" {
		_ = ConfigureDataDir(envDir)
		return
	}
	if envDir := os.Getenv("AGY_GEMINI_DIR"); envDir != "" {
		_ = ConfigureDataDir(envDir)
		return
	}

	// Default Go user-local layout: ~/.local/share/agy-pool
	home := os.Getenv("HOME")
	if home == "" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	defaultDataDir := filepath.Join(home, ".local", "share", "agy-pool")
	_ = ConfigureDataDir(defaultDataDir)
}

// DefaultStaticConfig returns a StaticConfig populated with defaults.
func DefaultStaticConfig() *StaticConfig {
	return &StaticConfig{
		Version: 1,
		Server: ServerConfig{
			Listen: "127.0.0.1",
			Port:   DefaultPort,
		},
		Scheduler: SchedulerConfig{
			Strategy: StrategyMaxQuota,
		},
		NativeAgy: NativeAgyConfig{
			Binary: nil,
		},
		Logging: LoggingConfig{
			MaxSizeBytes: DefaultMaxLogBytes,
			BackupCount:  DefaultBackupLogCount,
		},
	}
}

// LoadConfigFile loads and validates a v1 static configuration JSON file.
func LoadConfigFile(path string) (*StaticConfig, error) {
	absPath, err := filepath.Abs(expandUser(path))
	if err != nil {
		return nil, fmt.Errorf("invalid config file path %q: %w", path, err)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %q: %w", absPath, err)
	}

	// 1. Check for forbidden mutable/state keys with descriptive errors
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("malformed JSON in config file %q: %w", absPath, err)
	}
	forbiddenKeys := []string{
		"directory", "data_dir", "state_dir", "accounts", "tokens", "quota",
		"active_account_id", "round_robin cursor", "round_robin_last_account_id",
		"PID", "pid", "runtime counters", "runtime", "cooldown", "working directory",
		"working_directory", "cwd",
	}
	for _, key := range forbiddenKeys {
		if _, exists := raw[key]; exists {
			return nil, fmt.Errorf("config error: forbidden key %q must not be in static config file (mutable state belongs in -D DIR)", key)
		}
	}

	// 2. Strict decoding rejecting unknown fields
	cfg := &StaticConfig{
		Server: ServerConfig{
			Listen: "127.0.0.1",
			Port:   DefaultPort,
		},
		Scheduler: SchedulerConfig{
			Strategy: StrategyMaxQuota,
		},
		Logging: LoggingConfig{
			MaxSizeBytes: DefaultMaxLogBytes,
			BackupCount:  DefaultBackupLogCount,
		},
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("config error in %q: %w", absPath, err)
	}

	// 3. Schema & value validation
	if cfg.Version != 1 {
		return nil, fmt.Errorf("unsupported config version %d (expected version 1)", cfg.Version)
	}
	if cfg.Server.Port <= 0 || cfg.Server.Port > 65535 {
		return nil, fmt.Errorf("invalid server port %d (must be between 1 and 65535)", cfg.Server.Port)
	}
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = "127.0.0.1"
	} else if cfg.Server.Listen != "localhost" && net.ParseIP(cfg.Server.Listen) == nil {
		return nil, fmt.Errorf("invalid server listen address %q", cfg.Server.Listen)
	}

	switch cfg.Scheduler.Strategy {
	case StrategyMaxQuota, StrategyLeastUsed, StrategyRoundRobin:
		// Valid
	default:
		return nil, fmt.Errorf("invalid scheduler strategy %q", cfg.Scheduler.Strategy)
	}

	if cfg.Logging.MaxSizeBytes <= 0 {
		return nil, fmt.Errorf("invalid logging max_size_bytes %d (must be > 0)", cfg.Logging.MaxSizeBytes)
	}
	if cfg.Logging.BackupCount < 0 {
		return nil, fmt.Errorf("invalid logging backup_count %d (must be >= 0)", cfg.Logging.BackupCount)
	}

	// 4. Resolve relative native_agy.binary relative to config file directory
	if cfg.NativeAgy.Binary != nil && *cfg.NativeAgy.Binary != "" {
		binPath := *cfg.NativeAgy.Binary
		if !filepath.IsAbs(binPath) {
			resolved := filepath.Clean(filepath.Join(filepath.Dir(absPath), binPath))
			cfg.NativeAgy.Binary = &resolved
		}
	}

	return cfg, nil
}

// ComputeConfigFileHash returns the SHA-256 hex digest of the raw config file bytes.
func ComputeConfigFileHash(path string) (string, error) {
	absPath, err := filepath.Abs(expandUser(path))
	if err != nil {
		return "", fmt.Errorf("failed to resolve config path: %w", err)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("failed to read config file for hashing: %w", err)
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), nil
}

// ResolveDefaultConfigFile checks standard locations for an existing static config file.
func ResolveDefaultConfigFile() (string, error) {
	if explicit := os.Getenv("AGY_CONFIG_FILE"); explicit != "" {
		abs, err := filepath.Abs(expandUser(explicit))
		if err == nil {
			if fi, err := os.Stat(abs); err == nil && !fi.IsDir() {
				return abs, nil
			}
		}
	}

	home := os.Getenv("HOME")
	if home == "" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}

	// User-local layout: ~/.config/agy-pool/config.json
	if home != "" {
		userCfg := filepath.Join(home, ".config", "agy-pool", "config.json")
		if fi, err := os.Stat(userCfg); err == nil && !fi.IsDir() {
			return userCfg, nil
		}
	}

	// System layout: /etc/agy-pool/config.json
	sysCfg := "/etc/agy-pool/config.json"
	if fi, err := os.Stat(sysCfg); err == nil && !fi.IsDir() {
		return sysCfg, nil
	}

	return "", nil
}

// GetStaticConfig returns the active static configuration.
func GetStaticConfig() *StaticConfig {
	activeConfigMu.RLock()
	defer activeConfigMu.RUnlock()
	return activeStaticConfig
}

// SetStaticConfig updates the active static configuration.
func SetStaticConfig(cfg *StaticConfig) {
	activeConfigMu.Lock()
	defer activeConfigMu.Unlock()
	if cfg != nil {
		activeStaticConfig = cfg
	}
}

// GetDataDir returns the active writable data directory.
func GetDataDir() string {
	dataDirMu.RLock()
	dir := configuredDataDir
	dataDirMu.RUnlock()
	if dir != "" {
		return dir
	}
	_ = ConfigureDefaultDataDir()
	dataDirMu.RLock()
	defer dataDirMu.RUnlock()
	return configuredDataDir
}

// ConfigureDataDir sets the writable data directory with fail-closed safety checks.
func ConfigureDataDir(dir string) error {
	abs, err := filepath.Abs(expandUser(dir))
	if err != nil {
		return fmt.Errorf("failed to resolve data directory %q: %w", dir, err)
	}

	realTarget := abs
	if eval, err := filepath.EvalSymlinks(abs); err == nil {
		realTarget = eval
	}

	if isProtectedProductionDir(realTarget) {
		return fmt.Errorf("[FAIL-CLOSED GUARD] refusing to use Python production state (%s) during Go development", realTarget)
	}

	dataDirMu.Lock()
	configuredDataDir = abs
	configuredStateDir = abs
	dataDirMu.Unlock()

	// Ensure locks directory exists
	locksDir := filepath.Join(abs, "locks")
	_ = os.MkdirAll(locksDir, 0o700)

	return nil
}

// ConfigureDefaultDataDir sets the default data directory.
func ConfigureDefaultDataDir() error {
	if envDir := os.Getenv("AGY_DATA_DIR"); envDir != "" {
		return ConfigureDataDir(envDir)
	}
	if envDir := os.Getenv("AGY_GEMINI_DIR"); envDir != "" {
		return ConfigureDataDir(envDir)
	}

	home := os.Getenv("HOME")
	if home == "" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	defaultDir := filepath.Join(home, ".local", "share", "agy-pool")
	return ConfigureDataDir(defaultDir)
}

// IsTestMode reports whether the runtime is in test mode.
func IsTestMode() bool {
	testModeMu.RLock()
	defer testModeMu.RUnlock()
	return testMode || os.Getenv("AGY_TEST_MODE") == "1"
}

// SetTestMode sets or unsets test mode.
func SetTestMode(enabled bool) {
	testModeMu.Lock()
	testMode = enabled
	testModeMu.Unlock()
	if enabled {
		os.Setenv("AGY_TEST_MODE", "1")
	} else {
		os.Unsetenv("AGY_TEST_MODE")
	}
}

// GetRealProductionGeminiDir returns the detected real production state directory.
func GetRealProductionGeminiDir() string {
	forbiddenDirsMu.RLock()
	defer forbiddenDirsMu.RUnlock()
	return realProductionGeminiDir
}

// SetRealProductionGeminiDir overrides the detected real production directory (for testing safety boundaries).
func SetRealProductionGeminiDir(dir string) func() {
	forbiddenDirsMu.Lock()
	defer forbiddenDirsMu.Unlock()
	orig := realProductionGeminiDir
	origForbidden := make(map[string]bool)
	for k, v := range forbiddenWriteDirs {
		origForbidden[k] = v
	}

	realProductionGeminiDir = dir
	forbiddenWriteDirs[dir] = true
	forbiddenWriteDirs[filepath.Clean(dir)] = true

	return func() {
		forbiddenDirsMu.Lock()
		defer forbiddenDirsMu.Unlock()
		realProductionGeminiDir = orig
		forbiddenWriteDirs = origForbidden
	}
}

// GetStateDir returns the currently configured state directory.
func GetStateDir() string {
	return GetDataDir()
}

// GetStateDBFile returns the path to state.db in GetDataDir().
func GetStateDBFile() string {
	return filepath.Join(GetDataDir(), "state.db")
}

// GetAccountsFile returns the path to the accounts pool JSON file.
// Prefers accounts.json in the data directory, falling back to agy-pool-accounts.json if present.
func GetAccountsFile() string {
	dir := GetDataDir()
	cand1 := filepath.Join(dir, "accounts.json")
	if _, err := os.Stat(cand1); err == nil {
		return cand1
	}
	cand2 := filepath.Join(dir, "agy-pool-accounts.json")
	if _, err := os.Stat(cand2); err == nil {
		return cand2
	}
	return cand1
}

// GetRuntimeFile returns the path to runtime.json in GetDataDir().
func GetRuntimeFile() string {
	return filepath.Join(GetDataDir(), "runtime.json")
}

// GetQuotaFile returns the path to quota.json in GetDataDir().
func GetQuotaFile() string {
	return filepath.Join(GetDataDir(), "quota.json")
}

// GetPIDFile returns the path to the daemon PID file in GetDataDir().
func GetPIDFile() string {
	return filepath.Join(GetDataDir(), "agy-pool.pid")
}

// GetLogFile returns the path to the gateway log file in GetDataDir().
func GetLogFile() string {
	return filepath.Join(GetDataDir(), "agy-pool.log")
}

// GetLocksDir returns the path to the locks directory in GetDataDir().
func GetLocksDir() string {
	return filepath.Join(GetDataDir(), "locks")
}

// GetLockFile returns the path to the accounts lock file.
func GetLockFile() string {
	dir := GetDataDir()
	cand1 := filepath.Join(dir, "locks", "accounts.lock")
	if _, err := os.Stat(cand1); err == nil {
		return cand1
	}
	cand2 := filepath.Join(dir, "agy-pool-accounts.json.lock")
	if _, err := os.Stat(cand2); err == nil {
		return cand2
	}
	return cand1
}

// GetSpecificLockFile returns a specific lock file inside DIR/locks/.
func GetSpecificLockFile(name string) string {
	return filepath.Join(GetDataDir(), "locks", name)
}

// NativeAgyResolver resolves paths to native agy resources.
type NativeAgyResolver interface {
	Root() string
	CliDir() string
	TokenFile() string
	SessionDB() string
	PresenceDir() string
	PresenceLock(cid string) string
}

type defaultNativeAgyResolver struct {
	customRoot string
}

func (r *defaultNativeAgyResolver) Root() string {
	if r.customRoot != "" {
		return r.customRoot
	}
	if envDir := os.Getenv("AGY_GEMINI_DIR"); envDir != "" {
		return envDir
	}
	home := os.Getenv("HOME")
	if home == "" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	return filepath.Join(home, ".gemini")
}

func (r *defaultNativeAgyResolver) CliDir() string {
	return filepath.Join(r.Root(), "antigravity-cli")
}

func (r *defaultNativeAgyResolver) TokenFile() string {
	return filepath.Join(r.CliDir(), "antigravity-oauth-token")
}

func (r *defaultNativeAgyResolver) SessionDB() string {
	return filepath.Join(r.CliDir(), "conversation_summaries.db")
}

func (r *defaultNativeAgyResolver) PresenceDir() string {
	return filepath.Join(r.CliDir(), "presence")
}

func (r *defaultNativeAgyResolver) PresenceLock(cid string) string {
	return filepath.Join(r.PresenceDir(), fmt.Sprintf("%s.lock", cid))
}

var (
	nativeAgyMu       sync.RWMutex
	nativeAgyResolver NativeAgyResolver = &defaultNativeAgyResolver{}
)

// SetNativeAgyResolver sets a custom resolver for native agy paths.
func SetNativeAgyResolver(r NativeAgyResolver) {
	nativeAgyMu.Lock()
	defer nativeAgyMu.Unlock()
	if r == nil {
		nativeAgyResolver = &defaultNativeAgyResolver{}
	} else {
		nativeAgyResolver = r
	}
}

// SetNativeAgyDir sets an explicit root directory for native agy state (e.g. for synthetic test environments).
func SetNativeAgyDir(root string) {
	nativeAgyMu.Lock()
	defer nativeAgyMu.Unlock()
	nativeAgyResolver = &defaultNativeAgyResolver{customRoot: root}
}

// ResetNativeAgyDir resets native agy path resolution to defaults.
func ResetNativeAgyDir() {
	SetNativeAgyDir("")
}

// GetNativeAgyDir returns the active root directory for native agy state.
func GetNativeAgyDir() string {
	nativeAgyMu.RLock()
	defer nativeAgyMu.RUnlock()
	return nativeAgyResolver.Root()
}

// GetAgyCliDir returns the path to native agy's antigravity-cli directory (under ~/.gemini or custom root).
func GetAgyCliDir() string {
	nativeAgyMu.RLock()
	defer nativeAgyMu.RUnlock()
	return nativeAgyResolver.CliDir()
}

// GetAgyTokenFile returns the path to the native agy compatibility token file.
func GetAgyTokenFile() string {
	nativeAgyMu.RLock()
	defer nativeAgyMu.RUnlock()
	return nativeAgyResolver.TokenFile()
}

// GetConversationDBFile returns the path to the native agy conversation summaries database.
func GetConversationDBFile() string {
	nativeAgyMu.RLock()
	defer nativeAgyMu.RUnlock()
	return nativeAgyResolver.SessionDB()
}

// GetPresenceDir returns the path to the native agy presence directory.
func GetPresenceDir() string {
	nativeAgyMu.RLock()
	defer nativeAgyMu.RUnlock()
	return nativeAgyResolver.PresenceDir()
}

// GetPresenceLockFile returns the path to a conversation presence lock file.
func GetPresenceLockFile(cid string) string {
	nativeAgyMu.RLock()
	defer nativeAgyMu.RUnlock()
	return nativeAgyResolver.PresenceLock(cid)
}

// ConfigureStateDir configures the storage directory with fail-closed safety checks.
func ConfigureStateDir(dir string) error {
	return ConfigureDataDir(dir)
}

func isProtectedProductionDir(path string) bool {
	clean := filepath.Clean(path)

	syntheticSandboxMu.RLock()
	sandbox := syntheticSandboxRoot
	syntheticSandboxMu.RUnlock()

	if sandbox != "" && (clean == sandbox || strings.HasPrefix(clean, sandbox+string(os.PathSeparator))) {
		return false
	}

	forbiddenDirsMu.RLock()
	realProd := realProductionGeminiDir
	forbiddenDirsMu.RUnlock()

	if realProd != "" && (clean == realProd || strings.HasPrefix(clean, realProd+string(os.PathSeparator))) {
		return true
	}

	if IsTestMode() {
		forbiddenDirsMu.RLock()
		defer forbiddenDirsMu.RUnlock()
		for forbidden := range forbiddenWriteDirs {
			if clean == forbidden || strings.HasPrefix(clean, forbidden+string(os.PathSeparator)) {
				return true
			}
		}
	}
	return false
}

// AssertSafeDestructivePath guards against destructive operations (write, delete, truncate, symlink)
// targeting real production and real host locations in test mode.
func AssertSafeDestructivePath(path string) error {
	if path == "" {
		return nil
	}
	abs, err := filepath.Abs(expandUser(path))
	if err != nil {
		return fmt.Errorf("[FAIL-CLOSED GUARD] failed to resolve destructive path %q: %w", path, err)
	}
	cleanAbs := filepath.Clean(abs)

	// Check 1: nominal path
	if isProtectedProductionDir(cleanAbs) {
		return fmt.Errorf("[FAIL-CLOSED GUARD] refusing write to protected production path: %s", cleanAbs)
	}

	// Check 2: enclosing directory resolved
	if parentEval, err := filepath.EvalSymlinks(filepath.Dir(cleanAbs)); err == nil {
		cleanParent := filepath.Clean(parentEval)
		resolvedParentPath := filepath.Join(cleanParent, filepath.Base(cleanAbs))
		if isProtectedProductionDir(cleanParent) || isProtectedProductionDir(resolvedParentPath) {
			return fmt.Errorf("[FAIL-CLOSED GUARD] refusing write to protected production path: %s", resolvedParentPath)
		}
	}

	// Check 3: follow symlink hops
	curr := cleanAbs
	for hop := 0; hop < 16; hop++ {
		fi, err := os.Lstat(curr)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			break
		}
		target, err := os.Readlink(curr)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(curr), target)
		}
		cleanTarget := filepath.Clean(target)
		if isProtectedProductionDir(cleanTarget) {
			return fmt.Errorf("[FAIL-CLOSED GUARD] refusing write to protected production path: %s (via %s)", cleanTarget, curr)
		}
		if targetParentEval, err := filepath.EvalSymlinks(filepath.Dir(cleanTarget)); err == nil {
			cleanTargetParent := filepath.Clean(targetParentEval)
			if isProtectedProductionDir(cleanTargetParent) {
				return fmt.Errorf("[FAIL-CLOSED GUARD] refusing write to protected production path: %s (via %s)", cleanTargetParent, curr)
			}
		}
		curr = cleanTarget
	}

	// Check 4: fully evaluated target
	if eval, err := filepath.EvalSymlinks(cleanAbs); err == nil {
		cleanEval := filepath.Clean(eval)
		if isProtectedProductionDir(cleanEval) {
			return fmt.Errorf("[FAIL-CLOSED GUARD] refusing write to protected production path: %s", cleanEval)
		}
	}

	return nil
}

// AssertSafeWritePath ensures a target path never targets real production state.
func AssertSafeWritePath(path string) error {
	return AssertSafeDestructivePath(path)
}

// AssertSafeReadPath ensures test code does not accidentally read real production state.
func AssertSafeReadPath(path string) error {
	if !IsTestMode() || path == "" {
		return nil
	}
	abs, err := filepath.Abs(expandUser(path))
	if err != nil {
		return fmt.Errorf("[FAIL-CLOSED GUARD] failed to resolve read path %q: %w", path, err)
	}
	cleanAbs := filepath.Clean(abs)

	if isProtectedProductionDir(cleanAbs) {
		return fmt.Errorf("[FAIL-CLOSED GUARD] refusing read from protected production path in test mode: %s", cleanAbs)
	}

	if eval, err := filepath.EvalSymlinks(cleanAbs); err == nil {
		cleanEval := filepath.Clean(eval)
		if isProtectedProductionDir(cleanEval) {
			return fmt.Errorf("[FAIL-CLOSED GUARD] refusing read from protected production path in test mode: %s", cleanEval)
		}
	}
	return nil
}
