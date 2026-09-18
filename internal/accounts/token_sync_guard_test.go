package accounts

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/auth"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// A. Production-mode canonical token write and sync succeed with isolated fake HOME
func TestNativeTokenSync_ProductionModeCanonicalSucceeds(t *testing.T) {
	origTestMode := config.IsTestMode()
	config.SetTestMode(false)
	defer config.SetTestMode(origTestMode)

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	dataDir := filepath.Join(fakeHome, ".local", "share", "agy-pool")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := config.ConfigureStateDir(dataDir); err != nil {
		t.Fatal(err)
	}
	defer config.ResetDataDir()

	expiry := float64(time.Now().Unix() + 3600)
	acc := &storage.Account{
		ID:           "acc_1",
		Name:         "Main",
		Email:        "main@example.com",
		AccessToken:  "prod-access-token-12345",
		RefreshToken: "prod-refresh-token-67890",
		IDToken:      "header.payload.signature",
		TokenExpiry:  &expiry,
	}

	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{acc}
	pool.ActiveAccountID = &acc.ID
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("failed to save pool: %v", err)
	}

	// 1. WriteAgyTokenFile directly succeeds
	if err := WriteAgyTokenFile(acc); err != nil {
		t.Fatalf("WriteAgyTokenFile failed in production mode: %v", err)
	}

	expectedTokenFile := filepath.Join(fakeHome, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	data, err := os.ReadFile(expectedTokenFile)
	if err != nil {
		t.Fatalf("failed to read expected token file %s: %v", expectedTokenFile, err)
	}

	fi, err := os.Stat(expectedTokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("expected 0600 file permissions, got %v", fi.Mode().Perm())
	}

	var parsed struct {
		Token struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"token"`
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to parse token JSON: %v", err)
	}
	if parsed.Token.AccessToken != "prod-access-token-12345" {
		t.Fatalf("expected access token 'prod-access-token-12345', got %q", parsed.Token.AccessToken)
	}

	// 2. SyncActiveAgyTokenFile succeeds
	synced, err := SyncActiveAgyTokenFile()
	if err != nil || !synced {
		t.Fatalf("SyncActiveAgyTokenFile failed in production mode: synced=%v, err=%v", synced, err)
	}
}

// B. Test-mode real-production-like path rejected
func TestNativeTokenSync_TestModeRealProductionRejected(t *testing.T) {
	origTestMode := config.IsTestMode()
	config.SetTestMode(true)
	defer config.SetTestMode(origTestMode)

	// Configure synthetic sandbox elsewhere
	sandboxDir := t.TempDir()
	resetSandbox := config.SetSyntheticSandboxRoot(sandboxDir)
	defer resetSandbox()

	// Simulate real production home (isolated temp dir, never real /home/codex)
	fakeRealHome := t.TempDir()
	fakeRealGemini := filepath.Join(fakeRealHome, ".gemini")
	resetRealProd := config.SetRealProductionGeminiDir(fakeRealGemini)
	defer resetRealProd()

	// Point native agy dir to the simulated real production directory
	config.SetNativeAgyDir(fakeRealGemini)
	defer config.SetNativeAgyDir("")

	acc := &storage.Account{
		ID:          "acc_1",
		AccessToken: "secret-token",
	}

	// WriteAgyTokenFile must fail closed
	err := WriteAgyTokenFile(acc)
	if err == nil {
		t.Fatalf("expected WriteAgyTokenFile to fail closed on real-production-like path in test mode")
	}
	if !strings.Contains(err.Error(), "refusing write to protected production path in test mode") &&
		!strings.Contains(err.Error(), "protected production path") {
		t.Fatalf("expected fail-closed error, got: %v", err)
	}

	// Real production file must not have been created
	realTokenFile := filepath.Join(fakeRealGemini, "antigravity-cli", "antigravity-oauth-token")
	if _, err := os.Stat(realTokenFile); !os.IsNotExist(err) {
		t.Fatalf("real-production-like token file was created despite guard failure!")
	}
}

// C. Test-mode synthetic token path succeeds
func TestNativeTokenSync_TestModeSyntheticSucceeds(t *testing.T) {
	origTestMode := config.IsTestMode()
	config.SetTestMode(true)
	defer config.SetTestMode(origTestMode)

	tempDir := t.TempDir()
	resetSandbox := config.SetSyntheticSandboxRoot(tempDir)
	defer resetSandbox()

	dataDir := filepath.Join(tempDir, "data")
	_ = os.MkdirAll(dataDir, 0700)
	_ = config.ConfigureStateDir(dataDir)
	defer config.ResetDataDir()

	nativeDir := filepath.Join(tempDir, "synthetic_gemini")
	config.SetNativeAgyDir(nativeDir)
	defer config.SetNativeAgyDir("")

	acc := &storage.Account{
		ID:          "acc_1",
		AccessToken: "synced-token",
	}
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{acc}
	pool.ActiveAccountID = &acc.ID
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}

	if err := WriteAgyTokenFile(acc); err != nil {
		t.Fatalf("WriteAgyTokenFile failed in synthetic test sandbox: %v", err)
	}

	synced, err := SyncActiveAgyTokenFile()
	if err != nil || !synced {
		t.Fatalf("SyncActiveAgyTokenFile failed in synthetic test sandbox: synced=%v, err=%v", synced, err)
	}
}

// H. Privacy: failure output must not contain token secret, email, or raw OAuth body
func TestNativeTokenSync_PrivacyOnFailure(t *testing.T) {
	setupIsolatedTestDir(t)

	secretToken := "very-confidential-token-value-xyz"
	secretEmail := "confidential-user@secretcompany.com"
	rawOAuthBody := "raw-provider-body-marker " + secretEmail + " " + secretToken

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","description":"` + rawOAuthBody + `"}`))
	}))
	defer server.Close()

	oldURL := auth.TokenURL
	auth.TokenURL = server.URL
	defer func() { auth.TokenURL = oldURL }()

	past := float64(time.Now().Unix() - 100)
	acc := &storage.Account{
		ID:           "acc_1",
		Email:        secretEmail,
		AccessToken:  secretToken,
		RefreshToken: "refresh-secret-123",
		TokenExpiry:  &past,
	}

	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{acc}
	pool.ActiveAccountID = &acc.ID
	_ = storage.SavePool(pool)

	_, err := SyncActiveAgyTokenFile()
	if err == nil {
		t.Fatal("expected refresh error")
	}
	errStr := err.Error()
	if strings.Contains(errStr, secretToken) {
		t.Fatalf("token secret leaked in error: %s", errStr)
	}
	if strings.Contains(errStr, secretEmail) {
		t.Fatalf("email leaked in error: %s", errStr)
	}
	if strings.Contains(errStr, "raw-provider-body-marker") {
		t.Fatalf("raw OAuth body leaked in error: %s", errStr)
	}
}
