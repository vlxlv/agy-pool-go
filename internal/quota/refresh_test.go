package quota

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func setupQuotaTestDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	origMode := config.IsTestMode()
	config.SetTestMode(true)
	t.Cleanup(func() {
		config.SetTestMode(origMode)
		BackendURLBaseProvider = nil
		TokenRefresher = nil
		HTTPClient = nil
		ResetRefreshState()
		config.ResetDataDir()
	})

	if err := config.ConfigureStateDir(tmp); err != nil {
		t.Fatalf("failed to configure state dir: %v", err)
	}
	return tmp
}

func int64Ptr(i int64) *int64 {
	return &i
}

func stringPtr(s string) *string {
	return &s
}

// 1. Success persists fresh updated_at, q5, and q7 to state.db
func TestQueryQuota_SuccessPersistsFreshTelemetry(t *testing.T) {
	setupQuotaTestDir(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
			http.NotFound(w, r)
			return
		}
		authHdr := r.Header.Get("Authorization")
		if authHdr != "Bearer mock-access-token" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp := quotaSummaryResponse{
			Groups: []struct {
				Buckets []struct {
					BucketID          string `json:"bucketId"`
					RemainingFraction any    `json:"remainingFraction"`
					ResetTime         any    `json:"resetTime"`
					Window            string `json:"window"`
				} `json:"buckets"`
			}{
				{
					Buckets: []struct {
						BucketID          string `json:"bucketId"`
						RemainingFraction any    `json:"remainingFraction"`
						ResetTime         any    `json:"resetTime"`
						Window            string `json:"window"`
					}{
						{
							BucketID:          "gemini-5h-user",
							RemainingFraction: 0.85,
							ResetTime:         "2026-09-17T15:00:00Z",
							Window:            "5h",
						},
						{
							BucketID:          "gemini-weekly-user",
							RemainingFraction: 0.95,
							ResetTime:         "2026-09-24T00:00:00Z",
							Window:            "weekly",
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	BackendURLBaseProvider = func() string { return ts.URL }
	TokenRefresher = func(account *storage.Account) (string, error) { return "mock-access-token", nil }

	// Seed pool with stale account
	oldTime := time.Now().Unix() - 500
	acc := &storage.Account{
		ID:    "acc-1",
		Email: "user@example.com",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction: floatPtr(0.5),
			},
			UpdatedAt: &oldTime,
		},
	}
	pool := &storage.Pool{
		Version:  1,
		Strategy: "max_quota",
		Accounts: []*storage.Account{acc},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("failed to save initial pool: %v", err)
	}

	beforeQuery := time.Now().Unix()
	res, err := QueryQuota(acc)
	if err != nil {
		t.Fatalf("QueryQuota failed: %v", err)
	}

	if res == nil || res.Gemini5H == nil || res.GeminiWeekly == nil {
		t.Fatalf("expected both 5h and weekly windows, got: %+v", res)
	}
	if res.Gemini5H.Fraction == nil || *res.Gemini5H.Fraction != 0.85 {
		t.Errorf("expected 5h fraction 0.85, got %v", res.Gemini5H.Fraction)
	}
	if res.GeminiWeekly.Fraction == nil || *res.GeminiWeekly.Fraction != 0.95 {
		t.Errorf("expected weekly fraction 0.95, got %v", res.GeminiWeekly.Fraction)
	}
	if res.UpdatedAt == nil || *res.UpdatedAt < beforeQuery {
		t.Errorf("expected fresh UpdatedAt >= %d, got %v", beforeQuery, res.UpdatedAt)
	}

	// Verify persistence in state.db
	loadedPool, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("failed to reload pool: %v", err)
	}
	if len(loadedPool.Accounts) == 0 {
		t.Fatalf("no accounts in reloaded pool")
	}
	storedAcc := loadedPool.Accounts[0]
	if storedAcc.LastQuota == nil {
		t.Fatalf("expected stored LastQuota, got nil")
	}
	if storedAcc.LastQuota.Gemini5H == nil || *storedAcc.LastQuota.Gemini5H.Fraction != 0.85 {
		t.Errorf("stored 5h fraction mismatch: %+v", storedAcc.LastQuota.Gemini5H)
	}
	if storedAcc.LastQuota.GeminiWeekly == nil || *storedAcc.LastQuota.GeminiWeekly.Fraction != 0.95 {
		t.Errorf("stored weekly fraction mismatch: %+v", storedAcc.LastQuota.GeminiWeekly)
	}
	if storedAcc.LastQuota.UpdatedAt == nil || *storedAcc.LastQuota.UpdatedAt < beforeQuery {
		t.Errorf("stored UpdatedAt not updated: %v", storedAcc.LastQuota.UpdatedAt)
	}
}

// 2. Partial quota merge preserves known windows
func TestQueryQuota_PartialResponseMergesKnownWindows(t *testing.T) {
	setupQuotaTestDir(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Returns ONLY 5h bucket, omitting weekly
		resp := quotaSummaryResponse{
			Groups: []struct {
				Buckets []struct {
					BucketID          string `json:"bucketId"`
					RemainingFraction any    `json:"remainingFraction"`
					ResetTime         any    `json:"resetTime"`
					Window            string `json:"window"`
				} `json:"buckets"`
			}{
				{
					Buckets: []struct {
						BucketID          string `json:"bucketId"`
						RemainingFraction any    `json:"remainingFraction"`
						ResetTime         any    `json:"resetTime"`
						Window            string `json:"window"`
					}{
						{
							BucketID:          "gemini-5h-user",
							RemainingFraction: 0.70,
							ResetTime:         "2026-09-17T16:00:00Z",
							Window:            "5h",
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	BackendURLBaseProvider = func() string { return ts.URL }
	TokenRefresher = func(account *storage.Account) (string, error) { return "mock-token", nil }

	validationURL := "https://accounts.google.com/verify?id=partial"
	acc := &storage.Account{
		ID:            "acc-partial",
		Status:        "validation_required",
		ValidationURL: &validationURL,
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.60),
				ResetTime: "2026-09-17T12:00:00Z",
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.92),
				ResetTime: "2026-09-24T00:00:00Z",
			},
			UpdatedAt: int64Ptr(time.Now().Unix() - 400),
		},
	}
	pool := &storage.Pool{Accounts: []*storage.Account{acc}}
	_ = storage.SavePool(pool)

	res, err := QueryQuota(acc)
	if err != nil {
		t.Fatalf("QueryQuota failed: %v", err)
	}

	if *res.Gemini5H.Fraction != 0.70 {
		t.Errorf("expected 5h fraction 0.70, got %v", *res.Gemini5H.Fraction)
	}
	if res.GeminiWeekly == nil || *res.GeminiWeekly.Fraction != 0.92 {
		t.Errorf("expected preserved weekly fraction 0.92, got %+v", res.GeminiWeekly)
	}
	storedPool, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	stored := storage.FindAccount(storedPool, acc)
	if stored.Status != "validation_required" {
		t.Fatalf("status = %q, want validation_required", stored.Status)
	}
	if stored.ValidationURL == nil || *stored.ValidationURL != validationURL {
		t.Fatalf("validation URL = %v, want %q", stored.ValidationURL, validationURL)
	}
}

func TestQueryQuota_PreservesAuthRestrictions(t *testing.T) {
	setupQuotaTestDir(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"groups":[{"buckets":[{"bucketId":"gemini-5h-user","remainingFraction":0.7,"window":"5h"}]}]}`)
	}))
	defer ts.Close()

	BackendURLBaseProvider = func() string { return ts.URL }
	TokenRefresher = func(account *storage.Account) (string, error) { return "mock-token", nil }

	for _, status := range []string{"validation_required", "auth_error"} {
		t.Run(status, func(t *testing.T) {
			validationURL := "https://accounts.google.com/verify?id=" + status
			acc := &storage.Account{ID: "restricted-" + status, Status: status, ValidationURL: &validationURL}
			pool := &storage.Pool{Accounts: []*storage.Account{acc}}
			if err := storage.SavePool(pool); err != nil {
				t.Fatalf("SavePool failed: %v", err)
			}

			if _, err := QueryQuota(acc); err != nil {
				t.Fatalf("QueryQuota failed: %v", err)
			}
			storedPool, err := storage.LoadPool()
			if err != nil {
				t.Fatalf("LoadPool failed: %v", err)
			}
			stored := storage.FindAccount(storedPool, acc)
			if stored.Status != status {
				t.Fatalf("status = %q, want %q", stored.Status, status)
			}
			if stored.ValidationURL == nil || *stored.ValidationURL != validationURL {
				t.Fatalf("validation URL = %v, want %q", stored.ValidationURL, validationURL)
			}
			if stored.LastQuota == nil || stored.LastQuota.Gemini5H == nil || stored.LastQuota.Gemini5H.Fraction == nil || *stored.LastQuota.Gemini5H.Fraction != 0.7 {
				t.Fatalf("quota was not updated: %+v", stored.LastQuota)
			}
		})
	}
}

// 3. Stale account refresh scheduling vs Fresh
func TestScheduleQuotaRefresh_StaleAccountScheduling(t *testing.T) {
	setupQuotaTestDir(t)

	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"groups":[{"buckets":[{"bucketId":"gemini-5h-user","remainingFraction":0.9,"window":"5h"}]}]}`)
	}))
	defer ts.Close()

	BackendURLBaseProvider = func() string { return ts.URL }
	TokenRefresher = func(account *storage.Account) (string, error) { return "tok", nil }

	now := float64(time.Now().Unix())

	// Account fresh (updated 10s ago)
	freshAcc := &storage.Account{
		ID: "fresh",
		LastQuota: &storage.QuotaState{
			UpdatedAt: int64Ptr(int64(now - 10)),
		},
	}
	if RefreshNeeded(freshAcc, now) {
		t.Errorf("expected fresh account to NOT need refresh")
	}

	// Account stale (updated 400s ago)
	staleAcc := &storage.Account{
		ID: "stale",
		LastQuota: &storage.QuotaState{
			UpdatedAt: int64Ptr(int64(now - 400)),
		},
	}
	if !RefreshNeeded(staleAcc, now) {
		t.Errorf("expected stale account to need refresh")
	}

	pool := &storage.Pool{Accounts: []*storage.Account{staleAcc}}
	_ = storage.SavePool(pool)

	scheduled := ScheduleQuotaRefresh(staleAcc, now)
	if !scheduled {
		t.Errorf("expected ScheduleQuotaRefresh to return true for stale account")
	}

	// Wait briefly for background goroutine to execute
	time.Sleep(100 * time.Millisecond)
	if atomic.LoadInt32(&calls) == 0 {
		t.Errorf("expected background refresh to make HTTP call")
	}
}

