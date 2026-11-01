package proxy

import (
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/auth"
	"github.com/vlxlv/agy-pool-go/internal/observability"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// poolTransaction is replaceable by package tests to deterministically inject persistence failures.
var poolTransaction = storage.PoolTransaction

// recordSuccess is replaceable by package tests to inject persistence failures.
var recordSuccess = storage.RecordSuccess

func isPoolTransactionReplaced() bool {
	return reflect.ValueOf(poolTransaction).Pointer() != reflect.ValueOf(storage.PoolTransaction).Pointer()
}

func isRecordSuccessReplaced() bool {
	return reflect.ValueOf(recordSuccess).Pointer() != reflect.ValueOf(storage.RecordSuccess).Pointer()
}

func runPoolTransaction(fn func(*storage.Pool) error) error {
	err := poolTransaction(fn)
	if err != nil && isPoolTransactionReplaced() {
		observability.RecordPersistenceError()
	}
	return err
}

// RecordSuccess updates account counters and promotes active account under transaction.
func RecordSuccess(account *storage.Account, isGeneration bool) error {
	err := recordSuccess(account, isGeneration, NowFunc().Unix())
	if err != nil && isRecordSuccessReplaced() {
		observability.RecordPersistenceError()
	}
	return err
}

// RecordQuotaError parses Retry-After and applies rate limit cooldown under transaction.
// Without Retry-After, the reset time in the RESOURCE_EXHAUSTED body is used; a weekly limit can be days away.
func RecordQuotaError(account *storage.Account, headers http.Header, body []byte) (int64, error) {
	retryAfterStr := ""
	if headers != nil {
		retryAfterStr = headers.Get("Retry-After")
	}
	delay := quota.ParseRetryAfter(retryAfterStr, 300)
	now := float64(NowFunc().Unix())
	if retryAfterStr == "" {
		if ts := quota.ParseQuotaResetFromBody(body, time.Unix(int64(now), 0)); ts != nil && *ts > now {
			delay = int64(*ts - now)
		}
	}

	err := runPoolTransaction(func(pool *storage.Pool) error {
		stored := storage.FindAccount(pool, account)
		if stored != nil {
			cooldown := now + float64(delay)
			stored.RateLimitedUntil = &cooldown
			if stored.LastQuota != nil {
				var zero int64
				stored.LastQuota.UpdatedAt = &zero
			}
			stored.ErrorCount++
		}
		return nil
	})
	return delay, err
}

// RecordValidationError marks account as validation_required with 24h cooldown under transaction.
func RecordValidationError(account *storage.Account, errBody []byte) error {
	vURL := auth.ExtractValidationURL(errBody)
	now := float64(NowFunc().Unix())

	return runPoolTransaction(func(pool *storage.Pool) error {
		stored := storage.FindAccount(pool, account)
		if stored != nil {
			stored.Status = "validation_required"
			if vURL != "" {
				stored.ValidationURL = &vURL
			}
			cooldown := now + 86400 // 24h
			stored.RateLimitedUntil = &cooldown
			stored.ErrorCount++
			if stored.LastQuota != nil {
				var zero float64
				stored.LastQuota.RemainingFraction = &zero
			}
		}
		return nil
	})
}

// RecordAuthError marks account as auth_error with 1h cooldown under transaction.
func RecordAuthError(account *storage.Account) error {
	now := float64(NowFunc().Unix())

	return runPoolTransaction(func(pool *storage.Pool) error {
		stored := storage.FindAccount(pool, account)
		if stored != nil {
			stored.Status = "auth_error"
			cooldown := now + 3600 // 1h
			stored.RateLimitedUntil = &cooldown
			stored.ErrorCount++
			if stored.LastQuota != nil {
				var zero float64
				stored.LastQuota.RemainingFraction = &zero
			}
		}
		return nil
	})
}

// IsLocationError checks if a 400 or 403 error represents a temporary location restriction.
func IsLocationError(status int, body []byte) bool {
	if status != 400 && status != 403 {
		return false
	}
	text := strings.ToLower(string(body))
	markers := []string{
		"user location is not supported",
		"location is not supported",
		"location_not_supported",
		"user_location_not_supported",
		"not supported in your country",
		"not supported in your region",
		"country not supported",
		"region not supported",
	}
	for _, m := range markers {
		if strings.Contains(text, m) {
			return true
		}
	}
	if strings.Contains(text, "failed_precondition") && strings.Contains(text, "location") {
		return true
	}
	return false
}

// RecordSoftCooldown marks account with a soft 30s cooldown under transaction.
func RecordSoftCooldown(account *storage.Account) error {
	now := float64(NowFunc().Unix())
	return runPoolTransaction(func(pool *storage.Pool) error {
		stored := storage.FindAccount(pool, account)
		if stored != nil {
			cooldown := now + 30 // 30s soft cooldown
			stored.RateLimitedUntil = &cooldown
			stored.ErrorCount++
		}
		return nil
	})
}
