package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// Helper: sets up a two-account pool and default token refresher.
func setupTwoAccountPool(t *testing.T) (*storage.Account, *storage.Account) {
	t.Helper()
	setupIsolatedTestDir(t)
	resetProviders()

	pool := storage.NewEmptyPool()
	acc1 := &storage.Account{ID: "acc_1", Email: "1@gmail.com"}
	acc2 := &storage.Account{ID: "acc_2", Email: "2@gmail.com"}
	pool.Accounts = []*storage.Account{acc1, acc2}
	pool.ActiveAccountID = &acc1.ID
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("failed to save pool: %v", err)
	}

	TokenRefresher = func(account *storage.Account) (string, error) {
		return "token-" + account.ID, nil
	}
	return acc1, acc2
}

// Helper: awaits goroutine count stabilization to detect leaks.
func assertNoGoroutineLeak(t *testing.T, initial int) {
	t.Helper()
	for i := 0; i < 30; i++ {
		if runtime.NumGoroutine() <= initial {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if current := runtime.NumGoroutine(); current > initial {
		t.Fatalf("goroutine leak: initial=%d, final=%d", initial, current)
	}
}

// 1. EOF before headers
func TestTransportFailure_EOFBeforeHeaders_NoReplay(t *testing.T) {
	setupTwoAccountPool(t)
	defer resetProviders()

	var physicalAttempts int32
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer l.Close()

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&physicalAttempts, 1)
			// Read a small piece of request and close immediately before sending any headers
			buf := make([]byte, 128)
			_, _ = conn.Read(buf)
			_ = conn.Close()
		}
	}()

	BackendHostProvider = func() string { return l.Addr().String() }
	BackendURLProvider = func() string { return "http://" + l.Addr().String() }

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader(`{"prompt":"hi"}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	resp := rec.Result()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 Bad Gateway", resp.StatusCode)
	}
	if count := atomic.LoadInt32(&physicalAttempts); count != 1 {
		t.Fatalf("expected exactly 1 physical attempt on EOF, got %d", count)
	}
}

// 2. TCP Connection Reset (RST)
func TestTransportFailure_ConnectionReset_NoReplay(t *testing.T) {
	setupTwoAccountPool(t)
	defer resetProviders()

	var physicalAttempts int32
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer l.Close()

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&physicalAttempts, 1)
			buf := make([]byte, 128)
			_, _ = conn.Read(buf)
			if tcpConn, ok := conn.(*net.TCPConn); ok {
				_ = tcpConn.SetLinger(0) // Forces TCP RST on Close
			}
			_ = conn.Close()
		}
	}()

	BackendHostProvider = func() string { return l.Addr().String() }
	BackendURLProvider = func() string { return "http://" + l.Addr().String() }

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader(`{"prompt":"hi"}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	resp := rec.Result()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 Bad Gateway", resp.StatusCode)
	}
	if count := atomic.LoadInt32(&physicalAttempts); count != 1 {
		t.Fatalf("expected exactly 1 physical attempt on connection reset, got %d", count)
	}
}

// 3. Broken Pipe during request body
func TestTransportFailure_BrokenPipe_NoReplay(t *testing.T) {
	setupTwoAccountPool(t)
	defer resetProviders()

	var physicalAttempts int32
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer l.Close()

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&physicalAttempts, 1)
			if tcpConn, ok := conn.(*net.TCPConn); ok {
				_ = tcpConn.SetLinger(0)
			}
			_ = conn.Close() // Close immediately before body can finish writing
		}
	}()

	BackendHostProvider = func() string { return l.Addr().String() }
	BackendURLProvider = func() string { return "http://" + l.Addr().String() }

	handler := NewHandler()
	largeBody := strings.Repeat("x", 65536)
	req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader(largeBody))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	resp := rec.Result()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 Bad Gateway", resp.StatusCode)
	}
	if count := atomic.LoadInt32(&physicalAttempts); count != 1 {
		t.Fatalf("expected exactly 1 physical attempt on broken pipe, got %d", count)
	}
}

