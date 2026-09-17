package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// ListAccounts renders the account pool dashboard matching Python reference.
func ListAccounts(target string, stdout, stderr io.Writer) {
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
				fmt.Fprintf(stdout, "%s[Error] Account '%s' not found.%s\n", clrRed, target, clrReset)
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
				fmt.Fprintf(stdout, "%s[Error] Account '%s' not found.%s\n", clrRed, target, clrReset)
				return
			}
		}
	}

	daemonRunning := daemon.IsPortListening(config.DefaultPort)
	gatewayStatus := fmt.Sprintf("%sRUNNING (127.0.0.1:%d)%s", clrGreen, config.DefaultPort, clrReset)
	if !daemonRunning {
		gatewayStatus = fmt.Sprintf("%sSTOPPED%s", clrDim, clrReset)
	}

	sep := strings.Repeat("=", 68)
	subSep := strings.Repeat("-", 68)

	fmt.Fprintf(stdout, "\n%s\n", sep)
	title := fmt.Sprintf("Antigravity Multi-Account Pool v%s", config.Version)
	fmt.Fprintf(stdout, "%s%*s%s\n", clrBold, (68+len(title))/2, title, clrReset)
	fmt.Fprintf(stdout, "%s\n", sep)

	activeID := ""
	if pool.ActiveAccountID != nil {
		activeID = *pool.ActiveAccountID
	}

	nowTS := time.Now().Unix()

	for i, acc := range accList {
		if acc == nil {
			continue
		}
		accID := acc.ID
		isActive := (accID == activeID)
		q := acc.LastQuota
		status := acc.Status

		q5Frac, gwFrac := quota.DisplayQuotaFractions(q)

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

		g5Reset := quota.FormatRemainingTime(reset5)
		gwReset := quota.FormatRemainingTime(reset7)

		g5Bar := quota.RenderProgressBar(q5Frac, 10)
		gwBar := quota.RenderProgressBar(gwFrac, 10)

		isCooling := acc.RateLimitedUntil != nil && *acc.RateLimitedUntil > float64(nowTS)
		isExhausted := (q5Frac != nil && *q5Frac <= config.DepletedThreshold) ||
			(gwFrac != nil && *gwFrac <= config.DepletedThreshold)

		hits := acc.GetHits()

		marker := ""
		if status == "validation_required" {
			marker = fmt.Sprintf("%s⚠ Verify Required%s", clrYellow, clrReset)
		} else if status == "auth_error" {
			marker = fmt.Sprintf("%s✖ Auth Error%s", clrRed, clrReset)
		} else if isCooling {
			if isActive {
				marker = fmt.Sprintf("%s* Active (Cooldown)%s", clrYellow, clrReset)
			} else {
				marker = fmt.Sprintf("%sCooldown%s", clrYellow, clrReset)
			}
		} else if isExhausted {
			if isActive {
				marker = fmt.Sprintf("%s* Active (Exhausted)%s", clrRed, clrReset)
			} else {
				marker = fmt.Sprintf("%sExhausted%s", clrDim, clrReset)
			}
		} else if isActive {
			marker = fmt.Sprintf("%s* Active%s", clrGreen, clrReset)
		} else {
			marker = fmt.Sprintf("%sReady%s", clrCyan, clrReset)
		}

		dispName := accounts.DisplayAccountName(acc)
		fmt.Fprintf(stdout, "%s[%d] %s%s  [%s]  Hits: %d\n", clrBold, i+1, dispName, clrReset, marker, hits)

		if status == "validation_required" {
			fmt.Fprintf(stdout, "    • Gemini 5-Hour: %s[░░░░░░░░░░]  Action Required (Blocked)%s\n", clrYellow, clrReset)
			fmt.Fprintf(stdout, "    • Gemini Weekly: %s[░░░░░░░░░░]  Action Required (Blocked)%s\n", clrYellow, clrReset)
			fmt.Fprintf(stdout, "    %s↳ Google requires security verification for this account.%s\n", clrYellow, clrReset)
			fmt.Fprintf(stdout, "      Run %sagy-pool verify %d%s to unlock in browser.\n", clrBold, i+1, clrReset)
		} else if status == "auth_error" {
			fmt.Fprintf(stdout, "    • Gemini 5-Hour: %s[░░░░░░░░░░]  Authentication Failure%s\n", clrRed, clrReset)
			fmt.Fprintf(stdout, "    • Gemini Weekly: %s[░░░░░░░░░░]  Authentication Failure%s\n", clrRed, clrReset)
			fmt.Fprintf(stdout, "    %s↳ Token expired or revoked. Re-authenticate account.%s\n", clrRed, clrReset)
		} else {
			fmt.Fprintf(stdout, "    • Gemini 5-Hour: %s  (Resets %s)\n", g5Bar, g5Reset)
			fmt.Fprintf(stdout, "    • Gemini Weekly: %s  (Resets %s)\n", gwBar, gwReset)
			fmt.Fprintf(stdout, "    • Quota age: %s\n", quota.FormatQuotaAge(acc, float64(nowTS)))

			if q != nil && q.ThirdParty5H != nil && q.ThirdParty5H.Fraction != nil && *q.ThirdParty5H.Fraction < 1.0 {
				tpBar := quota.RenderProgressBar(q.ThirdParty5H.Fraction, 10)
				tpReset := quota.FormatRemainingTime(q.ThirdParty5H.ResetTime)
				fmt.Fprintf(stdout, "    • Claude/GPT 5h: %s  (Resets %s)\n", tpBar, tpReset)
			}
		}

		if i < len(accList)-1 {
			fmt.Fprintln(stdout)
		}
	}

	strat := pool.Strategy
	if strat == "" {
		strat = config.StrategyMaxQuota
	}

	fmt.Fprintf(stdout, "%s\n", subSep)
	fmt.Fprintf(stdout, " %s[* Active]%s %sCLI Base Token%s    %s[Ready]%s %sIn Rotation Pool%s\n",
		clrGreen, clrReset, clrDim, clrReset, clrCyan, clrReset, clrDim, clrReset)
	fmt.Fprintf(stdout, " %s[Cooldown]%s %sRate Limited%s      %s[Exhausted] Quota Depleted%s\n",
		clrYellow, clrReset, clrDim, clrReset, clrDim, clrReset)
	fmt.Fprintf(stdout, " Strategy: %s%s%s | Gateway Proxy: %s\n",
		clrBold, strat, clrReset, gatewayStatus)
	fmt.Fprintf(stdout, "%s\n\n", sep)
}

