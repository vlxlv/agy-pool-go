package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/auth"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/observability"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func init() {
	accounts.SetQuotaProber(QueryQuota, nil)
}

var (
	// BackendURLBaseProvider allows overriding the base URL for tests.
	BackendURLBaseProvider func() string

	// UserAgentProvider allows overriding the User-Agent header for tests.
	UserAgentProvider func() string

	// TokenRefresher allows mocking token acquisition for tests.
	TokenRefresher func(account *storage.Account) (string, error)

	// HTTPClient allows mocking or customizing the HTTP client for tests.
	HTTPClient *http.Client

	// WarnHook receives refresh anomalies worth surfacing in the gateway log; nil drops them.
	WarnHook func(msg string)

	refreshMu       sync.Mutex
	refreshInFlight = make(map[string]bool)
	refreshRetry    = make(map[string]retryEntry)
	poolTransaction = storage.PoolTransaction
)

func isPoolTransactionReplaced() bool {
	return reflect.ValueOf(poolTransaction).Pointer() != reflect.ValueOf(storage.PoolTransaction).Pointer()
}

type retryEntry struct {
	attempt int
	nextAt  float64
}

// ResetRefreshState resets in-flight and retry backoff state (useful for tests).
func ResetRefreshState() {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	refreshInFlight = make(map[string]bool)
	refreshRetry = make(map[string]retryEntry)
}

// GetRefreshRetryEntry returns the retry attempt, nextAt timestamp, and existence for an account key.
func GetRefreshRetryEntry(key string) (int, float64, bool) {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	entry, ok := refreshRetry[key]
	return entry.attempt, entry.nextAt, ok
}

// IsRefreshInFlight reports whether a refresh is currently running for an account key.
func IsRefreshInFlight(key string) bool {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	return refreshInFlight[key]
}

func getBackendURLBase() string {
	if BackendURLBaseProvider != nil {
		return strings.TrimRight(BackendURLBaseProvider(), "/")
	}
	return "https://" + BackendHost
}

func getUserAgent() string {
	if UserAgentProvider != nil {
		return UserAgentProvider()
	}
	return DefaultUA
}

