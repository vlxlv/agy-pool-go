package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/observability"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// TestInFlight_Lifecycle verifies in-flight count reaches positive during dispatch and returns to zero on all exit paths.
func TestInFlight_Lifecycle(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()
	observability.Reset()
	TokenRefresher = func(account *storage.Account) (string, error) { return "mock_token_" + account.ID, nil }

	now := time.Now().Unix()
	frac := 0.9
	acc1 := &storage.Account{
		ID:        "acc_flight_1",
		Name:      "FlightAcc1",
		Status:    "ready",
		CreatedAt: &now,
		LastQuota: &storage.QuotaState{RemainingFraction: &frac},
	}
	acc2 := &storage.Account{
		ID:        "acc_flight_2",
		Name:      "FlightAcc2",
		Status:    "ready",
		CreatedAt: &now,
		LastQuota: &storage.QuotaState{RemainingFraction: &frac},
	}
	pool := &storage.Pool{
		Version:  1,
		Accounts: []*storage.Account{acc1, acc2},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	// 1. Successful generation: verify in-flight is 1 during active execution and returns to 0
	inFlightSeen := make(chan int64, 1)
	releaseUpstream := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inFlightSeen <- observability.GetInFlightGeneration("acc_flight_1")
		<-releaseUpstream
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}))
	defer upstream.Close()

	origBackend := BackendURLProvider
	BackendURLProvider = func() string { return upstream.URL }
	defer func() { BackendURLProvider = origBackend }()

	handler := NewHandler()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := httptest.NewRequest("POST", "/v1/models/gemini-pro:streamGenerateContent", nil)
		rec := httptest.NewRecorder()
		handler.HandleProxy(rec, req)
	}()

	// Wait until upstream observes in-flight count
	select {
	case cnt := <-inFlightSeen:
		if cnt != 1 {
			t.Errorf("expected in-flight count 1 during active dispatch, got %d", cnt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for in-flight observation")
	}

	// Release upstream and wait for handler to finish
	close(releaseUpstream)
	wg.Wait()

	if inFlight := observability.GetInFlightGeneration("acc_flight_1"); inFlight != 0 {
		t.Fatalf("expected in-flight to return to 0 after success, got %d", inFlight)
	}

	// 2. Failover: candidate 1 returns 429, candidate 2 succeeds -> both return to 0
	upstream429Then200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHdr := r.Header.Get("Authorization")
		if authHdr == "Bearer mock_token_acc_flight_1" || authHdr == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Resource exhausted"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}))
	defer upstream429Then200.Close()
	BackendURLProvider = func() string { return upstream429Then200.URL }

	req429 := httptest.NewRequest("POST", "/v1/models/gemini-pro:streamGenerateContent", nil)
	rec429 := httptest.NewRecorder()
	handler.HandleProxy(rec429, req429)

	if inFlight1 := observability.GetInFlightGeneration("acc_flight_1"); inFlight1 != 0 {
		t.Errorf("expected acc_flight_1 in-flight 0 after 429, got %d", inFlight1)
	}
	if inFlight2 := observability.GetInFlightGeneration("acc_flight_2"); inFlight2 != 0 {
		t.Errorf("expected acc_flight_2 in-flight 0 after failover success, got %d", inFlight2)
	}

	// 3. Client disconnect / cancellation returns to 0
	blockUpstream := make(chan struct{})
	upstreamCancel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blockUpstream
	}))
	defer upstreamCancel.Close()
	BackendURLProvider = func() string { return upstreamCancel.URL }

	cancelCtx, cancelFn := context.WithCancel(context.Background())
	reqCancel := httptest.NewRequest("POST", "/v1/models/gemini-pro:streamGenerateContent", nil).WithContext(cancelCtx)
	recCancel := httptest.NewRecorder()

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancelFn()
		close(blockUpstream)
	}()

	handler.HandleProxy(recCancel, reqCancel)

	if inFlight := observability.GetInFlightGeneration("acc_flight_1"); inFlight != 0 {
		t.Errorf("expected in-flight to return to 0 after cancellation, got %d", inFlight)
	}

	// 4. Non-generation request bypasses in-flight tracking
	reqNonGen := httptest.NewRequest("GET", "/v1/models", nil)
	recNonGen := httptest.NewRecorder()
	handler.HandleProxy(recNonGen, reqNonGen)

	if inFlight := observability.GetInFlightGeneration("acc_flight_1"); inFlight != 0 {
		t.Errorf("expected in-flight to remain 0 for non-generation request, got %d", inFlight)
	}

	// 5. SSE stream committed then abruptly truncated: verify in-flight returns to 0
	sseUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack not supported", http.StatusInternalServerError)
			return
		}
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()

		_, _ = bufrw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n")
		_, _ = bufrw.WriteString("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n")
		_ = bufrw.Flush()
		// Abrupt close causes committed stream truncation
	}))
	defer sseUpstream.Close()
	BackendURLProvider = func() string { return sseUpstream.URL }

	reqSSE := httptest.NewRequest("POST", "/v1/models/gemini-pro:streamGenerateContent?alt=sse", nil)
	recSSE := httptest.NewRecorder()
	handler.HandleProxy(recSSE, reqSSE)

	if inFlight := observability.GetInFlightGeneration("acc_flight_1"); inFlight != 0 {
		t.Errorf("expected in-flight to return to 0 after SSE stream truncation, got %d", inFlight)
	}
}

