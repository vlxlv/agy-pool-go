package proxy

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func TestExplicitFailover_429(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()

	var acc1Hits, acc2Hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if strings.Contains(authHeader, "acc_1") {
			atomic.AddInt32(&acc1Hits, 1)
			w.Header().Set("Retry-After", "150")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error": {"code": 429, "message": "ResourceExhausted"}}`))
			return
		}
		if strings.Contains(authHeader, "acc_2") {
			atomic.AddInt32(&acc2Hits, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result": "success from acc_2"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }
	TokenRefresher = func(account *storage.Account) (string, error) {
		return "token-" + account.ID, nil
	}

	pool := storage.NewEmptyPool()
	frac := 0.9
	acc1 := &storage.Account{
		ID:        "acc_1",
		Email:     "acc1@gmail.com",
		LastQuota: &storage.QuotaState{RemainingFraction: &frac},
	}
	acc2 := &storage.Account{
		ID:        "acc_2",
		Email:     "acc2@gmail.com",
		LastQuota: &storage.QuotaState{RemainingFraction: &frac},
	}
	pool.Accounts = []*storage.Account{acc1, acc2}
	pool.ActiveAccountID = &acc1.ID
	_ = storage.SavePool(pool)

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader("{}"))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)
	resp := w.Result()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected failover to 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "success from acc_2") {
		t.Fatalf("body = %q, want 'success from acc_2'", string(body))
	}

	if atomic.LoadInt32(&acc1Hits) != 1 || atomic.LoadInt32(&acc2Hits) != 1 {
		t.Fatalf("expected 1 hit on acc_1 and 1 hit on acc_2, got acc1=%d, acc2=%d", acc1Hits, acc2Hits)
	}

	// Verify acc_1 was put on cooldown and quota updated_at set to 0
	loaded, _ := storage.LoadPool()
	stored1 := storage.FindAccount(loaded, acc1)
	if stored1.RateLimitedUntil == nil || *stored1.RateLimitedUntil <= float64(time.Now().Unix()) {
		t.Fatalf("acc_1 rate_limited_until not set: %v", stored1.RateLimitedUntil)
	}
	if stored1.ErrorCount != 1 {
		t.Fatalf("acc_1 error_count = %d, want 1", stored1.ErrorCount)
	}
	if stored1.LastQuota == nil || stored1.LastQuota.UpdatedAt == nil || *stored1.LastQuota.UpdatedAt != 0 {
		t.Fatalf("acc_1 last_quota.updated_at not set to 0: %+v", stored1.LastQuota)
	}
	// Active account promoted to acc_2
	if loaded.ActiveAccountID == nil || *loaded.ActiveAccountID != "acc_2" {
		t.Fatalf("active account not promoted to acc_2: %v", loaded.ActiveAccountID)
	}
}

func TestExplicitFailover_ValidationRequired(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "acc_1") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{
				"error": {
					"message": "validation_required: please verify your account",
					"details": [{"metadata": {"validation_url": "https://accounts.google.com/verify?id=99"}}]
				}
			}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }
	TokenRefresher = func(account *storage.Account) (string, error) {
		return "token-" + account.ID, nil
	}

	pool := storage.NewEmptyPool()
	acc1 := &storage.Account{ID: "acc_1", Email: "1@gmail.com"}
	acc2 := &storage.Account{ID: "acc_2", Email: "2@gmail.com"}
	pool.Accounts = []*storage.Account{acc1, acc2}
	pool.ActiveAccountID = &acc1.ID
	_ = storage.SavePool(pool)

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader("{}"))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)
	resp := w.Result()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	loaded, _ := storage.LoadPool()
	stored1 := storage.FindAccount(loaded, acc1)
	if stored1.Status != "validation_required" {
		t.Fatalf("acc_1 status = %q, want 'validation_required'", stored1.Status)
	}
	if stored1.ValidationURL == nil || *stored1.ValidationURL != "https://accounts.google.com/verify?id=99" {
		t.Fatalf("validation_url = %v", stored1.ValidationURL)
	}
}

func TestExplicitFailover_AuthError(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "acc_1") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error": "unauthenticated"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }
	TokenRefresher = func(account *storage.Account) (string, error) {
		return "token-" + account.ID, nil
	}

	pool := storage.NewEmptyPool()
	acc1 := &storage.Account{ID: "acc_1", Email: "1@gmail.com"}
	acc2 := &storage.Account{ID: "acc_2", Email: "2@gmail.com"}
	pool.Accounts = []*storage.Account{acc1, acc2}
	pool.ActiveAccountID = &acc1.ID
	_ = storage.SavePool(pool)

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader("{}"))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)
	resp := w.Result()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	loaded, _ := storage.LoadPool()
	stored1 := storage.FindAccount(loaded, acc1)
	if stored1.Status != "auth_error" {
		t.Fatalf("acc_1 status = %q, want 'auth_error'", stored1.Status)
	}
}

