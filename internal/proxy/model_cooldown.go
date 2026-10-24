package proxy

import (
	"encoding/json"
	"fmt"
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
// In-memory only: capacity outages are transient, and a restart costs at most one more failed attempt.
var (
	modelCooldownMu sync.Mutex
	modelCooldowns  = map[string]modelCooldown{}
)

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
	cooldown := RecordModelCapacityError(acc.ID, model)
	logMessage("SLOW", fmt.Sprintf("Account %s took %s to start answering %s. Paused for this model for %s.", accounts.DisplayAccountName(acc), fmtDur(ttfb), model, cooldown))
}

// ClearModelCooldown forgets the account's backoff for this model after it served the model successfully.
func ClearModelCooldown(accountID, model string) {
	modelCooldownMu.Lock()
	defer modelCooldownMu.Unlock()
	delete(modelCooldowns, modelCooldownKey(accountID, model))
}

func resetModelCooldowns() {
	modelCooldownMu.Lock()
	defer modelCooldownMu.Unlock()
	clear(modelCooldowns)
}

// ExcludeModelCooldowns drops accounts cooling down for model, keeping relative order, and returns how many
// were dropped. Unlike account-wide cooldowns they are not kept as a last resort: they are not tried at all.
func ExcludeModelCooldowns(candidates []*storage.Account, model string, now time.Time) ([]*storage.Account, int) {
	if model == "" {
		return candidates, 0
	}
	modelCooldownMu.Lock()
	defer modelCooldownMu.Unlock()
	if len(modelCooldowns) == 0 {
		return candidates, 0
	}
	ready := make([]*storage.Account, 0, len(candidates))
	for _, acc := range candidates {
		if acc == nil || !now.Before(modelCooldowns[modelCooldownKey(acc.ID, model)].until) {
			ready = append(ready, acc)
		}
	}
	return ready, len(candidates) - len(ready)
}

// requestModel returns the top-level "model" of a generation request body, or "" if absent.
func requestModel(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	return req.Model
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
