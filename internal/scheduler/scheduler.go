package scheduler

import (
	"math"
	"sort"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

func init() {
	accounts.SetSmartOrderHook(func(candidates []*storage.Account, pool *storage.Pool, now float64) []*storage.Account {
		return OrderCandidates(candidates, config.StrategySmart, pool, now)
	})
}

// FiveHourReserveFloor is the threshold below or equal to which an account's 5-hour quota is in reserve.
const FiveHourReserveFloor = 0.15

// WeeklyReserveFloor is the base threshold below or equal to which an account's weekly quota is in reserve.
const WeeklyReserveFloor = 0.10

// DynamicWeeklyReserveFloor returns the time-decaying weekly reserve floor.
// As weekly reset approaches, the reserve floor decays proportionally with the remaining time in the week,
// ensuring perishable quota is not stranded before weekly reset.
func DynamicWeeklyReserveFloor(r7 float64) float64 {
	if r7 <= 0 || r7 >= 1.0 {
		return WeeklyReserveFloor
	}
	floor := WeeklyReserveFloor * r7
	if floor < config.DepletedThreshold {
		return config.DepletedThreshold
	}
	return floor
}

// IsReserveCandidate reports whether a candidate account has one or more known quota windows
// at or below the reserve thresholds, while not being depleted.
func IsReserveCandidate(acc *storage.Account, now float64) bool {
	if acc == nil {
		return false
	}
	capState := quota.ComputeCapacityState(acc, now)
	return IsReserveCapacity(capState)
}

// IsReserveCapacity reports whether a computed CapacityState qualifies as reserve.
// Legacy fractions have no window identity; they retain existing capacity ranking
// and depletion behavior without applying a window-specific reserve floor.
func IsReserveCapacity(capState quota.CapacityState) bool {
	if capState.IsDepleted {
		return false
	}
	if capState.Q5Known && capState.Q5 != nil && *capState.Q5 <= FiveHourReserveFloor {
		return true
	}
	weeklyFloor := DynamicWeeklyReserveFloor(capState.R7)
	if capState.Q7Known && capState.Q7 != nil && *capState.Q7 <= weeklyFloor {
		return true
	}
	return false
}

// IsRestricted reports whether an account is blocked from generation routing.
func IsRestricted(acc *storage.Account) bool {
	return acc != nil && (acc.Status == storage.StatusValidationRequired ||
		acc.Status == storage.StatusVerificationPending ||
		acc.Status == storage.StatusAuthError ||
		acc.Status == storage.StatusDisabled)
}

// EffectiveWorstPace returns the scheduling pace for candidate ranking.
// When an account's weekly quota is abundant (>= 0.80), the weekly window is not
// a constraint bottleneck; we use 5H pace to avoid cold-start linear decay starvation
// at the beginning of a 7-day cycle.
func EffectiveWorstPace(capState quota.CapacityState) float64 {
	if capState.Q5Known && capState.Q5 != nil && capState.Pace5 != nil {
		if capState.Q7Known && capState.Q7 != nil && *capState.Q7 >= 0.80 {
			return *capState.Pace5
		}
	}
	return capState.WorstPace
}

// AccountRenewalTimes extracts renewal reset times in order of window significance (7D weekly, then 5H).
func AccountRenewalTimes(capState quota.CapacityState) []time.Time {
	var renews []time.Time
	if capState.Reset7Time != nil && !capState.Reset7Time.IsZero() {
		renews = append(renews, *capState.Reset7Time)
	}
	if capState.Reset5Time != nil && !capState.Reset5Time.IsZero() {
		renews = append(renews, *capState.Reset5Time)
	}
	return renews
}

// OrderCandidates orders generation candidates according to the specified strategy.
func OrderCandidates(candidates []*storage.Account, strategy string, pool *storage.Pool, now float64) []*storage.Account {
	if len(candidates) == 0 {
		return nil
	}

	var eligible []*storage.Account
	var depleted []*storage.Account
	var cooldown []*storage.Account

	for _, acc := range candidates {
		if acc == nil || IsRestricted(acc) {
			continue
		}
		if acc.RateLimitedUntil != nil && *acc.RateLimitedUntil > now {
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

	case config.StrategySmart:
		var healthy []*storage.Account
		var reserve []*storage.Account

		for _, acc := range eligible {
			if IsReserveCandidate(acc, now) {
				reserve = append(reserve, acc)
			} else {
				healthy = append(healthy, acc)
			}
		}

		smartCompare := func(a, b *storage.Account) bool {
			capA := quota.ComputeCapacityState(a, now)
			capB := quota.ComputeCapacityState(b, now)

			timesA := AccountRenewalTimes(capA)
			timesB := AccountRenewalTimes(capB)

			for k := 0; k < len(timesA) || k < len(timesB); k++ {
				var tA, tB time.Time
				if k < len(timesA) {
					tA = timesA[k].Truncate(time.Hour)
				}
				if k < len(timesB) {
					tB = timesB[k].Truncate(time.Hour)
				}

				switch {
				case tA.Equal(tB):
					continue
				case tA.IsZero() || tB.IsZero():
					return tB.IsZero() // known renewal time goes before unknown
				default:
					return tA.Before(tB) // renews sooner goes first
				}
			}

			hitsA, hitsB := a.GetHits(), b.GetHits()
			if hitsA != hitsB {
				return hitsA < hitsB // fewer hits preserves prompt cache
			}

			if capA.RawFloor != capB.RawFloor {
				return capA.RawFloor > capB.RawFloor
			}

			return a.ID < b.ID
		}

		sort.SliceStable(healthy, func(i, j int) bool {
			return smartCompare(healthy[i], healthy[j])
		})

		sort.SliceStable(reserve, func(i, j int) bool {
			return smartCompare(reserve[i], reserve[j])
		})

		eligible = make([]*storage.Account, 0, len(healthy)+len(reserve))
		eligible = append(eligible, healthy...)
		eligible = append(eligible, reserve...)

	default: // StrategyMaxQuota
		var healthy []*storage.Account
		var reserve []*storage.Account

		for _, acc := range eligible {
			if IsReserveCandidate(acc, now) {
				reserve = append(reserve, acc)
			} else {
				healthy = append(healthy, acc)
			}
		}

		const paceEpsilon = 0.005

		maxQuotaCompare := func(a, b *storage.Account) bool {
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

			paceA := EffectiveWorstPace(capA)
			paceB := EffectiveWorstPace(capB)
			if math.IsInf(paceA, 0) || math.IsInf(paceB, 0) {
				if paceA != paceB {
					return paceA > paceB
				}
			} else if math.Abs(paceA-paceB) > paceEpsilon {
				return paceA > paceB
			}

			if math.Abs(capA.RawFloor-capB.RawFloor) > paceEpsilon {
				return capA.RawFloor > capB.RawFloor
			}

			hitsA, hitsB := a.GetHits(), b.GetHits()
			if hitsA != hitsB {
				return hitsA < hitsB // Lowest hits first (key had -hits descending)
			}

			return a.ID < b.ID
		}

		sort.SliceStable(healthy, func(i, j int) bool {
			return maxQuotaCompare(healthy[i], healthy[j])
		})

		sort.SliceStable(reserve, func(i, j int) bool {
			return maxQuotaCompare(reserve[i], reserve[j])
		})

		eligible = make([]*storage.Account, 0, len(healthy)+len(reserve))
		eligible = append(eligible, healthy...)
		eligible = append(eligible, reserve...)
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

	result := make([]*storage.Account, 0, len(eligible)+len(depleted)+len(cooldown))
	result = append(result, eligible...)
	result = append(result, depleted...)
	result = append(result, cooldown...)
	return result
}

// GetSchedulerSnapshot reads an authoritative point-in-time snapshot from state.db.
func GetSchedulerSnapshot() (*storage.Pool, error) {
	return storage.LoadPool()
}

// ReserveRoundRobinCandidates atomically selects candidate ordering and advances
// the persisted round_robin cursor under an immediate SQLite transaction before network dispatch.
func ReserveRoundRobinCandidates(now float64) ([]*storage.Account, error) {
	sdb, err := storage.GetStateDB()
	if err != nil {
		return nil, err
	}
	return sdb.ReserveRoundRobinCandidates(now, OrderCandidates)
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
