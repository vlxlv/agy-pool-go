package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConstants(t *testing.T) {
	if Window5HSecs != 18000.0 {
		t.Fatalf("expected Window5HSecs 18000.0, got %f", Window5HSecs)
	}
	if Window7DSecs != 604800.0 {
		t.Fatalf("expected Window7DSecs 604800.0, got %f", Window7DSecs)
	}
	if QuotaFreshMaxAge != 60.0 {
		t.Fatalf("expected QuotaFreshMaxAge 60.0, got %f", QuotaFreshMaxAge)
	}
	if QuotaAgingMaxAge != 300.0 {
		t.Fatalf("expected QuotaAgingMaxAge 300.0, got %f", QuotaAgingMaxAge)
	}
	if DepletedThreshold != 0.005 {
		t.Fatalf("expected DepletedThreshold 0.005, got %f", DepletedThreshold)
	}
	if StrategyMaxQuota != "max_quota" || StrategyLeastUsed != "least_used" || StrategyRoundRobin != "round_robin" {
		t.Fatalf("unexpected strategy constants")
	}
}

func TestRefuseProductionState(t *testing.T) {
	prodDir := GetRealProductionGeminiDir()
	if prodDir == "" {
		t.Skip("real production dir not detected")
	}

	// Attempting to configure real production dir must fail
	err := ConfigureStateDir(prodDir)
	if err == nil {
		t.Fatalf("expected error when configuring production dir %s, got nil", prodDir)
	}

	// Subpath in production dir must fail write check
	subPath := filepath.Join(prodDir, "agy-pool-accounts.json")
	err = AssertSafeWritePath(subPath)
	if err == nil {
		t.Fatalf("expected error when asserting safe write to %s, got nil", subPath)
	}
}

func TestAllowDevAndTempDirs(t *testing.T) {
	tmpDir := t.TempDir()
	err := ConfigureStateDir(tmpDir)
	if err != nil {
		t.Fatalf("expected success configuring temp dir, got %v", err)
	}
	t.Cleanup(ResetDataDir)

	accountsFile := GetAccountsFile()
	expectedCanonical := filepath.Join(tmpDir, "accounts.json")
	expectedLegacy := filepath.Join(tmpDir, "agy-pool-accounts.json")
	if accountsFile != expectedCanonical && accountsFile != expectedLegacy {
		t.Fatalf("expected %s or %s, got %s", expectedCanonical, expectedLegacy, accountsFile)
	}

	err = AssertSafeWritePath(accountsFile)
	if err != nil {
		t.Fatalf("expected safe write path in temp dir, got %v", err)
	}
}

func TestStaticConfigLoadAndValidation(t *testing.T) {
	tmpDir := t.TempDir()
	validCfg := filepath.Join(tmpDir, "config.json")
	content := `{
  "version": 1,
  "server": {
    "listen": "127.0.0.1",
    "port": 8899
  },
  "scheduler": {
    "strategy": "max_quota"
  },
  "native_agy": {
    "binary": "/usr/bin/agy"
  },
  "logging": {
    "max_size_bytes": 10485760,
    "backup_count": 3
  }
}`
	if err := os.WriteFile(validCfg, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigFile(validCfg)
	if err != nil {
		t.Fatalf("failed to load valid config: %v", err)
	}
	if cfg.Version != 1 || cfg.Server.Port != 8899 || cfg.Scheduler.Strategy != "max_quota" {
		t.Fatalf("unexpected config values: %+v", cfg)
	}
	if cfg.NativeAgy.Binary == nil || *cfg.NativeAgy.Binary != "/usr/bin/agy" {
		t.Fatalf("unexpected native agy binary: %+v", cfg.NativeAgy)
	}
	if cfg.Logging.MaxSizeBytes != 10485760 || cfg.Logging.BackupCount != 3 {
		t.Fatalf("unexpected logging config: %+v", cfg.Logging)
	}
}

func TestStaticConfigForbiddenKeys(t *testing.T) {
	tmpDir := t.TempDir()
	forbiddenKeys := []string{"directory", "data_dir", "state_dir", "accounts", "runtime", "quota"}

	for _, key := range forbiddenKeys {
		cfgPath := filepath.Join(tmpDir, key+".json")
		content := `{"version": 1, "` + key + `": "/some/path"}`
		if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}
		_, err := LoadConfigFile(cfgPath)
		if err == nil {
			t.Fatalf("expected error loading config with forbidden key %q, got nil", key)
		}
	}
}

