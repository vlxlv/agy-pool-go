package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func setupIsolatedTestDir(t *testing.T) string {
	t.Helper()
	config.SetTestMode(true)
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	nativeDir := filepath.Join(tmp, "native_gemini")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("failed to create data dir: %v", err)
	}
	if err := os.MkdirAll(nativeDir, 0o700); err != nil {
		t.Fatalf("failed to create native gemini dir: %v", err)
	}

	if err := config.ConfigureStateDir(dataDir); err != nil {
		t.Fatalf("failed to configure state dir: %v", err)
	}
	config.SetNativeAgyDir(nativeDir)
	t.Cleanup(func() {
		config.SetNativeAgyDir("")
		config.ResetDataDir()
	})
	return dataDir
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

	realProdToken := filepath.Join(config.GetRealProductionGeminiDir(), "antigravity-cli", "antigravity-oauth-token")
	var realProdMtime time.Time
	if fi, err := os.Stat(realProdToken); err == nil {
		realProdMtime = fi.ModTime()
	}

	// 1. WriteAgyTokenFile writes only to synthetic token path
	if err := WriteAgyTokenFile(acc); err != nil {
		t.Fatalf("WriteAgyTokenFile failed: %v", err)
	}

	tokenFile := config.GetAgyTokenFile()
	if tokenFile == realProdToken {
		t.Fatalf("CRITICAL: tokenFile resolved to real production token path: %s", tokenFile)
	}

	// 2. File mode is user-private (0600)
	fi, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatalf("failed to stat written token file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %o, want 0600", fi.Mode().Perm())
	}

	// 3. Content matches expected native-agy compatibility schema
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

	// 4. SyncActiveAgyTokenFile updates the synthetic path
	acc.AccessToken = "synced-at-new"
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

	updatedData, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatalf("failed to read updated token file: %v", err)
	}
	if err := json.Unmarshal(updatedData, &parsed); err != nil {
		t.Fatalf("failed to parse updated token file: %v", err)
	}
	if parsed.Token.AccessToken != "synced-at-new" {
		t.Fatalf("expected updated access token 'synced-at-new', got %q", parsed.Token.AccessToken)
	}

	// 5. Real production token path is unchanged
	if !realProdMtime.IsZero() {
		if postFi, err := os.Stat(realProdToken); err == nil {
			if postFi.ModTime() != realProdMtime {
				t.Fatalf("CRITICAL: real production token file mtime changed during test!")
			}
		}
	}
}

func TestSyncActiveAgyTokenFile_RefreshFailureLeavesFileUnchanged(t *testing.T) {
	setupIsolatedTestDir(t)
	oldRefresh, oldWriter := tokenRefresher, tokenFileWriter
	t.Cleanup(func() { tokenRefresher, tokenFileWriter = oldRefresh, oldWriter })
	expired := float64(time.Now().Unix() - 60)
	stale := &storage.Account{ID: "acc_1", AccessToken: "access-secret", RefreshToken: "refresh-secret", TokenExpiry: &expired}
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{stale}
	pool.ActiveAccountID = &stale.ID
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgyTokenFile(stale); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(config.GetAgyTokenFile())
	if err != nil {
		t.Fatal(err)
	}
	tokenRefresher = func(*storage.Account, ...bool) (string, error) {
		return "", errors.New("token refresh failed (HTTP 400)")
	}
	tokenFileWriter = func(*storage.Account) error { t.Fatal("writer called after refresh failure"); return nil }
	_, err = SyncActiveAgyTokenFile()
	if err == nil || strings.Contains(err.Error(), "access-secret") || strings.Contains(err.Error(), "refresh-secret") {
		t.Fatalf("unexpected refresh error: %v", err)
	}
	after, err := os.ReadFile(config.GetAgyTokenFile())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("native token file changed after refresh failure")
	}
}

func TestSyncActiveAgyTokenFile_WriterFailure(t *testing.T) {
	setupIsolatedTestDir(t)
	oldRefresh, oldWriter := tokenRefresher, tokenFileWriter
	t.Cleanup(func() { tokenRefresher, tokenFileWriter = oldRefresh, oldWriter })
	acc := &storage.Account{ID: "acc_1", AccessToken: "access-secret"}
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{acc}
	pool.ActiveAccountID = &acc.ID
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}
	tokenFileWriter = func(*storage.Account) error { return errors.New("injected writer failure") }
	synced, err := SyncActiveAgyTokenFile()
	if synced || err == nil {
		t.Fatalf("synced=%v err=%v, want failure", synced, err)
	}
}

func TestSwitchAccount_RefreshFailureKeepsPreviousActive(t *testing.T) {
	setupIsolatedTestDir(t)
	oldRefresh, oldWriter := tokenRefresher, tokenFileWriter
	t.Cleanup(func() { tokenRefresher, tokenFileWriter = oldRefresh, oldWriter })
	first := &storage.Account{ID: "acc_1", AccessToken: "first-access"}
	secondExpiry := float64(time.Now().Unix() - 60)
	second := &storage.Account{ID: "acc_2", AccessToken: "", RefreshToken: "refresh-secret", TokenExpiry: &secondExpiry}
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{first, second}
	pool.ActiveAccountID = &first.ID
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgyTokenFile(first); err != nil {
		t.Fatal(err)
	}
	tokenRefresher = func(*storage.Account, ...bool) (string, error) {
		return "", errors.New("token refresh failed (HTTP 400)")
	}
	_, err := SwitchAccount("acc_2")
	if err == nil || strings.Contains(err.Error(), "access-secret") || strings.Contains(err.Error(), "refresh-secret") {
		t.Fatalf("unexpected switch error: %v", err)
	}
	loaded, loadErr := storage.LoadPool()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if loaded.ActiveAccountID == nil || *loaded.ActiveAccountID != "acc_1" {
		t.Fatalf("active account changed: %v", loaded.ActiveAccountID)
	}
	data, readErr := os.ReadFile(config.GetAgyTokenFile())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if bytes.Contains(data, []byte("second")) || !bytes.Contains(data, []byte("first-access")) {
		t.Fatalf("native token changed: %s", data)
	}
}

