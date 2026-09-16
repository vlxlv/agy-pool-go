package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/conversation"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
	"github.com/vlxlv/agy-pool-go/internal/diagnostics"
	"github.com/vlxlv/agy-pool-go/internal/installer"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// ANSI Color Codes matching Python reference
const (
	clrReset   = "\033[0m"
	clrBold    = "\033[1m"
	clrDim     = "\033[2m"
	clrGreen   = "\033[32m"
	clrYellow  = "\033[33m"
	clrBlue    = "\033[34m"
	clrMagenta = "\033[35m"
	clrCyan    = "\033[36m"
	clrRed     = "\033[31m"
)

var defaultExec = func(binary string, argv []string, env []string) error {
	return syscall.Exec(binary, argv, env)
}

// ExecHandler allows replacing syscall.Exec for test isolation.
var ExecHandler = defaultExec

// Main executes CLI commands and returns the process exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if stdin == nil {
		stdin = os.Stdin
	}

	if len(args) == 0 {
		ListAccounts("", stdout, stderr)
		return 0
	}

	cmd := args[0]

	// 1. Fast dispatch for version, help, runner
	switch cmd {
	case "-v", "--version", "version":
		fmt.Fprintf(stdout, "agy-pool %s\n", config.Version)
		return 0
	case "-h", "--help", "help":
		printHelp(stdout)
		return 0
	case "run":
		return RunAgyWithLB(args[1:], stdout, stderr)
	case "raw", "orig", "direct":
		return RunAgyDirect(args[1:], stdout, stderr)
	case "doctor", "check", "health":
		if !diagnostics.RunDoctor(stdout) {
			return 1
		}
		return 0
	case "login", "add":
		_, _ = accounts.Login(context.Background(), accounts.LoginOptions{})
		return 0
	case "import-current":
		_, _ = accounts.ImportCurrent()
		return 0
	case "list", "ls", "quota":
		target := ""
		if len(args) > 1 {
			target = args[1]
		}
		ListAccounts(target, stdout, stderr)
		return 0
	case "strategy", "strat":
		target := ""
		if len(args) > 1 {
			target = args[1]
		}
		if !ManageStrategy(target, stdout, stderr) {
			return 1
		}
		return 0
	case "rename":
		if len(args) < 3 {
			fmt.Fprintf(stderr, "usage: agy-pool rename [-h] target name\nagy-pool rename: error: the following arguments are required: target, name\n")
			return 2
		}
		if !RenameAccount(args[1], args[2], stdout, stderr) {
			return 1
		}
		return 0
	case "remove", "rm":
		if len(args) < 2 {
			fmt.Fprintf(stderr, "usage: agy-pool remove [-h] target\nagy-pool remove: error: the following arguments are required: target\n")
			return 2
		}
		RemoveAccount(args[1], stdout, stderr)
		return 0
	case "switch":
		target := "auto"
		if len(args) > 1 {
			target = args[1]
		}
		SwitchAccount(target, stdout, stderr)
		return 0
	case "export", "backup":
		return handleExport(args[1:], stdout, stderr)
	case "import", "restore":
		return handleImport(args[1:], stdout, stderr)
	case "verify":
		target := ""
		if len(args) > 1 {
			target = args[1]
		}
		_, _, _ = accounts.VerifyAccount(target)
		return 0
	case "log", "logs":
		return handleLogs(args[1:], stdout, stderr)
	case "start":
		StartDaemonCmd(stdout, stderr)
		return 0
	case "stop":
		StopDaemonCmd(stdout, stderr)
		return 0
	case "restart":
		RestartDaemonCmd(stdout, stderr)
		return 0
	case "status":
		StatusCmd(stdout, stderr)
		return 0
	case "daemon-run":
		_, _ = daemon.StartDaemon(daemon.LaunchOptions{Foreground: true})
		return 0
	case "migrate-legacy":
		return handleMigrateLegacy(args[1:], stdout, stderr)
	case "install":
		return handleInstall(args[1:], stdout, stderr)
	case "uninstall":
		return handleUninstall(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "usage: agy-pool [-h] [-v] {login,add,import-current,list,ls,quota,strategy,strat,rename,doctor,check,health,remove,rm,switch,export,backup,import,restore,verify,log,logs,start,stop,restart,status,version,daemon-run,run} ...\n")
		fmt.Fprintf(stderr, "agy-pool: error: argument command: invalid choice: '%s'\n", cmd)
		return 2
	}
}

