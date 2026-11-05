package proxy

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
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

	// 6. Cloud Code PA /v1internal nested request.contents and multi-part first user turn
	req5, _ := http.NewRequest("POST", "/v1internal:streamGenerateContent", nil)
	body5Turn1 := []byte(`{"model": "models/gemini-2.5-pro", "request": {"contents": [
		{"role": "user", "parts": [{"text": "system preamble"}, {"text": "Refactor scheduler.go"}]}
	]}}`)
	body5Turn2 := []byte(`{"model": "models/gemini-2.5-pro", "request": {"contents": [
		{"role": "user", "parts": [{"text": "system preamble"}, {"text": "Refactor scheduler.go"}]},
		{"role": "model", "parts": [{"text": "Done."}]},
		{"role": "user", "parts": [{"text": "Run tests"}]}
	]}}`)
	body5OtherSession := []byte(`{"model": "models/gemini-2.5-pro", "request": {"contents": [
		{"role": "user", "parts": [{"text": "system preamble"}, {"text": "Different task entirely"}]}
	]}}`)
	key5Turn1 := ExtractSessionKey(req5, body5Turn1)
	key5Turn2 := ExtractSessionKey(req5, body5Turn2)
	key5Other := ExtractSessionKey(req5, body5OtherSession)
	if key5Turn1 == "" || key5Turn1 != key5Turn2 {
		t.Fatalf("expected matching /v1internal session keys across turns, got %q vs %q", key5Turn1, key5Turn2)
	}
	if key5Turn1 == key5Other {
		t.Fatalf("expected distinct session keys when second part differs despite shared preamble, got %q", key5Turn1)
	}

	// 7. Nested request.session_id and metadata.conversation_id
	body6 := []byte(`{"request": {"session_id": "nested-sess-999"}}`)
	if got := ExtractSessionKey(req5, body6); got != "nested-sess-999" {
		t.Fatalf("expected nested-sess-999, got %q", got)
	}

	// 8. agy: request.sessionId is shared by all conversations, labels.cascade_id identifies one
	agy := func(cascade string) []byte {
		return []byte(`{"model":"gemini-3.8-flash-high","requestType":"agent","request":{"sessionId":"-3750763034362895579","labels":{"cascade_id":"` + cascade + `"}}}`)
	}
	if a, b := ExtractSessionKey(req5, agy("cascade-a")), ExtractSessionKey(req5, agy("cascade-b")); a != "cascade-a" || b != "cascade-b" {
		t.Fatalf("expected per-cascade keys, got %q and %q", a, b)
	}
	// agy checkpoint calls carry no cascade and keep the sessionId
	if got := ExtractSessionKey(req5, []byte(`{"request":{"sessionId":"-3750763034362895579"}}`)); got != "-3750763034362895579" {
		t.Fatalf("expected sessionId without a cascade, got %q", got)
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
	if !mgr.HeldByLoad(candidates, "session-concurrent", now) {
		t.Fatalf("expected a load-only bypass to keep the binding")
	}

	// 4. Once requests finish and in-flight drops back below threshold, affinity resumes
	done2()
	if !mgr.HeldByLoad(candidates, "session-concurrent", now) {
		t.Fatalf("expected the binding to stay held when a request finishes between ApplyAffinity and HeldByLoad")
	}
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
	if mgr.HeldByLoad(candidates, "session-depleted", now) {
		t.Fatalf("expected a depleted bound account to give up its binding")
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

func TestInspectRequestModelAndSessionKey(t *testing.T) {
	req, _ := http.NewRequest("POST", "/v1internal:streamGenerateContent", nil)
	// agy order: contents first, labels and model last.
	agyBody := []byte(`{"project":"p","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"labels":{"cascade_id":"c-1"},"sessionId":"-1"},"model":"m-1"}`)
	if model, key := inspectRequest(req, agyBody); model != "m-1" || key != "c-1" {
		t.Fatalf("agy body: got (%q, %q), want (m-1, c-1)", model, key)
	}

	hdr, _ := http.NewRequest("POST", "/v1internal:streamGenerateContent", nil)
	hdr.Header.Set("X-Session-Id", "h-1")
	if model, key := inspectRequest(hdr, agyBody); model != "m-1" || key != "h-1" {
		t.Fatalf("header: got (%q, %q), want (m-1, h-1)", model, key)
	}

	// A mistyped session field drops the body session key but keeps the model.
	if model, key := inspectRequest(req, []byte(`{"sessionId":42,"model":"m-2"}`)); model != "m-2" || key != "" {
		t.Fatalf("mistyped field: got (%q, %q), want (m-2, \"\")", model, key)
	}

	if model, key := inspectRequest(req, []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)); model != "" || len(key) != len("turn1:")+16 {
		t.Fatalf("turn1 fallback: got (%q, %q)", model, key)
	}
	if model, key := inspectRequest(req, []byte(`{not json`)); model != "" || key != "" {
		t.Fatalf("malformed: got (%q, %q), want empty", model, key)
	}
}

func TestAffinityManager_PersistenceRoundtrip(t *testing.T) {
	tmpDir := t.TempDir()
	if err := config.ConfigureStateDir(tmpDir); err != nil {
		t.Fatalf("failed to configure state dir: %v", err)
	}
	t.Cleanup(func() {
		SetPersistAffinity(false)
		GetAffinityManager().Clear()
		config.ResetDataDir()
	})

	SetPersistAffinity(true)
	mgr := GetAffinityManager()
	mgr.Clear()

	now := time.Now()
	// 1. Set active and unexpired bindings
	mgr.Set("conv-1\x00gemini-2.5-pro", "acc-pro", now)
	mgr.Set("conv-2\x00gemini-2.5-flash", "acc-flash", now)
	SaveAffinities()

	// Verify persistence in runtime_global
	val, err := storage.LoadRuntimeValue(sessionAffinitiesKey)
	if err != nil {
		t.Fatalf("LoadRuntimeValue failed: %v", err)
	}
	if !strings.Contains(val, "acc-pro") || !strings.Contains(val, "acc-flash") {
		t.Fatalf("expected saved affinities to contain acc-pro and acc-flash, got %s", val)
	}

	// 2. Simulate daemon restart: clear in-memory map and reload
	GetAffinityManager().Clear()

	if _, ok := mgr.Get("conv-1\x00gemini-2.5-pro", now); ok {
		t.Fatal("expected binding to be absent before reload")
	}
	if _, ok := mgr.Get("conv-2\x00gemini-2.5-flash", now); ok {
		t.Fatal("expected binding to be absent before reload")
	}

	LoadAffinities()

	boundID1, ok1 := mgr.Get("conv-1\x00gemini-2.5-pro", now)
	if !ok1 || boundID1 != "acc-pro" {
		t.Fatalf("expected restored binding acc-pro, got (%s, %v)", boundID1, ok1)
	}
	boundID2, ok2 := mgr.Get("conv-2\x00gemini-2.5-flash", now)
	if !ok2 || boundID2 != "acc-flash" {
		t.Fatalf("expected restored binding acc-flash, got (%s, %v)", boundID2, ok2)
	}
}

func TestAffinityPersistenceDoesNotDeadlockWithReserve(t *testing.T) {
	setupIsolatedTestDir(t)
	SetPersistAffinity(true)
	defer SetPersistAffinity(false)

	pool := &storage.Pool{
		Accounts: []*storage.Account{
			{ID: "acc_1", Email: "acc1@example.com"},
			{ID: "acc_2", Email: "acc2@example.com"},
		},
	}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("failed to save pool: %v", err)
	}

	m := GetAffinityManager()
	m.Clear()
	defer m.Clear()

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		wg.Add(2)

		// Goroutine 1: loops 500x m.Set and calls SaveAffinities() every 10 iterations
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				m.Set(fmt.Sprintf("s%d", i%50), "acc_1", time.Now())
				if i%10 == 0 {
					SaveAffinities()
				}
			}
		}()

		// Goroutine 2: loops 500x storage.ReserveInFlight
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				_, release, err := storage.ReserveInFlight(func(map[string]int64) string {
					m.ApplyAffinity(pool.Accounts, "s1", time.Now())
					return "acc_1"
				})
				if err == nil && release != nil {
					release()
				}
			}
		}()

		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("deadlock: TestAffinityPersistenceDoesNotDeadlockWithReserve did not finish within 20s")
	}
}

