package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/conversation"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
	"github.com/vlxlv/agy-pool-go/internal/diagnostics"
)

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

	// 1. Resolve explicit instance context
	ctx, err := daemon.ResolveInstanceContext(dataDir, configFile, 0, "")
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Failed to resolve instance: %v%s\n", clrRed, err, clrReset)
		return 1
	}

	// 2. Data directory configuration
	if err := config.ConfigureDataDir(ctx.DataDir); err != nil {
		fmt.Fprintf(stderr, "%s[Error] Invalid data directory %s: %v%s\n", clrRed, ctx.DataDir, err, clrReset)
		return 1
	}

	// 3. Ensure exact daemon is running for this instance
	status, info, msg := daemon.CheckInstanceStatus(ctx)
	switch status {
	case daemon.StatusRunningSameInstance:
		// Exact instance is running
	case daemon.StatusOutdatedBinary:
		fmt.Fprintf(stderr, "%s[agy-pool] Outdated daemon detected; reloading...%s\n", clrYellow, clrReset)
		_, err := daemon.RestartInstance(ctx, daemon.LaunchOptions{})
		if err != nil {
			fmt.Fprintf(stderr, "%s[Error] Failed to reload daemon: %v%s\n", clrRed, err, clrReset)
			return 1
		}
	case daemon.StatusConfigMismatch:
		fmt.Fprintf(stderr, "%s[agy-pool] Notice: %s%s\n", clrYellow, msg, clrReset)
		if info != nil && info.ConfigPath != ctx.ConfigPath {
			fmt.Fprintf(stderr, "%s[Error] Port %d is occupied by another daemon with config %q (requested %q)%s\n", clrRed, ctx.ListenPort, info.ConfigPath, ctx.ConfigPath, clrReset)
			return 1
		}
	case daemon.StatusPortOccupiedForeign, daemon.StatusForeignInstance:
		if !config.IsTestMode() {
			fmt.Fprintf(stderr, "%s[Error] Port %d or data directory is occupied by another process: %s%s\n", clrRed, ctx.ListenPort, msg, clrReset)
			return 1
		}
	case daemon.StatusStopped, daemon.StatusStalePID:
		_, err := daemon.StartInstance(ctx, daemon.LaunchOptions{})
		if err != nil && !config.IsTestMode() {
			fmt.Fprintf(stderr, "%s[Error] Failed to start gateway daemon: %v%s\n", clrRed, err, clrReset)
			return 1
		}
	}

	// 4. Set gateway environment
	env := os.Environ()
	gatewayURL := fmt.Sprintf("http://%s:%d", ctx.ListenHost, ctx.ListenPort)
	env = append(env, "CLOUD_CODE_URL="+gatewayURL)

	// 5. Sync active agy token
	_, _ = accounts.SyncActiveAgyTokenFile()

	// 6. Find native agy binary
	agyPath, _, err := diagnostics.ResolveNativeAgyBinary()
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Could not locate native 'agy' executable: %v%s\n", clrRed, err, clrReset)
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

	agyPath, _, err := diagnostics.ResolveNativeAgyBinary()
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Could not locate native 'agy' executable: %v%s\n", clrRed, err, clrReset)
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