func printHelp(out io.Writer) {
	fmt.Fprintf(out, "usage: agy-pool [-h] [-v] {login,add,import-current,list,ls,quota,strategy,strat,rename,doctor,check,health,remove,rm,switch,export,backup,import,restore,verify,log,logs,start,stop,restart,status,version,daemon-run,run} ...\n\n")
	fmt.Fprintf(out, "Antigravity Multi-Account Pool & Load Balancer for Termux & Linux\n\n")
	fmt.Fprintf(out, "Examples:\n")
	fmt.Fprintf(out, "  agy-pool login            Add/login a new Google account\n")
	fmt.Fprintf(out, "  agy-pool list             Refresh and show cached account quotas\n")
	fmt.Fprintf(out, "  agy-pool strategy         View or change load balancing strategy\n")
	fmt.Fprintf(out, "  agy-pool rename 1 \"Work\"  Set friendly label for an account\n")
	fmt.Fprintf(out, "  agy-pool doctor           Run comprehensive system diagnostic check\n")
	fmt.Fprintf(out, "  agy-pool switch auto      Switch active account to the one with highest quota\n")
	fmt.Fprintf(out, "  agy-pool export           Export accounts pool to a backup JSON file\n")
	fmt.Fprintf(out, "  agy-pool import file.json Restore accounts pool from a backup file\n")
	fmt.Fprintf(out, "  agy-pool start            Start the load balancing reverse proxy daemon\n")
	fmt.Fprintf(out, "  agy-pool run              Launch agy with automated quota load balancing\n")
}

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

// ManageStrategy displays or updates the pool load balancing strategy.
func ManageStrategy(targetStrategy string, stdout, stderr io.Writer) bool {
	validStrategies := []string{config.StrategyMaxQuota, config.StrategyLeastUsed, config.StrategyRoundRobin}

	pool, _ := storage.LoadPool()
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

	err := storage.PoolTransaction(func(p *storage.Pool) error {
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

func handleInstall(args []string, stdout, stderr io.Writer) int {
	opts := installer.Options{
		Stdout: stdout,
		Stderr: stderr,
	}
	if exe, err := os.Executable(); err == nil {
		opts.SourceBinary = exe
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			fmt.Fprintf(stdout, "usage: agy-pool install [--user] [--system] [--prefix PREFIX] [--target DIR] [--bin-dir DIR] [--config-dir DIR] [--data-dir DIR] [--native-agy PATH] [--skip-rc]\n")
			return 0
		case arg == "--user":
			opts.IsSystem = false
		case arg == "--system":
			opts.IsSystem = true
		case arg == "--prefix":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "agy-pool install: error: flag needs an argument: %s\n", arg)
				return 2
			}
			opts.Prefix = args[i+1]
			i++
		case strings.HasPrefix(arg, "--prefix="):
			opts.Prefix = strings.TrimPrefix(arg, "--prefix=")
		case arg == "--target" || arg == "--bin-dir":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "agy-pool install: error: flag needs an argument: %s\n", arg)
				return 2
			}
			opts.TargetDir = args[i+1]
			i++
		case strings.HasPrefix(arg, "--target="):
			opts.TargetDir = strings.TrimPrefix(arg, "--target=")
		case strings.HasPrefix(arg, "--bin-dir="):
			opts.TargetDir = strings.TrimPrefix(arg, "--bin-dir=")
		case arg == "--config-dir":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "agy-pool install: error: flag needs an argument: %s\n", arg)
				return 2
			}
			opts.ConfigDir = args[i+1]
			i++
		case strings.HasPrefix(arg, "--config-dir="):
			opts.ConfigDir = strings.TrimPrefix(arg, "--config-dir=")
		case arg == "--data-dir":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "agy-pool install: error: flag needs an argument: %s\n", arg)
				return 2
			}
			opts.DataDir = args[i+1]
			i++
		case strings.HasPrefix(arg, "--data-dir="):
			opts.DataDir = strings.TrimPrefix(arg, "--data-dir=")
		case arg == "--native-agy":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "agy-pool install: error: flag needs an argument: %s\n", arg)
				return 2
			}
			opts.NativeAgyPath = args[i+1]
			i++
		case strings.HasPrefix(arg, "--native-agy="):
			opts.NativeAgyPath = strings.TrimPrefix(arg, "--native-agy=")
		case arg == "--skip-rc":
			opts.SkipShellRC = true
		default:
			fmt.Fprintf(stderr, "agy-pool install: error: unrecognized argument: %s\n", arg)
			return 2
		}
	}

	if err := installer.Install(opts); err != nil {
		fmt.Fprintf(stderr, "%s[Error] Installation failed: %v%s\n", clrRed, err, clrReset)
		return 1
	}
	fmt.Fprintf(stdout, "%s✓ Successfully installed agy-pool%s\n", clrGreen, clrReset)
	return 0
}

