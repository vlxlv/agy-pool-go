package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func TestRenameCommand(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	// 1. Missing arguments -> exit 2
	var stdout, stderr bytes.Buffer
	code := Main([]string{"rename"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2 on missing rename args, got %d", code)
	}

	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"rename", "1"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2 on missing rename target name, got %d", code)
	}

	// 2. Empty name -> exit 1
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"rename", "1", "   "}, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 on empty rename name, got %d", code)
	}
	if !strings.Contains(stdout.String(), "[Error] Account name cannot be empty.") {
		t.Errorf("expected empty name error, got: %s", stdout.String())
	}

	// 3. Target not found in empty pool -> exit 1
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"rename", "nonexistent", "NewName"}, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 on account not found, got %d", code)
	}
	if !strings.Contains(stdout.String(), "[Error] Account 'nonexistent' not found.") {
		t.Errorf("expected not found error, got: %s", stdout.String())
	}
}

func TestListAccounts_EmptyAndPopulated(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	// 1. Empty pool
	var stdout, stderr bytes.Buffer
	code := Main([]string{"list"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on empty list, got %d", code)
	}
	if !strings.Contains(stdout.String(), "No accounts in pool yet.") {
		t.Errorf("expected empty pool notice, got: %s", stdout.String())
	}

	// 2. Populate pool
	now := time.Now().Unix()
	frac := 0.75
	acc1 := &storage.Account{
		ID:           "acc_1",
		Email:        "user1@gmail.com",
		Name:         "Personal",
		Status:       "active",
		RequestCount: 42,
		CreatedAt:    &now,
		LastQuota: &storage.QuotaState{
			RemainingFraction: &frac,
			Gemini5H:          &storage.QuotaWindow{Fraction: &frac},
			GeminiWeekly:      &storage.QuotaWindow{Fraction: &frac},
		},
	}
	activeID := "acc_1"
	pool := &storage.Pool{
		Version:         1,
		ActiveAccountID: &activeID,
		Strategy:        config.StrategyMaxQuota,
		Accounts:        []*storage.Account{acc1},
	}
	_ = storage.SavePool(pool)

	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"list"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on populated list, got %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Personal") {
		t.Errorf("expected account name Personal, got: %s", out)
	}
	if !strings.Contains(out, "CLI Base") {
		t.Errorf("expected CLI Base status, got: %s", out)
	}
	if !strings.Contains(out, "42") {
		t.Errorf("expected 42 hits, got: %s", out)
	}

	// Also verify quota command renders the full visual dashboard
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"quota"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on quota, got %d", code)
	}
	qOut := stdout.String()
	if !strings.Contains(qOut, "Personal") || !strings.Contains(qOut, "Hits: 42") || !strings.Contains(qOut, "[CLI Base]") {
		t.Errorf("expected dashboard cards in quota output, got: %s", qOut)
	}

	// 3. Target not found -> exit 0 matching Python
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"list", "nonexistent"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on list target not found, got %d", code)
	}
	if !strings.Contains(stdout.String(), "[Error] Account 'nonexistent' not found.") {
		t.Errorf("expected not found error on stdout, got: %s", stdout.String())
	}
}

func TestMigrateLegacyCommand(t *testing.T) {
	tempDir, cleanup := setupCLITestEnv(t)
	defer cleanup()

	srcFile := filepath.Join(tempDir, "legacy_pool.json")
	targetDir := filepath.Join(tempDir, "target_data")
	content := `{
  "version": 1,
  "strategy": "max_quota",
  "accounts": [
    {"id": "acc_1", "email": "migrated@example.com", "access_token": "tok1"}
  ]
}`
	if err := os.WriteFile(srcFile, []byte(content), 0600); err != nil {
		t.Fatalf("failed to write legacy pool: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Main([]string{"migrate-legacy", "-s", srcFile, "-D", targetDir}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("migrate-legacy failed with code %d, stderr: %s", code, stderr.String())
	}

	if !strings.Contains(stdout.String(), "Successfully migrated 1 account") {
		t.Fatalf("unexpected stdout: %s", stdout.String())
	}

	// Verify target accounts.json exists
	targetAccounts := filepath.Join(targetDir, "accounts.json")
	if _, err := os.Stat(targetAccounts); err != nil {
		t.Fatalf("target accounts file was not created: %v", err)
	}

	// Verify source was untouched
	data, err := os.ReadFile(srcFile)
	if err != nil || string(data) != content {
		t.Fatalf("source file was modified or unreadable")
	}
}
