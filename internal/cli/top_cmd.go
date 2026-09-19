package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
	"github.com/vlxlv/agy-pool-go/internal/observability"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/storage"
	"golang.org/x/term"
)

// Min and Max refresh interval bounds
const (
	MinTopInterval = 500 * time.Millisecond
	MaxTopInterval = 60 * time.Second
	DefaultTopRate = 2 * time.Second
)

// TopOptions configures the live top / watch monitor.
type TopOptions struct {
	TargetAccount string
	Interval      time.Duration
	Width         int
	Once          bool
	Stdin         io.Reader
	Stdout        io.Writer
	Stderr        io.Writer
}

// TopCmd starts the interactive real-time quota & activity monitor dashboard.
func TopCmd(stdin io.Reader, stdout, stderr io.Writer, args ...string) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if stdin == nil {
		stdin = os.Stdin
	}

	cfgFile, dataDir, port, rem, err := ParseInstanceFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Invalid flag: %v%s\n", clrRed, err, clrReset)
		return 1
	}

	interval := DefaultTopRate
	targetAccount := ""
	once := false

	for i := 0; i < len(rem); i++ {
		a := rem[i]
		if a == "-i" || a == "--interval" {
			if i+1 < len(rem) {
				if d, err := time.ParseDuration(rem[i+1]); err == nil {
					interval = d
					i++
					continue
				} else if secs, err := strconv.ParseFloat(rem[i+1], 64); err == nil {
					interval = time.Duration(secs * float64(time.Second))
					i++
					continue
				}
			}
		} else if strings.HasPrefix(a, "--interval=") {
			val := strings.TrimPrefix(a, "--interval=")
			if d, err := time.ParseDuration(val); err == nil {
				interval = d
				continue
			} else if secs, err := strconv.ParseFloat(val, 64); err == nil {
				interval = time.Duration(secs * float64(time.Second))
				continue
			}
		} else if a == "-w" || a == "--watch" {
			// Transparently accepted from quota -w or status -w
			continue
		} else if a == "--once" {
			once = true
			continue
		} else if !strings.HasPrefix(a, "-") && targetAccount == "" {
			targetAccount = a
		}
	}

	if interval < MinTopInterval {
		interval = MinTopInterval
	} else if interval > MaxTopInterval {
		interval = MaxTopInterval
	}

	ctx, err := daemon.ResolveInstanceContext(dataDir, cfgFile, port, "")
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Failed to resolve instance: %v%s\n", clrRed, err, clrReset)
		return 1
	}
	if err := config.ConfigureDataDir(ctx.DataDir); err != nil {
		fmt.Fprintf(stderr, "%s[Error] Failed to configure data dir: %v%s\n", clrRed, err, clrReset)
		return 1
	}

	opts := TopOptions{
		TargetAccount: targetAccount,
		Interval:      interval,
		Once:          once,
		Stdin:         stdin,
		Stdout:        stdout,
		Stderr:        stderr,
	}

	return RunTop(ctx, opts)
}