func handleUninstall(args []string, stdout, stderr io.Writer) int {
	opts := installer.Options{
		Stdout: stdout,
		Stderr: stderr,
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			fmt.Fprintf(stdout, "usage: agy-pool uninstall [--user] [--system] [--prefix PREFIX] [--target DIR] [--bin-dir DIR]\n")
			return 0
		case arg == "--user":
			opts.IsSystem = false
		case arg == "--system":
			opts.IsSystem = true
		case arg == "--prefix":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "agy-pool uninstall: error: flag needs an argument: %s\n", arg)
				return 2
			}
			opts.Prefix = args[i+1]
			i++
		case strings.HasPrefix(arg, "--prefix="):
			opts.Prefix = strings.TrimPrefix(arg, "--prefix=")
		case arg == "--target" || arg == "--bin-dir":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "agy-pool uninstall: error: flag needs an argument: %s\n", arg)
				return 2
			}
			opts.TargetDir = args[i+1]
			i++
		case strings.HasPrefix(arg, "--target="):
			opts.TargetDir = strings.TrimPrefix(arg, "--target=")
		case strings.HasPrefix(arg, "--bin-dir="):
			opts.TargetDir = strings.TrimPrefix(arg, "--bin-dir=")
		default:
			fmt.Fprintf(stderr, "agy-pool uninstall: error: unrecognized argument: %s\n", arg)
			return 2
		}
	}

	if err := installer.Uninstall(opts); err != nil {
		fmt.Fprintf(stderr, "%s[Error] Uninstallation failed: %v%s\n", clrRed, err, clrReset)
		return 1
	}
	fmt.Fprintf(stdout, "%s✓ Successfully uninstalled agy-pool%s\n", clrGreen, clrReset)
	return 0
}

