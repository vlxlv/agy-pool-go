package storage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"
)

func TestReserveInFlightSeesOtherProcessesAndPrunesDead(t *testing.T) {
	setupTestStorage(t)
	sdb, err := GetStateDB()
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()
	ownStaleLeasesCleared.Store(false)
	// a: live foreign process (our parent); b: dead PID (far above pid_max) in our namespace;
	// c: expired; d: dead PID in another namespace, which kill(pid, 0) cannot judge;
	// e: left by a dead process that had our PID and namespace.
	err = sdb.Update(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO inflight_leases (account_id, pid, started_at, pid_ns) VALUES
			('a', ?, ?, ?), ('a', ?, ?, ?), ('b', 99999999, ?, ?), ('c', ?, ?, ?), ('d', 99999999, ?, 'pid:[other]'), ('e', ?, ?, ?)`,
			os.Getppid(), now, pidNamespace, os.Getppid(), now, pidNamespace, now, pidNamespace,
			os.Getppid(), time.Now().Add(-2*time.Hour).Unix(), pidNamespace, now, os.Getpid(), now, pidNamespace)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	var seen map[string]int64
	id, release, err := ReserveInFlight(func(remote map[string]int64) string {
		seen = remote
		return "b"
	})
	if err != nil || id != "b" {
		t.Fatalf("ReserveInFlight = %q, %v", id, err)
	}
	if seen["a"] != 2 || seen["b"] != 0 || seen["c"] != 0 || seen["d"] != 1 || seen["e"] != 0 {
		t.Fatalf("remote counts = %v, want a=2, dead b and expired c pruned, foreign-namespace d kept, own e excluded", seen)
	}

	count := func() (n int) {
		_ = sdb.View(context.Background(), func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT COUNT(*) FROM inflight_leases WHERE pid = ?", os.Getpid()).Scan(&n)
		})
		return n
	}
	if n := count(); n != 1 {
		t.Fatalf("own leases = %d, want 1", n)
	}
	release()
	release()
	if n := count(); n != 0 {
		t.Fatalf("own leases after release = %d, want 0", n)
	}
}

func TestReserveInFlightGivesUpWhenWriteLockBusy(t *testing.T) {
	setupTestStorage(t)
	sdb, err := GetStateDB()
	if err != nil {
		t.Fatal(err)
	}

	// Another process holding the SQLite write lock.
	other, err := sql.Open("sqlite", "file:"+sdb.path+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	called := false
	_, release, err := ReserveInFlight(func(map[string]int64) string { called = true; return "a" })
	release()
	if !errors.Is(err, ErrWriteBusy) || called {
		t.Fatalf("ReserveInFlight = %v, pick called = %v; want ErrWriteBusy without pick", err, called)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("ReserveInFlight waited %v, want about %v", d, reserveBusyTimeout)
	}
	_ = tx.Rollback()

	if _, _, err := ReserveInFlight(func(map[string]int64) string { return "a" }); err != nil {
		t.Fatalf("ReserveInFlight after lock released = %v", err)
	}
}

func TestReserveInFlightWaitsOutBriefInProcessWriter(t *testing.T) {
	setupTestStorage(t)
	sdb, err := GetStateDB()
	if err != nil {
		t.Fatal(err)
	}
	holdWriteMu := func(d time.Duration) {
		sdb.writeMu.Lock()
		go func() { time.Sleep(d); sdb.writeMu.Unlock() }()
	}

	// A routine write (e.g. a lease release) finishing within the budget must not cause a fallback.
	holdWriteMu(30 * time.Millisecond)
	_, release, err := ReserveInFlight(func(map[string]int64) string { return "a" })
	release()
	if err != nil {
		t.Fatalf("ReserveInFlight behind a 30ms writer = %v, want success", err)
	}

	// A writer holding past the budget still yields ErrWriteBusy without calling pick.
	holdWriteMu(3 * reserveBusyTimeout)
	called := false
	_, release, err = ReserveInFlight(func(map[string]int64) string { called = true; return "a" })
	release()
	if !errors.Is(err, ErrWriteBusy) || called {
		t.Fatalf("ReserveInFlight behind a long writer = %v, pick called = %v; want ErrWriteBusy without pick", err, called)
	}
	time.Sleep(3 * reserveBusyTimeout) // let the holder release before cleanup
}

func TestEnsureLeaseColumnsMigratesOldTable(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/old.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE inflight_leases (id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id TEXT NOT NULL, pid INTEGER NOT NULL, started_at INTEGER NOT NULL);
		INSERT INTO inflight_leases (account_id, pid, started_at) VALUES ('a', 1, 0);`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // second call must be a no-op
		if err := ensureLeaseColumns(db); err != nil {
			t.Fatalf("ensureLeaseColumns #%d: %v", i+1, err)
		}
	}
	var ns string
	if err := db.QueryRow("SELECT pid_ns FROM inflight_leases WHERE account_id = 'a'").Scan(&ns); err != nil || ns != "" {
		t.Fatalf("migrated row pid_ns = %q, %v; want empty", ns, err)
	}
}
