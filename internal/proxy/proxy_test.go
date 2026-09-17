package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/quota"
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

func resetProviders() {
	BackendHostProvider = nil
	BackendURLProvider = nil
	UserAgentProvider = nil
	TokenRefresher = nil
	QuotaRefresher = quota.ScheduleQuotaRefreshSimple
	LogRotator = nil
	NowFunc = time.Now
	LogWriter = io.Discard
	HTTPClient = defaultHTTPClient
	GenerationClient = defaultGenerationClient
	RoundTripHook = nil
}

func TestHopByHopHeaders(t *testing.T) {
	headers := make(http.Header)
	headers.Add("Connection", "x-custom-hop, close")
	headers.Add("X-Custom-Hop", "strip-me")
	headers.Add("Keep-Alive", "timeout=5")
	headers.Add("User-Agent", "agy/1.0")

	names := HopByHopNames(headers)
	if !names["connection"] || !names["keep-alive"] || !names["proxy-connection"] {
		t.Fatal("standard hop-by-hop missing from set")
	}
	if !names["x-custom-hop"] || !names["close"] {
		t.Fatal("custom Connection headers missing from set")
	}
	if names["user-agent"] {
		t.Fatal("user-agent should not be hop-by-hop")
	}
}

func TestReadRequestBody(t *testing.T) {
	tests := []struct {
		name        string
		headers     map[string]string
		body        string
		wantErrMsg  string
		wantBodyLen int
	}{
		{
			name:        "valid plain body",
			headers:     map[string]string{"Content-Length": "11"},
			body:        "hello world",
			wantBodyLen: 11,
		},
		{
			name: "both content-length and transfer-encoding",
			headers: map[string]string{
				"Content-Length":    "10",
				"Transfer-Encoding": "chunked",
			},
			body:       "hello",
			wantErrMsg: "both Content-Length and Transfer-Encoding are present",
		},
		{
			name: "negative content-length",
			headers: map[string]string{
				"Content-Length": "-5",
			},
			body:       "hello",
			wantErrMsg: "negative Content-Length",
		},
		{
			name: "unexpected EOF",
			headers: map[string]string{
				"Content-Length": "20",
			},
			body:       "too short",
			wantErrMsg: "unexpected EOF in request body",
		},
		{
			name: "unsupported transfer-encoding",
			headers: map[string]string{
				"Transfer-Encoding": "gzip",
			},
			body:       "hello",
			wantErrMsg: "unsupported Transfer-Encoding",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest("POST", "/test", strings.NewReader(tt.body))
			if err != nil {
				t.Fatalf("failed to create request: %v", err)
			}
			req.Header = make(http.Header)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			readBytes, err := ReadRequestBody(req)
			if tt.wantErrMsg != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErrMsg, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(readBytes) != tt.wantBodyLen {
				t.Fatalf("got %d bytes, want %d", len(readBytes), tt.wantBodyLen)
			}
		})
	}
}

func TestHeaderRewrites(t *testing.T) {
	resetProviders()
	defer resetProviders()

	BackendHostProvider = func() string { return "custom-host.googleapis.com" }
	UserAgentProvider = func() string { return "custom-default-ua" }

	// 1. Client without antigravity/ UA -> gets replaced by default UA
	req1, _ := http.NewRequest("POST", "/test", nil)
	req1.Header.Set("User-Agent", "python-requests/2.0")
	req1.Header.Set("Connection", "close")
	req1.Header.Set("Authorization", "Old-Auth")
	req1.Header.Set("Accept-Encoding", "gzip, deflate")
	req1.Header.Set("X-Keep", "preserved")

	headers1 := FilterRequestHeaders(req1, "new-token-123", req1.Header.Get("User-Agent"))
	if headers1.Get("Host") != "custom-host.googleapis.com" {
		t.Fatalf("Host = %q, want custom-host.googleapis.com", headers1.Get("Host"))
	}
	if headers1.Get("Authorization") != "Bearer new-token-123" {
		t.Fatalf("Authorization = %q, want 'Bearer new-token-123'", headers1.Get("Authorization"))
	}
	if headers1.Get("Accept-Encoding") != "identity" {
		t.Fatalf("Accept-Encoding = %q, want 'identity'", headers1.Get("Accept-Encoding"))
	}
	if headers1.Get("Connection") != "" {
		t.Fatalf("Connection should be filtered out, got %q", headers1.Get("Connection"))
	}
	if headers1.Get("X-Keep") != "preserved" {
		t.Fatalf("X-Keep should be preserved, got %q", headers1.Get("X-Keep"))
	}

	// 2. Client with antigravity/ UA -> preserved!
	req2, _ := http.NewRequest("POST", "/test", nil)
	req2.Header.Set("User-Agent", "antigravity/cli-1.0")
	headers2 := FilterRequestHeaders(req2, "tok", req2.Header.Get("User-Agent"))
	if headers2.Get("User-Agent") != "antigravity/cli-1.0" {
		t.Fatalf("User-Agent = %q, want 'antigravity/cli-1.0'", headers2.Get("User-Agent"))
	}
}

