package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/observability"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// Case J: Auth refresh cached token does not increment attempts
func TestObservability_CaseJ_AuthRefreshCachedToken(t *testing.T) {
	setupIsolatedTestDir(t)
	observability.Reset()

	now := time.Now()
	NowFunc = func() time.Time { return now }
	defer func() { NowFunc = time.Now }()

	expiry := float64(now.Unix() + 300) // 5 minutes remaining (> 120s)
	acc := &storage.Account{
		ID:           "acc_cached",
		AccessToken:  "valid-cached-token",
		RefreshToken: "refresh-tok",
		TokenExpiry:  &expiry,
	}

	tok, err := RefreshToken(acc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "valid-cached-token" {
		t.Fatalf("got %q, want valid-cached-token", tok)
	}

	snap := observability.GetSnapshot()
	if snap.AuthRefreshAttempts != 0 {
		t.Errorf("auth_refresh_attempts_total = %d, want 0 on cached token", snap.AuthRefreshAttempts)
	}
	if snap.AuthRefreshSuccess != 0 {
		t.Errorf("auth_refresh_success_total = %d, want 0 on cached token", snap.AuthRefreshSuccess)
	}
	if snap.AuthRefreshFailure != 0 {
		t.Errorf("auth_refresh_failure_total = %d, want 0 on cached token", snap.AuthRefreshFailure)
	}
}

// Case K: Auth refresh real success/failure
func TestObservability_CaseK_AuthRefreshRealSuccessAndFailure(t *testing.T) {
	setupIsolatedTestDir(t)
	observability.Reset()

	var networkCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		networkCalls++
		_ = r.ParseForm()
		rf := r.FormValue("refresh_token")
		if rf == "bad-token" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh-access-token",
			"expires_in":   3600,
		})
	}))
	defer server.Close()

	origURL := TokenURL
	TokenURL = server.URL
	defer func() { TokenURL = origURL }()

	// 1. Successful refresh
	pool := storage.NewEmptyPool()
	accGood := &storage.Account{
		ID:           "acc_good",
		RefreshToken: "good-token",
	}
	accBad := &storage.Account{
		ID:           "acc_bad",
		RefreshToken: "bad-token",
	}
	pool.Accounts = []*storage.Account{accGood, accBad}
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}

	tok, err := RefreshToken(accGood)
	if err != nil || tok != "fresh-access-token" {
		t.Fatalf("RefreshToken failed: %v, tok: %s", err, tok)
	}

	snap := observability.GetSnapshot()
	if snap.AuthRefreshAttempts != 1 {
		t.Errorf("auth_refresh_attempts_total = %d, want 1", snap.AuthRefreshAttempts)
	}
	if snap.AuthRefreshSuccess != 1 {
		t.Errorf("auth_refresh_success_total = %d, want 1", snap.AuthRefreshSuccess)
	}
	if snap.AuthRefreshFailure != 0 {
		t.Errorf("auth_refresh_failure_total = %d, want 0", snap.AuthRefreshFailure)
	}

	// 2. Failed refresh
	_, err = RefreshToken(accBad)
	if err == nil {
		t.Fatal("expected error on bad refresh token")
	}

	snap = observability.GetSnapshot()
	if snap.AuthRefreshAttempts != 2 {
		t.Errorf("auth_refresh_attempts_total = %d, want 2", snap.AuthRefreshAttempts)
	}
	if snap.AuthRefreshSuccess != 1 {
		t.Errorf("auth_refresh_success_total = %d, want 1", snap.AuthRefreshSuccess)
	}
	if snap.AuthRefreshFailure != 1 {
		t.Errorf("auth_refresh_failure_total = %d, want 1", snap.AuthRefreshFailure)
	}
}
