package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// MoveCmd handles 'agy-pool move' (or 'mv') subcommand.
func MoveCmd(stdout, stderr io.Writer, args ...string) int {
	showEmail := accounts.IsShowEmailEnabled(nil)
	var positionals []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-h", "--help":
			fmt.Fprintf(stdout, "usage: agy-pool move [-h] [--show-email] [--hide-email] target destination\n\n")
			fmt.Fprintf(stdout, "Move an account to a new position (top, bottom, up, down, or index)\n\n")
			fmt.Fprintf(stdout, "positional arguments:\n")
			fmt.Fprintf(stdout, "  target        Account index (1-based), ID, name, or email\n")
			fmt.Fprintf(stdout, "  destination   Target position (1-based index, or top, bottom, up, down)\n")
			return 0
		case "-e", "--show-email", "--email":
			showEmail = true
		case "--hide-email":
			showEmail = false
		default:
			if strings.HasPrefix(arg, "-") {
				fmt.Fprintf(stderr, "agy-pool move: error: unrecognized argument: %s\n", arg)
				return 2
			}
			positionals = append(positionals, arg)
		}
	}

	if len(positionals) < 2 {
		fmt.Fprintf(stderr, "usage: agy-pool move [-h] target destination\n")
		fmt.Fprintf(stderr, "agy-pool move: error: the following arguments are required: target, destination\n")
		return 2
	}

	target := positionals[0]
	destination := positionals[1]

	res, err := accounts.MoveAccount(target, destination)
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] %s%s\n", clrRed, err.Error(), clrReset)
		return 1
	}

	if res.IsSame {
		dispName := accounts.DisplayAccountName(res.Account, showEmail)
		fmt.Fprintf(stdout, "%sAccount %s is already at position [%d].%s\n", clrYellow, dispName, res.DestIndex+1, clrReset)
		return 0
	}

	dispName := accounts.DisplayAccountName(res.Account, showEmail)
	fmt.Fprintf(stdout, "%s✓ Moved %s%s%s%s from position [%d] to [%d].%s\n",
		clrGreen, clrBold, dispName, clrReset, clrGreen, res.OrigIndex+1, res.DestIndex+1, clrReset)
	accounts.PrintAccountOrderPreview(stdout, res.Accounts, []int{res.DestIndex}, res.ActiveID, showEmail)
	return 0
}

// SwapCmd handles 'agy-pool swap' subcommand.
func SwapCmd(stdout, stderr io.Writer, args ...string) int {
	showEmail := accounts.IsShowEmailEnabled(nil)
	var positionals []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-h", "--help":
			fmt.Fprintf(stdout, "usage: agy-pool swap [-h] [--show-email] [--hide-email] target1 target2\n\n")
			fmt.Fprintf(stdout, "Swap the positions of two accounts in the pool\n\n")
			fmt.Fprintf(stdout, "positional arguments:\n")
			fmt.Fprintf(stdout, "  target1       First account index (1-based), ID, name, or email\n")
			fmt.Fprintf(stdout, "  target2       Second account index (1-based), ID, name, or email\n")
			return 0
		case "-e", "--show-email", "--email":
			showEmail = true
		case "--hide-email":
			showEmail = false
		default:
			if strings.HasPrefix(arg, "-") {
				fmt.Fprintf(stderr, "agy-pool swap: error: unrecognized argument: %s\n", arg)
				return 2
			}
			positionals = append(positionals, arg)
		}
	}

	if len(positionals) < 2 {
		fmt.Fprintf(stderr, "usage: agy-pool swap [-h] target1 target2\n")
		fmt.Fprintf(stderr, "agy-pool swap: error: the following arguments are required: target1, target2\n")
		return 2
	}

	target1 := positionals[0]
	target2 := positionals[1]

	res, err := accounts.SwapAccounts(target1, target2)
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] %s%s\n", clrRed, err.Error(), clrReset)
		return 1
	}

	if res.IsSame {
		dispName := accounts.DisplayAccountName(res.Account1, showEmail)
		fmt.Fprintf(stdout, "%sBoth targets refer to the same account: [%d] %s.%s\n", clrYellow, res.Index1+1, dispName, clrReset)
		return 0
	}

	name1 := accounts.DisplayAccountName(res.Account1, showEmail)
	name2 := accounts.DisplayAccountName(res.Account2, showEmail)
	fmt.Fprintf(stdout, "%s✓ Swapped [%d] %s%s%s ⇄ [%d] %s%s%s\n",
		clrGreen, res.Index1+1, clrBold, name1, clrReset, res.Index2+1, clrBold, name2, clrReset)
	accounts.PrintAccountOrderPreview(stdout, res.Accounts, []int{res.Index1, res.Index2}, res.ActiveID, showEmail)
	return 0
}

