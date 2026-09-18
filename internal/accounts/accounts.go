package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/auth"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// Quota callback types.
type QuotaProberFunc func(account *storage.Account) (*storage.QuotaState, error)
type QuotaFormatterFunc func(quota *storage.QuotaState)

var (
	quotaProber    QuotaProberFunc
	quotaFormatter QuotaFormatterFunc
	quotaMu        sync.RWMutex
)

// SetQuotaProber sets optional callbacks to probe/format quota during account management operations.
func SetQuotaProber(prober QuotaProberFunc, formatter QuotaFormatterFunc) {
	quotaMu.Lock()
	defer quotaMu.Unlock()
	quotaProber = prober
	quotaFormatter = formatter
}

func getQuotaProber() (QuotaProberFunc, QuotaFormatterFunc) {
	quotaMu.RLock()
	defer quotaMu.RUnlock()
	return quotaProber, quotaFormatter
}

// DisplayAccountName returns the user-visible friendly display name for an account.
// Never falls back to real email or values derived from email.
// Fallback order:
//  1. Explicit friendly name / label ('name' field)
//  2. 'Account N' if account id matches 'acc_N'
//  3. 'Account' as generic safe fallback
func DisplayAccountName(account *storage.Account) string {
	if account == nil {
		return "Account"
	}
	friendly := strings.TrimSpace(account.Name)
	if friendly != "" {
		return friendly
	}
	accountID := strings.TrimSpace(account.ID)
	if strings.HasPrefix(accountID, "acc_") {
		suffix := accountID[4:]
		if isDigits(suffix) {
			if n, err := strconv.Atoi(suffix); err == nil {
				return fmt.Sprintf("Account %d", n)
			}
		}
	}
	return "Account"
}

func isDigits(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// FindAccountByTarget resolves a target selector (1-based index, account ID, exact email, or friendly name).
func FindAccountByTarget(accounts []*storage.Account, target string) *storage.Account {
	targetStr := strings.TrimSpace(target)
	if targetStr == "" {
		return nil
	}
	if isDigits(targetStr) {
		idx, err := strconv.Atoi(targetStr)
		if err == nil {
			i := idx - 1
			if i >= 0 && i < len(accounts) {
				return accounts[i]
			}
			return nil
		}
	}
	for _, a := range accounts {
		if a == nil {
			continue
		}
		if a.ID == targetStr || a.Email == targetStr || a.Name == targetStr {
			return a
		}
	}
	return nil
}

func formatISOExpiry(ts float64) string {
	t := time.Unix(int64(ts), 0).UTC()
	return t.Format("2006-01-02T15:04:05+00:00")
}

// WriteAgyTokenFile writes the given account credentials into the native agy token file.
func WriteAgyTokenFile(account *storage.Account) error {
	if account == nil {
		return errors.New("nil account")
	}
	if err := storage.EnsureDirs(); err != nil {
		return err
	}

	tokenFile := config.GetAgyTokenFile()
	if err := config.AssertSafeWritePath(tokenFile); err != nil {
		return err
	}
	lockFile := tokenFile + ".lock"
	if err := config.AssertSafeWritePath(lockFile); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
		return fmt.Errorf("failed to create agy-cli directory: %w", err)
	}

	expiryTS := time.Now().Unix() + 3600
	if account.TokenExpiry != nil && *account.TokenExpiry > 0 {
		expiryTS = int64(*account.TokenExpiry)
	}

	payload := map[string]any{
		"token": map[string]any{
			"access_token":  account.AccessToken,
			"token_type":    "Bearer",
			"refresh_token": account.RefreshToken,
			"expiry":        formatISOExpiry(float64(expiryTS)),
		},
		"auth_method": "consumer",
		"id_token":    account.IDToken,
	}

	return storage.WithFileLock(lockFile, true, func() error {
		return storage.AtomicJSONWrite(tokenFile, payload)
	})
}

