package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/forecast"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// ForecastCmd predicts quota endurance and runway based on usage and staggered resets.
func ForecastCmd(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("forecast", flag.ContinueOnError)
	fs.SetOutput(w)

	var paceStr string
	var jsonOutput bool

	fs.StringVar(&paceStr, "pace", "", "Simulated generation rate in generations/hour")
	fs.StringVar(&paceStr, "p", "", "Simulated generation rate in generations/hour")
	fs.BoolVar(&jsonOutput, "json", false, "Output results in JSON format")

	if err := fs.Parse(args); err != nil {
		return err
	}

	var customPace *float64
	if paceStr != "" {
		p, err := strconv.ParseFloat(paceStr, 64)
		if err != nil {
			return fmt.Errorf("invalid pace value %q: %w", paceStr, err)
		}
		customPace = &p
	}

	pool, err := storage.LoadPool()
	if err != nil {
		return fmt.Errorf("failed to load accounts pool: %w", err)
	}

	if len(pool.Accounts) == 0 {
		if jsonOutput {
			out, _ := json.MarshalIndent(map[string]any{
				"status": "no_accounts",
				"error":  "No accounts in pool yet",
			}, "", "  ")
			fmt.Fprintln(w, string(out))
		} else {
			fmt.Fprintf(w, "\n%sNo accounts in pool yet.%s\n", clrYellow, clrReset)
			fmt.Fprintf(w, "Run %sagy-pool login%s or %sagy-pool import-current%s to add accounts.\n\n", clrBold, clrReset, clrBold, clrReset)
		}
		return nil
	}

	res := forecast.SimulateQuotaRunway(pool, customPace)

	if jsonOutput {
		out, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to encode JSON forecast: %w", err)
		}
		fmt.Fprintln(w, string(out))
		return nil
	}

	if res.Status == "all_restricted" {
		fmt.Fprintf(w, "\n%s[Error] All accounts are restricted (security verification required or auth failure).%s\n", clrRed, clrReset)
		fmt.Fprintf(w, "Run %sagy-pool verify%s to unlock restricted accounts in browser.\n\n", clrBold, clrReset)
		return nil
	}

	sep := strings.Repeat("=", 68)
	subSep := strings.Repeat("-", 68)

	fmt.Fprintf(w, "\n%s\n", sep)
	title := fmt.Sprintf("Quota Runway & Endurance Forecast (v%s)", config.Version)
	fmt.Fprintf(w, "%s%s%s\n", clrBold, centerString(title, 68), clrReset)
	fmt.Fprintf(w, "%s\n", sep)

	var paceMode string
	if res.IsCustomPace {
		paceMode = "User Specified"
	} else if res.IsIdle {
		paceMode = "Idle (Reference Pace)"
	} else {
		winUsed := "1h"
		if res.Telemetry != nil {
			winUsed = res.Telemetry.WindowUsed
		}
		paceMode = fmt.Sprintf("Measured (%s)", winUsed)
	}

	effPace := res.TargetPace
	if res.IsIdle {
		effPace = forecast.BenchmarkPaceGensPerHour
	}

	effWeeklyPace := res.TargetWeeklyPace
	if res.IsIdle {
		effWeeklyPace = forecast.BenchmarkPaceGensPerHour * (8.0 / 24.0)
	}

	activeSim := res.CurrentRunway
	if res.IsIdle {
		activeSim = res.BenchmarkRunway
	}

	fmt.Fprintf(w, "  Pool Health:     %s%d/%d%s Accounts Active (%d Cooling, %d Restricted)\n",
		clrBold, res.HealthyCount, res.TotalAccounts, clrReset, res.CoolingCount, res.RestrictedCount)
	fmt.Fprintf(w, "  Active Strategy: %s%s%s\n", clrBold, res.Strategy, clrReset)
	fmt.Fprintf(w, "  Simulated Pace:  %s%.1f generations/hour%s (%s)\n", clrBold, effPace, clrReset, paceMode)
	if !res.IsCustomPace && !res.IsIdle {
		fmt.Fprintf(w, "  Daily Work Pace: %s%.1f generations/hour%s (modeled across active work sessions)\n",
			clrBold, effWeeklyPace, clrReset)
	}
	fmt.Fprintf(w, "  Stock on Hand:   %s%.1f%%%s total 5h quota | %s%.1f%%%s total weekly quota\n",
		clrBold, res.TotalQ5Pct, clrReset, clrBold, res.TotalQWPct, clrReset)

	cost5hPct := forecast.DefaultFractionPerHit5H * 100.0
	costQwPct := forecast.DefaultFractionPerHitWeekly * 100.0

	fmt.Fprintf(w, "\n%s[5-Hour Rolling Burst Runway]%s\n", clrBold, clrReset)
	burn := effPace * forecast.DefaultFractionPerHit5H * 100.0
	replenish := res.ReplenishRatePctHr
	net := replenish - burn
	netSign := ""
	if net >= 0 {
		netSign = "+"
	}
	netColor := clrYellow
	if net >= 0 {
		netColor = clrGreen
	}
	netLabel := "Deficit"
	if net >= 0 {
		netLabel = "Surplus"
	}

	fmt.Fprintf(w, "  • Natural Replenish:  +%.1f%%/hr (%d accounts × 20.0%%/hr)\n", replenish, res.HealthyCount)
	fmt.Fprintf(w, "  • Estimated Burn:     ~%.1f%%/hr (~%.2f%% per generation)\n", burn, cost5hPct)
	fmt.Fprintf(w, "  • Net Balance:        %s%s%.1f%%/hr%s (%s)\n", netColor, netSign, net, clrReset, netLabel)

	if activeSim.IsSustainable5H {
		fmt.Fprintf(w, "  • 5-Hour Status:      %s⚡ SUSTAINABLE%s (Pool regenerates faster than consumption)\n", clrGreen, clrReset)
		fmt.Fprintf(w, "  • Continuous Burst:   %s> 48 Hours%s (No 5h quota starvation expected)\n", clrGreen, clrReset)
	} else {
		stallStr := forecast.FormatDurationShort(activeSim.FirstStallSec)
		fmt.Fprintf(w, "  • 5-Hour Status:      %s🔻 DRAINING%s\n", clrYellow, clrReset)
		fmt.Fprintf(w, "  • Time to 1st Stall:  %s~%s%s\n", clrYellow, stallStr, clrReset)
		if activeSim.StallDurationSec != nil && *activeSim.StallDurationSec > 0 {
			pauseStr := forecast.FormatDurationShort(activeSim.StallDurationSec)
			fmt.Fprintf(w, "  • Dry Period Pause:   ~%s until next account reset\n", pauseStr)
		}
		if activeSim.FirstEmptyAccID != "" {
			fmt.Fprintf(w, "  • 1st Account Empty:  Account %s\n", activeSim.FirstEmptyAccID)
		}
	}

	fmt.Fprintf(w, "\n%s[Weekly Budget Runway]%s\n", clrBold, clrReset)
	weeklyBurn := effWeeklyPace * forecast.DefaultFractionPerHitWeekly * 100.0
	dailyBurn := weeklyBurn * 24.0
	fmt.Fprintf(w, "  • Modeled Daily Burn: ~%.1f%%/day (~%.3f%% per gen, %.1f gens/hr avg)\n", dailyBurn, costQwPct, effWeeklyPace)

	if activeSim.IsSustainableWeekly {
		fmt.Fprintf(w, "  • Weekly Status:      %s⚡ SUSTAINABLE%s (> 7 Days capacity on hand)\n", clrGreen, clrReset)
	} else if activeSim.WeeklyExhaustSec != nil {
		wDays := *activeSim.WeeklyExhaustSec / 86400.0
		wColor := clrYellow
		if wDays <= 2.0 {
			wColor = clrRed
		}
		fmt.Fprintf(w, "  • Weekly Status:      %s⚠ WEEKLY LIMIT PINCH%s\n", wColor, clrReset)
		fmt.Fprintf(w, "  • Weekly Runway:      %s~%.1f Days remaining%s\n", wColor, wDays, clrReset)
	}

	fmt.Fprintf(w, "\n%s[Staggered Reset Timeline]%s\n", clrBold, clrReset)
	for _, a := range res.AccountsForecast {
		r5Sec := float64(a.Reset5InSec)
		rwSec := float64(a.ResetWInSec)
		r5Str := forecast.FormatDurationShort(&r5Sec)
		rwStr := forecast.FormatDurationShort(&rwSec)
		statusTag := ""
		if a.IsRestricted {
			statusTag = fmt.Sprintf(" %s[Restricted]%s", clrRed, clrReset)
		} else if a.IsCooling {
			statusTag = fmt.Sprintf(" %s[Cooldown]%s", clrYellow, clrReset)
		}
		fmt.Fprintf(w, "  • %s: 5h in %s (Now: %.1f%%) | Weekly in %s (Now: %.1f%%)%s\n",
			a.DisplayName, r5Str, a.Q5Pct, rwStr, a.QWPct, statusTag)
	}

	fmt.Fprintf(w, "%s\n", subSep)
	fmt.Fprintf(w, "%sRun 'agy-pool forecast --pace <N>' to simulate custom generation rates.%s\n", clrDim, clrReset)
	fmt.Fprintf(w, "%s\n\n", sep)

	return nil
}

func centerString(s string, width int) string {
	if len(s) >= width {
		return s
	}
	left := (width - len(s)) / 2
	right := width - len(s) - left
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", right)
}
