package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/auth"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/scheduler"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// Dynamic provider hooks for testing and entrypoint configuration.
var (
	BackendHostProvider func() string
	BackendURLProvider  func() string
	UserAgentProvider   func() string
	TokenRefresher      func(account *storage.Account) (string, error)
	QuotaRefresher      func(account *storage.Account) = quota.ScheduleQuotaRefreshSimple
	LogRotator          func()
	NowFunc                       = time.Now
	LogWriter           io.Writer = os.Stderr
	logMu               sync.Mutex

	// AuditObserver is an optional hook invoked on physical generation dispatch attempts.
	AuditObserver func(accountName string, status int, attempt int, failoverReason string, path string)
)

// Hop-by-hop headers per RFC 7230 matching Python proxy.
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"proxy-connection":    true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// HopByHopNames returns the complete set of hop-by-hop headers for a given header map.
func HopByHopNames(headers http.Header) map[string]bool {
	names := make(map[string]bool, len(hopByHopHeaders))
	for k, v := range hopByHopHeaders {
		names[k] = v
	}
	for _, val := range headers.Values("Connection") {
		for _, part := range strings.Split(val, ",") {
			trimmed := strings.ToLower(strings.TrimSpace(part))
			if trimmed != "" {
				names[trimmed] = true
			}
		}
	}
	return names
}

// GetBackendHost returns the target backend host.
func GetBackendHost() string {
	if BackendHostProvider != nil {
		return BackendHostProvider()
	}
	return quota.BackendHost
}

// GetBackendURL returns the target backend URL base.
func GetBackendURL() string {
	if BackendURLProvider != nil {
		return BackendURLProvider()
	}
	return quota.BackendURLBase
}

// GetUserAgent returns the default proxy user agent.
func GetUserAgent() string {
	if UserAgentProvider != nil {
		return UserAgentProvider()
	}
	return quota.DefaultUA
}

func doRefreshToken(account *storage.Account) (string, error) {
	if TokenRefresher != nil {
		return TokenRefresher(account)
	}
	return auth.RefreshToken(account)
}

func doScheduleQuotaRefresh(account *storage.Account) {
	if QuotaRefresher != nil {
		QuotaRefresher(account)
	}
}

func maybeRotateLog() {
	if LogRotator != nil {
		LogRotator()
	}
}

func logMessage(prefix, msg string) {
	logMu.Lock()
	defer logMu.Unlock()
	if LogWriter != nil {
		nowStr := NowFunc().Format("2006-01-02 15:04:05")
		fmt.Fprintf(LogWriter, "[%s] [%s] %s\n", nowStr, prefix, msg)
	}
}

// ReadRequestBody validates Content-Length / Transfer-Encoding constraints and returns body bytes.
func ReadRequestBody(r *http.Request) ([]byte, error) {
	contentLengths := r.Header.Values("Content-Length")
	te := r.Header.Get("Transfer-Encoding")

	if len(contentLengths) > 0 && te != "" {
		return nil, errors.New("both Content-Length and Transfer-Encoding are present")
	}

	// Check conflicting distinct Content-Length values
	if len(contentLengths) > 1 {
		first := contentLengths[0]
		for _, cl := range contentLengths[1:] {
			if cl != first {
				return nil, errors.New("conflicting Content-Length headers")
			}
		}
	}

	if len(contentLengths) > 0 {
		clVal, err := strconv.ParseInt(strings.TrimSpace(contentLengths[0]), 10, 64)
		if err != nil {
			return nil, errors.New("invalid Content-Length")
		}
		if clVal < 0 {
			return nil, errors.New("negative Content-Length")
		}
		buf := make([]byte, clVal)
		n, err := io.ReadFull(r.Body, buf)
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("unexpected EOF in request body (read %d of %d bytes): %w", n, clVal, err)
		}
		if int64(n) < clVal {
			return nil, errors.New("unexpected EOF in request body")
		}
		return buf, nil
	}

	if te != "" {
		codings := strings.Split(te, ",")
		for i := range codings {
			codings[i] = strings.ToLower(strings.TrimSpace(codings[i]))
		}
		if len(codings) != 1 || codings[0] != "chunked" {
			return nil, errors.New("unsupported Transfer-Encoding")
		}
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// IsGenerationRequest reports whether the request targets a content generation endpoint.
func IsGenerationRequest(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	path := r.URL.RequestURI()
	if path == "" {
		path = r.URL.Path
	}
	return strings.Contains(strings.ToLower(path), "generatecontent")
}

// IsStreamRequest reports whether the request targets a streaming endpoint or expects text/event-stream.
func IsStreamRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if IsGenerationRequest(r) {
		return true
	}
	path := ""
	if r.URL != nil {
		path = r.URL.RequestURI()
		if path == "" {
			path = r.URL.Path
		}
	}
	return strings.Contains(strings.ToLower(path), "stream") ||
		strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream")
}

