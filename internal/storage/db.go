package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	_ "modernc.org/sqlite"
)

const (
	// StateDBSchemaVersion defines the authoritative SQLite schema version.
	StateDBSchemaVersion = "1"
	// DefaultBusyTimeoutMs defines the bounded SQLite busy timeout in milliseconds.
	DefaultBusyTimeoutMs = 5000
)

// StateDB represents an open handle to the authoritative SQLite state store.
type StateDB struct {
	db      *sql.DB
	path    string
	mu      sync.Mutex
	writeMu sync.Mutex
}

var (
	activeStateDB     *StateDB
	activeStateDBPath string
	activeDBMu        sync.Mutex
)

// EnsureFilePermissions enforces 0600 on the database file and sidecars, and 0700 on the parent directory.
func EnsureFilePermissions(dbPath string) error {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}
	_ = os.Chmod(dir, 0o700)

	if fi, err := os.Stat(dbPath); err == nil {
		if fi.Mode().Perm() != 0o600 {
			_ = os.Chmod(dbPath, 0o600)
		}
	}
	walPath := dbPath + "-wal"
	if fi, err := os.Stat(walPath); err == nil {
		if fi.Mode().Perm() != 0o600 {
			_ = os.Chmod(walPath, 0o600)
		}
	}
	shmPath := dbPath + "-shm"
	if fi, err := os.Stat(shmPath); err == nil {
		if fi.Mode().Perm() != 0o600 {
			_ = os.Chmod(shmPath, 0o600)
		}
	}
	return nil
}

// OpenStateDB opens (and initializes if needed) the state database at path.
// Fails closed if the database is corrupt, 0 bytes, or has an unsupported schema version.
func OpenStateDB(dbPath string) (*StateDB, error) {
	if err := config.AssertSafeWritePath(dbPath); err != nil {
		return nil, err
	}

	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create state directory %q: %w", dir, err)
	}

	exists := false
	if fi, err := os.Stat(dbPath); err == nil {
		exists = true
		if fi.Size() == 0 {
			return nil, fmt.Errorf("corrupt state database %q: file is 0 bytes", dbPath)
		}
	}

	dsn := fmt.Sprintf("file:%s?_txlock=immediate&_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)",
		dbPath, DefaultBusyTimeoutMs)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open state database %q: %w", dbPath, err)
	}

	// Conservative connection pooling for embedded SQLite
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)

	// Apply conservative WAL and safety pragmas explicitly
	if _, err := db.Exec("PRAGMA journal_mode = WAL;"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to set WAL mode on %q: %w", dbPath, err)
	}
	if _, err := db.Exec("PRAGMA synchronous = NORMAL;"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to set synchronous mode on %q: %w", dbPath, err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON;"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to enable foreign keys on %q: %w", dbPath, err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA busy_timeout = %d;", DefaultBusyTimeoutMs)); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to set busy timeout on %q: %w", dbPath, err)
	}

	if exists {
		// Verify schema version in existing DB
		var schemaVer string
		err := db.QueryRow("SELECT value FROM meta WHERE key = 'schema_version'").Scan(&schemaVer)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("corrupt or uninitialized state database in %q: %w", dbPath, err)
		}
		if schemaVer != StateDBSchemaVersion {
			_ = db.Close()
			return nil, fmt.Errorf("unsupported schema version %q in %q (expected %s)", schemaVer, dbPath, StateDBSchemaVersion)
		}
	} else {
		// Initialize fresh database schema within a transaction
		tx, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("failed to begin schema initialization on %q: %w", dbPath, err)
		}
		schemaDDL := `
		CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS accounts (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL DEFAULT '',
			access_token TEXT NOT NULL DEFAULT '',
			refresh_token TEXT NOT NULL DEFAULT '',
			id_token TEXT NOT NULL DEFAULT '',
			token_expiry REAL,
			created_at INTEGER,
			updated_at INTEGER,
			extra TEXT
		);

		CREATE TABLE IF NOT EXISTS runtime_global (
			key TEXT PRIMARY KEY,
			value TEXT
		);

		CREATE TABLE IF NOT EXISTS account_runtime (
			account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			request_count INTEGER NOT NULL DEFAULT 0,
			gen_count INTEGER,
			error_count INTEGER NOT NULL DEFAULT 0,
			last_used_at INTEGER,
			status TEXT NOT NULL DEFAULT '',
			validation_url TEXT,
			rate_limited_until REAL
		);

		CREATE TABLE IF NOT EXISTS account_quota (
			account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			gemini_5h_fraction REAL,
			gemini_5h_reset TEXT,
			gemini_weekly_fraction REAL,
			gemini_weekly_reset TEXT,
			third_party_5h_fraction REAL,
			third_party_5h_reset TEXT,
			third_party_weekly_fraction REAL,
			third_party_weekly_reset TEXT,
			remaining_fraction REAL,
			reset_time TEXT,
			updated_at INTEGER,
			raw_extra TEXT
		);
		`
		if _, err := tx.Exec(schemaDDL); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, fmt.Errorf("failed to create schema on %q: %w", dbPath, err)
		}

		nowStr := time.Now().UTC().Format(time.RFC3339)
		if _, err := tx.Exec("INSERT INTO meta (key, value) VALUES ('schema_version', ?), ('created_at', ?);",
			StateDBSchemaVersion, nowStr); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, fmt.Errorf("failed to initialize meta on %q: %w", dbPath, err)
		}

		if _, err := tx.Exec("INSERT INTO runtime_global (key, value) VALUES ('strategy', 'max_quota');"); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, fmt.Errorf("failed to initialize strategy on %q: %w", dbPath, err)
		}

		if err := tx.Commit(); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("failed to commit schema initialization on %q: %w", dbPath, err)
		}
	}

	_ = EnsureFilePermissions(dbPath)

	return &StateDB{
		db:   db,
		path: dbPath,
	}, nil
}

