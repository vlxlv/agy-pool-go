package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func TestTransportFailure_Timeout_NoReplay(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()

	var acc1Hits, acc2Hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "acc_1") {
			atomic.AddInt32(&acc1Hits, 1)
			time.Sleep(150 * time.Millisecond) // Exceeds client timeout
			w.WriteHeader(http.StatusOK)
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

	// Configure client with short timeout
	origClient := HTTPClient
	HTTPClient = &http.Client{
		Timeout:   50 * time.Millisecond,
		Transport: BaseTransport,
	}
	defer func() { HTTPClient = origClient }()

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

	// Timeout must return 504 Gateway Timeout
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 Gateway Timeout", resp.StatusCode)
	}

	// CRITICAL INVARIANT: NO REPLAY on timeout! acc_2 must NOT be called!
	if atomic.LoadInt32(&acc1Hits) != 1 || atomic.LoadInt32(&acc2Hits) != 0 {
		t.Fatalf("NO REPLAY VIOLATED on timeout: acc_1=%d, acc_2=%d", acc1Hits, acc2Hits)
	}
}

func TestTransportFailure_ConnectionRefused_NoReplay(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()

	// Find an unopened local port
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	unopenedPort := l.Addr().String()
	_ = l.Close()

	BackendHostProvider = func() string { return unopenedPort }
	BackendURLProvider = func() string { return "http://" + unopenedPort }
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

	// Connection refused returns 502 Bad Gateway
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 Bad Gateway", resp.StatusCode)
	}
}

func TestClientDisconnect_NoReplay(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()

	upstreamStarted := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(upstreamStarted)
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	}))
	defer upstream.Close()
	defer upstream.CloseClientConnections()

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

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", "/v1/generateContent", strings.NewReader("{}"))
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		NewHandler().ServeHTTP(w, req)
		close(done)
	}()

	<-upstreamStarted
	// Client disconnects
	cancel()

	select {
	case <-done:
		// Succeeded cleanly
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not terminate promptly on client disconnect")
	}
}

func TestNetHttpTransport_NoAutomaticRetry(t *testing.T) {
	// Audit test: verify BaseTransport never automatically replays a request
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&attempts, 1)
		if count == 1 {
			// Hijack and close TCP connection abruptly to simulate broken socket
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("hijack not supported")
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &http.Client{
		Transport: BaseTransport,
		Timeout:   2 * time.Second,
	}

	req, _ := http.NewRequest("POST", server.URL, strings.NewReader(`{"data":"test"}`))
	req.GetBody = nil // Ensure GetBody is nil so net/http cannot rewind body

	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected network error on abruptly closed connection")
	}

	// Invariant: BaseTransport MUST NOT have retried
	if atomic.LoadInt32(&attempts) != 1 {
		t.Fatalf("BaseTransport executed hidden retry! attempts = %d, want 1", attempts)
	}
}

func TestRequestClassification(t *testing.T) {
	tests := []struct {
		url      string
		accept   string
		isGen    bool
		isStream bool
	}{
		{"/v1/models/gemini-pro:generateContent", "", true, true},
		{"/v1/models/gemini-pro:streamGenerateContent", "", true, true},
		{"/v1beta/projects/p/locations/l/publishers/google/models/gemini-1.5:generateContent?alt=sse", "", true, true},
		{"/v1/models", "", false, false},
		{"/v1/models?stream=true", "", false, true},
		{"/v1/models", "text/event-stream", false, true},
		{"/v1/models", "application/json", false, false},
	}

	for _, tt := range tests {
		req, _ := http.NewRequest("POST", tt.url, nil)
		if tt.accept != "" {
			req.Header.Set("Accept", tt.accept)
		}
		if got := IsGenerationRequest(req); got != tt.isGen {
			t.Errorf("IsGenerationRequest(%s) = %v, want %v", tt.url, got, tt.isGen)
		}
		if got := IsStreamRequest(req); got != tt.isStream {
			t.Errorf("IsStreamRequest(%s) = %v, want %v", tt.url, got, tt.isStream)
		}
	}
}

