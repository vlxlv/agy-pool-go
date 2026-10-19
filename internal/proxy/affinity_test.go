package proxy

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/observability"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func TestExtractSessionKey(t *testing.T) {
	// 1. Header takes precedence
	req, _ := http.NewRequest("POST", "/v1beta/models/gemini:generateContent", nil)
	req.Header.Set("X-Conversation-Id", "conv-header-123")
	if key := ExtractSessionKey(req, nil); key != "conv-header-123" {
		t.Fatalf("expected conv-header-123 from header, got %q", key)
	}

	// 2. Query parameter fallback
	req2, _ := http.NewRequest("POST", "/v1beta/models/gemini:generateContent?conversation_id=conv-query-456", nil)
	if key := ExtractSessionKey(req2, nil); key != "conv-query-456" {
		t.Fatalf("expected conv-query-456 from query, got %q", key)
	}

	// 3. Body JSON conversationId field
	req3, _ := http.NewRequest("POST", "/v1beta/models/gemini:generateContent", nil)
	body3 := []byte(`{"conversationId": "conv-body-789", "contents": []}`)
	if key := ExtractSessionKey(req3, body3); key != "conv-body-789" {
		t.Fatalf("expected conv-body-789 from json field, got %q", key)
	}

	// 4. Body JSON contents first user text hash
	req4, _ := http.NewRequest("POST", "/v1beta/models/gemini:generateContent", nil)
	body4 := []byte(`{"contents": [{"role": "user", "parts": [{"text": "Hello, write a poem"}]}]}`)
	key4 := ExtractSessionKey(req4, body4)
	if key4 == "" || len(key4) < 10 {
		t.Fatalf("expected hashed turn1 session key, got %q", key4)
	}

	// 5. Subsequent turn in same conversation shares the same turn1 key
	body4Turn2 := []byte(`{"contents": [
		{"role": "user", "parts": [{"text": "Hello, write a poem"}]},
		{"role": "model", "parts": [{"text": "Roses are red..."}]},
		{"role": "user", "parts": [{"text": "Make it longer"}]}
	]}`)
	key4Turn2 := ExtractSessionKey(req4, body4Turn2)
	if key4Turn2 != key4 {
		t.Fatalf("turn 2 did not produce identical session key: turn1=%q turn2=%q", key4, key4Turn2)
	}
}

func TestAffinityManagerLifecycle(t *testing.T) {
	mgr := NewAffinityManager(100 * time.Millisecond)
	now := time.Now()

	// Initially empty
	if _, ok := mgr.Get("sess-1", now); ok {
		t.Fatal("expected no affinity for sess-1")
	}

	// Set affinity
	mgr.Set("sess-1", "acc-a", now)
	got, ok := mgr.Get("sess-1", now)
	if !ok || got != "acc-a" {
		t.Fatalf("expected acc-a, got %q (ok=%v)", got, ok)
	}

	// ApplyAffinity reorders candidates
	accA := &storage.Account{ID: "acc-a"}
	accB := &storage.Account{ID: "acc-b"}
	accC := &storage.Account{ID: "acc-c"}
	candidates := []*storage.Account{accB, accC, accA}

	reordered, applied := mgr.ApplyAffinity(candidates, "sess-1", now)
	if !applied || len(reordered) != 3 || reordered[0].ID != "acc-a" {
		t.Fatalf("expected acc-a moved to front, got %v (applied=%v)", reordered, applied)
	}

	// If acc-a is in cooldown, affinity is skipped
	futureCool := float64(now.Add(1 * time.Hour).Unix())
	accA.RateLimitedUntil = &futureCool
	notReordered, applied2 := mgr.ApplyAffinity(candidates, "sess-1", now)
	if applied2 || notReordered[0].ID == "acc-a" {
		t.Fatalf("cooldown account must not get affinity priority")
	}
	accA.RateLimitedUntil = nil

	// Expiry test
	afterTTL := now.Add(200 * time.Millisecond)
	if _, ok := mgr.Get("sess-1", afterTTL); ok {
		t.Fatal("expected affinity to expire after TTL")
	}

	// Delete test
	mgr.Set("sess-2", "acc-b", now)
	mgr.Delete("sess-2")
	if _, ok := mgr.Get("sess-2", now); ok {
		t.Fatal("expected sess-2 to be deleted")
	}
}