func handleLogs(args []string, stdout, stderr io.Writer) int {
	lines := 20
	follow := false
	clear := false
	rotate := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-f" || arg == "--follow" {
			follow = true
		} else if arg == "--clear" || arg == "--clean" {
			clear = true
		} else if arg == "--rotate" {
			rotate = true
		} else if arg == "-n" || arg == "--lines" {
			if i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil {
					lines = n
				}
				i++
			}
		}
	}

	logFile := config.GetLogFile()
	maxLogBytes := int64(config.DefaultMaxLogBytes)
	backupCount := config.DefaultBackupLogCount

	if clear {
		daemon.ClearLog(logFile, backupCount)
		fmt.Fprintf(stdout, "%s✓ Successfully cleared gateway log (%s)%s\n", clrGreen, logFile, clrReset)
		return 0
	}

	if rotate {
		fi, err := os.Stat(logFile)
		if err != nil || fi.Size() == 0 {
			fmt.Fprintf(stdout, "%sLog file is empty or does not exist, rotation skipped.%s\n", clrYellow, clrReset)
			return 0
		}
		rotated, err := daemon.RotateLogIfNeeded(logFile, maxLogBytes, backupCount, true)
		if err == nil && rotated {
			fmt.Fprintf(stdout, "%s✓ Rotated active log: %s -> %s.1%s\n", clrGreen, logFile, logFile, clrReset)
		} else {
			fmt.Fprintf(stdout, "%s[Error] Log rotation failed.%s\n", clrRed, clrReset)
		}
		return 0
	}

	fi, err := os.Stat(logFile)
	if err != nil {
		fmt.Fprintf(stdout, "%sLog file does not exist yet: %s%s\n", clrYellow, logFile, clrReset)
		return 0
	}

	f, err := os.Open(logFile)
	if err != nil {
		fmt.Fprintf(stdout, "%s[Error] Failed to read log file: %v%s\n", clrRed, err, clrReset)
		return 0
	}
	defer f.Close()

	var allLines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		allLines = append(allLines, scanner.Text())
	}

	totalLines := len(allLines)
	backupInfo := ""
	backupFile := logFile + ".1"
	if bfi, err := os.Stat(backupFile); err == nil {
		backupInfo = fmt.Sprintf(" | Backup: %s", daemon.FormatSize(bfi.Size()))
	}

	fmt.Fprintln(stdout, strings.Repeat("=", 68))
	fmt.Fprintln(stdout, "                    Antigravity Gateway Log                         ")
	fmt.Fprintln(stdout, strings.Repeat("=", 68))
	fmt.Fprintf(stdout, "Path: %s\n", logFile)
	fmt.Fprintf(stdout, "Size: %s (%d lines)%s\n", daemon.FormatSize(fi.Size()), totalLines, backupInfo)
	fmt.Fprintf(stdout, "Policy: Auto-rotate at %s (retaining %d backup)\n", daemon.FormatSize(maxLogBytes), backupCount)
	fmt.Fprintln(stdout, strings.Repeat("-", 68))

	start := 0
	if lines > 0 && totalLines > lines {
		start = totalLines - lines
	}
	for _, l := range allLines[start:] {
		fmt.Fprintln(stdout, l)
	}

	if follow {
		fmt.Fprintln(stdout, strings.Repeat("-", 68))
		fmt.Fprintf(stdout, "%sStreaming live logs (Ctrl+C to stop)...%s\n", clrCyan, clrReset)
		// For follow, wait until canceled
	}

	return 0
}

// StartDaemonCmd starts the proxy gateway in background mode.
func StartDaemonCmd(stdout, stderr io.Writer) {
	res, err := daemon.StartDaemon(daemon.LaunchOptions{Foreground: false})
	if err != nil {
		fmt.Fprintf(stdout, "%s[Error] Failed to start gateway. Check %s%s\n",
			clrRed, config.GetLogFile(), clrReset)
		return
	}
	if res.AlreadyRun {
		fmt.Fprintf(stdout, "%sProxy daemon is already running (PID: %d).%s\n",
			clrYellow, res.PID, clrReset)
	} else {
		fmt.Fprintf(stdout, "%s✓ Started agy-pool gateway proxy on http://127.0.0.1:%d (PID: %d)%s\n",
			clrGreen, config.DefaultPort, res.PID, clrReset)
	}
}

// StopDaemonCmd stops the proxy gateway.
func StopDaemonCmd(stdout, stderr io.Writer) {
	err := daemon.StopDaemon("", config.DefaultPort, 3*time.Second)
	if err != nil {
		fmt.Fprintf(stdout, "%sProxy daemon is not running.%s\n", clrYellow, clrReset)
	} else {
		fmt.Fprintf(stdout, "%s✓ Stopped proxy daemon%s\n", clrGreen, clrReset)
	}
}

// RestartDaemonCmd restarts the proxy gateway.
func RestartDaemonCmd(stdout, stderr io.Writer) {
	StopDaemonCmd(stdout, stderr)
	time.Sleep(1 * time.Second)
	StartDaemonCmd(stdout, stderr)
}

// StatusCmd checks daemon status and displays account dashboard.
func StatusCmd(stdout, stderr io.Writer) {
	info := daemon.GetDaemonInfo("")
	if info != nil && daemon.IsPortListening(config.DefaultPort) {
		verNote := ""
		if info.Version != "" {
			verNote = fmt.Sprintf(" [v%s]", info.Version)
		}
		if daemon.IsDaemonOutdated("", config.DefaultPort) {
			fmt.Fprintf(stdout, "%s⚠ Gateway Proxy is RUNNING (PID: %d%s, Outdated Code). Run 'agy-pool restart' to reload.%s\n",
				clrYellow, info.PID, verNote, clrReset)
		} else {
			fmt.Fprintf(stdout, "%s✓ Gateway Proxy is RUNNING (PID: %d%s) on http://127.0.0.1:%d%s\n",
				clrGreen, info.PID, verNote, config.DefaultPort, clrReset)
		}
	} else {
		fmt.Fprintf(stdout, "%s○ Gateway Proxy is STOPPED%s\n", clrYellow, clrReset)
	}
	ListAccounts("", stdout, stderr)
}

