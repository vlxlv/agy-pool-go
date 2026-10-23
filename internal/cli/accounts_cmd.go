package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
	"github.com/vlxlv/agy-pool-go/internal/forecast"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/scheduler"
	"github.com/vlxlv/agy-pool-go/internal/storage"
	"golang.org/x/term"
)

// terminalSize detects terminal column width and height using golang.org/x/term when stdout is a TTY,
// or returns 100, 24 as the deterministic non-TTY default (desktop layout).
func terminalSize(w io.Writer) (int, int) {
	if f, ok := w.(*os.File); ok {
		fd := int(f.Fd())
		if term.IsTerminal(fd) {
			if width, height, err := term.GetSize(fd); err == nil && width > 0 && height > 0 {
				return width, height
			}
		}
	}
	return 100, 24
}

// terminalWidth detects terminal column width using golang.org/x/term when stdout is a TTY,
// or returns 100 as the deterministic non-TTY default (desktop layout).
func terminalWidth(w io.Writer) int {
	width, _ := terminalSize(w)
	return width
}

// chooseBarWidth calculates the largest bar width (up to maxDesired) that fits within lineWidth budget.
// If available space is less than minUseful, it returns 0 (no bar).
func chooseBarWidth(lineWidth, nonBarLen, maxDesired, minUseful int) int {
	available := lineWidth - nonBarLen
	if available >= maxDesired {
		return maxDesired
	}
	if available >= minUseful {
		return available
	}
	return 0
}

func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes == 1 {
		return "…"
	}
	return string(runes[:maxRunes-1]) + "…"
}

// ListAccounts renders the account pool dashboard with responsive layout based on terminal width.
func ListAccounts(target string, stdout, stderr io.Writer, showEmail ...bool) {
	width := terminalWidth(stdout)
	ListAccountsWithWidth(target, width, stdout, stderr, showEmail...)
}

func getAccountState(acc *storage.Account, pool *storage.Pool, nowTS int64, inFlightOpt ...int64) (string, string) {
	var inFlight int64
	if len(inFlightOpt) > 0 {
		inFlight = inFlightOpt[0]
	}

	q := acc.LastQuota
	q5Frac, gwFrac := quota.DisplayQuotaFractions(q)
	isCooling := acc.RateLimitedUntil != nil && *acc.RateLimitedUntil > float64(nowTS)
	isExhausted := (q5Frac != nil && *q5Frac <= config.DepletedThreshold) ||
		(gwFrac != nil && *gwFrac <= config.DepletedThreshold)

	isActive := pool.ActiveAccountID != nil && acc.ID == *pool.ActiveAccountID

	status := acc.Status
	if status == storage.StatusDisabled {
		if isActive {
			return "CLI Base (Disabled)", clrDim
		}
		return "Disabled", clrDim
	}
	if status == storage.StatusValidationRequired {
		return "Verify Required", clrYellow
	}
	if status == storage.StatusVerificationPending {
		return "Verification Pending", clrCyan
	}
	if status == storage.StatusAuthError {
		return "Auth Error", clrRed
	}
	if isCooling {
		if isActive {
			return "CLI Base (Cooldown)", clrYellow
		}
		return "Cooldown", clrYellow
	}
	if isExhausted {
		if isActive {
			return "CLI Base (Exhausted)", clrRed
		}
		return "Exhausted", clrDim
	}
	if scheduler.IsReserveCandidate(acc, float64(nowTS)) {
		if isActive {
			return "CLI Base (Protected)", clrYellow
		}
		return "Protected", clrYellow
	}
	if isActive {
		return "CLI Base", clrGreen
	}
	if inFlight > 0 {
		return fmt.Sprintf("⚡ Running (%d)", inFlight), clrCyan
	}
	return "Ready", clrCyan
}

func extractResetTimes(acc *storage.Account, q *storage.QuotaState, nowTS int64) (any, any) {
	var legacyReset any
	if q != nil && q.Gemini5H == nil && q.GeminiWeekly == nil {
		legacyReset = q.ResetTime
	}
	var reset5, reset7 any
	if q != nil && q.Gemini5H != nil {
		reset5 = q.Gemini5H.ResetTime
	} else {
		reset5 = legacyReset
	}
	if q != nil && q.GeminiWeekly != nil {
		reset7 = q.GeminiWeekly.ResetTime
	} else {
		reset7 = legacyReset
	}
	if reset5 == nil && acc.Gemini5HResetSec != nil {
		reset5 = float64(nowTS) + *acc.Gemini5HResetSec
	}
	if reset7 == nil && acc.GeminiWeeklyResetSec != nil {
		reset7 = float64(nowTS) + *acc.GeminiWeeklyResetSec
	}
	return reset5, reset7
}

