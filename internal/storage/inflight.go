package storage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// inflightLeaseDDL holds one row per generation request currently dispatched to an account,
// so proxy processes sharing a state DB can see each other's in-flight load.
const inflightLeaseDDL = `CREATE TABLE IF NOT EXISTS inflight_leases (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	account_id TEXT NOT NULL,
	pid INTEGER NOT NULL,
	started_at INTEGER NOT NULL,
	pid_ns TEXT NOT NULL DEFAULT ''
);`

// ensureLeaseColumns adds pid_ns to lease tables created before it existed. Rows from those versions
// keep ” (namespace unknown) and are never pruned by PID, only by leaseMaxAge.
func ensureLeaseColumns(db *sql.DB) error {
	rows, err := db.Query("SELECT name FROM pragma_table_info('inflight_leases')")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == "pid_ns" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec("ALTER TABLE inflight_leases ADD COLUMN pid_ns TEXT NOT NULL DEFAULT ''")
	return err
}

// pidNamespace identifies this process's PID namespace. A PID, and kill(pid, 0), only identify a
// process within one namespace, so processes sharing the DB from different containers are told apart.
var pidNamespace = func() string {
	ns, _ := os.Readlink("/proc/self/ns/pid")
	return ns
}()

// leaseMaxAge bounds how long a lease can outlive its request, so a lease whose release failed
// inside a long-lived process stops counting against the account.
const leaseMaxAge = time.Hour

// reserveBusyTimeout bounds how long selection waits for the shared write lock; past it the caller
// falls back to in-process in-flight counts instead of stalling every generation request.
const reserveBusyTimeout = 100 * time.Millisecond

// ownStaleLeasesCleared records that leases left by a dead process whose PID and namespace this
// process now reuses were deleted; it is set only after that delete commits, so a failed attempt retries.
var ownStaleLeasesCleared atomic.Bool

// deadLeasePruneInterval throttles pruneDeadLeases off the per-request path; a crashed process's leases
// keep counting for at most this long. lastDeadLeasePrune is the Unix second of the last committed prune.
const deadLeasePruneInterval = 5

var lastDeadLeasePrune atomic.Int64

// ReserveInFlight runs pick inside a cross-process write transaction (BEGIN IMMEDIATE), passing the
// in-flight counts held by other live processes, and records a lease for the account ID pick returns.
// An empty ID records nothing. release deletes the lease and is safe to call more than once.
// If the write lock stays busy past reserveBusyTimeout it returns ErrWriteBusy without calling pick.
func ReserveInFlight(pick func(remote map[string]int64) string) (accountID string, release func(), err error) {
	release = func() {}
	sdb, err := GetStateDB()
	if err != nil {
		return "", release, err
	}
	self := os.Getpid()

	var leaseID int64
	now := time.Now().Unix()
	pruned := false
	err = sdb.TryUpdate(context.Background(), reserveBusyTimeout, func(tx *sql.Tx) error {
		if !ownStaleLeasesCleared.Load() {
			if _, err := tx.Exec("DELETE FROM inflight_leases WHERE pid = ? AND pid_ns = ?", self, pidNamespace); err != nil {
				return err
			}
		}
		if pruned = now-lastDeadLeasePrune.Load() >= deadLeasePruneInterval; pruned {
			if err := pruneDeadLeases(tx, self); err != nil {
				return err
			}
		}

		remote := make(map[string]int64)
		rows, err := tx.Query("SELECT account_id, COUNT(*) FROM inflight_leases WHERE NOT (pid = ? AND pid_ns = ?) GROUP BY account_id", self, pidNamespace)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			var n int64
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				return err
			}
			remote[id] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		accountID = pick(remote)
		if accountID == "" {
			return nil
		}
		res, err := tx.Exec("INSERT INTO inflight_leases (account_id, pid, started_at, pid_ns) VALUES (?, ?, ?, ?)",
			accountID, self, now, pidNamespace)
		if err != nil {
			return err
		}
		leaseID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return accountID, release, err
	}
	ownStaleLeasesCleared.Store(true)
	if pruned {
		lastDeadLeasePrune.Store(now)
	}
	if leaseID == 0 {
		return accountID, release, nil
	}

	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = sdb.Update(context.Background(), func(tx *sql.Tx) error {
				_, err := tx.Exec("DELETE FROM inflight_leases WHERE id = ?", leaseID)
				return err
			})
		})
	}
	return accountID, release, nil
}

// LeaseInFlight records a lease for an account chosen without a cross-process pick (e.g. failover).
func LeaseInFlight(accountID string) (func(), error) {
	_, release, err := ReserveInFlight(func(map[string]int64) string { return accountID })
	return release, err
}

func pruneDeadLeases(tx *sql.Tx, self int) error {
	if _, err := tx.Exec("DELETE FROM inflight_leases WHERE started_at < ?", time.Now().Add(-leaseMaxAge).Unix()); err != nil {
		return err
	}
	// Only leases from this PID namespace can be checked with kill(pid, 0); others age out.
	rows, err := tx.Query("SELECT DISTINCT pid FROM inflight_leases WHERE pid != ? AND pid_ns = ?", self, pidNamespace)
	if err != nil {
		return err
	}
	var dead []int
	for rows.Next() {
		var pid int
		if err := rows.Scan(&pid); err != nil {
			rows.Close()
			return err
		}
		if !processAlive(pid) {
			dead = append(dead, pid)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, pid := range dead {
		if _, err := tx.Exec("DELETE FROM inflight_leases WHERE pid = ? AND pid_ns = ?", pid, pidNamespace); err != nil {
			return err
		}
	}
	return nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
