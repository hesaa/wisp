package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWipeDataKeepsOnlyTheLog(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"wisp.db", "wisp.db-wal", "wisp.db-shm", "session.db", "session.db-wal", "session.db-shm", "wisp.log", filepath.Join("cache", "a", "b.jpg")} {
		p := filepath.Join(dir, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"+n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := wipeData(dir); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 1 || ents[0].Name() != "wisp.log" {
		t.Fatalf("left %v, %v; want only wisp.log", ents, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "wisp.log")); string(b) != "xwisp.log" {
		t.Errorf("log changed: %q", b)
	}
}

// A session file that cannot be deleted (held open: Windows refuses) keeps wisp.db and its PIN.
func TestWipeDataKeepsThePINWhenASessionStays(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"wisp.db", "session.db"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := wipeData(dir); err == nil {
		t.Fatal("no error with session.db held open")
	}
	if _, err := os.Stat(filepath.Join(dir, "wisp.db")); err != nil {
		t.Errorf("wisp.db: %v; want it kept", err)
	}
}

func TestWipeDataWithNothingThere(t *testing.T) {
	if err := wipeData(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
