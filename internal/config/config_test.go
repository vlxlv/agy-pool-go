package config

import (
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
	expected := filepath.Join(tmpDir, "agy-pool-accounts.json")
	if accountsFile != expected {
		t.Fatalf("expected %s, got %s", expected, accountsFile)
	}

	err = AssertSafeWritePath(accountsFile)
	if err != nil {
		t.Fatalf("expected safe write path in temp dir, got %v", err)
	}
}