// RunTop executes the dashboard loop or single-frame render.
func RunTop(ctx *daemon.InstanceContext, opts TopOptions) int {
	if opts.Interval < MinTopInterval {
		opts.Interval = MinTopInterval
	} else if opts.Interval > MaxTopInterval {
		opts.Interval = MaxTopInterval
	}

	// Non-interactive / single-shot mode
	if opts.Once {
		w := opts.Width
		if w <= 0 {
			w = terminalWidth(opts.Stdout)
		}
		frame := renderCurrentSnapshot(ctx, opts.TargetAccount, opts.Interval, w, false)
		fmt.Fprint(opts.Stdout, frame)
		return 0
	}

	// Check if stdin is a terminal for raw mode
	var restoreTerm func()
	if f, ok := opts.Stdin.(*os.File); ok {
		termFd := int(f.Fd())
		if term.IsTerminal(termFd) {
			oldState, err := term.MakeRaw(termFd)
			if err == nil {
				restoreTerm = func() {
					_ = term.Restore(termFd, oldState)
				}
			}
		}
	}

	// Safe cleanup guarantees terminal state and cursor are always restored
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			if restoreTerm != nil {
				restoreTerm()
			}
			fmt.Fprint(opts.Stdout, "\033[?25h\033[0m\n")
		})
	}
	defer cleanup()

	// Panic safety
	defer func() {
		if r := recover(); r != nil {
			cleanup()
			panic(r)
		}
	}()

	// Hide cursor on start
	fmt.Fprint(opts.Stdout, "\033[?25l")

	sigChan := make(chan os.Signal, 4)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	notifyResize(sigChan)
	defer signal.Stop(sigChan)

	keyChan := make(chan rune, 16)
	readCtx, cancelRead := context.WithCancel(context.Background())
	defer cancelRead()

	go func() {
		buf := make([]byte, 16)
		for {
			select {
			case <-readCtx.Done():
				return
			default:
			}
			n, err := opts.Stdin.Read(buf)
			if err != nil {
				close(keyChan)
				return
			}
			for i := 0; i < n; i++ {
				keyChan <- rune(buf[i])
			}
		}
	}()

	interval := opts.Interval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	refreshBanner := false
	refreshClearTimer := time.NewTimer(time.Hour) // inactive until triggered
	refreshClearTimer.Stop()

	redraw := func() {
		w := opts.Width
		if w <= 0 {
			w = terminalWidth(opts.Stdout)
		}
		frame := renderCurrentSnapshot(ctx, opts.TargetAccount, interval, w, refreshBanner)
		var buf bytes.Buffer
		buf.WriteString("\033[H")
		buf.WriteString(frame)
		buf.WriteString("\033[J")
		_, _ = opts.Stdout.Write(buf.Bytes())
	}

	// Initial render
	redraw()

	for {
		select {
		case sig := <-sigChan:
			if sig == syscall.SIGINT || sig == syscall.SIGTERM {
				return 0
			}
			// SIGWINCH: immediate resize redraw
			redraw()

		case <-ticker.C:
			redraw()

		case <-refreshClearTimer.C:
			refreshBanner = false
			redraw()

		case key, ok := <-keyChan:
			if !ok {
				// EOF on stdin
				return 0
			}
			switch key {
			case 'q', 'Q', 0x03: // 0x03 is Ctrl-C in raw mode
				return 0

			case '+', '=':
				if interval > MinTopInterval {
					interval -= 500 * time.Millisecond
					if interval < MinTopInterval {
						interval = MinTopInterval
					}
					ticker.Reset(interval)
					redraw()
				}

			case '-', '_':
				if interval < MaxTopInterval {
					interval += 500 * time.Millisecond
					if interval > MaxTopInterval {
						interval = MaxTopInterval
					}
					ticker.Reset(interval)
					redraw()
				}

			case 'r', 'R':
				pool, err := storage.LoadPool()
				if err == nil && pool != nil {
					for _, acc := range pool.Accounts {
						if acc != nil {
							_ = quota.ScheduleQuotaRefresh(acc)
						}
					}
				}
				refreshBanner = true
				refreshClearTimer.Reset(2 * time.Second)
				redraw()
			}
		}
	}
}

func renderCurrentSnapshot(
	ctx *daemon.InstanceContext,
	targetAccount string,
	interval time.Duration,
	width int,
	refreshBanner bool,
) string {
	status, info, _ := daemon.CheckInstanceStatus(ctx)
	daemonRunning := (status == daemon.StatusRunningSameInstance || status == daemon.StatusOutdatedBinary)

	var stats *daemon.RuntimeStats
	if daemonRunning && info != nil && info.PID > 0 {
		s, err := ctx.GetRuntimeStats(info.PID)
		if err == nil {
			stats = s
		}
	}

	pool, _ := storage.LoadPool()
	return RenderTopFrame(pool, stats, info, daemonRunning, targetAccount, interval, width, refreshBanner)
}

// RenderTopFrame formats a complete, responsive dashboard frame as a string.
// Every rendered non-control line is guaranteed to have VisibleWidth(line) <= width.
func RenderTopFrame(
	pool *storage.Pool,
	stats *daemon.RuntimeStats,
	info *daemon.DaemonInfo,
	daemonRunning bool,
	targetAccount string,
	interval time.Duration,
	width int,
	refreshBanner bool,
) string {
	if width <= 0 {
		width = 100
	}

	var sb strings.Builder
	nowTS := time.Now().Unix()

	// 1. Header
	renderHeader(&sb, daemonRunning, info, pool, interval, width)

	// 2. Accounts List
	if pool == nil || len(pool.Accounts) == 0 {
		sb.WriteString(fmt.Sprintf("\n%sNo accounts in pool yet.%s\n", clrYellow, clrReset))
		sb.WriteString(fmt.Sprintf("Run %sagy-pool login%s or %sagy-pool import-current%s to add accounts.\n\n",
			clrBold, clrReset, clrBold, clrReset))
	} else {
		accList := filterAccounts(pool.Accounts, targetAccount)
		if len(accList) == 0 {
			sb.WriteString(fmt.Sprintf("\n%s[Error] The selected account was not found.%s\n\n", clrRed, clrReset))
		} else {
			if width >= 96 {
				renderAccountsWide(&sb, accList, pool, stats, nowTS, width)
			} else if width >= 60 {
				renderAccountsCompact(&sb, accList, pool, stats, nowTS, width)
			} else if width >= 40 {
				renderAccountsMobile(&sb, accList, pool, stats, nowTS, width)
			} else {
				renderAccountsVeryNarrow(&sb, accList, pool, stats, nowTS, width)
			}
		}
	}

	// 3. Runtime Counters
	renderRuntimeCounters(&sb, stats, daemonRunning, width)

	// 4. Footer
	renderFooter(&sb, interval, width, refreshBanner)

	// Final guarantee: every non-control visible line fits within requested terminal width
	lines := strings.Split(sb.String(), "\n")
	var boundedSb strings.Builder
	for i, line := range lines {
		if VisibleWidth(line) > width {
			line = TruncateVisible(line, width)
		}
		boundedSb.WriteString(line)
		if i < len(lines)-1 {
			boundedSb.WriteByte('\n')
		}
	}

	return boundedSb.String()
}