// ListAccountsWithWidth renders the account pool dashboard for a specific terminal width.
func ListAccountsWithWidth(target string, width int, stdout, stderr io.Writer, showEmailOpt ...bool) {
	showEmail := false
	if len(showEmailOpt) > 0 {
		showEmail = showEmailOpt[0]
	}

	if width <= 0 {
		width = 100
	}

	pool, err := storage.LoadPool()
	if err != nil || pool == nil || len(pool.Accounts) == 0 {
		fmt.Fprintf(stdout, "\n%sNo accounts in pool yet.%s\n", clrYellow, clrReset)
		fmt.Fprintf(stdout, "Run %sagy-pool login%s or %sagy-pool import-current%s to add accounts.\n\n",
			clrBold, clrReset, clrBold, clrReset)
		return
	}

	accList := pool.Accounts
	if target != "" {
		if idx, err := strconv.Atoi(target); err == nil {
			if idx >= 1 && idx <= len(accList) {
				accList = []*storage.Account{accList[idx-1]}
			} else {
				fmt.Fprintf(stdout, "%s[Error] The selected account was not found.%s\n", clrRed, clrReset)
				return
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
				return
			}
		}
	}

	daemonRunning := daemon.IsPortListening(config.DefaultPort)
	gatewayStatus := fmt.Sprintf("%sRUNNING (127.0.0.1:%d)%s", clrGreen, config.DefaultPort, clrReset)
	if !daemonRunning {
		gatewayStatus = fmt.Sprintf("%sSTOPPED%s", clrDim, clrReset)
	}

	nowTS := time.Now().Unix()

	if width >= 60 {
		renderWideQuotaDashboard(accList, pool, gatewayStatus, nowTS, width, showEmail, stdout)
		return
	}

	if width >= 40 {
		renderMobileQuotaDashboard(accList, pool, daemonRunning, nowTS, width, showEmail, stdout)
		return
	}

	renderVeryNarrowQuotaDashboard(accList, pool, nowTS, width, showEmail, stdout)
}

func resolveNextCandidateID(pool *storage.Pool, nowTS int64) string {
	if pool == nil || len(pool.Accounts) <= 1 {
		return ""
	}
	strat := pool.Strategy
	if strat == "" {
		strat = config.DefaultStrategy
	}
	ordered := scheduler.OrderCandidates(pool.Accounts, strat, pool, float64(nowTS))
	if len(ordered) > 0 && ordered[0] != nil && !scheduler.IsRestricted(ordered[0]) {
		return ordered[0].ID
	}
	return ""
}

