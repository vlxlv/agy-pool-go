package installer

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/vlxlv/agy-pool-go/internal/config"
)

const (
	MarkerStart = "# >>> agy-pool integration >>>"
	MarkerEnd   = "# <<< agy-pool integration <<<"

	AliasBlock = "\n# >>> agy-pool integration >>>\nalias agy='agy-pool run --'\nalias agy-orig='agy-raw'\n# <<< agy-pool integration <<<\n"
)

// RawScriptContent is the exact standalone shell script for agy-raw matching Python reference.
const RawScriptContent = `#!/usr/bin/env bash
# agy-raw: Directly execute original native Antigravity CLI without proxy gateway
PORT="${AGY_PORT:-8899}"
case "${CLOUD_CODE_URL:-}" in
    "http://127.0.0.1:${PORT}"|"http://127.0.0.1:${PORT}/"|"http://localhost:${PORT}"|"http://localhost:${PORT}/")
        unset CLOUD_CODE_URL
        ;;
esac

SELF="$(realpath "$0" 2>/dev/null || readlink -f "$0" 2>/dev/null || echo "$0")"

REAL_AGY="${AGY_BIN:-}"
if [ -n "$REAL_AGY" ] && [ -x "$REAL_AGY" ]; then
    exec "$REAL_AGY" "$@"
fi

# Dynamically locate the original agy binary without hardcoding internal paths
for cand in \
    "${PREFIX:+${PREFIX}/bin/agy}" \
    "/data/data/com.termux/files/usr/bin/agy" \
    "/usr/local/bin/agy" \
    "/usr/bin/agy" \
    "$HOME/.local/bin/agy" \
    "$(type -P agy 2>/dev/null || true)"; do
    if [ -n "$cand" ] && [ -x "$cand" ]; then
        cand_real="$(realpath "$cand" 2>/dev/null || readlink -f "$cand" 2>/dev/null || echo "$cand")"
        if [ "$cand_real" != "$SELF" ]; then
            exec "$cand" "$@"
        fi
    fi
done

echo "[Error] Could not locate native 'agy' executable in PATH." >&2
exit 1
`

type Options struct {
	SourceBinary string
	TargetDir    string
	Prefix       string
	HomeDir      string
	SkipShellRC  bool
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

	if err := AssertSafeInstallTarget(targetDir); err != nil {
		return err
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("failed to create target directory %s: %w", targetDir, err)
	}

	// 1. Install agy-pool binary
	dstPool := filepath.Join(targetDir, "agy-pool")
	if opts.SourceBinary != "" {
		if err := CopyFile(opts.SourceBinary, dstPool); err != nil {
			return fmt.Errorf("failed to copy binary to %s: %w", dstPool, err)
		}
	}

	// 2. Install agy-raw script
	dstRaw := filepath.Join(targetDir, "agy-raw")
	if err := os.WriteFile(dstRaw, []byte(RawScriptContent), 0755); err != nil {
		return fmt.Errorf("failed to write agy-raw script: %w", err)
	}

	// 3. Install agy-orig symlink
	dstOrig := filepath.Join(targetDir, "agy-orig")
	_ = os.Remove(dstOrig)
	if err := os.Symlink(dstRaw, dstOrig); err != nil {
		return fmt.Errorf("failed to symlink agy-orig to agy-raw: %w", err)
	}

	// 4. Configure Shell RC
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

	// 5. Initialize user-local config and data directory layout
	if home != "" {
		userConfigDir := filepath.Join(home, ".config", "agy-pool")
		userConfigFile := filepath.Join(userConfigDir, "config.json")
		if _, err := os.Stat(userConfigFile); os.IsNotExist(err) {
			_ = os.MkdirAll(userConfigDir, 0755)
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
			_ = os.WriteFile(userConfigFile, []byte(defaultCfg), 0644)
		}

		userDataDir := filepath.Join(home, ".local", "share", "agy-pool")
		_ = os.MkdirAll(filepath.Join(userDataDir, "locks"), 0700)
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

	// 2. Clean shell rc files
	if home != "" {
		for _, rc := range []string{filepath.Join(home, ".bashrc"), filepath.Join(home, ".zshrc")} {
			_ = RemoveAlias(rc)
		}
	}

	return nil
}
