package cli

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
	"github.com/vlxlv/agy-pool-go/internal/diagnostics"
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

// LoginOpener allows replacing browser opener for test isolation.
var LoginOpener func(url string) error

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

	if os.Getenv(daemon.EnvDaemonChild) == "1" {
		return runDaemonChild(stderr)
	}

	if len(args) == 0 {
		return ListCmd(stdout, stderr)
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
		return LoginCmd(stdin, stdout, stderr)
	case "import-current":
		acc, err := accounts.ImportCurrent()
		if err != nil {
			fmt.Fprintf(stderr, "%s[Error] Could not import current native credentials: %v%s\n", clrRed, err, clrReset)
			return 1
		}
		if acc == nil {
			fmt.Fprintf(stderr, "%s[Error] Current native credentials were not imported.%s\n", clrRed, clrReset)
			return 1
		}
		fmt.Fprintf(stdout, "%s✓ Imported current native credentials into the pool.%s\n", clrGreen, clrReset)
		return 0
	case "list", "ls":
		return ListCmd(stdout, stderr, args[1:]...)
	case "quota":
		return QuotaCmd(stdout, stderr, args[1:]...)
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
		if !RemoveAccount(args[1], stdout, stderr) {
			return 1
		}
		return 0
	case "switch":
		target := "auto"
		if len(args) > 1 {
			target = args[1]
		}
		if !SwitchAccount(target, stdout, stderr) {
			return 1
		}
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
		acc, verificationURL, err := accounts.VerifyAccount(target)
		if err != nil {
			fmt.Fprintf(stderr, "%s[Error] Verification could not be completed: %v%s\n", clrRed, err, clrReset)
			return 1
		}
		if acc == nil {
			fmt.Fprintf(stderr, "%s[Error] Verification could not be completed.%s\n", clrRed, clrReset)
			return 1
		}
		label := accounts.DisplayAccountName(acc)
		if verificationURL == "" {
			fmt.Fprintf(stdout, "Verification is not required for %s.\n", label)
			return 0
		}
		fmt.Fprintf(stdout, "Verification required for %s.\n", label)
		fmt.Fprintln(stdout, "Open the verification URL shown below:")
		if safeURL, ok := redactUserURL(verificationURL); ok {
			fmt.Fprintln(stdout, safeURL)
		} else {
			fmt.Fprintln(stdout, "Re-run login/re-authentication for this account.")
		}
		return 0
	case "log", "logs":
		return handleLogs(args[1:], stdout, stderr)
	case "start":
		return StartDaemonCmd(stdout, stderr, args[1:]...)
	case "stop":
		return StopDaemonCmd(stdout, stderr, args[1:]...)
	case "restart":
		return RestartDaemonCmd(stdout, stderr, args[1:]...)
	case "status":
		return StatusCmd(stdout, stderr, args[1:]...)
	case "migrate-legacy":
		return handleMigrateLegacy(args[1:], stdout, stderr)
	case "install":
		return handleInstall(args[1:], stdout, stderr)
	case "repair":
		return handleRepair(args[1:], stdout, stderr)
	case "uninstall":
		return handleUninstall(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "usage: agy-pool [-h] [-v] {login,add,import-current,list,ls,quota,strategy,strat,rename,doctor,check,health,remove,rm,switch,export,backup,import,restore,verify,log,logs,start,stop,restart,status,version,run} ...\n")
		fmt.Fprintf(stderr, "agy-pool: error: argument command: invalid choice: '%s'\n", cmd)
		return 2
	}
}

