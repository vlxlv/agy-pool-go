package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/auth"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/observability"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/scheduler"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// Dynamic provider hooks for testing and entrypoint configuration.
var (
	BackendHostProvider     func() string
	BackendURLProvider      func() string
	UserAgentProvider       func() string
	TokenRefresher          func(account *storage.Account) (string, error)
	TokenRefresherWithForce func(account *storage.Account, force bool) (string, error)
	QuotaRefresher          func(account *storage.Account) = quota.ScheduleQuotaRefreshSimple
	LogRotator              func()
	NowFunc                           = time.Now
	LogWriter               io.Writer = os.Stderr
	logMu                   sync.Mutex

	// AuditObserver is an optional hook invoked asynchronously outside the generation hot path.
	AuditObserver func(accountName string, status int, attempt int, failoverReason string, path string)
	// AuditObserverEvent is an optional hook receiving rich selection-time RoutingEvents outside the hot path.
	AuditObserverEvent func(ev RoutingEvent)
)

// RoutingEvent represents an immutable routing attempt event emitted on generation paths.
type RoutingEvent struct {
	Timestamp             time.Time `json:"timestamp"`
	Strategy              string    `json:"strategy"`
	Account               string    `json:"account"`
	AccountName           string    `json:"account_name"`
	AccountID             string    `json:"account_id,omitempty"`
	Classification        string    `json:"classification"` // "Healthy", "Reserve", "Depleted", "Unknown"
	HealthyCandidateCount int       `json:"healthy_candidate_count"`
	HealthyCandidates     []string  `json:"healthy_candidates,omitempty"`
	ReserveCandidateCount int       `json:"reserve_candidate_count"`
	ReserveCandidates     []string  `json:"reserve_candidates,omitempty"`

	// Identified 5H window
	FiveHourKnown      bool       `json:"five_hour_known,omitempty"`
	FiveHourRemaining  *float64   `json:"five_hour_remaining,omitempty"`
	FiveHourResetAt    *time.Time `json:"five_hour_reset_at,omitempty"`
	FiveHourResetRatio *float64   `json:"five_hour_reset_ratio,omitempty"`
	FiveHourPace       *float64   `json:"five_hour_pace,omitempty"`

	// Identified Weekly window
	WeeklyKnown      bool       `json:"weekly_known,omitempty"`
	WeeklyRemaining  *float64   `json:"weekly_remaining,omitempty"`
	WeeklyResetAt    *time.Time `json:"weekly_reset_at,omitempty"`
	WeeklyResetRatio *float64   `json:"weekly_reset_ratio,omitempty"`
	WeeklyPace       *float64   `json:"weekly_pace,omitempty"`

	// Ranking and freshness inputs
	KnownWindowCount int      `json:"known_window_count"`
	FreshnessRank    *int     `json:"freshness_rank,omitempty"`
	WorstPace        *float64 `json:"worst_pace,omitempty"`
	TotalPace        *float64 `json:"total_pace,omitempty"`
	RawFloor         *float64 `json:"raw_floor,omitempty"`

	Attempt        int    `json:"attempt"`
	Status         int    `json:"status"`
	FailoverReason string `json:"failover_reason,omitempty"`
	Path           string `json:"path"`
}

// SelectionAuditContext holds frozen pool candidate metadata computed at selection time.
type SelectionAuditContext struct {
	Strategy          string
	Now               float64
	HealthyCandidates []string
	ReserveCandidates []string
}

// DefaultAuditBufferSize defines the capacity of the non-blocking audit event queue.
const DefaultAuditBufferSize = 1024

var (
	auditQueue           = make(chan RoutingEvent, DefaultAuditBufferSize)
	droppedAuditEvents   int64
	sessionDroppedEvents int64
	activeAuditors       int32
	activeSubscribers    []chan RoutingEvent
	auditMu              sync.RWMutex
)

func init() {
	go runAuditWorker()
	quota.WarnHook = func(msg string) { logMessage("QUOTA WARN", msg) }
}

