package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/proxy"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// QuotaRefresher can be set by callers to perform background quota refreshes.
var QuotaRefresher func(account *storage.Account) = proxy.QuotaRefresher

// ServerOptions configures the foreground daemon HTTP server.
type ServerOptions struct {
	Port                 int
	EphemeralPort        bool
	PIDFile              string
	LogFile              string
	Handler              http.Handler
	ReadyChan            chan struct{}
	BoundPortChan        chan int
	QuotaRefreshInterval time.Duration
	DisableSignals       bool
	Instance             *InstanceContext
}

// RunForeground runs the agy-pool reverse proxy server in the foreground,
// acquiring the PID lock, writing PID metadata, and shutting down gracefully on signal or context cancellation.
func RunForeground(ctx context.Context, opts ServerOptions) error {
	ctxInst := opts.Instance
	if ctxInst == nil {
		dataDir := config.GetDataDir()
		if opts.PIDFile != "" {
			dataDir = filepath.Dir(opts.PIDFile)
		}
		var err error
		ctxInst, err = ResolveInstanceContext(dataDir, "", opts.Port, "")
		if err != nil {
			return err
		}
	}

	pidFile := opts.PIDFile
	if pidFile == "" {
		pidFile = ctxInst.PIDFile()
	}
	if err := config.AssertSafeWritePath(pidFile); err != nil {
		return err
	}

	port := opts.Port
	if port <= 0 && !opts.EphemeralPort {
		port = ctxInst.ListenPort
	}

	// Fail-closed test safety guard: never bind 8899 or 8901 in test mode!
	if config.IsTestMode() && (port == config.DefaultPort || port == 8899 || port == 8901) {
		return fmt.Errorf("[FAIL-CLOSED TEST GUARD] refusing to bind production port %d during test mode", port)
	}

	handler := opts.Handler
	if handler == nil {
		handler = proxy.NewHandler()
	}

	refreshInterval := opts.QuotaRefreshInterval
	if refreshInterval <= 0 {
		refreshInterval = 180 * time.Second
	}

	// 1. Acquire exclusive non-blocking PID lock
	unlock, err := TryAcquirePIDLock(pidFile)
	if err != nil {
		return err
	}
	defer unlock()

	// 2. Open TCP listener
	addr := fmt.Sprintf("%s:%d", ctxInst.ListenHost, port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("%w: failed to listen on %s: %v", ErrForeignPortOccupied, addr, err)
	}
	defer listener.Close()

	actualPort := listener.Addr().(*net.TCPAddr).Port
	if opts.BoundPortChan != nil {
		opts.BoundPortChan <- actualPort
	}

	// 3. Write PID metadata atomically with 0600 mode
	currExe := ctxInst.BinaryPath
	var scriptMtime int64
	if st, err := os.Stat(currExe); err == nil {
		scriptMtime = st.ModTime().Unix()
	} else {
		scriptMtime = time.Now().Unix()
	}

	currentPID := os.Getpid()
	pidInfo := DaemonInfo{
		PID:         currentPID,
		Version:     ctxInst.Version,
		ScriptMtime: scriptMtime,
		ConfigPath:  ctxInst.ConfigPath,
		ConfigHash:  ctxInst.ConfigHash,
		DataDir:     ctxInst.DataDir,
		ListenHost:  ctxInst.ListenHost,
		Listen:      ctxInst.ListenHost,
		Port:        actualPort,
	}
	if err := WritePIDFile(pidFile, pidInfo); err != nil {
		return fmt.Errorf("failed to write pid file: %w", err)
	}
	defer func() {
		_, _ = RemovePIDFileIfOwned(pidFile, currentPID)
	}()

	// 4. Setup signal handling if not disabled
	serverCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if !opts.DisableSignals {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
		go func() {
			select {
			case <-sigChan:
				cancel()
			case <-serverCtx.Done():
			}
			signal.Stop(sigChan)
		}()
	}

	// 5. Start background quota refresher (matching Python lines 405-411)
	refresherDone := make(chan struct{})
	go func() {
		defer close(refresherDone)
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-serverCtx.Done():
				return
			case <-ticker.C:
				pool, err := storage.LoadPool()
				if err == nil && pool != nil {
					now := float64(time.Now().Unix())
					for _, acc := range pool.Accounts {
						if quota.RefreshNeeded(acc, now) {
							if QuotaRefresher != nil {
								QuotaRefresher(acc)
							}
						}
					}
				}
			}
		}
	}()

	// 6. Create HTTP server
	server := &http.Server{
		Handler: handler,
	}

	// 7. Signal readiness if requested
	var once sync.Once
	if opts.ReadyChan != nil {
		once.Do(func() {
			close(opts.ReadyChan)
		})
	}

	// 8. Serve connections in goroutine
	serveErrChan := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErrChan <- err
		} else {
			serveErrChan <- nil
		}
	}()

	// 9. Wait for termination or error
	select {
	case err := <-serveErrChan:
		return err
	case <-serverCtx.Done():
		// Graceful shutdown with 5s timeout
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
		<-refresherDone
		return nil
	}
}

// IsPortListening reports whether a TCP connect to 127.0.0.1:port succeeds within 300ms.
func IsPortListening(port int) bool {
	if port <= 0 {
		port = DefaultPortProvider()
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return true
	}
	return false
}

// IsDaemonRunning returns true if a valid agy-pool process is alive and its port is listening.
func IsDaemonRunning(pidFile string, port int) bool {
	if pidFile == "" {
		pidFile = PIDFileProvider()
	}
	info := GetDaemonInfo(pidFile)
	if info == nil || info.PID <= 0 {
		return false
	}
	if port <= 0 {
		port = DefaultPortProvider()
	}
	return IsPortListening(port)
}

// IsDaemonOutdated checks if the running daemon has an older version or older binary mtime.
func IsDaemonOutdated(pidFile string, port int) bool {
	if pidFile == "" {
		pidFile = PIDFileProvider()
	}
	info := GetDaemonInfo(pidFile)
	if info == nil {
		return false
	}
	if port <= 0 {
		port = DefaultPortProvider()
	}
	if !IsPortListening(port) {
		return false
	}
	if info.Version == "" || info.Version != VersionProvider() {
		return true
	}

	currExe := GetEntrypointPath()
	if st, err := os.Stat(currExe); err == nil {
		currMtime := st.ModTime().Unix()
		if info.ScriptMtime > 0 && currMtime > info.ScriptMtime {
			return true
		}
	}
	return false
}