func filterAccounts(all []*storage.Account, target string) []*storage.Account {
	if target == "" {
		return all
	}
	if idx, err := strconv.Atoi(target); err == nil {
		if idx >= 1 && idx <= len(all) {
			return []*storage.Account{all[idx-1]}
		}
		return nil
	}
	for _, a := range all {
		if a != nil && (a.ID == target || a.Email == target || a.Name == target) {
			return []*storage.Account{a}
		}
	}
	return nil
}

func getInFlight(acc *storage.Account, stats *daemon.RuntimeStats) int64 {
	if acc == nil {
		return 0
	}
	if stats != nil && stats.InFlight != nil {
		if cnt, ok := stats.InFlight[acc.ID]; ok {
			return cnt
		}
	}
	return observability.GetInFlightGeneration(acc.ID)
}

func renderHeader(
	sb *strings.Builder,
	daemonRunning bool,
	info *daemon.DaemonInfo,
	pool *storage.Pool,
	interval time.Duration,
	width int,
) {
	gatewayStr := fmt.Sprintf("%sSTOPPED%s", clrDim, clrReset)
	if daemonRunning {
		pidStr := ""
		if info != nil && info.PID > 0 {
			pidStr = fmt.Sprintf(" (PID %d)", info.PID)
		}
		gatewayStr = fmt.Sprintf("%sRUNNING%s%s", clrGreen, pidStr, clrReset)
	}

	strat := config.StrategyMaxQuota
	if pool != nil && pool.Strategy != "" {
		strat = pool.Strategy
	}

	rateSec := float64(interval) / float64(time.Second)

	if width >= 96 {
		sep := strings.Repeat("=", 68)
		sb.WriteString(sep + "\n")
		title := fmt.Sprintf("Antigravity Multi-Account Pool v%s", config.Version)
		pad := (68 + len(title)) / 2
		sb.WriteString(fmt.Sprintf("%s%*s%s\n", clrBold, pad, title, clrReset))
		sb.WriteString(sep + "\n")
		sb.WriteString(fmt.Sprintf("Gateway: %s · Strategy: %s%s%s · Refresh: %.1fs\n",
			gatewayStr, clrBold, strat, clrReset, rateSec))
	} else if width >= 60 {
		headerLine := fmt.Sprintf("agy-pool v%s · Gateway: %s · %s · %.1fs",
			config.Version, gatewayStr, strat, rateSec)
		if VisibleWidth(headerLine) <= width {
			sb.WriteString(fmt.Sprintf("\n%s\n", headerLine))
		} else {
			sb.WriteString(fmt.Sprintf("\nagy-pool v%s · Gateway: %s\n", config.Version, gatewayStr))
			sb.WriteString(fmt.Sprintf("Strategy: %s · Refresh: %.1fs\n", strat, rateSec))
		}
	} else if width >= 40 {
		line1 := fmt.Sprintf("agy-pool v%s · %s", config.Version, gatewayStr)
		if VisibleWidth(line1) <= width {
			sb.WriteString(fmt.Sprintf("\n%s\n", line1))
		} else {
			sb.WriteString(fmt.Sprintf("\nagy-pool v%s\n%s\n", config.Version, gatewayStr))
		}
		line2 := fmt.Sprintf("Strategy: %s · Refresh: %.1fs", strat, rateSec)
		if VisibleWidth(line2) <= width {
			sb.WriteString(fmt.Sprintf("%s\n", line2))
		} else {
			sb.WriteString(fmt.Sprintf("Strategy: %s\nRefresh: %.1fs\n", strat, rateSec))
		}
	} else {
		// width < 40 (e.g. width = 35): split into clean multi-line header
		statusLine := "agy-pool · STOPPED"
		if daemonRunning {
			statusLine = fmt.Sprintf("agy-pool · %sRUNNING%s", clrGreen, clrReset)
		}
		sb.WriteString(fmt.Sprintf("\n%s\n", statusLine))

		if daemonRunning && info != nil && info.PID > 0 {
			sb.WriteString(fmt.Sprintf("PID %d · %.1fs\n", info.PID, rateSec))
		} else {
			sb.WriteString(fmt.Sprintf("Refresh: %.1fs\n", rateSec))
		}

		stratLine := fmt.Sprintf("Strategy: %s", strat)
		if VisibleWidth(stratLine) > width {
			stratLine = strat
		}
		if VisibleWidth(stratLine) > width {
			stratLine = TruncateVisible(stratLine, width)
		}
		sb.WriteString(fmt.Sprintf("%s\n", stratLine))
	}
}

