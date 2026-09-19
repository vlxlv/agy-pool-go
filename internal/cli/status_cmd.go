package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
	"github.com/vlxlv/agy-pool-go/internal/observability"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// ManageStrategy displays or updates the pool load balancing strategy.
func ManageStrategy(targetStrategy string, stdout, stderr io.Writer) bool {
	validStrategies := []string{config.StrategyMaxQuota, config.StrategyLeastUsed, config.StrategyRoundRobin}

	pool, err := storage.LoadPool()
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Could not load the account pool.%s\n", clrRed, clrReset)
		return false
	}
	current := config.StrategyMaxQuota
	if pool != nil && pool.Strategy != "" {
		current = pool.Strategy
	}

	if targetStrategy == "" {
		fmt.Fprintf(stdout, "\nCurrent load balancing strategy: %s%s%s%s\n\n", clrBold, clrGreen, current, clrReset)
		fmt.Fprintf(stdout, "Available strategies:\n")
		fmt.Fprintf(stdout, "  • %smax_quota%s   - Prioritizes accounts with safest remaining 5-hour and weekly quota, taking reset times into account (default)\n", clrBold, clrReset)
		fmt.Fprintf(stdout, "  • %sleast_used%s  - Distributes load evenly to accounts with lowest AI generation count (Hits)\n", clrBold, clrReset)
		fmt.Fprintf(stdout, "  • %sround_robin%s - Cycles through ready accounts in sequential rotation\n\n", clrBold, clrReset)
		fmt.Fprintf(stdout, "To update strategy: %sagy-pool strategy <name>%s\n\n", clrBold, clrReset)
		return true
	}

	targetStrategy = strings.ToLower(strings.TrimSpace(targetStrategy))
	isValid := false
	for _, s := range validStrategies {
		if s == targetStrategy {
			isValid = true
			break
		}
	}

	if !isValid {
		fmt.Fprintf(stdout, "%s[Error] Invalid strategy '%s'. Choose from: %s%s\n",
			clrRed, targetStrategy, strings.Join(validStrategies, ", "), clrReset)
		return false
	}

	err = storage.PoolTransaction(func(p *storage.Pool) error {
		p.Strategy = targetStrategy
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Failed to update strategy: %v%s\n", clrRed, err, clrReset)
		return false
	}

	fmt.Fprintf(stdout, "%s✓ Load balancing strategy set to: %s%s%s\n", clrGreen, clrBold, targetStrategy, clrReset)
	return true
}

func formatLastUsed(ts *int64, now int64) string {
	if ts == nil || *ts <= 0 {
		return "never"
	}
	diff := now - *ts
	if diff < 0 {
		return "just now"
	}
	if diff < 60 {
		return fmt.Sprintf("%ds ago", diff)
	}
	if diff < 3600 {
		return fmt.Sprintf("%dm ago", diff/60)
	}
	if diff < 86400 {
		return fmt.Sprintf("%dh ago", diff/3600)
	}
	return fmt.Sprintf("%dd ago", diff/86400)
}