// GetStateDB returns the active StateDB instance for the configured state directory.
func GetStateDB() (*StateDB, error) {
	activeDBMu.Lock()
	defer activeDBMu.Unlock()

	currentPath := config.GetStateDBFile()
	if activeStateDB != nil && activeStateDBPath == currentPath && activeStateDB.db != nil {
		return activeStateDB, nil
	}

	if activeStateDB != nil {
		_ = activeStateDB.Close()
		activeStateDB = nil
		activeStateDBPath = ""
	}

	sdb, err := OpenStateDB(currentPath)
	if err != nil {
		return nil, err
	}
	activeStateDB = sdb
	activeStateDBPath = currentPath
	return sdb, nil
}

// Close closes the database connection.
func (s *StateDB) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		err := s.db.Close()
		s.db = nil
		return err
	}
	return nil
}

// View executes a read-only transaction against the database.
func (s *StateDB) View(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return errors.New("state database is closed")
	}

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("failed to begin read transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Update executes an immediate write transaction (BEGIN IMMEDIATE).
func (s *StateDB) Update(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return errors.New("state database is closed")
	}

	var tx *sql.Tx
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		tx, err = db.BeginTx(ctx, nil)
		if err == nil {
			break
		}
		if strings.Contains(err.Error(), "SQLITE_BUSY") || strings.Contains(err.Error(), "database is locked") {
			time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
			continue
		}
		return fmt.Errorf("failed to begin immediate write transaction: %w", err)
	}
	if err != nil {
		return fmt.Errorf("failed to begin immediate write transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit write transaction: %w", err)
	}

	_ = EnsureFilePermissions(s.path)
	return nil
}

