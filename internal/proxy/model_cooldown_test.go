package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func TestModelCooldown_BackoffAndOrdering(t *testing.T) {
	resetProviders()
	defer resetProviders()
	now := time.Unix(1_800_000_000, 0)
	NowFunc = func() time.Time { return now }

	for _, want := range []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour} {
		if got := RecordModelCapacityError("a", "m"); got != want {
			t.Fatalf("backoff = %s, want %s", got, want)
		}
	}
	for range 10 {
		RecordModelCapacityError("a", "m")
	}
	if got := RecordModelCapacityError("a", "m"); got != modelCooldownMax {
		t.Fatalf("backoff not capped: %s", got)
	}

	a, b, c := &storage.Account{ID: "a"}, &storage.Account{ID: "b"}, &storage.Account{ID: "c"}
	ids := func(accs []*storage.Account) string {
		var s []string
		for _, x := range accs {
			s = append(s, x.ID)
		}
		return strings.Join(s, ",")
	}
	exclude := func(model string, at time.Time) string {
		got, n := ExcludeModelCooldowns([]*storage.Account{a, b, c}, model, at)
		return fmt.Sprintf("%s/%d", ids(got), n)
	}
	if got := exclude("m", now); got != "b,c/1" {
		t.Fatalf("cooling account not excluded: %s", got)
	}
	if got := exclude("other", now); got != "a,b,c/0" {
		t.Fatalf("other model affected: %s", got)
	}
	if got := exclude("m", now.Add(modelCooldownMax)); got != "a,b,c/0" {
		t.Fatalf("expired cooldown still applied: %s", got)
	}

	ClearModelCooldown("a", "m")
	if got := RecordModelCapacityError("a", "m"); got != time.Hour {
		t.Fatalf("backoff not reset by clear: %s", got)
	}
}

func TestProxy_ModelCapacityExhausted_CoolsDownOnlyThatModel(t *testing.T) {
	const capacityBody = `{"error":{"code":503,"message":"No capacity available for model busy-model on the server","status":"UNAVAILABLE","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MODEL_CAPACITY_EXHAUSTED"}]}}`
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"503-capacity", http.StatusServiceUnavailable, capacityBody},
		{"429-capacity", http.StatusTooManyRequests, capacityBody},
		{"503-plain", http.StatusServiceUnavailable, `{"error":{"code":503,"message":"The service is currently unavailable.","status":"UNAVAILABLE"}}`},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { testModelCapacityFailover(t, i, tc.status, tc.body) })
	}
}