func renderAccountsWide(
	sb *strings.Builder,
	accList []*storage.Account,
	pool *storage.Pool,
	stats *daemon.RuntimeStats,
	nowTS int64,
	width int,
) {
	targetCol := 64
	if width > 68 {
		targetCol = 64
	}

	sb.WriteString("\n")
	for i, acc := range accList {
		if acc == nil {
			continue
		}

		dispName := accounts.DisplayAccountName(acc)
		state, stateClr := getAccountState(acc, pool, nowTS)
		hits := acc.GetHits()
		inFlight := getInFlight(acc, stats)

		right := fmt.Sprintf("in-flight %d · hits %d", inFlight, hits)
		rightLen := VisibleWidth(right)

		suffix := " · " + state
		suffixLen := VisibleWidth(suffix)

		maxLeft := targetCol - 2 - rightLen - 2
		if VisibleWidth(dispName)+suffixLen > maxLeft {
			avail := maxLeft - suffixLen
			if avail > 3 {
				dispName = TruncateVisible(dispName, avail)
			}
		}

		leftVisible := 2 + VisibleWidth(dispName) + suffixLen
		spaces := targetCol - leftVisible - rightLen
		if spaces < 2 {
			spaces = 2
		}

		sb.WriteString(fmt.Sprintf("  %s%s%s · %s%s%s%s%s\n",
			clrBold, dispName, clrReset,
			stateClr, state, clrReset,
			strings.Repeat(" ", spaces),
			right))

		renderAccountDetailsWide(sb, acc, nowTS, i)

		if i < len(accList)-1 {
			sb.WriteString("\n")
		}
	}
}

func renderAccountDetailsWide(sb *strings.Builder, acc *storage.Account, nowTS int64, idx int) {
	status := acc.Status
	if status == "validation_required" {
		sb.WriteString(fmt.Sprintf("  %sAction Required (Verify needed)%s\n", clrYellow, clrReset))
		sb.WriteString(fmt.Sprintf("  Run: agy-pool verify %d\n", idx+1))
		return
	}
	if status == "auth_error" {
		sb.WriteString(fmt.Sprintf("  %sAuth Failure (Re-authenticate)%s\n", clrRed, clrReset))
		return
	}

	q := acc.LastQuota
	q5Frac, gwFrac := quota.DisplayQuotaFractions(q)
	reset5, reset7 := extractResetTimes(acc, q, nowTS)

	if q5Frac == nil {
		sb.WriteString("  5H -- unknown\n")
	} else {
		pctVal := int(math.Round(*q5Frac * 100.0))
		g5Pct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
		g5Bar := quota.RenderBlockProgressBar(q5Frac, 20)
		g5Reset := quota.FormatCompactRemainingTime(reset5)
		r5Str := "resets in " + g5Reset
		if g5Reset == "ready" || g5Reset == "N/A" {
			r5Str = g5Reset
		}
		sb.WriteString(fmt.Sprintf("  5H  %s %s   %s\n", g5Bar, g5Pct, r5Str))
	}

	if gwFrac == nil {
		sb.WriteString("  WK -- unknown\n")
	} else {
		pctVal := int(math.Round(*gwFrac * 100.0))
		gwPct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
		gwBar := quota.RenderBlockProgressBar(gwFrac, 20)
		gwReset := quota.FormatCompactRemainingTime(reset7)
		r7Str := "resets in " + gwReset
		if gwReset == "ready" || gwReset == "N/A" {
			r7Str = gwReset
		}
		sb.WriteString(fmt.Sprintf("  WK  %s %s   %s\n", gwBar, gwPct, r7Str))
	}

	if q != nil && q.ThirdParty5H != nil && q.ThirdParty5H.Fraction != nil && *q.ThirdParty5H.Fraction < 1.0 {
		pctVal := int(math.Round(*q.ThirdParty5H.Fraction * 100.0))
		tpPct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
		tpBar := quota.RenderBlockProgressBar(q.ThirdParty5H.Fraction, 20)
		tpReset := quota.FormatCompactRemainingTime(q.ThirdParty5H.ResetTime)
		rTpStr := "resets in " + tpReset
		if tpReset == "ready" || tpReset == "N/A" {
			rTpStr = tpReset
		}
		sb.WriteString(fmt.Sprintf("  3P  %s %s   %s\n", tpBar, tpPct, rTpStr))
	}

	ageStr := quota.FormatQuotaAge(acc, float64(nowTS))
	sb.WriteString(fmt.Sprintf("  quota age %s\n", ageStr))
}

