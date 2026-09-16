package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/vlxlv/agy-pool-go/internal/config"
)

var (
	poolMu       sync.RWMutex
	fileLocksMu  sync.Mutex
	pathMutexMap = make(map[string]*sync.RWMutex)
)

func getPathMutex(path string) *sync.RWMutex {
	fileLocksMu.Lock()
	defer fileLocksMu.Unlock()
	clean := filepath.Clean(path)
	mu, ok := pathMutexMap[clean]
	if !ok {
		mu = &sync.RWMutex{}
		pathMutexMap[clean] = mu
	}
	return mu
}

// EnsureDirs creates the configured state directory with mode 0700.
func EnsureDirs() error {
	dir := config.GetStateDir()
	if err := config.AssertSafeWritePath(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o700)
}

// WithFileLock acquires both an in-memory RWMutex and a POSIX flock on the stable sidecar lock file.
func WithFileLock(path string, exclusive bool, fn func() error) error {
	if err := config.AssertSafeWritePath(path); err != nil {
		return err
	}

	mu := getPathMutex(path)
	if exclusive {
		mu.Lock()
		defer mu.Unlock()
	} else {
		mu.RLock()
		defer mu.RUnlock()
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create lock directory: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("failed to open lock file %q: %w", path, err)
	}
	defer f.Close()

	// Ensure lock file permissions are 0600
	_ = f.Chmod(0o600)

	lockOp := syscall.LOCK_SH
	if exclusive {
		lockOp = syscall.LOCK_EX
	}

	if err := syscall.Flock(int(f.Fd()), lockOp); err != nil {
		return fmt.Errorf("flock failed on %q: %w", path, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	return fn()
}

func readPoolUnlocked(path string) (*Pool, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return NewEmptyPool(), nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read pool file: %w", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("corrupt pool state in %s: %w", path, err)
	}
	if _, ok := raw["accounts"]; !ok {
		return nil, fmt.Errorf("pool state must be an object containing an accounts list")
	}

	var pool Pool
	if err := json.Unmarshal(data, &pool); err != nil {
		return nil, fmt.Errorf("corrupt pool state in %s: %w", path, err)
	}

	return &pool, nil
}

// AtomicJSONWrite writes data to a file atomically via temp file with mode 0600 and fsync.
func AtomicJSONWrite(path string, data any) error {
	if err := config.AssertSafeWritePath(path); err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create directory %q: %w", dir, err)
	}

	prefix := filepath.Base(path) + "."
	tmpFile, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return fmt.Errorf("failed to create temp file in %q: %w", dir, err)
	}
	tmpPath := tmpFile.Name()

	cleanup := true
	defer func() {
		if cleanup {
			tmpFile.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	// Enforce 0600 file mode
	if err := tmpFile.Chmod(0o600); err != nil {
		return fmt.Errorf("failed to chmod temp file: %w", err)
	}

	encoder := json.NewEncoder(tmpFile)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(data); err != nil {
		return fmt.Errorf("failed to encode JSON: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("failed to fsync temp file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("failed to atomically replace %s with %s: %w", path, tmpPath, err)
	}

	cleanup = false

	// Fsync containing directory where supported
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}

	return nil
}

// LoadPool reads the pool state under a shared read lock.
func LoadPool() (*Pool, error) {
	if err := EnsureDirs(); err != nil {
		return nil, err
	}

	filePath := config.GetAccountsFile()
	lockPath := config.GetLockFile()

	var pool *Pool
	err := WithFileLock(lockPath, false, func() error {
		p, err := readPoolUnlocked(filePath)
		if err != nil {
			return err
		}
		pool = p
		return nil
	})

	if err != nil {
		return nil, err
	}
	return pool, nil
}

// PoolTransaction executes an atomic read-modify-write transaction on the pool file.
// If mutator returns an error or panics, the file is untouched.
func PoolTransaction(mutator func(pool *Pool) error) error {
	if err := EnsureDirs(); err != nil {
		return err
	}

	filePath := config.GetAccountsFile()
	lockPath := config.GetLockFile()

	poolMu.Lock()
	defer poolMu.Unlock()

	return WithFileLock(lockPath, true, func() error {
		pool, err := readPoolUnlocked(filePath)
		if err != nil {
			return err
		}

		if err := mutator(pool); err != nil {
			return err
		}

		return AtomicJSONWrite(filePath, pool)
	})
}

// SavePool replaces the entire pool under a transaction lock.
func SavePool(pool *Pool) error {
	return PoolTransaction(func(p *Pool) error {
		*p = *pool
		return nil
	})
}

// FindAccount searches for an account by ID or email.
func FindAccount(pool *Pool, account *Account) *Account {
	if pool == nil || account == nil {
		return nil
	}
	for _, a := range pool.Accounts {
		if (account.ID != "" && a.ID == account.ID) || (account.Email != "" && a.Email == account.Email) {
			return a
		}
	}
	return nil
}

// NextAccountID allocates the lowest positive integer n such that "acc_n" is unused.
func NextAccountID(accounts []*Account) string {
	used := make(map[string]bool)
	for _, a := range accounts {
		if a != nil && a.ID != "" {
			used[a.ID] = true
		}
	}
	number := 1
	for {
		candidate := fmt.Sprintf("acc_%d", number)
		if !used[candidate] {
			return candidate
		}
		number++
	}
}
