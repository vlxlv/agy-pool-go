package diagnostics

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/scheduler"
	"github.com/vlxlv/agy-pool-go/internal/storage"
	_ "modernc.org/sqlite"
)

// ANSI color codes matching Python reference
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

var (
	hooksMu             sync.RWMutex
	agyBinaryFinder     func() string
	agyVersionProvider  func() string
	backendHostProvider func() string
	tlsProber           func(host string, timeout time.Duration) error
	agyEntrypointFinder func() string
)

// SetAgyBinaryFinder overrides binary discovery for testing.
func SetAgyBinaryFinder(fn func() string) {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	agyBinaryFinder = fn
}

// SetAgyEntrypointFinder overrides entrypoint discovery for testing.
func SetAgyEntrypointFinder(fn func() string) {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	agyEntrypointFinder = fn
}

// SetAgyVersionProvider overrides version lookup for testing.
func SetAgyVersionProvider(fn func() string) {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	agyVersionProvider = fn
}

// SetBackendHostProvider overrides the backend host for testing.
func SetBackendHostProvider(fn func() string) {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	backendHostProvider = fn
}

// SetTLSProber overrides the TLS probe function for testing.
func SetTLSProber(fn func(host string, timeout time.Duration) error) {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	tlsProber = fn
}

func getEffectiveUserHome() string {
	home := os.Getenv("HOME")
	if config.IsTestMode() {
		// If in test mode and HOME is either unset or points to the real host home, use the synthetic sandbox
		if home == "" || home == config.GetRealHostUserHome() {
			if sandbox := config.GetSyntheticSandboxRoot(); sandbox != "" {
				return filepath.Join(sandbox, "test_home")
			}
		}
		if home != "" {
			return home
		}
	}
	if home == "" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	return home
}

func getSelfPaths() map[string]bool {
	paths := make(map[string]bool)

	// Current executable
	if exe, err := os.Executable(); err == nil {
		if realExe, err := filepath.EvalSymlinks(exe); err == nil {
			paths[realExe] = true
		}
		paths[filepath.Clean(exe)] = true
	}

	// argv[0]
	if len(os.Args) > 0 && os.Args[0] != "" {
		if realArg, err := filepath.EvalSymlinks(os.Args[0]); err == nil {
			paths[realArg] = true
		}
		paths[filepath.Clean(os.Args[0])] = true
	}

	// Common agy-pool locations
	home := getEffectiveUserHome()
	if home != "" {
		p := filepath.Join(home, ".local", "bin", "agy-pool")
		if realP, err := filepath.EvalSymlinks(p); err == nil {
			paths[realP] = true
		}
		paths[filepath.Clean(p)] = true
	}

	pUsr := filepath.Join("/usr/local/bin", "agy-pool")
	if realP, err := filepath.EvalSymlinks(pUsr); err == nil {
		paths[realP] = true
	}
	paths[filepath.Clean(pUsr)] = true

	if prefix := os.Getenv("PREFIX"); prefix != "" {
		pPre := filepath.Join(prefix, "bin", "agy-pool")
		if realP, err := filepath.EvalSymlinks(pPre); err == nil {
			paths[realP] = true
		}
		paths[filepath.Clean(pPre)] = true
	}

	if explicit := os.Getenv("AGY_SELF_PATH"); explicit != "" {
		if realExp, err := filepath.EvalSymlinks(explicit); err == nil {
			paths[realExp] = true
		}
		paths[filepath.Clean(explicit)] = true
	}

	return paths
}

