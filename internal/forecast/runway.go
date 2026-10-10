package forecast

import (
	"fmt"
	"math"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// FormatDurationShort formats seconds into compact human-readable duration (e.g. "4h 50m", "6d 22h", "45m", "30s").
func FormatDurationShort(sec *float64) string {
	if sec == nil {
		return "N/A"
	}
	s := int64(*sec)
	if s < 0 {
		s = 0
	}
	days := s / 86400
	rem := s % 86400
	hours := rem / 3600
	rem = rem % 3600
	mins := rem / 60
	secs := rem % 60

	if days > 0 {
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %02dm", hours, mins)
	}
	if mins > 0 {
		return fmt.Sprintf("%dm", mins)
	}
	return fmt.Sprintf("%ds", secs)
}

// RunSingleSimulation simulates chronological quota depletion and staggered reset replenishment.
// Models minute-by-minute account selection and capacity recovery.
func RunSingleSimulation(simAccs []SimAccount, pace float64, strategy string, now float64, simHours int, paceWeekly *float64) SingleRunwayResult {
	if pace <= 0 {
		return SingleRunwayResult{
			IsSustainable5H:     true,
			IsSustainableWeekly: true,
		}
	}

	pw := pace
	if paceWeekly != nil {
		pw = *paceWeekly
	}

	accs := make([]SimAccount, len(simAccs))
	copy(accs, simAccs)

	var firstEmptyAccID string
	var firstEmptySec *float64
	var firstStallSec *float64
	var firstStallCause string
	var stallDurationSec *float64
	var weeklyExhaustSec *float64

	t := now
	endT := now + float64(simHours*3600)

	cost5H := DefaultFractionPerHit5H
	costQW := DefaultFractionPerHitWeekly

	for t < endT {
		elapsed := t - now
		var dt float64
		if elapsed < 12*3600 {
			dt = 60.0
		} else if elapsed < 48*3600 {
			dt = 300.0
		} else {
			dt = 1800.0
		}

		t += dt

		// Apply quota resets
		for i := range accs {
			a := &accs[i]
			if !a.IsRestricted {
				if t >= a.Reset5TS {
					a.Q5 = 1.0
					for a.Reset5TS <= t {
						a.Reset5TS += Window5HSecs
					}
				}
				if t >= a.ResetWTS {
					a.QW = 1.0
					for a.ResetWTS <= t {
						a.ResetWTS += Window7DSecs
					}
				}
			}
		}

		demandGens5H := pace * (dt / 3600.0)
		demandGensQW := pw * (dt / 3600.0)
		demandQ5 := demandGens5H * cost5H
		demandQW := demandGensQW * costQW

		var eligible []*SimAccount
		for i := range accs {
			a := &accs[i]
			if !a.IsRestricted && t >= a.CooldownUntil && a.Q5 > 0.005 && a.QW > 0.005 {
				eligible = append(eligible, a)
			}
		}

		if len(eligible) == 0 {
			if firstStallSec == nil {
				stallSec := t - now
				firstStallSec = &stallSec

				weeklyDead := true
				coolingDead := true
				for _, a := range accs {
					if !a.IsRestricted {
						if a.QW > 0.005 {
							weeklyDead = false
						}
						if t >= a.CooldownUntil {
							coolingDead = false
						}
					}
				}
				if weeklyDead {
					firstStallCause = "weekly"
				} else if coolingDead {
					firstStallCause = "cooldown"
				} else {
					firstStallCause = "5h"
				}

				var reliefCandidates []float64
				for _, a := range accs {
					if !a.IsRestricted {
						reliefCandidates = append(reliefCandidates, a.Reset5TS, a.ResetWTS)
						if a.CooldownUntil > t {
							reliefCandidates = append(reliefCandidates, a.CooldownUntil)
						}
					}
				}
				if len(reliefCandidates) > 0 {
					minRelief := reliefCandidates[0]
					for _, r := range reliefCandidates[1:] {
						if r < minRelief {
							minRelief = r
						}
					}
					dur := math.Max(0.0, minRelief-t)
					stallDurationSec = &dur
				}
			}

			allWeeklyDead := true
			for _, a := range accs {
				if !a.IsRestricted && a.QW > 0.005 {
					allWeeklyDead = false
					break
				}
			}
			if allWeeklyDead {
				if weeklyExhaustSec == nil {
					exhaustSec := t - now
					weeklyExhaustSec = &exhaustSec
				}
				break
			}

			if elapsed >= 48*3600 {
				break
			}
			continue
		}

		var selected *SimAccount
		if strategy == "max_quota" {
			selected = eligible[0]
			for _, a := range eligible[1:] {
				if a.Q5 > selected.Q5 {
					selected = a
				} else if a.Q5 == selected.Q5 {
					if a.QW > selected.QW {
						selected = a
					} else if a.QW == selected.QW {
						if a.SimHits < selected.SimHits {
							selected = a
						}
					}
				}
			}
		} else { // smart (default)
			selected = eligible[0]
			for _, a := range eligible[1:] {
				// Prioritize account renewing soonest
				aRenew := a.ResetWTS
				if aRenew <= 0 || (a.Reset5TS > 0 && a.Reset5TS < aRenew) {
					aRenew = a.Reset5TS
				}
				selRenew := selected.ResetWTS
				if selRenew <= 0 || (selected.Reset5TS > 0 && selected.Reset5TS < selRenew) {
					selRenew = selected.Reset5TS
				}

				if aRenew > 0 && (selRenew <= 0 || aRenew < selRenew) {
					selected = a
				} else if aRenew == selRenew {
					if a.SimHits < selected.SimHits {
						selected = a
					} else if a.SimHits == selected.SimHits && a.Q5 > selected.Q5 {
						selected = a
					}
				}
			}
		}

		selected.Q5 = math.Max(0.0, selected.Q5-demandQ5)
		selected.QW = math.Max(0.0, selected.QW-demandQW)
		selected.SimHits += demandGens5H

		if selected.Q5 <= 0.005 && firstEmptySec == nil {
			es := t - now
			firstEmptySec = &es
			firstEmptyAccID = selected.ID
		}
		if selected.QW <= 0.005 && weeklyExhaustSec == nil {
			allDead := true
			for _, a := range accs {
				if !a.IsRestricted && a.QW > 0.005 {
					allDead = false
					break
				}
			}
			if allDead {
				es := t - now
				weeklyExhaustSec = &es
			}
		}
	}

	isSustainable5H := (firstStallSec == nil || *firstStallSec >= 24*3600)
	isSustainableWeekly := (weeklyExhaustSec == nil)

	return SingleRunwayResult{
		FirstStallSec:       firstStallSec,
		FirstStallCause:     firstStallCause,
		StallDurationSec:    stallDurationSec,
		FirstEmptyAccID:     firstEmptyAccID,
		FirstEmptySec:       firstEmptySec,
		WeeklyExhaustSec:    weeklyExhaustSec,
		IsSustainable5H:     isSustainable5H,
		IsSustainableWeekly: isSustainableWeekly,
	}
}

// SimulateQuotaRunway evaluates pool endurance by combining telemetry, staggered resets, and dual windows.
// Returns complete forecast metrics for current pace and standard benchmark pace.
func SimulateQuotaRunway(pool *storage.Pool, paceGensPerHour *float64, nowOpt ...float64) *RunwayForecast {
	now := float64(time.Now().Unix())
	if len(nowOpt) > 0 && nowOpt[0] > 0 {
		now = nowOpt[0]
	}

	if pool == nil || len(pool.Accounts) == 0 {
		return &RunwayForecast{
			Status:        "no_accounts",
			HealthyCount:  0,
			TotalAccounts: 0,
		}
	}

	simHours := 168

	var simAccs []SimAccount
	showEmail := accounts.IsShowEmailEnabled(pool)

	for _, acc := range pool.Accounts {
		if acc == nil {
			continue
		}
		status := acc.Status
		isRestricted := status == storage.StatusValidationRequired || status == storage.StatusAuthError || status == storage.StatusDisabled
		cooldownUntil := 0.0
		if acc.RateLimitedUntil != nil {
			cooldownUntil = *acc.RateLimitedUntil
		}

		g5Frac, gwFrac := quota.DisplayQuotaFractions(acc.LastQuota)
		q5 := 1.0
		if g5Frac != nil {
			q5 = *g5Frac
		}
		qw := 1.0
		if gwFrac != nil {
			qw = *gwFrac
		}

		var reset5, resetw any
		if acc.LastQuota != nil {
			if acc.LastQuota.Gemini5H != nil && acc.LastQuota.Gemini5H.ResetTime != nil {
				reset5 = acc.LastQuota.Gemini5H.ResetTime
			}
			if acc.LastQuota.GeminiWeekly != nil && acc.LastQuota.GeminiWeekly.ResetTime != nil {
				resetw = acc.LastQuota.GeminiWeekly.ResetTime
			}
			if reset5 == nil && resetw == nil {
				reset5 = acc.LastQuota.ResetTime
				resetw = acc.LastQuota.ResetTime
			}
		}

		reset5TS := quota.ParseISOOrTimestamp(reset5)
		resetwTS := quota.ParseISOOrTimestamp(resetw)
		var lastUpdated *float64
		if acc.LastQuota != nil && acc.LastQuota.UpdatedAt != nil {
			lu := float64(*acc.LastQuota.UpdatedAt)
			lastUpdated = &lu
		}

		var r5 float64
		if reset5TS != nil && *reset5TS <= now {
			if lastUpdated == nil || *lastUpdated < *reset5TS {
				q5 = 1.0
			}
			r5 = *reset5TS
			for r5 <= now {
				r5 += Window5HSecs
			}
		} else if reset5TS == nil {
			r5 = now + Window5HSecs
		} else {
			r5 = *reset5TS
		}

		var rw float64
		if resetwTS != nil && *resetwTS <= now {
			if lastUpdated == nil || *lastUpdated < *resetwTS {
				qw = 1.0
			}
			rw = *resetwTS
			for rw <= now {
				rw += Window7DSecs
			}
		} else if resetwTS == nil {
			rw = now + Window7DSecs
		} else {
			rw = *resetwTS
		}

		simHits := float64(acc.RequestCount)
		if acc.GenCount != nil {
			simHits = float64(*acc.GenCount)
		}

		simAccs = append(simAccs, SimAccount{
			ID:            acc.ID,
			DisplayName:   accounts.DisplayAccountName(acc, showEmail),
			IsRestricted:  isRestricted,
			CooldownUntil: cooldownUntil,
			Q5:            q5,
			QW:            qw,
			Reset5TS:      r5,
			ResetWTS:      rw,
			SimHits:       simHits,
		})
	}

	healthyCount := 0
	coolingCount := 0
	restrictedCount := 0
	for _, a := range simAccs {
		if !a.IsRestricted {
			healthyCount++
			if a.CooldownUntil > now {
				coolingCount++
			}
		} else {
			restrictedCount++
		}
	}

	if healthyCount == 0 {
		return &RunwayForecast{
			Status:          "all_restricted",
			HealthyCount:    0,
			CoolingCount:    0,
			RestrictedCount: restrictedCount,
			TotalAccounts:   len(pool.Accounts),
			Message:         "All accounts are restricted (verification required or auth error)",
		}
	}

	telemetry := GetBurnRate(pool, now)
	strategy := pool.Strategy
	if strategy == "" {
		strategy = "max_quota"
	}

	var targetPace float64
	var targetWeeklyPace float64
	isCustomPace := false
	isIdle := false

	if paceGensPerHour != nil {
		targetPace = math.Max(0.0, *paceGensPerHour)
		targetWeeklyPace = targetPace
		isCustomPace = true
		isIdle = false
	} else {
		targetPace = telemetry.GensPerHour
		targetWeeklyPace = telemetry.DailyPace
		isCustomPace = false
		isIdle = (targetPace == 0.0)
	}

	currentSim := RunSingleSimulation(simAccs, targetPace, strategy, now, simHours, &targetWeeklyPace)
	benchWeeklyPace := BenchmarkPaceGensPerHour * (8.0 / 24.0)
	benchmarkSim := RunSingleSimulation(simAccs, BenchmarkPaceGensPerHour, strategy, now, simHours, &benchWeeklyPace)

	replenishRate := float64(healthyCount) * 20.0
	burnRatePct := targetPace * DefaultFractionPerHit5H * 100.0
	netRatePct := replenishRate - burnRatePct

	var totalQ5, totalQW float64
	for _, a := range simAccs {
		if !a.IsRestricted {
			totalQ5 += a.Q5 * 100.0
			totalQW += a.QW * 100.0
		}
	}

	accForecasts := make([]AccountForecast, 0, len(simAccs))
	for _, a := range simAccs {
		accForecasts = append(accForecasts, AccountForecast{
			ID:           a.ID,
			DisplayName:  a.DisplayName,
			Q5Pct:        math.Round(a.Q5*1000.0) / 10.0,
			QWPct:        math.Round(a.QW*1000.0) / 10.0,
			Reset5InSec:  int64(math.Max(0, a.Reset5TS-now)),
			ResetWInSec:  int64(math.Max(0, a.ResetWTS-now)),
			IsCooling:    a.CooldownUntil > now,
			IsRestricted: a.IsRestricted,
		})
	}

	return &RunwayForecast{
		Status:             "ok",
		HealthyCount:       healthyCount,
		CoolingCount:       coolingCount,
		RestrictedCount:    restrictedCount,
		TotalAccounts:      len(pool.Accounts),
		Strategy:           strategy,
		TargetPace:         targetPace,
		TargetWeeklyPace:   targetWeeklyPace,
		IsCustomPace:       isCustomPace,
		IsIdle:             isIdle,
		Telemetry:          &telemetry,
		ReplenishRatePctHr: replenishRate,
		BurnRatePctHr:      burnRatePct,
		NetRatePctHr:       netRatePct,
		TotalQ5Pct:         totalQ5,
		TotalQWPct:         totalQW,
		CurrentRunway:      currentSim,
		BenchmarkRunway:    benchmarkSim,
		AccountsForecast:   accForecasts,
	}
}
