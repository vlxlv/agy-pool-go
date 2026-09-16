package storage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/config"
)

func setupTestStorage(t *testing.T) string {
	tmpDir := t.TempDir()
	if err := config.ConfigureStateDir(tmpDir); err != nil {
		t.Fatalf("failed to configure state dir: %v", err)
	}
	return tmpDir
}

func TestLoadMissingFileReturnsEmpty(t *testing.T) {
	setupTestStorage(t)

	pool, err := LoadPool()
	if err != nil {
		t.Fatalf("unexpected error loading missing file: %v", err)
	}
	if pool.Version != 1 || pool.Strategy != "max_quota" || len(pool.Accounts) != 0 {
		t.Fatalf("unexpected empty pool: %+v", pool)
	}
}

func TestSaveAndLoadRoundtripAndPermissions(t *testing.T) {
	setupTestStorage(t)

	p := NewEmptyPool()
	p.Strategy = "round_robin"
	p.Accounts = append(p.Accounts, &Account{
		ID:           "acc_1",
		Name:         "First",
		RequestCount: 42,
	})

	if err := SavePool(p); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	accountsFile := config.GetAccountsFile()
	info, err := os.Stat(accountsFile)
	if err != nil {
		t.Fatalf("failed to stat accounts file: %v", err)
	}

	// Check 0600 permissions
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("expected file permissions 0600, got %o", mode)
	}

	loaded, err := LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	if loaded.Strategy != "round_robin" || len(loaded.Accounts) != 1 {
		t.Fatalf("loaded pool mismatched: %+v", loaded)
	}
	if loaded.Accounts[0].ID != "acc_1" || loaded.Accounts[0].RequestCount != 42 {
		t.Fatalf("loaded account mismatched: %+v", loaded.Accounts[0])
	}
}

func TestCorruptFileReturnsError(t *testing.T) {
	setupTestStorage(t)

	accountsFile := config.GetAccountsFile()
	if err := os.WriteFile(accountsFile, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("failed to write corrupt file: %v", err)
	}

	_, err := LoadPool()
	if err == nil {
		t.Fatalf("expected error loading corrupt file, got nil")
	}

	// PoolTransaction must also fail and NOT overwrite corrupt file
	txErr := PoolTransaction(func(pool *Pool) error {
		pool.Strategy = "least_used"
		return nil
	})
	if txErr == nil {
		t.Fatalf("expected PoolTransaction to fail on corrupt file, got nil")
	}

	// File content must still be the corrupt string (not overwritten)
	raw, _ := os.ReadFile(accountsFile)
	if string(raw) != "{not valid json" {
		t.Fatalf("corrupt file was overwritten: %s", string(raw))
	}
}

func TestFailedTransactionAborts(t *testing.T) {
	setupTestStorage(t)

	initial := NewEmptyPool()
	initial.Strategy = "max_quota"
	if err := SavePool(initial); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	errExpected := errors.New("simulated business error")
	err := PoolTransaction(func(pool *Pool) error {
		pool.Strategy = "least_used"
		return errExpected
	})

	if !errors.Is(err, errExpected) {
		t.Fatalf("expected errExpected, got %v", err)
	}

	// State should still be max_quota
	loaded, err := LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	if loaded.Strategy != "max_quota" {
		t.Fatalf("aborted transaction mutated state: strategy is %s", loaded.Strategy)
	}
}

func TestTempFilesCleanedUp(t *testing.T) {
	stateDir := setupTestStorage(t)

	p := NewEmptyPool()
	if err := SavePool(p); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) == ".tmp" {
			t.Fatalf("found leftover temp file: %s", name)
		}
	}
}

func TestConcurrentGoroutinesNoLostUpdates(t *testing.T) {
	setupTestStorage(t)

	p := NewEmptyPool()
	p.Accounts = append(p.Accounts, &Account{
		ID:           "acc_counter",
		RequestCount: 0,
	})
	if err := SavePool(p); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	const concurrency = 20
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			err := PoolTransaction(func(pool *Pool) error {
				for _, a := range pool.Accounts {
					if a.ID == "acc_counter" {
						a.RequestCount++
					}
				}
				return nil
			})
			if err != nil {
				t.Errorf("PoolTransaction error: %v", err)
			}
		}()
	}

	wg.Wait()

	loaded, err := LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	if loaded.Accounts[0].RequestCount != int64(concurrency) {
		t.Fatalf("lost updates: expected %d, got %d", concurrency, loaded.Accounts[0].RequestCount)
	}
}