func renderAccountsCompact(
	sb *strings.Builder,
	accList []*storage.Account,
	pool *storage.Pool,
	stats *daemon.RuntimeStats,
	nowTS int64,
	width int,
) {
	sb.WriteString("\n")
	for i, acc := range accList {
		if acc == nil {
			continue
		}

		dispName := accounts.DisplayAccountName(acc)
		state, stateClr := getAccountState(acc, pool, nowTS)
		hits := acc.GetHits()
		inFlight := getInFlight(acc, stats)

		suffix := fmt.Sprintf(" · %s · in-flight %d · hits %d", state, inFlight, hits)
		suffixLen := VisibleWidth(suffix)
		if 2+VisibleWidth(dispName)+suffixLen > width {
			avail := width - 2 - suffixLen
			if avail > 3 {
				dispName = TruncateVisible(dispName, avail)
			}
		}

		sb.WriteString(fmt.Sprintf("  %s%s%s · %s%s%s · in-flight %d · hits %d\n",
			clrBold, dispName, clrReset, stateClr, state, clrReset, inFlight, hits))

		renderAccountDetailsCompact(sb, acc, nowTS, width, i)

		if i < len(accList)-1 {
			sb.WriteString("\n")
		}
	}
}

func renderAccountDetailsCompact(sb *strings.Builder, acc *storage.Account, nowTS int64, width int, idx int) {
	status := acc.Status
	if status == "validation_required" {
		sb.WriteString(fmt.Sprintf("  %sAction Required (Verify needed)%s\n", clrYellow, clrReset))
		sb.WriteString(fmt.Sprintf("  Run: agy-pool verify %d\n", idx+1))
		return
	}
	if status == "auth_error" {
		sb.WriteString(fmt.Sprintf("  %sAuth Failure (Re-authenticate)%s\n", clrRed, clrReset))
		return
	}

	q := acc.LastQuota
	q5Frac, gwFrac := quota.DisplayQuotaFractions(q)
	reset5, reset7 := extractResetTimes(acc, q, nowTS)

	if q5Frac == nil {
		sb.WriteString("  5H -- unknown\n")
	} else {
		pctVal := int(math.Round(*q5Frac * 100.0))
		g5Pct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
		g5Reset := quota.FormatCompactRemainingTime(reset5)
		r5Str := "reset " + g5Reset
		if g5Reset == "ready" || g5Reset == "N/A" {
			r5Str = g5Reset
		}
		nonBar5 := 12 + len(r5Str)
		barW5 := chooseBarWidth(width, nonBar5, 10, 4)
		if barW5 > 0 {
			g5Bar := quota.RenderBlockProgressBar(q5Frac, barW5)
			sb.WriteString(fmt.Sprintf("  5H %s %s  %s\n", g5Pct, g5Bar, r5Str))
		} else {
			sb.WriteString(fmt.Sprintf("  5H %s  %s\n", g5Pct, r5Str))
		}
	}

	if gwFrac == nil {
		sb.WriteString("  WK -- unknown\n")
	} else {
		pctVal := int(math.Round(*gwFrac * 100.0))
		gwPct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
		gwReset := quota.FormatCompactRemainingTime(reset7)
		r7Str := "reset " + gwReset
		if gwReset == "ready" || gwReset == "N/A" {
			r7Str = gwReset
		}
		nonBar7 := 12 + len(r7Str)
		barW7 := chooseBarWidth(width, nonBar7, 10, 4)
		if barW7 > 0 {
			gwBar := quota.RenderBlockProgressBar(gwFrac, barW7)
			sb.WriteString(fmt.Sprintf("  WK %s %s  %s\n", gwPct, gwBar, r7Str))
		} else {
			sb.WriteString(fmt.Sprintf("  WK %s  %s\n", gwPct, r7Str))
		}
	}

	if q != nil && q.ThirdParty5H != nil && q.ThirdParty5H.Fraction != nil && *q.ThirdParty5H.Fraction < 1.0 {
		pctVal := int(math.Round(*q.ThirdParty5H.Fraction * 100.0))
		tpPct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
		tpReset := quota.FormatCompactRemainingTime(q.ThirdParty5H.ResetTime)
		rTpStr := "reset " + tpReset
		if tpReset == "ready" || tpReset == "N/A" {
			rTpStr = tpReset
		}
		nonBarTp := 12 + len(rTpStr)
		barWTp := chooseBarWidth(width, nonBarTp, 10, 4)
		if barWTp > 0 {
			tpBar := quota.RenderBlockProgressBar(q.ThirdParty5H.Fraction, barWTp)
			sb.WriteString(fmt.Sprintf("  3P %s %s  %s\n", tpPct, tpBar, rTpStr))
		} else {
			sb.WriteString(fmt.Sprintf("  3P %s  %s\n", tpPct, rTpStr))
		}
	}

	ageStr := quota.FormatQuotaAge(acc, float64(nowTS))
	sb.WriteString(fmt.Sprintf("  age %s\n", ageStr))
}

