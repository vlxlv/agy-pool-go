package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/vlxlv/agy-pool-go/internal/installer"
)

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

func handleRepair(args []string, stdout, stderr io.Writer) int {
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
			fmt.Fprintf(stdout, "usage: agy-pool repair [--user] [--system] [--prefix PREFIX] [--target DIR] [--bin-dir DIR]\n")
			return 0
		case arg == "--user":
			opts.IsSystem = false
		case arg == "--system":
			opts.IsSystem = true
		case arg == "--prefix":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "agy-pool repair: error: flag needs an argument: %s\n", arg)
				return 2
			}
			opts.Prefix = args[i+1]
			i++
		case strings.HasPrefix(arg, "--prefix="):
			opts.Prefix = strings.TrimPrefix(arg, "--prefix=")
		case arg == "--target" || arg == "--bin-dir":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "agy-pool repair: error: flag needs an argument: %s\n", arg)
				return 2
			}
			opts.TargetDir = args[i+1]
			i++
		case strings.HasPrefix(arg, "--target="):
			opts.TargetDir = strings.TrimPrefix(arg, "--target=")
		case strings.HasPrefix(arg, "--bin-dir="):
			opts.TargetDir = strings.TrimPrefix(arg, "--bin-dir=")
		default:
			fmt.Fprintf(stderr, "agy-pool repair: error: unrecognized argument: %s\n", arg)
			return 2
		}
	}

	if err := installer.Repair(opts); err != nil {
		fmt.Fprintf(stderr, "%s[Error] Repair failed: %v%s\n", clrRed, err, clrReset)
		return 1
	}
	fmt.Fprintf(stdout, "%s✓ Successfully repaired agy-pool integration%s\n", clrGreen, clrReset)
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
		case arg == "--clean-rc" || arg == "--clean-legacy-rc":
			opts.CleanLegacyRC = true
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