func runAuditWorker() {
	for ev := range auditQueue {
		obs := GetAuditObserver()
		obsEv := GetAuditObserverEvent()

		if obs != nil {
			func() {
				defer func() { _ = recover() }()
				obs(ev.AccountName, ev.Status, ev.Attempt, ev.FailoverReason, ev.Path)
			}()
		}
		if obsEv != nil {
			func() {
				defer func() { _ = recover() }()
				obsEv(ev)
			}()
		}
	}
}

// HasActiveAuditor reports whether any auditor or observer is actively listening for events.
func HasActiveAuditor() bool {
	return atomic.LoadInt32(&activeAuditors) > 0
}

// SubscribeAudit registers an audit listener channel with a bounded buffer.
// It resets the session dropped-event counter and returns a cancel function.
func SubscribeAudit(bufSize int) (events <-chan RoutingEvent, cancel func()) {
	if bufSize <= 0 {
		bufSize = DefaultAuditBufferSize
	}
	ch := make(chan RoutingEvent, bufSize)

	auditMu.Lock()
	atomic.StoreInt64(&sessionDroppedEvents, 0)
	activeSubscribers = append(activeSubscribers, ch)
	updateActiveAuditorsLocked()
	auditMu.Unlock()

	cancel = func() {
		auditMu.Lock()
		defer auditMu.Unlock()
		for i, sub := range activeSubscribers {
			if sub == ch {
				activeSubscribers = append(activeSubscribers[:i], activeSubscribers[i+1:]...)
				close(ch)
				break
			}
		}
		updateActiveAuditorsLocked()
	}
	return ch, cancel
}

func updateActiveAuditorsLocked() {
	count := len(activeSubscribers)
	if AuditObserver != nil {
		count++
	}
	if AuditObserverEvent != nil {
		count++
	}
	atomic.StoreInt32(&activeAuditors, int32(count))
}

// DroppedAuditEvents returns the number of audit events dropped in the active session.
func DroppedAuditEvents() int64 {
	return atomic.LoadInt64(&sessionDroppedEvents)
}

// ResetDroppedAuditEvents resets the dropped audit event counter.
func ResetDroppedAuditEvents() {
	atomic.StoreInt64(&sessionDroppedEvents, 0)
	atomic.StoreInt64(&droppedAuditEvents, 0)
}

func drainAuditQueueLocked() {
	for {
		select {
		case <-auditQueue:
		default:
			return
		}
	}
}

// SetAuditObserver safely sets the legacy audit observer callback.
func SetAuditObserver(fn func(accountName string, status int, attempt int, failoverReason string, path string)) {
	auditMu.Lock()
	AuditObserver = fn
	drainAuditQueueLocked()
	if fn != nil {
		atomic.StoreInt64(&sessionDroppedEvents, 0)
	}
	updateActiveAuditorsLocked()
	auditMu.Unlock()
}

// SetAuditObserverEvent safely sets the rich RoutingEvent audit observer callback.
func SetAuditObserverEvent(fn func(ev RoutingEvent)) {
	auditMu.Lock()
	AuditObserverEvent = fn
	drainAuditQueueLocked()
	if fn != nil {
		atomic.StoreInt64(&sessionDroppedEvents, 0)
	}
	updateActiveAuditorsLocked()
	auditMu.Unlock()
}

// GetAuditObserver safely returns the current legacy audit observer.
func GetAuditObserver() func(accountName string, status int, attempt int, failoverReason string, path string) {
	auditMu.RLock()
	defer auditMu.RUnlock()
	return AuditObserver
}

// GetAuditObserverEvent safely returns the current rich audit observer.
func GetAuditObserverEvent() func(ev RoutingEvent) {
	auditMu.RLock()
	defer auditMu.RUnlock()
	return AuditObserverEvent
}

// AuditQueueLen returns the current number of events queued in auditQueue (for testing).
func AuditQueueLen() int {
	return len(auditQueue)
}