// TestInFlight_ConcurrentDispatchDistribution verifies that when account 1 is actively running
// a generation request, a concurrent generation request automatically routes to idle account 2.
func TestInFlight_ConcurrentDispatchDistribution(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()
	observability.Reset()
	TokenRefresher = func(account *storage.Account) (string, error) { return "mock_token_" + account.ID, nil }

	now := time.Now().Unix()
	frac := 0.95
	acc1 := &storage.Account{
		ID:        "acc_flight_1",
		Name:      "FlightAcc1",
		Status:    "ready",
		CreatedAt: &now,
		LastQuota: &storage.QuotaState{RemainingFraction: &frac},
	}
	acc2 := &storage.Account{
		ID:        "acc_flight_2",
		Name:      "FlightAcc2",
		Status:    "ready",
		CreatedAt: &now,
		LastQuota: &storage.QuotaState{RemainingFraction: &frac},
	}
	pool := &storage.Pool{
		Version:  1,
		Strategy: "smart",
		Accounts: []*storage.Account{acc1, acc2},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	req1Started := make(chan struct{})
	req2Started := make(chan struct{})
	releaseAll := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHdr := r.Header.Get("Authorization")
		if authHdr == "Bearer mock_token_acc_flight_1" {
			close(req1Started)
			<-releaseAll
		} else if authHdr == "Bearer mock_token_acc_flight_2" {
			close(req2Started)
			<-releaseAll
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}))
	defer upstream.Close()

	origBackend := BackendURLProvider
	BackendURLProvider = func() string { return upstream.URL }
	defer func() { BackendURLProvider = origBackend }()

	handler := NewHandler()

	var wg sync.WaitGroup
	// Launch request 1 -> should pick acc_flight_1 (first in pool)
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := httptest.NewRequest("POST", "/v1/models/gemini-pro:streamGenerateContent", nil)
		rec := httptest.NewRecorder()
		handler.HandleProxy(rec, req)
	}()

	// Wait for request 1 to be in-flight on acc_flight_1
	select {
	case <-req1Started:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for request 1 to start")
	}

	// While request 1 is running, launch concurrent request 2 -> must be routed to acc_flight_2
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := httptest.NewRequest("POST", "/v1/models/gemini-pro:streamGenerateContent", nil)
		rec := httptest.NewRecorder()
		handler.HandleProxy(rec, req)
	}()

	// Wait for request 2 to be received by upstream on acc_flight_2
	select {
	case <-req2Started:
		// Success: concurrent request 2 was routed to acc_flight_2!
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for concurrent request 2 to route to acc_flight_2")
	}

	// Release both requests and wait for completion
	close(releaseAll)
	wg.Wait()

	if f1 := observability.GetInFlightGeneration("acc_flight_1"); f1 != 0 {
		t.Errorf("expected acc_flight_1 in-flight to return to 0, got %d", f1)
	}
	if f2 := observability.GetInFlightGeneration("acc_flight_2"); f2 != 0 {
		t.Errorf("expected acc_flight_2 in-flight to return to 0, got %d", f2)
	}
}

