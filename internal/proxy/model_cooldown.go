package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// Long on purpose: once upstream reports no capacity for a model on an account, stop sending it that model
// instead of re-probing every few minutes.
const (
	modelCooldownMin = time.Hour
	modelCooldownMax = 24 * time.Hour
)

// modelSlowTTFB is how long a generation may wait for response headers before the account is treated as
// short of capacity for the model: healthy accounts start in 1-4s, a starved one stalls for minutes.
// A var so tests can shorten it.
var modelSlowTTFB = 30 * time.Second

// modelCooldowns tracks per-(account, model) capacity cooldowns. Upstream can run out of capacity for one
// model on one account while the account keeps serving other models, so this is narrower than RateLimitedUntil.
// The daemon saves them in the state DB (see LoadModelCooldowns) so a restart does not retry paused accounts.
var (
	modelCooldownMu      sync.Mutex
	modelCooldowns       = map[string]modelCooldown{}
	modelSlowStrikes     = map[string]bool{} // (account, model) whose last answer was slow; in memory only
	lastModel            string              // model of the latest generation request, published so the CLI can mirror routing
	persistModelCooldown bool                // set by LoadModelCooldowns; off in tests and CLI processes
)

const modelPausesKey = "model_pauses"

type persistedModelCooldown struct {
	Until   time.Time     `json:"until"`
	Backoff time.Duration `json:"backoff"`
}

// LoadModelCooldowns restores pauses saved by a previous daemon run and turns on saving for later
// changes. Only the daemon calls it, under its PID lock. A failed load is logged and starts empty.
func LoadModelCooldowns() {
	modelCooldownMu.Lock()
	defer modelCooldownMu.Unlock()
	persistModelCooldown = true
	raw, err := storage.LoadRuntimeValue(modelPausesKey)
	var saved map[string]persistedModelCooldown
	if err == nil && raw != "" {
		err = json.Unmarshal([]byte(raw), &saved)
	}
	if err != nil {
		logMessage("PROXY WARN", fmt.Sprintf("could not restore model pauses, starting with none: %v", err))
		return
	}
	for key, p := range saved {
		modelCooldowns[key] = modelCooldown{until: p.Until, backoff: p.Backoff}
	}
}

// saveModelCooldownsLocked writes all pauses to the state DB. Caller holds modelCooldownMu.
// A failed write is logged; routing keeps using the in-memory state.
func saveModelCooldownsLocked() {
	if !persistModelCooldown {
		return
	}
	value := ""
	if len(modelCooldowns) > 0 {
		out := make(map[string]persistedModelCooldown, len(modelCooldowns))
		for key, c := range modelCooldowns {
			out[key] = persistedModelCooldown{Until: c.until, Backoff: c.backoff}
		}
		b, err := json.Marshal(out)
		if err != nil {
			logMessage("PROXY WARN", fmt.Sprintf("failed to encode model pauses: %v", err))
			return
		}
		value = string(b)
	}
	if err := storage.SaveRuntimeValue(modelPausesKey, value); err != nil {
		logMessage("PROXY WARN", fmt.Sprintf("failed to save model pauses; they will not survive a restart: %v", err))
	}
}

type modelCooldown struct {
	until   time.Time
	backoff time.Duration
}

func modelCooldownKey(accountID, model string) string {
	return accountID + "\x00" + model
}

// RecordModelCapacityError puts the account on cooldown for this model, doubling the previous backoff
// (1h, 2h, 4h ... capped at 24h) until a success clears it. Returns the applied cooldown.
func RecordModelCapacityError(accountID, model string) time.Duration {
	modelCooldownMu.Lock()
	defer modelCooldownMu.Unlock()
	key := modelCooldownKey(accountID, model)
	backoff := min(max(modelCooldowns[key].backoff*2, modelCooldownMin), modelCooldownMax)
	modelCooldowns[key] = modelCooldown{until: NowFunc().Add(backoff), backoff: backoff}
	delete(modelSlowStrikes, key)
	saveModelCooldownsLocked()
	return backoff
}

// recordModelLatency clears the account's cooldown for model after a normal-speed answer, and starts one
// after a stalled answer, since upstream sometimes queues instead of refusing when out of capacity.
func recordModelLatency(acc *storage.Account, model string, ttfb time.Duration) {
	if acc == nil || model == "" {
		return
	}
	if ttfb < modelSlowTTFB {
		ClearModelCooldown(acc.ID, model)
		return
	}
	if firstSlowStrike(acc.ID, model) {
		logMessage("SLOW", fmt.Sprintf("Account %s took %s to start answering %s. Pausing it for this model if the next answer is slow too.", accounts.DisplayAccountName(acc), fmtDur(ttfb), model))
		return
	}
	cooldown := RecordModelCapacityError(acc.ID, model)
	logMessage("SLOW", fmt.Sprintf("Account %s took %s to start answering %s. Paused for this model for %s.", accounts.DisplayAccountName(acc), fmtDur(ttfb), model, cooldown))
}