// 4. TLS handshake / certificate failure
func TestTransportFailure_TLSFailure_NoReplay(t *testing.T) {
	setupTwoAccountPool(t)
	defer resetProviders()

	var physicalAttempts int32
	// Server returns raw plaintext to TLS client
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer l.Close()

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&physicalAttempts, 1)
			_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
			_ = conn.Close()
		}
	}()

	BackendHostProvider = func() string { return l.Addr().String() }
	BackendURLProvider = func() string { return "https://" + l.Addr().String() }

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	resp := rec.Result()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 Bad Gateway", resp.StatusCode)
	}
	if count := atomic.LoadInt32(&physicalAttempts); count != 1 {
		t.Fatalf("expected exactly 1 physical attempt on TLS failure, got %d", count)
	}
}

// 5. Malformed HTTP response status/headers
func TestTransportFailure_MalformedResponse_NoReplay(t *testing.T) {
	setupTwoAccountPool(t)
	defer resetProviders()

	var physicalAttempts int32
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer l.Close()

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&physicalAttempts, 1)
			buf := make([]byte, 256)
			_, _ = conn.Read(buf)
			_, _ = conn.Write([]byte("INVALID_HTTP_STATUS_LINE\r\n\r\n"))
			_ = conn.Close()
		}
	}()

	BackendHostProvider = func() string { return l.Addr().String() }
	BackendURLProvider = func() string { return "http://" + l.Addr().String() }

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	resp := rec.Result()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 Bad Gateway", resp.StatusCode)
	}
	if count := atomic.LoadInt32(&physicalAttempts); count != 1 {
		t.Fatalf("expected exactly 1 physical attempt on malformed response, got %d", count)
	}
}

// 6. Truncated Content-Length (Content-Length: 100, sent 20 bytes then EOF)
func TestTransportFailure_TruncatedBufferedBody_NoReplay(t *testing.T) {
	setupTwoAccountPool(t)
	defer resetProviders()

	var physicalAttempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&physicalAttempts, 1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("hijacker not supported")
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack failed: %v", err)
		}
		// Write header claiming 100 bytes, but send only 20 bytes and close
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\nContent-Type: application/json\r\n\r\n12345678901234567890")
		_ = buf.Flush()
		_ = conn.Close()
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return server.URL }

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/models", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	resp := rec.Result()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 Bad Gateway", resp.StatusCode)
	}
	if count := atomic.LoadInt32(&physicalAttempts); count != 1 {
		t.Fatalf("expected exactly 1 physical attempt on truncated buffered body, got %d", count)
	}
}

// 7. Reused connection failure: connection reuse must not cause generation request replay
func TestTransportFailure_ReusedConnectionFailure_NoReplay(t *testing.T) {
	setupTwoAccountPool(t)
	defer resetProviders()

	var requestCount int32
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer l.Close()

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					count := atomic.AddInt32(&requestCount, 1)
					if count == 1 {
						// First request succeeds with keep-alive
						_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nOK"))
					} else {
						// Second request: close connection abruptly without reply
						if tcpConn, ok := c.(*net.TCPConn); ok {
							_ = tcpConn.SetLinger(0)
						}
						return
					}
					_ = n
				}
			}(conn)
		}
	}()

	// Real http.Transport with connection reuse enabled
	reusedTransport := &http.Transport{
		DisableKeepAlives: false,
		MaxIdleConns:      10,
		IdleConnTimeout:   10 * time.Second,
	}
	reusedClient := &http.Client{
		Transport: reusedTransport,
		Timeout:   2 * time.Second,
	}

	// First request primes the idle connection
	primeReq, _ := http.NewRequest("GET", "http://"+l.Addr().String()+"/health", nil)
	primeResp, err := reusedClient.Do(primeReq)
	if err != nil {
		t.Fatalf("prime request failed: %v", err)
	}
	_, _ = io.ReadAll(primeResp.Body)
	_ = primeResp.Body.Close()

	// Ensure connection was idle
	if atomic.LoadInt32(&requestCount) != 1 {
		t.Fatalf("expected requestCount=1 after prime, got %d", requestCount)
	}

	// Now send generation request using GenerationRoundTripper on reusedTransport
	genClient := &http.Client{
		Transport: &GenerationRoundTripper{Base: reusedTransport},
		Timeout:   2 * time.Second,
	}

	var hookAttempts int32
	RoundTripHook = func(r *http.Request) {
		atomic.AddInt32(&hookAttempts, 1)
	}
	defer func() { RoundTripHook = nil }()

	genReq, _ := http.NewRequest("POST", "http://"+l.Addr().String()+"/v1/generateContent", strings.NewReader(`{"prompt":"hello"}`))
	genReq.Header.Set("Idempotency-Key", "should-be-stripped")

	resp, err := genClient.Do(genReq)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected error on abrupt closed connection")
	}

	// CRITICAL: Must be attempted exactly ONCE! No automatic transport replay!
	if atomic.LoadInt32(&hookAttempts) != 1 {
		t.Fatalf("RoundTripHook attempts = %d, want 1", hookAttempts)
	}
	if total := atomic.LoadInt32(&requestCount); total != 2 {
		t.Fatalf("server received %d total requests (want 2: 1 prime + 1 generation)", total)
	}
}

