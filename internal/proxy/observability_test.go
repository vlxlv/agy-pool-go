package proxy

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/observability"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// Case A: Simple successful generation
func TestObservability_CaseA_SimpleSuccessfulGeneration(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()
	observability.Reset()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"ok"}`))
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }
	TokenRefresher = func(account *storage.Account) (string, error) { return "token-" + account.ID, nil }

	frac := 0.9
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{
		{ID: "acc_1", LastQuota: &storage.QuotaState{RemainingFraction: &frac}},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader(`{"prompt":"hi"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	snap := observability.GetSnapshot()
	if snap.GenerationRequests != 1 {
		t.Errorf("generation_requests_total = %d, want 1", snap.GenerationRequests)
	}
	if snap.RoutingDecisions != 1 {
		t.Errorf("routing_decisions_total = %d, want 1", snap.RoutingDecisions)
	}
	if snap.GenerationSuccess != 1 {
		t.Errorf("generation_success_total = %d, want 1", snap.GenerationSuccess)
	}
	if snap.FailoverAttempts != 0 {
		t.Errorf("failover_attempts_total = %d, want 0", snap.FailoverAttempts)
	}
	if snap.FailoverSuccess != 0 {
		t.Errorf("failover_success_total = %d, want 0", snap.FailoverSuccess)
	}
	if snap.NoReplayPrevented != 0 {
		t.Errorf("no_replay_prevented_total = %d, want 0", snap.NoReplayPrevented)
	}
}

// Case B: Explicit 429 failover then success
func TestObservability_CaseB_Explicit429FailoverThenSuccess(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()
	observability.Reset()

	var firstHits, secondHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "acc_1") {
			atomic.AddInt32(&firstHits, 1)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Resource has been exhausted"}}`))
			return
		}
		atomic.AddInt32(&secondHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"ok"}`))
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }
	TokenRefresher = func(account *storage.Account) (string, error) { return "token-" + account.ID, nil }

	frac1, frac2 := 0.9, 0.8
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{
		{ID: "acc_1", LastQuota: &storage.QuotaState{RemainingFraction: &frac1}},
		{ID: "acc_2", LastQuota: &storage.QuotaState{RemainingFraction: &frac2}},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader(`{"prompt":"hi"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if atomic.LoadInt32(&firstHits) != 1 || atomic.LoadInt32(&secondHits) != 1 {
		t.Fatalf("expected 1 hit each, got first=%d second=%d", firstHits, secondHits)
	}

	snap := observability.GetSnapshot()
	if snap.GenerationRequests != 1 {
		t.Errorf("generation_requests_total = %d, want 1", snap.GenerationRequests)
	}
	if snap.RoutingDecisions != 2 {
		t.Errorf("routing_decisions_total = %d, want 2", snap.RoutingDecisions)
	}
	if snap.FailoverAttempts != 1 {
		t.Errorf("failover_attempts_total = %d, want 1", snap.FailoverAttempts)
	}
	if snap.FailoverSuccess != 1 {
		t.Errorf("failover_success_total = %d, want 1", snap.FailoverSuccess)
	}
	if snap.GenerationSuccess != 1 {
		t.Errorf("generation_success_total = %d, want 1", snap.GenerationSuccess)
	}
}

