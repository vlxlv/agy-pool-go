package storage

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestEmptyPoolRoundtrip(t *testing.T) {
	emptyJSON := `{"version":1,"strategy":"max_quota","active_account_id":null,"accounts":[]}`
	var p Pool
	if err := json.Unmarshal([]byte(emptyJSON), &p); err != nil {
		t.Fatalf("failed to unmarshal empty pool: %v", err)
	}

	if p.Version != 1 || p.Strategy != "max_quota" || p.ActiveAccountID != nil || len(p.Accounts) != 0 {
		t.Fatalf("unexpected empty pool content: %+v", p)
	}

	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("failed to marshal empty pool: %v", err)
	}

	if string(encoded) != emptyJSON {
		t.Fatalf("expected %s, got %s", emptyJSON, string(encoded))
	}
}

func TestUnknownFieldPreservation(t *testing.T) {
	rawJSON := `{
  "version": 1,
  "strategy": "max_quota",
  "active_account_id": "acc_1",
  "custom_root_field": "preserved_root_value",
  "accounts": [
    {
      "id": "acc_1",
      "name": "Primary",
      "email": "user@example.com",
      "request_count": 5,
      "gen_count": 2,
      "error_count": 0,
      "custom_account_field": 12345,
      "last_quota": {
        "gemini_5h": {
          "fraction": 0.85,
          "custom_window_field": true
        }
      }
    }
  ]
}`

	var p Pool
	if err := json.Unmarshal([]byte(rawJSON), &p); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	// Verify typed fields
	if p.ActiveAccountID == nil || *p.ActiveAccountID != "acc_1" {
		t.Fatalf("expected active_account_id acc_1")
	}
	if len(p.Accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(p.Accounts))
	}
	acc := p.Accounts[0]
	if acc.ID != "acc_1" || acc.Name != "Primary" || acc.RequestCount != 5 {
		t.Fatalf("unexpected account values: %+v", acc)
	}

	// Verify root unknown field
	if p.Extra == nil || string(p.Extra["custom_root_field"]) != `"preserved_root_value"` {
		t.Fatalf("root extra field missing or corrupted: %+v", p.Extra)
	}

	// Verify account unknown field
	if acc.Extra == nil || string(acc.Extra["custom_account_field"]) != "12345" {
		t.Fatalf("account extra field missing or corrupted: %+v", acc.Extra)
	}

	// Verify window unknown field
	if acc.LastQuota.Gemini5H.Extra == nil || string(acc.LastQuota.Gemini5H.Extra["custom_window_field"]) != "true" {
		t.Fatalf("window extra field missing or corrupted: %+v", acc.LastQuota.Gemini5H.Extra)
	}

	// Re-marshal and verify fields exist
	reencoded, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(reencoded, &parsed); err != nil {
		t.Fatalf("unmarshal reencoded failed: %v", err)
	}

	if parsed["custom_root_field"] != "preserved_root_value" {
		t.Fatalf("re-encoded root extra field lost")
	}

	accs := parsed["accounts"].([]any)
	acc0 := accs[0].(map[string]any)
	if acc0["custom_account_field"] != float64(12345) {
		t.Fatalf("re-encoded account extra field lost")
	}

	lq := acc0["last_quota"].(map[string]any)
	g5 := lq["gemini_5h"].(map[string]any)
	if g5["custom_window_field"] != true {
		t.Fatalf("re-encoded window extra field lost")
	}
}

func TestInvalidPoolSchemaFails(t *testing.T) {
	// Accounts is not a list
	invalid := `{"version": 1, "strategy": "max_quota", "accounts": "invalid"}`
	var p Pool
	if err := json.Unmarshal([]byte(invalid), &p); err == nil {
		t.Fatalf("expected error when accounts is not a list")
	}
}

func TestLegacyFieldsDecoded(t *testing.T) {
	legacyJSON := `{
  "version": 1,
  "strategy": "max_quota",
  "accounts": [
    {
      "id": "acc_legacy",
      "quota": 0.75,
      "gemini_5h_pct": 80.0,
      "gemini_weekly_pct": 90.0,
      "gemini_5h_reset_sec": 3600.0,
      "request_count": 10,
      "gen_count": 4,
      "error_count": 0
    }
  ]
}`
	var p Pool
	if err := json.Unmarshal([]byte(legacyJSON), &p); err != nil {
		t.Fatalf("failed to decode legacy pool: %v", err)
	}
	acc := p.Accounts[0]
	if acc.Quota == nil || *acc.Quota != 0.75 {
		t.Fatalf("expected legacy quota 0.75")
	}
	if acc.Gemini5HPct == nil || *acc.Gemini5HPct != 80.0 {
		t.Fatalf("expected gemini_5h_pct 80.0")
	}
	if acc.Gemini5HResetSec == nil || *acc.Gemini5HResetSec != 3600.0 {
		t.Fatalf("expected gemini_5h_reset_sec 3600.0")
	}
}

func TestAbsentVsZero(t *testing.T) {
	data := `{"id":"acc_1","request_count":0,"gen_count":0,"error_count":0}`
	var acc Account
	if err := json.Unmarshal([]byte(data), &acc); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if acc.RateLimitedUntil != nil {
		t.Fatalf("expected RateLimitedUntil to be nil (absent), got %v", acc.RateLimitedUntil)
	}
	if acc.LastUsedAt != nil {
		t.Fatalf("expected LastUsedAt to be nil (absent), got %v", acc.LastUsedAt)
	}
	if acc.TokenExpiry != nil {
		t.Fatalf("expected TokenExpiry to be nil (absent), got %v", acc.TokenExpiry)
	}

	dataZero := `{"id":"acc_1","rate_limited_until":0.0,"request_count":0,"gen_count":0,"error_count":0}`
	var accZero Account
	if err := json.Unmarshal([]byte(dataZero), &accZero); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if accZero.RateLimitedUntil == nil || *accZero.RateLimitedUntil != 0.0 {
		t.Fatalf("expected RateLimitedUntil to be 0.0, got %v", accZero.RateLimitedUntil)
	}
	_ = reflect.TypeOf(acc)
}