func TestExplicitFailover_TokenRefreshFailure(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }

	TokenRefresher = func(account *storage.Account) (string, error) {
		if account.ID == "acc_1" {
			return "", net.UnknownNetworkError("token refresh revoked")
		}
		return "token-acc_2", nil
	}

	pool := storage.NewEmptyPool()
	acc1 := &storage.Account{ID: "acc_1", Email: "1@gmail.com"}
	acc2 := &storage.Account{ID: "acc_2", Email: "2@gmail.com"}
	pool.Accounts = []*storage.Account{acc1, acc2}
	pool.ActiveAccountID = &acc1.ID
	_ = storage.SavePool(pool)

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader("{}"))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)
	resp := w.Result()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	loaded, _ := storage.LoadPool()
	stored1 := storage.FindAccount(loaded, acc1)
	if stored1.Status != "auth_error" {
		t.Fatalf("acc_1 status = %q, want 'auth_error' on refresh failure", stored1.Status)
	}
}

func TestOrdinaryUpstreamError_NoFailover(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()

	var acc1Hits, acc2Hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "acc_1") {
			atomic.AddInt32(&acc1Hits, 1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error": "invalid parameter"}`))
			return
		}
		atomic.AddInt32(&acc2Hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }
	TokenRefresher = func(account *storage.Account) (string, error) {
		return "token-" + account.ID, nil
	}

	pool := storage.NewEmptyPool()
	acc1 := &storage.Account{ID: "acc_1", Email: "1@gmail.com"}
	acc2 := &storage.Account{ID: "acc_2", Email: "2@gmail.com"}
	pool.Accounts = []*storage.Account{acc1, acc2}
	pool.ActiveAccountID = &acc1.ID
	_ = storage.SavePool(pool)

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader("{}"))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)
	resp := w.Result()

	// Ordinary 400 Bad Request MUST be forwarded directly to client without failover!
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "invalid parameter") {
		t.Fatalf("body = %q, want 'invalid parameter'", string(body))
	}

	// NO REPLAY: acc_2 must NOT have been called!
	if atomic.LoadInt32(&acc1Hits) != 1 || atomic.LoadInt32(&acc2Hits) != 0 {
		t.Fatalf("expected acc_1=1, acc_2=0, got acc1=%d, acc2=%d", acc1Hits, acc2Hits)
	}
}

func TestAllAccountsExhausted_503(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }
	TokenRefresher = func(account *storage.Account) (string, error) {
		return "token-" + account.ID, nil
	}

	pool := storage.NewEmptyPool()
	acc1 := &storage.Account{ID: "acc_1", Email: "1@gmail.com"}
	pool.Accounts = []*storage.Account{acc1}
	_ = storage.SavePool(pool)

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader("{}"))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)
	resp := w.Result()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestUnknownFieldPreservationDuringFailover(t *testing.T) {
	tmp := setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }
	TokenRefresher = func(account *storage.Account) (string, error) {
		return "token-" + account.ID, nil
	}

	// Write custom JSON with future unknown fields at root, account, and quota levels
	accountsPath := config.GetAccountsFile()
	rawJSON := `{
		"version": 1,
		"strategy": "max_quota",
		"active_account_id": "acc_1",
		"future_root_field": "root_val_123",
		"accounts": [
			{
				"id": "acc_1",
				"email": "1@gmail.com",
				"custom_account_feature": {"beta_tier": 42},
				"last_quota": {
					"remaining_fraction": 0.95,
					"future_quota_metric": 999
				}
			}
		]
	}`
	_ = storage.AtomicJSONWrite(accountsPath, json.RawMessage(rawJSON))

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader("{}"))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// Check that raw JSON on disk still contains the unknown fields
	data, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("failed to load pool: %v", err)
	}

	if string(data.Extra["future_root_field"]) != `"root_val_123"` {
		t.Fatalf("future_root_field lost: %+v", data.Extra)
	}
	stored := data.Accounts[0]
	if !strings.Contains(string(stored.Extra["custom_account_feature"]), "42") {
		t.Fatalf("custom_account_feature lost: %+v", stored.Extra)
	}
	if string(stored.LastQuota.Extra["future_quota_metric"]) != "999" {
		t.Fatalf("future_quota_metric lost: %+v", stored.LastQuota.Extra)
	}
	_ = tmp
}