func renderAccountsMobile(
	sb *strings.Builder,
	accList []*storage.Account,
	pool *storage.Pool,
	stats *daemon.RuntimeStats,
	nowTS int64,
	width int,
) {
	sb.WriteString("\n")
	for i, acc := range accList {
		if acc == nil {
			continue
		}

		dispName := accounts.DisplayAccountName(acc)
		state, stateClr := getAccountState(acc, pool, nowTS)
		hits := acc.GetHits()
		inFlight := getInFlight(acc, stats)

		suffix := " · " + state
		suffixLen := VisibleWidth(suffix)
		if VisibleWidth(dispName)+suffixLen > width {
			avail := width - suffixLen
			if avail > 3 {
				dispName = TruncateVisible(dispName, avail)
			}
		}

		sb.WriteString(fmt.Sprintf("%s%s%s · %s%s%s\n", clrBold, dispName, clrReset, stateClr, state, clrReset))

		status := acc.Status
		if status == "validation_required" {
			sb.WriteString(fmt.Sprintf("%sAction Required (Verify needed)%s\n", clrYellow, clrReset))
			sb.WriteString(fmt.Sprintf("Run: agy-pool verify %d\n", i+1))
		} else if status == "auth_error" {
			sb.WriteString(fmt.Sprintf("%sAuth Failure (Re-authenticate)%s\n", clrRed, clrReset))
		} else {
			q := acc.LastQuota
			q5Frac, gwFrac := quota.DisplayQuotaFractions(q)
			reset5, reset7 := extractResetTimes(acc, q, nowTS)

			if q5Frac == nil {
				sb.WriteString("5H -- unknown\n")
			} else {
				pctVal := int(math.Round(*q5Frac * 100.0))
				g5Pct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
				g5Reset := quota.FormatCompactRemainingTime(reset5)
				nonBar5 := 10 + len(g5Reset)
				barW5 := chooseBarWidth(width, nonBar5, 10, 4)
				if barW5 > 0 {
					g5Bar := quota.RenderBlockProgressBar(q5Frac, barW5)
					sb.WriteString(fmt.Sprintf("5H %s %s  %s\n", g5Pct, g5Bar, g5Reset))
				} else {
					sb.WriteString(fmt.Sprintf("5H %s  %s\n", g5Pct, g5Reset))
				}
			}

			if gwFrac == nil {
				sb.WriteString("WK -- unknown\n")
			} else {
				pctVal := int(math.Round(*gwFrac * 100.0))
				gwPct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
				gwReset := quota.FormatCompactRemainingTime(reset7)
				nonBar7 := 10 + len(gwReset)
				barW7 := chooseBarWidth(width, nonBar7, 10, 4)
				if barW7 > 0 {
					gwBar := quota.RenderBlockProgressBar(gwFrac, barW7)
					sb.WriteString(fmt.Sprintf("WK %s %s  %s\n", gwPct, gwBar, gwReset))
				} else {
					sb.WriteString(fmt.Sprintf("WK %s  %s\n", gwPct, gwReset))
				}
			}

			ageStr := quota.FormatShortQuotaAge(acc, float64(nowTS))
			sb.WriteString(fmt.Sprintf("age %s · in-flight %d · hits %d\n", ageStr, inFlight, hits))
		}

		if i < len(accList)-1 {
			sb.WriteString("\n")
		}
	}
}

func renderAccountsVeryNarrow(
	sb *strings.Builder,
	accList []*storage.Account,
	pool *storage.Pool,
	stats *daemon.RuntimeStats,
	nowTS int64,
	width int,
) {
	sb.WriteString("\n")
	for i, acc := range accList {
		if acc == nil {
			continue
		}

		dispName := accounts.DisplayAccountName(acc)
		state, stateClr := getAccountState(acc, pool, nowTS)
		hits := acc.GetHits()
		inFlight := getInFlight(acc, stats)

		headerLine := fmt.Sprintf("%s%s%s · %s%s%s", clrBold, dispName, clrReset, stateClr, state, clrReset)
		if VisibleWidth(headerLine) <= width {
			sb.WriteString(headerLine + "\n")
		} else {
			// Split dispName and state onto separate lines to avoid truncating important state
			if VisibleWidth(dispName) > width {
				dispName = TruncateVisible(dispName, width)
			}
			sb.WriteString(fmt.Sprintf("%s%s%s\n", clrBold, dispName, clrReset))
			if VisibleWidth(state) > width {
				state = TruncateVisible(state, width)
			}
			sb.WriteString(fmt.Sprintf("%s%s%s\n", stateClr, state, clrReset))
		}

		status := acc.Status
		if status == "validation_required" {
			sb.WriteString(fmt.Sprintf("%sVerify Needed%s\n", clrYellow, clrReset))
			sb.WriteString(fmt.Sprintf("agy-pool verify %d\n", i+1))
		} else if status == "auth_error" {
			sb.WriteString(fmt.Sprintf("%sAuth Error%s\n", clrRed, clrReset))
		} else {
			q := acc.LastQuota
			q5Frac, gwFrac := quota.DisplayQuotaFractions(q)
			reset5, reset7 := extractResetTimes(acc, q, nowTS)

			if q5Frac == nil {
				sb.WriteString("5H -- unknown\n")
			} else {
				pctVal := int(math.Round(*q5Frac * 100.0))
				g5Pct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
				g5Reset := quota.FormatCompactRemainingTime(reset5)
				line5 := fmt.Sprintf("5H %s  %s", g5Pct, g5Reset)
				if VisibleWidth(line5) > width {
					line5 = TruncateVisible(line5, width)
				}
				sb.WriteString(line5 + "\n")
			}

			if gwFrac == nil {
				sb.WriteString("WK -- unknown\n")
			} else {
				pctVal := int(math.Round(*gwFrac * 100.0))
				gwPct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
				gwReset := quota.FormatCompactRemainingTime(reset7)
				lineWk := fmt.Sprintf("WK %s  %s", gwPct, gwReset)
				if VisibleWidth(lineWk) > width {
					lineWk = TruncateVisible(lineWk, width)
				}
				sb.WriteString(lineWk + "\n")
			}

			if q != nil && q.ThirdParty5H != nil && q.ThirdParty5H.Fraction != nil && *q.ThirdParty5H.Fraction < 1.0 {
				pctVal := int(math.Round(*q.ThirdParty5H.Fraction * 100.0))
				tpPct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
				tpReset := quota.FormatCompactRemainingTime(q.ThirdParty5H.ResetTime)
				lineTp := fmt.Sprintf("3P %s  %s", tpPct, tpReset)
				if VisibleWidth(lineTp) > width {
					lineTp = TruncateVisible(lineTp, width)
				}
				sb.WriteString(lineTp + "\n")
			}

			ageStr := quota.FormatShortQuotaAge(acc, float64(nowTS))
			infoLine := fmt.Sprintf("age %s · in-flight %d · hits %d", ageStr, inFlight, hits)
			if VisibleWidth(infoLine) <= width {
				sb.WriteString(infoLine + "\n")
			} else {
				// Split into two lines rather than truncating counters
				line1 := fmt.Sprintf("age %s · in-flight %d", ageStr, inFlight)
				line2 := fmt.Sprintf("hits %d", hits)
				if VisibleWidth(line1) > width {
					line1 = TruncateVisible(line1, width)
				}
				if VisibleWidth(line2) > width {
					line2 = TruncateVisible(line2, width)
				}
				sb.WriteString(line1 + "\n")
				sb.WriteString(line2 + "\n")
			}
		}

		if i < len(accList)-1 {
			sb.WriteString("\n")
		}
	}
}

