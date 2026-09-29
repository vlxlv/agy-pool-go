package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// 1. At most ONE confirmation attempt in flight per process.
// While a confirmation request is in flight for pending_1, a concurrent request
// must bypass pending_1 and route to the healthy account.
func TestVerificationPending_OnlyOneConfirmationInFlight(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()
	ClearAllVerificationConfirmationClaims()
	t.Cleanup(ClearAllVerificationConfirmationClaims)

	pendingEntered := make(chan struct{})
	proceedPending := make(chan struct{})
	var pendingHits, healthyHits int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if strings.Contains(authHeader, "pending_1") {
			atomic.AddInt32(&pendingHits, 1)
			close(pendingEntered)
			<-proceedPending
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result":"pending_success"}`))
			return
		}
		if strings.Contains(authHeader, "healthy_1") {
			atomic.AddInt32(&healthyHits, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result":"healthy_success"}`))
			return
		}
		http.Error(w, "unknown account", http.StatusBadRequest)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }
	TokenRefresher = func(account *storage.Account) (string, error) {
		return "token-" + account.ID, nil
	}

	fraction := 0.9
	pool := storage.NewEmptyPool()
	pAcc := &storage.Account{
		ID:        "pending_1",
		Email:     "pending@example.com",
		Status:    storage.StatusVerificationPending,
		LastQuota: &storage.QuotaState{RemainingFraction: &fraction},
	}
	hAcc := &storage.Account{
		ID:        "healthy_1",
		Email:     "healthy@example.com",
		Status:    "",
		LastQuota: &storage.QuotaState{RemainingFraction: &fraction},
	}
	pool.Accounts = []*storage.Account{pAcc, hAcc}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	handler := NewHandler()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := httptest.NewRequest("POST", "/v1/models/gemini:generateContent", strings.NewReader("{}"))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("req1 expected 200, got %d: %s", w.Code, w.Body.String())
		}
	}()

	// Wait until req1 has claimed pending_1 and entered upstream
	select {
	case <-pendingEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for req1 to reach upstream")
	}

	// Verify that pending_1 is currently claimed
	if !IsVerificationConfirmationClaimed("pending_1") {
		t.Fatal("expected pending_1 to be claimed while req1 is in flight")
	}

	// Now launch req2 while req1 is still in flight
	req2 := httptest.NewRequest("POST", "/v1/models/gemini:generateContent", strings.NewReader("{}"))
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("req2 expected 200, got %d: %s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "healthy_success") {
		t.Fatalf("req2 expected healthy_success, got: %s", w2.Body.String())
	}
	if atomic.LoadInt32(&healthyHits) != 1 {
		t.Fatalf("expected healthyHits = 1, got %d", atomic.LoadInt32(&healthyHits))
	}

	// Now release req1 to finish
	close(proceedPending)
	wg.Wait()

	if atomic.LoadInt32(&pendingHits) != 1 {
		t.Fatalf("expected pendingHits = 1, got %d", atomic.LoadInt32(&pendingHits))
	}

	// Verify claim is released
	if IsVerificationConfirmationClaimed("pending_1") {
		t.Fatal("expected pending_1 claim to be released after completion")
	}

	// Verify persisted status transitioned to Ready ("")
	storedPool, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	storedPending := storage.FindAccount(storedPool, pAcc)
	if storedPending == nil {
		t.Fatal("storedPending not found")
	}
	if storedPending.Status != "" {
		t.Fatalf("persisted Status = %q, want empty (Ready)", storedPending.Status)
	}
}

