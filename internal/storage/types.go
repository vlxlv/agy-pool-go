package storage

import (
	"encoding/json"
)

// QuotaWindow represents a single quota window (e.g. 5-hour or weekly).
type QuotaWindow struct {
	Fraction  *float64 `json:"fraction,omitempty"`
	ResetTime any      `json:"reset_time,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

func (w *QuotaWindow) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	type alias QuotaWindow
	var typed alias
	if err := json.Unmarshal(data, &typed); err != nil {
		return err
	}

	*w = QuotaWindow(typed)

	delete(raw, "fraction")
	delete(raw, "reset_time")
	if len(raw) > 0 {
		w.Extra = raw
	}
	return nil
}

func (w QuotaWindow) MarshalJSON() ([]byte, error) {
	type alias QuotaWindow
	data, err := json.Marshal(alias(w))
	if err != nil {
		return nil, err
	}

	if len(w.Extra) == 0 {
		return data, nil
	}

	var combined map[string]json.RawMessage
	if err := json.Unmarshal(data, &combined); err != nil {
		return nil, err
	}
	for k, v := range w.Extra {
		if _, exists := combined[k]; !exists {
			combined[k] = v
		}
	}
	return json.Marshal(combined)
}

// QuotaState represents the last recorded quota telemetry for an account.
type QuotaState struct {
	Gemini5H          *QuotaWindow `json:"gemini_5h,omitempty"`
	GeminiWeekly      *QuotaWindow `json:"gemini_weekly,omitempty"`
	ThirdParty5H      *QuotaWindow `json:"third_party_5h,omitempty"`
	ThirdPartyWeekly  *QuotaWindow `json:"third_party_weekly,omitempty"`
	RemainingFraction *float64     `json:"remaining_fraction,omitempty"`
	ResetTime         any          `json:"reset_time,omitempty"`
	UpdatedAt         *int64       `json:"updated_at,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

func (q *QuotaState) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	type alias QuotaState
	var typed alias
	if err := json.Unmarshal(data, &typed); err != nil {
		return err
	}

	*q = QuotaState(typed)

	delete(raw, "gemini_5h")
	delete(raw, "gemini_weekly")
	delete(raw, "third_party_5h")
	delete(raw, "third_party_weekly")
	delete(raw, "remaining_fraction")
	delete(raw, "reset_time")
	delete(raw, "updated_at")
	if len(raw) > 0 {
		q.Extra = raw
	}
	return nil
}

func (q QuotaState) MarshalJSON() ([]byte, error) {
	type alias QuotaState
	data, err := json.Marshal(alias(q))
	if err != nil {
		return nil, err
	}

	if len(q.Extra) == 0 {
		return data, nil
	}

	var combined map[string]json.RawMessage
	if err := json.Unmarshal(data, &combined); err != nil {
		return nil, err
	}
	for k, v := range q.Extra {
		if _, exists := combined[k]; !exists {
			combined[k] = v
		}
	}
	return json.Marshal(combined)
}