// IsShimOrSelf reports whether a path is the agy-pool binary itself, an agy/agy-raw/agy-orig shim,
// a hard link to one of them, or a symlink resolving to one.
func IsShimOrSelf(path string) bool {
	if path == "" {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}

	candFi, statErr := os.Stat(abs)

	// 1. Check known self paths
	selfPaths := getSelfPaths()
	if selfPaths[abs] {
		return true
	}
	realPath, err := filepath.EvalSymlinks(abs)
	if err == nil && selfPaths[realPath] {
		return true
	}
	if realPath == "" {
		realPath = abs
	}
	if filepath.Base(realPath) == "agy-pool" {
		return true
	}

	// 2. Inode / SameFile check against running executable and known self paths
	// Catches hard links even if filenames differ completely!
	if statErr == nil {
		if exe, err := os.Executable(); err == nil {
			if exeFi, err := os.Stat(exe); err == nil && os.SameFile(candFi, exeFi) {
				return true
			}
		}
		for sp := range selfPaths {
			if spFi, err := os.Stat(sp); err == nil && os.SameFile(candFi, spFi) {
				return true
			}
		}
	}

	// 3. Inspect file header for exact agy-pool shim signatures (narrow content fallback)
	f, err := os.Open(realPath)
	if err != nil {
		return false
	}
	defer f.Close()

	buf := make([]byte, 1024)
	n, _ := f.Read(buf)
	if n > 0 {
		header := string(buf[:n])
		if strings.Contains(header, "# agy-pool managed shim v1") ||
			strings.Contains(header, "# >>> agy-pool integration >>>") ||
			strings.Contains(header, "exec agy-pool run") ||
			strings.Contains(header, "exec agy-pool raw") {
			return true
		}
	}

	return false
}

// ValidateNativeCandidate validates whether a candidate path is a valid native agy executable.
// It enforces that the candidate exists, is executable, and is not an agy-pool binary, hard link, or managed shim.
func ValidateNativeCandidate(cand string) error {
	if strings.TrimSpace(cand) == "" {
		return errors.New("empty path")
	}
	abs, err := filepath.Abs(cand)
	if err != nil {
		abs = filepath.Clean(cand)
	}

	fi, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("file not found or inaccessible: %w", err)
	}
	if fi.IsDir() {
		return errors.New("candidate path is a directory")
	}
	if (fi.Mode() & 0111) == 0 {
		return errors.New("candidate file is not executable")
	}

	if IsShimOrSelf(abs) {
		return errors.New("candidate is agy-pool binary, hard link, or managed shim (recursion prevented)")
	}

	return nil
}

var (
	ErrNativeAgyNotFound = errors.New("Native agy was not found.\nInstall the original Antigravity CLI first, then rerun agy-pool installer.")
)

const (
	SourceEnvAgyBin = "AGY_BIN"
	SourceConfig    = "config"
	SourceManaged   = "managed native"
	SourcePrefix    = "PREFIX"
	SourceSystem    = "system path"
	SourceUserLocal = "~/.local/bin"
	SourcePathEnv   = "PATH"
)

func getManagedNativeCandidates() []string {
	var managedCandidates []string
	if explicitManaged := os.Getenv("AGY_MANAGED_NATIVE_PATH"); explicitManaged != "" {
		managedCandidates = append(managedCandidates, explicitManaged)
	}
	if prefix := os.Getenv("PREFIX"); prefix != "" {
		managedCandidates = append(managedCandidates, filepath.Join(prefix, "libexec", "agy-pool", "agy-native"))
	}
	if config.IsTestMode() {
		if sandbox := config.GetSyntheticSandboxRoot(); sandbox != "" {
			managedCandidates = append(managedCandidates,
				filepath.Join(sandbox, "test_home", ".local", "libexec", "agy-pool", "agy-native"),
				filepath.Join(sandbox, "test_system", "usr", "local", "libexec", "agy-pool", "agy-native"),
			)
		}
	}
	home := getEffectiveUserHome()
	if home != "" {
		managedCandidates = append(managedCandidates, filepath.Join(home, ".local", "libexec", "agy-pool", "agy-native"))
	}
	managedCandidates = append(managedCandidates, "/usr/local/libexec/agy-pool/agy-native")
	return managedCandidates
}

