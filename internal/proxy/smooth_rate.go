package proxy

import (
	"strings"
	"sync"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/scheduler"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

var (
	smoothRateMu        sync.RWMutex
	modelLastDispatched = make(map[string]time.Time) // key: accountID + "\x00" + model
)

// ResetSmoothRate clears the dispatch history (used in tests).
func ResetSmoothRate() {
	smoothRateMu.Lock()
	defer smoothRateMu.Unlock()
	modelLastDispatched = make(map[string]time.Time)
}

// ModelSmoothInterval returns the minimum recommended interval between requests for a given model
// to stay well within upstream provider rate-limit bounds (RPM).
func ModelSmoothInterval(model string) time.Duration {
	lower := strings.ToLower(model)
	if strings.Contains(lower, "pro") {
		return 6 * time.Second
	}
	return 1 * time.Second
}

// RecordModelDispatch records that a request for model was dispatched to accountID.
func RecordModelDispatch(accountID, model string, now time.Time) {
	if accountID == "" || model == "" {
		return
	}
	smoothRateMu.Lock()
	defer smoothRateMu.Unlock()
	key := accountID + "\x00" + model
	modelLastDispatched[key] = now
}

// SmoothModelRate reorders candidates to prioritize accounts that have rested past the model's
// smooth interval, preventing burst requests from hammering a single account and triggering upstream 429s.
// It only reorders within the top tier so rate-limited, depleted or reserve accounts are never promoted.
func SmoothModelRate(candidates []*storage.Account, model string, now time.Time) []*storage.Account {
	if len(candidates) < 2 || model == "" {
		return candidates
	}

	n := scheduler.TopTierCount(candidates, float64(now.Unix()))
	if n < 2 {
		return candidates
	}

	interval := ModelSmoothInterval(model)

	smoothRateMu.RLock()
	defer smoothRateMu.RUnlock()

	ready := make([]*storage.Account, 0, n)
	cooling := make([]*storage.Account, 0, n)

	for _, acc := range candidates[:n] {
		if acc == nil {
			continue
		}
		key := acc.ID + "\x00" + model
		last, exists := modelLastDispatched[key]
		if !exists || now.Sub(last) >= interval {
			ready = append(ready, acc)
		} else {
			cooling = append(cooling, acc)
		}
	}

	out := make([]*storage.Account, 0, len(candidates))
	out = append(out, ready...)
	out = append(out, cooling...)
	out = append(out, candidates[n:]...)
	return out
}
