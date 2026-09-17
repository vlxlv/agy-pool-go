package cli

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
)

func getFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func TestCLI_Status_MultiInstance(t *testing.T) {
	tmpDir := t.TempDir()
	cleanup := config.SetSyntheticSandboxRoot(tmpDir)
	defer cleanup()
	defer config.ResetDataDir()

	portA := getFreePort(t)
	portB := getFreePort(t)

	dataDirA := filepath.Join(tmpDir, "cli-data-a")
	dataDirB := filepath.Join(tmpDir, "cli-data-b")
	_ = os.MkdirAll(dataDirA, 0o700)
	_ = os.MkdirAll(dataDirB, 0o700)

	cfgA := filepath.Join(tmpDir, "cfg-a.json")
	cfgB := filepath.Join(tmpDir, "cfg-b.json")
	_ = os.WriteFile(cfgA, []byte(fmt.Sprintf(`{"version": 1, "server":{"port":%d}}`, portA)), 0o600)
	_ = os.WriteFile(cfgB, []byte(fmt.Sprintf(`{"version": 1, "server":{"port":%d}}`, portB)), 0o600)

	ctxA, _ := daemon.ResolveInstanceContext(dataDirA, cfgA, portA, "127.0.0.1")
	_, _ = daemon.ResolveInstanceContext(dataDirB, cfgB, portB, "127.0.0.1")

	// Both stopped initially
	var outA, errA bytes.Buffer
	codeA := StatusCmd(&outA, &errA, "-D", dataDirA, "-c", cfgA)
	if codeA != 0 {
		t.Fatalf("expected 0, got %d", codeA)
	}
	if !strings.Contains(outA.String(), "Gateway :") || !strings.Contains(outA.String(), "STOPPED") {
		t.Fatalf("expected STOPPED for A, got: %s", outA.String())
	}

	var outB, errB bytes.Buffer
	codeB := StatusCmd(&outB, &errB, "-D", dataDirB, "-c", cfgB)
	if codeB != 0 {
		t.Fatalf("expected 0, got %d", codeB)
	}
	if !strings.Contains(outB.String(), "Gateway :") || !strings.Contains(outB.String(), "STOPPED") {
		t.Fatalf("expected STOPPED for B, got: %s", outB.String())
	}

	// Write simulated running PID file for A
	pidInfoA := daemon.DaemonInfo{
		PID:        os.Getpid(),
		Version:    ctxA.Version,
		ConfigPath: ctxA.ConfigPath,
		ConfigHash: ctxA.ConfigHash,
		DataDir:    ctxA.DataDir,
		Port:       portA,
		ListenHost: "127.0.0.1",
	}
	_ = daemon.WritePIDFile(ctxA.PIDFile(), pidInfoA)

	// Simulate listening on portA
	lA, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", portA))
	if err != nil {
		t.Fatalf("failed to listen on portA: %v", err)
	}
	defer lA.Close()

	// Query status on A -> RUNNING
	outA.Reset()
	errA.Reset()
	codeA = StatusCmd(&outA, &errA, "-D", dataDirA, "-c", cfgA)
	if codeA != 0 {
		t.Fatalf("expected 0, got %d", codeA)
	}
	if !strings.Contains(outA.String(), "Gateway :") || !strings.Contains(outA.String(), "RUNNING") {
		t.Fatalf("expected RUNNING for A, got: %s", outA.String())
	}

	// Query status on B -> STILL STOPPED!
	outB.Reset()
	errB.Reset()
	codeB = StatusCmd(&outB, &errB, "-D", dataDirB, "-c", cfgB)
	if codeB != 0 {
		t.Fatalf("expected 0, got %d", codeB)
	}
	if !strings.Contains(outB.String(), "Gateway :") || !strings.Contains(outB.String(), "STOPPED") {
		t.Fatalf("expected STOPPED for B while A is running, got: %s", outB.String())
	}
}

func TestCLI_Stop_CrossInstanceRejection(t *testing.T) {
	tmpDir := t.TempDir()
	cleanup := config.SetSyntheticSandboxRoot(tmpDir)
	defer cleanup()
	defer config.ResetDataDir()

	port := getFreePort(t)
	dataDir := filepath.Join(tmpDir, "cli-stop-data")
	_ = os.MkdirAll(dataDir, 0o700)

	cfgA := filepath.Join(tmpDir, "cfg-a.json")
	cfgB := filepath.Join(tmpDir, "cfg-b.json")
	_ = os.WriteFile(cfgA, []byte(fmt.Sprintf(`{"version": 1, "server":{"port":%d}}`, port)), 0o600)
	_ = os.WriteFile(cfgB, []byte(fmt.Sprintf(`{"version": 1, "server":{"port":%d}}`, port)), 0o600)

	ctxA, _ := daemon.ResolveInstanceContext(dataDir, cfgA, port, "127.0.0.1")

	// Simulate running daemon with cfgA
	pidInfoA := daemon.DaemonInfo{
		PID:        os.Getpid(),
		Version:    ctxA.Version,
		ConfigPath: ctxA.ConfigPath,
		ConfigHash: ctxA.ConfigHash,
		DataDir:    ctxA.DataDir,
		Port:       port,
		ListenHost: "127.0.0.1",
	}
	_ = daemon.WritePIDFile(ctxA.PIDFile(), pidInfoA)

	// Attempt stop with cfgB -> REJECTED
	var stdout, stderr bytes.Buffer
	code := StopDaemonCmd(&stdout, &stderr, "-D", dataDir, "-c", cfgB)
	if code == 0 {
		t.Fatalf("expected non-zero exit code when stopping with wrong config, got %d", code)
	}
	if !strings.Contains(stderr.String(), "daemon configuration mismatch") && !strings.Contains(stderr.String(), "cannot stop with config") {
		t.Fatalf("expected config mismatch error, got: %s", stderr.String())
	}

	// PID file must still exist
	if _, err := os.Stat(ctxA.PIDFile()); err != nil {
		t.Fatal("PID file must not be removed on rejected stop")
	}
}