func TestDataDirectoryLayout(t *testing.T) {
	tmpDir := t.TempDir()
	if err := ConfigureDataDir(tmpDir); err != nil {
		t.Fatalf("failed to configure data dir: %v", err)
	}
	t.Cleanup(ResetDataDir)

	// Verify path getters
	if GetAccountsFile() != filepath.Join(tmpDir, "accounts.json") {
		t.Fatalf("unexpected accounts file: %s", GetAccountsFile())
	}
	if GetRuntimeFile() != filepath.Join(tmpDir, "runtime.json") {
		t.Fatalf("unexpected runtime file: %s", GetRuntimeFile())
	}
	if GetQuotaFile() != filepath.Join(tmpDir, "quota.json") {
		t.Fatalf("unexpected quota file: %s", GetQuotaFile())
	}
	if GetPIDFile() != filepath.Join(tmpDir, "agy-pool.pid") {
		t.Fatalf("unexpected PID file: %s", GetPIDFile())
	}
	if GetLogFile() != filepath.Join(tmpDir, "agy-pool.log") {
		t.Fatalf("unexpected log file: %s", GetLogFile())
	}
	if GetLocksDir() != filepath.Join(tmpDir, "locks") {
		t.Fatalf("unexpected locks dir: %s", GetLocksDir())
	}
	if GetLockFile() != filepath.Join(tmpDir, "locks", "accounts.lock") {
		t.Fatalf("unexpected lock file: %s", GetLockFile())
	}
	if GetSpecificLockFile("pid.lock") != filepath.Join(tmpDir, "locks", "pid.lock") {
		t.Fatalf("unexpected specific lock file: %s", GetSpecificLockFile("pid.lock"))
	}

	// Verify legacy accounts file fallback
	legacyFile := filepath.Join(tmpDir, "agy-pool-accounts.json")
	if err := os.WriteFile(legacyFile, []byte("{}"), 0600); err != nil {
		t.Fatalf("failed to write legacy file: %v", err)
	}
	if GetAccountsFile() != legacyFile {
		t.Fatalf("expected fallback to legacy accounts file %s, got %s", legacyFile, GetAccountsFile())
	}
}

