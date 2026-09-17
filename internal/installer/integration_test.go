package installer

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegration_AgyManagedShimAndRawBypass(t *testing.T) {
	tempDir, cleanup := setupInstallerTestEnv(t)
	defer cleanup()

	// 1. Allocate ephemeral port for test
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate ephemeral port: %v", err)
	}
	testPort := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	userHome := filepath.Join(tempDir, "int_user_home")
	targetBin := filepath.Join(userHome, ".local", "bin")
	libexecDir := filepath.Join(userHome, ".local", "libexec", "agy-pool")
	cfgDir := filepath.Join(userHome, ".config", "agy-pool")
	dataDir := filepath.Join(userHome, ".local", "share", "agy-pool")

	_ = os.MkdirAll(targetBin, 0755)
	_ = os.MkdirAll(cfgDir, 0755)
	_ = os.MkdirAll(dataDir, 0755)

	t.Setenv("HOME", userHome)
	t.Setenv("PATH", targetBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Create test config.json with ephemeral testPort
	cfgFile := filepath.Join(cfgDir, "config.json")
	cfgContent := fmt.Sprintf(`{
  "version": 1,
  "server": {
    "listen": "127.0.0.1",
    "port": %d
  },
  "scheduler": {
    "strategy": "max_quota"
  },
  "native_agy": {
    "binary": null
  },
  "logging": {
    "max_size_bytes": 5242880,
    "backup_count": 1
  }
}
`, testPort)
	if err := os.WriteFile(cfgFile, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("failed to write config.json: %v", err)
	}

	// 2. Setup log paths for fake native agy
	envLog := filepath.Join(tempDir, "env.log")
	argsLog := filepath.Join(tempDir, "args.log")

	// Fake native agy executable initially placed at targetBin/agy (simulating pre-install state)
	initialNativeAgy := filepath.Join(targetBin, "agy")
	fakeNativeScript := fmt.Sprintf(`#!/bin/sh
printf "%%s" "$CLOUD_CODE_URL" > %q
: > %q
for arg in "$@"; do
  printf "%%s\n" "$arg" >> %q
done
exit 0
`, envLog, argsLog, argsLog)

	if err := os.WriteFile(initialNativeAgy, []byte(fakeNativeScript), 0755); err != nil {
		t.Fatalf("failed to write fake native agy: %v", err)
	}

	// 3. Setup staged agy-pool binary that executes the run/raw protocol
	sourcePoolBin := filepath.Join(tempDir, "source-agy-pool")
	stagedPoolScript := `#!/bin/sh
cmd="$1"
shift
if [ "$cmd" = "run" ]; then
  cfgFile=""
  while [ $# -gt 0 ]; do
    case "$1" in
      -c)
        cfgFile="$2"
        shift 2
        ;;
      --)
        shift
        break
        ;;
      *)
        shift
        ;;
    esac
  done
  port=""
  if [ -n "$cfgFile" ] && [ -f "$cfgFile" ]; then
    port=$(grep -o '"port":[ ]*[0-9]*' "$cfgFile" | head -n1 | tr -dc '0-9')
  fi
  if [ -z "$port" ]; then
    port="8899"
  fi
  export CLOUD_CODE_URL="http://127.0.0.1:${port}"
  managed="$(dirname "$0")/../libexec/agy-pool/agy-native"
  exec "$managed" "$@"
elif [ "$cmd" = "raw" ]; then
  while [ $# -gt 0 ]; do
    if [ "$1" = "--" ]; then
      shift
      break
    fi
    shift
  done
  unset CLOUD_CODE_URL
  managed="$(dirname "$0")/../libexec/agy-pool/agy-native"
  exec "$managed" "$@"
else
  echo "Unknown command: $cmd" >&2
  exit 1
fi
`
	if err := os.WriteFile(sourcePoolBin, []byte(stagedPoolScript), 0755); err != nil {
		t.Fatalf("failed to write source agy-pool: %v", err)
	}

	// 4. Run installer into user root
	opts := Options{
		SourceBinary: sourcePoolBin,
		TargetDir:    targetBin,
		HomeDir:      userHome,
		ConfigDir:    cfgDir,
		DataDir:      dataDir,
	}

	if err := Install(opts); err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	// 5. Verify installed artifacts
	installedAgy := filepath.Join(targetBin, "agy")
	installedPool := filepath.Join(targetBin, "agy-pool")
	installedRaw := filepath.Join(targetBin, "agy-raw")
	installedOrig := filepath.Join(targetBin, "agy-orig")
	managedNative := filepath.Join(libexecDir, "agy-native")

	for _, p := range []string{installedAgy, installedPool, installedRaw, installedOrig, managedNative} {
		if fi, err := os.Lstat(p); err != nil {
			t.Fatalf("expected installed file %s to exist: %v", p, err)
		} else if fi.Mode()&os.ModeSymlink == 0 && fi.Mode()&0111 == 0 {
			t.Fatalf("expected installed file %s to be executable", p)
		}
	}

	// Verify agy is the managed shim
	shimBytes, _ := os.ReadFile(installedAgy)
	if !strings.Contains(string(shimBytes), ManagedShimMarker) {
		t.Errorf("installed agy is not a managed shim: %s", string(shimBytes))
	}

	// 6. Invoke managed 'agy' shim with test arguments
	testArgs := []string{"--mode", "accept-edits", "--dangerously-skip-permissions", "-p", "hello test"}
	cmd := exec.Command(installedAgy, testArgs...)
	cmd.Env = append(os.Environ(), "HOME="+userHome, "PATH="+targetBin+":"+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("running installed agy shim failed: %v (stderr: %s)", err, stderr.String())
	}

	// Verify CLOUD_CODE_URL=http://127.0.0.1:<test-port> was injected
	expectedURL := fmt.Sprintf("http://127.0.0.1:%d", testPort)
	actualURL, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatalf("failed to read envLog: %v", err)
	}
	if string(actualURL) != expectedURL {
		t.Errorf("expected CLOUD_CODE_URL %q, got %q", expectedURL, string(actualURL))
	}

	// Verify arguments reached fake native agy unchanged
	argsData, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("failed to read argsLog: %v", err)
	}
	receivedArgs := strings.Split(strings.TrimSpace(string(argsData)), "\n")
	if len(receivedArgs) != len(testArgs) {
		t.Fatalf("expected %d args, got %d: %v", len(testArgs), len(receivedArgs), receivedArgs)
	}
	for i, arg := range testArgs {
		if receivedArgs[i] != arg {
			t.Errorf("arg[%d] mismatch: expected %q, got %q", i, arg, receivedArgs[i])
		}
	}

	// 7. Invoke 'agy-raw' shim and verify CLOUD_CODE_URL is absent
	_ = os.WriteFile(envLog, []byte("stale-url"), 0644)
	rawArgs := []string{"--raw-check", "arg1"}
	rawCmd := exec.Command(installedRaw, rawArgs...)
	rawCmd.Env = append(os.Environ(), "HOME="+userHome, "CLOUD_CODE_URL=http://should-be-stripped", "PATH="+targetBin+":"+os.Getenv("PATH"))
	stdout.Reset()
	stderr.Reset()
	rawCmd.Stdout = &stdout
	rawCmd.Stderr = &stderr

	if err := rawCmd.Run(); err != nil {
		t.Fatalf("running agy-raw shim failed: %v (stderr: %s)", err, stderr.String())
	}

	actualRawURL, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatalf("failed to read envLog after raw: %v", err)
	}
	if len(actualRawURL) != 0 {
		t.Errorf("expected CLOUD_CODE_URL to be absent in agy-raw, got: %q", string(actualRawURL))
	}

	// 8. Invoke 'agy-orig' symlink and verify same raw bypass behavior
	_ = os.WriteFile(envLog, []byte("stale-url"), 0644)
	origArgs := []string{"--orig-check"}
	origCmd := exec.Command(installedOrig, origArgs...)
	origCmd.Env = append(os.Environ(), "HOME="+userHome, "CLOUD_CODE_URL=http://should-be-stripped", "PATH="+targetBin+":"+os.Getenv("PATH"))
	stdout.Reset()
	stderr.Reset()
	origCmd.Stdout = &stdout
	origCmd.Stderr = &stderr

	if err := origCmd.Run(); err != nil {
		t.Fatalf("running agy-orig shim failed: %v (stderr: %s)", err, stderr.String())
	}

	actualOrigURL, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatalf("failed to read envLog after orig: %v", err)
	}
	if len(actualOrigURL) != 0 {
		t.Errorf("expected CLOUD_CODE_URL to be absent in agy-orig, got: %q", string(actualOrigURL))
	}
}