// ParseRunArgs parses options scoped to `agy-pool run`.
// Only parses options before "--".
// After "--", native agy arguments are preserved byte-for-byte without parsing or modification.
func ParseRunArgs(args []string) (configFile string, dataDir string, nativeArgs []string, err error) {
	dashIdx := -1
	for i, arg := range args {
		if arg == "--" {
			dashIdx = i
			break
		}
	}

	var agyPoolArgs []string
	if dashIdx != -1 {
		agyPoolArgs = args[:dashIdx]
		nativeArgs = args[dashIdx+1:]
	} else {
		// When no "--" delimiter is present:
		// Scan leading -c/--config and -D/--directory options.
		// Stop at the first non-option argument, treating it and all subsequent arguments as native args.
		i := 0
		for i < len(args) {
			arg := args[i]
			if arg == "-c" || arg == "--config" {
				if i+1 >= len(args) {
					return "", "", nil, fmt.Errorf("flag needs an argument: %s", arg)
				}
				configFile = args[i+1]
				i += 2
			} else if strings.HasPrefix(arg, "--config=") {
				configFile = strings.TrimPrefix(arg, "--config=")
				i++
			} else if arg == "-D" || arg == "--directory" {
				if i+1 >= len(args) {
					return "", "", nil, fmt.Errorf("flag needs an argument: %s", arg)
				}
				dataDir = args[i+1]
				i += 2
			} else if strings.HasPrefix(arg, "--directory=") {
				dataDir = strings.TrimPrefix(arg, "--directory=")
				i++
			} else {
				nativeArgs = args[i:]
				break
			}
		}
		return configFile, dataDir, nativeArgs, nil
	}

	// Strictly parse agyPoolArgs before "--"
	for i := 0; i < len(agyPoolArgs); i++ {
		arg := agyPoolArgs[i]
		if arg == "-c" || arg == "--config" {
			if i+1 >= len(agyPoolArgs) {
				return "", "", nil, fmt.Errorf("flag needs an argument: %s", arg)
			}
			configFile = agyPoolArgs[i+1]
			i++
		} else if strings.HasPrefix(arg, "--config=") {
			configFile = strings.TrimPrefix(arg, "--config=")
		} else if arg == "-D" || arg == "--directory" {
			if i+1 >= len(agyPoolArgs) {
				return "", "", nil, fmt.Errorf("flag needs an argument: %s", arg)
			}
			dataDir = agyPoolArgs[i+1]
			i++
		} else if strings.HasPrefix(arg, "--directory=") {
			dataDir = strings.TrimPrefix(arg, "--directory=")
		} else {
			return "", "", nil, fmt.Errorf("unknown option for run: %s", arg)
		}
	}

	return configFile, dataDir, nativeArgs, nil
}

