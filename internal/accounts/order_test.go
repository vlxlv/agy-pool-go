package accounts

import (
	"bytes"
	"strings"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func floatPtr(v float64) *float64 {
	return &v
}

func createTestOrderPool(t *testing.T) *storage.Pool {
	setupIsolatedTestDir(t)
	acc1 := &storage.Account{
		ID:           "acc_1",
		Name:         "Main CLI",
		Email:        "alpha@test.com",
		RequestCount: 100,
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.8)},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.9)},
		},
	}
	acc2 := &storage.Account{
		ID:           "acc_2",
		Name:         "Backup Node",
		Email:        "beta@test.com",
		RequestCount: 200,
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.5)},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.6)},
		},
	}
	acc3 := &storage.Account{
		ID:           "acc_3",
		Name:         "Work Node",
		Email:        "gamma@test.com",
		RequestCount: 50,
		LastQuota: &storage.QuotaState{
			Gemini5H:     &storage.QuotaWindow{Fraction: floatPtr(0.95)},
			GeminiWeekly: &storage.QuotaWindow{Fraction: floatPtr(0.95)},
		},
	}
	acc4 := &storage.Account{
		ID:           "acc_10",
		Name:         "Dev Node",
		Email:        "delta@test.com",
		RequestCount: 10,
		Status:       storage.StatusValidationRequired,
	}

	p := storage.NewEmptyPool()
	p.Accounts = []*storage.Account{acc1, acc2, acc3, acc4}
	p.ActiveAccountID = &acc1.ID
	if err := storage.SavePool(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveAccountIndex(t *testing.T) {
	accs := []*storage.Account{
		{ID: "acc_1", Name: "Alpha Dev", Email: "alpha@corp.com"},
		{ID: "acc_2", Name: "Beta Ops", Email: "beta@corp.com"},
		{ID: "acc_3", Name: "Gamma QA", Email: "gamma@corp.com"},
	}

	// 1-based index
	if idx := ResolveAccountIndex(accs, "1"); idx != 0 {
		t.Fatalf("expected 0, got %d", idx)
	}
	if idx := ResolveAccountIndex(accs, "3"); idx != 2 {
		t.Fatalf("expected 2, got %d", idx)
	}
	if idx := ResolveAccountIndex(accs, "0"); idx != -1 {
		t.Fatalf("expected -1, got %d", idx)
	}
	if idx := ResolveAccountIndex(accs, "4"); idx != -1 {
		t.Fatalf("expected -1, got %d", idx)
	}

	// Exact ID, name, email
	if idx := ResolveAccountIndex(accs, "acc_2"); idx != 1 {
		t.Fatalf("expected 1, got %d", idx)
	}
	if idx := ResolveAccountIndex(accs, "Alpha Dev"); idx != 0 {
		t.Fatalf("expected 0, got %d", idx)
	}
	if idx := ResolveAccountIndex(accs, "gamma@corp.com"); idx != 2 {
		t.Fatalf("expected 2, got %d", idx)
	}

	// Case-insensitive
	if idx := ResolveAccountIndex(accs, "alpha dev"); idx != 0 {
		t.Fatalf("expected 0, got %d", idx)
	}
	if idx := ResolveAccountIndex(accs, "BETA@CORP.COM"); idx != 1 {
		t.Fatalf("expected 1, got %d", idx)
	}

	// Unique substring
	if idx := ResolveAccountIndex(accs, "gamma"); idx != 2 {
		t.Fatalf("expected 2, got %d", idx)
	}

	// Ambiguous substring ("corp.com" matches all)
	if idx := ResolveAccountIndex(accs, "corp.com"); idx != -1 {
		t.Fatalf("expected -1 for ambiguous, got %d", idx)
	}

	// Non-existent
	if idx := ResolveAccountIndex(accs, "omega"); idx != -1 {
		t.Fatalf("expected -1, got %d", idx)
	}
}

func TestMoveAccount(t *testing.T) {
	createTestOrderPool(t)

	// Move acc_1 to bottom
	res, err := MoveAccount("acc_1", "bottom")
	if err != nil {
		t.Fatal(err)
	}
	if res.DestIndex != 3 || res.Accounts[3].ID != "acc_1" {
		t.Fatalf("unexpected move result: %+v", res)
	}
	if res.Accounts[0].ID != "acc_2" {
		t.Fatalf("expected acc_2 at top, got %s", res.Accounts[0].ID)
	}

	// Move acc_1 back to top
	res, err = MoveAccount("acc_1", "top")
	if err != nil {
		t.Fatal(err)
	}
	if res.DestIndex != 0 || res.Accounts[0].ID != "acc_1" {
		t.Fatalf("unexpected move result: %+v", res)
	}

	// Move down by 1 (+1)
	res, err = MoveAccount("acc_1", "down")
	if err != nil {
		t.Fatal(err)
	}
	if res.DestIndex != 1 || res.Accounts[1].ID != "acc_1" {
		t.Fatalf("expected acc_1 at 1, got %d", res.DestIndex)
	}

	// Move up by 1 (-1)
	res, err = MoveAccount("acc_1", "up")
	if err != nil {
		t.Fatal(err)
	}
	if res.DestIndex != 0 || res.Accounts[0].ID != "acc_1" {
		t.Fatalf("expected acc_1 at 0, got %d", res.DestIndex)
	}

	// Move to absolute 1-based index "3"
	res, err = MoveAccount("acc_1", "3")
	if err != nil {
		t.Fatal(err)
	}
	if res.DestIndex != 2 || res.Accounts[2].ID != "acc_1" {
		t.Fatalf("expected acc_1 at index 2, got %+v", res)
	}

	// Move to same position
	res, err = MoveAccount("acc_1", "3")
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsSame {
		t.Fatalf("expected IsSame=true, got %+v", res)
	}

	// Out of range index
	_, err = MoveAccount("acc_1", "99")
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("expected out of range error, got %v", err)
	}

	// Invalid target
	_, err = MoveAccount("non_existent", "top")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got %v", err)
	}
}

