package forecast

import (
	"fmt"
	"io"
	"strings"

	"github.com/vlxlv/agy-pool-go/internal/storage"
)

const (
	clrReset  = "\033[0m"
	clrBold   = "\033[1m"
	clrDim    = "\033[2m"
	clrGreen  = "\033[32m"
	clrYellow = "\033[33m"
	clrRed    = "\033[31m"
	clrCyan   = "\033[36m"
)

// PrintRunwaySummary renders compact runway forecast block inside the main quota dashboard.
func PrintRunwaySummary(w io.Writer, pool *storage.Pool, now float64) {
	res := SimulateQuotaRunway(pool, nil, now)
	if res.Status == "no_accounts" || res.Status == "all_restricted" {
		return
	}

	subSep := strings.Repeat("-", 68)
	fmt.Fprintln(w, subSep)
	fmt.Fprintf(w, " %s%s⚡ Quota Runway & Endurance Forecast:%s\n", clrBold, clrCyan, clrReset)

	hCount := res.HealthyCount
	tot := res.TotalAccounts
	cool := res.CoolingCount
	isIdle := res.IsIdle
	pace := res.TargetPace
	sim := res.CurrentRunway
	bench := res.BenchmarkRunway

	healthInfo := fmt.Sprintf("%d/%d Active", hCount, tot)
	if cool > 0 {
		healthInfo += fmt.Sprintf(" (%d Cooling)", cool)
	}

	var paceInfo string
	if isIdle {
		paceInfo = fmt.Sprintf("%sIdle (0 gens/hr)%s", clrDim, clrReset)
	} else {
		winLabel := "1h"
		if res.Telemetry != nil {
			winLabel = res.Telemetry.WindowUsed
		}
		if winLabel == "smoothed" {
			winLabel = "smoothed active"
		} else {
			winLabel = fmt.Sprintf("%s window", winLabel)
		}
		paceInfo = fmt.Sprintf("%s%.1f gens/hr%s (%s%s%s)", clrBold, pace, clrReset, clrCyan, winLabel, clrReset)
	}

	fmt.Fprintf(w, "  • Health & Pace:   %s | %s\n", healthInfo, paceInfo)

	replenish := res.ReplenishRatePctHr
	if !isIdle {
		burn := res.BurnRatePctHr
		net := res.NetRatePctHr
		sign := ""
		if net >= 0 {
			sign = "+"
		}
		netColor := clrYellow
		if net >= 0 {
			netColor = clrGreen
		}
		fmt.Fprintf(w, "  • Burn/Replenish:  ~%.1f%%/hr burn | +%.1f%%/hr reload (%s%s%.1f%%/hr net%s)\n",
			burn, replenish, netColor, sign, net, clrReset)
	} else {
		fmt.Fprintf(w, "  • Capacity Reload: +%.1f%%/hr natural recovery across pool\n", replenish)
	}

	// 5-hour rolling runway
	if isIdle {
		var b5h string
		if bench.IsSustainable5H {
			b5h = fmt.Sprintf("%s⚡ Sustainable%s (> 24h at standard ~20 gens/hr pace)", clrGreen, clrReset)
		} else if bench.FirstStallSec != nil {
			stallStr := FormatDurationShort(bench.FirstStallSec)
			b5h = fmt.Sprintf("%s~%s%s at standard ~20 gens/hr pace", clrYellow, stallStr, clrReset)
		} else {
			b5h = fmt.Sprintf("%s⚡ Sustainable%s", clrGreen, clrReset)
		}
		fmt.Fprintf(w, "  • Active Coding:   %s\n", b5h)
	} else {
		var run5h string
		if sim.IsSustainable5H {
			run5h = fmt.Sprintf("%s⚡ Sustainable%s (Pool replenishes faster than burn rate)", clrGreen, clrReset)
		} else if sim.FirstStallSec != nil {
			stallStr := FormatDurationShort(sim.FirstStallSec)
			causeNote := ""
			if sim.StallDurationSec != nil && *sim.StallDurationSec > 0 {
				pauseStr := FormatDurationShort(sim.StallDurationSec)
				causeNote = fmt.Sprintf(" (Dry pause: ~%s until next reset)", pauseStr)
			}
			run5h = fmt.Sprintf("%s~%s until first stall%s%s", clrYellow, stallStr, clrReset, causeNote)
		} else {
			run5h = fmt.Sprintf("%s⚡ Sustainable%s", clrGreen, clrReset)
		}
		fmt.Fprintf(w, "  • 5-Hour Runway:   %s\n", run5h)
	}

	// Weekly budget runway
	activeSim := sim
	if isIdle {
		activeSim = bench
	}
	var wRun string
	if activeSim.IsSustainableWeekly {
		wRun = fmt.Sprintf("%s⚡ Sustainable%s (> 7 Days remaining)", clrGreen, clrReset)
	} else if activeSim.WeeklyExhaustSec != nil {
		wDays := *activeSim.WeeklyExhaustSec / 86400.0
		wColor := clrYellow
		if wDays <= 2.0 {
			wColor = clrRed
		}
		wRun = fmt.Sprintf("%s~%.1f Days remaining%s", wColor, wDays, clrReset)
	} else {
		wRun = fmt.Sprintf("%s⚡ Sustainable%s", clrGreen, clrReset)
	}

	wLabel := "Weekly Runway:  "
	if isIdle {
		wLabel = "Weekly Budget:  "
	}
	fmt.Fprintf(w, "  • %s %s\n", wLabel, wRun)
}
