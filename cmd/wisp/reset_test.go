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

func TestWipeDataWithNothingThere(t *testing.T) {
	if err := wipeData(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