func TestSwitchAccount_WriterFailureKeepsPreviousActive(t *testing.T) {
	setupIsolatedTestDir(t)
	oldRefresh, oldWriter := tokenRefresher, tokenFileWriter
	t.Cleanup(func() { tokenRefresher, tokenFileWriter = oldRefresh, oldWriter })
	first := &storage.Account{ID: "acc_1", AccessToken: "first-access"}
	second := &storage.Account{ID: "acc_2", AccessToken: "second-access"}
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{first, second}
	pool.ActiveAccountID = &first.ID
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgyTokenFile(first); err != nil {
		t.Fatal(err)
	}
	tokenFileWriter = func(*storage.Account) error { return errors.New("injected writer failure") }
	if _, err := SwitchAccount("acc_2"); err == nil {
		t.Fatal("expected switch failure")
	}
	loaded, err := storage.LoadPool()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ActiveAccountID == nil || *loaded.ActiveAccountID != "acc_1" {
		t.Fatalf("active account changed: %v", loaded.ActiveAccountID)
	}
	data, err := os.ReadFile(config.GetAgyTokenFile())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("first-access")) || bytes.Contains(data, []byte("second-access")) {
		t.Fatalf("native token changed: %s", data)
	}
}

func TestRemoveAccount_ReplacementSyncFailureKeepsPool(t *testing.T) {
	setupIsolatedTestDir(t)
	oldRefresh, oldWriter := tokenRefresher, tokenFileWriter
	t.Cleanup(func() { tokenRefresher, tokenFileWriter = oldRefresh, oldWriter })
	first := &storage.Account{ID: "acc_1", AccessToken: "first-access"}
	second := &storage.Account{ID: "acc_2", AccessToken: ""}
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{first, second}
	pool.ActiveAccountID = &first.ID
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}
	tokenRefresher = func(*storage.Account, ...bool) (string, error) { return "", errors.New("refresh failed") }
	if _, err := RemoveAccount("acc_1"); err == nil {
		t.Fatal("expected replacement sync failure")
	}
	loaded, err := storage.LoadPool()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Accounts) != 2 || loaded.ActiveAccountID == nil || *loaded.ActiveAccountID != "acc_1" {
		t.Fatalf("pool changed: %+v", loaded)
	}
}

func TestRemoveAccount(t *testing.T) {
	setupIsolatedTestDir(t)

	pool := storage.NewEmptyPool()
	acc1 := &storage.Account{ID: "acc_1", Email: "1@gmail.com", AccessToken: "token-acc-1"}
	acc2 := &storage.Account{ID: "acc_2", Email: "2@gmail.com", AccessToken: "token-acc-2"}
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
		ID:          "acc_1",
		Email:       "1@gmail.com",
		AccessToken: "token-acc-1",
		LastQuota:   &storage.QuotaState{RemainingFraction: &frac1},
	}
	acc2 := &storage.Account{
		ID:          "acc_2",
		Email:       "2@gmail.com",
		AccessToken: "token-acc-2",
		LastQuota:   &storage.QuotaState{RemainingFraction: &frac2},
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

func TestDisableAndEnableAccount(t *testing.T) {
	setupIsolatedTestDir(t)

	pool := storage.NewEmptyPool()
	acc := &storage.Account{ID: "acc_1", Email: "test@gmail.com"}
	pool.Accounts = []*storage.Account{acc}
	_ = storage.SavePool(pool)

	// Non-existent target
	if _, err := DisableAccount("acc_999"); err == nil {
		t.Fatal("expected error on non-existent account")
	}

	// Disable
	disabled, err := DisableAccount("acc_1")
	if err != nil {
		t.Fatalf("DisableAccount failed: %v", err)
	}
	if disabled.Status != storage.StatusDisabled {
		t.Fatalf("got status %q, want %q", disabled.Status, storage.StatusDisabled)
	}

	loaded, _ := storage.LoadPool()
	if loaded.Accounts[0].Status != storage.StatusDisabled {
		t.Fatalf("loaded status = %q, want %q", loaded.Accounts[0].Status, storage.StatusDisabled)
	}

	// Enable
	enabled, err := EnableAccount("acc_1")
	if err != nil {
		t.Fatalf("EnableAccount failed: %v", err)
	}
	if enabled.Status != "" {
		t.Fatalf("got status %q, want empty", enabled.Status)
	}

	loaded, _ = storage.LoadPool()
	if loaded.Accounts[0].Status != "" {
		t.Fatalf("loaded status after enable = %q, want empty", loaded.Accounts[0].Status)
	}
}

func TestImportCurrent(t *testing.T) {
	setupIsolatedTestDir(t)

	// Case A: synthetic token file absent => ImportCurrent returns expected missing-token result
	if _, err := ImportCurrent(); err == nil || !strings.Contains(err.Error(), "no existing token file found") {
		t.Fatalf("Case A: expected missing token file error, got: %v", err)
	}

	tokenDir := config.GetAgyCliDir()
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatalf("failed to create token dir: %v", err)
	}
	tokenPath := config.GetAgyTokenFile()

	// Case C: synthetic token exists but lacks refresh_token => expected failure
	invalidPayload := map[string]any{
		"token": map[string]any{
			"access_token": "synthetic-at-no-refresh",
		},
		"auth_method": "consumer",
	}
	invalidData, _ := json.Marshal(invalidPayload)
	if err := os.WriteFile(tokenPath, invalidData, 0o600); err != nil {
		t.Fatalf("failed to write invalid token file: %v", err)
	}
	if _, err := ImportCurrent(); err == nil || !strings.Contains(err.Error(), "no refresh_token found") {
		t.Fatalf("Case C: expected no refresh_token error, got: %v", err)
	}

	// Case B: synthetic token exists with valid synthetic data => import succeeds using only that fixture
	mockJWT := "eyJhbGciOiJub25lIn0.eyJlbWFpbCI6ICJzeW50aGV0aWNfcHJpbWFyeUBleGFtcGxlLmNvbSJ9."
	validPayload := map[string]any{
		"token": map[string]any{
			"access_token":  "synthetic-at-123",
			"refresh_token": "synthetic-rf-456",
		},
		"auth_method": "consumer",
		"id_token":    mockJWT,
	}
	validData, _ := json.Marshal(validPayload)
	if err := os.WriteFile(tokenPath, validData, 0o600); err != nil {
		t.Fatalf("failed to write valid token file: %v", err)
	}

	imported, err := ImportCurrent()
	if err != nil {
		t.Fatalf("Case B: ImportCurrent failed: %v", err)
	}
	if imported.Email != "synthetic_primary@example.com" {
		t.Fatalf("Case B: imported email = %q, want 'synthetic_primary@example.com'", imported.Email)
	}
	if imported.RefreshToken != "synthetic-rf-456" {
		t.Fatalf("Case B: imported refresh_token = %q, want 'synthetic-rf-456'", imported.RefreshToken)
	}

	loaded, err := storage.LoadPool()
	if err != nil || len(loaded.Accounts) != 1 || loaded.Accounts[0].Email != "synthetic_primary@example.com" {
		t.Fatalf("pool not updated with imported account: %+v", loaded)
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

func TestLogin_NativeSyncFailureIsObservable(t *testing.T) {
	setupIsolatedTestDir(t)
	oldWriter := tokenFileWriter
	t.Cleanup(func() { tokenFileWriter = oldWriter })
	tokenFileWriter = func(*storage.Account) error { return errors.New("injected native writer failure") }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-secret", "refresh_token": "refresh-secret",
			"id_token": "", "expires_in": 3600,
		})
	}))
	defer server.Close()
	acc, err := Login(context.Background(), LoginOptions{
		TokenEndpoint: server.URL,
		BrowserOpener: func(string) error { return nil },
		PromptFn:      func(string) (string, error) { return "code", nil },
		Timeout:       2 * time.Second, PortRange: [2]int{18600, 18650},
	})
	if err == nil || acc == nil || !strings.Contains(err.Error(), "native agy synchronization failed") {
		t.Fatalf("account=%v err=%v, want degraded login error", acc, err)
	}
	if strings.Contains(err.Error(), "access-secret") || strings.Contains(err.Error(), "refresh-secret") || strings.Contains(err.Error(), "@") {
		t.Fatalf("login error leaked sensitive data: %v", err)
	}
}