func TestAttachDebugHeaders(t *testing.T) {
	h := make(http.Header)
	acc := &storage.Account{
		ID:   "acc-test-1",
		Name: "TestWorker",
	}

	attachDebugHeaders(h, acc, "smart", "hit")

	if got := h.Get("X-Agy-Pool-Account"); got != "TestWorker" {
		t.Fatalf("expected TestWorker, got %q", got)
	}
	if got := h.Get("X-Agy-Pool-Account-Id"); got != "acc-test-1" {
		t.Fatalf("expected acc-test-1, got %q", got)
	}
	if got := h.Get("X-Agy-Pool-Strategy"); got != "smart" {
		t.Fatalf("expected smart, got %q", got)
	}
	if got := h.Get("X-Agy-Pool-Affinity"); got != "hit" {
		t.Fatalf("expected hit, got %q", got)
	}
}

func TestApplyAffinity_BypassedOnHighInFlight(t *testing.T) {
	observability.ResetInFlight()
	defer observability.ResetInFlight()

	mgr := NewAffinityManager(5 * time.Minute)
	now := time.Now()

	accA := &storage.Account{ID: "acc-affinity-busy"}
	accB := &storage.Account{ID: "acc-affinity-idle"}
	candidates := []*storage.Account{accB, accA}

	mgr.Set("session-concurrent", "acc-affinity-busy", now)

	// 1. With in-flight = 0, affinity is applied and accA is moved to the front
	reordered, applied := mgr.ApplyAffinity(candidates, "session-concurrent", now)
	if !applied || reordered[0].ID != "acc-affinity-busy" {
		t.Fatalf("expected acc-affinity-busy to be moved to front when idle")
	}

	// 2. With in-flight = 1 (< MaxAffinityInFlight = 2), affinity is still applied
	done1 := observability.StartInFlightGeneration("acc-affinity-busy")
	reordered1, applied1 := mgr.ApplyAffinity(candidates, "session-concurrent", now)
	if !applied1 || reordered1[0].ID != "acc-affinity-busy" {
		t.Fatalf("expected acc-affinity-busy to still get affinity when in-flight=1")
	}

	// 3. With in-flight = 2 (>= MaxAffinityInFlight), affinity is bypassed to protect against 429 hot-spots
	done2 := observability.StartInFlightGeneration("acc-affinity-busy")
	reordered2, applied2 := mgr.ApplyAffinity(candidates, "session-concurrent", now)
	if applied2 || reordered2[0].ID != "acc-affinity-idle" {
		t.Fatalf("expected affinity to be bypassed when in-flight >= MaxAffinityInFlight, got applied=%v first=%s", applied2, reordered2[0].ID)
	}

	// 4. Once requests finish and in-flight drops back below threshold, affinity resumes
	done2()
	done1()
	reorderedResumed, appliedResumed := mgr.ApplyAffinity(candidates, "session-concurrent", now)
	if !appliedResumed || reorderedResumed[0].ID != "acc-affinity-busy" {
		t.Fatalf("expected affinity to resume after in-flight drops")
	}
}