// 8. SSE Truncation mid-stream: client receives partial events, no cross-account fallback
func TestSSE_TruncationMidStream_NoReplay(t *testing.T) {
	setupTwoAccountPool(t)
	defer resetProviders()

	var acc1Attempts, acc2Attempts int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if strings.Contains(authHeader, "acc_1") {
			atomic.AddInt32(&acc1Attempts, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			_, _ = w.Write([]byte("data: event 1\n\n"))
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
			_, _ = w.Write([]byte("data: event 2\n\n"))
			flusher.Flush()
			// Abruptly close
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
			}
			return
		}
		atomic.AddInt32(&acc2Attempts, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/streamGenerateContent", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	resp := rec.Result()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 OK (headers were committed)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "data: event 1") || !strings.Contains(bodyStr, "data: event 2") {
		t.Fatalf("expected partial events in body, got: %q", bodyStr)
	}

	if count := atomic.LoadInt32(&acc1Attempts); count != 1 {
		t.Fatalf("acc_1 attempts = %d, want 1", count)
	}
	if count := atomic.LoadInt32(&acc2Attempts); count != 0 {
		t.Fatalf("acc_2 attempts = %d, want 0 (cross-account replay forbidden)", count)
	}
}

// 9. SSE Connection Reset mid-stream
func TestSSE_ConnectionResetMidStream_NoReplay(t *testing.T) {
	setupTwoAccountPool(t)
	defer resetProviders()

	var acc1Attempts, acc2Attempts int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "acc_1") {
			atomic.AddInt32(&acc1Attempts, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			_, _ = w.Write([]byte("data: partial\n\n"))
			flusher.Flush()
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				if tcpConn, ok := conn.(*net.TCPConn); ok {
					_ = tcpConn.SetLinger(0)
				}
				_ = conn.Close()
			}
			return
		}
		atomic.AddInt32(&acc2Attempts, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/streamGenerateContent", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if count := atomic.LoadInt32(&acc1Attempts); count != 1 {
		t.Fatalf("acc_1 attempts = %d, want 1", count)
	}
	if count := atomic.LoadInt32(&acc2Attempts); count != 0 {
		t.Fatalf("acc_2 attempts = %d, want 0", count)
	}
}

// 10. SSE Timeout mid-stream
func TestSSE_TimeoutMidStream_NoReplay(t *testing.T) {
	setupTwoAccountPool(t)
	defer resetProviders()

	var acc1Attempts, acc2Attempts int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "acc_1") {
			atomic.AddInt32(&acc1Attempts, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			_, _ = w.Write([]byte("data: starting\n\n"))
			flusher.Flush()
			time.Sleep(200 * time.Millisecond) // exceeds client timeout
			_, _ = w.Write([]byte("data: done\n\n"))
			flusher.Flush()
			return
		}
		atomic.AddInt32(&acc2Attempts, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }

	// Short timeout client
	HTTPClient = &http.Client{
		Transport: BaseTransport,
		Timeout:   50 * time.Millisecond,
	}

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/streamGenerateContent", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if count := atomic.LoadInt32(&acc1Attempts); count != 1 {
		t.Fatalf("acc_1 attempts = %d, want 1", count)
	}
	if count := atomic.LoadInt32(&acc2Attempts); count != 0 {
		t.Fatalf("acc_2 attempts = %d, want 0", count)
	}
}