func TestBufferedResponseSuccessAndCounters(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()

	var upstreamHits int32
	var receivedAuth string
	var receivedPath string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		receivedAuth = r.Header.Get("Authorization")
		receivedPath = r.URL.RequestURI()

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Header", "true")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates": [{"output": "generated text"}]}`))
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }
	TokenRefresher = func(account *storage.Account) (string, error) {
		return "token-for-" + account.ID, nil
	}

	// Setup pool with 1 account
	pool := storage.NewEmptyPool()
	acc := &storage.Account{
		ID:           "acc_1",
		Email:        "dev@example.com",
		RequestCount: 10,
	}
	pool.Accounts = []*storage.Account{acc}
	pool.ActiveAccountID = &acc.ID
	_ = storage.SavePool(pool)

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/models/gemini:generateContent?alt=json", strings.NewReader(`{"prompt":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)
	resp := w.Result()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "generated text") {
		t.Fatalf("body = %q, want 'generated text'", string(body))
	}
	if receivedAuth != "Bearer token-for-acc_1" {
		t.Fatalf("receivedAuth = %q, want 'Bearer token-for-acc_1'", receivedAuth)
	}
	if receivedPath != "/v1/models/gemini:generateContent?alt=json" {
		t.Fatalf("receivedPath = %q", receivedPath)
	}

	// Verify counters updated under transaction
	loaded, _ := storage.LoadPool()
	updatedAcc := storage.FindAccount(loaded, acc)
	if updatedAcc.RequestCount != 11 {
		t.Fatalf("request_count = %d, want 11", updatedAcc.RequestCount)
	}
	if updatedAcc.GetHits() != 1 {
		t.Fatalf("gen_count = %d, want 1", updatedAcc.GetHits())
	}
	if updatedAcc.LastUsedAt == nil || *updatedAcc.LastUsedAt <= 0 {
		t.Fatal("last_used_at should be set for generation")
	}
}

func TestProxy_GenerationTriggersAsyncQuotaRefreshForStaleCandidate(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates": [{"output": "generated text"}]}`))
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }
	TokenRefresher = func(account *storage.Account) (string, error) {
		return "test-token", nil
	}

	refreshed := make(chan string, 1)
	QuotaRefresher = func(account *storage.Account) {
		select {
		case refreshed <- account.ID:
		default:
		}
	}

	staleTime := time.Now().Unix() - 600
	acc := &storage.Account{
		ID:    "acc-stale",
		Email: "stale@example.com",
		LastQuota: &storage.QuotaState{
			UpdatedAt: &staleTime,
		},
	}
	pool := &storage.Pool{
		Accounts:        []*storage.Account{acc},
		ActiveAccountID: &acc.ID,
	}
	_ = storage.SavePool(pool)

	handler := NewHandler()

	// 1. Non-generation request should NOT trigger QuotaRefresher
	nonGenReq := httptest.NewRequest("GET", "/v1internal:models", nil)
	wNonGen := httptest.NewRecorder()
	handler.ServeHTTP(wNonGen, nonGenReq)
	select {
	case id := <-refreshed:
		t.Fatalf("unexpected QuotaRefresher call for non-generation request: %s", id)
	case <-time.After(50 * time.Millisecond):
		// OK
	}

	// 2. Generation request for stale candidate SHOULD trigger QuotaRefresher
	genReq := httptest.NewRequest("POST", "/v1/models/gemini:generateContent", strings.NewReader(`{}`))
	wGen := httptest.NewRecorder()
	handler.ServeHTTP(wGen, genReq)

	select {
	case id := <-refreshed:
		if id != "acc-stale" {
			t.Fatalf("expected QuotaRefresher for acc-stale, got %s", id)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for QuotaRefresher call on stale candidate generation")
	}

	// 3. Generation request for fresh candidate should NOT trigger QuotaRefresher
	freshTime := time.Now().Unix() - 10
	acc.LastQuota.UpdatedAt = &freshTime
	_ = storage.SavePool(pool)

	wFresh := httptest.NewRecorder()
	handler.ServeHTTP(wFresh, genReq)
	select {
	case id := <-refreshed:
		t.Fatalf("unexpected QuotaRefresher call for fresh candidate: %s", id)
	case <-time.After(50 * time.Millisecond):
		// OK
	}
}
