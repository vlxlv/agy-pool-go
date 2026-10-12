package proxy

import (
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
