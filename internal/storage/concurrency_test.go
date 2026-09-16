package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestStateDB_SerializableImmediateConcurrency tests that concurrent transactions
// using StateDB.Update serialize cleanly via BEGIN IMMEDIATE with zero lost updates.
func TestStateDB_SerializableImmediateConcurrency(t *testing.T) {
	tmpDir := setupTestStorage(t)
	dbPath := filepath.Join(tmpDir, "state.db")

	sdb, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("failed to open state db: %v", err)
	}
	defer sdb.Close()

	// Initialize a counter in runtime_global
	err = sdb.Update(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO runtime_global (key, value) VALUES ('counter', '0')")
		return err
	})
	if err != nil {
		t.Fatalf("failed to initialize counter: %v", err)
	}

	const goroutines = 20
	const incrementsPerGoroutine = 25
	const expectedFinal = goroutines * incrementsPerGoroutine

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < incrementsPerGoroutine; i++ {
				err := sdb.Update(context.Background(), func(tx *sql.Tx) error {
					var val int
					err := tx.QueryRow("SELECT CAST(value AS INTEGER) FROM runtime_global WHERE key = 'counter'").Scan(&val)
					if err != nil {
						return err
					}
					_, err = tx.Exec("UPDATE runtime_global SET value = ? WHERE key = 'counter'", fmt.Sprintf("%d", val+1))
					return err
				})
				if err != nil {
					errCh <- err
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent transaction failed: %v", err)
	}

	// Verify the final counter matches expected
	var finalVal int
	err = sdb.View(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT CAST(value AS INTEGER) FROM runtime_global WHERE key = 'counter'").Scan(&finalVal)
	})
	if err != nil {
		t.Fatalf("failed to read counter: %v", err)
	}

	if finalVal != expectedFinal {
		t.Fatalf("lost updates detected! expected %d, got %d", expectedFinal, finalVal)
	}
}

// TestStateDB_RoundRobinCursorIntegrityUnderConcurrency verifies that concurrent
// round-robin cursor updates never corrupt the cursor or result in invalid account IDs.
func TestStateDB_RoundRobinCursorIntegrityUnderConcurrency(t *testing.T) {
	tmpDir := setupTestStorage(t)
	dbPath := filepath.Join(tmpDir, "state.db")

	sdb, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("failed to open state db: %v", err)
	}
	defer sdb.Close()

	// Seed 3 accounts
	pool := NewEmptyPool()
	pool.Accounts = []*Account{
		{ID: "acc_1", Email: "acc1@example.com"},
		{ID: "acc_2", Email: "acc2@example.com"},
		{ID: "acc_3", Email: "acc3@example.com"},
	}
	err = sdb.Update(context.Background(), func(tx *sql.Tx) error {
		return SavePoolToTx(tx, pool)
	})
	if err != nil {
		t.Fatalf("failed to seed accounts: %v", err)
	}

	const goroutines = 25
	const reservationsPerGoroutine = 20

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < reservationsPerGoroutine; i++ {
				now := float64(time.Now().Unix())
				candidates, err := sdb.ReserveRoundRobinCandidates(now, func(c []*Account, s string, p *Pool, n float64) []*Account {
					return c
				})
				if err != nil {
					errCh <- err
					return
				}
				if len(candidates) != 3 {
					errCh <- fmt.Errorf("expected 3 candidates, got %d", len(candidates))
					return
				}
				primary := candidates[0]
				if primary.ID != "acc_1" && primary.ID != "acc_2" && primary.ID != "acc_3" {
					errCh <- fmt.Errorf("invalid candidate reserved: %s", primary.ID)
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent RR reservation failed: %v", err)
	}

	// Verify final cursor is valid
	var finalCursor string
	err = sdb.View(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT value FROM runtime_global WHERE key = 'round_robin_last_account_id'").Scan(&finalCursor)
	})
	if err != nil {
		t.Fatalf("failed to read final RR cursor: %v", err)
	}
	if finalCursor != "acc_1" && finalCursor != "acc_2" && finalCursor != "acc_3" {
		t.Fatalf("final RR cursor is invalid: %s", finalCursor)
	}
}

