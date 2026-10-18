package scheduler

import (
	"math"
	"sort"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/observability"
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

// Smart scheduling constants
const (
	SmartHarvest5HWindow     = 90 * time.Minute
	SmartHarvestWeeklyWindow = 36 * time.Hour
	SmartHarvestMinHeadroom  = 0.25
)

// smartHeadroomTier maps an account's bottleneck headroom to discrete tiers (20% bands).
// This guarantees strict weak ordering (transitivity) in sorting while enabling
// active concurrent multi-account rotation within each health band.
func smartHeadroomTier(headroom float64) int {
	switch {
	case headroom >= 0.80:
		return 4 // Prime: 80% - 100%
	case headroom >= 0.60:
		return 3 // Healthy: 60% - 80%
	case headroom >= 0.40:
		return 2 // Moderate: 40% - 60%
	case headroom >= 0.20:
		return 1 // Low/Conservation: 20% - 40%
	default:
		return 0 // Near Reserve: < 20%
	}
}

// smartHarvestFloor is the headroom an expiring window needs to be harvested. It decays linearly
// from SmartHarvestMinHeadroom at the start of the harvest window to the depleted threshold at reset,
// so small leftovers are still spent right before they expire.
func smartHarvestFloor(rem, window time.Duration) float64 {
	floor := SmartHarvestMinHeadroom * float64(rem) / float64(window)
	if floor < config.DepletedThreshold {
		return config.DepletedThreshold
	}
	return floor
}

// SmartHarvestCandidate checks if an account qualifies for harvest mode: an upcoming reset
// (either 5H within 90m or Weekly within 36h) whose expiring window still has at least
// smartHarvestFloor headroom, while the other (non-expiring) window keeps the full 25%.
// Legacy combined fractions keep the flat RawFloor >= 25% rule. Returns (earliestResetTime, true).
func SmartHarvestCandidate(capState quota.CapacityState, nowTime time.Time) (time.Time, bool) {
	legacy := !capState.Q5Known && !capState.Q7Known
	if legacy && capState.RawFloor < SmartHarvestMinHeadroom {
		return time.Time{}, false
	}
	// atLeast reports whether a window is unknown or has at least min headroom.
	atLeast := func(known bool, q *float64, min float64) bool {
		return legacy || !known || q == nil || *q >= min
	}

	var best time.Time
	hasBest := false

	if capState.Reset5Time != nil && !capState.Reset5Time.IsZero() {
		rem5 := capState.Reset5Time.Sub(nowTime)
		if rem5 > 0 && rem5 <= SmartHarvest5HWindow &&
			atLeast(capState.Q5Known, capState.Q5, smartHarvestFloor(rem5, SmartHarvest5HWindow)) &&
			atLeast(capState.Q7Known, capState.Q7, SmartHarvestMinHeadroom) {
			best = *capState.Reset5Time
			hasBest = true
		}
	}

	if capState.Reset7Time != nil && !capState.Reset7Time.IsZero() {
		rem7 := capState.Reset7Time.Sub(nowTime)
		if rem7 > 0 && rem7 <= SmartHarvestWeeklyWindow &&
			atLeast(capState.Q7Known, capState.Q7, smartHarvestFloor(rem7, SmartHarvestWeeklyWindow)) &&
			atLeast(capState.Q5Known, capState.Q5, SmartHarvestMinHeadroom) {
			if !hasBest || capState.Reset7Time.Before(best) {
				best = *capState.Reset7Time
				hasBest = true
			}
		}
	}

	return best, hasBest
}

// NextUpcomingReset returns the earliest upcoming reset time for the account.
func NextUpcomingReset(capState quota.CapacityState) *time.Time {
	var earliest *time.Time
	if capState.Reset5Time != nil && !capState.Reset5Time.IsZero() {
		earliest = capState.Reset5Time
	}
	if capState.Reset7Time != nil && !capState.Reset7Time.IsZero() {
		if earliest == nil || capState.Reset7Time.Before(*earliest) {
			earliest = capState.Reset7Time
		}
	}
	return earliest
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
	case config.StrategyMaxQuota:
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
			flightA := observability.GetInFlightGeneration(a.ID)
			flightB := observability.GetInFlightGeneration(b.ID)
			if flightA != flightB {
				return flightA < flightB
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

	default: // StrategySmart (default strategy)
		var healthy []*storage.Account
		var reserve []*storage.Account

		for _, acc := range eligible {
			if IsReserveCandidate(acc, now) {
				reserve = append(reserve, acc)
			} else {
				healthy = append(healthy, acc)
			}
		}

		nowTime := time.Unix(int64(now), 0)

		smartCompare := func(a, b *storage.Account) bool {
			// 0. Active In-Flight Concurrency:
			// Distribute concurrent requests across idle accounts.
			// An account with fewer active in-flight requests is strictly preferred.
			flightA := observability.GetInFlightGeneration(a.ID)
			flightB := observability.GetInFlightGeneration(b.ID)
			if flightA != flightB {
				return flightA < flightB
			}

			capA := quota.ComputeCapacityState(a, now)
			capB := quota.ComputeCapacityState(b, now)

			// 1. Freshness rank (prefer freshly probed accounts when unknown)
			rankA := quota.FreshnessRank(a, now)
			rankB := quota.FreshnessRank(b, now)
			if rankA != rankB {
				return rankA > rankB
			}

			// 2. Harvest Mode: Prioritize accounts with surplus headroom (>= 25%)
			// that are about to reset (5H <= 90m, WK <= 36h) to capture expiring quota.
			tA, harvestA := SmartHarvestCandidate(capA, nowTime)
			tB, harvestB := SmartHarvestCandidate(capB, nowTime)

			if harvestA != harvestB {
				return harvestA // Qualifying harvest account wins over cruising account
			}
			if harvestA && harvestB {
				// Both have expiring quota: consume sooner reset first (hour resolution)
				truncA := tA.Truncate(time.Hour)
				truncB := tB.Truncate(time.Hour)
				if !truncA.Equal(truncB) {
					return truncA.Before(truncB)
				}
				// Equal reset hour: higher headroom tier first
				tierA := smartHeadroomTier(capA.RawFloor)
				tierB := smartHeadroomTier(capB.RawFloor)
				if tierA != tierB {
					return tierA > tierB
				}
				hitsA, hitsB := a.GetHits(), b.GetHits()
				if hitsA != hitsB {
					return hitsA < hitsB
				}
				return a.ID < b.ID
			}

			// 3. Cruising Mode: Headroom Health Tier (Discrete 20% bands)
			// Accounts in higher tiers strictly take precedence over lower tiers.
			tierA := smartHeadroomTier(capA.RawFloor)
			tierB := smartHeadroomTier(capB.RawFloor)
			if tierA != tierB {
				return tierA > tierB
			}

			// 4. Multi-Account Concurrent Load Balancing:
			// Within the same health tier, distribute traffic across all accounts by request hits.
			// This maximizes active multi-account utilization and parallel 5H rolling recoveries.
			hitsA, hitsB := a.GetHits(), b.GetHits()
			if hitsA != hitsB {
				return hitsA < hitsB
			}

			// 5. Renewal Timing: when hits and tier are tied, prefer the account that resets sooner
			nextA := NextUpcomingReset(capA)
			nextB := NextUpcomingReset(capB)
			if nextA != nil && nextB != nil {
				truncA := nextA.Truncate(time.Hour)
				truncB := nextB.Truncate(time.Hour)
				if !truncA.Equal(truncB) {
					return truncA.Before(truncB)
				}
			} else if nextA != nil || nextB != nil {
				return nextA != nil
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
	}

	// Secondary tiers ordering: Depleted accounts ordered by in_flight ASC, raw_floor DESC, hits ASC
	sort.SliceStable(depleted, func(i, j int) bool {
		a, b := depleted[i], depleted[j]
		flightA := observability.GetInFlightGeneration(a.ID)
		flightB := observability.GetInFlightGeneration(b.ID)
		if flightA != flightB {
			return flightA < flightB
		}
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