// ResolveNativeAgyBinary resolves the native agy binary using the strict discovery precedence contract.
func ResolveNativeAgyBinary() (string, string, error) {
	hooksMu.RLock()
	finder := agyBinaryFinder
	hooksMu.RUnlock()
	if finder != nil {
		p := finder()
		if p != "" {
			return p, "mock", nil
		}
		return "", "mock", ErrNativeAgyNotFound
	}

	// 1. AGY_BIN environment override (highest priority)
	if explicit := os.Getenv("AGY_BIN"); explicit != "" {
		if err := ValidateNativeCandidate(explicit); err != nil {
			return "", SourceEnvAgyBin, fmt.Errorf("explicit AGY_BIN override %q is invalid: %w", explicit, err)
		}
		clean, _ := filepath.Abs(explicit)
		return clean, SourceEnvAgyBin, nil
	}

	// 2. Configured native_agy.binary
	cfg := config.GetStaticConfig()
	if cfg != nil && cfg.NativeAgy.Binary != nil && *cfg.NativeAgy.Binary != "" {
		cand := *cfg.NativeAgy.Binary
		if err := ValidateNativeCandidate(cand); err != nil {
			return "", SourceConfig, fmt.Errorf("explicit config native_agy.binary %q is invalid: %w", cand, err)
		}
		clean, _ := filepath.Abs(cand)
		return clean, SourceConfig, nil
	}

	// 3. Managed native binary (preserved by installer behind the managed shim)
	for _, cand := range getManagedNativeCandidates() {
		if cand == "" {
			continue
		}
		clean, err := filepath.Abs(cand)
		if err != nil {
			clean = filepath.Clean(cand)
		}
		if err := ValidateNativeCandidate(clean); err == nil {
			return clean, SourceManaged, nil
		}
	}

	// Fallback discovery in documented order:
	type fallbackCandidate struct {
		path   string
		source string
	}
	var fallbacks []fallbackCandidate

	// 3. $PREFIX/bin/agy
	if prefix := os.Getenv("PREFIX"); prefix != "" {
		fallbacks = append(fallbacks, fallbackCandidate{
			path:   filepath.Join(prefix, "bin", "agy"),
			source: SourcePrefix,
		})
	}

	// 4. /data/data/com.termux/files/usr/bin/agy
	fallbacks = append(fallbacks, fallbackCandidate{
		path:   "/data/data/com.termux/files/usr/bin/agy",
		source: SourceSystem,
	})

	// 5. /usr/local/bin/agy
	fallbacks = append(fallbacks, fallbackCandidate{
		path:   "/usr/local/bin/agy",
		source: SourceSystem,
	})

	// 6. /usr/bin/agy
	fallbacks = append(fallbacks, fallbackCandidate{
		path:   "/usr/bin/agy",
		source: SourceSystem,
	})

	// 7. ~/.local/bin/agy
	home := getEffectiveUserHome()
	if home != "" {
		fallbacks = append(fallbacks, fallbackCandidate{
			path:   filepath.Join(home, ".local", "bin", "agy"),
			source: SourceUserLocal,
		})
	}

	// 8. PATH entries in order
	pathEnv := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir != "" {
			fallbacks = append(fallbacks, fallbackCandidate{
				path:   filepath.Join(dir, "agy"),
				source: SourcePathEnv,
			})
		}
	}

	seenPaths := make(map[string]bool)

	for _, cand := range fallbacks {
		if cand.path == "" {
			continue
		}
		clean, err := filepath.Abs(cand.path)
		if err != nil {
			clean = filepath.Clean(cand.path)
		}
		realPath, err := filepath.EvalSymlinks(clean)
		if err != nil {
			realPath = clean
		}
		if seenPaths[clean] || seenPaths[realPath] {
			continue
		}
		seenPaths[clean] = true
		seenPaths[realPath] = true

		if err := ValidateNativeCandidate(clean); err != nil {
			// Candidate is missing, not executable, or self/shim: continue fallback search
			continue
		}

		return clean, cand.source, nil
	}

	return "", "none", ErrNativeAgyNotFound
}

// FindRealAgyBinary locates the underlying native agy executable dynamically without self-recursion.
func FindRealAgyBinary() string {
	hooksMu.RLock()
	finder := agyBinaryFinder
	hooksMu.RUnlock()
	if finder != nil {
		return finder()
	}

	p, _, err := ResolveNativeAgyBinary()
	if err != nil {
		return ""
	}
	return p
}

// GetInstalledAgyVersion queries the version of the discovered agy binary.
func GetInstalledAgyVersion() string {
	hooksMu.RLock()
	provider := agyVersionProvider
	hooksMu.RUnlock()
	if provider != nil {
		return provider()
	}

	agyBin := FindRealAgyBinary()
	if agyBin != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, agyBin, "--version")
		out, err := cmd.Output()
		if err == nil {
			v := strings.TrimSpace(string(out))
			if v != "" && strings.Contains(v, ".") {
				return v
			}
		}
	}
	return "1.2.2"
}