// SortCmd handles 'agy-pool sort' subcommand.
func SortCmd(stdout, stderr io.Writer, args ...string) int {
	showEmail := accounts.IsShowEmailEnabled(nil)
	reverse := false
	refresh := false
	by := "quota"
	bySpecified := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-h", "--help":
			fmt.Fprintf(stdout, "usage: agy-pool sort [-h] [-r] [--refresh] [--show-email] [--hide-email] [by]\n\n")
			fmt.Fprintf(stdout, "Sort account pool by criteria (smart, quota, hits, name, email, status, id, created)\n\n")
			fmt.Fprintf(stdout, "positional arguments:\n")
			fmt.Fprintf(stdout, "  by            Sort criterion (default: quota, options: smart, quota, hits, name, email, status, id, created)\n\n")
			fmt.Fprintf(stdout, "options:\n")
			fmt.Fprintf(stdout, "  -r, --reverse Reverse the sort order\n")
			fmt.Fprintf(stdout, "  --refresh     Refresh quotas before sorting\n")
			return 0
		case "-r", "--reverse":
			reverse = true
		case "--refresh":
			refresh = true
		case "-e", "--show-email", "--email":
			showEmail = true
		case "--hide-email":
			showEmail = false
		default:
			if strings.HasPrefix(arg, "-") {
				fmt.Fprintf(stderr, "agy-pool sort: error: unrecognized argument: %s\n", arg)
				return 2
			}
			if !bySpecified {
				by = arg
				bySpecified = true
			}
		}
	}

	res, err := accounts.SortAccounts(by, reverse, refresh)
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] %s%s\n", clrRed, err.Error(), clrReset)
		return 1
	}

	var isDesc bool
	if res.Criterion == "smart" || res.Criterion == "quota" || res.Criterion == "hits" || res.Criterion == "usage" {
		isDesc = !reverse
	} else {
		isDesc = reverse
	}
	orderDesc := "ascending"
	if isDesc {
		orderDesc = "descending"
	}
	revTag := ""
	if reverse {
		revTag = " (reversed)"
	}

	fmt.Fprintf(stdout, "%s✓ Sorted account pool by %s%s%s%s (%s%s).%s\n",
		clrGreen, clrBold, res.Criterion, clrReset, clrGreen, orderDesc, revTag, clrReset)
	accounts.PrintAccountOrderPreview(stdout, res.Accounts, nil, res.ActiveID, showEmail)
	return 0
}

// ReorderCmd handles 'agy-pool reorder' (or 'order') subcommand.
func ReorderCmd(stdout, stderr io.Writer, args ...string) int {
	showEmail := accounts.IsShowEmailEnabled(nil)
	var sequence []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-h", "--help":
			fmt.Fprintf(stdout, "usage: agy-pool reorder [-h] [--show-email] [--hide-email] [sequence ...]\n\n")
			fmt.Fprintf(stdout, "Batch reorder accounts or display ordering guide\n\n")
			fmt.Fprintf(stdout, "positional arguments:\n")
			fmt.Fprintf(stdout, "  sequence      Desired sequence of account indices / names (or leave empty for guide)\n")
			return 0
		case "-e", "--show-email", "--email":
			showEmail = true
		case "--hide-email":
			showEmail = false
		default:
			if strings.HasPrefix(arg, "-") {
				fmt.Fprintf(stderr, "agy-pool reorder: error: unrecognized argument: %s\n", arg)
				return 2
			}
			parts := strings.Fields(strings.ReplaceAll(arg, ",", " "))
			sequence = append(sequence, parts...)
		}
	}

	if len(sequence) == 0 {
		pool, err := storage.LoadPool()
		if err != nil || pool == nil || len(pool.Accounts) == 0 {
			fmt.Fprintf(stderr, "%s[Error] Account pool is empty.%s\n", clrRed, clrReset)
			return 1
		}
		fmt.Fprintf(stdout, "%sCurrent Account Pool Order:%s\n", clrCyan, clrReset)
		accounts.PrintAccountOrderPreview(stdout, pool.Accounts, nil, pool.ActiveAccountID, showEmail)
		fmt.Fprintf(stdout, "%sUsage:%s\n", clrBold, clrReset)
		fmt.Fprintf(stdout, "  agy-pool move <account> <position|top|bottom|up|down>  (Move single account)\n")
		fmt.Fprintf(stdout, "  agy-pool swap <account1> <account2>                   (Swap two accounts)\n")
		fmt.Fprintf(stdout, "  agy-pool sort [smart|quota|hits|name|email|status|id] [-r] (Sort pool by criteria)\n")
		fmt.Fprintf(stdout, "  agy-pool reorder <pos1> <pos2> ... <posN>             (Custom batch reordering)\n")
		return 0
	}

	pool, err := storage.LoadPool()
	if err != nil || pool == nil || len(pool.Accounts) == 0 {
		fmt.Fprintf(stderr, "%s[Error] Account pool is empty.%s\n", clrRed, clrReset)
		return 1
	}

	if len(sequence) == 2 && (len(pool.Accounts) != 2 || isRelativeKeyword(sequence[1])) {
		return MoveCmd(stdout, stderr, sequence[0], sequence[1])
	}

	res, err := accounts.ReorderAccounts(sequence)
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] %s%s\n", clrRed, err.Error(), clrReset)
		return 1
	}

	fmt.Fprintf(stdout, "%s✓ Reordered all %d accounts successfully.%s\n", clrGreen, len(res.Accounts), clrReset)
	accounts.PrintAccountOrderPreview(stdout, res.Accounts, nil, res.ActiveID, showEmail)
	return 0
}