// FilterRequestHeaders builds upstream headers, rewriting Host, Auth, User-Agent, and Accept-Encoding.
func FilterRequestHeaders(r *http.Request, accessToken, clientUA string) http.Header {
	excluded := HopByHopNames(r.Header)
	excluded["host"] = true
	excluded["content-length"] = true
	excluded["authorization"] = true
	excluded["accept-encoding"] = true
	excluded["expect"] = true
	if IsGenerationRequest(r) {
		excluded["idempotency-key"] = true
		excluded["x-idempotency-key"] = true
	}

	headers := make(http.Header)
	for k, vv := range r.Header {
		if !excluded[strings.ToLower(k)] {
			for _, v := range vv {
				headers.Add(k, v)
			}
		}
	}

	headers.Set("Host", GetBackendHost())
	headers.Set("Authorization", "Bearer "+accessToken)
	headers.Set("User-Agent", clientUA)
	headers.Set("Accept-Encoding", "identity")
	return headers
}

// FilterResponseHeaders strips hop-by-hop, Content-Length, and Transfer-Encoding headers.
func FilterResponseHeaders(headers http.Header) http.Header {
	excluded := HopByHopNames(headers)
	excluded["content-length"] = true
	excluded["transfer-encoding"] = true

	out := make(http.Header)
	for k, vv := range headers {
		if !excluded[strings.ToLower(k)] {
			for _, v := range vv {
				out.Add(k, v)
			}
		}
	}
	return out
}

// SendBuffered writes a buffered HTTP response with Content-Length and Connection: close.
func SendBuffered(w http.ResponseWriter, status int, headers http.Header, body []byte) {
	committer := NewResponseCommitter(w)

	filtered := FilterResponseHeaders(headers)
	for k, vv := range filtered {
		for _, v := range vv {
			committer.Header().Add(k, v)
		}
	}
	committer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	committer.Header().Set("Connection", "close")
	committer.WriteHeader(status)
	_, _ = committer.Write(body)
	committer.MarkBody()
	committer.Flush()
}

// Handler implements the HTTP proxy handler matching Python SmartProxyHandler.
type Handler struct{}

// NewHandler creates a new proxy handler.
func NewHandler() *Handler {
	return &Handler{}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.HandleProxy(w, r)
}