// TestInFlight_BurstSpreadsAcrossAccounts verifies that simultaneous generation requests reserve
// distinct idle accounts at selection time instead of all picking the same one.
func TestInFlight_BurstSpreadsAcrossAccounts(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()
	observability.Reset()
	TokenRefresher = func(account *storage.Account) (string, error) { return "mock_token_" + account.ID, nil }

	const n = 12
	now := time.Now().Unix()
	frac := 0.95
	pool := &storage.Pool{Version: 1, Strategy: "smart"}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("acc_burst_%d", i)
		pool.Accounts = append(pool.Accounts, &storage.Account{
			ID: id, Name: id, Status: "ready", CreatedAt: &now,
			LastQuota: &storage.QuotaState{RemainingFraction: &frac},
		})
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	var mu sync.Mutex
	seen := map[string]int{}
	arrived := make(chan struct{}, n)
	releaseAll := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("Authorization")]++
		mu.Unlock()
		arrived <- struct{}{}
		<-releaseAll
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}))
	defer upstream.Close()

	origBackend := BackendURLProvider
	BackendURLProvider = func() string { return upstream.URL }
	defer func() { BackendURLProvider = origBackend }()

	handler := NewHandler()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := httptest.NewRequest("POST", "/v1/models/gemini-pro:streamGenerateContent", nil)
			handler.HandleProxy(httptest.NewRecorder(), req)
		}()
	}
	close(start)
	for i := 0; i < n; i++ {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for request %d to reach upstream", i+1)
		}
	}
	close(releaseAll)
	wg.Wait()

	if len(seen) != n {
		t.Errorf("expected %d distinct accounts for %d concurrent requests, got %v", n, n, seen)
	}
	for _, acc := range pool.Accounts {
		if f := observability.GetInFlightGeneration(acc.ID); f != 0 {
			t.Errorf("expected %s in-flight to return to 0, got %d", acc.ID, f)
		}
	}
}

// TestInFlight_HundredConcurrentUnknownQuota covers a cold-start burst: 100 simultaneous generation
// requests on 5 accounts with no quota data at all must spread exactly evenly (20 each).
func TestInFlight_HundredConcurrentUnknownQuota(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()
	observability.Reset()
	TokenRefresher = func(account *storage.Account) (string, error) { return "mock_token_" + account.ID, nil }
	QuotaRefresher = func(*storage.Account) {} // unknown quota must not trigger real network refreshes

	const accountsN, requestsN = 5, 100
	now := time.Now().Unix()
	pool := &storage.Pool{Version: 1, Strategy: "smart"}
	for i := 0; i < accountsN; i++ {
		id := fmt.Sprintf("acc_cold_%d", i)
		pool.Accounts = append(pool.Accounts, &storage.Account{ID: id, Name: id, Status: "ready", CreatedAt: &now})
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	var mu sync.Mutex
	seen := map[string]int{}
	arrived := make(chan struct{}, requestsN)
	releaseAll := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("Authorization")]++
		mu.Unlock()
		arrived <- struct{}{}
		<-releaseAll
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}))
	defer upstream.Close()

	origBackend := BackendURLProvider
	BackendURLProvider = func() string { return upstream.URL }
	defer func() { BackendURLProvider = origBackend }()

	handler := NewHandler()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < requestsN; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := httptest.NewRequest("POST", "/v1/models/gemini-pro:streamGenerateContent", nil)
			handler.HandleProxy(httptest.NewRecorder(), req)
		}()
	}
	close(start)
	for i := 0; i < requestsN; i++ {
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			close(releaseAll)
			t.Fatalf("timed out after %d/%d requests reached upstream", i, requestsN)
		}
	}
	close(releaseAll)
	wg.Wait()

	if len(seen) != accountsN {
		t.Fatalf("expected all %d accounts used, got %v", accountsN, seen)
	}
	for auth, n := range seen {
		if n != requestsN/accountsN {
			t.Errorf("%s got %d requests, want %d (all: %v)", auth, n, requestsN/accountsN, seen)
		}
	}
	for _, acc := range pool.Accounts {
		if f := observability.GetInFlightGeneration(acc.ID); f != 0 {
			t.Errorf("expected %s in-flight to return to 0, got %d", acc.ID, f)
		}
	}
}