// ReserveRoundRobinCandidates executes candidate ordering and advances the round_robin cursor
// within ONE immediate SQLite transaction, updating ONLY the cursor in runtime_global.
func (s *StateDB) ReserveRoundRobinCandidates(now float64, orderFn func(candidates []*Account, strategy string, pool *Pool, now float64) []*Account) ([]*Account, error) {
	var candidates []*Account
	err := s.Update(context.Background(), func(tx *sql.Tx) error {
		pool, err := LoadPoolFromTx(tx)
		if err != nil {
			return err
		}
		if len(pool.Accounts) == 0 {
			candidates = nil
			return nil
		}
		ordered := orderFn(pool.Accounts, config.StrategyRoundRobin, pool, now)
		if len(ordered) > 0 && ordered[0].ID != "" {
			resID := ordered[0].ID
			pool.RoundRobinLastAccountID = &resID
			if _, err := tx.Exec("INSERT INTO runtime_global (key, value) VALUES ('round_robin_last_account_id', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", resID); err != nil {
				return fmt.Errorf("failed to advance round_robin cursor: %w", err)
			}
		}
		candidates = ordered
		return nil
	})
	if err != nil {
		return nil, err
	}
	return candidates, nil
}

func serializeAny(v any) *string {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		s := fmt.Sprint(v)
		return &s
	}
	s := string(b)
	return &s
}

func deserializeAny(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(*s), &v); err == nil {
		return v
	}
	return *s
}

