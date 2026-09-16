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
