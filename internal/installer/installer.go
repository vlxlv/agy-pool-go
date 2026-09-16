package installer

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/diagnostics"
)

const (
	MarkerStart = "# >>> agy-pool integration >>>"
	MarkerEnd   = "# <<< agy-pool integration <<<"

	AliasBlock = "\n# >>> agy-pool integration >>>\nalias agy='agy-pool run --'\nalias agy-orig='agy-raw'\n# <<< agy-pool integration <<<\n"
)

// UserAgyShimContent is the thin POSIX shell shim for user-mode agy integration.
const UserAgyShimContent = `#!/bin/sh
# agy: agy-pool integration shim
CONFIG_FILE="${AGY_CONFIG_FILE:-$HOME/.config/agy-pool/config.json}"
DATA_DIR="${AGY_DATA_DIR:-$HOME/.local/share/agy-pool}"
exec agy-pool run -c "$CONFIG_FILE" -D "$DATA_DIR" -- "$@"
`

// SystemAgyShimContent is the thin POSIX shell shim for system-mode agy integration.
const SystemAgyShimContent = `#!/bin/sh
# agy: agy-pool integration shim
CONFIG_FILE="${AGY_CONFIG_FILE:-/etc/agy-pool/config.json}"
DATA_DIR="${AGY_DATA_DIR:-/var/lib/agy-pool}"
exec agy-pool run -c "$CONFIG_FILE" -D "$DATA_DIR" -- "$@"
`

// AgyRawShimContent is the thin POSIX shell shim invoking agy-pool's raw/direct execution.
const AgyRawShimContent = `#!/bin/sh
# agy-raw: agy-pool raw mode shim
exec agy-pool raw -- "$@"
`

// Legacy RawScriptContent retained for backwards-compatibility reference.
const RawScriptContent = AgyRawShimContent

type Options struct {
	SourceBinary  string
	TargetDir     string
	Prefix        string
	HomeDir       string
	IsSystem      bool
	ConfigDir     string
	DataDir       string
	NativeAgyPath string
	SkipShellRC   bool
	Stdout        io.Writer
	Stderr        io.Writer
}

// DetermineTargetDir resolves the destination bin directory following Python precedence.
func DetermineTargetDir(prefix, home string, isUsrLocalWritable func() bool) string {
	if prefix != "" {
		pBin := filepath.Join(prefix, "bin")
		if fi, err := os.Stat(pBin); err == nil && fi.IsDir() {
			return pBin
		}
	}

	if isUsrLocalWritable != nil && isUsrLocalWritable() {
		return "/usr/local/bin"
	}

	if home != "" {
		userBin := filepath.Join(home, ".local", "bin")
		if err := os.MkdirAll(userBin, 0755); err == nil {
			return userBin
		}
	}

	return "/usr/local/bin"
}

// AssertSafeInstallTarget guards against accidentally writing into forbidden host locations in tests.
func AssertSafeInstallTarget(targetDir string) error {
	if !config.IsTestMode() {
		return nil
	}
	clean := filepath.Clean(targetDir)
	if clean == "/usr/local/bin" || clean == "/usr/bin" {
		return fmt.Errorf("[FAIL-CLOSED GUARD] installer test refused write to host system path: %s", clean)
	}
	realProd := config.GetRealProductionGeminiDir()
	if strings.HasPrefix(clean, realProd) {
		return fmt.Errorf("[FAIL-CLOSED GUARD] installer test refused write into production gemini dir: %s", clean)
	}
	return nil
}

// CopyFile copies src to dst and ensures mode 0755 permissions.
func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	// Write to temp file then rename for atomic replacement
	tmpDst := dst + ".tmp"
	out, err := os.OpenFile(tmpDst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(tmpDst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpDst)
		return err
	}

	return os.Rename(tmpDst, dst)
}