// LoadPoolFromTx reads the full pool state from an active read transaction.
func LoadPoolFromTx(tx *sql.Tx) (*Pool, error) {
	// 1. Query runtime_global
	rows, err := tx.Query("SELECT key, value FROM runtime_global")
	if err != nil {
		return nil, fmt.Errorf("failed to query runtime_global: %w", err)
	}
	defer rows.Close()

	strategy := "max_quota"
	var activeID *string
	var rrID *string
	var poolExtra map[string]json.RawMessage

	for rows.Next() {
		var k, v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		if !k.Valid {
			continue
		}
		switch k.String {
		case "strategy":
			if v.Valid && v.String != "" {
				strategy = v.String
			}
		case "active_account_id":
			if v.Valid && v.String != "" {
				val := v.String
				activeID = &val
			}
		case "round_robin_last_account_id":
			if v.Valid && v.String != "" {
				val := v.String
				rrID = &val
			}
		case "pool_extra":
			if v.Valid && v.String != "" {
				var ex map[string]json.RawMessage
				if err := json.Unmarshal([]byte(v.String), &ex); err == nil {
					poolExtra = ex
				}
			}
		}
	}
	_ = rows.Close()

	// 2. Query accounts with LEFT JOIN to account_runtime and account_quota
	query := `
	SELECT
		a.id, a.name, a.email, a.access_token, a.refresh_token, a.id_token, a.token_expiry, a.created_at, a.updated_at, a.extra,
		r.request_count, r.gen_count, r.error_count, r.last_used_at, r.status, r.validation_url, r.rate_limited_until,
		q.gemini_5h_fraction, q.gemini_5h_reset, q.gemini_weekly_fraction, q.gemini_weekly_reset,
		q.third_party_5h_fraction, q.third_party_5h_reset, q.third_party_weekly_fraction, q.third_party_weekly_reset,
		q.remaining_fraction, q.reset_time, q.updated_at, q.raw_extra
	FROM accounts a
	LEFT JOIN account_runtime r ON a.id = r.account_id
	LEFT JOIN account_quota q ON a.id = q.account_id
	ORDER BY a.rowid ASC
	`
	accRows, err := tx.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to query accounts: %w", err)
	}
	defer accRows.Close()

	accountsList := make([]*Account, 0)
	for accRows.Next() {
		var (
			id, name, email, at, rt, idt sql.NullString
			tokenExpiry                  sql.NullFloat64
			createdAt, updatedAt         sql.NullInt64
			extraJSON                    sql.NullString

			reqCount         sql.NullInt64
			genCount         sql.NullInt64
			errCount         sql.NullInt64
			lastUsed         sql.NullInt64
			status, valURL   sql.NullString
			rateLimitedUntil sql.NullFloat64

			g5hFrac, gwFrac, tp5hFrac, tpwFrac, remFrac    sql.NullFloat64
			g5hReset, gwReset, tp5hReset, tpwReset, qReset sql.NullString
			qUpdatedAt                                     sql.NullInt64
			rawExtra                                       sql.NullString
		)

		if err := accRows.Scan(
			&id, &name, &email, &at, &rt, &idt, &tokenExpiry, &createdAt, &updatedAt, &extraJSON,
			&reqCount, &genCount, &errCount, &lastUsed, &status, &valURL, &rateLimitedUntil,
			&g5hFrac, &g5hReset, &gwFrac, &gwReset,
			&tp5hFrac, &tp5hReset, &tpwFrac, &tpwReset,
			&remFrac, &qReset, &qUpdatedAt, &rawExtra,
		); err != nil {
			return nil, fmt.Errorf("failed to scan account row: %w", err)
		}

		acc := &Account{
			ID:           id.String,
			Name:         name.String,
			Email:        email.String,
			AccessToken:  at.String,
			RefreshToken: rt.String,
			IDToken:      idt.String,
			Status:       status.String,
		}
		if tokenExpiry.Valid {
			acc.TokenExpiry = &tokenExpiry.Float64
		}
		if createdAt.Valid {
			acc.CreatedAt = &createdAt.Int64
		}
		if updatedAt.Valid {
			acc.UpdatedAt = &updatedAt.Int64
		}
		if valURL.Valid && valURL.String != "" {
			acc.ValidationURL = &valURL.String
		}
		if rateLimitedUntil.Valid {
			acc.RateLimitedUntil = &rateLimitedUntil.Float64
		}
		if reqCount.Valid {
			acc.RequestCount = reqCount.Int64
		}
		if genCount.Valid {
			acc.GenCount = &genCount.Int64
		}
		if errCount.Valid {
			acc.ErrorCount = errCount.Int64
		}
		if lastUsed.Valid {
			acc.LastUsedAt = &lastUsed.Int64
		}
		if extraJSON.Valid && extraJSON.String != "" {
			var ex map[string]json.RawMessage
			if err := json.Unmarshal([]byte(extraJSON.String), &ex); err == nil {
				acc.Extra = ex
			}
		}

		// Check if quota row exists
		hasQuota := g5hFrac.Valid || gwFrac.Valid || remFrac.Valid || qUpdatedAt.Valid
		if hasQuota {
			q := &QuotaState{}
			if g5hFrac.Valid || g5hReset.Valid {
				qw := &QuotaWindow{}
				if g5hFrac.Valid {
					qw.Fraction = &g5hFrac.Float64
				}
				if g5hReset.Valid {
					qw.ResetTime = deserializeAny(&g5hReset.String)
				}
				q.Gemini5H = qw
			}
			if gwFrac.Valid || gwReset.Valid {
				qw := &QuotaWindow{}
				if gwFrac.Valid {
					qw.Fraction = &gwFrac.Float64
				}
				if gwReset.Valid {
					qw.ResetTime = deserializeAny(&gwReset.String)
				}
				q.GeminiWeekly = qw
			}
			if tp5hFrac.Valid || tp5hReset.Valid {
				qw := &QuotaWindow{}
				if tp5hFrac.Valid {
					qw.Fraction = &tp5hFrac.Float64
				}
				if tp5hReset.Valid {
					qw.ResetTime = deserializeAny(&tp5hReset.String)
				}
				q.ThirdParty5H = qw
			}
			if tpwFrac.Valid || tpwReset.Valid {
				qw := &QuotaWindow{}
				if tpwFrac.Valid {
					qw.Fraction = &tpwFrac.Float64
				}
				if tpwReset.Valid {
					qw.ResetTime = deserializeAny(&tpwReset.String)
				}
				q.ThirdPartyWeekly = qw
			}
			if remFrac.Valid {
				q.RemainingFraction = &remFrac.Float64
			}
			if qReset.Valid {
				q.ResetTime = deserializeAny(&qReset.String)
			}
			if qUpdatedAt.Valid {
				q.UpdatedAt = &qUpdatedAt.Int64
			}
			if rawExtra.Valid && rawExtra.String != "" {
				var qex map[string]json.RawMessage
				if err := json.Unmarshal([]byte(rawExtra.String), &qex); err == nil {
					q.Extra = qex
				}
			}
			acc.LastQuota = q
		}

		accountsList = append(accountsList, acc)
	}

	return &Pool{
		Version:                 1,
		Strategy:                strategy,
		ActiveAccountID:         activeID,
		RoundRobinLastAccountID: rrID,
		Accounts:                accountsList,
		Extra:                   poolExtra,
	}, nil
}