// ListCmd renders a compact account inventory without forced quota refresh.
func ListCmd(stdout, stderr io.Writer, args ...string) int {
	_, rem, err := resolveContextForCmd(args)
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Failed to resolve instance: %v%s\n", clrRed, err, clrReset)
		return 1
	}

	target := ""
	if len(rem) > 0 {
		target = rem[0]
	}

	pool, err := storage.LoadPool()
	if err != nil || pool == nil || len(pool.Accounts) == 0 {
		fmt.Fprintf(stdout, "\n%sNo accounts in pool yet.%s\n", clrYellow, clrReset)
		fmt.Fprintf(stdout, "Run %sagy-pool login%s or %sagy-pool import-current%s to add accounts.\n\n",
			clrBold, clrReset, clrBold, clrReset)
		return 0
	}

	accList := pool.Accounts
	if target != "" {
		if idx, err := strconv.Atoi(target); err == nil {
			if idx >= 1 && idx <= len(accList) {
				accList = []*storage.Account{accList[idx-1]}
			} else {
				fmt.Fprintf(stdout, "%s[Error] The selected account was not found.%s\n", clrRed, clrReset)
				return 0
			}
		} else {
			var found *storage.Account
			for _, a := range accList {
				if a != nil && (a.ID == target || a.Email == target || a.Name == target) {
					found = a
					break
				}
			}
			if found != nil {
				accList = []*storage.Account{found}
			} else {
				fmt.Fprintf(stdout, "%s[Error] The selected account was not found.%s\n", clrRed, clrReset)
				return 0
			}
		}
	}

	nowTS := time.Now().Unix()
	activeID := ""
	if pool.ActiveAccountID != nil {
		activeID = *pool.ActiveAccountID
	}

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "#\tACCOUNT ID\tNAME\tSTATUS\tHITS\t5H QUOTA\tWEEKLY QUOTA\tLAST USED")

	for i, a := range accList {
		if a == nil {
			continue
		}
		isActive := (a.ID == activeID)
		q5Frac, gwFrac := quota.DisplayQuotaFractions(a.LastQuota)
		isCooling := a.RateLimitedUntil != nil && *a.RateLimitedUntil > float64(nowTS)
		isExhausted := (q5Frac != nil && *q5Frac <= config.DepletedThreshold) ||
			(gwFrac != nil && *gwFrac <= config.DepletedThreshold)

		statusStr := "Ready"
		if a.Status == "validation_required" {
			statusStr = "⚠ Verify Required"
		} else if a.Status == "auth_error" {
			statusStr = "✖ Auth Error"
		} else if isCooling {
			if isActive {
				statusStr = "CLI Base (Cooldown)"
			} else {
				statusStr = "Cooldown"
			}
		} else if isExhausted {
			if isActive {
				statusStr = "CLI Base (Exhausted)"
			} else {
				statusStr = "Exhausted"
			}
		} else if isActive {
			statusStr = "CLI Base"
		}

		hits := a.GetHits()
		q5Str := "--"
		if q5Frac != nil {
			q5Str = fmt.Sprintf("%.1f%%", *q5Frac*100)
		}
		gwStr := "--"
		if gwFrac != nil {
			gwStr = fmt.Sprintf("%.1f%%", *gwFrac*100)
		}
		lastUsedStr := formatLastUsed(a.LastUsedAt, nowTS)
		dispName := accounts.DisplayAccountName(a)

		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			i+1, a.ID, dispName, statusStr, hits, q5Str, gwStr, lastUsedStr)
	}
	w.Flush()

	strat := pool.Strategy
	if strat == "" {
		strat = config.StrategyMaxQuota
	}
	fmt.Fprintf(stdout, "\nStrategy: %s | CLI Base: %s | Total: %d account(s)\n",
		strat, activeID, len(pool.Accounts))
	return 0
}

// QuotaCmd renders the dedicated full quota dashboard with progress bars and reset countdowns.
func QuotaCmd(stdout, stderr io.Writer, args ...string) int {
	_, rem, err := resolveContextForCmd(args)
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Failed to resolve instance: %v%s\n", clrRed, err, clrReset)
		return 1
	}

	if len(rem) > 0 && rem[0] == "--refresh" {
		fmt.Fprintln(stderr, "usage: agy-pool quota [account]")
		fmt.Fprintln(stderr, "agy-pool quota: error: unrecognized argument '--refresh'")
		return 2
	}
	target := ""
	if len(rem) > 0 {
		target = rem[0]
	}

	ListAccounts(target, stdout, stderr)
	return 0
}

