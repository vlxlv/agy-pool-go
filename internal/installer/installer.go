package installer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/diagnostics"
)

const (
	MarkerStart = "# >>> agy-pool integration >>>"
	MarkerEnd   = "# <<< agy-pool integration <<<"

	AliasBlock = "\n# >>> agy-pool integration >>>\nalias agy='agy-pool run --'\nalias agy-orig='agy-raw'\n# <<< agy-pool integration <<<\n"
)

// ManagedShimMarker is the exact ownership marker embedded in all agy-pool generated shims.
const ManagedShimMarker = "# agy-pool managed shim v1"

// UserAgyShimContent is the thin POSIX shell shim for user-mode agy integration.
const UserAgyShimContent = `#!/bin/sh
# agy-pool managed shim v1
CONFIG_FILE="${AGY_CONFIG_FILE:-$HOME/.config/agy-pool/config.json}"
DATA_DIR="${AGY_DATA_DIR:-$HOME/.local/share/agy-pool}"
exec agy-pool run -c "$CONFIG_FILE" -D "$DATA_DIR" -- "$@"
`

// SystemAgyShimContent is the thin POSIX shell shim for system-mode agy integration.
const SystemAgyShimContent = `#!/bin/sh
# agy-pool managed shim v1
CONFIG_FILE="${AGY_CONFIG_FILE:-/etc/agy-pool/config.json}"
DATA_DIR="${AGY_DATA_DIR:-/var/lib/agy-pool}"
exec agy-pool run -c "$CONFIG_FILE" -D "$DATA_DIR" -- "$@"
`

