package accounts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func setupIsolatedTestDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	if err := config.ConfigureStateDir(tmp); err != nil {
		t.Fatalf("failed to configure state dir: %v", err)
	}
	return tmp
}

func TestDisplayAccountName(t *testing.T) {
	tests := []struct {
		name     string
		acc      *storage.Account
		expected string
	}{
		{
			name:     "nil account",
			acc:      nil,
			expected: "Account",
		},
		{
			name: "explicit friendly name",
			acc: &storage.Account{
				ID:    "acc_1",
				Email: "real_email@gmail.com",
				Name:  "Primary Dev",
			},
			expected: "Primary Dev",
		},
		{
			name: "acc_N suffix",
			acc: &storage.Account{
				ID:    "acc_3",
				Email: "sensitive@gmail.com",
			},
			expected: "Account 3",
		},
		{
			name: "non-standard id without name",
			acc: &storage.Account{
				ID:    "custom-id",
				Email: "sensitive@gmail.com",
			},
			expected: "Account",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DisplayAccountName(tt.acc)
			if got != tt.expected {
				t.Fatalf("DisplayAccountName() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestFindAccountByTarget(t *testing.T) {
	accounts := []*storage.Account{
		{ID: "acc_1", Email: "first@gmail.com", Name: "Alpha"},
		{ID: "acc_2", Email: "second@gmail.com", Name: "Beta"},
		{ID: "acc_3", Email: "third@gmail.com", Name: "Gamma"},
	}

	// 1-based index
	if got := FindAccountByTarget(accounts, "1"); got == nil || got.ID != "acc_1" {
		t.Fatalf("target '1' failed: %+v", got)
	}
	if got := FindAccountByTarget(accounts, "3"); got == nil || got.ID != "acc_3" {
		t.Fatalf("target '3' failed: %+v", got)
	}
	if got := FindAccountByTarget(accounts, "0"); got != nil {
		t.Fatalf("target '0' should be nil, got: %+v", got)
	}
	if got := FindAccountByTarget(accounts, "4"); got != nil {
		t.Fatalf("target '4' out of bounds should be nil, got: %+v", got)
	}

	// By ID
	if got := FindAccountByTarget(accounts, "acc_2"); got == nil || got.ID != "acc_2" {
		t.Fatalf("target 'acc_2' failed: %+v", got)
	}

	// By Email
	if got := FindAccountByTarget(accounts, "third@gmail.com"); got == nil || got.ID != "acc_3" {
		t.Fatalf("target email failed: %+v", got)
	}

	// By Name
	if got := FindAccountByTarget(accounts, "Beta"); got == nil || got.ID != "acc_2" {
		t.Fatalf("target name 'Beta' failed: %+v", got)
	}

	// Missing
	if got := FindAccountByTarget(accounts, "non-existent"); got != nil {
		t.Fatalf("expected nil for non-existent, got %+v", got)
	}
}

func TestWriteAndSyncAgyTokenFile(t *testing.T) {
	setupIsolatedTestDir(t)

	acc := &storage.Account{
		ID:           "acc_1",
		Email:        "user@gmail.com",
		AccessToken:  "at-123",
		RefreshToken: "rf-456",
		IDToken:      "id-tok-789",
	}

	if err := WriteAgyTokenFile(acc); err != nil {
		t.Fatalf("WriteAgyTokenFile failed: %v", err)
	}

	tokenFile := config.GetAgyTokenFile()
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatalf("failed to read written token file: %v", err)
	}

	var parsed struct {
		Token struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			TokenType    string `json:"token_type"`
			Expiry       string `json:"expiry"`
		} `json:"token"`
		AuthMethod string `json:"auth_method"`
		IDToken    string `json:"id_token"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to parse token file: %v", err)
	}
	if parsed.Token.AccessToken != "at-123" || parsed.Token.RefreshToken != "rf-456" {
		t.Fatalf("unexpected token content: %+v", parsed)
	}
	if parsed.AuthMethod != "consumer" {
		t.Fatalf("auth_method = %q, want 'consumer'", parsed.AuthMethod)
	}

	// Test SyncActiveAgyTokenFile
	pool := storage.NewEmptyPool()
	pool.Accounts = append(pool.Accounts, acc)
	pool.ActiveAccountID = &acc.ID
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("failed to save pool: %v", err)
	}

	synced, err := SyncActiveAgyTokenFile()
	if err != nil || !synced {
		t.Fatalf("SyncActiveAgyTokenFile failed: synced=%v, err=%v", synced, err)
	}
}

func TestRemoveAccount(t *testing.T) {
	setupIsolatedTestDir(t)

	pool := storage.NewEmptyPool()
	acc1 := &storage.Account{ID: "acc_1", Email: "1@gmail.com"}
	acc2 := &storage.Account{ID: "acc_2", Email: "2@gmail.com"}
	pool.Accounts = []*storage.Account{acc1, acc2}
	pool.ActiveAccountID = &acc1.ID
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("failed to save pool: %v", err)
	}

	removed, err := RemoveAccount("acc_1")
	if err != nil {
		t.Fatalf("RemoveAccount failed: %v", err)
	}
	if removed.ID != "acc_1" {
		t.Fatalf("got removed ID %q, want 'acc_1'", removed.ID)
	}

	loaded, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("failed to load pool: %v", err)
	}
	if len(loaded.Accounts) != 1 || loaded.Accounts[0].ID != "acc_2" {
		t.Fatalf("remaining accounts mismatch: %+v", loaded.Accounts)
	}
	// Active account should have transferred to acc_2
	if loaded.ActiveAccountID == nil || *loaded.ActiveAccountID != "acc_2" {
		t.Fatalf("active account did not update to acc_2: %v", loaded.ActiveAccountID)
	}
}

func TestSwitchAccount(t *testing.T) {
	setupIsolatedTestDir(t)

	frac1 := 0.2
	frac2 := 0.8
	pool := storage.NewEmptyPool()
	acc1 := &storage.Account{
		ID:        "acc_1",
		Email:     "1@gmail.com",
		LastQuota: &storage.QuotaState{RemainingFraction: &frac1},
	}
	acc2 := &storage.Account{
		ID:        "acc_2",
		Email:     "2@gmail.com",
		LastQuota: &storage.QuotaState{RemainingFraction: &frac2},
	}
	pool.Accounts = []*storage.Account{acc1, acc2}
	pool.ActiveAccountID = &acc1.ID
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("failed to save pool: %v", err)
	}

	// Explicit switch
	selected, err := SwitchAccount("acc_2")
	if err != nil {
		t.Fatalf("SwitchAccount explicit failed: %v", err)
	}
	if selected.ID != "acc_2" {
		t.Fatalf("selected = %q, want 'acc_2'", selected.ID)
	}

	// Auto switch (should pick acc_2 because frac2=0.8 > frac1=0.2)
	pool.ActiveAccountID = &acc1.ID
	_ = storage.SavePool(pool)

	autoSelected, err := SwitchAccount("auto")
	if err != nil {
		t.Fatalf("SwitchAccount auto failed: %v", err)
	}
	if autoSelected.ID != "acc_2" {
		t.Fatalf("autoSelected = %q, want 'acc_2'", autoSelected.ID)
	}
}

func TestRenameAccount(t *testing.T) {
	setupIsolatedTestDir(t)

	pool := storage.NewEmptyPool()
	acc := &storage.Account{ID: "acc_1", Email: "test@gmail.com"}
	pool.Accounts = []*storage.Account{acc}
	_ = storage.SavePool(pool)

	// Empty name rejected
	if _, err := RenameAccount("acc_1", "   "); err == nil {
		t.Fatal("expected error on empty name")
	}

	renamed, err := RenameAccount("acc_1", "New Friendly Name")
	if err != nil {
		t.Fatalf("RenameAccount failed: %v", err)
	}
	if renamed.Name != "New Friendly Name" {
		t.Fatalf("got Name %q, want 'New Friendly Name'", renamed.Name)
	}

	loaded, _ := storage.LoadPool()
	if loaded.Accounts[0].Name != "New Friendly Name" {
		t.Fatalf("loaded Name = %q, want 'New Friendly Name'", loaded.Accounts[0].Name)
	}
}

func TestImportCurrent(t *testing.T) {
	setupIsolatedTestDir(t)

	// No file initially
	if _, err := ImportCurrent(); err == nil {
		t.Fatal("expected error when token file missing")
	}

	// Write mock agy token file
	tokenDir := config.GetAgyCliDir()
	_ = os.MkdirAll(tokenDir, 0o700)
	tokenPath := config.GetAgyTokenFile()

	// JWT with email="primary@domain.com"
	// header: eyJhbGciOiJub25lIn0
	// payload: eyJlbWFpbCI6ICJwcmltYXJ5QGRvbWFpbi5jb20ifQ
	mockJWT := "eyJhbGciOiJub25lIn0.eyJlbWFpbCI6ICJwcmltYXJ5QGRvbWFpbi5jb20ifQ."
	tokenPayload := map[string]any{
		"token": map[string]any{
			"access_token":  "mock-at-123",
			"refresh_token": "mock-rf-456",
		},
		"id_token": mockJWT,
	}
	data, _ := json.Marshal(tokenPayload)
	_ = os.WriteFile(tokenPath, data, 0o600)

	imported, err := ImportCurrent()
	if err != nil {
		t.Fatalf("ImportCurrent failed: %v", err)
	}
	if imported.Email != "primary@domain.com" {
		t.Fatalf("imported email = %q, want 'primary@domain.com'", imported.Email)
	}
	if imported.RefreshToken != "mock-rf-456" {
		t.Fatalf("imported refresh_token = %q, want 'mock-rf-456'", imported.RefreshToken)
	}

	loaded, _ := storage.LoadPool()
	if len(loaded.Accounts) != 1 || loaded.Accounts[0].Email != "primary@domain.com" {
		t.Fatalf("pool not updated with imported account: %+v", loaded.Accounts)
	}
}

func TestLoginFlow(t *testing.T) {
	setupIsolatedTestDir(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("code") != "valid-test-code" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mockJWT := "eyJhbGciOiJub25lIn0.eyJlbWFpbCI6ICJsb2dpbl91c2VyQGdtYWlsLmNvbSJ9."
		resp := map[string]any{
			"access_token":  "login-at-123",
			"refresh_token": "login-rf-456",
			"id_token":      mockJWT,
			"expires_in":    3600,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	opts := LoginOptions{
		TokenEndpoint: server.URL,
		BrowserOpener: func(url string) error { return nil },
		PromptFn: func(authURL string) (string, error) {
			return "valid-test-code", nil
		},
		Timeout:   5 * time.Second,
		PortRange: [2]int{18085, 18135},
	}

	acc, err := Login(context.Background(), opts)
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}
	if acc.Email != "login_user@gmail.com" {
		t.Fatalf("acc email = %q, want 'login_user@gmail.com'", acc.Email)
	}
	if acc.AccessToken != "login-at-123" {
		t.Fatalf("acc access_token = %q, want 'login-at-123'", acc.AccessToken)
	}
}

func TestVerifyAccount(t *testing.T) {
	setupIsolatedTestDir(t)

	pool := storage.NewEmptyPool()
	vURL := "https://accounts.google.com/verify?id=xyz"
	acc1 := &storage.Account{
		ID:            "acc_1",
		Email:         "verify_me@gmail.com",
		Status:        "validation_required",
		ValidationURL: &vURL,
	}
	acc2 := &storage.Account{
		ID:     "acc_2",
		Email:  "good@gmail.com",
		Status: "",
	}
	pool.Accounts = []*storage.Account{acc1, acc2}
	_ = storage.SavePool(pool)

	// Acc 1 requires validation
	acc, formattedURL, err := VerifyAccount("acc_1")
	if err != nil {
		t.Fatalf("VerifyAccount acc_1 failed: %v", err)
	}
	if acc.ID != "acc_1" {
		t.Fatalf("got acc %q, want 'acc_1'", acc.ID)
	}
	if formattedURL != "https://accounts.google.com/verify?id=xyz&Email=verify_me%40gmail.com" {
		t.Fatalf("formattedURL = %q, want email query parameter appended", formattedURL)
	}

	// Acc 2 is already verified
	accGood, goodURL, err := VerifyAccount("acc_2")
	if err != nil {
		t.Fatalf("VerifyAccount acc_2 failed: %v", err)
	}
	if goodURL != "" {
		t.Fatalf("expected empty url for verified account, got %q", goodURL)
	}
	if accGood.ID != "acc_2" {
		t.Fatalf("got acc %q, want 'acc_2'", accGood.ID)
	}
}