func testModelCapacityFailover(t *testing.T, run, status int, errBody string) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()

	var acc1Hits, acc2Hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(r.Header.Get("Authorization"), "token-acc_1") {
			atomic.AddInt32(&acc1Hits, 1)
			if strings.Contains(string(body), `"busy-model"`) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(errBody))
				return
			}
			_, _ = w.Write([]byte(`{"result":"acc1"}`))
			return
		}
		atomic.AddInt32(&acc2Hits, 1)
		_, _ = w.Write([]byte(`{"result":"acc2"}`))
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }
	TokenRefresher = func(account *storage.Account) (string, error) { return "token-" + account.ID, nil }

	// acc_1 has more headroom, so max_quota tries it first.
	hi, lo := 0.95, 0.90
	pool := storage.NewEmptyPool()
	pool.Strategy = config.StrategyMaxQuota
	acc1 := &storage.Account{ID: "acc_1", Email: "1@example.com", LastQuota: &storage.QuotaState{RemainingFraction: &hi}}
	acc2 := &storage.Account{ID: "acc_2", Email: "2@example.com", LastQuota: &storage.QuotaState{RemainingFraction: &lo}}
	pool.Accounts = []*storage.Account{acc1, acc2}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	handler := NewHandler()
	do := func(model, text string) *httptest.ResponseRecorder {
		// Run index in the text keeps session affinity (global) from leaking between subtests.
		body := fmt.Sprintf(`{"model":%q,"request":{"contents":[{"role":"user","parts":[{"text":"%s-%d"}]}]}}`, model, text, run)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1internal:generateContent", strings.NewReader(body)))
		return w
	}
	send := func(model, text string) string {
		w := do(model, text)
		if w.Code != http.StatusOK {
			t.Fatalf("model %s: HTTP %d: %s", model, w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	// 1. Capacity error on acc_1 fails over to acc_2.
	if got := send("busy-model", "first"); !strings.Contains(got, "acc2") {
		t.Fatalf("expected failover to acc2, got %s", got)
	}
	// 2. Same model again: acc_1 is skipped while cooling down.
	if got := send("busy-model", "second"); !strings.Contains(got, "acc2") {
		t.Fatalf("expected acc2 for cooling model, got %s", got)
	}
	if n := atomic.LoadInt32(&acc1Hits); n != 1 {
		t.Fatalf("acc_1 contacted %d times for busy-model, want 1", n)
	}
	// 3. A different model still goes to acc_1.
	if got := send("other-model", "third"); !strings.Contains(got, "acc1") {
		t.Fatalf("expected acc1 for unaffected model, got %s", got)
	}

	// 4. Every account paused for the model: refuse without contacting upstream.
	RecordModelCapacityError("acc_2", "busy-model")
	before1, before2 := atomic.LoadInt32(&acc1Hits), atomic.LoadInt32(&acc2Hits)
	if w := do("busy-model", "fourth"); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "paused for busy-model") {
		t.Fatalf("expected 503 naming the paused model, got %d: %s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&acc1Hits) != before1 || atomic.LoadInt32(&acc2Hits) != before2 {
		t.Fatal("paused accounts were contacted")
	}

	// No account-wide cooldown or quota restriction was recorded.
	loaded, err := storage.LoadPool()
	if err != nil {
		t.Fatalf("LoadPool failed: %v", err)
	}
	if stored := storage.FindAccount(loaded, acc1); stored.RateLimitedUntil != nil || stored.Status != "" {
		t.Fatalf("acc_1 got account-wide restriction: until=%v status=%q", stored.RateLimitedUntil, stored.Status)
	}
}

func TestProxy_SlowStart_CoolsDownOnlyThatModel(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()
	defer func(orig time.Duration) { modelSlowTTFB = orig }(modelSlowTTFB)
	modelSlowTTFB = 50 * time.Millisecond

	var acc1Hits int32
	var slow atomic.Bool
	slow.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(r.Header.Get("Authorization"), "token-acc_1") {
			atomic.AddInt32(&acc1Hits, 1)
			if slow.Load() && strings.Contains(string(body), `"slow-model"`) {
				time.Sleep(150 * time.Millisecond) // stalls, then answers
			}
			_, _ = w.Write([]byte(`{"result":"acc1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"result":"acc2"}`))
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	BackendHostProvider = func() string { return u.Host }
	BackendURLProvider = func() string { return upstream.URL }
	TokenRefresher = func(account *storage.Account) (string, error) { return "token-" + account.ID, nil }

	hi, lo := 0.95, 0.90
	pool := storage.NewEmptyPool()
	pool.Strategy = config.StrategyMaxQuota
	acc1 := &storage.Account{ID: "acc_1", Email: "1@example.com", LastQuota: &storage.QuotaState{RemainingFraction: &hi}}
	acc2 := &storage.Account{ID: "acc_2", Email: "2@example.com", LastQuota: &storage.QuotaState{RemainingFraction: &lo}}
	pool.Accounts = []*storage.Account{acc1, acc2}
	if err := storage.SavePool(pool); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	handler := NewHandler()
	send := func(model, text string) string {
		body := fmt.Sprintf(`{"model":%q,"request":{"contents":[{"role":"user","parts":[{"text":"slow-%s"}]}]}}`, model, text)
		req := httptest.NewRequest("POST", "/v1internal:generateContent", strings.NewReader(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("model %s: HTTP %d: %s", model, w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	// 1. Slow but successful answer is still delivered.
	if got := send("slow-model", "first"); !strings.Contains(got, "acc1") {
		t.Fatalf("expected slow answer from acc1, got %s", got)
	}
	// 2. Same model next time skips acc_1.
	if got := send("slow-model", "second"); !strings.Contains(got, "acc2") {
		t.Fatalf("expected acc2 after slow start, got %s", got)
	}
	// 3. Other models still use acc_1.
	if got := send("other-model", "third"); !strings.Contains(got, "acc1") {
		t.Fatalf("expected acc1 for unaffected model, got %s", got)
	}
	// 4. After the cooldown, a fast answer clears it for good.
	slow.Store(false)
	NowFunc = func() time.Time { return time.Now().Add(modelCooldownMax) }
	if got := send("slow-model", "fourth"); !strings.Contains(got, "acc1") {
		t.Fatalf("expected acc1 once cooldown expired, got %s", got)
	}
	NowFunc = time.Now
	if got := send("slow-model", "fifth"); !strings.Contains(got, "acc1") {
		t.Fatalf("expected cooldown cleared by fast answer, got %s", got)
	}
}

func TestModelCooldown_PublishedState(t *testing.T) {
	resetProviders()
	defer resetProviders()
	now := time.Unix(1_800_000_000, 0)
	NowFunc = func() time.Time { return now }

	if LastGenerationModel() != "" || ActiveModelPauses(now) != nil {
		t.Fatal("expected empty state after reset")
	}
	RecordModelCapacityError("a", "m")
	ExcludeModelCooldowns(nil, "m2", now)

	if got := LastGenerationModel(); got != "m2" {
		t.Fatalf("LastGenerationModel = %q, want m2", got)
	}
	pauses := ActiveModelPauses(now)
	if until := pauses["a"]["m"]; !until.Equal(now.Add(time.Hour)) {
		t.Fatalf("pause for a/m = %v, want %v (all: %v)", until, now.Add(time.Hour), pauses)
	}
	if ActiveModelPauses(now.Add(time.Hour)) != nil {
		t.Fatal("expired pause still published")
	}
}

func TestModelCooldown_SurvivesRestart(t *testing.T) {
	setupIsolatedTestDir(t)
	resetProviders()
	defer resetProviders()
	now := time.Unix(1_800_000_000, 0)
	NowFunc = func() time.Time { return now }
	if err := storage.SavePool(storage.NewEmptyPool()); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}
	restart := func() {
		modelCooldownMu.Lock()
		clear(modelCooldowns)
		modelCooldownMu.Unlock()
		LoadModelCooldowns()
	}

	LoadModelCooldowns() // daemon start: nothing saved yet
	RecordModelCapacityError("a", "m")
	RecordModelCapacityError("a", "m") // backoff now 2h

	// An ordinary pool save must not wipe the saved pauses.
	if err := storage.SavePool(storage.NewEmptyPool()); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}
	restart()
	if until := ActiveModelPauses(now)["a"]["m"]; !until.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("restored pause = %v, want %v", until, now.Add(2*time.Hour))
	}
	if got := RecordModelCapacityError("a", "m"); got != 4*time.Hour {
		t.Fatalf("backoff after restart = %s, want 4h", got)
	}

	ClearModelCooldown("a", "m")
	restart()
	if p := ActiveModelPauses(now); p != nil {
		t.Fatalf("cleared pause came back after restart: %v", p)
	}
}