// 2. Multiple pending accounts: at most ONE pending account is tried per request.
// If pending_1 fails with 403, failover must go directly to healthy_1, NOT to pending_2.
func TestVerificationPending_MultiplePendingOnlyOneTriedPerRequest(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()
	ClearAllVerificationConfirmationClaims()
	t.Cleanup(ClearAllVerificationConfirmationClaims)

	var p1Hits, p2Hits, hHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if strings.Contains(authHeader, "pending_1") {
			atomic.AddInt32(&p1Hits, 1)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"validation_required","details":[{"url":"https://accounts.google.com/verify?id=fresh"}]}}`))
			return
		}
		if strings.Contains(authHeader, "pending_2") {
			atomic.AddInt32(&p2Hits, 1)
			t.Errorf("pending_2 was dispatched! Multiple pending accounts must not be swept in one request")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if strings.Contains(authHeader, "healthy_1") {
			atomic.AddInt32(&hHits, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result":"ok"}`))
			return
		}
		http.Error(w, "unknown", http.StatusBadRequest)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }
	TokenRefresher = func(account *storage.Account) (string, error) {
		return "token-" + account.ID, nil
	}

	fraction := 0.9
	pool := storage.NewEmptyPool()
	p1 := &storage.Account{
		ID:        "pending_1",
		Email:     "p1@example.com",
		Status:    storage.StatusVerificationPending,
		LastQuota: &storage.QuotaState{RemainingFraction: &fraction},
	}
	p2 := &storage.Account{
		ID:        "pending_2",
		Email:     "p2@example.com",
		Status:    storage.StatusVerificationPending,
		LastQuota: &storage.QuotaState{RemainingFraction: &fraction},
	}
	h1 := &storage.Account{
		ID:        "healthy_1",
		Email:     "h1@example.com",
		Status:    "",
		LastQuota: &storage.QuotaState{RemainingFraction: &fraction},
	}
	pool.Accounts = []*storage.Account{p1, p2, h1}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/models/gemini:generateContent", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after failover, got %d: %s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&p1Hits) != 1 {
		t.Fatalf("expected p1Hits = 1, got %d", atomic.LoadInt32(&p1Hits))
	}
	if atomic.LoadInt32(&p2Hits) != 0 {
		t.Fatalf("expected p2Hits = 0, got %d", atomic.LoadInt32(&p2Hits))
	}
	if atomic.LoadInt32(&hHits) != 1 {
		t.Fatalf("expected hHits = 1, got %d", atomic.LoadInt32(&hHits))
	}

	// Verify authoritative storage
	storedPool, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	storedP1 := storage.FindAccount(storedPool, p1)
	if storedP1.Status != storage.StatusValidationRequired {
		t.Fatalf("storedP1 status = %q, want validation_required", storedP1.Status)
	}
	storedP2 := storage.FindAccount(storedPool, p2)
	if storedP2.Status != storage.StatusVerificationPending {
		t.Fatalf("storedP2 status = %q, want verification_pending", storedP2.Status)
	}

	// Verify all claims released
	if IsVerificationConfirmationClaimed("pending_1") || IsVerificationConfirmationClaimed("pending_2") {
		t.Fatal("expected all confirmation claims to be released")
	}
}

// 3. Pre-classification transport failure: account remains verification_pending
// and in-flight confirmation claim is released.
func TestVerificationPending_TransportFailureRemainsPending(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()
	ClearAllVerificationConfirmationClaims()
	t.Cleanup(ClearAllVerificationConfirmationClaims)

	// Create listener that immediately closes accepted connections
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	defer listener.Close()

	BackendHostProvider = func() string { return listener.Addr().String() }
	BackendURLProvider = func() string { return "http://" + listener.Addr().String() }
	TokenRefresher = func(account *storage.Account) (string, error) {
		return "token-" + account.ID, nil
	}

	fraction := 0.9
	pool := storage.NewEmptyPool()
	p1 := &storage.Account{
		ID:        "pending_1",
		Email:     "p1@example.com",
		Status:    storage.StatusVerificationPending,
		LastQuota: &storage.QuotaState{RemainingFraction: &fraction},
	}
	pool.Accounts = []*storage.Account{p1}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/models/gemini:generateContent", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should fail with 502/503
	if w.Code == http.StatusOK {
		t.Fatalf("expected non-200 on transport drop, got %d", w.Code)
	}

	// Invariant: Status remains verification_pending
	storedPool, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	storedP1 := storage.FindAccount(storedPool, p1)
	if storedP1.Status != storage.StatusVerificationPending {
		t.Fatalf("storedP1 status = %q, want verification_pending", storedP1.Status)
	}

	// Claim must be released
	if IsVerificationConfirmationClaimed("pending_1") {
		t.Fatal("expected claim to be released on transport failure")
	}
}