// SyncActiveAgyTokenFile synchronizes native agy's compatibility token with the current active account.
func SyncActiveAgyTokenFile() (bool, error) {
	tokenFile := config.GetAgyTokenFile()
	if err := config.AssertSafeWritePath(tokenFile); err != nil {
		return false, err
	}
	lockFile := tokenFile + ".lock"
	if err := config.AssertSafeWritePath(lockFile); err != nil {
		return false, err
	}

	var synced bool
	err := storage.WithFileLock(lockFile, true, func() error {
		for attempt := 0; attempt < 3; attempt++ {
			pool, err := storage.LoadPool()
			if err != nil {
				return err
			}
			if pool.ActiveAccountID == nil {
				return nil
			}
			activeID := *pool.ActiveAccountID
			var account *storage.Account
			for _, a := range pool.Accounts {
				if a.ID == activeID {
					account = a
					break
				}
			}
			if account == nil {
				return nil
			}

			// Attempt refresh if near expiry
			_, _ = auth.RefreshToken(account)

			// Re-verify active account did not change during refresh
			refreshedPool, err := storage.LoadPool()
			if err != nil {
				return err
			}
			if refreshedPool.ActiveAccountID != nil && *refreshedPool.ActiveAccountID == activeID {
				expiryTS := time.Now().Unix() + 3600
				if account.TokenExpiry != nil && *account.TokenExpiry > 0 {
					expiryTS = int64(*account.TokenExpiry)
				}
				payload := map[string]any{
					"token": map[string]any{
						"access_token":  account.AccessToken,
						"token_type":    "Bearer",
						"refresh_token": account.RefreshToken,
						"expiry":        formatISOExpiry(float64(expiryTS)),
					},
					"auth_method": "consumer",
					"id_token":    account.IDToken,
				}
				if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
					return err
				}
				if err := storage.AtomicJSONWrite(tokenFile, payload); err != nil {
					return err
				}
				synced = true
				return nil
			}
		}
		return nil
	})

	return synced, err
}

// RemoveAccount removes an account from the pool by target selector.
func RemoveAccount(target string) (*storage.Account, error) {
	var removed *storage.Account
	err := storage.PoolTransaction(func(pool *storage.Pool) error {
		found := FindAccountByTarget(pool.Accounts, target)
		if found == nil {
			return fmt.Errorf("account '%s' not found", target)
		}
		newAccounts := make([]*storage.Account, 0, len(pool.Accounts)-1)
		for _, a := range pool.Accounts {
			if a.ID != found.ID {
				newAccounts = append(newAccounts, a)
			}
		}
		pool.Accounts = newAccounts
		if pool.ActiveAccountID != nil && *pool.ActiveAccountID == found.ID {
			if len(pool.Accounts) > 0 {
				pool.ActiveAccountID = &pool.Accounts[0].ID
			} else {
				pool.ActiveAccountID = nil
			}
		}
		if pool.RoundRobinLastAccountID != nil && *pool.RoundRobinLastAccountID == found.ID {
			pool.RoundRobinLastAccountID = nil
		}
		removed = found
		return nil
	})
	if err != nil {
		return nil, err
	}

	_, _ = SyncActiveAgyTokenFile()
	return removed, nil
}

// SwitchAccount sets the active account, either explicitly or automatically by highest quota.
func SwitchAccount(target string) (*storage.Account, error) {
	pool, err := storage.LoadPool()
	if err != nil {
		return nil, err
	}
	if len(pool.Accounts) == 0 {
		return nil, errors.New("account pool is empty")
	}

	prober, _ := getQuotaProber()
	isAuto := target == "" || target == "auto"

	if isAuto && prober != nil {
		for _, acc := range pool.Accounts {
			q, err := prober(acc)
			if err == nil && q != nil {
				acc.LastQuota = q
				_ = auth.PersistAccountFields(acc, true, true, true)
			}
		}
	}

	var selected *storage.Account
	err = storage.PoolTransaction(func(p *storage.Pool) error {
		if len(p.Accounts) == 0 {
			return errors.New("account pool is empty")
		}
		if isAuto {
			nowTS := float64(time.Now().Unix())
			sorted := make([]*storage.Account, len(p.Accounts))
			copy(sorted, p.Accounts)

			sort.SliceStable(sorted, func(i, j int) bool {
				score := func(a *storage.Account) (int, float64) {
					if a.Status == "validation_required" || a.Status == "auth_error" {
						return -2, 0.0
					}
					if a.RateLimitedUntil != nil && *a.RateLimitedUntil > nowTS {
						return -1, 0.0
					}
					var frac float64
					if a.LastQuota != nil && a.LastQuota.RemainingFraction != nil {
						frac = *a.LastQuota.RemainingFraction
					}
					return 1, frac
				}

				tierI, fracI := score(sorted[i])
				tierJ, fracJ := score(sorted[j])
				if tierI != tierJ {
					return tierI > tierJ
				}
				return fracI > fracJ
			})
			selected = sorted[0]
		} else {
			selected = FindAccountByTarget(p.Accounts, target)
			if selected == nil {
				return fmt.Errorf("account '%s' not found", target)
			}
		}

		p.ActiveAccountID = &selected.ID
		return nil
	})
	if err != nil {
		return nil, err
	}

	_, _ = auth.RefreshToken(selected)
	_, _ = SyncActiveAgyTokenFile()
	return selected, nil
}

