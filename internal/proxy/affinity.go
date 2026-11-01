package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/observability"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/scheduler"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// DefaultAffinityTTL is the duration a conversation stays with the account that answered it,
// matching upstream prompt cache warm windows (5 minutes, as in Magpie).
const DefaultAffinityTTL = 5 * time.Minute

// MaxAffinityInFlight is the maximum number of concurrent in-flight generation requests
// an account can serve before affinity pinning is bypassed to prevent concurrency hot-spots and 429s.
const MaxAffinityInFlight = 2

// affinityPruneAt and affinityMaxEntries bound the session map: Set sweeps expired bindings once it
// reaches affinityPruneAt, and evicts the soonest-expiring binding if active ones still fill affinityMaxEntries.
const (
	affinityPruneAt    = 512
	affinityMaxEntries = 1024
)

type affinityRecord struct {
	accountID string
	expiresAt time.Time
}

// AffinityManager tracks active session-to-account bindings to maximize prompt cache hits.
type AffinityManager struct {
	mu      sync.RWMutex
	entries map[string]affinityRecord
	ttl     time.Duration
}

var (
	globalAffinityMgr *AffinityManager
	affinityMgrOnce   sync.Once
)

// GetAffinityManager returns the singleton AffinityManager.
func GetAffinityManager() *AffinityManager {
	affinityMgrOnce.Do(func() {
		globalAffinityMgr = NewAffinityManager(DefaultAffinityTTL)
	})
	return globalAffinityMgr
}

// NewAffinityManager creates a new AffinityManager with the given TTL.
func NewAffinityManager(ttl time.Duration) *AffinityManager {
	if ttl <= 0 {
		ttl = DefaultAffinityTTL
	}
	return &AffinityManager{
		entries: make(map[string]affinityRecord),
		ttl:     ttl,
	}
}

// Get returns the bound account ID for the session if active and unexpired.
func (m *AffinityManager) Get(sessionKey string, now time.Time) (string, bool) {
	if m == nil || sessionKey == "" {
		return "", false
	}
	m.mu.RLock()
	rec, ok := m.entries[sessionKey]
	m.mu.RUnlock()
	if !ok || now.After(rec.expiresAt) {
		return "", false
	}
	return rec.accountID, true
}

// Set binds or renews the session to the given account ID.
func (m *AffinityManager) Set(sessionKey, accountID string, now time.Time) {
	if m == nil || sessionKey == "" || accountID == "" {
		return
	}
	m.mu.Lock()
	if _, exists := m.entries[sessionKey]; !exists && len(m.entries) >= affinityPruneAt {
		m.pruneLocked(now)
		if len(m.entries) >= affinityMaxEntries {
			m.evictOldestLocked()
		}
	}
	m.entries[sessionKey] = affinityRecord{
		accountID: accountID,
		expiresAt: now.Add(m.ttl),
	}
	m.mu.Unlock()
}

// Prune deletes expired bindings.
func (m *AffinityManager) Prune(now time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.pruneLocked(now)
	m.mu.Unlock()
}

func (m *AffinityManager) pruneLocked(now time.Time) {
	for k, rec := range m.entries {
		if now.After(rec.expiresAt) {
			delete(m.entries, k)
		}
	}
}

// evictOldestLocked drops the binding that expires first; with one shared TTL that is the least recently set.
func (m *AffinityManager) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, rec := range m.entries {
		if oldestKey == "" || rec.expiresAt.Before(oldest) {
			oldestKey, oldest = k, rec.expiresAt
		}
	}
	delete(m.entries, oldestKey)
}

// Delete removes any binding for the given session.
func (m *AffinityManager) Delete(sessionKey string) {
	if m == nil || sessionKey == "" {
		return
	}
	m.mu.Lock()
	delete(m.entries, sessionKey)
	m.mu.Unlock()
}

// Clear removes all active bindings.
func (m *AffinityManager) Clear() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.entries = make(map[string]affinityRecord)
	m.mu.Unlock()
}