func TestVerifyAccount_ClearsValidationRequiredOnSuccess(t *testing.T) {
	setupIsolatedTestDir(t)
	fraction := 0.8
	SetQuotaProber(func(account *storage.Account) (*storage.QuotaState, error) {
		return &storage.QuotaState{RemainingFraction: &fraction}, nil
	}, nil)
	t.Cleanup(func() { SetQuotaProber(nil, nil) })

	pool := storage.NewEmptyPool()
	vURL := "https://accounts.google.com/verify?id=xyz"
	cooldown := float64(time.Now().Unix() + 86400)
	acc1 := &storage.Account{
		ID:               "acc_1",
		Email:            "verify_me@gmail.com",
		Status:           "validation_required",
		ValidationURL:    &vURL,
		RateLimitedUntil: &cooldown,
	}
	acc2 := &storage.Account{
		ID:     "acc_2",
		Email:  "good@gmail.com",
		Status: "",
	}
	pool.Accounts = []*storage.Account{acc1, acc2}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	acc, formattedURL, err := VerifyAccount("acc_1")
	if err != nil {
		t.Fatalf("VerifyAccount acc_1 failed: %v", err)
	}
	if formattedURL != "" {
		t.Fatalf("expected empty formattedURL, got %q", formattedURL)
	}
	if acc.Status != "" {
		t.Fatalf("expected acc.Status to be empty (Ready), got %q", acc.Status)
	}
	if acc.ValidationURL != nil {
		t.Fatalf("expected acc.ValidationURL to be nil, got %v", acc.ValidationURL)
	}
	if acc.RateLimitedUntil != nil {
		t.Fatalf("expected acc.RateLimitedUntil to be nil, got %v", acc.RateLimitedUntil)
	}

	// Verify authoritative persisted state in storage
	storedPool, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	stored := storage.FindAccount(storedPool, acc1)
	if stored == nil {
		t.Fatal("stored account not found")
	}
	if stored.Status != "" {
		t.Fatalf("persisted Status = %q, want empty (Ready)", stored.Status)
	}
	if stored.ValidationURL != nil {
		t.Fatalf("persisted ValidationURL = %v, want nil", stored.ValidationURL)
	}
	if stored.RateLimitedUntil != nil {
		t.Fatalf("persisted RateLimitedUntil = %v, want nil", stored.RateLimitedUntil)
	}
	if stored.LastQuota == nil || stored.LastQuota.RemainingFraction == nil || *stored.LastQuota.RemainingFraction != 0.8 {
		t.Fatalf("persisted LastQuota unexpected: %+v", stored.LastQuota)
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

func TestVerifyAccount_DoesNotClearAuthError(t *testing.T) {
	setupIsolatedTestDir(t)
	fraction := 0.8
	SetQuotaProber(func(account *storage.Account) (*storage.QuotaState, error) {
		return &storage.QuotaState{RemainingFraction: &fraction}, nil
	}, nil)
	t.Cleanup(func() { SetQuotaProber(nil, nil) })

	pool := storage.NewEmptyPool()
	acc := &storage.Account{
		ID:     "acc_auth_err",
		Email:  "auth_err@gmail.com",
		Status: "auth_error",
	}
	pool.Accounts = []*storage.Account{acc}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	resAcc, formattedURL, err := VerifyAccount("acc_auth_err")
	if err != nil {
		t.Fatalf("VerifyAccount failed: %v", err)
	}
	if formattedURL != "" {
		t.Fatalf("expected empty formattedURL, got %q", formattedURL)
	}
	if resAcc.Status != "auth_error" {
		t.Fatalf("expected resAcc.Status to remain auth_error, got %q", resAcc.Status)
	}

	storedPool, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	stored := storage.FindAccount(storedPool, acc)
	if stored == nil {
		t.Fatal("stored account not found")
	}
	if stored.Status != "auth_error" {
		t.Fatalf("persisted Status = %q, want auth_error", stored.Status)
	}
}

func TestVerifyAccount_PreservesRestrictionWhenValidationStillRequired(t *testing.T) {
	setupIsolatedTestDir(t)
	challengeURL := "https://accounts.google.com/verify?id=fresh_challenge"
	SetQuotaProber(func(account *storage.Account) (*storage.QuotaState, error) {
		account.Status = "validation_required"
		account.ValidationURL = &challengeURL
		fraction := 0.0
		return &storage.QuotaState{RemainingFraction: &fraction}, nil
	}, nil)
	t.Cleanup(func() { SetQuotaProber(nil, nil) })

	pool := storage.NewEmptyPool()
	oldURL := "https://accounts.google.com/verify?id=old"
	cooldown := float64(time.Now().Unix() + 86400)
	acc := &storage.Account{
		ID:               "acc_val_req",
		Email:            "val_req@gmail.com",
		Status:           "validation_required",
		ValidationURL:    &oldURL,
		RateLimitedUntil: &cooldown,
	}
	pool.Accounts = []*storage.Account{acc}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	resAcc, formattedURL, err := VerifyAccount("acc_val_req")
	if err != nil {
		t.Fatalf("VerifyAccount failed: %v", err)
	}
	if !strings.Contains(formattedURL, "fresh_challenge") {
		t.Fatalf("expected formattedURL containing fresh_challenge, got %q", formattedURL)
	}
	if !strings.Contains(formattedURL, "Email=val_req%40gmail.com") {
		t.Fatalf("expected email appended to URL, got %q", formattedURL)
	}
	if resAcc.Status != "validation_required" {
		t.Fatalf("expected Status to remain validation_required, got %q", resAcc.Status)
	}

	storedPool, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	stored := storage.FindAccount(storedPool, acc)
	if stored == nil {
		t.Fatal("stored account not found")
	}
	if stored.Status != "validation_required" {
		t.Fatalf("persisted Status = %q, want validation_required", stored.Status)
	}
	if stored.ValidationURL == nil || *stored.ValidationURL != challengeURL {
		t.Fatalf("persisted ValidationURL = %v, want %q", stored.ValidationURL, challengeURL)
	}
}

func TestVerifyAccount_PreservesRestrictionOnProbeError(t *testing.T) {
	setupIsolatedTestDir(t)
	SetQuotaProber(func(account *storage.Account) (*storage.QuotaState, error) {
		return nil, errors.New("temporary network timeout")
	}, nil)
	t.Cleanup(func() { SetQuotaProber(nil, nil) })

	pool := storage.NewEmptyPool()
	vURL := "https://accounts.google.com/verify?id=existing_url"
	cooldown := float64(time.Now().Unix() + 86400)
	acc := &storage.Account{
		ID:               "acc_err",
		Email:            "err_acc@gmail.com",
		Status:           "validation_required",
		ValidationURL:    &vURL,
		RateLimitedUntil: &cooldown,
	}
	pool.Accounts = []*storage.Account{acc}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	resAcc, formattedURL, err := VerifyAccount("acc_err")
	if err != nil {
		t.Fatalf("VerifyAccount failed: %v", err)
	}
	if !strings.Contains(formattedURL, "existing_url") {
		t.Fatalf("expected formattedURL containing existing_url, got %q", formattedURL)
	}
	if resAcc.Status != "validation_required" {
		t.Fatalf("expected Status to remain validation_required, got %q", resAcc.Status)
	}

	storedPool, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	stored := storage.FindAccount(storedPool, acc)
	if stored == nil {
		t.Fatal("stored account not found")
	}
	if stored.Status != "validation_required" {
		t.Fatalf("persisted Status = %q, want validation_required", stored.Status)
	}
	if stored.ValidationURL == nil || *stored.ValidationURL != vURL {
		t.Fatalf("persisted ValidationURL = %v, want %q", stored.ValidationURL, vURL)
	}
	if stored.RateLimitedUntil == nil || *stored.RateLimitedUntil != cooldown {
		t.Fatalf("persisted RateLimitedUntil = %v, want %v", stored.RateLimitedUntil, cooldown)
	}
}

func TestVerifyAccount_PersistenceFailureReturnsError(t *testing.T) {
	setupIsolatedTestDir(t)
	fraction := 0.8
	SetQuotaProber(func(account *storage.Account) (*storage.QuotaState, error) {
		return &storage.QuotaState{RemainingFraction: &fraction}, nil
	}, nil)
	t.Cleanup(func() { SetQuotaProber(nil, nil) })

	SetAccountFieldsPersister(func(account *storage.Account, updateTokens, updateStatus, updateQuota bool) error {
		return errors.New("simulated disk write failure")
	})
	t.Cleanup(func() { SetAccountFieldsPersister(nil) })

	pool := storage.NewEmptyPool()
	vURL := "https://accounts.google.com/verify?id=xyz"
	acc := &storage.Account{
		ID:            "acc_fail",
		Email:         "fail@gmail.com",
		Status:        "validation_required",
		ValidationURL: &vURL,
	}
	pool.Accounts = []*storage.Account{acc}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	resAcc, formattedURL, err := VerifyAccount("acc_fail")
	if err == nil {
		t.Fatal("expected error on persistence failure, got nil")
	}
	if !strings.Contains(err.Error(), "simulated disk write failure") {
		t.Fatalf("unexpected error message: %v", err)
	}
	if formattedURL != "" {
		t.Fatalf("expected empty formattedURL on error, got %q", formattedURL)
	}
	if resAcc != nil {
		t.Fatalf("expected nil account on persistence error, got %+v", resAcc)
	}
}

func TestVerifyAccount_DoesNotClearNewerValidationRestriction(t *testing.T) {
	setupIsolatedTestDir(t)
	fraction := 0.8
	SetQuotaProber(func(account *storage.Account) (*storage.QuotaState, error) {
		return &storage.QuotaState{RemainingFraction: &fraction}, nil
	}, nil)
	t.Cleanup(func() { SetQuotaProber(nil, nil) })

	urlA := "https://accounts.google.com/verify?id=val_A"
	cooldownA := float64(time.Now().Unix() + 86400)
	acc := &storage.Account{
		ID:               "acc_race_1",
		Email:            "race1@gmail.com",
		AccessToken:      "token_gen_A",
		RefreshToken:     "refresh_gen_A",
		Status:           "validation_required",
		ValidationURL:    &urlA,
		RateLimitedUntil: &cooldownA,
	}
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{acc}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	beforeClearReached := make(chan struct{})
	resumeClear := make(chan struct{})
	SetBeforeClearHook(func(id string) {
		close(beforeClearReached)
		<-resumeClear
	})
	t.Cleanup(func() { SetBeforeClearHook(nil) })

	type verifyResult struct {
		acc          *storage.Account
		formattedURL string
		err          error
	}
	resultChan := make(chan verifyResult, 1)

	go func() {
		resAcc, fURL, err := VerifyAccount("acc_race_1")
		resultChan <- verifyResult{acc: resAcc, formattedURL: fURL, err: err}
	}()

	// Wait until VerifyAccount finishes probe and pauses before clear transaction
	<-beforeClearReached

	// Concurrent writer stores validation B
	urlB := "https://accounts.google.com/verify?id=val_B"
	cooldownB := float64(time.Now().Unix() + 99999)
	err := storage.PoolTransaction(func(p *storage.Pool) error {
		stored := storage.FindAccount(p, acc)
		if stored == nil {
			return errors.New("account not found")
		}
		stored.Status = "validation_required"
		stored.ValidationURL = &urlB
		stored.RateLimitedUntil = &cooldownB
		return nil
	})
	if err != nil {
		t.Fatalf("concurrent writer failed: %v", err)
	}

	// Resume clear transaction in VerifyAccount
	close(resumeClear)

	res := <-resultChan
	if res.err != nil {
		t.Fatalf("VerifyAccount returned unexpected error: %v", res.err)
	}

	// Verify that newer validation B survived in memory return
	if !strings.Contains(res.formattedURL, "val_B") {
		t.Fatalf("expected formattedURL containing val_B, got: %q", res.formattedURL)
	}
	if res.acc == nil || res.acc.Status != "validation_required" {
		t.Fatalf("expected account Status to remain validation_required, got %+v", res.acc)
	}

	// Verify that newer validation B survived in authoritative storage
	storedPool, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	stored := storage.FindAccount(storedPool, acc)
	if stored == nil {
		t.Fatal("stored account not found")
	}
	if stored.Status != "validation_required" {
		t.Fatalf("persisted Status = %q, want validation_required", stored.Status)
	}
	if stored.ValidationURL == nil || *stored.ValidationURL != urlB {
		t.Fatalf("persisted ValidationURL = %v, want %q", stored.ValidationURL, urlB)
	}
	if stored.RateLimitedUntil == nil || *stored.RateLimitedUntil != cooldownB {
		t.Fatalf("persisted RateLimitedUntil = %v, want %v", stored.RateLimitedUntil, cooldownB)
	}
}

func TestVerifyAccount_DoesNotClearConcurrentAuthError(t *testing.T) {
	setupIsolatedTestDir(t)
	fraction := 0.8
	SetQuotaProber(func(account *storage.Account) (*storage.QuotaState, error) {
		return &storage.QuotaState{RemainingFraction: &fraction}, nil
	}, nil)
	t.Cleanup(func() { SetQuotaProber(nil, nil) })

	urlA := "https://accounts.google.com/verify?id=val_A"
	cooldownA := float64(time.Now().Unix() + 86400)
	acc := &storage.Account{
		ID:               "acc_race_2",
		Email:            "race2@gmail.com",
		AccessToken:      "token_gen_A",
		RefreshToken:     "refresh_gen_A",
		Status:           "validation_required",
		ValidationURL:    &urlA,
		RateLimitedUntil: &cooldownA,
	}
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{acc}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	beforeClearReached := make(chan struct{})
	resumeClear := make(chan struct{})
	SetBeforeClearHook(func(id string) {
		close(beforeClearReached)
		<-resumeClear
	})
	t.Cleanup(func() { SetBeforeClearHook(nil) })

	type verifyResult struct {
		acc          *storage.Account
		formattedURL string
		err          error
	}
	resultChan := make(chan verifyResult, 1)

	go func() {
		resAcc, fURL, err := VerifyAccount("acc_race_2")
		resultChan <- verifyResult{acc: resAcc, formattedURL: fURL, err: err}
	}()

	// Wait until VerifyAccount finishes probe and pauses before clear transaction
	<-beforeClearReached

	// Concurrent writer stores auth_error
	err := storage.PoolTransaction(func(p *storage.Pool) error {
		stored := storage.FindAccount(p, acc)
		if stored == nil {
			return errors.New("account not found")
		}
		stored.Status = "auth_error"
		return nil
	})
	if err != nil {
		t.Fatalf("concurrent writer failed: %v", err)
	}

	// Resume clear transaction in VerifyAccount
	close(resumeClear)

	res := <-resultChan
	if res.err != nil {
		t.Fatalf("VerifyAccount returned unexpected error: %v", res.err)
	}

	// Verify that auth_error survived in authoritative storage
	storedPool, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	stored := storage.FindAccount(storedPool, acc)
	if stored == nil {
		t.Fatal("stored account not found")
	}
	if stored.Status != "auth_error" {
		t.Fatalf("persisted Status = %q, want auth_error", stored.Status)
	}
}

func TestVerifyAccount_DoesNotClearAfterCredentialGenerationChange(t *testing.T) {
	setupIsolatedTestDir(t)
	fraction := 0.8
	SetQuotaProber(func(account *storage.Account) (*storage.QuotaState, error) {
		return &storage.QuotaState{RemainingFraction: &fraction}, nil
	}, nil)
	t.Cleanup(func() { SetQuotaProber(nil, nil) })

	urlA := "https://accounts.google.com/verify?id=val_A"
	cooldownA := float64(time.Now().Unix() + 86400)
	acc := &storage.Account{
		ID:               "acc_race_3",
		Email:            "race3@gmail.com",
		AccessToken:      "token_gen_A",
		RefreshToken:     "refresh_gen_A",
		Status:           "validation_required",
		ValidationURL:    &urlA,
		RateLimitedUntil: &cooldownA,
	}
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{acc}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	beforeClearReached := make(chan struct{})
	resumeClear := make(chan struct{})
	SetBeforeClearHook(func(id string) {
		close(beforeClearReached)
		<-resumeClear
	})
	t.Cleanup(func() { SetBeforeClearHook(nil) })

	type verifyResult struct {
		acc          *storage.Account
		formattedURL string
		err          error
	}
	resultChan := make(chan verifyResult, 1)

	go func() {
		resAcc, fURL, err := VerifyAccount("acc_race_3")
		resultChan <- verifyResult{acc: resAcc, formattedURL: fURL, err: err}
	}()

	// Wait until VerifyAccount finishes probe and pauses before clear transaction
	<-beforeClearReached

	// Concurrent writer updates token generation B
	err := storage.PoolTransaction(func(p *storage.Pool) error {
		stored := storage.FindAccount(p, acc)
		if stored == nil {
			return errors.New("account not found")
		}
		stored.AccessToken = "token_gen_B"
		stored.RefreshToken = "refresh_gen_B"
		return nil
	})
	if err != nil {
		t.Fatalf("concurrent writer failed: %v", err)
	}

	// Resume clear transaction in VerifyAccount
	close(resumeClear)

	res := <-resultChan
	if res.err != nil {
		t.Fatalf("VerifyAccount returned unexpected error: %v", res.err)
	}

	// Verify that restriction was NOT cleared by stale verification result
	storedPool, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	stored := storage.FindAccount(storedPool, acc)
	if stored == nil {
		t.Fatal("stored account not found")
	}
	if stored.Status != "validation_required" {
		t.Fatalf("persisted Status = %q, want validation_required", stored.Status)
	}
	if stored.ValidationURL == nil || *stored.ValidationURL != urlA {
		t.Fatalf("persisted ValidationURL = %v, want %q", stored.ValidationURL, urlA)
	}
	if stored.RefreshToken != "refresh_gen_B" {
		t.Fatalf("persisted RefreshToken = %q, want refresh_gen_B", stored.RefreshToken)
	}
}

func TestVerifyAccount_AllowsSelfAccessTokenRefresh(t *testing.T) {
	setupIsolatedTestDir(t)
	fraction := 0.85
	SetQuotaProber(func(account *storage.Account) (*storage.QuotaState, error) {
		// Mock prober simulates normal access token self-refresh
		account.AccessToken = "access_token_new"
		return &storage.QuotaState{RemainingFraction: &fraction}, nil
	}, nil)
	t.Cleanup(func() { SetQuotaProber(nil, nil) })

	urlA := "https://accounts.google.com/verify?id=val_A"
	cooldownA := float64(time.Now().Unix() + 86400)
	acc := &storage.Account{
		ID:               "acc_self_refresh",
		Email:            "self_refresh@gmail.com",
		AccessToken:      "access_token_old",
		RefreshToken:     "refresh_token_R",
		Status:           "validation_required",
		ValidationURL:    &urlA,
		RateLimitedUntil: &cooldownA,
	}
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{acc}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	resAcc, formattedURL, err := VerifyAccount("acc_self_refresh")
	if err != nil {
		t.Fatalf("VerifyAccount failed: %v", err)
	}
	if formattedURL != "" {
		t.Fatalf("expected empty formattedURL, got %q", formattedURL)
	}
	if resAcc == nil {
		t.Fatal("expected non-nil account")
	}
	if resAcc.Status != "" {
		t.Fatalf("expected Status to be empty (Ready), got %q", resAcc.Status)
	}
	if resAcc.ValidationURL != nil {
		t.Fatalf("expected ValidationURL to be nil, got %v", resAcc.ValidationURL)
	}
	if resAcc.RateLimitedUntil != nil {
		t.Fatalf("expected RateLimitedUntil to be nil, got %v", resAcc.RateLimitedUntil)
	}

	// Verify authoritative persisted state in state.db
	storedPool, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	stored := storage.FindAccount(storedPool, acc)
	if stored == nil {
		t.Fatal("stored account not found")
	}
	if stored.Status != "" {
		t.Fatalf("persisted Status = %q, want empty (Ready)", stored.Status)
	}
	if stored.ValidationURL != nil {
		t.Fatalf("persisted ValidationURL = %v, want nil", stored.ValidationURL)
	}
	if stored.RateLimitedUntil != nil {
		t.Fatalf("persisted RateLimitedUntil = %v, want nil", stored.RateLimitedUntil)
	}
	if stored.AccessToken != "access_token_new" {
		t.Fatalf("persisted AccessToken = %q, want access_token_new", stored.AccessToken)
	}
	if stored.RefreshToken != "refresh_token_R" {
		t.Fatalf("persisted RefreshToken = %q, want refresh_token_R", stored.RefreshToken)
	}
}

func TestNativeAgyEnvironmentIndependence(t *testing.T) {
	// Condition 1: with ambient default resolver
	// Condition 2: with simulated alternate resolver that points to an alternate empty path
	// Both must behave identically when a synthetic test environment is established.
	tmp1 := t.TempDir()
	dataDir1 := filepath.Join(tmp1, "data")
	nativeDir1 := filepath.Join(tmp1, "native")
	_ = os.MkdirAll(dataDir1, 0o700)
	_ = os.MkdirAll(nativeDir1, 0o700)
	t.Cleanup(func() {
		config.SetNativeAgyDir("")
		config.ResetDataDir()
	})
	if err := config.ConfigureStateDir(dataDir1); err != nil {
		t.Fatalf("failed to configure state dir: %v", err)
	}
	config.SetNativeAgyDir(nativeDir1)

	// 1. Absent token file behavior
	_, err1 := ImportCurrent()
	if err1 == nil || !strings.Contains(err1.Error(), "no existing token file found") {
		t.Fatalf("env1: expected missing token error, got: %v", err1)
	}

	// 2. Token write and read behavior
	acc := &storage.Account{
		ID:           "acc_indep",
		Email:        "indep@example.com",
		AccessToken:  "at-indep",
		RefreshToken: "rf-indep",
		IDToken:      "id-indep",
	}
	if err := WriteAgyTokenFile(acc); err != nil {
		t.Fatalf("env1: WriteAgyTokenFile failed: %v", err)
	}
	tokBytes1, err := os.ReadFile(config.GetAgyTokenFile())
	if err != nil {
		t.Fatalf("env1: failed to read token file: %v", err)
	}

	// Now switch to Condition 2: alternate custom native resolver
	tmp2 := t.TempDir()
	dataDir2 := filepath.Join(tmp2, "data")
	nativeDir2 := filepath.Join(tmp2, "native")
	_ = os.MkdirAll(dataDir2, 0o700)
	_ = os.MkdirAll(nativeDir2, 0o700)
	if err := config.ConfigureStateDir(dataDir2); err != nil {
		t.Fatalf("failed to configure state dir: %v", err)
	}
	config.SetNativeAgyDir(nativeDir2)

	// 1. Absent token file behavior
	_, err2 := ImportCurrent()
	if err2 == nil || !strings.Contains(err2.Error(), "no existing token file found") {
		t.Fatalf("env2: expected missing token error, got: %v", err2)
	}

	// Both errors must be missing-token errors
	if !strings.Contains(err1.Error(), "no existing token file found") || !strings.Contains(err2.Error(), "no existing token file found") {
		t.Fatalf("expected both errors to be missing token errors, got %q vs %q", err1.Error(), err2.Error())
	}

	// 2. Token write behavior
	if err := WriteAgyTokenFile(acc); err != nil {
		t.Fatalf("env2: WriteAgyTokenFile failed: %v", err)
	}
	tokBytes2, err := os.ReadFile(config.GetAgyTokenFile())
	if err != nil {
		t.Fatalf("env2: failed to read token file: %v", err)
	}

	// Both written files are identical
	var m1, m2 map[string]any
	_ = json.Unmarshal(tokBytes1, &m1)
	_ = json.Unmarshal(tokBytes2, &m2)
	if m1["auth_method"] != m2["auth_method"] {
		t.Fatalf("mismatched auth_method between environments")
	}
}

func TestHomeOverrideRegression(t *testing.T) {
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)

	fakeHome := t.TempDir()
	os.Setenv("HOME", fakeHome)

	// 1. Real production dir remains independently known from OS user database
	realProd := config.GetRealProductionGeminiDir()
	if realProd == "" {
		t.Skip("no real production dir detected")
	}
	if strings.HasPrefix(realProd, fakeHome) {
		t.Fatalf("real production dir %q was redirected by HOME to fakeHome %q", realProd, fakeHome)
	}

	// 2. Production guard still protects real production path despite HOME override
	realToken := filepath.Join(realProd, "antigravity-cli", "antigravity-oauth-token")
	if err := config.AssertSafeWritePath(realToken); err == nil {
		t.Fatalf("expected write to %q to be rejected by production guard despite HOME override", realToken)
	}

	// 3. Synthetic test paths resolve where intended
	syntheticGemini := filepath.Join(fakeHome, ".gemini")
	config.SetNativeAgyDir(syntheticGemini)
	defer config.SetNativeAgyDir("")

	if config.GetNativeAgyDir() != syntheticGemini {
		t.Fatalf("expected native dir %q, got %q", syntheticGemini, config.GetNativeAgyDir())
	}
	expectedTokenFile := filepath.Join(syntheticGemini, "antigravity-cli", "antigravity-oauth-token")
	if config.GetAgyTokenFile() != expectedTokenFile {
		t.Fatalf("expected token file %q, got %q", expectedTokenFile, config.GetAgyTokenFile())
	}
}

func TestSymlinkToProductionRejected(t *testing.T) {
	realProd := config.GetRealProductionGeminiDir()
	if realProd == "" {
		t.Skip("no real production dir detected")
	}

	tmpDir := t.TempDir()

	// 1. Direct file symlink pointing to real production token
	realToken := filepath.Join(realProd, "antigravity-cli", "antigravity-oauth-token")
	symlinkFile := filepath.Join(tmpDir, "symlink-token")
	if err := os.Symlink(realToken, symlinkFile); err != nil {
		t.Fatalf("failed to create file symlink: %v", err)
	}

	// Fail-closed write rejection
	if err := config.AssertSafeWritePath(symlinkFile); err == nil {
		t.Fatalf("expected write to symlink %q targeting production to be rejected", symlinkFile)
	} else if !strings.Contains(err.Error(), "refusing write to protected production path") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// Fail-closed read rejection in test mode
	config.SetTestMode(true)
	if err := config.AssertSafeReadPath(symlinkFile); err == nil {
		t.Fatalf("expected read from symlink %q targeting production to be rejected in test mode", symlinkFile)
	} else if !strings.Contains(err.Error(), "refusing read from protected production path in test mode") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// 2. Directory symlink pointing to real production gemini dir
	symlinkDir := filepath.Join(tmpDir, "symlink-gemini-dir")
	if err := os.Symlink(realProd, symlinkDir); err != nil {
		t.Fatalf("failed to create dir symlink: %v", err)
	}
	targetInSymlinkDir := filepath.Join(symlinkDir, "antigravity-cli", "antigravity-oauth-token")
	if err := config.AssertSafeWritePath(targetInSymlinkDir); err == nil {
		t.Fatalf("expected write through dir symlink %q to be rejected", targetInSymlinkDir)
	}

	// 3. Path normalization with '..'
	dotDotPath := filepath.Join(realProd, "..", filepath.Base(realProd), "antigravity-cli", "antigravity-oauth-token")
	if err := config.AssertSafeWritePath(dotDotPath); err == nil {
		t.Fatalf("expected write with '..' traversing into production path to be rejected: %s", dotDotPath)
	}

	// 4. Prefix collision protection (e.g. /home/codex/.gemini_fake must NOT be falsely rejected)
	fakePrefixPath := realProd + "_fake" + string(os.PathSeparator) + "token"
	if err := config.AssertSafeWritePath(fakePrefixPath); err != nil {
		t.Fatalf("expected path with distinct prefix name not to be rejected, got: %v", err)
	}
}

func TestRemoveAccountRepairsActive(t *testing.T) {
	setupIsolatedTestDir(t)

	// 1. Setup 3 accounts: acc_1, acc_2, acc_3
	p := storage.NewEmptyPool()
	p.Accounts = []*storage.Account{
		{ID: "acc_1", Email: "first@example.com", AccessToken: "token-acc-1"},
		{ID: "acc_2", Email: "second@example.com", AccessToken: "token-acc-2"},
		{ID: "acc_3", Email: "third@example.com", AccessToken: "token-acc-3"},
	}
	p.ActiveAccountID = &p.Accounts[0].ID
	if err := storage.SavePool(p); err != nil {
		t.Fatalf("failed to save initial pool: %v", err)
	}

	// 2. Remove middle account (acc_2)
	rem, err := RemoveAccount("acc_2")
	if err != nil {
		t.Fatalf("RemoveAccount acc_2 failed: %v", err)
	}
	if rem.ID != "acc_2" {
		t.Fatalf("expected removed account acc_2, got %s", rem.ID)
	}

	loaded, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("failed to load pool: %v", err)
	}
	if len(loaded.Accounts) != 2 {
		t.Fatalf("expected 2 remaining accounts, got %d", len(loaded.Accounts))
	}
	// Active account should still be acc_1
	if loaded.ActiveAccountID == nil || *loaded.ActiveAccountID != "acc_1" {
		t.Fatalf("active account corrupted: got %v, want acc_1", loaded.ActiveAccountID)
	}

	// 3. Remove active account (acc_1)
	rem, err = RemoveAccount("acc_1")
	if err != nil {
		t.Fatalf("RemoveAccount acc_1 failed: %v", err)
	}
	if rem.ID != "acc_1" {
		t.Fatalf("expected removed account acc_1, got %s", rem.ID)
	}

	loaded, err = storage.LoadPool()
	if err != nil {
		t.Fatalf("failed to load pool: %v", err)
	}
	if len(loaded.Accounts) != 1 {
		t.Fatalf("expected 1 remaining account, got %d", len(loaded.Accounts))
	}
	// Active account must be repaired to the remaining account (acc_3)
	if loaded.ActiveAccountID == nil || *loaded.ActiveAccountID != "acc_3" {
		t.Fatalf("active account was not repaired to remaining account: got %v, want acc_3", loaded.ActiveAccountID)
	}

	// 4. Remove final account (acc_3)
	rem, err = RemoveAccount("acc_3")
	if err != nil {
		t.Fatalf("RemoveAccount acc_3 failed: %v", err)
	}
	if rem.ID != "acc_3" {
		t.Fatalf("expected removed account acc_3, got %s", rem.ID)
	}

	loaded, err = storage.LoadPool()
	if err != nil {
		t.Fatalf("failed to load pool: %v", err)
	}
	if len(loaded.Accounts) != 0 {
		t.Fatalf("expected 0 remaining accounts, got %d", len(loaded.Accounts))
	}
	if loaded.ActiveAccountID != nil {
		t.Fatalf("active account not nil after removing final account: got %v", loaded.ActiveAccountID)
	}
}