func renderWideQuotaDashboard(accList []*storage.Account, pool *storage.Pool, gatewayStatus string, nowTS int64, width int, showEmail bool, stdout io.Writer) {
	borderWidth := 68
	if width < 68 {
		borderWidth = width
	}
	sep := strings.Repeat("=", borderWidth)
	subSep := strings.Repeat("-", borderWidth)

	fmt.Fprintf(stdout, "\n%s\n", sep)
	title := fmt.Sprintf("Antigravity Multi-Account Pool v%s", config.Version)
	fmt.Fprintf(stdout, "%s%*s%s\n", clrBold, (68+len(title))/2, title, clrReset)
	fmt.Fprintf(stdout, "%s\n", sep)

	nextID := resolveNextCandidateID(pool, nowTS)
	for i, acc := range accList {
		if acc == nil {
			continue
		}

		dispName := accounts.DisplayAccountName(acc)
		state, stateClr := getAccountState(acc, pool, nowTS)
		hits := acc.GetHits()
		ageStr := quota.FormatQuotaAge(acc, float64(nowTS))

		right := fmt.Sprintf("hits %d", hits)
		if acc.LastQuota != nil {
			if borderWidth >= 68 {
				right = fmt.Sprintf("hits %d · quota age %s", hits, ageStr)
			} else {
				right = fmt.Sprintf("hits %d · age %s", hits, ageStr)
			}
		}
		rightLen := len(right)
		targetCol := 64
		if borderWidth < 68 {
			targetCol = borderWidth - 4
		}

		nextTag := ""
		nextTagVisible := ""
		if nextID != "" && acc.ID == nextID {
			nextTag = fmt.Sprintf(" %s%s[Next ⚡]%s", clrBold, clrCyan, clrReset)
			nextTagVisible = " [Next ⚡]"
		}

		suffixLen := utf8.RuneCountInString(" · ") + utf8.RuneCountInString(state) + utf8.RuneCountInString(nextTagVisible)
		maxLeftLen := targetCol - 2 - rightLen - 2
		if utf8.RuneCountInString(dispName)+suffixLen > maxLeftLen {
			avail := maxLeftLen - suffixLen
			if avail > 3 {
				dispName = truncateRunes(dispName, avail)
			}
		}

		leftVisible := 2 + utf8.RuneCountInString(dispName) + suffixLen
		spaces := targetCol - leftVisible - rightLen
		if spaces < 2 {
			spaces = 2
		}

		fmt.Fprintf(stdout, "  %s%s%s · %s%s%s%s%s%s\n",
			clrBold, dispName, clrReset,
			stateClr, state, clrReset,
			nextTag,
			strings.Repeat(" ", spaces),
			right)

		if showEmail && acc.Email != "" {
			fmt.Fprintf(stdout, "  %sEmail: %s%s\n", clrDim, acc.Email, clrReset)
		}

		status := acc.Status
		if status == storage.StatusDisabled {
			fmt.Fprintf(stdout, "  %sDisabled (Excluded from pool routing)%s\n", clrDim, clrReset)
		} else if status == storage.StatusValidationRequired {
			fmt.Fprintf(stdout, "  %sAction Required (Verify needed)%s\n", clrYellow, clrReset)
			fmt.Fprintf(stdout, "  Run: agy-pool verify %d\n", i+1)
		} else if status == storage.StatusVerificationPending {
			fmt.Fprintf(stdout, "  %sWaiting for generation confirmation%s\n", clrCyan, clrReset)
		} else if status == storage.StatusAuthError {
			fmt.Fprintf(stdout, "  %sAuth Failure (Re-authenticate)%s\n", clrRed, clrReset)
		} else {
			if scheduler.IsReserveCandidate(acc, float64(nowTS)) {
				capState := quota.ComputeCapacityState(acc, float64(nowTS))
				reason := "Weekly quota low"
				if capState.Q5Known && capState.Q5 != nil && *capState.Q5 <= scheduler.FiveHourReserveFloor {
					reason = "5H quota <= 15%"
				} else if capState.Q7Known && capState.Q7 != nil {
					reason = fmt.Sprintf("Weekly quota <= %.1f%%", scheduler.DynamicWeeklyReserveFloor(capState.R7)*100)
				}
				if width >= 74 {
					fmt.Fprintf(stdout, "  %s[Reserve Protection: %s, held back from pool routing]%s\n", clrYellow, reason, clrReset)
				} else {
					fmt.Fprintf(stdout, "  %s[Reserve Protection: %s]%s\n", clrYellow, reason, clrReset)
				}
			}
			q := acc.LastQuota
			q5Frac, gwFrac := quota.DisplayQuotaFractions(q)
			reset5, reset7 := extractResetTimes(acc, q, nowTS)

			if q5Frac == nil {
				fmt.Fprintln(stdout, "  5H -- unknown")
			} else {
				pctVal := int(math.Round(*q5Frac * 100.0))
				g5Pct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
				g5Bar := quota.RenderBlockProgressBar(q5Frac, 20)
				g5Reset := quota.FormatCompactRemainingTime(reset5)
				r5Str := "resets in " + g5Reset
				if g5Reset == "ready" || g5Reset == "N/A" {
					r5Str = g5Reset
				}
				fmt.Fprintf(stdout, "  5H  %s %s   %s\n", g5Bar, g5Pct, r5Str)
			}

			if gwFrac == nil {
				fmt.Fprintln(stdout, "  "+quota.WeeklyUnknownLabel(acc))
			} else {
				pctVal := int(math.Round(*gwFrac * 100.0))
				gwPct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
				gwBar := quota.RenderBlockProgressBar(gwFrac, 20)
				gwReset := quota.FormatCompactRemainingTime(reset7)
				r7Str := "resets in " + gwReset
				if gwReset == "ready" || gwReset == "N/A" {
					r7Str = gwReset
				}
				fmt.Fprintf(stdout, "  WK  %s %s   %s\n", gwBar, gwPct, r7Str)
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
				fmt.Fprintf(stdout, "  3P  %s %s   %s\n", tpBar, tpPct, rTpStr)
			}
		}

		if i < len(accList)-1 {
			fmt.Fprintln(stdout)
		}
	}

	strat := pool.Strategy
	if strat == "" {
		strat = config.DefaultStrategy
	}

	fmt.Fprintf(stdout, "%s\n", subSep)
	fmt.Fprintf(stdout, " %s[CLI Base]%s %sBase account for CLI/auth metadata%s\n",
		clrGreen, clrReset, clrDim, clrReset)
	fmt.Fprintf(stdout, " %s[Ready]%s    %sAvailable in rotation pool%s\n",
		clrCyan, clrReset, clrDim, clrReset)
	fmt.Fprintf(stdout, " %s[Cooldown]%s %sRate Limited%s      %s[Exhausted] Quota Depleted%s\n",
		clrYellow, clrReset, clrDim, clrReset, clrDim, clrReset)
	if width >= 64 {
		fmt.Fprintf(stdout, " %s[Protected]%s %sLow Quota Reserve%s   %s[Disabled] Excluded from Pool%s\n",
			clrYellow, clrReset, clrDim, clrReset, clrDim, clrReset)
	} else {
		fmt.Fprintf(stdout, " %s[Protected]%s %sLow Quota Reserve%s  %s[Disabled] Excluded%s\n",
			clrYellow, clrReset, clrDim, clrReset, clrDim, clrReset)
	}
	if width >= 64 {
		fmt.Fprintf(stdout, " Strategy: %s%s%s | Gateway Proxy: %s\n",
			clrBold, strat, clrReset, gatewayStatus)
	} else {
		shortStatus := fmt.Sprintf("%sRUNNING (:8899)%s", clrGreen, clrReset)
		if !daemon.IsPortListening(config.DefaultPort) {
			shortStatus = fmt.Sprintf("%sSTOPPED%s", clrDim, clrReset)
		}
		fmt.Fprintf(stdout, " Strategy: %s%s%s | Proxy: %s\n",
			clrBold, strat, clrReset, shortStatus)
	}
	forecast.PrintRunwaySummary(stdout, pool, float64(nowTS), width)
	fmt.Fprintf(stdout, "%s\n\n", sep)
}

