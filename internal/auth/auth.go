package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// DecodeCred decodes an obfuscated hex string by XORing each byte with key.
func DecodeCred(h string, k byte) string {
	b, err := hex.DecodeString(h)
	if err != nil {
		return ""
	}
	out := make([]byte, len(b))
	for i, v := range b {
		out[i] = v ^ k
	}
	return string(out)
}

const (
	defaultClientIDHex     = "6b6a6d6b6a6a6c6a6c6a6f636b772e3732292933346832686b3639283f68696f2c2e35363530326e3d6e6a693f2a743b2a2a29743d35353d363f2f293f283935342e3f342e74393537"
	defaultClientSecretHex = "1d1519090a0277116f621c0d086e626c163e16106b371618622902196e206c2b1e1b3c"
)

var (
	// DefaultClientID is the decoded default Google OAuth Client ID.
	DefaultClientID = DecodeCred(defaultClientIDHex, 0x5A)

	// DefaultClientSecret is the decoded default Google OAuth Client Secret.
	DefaultClientSecret = DecodeCred(defaultClientSecretHex, 0x5A)

	// OAuthScopes matches the exact OAuth scopes requested by Python agy-pool.
	OAuthScopes = "openid email profile " +
		"https://www.googleapis.com/auth/userinfo.email " +
		"https://www.googleapis.com/auth/userinfo.profile " +
		"https://www.googleapis.com/auth/cloud-platform " +
		"https://www.googleapis.com/auth/cclog " +
		"https://www.googleapis.com/auth/experimentsandconfigs " +
		"https://www.googleapis.com/auth/aicode"
)

// GetClientID returns the configured Client ID or default.
func GetClientID() string {
	if val := os.Getenv("AGY_CLIENT_ID"); val != "" {
		return val
	}
	return DefaultClientID
}

// GetClientSecret returns the configured Client Secret or default.
func GetClientSecret() string {
	if val := os.Getenv("AGY_CLIENT_SECRET"); val != "" {
		return val
	}
	return DefaultClientSecret
}

// Configurable hooks for testing.
var (
	TokenURL   = "https://oauth2.googleapis.com/token"
	NowFunc    = time.Now
	HTTPClient = &http.Client{Timeout: 10 * time.Second}
)