// 4. Upstream 200 OK, but concurrent writer stored validation_required:
// newer validation_required wins and is not overwritten by RecordSuccess.
func TestVerificationPending_SuccessRaceWithNewValidation(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()
	ClearAllVerificationConfirmationClaims()
	t.Cleanup(ClearAllVerificationConfirmationClaims)

	fraction := 0.9
	valURL := "https://accounts.google.com/verify?id=race_val"
	pool := storage.NewEmptyPool()
	p1 := &storage.Account{
		ID:            "pending_1",
		Email:         "p1@example.com",
		Status:        storage.StatusValidationRequired,
		ValidationURL: &valURL,
		LastQuota:     &storage.QuotaState{RemainingFraction: &fraction},
	}
	pool.Accounts = []*storage.Account{p1}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	// Simulate RecordSuccess called with the account that was verification_pending at dispatch time
	dispatchAcc := &storage.Account{
		ID:     "pending_1",
		Status: storage.StatusVerificationPending,
	}

	err := RecordSuccess(dispatchAcc, true)
	if err != nil {
		t.Fatalf("RecordSuccess failed: %v", err)
	}

	// Verify that storage was NOT overwritten to ""
	storedPool, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	stored := storage.FindAccount(storedPool, dispatchAcc)
	if stored.Status != storage.StatusValidationRequired {
		t.Fatalf("persisted Status = %q, want validation_required", stored.Status)
	}
	if stored.ValidationURL == nil || *stored.ValidationURL != valURL {
		t.Fatalf("persisted ValidationURL was corrupted: %v", stored.ValidationURL)
	}
}

// 5. Upstream 200 OK, but concurrent writer stored auth_error:
// newer auth_error wins and is not overwritten by RecordSuccess.
func TestVerificationPending_SuccessRaceWithAuthError(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()
	ClearAllVerificationConfirmationClaims()
	t.Cleanup(ClearAllVerificationConfirmationClaims)

	fraction := 0.9
	pool := storage.NewEmptyPool()
	p1 := &storage.Account{
		ID:        "pending_1",
		Email:     "p1@example.com",
		Status:    storage.StatusAuthError,
		LastQuota: &storage.QuotaState{RemainingFraction: &fraction},
	}
	pool.Accounts = []*storage.Account{p1}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	dispatchAcc := &storage.Account{
		ID:     "pending_1",
		Status: storage.StatusVerificationPending,
	}

	err := RecordSuccess(dispatchAcc, true)
	if err != nil {
		t.Fatalf("RecordSuccess failed: %v", err)
	}

	storedPool, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	stored := storage.FindAccount(storedPool, dispatchAcc)
	if stored.Status != storage.StatusAuthError {
		t.Fatalf("persisted Status = %q, want auth_error", stored.Status)
	}
}