func TestApplyAffinity_YieldsOnQuotaDepletion(t *testing.T) {
	mgr := NewAffinityManager(5 * time.Minute)
	now := time.Now()
	reset5 := float64(now.Add(4 * time.Hour).Unix())
	reset7 := float64(now.Add(5 * 24 * time.Hour).Unix())

	makeAcc := func(id string, q5, q7 float64) *storage.Account {
		return &storage.Account{
			ID: id,
			LastQuota: &storage.QuotaState{
				Gemini5H:     &storage.QuotaWindow{Fraction: &q5, ResetTime: reset5},
				GeminiWeekly: &storage.QuotaWindow{Fraction: &q7, ResetTime: reset7},
			},
		}
	}

	// Case 1: Bound account 5H quota drops below AffinityMin5HFloor (0.35) while top candidate is healthy (0.90)
	boundLow5H := makeAcc("acc-low-5h", 0.25, 0.80)
	healthyTop := makeAcc("acc-healthy-top", 0.90, 0.90)
	candidates1 := []*storage.Account{healthyTop, boundLow5H}

	mgr.Set("session-low-5h", "acc-low-5h", now)
	reordered1, matched1 := mgr.ApplyAffinity(candidates1, "session-low-5h", now)
	if matched1 {
		t.Fatalf("expected affinity to yield when bound account 5H quota is below 0.35 and top is healthy")
	}
	if reordered1[0].ID != "acc-healthy-top" {
		t.Fatalf("expected top candidate to remain first, got %s", reordered1[0].ID)
	}
	if mgr.HeldByLoad(candidates1, "session-low-5h", now) {
		t.Fatalf("expected HeldByLoad to be false on quota yield so session can migrate")
	}

	// Case 2: Bound account Weekly quota drops below AffinityMinWeeklyFloor (0.25) while top candidate is healthy (0.85)
	boundLowWeekly := makeAcc("acc-low-weekly", 0.90, 0.15)
	candidates2 := []*storage.Account{healthyTop, boundLowWeekly}

	mgr.Set("session-low-weekly", "acc-low-weekly", now)
	reordered2, matched2 := mgr.ApplyAffinity(candidates2, "session-low-weekly", now)
	if matched2 {
		t.Fatalf("expected affinity to yield when bound account Weekly quota is below 0.25 and top is healthy")
	}
	if reordered2[0].ID != "acc-healthy-top" {
		t.Fatalf("expected top candidate to remain first, got %s", reordered2[0].ID)
	}

	// Case 3: Both accounts are low: no healthier alternative exists in pool -> affinity is preserved
	bothLowTop := makeAcc("acc-low-top", 0.30, 0.80)
	bothLowBound := makeAcc("acc-low-bound", 0.25, 0.80)
	candidates3 := []*storage.Account{bothLowTop, bothLowBound}

	mgr.Set("session-both-low", "acc-low-bound", now)
	reordered3, matched3 := mgr.ApplyAffinity(candidates3, "session-both-low", now)
	if !matched3 || reordered3[0].ID != "acc-low-bound" {
		t.Fatalf("expected affinity to be preserved when whole pool is low (no thrashing), got matched=%v first=%s", matched3, reordered3[0].ID)
	}
}