// DecodeJWTPayload decodes the unverified payload claims from a JWT string.
func DecodeJWTPayload(jwtStr string) map[string]any {
	parts := strings.Split(jwtStr, ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	payloadB64 := parts[1]
	if rem := len(payloadB64) % 4; rem != 0 {
		payloadB64 += strings.Repeat("=", 4-rem)
	}
	data, err := base64.URLEncoding.DecodeString(payloadB64)
	if err != nil {
		return map[string]any{}
	}
	var claims map[string]any
	if err := json.Unmarshal(data, &claims); err != nil {
		return map[string]any{}
	}
	if claims == nil {
		return map[string]any{}
	}
	return claims
}

// IsValidationError checks if the response indicates Google account verification is required.
func IsValidationError(status int, body []byte) bool {
	if status != 403 {
		return false
	}
	lower := strings.ToLower(string(body))
	markers := []string{"validation_required", "verify your account", "verify your account to continue"}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// ExtractValidationURL extracts a Google verification/reauth URL from the error body.
func ExtractValidationURL(body []byte) string {
	var data struct {
		Error struct {
			Details []struct {
				Metadata map[string]string `json:"metadata"`
				Links    []struct {
					Description string `json:"description"`
					URL         string `json:"url"`
				} `json:"links"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return ""
	}
	for _, item := range data.Error.Details {
		if u, ok := item.Metadata["validation_url"]; ok && u != "" {
			return u
		}
		for _, link := range item.Links {
			desc := strings.ToLower(link.Description)
			u := strings.ToLower(link.URL)
			if strings.Contains(desc, "verify") || strings.Contains(u, "verify") {
				return link.URL
			}
		}
	}
	return ""
}

// IsAuthError checks if the status or response indicates authentication failure.
func IsAuthError(status int, body []byte) bool {
	if status == 401 {
		return true
	}
	if status == 403 {
		lower := strings.ToLower(string(body))
		markers := []string{
			"unauthenticated", "invalid_grant", "access_token_expired",
			"account is disabled", "user disabled",
		}
		for _, m := range markers {
			if strings.Contains(lower, m) {
				return true
			}
		}
	}
	return false
}

// PersistAccountFields persists specified account fields into the storage pool.
func PersistAccountFields(account *storage.Account, updateTokens, updateStatus, updateQuota bool) error {
	return storage.PoolTransaction(func(pool *storage.Pool) error {
		stored := storage.FindAccount(pool, account)
		if stored == nil {
			return fmt.Errorf("account '%s' does not exist in pool", account.ID)
		}
		if updateTokens {
			stored.AccessToken = account.AccessToken
			if account.RefreshToken != "" {
				stored.RefreshToken = account.RefreshToken
			}
			stored.TokenExpiry = account.TokenExpiry
			stored.UpdatedAt = account.UpdatedAt
			if account.IDToken != "" {
				stored.IDToken = account.IDToken
			}
		}
		if updateStatus {
			stored.Status = account.Status
			stored.ValidationURL = account.ValidationURL
			stored.RateLimitedUntil = account.RateLimitedUntil
		}
		if updateQuota {
			stored.LastQuota = account.LastQuota
		}
		return nil
	})
}

// RefreshToken refreshes the access token using refresh_token if close to expiry.
func safeProviderReason(body []byte) string {
	var envelope struct {
		Error struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return ""
	}
	code := envelope.Error.Code
	if code == "" {
		code = envelope.Error.Error
	}
	if code == "" {
		return ""
	}
	switch code {
	case "invalid_grant", "invalid_client", "invalid_request", "unauthorized_client", "access_denied":
		return ": " + code
	default:
		return ""
	}
}

func RefreshToken(account *storage.Account) (string, error) {
	if account == nil {
		return "", errors.New("nil account")
	}

	now := NowFunc()
	nowUnix := float64(now.Unix())

	// If valid for at least another 2 minutes, return existing access_token
	if account.AccessToken != "" && account.TokenExpiry != nil && (*account.TokenExpiry-nowUnix > 120) {
		return account.AccessToken, nil
	}

	if account.RefreshToken == "" {
		return "", errors.New("Missing refresh_token")
	}

	lockKey := account.ID
	if lockKey == "" {
		lockKey = account.Email
	}
	if lockKey == "" {
		lockKey = account.RefreshToken
	}
	h := sha256.Sum256([]byte(lockKey))
	lockName := hex.EncodeToString(h[:])[:24]
	lockPath := filepath.Join(config.GetLocksDir(), fmt.Sprintf("refresh-%s.lock", lockName))

	var refreshedToken string
	err := storage.WithFileLock(lockPath, true, func() error {
		// Verify account exists in state store before proceeding
		storedPool, err := storage.LoadPool()
		if err != nil {
			return fmt.Errorf("failed to load pool state: %w", err)
		}
		stored := storage.FindAccount(storedPool, account)
		if stored == nil {
			return fmt.Errorf("account '%s' not found", account.ID)
		}

		// Another goroutine/process may have refreshed while waiting for lock.
		if stored.AccessToken != "" {
			account.AccessToken = stored.AccessToken
		}
		if stored.RefreshToken != "" {
			account.RefreshToken = stored.RefreshToken
		}
		if stored.TokenExpiry != nil {
			account.TokenExpiry = stored.TokenExpiry
		}
		if stored.UpdatedAt != nil {
			account.UpdatedAt = stored.UpdatedAt
		}
		if stored.IDToken != "" {
			account.IDToken = stored.IDToken
		}

		currentNow := NowFunc()
		currentNowUnix := float64(currentNow.Unix())
		if account.AccessToken != "" && account.TokenExpiry != nil && (*account.TokenExpiry-currentNowUnix > 120) {
			refreshedToken = account.AccessToken
			return nil
		}

		rf := account.RefreshToken
		if rf == "" {
			return errors.New("Missing refresh_token")
		}

		form := url.Values{
			"client_id":     {GetClientID()},
			"client_secret": {GetClientSecret()},
			"refresh_token": {rf},
			"grant_type":    {"refresh_token"},
		}

		resp, err := HTTPClient.PostForm(TokenURL, form)
		if err != nil {
			return fmt.Errorf("token refresh network request failed: %w", err)
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read token refresh response: %w", err)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("token refresh failed (HTTP %d%s)", resp.StatusCode, safeProviderReason(respBody))
		}

		var res struct {
			AccessToken  string  `json:"access_token"`
			RefreshToken string  `json:"refresh_token"`
			ExpiresIn    float64 `json:"expires_in"`
		}
		if err := json.Unmarshal(respBody, &res); err != nil {
			return fmt.Errorf("failed to parse token refresh JSON: %w", err)
		}

		if res.AccessToken == "" {
			return errors.New("Token refresh failed: No access_token returned")
		}

		expiresIn := res.ExpiresIn
		if expiresIn <= 0 {
			expiresIn = 3600
		}

		account.AccessToken = res.AccessToken
		if res.RefreshToken != "" {
			account.RefreshToken = res.RefreshToken
		}
		expiry := currentNowUnix + expiresIn
		account.TokenExpiry = &expiry
		updatedAt := currentNow.Unix()
		account.UpdatedAt = &updatedAt

		if err := PersistAccountFields(account, true, false, false); err != nil {
			return fmt.Errorf("failed to persist refreshed tokens: %w", err)
		}

		refreshedToken = res.AccessToken
		return nil
	})

	if err != nil {
		return "", err
	}
	return refreshedToken, nil
}
