package storage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/vlxlv/agy-pool-go/internal/config"
)

func TestOpenStateDBCreationAndPermissions(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "state.db")

	sdb, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("OpenStateDB failed: %v", err)
	}
	defer sdb.Close()

	fi, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("failed to stat state.db: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected state.db permissions 0600, got %o", perm)
	}

	// Verify schema_version in meta
	var schemaVer string
	err = sdb.View(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT value FROM meta WHERE key = 'schema_version'").Scan(&schemaVer)
	})
	if err != nil {
		t.Fatalf("failed to query schema_version: %v", err)
	}
	if schemaVer != StateDBSchemaVersion {
		t.Fatalf("expected schema version %s, got %s", StateDBSchemaVersion, schemaVer)
	}

	// Verify sidecars permissions
	walPath := dbPath + "-wal"
	if wfi, err := os.Stat(walPath); err == nil {
		if perm := wfi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("expected WAL permissions 0600, got %o", perm)
		}
	}
	shmPath := dbPath + "-shm"
	if sfi, err := os.Stat(shmPath); err == nil {
		if perm := sfi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("expected SHM permissions 0600, got %o", perm)
		}
	}
}

func TestOpenStateDBZeroBytesRefusal(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "state.db")

	// Create empty 0-byte file
	if err := os.WriteFile(dbPath, []byte{}, 0o600); err != nil {
		t.Fatalf("failed to write 0-byte file: %v", err)
	}

	_, err := OpenStateDB(dbPath)
	if err == nil {
		t.Fatalf("expected error opening 0-byte state.db, got nil")
	}

	// Verify file was NOT replaced or modified
	fi, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("failed to stat state.db: %v", err)
	}
	if fi.Size() != 0 {
		t.Fatalf("0-byte state.db was overwritten!")
	}
}

func TestOpenStateDBCorruptRefusal(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "state.db")

	// Write garbage
	if err := os.WriteFile(dbPath, []byte("NOT A SQLITE FILE HEADER"), 0o600); err != nil {
		t.Fatalf("failed to write corrupt file: %v", err)
	}

	_, err := OpenStateDB(dbPath)
	if err == nil {
		t.Fatalf("expected error opening corrupt state.db, got nil")
	}

	// Verify garbage was NOT overwritten
	content, _ := os.ReadFile(dbPath)
	if string(content) != "NOT A SQLITE FILE HEADER" {
		t.Fatalf("corrupt state.db was overwritten!")
	}
}

func TestOpenStateDBUnsupportedVersionFailsClosed(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "state.db")

	sdb, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("initial open failed: %v", err)
	}

	// Mutate schema_version to future version 99
	err = sdb.Update(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE meta SET value = '99' WHERE key = 'schema_version'")
		return err
	})
	if err != nil {
		t.Fatalf("failed to update schema_version: %v", err)
	}
	_ = sdb.Close()

	// Reopen must fail closed
	_, err = OpenStateDB(dbPath)
	if err == nil {
		t.Fatalf("expected error opening unsupported schema version, got nil")
	}
}

func TestCascadeDeletion(t *testing.T) {
	tmpDir := t.TempDir()
	if err := config.ConfigureStateDir(tmpDir); err != nil {
		t.Fatalf("failed to configure state dir: %v", err)
	}
	t.Cleanup(config.ResetDataDir)

	p := NewEmptyPool()
	p.Accounts = append(p.Accounts, &Account{
		ID:           "acc_1",
		Email:        "user@example.com",
		RequestCount: 10,
		LastQuota: &QuotaState{
			Gemini5H: &QuotaWindow{Fraction: new(float64)},
		},
	})
	if err := SavePool(p); err != nil {
		t.Fatalf("SavePool failed: %v", err)
	}

	sdb, err := GetStateDB()
	if err != nil {
		t.Fatalf("GetStateDB failed: %v", err)
	}

	// Verify row in account_runtime and account_quota
	var rtCount, qCount int
	_ = sdb.View(context.Background(), func(tx *sql.Tx) error {
		_ = tx.QueryRow("SELECT COUNT(*) FROM account_runtime WHERE account_id = 'acc_1'").Scan(&rtCount)
		_ = tx.QueryRow("SELECT COUNT(*) FROM account_quota WHERE account_id = 'acc_1'").Scan(&qCount)
		return nil
	})
	if rtCount != 1 || qCount != 1 {
		t.Fatalf("expected 1 runtime and 1 quota row, got rt=%d, q=%d", rtCount, qCount)
	}

	// Delete account from accounts table
	err = sdb.Update(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM accounts WHERE id = 'acc_1'")
		return err
	})
	if err != nil {
		t.Fatalf("DELETE failed: %v", err)
	}

	// Verify CASCADE deleted rows from account_runtime and account_quota
	_ = sdb.View(context.Background(), func(tx *sql.Tx) error {
		_ = tx.QueryRow("SELECT COUNT(*) FROM account_runtime WHERE account_id = 'acc_1'").Scan(&rtCount)
		_ = tx.QueryRow("SELECT COUNT(*) FROM account_quota WHERE account_id = 'acc_1'").Scan(&qCount)
		return nil
	})
	if rtCount != 0 || qCount != 0 {
		t.Fatalf("foreign key CASCADE failed: rt=%d, q=%d", rtCount, qCount)
	}
}

func TestCheckpointPassiveBoundsWAL(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	sdb, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("OpenStateDB failed: %v", err)
	}
	defer sdb.Close()

	put := func(v string) {
		t.Helper()
		if err := sdb.Update(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.Exec("INSERT OR REPLACE INTO meta (key, value) VALUES ('wal_test', ?)", v)
			return err
		}); err != nil {
			t.Fatalf("write failed: %v", err)
		}
	}
	big := make([]byte, 3<<20)
	for i := range big {
		big[i] = 'x'
	}
	put(string(big))
	put("")
	if err := sdb.CheckpointPassive(); err != nil {
		t.Fatalf("CheckpointPassive failed: %v", err)
	}
	put("small") // the first write after a full checkpoint restarts the WAL and applies journal_size_limit
	fi, err := os.Stat(dbPath + "-wal")
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	if fi.Size() > 1<<20 {
		t.Fatalf("WAL not truncated to journal_size_limit: %d bytes", fi.Size())
	}
}