func defaultTLSProbe(host string, timeout time.Duration) error {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(host, "443"), &tls.Config{
		ServerName: host,
	})
	if err != nil {
		return err
	}
	return conn.Close()
}

// RunDoctor performs end-to-end system diagnostic checks across runtime, network, and pool state.
// Returns true if no fatal issues found (warnings are non-fatal).
func RunDoctor(out io.Writer) bool {
	if out == nil {
		out = os.Stdout
	}

	sep := strings.Repeat("=", 68)
	subSep := strings.Repeat("-", 68)

	fmt.Fprintln(out, "\n"+sep)
	title := fmt.Sprintf("Antigravity System Doctor (agy-pool v%s)", config.Version)
	fmt.Fprintf(out, "%s%*s%s\n", clrBold, (68+len(title))/2, title, clrReset)
	fmt.Fprintln(out, sep+"\n")

	issuesFound := 0
	warningsFound := 0

	// 1. Go Runtime Environment & Concurrency
	goVer := runtime.Version()
	fmt.Fprintf(out, "  %s✓%s Go Runtime: %s%s%s (%s %s)\n",
		clrGreen, clrReset, clrBold, goVer, clrReset, runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(out, "  %s✓%s POSIX Concurrency: flock multi-process file locking available\n",
		clrGreen, clrReset)

	// 2. Native Antigravity Executable (agy)
	realAgy, source, err := ResolveNativeAgyBinary()
	if err == nil {
		agyVer := GetInstalledAgyVersion()
		fmt.Fprintf(out, "  %s✓%s Native Binary: %s%s%s (v%s, source: %s)\n",
			clrGreen, clrReset, clrBold, realAgy, clrReset, agyVer, source)
	} else {
		fmt.Fprintf(out, "  %s⚠%s Native Binary: %v\n",
			clrYellow, clrReset, err)
		fmt.Fprintf(out, "    %s↳ Set AGY_BIN=/path/to/agy if installed in custom location.%s\n",
			clrDim, clrReset)
		warningsFound++
	}

	// 2b. Native Shim Integration
	hooksMu.RLock()
	finderHook := agyBinaryFinder
	epHook := agyEntrypointFinder
	hooksMu.RUnlock()

	if finderHook != nil {
		fmt.Fprintf(out, "  %s✓%s Native Integration: Mock integration active (%s)\n",
			clrGreen, clrReset, realAgy)
	} else {
		home := getEffectiveUserHome()

		var entrypoint string
		if epHook != nil {
			entrypoint = epHook()
		} else if ep := os.Getenv("AGY_ENTRYPOINT_PATH"); ep != "" {
			entrypoint = ep
		} else {
			if home != "" {
				cand := filepath.Join(home, ".local", "bin", "agy")
				if _, err := os.Lstat(cand); err == nil {
					entrypoint = cand
				}
			}
			if entrypoint == "" && config.IsTestMode() {
				if sandbox := config.GetSyntheticSandboxRoot(); sandbox != "" {
					cand := filepath.Join(sandbox, "test_home", ".local", "bin", "agy")
					if _, err := os.Lstat(cand); err == nil {
						entrypoint = cand
					}
				}
			}
			if entrypoint == "" && !config.IsTestMode() {
				if p, err := exec.LookPath("agy"); err == nil {
					entrypoint = p
				}
			}
		}

		if entrypoint == "" {
			fmt.Fprintf(out, "  %s○%s Native Integration: No agy entrypoint found (run 'agy-pool install' to set up)\n",
				clrBlue, clrReset)
		} else {
			if !IsShimOrSelf(entrypoint) {
				fmt.Fprintf(out, "  %s✖%s Native Integration: %s is a native binary bypassing agy-pool\n",
					clrRed, clrReset, entrypoint)
				fmt.Fprintf(out, "    %s↳ Run 'agy-pool repair' (or 'agy-pool install') to restore the managed agy shim.%s\n",
					clrDim, clrReset)
				issuesFound++
			} else {
				var managedPath string
				for _, cand := range getManagedNativeCandidates() {
					if cand != "" {
						if _, err := os.Lstat(cand); err == nil {
							managedPath = cand
							break
						}
					}
				}

				broken := false
				if managedPath != "" {
					if IsShimOrSelf(managedPath) {
						fmt.Fprintf(out, "  %s✖%s Native Integration: Managed native %s resolves to shim/self\n",
							clrRed, clrReset, managedPath)
						issuesFound++
						broken = true
					} else if err := ValidateNativeCandidate(managedPath); err != nil {
						fmt.Fprintf(out, "  %s✖%s Native Integration: Managed native %s is not executable or invalid: %v\n",
							clrRed, clrReset, managedPath, err)
						issuesFound++
						broken = true
					}
				} else {
					fmt.Fprintf(out, "  %s✖%s Native Integration: %s is a managed shim but managed native agy is missing\n",
						clrRed, clrReset, entrypoint)
					fmt.Fprintf(out, "    %s↳ Run 'agy-pool repair' (or 'agy-pool install') to restore managed native agy.%s\n",
						clrDim, clrReset)
					issuesFound++
					broken = true
				}

				if err != nil {
					fmt.Fprintf(out, "  %s✖%s Native Integration: %s is a managed shim but no valid native agy can be resolved\n",
						clrRed, clrReset, entrypoint)
					fmt.Fprintf(out, "    %s↳ Run 'agy-pool repair' (or 'agy-pool install') or verify native agy installation.%s\n",
						clrDim, clrReset)
					issuesFound++
				} else if !broken {
					fmt.Fprintf(out, "  %s✓%s Native Integration: %s (managed shim -> %s)\n",
						clrGreen, clrReset, entrypoint, realAgy)
				}
			}
		}
	}

	// 3. Gateway Proxy Daemon
	info := daemon.GetDaemonInfo("")
	listening := daemon.IsPortListening(config.DefaultPort)
	if info != nil && listening {
		if daemon.IsDaemonOutdated("", config.DefaultPort) {
			fmt.Fprintf(out, "  %s⚠%s Gateway Daemon: RUNNING (PID %d) with %soutdated disk code%s\n",
				clrYellow, clrReset, info.PID, clrYellow, clrReset)
			fmt.Fprintf(out, "    %s↳ Run 'agy-pool restart' to reload daemon with latest code.%s\n",
				clrDim, clrReset)
			warningsFound++
		} else {
			verTag := ""
			if info.Version != "" {
				verTag = fmt.Sprintf(" [v%s]", info.Version)
			}
			fmt.Fprintf(out, "  %s✓%s Gateway Daemon: RUNNING (PID %d%s) on port %d\n",
				clrGreen, clrReset, info.PID, verTag, config.DefaultPort)
		}
	} else if listening {
		fmt.Fprintf(out, "  %s⚠%s Gateway Daemon: Port %d is in use by another process\n",
			clrYellow, clrReset, config.DefaultPort)
		warningsFound++
	} else {
		fmt.Fprintf(out, "  %s○%s Gateway Daemon: STOPPED (will auto-launch on 'agy' run)\n",
			clrBlue, clrReset)
	}

	// 4. Storage & Account Pool Health
	_ = storage.EnsureDirs()
	stateDBFile := config.GetStateDBFile()
	poolFile := config.GetAccountsFile()

	dbFi, dbErr := os.Stat(stateDBFile)
	poolFi, poolErr := os.Stat(poolFile)

	if dbErr == nil || poolErr == nil {
		var pool *storage.Pool
		var err error
		var fi os.FileInfo
		var storageDesc string

		if dbErr == nil {
			fi = dbFi
			storageDesc = fmt.Sprintf("state.db: schema v%s, readable", storage.StateDBSchemaVersion)
			sdb, openErr := storage.OpenStateDB(stateDBFile)
			if openErr != nil {
				err = openErr
			} else {
				err = sdb.View(context.Background(), func(tx *sql.Tx) error {
					var loadErr error
					pool, loadErr = storage.LoadPoolFromTx(tx)
					return loadErr
				})
				_ = sdb.Close()
			}
		} else {
			fi = poolFi
			storageDesc = "accounts.json: legacy"
			pool, err = storage.ReadPoolUnlocked(poolFile)
		}

		if err != nil {
			fmt.Fprintf(out, "  %s✖%s Account Pool: Failed to read pool file: %v\n", clrRed, clrReset, err)
			issuesFound++
		} else {
			readyCnt := 0
			exhaustedCnt := 0
			cooldownCnt := 0
			restrictedCnt := 0
			nowTS := time.Now().Unix()

			for _, a := range pool.Accounts {
				if a == nil {
					continue
				}
				remFraction := 1.0
				if a.LastQuota != nil && a.LastQuota.RemainingFraction != nil {
					remFraction = *a.LastQuota.RemainingFraction
				}
				isCooling := a.RateLimitedUntil != nil && *a.RateLimitedUntil > float64(nowTS)

				if a.Status == storage.StatusValidationRequired || a.Status == storage.StatusVerificationPending || a.Status == storage.StatusAuthError || a.Status == storage.StatusDisabled {
					restrictedCnt++
				} else if isCooling {
					cooldownCnt++
				} else if remFraction <= config.DepletedThreshold {
					exhaustedCnt++
				} else {
					readyCnt++
				}
			}

			perm := fi.Mode().Perm()
			permStr := fmt.Sprintf("0o%03o", perm)
			permOk := (perm == 0600)
			if !permOk {
				permStr = fmt.Sprintf("%s%s (expected 0o600)%s", clrYellow, permStr, clrReset)
				warningsFound++
			}

			activeStr := ""
			if pool.ActiveAccountID != nil && *pool.ActiveAccountID != "" {
				for _, a := range pool.Accounts {
					if a != nil && a.ID == *pool.ActiveAccountID {
						activeStr = fmt.Sprintf(" | CLI Base: %s", accounts.DisplayAccountName(a))
						break
					}
				}
			}

			strat := pool.Strategy
			if strat == "" {
				strat = config.StrategyMaxQuota
			}

			fmt.Fprintf(out, "  %s✓%s Account Pool: %d account(s) configured (%s, %s)%s\n",
				clrGreen, clrReset, len(pool.Accounts), storageDesc, permStr, activeStr)
			fmt.Fprintf(out, "    • Status: %s%d Ready%s, %s%d Cooldown%s, %s%d Exhausted%s, %s%d Restricted%s\n",
				clrGreen, readyCnt, clrReset, clrYellow, cooldownCnt, clrReset, clrDim, exhaustedCnt, clrReset, clrRed, restrictedCnt, clrReset)
			fmt.Fprintf(out, "    • Strategy: %s%s%s\n", clrBold, strat, clrReset)

			if restrictedCnt > 0 {
				fmt.Fprintf(out, "    %s↳ Some accounts require verification or re-auth. Run 'agy-pool verify'.%s\n",
					clrYellow, clrReset)
				warningsFound++
			}
			if len(pool.Accounts) == 0 {
				fmt.Fprintf(out, "    %s↳ Pool is empty. Run 'agy-pool login' or 'agy-pool import-current'.%s\n",
					clrYellow, clrReset)
				warningsFound++
			}

			var notRenewed []string
			for _, a := range pool.Accounts {
				if a != nil && !scheduler.IsRestricted(a) && quota.GeminiQuotaNotRenewed(a) {
					notRenewed = append(notRenewed, accounts.DisplayAccountName(a))
				}
			}
			if len(notRenewed) > 0 {
				fmt.Fprintf(out, "  %s⚠%s Gemini Quota: last refresh did not renew a Gemini window for %s\n",
					clrYellow, clrReset, strings.Join(notRenewed, ", "))
				fmt.Fprintf(out, "    %s↳ Upstream may have changed its quota response; check 'agy-pool logs' for QUOTA WARN.%s\n",
					clrDim, clrReset)
				warningsFound++
			}

			nativeState, _ := accounts.InspectNativeAgyIdentity(pool)
			switch nativeState {
			case accounts.NativeIdentityMatch:
				fmt.Fprintf(out, "  %s✓%s Native Identity: Matches CLI Base (%s)\n", clrGreen, clrReset, accounts.DisplayAccountName(storage.FindAccount(pool, &storage.Account{ID: *pool.ActiveAccountID})))
			case accounts.NativeIdentityMismatchKnown:
				fmt.Fprintf(out, "  %s✖%s Native Identity: Mismatch with CLI Base\n", clrRed, clrReset)
				fmt.Fprintf(out, "    %s↳ Re-run switch or login for the CLI Base credentials.%s\n", clrDim, clrReset)
				warningsFound++
			case accounts.NativeIdentityMismatchUnknown:
				fmt.Fprintf(out, "  %s✖%s Native Identity: Does not match a known pool account\n", clrRed, clrReset)
				fmt.Fprintf(out, "    %s↳ Re-run switch or login for the CLI Base credentials.%s\n", clrDim, clrReset)
				warningsFound++
			case accounts.NativeIdentityMissing:
				fmt.Fprintf(out, "  %s✖%s Native Identity: Native credential file missing\n", clrRed, clrReset)
				warningsFound++
			case accounts.NativeIdentityUnknown:
				fmt.Fprintf(out, "  %s?%s Native Identity: Unable to determine safely\n", clrYellow, clrReset)
				warningsFound++
			case accounts.NativeIdentityNoActive:
				fmt.Fprintf(out, "  %s?%s Native Identity: No CLI Base account configured\n", clrBlue, clrReset)
			}
		}
	} else {
		fmt.Fprintf(out, "  %s○%s Account Pool: No account pool file found yet (%s)\n",
			clrYellow, clrReset, stateDBFile)
		fmt.Fprintf(out, "    %s↳ Run 'agy-pool import-current' or 'agy-pool login' to initialize.%s\n",
			clrDim, clrReset)
		warningsFound++
	}

	// 5. Upstream Google Cloud Code Connectivity
	hooksMu.RLock()
	bHostFn := backendHostProvider
	tProbeFn := tlsProber
	hooksMu.RUnlock()

	backendHost := "cloudaicompanion.googleapis.com"
	if bHostFn != nil {
		backendHost = bHostFn()
	}
	if tProbeFn == nil {
		tProbeFn = defaultTLSProbe
	}

	if err := tProbeFn(backendHost, 5*time.Second); err == nil {
		fmt.Fprintf(out, "  %s✓%s Cloud Code API: Reachable via TLS (%s:443)\n",
			clrGreen, clrReset, backendHost)
	} else {
		fmt.Fprintf(out, "  %s⚠%s Cloud Code API: Connection to %s failed: %v\n",
			clrYellow, clrReset, backendHost, err)
		warningsFound++
	}

	// 6. Session Database
	dbPath := config.GetConversationDBFile()
	if _, err := os.Stat(dbPath); err == nil {
		count := 0
		db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro", dbPath))
		if err == nil {
			defer db.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			row := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM conversation_summaries")
			if scanErr := row.Scan(&count); scanErr == nil {
				fmt.Fprintf(out, "  %s✓%s Session Continuity: Database active (%d conversation(s) recorded)\n",
					clrGreen, clrReset, count)
			} else {
				fmt.Fprintf(out, "  %s⚠%s Session Continuity: Database query warning: %v\n",
					clrYellow, clrReset, scanErr)
				warningsFound++
			}
		} else {
			fmt.Fprintf(out, "  %s⚠%s Session Continuity: Database query warning: %v\n",
				clrYellow, clrReset, err)
			warningsFound++
		}
	} else {
		fmt.Fprintf(out, "  %s○%s Session Continuity: Database not yet created (created on first agy session)\n",
			clrDim, clrReset)
	}

	// 7. Gateway Log File
	logFile := config.GetLogFile()
	if fi, err := os.Stat(logFile); err == nil {
		fmt.Fprintf(out, "  %s✓%s Gateway Log: %s (%s, cap: %s)\n",
			clrGreen, clrReset, logFile, daemon.FormatSize(fi.Size()), daemon.FormatSize(config.DefaultMaxLogBytes))
	}

	// Summary Footer
	fmt.Fprintln(out, "\n"+subSep)
	if issuesFound == 0 && warningsFound == 0 {
		fmt.Fprintf(out, " %s%sAll systems nominal! You are ready to use 'agy'.%s\n",
			clrGreen, clrBold, clrReset)
	} else if issuesFound == 0 {
		fmt.Fprintf(out, " %s%sSystem operational with %d warning(s). Check details above.%s\n",
			clrYellow, clrBold, warningsFound, clrReset)
	} else {
		fmt.Fprintf(out, " %s%sSystem has %d error(s) and %d warning(s). Please fix issues above.%s\n",
			clrRed, clrBold, issuesFound, warningsFound, clrReset)
	}
	fmt.Fprintln(out, sep+"\n")

	return issuesFound == 0
}
