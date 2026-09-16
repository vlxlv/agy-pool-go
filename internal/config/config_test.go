package config

import (
	"os"
	"path/filepath"
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