func isRelativeKeyword(kw string) bool {
	switch strings.ToLower(strings.TrimSpace(kw)) {
	case "top", "bottom", "up", "down", "first", "last", "head", "tail", "begin", "end":
		return true
	}
	return strings.HasPrefix(kw, "+") || strings.HasPrefix(kw, "-")
}

// ConfigCmd handles 'agy-pool config [key] [value]'.
func ConfigCmd(stdout, stderr io.Writer, args ...string) int {
	var positionals []string
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Fprintf(stdout, "usage: agy-pool config [-h] [key] [value]\n\n")
			fmt.Fprintf(stdout, "View or update pool configuration settings (e.g. show_email, strategy)\n")
			return 0
		}
		if !strings.HasPrefix(a, "-") {
			positionals = append(positionals, a)
		}
	}

	pool, err := storage.LoadPool()
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Could not load the account pool: %v%s\n", clrRed, err, clrReset)
		return 1
	}

	if len(positionals) == 0 {
		strat := config.DefaultStrategy
		if pool != nil && pool.Strategy != "" {
			strat = pool.Strategy
		}
		showEmailEff := accounts.IsShowEmailEnabled(pool)
		cfgStr := "nil"
		if pool != nil && pool.ShowEmail != nil {
			cfgStr = fmt.Sprintf("%v", *pool.ShowEmail)
		}

		fmt.Fprintf(stdout, "\n%sPool Configuration Settings:%s\n", clrBold, clrReset)
		fmt.Fprintf(stdout, "  • %sstrategy%s   : %s%s%s\n", clrBold, clrReset, clrGreen, strat, clrReset)
		fmt.Fprintf(stdout, "  • %sshow_email%s : %s%v%s (configured: %s)\n", clrBold, clrReset, clrCyan, showEmailEff, clrReset, cfgStr)
		fmt.Fprintf(stdout, "\nTo change a setting: %sagy-pool config <key> <value>%s\n", clrBold, clrReset)
		fmt.Fprintf(stdout, "Example: %sagy-pool config show_email true%s\n\n", clrBold, clrReset)
		return 0
	}

	key := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(positionals[0]), "-", "_"))
	var val *string
	if len(positionals) > 1 {
		v := strings.TrimSpace(positionals[1])
		val = &v
	}

	switch key {
	case "show_email", "email":
		if val == nil {
			fmt.Fprintf(stdout, "show_email: %v\n", accounts.IsShowEmailEnabled(pool))
			return 0
		}
		bVal := strings.ToLower(*val) == "1" || strings.ToLower(*val) == "true" || strings.ToLower(*val) == "yes" || strings.ToLower(*val) == "on" || strings.ToLower(*val) == "enable"
		err := storage.PoolTransaction(func(p *storage.Pool) error {
			p.ShowEmail = &bVal
			return nil
		})
		if err != nil {
			fmt.Fprintf(stderr, "%s[Error] Failed to update config: %v%s\n", clrRed, err, clrReset)
			return 1
		}
		statusTag := fmt.Sprintf("%senabled (true)%s", clrGreen, clrReset)
		if !bVal {
			statusTag = fmt.Sprintf("%sdisabled (false)%s", clrYellow, clrReset)
		}
		fmt.Fprintf(stdout, "%s✓ Configuration updated: %s%sshow_email%s = %s\n", clrGreen, clrBold, clrReset, clrGreen, statusTag)
		return 0

	case "strategy", "strat":
		if val == nil {
			strat := config.DefaultStrategy
			if pool != nil && pool.Strategy != "" {
				strat = pool.Strategy
			}
			fmt.Fprintf(stdout, "strategy: %s\n", strat)
			return 0
		}
		if !ManageStrategy(*val, stdout, stderr) {
			return 1
		}
		return 0

	default:
		fmt.Fprintf(stderr, "%s[Error] Unknown configuration key '%s'. Supported keys: show_email, strategy%s\n", clrRed, positionals[0], clrReset)
		return 1
	}
}