// firstSlowStrike records a slow answer and reports whether it is the first in a row for (account, model).
// Upstream sends headers only after thinking, so one long reasoning turn looks like a stall; two in a row
// without a fast answer between them is treated as missing capacity.
func firstSlowStrike(accountID, model string) bool {
	modelCooldownMu.Lock()
	defer modelCooldownMu.Unlock()
	key := modelCooldownKey(accountID, model)
	if modelSlowStrikes[key] {
		return false
	}
	modelSlowStrikes[key] = true
	return true
}

// ClearModelCooldown forgets the account's backoff for this model after it served the model successfully.
func ClearModelCooldown(accountID, model string) {
	modelCooldownMu.Lock()
	defer modelCooldownMu.Unlock()
	key := modelCooldownKey(accountID, model)
	delete(modelSlowStrikes, key)
	entry, ok := modelCooldowns[key]
	if !ok {
		return // the common case on every fast answer: nothing to clear, so no DB write
	}
	wasActive := NowFunc().Before(entry.until)
	delete(modelCooldowns, key)
	if wasActive {
		// A probe succeeding while still paused indicates capacity is back, so unpause all accounts for this model.
		modelSuffix := modelCooldownKey("", model)
		for k := range modelCooldowns {
			if strings.HasSuffix(k, modelSuffix) {
				delete(modelCooldowns, k)
				delete(modelSlowStrikes, k)
			}
		}
	}
	saveModelCooldownsLocked()
}

func resetModelCooldowns() {
	modelCooldownMu.Lock()
	defer modelCooldownMu.Unlock()
	clear(modelCooldowns)
	clear(modelSlowStrikes)
	lastModel = ""
	persistModelCooldown = false
}

// ActiveModelPauses returns accountID -> model -> pause end for pauses still running at now.
func ActiveModelPauses(now time.Time) map[string]map[string]time.Time {
	modelCooldownMu.Lock()
	defer modelCooldownMu.Unlock()
	var out map[string]map[string]time.Time
	for key, c := range modelCooldowns {
		if !now.Before(c.until) {
			continue
		}
		id, model, _ := strings.Cut(key, "\x00")
		if out == nil {
			out = map[string]map[string]time.Time{}
		}
		if out[id] == nil {
			out[id] = map[string]time.Time{}
		}
		out[id][model] = c.until
	}
	return out
}

// LastGenerationModel returns the model of the latest generation request, or "" if none yet.
func LastGenerationModel() string {
	modelCooldownMu.Lock()
	defer modelCooldownMu.Unlock()
	return lastModel
}

// ExcludeModelCooldowns drops accounts cooling down for model, keeping relative order, and returns how many
// were dropped. Paused accounts are not tried, except that when all are paused the one whose pause ends soonest
// is tried so a model is never shut off for hours by a pool-wide 503 burst.
func ExcludeModelCooldowns(candidates []*storage.Account, model string, now time.Time) ([]*storage.Account, int) {
	if model == "" {
		return candidates, 0
	}
	modelCooldownMu.Lock()
	defer modelCooldownMu.Unlock()
	lastModel = model // every model-bearing generation passes through here
	if len(modelCooldowns) == 0 {
		return candidates, 0
	}
	ready := make([]*storage.Account, 0, len(candidates))
	for _, acc := range candidates {
		if acc == nil || !now.Before(modelCooldowns[modelCooldownKey(acc.ID, model)].until) {
			ready = append(ready, acc)
		}
	}
	if len(ready) == 0 {
		var best *storage.Account
		var bestUntil time.Time
		for _, acc := range candidates {
			if acc == nil {
				continue
			}
			u := modelCooldowns[modelCooldownKey(acc.ID, model)].until
			if best == nil || u.Before(bestUntil) {
				best = acc
				bestUntil = u
			}
		}
		if best != nil {
			return []*storage.Account{best}, len(candidates) - 1
		}
	}
	return ready, len(candidates) - len(ready)
}

// upstreamErrorReason returns the first ErrorInfo reason of a Google API error body, falling back to its status.
func upstreamErrorReason(body []byte) string {
	var e struct {
		Error struct {
			Status  string `json:"status"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	for _, d := range e.Error.Details {
		if d.Reason != "" {
			return d.Reason
		}
	}
	return e.Error.Status
}