func TestSwapAccounts(t *testing.T) {
	createTestOrderPool(t)

	// Swap acc_1 and acc_3
	res, err := SwapAccounts("acc_1", "acc_3")
	if err != nil {
		t.Fatal(err)
	}
	if res.Accounts[0].ID != "acc_3" || res.Accounts[2].ID != "acc_1" {
		t.Fatalf("swap failed: accounts=%+v", res.Accounts)
	}

	// Swap same account
	res, err = SwapAccounts("acc_1", "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsSame {
		t.Fatalf("expected IsSame=true")
	}

	// Unknown account
	_, err = SwapAccounts("acc_1", "unknown")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got %v", err)
	}
}

func TestSortAccounts(t *testing.T) {
	createTestOrderPool(t)

	// Sort by quota (default descending: acc_3 (95%), acc_1 (80%), acc_2 (50%), acc_10 (blocked: penalty -1))
	res, err := SortAccounts("quota", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accounts[0].ID != "acc_3" || res.Accounts[1].ID != "acc_1" || res.Accounts[2].ID != "acc_2" || res.Accounts[3].ID != "acc_10" {
		t.Fatalf("sort by quota failed: [%s, %s, %s, %s]",
			res.Accounts[0].ID, res.Accounts[1].ID, res.Accounts[2].ID, res.Accounts[3].ID)
	}

	// Reverse sort by quota
	res, err = SortAccounts("quota", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accounts[0].ID != "acc_10" || res.Accounts[3].ID != "acc_3" {
		t.Fatalf("reverse sort by quota failed: [%s, %s, %s, %s]",
			res.Accounts[0].ID, res.Accounts[1].ID, res.Accounts[2].ID, res.Accounts[3].ID)
	}

	// Sort by hits (descending: acc_2 (200), acc_1 (100), acc_3 (50), acc_10 (10))
	res, err = SortAccounts("hits", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accounts[0].ID != "acc_2" || res.Accounts[1].ID != "acc_1" || res.Accounts[2].ID != "acc_3" || res.Accounts[3].ID != "acc_10" {
		t.Fatalf("sort by hits failed: [%s, %s, %s, %s]",
			res.Accounts[0].ID, res.Accounts[1].ID, res.Accounts[2].ID, res.Accounts[3].ID)
	}

	// Sort by name (ascending: "Backup Node", "Dev Node", "Main CLI", "Work Node")
	res, err = SortAccounts("name", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accounts[0].ID != "acc_2" || res.Accounts[1].ID != "acc_10" || res.Accounts[2].ID != "acc_1" || res.Accounts[3].ID != "acc_3" {
		t.Fatalf("sort by name failed: [%s, %s, %s, %s]",
			res.Accounts[0].ID, res.Accounts[1].ID, res.Accounts[2].ID, res.Accounts[3].ID)
	}

	// Sort by id (natural ascending: acc_1, acc_2, acc_3, acc_10)
	res, err = SortAccounts("id", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accounts[0].ID != "acc_1" || res.Accounts[1].ID != "acc_2" || res.Accounts[2].ID != "acc_3" || res.Accounts[3].ID != "acc_10" {
		t.Fatalf("sort by id failed: [%s, %s, %s, %s]",
			res.Accounts[0].ID, res.Accounts[1].ID, res.Accounts[2].ID, res.Accounts[3].ID)
	}

	// Unknown criterion
	_, err = SortAccounts("invalid_crit", false, false)
	if err == nil || !strings.Contains(err.Error(), "unknown sort criterion") {
		t.Fatalf("expected unknown criterion error, got %v", err)
	}
}

func TestReorderAccounts(t *testing.T) {
	createTestOrderPool(t)

	// Permute all 4 accounts: 4, 3, 2, 1
	res, err := ReorderAccounts([]string{"4", "3", "2", "1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Accounts[0].ID != "acc_10" || res.Accounts[1].ID != "acc_3" || res.Accounts[2].ID != "acc_2" || res.Accounts[3].ID != "acc_1" {
		t.Fatalf("reorder failed: [%s, %s, %s, %s]",
			res.Accounts[0].ID, res.Accounts[1].ID, res.Accounts[2].ID, res.Accounts[3].ID)
	}

	// Count mismatch
	_, err = ReorderAccounts([]string{"1", "2"})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected count mismatch error, got %v", err)
	}

	// Duplicate account
	_, err = ReorderAccounts([]string{"1", "1", "2", "3"})
	if err == nil || !strings.Contains(err.Error(), "duplicate account") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestPrintAccountOrderPreview(t *testing.T) {
	p := createTestOrderPool(t)
	var buf bytes.Buffer
	PrintAccountOrderPreview(&buf, p.Accounts, []int{1}, p.ActiveAccountID, false)
	out := buf.String()
	if !strings.Contains(out, "Account Pool Order (4 accounts)") {
		t.Fatalf("missing header in preview: %s", out)
	}
	if !strings.Contains(out, "[* Active]") {
		t.Fatalf("missing active badge in preview: %s", out)
	}
	if !strings.Contains(out, "➜ [ 2]") {
		t.Fatalf("missing highlight indicator in preview: %s", out)
	}
}
