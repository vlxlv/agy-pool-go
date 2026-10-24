package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func TestSSE_ForwardingAndTruncation(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()

	var acc1Hits, acc2Hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "acc_1") {
			atomic.AddInt32(&acc1Hits, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			_, _ = w.Write([]byte("data: event 1\n\n"))
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
			_, _ = w.Write([]byte("data: event 2\n\n"))
			flusher.Flush()
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

	var logBuf bytes.Buffer
	LogWriter = &logBuf

	handler := NewHandler()
	req := httptest.NewRequest("GET", "/v1/streamGenerateContent", nil)
	req.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)
	resp := w.Result()

	// Stream log line is written after the stream ends and carries both timings
	if !regexp.MustCompile(`\(Status: 200, ttfb \S+, total \S+\)`).MatchString(logBuf.String()) {
		t.Errorf("stream log line missing ttfb/total: %q", logBuf.String())
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "data: event 1") || !strings.Contains(string(body), "data: event 2") {
		t.Fatalf("stream body missing events: %q", string(body))
	}

	// CRITICAL: acc_2 must never be contacted
	if atomic.LoadInt32(&acc1Hits) != 1 || atomic.LoadInt32(&acc2Hits) != 0 {
		t.Fatalf("SSE dispatches mismatch: acc1=%d, acc2=%d", acc1Hits, acc2Hits)
	}
}

func TestSSE_CommitPoint_NoReplay(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()

	var acc1Hits, acc2Hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "acc_1") {
			atomic.AddInt32(&acc1Hits, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			_, _ = w.Write([]byte("data: partial\n\n"))
			flusher.Flush()
			// Hijack and abruptly terminate connection mid-stream
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
			}
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
	req := httptest.NewRequest("POST", "/v1/models/gemini:streamGenerateContent", strings.NewReader("{}"))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// Invariant: Once stream headers/data are committed downstream, NEVER replay to acc_2!
	if atomic.LoadInt32(&acc1Hits) != 1 || atomic.LoadInt32(&acc2Hits) != 0 {
		t.Fatalf("stream commit point violated: acc1=%d, acc2=%d", acc1Hits, acc2Hits)
	}
}
