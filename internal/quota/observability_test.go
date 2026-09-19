package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/observability"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// Case G: Quota refresh success
func TestObservability_CaseG_QuotaRefreshSuccess(t *testing.T) {
	setupQuotaTestDir(t)
	observability.Reset()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"groups": [{
				"buckets": [
					{"bucketId": "gemini-5h", "window": "5h", "remainingFraction": 0.85, "resetTime": "2026-09-20T12:00:00Z"},
					{"bucketId": "gemini-weekly", "window": "weekly", "remainingFraction": 0.65, "resetTime": "2026-09-25T12:00:00Z"}
				]
			}]
		}`))
	}))
	defer ts.Close()

	BackendURLBaseProvider = func() string { return ts.URL }
	TokenRefresher = func(acc *storage.Account) (string, error) { return "mock-token", nil }

	acc := &storage.Account{ID: "acc_refresh_success"}
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{acc}
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}

	res, err := QueryQuotaContext(context.Background(), acc)
	if err != nil || res == nil {
		t.Fatalf("QueryQuotaContext failed: %v", err)
	}

	snap := observability.GetSnapshot()
	if snap.QuotaRefreshAttempts != 1 {
		t.Errorf("quota_refresh_attempts_total = %d, want 1", snap.QuotaRefreshAttempts)
	}
	if snap.QuotaRefreshSuccess != 1 {
		t.Errorf("quota_refresh_success_total = %d, want 1", snap.QuotaRefreshSuccess)
	}
	if snap.QuotaRefreshFailure != 0 {
		t.Errorf("quota_refresh_failure_total = %d, want 0", snap.QuotaRefreshFailure)
	}
}

// Case H: Quota refresh failure
func TestObservability_CaseH_QuotaRefreshFailure(t *testing.T) {
	setupQuotaTestDir(t)
	observability.Reset()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	defer ts.Close()

	BackendURLBaseProvider = func() string { return ts.URL }
	TokenRefresher = func(acc *storage.Account) (string, error) { return "mock-token", nil }

	acc := &storage.Account{ID: "acc_refresh_fail"}
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{acc}
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}

	res, err := QueryQuotaContext(context.Background(), acc)
	if err == nil {
		t.Fatalf("expected error from 500 upstream, got res: %v", res)
	}

	snap := observability.GetSnapshot()
	if snap.QuotaRefreshAttempts != 1 {
		t.Errorf("quota_refresh_attempts_total = %d, want 1", snap.QuotaRefreshAttempts)
	}
	if snap.QuotaRefreshFailure != 1 {
		t.Errorf("quota_refresh_failure_total = %d, want 1", snap.QuotaRefreshFailure)
	}
	if snap.QuotaRefreshSuccess != 0 {
		t.Errorf("quota_refresh_success_total = %d, want 0", snap.QuotaRefreshSuccess)
	}
}

// Case I: Single-flight quota refresh
func TestObservability_CaseI_SingleFlightQuotaRefresh(t *testing.T) {
	setupQuotaTestDir(t)
	observability.Reset()

	var upstreamCalls int32
	blockChan := make(chan struct{})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		<-blockChan
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"groups": [{
				"buckets": [
					{"bucketId": "gemini-5h", "window": "5h", "remainingFraction": 0.9, "resetTime": "2026-09-20T12:00:00Z"}
				]
			}]
		}`))
	}))
	defer ts.Close()

	BackendURLBaseProvider = func() string { return ts.URL }
	TokenRefresher = func(acc *storage.Account) (string, error) { return "mock-token", nil }

	acc := &storage.Account{ID: "acc_single_flight"}
	pool := storage.NewEmptyPool()
	pool.Accounts = []*storage.Account{acc}
	if err := storage.SavePool(pool); err != nil {
		t.Fatal(err)
	}

	// 10 concurrent callers attempting to schedule refresh for the same account
	var wg sync.WaitGroup
	var scheduledCount int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ScheduleQuotaRefresh(acc) {
				atomic.AddInt32(&scheduledCount, 1)
			}
		}()
	}
	wg.Wait()

	// Only 1 should have been scheduled due to single-flight
	if count := atomic.LoadInt32(&scheduledCount); count != 1 {
		t.Fatalf("expected 1 caller scheduled, got %d", count)
	}

	// Wait briefly to ensure background goroutine is running QueryQuota
	time.Sleep(20 * time.Millisecond)

	// Release upstream server to finish query
	close(blockChan)

	// Wait for background refresh to complete
	deadline := time.Now().Add(2 * time.Second)
	for IsRefreshInFlight(acc.ID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if IsRefreshInFlight(acc.ID) {
		t.Fatal("refresh did not complete within deadline")
	}

	snap := observability.GetSnapshot()
	if snap.QuotaRefreshAttempts != 1 {
		t.Errorf("quota_refresh_attempts_total = %d, want exactly 1", snap.QuotaRefreshAttempts)
	}
	if snap.QuotaRefreshSuccess != 1 {
		t.Errorf("quota_refresh_success_total = %d, want exactly 1", snap.QuotaRefreshSuccess)
	}
	if snap.QuotaRefreshFailure != 0 {
		t.Errorf("quota_refresh_failure_total = %d, want 0", snap.QuotaRefreshFailure)
	}
}