func renderCompactQuotaDashboard(accList []*storage.Account, pool *storage.Pool, nowTS int64, width int, showEmail bool, stdout io.Writer) {
	fmt.Fprintln(stdout)
	nextID := resolveNextCandidateID(pool, nowTS)
	for i, acc := range accList {
		if acc == nil {
			continue
		}

		dispName := accounts.DisplayAccountName(acc)
		state, stateClr := getAccountState(acc, pool, nowTS)
		hits := acc.GetHits()

		nextTag := ""
		nextTagVisible := ""
		if nextID != "" && acc.ID == nextID {
			nextTag = fmt.Sprintf(" %s%s[Next ⚡]%s", clrBold, clrCyan, clrReset)
			nextTagVisible = " [Next ⚡]"
		}

		suffix := fmt.Sprintf(" · %s%s · hits %d", state, nextTagVisible, hits)
		suffixLen := utf8.RuneCountInString(suffix)
		if 2+utf8.RuneCountInString(dispName)+suffixLen > width {
			avail := width - 2 - suffixLen
			if avail > 3 {
				dispName = truncateRunes(dispName, avail)
			}
		}

		fmt.Fprintf(stdout, "  %s%s%s · %s%s%s%s · hits %d\n",
			clrBold, dispName, clrReset, stateClr, state, clrReset, nextTag, hits)

		if showEmail && acc.Email != "" {
			fmt.Fprintf(stdout, "  %sEmail: %s%s\n", clrDim, acc.Email, clrReset)
		}

		status := acc.Status
		if status == storage.StatusDisabled {
			fmt.Fprintf(stdout, "  %sDisabled (Excluded from pool routing)%s\n", clrDim, clrReset)
		} else if status == storage.StatusValidationRequired {
			fmt.Fprintf(stdout, "  %sAction Required (Verify needed)%s\n", clrYellow, clrReset)
			fmt.Fprintf(stdout, "  Run: agy-pool verify %d\n", i+1)
		} else if status == storage.StatusVerificationPending {
			fmt.Fprintf(stdout, "  %sWaiting for generation confirmation%s\n", clrCyan, clrReset)
		} else if status == storage.StatusAuthError {
			fmt.Fprintf(stdout, "  %sAuth Failure (Re-authenticate)%s\n", clrRed, clrReset)
		} else {
			if scheduler.IsReserveCandidate(acc, float64(nowTS)) {
				fmt.Fprintf(stdout, "  %s[Reserve Protection: Low Quota]%s\n", clrYellow, clrReset)
			}
			q := acc.LastQuota
			q5Frac, gwFrac := quota.DisplayQuotaFractions(q)
			reset5, reset7 := extractResetTimes(acc, q, nowTS)

			if q5Frac == nil {
				fmt.Fprintln(stdout, "  5H -- unknown")
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
					fmt.Fprintf(stdout, "  5H %s %s  %s\n", g5Pct, g5Bar, r5Str)
				} else {
					fmt.Fprintf(stdout, "  5H %s  %s\n", g5Pct, r5Str)
				}
			}

			if gwFrac == nil {
				fmt.Fprintln(stdout, "  "+quota.WeeklyUnknownLabel(acc))
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
					fmt.Fprintf(stdout, "  WK %s %s  %s\n", gwPct, gwBar, r7Str)
				} else {
					fmt.Fprintf(stdout, "  WK %s  %s\n", gwPct, r7Str)
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
					fmt.Fprintf(stdout, "  3P %s %s  %s\n", tpPct, tpBar, rTpStr)
				} else {
					fmt.Fprintf(stdout, "  3P %s  %s\n", tpPct, rTpStr)
				}
			}

			ageStr := quota.FormatQuotaAge(acc, float64(nowTS))
			fmt.Fprintf(stdout, "  age %s\n", ageStr)
		}

		if i < len(accList)-1 {
			fmt.Fprintln(stdout)
		}
	}
	fmt.Fprintln(stdout)
}

