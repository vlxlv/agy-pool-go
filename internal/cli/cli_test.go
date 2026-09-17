package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
	"github.com/vlxlv/agy-pool-go/internal/diagnostics"
	"github.com/vlxlv/agy-pool-go/internal/installer"
)

func setupCLITestEnv(t *testing.T) (string, func()) {
	t.Helper()
	tempDir := t.TempDir()

	stateDir := filepath.Join(tempDir, ".gemini")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatalf("failed to create state dir: %v", err)
	}

	origState := config.GetStateDir()
	origTestMode := config.IsTestMode()
	config.SetTestMode(true)
	resetSandbox := config.SetSyntheticSandboxRoot(tempDir)
	config.SetNativeAgyDir(stateDir)
	if err := config.ConfigureStateDir(stateDir); err != nil {
		t.Fatalf("ConfigureStateDir failed: %v", err)
	}

	testHome := filepath.Join(tempDir, "test_home")
	_ = os.MkdirAll(filepath.Join(testHome, ".local", "bin"), 0755)
	_ = os.MkdirAll(filepath.Join(testHome, ".config", "agy-pool"), 0755)
	_ = os.MkdirAll(filepath.Join(testHome, ".local", "share", "agy-pool"), 0755)
	resetRoots := installer.SetTestInstallerRoots(&installer.InstallerRoots{
		UserHome:   testHome,
		BinDir:     filepath.Join(testHome, ".local", "bin"),
		ConfigDir:  filepath.Join(testHome, ".config", "agy-pool"),
		DataDir:    filepath.Join(testHome, ".local", "share", "agy-pool"),
		SystemRoot: filepath.Join(tempDir, "test_system"),
	})

	origExec := ExecHandler
	origEntrypoint := daemon.EntrypointProvider
	daemon.EntrypointProvider = func() string { return "/bin/true" }

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			resetRoots()
			resetSandbox()
			daemon.EntrypointProvider = origEntrypoint
			ExecHandler = origExec
			config.SetNativeAgyDir("")
			if origState != "" {
				if _, err := os.Stat(origState); err == nil {
					_ = config.ConfigureStateDir(origState)
				} else {
					config.ResetDataDir()
				}
			} else {
				config.ResetDataDir()
			}
			config.SetTestMode(origTestMode)
			diagnostics.SetAgyBinaryFinder(nil)
			diagnostics.SetAgyVersionProvider(nil)
			diagnostics.SetTLSProber(nil)
		})
	}
	t.Cleanup(cleanup)

	return tempDir, cleanup
}

func TestVersionAndHelp(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	for _, vFlag := range [][]string{{"--version"}, {"-v"}, {"version"}} {
		var stdout, stderr bytes.Buffer
		code := Main(vFlag, nil, &stdout, &stderr)
		if code != 0 {
			t.Errorf("%v expected exit 0, got %d", vFlag, code)
		}
		if !strings.HasPrefix(stdout.String(), "agy-pool ") {
			t.Errorf("%v expected 'agy-pool <ver>', got %q", vFlag, stdout.String())
		}
	}

	for _, hFlag := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		var stdout, stderr bytes.Buffer
		code := Main(hFlag, nil, &stdout, &stderr)
		if code != 0 {
			t.Errorf("%v expected exit 0, got %d", hFlag, code)
		}
		if !strings.Contains(stdout.String(), "usage: agy-pool") {
			t.Errorf("%v expected usage text, got %q", hFlag, stdout.String())
		}
	}
}

func TestUnknownCommandAndInvalidChoice(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	var stdout, stderr bytes.Buffer
	code := Main([]string{"nonexistent_command"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit code 2 on unknown command, got %d", code)
	}
	if !strings.Contains(stderr.String(), "invalid choice: 'nonexistent_command'") {
		t.Errorf("expected invalid choice error on stderr, got: %s", stderr.String())
	}
}

func TestDoctorCommand(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	diagnostics.SetAgyBinaryFinder(func() string { return "/bin/true" })
	diagnostics.SetTLSProber(func(host string, timeout time.Duration) error { return nil })

	var stdout, stderr bytes.Buffer
	code := Main([]string{"doctor"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doctor expected 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "Antigravity System Doctor") {
		t.Errorf("expected doctor header, got: %s", stdout.String())
	}
}

func TestDoctorCmd_BehaviorUnchanged(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	var stdout, stderr bytes.Buffer
	code := Main([]string{"doctor"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 on doctor, got %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Antigravity System Doctor") {
		t.Errorf("doctor output missing header, got: %s", out)
	}
	if !strings.Contains(out, "Native Binary:") {
		t.Errorf("doctor output missing Native Binary check, got: %s", out)
	}
}
