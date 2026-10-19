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
	// Leases from a live foreign process (our parent) and a dead one (PID far above pid_max).
	err = sdb.Update(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO inflight_leases (account_id, pid, started_at) VALUES
			('a', ?, ?), ('a', ?, ?), ('b', 99999999, 0), ('c', ?, ?)`,
			os.Getppid(), now, os.Getppid(), now, os.Getppid(), time.Now().Add(-2*time.Hour).Unix())
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
	if seen["a"] != 2 || seen["b"] != 0 || seen["c"] != 0 {
		t.Fatalf("remote counts = %v, want a=2, dead b lease pruned, and expired c lease pruned", seen)
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