func renderMobileQuotaDashboard(accList []*storage.Account, pool *storage.Pool, daemonRunning bool, nowTS int64, width int, showEmail bool, stdout io.Writer) {
	fmt.Fprintln(stdout)
	nextID := resolveNextCandidateID(pool, nowTS)
	for i, acc := range accList {
		if acc == nil {
			continue
		}

		dispName := accounts.DisplayAccountName(acc)
		state, stateClr := getAccountState(acc, pool, nowTS)
		hits := acc.GetHits()

		nextTag := ""
		nextTagVisible := ""
		if nextID != "" && acc.ID == nextID {
			nextTag = fmt.Sprintf(" %s%s[Next ⚡]%s", clrBold, clrCyan, clrReset)
			nextTagVisible = " [Next ⚡]"
		}

		suffix := fmt.Sprintf(" · %s%s", state, nextTagVisible)
		suffixLen := utf8.RuneCountInString(suffix)
		if utf8.RuneCountInString(dispName)+suffixLen > width {
			avail := width - suffixLen
			if avail > 3 {
				dispName = truncateRunes(dispName, avail)
			}
		}

		fmt.Fprintf(stdout, "%s%s%s · %s%s%s%s\n", clrBold, dispName, clrReset, stateClr, state, clrReset, nextTag)

		if showEmail && acc.Email != "" {
			fmt.Fprintf(stdout, "%sEmail: %s%s\n", clrDim, acc.Email, clrReset)
		}

		status := acc.Status
		if status == storage.StatusDisabled {
			fmt.Fprintf(stdout, "%sDisabled (Excluded from pool routing)%s\n", clrDim, clrReset)
		} else if status == storage.StatusValidationRequired {
			fmt.Fprintf(stdout, "%sAction Required (Verify needed)%s\n", clrYellow, clrReset)
			fmt.Fprintf(stdout, "Run: agy-pool verify %d\n", i+1)
		} else if status == storage.StatusVerificationPending {
			fmt.Fprintf(stdout, "%sWaiting for generation confirmation%s\n", clrCyan, clrReset)
		} else if status == storage.StatusAuthError {
			fmt.Fprintf(stdout, "%sAuth Failure (Re-authenticate)%s\n", clrRed, clrReset)
		} else {
			if scheduler.IsReserveCandidate(acc, float64(nowTS)) {
				fmt.Fprintf(stdout, "%s[Reserve Protection: Low Quota]%s\n", clrYellow, clrReset)
			}
			q := acc.LastQuota
			q5Frac, gwFrac := quota.DisplayQuotaFractions(q)
			reset5, reset7 := extractResetTimes(acc, q, nowTS)

			if q5Frac == nil {
				fmt.Fprintln(stdout, "5H -- unknown")
			} else {
				pctVal := int(math.Round(*q5Frac * 100.0))
				g5Pct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
				g5Reset := quota.FormatCompactRemainingTime(reset5)
				nonBar5 := 10 + len(g5Reset)
				barW5 := chooseBarWidth(width, nonBar5, 10, 4)
				if barW5 > 0 {
					g5Bar := quota.RenderBlockProgressBar(q5Frac, barW5)
					fmt.Fprintf(stdout, "5H %s %s  %s\n", g5Pct, g5Bar, g5Reset)
				} else {
					fmt.Fprintf(stdout, "5H %s  %s\n", g5Pct, g5Reset)
				}
			}

			if gwFrac == nil {
				fmt.Fprintln(stdout, quota.WeeklyUnknownLabel(acc))
			} else {
				pctVal := int(math.Round(*gwFrac * 100.0))
				gwPct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
				gwReset := quota.FormatCompactRemainingTime(reset7)
				nonBar7 := 10 + len(gwReset)
				barW7 := chooseBarWidth(width, nonBar7, 10, 4)
				if barW7 > 0 {
					gwBar := quota.RenderBlockProgressBar(gwFrac, barW7)
					fmt.Fprintf(stdout, "WK %s %s  %s\n", gwPct, gwBar, gwReset)
				} else {
					fmt.Fprintf(stdout, "WK %s  %s\n", gwPct, gwReset)
				}
			}

			if q != nil && q.ThirdParty5H != nil && q.ThirdParty5H.Fraction != nil && *q.ThirdParty5H.Fraction < 1.0 {
				pctVal := int(math.Round(*q.ThirdParty5H.Fraction * 100.0))
				tpPct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
				tpReset := quota.FormatCompactRemainingTime(q.ThirdParty5H.ResetTime)
				nonBarTp := 10 + len(tpReset)
				barWTp := chooseBarWidth(width, nonBarTp, 10, 4)
				if barWTp > 0 {
					tpBar := quota.RenderBlockProgressBar(q.ThirdParty5H.Fraction, barWTp)
					fmt.Fprintf(stdout, "3P %s %s  %s\n", tpPct, tpBar, tpReset)
				} else {
					fmt.Fprintf(stdout, "3P %s  %s\n", tpPct, tpReset)
				}
			}

			ageStr := quota.FormatShortQuotaAge(acc, float64(nowTS))
			fmt.Fprintf(stdout, "age %s · hits %d\n", ageStr, hits)
		}

		if i < len(accList)-1 {
			fmt.Fprintln(stdout)
		}
	}

	strat := pool.Strategy
	if strat == "" {
		strat = config.DefaultStrategy
	}
	fmt.Fprintln(stdout)
	if daemonRunning {
		fmt.Fprintf(stdout, "%s · proxy up · :%d\n\n", strat, config.DefaultPort)
	} else {
		fmt.Fprintf(stdout, "%s · proxy down\n\n", strat)
	}
}