func printHelp(out io.Writer) {
	fmt.Fprintf(out, "usage: agy-pool [-h] [-v] {login,add,import-current,list,ls,quota,strategy,strat,rename,doctor,check,health,remove,rm,switch,export,backup,import,restore,verify,log,logs,start,stop,restart,status,version,run} ...\n\n")
	fmt.Fprintf(out, "Antigravity Multi-Account Pool & Load Balancer for Termux & Linux\n\n")
	fmt.Fprintf(out, "Examples:\n")
	fmt.Fprintf(out, "  agy-pool login            Add/login a new Google account\n")
	fmt.Fprintf(out, "  agy-pool list             List accounts in rotation pool (compact)\n")
	fmt.Fprintf(out, "  agy-pool quota            Display per-account quota progress bars and reset countdowns\n")
	fmt.Fprintf(out, "  agy-pool status           Show gateway process and runtime summary\n")
	fmt.Fprintf(out, "  agy-pool strategy         View or change load balancing strategy\n")
	fmt.Fprintf(out, "  agy-pool rename 1 \"Work\"  Set friendly label for an account\n")
	fmt.Fprintf(out, "  agy-pool doctor           Run comprehensive system diagnostic check\n")
	fmt.Fprintf(out, "  agy-pool switch auto      Switch CLI base account to the one with highest quota\n")
	fmt.Fprintf(out, "  agy-pool export           Export accounts pool to a backup JSON file\n")
	fmt.Fprintf(out, "  agy-pool import file.json Restore accounts pool from a backup file\n")
	fmt.Fprintf(out, "  agy-pool start            Start the load balancing reverse proxy daemon\n")
	fmt.Fprintf(out, "  agy-pool run              Launch agy with automated quota load balancing\n")
}

// ParseInstanceFlags parses -c/--config, -D/--directory, and -p/--port flags.
func ParseInstanceFlags(args []string) (configFile string, dataDir string, port int, remainingArgs []string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-c" || arg == "--config" {
			if i+1 >= len(args) {
				return "", "", 0, nil, fmt.Errorf("flag needs an argument: %s", arg)
			}
			configFile = args[i+1]
			i++
		} else if strings.HasPrefix(arg, "--config=") {
			configFile = strings.TrimPrefix(arg, "--config=")
		} else if arg == "-D" || arg == "--directory" {
			if i+1 >= len(args) {
				return "", "", 0, nil, fmt.Errorf("flag needs an argument: %s", arg)
			}
			dataDir = args[i+1]
			i++
		} else if strings.HasPrefix(arg, "--directory=") {
			dataDir = strings.TrimPrefix(arg, "--directory=")
		} else if arg == "-p" || arg == "--port" {
			if i+1 >= len(args) {
				return "", "", 0, nil, fmt.Errorf("flag needs an argument: %s", arg)
			}
			p, err := strconv.Atoi(args[i+1])
			if err != nil {
				return "", "", 0, nil, fmt.Errorf("invalid port %q: %w", args[i+1], err)
			}
			port = p
			i++
		} else if strings.HasPrefix(arg, "--port=") {
			p, err := strconv.Atoi(strings.TrimPrefix(arg, "--port="))
			if err != nil {
				return "", "", 0, nil, fmt.Errorf("invalid port: %s", arg)
			}
			port = p
		} else {
			remainingArgs = append(remainingArgs, arg)
		}
	}
	return configFile, dataDir, port, remainingArgs, nil
}

func resolveContextForCmd(args []string) (*daemon.InstanceContext, []string, error) {
	cfgFile, dataDir, port, rem, err := ParseInstanceFlags(args)
	if err != nil {
		return nil, nil, err
	}
	ctx, err := daemon.ResolveInstanceContext(dataDir, cfgFile, port, "")
	if err != nil {
		return nil, nil, err
	}
	if err := config.ConfigureDataDir(ctx.DataDir); err != nil {
		return nil, nil, err
	}
	return ctx, rem, nil
}

func safeSelector(target string) string {
	if strings.Contains(target, "@") {
		return "the selected account"
	}
	return target
}

func redactUserURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	q := u.Query()
	for key := range q {
		switch strings.ToLower(key) {
		case "email", "login_hint", "user", "account", "token", "access_token", "refresh_token", "id_token", "code":
			q.Set(key, "REDACTED")
		}
	}
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String(), true
}