func (h *Handler) HandleProxy(w http.ResponseWriter, r *http.Request) {
	path := r.URL.RequestURI()
	if path == "" {
		path = "/"
	}

	body, err := ReadRequestBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	isGeneration := IsGenerationRequest(r)
	isSSE := IsStreamRequest(r)

	maybeRotateLog()

	pool, err := storage.LoadPool()
	if err != nil || pool == nil || len(pool.Accounts) == 0 {
		http.Error(w, "No accounts configured in agy-pool", http.StatusServiceUnavailable)
		return
	}

	now := float64(NowFunc().Unix())
	strategy := pool.Strategy
	if strategy == "" {
		strategy = "max_quota"
	}

	var candidates []*storage.Account
	if isGeneration {
		if strategy == "round_robin" {
			var rrErr error
			candidates, rrErr = scheduler.ReserveRoundRobinCandidates(now)
			if rrErr != nil {
				candidates = scheduler.OrderCandidates(pool.Accounts, strategy, pool, now)
			}
		} else {
			candidates = scheduler.OrderCandidates(pool.Accounts, strategy, pool, now)
		}
	} else {
		// Non-generation: prefer active account, deprioritize restricted/cooldown accounts
		activeID := ""
		if pool.ActiveAccountID != nil {
			activeID = *pool.ActiveAccountID
		}
		candidates = make([]*storage.Account, len(pool.Accounts))
		copy(candidates, pool.Accounts)

		sort.SliceStable(candidates, func(i, j int) bool {
			score := func(a *storage.Account) int {
				if a.Status == "validation_required" || a.Status == "auth_error" {
					return 3
				}
				if a.RateLimitedUntil != nil && *a.RateLimitedUntil > now {
					return 2
				}
				if a.ID == activeID {
					return 0
				}
				return 1
			}
			return score(candidates[i]) < score(candidates[j])
		})
	}

	if isGeneration && len(candidates) > 0 && quota.RefreshNeeded(candidates[0], now) {
		doScheduleQuotaRefresh(candidates[0])
	}

	clientUA := r.Header.Get("User-Agent")
	if !strings.HasPrefix(clientUA, "antigravity/") {
		clientUA = GetUserAgent()
	}

	committer := NewResponseCommitter(w)

	var lastError error
	attempt := 0
	for _, acc := range candidates {
		attempt++
		outcome := h.dispatchAccount(committer, r, path, body, isGeneration, isSSE, acc, clientUA, attempt)
		if outcome.Err != nil {
			lastError = outcome.Err
		}

		// Structural invariant: once committed, never advance to another account.
		if committer.IsCommitted() {
			return
		}

		// Only ActionFailoverNext may advance to another account.
		if outcome.Action == ActionFailoverNext {
			continue
		}

		// Any other outcome is terminal.
		return
	}

	// All accounts exhausted
	if !committer.IsCommitted() {
		logMessage("ALL EXHAUSTED", fmt.Sprintf("%s %s failed across all accounts: %v", r.Method, filepathBase(path), lastError))
		http.Error(committer, fmt.Sprintf("All accounts in pool exhausted or unavailable: %v", lastError), http.StatusServiceUnavailable)
	}
}