func TestGenerationTransport_NoReplayConfiguration(t *testing.T) {
	if !GenerationTransport.DisableKeepAlives {
		t.Fatal("GenerationTransport must have DisableKeepAlives == true")
	}
	if GenerationTransport.ForceAttemptHTTP2 {
		t.Fatal("GenerationTransport must not force HTTP/2")
	}
	if GenerationTransport.TLSHandshakeTimeout == 0 {
		t.Fatal("GenerationTransport TLSHandshakeTimeout must be set")
	}
}

func TestGenerationRoundTripper_StripsIdempotencyAndGetBody(t *testing.T) {
	var hookCalled bool
	RoundTripHook = func(req *http.Request) {
		hookCalled = true
	}
	defer func() { RoundTripHook = nil }()

	var seenGetBody bool
	var seenIdempotencyKey string
	var seenXIdempotencyKey string

	mockRT := &mockRoundTripper{
		roundTripFunc: func(req *http.Request) (*http.Response, error) {
			seenGetBody = req.GetBody != nil
			seenIdempotencyKey = req.Header.Get("Idempotency-Key")
			seenXIdempotencyKey = req.Header.Get("X-Idempotency-Key")
			return &http.Response{
				StatusCode: 200,
				Body:       http.NoBody,
				Header:     make(http.Header),
			}, nil
		},
	}

	rt := &GenerationRoundTripper{Base: mockRT}
	req, _ := http.NewRequest("POST", "http://example.com/generateContent", strings.NewReader("body"))
	req.Header.Set("Idempotency-Key", "test-key")
	req.Header.Set("X-Idempotency-Key", "test-x-key")

	if req.GetBody == nil {
		t.Fatal("expected NewRequest with strings.Reader to have initialized GetBody")
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}

	if !hookCalled {
		t.Fatal("expected RoundTripHook to be called")
	}
	if seenGetBody {
		t.Fatal("expected GenerationRoundTripper to have cleared GetBody")
	}
	if seenIdempotencyKey != "" {
		t.Fatalf("expected Idempotency-Key to be stripped, got %q", seenIdempotencyKey)
	}
	if seenXIdempotencyKey != "" {
		t.Fatalf("expected X-Idempotency-Key to be stripped, got %q", seenXIdempotencyKey)
	}
}

func TestFilterRequestHeaders_StripsIdempotencyForGeneration(t *testing.T) {
	// Generation request: idempotency headers stripped
	genReq, _ := http.NewRequest("POST", "/v1/generateContent", nil)
	genReq.Header.Set("Idempotency-Key", "gen-key")
	genReq.Header.Set("X-Idempotency-Key", "gen-x-key")
	genReq.Header.Set("X-Custom", "custom-val")

	h1 := FilterRequestHeaders(genReq, "token", "ua")
	if h1.Get("Idempotency-Key") != "" {
		t.Fatal("FilterRequestHeaders must strip Idempotency-Key for generation requests")
	}
	if h1.Get("X-Idempotency-Key") != "" {
		t.Fatal("FilterRequestHeaders must strip X-Idempotency-Key for generation requests")
	}
	if h1.Get("X-Custom") != "custom-val" {
		t.Fatal("FilterRequestHeaders must retain non-idempotency headers")
	}

	// Non-generation request: idempotency headers preserved
	nonGenReq, _ := http.NewRequest("POST", "/v1/models", nil)
	nonGenReq.Header.Set("Idempotency-Key", "non-gen-key")
	h2 := FilterRequestHeaders(nonGenReq, "token", "ua")
	if h2.Get("Idempotency-Key") != "non-gen-key" {
		t.Fatal("FilterRequestHeaders should retain Idempotency-Key for non-generation requests")
	}
}

type mockRoundTripper struct {
	roundTripFunc func(req *http.Request) (*http.Response, error)
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.roundTripFunc(req)
}