func TestImportReplaceAtomicity(t *testing.T) {
	setupIsolatedTestDir(t)

	// Setup initial state with 2 accounts
	p := storage.NewEmptyPool()
	p.Accounts = []*storage.Account{
		{ID: "acc_1", Email: "keep1@example.com", AccessToken: "tok1"},
		{ID: "acc_2", Email: "keep2@example.com", AccessToken: "tok2"},
	}
	p.ActiveAccountID = &p.Accounts[0].ID
	if err := storage.SavePool(p); err != nil {
		t.Fatalf("failed to save initial pool: %v", err)
	}

	// Attempt import with corrupt/invalid backup file in replace mode
	tmpDir := t.TempDir()
	badBackup := filepath.Join(tmpDir, "bad_backup.json")
	_ = os.WriteFile(badBackup, []byte("NOT VALID JSON BACKUP"), 0o600)

	_, err := ImportPool(badBackup, "", true, false)
	if err == nil {
		t.Fatalf("expected error importing bad backup, got nil")
	}

	// CRITICAL CHECK: Existing accounts MUST remain 100% intact!
	loaded, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("failed to load pool: %v", err)
	}
	if len(loaded.Accounts) != 2 {
		t.Fatalf("pool was corrupted or cleared by failed replace import! got %d accounts", len(loaded.Accounts))
	}
	if loaded.Accounts[0].Email != "keep1@example.com" || loaded.Accounts[1].Email != "keep2@example.com" {
		t.Fatalf("pool accounts modified after failed replace: %+v", loaded.Accounts)
	}
	if loaded.ActiveAccountID == nil || *loaded.ActiveAccountID != "acc_1" {
		t.Fatalf("active account modified after failed replace: %v", loaded.ActiveAccountID)
	}
}