func TestStrictConfigParsingErrors(t *testing.T) {
	tmpDir := t.TempDir()

	cases := []struct {
		name    string
		json    string
		wantErr string
	}{
		{
			name:    "unknown_top_level_field",
			json:    `{"version": 1, "sever": {"port": 9000}}`,
			wantErr: "unknown field",
		},
		{
			name:    "unknown_nested_server_field",
			json:    `{"version": 1, "server": {"listen": "127.0.0.1", "port": 8899, "extra": 1}}`,
			wantErr: "unknown field",
		},
		{
			name:    "future_version_2",
			json:    `{"version": 2}`,
			wantErr: "unsupported config version 2",
		},
		{
			name:    "omitted_version",
			json:    `{"server": {"port": 8899}}`,
			wantErr: "unsupported config version 0",
		},
		{
			name:    "invalid_port_high",
			json:    `{"version": 1, "server": {"port": 70000}}`,
			wantErr: "invalid server port 70000",
		},
		{
			name:    "invalid_port_zero",
			json:    `{"version": 1, "server": {"port": 0}}`,
			wantErr: "invalid server port 0",
		},
		{
			name:    "invalid_listen_ip",
			json:    `{"version": 1, "server": {"listen": "999.999.999.999", "port": 8899}}`,
			wantErr: "invalid server listen address",
		},
		{
			name:    "invalid_scheduler_strategy",
			json:    `{"version": 1, "scheduler": {"strategy": "fastest"}}`,
			wantErr: "invalid scheduler strategy",
		},
		{
			name:    "invalid_logging_size",
			json:    `{"version": 1, "logging": {"max_size_bytes": 0}}`,
			wantErr: "invalid logging max_size_bytes",
		},
		{
			name:    "invalid_logging_backup_negative",
			json:    `{"version": 1, "logging": {"backup_count": -1}}`,
			wantErr: "invalid logging backup_count",
		},
		{
			name:    "malformed_json",
			json:    `{"version": 1, server: bad`,
			wantErr: "malformed JSON",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(tmpDir, tc.name+".json")
			if err := os.WriteFile(p, []byte(tc.json), 0644); err != nil {
				t.Fatalf("failed to write test file: %v", err)
			}
			_, err := LoadConfigFile(p)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestRelativeNativeAgyBinaryResolution(t *testing.T) {
	tmpDir := t.TempDir()
	cfgDir := filepath.Join(tmpDir, "sub", "config")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}

	cfgPath := filepath.Join(cfgDir, "config.json")
	content := `{
  "version": 1,
  "native_agy": {
    "binary": "../bin/native-agy"
  }
}`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigFile(cfgPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}
	if cfg.NativeAgy.Binary == nil {
		t.Fatalf("expected resolved binary, got nil")
	}

	expectedPath := filepath.Clean(filepath.Join(tmpDir, "sub", "bin", "native-agy"))
	if *cfg.NativeAgy.Binary != expectedPath {
		t.Fatalf("expected binary resolved to %s, got %s", expectedPath, *cfg.NativeAgy.Binary)
	}
}

func TestComputeConfigFileHash(t *testing.T) {
	tmpDir := t.TempDir()
	p := filepath.Join(tmpDir, "config.json")
	data := []byte(`{"version": 1}`)
	if err := os.WriteFile(p, data, 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	hash, err := ComputeConfigFileHash(p)
	if err != nil {
		t.Fatalf("ComputeConfigFileHash failed: %v", err)
	}
	if len(hash) != 64 {
		t.Fatalf("expected 64-char hex hash, got %s", hash)
	}
}

func TestNativeAgyResolverAndReadPathGuard(t *testing.T) {
	tmpDir := t.TempDir()
	syntheticRoot := filepath.Join(tmpDir, "synthetic_gemini")
	SetNativeAgyDir(syntheticRoot)
	defer ResetNativeAgyDir()

	if GetNativeAgyDir() != syntheticRoot {
		t.Fatalf("expected native agy dir %s, got %s", syntheticRoot, GetNativeAgyDir())
	}
	expectedCliDir := filepath.Join(syntheticRoot, "antigravity-cli")
	if GetAgyCliDir() != expectedCliDir {
		t.Fatalf("expected cli dir %s, got %s", expectedCliDir, GetAgyCliDir())
	}
	expectedToken := filepath.Join(expectedCliDir, "antigravity-oauth-token")
	if GetAgyTokenFile() != expectedToken {
		t.Fatalf("expected token file %s, got %s", expectedToken, GetAgyTokenFile())
	}
	expectedDB := filepath.Join(expectedCliDir, "conversation_summaries.db")
	if GetConversationDBFile() != expectedDB {
		t.Fatalf("expected db file %s, got %s", expectedDB, GetConversationDBFile())
	}
	expectedPresence := filepath.Join(expectedCliDir, "presence")
	if GetPresenceDir() != expectedPresence {
		t.Fatalf("expected presence dir %s, got %s", expectedPresence, GetPresenceDir())
	}
	expectedLock := filepath.Join(expectedPresence, "test_cid.lock")
	if GetPresenceLockFile("test_cid") != expectedLock {
		t.Fatalf("expected lock file %s, got %s", expectedLock, GetPresenceLockFile("test_cid"))
	}

	// Test AssertSafeReadPath in test mode
	SetTestMode(true)
	defer SetTestMode(false)

	// Reading synthetic token is permitted
	if err := AssertSafeReadPath(expectedToken); err != nil {
		t.Fatalf("expected read of synthetic token to be safe, got: %v", err)
	}

	// Reading production token in test mode is REJECTED fail-closed
	prodDir := GetRealProductionGeminiDir()
	if prodDir != "" {
		prodToken := filepath.Join(prodDir, "antigravity-cli", "antigravity-oauth-token")
		if err := AssertSafeReadPath(prodToken); err == nil {
			t.Fatalf("expected AssertSafeReadPath to reject production token %s in test mode", prodToken)
		} else if !strings.Contains(err.Error(), "refusing read from protected production path in test mode") {
			t.Fatalf("unexpected error message: %v", err)
		}
	}
}

func TestConfigExampleJsonValidation(t *testing.T) {
	examplePath := filepath.Join("..", "..", "config.example.json")
	cfg, err := LoadConfigFile(examplePath)
	if err != nil {
		t.Fatalf("failed to load and validate config.example.json: %v", err)
	}

	if cfg.Version != 1 {
		t.Errorf("expected version 1, got %d", cfg.Version)
	}
	if cfg.Server.Listen != "127.0.0.1" || cfg.Server.Port != 8899 {
		t.Errorf("unexpected server config: %+v", cfg.Server)
	}
	if cfg.Scheduler.Strategy != StrategyMaxQuota {
		t.Errorf("unexpected scheduler strategy: %s", cfg.Scheduler.Strategy)
	}
	if cfg.NativeAgy.Binary != nil {
		t.Errorf("expected native_agy.binary to be null, got %v", *cfg.NativeAgy.Binary)
	}
	if cfg.Logging.MaxSizeBytes != DefaultMaxLogBytes || cfg.Logging.BackupCount != DefaultBackupLogCount {
		t.Errorf("unexpected logging config: %+v", cfg.Logging)
	}
}