// RenameAccount assigns a new friendly name to the targeted account.
func RenameAccount(target, newName string) (*storage.Account, error) {
	name := strings.TrimSpace(newName)
	if name == "" {
		return nil, errors.New("account name cannot be empty")
	}

	var updated *storage.Account
	err := storage.PoolTransaction(func(pool *storage.Pool) error {
		found := FindAccountByTarget(pool.Accounts, target)
		if found == nil {
			return fmt.Errorf("account '%s' not found", target)
		}
		found.Name = name
		updated = found
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// ImportCurrent imports credentials from ~/.gemini/antigravity-cli/antigravity-oauth-token into pool.
func ImportCurrent() (*storage.Account, error) {
	tokenFile := config.GetAgyTokenFile()
	if err := config.AssertSafeReadPath(tokenFile); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("no existing token file found: %w", err)
	}

	var raw struct {
		Token struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"token"`
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse token file: %w", err)
	}
	if raw.Token.RefreshToken == "" {
		return nil, errors.New("no refresh_token found in existing token file")
	}

	claims := auth.DecodeJWTPayload(raw.IDToken)
	email, _ := claims["email"].(string)
	if email == "" {
		email = "primary_user@gmail.com"
	}

	now := time.Now().Unix()
	var zeroExpiry float64 = 0

	var imported *storage.Account
	err = storage.PoolTransaction(func(pool *storage.Pool) error {
		for _, a := range pool.Accounts {
			if a.Email == email {
				a.RefreshToken = raw.Token.RefreshToken
				a.AccessToken = raw.Token.AccessToken
				a.TokenExpiry = &zeroExpiry
				a.IDToken = raw.IDToken
				imported = a
				return nil
			}
		}

		acc := &storage.Account{
			ID:           storage.NextAccountID(pool.Accounts),
			Email:        email,
			RefreshToken: raw.Token.RefreshToken,
			AccessToken:  raw.Token.AccessToken,
			TokenExpiry:  &zeroExpiry,
			IDToken:      raw.IDToken,
			CreatedAt:    &now,
			RequestCount: 0,
			ErrorCount:   0,
		}
		pool.Accounts = append(pool.Accounts, acc)
		if pool.ActiveAccountID == nil {
			pool.ActiveAccountID = &acc.ID
		}
		imported = acc
		return nil
	})
	if err != nil {
		return nil, err
	}

	prober, _ := getQuotaProber()
	if prober != nil {
		if q, err := prober(imported); err == nil && q != nil {
			imported.LastQuota = q
			_ = auth.PersistAccountFields(imported, true, false, true)
		}
	}

	return imported, nil
}

// LoginOptions configures the OAuth login flow.
type LoginOptions struct {
	AuthEndpoint  string
	TokenEndpoint string
	PromptFn      func(authURL string) (string, error)
	BrowserOpener func(url string) error
	PortRange     [2]int // inclusive start, end
	Timeout       time.Duration
	Stdout        io.Writer
}

func findFreePort(start, end int) (int, error) {
	for p := start; p <= end; p++ {
		addr := fmt.Sprintf("127.0.0.1:%d", p)
		l, err := net.Listen("tcp", addr)
		if err == nil {
			_ = l.Close()
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port available in range %d-%d", start, end)
}

// DefaultBrowserOpener attempts to open the target URL in the system default browser.
func DefaultBrowserOpener(targetURL string) error {
	for _, opener := range []string{"termux-open-url", "termux-open", "xdg-open", "open"} {
		if opener == "xdg-open" || opener == "open" {
			if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
				continue
			}
		}
		if path, err := exec.LookPath(opener); err == nil {
			cmd := exec.Command(path, targetURL)
			cmd.Stdout = nil
			cmd.Stderr = nil
			if err := cmd.Start(); err == nil {
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				select {
				case err := <-done:
					if err != nil {
						continue
					}
					return nil
				case <-time.After(100 * time.Millisecond):
					return nil
				}
			}
		}
	}
	return errors.New("no browser opener found")
}

// Login runs the OAuth 2.0 authorization code flow to add an account to the pool.
func Login(ctx context.Context, opts LoginOptions) (*storage.Account, error) {
	authEndpoint := opts.AuthEndpoint
	if authEndpoint == "" {
		authEndpoint = "https://accounts.google.com/o/oauth2/v2/auth"
	}
	tokenEndpoint := opts.TokenEndpoint
	if tokenEndpoint == "" {
		tokenEndpoint = auth.TokenURL
	}
	opener := opts.BrowserOpener
	if opener == nil {
		opener = DefaultBrowserOpener
	}
	startPort := opts.PortRange[0]
	endPort := opts.PortRange[1]
	if startPort == 0 && endPort == 0 {
		startPort = 8085
		endPort = 8135
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 180 * time.Second
	}

	port, err := findFreePort(startPort, endPort)
	if err != nil {
		return nil, err
	}

	redirectURI := fmt.Sprintf("http://localhost:%d/auth/callback", port)
	params := url.Values{
		"client_id":     {auth.GetClientID()},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {auth.OAuthScopes},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
	}
	authURL := authEndpoint + "?" + params.Encode()

	codeChan := make(chan string, 1)
	errChan := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if code := q.Get("code"); code != "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `<html><body style="font-family:sans-serif;text-align:center;padding:40px;background:#0f172a;color:#f8fafc;"><h2>Authorization Successful!</h2><p>Antigravity multi-account token has been saved.</p></body></html>`)
			select {
			case codeChan <- code:
			default:
			}
			return
		}
		if errStr := q.Get("error"); errStr != "" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "Authorization failed: "+errStr)
			select {
			case errChan <- fmt.Errorf("OAuth callback error: %s", errStr):
			default:
			}
			return
		}
		http.NotFound(w, r)
	})

	server := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: mux,
	}

	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", server.Addr, err)
	}

	go func() {
		_ = server.Serve(listener)
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	openerErr := opener(authURL)
	if openerErr == nil {
		if opts.Stdout != nil {
			fmt.Fprintln(opts.Stdout, "Opening browser for Google authentication...")
			fmt.Fprintln(opts.Stdout, "Waiting for authorization...")
		}
	} else {
		if opts.Stdout != nil {
			fmt.Fprintln(opts.Stdout, "Could not open a browser automatically.")
			fmt.Fprintln(opts.Stdout, "Open this URL:")
			fmt.Fprintln(opts.Stdout, authURL)
		}
	}

	// If prompt function provided, run it concurrently
	if opts.PromptFn != nil {
		go func() {
			val, pErr := opts.PromptFn(authURL)
			if pErr != nil {
				select {
				case errChan <- pErr:
				default:
				}
				return
			}
			val = strings.TrimSpace(val)
			if strings.Contains(val, "code=") {
				if parsed, err := url.Parse(val); err == nil {
					val = parsed.Query().Get("code")
				}
			}
			if val != "" {
				select {
				case codeChan <- val:
				default:
				}
			}
		}()
	}

	var code string
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(timeout):
		return nil, errors.New("login timed out or cancelled")
	case cErr := <-errChan:
		return nil, cErr
	case c := <-codeChan:
		code = c
	}

	// Exchange code for tokens
	tokenForm := url.Values{
		"code":          {code},
		"client_id":     {auth.GetClientID()},
		"client_secret": {auth.GetClientSecret()},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}

	resp, err := auth.HTTPClient.PostForm(tokenEndpoint, tokenForm)
	if err != nil {
		return nil, fmt.Errorf("OAuth token exchange failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read OAuth exchange response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("OAuth exchange failed HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var tokenRes struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		IDToken      string  `json:"id_token"`
		ExpiresIn    float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(respBody, &tokenRes); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}

	if tokenRes.RefreshToken == "" {
		return nil, errors.New("Google did not return a refresh_token. Please revoke access or re-run with prompt=consent")
	}

	claims := auth.DecodeJWTPayload(tokenRes.IDToken)
	email, _ := claims["email"].(string)
	if email == "" {
		email = fmt.Sprintf("user_%d@gmail.com", time.Now().Unix())
	}

	now := time.Now().Unix()
	expiresIn := tokenRes.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	expiryTS := float64(now) + expiresIn

	var account *storage.Account
	err = storage.PoolTransaction(func(pool *storage.Pool) error {
		for _, a := range pool.Accounts {
			if a.Email == email {
				a.RefreshToken = tokenRes.RefreshToken
				a.AccessToken = tokenRes.AccessToken
				a.TokenExpiry = &expiryTS
				a.IDToken = tokenRes.IDToken
				a.UpdatedAt = &now
				a.Status = ""
				a.ValidationURL = nil
				a.RateLimitedUntil = nil
				account = a
				return nil
			}
		}

		newAcc := &storage.Account{
			ID:           storage.NextAccountID(pool.Accounts),
			Email:        email,
			RefreshToken: tokenRes.RefreshToken,
			AccessToken:  tokenRes.AccessToken,
			TokenExpiry:  &expiryTS,
			IDToken:      tokenRes.IDToken,
			CreatedAt:    &now,
			UpdatedAt:    &now,
			RequestCount: 0,
			ErrorCount:   0,
		}
		pool.Accounts = append(pool.Accounts, newAcc)
		if pool.ActiveAccountID == nil {
			pool.ActiveAccountID = &newAcc.ID
		}
		account = newAcc
		return nil
	})
	if err != nil {
		return nil, err
	}

	prober, _ := getQuotaProber()
	if prober != nil {
		if q, err := prober(account); err == nil && q != nil {
			account.LastQuota = q
			_ = auth.PersistAccountFields(account, true, true, true)
		}
	}

	_, _ = SyncActiveAgyTokenFile()
	return account, nil
}

// VerifyAccount checks the verification status of an account and formats its security URL.
func VerifyAccount(target string) (*storage.Account, string, error) {
	pool, err := storage.LoadPool()
	if err != nil {
		return nil, "", err
	}
	if len(pool.Accounts) == 0 {
		return nil, "", errors.New("account pool is empty")
	}

	var targetAcc *storage.Account
	if target != "" {
		targetAcc = FindAccountByTarget(pool.Accounts, target)
	} else {
		for _, a := range pool.Accounts {
			if a.Status == "validation_required" {
				targetAcc = a
				break
			}
		}
		if targetAcc == nil {
			targetAcc = pool.Accounts[0]
		}
	}

	if targetAcc == nil {
		return nil, "", errors.New("account not found")
	}

	prober, _ := getQuotaProber()
	if prober != nil {
		if q, err := prober(targetAcc); err == nil && q != nil {
			targetAcc.LastQuota = q
			_ = auth.PersistAccountFields(targetAcc, true, true, true)
		}
	}

	refreshedPool, err := storage.LoadPool()
	if err != nil {
		return nil, "", err
	}
	currentAcc := storage.FindAccount(refreshedPool, targetAcc)
	if currentAcc == nil {
		currentAcc = targetAcc
	}

	if currentAcc.Status != "validation_required" {
		return currentAcc, "", nil
	}

	if currentAcc.ValidationURL == nil || *currentAcc.ValidationURL == "" {
		return currentAcc, "", errors.New("Google did not return a validation URL")
	}

	vURL := *currentAcc.ValidationURL
	if currentAcc.Email != "" && !strings.Contains(vURL, "&Email=") && !strings.Contains(vURL, "&login_hint=") {
		sep := "&"
		if !strings.Contains(vURL, "?") {
			sep = "?"
		}
		vURL += sep + "Email=" + url.QueryEscape(currentAcc.Email)
	}

	return currentAcc, vURL, nil
}