func TestApplyAffinity_BypassedOnDepleted(t *testing.T) {
	mgr := NewAffinityManager(5 * time.Minute)
	now := time.Now()

	zeroFrac := 0.0
	futureReset := float64(now.Add(1 * time.Hour).Unix())

	accA := &storage.Account{ID: "acc-healthy"}
	accB := &storage.Account{
		ID: "acc-depleted",
		LastQuota: &storage.QuotaState{
			Gemini5H: &storage.QuotaWindow{
				Fraction:  &zeroFrac,
				ResetTime: futureReset,
			},
		},
	}
	candidates := []*storage.Account{accA, accB}

	mgr.Set("session-depleted", "acc-depleted", now)

	reordered, matched := mgr.ApplyAffinity(candidates, "session-depleted", now)
	if matched {
		t.Fatalf("expected matched == false for depleted account, got true")
	}
	if len(reordered) != 2 || reordered[0].ID != "acc-healthy" || reordered[1].ID != "acc-depleted" {
		t.Fatalf("expected candidates unchanged, got %v", reordered)
	}
}

func TestApplyAffinity_BypassedOverWeeklyPace(t *testing.T) {
	mgr := NewAffinityManager(5 * time.Minute)
	now := time.Now()
	reset5 := float64(now.Add(4 * time.Hour).Unix())
	reset7 := float64(now.Add(6 * 24 * time.Hour).Unix())
	bound := func(q7 float64) *storage.Account {
		q5 := 0.9
		return &storage.Account{
			ID: "acc-bound",
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: &q5, ResetTime: reset5},
				GeminiWeekly: &storage.QuotaWindow{Fraction: &q7, ResetTime: reset7},
			},
		}
	}
	other := &storage.Account{ID: "acc-other"}
	mgr.Set("session-pace", "acc-bound", now)

	// 40% weekly left with 6 of 7 days to go: far ahead of pace, so the session must not pin it.
	if reordered, matched := mgr.ApplyAffinity([]*storage.Account{other, bound(0.40)}, "session-pace", now); matched || reordered[0].ID != "acc-other" {
		t.Fatalf("over-pace account: matched=%v first=%s, want affinity bypassed", matched, reordered[0].ID)
	}
	// Same account on pace keeps its session.
	if reordered, matched := mgr.ApplyAffinity([]*storage.Account{other, bound(0.90)}, "session-pace", now); !matched || reordered[0].ID != "acc-bound" {
		t.Fatalf("on-pace account: matched=%v first=%s, want affinity kept", matched, reordered[0].ID)
	}
}

func TestAffinityManagerPrunesExpiredAndCapsEntries(t *testing.T) {
	m := NewAffinityManager(time.Minute)
	t0 := time.Unix(1_000_000, 0)
	m.Set("old", "acc-1", t0)
	m.Set("live", "acc-2", t0.Add(50*time.Second))
	m.Prune(t0.Add(70 * time.Second))
	if _, ok := m.entries["old"]; ok {
		t.Fatal("expired binding survived Prune")
	}
	if id, ok := m.Get("live", t0.Add(70*time.Second)); !ok || id != "acc-2" {
		t.Fatalf("active binding lost: %q %v", id, ok)
	}

	// Set sweeps expired bindings once the map reaches affinityPruneAt.
	m.Clear()
	for i := 0; i < affinityPruneAt; i++ {
		m.Set(fmt.Sprintf("s%d", i), "acc", t0)
	}
	m.Set("fresh", "acc", t0.Add(2*time.Minute))
	if len(m.entries) != 1 {
		t.Fatalf("entries after expired sweep = %d, want 1", len(m.entries))
	}

	// Active bindings never exceed affinityMaxEntries; the soonest-expiring one is evicted.
	m.Clear()
	for i := 0; i < affinityMaxEntries+10; i++ {
		m.Set(fmt.Sprintf("s%d", i), "acc", t0.Add(time.Duration(i)*time.Millisecond))
	}
	if len(m.entries) != affinityMaxEntries {
		t.Fatalf("entries = %d, want cap %d", len(m.entries), affinityMaxEntries)
	}
	if _, ok := m.entries["s0"]; ok {
		t.Fatal("oldest binding was not evicted")
	}
	if _, ok := m.entries[fmt.Sprintf("s%d", affinityMaxEntries+9)]; !ok {
		t.Fatal("newest binding missing")
	}
}