func (h *Handler) dispatchAccount(
	committer *ResponseCommitter,
	r *http.Request,
	path string,
	body []byte,
	isGeneration bool,
	isSSE bool,
	acc *storage.Account,
	clientUA string,
	attempt int,
) DispatchOutcome {
	dispName := accounts.DisplayAccountName(acc)
	notifyAudit := func(status int, reason string) {
		if isGeneration && AuditObserver != nil {
			AuditObserver(dispName, status, attempt, reason, path)
		}
	}

	at, err := doRefreshToken(acc)
	if err != nil {
		logMessage("PROXY WARN", fmt.Sprintf("Token refresh failed for %s: %v. Switching to next account...", accounts.DisplayAccountName(acc), err))
		notifyAudit(401, "TokenRefreshFailed")
		RecordAuthError(acc)
		return DispatchOutcome{
			Action: ActionFailoverNext,
			Reason: "TokenRefreshFailed",
			Err:    err,
		}
	}

	targetURL := GetBackendURL() + path
	reqHeaders := FilterRequestHeaders(r, at, clientUA)

	var reqBody io.Reader
	if r.Method == "POST" && len(body) == 0 {
		reqHeaders.Set("Content-Length", "0")
		reqBody = bytes.NewReader([]byte{})
	} else if len(body) > 0 {
		reqBody = bytes.NewReader(body)
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, reqBody)
	if err != nil {
		return DispatchOutcome{
			Action: ActionTerminal,
			Reason: "RequestCreationFailed",
			Err:    err,
		}
	}
	upstreamReq.Header = reqHeaders

	// CRITICAL AUDIT: Ensure req.GetBody is nil so net/http cannot automatically rewind/retry POST
	upstreamReq.GetBody = nil
	if isGeneration {
		upstreamReq.Close = true
	}

	client := GetClient(isGeneration)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		// Check client disconnect
		if errors.Is(r.Context().Err(), context.Canceled) {
			return DispatchOutcome{
				Action: ActionTerminal,
				Reason: "ClientDisconnected",
				Err:    ErrClientDisconnected,
			}
		}

		pathBase := filepathBase(path)
		dispName := accounts.DisplayAccountName(acc)
		logMessage("PROXY EXCEPTION", fmt.Sprintf("%s %s -> %s: %v", r.Method, pathBase, dispName, err))
		notifyAudit(502, "AmbiguousTransportFailure")

		// Ambiguous transport failure: DO NOT REPLAY!
		if IsTimeoutError(err) {
			http.Error(committer, "Upstream request failed", http.StatusGatewayTimeout)
		} else {
			http.Error(committer, "Upstream request failed", http.StatusBadGateway)
		}
		return DispatchOutcome{
			Action: ActionTerminal,
			Reason: "AmbiguousTransportFailure",
			Err:    ErrAmbiguousTransport,
		}
	}
	defer resp.Body.Close()

	statusCode := resp.StatusCode
	pathBase := filepathBase(path)
	dispName = accounts.DisplayAccountName(acc)

	if statusCode >= 400 {
		errBody, _ := io.ReadAll(resp.Body)

		logMessage("PROXY ERROR", fmt.Sprintf("%s %s -> %s HTTP %d", r.Method, pathBase, dispName, statusCode))

		// Failover on Google Security Validation Required
		if auth.IsValidationError(statusCode, errBody) {
			RecordValidationError(acc, errBody)
			logMessage("FAILOVER", fmt.Sprintf("Account %s requires account verification! Auto-switching to next account...", dispName))
			notifyAudit(statusCode, "ValidationError")
			return DispatchOutcome{
				Action: ActionFailoverNext,
				Reason: "ExplicitRetryableUpstreamResponse:ValidationError",
			}
		}

		// Failover on 429 or 403 quota exhaustion
		if quota.IsQuotaError(statusCode, errBody) {
			delay := RecordQuotaError(acc, resp.Header)
			logMessage("FAILOVER", fmt.Sprintf("Account %s hit rate limit/quota error! Cooldown %ds. Auto-switching to next account...", dispName, delay))
			notifyAudit(statusCode, "QuotaExhausted")
			return DispatchOutcome{
				Action: ActionFailoverNext,
				Reason: "ExplicitRetryableUpstreamResponse:QuotaExhausted",
			}
		}

		// Failover on 401 or recognized auth failure
		if auth.IsAuthError(statusCode, errBody) {
			RecordAuthError(acc)
			logMessage("FAILOVER", fmt.Sprintf("Account %s auth/permission failure! Auto-switching to next account...", dispName))
			notifyAudit(statusCode, "AuthFailure")
			return DispatchOutcome{
				Action: ActionFailoverNext,
				Reason: "ExplicitRetryableUpstreamResponse:AuthFailure",
			}
		}

		// Ordinary upstream error (400, 404, 500, etc.): Forward directly, DO NOT REPLAY!
		notifyAudit(statusCode, "NonRetryableError")
		SendBuffered(committer, statusCode, resp.Header, errBody)
		return DispatchOutcome{
			Action: ActionTerminal,
			Reason: "ExplicitNonRetryableUpstreamResponse",
		}
	}

	// 2xx response
	notifyAudit(statusCode, "")
	logMessage("PROXY", fmt.Sprintf("%s %s -> %s (Status: %d)", r.Method, pathBase, dispName, statusCode))

	respContentType := strings.ToLower(resp.Header.Get("Content-Type"))
	isStream := isSSE || strings.Contains(respContentType, "text/event-stream")

	if isStream {
		RecordSuccess(acc, isGeneration)
		streamErr := SendStream(committer, statusCode, resp.Header, resp.Body)
		if streamErr != nil {
			return DispatchOutcome{
				Action: ActionTerminal,
				Reason: "CommittedStreamTruncated",
				Err:    streamErr,
			}
		}
		return DispatchOutcome{
			Action: ActionTerminal,
			Reason: "CommittedStreamSuccess",
		}
	}

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		logMessage("PROXY EXCEPTION", fmt.Sprintf("%s %s -> %s: read error: %v", r.Method, pathBase, dispName, readErr))
		if IsTimeoutError(readErr) {
			http.Error(committer, "Upstream request failed", http.StatusGatewayTimeout)
		} else {
			http.Error(committer, "Upstream request failed", http.StatusBadGateway)
		}
		return DispatchOutcome{
			Action: ActionTerminal,
			Reason: "AmbiguousTransportFailure:BodyReadError",
			Err:    readErr,
		}
	}

	RecordSuccess(acc, isGeneration)
	SendBuffered(committer, statusCode, resp.Header, respBody)
	return DispatchOutcome{
		Action: ActionTerminal,
		Reason: "TerminalSuccess",
	}
}

func filepathBase(path string) string {
	idx := strings.IndexByte(path, '?')
	p := path
	if idx >= 0 {
		p = path[:idx]
	}
	parts := strings.Split(strings.TrimRight(p, "/"), "/")
	if len(parts) > 0 && parts[len(parts)-1] != "" {
		return parts[len(parts)-1]
	}
	return "/"
}