// 11. Client disconnect after stream committed
func TestClientDisconnect_AfterStreamCommitted_NoReplay(t *testing.T) {
	setupTwoAccountPool(t)
	defer resetProviders()

	var acc1Attempts, acc2Attempts int32
	var upstreamReqSeen *http.Request
	RoundTripHook = func(r *http.Request) {
		upstreamReqSeen = r
	}
	defer func() { RoundTripHook = nil }()

	streamStarted := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "acc_1") {
			atomic.AddInt32(&acc1Attempts, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			_, _ = w.Write([]byte("data: start\n\n"))
			flusher.Flush()
			close(streamStarted)
			select {
			case <-r.Context().Done():
			case <-time.After(500 * time.Millisecond):
			}
			return
		}
		atomic.AddInt32(&acc2Attempts, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	defer upstream.CloseClientConnections()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", "/v1/streamGenerateContent", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		NewHandler().ServeHTTP(rec, req)
		close(done)
	}()

	<-streamStarted
	cancel() // Downstream client cancels after stream begins

	select {
	case <-done:
		// Succeeded: proxy handler exited promptly
	case <-time.After(1 * time.Second):
		t.Fatal("handler did not exit promptly after client disconnect")
	}

	if upstreamReqSeen == nil || upstreamReqSeen.Context().Err() == nil {
		t.Fatal("expected upstream request context to be canceled on client disconnect")
	}
	if count := atomic.LoadInt32(&acc1Attempts); count != 1 {
		t.Fatalf("acc_1 attempts = %d, want 1", count)
	}
	if count := atomic.LoadInt32(&acc2Attempts); count != 0 {
		t.Fatalf("acc_2 attempts = %d, want 0 (no replay on client disconnect)", count)
	}
}

// 12. Client disconnect before commit
func TestClientDisconnect_BeforeCommit_NoReplay(t *testing.T) {
	setupTwoAccountPool(t)
	defer resetProviders()

	var acc1Attempts, acc2Attempts int32
	var upstreamReqSeen *http.Request
	RoundTripHook = func(r *http.Request) {
		upstreamReqSeen = r
	}
	defer func() { RoundTripHook = nil }()

	upstreamEntered := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "acc_1") {
			atomic.AddInt32(&acc1Attempts, 1)
			close(upstreamEntered)
			select {
			case <-r.Context().Done():
			case <-time.After(500 * time.Millisecond):
			}
			return
		}
		atomic.AddInt32(&acc2Attempts, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	defer upstream.CloseClientConnections()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", "/v1/generateContent", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		NewHandler().ServeHTTP(rec, req)
		close(done)
	}()

	<-upstreamEntered
	cancel() // Cancel before headers are written downstream

	select {
	case <-done:
		// Handler terminated promptly
	case <-time.After(1 * time.Second):
		t.Fatal("handler did not terminate promptly")
	}

	if upstreamReqSeen == nil || upstreamReqSeen.Context().Err() == nil {
		t.Fatal("expected upstream request context to be canceled on client disconnect")
	}
	if count := atomic.LoadInt32(&acc1Attempts); count != 1 {
		t.Fatalf("acc_1 attempts = %d, want 1", count)
	}
	if count := atomic.LoadInt32(&acc2Attempts); count != 0 {
		t.Fatalf("acc_2 attempts = %d, want 0", count)
	}
}

// 13. Explicit HTTP Failover (429, 401, validation_required) preserved
func TestExplicitFailover_429AndAuthPreserved(t *testing.T) {
	setupTwoAccountPool(t)
	defer resetProviders()

	var acc1Attempts, acc2Attempts int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "acc_1") {
			atomic.AddInt32(&acc1Attempts, 1)
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"quota exceeded"}}`))
			return
		}
		atomic.AddInt32(&acc2Attempts, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[{"text":"success from acc_2"}]}`))
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	resp := rec.Result()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 OK after failover", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "success from acc_2") {
		t.Fatalf("expected response from acc_2, got: %q", string(body))
	}

	// Both accounts attempted exactly once
	if count := atomic.LoadInt32(&acc1Attempts); count != 1 {
		t.Fatalf("acc_1 attempts = %d, want 1", count)
	}
	if count := atomic.LoadInt32(&acc2Attempts); count != 1 {
		t.Fatalf("acc_2 attempts = %d, want 1", count)
	}
}