// Install copies binaries, creates symlinks, and sets up shell aliases.
func Install(opts Options) error {
	out := opts.Stdout
	if out == nil {
		out = io.Discard
	}

	// 1. Precondition: discover real native agy
	nativeAgy := opts.NativeAgyPath
	if nativeAgy == "" {
		nativeAgy = diagnostics.FindRealAgyBinary()
	}
	fi, err := os.Stat(nativeAgy)
	if err != nil || fi.IsDir() || (fi.Mode()&0111) == 0 || diagnostics.IsShimOrSelf(nativeAgy) {
		return errors.New("Native agy was not found.\nInstall the original Antigravity CLI first, then rerun agy-pool installer.")
	}
	fmt.Fprintf(out, "Found native agy: %s\n", nativeAgy)

	prefix := opts.Prefix
	if prefix == "" {
		prefix = os.Getenv("PREFIX")
	}
	home := opts.HomeDir
	if home == "" {
		home = os.Getenv("HOME")
	}

	targetDir := opts.TargetDir
	if targetDir == "" {
		if opts.IsSystem {
			targetDir = "/usr/local/bin"
		} else {
			targetDir = DetermineTargetDir(prefix, home, func() bool {
				// Probe /usr/local/bin writability
				testFile := "/usr/local/bin/.agy_write_probe"
				if f, err := os.OpenFile(testFile, os.O_CREATE|os.O_WRONLY, 0600); err == nil {
					f.Close()
					_ = os.Remove(testFile)
					return true
				}
				return false
			})
		}
	}

	if err := AssertSafeInstallTarget(targetDir); err != nil {
		return err
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("failed to create target directory %s: %w", targetDir, err)
	}

	// 2. Install agy-pool binary
	dstPool := filepath.Join(targetDir, "agy-pool")
	if opts.SourceBinary != "" {
		if err := CopyFile(opts.SourceBinary, dstPool); err != nil {
			return fmt.Errorf("failed to copy binary to %s: %w", dstPool, err)
		}
	}

	// 3. Install thin agy shim
	dstAgy := filepath.Join(targetDir, "agy")
	agyContent := UserAgyShimContent
	if opts.IsSystem {
		agyContent = SystemAgyShimContent
	}
	if err := os.WriteFile(dstAgy, []byte(agyContent), 0755); err != nil {
		return fmt.Errorf("failed to write agy shim: %w", err)
	}

	// 4. Install thin agy-raw shim
	dstRaw := filepath.Join(targetDir, "agy-raw")
	if err := os.WriteFile(dstRaw, []byte(AgyRawShimContent), 0755); err != nil {
		return fmt.Errorf("failed to write agy-raw script: %w", err)
	}

	// 5. Install agy-orig symlink
	dstOrig := filepath.Join(targetDir, "agy-orig")
	_ = os.Remove(dstOrig)
	if err := os.Symlink("agy-raw", dstOrig); err != nil {
		_ = os.WriteFile(dstOrig, []byte(AgyRawShimContent), 0755)
	}

	// 6. Initialize default config and data directory layout
	configDir := opts.ConfigDir
	dataDir := opts.DataDir
	if opts.IsSystem {
		if configDir == "" {
			configDir = "/etc/agy-pool"
		}
		if dataDir == "" {
			dataDir = "/var/lib/agy-pool"
		}
	} else if home != "" {
		if configDir == "" {
			configDir = filepath.Join(home, ".config", "agy-pool")
		}
		if dataDir == "" {
			dataDir = filepath.Join(home, ".local", "share", "agy-pool")
		}
	}

	if configDir != "" {
		_ = os.MkdirAll(configDir, 0755)
		cfgFile := filepath.Join(configDir, "config.json")
		if _, err := os.Stat(cfgFile); os.IsNotExist(err) {
			defaultCfg := `{
  "version": 1,
  "server": {
    "listen": "127.0.0.1",
    "port": 8899
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
`
			_ = os.WriteFile(cfgFile, []byte(defaultCfg), 0644)
		}
	}

	if dataDir != "" {
		_ = os.MkdirAll(filepath.Join(dataDir, "locks"), 0700)
	}

	// 7. Check if targetDir is in PATH
	isOnPath := false
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(p) == filepath.Clean(targetDir) {
			isOnPath = true
			break
		}
	}
	if !isOnPath {
		fmt.Fprintf(out, "Note: %s is not in your PATH. Add it to your PATH to run 'agy' directly.\n", targetDir)
	}

	// 8. Configure Shell RC only if explicitly not skipped
	if !opts.SkipShellRC && home != "" {
		shellFiles := []string{
			filepath.Join(home, ".bashrc"),
			filepath.Join(home, ".zshrc"),
		}
		for _, rc := range shellFiles {
			if _, err := os.Stat(rc); err == nil {
				_ = AppendAliasIfMissing(rc)
			}
		}
	}

	return nil
}

// AppendAliasIfMissing adds the agy-pool alias block to an rc file idempotently.
func AppendAliasIfMissing(rcPath string) error {
	content, err := os.ReadFile(rcPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	if strings.Contains(string(content), MarkerStart) {
		// Already present, idempotent no-op
		return nil
	}

	f, err := os.OpenFile(rcPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString(AliasBlock)
	return err
}

// RemoveAlias cleans the agy-pool marker block from an rc file.
func RemoveAlias(rcPath string) error {
	data, err := os.ReadFile(rcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	content := string(data)
	if !strings.Contains(content, MarkerStart) {
		return nil
	}

	var newLines []string
	scanner := bufio.NewScanner(strings.NewReader(content))
	insideBlock := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, MarkerStart) {
			insideBlock = true
			continue
		}
		if insideBlock {
			if strings.Contains(line, MarkerEnd) {
				insideBlock = false
			}
			continue
		}
		newLines = append(newLines, line)
	}

	result := strings.Join(newLines, "\n")
	if len(newLines) > 0 && !strings.HasSuffix(result, "\n") {
		result += "\n"
	}

	return os.WriteFile(rcPath, []byte(result), 0644)
}

// Uninstall cleans up installed binaries, symlinks, and aliases while preserving user data.
func Uninstall(opts Options) error {
	prefix := opts.Prefix
	if prefix == "" {
		prefix = os.Getenv("PREFIX")
	}
	home := opts.HomeDir
	if home == "" {
		home = os.Getenv("HOME")
	}

	targetDir := opts.TargetDir
	if targetDir == "" {
		targetDir = DetermineTargetDir(prefix, home, nil)
	}

	if err := AssertSafeInstallTarget(targetDir); err != nil {
		return err
	}

	// 1. Remove executables and symlinks from targetDir
	for _, name := range []string{"agy-pool", "agy-raw", "agy-orig"} {
		_ = os.Remove(filepath.Join(targetDir, name))
		if home != "" {
			_ = os.Remove(filepath.Join(home, ".local", "bin", name))
		}
	}

	// 2. Remove agy shim ONLY if it is an agy-pool shim (never delete native agy!)
	agyPath := filepath.Join(targetDir, "agy")
	if diagnostics.IsShimOrSelf(agyPath) {
		_ = os.Remove(agyPath)
	}
	if home != "" {
		userAgy := filepath.Join(home, ".local", "bin", "agy")
		if diagnostics.IsShimOrSelf(userAgy) {
			_ = os.Remove(userAgy)
		}
	}

	// 2. Clean shell rc files
	if home != "" {
		for _, rc := range []string{filepath.Join(home, ".bashrc"), filepath.Join(home, ".zshrc")} {
			_ = RemoveAlias(rc)
		}
	}

	return nil
}