// EmitRoutingEvent enqueues a routing event into active bounded buffers in a non-blocking manner.
// If no auditor is active, it returns false immediately without performing channel operations.
// If buffers are full, events are dropped and dropped counter is incremented.
func EmitRoutingEvent(ev RoutingEvent) bool {
	if !HasActiveAuditor() {
		return false
	}

	auditMu.RLock()
	defer auditMu.RUnlock()

	if !HasActiveAuditor() {
		return false
	}

	delivered := true
	for _, sub := range activeSubscribers {
		select {
		case sub <- ev:
		default:
			atomic.AddInt64(&sessionDroppedEvents, 1)
			atomic.AddInt64(&droppedAuditEvents, 1)
			delivered = false
		}
	}

	if AuditObserver != nil || AuditObserverEvent != nil {
		select {
		case auditQueue <- ev:
		default:
			atomic.AddInt64(&sessionDroppedEvents, 1)
			atomic.AddInt64(&droppedAuditEvents, 1)
			delivered = false
		}
	}
	return delivered
}

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

func doRefreshToken(account *storage.Account, forceOpt ...bool) (string, error) {
	force := len(forceOpt) > 0 && forceOpt[0]
	if TokenRefresherWithForce != nil {
		return TokenRefresherWithForce(account, force)
	}
	if TokenRefresher != nil {
		return TokenRefresher(account)
	}
	return auth.RefreshToken(account, forceOpt...)
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

// maxRequestBodyBytes limits incoming request bodies to 64 MiB to prevent memory exhaustion.
const maxRequestBodyBytes = 64 << 20

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
		if clVal > maxRequestBodyBytes {
			return nil, errors.New("request body exceeds maximum allowed size")
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

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxRequestBodyBytes {
		return nil, errors.New("request body exceeds maximum allowed size")
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

// attachDebugHeaders appends observable pool routing metadata headers to downstream responses.
func attachDebugHeaders(h http.Header, acc *storage.Account, strategy string, affinityStatus string) {
	if h == nil || acc == nil {
		return
	}
	disp := accounts.DisplayAccountName(acc)
	h.Set("X-Agy-Pool-Account", disp)
	h.Set("X-Agy-Pool-Account-Id", acc.ID)
	if strategy != "" {
		h.Set("X-Agy-Pool-Strategy", strategy)
	}
	if affinityStatus != "" {
		h.Set("X-Agy-Pool-Affinity", affinityStatus)
	}
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

// selectionMu serializes generation candidate ordering with the first account's in-flight reservation.
var selectionMu sync.Mutex

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

	if isGeneration {
		observability.RecordGenerationRequest()
		for _, a := range pool.Accounts {
			if scheduler.IsRestricted(a) {
				observability.RecordRestrictedSkip()
			}
		}
	}

	now := float64(NowFunc().Unix())
	strategy := pool.Strategy
	if strategy == "" {
		strategy = config.DefaultStrategy
	}

	var candidates []*storage.Account
	var pendingClaimedID string
	var sessionKey string
	affinityStatus := "none"
	var reservedID string
	var reservedDone func()
	var model string
	var modelExcluded int
	if isGeneration {
		model = requestModel(body)
		sessionKey = ExtractSessionKey(r, body)
		if sessionKey != "" && model != "" {
			// Upstream prompt caches are per model, so each model in a conversation keeps its own binding.
			sessionKey += "\x00" + model
		}
		// Ordering reads in-flight counts, so ordering and reserving the first pick must be atomic,
		// otherwise a burst of requests all see In-Flight=0 and pick the same account.
		// selectionMu covers this process; the DB write transaction covers other processes sharing the pool.
		selectionMu.Lock()
		picked := false
		pick := func(remote map[string]int64) string {
			picked = true
			observability.SetRemoteInFlight(remote)
			candidates = scheduler.OrderCandidates(pool.Accounts, strategy, pool, now)

			if sessionKey != "" {
				var matched bool
				candidates, matched = GetAffinityManager().ApplyAffinity(candidates, sessionKey, NowFunc())
				if matched {
					affinityStatus = "hit"
				} else {
					affinityStatus = "new"
					// For new sessions under smart strategy, rotate candidates within top health tier
					// using the round-robin cursor to achieve balanced multi-account session distribution.
					if strategy == config.StrategySmart {
						candidates = scheduler.RotateTopTier(candidates, scheduler.GetSmartRRCursor(), now)
						scheduler.AdvanceSmartRRCursor()
					}
				}
			}
			// After affinity, so a conversation bound to an account with no capacity for its model moves on.
			candidates, modelExcluded = ExcludeModelCooldowns(candidates, model, NowFunc())
			if affinityStatus == "new" && GetAffinityManager().HeldByLoad(candidates, sessionKey, NowFunc()) {
				affinityStatus = "spill"
			}

			if confCand := SelectAndClaimConfirmationCandidate(pool.Accounts); confCand != nil {
				pendingClaimedID = confCand.ID
				candidates = append([]*storage.Account{confCand}, candidates...)
			}

			// Reserve the account the dispatch loop below will try first (same skip rule).
			for _, acc := range candidates {
				if scheduler.IsRestricted(acc) && acc.ID != pendingClaimedID {
					continue
				}
				if acc != nil && acc.ID != "" {
					reservedID = acc.ID
					reservedDone = observability.StartInFlightGeneration(acc.ID)
				}
				break
			}
			return reservedID
		}
		_, leaseRelease, leaseErr := storage.ReserveInFlight(pick)
		if !picked {
			// State DB unavailable or busy: fall back to the in-process reservation alone.
			pick(nil)
		}
		if leaseErr != nil && reservedID != "" {
			// Other processes sharing the pool will not see this request's in-flight load.
			observability.RecordLeaseFallback()
			logMessage("PROXY WARN", fmt.Sprintf("in-flight lease not recorded for %s, balancing on local counts: %v", reservedID, leaseErr))
		}
		selectionMu.Unlock()

		if pendingClaimedID != "" {
			defer func() {
				if pendingClaimedID != "" {
					ReleaseVerificationConfirmation(pendingClaimedID)
					pendingClaimedID = ""
				}
			}()
		}
		if reservedDone != nil {
			localDone := reservedDone
			reservedDone = func() { localDone(); leaseRelease() }
			defer reservedDone()
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
				if scheduler.IsRestricted(a) {
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

	var selectionAuditCtx *SelectionAuditContext
	if isGeneration && HasActiveAuditor() {
		ctx := &SelectionAuditContext{
			Strategy: strategy,
			Now:      now,
		}
		for _, cand := range pool.Accounts {
			if cand == nil {
				continue
			}
			if scheduler.IsRestricted(cand) {
				continue
			}
			if cand.RateLimitedUntil != nil && *cand.RateLimitedUntil > now {
				continue
			}
			candCap := quota.ComputeCapacityState(cand, now)
			if candCap.IsDepleted {
				continue
			}
			disp := accounts.DisplayAccountName(cand)
			if scheduler.IsReserveCapacity(candCap) {
				ctx.ReserveCandidates = append(ctx.ReserveCandidates, disp)
			} else if candCap.KnownWindowCount > 0 || candCap.LegacyFractionKnown {
				ctx.HealthyCandidates = append(ctx.HealthyCandidates, disp)
			}
		}
		selectionAuditCtx = ctx
	}

	committer := NewResponseCommitter(w)

	var lastError error
	attempt := 0
	hasFailedOver := false
	for _, acc := range candidates {
		if isGeneration && scheduler.IsRestricted(acc) && acc.ID != pendingClaimedID {
			continue
		}
		attempt++
		currentAffinity := affinityStatus
		if attempt > 1 {
			currentAffinity = "failover"
		}
		if isGeneration {
			observability.RecordRoutingDecision()
			if attempt > 1 {
				observability.RecordFailoverAttempt()
				hasFailedOver = true
			}
		}
		var reservation func()
		if acc.ID == reservedID {
			reservation, reservedID = reservedDone, ""
		}
		outcome := h.dispatchAccount(committer, r, path, body, model, isGeneration, isSSE, acc, clientUA, attempt, strategy, currentAffinity, selectionAuditCtx, reservation)
		if acc.ID == pendingClaimedID {
			ReleaseVerificationConfirmation(pendingClaimedID)
			pendingClaimedID = ""
		}
		if isGeneration && (outcome.Reason == "TerminalSuccess" || outcome.Reason == "CommittedStreamSuccess") {
			observability.RecordGenerationSuccess()
			if hasFailedOver {
				observability.RecordFailoverSuccess()
			}
			// A load spill leaves the session bound to its healthy account, whose prompt cache is still warm.
			if sessionKey != "" && acc != nil && affinityStatus != "spill" {
				GetAffinityManager().Set(sessionKey, acc.ID, NowFunc())
			}
			if outcome.PromptTokens > 0 {
				cached := "not reported"
				if outcome.CachedTokens >= 0 {
					cached = fmt.Sprintf("%d (%d%%)", outcome.CachedTokens, outcome.CachedTokens*100/outcome.PromptTokens)
				}
				logMessage("CACHE", fmt.Sprintf("%s session %s -> %s: prompt %d, cached %s", model, sessionTag(sessionKey),
					accounts.DisplayAccountName(acc), outcome.PromptTokens, cached))
			}
		}
		if outcome.Err != nil {
			lastError = outcome.Err
		}

		// Structural invariant: once committed, never advance to another account.
		if committer.IsCommitted() {
			return
		}

		// Only ActionFailoverNext may advance to another account.
		if outcome.Action == ActionFailoverNext {
			// Only the bound account failing ends the binding; a spill account failing leaves it in place.
			if bound, ok := GetAffinityManager().Get(sessionKey, NowFunc()); ok && bound == acc.ID {
				GetAffinityManager().Delete(sessionKey)
			}
			continue
		}

		// Any other outcome is terminal.
		return
	}

	// All accounts exhausted
	if !committer.IsCommitted() {
		if lastError == nil && modelExcluded > 0 {
			lastError = fmt.Errorf("%d account(s) paused for %s after 503/capacity errors", modelExcluded, model)
		}
		committer.Header().Set("X-Agy-Pool-Strategy", strategy)
		logMessage("ALL EXHAUSTED", fmt.Sprintf("%s %s failed across all accounts: %v", r.Method, filepathBase(path), lastError))
		http.Error(committer, fmt.Sprintf("All accounts in pool exhausted or unavailable: %v", lastError), http.StatusServiceUnavailable)
	}
}

func (h *Handler) dispatchAccount(
	committer *ResponseCommitter,
	r *http.Request,
	path string,
	body []byte,
	model string,
	isGeneration bool,
	isSSE bool,
	acc *storage.Account,
	clientUA string,
	attempt int,
	strategy string,
	affinityStatus string,
	auditCtx *SelectionAuditContext,
	reservation func(), // in-flight slot already taken at selection time; nil means take one now
) DispatchOutcome {
	if reservation != nil {
		defer reservation()
	} else if isGeneration && acc != nil && acc.ID != "" {
		done := observability.StartInFlightGeneration(acc.ID)
		defer done()
		if release, err := storage.LeaseInFlight(acc.ID); err == nil {
			defer release()
		} else {
			observability.RecordLeaseFallback()
			logMessage("PROXY WARN", fmt.Sprintf("in-flight lease not recorded on failover for %s, balancing on local counts: %v", acc.ID, err))
		}
	}

	dispName := accounts.DisplayAccountName(acc)

	var frozenEvent *RoutingEvent
	if isGeneration && auditCtx != nil && HasActiveAuditor() {
		candCap := quota.ComputeCapacityState(acc, auditCtx.Now)
		freshness := quota.FreshnessRank(acc, auditCtx.Now)

		var worstPacePtr, totalPacePtr, rawFloorPtr *float64
		if !math.IsInf(candCap.WorstPace, 0) {
			wp := candCap.WorstPace
			worstPacePtr = &wp
		}
		if !math.IsInf(candCap.TotalPace, 0) {
			tp := candCap.TotalPace
			totalPacePtr = &tp
		}
		rf := candCap.RawFloor
		rawFloorPtr = &rf

		var r5Ptr, r7Ptr *float64
		if candCap.Q5 != nil {
			r5Val := candCap.R5
			r5Ptr = &r5Val
		}
		if candCap.Q7 != nil {
			r7Val := candCap.R7
			r7Ptr = &r7Val
		}

		var classification string
		if candCap.IsDepleted {
			classification = "Depleted"
		} else if scheduler.IsReserveCapacity(candCap) {
			classification = "Reserve"
		} else if candCap.KnownWindowCount > 0 || candCap.LegacyFractionKnown {
			classification = "Healthy"
		} else {
			classification = "Unknown"
		}

		frozenEvent = &RoutingEvent{
			Strategy:              auditCtx.Strategy,
			Account:               dispName,
			AccountName:           dispName,
			AccountID:             acc.ID,
			Classification:        classification,
			HealthyCandidateCount: len(auditCtx.HealthyCandidates),
			HealthyCandidates:     auditCtx.HealthyCandidates,
			ReserveCandidateCount: len(auditCtx.ReserveCandidates),
			ReserveCandidates:     auditCtx.ReserveCandidates,

			FiveHourKnown:      candCap.Q5Known,
			FiveHourRemaining:  candCap.Q5,
			FiveHourResetAt:    candCap.Reset5Time,
			FiveHourResetRatio: r5Ptr,
			FiveHourPace:       candCap.Pace5,

			WeeklyKnown:      candCap.Q7Known,
			WeeklyRemaining:  candCap.Q7,
			WeeklyResetAt:    candCap.Reset7Time,
			WeeklyResetRatio: r7Ptr,
			WeeklyPace:       candCap.Pace7,

			KnownWindowCount: candCap.KnownWindowCount,
			FreshnessRank:    &freshness,
			WorstPace:        worstPacePtr,
			TotalPace:        totalPacePtr,
			RawFloor:         rawFloorPtr,
			Path:             path,
		}
	}

	notifyAudit := func(status int, reason string) {
		if frozenEvent == nil || !HasActiveAuditor() {
			return
		}
		ev := *frozenEvent
		ev.Timestamp = NowFunc()
		ev.Attempt = attempt
		ev.Status = status
		ev.FailoverReason = reason
		EmitRoutingEvent(ev)
	}

	forceToken := isGeneration && acc != nil && acc.Status == storage.StatusVerificationPending
	at, err := doRefreshToken(acc, forceToken)
	if err != nil {
		logMessage("PROXY WARN", fmt.Sprintf("Token refresh failed for %s: %v. Switching to next account...", accounts.DisplayAccountName(acc), err))
		notifyAudit(401, "TokenRefreshFailed")
		if persistErr := RecordAuthError(acc); persistErr != nil {
			logMessage("PROXY WARN", "failed to persist auth restriction; continuing failover")
		}
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
	start := NowFunc()
	resp, err := client.Do(upstreamReq)
	ttfb := NowFunc().Sub(start) // time until upstream response headers
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
		observability.RecordNoReplayPrevented()

		// Ambiguous transport failure: DO NOT REPLAY!
		attachDebugHeaders(committer.Header(), acc, strategy, affinityStatus)
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

		auditSuffix := ""
		if frozenEvent != nil {
			auditSuffix = fmt.Sprintf(" [audit: strategy=%s tier=%s healthy=%d reserve=%d]",
				frozenEvent.Strategy, frozenEvent.Classification, frozenEvent.HealthyCandidateCount, frozenEvent.ReserveCandidateCount)
		}
		reason := upstreamErrorReason(errBody)
		reasonSuffix := ""
		if reason != "" {
			reasonSuffix = " " + reason
		}
		logMessage("PROXY ERROR", fmt.Sprintf("%s %s -> %s HTTP %d%s (ttfb %s)%s", r.Method, pathBase, dispName, statusCode, reasonSuffix, fmtDur(ttfb), auditSuffix))

		// Failover on 503 or model capacity exhaustion (seen as 503, possibly 429): the account still serves
		// other models, so pause only this model instead of the whole account. Checked before the 429 quota rule.
		if (reason == "MODEL_CAPACITY_EXHAUSTED" || statusCode == http.StatusServiceUnavailable) && model != "" && acc != nil {
			cooldown := RecordModelCapacityError(acc.ID, model)
			logMessage("FAILOVER", fmt.Sprintf("Account %s has no capacity for %s (HTTP %d)! Paused for this model for %s. Auto-switching to next account...", dispName, model, statusCode, cooldown))
			notifyAudit(statusCode, "ModelCapacityExhausted")
			return DispatchOutcome{
				Action: ActionFailoverNext,
				Reason: "ExplicitRetryableUpstreamResponse:ModelCapacityExhausted",
			}
		}

		// Failover on Google Security Validation Required
		if auth.IsValidationError(statusCode, errBody) {
			persistErr := RecordValidationError(acc, errBody)
			if persistErr != nil {
				logMessage("PROXY WARN", "failed to persist validation restriction; continuing failover")
			}
			logMessage("FAILOVER", fmt.Sprintf("Account %s requires account verification! Auto-switching to next account...", dispName))
			notifyAudit(statusCode, "ValidationError")
			outcome := DispatchOutcome{
				Action: ActionFailoverNext,
				Reason: "ExplicitRetryableUpstreamResponse:ValidationError",
			}
			if persistErr != nil {
				outcome.Err = persistErr
			}
			return outcome
		}

		// Failover on 429 or 403 quota exhaustion
		if quota.IsQuotaError(statusCode, errBody) {
			delay, persistErr := RecordQuotaError(acc, resp.Header, errBody)
			if persistErr != nil {
				logMessage("PROXY WARN", "failed to persist quota cooldown; continuing failover")
			}
			logMessage("FAILOVER", fmt.Sprintf("Account %s hit rate limit/quota error! Cooldown %ds. Auto-switching to next account...", dispName, delay))
			notifyAudit(statusCode, "QuotaExhausted")
			outcome := DispatchOutcome{
				Action: ActionFailoverNext,
				Reason: "ExplicitRetryableUpstreamResponse:QuotaExhausted",
			}
			if persistErr != nil {
				outcome.Err = persistErr
			}
			return outcome
		}

		// Failover on 401 or recognized auth failure
		if auth.IsAuthError(statusCode, errBody) {
			persistErr := RecordAuthError(acc)
			if persistErr != nil {
				logMessage("PROXY WARN", "failed to persist auth restriction; continuing failover")
			}
			logMessage("FAILOVER", fmt.Sprintf("Account %s auth/permission failure! Auto-switching to next account...", dispName))
			notifyAudit(statusCode, "AuthFailure")
			outcome := DispatchOutcome{
				Action: ActionFailoverNext,
				Reason: "ExplicitRetryableUpstreamResponse:AuthFailure",
			}
			if persistErr != nil {
				outcome.Err = persistErr
			}
			return outcome
		}

		// Failover on Location / GeoIP Not Supported (temporary datacenter IP / Anycast routing glitch)
		if IsLocationError(statusCode, errBody) {
			persistErr := RecordSoftCooldown(acc)
			if persistErr != nil {
				logMessage("PROXY WARN", "failed to persist location cooldown; continuing failover")
			}
			logMessage("FAILOVER", fmt.Sprintf("Account %s hit temporary location restriction (HTTP %d)! Auto-switching to next account (soft cooldown 30s)...", dispName, statusCode))
			notifyAudit(statusCode, "LocationError")
			outcome := DispatchOutcome{
				Action: ActionFailoverNext,
				Reason: "ExplicitRetryableUpstreamResponse:LocationError",
			}
			if persistErr != nil {
				outcome.Err = persistErr
			}
			return outcome
		}

		// Failover on 503 for requests without a model (model-bearing ones are paused per model above):
		// upstream did not process the request, so replaying on another account is safe
		if statusCode == http.StatusServiceUnavailable {
			persistErr := RecordSoftCooldown(acc)
			if persistErr != nil {
				logMessage("PROXY WARN", "failed to persist unavailable cooldown; continuing failover")
			}
			logMessage("FAILOVER", fmt.Sprintf("Account %s upstream unavailable (HTTP 503)! Auto-switching to next account (soft cooldown 30s)...", dispName))
			notifyAudit(statusCode, "UpstreamUnavailable")
			outcome := DispatchOutcome{
				Action: ActionFailoverNext,
				Reason: "ExplicitRetryableUpstreamResponse:UpstreamUnavailable",
			}
			if persistErr != nil {
				outcome.Err = persistErr
			}
			return outcome
		}

		// Ordinary upstream error (400, 404, 500, etc.): Forward directly, DO NOT REPLAY!
		notifyAudit(statusCode, "NonRetryableError")
		attachDebugHeaders(resp.Header, acc, strategy, affinityStatus)
		SendBuffered(committer, statusCode, resp.Header, errBody)
		return DispatchOutcome{
			Action: ActionTerminal,
			Reason: "ExplicitNonRetryableUpstreamResponse",
		}
	}

	// 2xx response
	notifyAudit(statusCode, "")
	if isGeneration {
		recordModelLatency(acc, model, ttfb)
	}

	auditSuffix := ""
	if frozenEvent != nil {
		auditSuffix = fmt.Sprintf(" [audit: strategy=%s tier=%s healthy=%d reserve=%d]",
			frozenEvent.Strategy, frozenEvent.Classification, frozenEvent.HealthyCandidateCount, frozenEvent.ReserveCandidateCount)
	}
	respContentType := strings.ToLower(resp.Header.Get("Content-Type"))
	isStream := isSSE || strings.Contains(respContentType, "text/event-stream")

	if isStream {
		if persistErr := RecordSuccess(acc, isGeneration); persistErr != nil {
			logMessage("PROXY WARN", "failed to persist success bookkeeping")
		}
		attachDebugHeaders(resp.Header, acc, strategy, affinityStatus)
		usage := &usageTap{r: resp.Body, cached: -1}
		streamErr := SendStream(committer, statusCode, resp.Header, usage)
		// Logged after the stream ends so the line carries the full duration.
		logMessage("PROXY", fmt.Sprintf("%s %s -> %s (Status: %d, ttfb %s, total %s)%s", r.Method, pathBase, dispName, statusCode, fmtDur(ttfb), fmtDur(NowFunc().Sub(start)), auditSuffix))
		if streamErr != nil {
			observability.RecordNoReplayPrevented()
			return DispatchOutcome{
				Action: ActionTerminal,
				Reason: "CommittedStreamTruncated",
				Err:    streamErr,
			}
		}
		return DispatchOutcome{
			Action:       ActionTerminal,
			Reason:       "CommittedStreamSuccess",
			PromptTokens: usage.prompt,
			CachedTokens: usage.cached,
		}
	}

	logMessage("PROXY", fmt.Sprintf("%s %s -> %s (Status: %d, ttfb %s)%s", r.Method, pathBase, dispName, statusCode, fmtDur(ttfb), auditSuffix))

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		logMessage("PROXY EXCEPTION", fmt.Sprintf("%s %s -> %s: read error: %v", r.Method, pathBase, dispName, readErr))
		observability.RecordNoReplayPrevented()
		attachDebugHeaders(committer.Header(), acc, strategy, affinityStatus)
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

	if persistErr := RecordSuccess(acc, isGeneration); persistErr != nil {
		logMessage("PROXY WARN", "failed to persist success bookkeeping")
	}
	attachDebugHeaders(resp.Header, acc, strategy, affinityStatus)
	SendBuffered(committer, statusCode, resp.Header, respBody)
	usage := usageTap{cached: -1}
	usage.scan(respBody)
	return DispatchOutcome{
		Action:       ActionTerminal,
		Reason:       "TerminalSuccess",
		PromptTokens: usage.prompt,
		CachedTokens: usage.cached,
	}
}

func fmtDur(d time.Duration) string {
	return d.Round(10 * time.Millisecond).String()
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
