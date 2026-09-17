package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDaemonChild_ProductionLifecycle tests the full production-shaped lifecycle
// using the real compiled agy-pool binary, verifying that the background daemon starts
// without any "daemon-run" subcommand, reaches RunForeground directly via the internal
// child environment marker, and properly supports status, restart, stop, and foreign port protection.
func TestDaemonChild_ProductionLifecycle(t *testing.T) {
	// J. Actual child lifecycle uses a temp config/data dir and dynamically allocated loopback port
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	configDir := filepath.Join(tmpDir, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}
	cfgFile := filepath.Join(configDir, "config.json")

	// Pick an available ephemeral port
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get ephemeral port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	// Write static config for the instance
	configJSON := fmt.Sprintf(`{
  "version": 1,
  "server": {
    "listen": "127.0.0.1",
    "port": %d
  },
  "scheduler": {
    "strategy": "max_quota"
  }
}`, port)
	if err := os.WriteFile(cfgFile, []byte(configJSON), 0o600); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	// Build the real agy-pool executable
	binPath := filepath.Join(tmpDir, "agy-pool-bin")
	buildCmd := exec.Command("go", "build", "-o", binPath, "../../cmd/agy-pool")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build real agy-pool binary: %v\nOutput: %s", err, string(out))
	}

	// Resolve instance context
	ctx, err := ResolveInstanceContext(dataDir, cfgFile, port, "127.0.0.1")
	if err != nil {
		t.Fatalf("failed to resolve instance context: %v", err)
	}

	// LaunchOptions without CustomArgs: exercises the real production exec/spawn boundary
	opts := LaunchOptions{
		EntrypointPath: binPath,
	}

	// A. `start` launches a real background child successfully
	res, err := StartInstance(ctx, opts)
	if err != nil {
		logContent, _ := os.ReadFile(ctx.LogFile())
		t.Fatalf("StartInstance failed: %v\nChild log:\n%s", err, string(logContent))
	}
	defer func() {
		_ = StopInstance(ctx, 3*time.Second)
	}()

	if res.PID <= 0 {
		t.Fatalf("expected valid child PID, got %d", res.PID)
	}
	if res.AlreadyRun {
		t.Fatalf("expected AlreadyRun to be false on initial start")
	}
	if !IsProcessAlive(res.PID) {
		t.Fatalf("expected child PID %d to be alive", res.PID)
	}

	// C. Readiness handshake succeeds
	if !IsPortListening(port) {
		t.Fatalf("expected port %d to be listening after readiness handshake", port)
	}
	pidInfo := GetDaemonInfo(ctx.PIDFile())
	if pidInfo == nil || pidInfo.PID != res.PID {
		t.Fatalf("expected PID file %s to record PID %d, got %+v", ctx.PIDFile(), res.PID, pidInfo)
	}

	// B. Child reaches RunForeground without any daemon-run command
	cmdlineBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", res.PID))
	if err == nil {
		cmdline := string(bytes.ReplaceAll(cmdlineBytes, []byte{0}, []byte(" ")))
		if strings.Contains(cmdline, "daemon-run") {
			t.Fatalf("child process cmdline contains 'daemon-run': %q", cmdline)
		}
	}

	// H & I: Verify child process environment from /proc/<pid>/environ
	environBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", res.PID))
	if err == nil {
		envParts := strings.Split(string(environBytes), "\x00")
		envMap := make(map[string]string)
		for _, part := range envParts {
			if idx := strings.Index(part, "="); idx != -1 {
				envMap[part[:idx]] = part[idx+1:]
			}
		}

		// I. start child environment contains the expected:
		// AGY_DATA_DIR, AGY_CONFIG_FILE, daemon-child marker
		if envMap[EnvDaemonChild] != "1" {
			t.Fatalf("expected %s=1 in child environ, got %q", EnvDaemonChild, envMap[EnvDaemonChild])
		}
		if envMap["AGY_DATA_DIR"] != ctx.DataDir {
			t.Fatalf("expected AGY_DATA_DIR=%q in child environ, got %q", ctx.DataDir, envMap["AGY_DATA_DIR"])
		}
		if envMap["AGY_CONFIG_FILE"] != ctx.ConfigPath {
			t.Fatalf("expected AGY_CONFIG_FILE=%q in child environ, got %q", ctx.ConfigPath, envMap["AGY_CONFIG_FILE"])
		}

		// H. AGY_GEMINI_DIR is not rewritten to pool data dir
		if val, exists := envMap["AGY_GEMINI_DIR"]; exists && val == ctx.DataDir {
			t.Fatalf("AGY_GEMINI_DIR was incorrectly rewritten to pool data dir %q", val)
		}
	}

	// D. `status` identifies same instance
	status, info, msg := CheckInstanceStatus(ctx)
	if status != StatusRunningSameInstance {
		t.Fatalf("expected StatusRunningSameInstance, got %s (msg: %s)", status, msg)
	}
	if info == nil || info.PID != res.PID {
		t.Fatalf("expected status PID %d, got %+v", res.PID, info)
	}

	// F. `restart` works
	resRestart, err := RestartInstance(ctx, LaunchOptions{EntrypointPath: binPath})
	if err != nil {
		logContent, _ := os.ReadFile(ctx.LogFile())
		t.Fatalf("RestartInstance failed: %v\nLog:\n%s", err, string(logContent))
	}
	if !resRestart.WasRestarted {
		t.Fatalf("expected WasRestarted == true")
	}
	if resRestart.PID <= 0 || resRestart.PID == res.PID {
		t.Fatalf("expected new PID after restart, got %d (old was %d)", resRestart.PID, res.PID)
	}
	if !IsPortListening(port) {
		t.Fatalf("expected port %d to be listening after restart", port)
	}
	if !IsProcessAlive(resRestart.PID) {
		t.Fatalf("expected new PID %d to be alive", resRestart.PID)
	}
	if IsProcessAlive(res.PID) {
		t.Fatalf("expected old PID %d to be terminated", res.PID)
	}

	// E. `stop` stops only same instance
	if err := StopInstance(ctx, 5*time.Second); err != nil {
		t.Fatalf("StopInstance failed: %v", err)
	}
	if IsPortListening(port) {
		t.Fatalf("expected port %d to be free after stop", port)
	}
	if IsProcessAlive(resRestart.PID) {
		t.Fatalf("expected PID %d to be stopped", resRestart.PID)
	}
	if _, err := os.Stat(ctx.PIDFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected PID file to be removed after stop, but it still exists")
	}

	// G. Foreign port/process remains fail-closed
	foreignListener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("failed to bind foreign listener: %v", err)
	}
	defer foreignListener.Close()

	_, err = StartInstance(ctx, opts)
	if err == nil {
		t.Fatalf("expected StartInstance to fail when port is occupied by foreign process, but it succeeded")
	}
	if !errors.Is(err, ErrForeignPortOccupied) {
		t.Fatalf("expected ErrForeignPortOccupied, got: %v", err)
	}

	// Verify foreign listener was not terminated
	if !IsPortListening(port) {
		t.Fatalf("foreign listener should remain untouched and listening")
	}
}
