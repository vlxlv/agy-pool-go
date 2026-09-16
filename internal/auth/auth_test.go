package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func TestDecodeCred(t *testing.T) {
	if DefaultClientID == "" {
		t.Fatal("DefaultClientID is empty")
	}
	expectedID := "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	if DefaultClientID != expectedID {
		t.Fatalf("DefaultClientID = %q, want %q", DefaultClientID, expectedID)
	}

	expectedSecret := "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"
	if DefaultClientSecret != expectedSecret {
		t.Fatalf("DefaultClientSecret = %q, want %q", DefaultClientSecret, expectedSecret)
	}
}

func TestDecodeJWTPayload(t *testing.T) {
	tests := []struct {
		name      string
		jwt       string
		wantEmail string
		wantEmpty bool
	}{
		{
			name:      "valid jwt",
			jwt:       "header.eyJlbWFpbCI6ICJ1c2VyQGV4YW1wbGUuY29tIiwgInN1YiI6ICIxMjM0NSJ9.sig",
			wantEmail: "user@example.com",
		},
		{
			name:      "single part",
			jwt:       "not-a-jwt",
			wantEmpty: true,
		},
		{
			name:      "invalid base64",
			jwt:       "header.!!!notb64!!!.sig",
			wantEmpty: true,
		},
		{
			name:      "invalid json",
			jwt:       "header.bm90LWpzb24.sig", // "not-json"
			wantEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := DecodeJWTPayload(tt.jwt)
			if tt.wantEmpty {
				if len(res) != 0 {
					t.Fatalf("expected empty claims, got %+v", res)
				}
				return
			}
			if email, _ := res["email"].(string); email != tt.wantEmail {
				t.Fatalf("email = %q, want %q", email, tt.wantEmail)
			}
		})
	}
}

func TestErrorClassifiers(t *testing.T) {
	t.Run("IsValidationError", func(t *testing.T) {
		if IsValidationError(200, []byte("validation_required")) {
			t.Fatal("200 should not be validation error")
		}
		if !IsValidationError(403, []byte(`{"error": "validation_required"}`)) {
			t.Fatal("403 with validation_required should match")
		}
		if !IsValidationError(403, []byte("Please Verify Your Account to continue")) {
			t.Fatal("403 with verify your account should match")
		}
		if IsValidationError(403, []byte("other 403 error")) {
			t.Fatal("unrelated 403 should not match")
		}
	})

	t.Run("ExtractValidationURL", func(t *testing.T) {
		bodyMetadata := []byte(`{
			"error": {
				"details": [
					{"metadata": {"validation_url": "https://accounts.google.com/verify?id=123"}}
				]
			}
		}`)
		if u := ExtractValidationURL(bodyMetadata); u != "https://accounts.google.com/verify?id=123" {
			t.Fatalf("got %q, want metadata url", u)
		}

		bodyLink := []byte(`{
			"error": {
				"details": [
					{"links": [{"description": "Please verify", "url": "https://google.com/verify"}]}
				]
			}
		}`)
		if u := ExtractValidationURL(bodyLink); u != "https://google.com/verify" {
			t.Fatalf("got %q, want link url", u)
		}

		if u := ExtractValidationURL([]byte("invalid json")); u != "" {
			t.Fatalf("expected empty url on invalid json, got %q", u)
		}
	})

	t.Run("IsAuthError", func(t *testing.T) {
		if !IsAuthError(401, []byte("Unauthorized")) {
			t.Fatal("401 must be auth error")
		}
		if !IsAuthError(403, []byte("invalid_grant")) {
			t.Fatal("403 invalid_grant must be auth error")
		}
		if !IsAuthError(403, []byte("Access_Token_Expired")) {
			t.Fatal("403 token expired must be auth error")
		}
		if IsAuthError(403, []byte("permission denied for resource")) {
			t.Fatal("generic 403 should not be auth error")
		}
		if IsAuthError(500, []byte("invalid_grant")) {
			t.Fatal("500 must not be auth error")
		}
	})
}

func setupIsolatedTestDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	if err := config.ConfigureStateDir(tmp); err != nil {
		t.Fatalf("failed to configure state dir: %v", err)
	}
	return tmp
}