// Account represents a single user account record in the pool.
type Account struct {
	ID               string      `json:"id"`
	Name             string      `json:"name,omitempty"`
	Email            string      `json:"email,omitempty"`
	AccessToken      string      `json:"access_token,omitempty"`
	RefreshToken     string      `json:"refresh_token,omitempty"`
	IDToken          string      `json:"id_token,omitempty"`
	TokenExpiry      *float64    `json:"token_expiry,omitempty"`
	CreatedAt        *int64      `json:"created_at,omitempty"`
	UpdatedAt        *int64      `json:"updated_at,omitempty"`
	Status           string      `json:"status,omitempty"`
	ValidationURL    *string     `json:"validation_url,omitempty"`
	RateLimitedUntil *float64    `json:"rate_limited_until,omitempty"`
	RequestCount     int64       `json:"request_count"`
	GenCount         *int64      `json:"gen_count,omitempty"`
	ErrorCount       int64       `json:"error_count"`
	LastUsedAt       *int64      `json:"last_used_at,omitempty"`
	LastQuota        *QuotaState `json:"last_quota,omitempty"`

	// Legacy fields for alpha compatibility
	Quota                *float64 `json:"quota,omitempty"`
	Gemini5HPct          *float64 `json:"gemini_5h_pct,omitempty"`
	GeminiWeeklyPct      *float64 `json:"gemini_weekly_pct,omitempty"`
	Gemini5HReset        any      `json:"gemini_5h_reset,omitempty"`
	GeminiWeeklyReset    any      `json:"gemini_weekly_reset,omitempty"`
	Gemini5HResetSec     *float64 `json:"gemini_5h_reset_sec,omitempty"`
	GeminiWeeklyResetSec *float64 `json:"gemini_weekly_reset_sec,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// GetHits returns gen_count if present, falling back to request_count matching Python.
func (a *Account) GetHits() int64 {
	if a == nil {
		return 0
	}
	if a.GenCount != nil {
		return *a.GenCount
	}
	return a.RequestCount
}

func (a *Account) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	type alias Account
	var typed alias
	if err := json.Unmarshal(data, &typed); err != nil {
		return err
	}

	*a = Account(typed)

	delete(raw, "id")
	delete(raw, "name")
	delete(raw, "email")
	delete(raw, "access_token")
	delete(raw, "refresh_token")
	delete(raw, "id_token")
	delete(raw, "token_expiry")
	delete(raw, "created_at")
	delete(raw, "updated_at")
	delete(raw, "status")
	delete(raw, "validation_url")
	delete(raw, "rate_limited_until")
	delete(raw, "request_count")
	delete(raw, "gen_count")
	delete(raw, "error_count")
	delete(raw, "last_used_at")
	delete(raw, "last_quota")
	delete(raw, "quota")
	delete(raw, "gemini_5h_pct")
	delete(raw, "gemini_weekly_pct")
	delete(raw, "gemini_5h_reset")
	delete(raw, "gemini_weekly_reset")
	delete(raw, "gemini_5h_reset_sec")
	delete(raw, "gemini_weekly_reset_sec")

	if len(raw) > 0 {
		a.Extra = raw
	}
	return nil
}

func (a Account) MarshalJSON() ([]byte, error) {
	type alias Account
	data, err := json.Marshal(alias(a))
	if err != nil {
		return nil, err
	}

	if len(a.Extra) == 0 {
		return data, nil
	}

	var combined map[string]json.RawMessage
	if err := json.Unmarshal(data, &combined); err != nil {
		return nil, err
	}
	for k, v := range a.Extra {
		if _, exists := combined[k]; !exists {
			combined[k] = v
		}
	}
	return json.Marshal(combined)
}

// Pool represents the top-level agy-pool-accounts.json data structure.
type Pool struct {
	Version                 int        `json:"version"`
	Strategy                string     `json:"strategy"`
	ActiveAccountID         *string    `json:"active_account_id"`
	RoundRobinLastAccountID *string    `json:"round_robin_last_account_id,omitempty"`
	Accounts                []*Account `json:"accounts"`

	Extra map[string]json.RawMessage `json:"-"`
}

// NewEmptyPool returns a newly initialized empty pool matching Python _empty_pool().
func NewEmptyPool() *Pool {
	return &Pool{
		Version:         1,
		Strategy:        "max_quota",
		ActiveAccountID: nil,
		Accounts:        make([]*Account, 0),
	}
}

func (p *Pool) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	type alias Pool
	var typed alias
	if err := json.Unmarshal(data, &typed); err != nil {
		return err
	}

	if typed.Accounts == nil {
		typed.Accounts = make([]*Account, 0)
	}

	*p = Pool(typed)

	delete(raw, "version")
	delete(raw, "strategy")
	delete(raw, "active_account_id")
	delete(raw, "round_robin_last_account_id")
	delete(raw, "accounts")

	if len(raw) > 0 {
		p.Extra = raw
	}
	return nil
}

func (p Pool) MarshalJSON() ([]byte, error) {
	type alias Pool
	data, err := json.Marshal(alias(p))
	if err != nil {
		return nil, err
	}

	if len(p.Extra) == 0 {
		return data, nil
	}

	var combined map[string]json.RawMessage
	if err := json.Unmarshal(data, &combined); err != nil {
		return nil, err
	}
	for k, v := range p.Extra {
		if _, exists := combined[k]; !exists {
			combined[k] = v
		}
	}
	return json.Marshal(combined)
}

// ============================================================================
// Target Go-Native Architecture State Split Models
//
// In the Go-native state layout under -D DIR:
//   1. accounts.json owns durable identity and authentication credentials.
//   2. runtime.json owns ephemeral/mutable runtime counters and failover status.
//   3. quota.json owns rebuildable quota telemetry and reset schedules.
//
// During the transitional compatibility phase, Pool remains fully backwards-
// compatible with Python reference JSON while supporting migration to this split.
// ============================================================================

// TargetAccountAuthRecord defines durable identity/auth material in accounts.json.
type TargetAccountAuthRecord struct {
	ID           string   `json:"id"`
	Name         string   `json:"name,omitempty"`
	Email        string   `json:"email,omitempty"`
	AccessToken  string   `json:"access_token,omitempty"`
	RefreshToken string   `json:"refresh_token,omitempty"`
	IDToken      string   `json:"id_token,omitempty"`
	TokenExpiry  *float64 `json:"token_expiry,omitempty"`
	CreatedAt    *int64   `json:"created_at,omitempty"`
	UpdatedAt    *int64   `json:"updated_at,omitempty"`
}

// TargetAccountsFile represents the target accounts.json structure.
type TargetAccountsFile struct {
	Version  int                        `json:"version"`
	Accounts []*TargetAccountAuthRecord `json:"accounts"`
}

// TargetAccountRuntimeRecord defines mutable non-cache runtime state in runtime.json.
type TargetAccountRuntimeRecord struct {
	Status           string   `json:"status,omitempty"`
	ValidationURL    *string  `json:"validation_url,omitempty"`
	RateLimitedUntil *float64 `json:"rate_limited_until,omitempty"`
	RequestCount     int64    `json:"request_count"`
	GenCount         *int64   `json:"gen_count,omitempty"`
	ErrorCount       int64    `json:"error_count"`
	LastUsedAt       *int64   `json:"last_used_at,omitempty"`
}

// TargetRuntimeFile represents the target runtime.json structure.
type TargetRuntimeFile struct {
	Version                 int                                    `json:"version"`
	ActiveAccountID         *string                                `json:"active_account_id"`
	RoundRobinLastAccountID *string                                `json:"round_robin_last_account_id,omitempty"`
	AccountsRuntime         map[string]*TargetAccountRuntimeRecord `json:"accounts"`
}

// TargetQuotaFile represents the target quota.json structure.
type TargetQuotaFile struct {
	Version       int                    `json:"version"`
	AccountsQuota map[string]*QuotaState `json:"accounts"`
	UpdatedAt     *int64                 `json:"updated_at,omitempty"`
}