// AgyRawShimContent is the thin POSIX shell shim invoking agy-pool's raw/direct execution.
const AgyRawShimContent = `#!/bin/sh
# agy-pool managed shim v1
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
	SkipShellRC   bool // Deprecated: installs never modify shell RC files.
	CleanLegacyRC bool // Opt-in cleanup of historical shell RC marker blocks during uninstall.
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
		return filepath.Join(home, ".local", "bin")
	}

	roots := GetInstallerRoots()
	return roots.BinDir
}

// AssertSafeInstallTarget guards against accidentally writing into forbidden host locations in tests.
func AssertSafeInstallTarget(targetDir string) error {
	return config.AssertSafeDestructivePath(targetDir)
}

// CopyFile copies src to dst and ensures mode 0755 permissions.
func CopyFile(src, dst string) error {
	if err := config.AssertSafeDestructivePath(dst); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	// Write to temp file then rename for atomic replacement
	tmpDst := dst + ".tmp"
	if err := config.AssertSafeDestructivePath(tmpDst); err != nil {
		return err
	}
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
	if nativeAgy != "" {
		if err := diagnostics.ValidateNativeCandidate(nativeAgy); err != nil {
			return errors.New("Native agy was not found.\nInstall the original Antigravity CLI first, then rerun agy-pool installer.")
		}
	} else {
		var err error
		nativeAgy, _, err = diagnostics.ResolveNativeAgyBinary()
		if err != nil {
			return errors.New("Native agy was not found.\nInstall the original Antigravity CLI first, then rerun agy-pool installer.")
		}
	}
	fmt.Fprintf(out, "Found native agy: %s\n", nativeAgy)

	roots := GetInstallerRoots()

	prefix := opts.Prefix
	if prefix == "" {
		prefix = os.Getenv("PREFIX")
	}
	home := opts.HomeDir
	if home == "" {
		home = roots.UserHome
	}

	targetDir := opts.TargetDir
	if targetDir == "" {
		if opts.IsSystem {
			if roots.SystemRoot != "" {
				targetDir = filepath.Join(roots.SystemRoot, "usr", "local", "bin")
			} else {
				targetDir = "/usr/local/bin"
			}
		} else {
			targetDir = DetermineTargetDir(prefix, home, func() bool {
				if config.IsTestMode() {
					return false
				}
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

	// 2. Stage files in temporary files inside targetDir to prevent partial installs
	pid := os.Getpid()
	tmpPool := filepath.Join(targetDir, fmt.Sprintf(".agy-pool.tmp.%d", pid))
	tmpAgy := filepath.Join(targetDir, fmt.Sprintf(".agy.tmp.%d", pid))
	tmpRaw := filepath.Join(targetDir, fmt.Sprintf(".agy-raw.tmp.%d", pid))
	tmpOrig := filepath.Join(targetDir, fmt.Sprintf(".agy-orig.tmp.%d", pid))

	staged := []string{}
	cleanupStaged := func() {
		for _, s := range staged {
			_ = os.Remove(s)
		}
	}

	dstPool := filepath.Join(targetDir, "agy-pool")
	if opts.SourceBinary != "" {
		if err := config.AssertSafeDestructivePath(tmpPool); err != nil {
			return err
		}
		if err := CopyFile(opts.SourceBinary, tmpPool); err != nil {
			cleanupStaged()
			return fmt.Errorf("failed to stage binary to %s: %w", tmpPool, err)
		}
		staged = append(staged, tmpPool)
	}

	dstAgy := filepath.Join(targetDir, "agy")
	if err := config.AssertSafeDestructivePath(tmpAgy); err != nil {
		cleanupStaged()
		return err
	}
	agyContent := UserAgyShimContent
	if opts.IsSystem {
		agyContent = SystemAgyShimContent
	}
	if err := os.WriteFile(tmpAgy, []byte(agyContent), 0755); err != nil {
		cleanupStaged()
		return fmt.Errorf("failed to stage agy shim: %w", err)
	}
	staged = append(staged, tmpAgy)

	dstRaw := filepath.Join(targetDir, "agy-raw")
	if err := config.AssertSafeDestructivePath(tmpRaw); err != nil {
		cleanupStaged()
		return err
	}
	if err := os.WriteFile(tmpRaw, []byte(AgyRawShimContent), 0755); err != nil {
		cleanupStaged()
		return fmt.Errorf("failed to stage agy-raw script: %w", err)
	}
	staged = append(staged, tmpRaw)

	dstOrig := filepath.Join(targetDir, "agy-orig")
	if err := config.AssertSafeDestructivePath(tmpOrig); err != nil {
		cleanupStaged()
		return err
	}
	_ = os.Remove(tmpOrig)
	if err := os.Symlink("agy-raw", tmpOrig); err != nil {
		if err := os.WriteFile(tmpOrig, []byte(AgyRawShimContent), 0755); err != nil {
			cleanupStaged()
			return fmt.Errorf("failed to stage agy-orig shim: %w", err)
		}
	}
	staged = append(staged, tmpOrig)

	// Atomically publish staged files into place
	if opts.SourceBinary != "" {
		if err := config.AssertSafeDestructivePath(dstPool); err != nil {
			cleanupStaged()
			return err
		}
		if err := os.Rename(tmpPool, dstPool); err != nil {
			cleanupStaged()
			return fmt.Errorf("failed to publish binary to %s: %w", dstPool, err)
		}
	}
	if err := config.AssertSafeDestructivePath(dstAgy); err != nil {
		cleanupStaged()
		return err
	}
	if err := os.Rename(tmpAgy, dstAgy); err != nil {
		cleanupStaged()
		return fmt.Errorf("failed to publish agy shim to %s: %w", dstAgy, err)
	}
	if err := config.AssertSafeDestructivePath(dstRaw); err != nil {
		cleanupStaged()
		return err
	}
	if err := os.Rename(tmpRaw, dstRaw); err != nil {
		cleanupStaged()
		return fmt.Errorf("failed to publish agy-raw script to %s: %w", dstRaw, err)
	}
	if err := config.AssertSafeDestructivePath(dstOrig); err != nil {
		cleanupStaged()
		return err
	}
	_ = os.Remove(dstOrig)
	if err := os.Rename(tmpOrig, dstOrig); err != nil {
		cleanupStaged()
		return fmt.Errorf("failed to publish agy-orig to %s: %w", dstOrig, err)
	}

	// 6. Initialize default config and data directory layout
	configDir := opts.ConfigDir
	dataDir := opts.DataDir
	if opts.IsSystem {
		if configDir == "" {
			if roots.SystemRoot != "" {
				configDir = filepath.Join(roots.SystemRoot, "etc", "agy-pool")
			} else {
				configDir = "/etc/agy-pool"
			}
		}
		if dataDir == "" {
			if roots.SystemRoot != "" {
				dataDir = filepath.Join(roots.SystemRoot, "var", "lib", "agy-pool")
			} else {
				dataDir = "/var/lib/agy-pool"
			}
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
		if err := config.AssertSafeDestructivePath(configDir); err != nil {
			return err
		}
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
			if err := config.AssertSafeDestructivePath(cfgFile); err != nil {
				return err
			}
			_ = os.WriteFile(cfgFile, []byte(defaultCfg), 0644)
		}
	}

	if dataDir != "" {
		if err := config.AssertSafeDestructivePath(dataDir); err != nil {
			return err
		}
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

	// 8. New installs never modify shell startup / RC files; the installed shim is the sole integration.
	return nil
}

// RemoveLegacyRCBlock excises the exact historical agy-pool marker block from data,
// leaving all surrounding content byte-for-byte identical.
func RemoveLegacyRCBlock(data []byte) ([]byte, bool) {
	s := string(data)
	idxStart := strings.Index(s, MarkerStart)
	if idxStart == -1 {
		return data, false
	}
	idxEnd := strings.Index(s, MarkerEnd)
	if idxEnd == -1 || idxEnd < idxStart {
		return data, false
	}
	endBlock := idxEnd + len(MarkerEnd)
	if endBlock < len(data) && data[endBlock] == '\r' {
		endBlock++
	}
	if endBlock < len(data) && data[endBlock] == '\n' {
		endBlock++
	}

	startBlock := idxStart
	if startBlock > 0 && data[startBlock-1] == '\n' {
		startBlock--
		if startBlock > 0 && data[startBlock-1] == '\r' {
			startBlock--
		}
	}

	result := make([]byte, 0, len(data)-(endBlock-startBlock))
	result = append(result, data[:startBlock]...)
	result = append(result, data[endBlock:]...)
	return result, true
}

// AppendAliasIfMissing adds the agy-pool alias block to an rc file idempotently (retained for legacy fixture testing).
func AppendAliasIfMissing(rcPath string) error {
	if err := config.AssertSafeDestructivePath(rcPath); err != nil {
		return err
	}
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

// RemoveAlias cleans the historical agy-pool marker block from an rc file if present.
func RemoveAlias(rcPath string) error {
	if err := config.AssertSafeDestructivePath(rcPath); err != nil {
		return err
	}
	data, err := os.ReadFile(rcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	cleaned, changed := RemoveLegacyRCBlock(data)
	if !changed {
		return nil
	}

	return os.WriteFile(rcPath, cleaned, 0644)
}

// SafeRemoveOwnedEntry removes an agy-pool owned file or symlink with strict ownership verification
// and no-follow TOCTOU hardening.
func SafeRemoveOwnedEntry(dir string, name string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	fullPath := filepath.Join(dir, name)
	if err := config.AssertSafeDestructivePath(fullPath); err != nil {
		return err
	}

	dirFile, err := os.Open(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer dirFile.Close()
	dirFd := int(dirFile.Fd())

	st, err := os.Lstat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	// 1. If symlink, ensure no-follow and verify ownership
	if st.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(fullPath)
		if err != nil {
			return err
		}
		if name == "agy-orig" {
			if target != "agy-raw" && !strings.HasSuffix(target, "/agy-raw") {
				fmt.Fprintf(out, "Refusing to delete symlink %s: target is not agy-raw (points to %q)\n", fullPath, target)
				return nil
			}
		} else {
			if !strings.Contains(target, "agy-pool") && !strings.Contains(target, "agy-raw") {
				fmt.Fprintf(out, "Refusing to delete symlink %s: unverified target %q\n", fullPath, target)
				return nil
			}
		}
		return syscall.Unlinkat(dirFd, name)
	}

	// 2. Reject non-regular files (directories, sockets, devices)
	if !st.Mode().IsRegular() {
		fmt.Fprintf(out, "Refusing to delete %s: not a regular file or symlink (mode: %v)\n", fullPath, st.Mode())
		return nil
	}

	// 3. Regular file: open with O_NOFOLLOW to avoid symlink swap race
	fd, err := syscall.Openat(dirFd, name, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			fmt.Fprintf(out, "Refusing to delete %s: symlink race detected (O_NOFOLLOW failed)\n", fullPath)
			return nil
		}
		return err
	}
	defer syscall.Close(fd)

	var fdSt syscall.Stat_t
	if err := syscall.Fstat(fd, &fdSt); err != nil {
		return err
	}
	if sysSt, ok := st.Sys().(*syscall.Stat_t); ok {
		if fdSt.Ino != sysSt.Ino || fdSt.Dev != sysSt.Dev {
			fmt.Fprintf(out, "Refusing to delete %s: file replaced during validation (TOCTOU detected)\n", fullPath)
			return nil
		}
	}

	// 4. Validate ownership
	if name == "agy-pool" {
		nativeAgy := diagnostics.FindRealAgyBinary()
		if nativeAgy != "" {
			if nativeFi, err := os.Stat(nativeAgy); err == nil {
				if sysSt, ok := nativeFi.Sys().(*syscall.Stat_t); ok {
					if sysSt.Ino == fdSt.Ino && sysSt.Dev == fdSt.Dev {
						fmt.Fprintf(out, "CRITICAL: Refusing to delete %s: hard-linked to native agy!\n", fullPath)
						return nil
					}
				}
			}
		}
	} else {
		// Shims (agy, agy-raw, agy-orig): verify exact ownership marker
		buf := make([]byte, 1024)
		n, _ := syscall.Read(fd, buf)
		header := string(buf[:n])
		if !strings.Contains(header, ManagedShimMarker) && !strings.Contains(header, MarkerStart) {
			fmt.Fprintf(out, "Refusing to delete unowned file %s: missing agy-pool ownership marker\n", fullPath)
			return nil
		}
	}

	// 5. Unlink verified regular file directly via parent dir fd
	return syscall.Unlinkat(dirFd, name)
}

// Uninstall cleans up installed binaries, symlinks, and aliases while preserving user data.
func Uninstall(opts Options) error {
	roots := GetInstallerRoots()

	prefix := opts.Prefix
	if prefix == "" {
		prefix = os.Getenv("PREFIX")
	}
	home := opts.HomeDir
	if home == "" {
		home = roots.UserHome
	}

	targetDir := opts.TargetDir
	if targetDir == "" {
		if opts.IsSystem {
			if roots.SystemRoot != "" {
				targetDir = filepath.Join(roots.SystemRoot, "usr", "local", "bin")
			} else {
				targetDir = "/usr/local/bin"
			}
		} else {
			targetDir = DetermineTargetDir(prefix, home, nil)
		}
	}

	if err := AssertSafeInstallTarget(targetDir); err != nil {
		return err
	}

	out := opts.Stdout
	if out == nil {
		out = io.Discard
	}

	// 1. Remove owned shims and binaries using SafeRemoveOwnedEntry
	for _, name := range []string{"agy", "agy-raw", "agy-orig", "agy-pool"} {
		if err := SafeRemoveOwnedEntry(targetDir, name, out); err != nil {
			return fmt.Errorf("failed to safely remove %s: %w", name, err)
		}
	}

	// 2. Clean legacy shell rc files ONLY if explicitly requested
	if opts.CleanLegacyRC && home != "" {
		for _, rc := range []string{filepath.Join(home, ".bashrc"), filepath.Join(home, ".zshrc")} {
			if _, err := os.Stat(rc); err == nil {
				if err := config.AssertSafeDestructivePath(rc); err != nil {
					return err
				}
				_ = RemoveAlias(rc)
			}
		}
	}

	return nil
}
