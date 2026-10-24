package scheduler

import (
	"math"
	"sort"
	"sync/atomic"
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

var smartRRCursor atomic.Uint64

// AdvanceSmartRRCursor advances the smart round-robin cursor by 1.
func AdvanceSmartRRCursor() uint64 {
	return smartRRCursor.Add(1)
}

// GetSmartRRCursor returns the current smart round-robin cursor.
func GetSmartRRCursor() uint64 {
	return smartRRCursor.Load()
}

// ResetSmartRRCursor resets the smart round-robin cursor to 0 (used for test isolation).
func ResetSmartRRCursor() {
	smartRRCursor.Store(0)
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
// at the beginning of a 7-day cycle. Between 0.50 and 0.80 weekly quota, it blends
// smoothly from Pace5 into WorstPace to avoid a cliff discontinuity at 0.80.
func EffectiveWorstPace(capState quota.CapacityState) float64 {
	if capState.Q5Known && capState.Q5 != nil && capState.Pace5 != nil {
		if capState.Q7Known && capState.Q7 != nil {
			q7 := *capState.Q7
			if q7 >= 0.80 {
				return *capState.Pace5
			}
			if q7 > 0.50 && capState.WorstPace < *capState.Pace5 {
				w := (0.80 - q7) / 0.30
				return (1.0-w)*(*capState.Pace5) + w*capState.WorstPace
			}
		}
	}
	return capState.WorstPace
}

// Smart scheduling constants
const (
	SmartHarvest5HWindow    = 90 * time.Minute
	SmartHarvestMinHeadroom = 0.25
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

// SmartHarvestCandidate checks if an account qualifies for harvest mode: an upcoming 5H reset
// within 90m whose expiring window still has at least smartHarvestFloor headroom, while
// weekly quota maintains at least 25% reserve. Weekly quota is never harvested as it is a
// hard stock ceiling for the entire week. Returns (earliestResetTime, true).
func SmartHarvestCandidate(capState quota.CapacityState, nowTime time.Time) (time.Time, bool) {
	legacy := !capState.Q5Known && !capState.Q7Known
	if legacy && capState.RawFloor < SmartHarvestMinHeadroom {
		return time.Time{}, false
	}
	// atLeast reports whether a window is unknown or has at least min headroom.
	atLeast := func(known bool, q *float64, min float64) bool {
		return legacy || !known || q == nil || *q >= min
	}

	if capState.Reset5Time != nil && !capState.Reset5Time.IsZero() {
		rem5 := capState.Reset5Time.Sub(nowTime)
		if rem5 > 0 && rem5 <= SmartHarvest5HWindow &&
			atLeast(capState.Q5Known, capState.Q5, smartHarvestFloor(rem5, SmartHarvest5HWindow)) &&
			atLeast(capState.Q7Known, capState.Q7, SmartHarvestMinHeadroom) {
			return *capState.Reset5Time, true
		}
	}

	return time.Time{}, false
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

// smartEffectiveHeadroom returns the quota fraction used to assign an account's cruising health tier.
// When weekly quota is abundant (>= 80%) or on/ahead of linear weekly pace (Pace7 >= 0), the weekly
// window is not a bottleneck, so the regenerable 5H window determines the health tier rather than
// letting mid-week Q7 < 80% strand a full 5H window.
func smartEffectiveHeadroom(capState quota.CapacityState) float64 {
	if capState.Q5Known && capState.Q5 != nil && capState.Q7Known && capState.Q7 != nil {
		if *capState.Q7 >= 0.80 || (capState.Pace7 != nil && *capState.Pace7 >= -1e-9) {
			return *capState.Q5
		}
	}
	return capState.RawFloor
}

// resetBucket quantizes a reset timestamp relative to nowTime: 15-minute buckets within the 90m
// 5H harvest range so imminent resets are never merged with hour-away resets across a UTC clock seam,
// and 1-hour buckets (offset by 100 to preserve monotonicity across the 90m boundary) beyond 90m.
func resetBucket(resetTime time.Time, nowTime time.Time) int64 {
	rem := resetTime.Sub(nowTime)
	if rem <= SmartHarvest5HWindow {
		return int64(rem / (15 * time.Minute))
	}
	return 100 + int64(rem/time.Hour)
}

// compareResetTiming compares two accounts' upcoming reset schedules (earliest reset first, then
// secondary window reset if earliest buckets tie). Returns <0 if a resets sooner, >0 if b resets sooner.
func compareResetTiming(a, b quota.CapacityState, nowTime time.Time) int {
	nextA, nextB := NextUpcomingReset(a), NextUpcomingReset(b)
	if nextA == nil && nextB == nil {
		return 0
	}
	if nextA == nil {
		return 1
	}
	if nextB == nil {
		return -1
	}
	ba, bb := resetBucket(*nextA, nowTime), resetBucket(*nextB, nowTime)
	if ba != bb {
		if ba < bb {
			return -1
		}
		return 1
	}

	laterWindow := func(c quota.CapacityState) *time.Time {
		if c.Reset5Time != nil && !c.Reset5Time.IsZero() && c.Reset7Time != nil && !c.Reset7Time.IsZero() {
			if c.Reset5Time.After(*c.Reset7Time) {
				return c.Reset5Time
			}
			return c.Reset7Time
		}
		return nil
	}
	la, lb := laterWindow(a), laterWindow(b)
	if la != nil && lb != nil {
		lba, lbb := resetBucket(*la, nowTime), resetBucket(*lb, nowTime)
		if lba != lbb {
			if lba < lbb {
				return -1
			}
			return 1
		}
	}
	return 0
}

// smartInFlightPerTier is how many in-flight requests cost one health tier under smart, so a burst
// spills from a lone top-tier account onto the next tier instead of piling onto it until it hits 429.
// It also caps how many concurrent requests a harvest account takes before it competes as cruising.
const smartInFlightPerTier = 4

// maxQuotaInFlightPenalty is the virtual quota/pace deduction per active in-flight request under
// max_quota (3% per concurrent request), allowing bursts to spill across closely matched accounts
// without overriding a whole-tier (20%) headroom advantage on a single request.
const maxQuotaInFlightPenalty = 0.03

// sortKey is computed once per account before sorting, so comparators never see in-flight counts
// change mid-sort and capacity state is not recomputed per comparison.
type sortKey struct {
	cap       quota.CapacityState
	fresh     int
	flight    int64
	hits      int64
	harvest   bool
	harvestB  int64
	score     int // smart: health tier minus in-flight penalty
	pace      float64
	floor     float64
	totalPace float64
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
		flight := observability.GetInFlightGeneration(acc.ID)
		flightPenalty := float64(flight) * maxQuotaInFlightPenalty
		k := &sortKey{
			cap:       capState,
			fresh:     quota.FreshnessRank(acc, now),
			flight:    flight,
			hits:      acc.GetHits(),
			pace:      bucket(EffectiveWorstPace(capState)-flightPenalty, paceEpsilon),
			floor:     bucket(capState.RawFloor-flightPenalty, paceEpsilon),
			totalPace: bucket(capState.TotalPace-flightPenalty, paceEpsilon),
		}
		harvestT, harvest := SmartHarvestCandidate(capState, nowTime)
		k.harvest = harvest && k.flight < smartInFlightPerTier
		if k.harvest {
			k.harvestB = resetBucket(harvestT, nowTime)
		}
		k.score = smartHeadroomTier(smartEffectiveHeadroom(capState))*smartInFlightPerTier - int(k.flight)
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
			if a.totalPace != b.totalPace {
				return a.totalPace > b.totalPace
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

			// 2. Harvest Mode: Prioritize accounts with surplus headroom
			// that are about to reset (5H <= 90m, WK <= 36h) to capture expiring quota.
			if a.harvest != b.harvest {
				return a.harvest // Qualifying harvest account wins over cruising account
			}
			if a.harvest && b.harvest {
				// Both have expiring quota: consume sooner reset bucket first (15m for 5H, 1h for weekly)
				if a.harvestB != b.harvestB {
					return a.harvestB < b.harvestB
				}
				// Equal reset bucket: higher in-flight-adjusted headroom tier first
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

			// 4. Renewal Timing: within the same health tier and in-flight load, prefer sooner reset.
			if cmp := compareResetTiming(a.cap, b.cap, nowTime); cmp != 0 {
				return cmp < 0
			}

			// 5. Hits tie-breaker:
			if a.hits != b.hits {
				return a.hits < b.hits
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

// RotateTopTier takes ordered candidates and rotates accounts within the top health tier by cursor.
// It is applied to new sessions to spread them round-robin across equally healthy accounts. The group
// uses smart's cruising key (health tier lowered by in-flight load) and stops at the first account smart
// would not treat as a healthy peer: cooling down, depleted, reserve, or over weekly pace. Nothing is
// rotated while the first account is harvesting an expiring 5H window, so that quota still gets used.
func RotateTopTier(candidates []*storage.Account, cursor uint64, now float64) []*storage.Account {
	if len(candidates) <= 1 {
		return candidates
	}
	score := func(acc *storage.Account) (int, bool) {
		if acc == nil || IsRestricted(acc) || (acc.RateLimitedUntil != nil && *acc.RateLimitedUntil > now) || OverWeeklyPace(acc, now) {
			return 0, false
		}
		c := quota.ComputeCapacityState(acc, now)
		if c.IsDepleted || IsReserveCapacity(c) {
			return 0, false
		}
		return smartHeadroomTier(smartEffectiveHeadroom(c))*smartInFlightPerTier - int(observability.GetInFlightGeneration(acc.ID)), true
	}
	top, ok := score(candidates[0])
	if !ok {
		return candidates
	}
	if _, harvest := SmartHarvestCandidate(quota.ComputeCapacityState(candidates[0], now), time.Unix(int64(now), 0)); harvest {
		return candidates
	}

	topCount := 1
	for _, acc := range candidates[1:] {
		if s, ok := score(acc); !ok || s != top {
			break
		}
		topCount++
	}
	if topCount <= 1 {
		return candidates
	}

	shift := int(cursor % uint64(topCount))
	if shift == 0 {
		return candidates
	}

	rotated := make([]*storage.Account, len(candidates))
	copy(rotated, candidates[shift:topCount])
	copy(rotated[topCount-shift:], candidates[:shift])
	copy(rotated[topCount:], candidates[topCount:])
	return rotated
}