// RenameAccount renames an account's friendly label.
func RenameAccount(target, name string, stdout, stderr io.Writer) bool {
	if strings.TrimSpace(name) == "" {
		fmt.Fprintf(stdout, "%s[Error] Account name cannot be empty.%s\n", clrRed, clrReset)
		return false
	}

	acc, err := accounts.RenameAccount(target, name)
	if err != nil || acc == nil {
		fmt.Fprintf(stdout, "%s[Error] Account '%s' not found.%s\n", clrRed, target, clrReset)
		return false
	}
	return true
}

// RemoveAccount removes an account from the pool.
func RemoveAccount(target string, stdout, stderr io.Writer) {
	acc, err := accounts.RemoveAccount(target)
	if err != nil || acc == nil {
		fmt.Fprintf(stdout, "%s[Error] Account '%s' not found.%s\n", clrRed, target, clrReset)
		return
	}
	fmt.Fprintf(stdout, "%s✓ Removed account %s from pool.%s\n", clrGreen, accounts.DisplayAccountName(acc), clrReset)
}

// SwitchAccount switches the active account in the pool.
func SwitchAccount(target string, stdout, stderr io.Writer) {
	pool, _ := storage.LoadPool()
	if pool == nil || len(pool.Accounts) == 0 {
		fmt.Fprintf(stdout, "%s[Error] Account pool is empty.%s\n", clrRed, clrReset)
		return
	}
	acc, err := accounts.SwitchAccount(target)
	if err != nil || acc == nil {
		fmt.Fprintf(stdout, "%s[Error] Account '%s' not found.%s\n", clrRed, target, clrReset)
		return
	}
	fmt.Fprintf(stdout, "%s✓ Switched active account to: %s%s%s\n", clrGreen, clrBold, accounts.DisplayAccountName(acc), clrReset)
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

	pool, _ := storage.LoadPool()
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
		fmt.Fprintf(stderr, "%s[Error] %v%s\n", clrRed, err, clrReset)
		return 0
	}
	finalPool, _ := storage.LoadPool()
	total := 0
	if finalPool != nil {
		total = len(finalPool.Accounts)
	}
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
