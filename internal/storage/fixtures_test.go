package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadGoldenPoolFixtures(t *testing.T) {
	testdataDir := filepath.Join("..", "..", "testdata", "pool")

	// 1. Empty pool
	emptyBytes, err := os.ReadFile(filepath.Join(testdataDir, "empty.json"))
	if err != nil {
		t.Fatalf("failed to read empty.json: %v", err)
	}
	var emptyPool Pool
	if err := json.Unmarshal(emptyBytes, &emptyPool); err != nil {
		t.Fatalf("failed to parse empty.json: %v", err)
	}
	if len(emptyPool.Accounts) != 0 || emptyPool.Version != 1 || emptyPool.Strategy != "max_quota" {
		t.Fatalf("unexpected empty pool: %+v", emptyPool)
	}

	// 2. Valid 3 accounts
	validBytes, err := os.ReadFile(filepath.Join(testdataDir, "valid_3_accounts.json"))
	if err != nil {
		t.Fatalf("failed to read valid_3_accounts.json: %v", err)
	}
	var validPool Pool
	if err := json.Unmarshal(validBytes, &validPool); err != nil {
		t.Fatalf("failed to parse valid_3_accounts.json: %v", err)
	}
	if len(validPool.Accounts) != 3 {
		t.Fatalf("expected 3 accounts, got %d", len(validPool.Accounts))
	}
	if validPool.ActiveAccountID == nil || *validPool.ActiveAccountID != "acc_1" {
		t.Fatalf("expected active_account_id acc_1")
	}
	if validPool.RoundRobinLastAccountID == nil || *validPool.RoundRobinLastAccountID != "acc_2" {
		t.Fatalf("expected round_robin_last_account_id acc_2")
	}

	// 3. Legacy alpha.9 pool
	legacyBytes, err := os.ReadFile(filepath.Join(testdataDir, "legacy_alpha9.json"))
	if err != nil {
		t.Fatalf("failed to read legacy_alpha9.json: %v", err)
	}
	var legacyPool Pool
	if err := json.Unmarshal(legacyBytes, &legacyPool); err != nil {
		t.Fatalf("failed to parse legacy_alpha9.json: %v", err)
	}
	if len(legacyPool.Accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(legacyPool.Accounts))
	}
	if legacyPool.Accounts[0].LastQuota.RemainingFraction == nil || *legacyPool.Accounts[0].LastQuota.RemainingFraction != 0.65 {
		t.Fatalf("expected remaining_fraction 0.65")
	}

	// 4. Corrupt syntax returns error
	corruptBytes, err := os.ReadFile(filepath.Join(testdataDir, "corrupt.json"))
	if err != nil {
		t.Fatalf("failed to read corrupt.json: %v", err)
	}
	var corruptPool Pool
	if err := json.Unmarshal(corruptBytes, &corruptPool); err == nil {
		t.Fatalf("expected error unmarshaling corrupt.json, got nil")
	}
}