// StatusCmd checks daemon status and displays gateway and runtime summary only.
func StatusCmd(stdout, stderr io.Writer, args ...string) int {
	ctx, rem, err := resolveContextForCmd(args)
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Failed to resolve instance: %v%s\n", clrRed, err, clrReset)
		return 1
	}

	verbose := false
	for _, a := range rem {
		if a == "-v" || a == "--verbose" {
			verbose = true
			break
		}
	}

	fmt.Fprintf(stdout, "agy-pool v%s\n\n", config.Version)

	status, info, msg := daemon.CheckInstanceStatus(ctx)
	switch status {
	case daemon.StatusRunningSameInstance:
		fmt.Fprintf(stdout, "Gateway : %sRUNNING%s  PID %d  %s:%d\n",
			clrGreen, clrReset, info.PID, ctx.ListenHost, ctx.ListenPort)
	case daemon.StatusOutdatedBinary:
		fmt.Fprintf(stdout, "Gateway : %sRUNNING (Outdated Code)%s  PID %d  %s:%d\n",
			clrYellow, clrReset, info.PID, ctx.ListenHost, ctx.ListenPort)
	case daemon.StatusConfigMismatch:
		fmt.Fprintf(stdout, "Gateway : %sCONFIG MISMATCH%s (%s)\n", clrYellow, clrReset, msg)
	case daemon.StatusPortOccupiedForeign:
		fmt.Fprintf(stdout, "Gateway : %sPORT OCCUPIED%s (port %d is occupied by another process)\n", clrYellow, clrReset, ctx.ListenPort)
	case daemon.StatusForeignInstance:
		fmt.Fprintf(stdout, "Gateway : %sFOREIGN INSTANCE%s (%s)\n", clrYellow, clrReset, msg)
	case daemon.StatusStalePID:
		_ = os.Remove(ctx.PIDFile())
		_ = os.Remove(ctx.RuntimeFile())
		fmt.Fprintf(stdout, "Gateway : %sSTOPPED%s (stale PID file cleaned)\n", clrYellow, clrReset)
	case daemon.StatusLegacyPID:
		fmt.Fprintf(stdout, "Gateway : %sLEGACY PID%s (PID: %d)\n", clrYellow, clrReset, info.PID)
	case daemon.StatusStopped:
		fmt.Fprintf(stdout, "Gateway : %sSTOPPED%s\n", clrYellow, clrReset)
	default:
		fmt.Fprintf(stdout, "Gateway : %sSTOPPED%s (%s)\n", clrYellow, clrReset, msg)
	}

	if status == daemon.StatusRunningSameInstance || status == daemon.StatusOutdatedBinary {
		if info != nil && info.PID > 0 {
			stats, err := ctx.GetRuntimeStats(info.PID)
			if err == nil && stats != nil {
				uptime := daemon.FormatUptime(time.Since(stats.StartedAt))
				goVer := daemon.FormatGoVersion(stats.GoVersion)
				allocStr := daemon.FormatMemoryBytes(stats.AllocBytes)
				heapStr := daemon.FormatMemoryBytes(stats.HeapBytes)
				sysStr := daemon.FormatMemoryBytes(stats.SysBytes)

				fmt.Fprintf(stdout, "Uptime  : %s\n", uptime)
				fmt.Fprintf(stdout, "Runtime : %s | Goroutines: %d\n", goVer, stats.Goroutines)
				fmt.Fprintf(stdout, "Memory  : Alloc %s | Heap %s | Sys %s\n", allocStr, heapStr, sysStr)
				fmt.Fprintf(stdout, "GC      : %d cycles\n", stats.NumGC)

				if verbose {
					fmt.Fprintf(stdout, "\n  %sRuntime Counters%s\n", clrBold, clrReset)
					fmt.Fprintf(stdout, "    %-18s %6s requests / %s success\n", "Generation",
						observability.FormatNumber(stats.GenerationRequests), observability.FormatNumber(stats.GenerationSuccess))
					fmt.Fprintf(stdout, "    %-18s %6s decisions\n", "Routing",
						observability.FormatNumber(stats.RoutingDecisions))
					fmt.Fprintf(stdout, "    %-18s %6s attempts / %s recovered\n", "Failover",
						observability.FormatNumber(stats.FailoverAttempts), observability.FormatNumber(stats.FailoverSuccess))
					fmt.Fprintf(stdout, "    %-18s %6s\n", "Restricted skips",
						observability.FormatNumber(stats.RestrictedSkips))
					fmt.Fprintf(stdout, "    %-18s %6s / %s ok / %s failed\n", "Quota refresh",
						observability.FormatNumber(stats.QuotaRefreshAttempts), observability.FormatNumber(stats.QuotaRefreshSuccess), observability.FormatNumber(stats.QuotaRefreshFailure))
					fmt.Fprintf(stdout, "    %-18s %6s / %s ok / %s failed\n", "Auth refresh",
						observability.FormatNumber(stats.AuthRefreshAttempts), observability.FormatNumber(stats.AuthRefreshSuccess), observability.FormatNumber(stats.AuthRefreshFailure))
					fmt.Fprintf(stdout, "    %-18s %6s\n", "Persistence errors",
						observability.FormatNumber(stats.PersistenceErrors))
					fmt.Fprintf(stdout, "    %-18s %6s\n", "Replay prevented",
						observability.FormatNumber(stats.NoReplayPrevented))
				}
			} else {
				fmt.Fprintf(stdout, "Runtime : (metrics unavailable)\n")
				if verbose {
					fmt.Fprintf(stdout, "\n  %sRuntime Counters%s\n", clrBold, clrReset)
					fmt.Fprintf(stdout, "    (metrics unavailable)\n")
				}
			}
		}
	} else if verbose {
		fmt.Fprintf(stdout, "\n  %sRuntime Counters%s\n", clrBold, clrReset)
		fmt.Fprintf(stdout, "    (gateway not running)\n")
	}

	fmt.Fprintf(stdout, "\n")
	if ctx.ConfigPath != "" {
		fmt.Fprintf(stdout, "Config  : %s\n", ctx.ConfigPath)
	}
	dbPath := filepath.Join(ctx.DataDir, "state.db")
	fmt.Fprintf(stdout, "State   : %s  (schema v%s)\n", dbPath, storage.StateDBSchemaVersion)

	pool, err := storage.LoadPool()
	if err != nil || pool == nil {
		fmt.Fprintf(stdout, "Pool    : (unavailable: %v)\n", err)
		return 0
	}

	totalAccounts := len(pool.Accounts)
	strat := pool.Strategy
	if strat == "" {
		strat = config.StrategyMaxQuota
	}

	nowTS := time.Now().Unix()
	readyCnt, cooldownCnt, exhaustedCnt, restrictedCnt := 0, 0, 0, 0
	activeName := "none"

	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		if pool.ActiveAccountID != nil && a.ID == *pool.ActiveAccountID {
			activeName = accounts.DisplayAccountName(a)
		}

		q5Frac, gwFrac := quota.DisplayQuotaFractions(a.LastQuota)
		remFraction := 1.0
		if q5Frac != nil {
			remFraction = *q5Frac
		}
		if gwFrac != nil && *gwFrac < remFraction {
			remFraction = *gwFrac
		}

		if a.Status == "validation_required" || a.Status == "auth_error" {
			restrictedCnt++
		} else if a.RateLimitedUntil != nil && *a.RateLimitedUntil > float64(nowTS) {
			cooldownCnt++
		} else if remFraction <= config.DepletedThreshold {
			exhaustedCnt++
		} else {
			readyCnt++
		}
	}

	if totalAccounts == 0 {
		fmt.Fprintf(stdout, "Pool    : 0 accounts (empty)\n")
	} else {
		fmt.Fprintf(stdout, "Pool    : %d accounts | CLI Base: %s | %d Ready | %d Cooldown | %d Restricted\n",
			totalAccounts, activeName, readyCnt, cooldownCnt, restrictedCnt)
	}
	fmt.Fprintf(stdout, "Strategy: %s\n", strat)

	fmt.Fprintf(stdout, "\n")
	if status == daemon.StatusRunningSameInstance {
		if restrictedCnt > 0 || cooldownCnt > 0 {
			fmt.Fprintf(stdout, "%s⚠ Degraded (%d restricted, %d cooling)%s\n", clrYellow, restrictedCnt, cooldownCnt, clrReset)
		} else {
			fmt.Fprintf(stdout, "%s✓ Healthy%s\n", clrGreen, clrReset)
		}
	} else if status == daemon.StatusStopped {
		fmt.Fprintf(stdout, "%s○ Stopped%s\n", clrYellow, clrReset)
	} else {
		fmt.Fprintf(stdout, "%s⚠ %s%s\n", clrYellow, msg, clrReset)
	}

	return 0
}
