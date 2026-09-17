package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallCommand_PreconditionFailure(t *testing.T) {
	tempDir, cleanup := setupCLITestEnv(t)
	defer cleanup()

	targetDir := filepath.Join(tempDir, "bin")
	nonExistentAgy := filepath.Join(tempDir, "no_such_agy")

	var stdout, stderr bytes.Buffer
	code := Main([]string{"install", "--target", targetDir, "--native-agy", nonExistentAgy, "--skip-rc"}, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1 on missing native agy, got %d. stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Native agy was not found") {
		t.Errorf("expected error message about native agy, got: %s", stderr.String())
	}
}

func TestInstallAndUninstallCommand_UserMode(t *testing.T) {
	tempDir, cleanup := setupCLITestEnv(t)
	defer cleanup()

	targetBin := filepath.Join(tempDir, "usr_bin")
	configDir := filepath.Join(tempDir, "cfg")
	dataDir := filepath.Join(tempDir, "data")
	realAgyDir := filepath.Join(tempDir, "real_bin")
	if err := os.MkdirAll(realAgyDir, 0755); err != nil {
		t.Fatalf("failed to create realAgyDir: %v", err)
	}

	mockNativeAgy := filepath.Join(realAgyDir, "agy")
	if err := os.WriteFile(mockNativeAgy, []byte("#!/bin/sh\necho \"native agy\"\n"), 0755); err != nil {
		t.Fatalf("failed to write mock native agy: %v", err)
	}

	// 1. Install
	var stdout, stderr bytes.Buffer
	code := Main([]string{
		"install",
		"--user",
		"--target", targetBin,
		"--native-agy", mockNativeAgy,
		"--config-dir", configDir,
		"--data-dir", dataDir,
		"--skip-rc",
	}, nil, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("install failed with exit code %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Successfully installed agy-pool") {
		t.Errorf("expected success message in stdout, got: %s", stdout.String())
	}

	// Verify installed files
	shimPath := filepath.Join(targetBin, "agy")
	shimBytes, err := os.ReadFile(shimPath)
	if err != nil {
		t.Fatalf("failed to read installed shim: %v", err)
	}
	if !strings.Contains(string(shimBytes), "agy-pool run") {
		t.Errorf("expected shim to run agy-pool, got: %s", string(shimBytes))
	}

	rawPath := filepath.Join(targetBin, "agy-raw")
	if _, err := os.Stat(rawPath); err != nil {
		t.Fatalf("agy-raw was not created: %v", err)
	}

	cfgPath := filepath.Join(configDir, "config.json")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("config.json was not created: %v", err)
	}

	locksDir := filepath.Join(dataDir, "locks")
	if fi, err := os.Stat(locksDir); err != nil || !fi.IsDir() {
		t.Fatalf("data locks directory was not created: %v", err)
	}

	// 2. Uninstall
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"uninstall", "--target", targetBin}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("uninstall failed with exit code %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Successfully uninstalled agy-pool") {
		t.Errorf("expected success message in stdout, got: %s", stdout.String())
	}

	// Shim in targetBin should be removed
	if _, err := os.Stat(shimPath); !os.IsNotExist(err) {
		t.Errorf("shim at %s was not removed during uninstall", shimPath)
	}

	// Real native agy must still exist!
	if _, err := os.Stat(mockNativeAgy); err != nil {
		t.Errorf("CRITICAL: real native agy was deleted by uninstall!")
	}

	// Config and data dir must remain intact
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("user config.json was unexpectedly deleted by uninstall")
	}
	if _, err := os.Stat(locksDir); err != nil {
		t.Errorf("user data locks dir was unexpectedly deleted by uninstall")
	}
}

func TestInstallAndUninstall_InvalidArgs(t *testing.T) {
	_, cleanup := setupCLITestEnv(t)
	defer cleanup()

	var stdout, stderr bytes.Buffer

	// Missing flag argument
	code := Main([]string{"install", "--prefix"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Errorf("expected code 2 for install --prefix without value, got %d", code)
	}

	// Unknown argument
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"install", "--unknown-flag"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Errorf("expected code 2 for install --unknown-flag, got %d", code)
	}

	// Help flag
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"install", "--help"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected code 0 for install --help, got %d", code)
	}
	if !strings.Contains(stdout.String(), "usage: agy-pool install") {
		t.Errorf("expected usage in help output, got: %s", stdout.String())
	}

	// Uninstall missing flag argument
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"uninstall", "--target"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Errorf("expected code 2 for uninstall --target without value, got %d", code)
	}

	// Uninstall unknown argument
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"uninstall", "--bogus"}, nil, &stdout, &stderr)
	if code != 2 {
		t.Errorf("expected code 2 for uninstall --bogus, got %d", code)
	}

	// Uninstall help flag
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"uninstall", "-h"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected code 0 for uninstall -h, got %d", code)
	}
	if !strings.Contains(stdout.String(), "usage: agy-pool uninstall") {
		t.Errorf("expected usage in help output, got: %s", stdout.String())
	}
}
