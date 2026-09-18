package proxy

import (
	"net/http"

	"github.com/vlxlv/agy-pool-go/internal/auth"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// poolTransaction is replaceable by package tests to deterministically inject persistence failures.
var poolTransaction = storage.PoolTransaction

// RecordSuccess updates account counters and promotes active account under transaction.
func RecordSuccess(account *storage.Account, isGeneration bool) error {
	now := NowFunc().Unix()
	return poolTransaction(func(pool *storage.Pool) error {
		stored := storage.FindAccount(pool, account)
		if stored == nil {
			return nil
		}
		stored.RequestCount++
		if isGeneration {
			var hits int64 = 1
			if stored.GenCount != nil {
				hits = *stored.GenCount + 1
			}
			stored.GenCount = &hits
			stored.LastUsedAt = &now
		}
		activeID := ""
		if pool.ActiveAccountID != nil {
			activeID = *pool.ActiveAccountID
		}
		var active *storage.Account
		for _, a := range pool.Accounts {
			if a.ID == activeID {
				active = a
				break
			}
		}

		if activeID == "" || (active != nil && active.RateLimitedUntil != nil && *active.RateLimitedUntil > float64(now) && stored.ID != active.ID) {
			pool.ActiveAccountID = &stored.ID
		}
		return nil
	})
}

// RecordQuotaError parses Retry-After and applies rate limit cooldown under transaction.
func RecordQuotaError(account *storage.Account, headers http.Header) (int64, error) {
	retryAfterStr := ""
	if headers != nil {
		retryAfterStr = headers.Get("Retry-After")
	}
	delay := quota.ParseRetryAfter(retryAfterStr, 300)
	now := float64(NowFunc().Unix())

	err := poolTransaction(func(pool *storage.Pool) error {
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

	return poolTransaction(func(pool *storage.Pool) error {
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

	return poolTransaction(func(pool *storage.Pool) error {
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