func TestRefreshToken_Cached(t *testing.T) {
	setupIsolatedTestDir(t)

	now := time.Now()
	NowFunc = func() time.Time { return now }
	defer func() { NowFunc = time.Now }()

	expiry := float64(now.Unix() + 300) // 5 minutes remaining (> 120s)
	acc := &storage.Account{
		ID:          "acc_1",
		AccessToken: "cached-token-123",
		TokenExpiry: &expiry,
	}

	tok, err := RefreshToken(acc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "cached-token-123" {
		t.Fatalf("got %q, want cached-token-123", tok)
	}
}

func TestRefreshToken_MissingRefreshToken(t *testing.T) {
	setupIsolatedTestDir(t)

	acc := &storage.Account{
		ID: "acc_1",
	}

	_, err := RefreshToken(acc)
	if err == nil || err.Error() != "Missing refresh_token" {
		t.Fatalf("expected 'Missing refresh_token', got %v", err)
	}
}

func TestRefreshToken_UpstreamSuccess(t *testing.T) {
	setupIsolatedTestDir(t)

	var serverHits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&serverHits, 1)
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.FormValue("grant_type") != "refresh_token" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.FormValue("refresh_token") != "valid-rf" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		resp := map[string]any{
			"access_token":  "new-at-456",
			"refresh_token": "new-rf-789",
			"expires_in":    3600,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	origTokenURL := TokenURL
	TokenURL = server.URL
	defer func() { TokenURL = origTokenURL }()

	pool := storage.NewEmptyPool()
	acc := &storage.Account{
		ID:           "acc_1",
		Email:        "user@example.com",
		RefreshToken: "valid-rf",
	}
	pool.Accounts = append(pool.Accounts, acc)
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("failed to save pool: %v", err)
	}

	tok, err := RefreshToken(acc)
	if err != nil {
		t.Fatalf("RefreshToken failed: %v", err)
	}
	if tok != "new-at-456" {
		t.Fatalf("got token %q, want new-at-456", tok)
	}
	if acc.RefreshToken != "new-rf-789" {
		t.Fatalf("got rf %q, want new-rf-789", acc.RefreshToken)
	}

	// Verify persistence in disk pool
	loaded, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("failed to load pool: %v", err)
	}
	stored := storage.FindAccount(loaded, acc)
	if stored == nil || stored.AccessToken != "new-at-456" || stored.RefreshToken != "new-rf-789" {
		t.Fatalf("stored account tokens not updated: %+v", stored)
	}
}

func TestRefreshToken_ConcurrentSingleFlight(t *testing.T) {
	setupIsolatedTestDir(t)

	var serverHits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&serverHits, 1)
		time.Sleep(50 * time.Millisecond) // Simulate network latency

		resp := map[string]any{
			"access_token":  "new-concurrent-at",
			"refresh_token": "valid-rf",
			"expires_in":    3600,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	origTokenURL := TokenURL
	TokenURL = server.URL
	defer func() { TokenURL = origTokenURL }()

	pool := storage.NewEmptyPool()
	acc := &storage.Account{
		ID:           "acc_1",
		Email:        "user@example.com",
		RefreshToken: "valid-rf",
	}
	pool.Accounts = append(pool.Accounts, acc)
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("failed to save pool: %v", err)
	}

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			// Each goroutine passes a copy pointing to acc_1
			localAcc := &storage.Account{
				ID:           "acc_1",
				RefreshToken: "valid-rf",
			}
			tok, err := RefreshToken(localAcc)
			if err != nil {
				t.Errorf("RefreshToken concurrent error: %v", err)
				return
			}
			if tok != "new-concurrent-at" {
				t.Errorf("got token %q, want new-concurrent-at", tok)
			}
		}()
	}

	wg.Wait()

	// Upstream should only have been contacted ONCE due to file lock and post-lock re-read!
	hits := atomic.LoadInt32(&serverHits)
	if hits != 1 {
		t.Fatalf("expected exactly 1 upstream request due to lock reuse, got %d", hits)
	}
}