// ApplyAffinity reorders candidates so that a healthy affinity-bound account is placed first.
func (m *AffinityManager) ApplyAffinity(candidates []*storage.Account, sessionKey string, now time.Time) ([]*storage.Account, bool) {
	if m == nil || sessionKey == "" || len(candidates) < 2 {
		return candidates, false
	}
	boundID, ok := m.Get(sessionKey, now)
	if !ok || boundID == "" {
		return candidates, false
	}

	targetIdx := -1
	for i, acc := range candidates {
		if acc != nil && acc.ID == boundID {
			if affinityHealthy(acc, now) && observability.GetInFlightGeneration(acc.ID) < MaxAffinityInFlight {
				targetIdx = i
			}
			break
		}
	}

	if targetIdx <= 0 {
		return candidates, targetIdx == 0
	}

	reordered := make([]*storage.Account, 0, len(candidates))
	reordered = append(reordered, candidates[targetIdx])
	reordered = append(reordered, candidates[:targetIdx]...)
	reordered = append(reordered, candidates[targetIdx+1:]...)
	return reordered, true
}

// HeldByLoad reports whether the session's bound account is among candidates and healthy, so affinity skipped
// it only for in-flight load: the request spills elsewhere, but the binding and its warm cache should stay.
func (m *AffinityManager) HeldByLoad(candidates []*storage.Account, sessionKey string, now time.Time) bool {
	boundID, ok := m.Get(sessionKey, now)
	if !ok {
		return false
	}
	for _, acc := range candidates {
		if acc != nil && acc.ID == boundID {
			return affinityHealthy(acc, now)
		}
	}
	return false
}

// affinityHealthy reports whether a bound account may keep its session regardless of load: not restricted,
// rate limited, in reserve, depleted or over weekly pace.
func affinityHealthy(acc *storage.Account, now time.Time) bool {
	nowSecs := float64(now.Unix())
	return !scheduler.IsRestricted(acc) && !scheduler.IsReserveCandidate(acc, nowSecs) &&
		!quota.ComputeCapacityState(acc, nowSecs).IsDepleted && !scheduler.OverWeeklyPace(acc, nowSecs) &&
		(acc.RateLimitedUntil == nil || *acc.RateLimitedUntil <= nowSecs)
}

// ExtractSessionKey extracts a conversation identifier from request headers, query params, or body.
func ExtractSessionKey(r *http.Request, body []byte) string {
	if r == nil {
		return ""
	}

	// 1. Check HTTP headers
	for _, h := range []string{"X-Conversation-Id", "X-Session-Id", "X-Request-Session-Id", "Conversation-Id", "Session-Id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}

	// 2. Check URL query parameters
	if r.URL != nil {
		for _, q := range []string{"conversation_id", "session_id", "cid"} {
			if v := strings.TrimSpace(r.URL.Query().Get(q)); v != "" {
				return v
			}
		}
	}

	// 3. Check JSON body explicit conversation fields and contents (top-level and /v1internal nested request/metadata)
	if len(body) > 0 {
		type turnPart struct {
			Text string `json:"text"`
		}
		type turnContent struct {
			Role  string     `json:"role"`
			Parts []turnPart `json:"parts"`
		}
		type sessionFields struct {
			ConversationID string            `json:"conversationId"`
			ConvID         string            `json:"conversation_id"`
			SessionID      string            `json:"sessionId"`
			SessID         string            `json:"session_id"`
			Contents       []json.RawMessage `json:"contents"`
			Labels         struct {
				CascadeID string `json:"cascade_id"`
			} `json:"labels"`
		}
		var payload struct {
			sessionFields
			Request  sessionFields `json:"request"`
			Metadata sessionFields `json:"metadata"`
		}
		if json.Unmarshal(body, &payload) == nil {
			// agy (Antigravity) sends one request.sessionId for every conversation; the conversation is its cascade.
			if v := strings.TrimSpace(payload.Request.Labels.CascadeID); v != "" {
				return v
			}
			for _, s := range []sessionFields{payload.sessionFields, payload.Request, payload.Metadata} {
				for _, id := range []string{s.ConversationID, s.ConvID, s.SessionID, s.SessID} {
					if v := strings.TrimSpace(id); v != "" {
						return v
					}
				}
			}

			// 4. Fallback: digest of the first user turn in contents (top-level or /v1internal request.contents)
			contents := payload.Contents
			if len(contents) == 0 {
				contents = payload.Request.Contents
			}
			for _, raw := range contents {
				var c turnContent
				if err := json.Unmarshal(raw, &c); err != nil {
					continue
				}
				if c.Role == "user" || (c.Role == "" && len(contents) == 1) {
					var texts []string
					for _, p := range c.Parts {
						if t := strings.TrimSpace(p.Text); t != "" {
							texts = append(texts, t)
						}
					}
					if len(texts) > 0 {
						sum := sha256.Sum256([]byte(strings.Join(texts, "\n")))
						return "turn1:" + hex.EncodeToString(sum[:8])
					}
				}
			}
		}
	}

	return ""
}