// 4. Per-account single-flight
func TestScheduleQuotaRefresh_SingleFlight(t *testing.T) {
	setupQuotaTestDir(t)

	gate := make(chan struct{})
	started := make(chan struct{})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-gate
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"groups":[{"buckets":[{"bucketId":"gemini-5h-user","remainingFraction":0.9,"window":"5h"}]}]}`)
	}))
	defer ts.Close()

	BackendURLBaseProvider = func() string { return ts.URL }
	TokenRefresher = func(account *storage.Account) (string, error) { return "tok", nil }

	acc := &storage.Account{
		ID:        "single-flight-acc",
		LastQuota: &storage.QuotaState{UpdatedAt: int64Ptr(100)},
	}
	_ = storage.SavePool(&storage.Pool{Accounts: []*storage.Account{acc}})

	first := ScheduleQuotaRefresh(acc)
	if !first {
		t.Fatalf("expected first schedule to succeed")
	}

	// Wait until HTTP request is in-flight
	<-started

	// Second concurrent call for same account must be rejected (single-flight)
	second := ScheduleQuotaRefresh(acc)
	if second {
		t.Errorf("expected second concurrent ScheduleQuotaRefresh to return false (single-flight)")
	}

	if !IsRefreshInFlight(acc.ID) {
		t.Errorf("expected IsRefreshInFlight to be true while request is running")
	}

	close(gate)
	time.Sleep(100 * time.Millisecond)

	if IsRefreshInFlight(acc.ID) {
		t.Errorf("expected IsRefreshInFlight to be false after request completes")
	}
}

// 5. Failed refresh backoff and success resets backoff
func TestScheduleQuotaRefresh_BackoffAndReset(t *testing.T) {
	setupQuotaTestDir(t)

	var statusCode int32 = 500
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := int(atomic.LoadInt32(&statusCode))
		if code >= 400 {
			http.Error(w, "internal error", code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"groups":[{"buckets":[{"bucketId":"gemini-5h-user","remainingFraction":0.9,"window":"5h"}]}]}`)
	}))
	defer ts.Close()

	BackendURLBaseProvider = func() string { return ts.URL }
	TokenRefresher = func(account *storage.Account) (string, error) { return "tok", nil }

	acc := &storage.Account{
		ID:        "backoff-acc",
		LastQuota: &storage.QuotaState{UpdatedAt: int64Ptr(100)},
	}
	_ = storage.SavePool(&storage.Pool{Accounts: []*storage.Account{acc}})

	now := float64(time.Now().Unix())

	// First attempt fails (HTTP 500)
	scheduled := ScheduleQuotaRefresh(acc, now)
	if !scheduled {
		t.Fatalf("expected initial schedule to return true")
	}

	time.Sleep(100 * time.Millisecond)

	attempt, nextAt, ok := GetRefreshRetryEntry(acc.ID)
	if !ok {
		t.Fatalf("expected retry entry to be recorded on failure")
	}
	if attempt != 1 {
		t.Errorf("expected attempt 1, got %d", attempt)
	}
	expectedDelay := config.QuotaRefreshBackoff[0].Seconds()
	if nextAt < now+expectedDelay-2 || nextAt > now+expectedDelay+5 {
		t.Errorf("expected nextAt around %v, got %v", now+expectedDelay, nextAt)
	}

	// Attempting to schedule again before nextAt should return false
	if ScheduleQuotaRefresh(acc, now+5) {
		t.Errorf("expected ScheduleQuotaRefresh during backoff to return false")
	}

	// Now switch mock to success
	atomic.StoreInt32(&statusCode, 200)

	// Schedule after backoff expires
	scheduledAfter := ScheduleQuotaRefresh(acc, nextAt+1)
	if !scheduledAfter {
		t.Fatalf("expected ScheduleQuotaRefresh after backoff window to return true")
	}

	time.Sleep(100 * time.Millisecond)

	// Success should clear the retry backoff entry
	_, _, okAfter := GetRefreshRetryEntry(acc.ID)
	if okAfter {
		t.Errorf("expected retry entry to be cleared after successful refresh")
	}
}