func renderVeryNarrowQuotaDashboard(accList []*storage.Account, pool *storage.Pool, nowTS int64, width int, showEmail bool, stdout io.Writer) {
	fmt.Fprintln(stdout)
	nextID := resolveNextCandidateID(pool, nowTS)
	for i, acc := range accList {
		if acc == nil {
			continue
		}

		dispName := accounts.DisplayAccountName(acc)
		state, stateClr := getAccountState(acc, pool, nowTS)
		hits := acc.GetHits()

		nextTag := ""
		nextTagVisible := ""
		if nextID != "" && acc.ID == nextID {
			nextTag = fmt.Sprintf(" %s%s[Next ⚡]%s", clrBold, clrCyan, clrReset)
			nextTagVisible = " [Next ⚡]"
		}

		suffix := fmt.Sprintf(" · %s%s", state, nextTagVisible)
		suffixLen := utf8.RuneCountInString(suffix)
		if utf8.RuneCountInString(dispName)+suffixLen > width {
			avail := width - suffixLen
			if avail > 3 {
				dispName = truncateRunes(dispName, avail)
			}
		}

		fmt.Fprintf(stdout, "%s%s%s · %s%s%s%s\n", clrBold, dispName, clrReset, stateClr, state, clrReset, nextTag)

		if showEmail && acc.Email != "" {
			emailLine := fmt.Sprintf("%sEmail: %s%s", clrDim, acc.Email, clrReset)
			if utf8.RuneCountInString(emailLine) > width {
				emailLine = truncateRunes(emailLine, width)
			}
			fmt.Fprintln(stdout, emailLine)
		}

		status := acc.Status
		if status == storage.StatusDisabled {
			fmt.Fprintf(stdout, "%sDisabled%s\n", clrDim, clrReset)
		} else if status == storage.StatusValidationRequired {
			fmt.Fprintf(stdout, "%sVerify Needed%s\n", clrYellow, clrReset)
			fmt.Fprintf(stdout, "agy-pool verify %d\n", i+1)
		} else if status == storage.StatusVerificationPending {
			fmt.Fprintf(stdout, "%sWaiting for confirmation%s\n", clrCyan, clrReset)
		} else if status == storage.StatusAuthError {
			fmt.Fprintf(stdout, "%sAuth Error%s\n", clrRed, clrReset)
		} else {
			q := acc.LastQuota
			q5Frac, gwFrac := quota.DisplayQuotaFractions(q)
			reset5, reset7 := extractResetTimes(acc, q, nowTS)

			if q5Frac == nil {
				fmt.Fprintln(stdout, "5H -- unknown")
			} else {
				pctVal := int(math.Round(*q5Frac * 100.0))
				g5Pct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
				g5Reset := quota.FormatCompactRemainingTime(reset5)
				fmt.Fprintf(stdout, "5H %s  %s\n", g5Pct, g5Reset)
			}

			if gwFrac == nil {
				fmt.Fprintln(stdout, quota.WeeklyUnknownLabel(acc))
			} else {
				pctVal := int(math.Round(*gwFrac * 100.0))
				gwPct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
				gwReset := quota.FormatCompactRemainingTime(reset7)
				fmt.Fprintf(stdout, "WK %s  %s\n", gwPct, gwReset)
			}

			if q != nil && q.ThirdParty5H != nil && q.ThirdParty5H.Fraction != nil && *q.ThirdParty5H.Fraction < 1.0 {
				pctVal := int(math.Round(*q.ThirdParty5H.Fraction * 100.0))
				tpPct := fmt.Sprintf("%4s", fmt.Sprintf("%d%%", pctVal))
				tpReset := quota.FormatCompactRemainingTime(q.ThirdParty5H.ResetTime)
				fmt.Fprintf(stdout, "3P %s  %s\n", tpPct, tpReset)
			}

			ageStr := quota.FormatShortQuotaAge(acc, float64(nowTS))
			fmt.Fprintf(stdout, "%s · %d hits\n", ageStr, hits)
		}

		if i < len(accList)-1 {
			fmt.Fprintln(stdout)
		}
	}
	fmt.Fprintln(stdout)
}