func renderRuntimeCounters(
	sb *strings.Builder,
	stats *daemon.RuntimeStats,
	daemonRunning bool,
	width int,
) {
	if !daemonRunning {
		sb.WriteString(fmt.Sprintf("\n  %sRuntime Counters%s\n", clrBold, clrReset))
		sb.WriteString("    (gateway not running)\n")
		return
	}
	if stats == nil {
		sb.WriteString(fmt.Sprintf("\n  %sRuntime Counters%s\n", clrBold, clrReset))
		sb.WriteString("    (metrics unavailable)\n")
		return
	}

	if width >= 60 {
		sb.WriteString(fmt.Sprintf("\n  %sRuntime Counters%s\n", clrBold, clrReset))
		sb.WriteString(fmt.Sprintf("    %-18s %6s requests / %s success\n", "Generation",
			observability.FormatNumber(stats.GenerationRequests), observability.FormatNumber(stats.GenerationSuccess)))
		sb.WriteString(fmt.Sprintf("    %-18s %6s decisions\n", "Routing",
			observability.FormatNumber(stats.RoutingDecisions)))
		sb.WriteString(fmt.Sprintf("    %-18s %6s attempts / %s recovered\n", "Failover",
			observability.FormatNumber(stats.FailoverAttempts), observability.FormatNumber(stats.FailoverSuccess)))
		sb.WriteString(fmt.Sprintf("    %-18s %6s\n", "Restricted skips",
			observability.FormatNumber(stats.RestrictedSkips)))
		sb.WriteString(fmt.Sprintf("    %-18s %6s / %s ok / %s failed\n", "Quota refresh",
			observability.FormatNumber(stats.QuotaRefreshAttempts), observability.FormatNumber(stats.QuotaRefreshSuccess), observability.FormatNumber(stats.QuotaRefreshFailure)))
		sb.WriteString(fmt.Sprintf("    %-18s %6s / %s ok / %s failed\n", "Auth refresh",
			observability.FormatNumber(stats.AuthRefreshAttempts), observability.FormatNumber(stats.AuthRefreshSuccess), observability.FormatNumber(stats.AuthRefreshFailure)))
		sb.WriteString(fmt.Sprintf("    %-18s %6s\n", "Persistence errors",
			observability.FormatNumber(stats.PersistenceErrors)))
		sb.WriteString(fmt.Sprintf("    %-18s %6s\n", "Replay prevented",
			observability.FormatNumber(stats.NoReplayPrevented)))
	} else if width >= 40 {
		sb.WriteString(fmt.Sprintf("\n%sRuntime Counters%s\n", clrBold, clrReset))
		sb.WriteString(fmt.Sprintf("Generation : %s req / %s ok\n",
			observability.FormatNumber(stats.GenerationRequests), observability.FormatNumber(stats.GenerationSuccess)))
		sb.WriteString(fmt.Sprintf("Routing    : %s decisions\n",
			observability.FormatNumber(stats.RoutingDecisions)))
		sb.WriteString(fmt.Sprintf("Failover   : %s att / %s rec\n",
			observability.FormatNumber(stats.FailoverAttempts), observability.FormatNumber(stats.FailoverSuccess)))
		sb.WriteString(fmt.Sprintf("Restricted : %s skips\n",
			observability.FormatNumber(stats.RestrictedSkips)))
		sb.WriteString(fmt.Sprintf("Quota ref  : %s / %s ok / %s err\n",
			observability.FormatNumber(stats.QuotaRefreshAttempts), observability.FormatNumber(stats.QuotaRefreshSuccess), observability.FormatNumber(stats.QuotaRefreshFailure)))
		sb.WriteString(fmt.Sprintf("Auth ref   : %s / %s ok / %s err\n",
			observability.FormatNumber(stats.AuthRefreshAttempts), observability.FormatNumber(stats.AuthRefreshSuccess), observability.FormatNumber(stats.AuthRefreshFailure)))
		sb.WriteString(fmt.Sprintf("Persist err: %s\n",
			observability.FormatNumber(stats.PersistenceErrors)))
		sb.WriteString(fmt.Sprintf("No-replay  : %s\n",
			observability.FormatNumber(stats.NoReplayPrevented)))
	} else {
		// width < 40 (e.g. width = 35)
		sb.WriteString(fmt.Sprintf("\n%sRuntime Counters%s\n", clrBold, clrReset))
		sb.WriteString(fmt.Sprintf("Generation : %s / %s ok\n",
			observability.FormatNumber(stats.GenerationRequests), observability.FormatNumber(stats.GenerationSuccess)))
		sb.WriteString(fmt.Sprintf("Routing    : %s\n",
			observability.FormatNumber(stats.RoutingDecisions)))
		sb.WriteString(fmt.Sprintf("Failover   : %s / %s rec\n",
			observability.FormatNumber(stats.FailoverAttempts), observability.FormatNumber(stats.FailoverSuccess)))
		sb.WriteString(fmt.Sprintf("Restricted : %s\n",
			observability.FormatNumber(stats.RestrictedSkips)))

		qLine := fmt.Sprintf("Quota ref  : %s ok / %s err",
			observability.FormatNumber(stats.QuotaRefreshSuccess), observability.FormatNumber(stats.QuotaRefreshFailure))
		if VisibleWidth(qLine) <= width {
			sb.WriteString(qLine + "\n")
		} else {
			sb.WriteString(fmt.Sprintf("Quota ok   : %s\n", observability.FormatNumber(stats.QuotaRefreshSuccess)))
			sb.WriteString(fmt.Sprintf("Quota err  : %s\n", observability.FormatNumber(stats.QuotaRefreshFailure)))
		}

		aLine := fmt.Sprintf("Auth ref   : %s ok / %s err",
			observability.FormatNumber(stats.AuthRefreshSuccess), observability.FormatNumber(stats.AuthRefreshFailure))
		if VisibleWidth(aLine) <= width {
			sb.WriteString(aLine + "\n")
		} else {
			sb.WriteString(fmt.Sprintf("Auth ok    : %s\n", observability.FormatNumber(stats.AuthRefreshSuccess)))
			sb.WriteString(fmt.Sprintf("Auth err   : %s\n", observability.FormatNumber(stats.AuthRefreshFailure)))
		}

		sb.WriteString(fmt.Sprintf("Persist err: %s\n",
			observability.FormatNumber(stats.PersistenceErrors)))
		sb.WriteString(fmt.Sprintf("No-replay  : %s\n",
			observability.FormatNumber(stats.NoReplayPrevented)))
	}
}

func renderFooter(sb *strings.Builder, interval time.Duration, width int, refreshBanner bool) {
	rateSec := float64(interval) / float64(time.Second)
	sb.WriteString("\n")
	if width >= 60 {
		footer := fmt.Sprintf("%s[q] Quit  [r] Refresh Quota  [+] Faster  [-] Slower  (%.1fs)%s",
			clrDim, rateSec, clrReset)
		sb.WriteString(footer + "\n")
	} else if width >= 40 {
		footer := fmt.Sprintf("%s[q]Quit [r]Ref [+]Fast [-]Slow (%.1fs)%s",
			clrDim, rateSec, clrReset)
		sb.WriteString(footer + "\n")
	} else {
		// width < 40 (e.g. width = 35): split shortcuts across two lines so they never wrap or exceed width
		sb.WriteString(fmt.Sprintf("%s[q] Quit  [r] Refresh%s\n", clrDim, clrReset))
		sb.WriteString(fmt.Sprintf("%s[+] Faster  [-] Slower  (%.1fs)%s\n", clrDim, rateSec, clrReset))
	}

	if refreshBanner {
		sb.WriteString(fmt.Sprintf("%sRefreshing quota...%s\n", clrYellow, clrReset))
	}
}
