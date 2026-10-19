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

// WeeklyPaceSlack is how far weekly quota may run ahead of elapsed weekly time (q7 - r7) before
// routing holds the account back. Accounts with >= 70% weekly quota left are never affected.
const WeeklyPaceSlack = 0.30

// OverWeeklyPace reports whether an account is spending its week faster than time passes, so routing
// can move it behind on-pace accounts and concurrency cannot drain one weekly quota early in the cycle.
// Ordering and affinity use it; reserve labels and audit still use IsReserveCandidate.
func OverWeeklyPace(acc *storage.Account, now float64) bool {
	c := quota.ComputeCapacityState(acc, now)
	return c.Q7Known && c.Pace7 != nil && c.Reset7Time != nil && *c.Pace7 < -WeeklyPaceSlack-1e-9 // epsilon keeps q7 >= 0.70 exempt despite float rounding
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

// smartInFlightPerTier is how many in-flight requests cost one health tier under smart, so a burst
// spills from a lone top-tier account onto the next tier instead of piling onto it until it hits 429.
// It also caps how many concurrent requests a harvest account takes before it competes as cruising.
const smartInFlightPerTier = 4

// sortKey is computed once per account before sorting, so comparators never see in-flight counts
// change mid-sort and capacity state is not recomputed per comparison.
type sortKey struct {
	cap      quota.CapacityState
	fresh    int
	flight   int64
	hits     int64
	harvest  bool
	harvestT time.Time
	score    int // smart: health tier minus in-flight penalty
	pace     float64
	floor    float64
}

// bucket quantizes v so epsilon comparisons stay transitive; infinities pass through unchanged.
func bucket(v, eps float64) float64 {
	return math.Floor(v / eps)
}

// OrderCandidates orders generation candidates according to the specified strategy.
func OrderCandidates(candidates []*storage.Account, strategy string, pool *storage.Pool, now float64) []*storage.Account {
	if len(candidates) == 0 {
		return nil
	}

	const paceEpsilon = 0.005
	nowTime := time.Unix(int64(now), 0)

	var eligible []*storage.Account
	var depleted []*storage.Account
	var cooldown []*storage.Account
	keys := make(map[*storage.Account]*sortKey, len(candidates))

	for _, acc := range candidates {
		if acc == nil || IsRestricted(acc) {
			continue
		}
		capState := quota.ComputeCapacityState(acc, now)
		k := &sortKey{
			cap:    capState,
			fresh:  quota.FreshnessRank(acc, now),
			flight: observability.GetInFlightGeneration(acc.ID),
			hits:   acc.GetHits(),
			pace:   bucket(EffectiveWorstPace(capState), paceEpsilon),
			floor:  bucket(capState.RawFloor, paceEpsilon),
		}
		k.harvestT, k.harvest = SmartHarvestCandidate(capState, nowTime)
		k.harvest = k.harvest && k.flight < smartInFlightPerTier
		k.score = smartHeadroomTier(capState.RawFloor)*smartInFlightPerTier - int(k.flight)
		keys[acc] = k

		if acc.RateLimitedUntil != nil && *acc.RateLimitedUntil > now {
			cooldown = append(cooldown, acc)
		} else if capState.IsDepleted {
			depleted = append(depleted, acc)
		} else {
			eligible = append(eligible, acc)
		}
	}

	var healthy []*storage.Account
	var reserve []*storage.Account
	for _, acc := range eligible {
		if IsReserveCapacity(keys[acc].cap) || OverWeeklyPace(acc, now) {
			reserve = append(reserve, acc)
		} else {
			healthy = append(healthy, acc)
		}
	}

	var less func(a, b *sortKey, idA, idB string) bool
	switch strategy {
	case config.StrategyMaxQuota:
		less = func(a, b *sortKey, idA, idB string) bool {
			if a.cap.KnownWindowCount != b.cap.KnownWindowCount {
				return a.cap.KnownWindowCount > b.cap.KnownWindowCount
			}
			if a.fresh != b.fresh {
				return a.fresh > b.fresh
			}
			if a.pace != b.pace {
				return a.pace > b.pace
			}
			if a.floor != b.floor {
				return a.floor > b.floor
			}
			if a.flight != b.flight {
				return a.flight < b.flight
			}
			if a.hits != b.hits {
				return a.hits < b.hits // Lowest hits first (key had -hits descending)
			}
			return idA < idB
		}

	default: // StrategySmart (default strategy)
		less = func(a, b *sortKey, idA, idB string) bool {
			knownA := a.cap.KnownWindowCount > 0
			knownB := b.cap.KnownWindowCount > 0
			if knownA != knownB {
				return knownA
			}

			// 1. Freshness rank (prefer freshly probed accounts when unknown)
			if a.fresh != b.fresh {
				return a.fresh > b.fresh
			}

			// 2. Harvest Mode: Prioritize accounts with surplus headroom (>= 25%)
			// that are about to reset (5H <= 90m, WK <= 36h) to capture expiring quota.
			if a.harvest != b.harvest {
				return a.harvest // Qualifying harvest account wins over cruising account
			}
			if a.harvest && b.harvest {
				// Both have expiring quota: consume sooner reset first (hour resolution)
				truncA := a.harvestT.Truncate(time.Hour)
				truncB := b.harvestT.Truncate(time.Hour)
				if !truncA.Equal(truncB) {
					return truncA.Before(truncB)
				}
				// Equal reset hour: higher in-flight-adjusted headroom tier first
				if a.score != b.score {
					return a.score > b.score
				}
				if a.flight != b.flight {
					return a.flight < b.flight
				}
				if a.hits != b.hits {
					return a.hits < b.hits
				}
				return idA < idB
			}

			// 3. Cruising Mode: Headroom Health Tier (Discrete 20% bands), lowered by in-flight load
			// so concurrent requests spread to the next tier before saturating one account.
			if a.score != b.score {
				return a.score > b.score
			}

			if a.flight != b.flight {
				return a.flight < b.flight
			}

			// 4. Multi-Account Concurrent Load Balancing:
			// Within the same health tier, distribute traffic across all accounts by request hits.
			// This maximizes active multi-account utilization and parallel 5H rolling recoveries.
			if a.hits != b.hits {
				return a.hits < b.hits
			}

			// 5. Renewal Timing: when hits and tier are tied, prefer the account that resets sooner
			nextA := NextUpcomingReset(a.cap)
			nextB := NextUpcomingReset(b.cap)
			if nextA != nil && nextB != nil {
				truncA := nextA.Truncate(time.Hour)
				truncB := nextB.Truncate(time.Hour)
				if !truncA.Equal(truncB) {
					return truncA.Before(truncB)
				}
			} else if nextA != nil || nextB != nil {
				return nextA != nil
			}

			return idA < idB
		}
	}

	sortBy := func(list []*storage.Account, less func(a, b *sortKey, idA, idB string) bool) {
		sort.SliceStable(list, func(i, j int) bool {
			return less(keys[list[i]], keys[list[j]], list[i].ID, list[j].ID)
		})
	}
	sortBy(healthy, less)
	sortBy(reserve, less)

	// Secondary tiers ordering: Depleted accounts ordered by in_flight ASC, raw_floor DESC, hits ASC
	sortBy(depleted, func(a, b *sortKey, _, _ string) bool {
		if a.flight != b.flight {
			return a.flight < b.flight
		}
		if a.cap.RawFloor != b.cap.RawFloor {
			return a.cap.RawFloor > b.cap.RawFloor
		}
		return a.hits < b.hits
	})

	result := make([]*storage.Account, 0, len(healthy)+len(reserve)+len(depleted)+len(cooldown))
	result = append(result, healthy...)
	result = append(result, reserve...)
	result = append(result, depleted...)
	result = append(result, cooldown...)
	return result
}

// GetSchedulerSnapshot reads an authoritative point-in-time snapshot from state.db.
func GetSchedulerSnapshot() (*storage.Pool, error) {
	return storage.LoadPool()
}