func getHTTPClient() *http.Client {
	if HTTPClient != nil {
		return HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func doRefreshToken(account *storage.Account, force ...bool) (string, error) {
	if TokenRefresher != nil {
		return TokenRefresher(account)
	}
	return auth.RefreshToken(account, force...)
}

type quotaSummaryResponse struct {
	Groups []struct {
		Buckets []struct {
			BucketID          string `json:"bucketId"`
			RemainingFraction any    `json:"remainingFraction"`
			ResetTime         any    `json:"resetTime"`
			Window            string `json:"window"`
		} `json:"buckets"`
	} `json:"groups"`
}

type availableModelsResponse struct {
	DefaultAgentModelID string `json:"defaultAgentModelId"`
	Models              map[string]struct {
		QuotaInfo *struct {
			RemainingFraction any `json:"remainingFraction"`
			ResetTime         any `json:"resetTime"`
		} `json:"quotaInfo"`
	} `json:"models"`
}

// QueryQuota refreshes cached quota for the given account against the Google quota endpoint.
// Preserves known windows absent from a partial response, updates updated_at, and persists to state.db.
func QueryQuota(account *storage.Account) (*storage.QuotaState, error) {
	return QueryQuotaContext(context.Background(), account)
}

// QueryQuotaContext refreshes cached quota with context support.
func QueryQuotaContext(ctx context.Context, account *storage.Account) (res *storage.QuotaState, err error) {
	if account == nil {
		return nil, errors.New("account is required")
	}
	if account.Status == storage.StatusDisabled {
		return nil, errors.New("cannot refresh quota for disabled account")
	}

	observability.RecordQuotaRefreshAttempt()
	defer func() {
		if err != nil {
			observability.RecordQuotaRefreshFailure()
		} else {
			observability.RecordQuotaRefreshSuccess()
		}
	}()

	// Fail-closed guard: prevent real external requests in test mode without an explicit mock backend
	if BackendURLBaseProvider == nil && config.IsTestMode() {
		return nil, errors.New("[FAIL-CLOSED TEST GUARD] refusing external quota query in test mode without mock backend")
	}

	forceRefresh := account.Status == storage.StatusValidationRequired ||
		account.Status == storage.StatusAuthError ||
		(account.ValidationURL != nil && *account.ValidationURL != "")
	at, err := doRefreshToken(account, forceRefresh)
	if err != nil {
		if forceRefresh && account.AccessToken != "" {
			at = account.AccessToken
		} else {
			return nil, fmt.Errorf("token refresh failed for account %s: %w", account.ID, err)
		}
	}

	// Copy existing windows if present to preserve known windows absent from partial response
	quotaData := &storage.QuotaState{}
	if account.LastQuota != nil {
		if account.LastQuota.Gemini5H != nil {
			quotaData.Gemini5H = &storage.QuotaWindow{
				Fraction:  account.LastQuota.Gemini5H.Fraction,
				ResetTime: account.LastQuota.Gemini5H.ResetTime,
			}
		}
		if account.LastQuota.GeminiWeekly != nil {
			quotaData.GeminiWeekly = &storage.QuotaWindow{
				Fraction:  account.LastQuota.GeminiWeekly.Fraction,
				ResetTime: account.LastQuota.GeminiWeekly.ResetTime,
			}
		}
		if account.LastQuota.ThirdParty5H != nil {
			quotaData.ThirdParty5H = &storage.QuotaWindow{
				Fraction:  account.LastQuota.ThirdParty5H.Fraction,
				ResetTime: account.LastQuota.ThirdParty5H.ResetTime,
			}
		}
		if account.LastQuota.ThirdPartyWeekly != nil {
			quotaData.ThirdPartyWeekly = &storage.QuotaWindow{
				Fraction:  account.LastQuota.ThirdPartyWeekly.Fraction,
				ResetTime: account.LastQuota.ThirdPartyWeekly.ResetTime,
			}
		}
	}

	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+at)
	headers.Set("Content-Type", "application/json")
	headers.Set("User-Agent", getUserAgent())

	quotaURL := getBackendURLBase() + "/v1internal:retrieveUserQuotaSummary"
	req, err := http.NewRequestWithContext(ctx, "POST", quotaURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header = headers

	resp, err := getHTTPClient().Do(req)
	var quotaErr error
	if err != nil {
		quotaErr = err
	} else {
		defer resp.Body.Close()
		bodyBytes, _ := io.ReadAll(resp.Body)

		if resp.StatusCode >= 400 {
			if auth.IsValidationError(resp.StatusCode, bodyBytes) || auth.IsAuthError(resp.StatusCode, bodyBytes) {
				isVal := auth.IsValidationError(resp.StatusCode, bodyBytes)
				status := "auth_error"
				delay := 3600.0
				if isVal {
					status = "validation_required"
					delay = 86400.0
				}
				vURL := auth.ExtractValidationURL(bodyBytes)
				now := float64(time.Now().Unix())
				cooldown := now + delay

				var zeroFrac float64 = 0.0
				quotaData.Gemini5H = &storage.QuotaWindow{Fraction: &zeroFrac, ResetTime: nil}
				quotaData.GeminiWeekly = &storage.QuotaWindow{Fraction: &zeroFrac, ResetTime: nil}
				recomputeCompatQuota(quotaData)
				nowSec := time.Now().Unix()
				quotaData.UpdatedAt = &nowSec

				if err := poolTransaction(func(pool *storage.Pool) error {
					stored := storage.FindAccount(pool, account)
					if stored != nil {
						stored.Status = status
						if vURL != "" {
							stored.ValidationURL = &vURL
						}
						stored.RateLimitedUntil = &cooldown
						stored.ErrorCount++
						stored.LastQuota = quotaData
					}
					return nil
				}); err != nil {
					if isPoolTransactionReplaced() {
						observability.RecordPersistenceError()
					}
					return nil, fmt.Errorf("failed to persist %s state for account %s: %w", status, account.ID, err)
				}
				account.Status = status
				if vURL != "" {
					account.ValidationURL = &vURL
				}
				account.RateLimitedUntil = &cooldown
				account.LastQuota = quotaData
				return quotaData, nil
			}

			// Try fallback to fetchAvailableModels
			quotaErr = fmt.Errorf("retrieveUserQuotaSummary returned HTTP %d: %s", resp.StatusCode, string(bodyBytes))
		} else {
			// Parse summary response
			var summary quotaSummaryResponse
			if err := json.Unmarshal(bodyBytes, &summary); err == nil {
				foundQuota := false
				// Upstream also lists variant Gemini buckets (e.g. "gemini-argon-wtu-weekly") with their own
				// limits; the exact "gemini-5h"/"gemini-weekly" buckets are the agy limits and must win.
				exact5H, exactWeekly := false, false
				for _, group := range summary.Groups {
					for _, bucket := range group.Buckets {
						fraction := remainingFraction(bucket.RemainingFraction, bucket.ResetTime)
						if fraction == nil {
							continue
						}
						bid := strings.ToLower(bucket.BucketID)
						w := strings.ToLower(bucket.Window)
						if strings.Contains(bid, "gemini") {
							if strings.Contains(bid, "5h") || w == "5h" {
								if exact5H {
									continue
								}
								exact5H = bid == "gemini-5h"
								quotaData.Gemini5H = &storage.QuotaWindow{Fraction: fraction, ResetTime: bucket.ResetTime}
								foundQuota = true
							} else if strings.Contains(bid, "weekly") || w == "weekly" {
								if exactWeekly {
									continue
								}
								exactWeekly = bid == "gemini-weekly"
								quotaData.GeminiWeekly = &storage.QuotaWindow{Fraction: fraction, ResetTime: bucket.ResetTime}
								foundQuota = true
							}
						} else if strings.Contains(bid, "3p") || strings.Contains(bid, "third_party") {
							if strings.Contains(bid, "5h") || w == "5h" {
								quotaData.ThirdParty5H = &storage.QuotaWindow{Fraction: fraction, ResetTime: bucket.ResetTime}
							} else if strings.Contains(bid, "weekly") || w == "weekly" {
								quotaData.ThirdPartyWeekly = &storage.QuotaWindow{Fraction: fraction, ResetTime: bucket.ResetTime}
							}
						}
					}
				}
				if foundQuota {
					probeAndApplyValidationChallenge(ctx, headers, account)
					return finalizeAndPersistQuota(account, quotaData)
				}
			}
			quotaErr = errors.New("quota response contained no Gemini quota buckets")
			if WarnHook != nil {
				WarnHook(fmt.Sprintf("account %s: quota summary returned no Gemini buckets; falling back to fetchAvailableModels, Gemini weekly quota is not refreshed", account.ID))
			}
		}
	}

	// Fallback to fetchAvailableModels
	fallbackErr := fetchAvailableModelsQuota(ctx, headers, account, quotaData)
	if fallbackErr != nil {
		if quotaErr != nil {
			return nil, fmt.Errorf("quota refresh failed (%w) and fallback failed (%v)", quotaErr, fallbackErr)
		}
		return nil, fallbackErr
	}

	probeAndApplyValidationChallenge(ctx, headers, account)
	return finalizeAndPersistQuota(account, quotaData)
}

func fetchAvailableModelsQuota(ctx context.Context, headers http.Header, account *storage.Account, quotaData *storage.QuotaState) error {
	modelsURL := getBackendURLBase() + "/v1internal:fetchAvailableModels"
	req, err := http.NewRequestWithContext(ctx, "POST", modelsURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header = headers

	resp, err := getHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		if auth.IsValidationError(resp.StatusCode, b) || auth.IsAuthError(resp.StatusCode, b) {
			isVal := auth.IsValidationError(resp.StatusCode, b)
			status := "auth_error"
			delay := 3600.0
			if isVal {
				status = "validation_required"
				delay = 86400.0
			}
			vURL := auth.ExtractValidationURL(b)
			now := float64(time.Now().Unix())
			cooldown := now + delay

			var zeroFrac float64 = 0.0
			quotaData.Gemini5H = &storage.QuotaWindow{Fraction: &zeroFrac, ResetTime: nil}
			quotaData.GeminiWeekly = &storage.QuotaWindow{Fraction: &zeroFrac, ResetTime: nil}
			recomputeCompatQuota(quotaData)
			nowSec := time.Now().Unix()
			quotaData.UpdatedAt = &nowSec

			_ = poolTransaction(func(pool *storage.Pool) error {
				stored := storage.FindAccount(pool, account)
				if stored != nil {
					stored.Status = status
					if vURL != "" {
						stored.ValidationURL = &vURL
					}
					stored.RateLimitedUntil = &cooldown
					stored.ErrorCount++
					stored.LastQuota = quotaData
				}
				return nil
			})
			account.Status = status
			if vURL != "" {
				account.ValidationURL = &vURL
			}
			account.RateLimitedUntil = &cooldown
			account.LastQuota = quotaData
			return nil
		}
		return fmt.Errorf("fetchAvailableModels HTTP %d: %s", resp.StatusCode, string(b))
	}

	var res availableModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return err
	}

	candidates := []string{}
	if res.DefaultAgentModelID != "" {
		candidates = append(candidates, res.DefaultAgentModelID)
	}
	candidates = append(candidates, "gemini-3.8-flash-high", "gemini-3.6-flash-high", "gemini-3.5-flash-medium")
	// Remaining models in sorted order, so the pick does not depend on map iteration.
	rest := make([]string, 0, len(res.Models))
	for name := range res.Models {
		if !slices.Contains(candidates, name) {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	candidates = append(candidates, rest...)

	for _, name := range candidates {
		// Only Gemini models carry the Gemini window; the default agent model may be Claude/GPT,
		// whose quota and reset time are unrelated.
		if !strings.HasPrefix(name, "gemini") {
			continue
		}
		if model, exists := res.Models[name]; exists && model.QuotaInfo != nil {
			frac := remainingFraction(model.QuotaInfo.RemainingFraction, model.QuotaInfo.ResetTime)
			if frac != nil {
				quotaData.Gemini5H = &storage.QuotaWindow{
					Fraction:  frac,
					ResetTime: model.QuotaInfo.ResetTime,
				}
				// This endpoint has no weekly window, so the copied one would be stale. An exhausted model
				// whose reset lies beyond the 5h window is locked out by the weekly limit.
				quotaData.GeminiWeekly = nil
				if ts := ParseISOOrTimestamp(model.QuotaInfo.ResetTime); *frac == 0 && ts != nil && *ts-float64(time.Now().Unix()) > config.Window5HSecs {
					quotaData.GeminiWeekly = &storage.QuotaWindow{Fraction: frac, ResetTime: model.QuotaInfo.ResetTime}
				}
				return nil
			}
		}
	}

	return errors.New("model response contained no quota information")
}

// remainingFraction parses an upstream quota fraction. Upstream omits remainingFraction when it is 0
// (proto3 JSON drops default values), so a reset time without a fraction means the quota is exhausted.
func remainingFraction(fraction, reset any) *float64 {
	if fraction == nil && ParseISOOrTimestamp(reset) != nil {
		zero := 0.0
		return &zero
	}
	return ParseQuotaFraction(fraction)
}

func probeAndApplyValidationChallenge(ctx context.Context, headers http.Header, account *storage.Account) {
	if account == nil {
		return
	}
	if (account.ValidationURL == nil || *account.ValidationURL == "") && account.Status != storage.StatusValidationRequired {
		return
	}
	loadURL := getBackendURLBase() + "/v1internal:loadCodeAssist"
	req, err := http.NewRequestWithContext(ctx, "POST", loadURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return
	}
	req.Header = headers

	resp, err := getHTTPClient().Do(req)
	if err != nil {
		if account.ValidationURL != nil && *account.ValidationURL != "" {
			account.Status = storage.StatusValidationRequired
		}
		return
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)

	if auth.IsValidationError(resp.StatusCode, bodyBytes) {
		vURL := auth.ExtractValidationURL(bodyBytes)
		if vURL == "" && account.ValidationURL != nil {
			vURL = *account.ValidationURL
		}
		applyValidationRequired(account, vURL)
		return
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var res struct {
			IneligibleTiers []struct {
				ReasonCode    string `json:"reasonCode"`
				ValidationURL string `json:"validationUrl"`
			} `json:"ineligibleTiers"`
		}
		if err := json.Unmarshal(bodyBytes, &res); err == nil && len(res.IneligibleTiers) > 0 {
			for _, tier := range res.IneligibleTiers {
				if strings.EqualFold(tier.ReasonCode, "VALIDATION_REQUIRED") {
					vURL := tier.ValidationURL
					if vURL == "" && account.ValidationURL != nil {
						vURL = *account.ValidationURL
					}
					applyValidationRequired(account, vURL)
					return
				}
			}
		}

		account.Status = ""
		account.ValidationURL = nil
		account.RateLimitedUntil = nil
		return
	}

	if account.ValidationURL != nil && *account.ValidationURL != "" {
		account.Status = storage.StatusValidationRequired
	}
}

func applyValidationRequired(account *storage.Account, vURL string) {
	account.Status = storage.StatusValidationRequired
	if vURL != "" {
		account.ValidationURL = &vURL
	}
	now := float64(time.Now().Unix())
	cooldown := now + 86400.0
	account.RateLimitedUntil = &cooldown
	_ = poolTransaction(func(pool *storage.Pool) error {
		stored := storage.FindAccount(pool, account)
		if stored != nil {
			stored.Status = storage.StatusValidationRequired
			if vURL != "" {
				stored.ValidationURL = &vURL
			}
			stored.RateLimitedUntil = &cooldown
		}
		return nil
	})
}

// finalizeAndPersistQuota persists quota telemetry without changing auth state.
// Quota health and auth/verification restriction are orthogonal: a successful
// quota probe is not proof that verification or re-authentication completed.
func finalizeAndPersistQuota(account *storage.Account, quotaData *storage.QuotaState) (*storage.QuotaState, error) {
	if quotaData.GeminiWeekly != nil && quotaData.GeminiWeekly.ResetTime == nil && quotaData.GeminiWeekly.Fraction != nil && *quotaData.GeminiWeekly.Fraction == 0.0 {
		quotaData.GeminiWeekly = nil
	}
	recomputeCompatQuota(quotaData)
	nowSec := time.Now().Unix()
	quotaData.UpdatedAt = &nowSec

	err := storage.PoolTransaction(func(pool *storage.Pool) error {
		stored := storage.FindAccount(pool, account)
		if stored != nil {
			stored.LastQuota = quotaData
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to persist quota update: %w", err)
	}
	account.LastQuota = quotaData
	return quotaData, nil
}

func recomputeCompatQuota(q *storage.QuotaState) {
	var known []struct {
		frac  float64
		reset any
	}
	for _, w := range []*storage.QuotaWindow{q.Gemini5H, q.GeminiWeekly} {
		if w != nil && w.Fraction != nil {
			known = append(known, struct {
				frac  float64
				reset any
			}{*w.Fraction, w.ResetTime})
		}
	}
	if len(known) > 0 {
		minItem := known[0]
		for _, item := range known {
			if item.frac < minItem.frac {
				minItem = item
			}
		}
		q.RemainingFraction = &minItem.frac
		q.ResetTime = minItem.reset
	} else {
		q.RemainingFraction = nil
		q.ResetTime = nil
	}
}

// ScheduleQuotaRefresh starts one bounded, non-blocking refresh per account.
// Returns true if a refresh was scheduled, or false if blocked by an in-flight refresh or active backoff.
func ScheduleQuotaRefresh(account *storage.Account, now ...float64) bool {
	if account == nil || account.Status == storage.StatusDisabled {
		return false
	}
	key := account.ID
	if key == "" {
		key = account.Email
	}
	if key == "" {
		return false
	}

	currentTime := float64(time.Now().Unix())
	if len(now) > 0 {
		currentTime = now[0]
	}

	refreshMu.Lock()
	if refreshInFlight[key] {
		refreshMu.Unlock()
		return false
	}
	if retry, exists := refreshRetry[key]; exists && retry.nextAt > currentTime {
		refreshMu.Unlock()
		return false
	}
	refreshInFlight[key] = true
	refreshMu.Unlock()

	go func() {
		defer func() {
			refreshMu.Lock()
			delete(refreshInFlight, key)
			refreshMu.Unlock()
		}()

		_, err := QueryQuota(account)
		refreshMu.Lock()
		defer refreshMu.Unlock()
		if err == nil {
			delete(refreshRetry, key)
		} else {
			prev := refreshRetry[key]
			delay := NextBackoffDelay(prev.attempt)
			refreshRetry[key] = retryEntry{
				attempt: prev.attempt + 1,
				nextAt:  float64(time.Now().Unix()) + delay.Seconds(),
			}
		}
	}()

	return true
}

// ScheduleQuotaRefreshSimple adapts ScheduleQuotaRefresh to func(*storage.Account).
func ScheduleQuotaRefreshSimple(account *storage.Account) {
	_ = ScheduleQuotaRefresh(account)
}