// RenameAccount renames an account's friendly label.
func RenameAccount(target, name string, stdout, stderr io.Writer) bool {
	if strings.TrimSpace(name) == "" {
		fmt.Fprintf(stdout, "%s[Error] Account name cannot be empty.%s\n", clrRed, clrReset)
		return false
	}

	acc, err := accounts.RenameAccount(target, name)
	if err != nil || acc == nil {
		fmt.Fprintf(stdout, "%s[Error] The selected account was not found.%s\n", clrRed, clrReset)
		return false
	}
	fmt.Fprintf(stdout, "%s✓ Renamed account [%s] to: %s%s%s\n", clrGreen, acc.ID, clrBold, acc.Name, clrReset)
	return true
}

// RemoveAccount removes an account from the pool.
func RemoveAccount(target string, stdout, stderr io.Writer) bool {
	acc, err := accounts.RemoveAccount(target)
	if err != nil || acc == nil {
		fmt.Fprintf(stderr, "%s[Error] Could not remove the selected account.%s\n", clrRed, clrReset)
		return false
	}
	fmt.Fprintf(stdout, "%s✓ Removed account %s from pool.%s\n", clrGreen, accounts.DisplayAccountName(acc), clrReset)
	return true
}

// DisableAccount disables an account in the pool.
func DisableAccount(target string, stdout, stderr io.Writer) bool {
	acc, err := accounts.DisableAccount(target)
	if err != nil || acc == nil {
		fmt.Fprintf(stderr, "%s[Error] Could not disable the selected account.%s\n", clrRed, clrReset)
		return false
	}
	fmt.Fprintf(stdout, "%s✓ Disabled account %s.%s\n", clrGreen, accounts.DisplayAccountName(acc), clrReset)
	return true
}

// EnableAccount enables a previously disabled account in the pool.
func EnableAccount(target string, stdout, stderr io.Writer) bool {
	acc, err := accounts.EnableAccount(target)
	if err != nil || acc == nil {
		fmt.Fprintf(stderr, "%s[Error] Could not enable the selected account.%s\n", clrRed, clrReset)
		return false
	}
	fmt.Fprintf(stdout, "%s✓ Enabled account %s.%s\n", clrGreen, accounts.DisplayAccountName(acc), clrReset)
	return true
}

// SwitchAccount switches the active account in the pool.
func SwitchAccount(target string, stdout, stderr io.Writer) bool {
	pool, err := storage.LoadPool()
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Could not load the account pool.%s\n", clrRed, clrReset)
		return false
	}
	if pool == nil || len(pool.Accounts) == 0 {
		fmt.Fprintf(stderr, "%s[Error] Account pool is empty.%s\n", clrRed, clrReset)
		return false
	}
	acc, err := accounts.SwitchAccount(target)
	if err != nil || acc == nil {
		fmt.Fprintf(stderr, "%s[Error] Could not switch the selected account.%s\n", clrRed, clrReset)
		return false
	}
	fmt.Fprintf(stdout, "%s✓ Switched CLI base account to: %s%s%s\n", clrGreen, clrBold, accounts.DisplayAccountName(acc), clrReset)
	return true
}

func handleExport(args []string, stdout, stderr io.Writer) int {
	filePath := ""
	encrypt := false
	password := ""
	noStats := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-e" || arg == "--encrypt" {
			encrypt = true
		} else if arg == "--no-stats" {
			noStats = true
		} else if arg == "-p" || arg == "--password" {
			if i+1 < len(args) {
				password = args[i+1]
				i++
			}
		} else if strings.HasPrefix(arg, "--password=") {
			password = strings.TrimPrefix(arg, "--password=")
		} else if !strings.HasPrefix(arg, "-") && filePath == "" {
			filePath = arg
		}
	}

	pool, err := storage.LoadPool()
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Could not load the account pool: %v%s\n", clrRed, err, clrReset)
		return 1
	}
	if pool == nil || len(pool.Accounts) == 0 {
		fmt.Fprintf(stdout, "\n%sNo accounts in pool to export.%s\n", clrYellow, clrReset)
		return 0
	}

	outPath, err := accounts.ExportPool(filePath, encrypt, password, noStats)
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Export failed: %v%s\n", clrRed, err, clrReset)
		return 1
	}
	if filePath != "-" {
		encLabel := ""
		if encrypt || password != "" {
			encLabel = fmt.Sprintf(" (%sEncrypted with passphrase%s)", clrGreen, clrReset)
		}
		fmt.Fprintf(stdout, "\n%s✓ Successfully exported %d account(s) to:%s\n", clrGreen, len(pool.Accounts), clrReset)
		fmt.Fprintf(stdout, "  %s%s%s%s\n", clrBold, outPath, clrReset, encLabel)
		fmt.Fprintf(stdout, "%s  File permissions: 0600 (Restricted to current user).%s\n\n", clrDim, clrReset)
	}
	return 0
}

