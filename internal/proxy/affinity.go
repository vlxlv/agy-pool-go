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
	"github.com/vlxlv/agy-pool-go/internal/scheduler"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// DefaultAffinityTTL is the duration a conversation stays with the account that answered it,
// matching upstream prompt cache warm windows (5 minutes, as in Magpie).
const DefaultAffinityTTL = 5 * time.Minute

// MaxAffinityInFlight is the maximum number of concurrent in-flight generation requests
// an account can serve before affinity pinning is bypassed to prevent concurrency hot-spots and 429s.
const MaxAffinityInFlight = 2

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
	m.entries[sessionKey] = affinityRecord{
		accountID: accountID,
		expiresAt: now.Add(m.ttl),
	}
	m.mu.Unlock()
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
	nowSecs := float64(now.Unix())
	for i, acc := range candidates {
		if acc != nil && acc.ID == boundID {
			// Only keep affinity if account is healthy, not in reserve, not in cooldown or restricted,
			// and active in-flight concurrency does not exceed MaxAffinityInFlight threshold.
			if !scheduler.IsRestricted(acc) && !scheduler.IsReserveCandidate(acc, nowSecs) &&
				(acc.RateLimitedUntil == nil || *acc.RateLimitedUntil <= nowSecs) &&
				observability.GetInFlightGeneration(acc.ID) < MaxAffinityInFlight {
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

	// 3. Check JSON body explicit conversation fields
	if len(body) > 0 {
		var fields struct {
			ConversationID string `json:"conversationId"`
			ConvID         string `json:"conversation_id"`
			SessionID      string `json:"sessionId"`
			SessID         string `json:"session_id"`
		}
		if json.Unmarshal(body, &fields) == nil {
			if fields.ConversationID != "" {
				return fields.ConversationID
			}
			if fields.ConvID != "" {
				return fields.ConvID
			}
			if fields.SessionID != "" {
				return fields.SessionID
			}
			if fields.SessID != "" {
				return fields.SessID
			}
		}

		// 4. Fallback: digest of the first user turn in contents (Magpie convOf pattern)
		var contentsReq struct {
			Contents []struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
		}
		if json.Unmarshal(body, &contentsReq) == nil && len(contentsReq.Contents) > 0 {
			for _, c := range contentsReq.Contents {
				if c.Role == "user" && len(c.Parts) > 0 && strings.TrimSpace(c.Parts[0].Text) != "" {
					sum := sha256.Sum256([]byte(strings.TrimSpace(c.Parts[0].Text)))
					return "turn1:" + hex.EncodeToString(sum[:8])
				}
			}
		}
	}

	return ""
}