// RunAgyWithLB parses run-scoped options (-c, -D), configures daemon/state, and execs native agy.
func RunAgyWithLB(extraArgs []string, stdout, stderr io.Writer) int {
	configFile, dataDir, nativeArgs, err := ParseRunArgs(extraArgs)
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] agy-pool run: %v%s\n", clrRed, err, clrReset)
		return 2
	}

	// 1. Static config loading
	var cfg *config.StaticConfig
	if configFile != "" {
		c, err := config.LoadConfigFile(configFile)
		if err != nil {
			fmt.Fprintf(stderr, "%s[Error] Failed to load config %s: %v%s\n", clrRed, configFile, err, clrReset)
			return 1
		}
		cfg = c
		config.SetStaticConfig(cfg)
	} else {
		resolvedFile, _ := config.ResolveDefaultConfigFile()
		if resolvedFile != "" {
			c, err := config.LoadConfigFile(resolvedFile)
			if err == nil {
				cfg = c
				config.SetStaticConfig(cfg)
			}
		}
		if cfg == nil {
			cfg = config.GetStaticConfig()
		}
	}

	// 2. Data directory configuration
	if dataDir != "" {
		if err := config.ConfigureDataDir(dataDir); err != nil {
			fmt.Fprintf(stderr, "%s[Error] Invalid data directory %s: %v%s\n", clrRed, dataDir, err, clrReset)
			return 1
		}
	} else {
		_ = config.ConfigureDefaultDataDir()
	}

	// 3. Ensure daemon is running for this instance
	port := cfg.Server.Port
	listen := cfg.Server.Listen
	if listen == "" {
		listen = "127.0.0.1"
	}
	pidFile := config.GetPIDFile()
	logFile := config.GetLogFile()

	if mismatch := daemon.CheckDaemonMismatch(pidFile, port, configFile); mismatch != "" {
		fmt.Fprintf(stderr, "%s[agy-pool] Notice: %s%s\n", clrYellow, mismatch, clrReset)
	}

	if !daemon.IsDaemonRunning(pidFile, port) {
		_, _ = daemon.StartDaemon(daemon.LaunchOptions{
			Foreground: false,
			Port:       port,
			StateDir:   config.GetDataDir(),
			ConfigFile: configFile,
			PIDFile:    pidFile,
			LogFile:    logFile,
		})
	}

	// 4. Set gateway environment
	env := os.Environ()
	gatewayURL := fmt.Sprintf("http://%s:%d", listen, port)
	env = append(env, "CLOUD_CODE_URL="+gatewayURL)

	// 5. Sync active agy token
	_, _ = accounts.SyncActiveAgyTokenFile()

	// 6. Find native agy binary
	var agyPath string
	if cfg.NativeAgy.Binary != nil && *cfg.NativeAgy.Binary != "" {
		agyPath = *cfg.NativeAgy.Binary
		if fi, err := os.Stat(agyPath); err != nil || fi.IsDir() || (fi.Mode()&0111) == 0 {
			fmt.Fprintf(stderr, "%s[Error] Specified native_agy.binary %q is not executable.%s\n", clrRed, agyPath, clrReset)
			return 1
		}
	} else {
		agyPath = diagnostics.FindRealAgyBinary()
	}
	if agyPath == "" {
		fmt.Fprintf(stderr, "%s[Error] Could not locate 'agy' executable in PATH.%s\n", clrRed, clrReset)
		return 1
	}

	// 7. Resolve native agy arguments (e.g. -c / --continue to --conversation <id>)
	resolvedArgs := conversation.ResolveContinueArg(nativeArgs, stderr)

	// 8. Execute native agy with resolvedArgs
	// Notice: caller's cwd is preserved unchanged (no os.Chdir).
	cmd := append([]string{agyPath}, resolvedArgs...)
	if err := ExecHandler(agyPath, cmd, env); err != nil {
		fmt.Fprintf(stderr, "%s[Error] Failed to execute agy: %v%s\n", clrRed, err, clrReset)
		return 1
	}
	return 0
}

// RunAgyDirect resolves native agy execution bypassing the proxy gateway.
func RunAgyDirect(extraArgs []string, stdout, stderr io.Writer) int {
	var nativeArgs []string
	dashIdx := -1
	for i, arg := range extraArgs {
		if arg == "--" {
			dashIdx = i
			break
		}
	}
	if dashIdx != -1 {
		nativeArgs = extraArgs[dashIdx+1:]
	} else {
		nativeArgs = extraArgs
	}

	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CLOUD_CODE_URL=") {
			env = append(env, e)
		}
	}

	agyPath := diagnostics.FindRealAgyBinary()
	if agyPath == "" {
		fmt.Fprintf(stderr, "%s[Error] Could not locate 'agy' executable in PATH.%s\n", clrRed, clrReset)
		return 1
	}

	resolvedArgs := conversation.ResolveContinueArg(nativeArgs, stderr)
	cmd := append([]string{agyPath}, resolvedArgs...)
	if err := ExecHandler(agyPath, cmd, env); err != nil {
		fmt.Fprintf(stderr, "%s[Error] Failed to execute agy: %v%s\n", clrRed, err, clrReset)
		return 1
	}
	return 0
}
