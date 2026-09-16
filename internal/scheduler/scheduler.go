package scheduler

import (
	"sort"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// OrderCandidates orders account candidates according to the specified strategy and reference time.
func OrderCandidates(candidates []*storage.Account, strategy string, pool *storage.Pool, now float64) []*storage.Account {
	if len(candidates) == 0 {
		return nil
	}

	var eligible []*storage.Account
	var depleted []*storage.Account
	var cooldown []*storage.Account
	var restricted []*storage.Account

	for _, acc := range candidates {
		if acc == nil {
			continue
		}
		if acc.Status == "validation_required" || acc.Status == "auth_error" {
			restricted = append(restricted, acc)
		} else if acc.RateLimitedUntil != nil && *acc.RateLimitedUntil > now {
			cooldown = append(cooldown, acc)
		} else {
			capState := quota.ComputeCapacityState(acc, now)
			if capState.IsDepleted {
				depleted = append(depleted, acc)
			} else {
				eligible = append(eligible, acc)
			}
		}
	}

	switch strategy {
	case config.StrategyLeastUsed:
		sort.SliceStable(eligible, func(i, j int) bool {
			a, b := eligible[i], eligible[j]
			hitsA, hitsB := a.GetHits(), b.GetHits()
			if hitsA != hitsB {
				return hitsA < hitsB
			}

			capA := quota.ComputeCapacityState(a, now)
			capB := quota.ComputeCapacityState(b, now)

			if capA.KnownWindowCount != capB.KnownWindowCount {
				return capA.KnownWindowCount > capB.KnownWindowCount
			}

			rankA := quota.FreshnessRank(a, now)
			rankB := quota.FreshnessRank(b, now)
			if rankA != rankB {
				return rankA > rankB
			}

			if capA.WorstPace != capB.WorstPace {
				return capA.WorstPace > capB.WorstPace
			}

			if capA.TotalPace != capB.TotalPace {
				return capA.TotalPace > capB.TotalPace
			}

			if capA.RawFloor != capB.RawFloor {
				return capA.RawFloor > capB.RawFloor
			}

			return a.ID < b.ID
		})

	case config.StrategyRoundRobin:
		if pool != nil && pool.RoundRobinLastAccountID != nil {
			lastID := *pool.RoundRobinLastAccountID
			foundIdx := -1
			for i, a := range eligible {
				if a.ID == lastID {
					foundIdx = i
					break
				}
			}
			if foundIdx != -1 {
				rotated := make([]*storage.Account, 0, len(eligible))
				rotated = append(rotated, eligible[foundIdx+1:]...)
				rotated = append(rotated, eligible[:foundIdx+1]...)
				eligible = rotated
			}
		}

	default: // StrategyMaxQuota
		sort.SliceStable(eligible, func(i, j int) bool {
			a, b := eligible[i], eligible[j]
			capA := quota.ComputeCapacityState(a, now)
			capB := quota.ComputeCapacityState(b, now)

			if capA.KnownWindowCount != capB.KnownWindowCount {
				return capA.KnownWindowCount > capB.KnownWindowCount
			}

			rankA := quota.FreshnessRank(a, now)
			rankB := quota.FreshnessRank(b, now)
			if rankA != rankB {
				return rankA > rankB
			}

			if capA.WorstPace != capB.WorstPace {
				return capA.WorstPace > capB.WorstPace
			}

			if capA.TotalPace != capB.TotalPace {
				return capA.TotalPace > capB.TotalPace
			}

			if capA.RawFloor != capB.RawFloor {
				return capA.RawFloor > capB.RawFloor
			}

			hitsA, hitsB := a.GetHits(), b.GetHits()
			return hitsA < hitsB // Lowest hits first (key had -hits descending)
		})
	}

	// Secondary tiers ordering: Depleted accounts ordered by raw_floor DESC, hits ASC
	sort.SliceStable(depleted, func(i, j int) bool {
		a, b := depleted[i], depleted[j]
		capA := quota.ComputeCapacityState(a, now)
		capB := quota.ComputeCapacityState(b, now)

		if capA.RawFloor != capB.RawFloor {
			return capA.RawFloor > capB.RawFloor
		}

		hitsA, hitsB := a.GetHits(), b.GetHits()
		return hitsA < hitsB
	})

	result := make([]*storage.Account, 0, len(eligible)+len(depleted)+len(cooldown)+len(restricted))
	result = append(result, eligible...)
	result = append(result, depleted...)
	result = append(result, cooldown...)
	result = append(result, restricted...)
	return result
}

// ReserveRoundRobinCandidates atomically selects candidate ordering and advances
// the persisted round_robin cursor under a storage transaction before network dispatch.
func ReserveRoundRobinCandidates(now float64) ([]*storage.Account, error) {
	var candidates []*storage.Account
	err := storage.PoolTransaction(func(pool *storage.Pool) error {
		if len(pool.Accounts) == 0 {
			candidates = nil
			return nil
		}
		ordered := OrderCandidates(pool.Accounts, config.StrategyRoundRobin, pool, now)
		if len(ordered) > 0 && ordered[0].ID != "" {
			pool.RoundRobinLastAccountID = &ordered[0].ID
		}
		candidates = ordered
		return nil
	})
	if err != nil {
		return nil, err
	}
	return candidates, nil
}

// ReserveRoundRobinAccount atomically reserves and returns the next account candidate.
func ReserveRoundRobinAccount(now float64) (*storage.Account, error) {
	candidates, err := ReserveRoundRobinCandidates(now)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	return candidates[0], nil
}