func TestLogin_StatusOutput(t *testing.T) {
	setupIsolatedTestDir(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mockJWT := "eyJhbGciOiJub25lIn0.eyJlbWFpbCI6ICJzdGF0dXNfdXNlckBnbWFpbC5jb20ifQ."
		resp := map[string]any{
			"access_token":  "status-at-123",
			"refresh_token": "status-rf-456",
			"id_token":      mockJWT,
			"expires_in":    3600,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	// 1. Browser opener succeeds
	var stdoutSuccess bytes.Buffer
	optsSuccess := LoginOptions{
		TokenEndpoint: server.URL,
		BrowserOpener: func(url string) error { return nil },
		PromptFn: func(authURL string) (string, error) {
			return "valid-code", nil
		},
		Timeout:   5 * time.Second,
		PortRange: [2]int{18140, 18190},
		Stdout:    &stdoutSuccess,
	}

	acc, err := Login(context.Background(), optsSuccess)
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}
	if acc.Email != "status_user@gmail.com" {
		t.Fatalf("unexpected email: %s", acc.Email)
	}
	if !strings.Contains(stdoutSuccess.String(), "Opening browser for Google authentication...\nWaiting for authorization...") {
		t.Fatalf("missing success status, got: %q", stdoutSuccess.String())
	}
	if strings.Contains(stdoutSuccess.String(), "Could not open a browser automatically.") {
		t.Fatalf("unexpected failure message in success output: %q", stdoutSuccess.String())
	}

	// 2. Browser opener fails
	var stdoutFail bytes.Buffer
	optsFail := LoginOptions{
		TokenEndpoint: server.URL,
		BrowserOpener: func(url string) error { return errors.New("headless failure") },
		PromptFn: func(authURL string) (string, error) {
			return "valid-code", nil
		},
		Timeout:   5 * time.Second,
		PortRange: [2]int{18191, 18240},
		Stdout:    &stdoutFail,
	}

	acc2, err := Login(context.Background(), optsFail)
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}
	if acc2.Email != "status_user@gmail.com" {
		t.Fatalf("unexpected email: %s", acc2.Email)
	}
	if !strings.Contains(stdoutFail.String(), "Could not open a browser automatically.\nOpen this URL:\nhttps://accounts.google.com/o/oauth2/v2/auth?") {
		t.Fatalf("missing fail status or auth URL, got: %q", stdoutFail.String())
	}
	if strings.Contains(stdoutFail.String(), "Opening browser for Google authentication...") {
		t.Fatalf("unexpected browser opening message in failure output: %q", stdoutFail.String())
	}
}