// 6. Claim is released on EVERY exit path:
// - 2xx generation success
// - 403 validation_required
// - 401 auth_error
// - Transport error
// - Context cancellation
func TestVerificationPending_ClaimReleasedOnEveryExitPath(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       string
		cancelCtx  bool
		dropConn   bool
	}{
		{name: "2xx_success", statusCode: http.StatusOK, body: `{"result":"ok"}`},
		{name: "403_validation_required", statusCode: http.StatusForbidden, body: `{"error":{"message":"validation_required"}}`},
		{name: "401_auth_error", statusCode: http.StatusUnauthorized, body: `{"error":{"message":"invalid_grant"}}`},
		{name: "transport_error", dropConn: true},
		{name: "context_canceled", cancelCtx: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupIsolatedTestDir(t)
			resetProviders()
			defer resetProviders()
			ClearAllVerificationConfirmationClaims()
			t.Cleanup(ClearAllVerificationConfirmationClaims)

			var upstream *httptest.Server
			if tc.dropConn {
				upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hj, ok := w.(http.Hijacker)
					if ok {
						conn, _, _ := hj.Hijack()
						_ = conn.Close()
					}
				}))
			} else if tc.cancelCtx {
				upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					select {
					case <-r.Context().Done():
					case <-time.After(500 * time.Millisecond):
					}
				}))
			} else {
				upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.statusCode)
					_, _ = w.Write([]byte(tc.body))
				}))
			}
			defer upstream.Close()
			defer upstream.CloseClientConnections()

			u, _ := url.Parse(upstream.URL)
			BackendHostProvider = func() string { return u.Host }
			BackendURLProvider = func() string { return upstream.URL }
			TokenRefresher = func(account *storage.Account) (string, error) {
				return "token-" + account.ID, nil
			}

			fraction := 0.9
			pool := storage.NewEmptyPool()
			p1 := &storage.Account{
				ID:        "pending_exit_test",
				Email:     "exit@example.com",
				Status:    storage.StatusVerificationPending,
				LastQuota: &storage.QuotaState{RemainingFraction: &fraction},
			}
			pool.Accounts = []*storage.Account{p1}
			if err := storage.SavePool(pool); err != nil {
				t.Fatalf("SavePool failed: %v", err)
			}

			handler := NewHandler()

			var req *http.Request
			if tc.cancelCtx {
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer cancel()
				req = httptest.NewRequestWithContext(ctx, "POST", "/v1/models/gemini:generateContent", strings.NewReader("{}"))
			} else {
				req = httptest.NewRequest("POST", "/v1/models/gemini:generateContent", strings.NewReader("{}"))
			}

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			// Invariant: Claim MUST be released on every exit path
			if IsVerificationConfirmationClaimed("pending_exit_test") {
				t.Fatalf("[%s] confirmation claim was not released!", tc.name)
			}
		})
	}
}

// 7. No replay after commit:
// If downstream stream headers/data are committed and upstream drops mid-stream,
// failover to healthy account is strictly prohibited.
func TestVerificationPending_NoReplayAfterCommit(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()
	ClearAllVerificationConfirmationClaims()
	t.Cleanup(ClearAllVerificationConfirmationClaims)

	var p1Hits, h1Hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if strings.Contains(authHeader, "pending_1") {
			atomic.AddInt32(&p1Hits, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, ok := w.(http.Flusher)
			if ok {
				_, _ = w.Write([]byte("data: partial-chunk\n\n"))
				flusher.Flush()
			}
			// Abruptly terminate connection mid-stream after commit
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
			}
			return
		}
		if strings.Contains(authHeader, "healthy_1") {
			atomic.AddInt32(&h1Hits, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result":"should_never_be_reached"}`))
			return
		}
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }
	TokenRefresher = func(account *storage.Account) (string, error) {
		return "token-" + account.ID, nil
	}

	fraction := 0.9
	pool := storage.NewEmptyPool()
	p1 := &storage.Account{
		ID:        "pending_1",
		Email:     "pending@example.com",
		Status:    storage.StatusVerificationPending,
		LastQuota: &storage.QuotaState{RemainingFraction: &fraction},
	}
	h1 := &storage.Account{
		ID:        "healthy_1",
		Email:     "healthy@example.com",
		Status:    "",
		LastQuota: &storage.QuotaState{RemainingFraction: &fraction},
	}
	pool.Accounts = []*storage.Account{p1, h1}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	handler := NewHandler()
	req := httptest.NewRequest("POST", "/v1/models/gemini:streamGenerateContent", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Invariant: Once stream is committed, never failover/replay to healthy_1!
	if atomic.LoadInt32(&p1Hits) != 1 {
		t.Fatalf("expected p1Hits = 1, got %d", atomic.LoadInt32(&p1Hits))
	}
	if atomic.LoadInt32(&h1Hits) != 0 {
		t.Fatalf("stream committed replay violation: healthy_1 was hit %d times, want 0", atomic.LoadInt32(&h1Hits))
	}

	// Claim must be released
	if IsVerificationConfirmationClaimed("pending_1") {
		t.Fatal("expected pending_1 claim to be released after committed failure")
	}
}