func TestFindAccountAndNextID(t *testing.T) {
	pool := &Pool{
		Accounts: []*Account{
			{ID: "acc_1", Email: "alpha@example.com"},
			{ID: "acc_3", Email: "gamma@example.com"},
		},
	}

	// Find by ID
	found := FindAccount(pool, &Account{ID: "acc_1"})
	if found == nil || found.Email != "alpha@example.com" {
		t.Fatalf("failed to find by ID")
	}

	// Find by Email
	found = FindAccount(pool, &Account{Email: "gamma@example.com"})
	if found == nil || found.ID != "acc_3" {
		t.Fatalf("failed to find by email")
	}

	// Next ID should fill gap at acc_2
	next := NextAccountID(pool.Accounts)
	if next != "acc_2" {
		t.Fatalf("expected next ID to be acc_2, got %s", next)
	}

	// Fill acc_2
	pool.Accounts = append(pool.Accounts, &Account{ID: "acc_2"})
	next = NextAccountID(pool.Accounts)
	if next != "acc_4" {
		t.Fatalf("expected next ID to be acc_4, got %s", next)
	}
}

func TestMigrateLegacyPool(t *testing.T) {
	srcDir := t.TempDir()
	targetDir := t.TempDir()

	legacyFile := filepath.Join(srcDir, "agy-pool-accounts.json")
	legacyContent := `{
  "version": 1,
  "strategy": "least_used",
  "active_account_id": "acc_1",
  "accounts": [
    {
      "id": "acc_1",
      "email": "user1@example.com",
      "access_token": "token1",
      "refresh_token": "refresh1",
      "request_count": 5
    },
    {
      "id": "acc_2",
      "email": "user2@example.com",
      "access_token": "token2",
      "refresh_token": "refresh2",
      "request_count": 10
    }
  ]
}`
	if err := os.WriteFile(legacyFile, []byte(legacyContent), 0600); err != nil {
		t.Fatalf("failed to write legacy file: %v", err)
	}

	srcHashBefore, err := config.ComputeConfigFileHash(legacyFile)
	if err != nil {
		t.Fatalf("failed to compute hash: %v", err)
	}

	res, err := MigrateLegacyPool(legacyFile, targetDir)
	if err != nil {
		t.Fatalf("MigrateLegacyPool failed: %v", err)
	}

	if res.AccountsCount != 2 {
		t.Fatalf("expected 2 accounts migrated, got %d", res.AccountsCount)
	}
	if res.ActiveAccountID != "acc_1" {
		t.Fatalf("expected active account acc_1, got %s", res.ActiveAccountID)
	}
	if res.Strategy != "least_used" {
		t.Fatalf("expected strategy least_used, got %s", res.Strategy)
	}

	// Verify source was completely untouched
	srcHashAfter, err := config.ComputeConfigFileHash(legacyFile)
	if err != nil {
		t.Fatalf("failed to compute source hash after migration: %v", err)
	}
	if srcHashBefore != srcHashAfter {
		t.Fatalf("legacy source file was mutated during migration! before: %s, after: %s", srcHashBefore, srcHashAfter)
	}

	// Verify target accounts.json exists and loads
	targetAccounts := filepath.Join(targetDir, "accounts.json")
	loaded, err := readPoolUnlocked(targetAccounts)
	if err != nil {
		t.Fatalf("failed to load migrated target accounts.json: %v", err)
	}
	if len(loaded.Accounts) != 2 || loaded.Accounts[0].ID != "acc_1" || loaded.Accounts[1].ID != "acc_2" {
		t.Fatalf("unexpected migrated accounts in target pool: %+v", loaded.Accounts)
	}

	// Verify fail-closed guard refuses protected production destination
	prodDir := config.GetRealProductionGeminiDir()
	if prodDir != "" {
		_, err = MigrateLegacyPool(legacyFile, prodDir)
		if err == nil {
			t.Fatalf("expected fail-closed error migrating to production dir %s, got nil", prodDir)
		}
	}
}

func TestTargetSchemas(t *testing.T) {
	acc := &TargetAccountAuthRecord{
		ID:          "acc_1",
		Email:       "test@example.com",
		AccessToken: "ya29.test",
	}
	accs := &TargetAccountsFile{
		Version:  1,
		Accounts: []*TargetAccountAuthRecord{acc},
	}
	data, err := json.Marshal(accs)
	if err != nil {
		t.Fatalf("failed to marshal target accounts: %v", err)
	}

	var parsed TargetAccountsFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal target accounts: %v", err)
	}
	if len(parsed.Accounts) != 1 || parsed.Accounts[0].Email != "test@example.com" {
		t.Fatalf("unexpected target accounts data")
	}

	rt := &TargetRuntimeFile{
		Version: 1,
		AccountsRuntime: map[string]*TargetAccountRuntimeRecord{
			"acc_1": {
				RequestCount: 42,
				Status:       "active",
			},
		},
	}
	rtData, err := json.Marshal(rt)
	if err != nil {
		t.Fatalf("failed to marshal target runtime: %v", err)
	}
	var parsedRT TargetRuntimeFile
	if err := json.Unmarshal(rtData, &parsedRT); err != nil {
		t.Fatalf("failed to unmarshal target runtime: %v", err)
	}
	if parsedRT.AccountsRuntime["acc_1"].RequestCount != 42 {
		t.Fatalf("unexpected runtime request count")
	}
}