// 14. Ordinary HTTP errors (400, 404, 500) forwarded directly without replay
func TestExplicitOrdinaryError_NoReplay(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(fmt.Sprintf("HTTP_%d", code), func(t *testing.T) {
			setupTwoAccountPool(t)
			defer resetProviders()

			var acc1Attempts, acc2Attempts int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.Header.Get("Authorization"), "acc_1") {
					atomic.AddInt32(&acc1Attempts, 1)
					w.WriteHeader(code)
					_, _ = w.Write([]byte(fmt.Sprintf(`{"error":"code %d"}`, code)))
					return
				}
				atomic.AddInt32(&acc2Attempts, 1)
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()

			u, _ := url.Parse(upstream.URL)
			BackendHostProvider = func() string { return u.Host }
			BackendURLProvider = func() string { return upstream.URL }

			handler := NewHandler()
			req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader(`{}`))
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)
			resp := rec.Result()

			if resp.StatusCode != code {
				t.Fatalf("status = %d, want %d", resp.StatusCode, code)
			}
			if count := atomic.LoadInt32(&acc1Attempts); count != 1 {
				t.Fatalf("acc_1 attempts = %d, want 1", count)
			}
			if count := atomic.LoadInt32(&acc2Attempts); count != 0 {
				t.Fatalf("acc_2 attempts = %d, want 0 (no replay on ordinary %d)", count, code)
			}
		})
	}
}

// 15. Round-Robin cursor is NOT rolled back on ambiguous failure
func TestRoundRobin_CursorNotRolledBackOnFailure(t *testing.T) {
	setupTwoAccountPool(t)
	defer resetProviders()

	_ = storage.PoolTransaction(func(pool *storage.Pool) error {
		pool.Strategy = "round_robin"
		return nil
	})

	var acc1Attempts, acc2Attempts int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "acc_1") {
			atomic.AddInt32(&acc1Attempts, 1)
			// Ambrupt close -> ambiguous failure
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
			}
			return
		}
		atomic.AddInt32(&acc2Attempts, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"acc2_success"}`))
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }

	handler := NewHandler()

	// Request 1: targets acc_1, fails ambiguously -> 502
	req1 := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader(`{}`))
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	if rec1.Result().StatusCode != http.StatusBadGateway {
		t.Fatalf("req1 status = %d, want 502 Bad Gateway", rec1.Result().StatusCode)
	}
	if count := atomic.LoadInt32(&acc1Attempts); count != 1 {
		t.Fatalf("req1 acc_1 attempts = %d, want 1", count)
	}
	if count := atomic.LoadInt32(&acc2Attempts); count != 0 {
		t.Fatalf("req1 acc_2 attempts = %d, want 0", count)
	}

	// Request 2: RR cursor was NOT rolled back, so it must pick acc_2!
	req2 := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader(`{}`))
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Result().StatusCode != http.StatusOK {
		t.Fatalf("req2 status = %d, want 200 OK from acc_2", rec2.Result().StatusCode)
	}
	if count := atomic.LoadInt32(&acc2Attempts); count != 1 {
		t.Fatalf("req2 acc_2 attempts = %d, want 1", count)
	}
}

// 16. Counter semantics: consistent accounting, no double-counting
func TestCounterSemantics_Consistent(t *testing.T) {
	setupTwoAccountPool(t)
	defer resetProviders()

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Result().StatusCode)
	}

	pool, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	acc := pool.Accounts[0]
	if acc.RequestCount != 1 {
		t.Fatalf("RequestCount = %d, want 1", acc.RequestCount)
	}
	if acc.GenCount == nil || *acc.GenCount != 1 {
		t.Fatalf("GenCount = %v, want 1", acc.GenCount)
	}
}

// 17. Goroutine leak check for SSE and transport cancellation
func TestGoroutineLeak_SSEAndCancellation(t *testing.T) {
	setupTwoAccountPool(t)
	defer resetProviders()

	initialGoroutines := runtime.NumGoroutine()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("data: heartbeat\n\n"))
		flusher.Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }

	// Run multiple client connect/disconnect cycles

	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "POST", "/v1/streamGenerateContent", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			NewHandler().ServeHTTP(rec, req)
			close(done)
		}()

		time.Sleep(15 * time.Millisecond)
		cancel()
		<-done
	}

	upstream.CloseClientConnections()
	upstream.Close()

	assertNoGoroutineLeak(t, initialGoroutines)
}
