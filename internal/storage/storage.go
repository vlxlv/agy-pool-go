package storage

import (
	"context"
	"database/sql"
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

// ReadPoolUnlocked reads and unmarshals a legacy or transitional pool JSON file without locking.
func ReadPoolUnlocked(path string) (*Pool, error) {
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

// LoadPool reads the pool state from the authoritative SQLite state store under a read transaction.
func LoadPool() (*Pool, error) {
	if err := EnsureDirs(); err != nil {
		return nil, err
	}

	sdb, err := GetStateDB()
	if err != nil {
		return nil, err
	}

	var pool *Pool
	err = sdb.View(context.Background(), func(tx *sql.Tx) error {
		p, err := LoadPoolFromTx(tx)
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

// PoolTransaction executes an atomic read-modify-write transaction on the authoritative state database.
// If mutator returns an error or panics, the transaction is rolled back and state is untouched.
func PoolTransaction(mutator func(pool *Pool) error) error {
	if err := EnsureDirs(); err != nil {
		return err
	}

	sdb, err := GetStateDB()
	if err != nil {
		return err
	}

	poolMu.Lock()
	defer poolMu.Unlock()

	return sdb.Update(context.Background(), func(tx *sql.Tx) error {
		pool, err := LoadPoolFromTx(tx)
		if err != nil {
			return err
		}

		if err := mutator(pool); err != nil {
			return err
		}

		return SavePoolToTx(tx, pool)
	})
}

// SavePool replaces the entire pool under an immediate write transaction.
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

// MigrationResult describes the result of migrating legacy Python state to Go-native state.
type MigrationResult struct {
	SourcePath      string `json:"source_path"`
	TargetFile      string `json:"target_file"`
	AccountsCount   int    `json:"accounts_count"`
	ActiveAccountID string `json:"active_account_id,omitempty"`
	Strategy        string `json:"strategy,omitempty"`
}

// MigrateLegacyPool reads legacy Python state (e.g. ~/.gemini/agy-pool-accounts.json)
// as read-only input and converts it into Go-native state.db under targetDataDir.
// The source file is strictly preserved untouched (no deletion, no truncation, no mutation).
func MigrateLegacyPool(sourcePath, targetDataDir string) (*MigrationResult, error) {
	if sourcePath == "" {
		return nil, errors.New("source path is required")
	}
	if targetDataDir == "" {
		return nil, errors.New("target data directory is required")
	}

	absSource, err := filepath.Abs(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("invalid source path %q: %w", sourcePath, err)
	}

	// 1. Read source state (strictly read-only)
	sourceData, err := os.ReadFile(absSource)
	if err != nil {
		return nil, fmt.Errorf("failed to read legacy pool source %q: %w", absSource, err)
	}

	var pool Pool
	if err := json.Unmarshal(sourceData, &pool); err != nil {
		return nil, fmt.Errorf("corrupt or invalid legacy pool source %q: %w", absSource, err)
	}

	// 2. Prepare target directory under -D
	if err := config.AssertSafeWritePath(targetDataDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(targetDataDir, "locks"), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create target data directory: %w", err)
	}

	targetDB := filepath.Join(targetDataDir, "state.db")
	if err := config.AssertSafeWritePath(targetDB); err != nil {
		return nil, err
	}

	// 3. Atomically build target database via temporary DB
	tmpDBPath := filepath.Join(targetDataDir, fmt.Sprintf("state.db.tmp.%d", os.Getpid()))
	_ = os.Remove(tmpDBPath)
	_ = os.Remove(tmpDBPath + "-wal")
	_ = os.Remove(tmpDBPath + "-shm")
	defer func() {
		_ = os.Remove(tmpDBPath)
		_ = os.Remove(tmpDBPath + "-wal")
		_ = os.Remove(tmpDBPath + "-shm")
	}()

	tmpDB, err := OpenStateDB(tmpDBPath)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize migration temp db: %w", err)
	}

	err = tmpDB.Update(context.Background(), func(tx *sql.Tx) error {
		return SavePoolToTx(tx, &pool)
	})
	if err != nil {
		_ = tmpDB.Close()
		return nil, fmt.Errorf("failed to migrate pool into temp db: %w", err)
	}

	_, _ = tmpDB.db.Exec("PRAGMA wal_checkpoint(TRUNCATE);")
	if err := tmpDB.Close(); err != nil {
		return nil, fmt.Errorf("failed to close migration temp db: %w", err)
	}

	// Atomically rename temporary database into destination state.db
	if err := os.Rename(tmpDBPath, targetDB); err != nil {
		return nil, fmt.Errorf("failed to atomically publish state database: %w", err)
	}
	_ = EnsureFilePermissions(targetDB)

	// Also write accounts.json for transitional compatibility with test assertions
	targetAccounts := filepath.Join(targetDataDir, "accounts.json")
	if err := config.AssertSafeWritePath(targetAccounts); err == nil {
		_ = AtomicJSONWrite(targetAccounts, &pool)
	}

	activeID := ""
	if pool.ActiveAccountID != nil {
		activeID = *pool.ActiveAccountID
	}

	return &MigrationResult{
		SourcePath:      absSource,
		TargetFile:      targetDB,
		AccountsCount:   len(pool.Accounts),
		ActiveAccountID: activeID,
		Strategy:        pool.Strategy,
	}, nil
}

// MigrateSplitJSONToDB imports preexisting Go split JSON state (accounts.json, runtime.json, quota.json)
// from sourceDir into the authoritative state.db in targetDataDir.
func MigrateSplitJSONToDB(sourceDir, targetDataDir string) (*MigrationResult, error) {
	if sourceDir == "" {
		return nil, errors.New("source directory is required")
	}
	if targetDataDir == "" {
		return nil, errors.New("target data directory is required")
	}

	accountsFile := filepath.Join(sourceDir, "accounts.json")
	if _, err := os.Stat(accountsFile); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("source accounts.json not found in %s", sourceDir)
	}

	pool, err := ReadPoolUnlocked(accountsFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read accounts.json: %w", err)
	}

	// Optional runtime.json
	runtimeFile := filepath.Join(sourceDir, "runtime.json")
	if data, err := os.ReadFile(runtimeFile); err == nil {
		var rf TargetRuntimeFile
		if err := json.Unmarshal(data, &rf); err == nil {
			if rf.ActiveAccountID != nil {
				pool.ActiveAccountID = rf.ActiveAccountID
			}
			if rf.RoundRobinLastAccountID != nil {
				pool.RoundRobinLastAccountID = rf.RoundRobinLastAccountID
			}
			for _, acc := range pool.Accounts {
				if rRec, ok := rf.AccountsRuntime[acc.ID]; ok && rRec != nil {
					acc.Status = rRec.Status
					acc.ValidationURL = rRec.ValidationURL
					acc.RateLimitedUntil = rRec.RateLimitedUntil
					acc.RequestCount = rRec.RequestCount
					acc.GenCount = rRec.GenCount
					acc.ErrorCount = rRec.ErrorCount
					acc.LastUsedAt = rRec.LastUsedAt
				}
			}
		}
	}

	// Optional quota.json
	quotaFile := filepath.Join(sourceDir, "quota.json")
	if data, err := os.ReadFile(quotaFile); err == nil {
		var qf TargetQuotaFile
		if err := json.Unmarshal(data, &qf); err == nil {
			for _, acc := range pool.Accounts {
				if qRec, ok := qf.AccountsQuota[acc.ID]; ok && qRec != nil {
					acc.LastQuota = qRec
				}
			}
		}
	}

	if err := config.AssertSafeWritePath(targetDataDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(targetDataDir, "locks"), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create target directory: %w", err)
	}

	targetDB := filepath.Join(targetDataDir, "state.db")
	tmpDBPath := filepath.Join(targetDataDir, fmt.Sprintf("state.db.tmp.%d", os.Getpid()))
	_ = os.Remove(tmpDBPath)
	_ = os.Remove(tmpDBPath + "-wal")
	_ = os.Remove(tmpDBPath + "-shm")
	defer func() {
		_ = os.Remove(tmpDBPath)
		_ = os.Remove(tmpDBPath + "-wal")
		_ = os.Remove(tmpDBPath + "-shm")
	}()

	tmpDB, err := OpenStateDB(tmpDBPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open temp db: %w", err)
	}

	err = tmpDB.Update(context.Background(), func(tx *sql.Tx) error {
		return SavePoolToTx(tx, pool)
	})
	if err != nil {
		_ = tmpDB.Close()
		return nil, fmt.Errorf("failed to save migrated split state: %w", err)
	}

	_, _ = tmpDB.db.Exec("PRAGMA wal_checkpoint(TRUNCATE);")
	_ = tmpDB.Close()

	if err := os.Rename(tmpDBPath, targetDB); err != nil {
		return nil, fmt.Errorf("failed to atomically rename state database: %w", err)
	}
	_ = EnsureFilePermissions(targetDB)

	activeID := ""
	if pool.ActiveAccountID != nil {
		activeID = *pool.ActiveAccountID
	}

	return &MigrationResult{
		SourcePath:      sourceDir,
		TargetFile:      targetDB,
		AccountsCount:   len(pool.Accounts),
		ActiveAccountID: activeID,
		Strategy:        pool.Strategy,
	}, nil
}