// 6. Fail-closed test guard: refuses external requests in test mode without mock backend
func TestQueryQuota_FailClosedInTestMode(t *testing.T) {
	origMode := config.IsTestMode()
	config.SetTestMode(true)
	defer config.SetTestMode(origMode)

	BackendURLBaseProvider = nil
	acc := &storage.Account{ID: "test-acc"}
	_, err := QueryQuota(acc)
	if err == nil {
		t.Fatalf("expected fail-closed guard error in test mode without mock backend, got nil")
	}
	if !strings.Contains(err.Error(), "[FAIL-CLOSED TEST GUARD]") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// 7. Validation required error isolates account and zeros quota
func TestQueryQuota_ValidationError(t *testing.T) {
	setupQuotaTestDir(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `{"error":{"message":"VALIDATION_REQUIRED: please verify your account at https://accounts.google.com/verify"}}`)
	}))
	defer ts.Close()

	BackendURLBaseProvider = func() string { return ts.URL }
	TokenRefresher = func(account *storage.Account) (string, error) { return "tok", nil }

	acc := &storage.Account{
		ID: "val-acc",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{Fraction: floatPtr(0.9)},
		},
	}
	_ = storage.SavePool(&storage.Pool{Accounts: []*storage.Account{acc}})

	res, err := QueryQuota(acc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if acc.Status != "validation_required" {
		t.Errorf("expected status validation_required, got %q", acc.Status)
	}
	if acc.RateLimitedUntil == nil || *acc.RateLimitedUntil <= float64(time.Now().Unix()) {
		t.Errorf("expected 24h rate limit cooldown")
	}
	if res.Gemini5H == nil || *res.Gemini5H.Fraction != 0.0 {
		t.Errorf("expected zeroed 5h fraction")
	}

	// Verify persistence in state.db
	reloaded, _ := storage.LoadPool()
	if reloaded.Accounts[0].Status != "validation_required" {
		t.Errorf("persisted status mismatch: %s", reloaded.Accounts[0].Status)
	}
}