// Case C: Validation/auth failover then success
func TestObservability_CaseC_ValidationAuthFailoverThenSuccess(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()
	observability.Reset()

	var firstHits, secondHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "acc_1") {
			atomic.AddInt32(&firstHits, 1)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"validation_required"}}`))
			return
		}
		atomic.AddInt32(&secondHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"ok"}`))
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }
	TokenRefresher = func(account *storage.Account) (string, error) { return "token-" + account.ID, nil }

	frac1, frac2 := 0.9, 0.8
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{
		{ID: "acc_1", LastQuota: &storage.QuotaState{RemainingFraction: &frac1}},
		{ID: "acc_2", LastQuota: &storage.QuotaState{RemainingFraction: &frac2}},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader(`{"prompt":"hi"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	snap := observability.GetSnapshot()
	if snap.GenerationRequests != 1 {
		t.Errorf("generation_requests_total = %d, want 1", snap.GenerationRequests)
	}
	if snap.RoutingDecisions != 2 {
		t.Errorf("routing_decisions_total = %d, want 2", snap.RoutingDecisions)
	}
	if snap.FailoverAttempts != 1 {
		t.Errorf("failover_attempts_total = %d, want 1", snap.FailoverAttempts)
	}
	if snap.FailoverSuccess != 1 {
		t.Errorf("failover_success_total = %d, want 1", snap.FailoverSuccess)
	}
	if snap.GenerationSuccess != 1 {
		t.Errorf("generation_success_total = %d, want 1", snap.GenerationSuccess)
	}
}

// Case D: Ambiguous transport failure
func TestObservability_CaseD_AmbiguousTransportFailure(t *testing.T) {
	setupTwoAccountPool(t)
	defer resetProviders()
	observability.Reset()

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

	snap := observability.GetSnapshot()
	if snap.GenerationRequests != 1 {
		t.Errorf("generation_requests_total = %d, want 1", snap.GenerationRequests)
	}
	if snap.NoReplayPrevented != 1 {
		t.Errorf("no_replay_prevented_total = %d, want 1", snap.NoReplayPrevented)
	}
	if snap.FailoverAttempts != 0 {
		t.Errorf("failover_attempts_total = %d, want 0", snap.FailoverAttempts)
	}
}

// Case E: SSE committed then truncation
func TestObservability_CaseE_SSECommittedThenTruncation(t *testing.T) {
	setupTwoAccountPool(t)
	defer resetProviders()
	observability.Reset()

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
			// Abruptly close mid-stream after commit
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

	if count := atomic.LoadInt32(&acc1Attempts); count != 1 {
		t.Fatalf("acc_1 attempts = %d, want 1", count)
	}
	if count := atomic.LoadInt32(&acc2Attempts); count != 0 {
		t.Fatalf("acc_2 attempts = %d, want 0", count)
	}

	snap := observability.GetSnapshot()
	if snap.GenerationRequests != 1 {
		t.Errorf("generation_requests_total = %d, want 1", snap.GenerationRequests)
	}
	if snap.NoReplayPrevented != 1 {
		t.Errorf("no_replay_prevented_total = %d, want 1", snap.NoReplayPrevented)
	}
	if snap.FailoverAttempts != 0 {
		t.Errorf("failover_attempts_total = %d, want 0", snap.FailoverAttempts)
	}
}

// Case F: All accounts restricted
func TestObservability_CaseF_AllAccountsRestricted(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()
	observability.Reset()

	var upstreamHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }
	TokenRefresher = func(account *storage.Account) (string, error) {
		t.Fatalf("restricted account %s was considered for dispatch", account.ID)
		return "", nil
	}

	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{
		{ID: "validation", Status: "validation_required"},
		{ID: "auth", Status: "auth_error"},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 Service Unavailable", w.Code)
	}
	if hits := atomic.LoadInt32(&upstreamHits); hits != 0 {
		t.Fatalf("upstreamHits = %d, want 0", hits)
	}

	snap := observability.GetSnapshot()
	if snap.GenerationRequests != 1 {
		t.Errorf("generation_requests_total = %d, want 1", snap.GenerationRequests)
	}
	if snap.RestrictedSkips != 2 {
		t.Errorf("restricted_account_skips_total = %d, want 2", snap.RestrictedSkips)
	}
	if snap.RoutingDecisions != 0 {
		t.Errorf("routing_decisions_total = %d, want 0", snap.RoutingDecisions)
	}
}

// Case L (proxy failover test): Persistence failure increments exactly once, failover proceeds
func TestObservability_CaseL_ProxyPersistenceFailure(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()
	observability.Reset()

	var firstHits, secondHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "acc_1") {
			atomic.AddInt32(&firstHits, 1)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"validation_required"}}`))
			return
		}
		atomic.AddInt32(&secondHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"ok"}`))
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }
	TokenRefresher = func(account *storage.Account) (string, error) { return "token-" + account.ID, nil }

	var calls int32
	original := poolTransaction
	t.Cleanup(func() { poolTransaction = original })
	poolTransaction = func(fn func(*storage.Pool) error) error {
		if atomic.AddInt32(&calls, 1) == 1 {
			return errors.New("injected persistence failure")
		}
		return fn(loadPoolForTest(t))
	}

	frac := 0.9
	first := &storage.Account{ID: "acc_1", LastQuota: &storage.QuotaState{RemainingFraction: &frac}}
	second := &storage.Account{ID: "acc_2", LastQuota: &storage.QuotaState{RemainingFraction: &frac}}
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{first, second}
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}

	handler := NewHandler()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1/generateContent", strings.NewReader("{}")))

	if w.Code != http.StatusOK || atomic.LoadInt32(&firstHits) != 1 || atomic.LoadInt32(&secondHits) != 1 {
		t.Fatalf("status=%d first=%d second=%d, want 200/1/1", w.Code, firstHits, secondHits)
	}

	snap := observability.GetSnapshot()
	if snap.PersistenceErrors != 1 {
		t.Errorf("persistence_errors_total = %d, want 1", snap.PersistenceErrors)
	}
}
