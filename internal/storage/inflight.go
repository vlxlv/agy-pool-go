package storage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"syscall"
	"time"
)

// inflightLeaseDDL holds one row per generation request currently dispatched to an account,
// so proxy processes sharing a state DB can see each other's in-flight load.
const inflightLeaseDDL = `CREATE TABLE IF NOT EXISTS inflight_leases (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	account_id TEXT NOT NULL,
	pid INTEGER NOT NULL,
	started_at INTEGER NOT NULL
);`

// clearOwnStaleLeases drops leases left by a dead process whose PID this process now reuses.
var clearOwnStaleLeases sync.Once

// ReserveInFlight runs pick inside a cross-process write transaction (BEGIN IMMEDIATE), passing the
// in-flight counts held by other live processes, and records a lease for the account ID pick returns.
// An empty ID records nothing. release deletes the lease and is safe to call more than once.
func ReserveInFlight(pick func(remote map[string]int64) string) (accountID string, release func(), err error) {
	release = func() {}
	sdb, err := GetStateDB()
	if err != nil {
		return "", release, err
	}
	self := os.Getpid()

	var leaseID int64
	err = sdb.Update(context.Background(), func(tx *sql.Tx) error {
		var ferr error
		clearOwnStaleLeases.Do(func() {
			_, ferr = tx.Exec("DELETE FROM inflight_leases WHERE pid = ?", self)
		})
		if ferr != nil {
			return ferr
		}
		if err := pruneDeadLeases(tx, self); err != nil {
			return err
		}

		remote := make(map[string]int64)
		rows, err := tx.Query("SELECT account_id, COUNT(*) FROM inflight_leases WHERE pid != ? GROUP BY account_id", self)
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
		res, err := tx.Exec("INSERT INTO inflight_leases (account_id, pid, started_at) VALUES (?, ?, ?)",
			accountID, self, time.Now().Unix())
		if err != nil {
			return err
		}
		leaseID, err = res.LastInsertId()
		return err
	})
	if err != nil || leaseID == 0 {
		return accountID, release, err
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
	rows, err := tx.Query("SELECT DISTINCT pid FROM inflight_leases WHERE pid != ?", self)
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
		if _, err := tx.Exec("DELETE FROM inflight_leases WHERE pid = ?", pid); err != nil {
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