// 8. 429 invalidates freshness without erasing known windows
func TestQuotaFreshness_429InvalidationSemantics(t *testing.T) {
	future5h := time.Now().Add(4 * time.Hour).UTC().Format(time.RFC3339)
	futureWeekly := time.Now().Add(6 * 24 * time.Hour).UTC().Format(time.RFC3339)

	acc := &storage.Account{
		ID: "429-acc",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  floatPtr(0.75),
				ResetTime: future5h,
			},
			GeminiWeekly: &storage.QuotaWindow{
				Fraction:  floatPtr(0.85),
				ResetTime: futureWeekly,
			},
			UpdatedAt: int64Ptr(time.Now().Unix() - 20),
		},
	}

	// Initially fresh
	now := float64(time.Now().Unix())
	infoBefore := Freshness(acc, now)
	if infoBefore.Class != "fresh" {
		t.Fatalf("expected initial state fresh, got %s", infoBefore.Class)
	}

	// Invalidate freshness by setting UpdatedAt = 0 (as done on 429)
	var zero int64 = 0
	acc.LastQuota.UpdatedAt = &zero

	infoAfter := Freshness(acc, now)
	if infoAfter.Class != "unknown" {
		t.Errorf("expected freshness class unknown after 429 invalidation, got %s", infoAfter.Class)
	}

	// Invariant: known windows must remain intact
	if acc.LastQuota.Gemini5H == nil || *acc.LastQuota.Gemini5H.Fraction != 0.75 {
		t.Errorf("expected Gemini5H fraction 0.75 preserved, got %+v", acc.LastQuota.Gemini5H)
	}
	if acc.LastQuota.GeminiWeekly == nil || *acc.LastQuota.GeminiWeekly.Fraction != 0.85 {
		t.Errorf("expected GeminiWeekly fraction 0.85 preserved, got %+v", acc.LastQuota.GeminiWeekly)
	}
}
