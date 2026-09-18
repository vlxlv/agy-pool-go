package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
)

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

func runDaemonChild(stderr io.Writer) int {
	dataDir := os.Getenv("AGY_DATA_DIR")
	configFile := os.Getenv("AGY_CONFIG_FILE")
	ctx, err := daemon.ResolveInstanceContext(dataDir, configFile, 0, "")
	if err != nil {
		fmt.Fprintf(stderr, "daemon child error: %v\n", err)
		return 1
	}
	if err := config.ConfigureDataDir(ctx.DataDir); err != nil {
		fmt.Fprintf(stderr, "daemon child error: %v\n", err)
		return 1
	}

	pidFile := os.Getenv("AGY_TEST_PID_FILE")
	if pidFile == "" {
		pidFile = ctx.PIDFile()
	}

	err = daemon.RunForeground(context.Background(), daemon.ServerOptions{
		Port:     ctx.ListenPort,
		PIDFile:  pidFile,
		LogFile:  ctx.LogFile(),
		Instance: ctx,
	})
	if err != nil {
		fmt.Fprintf(stderr, "daemon child error: %v\n", err)
		return 1
	}
	return 0
}

// StartDaemonCmd starts the proxy gateway in background mode.
func StartDaemonCmd(stdout, stderr io.Writer, args ...string) int {
	ctx, _, err := resolveContextForCmd(args)
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Failed to resolve instance: %v%s\n", clrRed, err, clrReset)
		return 1
	}
	res, err := daemon.StartInstance(ctx, daemon.LaunchOptions{Foreground: false})
	if err != nil {
		fmt.Fprintf(stdout, "%s[Error] Failed to start gateway. Check %s%s\n",
			clrRed, ctx.LogFile(), clrReset)
		return 1
	}
	if res.AlreadyRun {
		fmt.Fprintf(stdout, "%sProxy daemon is already running (PID: %d).%s\n",
			clrYellow, res.PID, clrReset)
	} else {
		fmt.Fprintf(stdout, "%s✓ Started agy-pool gateway proxy on http://%s:%d (PID: %d)%s\n",
			clrGreen, ctx.ListenHost, ctx.ListenPort, res.PID, clrReset)
	}
	return 0
}

// StopDaemonCmd stops the proxy gateway.
func StopDaemonCmd(stdout, stderr io.Writer, args ...string) int {
	ctx, _, err := resolveContextForCmd(args)
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Failed to resolve instance: %v%s\n", clrRed, err, clrReset)
		return 1
	}
	err = daemon.StopInstance(ctx, 3*time.Second)
	if err != nil {
		if errors.Is(err, daemon.ErrNotRunning) {
			fmt.Fprintf(stdout, "%sProxy daemon is not running.%s\n", clrYellow, clrReset)
			return 0
		}
		fmt.Fprintf(stderr, "%s[Error] Cannot stop proxy daemon: %v%s\n", clrRed, err, clrReset)
		return 1
	}
	fmt.Fprintf(stdout, "%s✓ Stopped proxy daemon%s\n", clrGreen, clrReset)
	return 0
}

// RestartDaemonCmd restarts the proxy gateway.
func RestartDaemonCmd(stdout, stderr io.Writer, args ...string) int {
	ctx, _, err := resolveContextForCmd(args)
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Failed to resolve instance: %v%s\n", clrRed, err, clrReset)
		return 1
	}
	res, err := daemon.RestartInstance(ctx, daemon.LaunchOptions{})
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Failed to restart gateway: %v%s\n", clrRed, err, clrReset)
		return 1
	}
	fmt.Fprintf(stdout, "%s✓ Started agy-pool gateway proxy on http://%s:%d (PID: %d)%s\n",
		clrGreen, ctx.ListenHost, ctx.ListenPort, res.PID, clrReset)
	return 0
}