// SavePoolToTx writes the entire pool state within an active write transaction.
func SavePoolToTx(tx *sql.Tx, pool *Pool) error {
	if pool == nil {
		return errors.New("cannot save nil pool")
	}

	// 1. Update runtime_global
	strat := pool.Strategy
	if strat == "" {
		strat = "max_quota"
	}
	if _, err := tx.Exec("INSERT INTO runtime_global (key, value) VALUES ('strategy', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", strat); err != nil {
		return fmt.Errorf("failed to save strategy: %w", err)
	}

	if pool.ActiveAccountID != nil && *pool.ActiveAccountID != "" {
		if _, err := tx.Exec("INSERT INTO runtime_global (key, value) VALUES ('active_account_id', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", *pool.ActiveAccountID); err != nil {
			return fmt.Errorf("failed to save active_account_id: %w", err)
		}
	} else {
		if _, err := tx.Exec("DELETE FROM runtime_global WHERE key = 'active_account_id'"); err != nil {
			return fmt.Errorf("failed to delete active_account_id: %w", err)
		}
	}

	if pool.RoundRobinLastAccountID != nil && *pool.RoundRobinLastAccountID != "" {
		if _, err := tx.Exec("INSERT INTO runtime_global (key, value) VALUES ('round_robin_last_account_id', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", *pool.RoundRobinLastAccountID); err != nil {
			return fmt.Errorf("failed to save round_robin_last_account_id: %w", err)
		}
	} else {
		if _, err := tx.Exec("DELETE FROM runtime_global WHERE key = 'round_robin_last_account_id'"); err != nil {
			return fmt.Errorf("failed to delete round_robin_last_account_id: %w", err)
		}
	}

	if len(pool.Extra) > 0 {
		if b, err := json.Marshal(pool.Extra); err == nil {
			_, _ = tx.Exec("INSERT INTO runtime_global (key, value) VALUES ('pool_extra', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(b))
		}
	} else {
		_, _ = tx.Exec("DELETE FROM runtime_global WHERE key = 'pool_extra'")
	}

	// 2. Remove accounts not in pool
	existingIDs := make(map[string]bool)
	rows, err := tx.Query("SELECT id FROM accounts")
	if err != nil {
		return fmt.Errorf("failed to query existing account ids: %w", err)
	}
	for rows.Next() {
		var aid string
		if err := rows.Scan(&aid); err == nil {
			existingIDs[aid] = true
		}
	}
	_ = rows.Close()

	newIDMap := make(map[string]bool)
	for _, a := range pool.Accounts {
		if a != nil && a.ID != "" {
			newIDMap[a.ID] = true
		}
	}

	for existingID := range existingIDs {
		if !newIDMap[existingID] {
			if _, err := tx.Exec("DELETE FROM accounts WHERE id = ?", existingID); err != nil {
				return fmt.Errorf("failed to delete removed account %s: %w", existingID, err)
			}
		}
	}

	// 3. Upsert each account, account_runtime, and account_quota
	for _, a := range pool.Accounts {
		if a == nil || a.ID == "" {
			continue
		}

		var extraStr *string
		if len(a.Extra) > 0 {
			if b, err := json.Marshal(a.Extra); err == nil {
				s := string(b)
				extraStr = &s
			}
		}

		_, err := tx.Exec(`
		INSERT INTO accounts (id, name, email, access_token, refresh_token, id_token, token_expiry, created_at, updated_at, extra)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name,
			email=excluded.email,
			access_token=excluded.access_token,
			refresh_token=excluded.refresh_token,
			id_token=excluded.id_token,
			token_expiry=excluded.token_expiry,
			created_at=excluded.created_at,
			updated_at=excluded.updated_at,
			extra=excluded.extra;
		`, a.ID, a.Name, a.Email, a.AccessToken, a.RefreshToken, a.IDToken, a.TokenExpiry, a.CreatedAt, a.UpdatedAt, extraStr)
		if err != nil {
			return fmt.Errorf("failed to upsert account %s: %w", a.ID, err)
		}

		_, err = tx.Exec(`
		INSERT INTO account_runtime (account_id, request_count, gen_count, error_count, last_used_at, status, validation_url, rate_limited_until)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET
			request_count=excluded.request_count,
			gen_count=excluded.gen_count,
			error_count=excluded.error_count,
			last_used_at=excluded.last_used_at,
			status=excluded.status,
			validation_url=excluded.validation_url,
			rate_limited_until=excluded.rate_limited_until;
		`, a.ID, a.RequestCount, a.GenCount, a.ErrorCount, a.LastUsedAt, a.Status, a.ValidationURL, a.RateLimitedUntil)
		if err != nil {
			return fmt.Errorf("failed to upsert account_runtime for %s: %w", a.ID, err)
		}

		if a.LastQuota != nil {
			var g5hFrac, gwFrac, tp5hFrac, tpwFrac *float64
			var g5hReset, gwReset, tp5hReset, tpwReset *string
			if a.LastQuota.Gemini5H != nil {
				g5hFrac = a.LastQuota.Gemini5H.Fraction
				g5hReset = serializeAny(a.LastQuota.Gemini5H.ResetTime)
			}
			if a.LastQuota.GeminiWeekly != nil {
				gwFrac = a.LastQuota.GeminiWeekly.Fraction
				gwReset = serializeAny(a.LastQuota.GeminiWeekly.ResetTime)
			}
			if a.LastQuota.ThirdParty5H != nil {
				tp5hFrac = a.LastQuota.ThirdParty5H.Fraction
				tp5hReset = serializeAny(a.LastQuota.ThirdParty5H.ResetTime)
			}
			if a.LastQuota.ThirdPartyWeekly != nil {
				tpwFrac = a.LastQuota.ThirdPartyWeekly.Fraction
				tpwReset = serializeAny(a.LastQuota.ThirdPartyWeekly.ResetTime)
			}
			qReset := serializeAny(a.LastQuota.ResetTime)

			var rawExtraStr *string
			if len(a.LastQuota.Extra) > 0 {
				if b, err := json.Marshal(a.LastQuota.Extra); err == nil {
					s := string(b)
					rawExtraStr = &s
				}
			}

			_, err = tx.Exec(`
			INSERT INTO account_quota (account_id, gemini_5h_fraction, gemini_5h_reset, gemini_weekly_fraction, gemini_weekly_reset, third_party_5h_fraction, third_party_5h_reset, third_party_weekly_fraction, third_party_weekly_reset, remaining_fraction, reset_time, updated_at, raw_extra)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(account_id) DO UPDATE SET
				gemini_5h_fraction=excluded.gemini_5h_fraction,
				gemini_5h_reset=excluded.gemini_5h_reset,
				gemini_weekly_fraction=excluded.gemini_weekly_fraction,
				gemini_weekly_reset=excluded.gemini_weekly_reset,
				third_party_5h_fraction=excluded.third_party_5h_fraction,
				third_party_5h_reset=excluded.third_party_5h_reset,
				third_party_weekly_fraction=excluded.third_party_weekly_fraction,
				third_party_weekly_reset=excluded.third_party_weekly_reset,
				remaining_fraction=excluded.remaining_fraction,
				reset_time=excluded.reset_time,
				updated_at=excluded.updated_at,
				raw_extra=excluded.raw_extra;
			`, a.ID, g5hFrac, g5hReset, gwFrac, gwReset, tp5hFrac, tp5hReset, tpwFrac, tpwReset, a.LastQuota.RemainingFraction, qReset, a.LastQuota.UpdatedAt, rawExtraStr)
			if err != nil {
				return fmt.Errorf("failed to upsert account_quota for %s: %w", a.ID, err)
			}
		} else {
			if _, err := tx.Exec("DELETE FROM account_quota WHERE account_id = ?", a.ID); err != nil {
				return fmt.Errorf("failed to delete account_quota for %s: %w", a.ID, err)
			}
		}
	}

	return nil
}
