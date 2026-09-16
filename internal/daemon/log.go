package daemon

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

const (
	LogRotateInterval = 30 * time.Second
)

var (
	lastLogRotateCheck time.Time
	logRotateMu        sync.Mutex
)

// FormatSize formats a byte count into a human-readable string matching Python reference.
func FormatSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	} else if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024.0)
	}
	return fmt.Sprintf("%.1f MB", float64(bytes)/(1024.0*1024.0))
}

// RotateLogIfNeeded performs copytruncate log rotation if logPath exceeds maxBytes (or force=true).
// Uses copytruncate semantics to keep open file descriptors valid without disrupting active daemons.
func RotateLogIfNeeded(logPath string, maxBytes int64, backupCount int, force bool) (bool, error) {
	if logPath == "" {
		logPath = LogFileProvider()
	}
	if maxBytes <= 0 {
		maxBytes = config.DefaultMaxLogBytes
	}
	if backupCount < 0 {
		backupCount = config.DefaultBackupLogCount
	}

	if err := config.AssertSafeWritePath(logPath); err != nil {
		return false, err
	}

	st, err := os.Stat(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	if !force && st.Size() < maxBytes {
		return false, nil
	}

	lockFile := logPath + ".lock"
	var rotated bool
	lockErr := storage.WithFileLock(lockFile, true, func() error {
		curSt, err := os.Stat(logPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}

		if !force && curSt.Size() < maxBytes {
			return nil
		}

		// 1. Shift existing backups: e.g. .2 -> .3, .1 -> .2
		for i := backupCount - 1; i >= 1; i-- {
			src := fmt.Sprintf("%s.%d", logPath, i)
			dst := fmt.Sprintf("%s.%d", logPath, i+1)
			if _, err := os.Stat(src); err == nil {
				_ = os.Rename(src, dst)
			}
		}

		// 2. Copy active log to .1
		if backupCount > 0 {
			backupDst := fmt.Sprintf("%s.1", logPath)
			if err := copyFile(logPath, backupDst); err != nil {
				return fmt.Errorf("failed to copy log backup: %w", err)
			}
		}

		// 3. In-place truncation preserving open file descriptors (copytruncate)
		f, err := os.OpenFile(logPath, os.O_RDWR, 0o600)
		if err != nil {
			return fmt.Errorf("failed to open log for truncation: %w", err)
		}
		defer f.Close()

		if err := f.Truncate(0); err != nil {
			return fmt.Errorf("failed to truncate log file: %w", err)
		}
		_ = f.Sync()

		rotated = true
		return nil
	})

	if lockErr != nil {
		return false, lockErr
	}
	return rotated, nil
}

// ClearLog truncates the active log file and removes backup logs.
func ClearLog(logPath string, backupCount int) error {
	if logPath == "" {
		logPath = LogFileProvider()
	}
	if backupCount < 0 {
		backupCount = config.DefaultBackupLogCount
	}

	if err := config.AssertSafeWritePath(logPath); err != nil {
		return err
	}

	lockFile := logPath + ".lock"
	return storage.WithFileLock(lockFile, true, func() error {
		if _, err := os.Stat(logPath); err == nil {
			f, err := os.OpenFile(logPath, os.O_RDWR, 0o600)
			if err == nil {
				_ = f.Truncate(0)
				_ = f.Sync()
				f.Close()
			}
		}

		for i := 1; i <= backupCount+2; i++ {
			bPath := fmt.Sprintf("%s.%d", logPath, i)
			_ = os.Remove(bPath)
		}
		return nil
	})
}

// MaybeRotateLog rate-limits log rotation checks to at most once every LogRotateInterval.
func MaybeRotateLog() bool {
	logRotateMu.Lock()
	now := time.Now()
	if now.Sub(lastLogRotateCheck) < LogRotateInterval {
		logRotateMu.Unlock()
		return false
	}
	lastLogRotateCheck = now
	logRotateMu.Unlock()

	rotated, _ := RotateLogIfNeeded("", 0, -1, false)
	return rotated
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
