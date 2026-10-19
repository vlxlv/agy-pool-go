package storage

import (
	"context"
	"database/sql"
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
