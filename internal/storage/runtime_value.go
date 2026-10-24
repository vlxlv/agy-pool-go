package storage

import (
	"context"
	"database/sql"
	"errors"
)

// LoadRuntimeValue returns a runtime_global entry kept outside the pool snapshot, or "" if absent.
// The pool load/save code ignores keys it does not know, so such entries survive pool writes.
func LoadRuntimeValue(key string) (string, error) {
	sdb, err := GetStateDB()
	if err != nil {
		return "", err
	}
	var v sql.NullString
	err = sdb.View(context.Background(), func(tx *sql.Tx) error {
		err := tx.QueryRow("SELECT value FROM runtime_global WHERE key = ?", key).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return v.String, err
}

// SaveRuntimeValue writes a runtime_global entry kept outside the pool snapshot; "" deletes it.
func SaveRuntimeValue(key, value string) error {
	sdb, err := GetStateDB()
	if err != nil {
		return err
	}
	return sdb.Update(context.Background(), func(tx *sql.Tx) error {
		if value == "" {
			_, err := tx.Exec("DELETE FROM runtime_global WHERE key = ?", key)
			return err
		}
		_, err := tx.Exec("INSERT INTO runtime_global (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
		return err
	})
}
