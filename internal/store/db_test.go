package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenRefusesANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wa.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(path)
	if err == nil {
		db.Close()
		t.Fatal("a database from a newer Wisp was opened")
	}
	if !strings.Contains(err.Error(), "newer") {
		t.Fatalf("unclear error: %v", err)
	}
}

func TestReopenKeepsTheSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wa.db")
	for range 2 {
		db, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var accounts int
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'accounts'`).Scan(&accounts); err != nil || accounts != 0 {
			t.Fatalf("accounts table present (%d, %v): nothing reads it", accounts, err)
		}
		db.Close()
	}
}
