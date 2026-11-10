package store

import (
	"database/sql"
	"errors"
)

// GetSetting returns accountID's value for key (accountID "" holds the app-global settings); ok is
// false when it was never set.
func (d *DB) GetSetting(accountID, key string) (value string, ok bool, err error) {
	err = d.sql.QueryRow(`SELECT value FROM settings WHERE account_id = ? AND key = ?`, accountID, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

// SetSetting stores accountID's value for key ("" for app-global), replacing any earlier one.
func (d *DB) SetSetting(accountID, key, value string) error {
	_, err := d.sql.Exec(`INSERT INTO settings (account_id, key, value) VALUES (?, ?, ?)
		ON CONFLICT (account_id, key) DO UPDATE SET value = excluded.value`, accountID, key, value)
	return err
}