func handleImport(args []string, stdout, stderr io.Writer) int {
	filePath := ""
	password := ""
	replace := false
	skipExisting := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--replace" {
			replace = true
		} else if arg == "--skip-existing" {
			skipExisting = true
		} else if arg == "-p" || arg == "--password" {
			if i+1 < len(args) {
				password = args[i+1]
				i++
			}
		} else if strings.HasPrefix(arg, "--password=") {
			password = strings.TrimPrefix(arg, "--password=")
		} else if !strings.HasPrefix(arg, "-") && filePath == "" {
			filePath = arg
		}
	}

	if filePath == "" {
		fmt.Fprintf(stderr, "usage: agy-pool import [-h] [-p PASSWORD] [--replace] [--skip-existing] file\nagy-pool import: error: the following arguments are required: file\n")
		return 2
	}

	res, err := accounts.ImportPool(filePath, password, replace, skipExisting)
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Import failed: %v%s\n", clrRed, err, clrReset)
		return 1
	}
	finalPool, err := storage.LoadPool()
	if err != nil || finalPool == nil {
		fmt.Fprintf(stderr, "%s[Error] Import completed but the account pool could not be verified.%s\n", clrRed, clrReset)
		return 1
	}
	total := len(finalPool.Accounts)
	fmt.Fprintf(stdout, "%s✓ Successfully imported %d account(s) (Pool total: %d)%s\n", clrGreen, res.Added+res.Updated, total, clrReset)
	return 0
}

func handleMigrateLegacy(args []string, stdout, stderr io.Writer) int {
	sourcePath := ""
	dataDir := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-s" || arg == "--source" {
			if i+1 < len(args) {
				sourcePath = args[i+1]
				i++
			}
		} else if strings.HasPrefix(arg, "--source=") {
			sourcePath = strings.TrimPrefix(arg, "--source=")
		} else if arg == "-D" || arg == "--directory" {
			if i+1 < len(args) {
				dataDir = args[i+1]
				i++
			}
		} else if strings.HasPrefix(arg, "--directory=") {
			dataDir = strings.TrimPrefix(arg, "--directory=")
		} else if !strings.HasPrefix(arg, "-") && sourcePath == "" {
			sourcePath = arg
		}
	}
	if sourcePath == "" {
		home := os.Getenv("HOME")
		sourcePath = filepath.Join(home, ".gemini", "agy-pool-accounts.json")
	}
	if dataDir == "" {
		dataDir = config.GetDataDir()
	}

	res, err := storage.MigrateLegacyPool(sourcePath, dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Legacy migration failed: %v%s\n", clrRed, err, clrReset)
		return 1
	}

	fmt.Fprintf(stdout, "%s✓ Successfully migrated %d account(s) to %s%s\n", clrGreen, res.AccountsCount, res.TargetFile, clrReset)
	fmt.Fprintf(stdout, "%sSource file %s remains untouched.%s\n", clrDim, res.SourcePath, clrReset)
	return 0
}

// LoginCmd runs the OAuth login flow for local and SSH/headless environments.
func LoginCmd(stdin io.Reader, stdout, stderr io.Writer) int {
	return LoginCmdWithOptions(stdin, stdout, stderr, accounts.LoginOptions{})
}

// LoginCmdWithOptions runs the login flow with optional overrides for testing.
func LoginCmdWithOptions(stdin io.Reader, stdout, stderr io.Writer, extraOpts accounts.LoginOptions) int {
	var browserOpened bool
	opener := extraOpts.BrowserOpener
	if opener == nil && LoginOpener != nil {
		opener = LoginOpener
	}
	wrappedOpener := func(targetURL string) error {
		var err error
		if opener != nil {
			err = opener(targetURL)
		} else {
			err = accounts.DefaultBrowserOpener(targetURL)
		}
		if err == nil {
			browserOpened = true
		}
		return err
	}

	opts := extraOpts
	opts.Stdout = stdout
	opts.BrowserOpener = wrappedOpener

	if opts.PromptFn == nil {
		opts.PromptFn = func(authURL string) (string, error) {
			if browserOpened {
				return "", nil
			}
			fmt.Fprintln(stdout, "")
			fmt.Fprintln(stdout, "Paste the authorization code or callback URL:")
			fmt.Fprint(stdout, "> ")
			scanner := bufio.NewScanner(stdin)
			if scanner.Scan() {
				return strings.TrimSpace(scanner.Text()), nil
			}
			if err := scanner.Err(); err != nil {
				return "", err
			}
			return "", nil
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	acc, err := accounts.Login(ctx, opts)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintf(stderr, "\n%s[Error] Login cancelled.%s\n", clrRed, clrReset)
			return 130
		}
		message := "Login failed."
		if strings.Contains(err.Error(), "login timed out or cancelled") {
			message = "login timed out or cancelled"
		} else if strings.Contains(err.Error(), "OAuth callback error: access_denied") {
			message = "OAuth callback error: access_denied"
		}
		fmt.Fprintf(stderr, "%s[Error] %s%s\n", clrRed, message, clrReset)
		return 1
	}

	fmt.Fprintf(stdout, "\n%s✓ Successfully authenticated as %s%s\n", clrGreen, accounts.DisplayAccountName(acc), clrReset)
	return 0
}
