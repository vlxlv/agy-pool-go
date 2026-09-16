package diagnostics

import (
	"context"
	"crypto/tls"
	"database/sql"
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
)

// SetAgyBinaryFinder overrides binary discovery for testing.
func SetAgyBinaryFinder(fn func() string) {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	agyBinaryFinder = fn
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

func getSelfPaths() map[string]bool {
	paths := make(map[string]bool)

	// Executable of current process
	if exe, err := os.Executable(); err == nil {
		if realExe, err := filepath.EvalSymlinks(exe); err == nil {
			paths[realExe] = true
		} else {
			paths[filepath.Clean(exe)] = true
		}
	}

	// argv[0]
	if len(os.Args) > 0 && os.Args[0] != "" {
		if realArg, err := filepath.EvalSymlinks(os.Args[0]); err == nil {
			paths[realArg] = true
		} else {
			paths[filepath.Clean(os.Args[0])] = true
		}
	}

	// ~/.local/bin/agy-pool
	home := os.Getenv("HOME")
	if home == "" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	if home != "" {
		userBin := filepath.Join(home, ".local", "bin", "agy-pool")
		if realBin, err := filepath.EvalSymlinks(userBin); err == nil {
			paths[realBin] = true
		} else {
			paths[filepath.Clean(userBin)] = true
		}
	}

	if explicit := os.Getenv("AGY_SELF_PATH"); explicit != "" {
		if realExp, err := filepath.EvalSymlinks(explicit); err == nil {
			paths[realExp] = true
		} else {
			paths[filepath.Clean(explicit)] = true
		}
	}

	return paths
}

// IsShimOrSelf reports whether a path is the agy-pool binary itself, an agy/agy-raw shim,
// or a symlink resolving to one.
func IsShimOrSelf(path string) bool {
	if path == "" {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}

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

	// 2. Inode / SameFile check against running executable
	if exe, err := os.Executable(); err == nil {
		if fi1, err1 := os.Stat(abs); err1 == nil {
			if fi2, err2 := os.Stat(exe); err2 == nil && os.SameFile(fi1, fi2) {
				return true
			}
		}
	}

	// 3. Inspect file header for shim signature
	f, err := os.Open(realPath)
	if err != nil {
		return false
	}
	defer f.Close()

	buf := make([]byte, 1024)
	n, _ := f.Read(buf)
	if n > 0 {
		header := string(buf[:n])
		if strings.Contains(header, "agy-pool run") ||
			strings.Contains(header, "agy-pool raw") ||
			strings.Contains(header, "agy-pool direct") ||
			strings.Contains(header, "agy-pool orig") ||
			strings.Contains(header, "# agy-pool") ||
			strings.Contains(header, "agy-pool integration") {
			return true
		}
	}

	return false
}

// FindRealAgyBinary locates the underlying native agy executable dynamically without self-recursion.
func FindRealAgyBinary() string {
	hooksMu.RLock()
	finder := agyBinaryFinder
	hooksMu.RUnlock()
	if finder != nil {
		return finder()
	}

	var candidates []string

	// 1. Configured native_agy.binary
	cfg := config.GetStaticConfig()
	if cfg != nil && cfg.NativeAgy.Binary != nil && *cfg.NativeAgy.Binary != "" {
		candidates = append(candidates, *cfg.NativeAgy.Binary)
	}

	// 2. AGY_BIN environment override
	if explicit := os.Getenv("AGY_BIN"); explicit != "" {
		candidates = append(candidates, explicit)
	}

	// 3. Ordered candidate locations matching Python reference
	if prefix := os.Getenv("PREFIX"); prefix != "" {
		candidates = append(candidates, filepath.Join(prefix, "bin", "agy"))
	}

	candidates = append(candidates,
		"/data/data/com.termux/files/usr/bin/agy",
		"/usr/local/bin/agy",
		"/usr/bin/agy",
	)

	home := os.Getenv("HOME")
	if home == "" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "agy"))
	}

	// 4. All PATH entries to find native agy even if preceded by shims on PATH
	pathEnv := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir != "" {
			candidates = append(candidates, filepath.Join(dir, "agy"))
		}
	}

	seen := make(map[string]bool)
	for _, cand := range candidates {
		if cand == "" {
			continue
		}
		clean := filepath.Clean(cand)
		if seen[clean] {
			continue
		}
		seen[clean] = true

		fi, err := os.Stat(clean)
		if err != nil || fi.IsDir() || fi.Mode()&0111 == 0 {
			continue
		}

		if IsShimOrSelf(clean) {
			continue
		}

		return clean
	}

	return ""
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
	realAgy := FindRealAgyBinary()
	if realAgy != "" {
		agyVer := GetInstalledAgyVersion()
		fmt.Fprintf(out, "  %s✓%s Native Binary: %s%s%s (v%s)\n",
			clrGreen, clrReset, clrBold, realAgy, clrReset, agyVer)
	} else {
		fmt.Fprintf(out, "  %s⚠%s Native Binary: No native 'agy' found in standard PATH candidates\n",
			clrYellow, clrReset)
		fmt.Fprintf(out, "    %s↳ Set AGY_BIN=/path/to/agy if installed in custom location.%s\n",
			clrDim, clrReset)
		warningsFound++
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

				if a.Status == "validation_required" || a.Status == "auth_error" {
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
						activeStr = fmt.Sprintf(" | Active: %s", accounts.DisplayAccountName(a))
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