// TestStateDB_MultiProcessSimulation simulates multiple independent processes
// opening their own StateDB connection to the same SQLite file and performing concurrent operations.
func TestStateDB_MultiProcessSimulation(t *testing.T) {
	tmpDir := setupTestStorage(t)
	dbPath := filepath.Join(tmpDir, "state.db")

	// Process A initializes DB
	procA, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("procA failed to open db: %v", err)
	}
	defer procA.Close()

	pool := NewEmptyPool()
	pool.Accounts = []*Account{
		{ID: "acc_1", Email: "proc@example.com"},
	}
	if err := procA.Update(context.Background(), func(tx *sql.Tx) error {
		return SavePoolToTx(tx, pool)
	}); err != nil {
		t.Fatalf("procA failed to save initial pool: %v", err)
	}

	// Process B opens the same DB
	procB, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("procB failed to open db: %v", err)
	}
	defer procB.Close()

	// Process C opens the same DB
	procC, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("procC failed to open db: %v", err)
	}
	defer procC.Close()

	var wg sync.WaitGroup
	errCh := make(chan error, 3)

	// Proc A writes request_count
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			err := procA.Update(context.Background(), func(tx *sql.Tx) error {
				_, err := tx.Exec("UPDATE account_runtime SET request_count = request_count + 1 WHERE account_id = 'acc_1'")
				return err
			})
			if err != nil {
				errCh <- fmt.Errorf("procA write error: %w", err)
				return
			}
		}
	}()

	// Proc B reads consistently
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			err := procB.View(context.Background(), func(tx *sql.Tx) error {
				var cnt int
				return tx.QueryRow("SELECT request_count FROM account_runtime WHERE account_id = 'acc_1'").Scan(&cnt)
			})
			if err != nil {
				errCh <- fmt.Errorf("procB read error: %w", err)
				return
			}
		}
	}()

	// Proc C updates error_count
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			err := procC.Update(context.Background(), func(tx *sql.Tx) error {
				_, err := tx.Exec("UPDATE account_runtime SET error_count = error_count + 1 WHERE account_id = 'acc_1'")
				return err
			})
			if err != nil {
				errCh <- fmt.Errorf("procC write error: %w", err)
				return
			}
		}
	}()

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("multi-process simulation error: %v", err)
	}

	// Verify final counts
	var reqCount, errCount int
	err = procA.View(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT request_count, error_count FROM account_runtime WHERE account_id = 'acc_1'").Scan(&reqCount, &errCount)
	})
	if err != nil {
		t.Fatalf("failed to read counts: %v", err)
	}
	if reqCount != 20 || errCount != 20 {
		t.Fatalf("expected req=20, err=20; got req=%d, err=%d", reqCount, errCount)
	}
}

// TestMigration_RollbackOnFailure verifies that if migration encounters a failure
// (e.g. malformed JSON input or unsafe target), no corrupted or half-baked target state.db is created.
func TestMigration_RollbackOnFailure(t *testing.T) {
	setupTestStorage(t)

	targetDir := filepath.Join(t.TempDir(), "target")
	malformedFile := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(malformedFile, []byte("NOT JSON AT ALL"), 0600); err != nil {
		t.Fatalf("failed to write malformed file: %v", err)
	}

	_, err := MigrateLegacyPool(malformedFile, targetDir)
	if err == nil {
		t.Fatalf("expected error on malformed legacy file, got nil")
	}

	// Verify target state.db was not left behind
	targetDB := filepath.Join(targetDir, "state.db")
	if _, err := os.Stat(targetDB); !os.IsNotExist(err) {
		t.Fatalf("target state.db should not exist after failed migration, but stat returned: %v", err)
	}

	// Verify no tmp files were left behind in targetDir
	entries, _ := os.ReadDir(targetDir)
	for _, entry := range entries {
		t.Fatalf("unexpected leftover file in target directory: %s", entry.Name())
	}
}
