package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogAppendsAndStartsOverPastTheLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wisp.log")
	write := func(s string) {
		t.Helper()
		f, err := openLog(path, 10)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(s)
		f.Close()
	}
	write("one\n")
	write("two\n") // 8 bytes: under the limit, appended
	if b, _ := os.ReadFile(path); string(b) != "one\ntwo\n" {
		t.Fatalf("log = %q, want both lines", b)
	}
	write(strings.Repeat("x", 10)) // now 18 bytes
	write("three\n")               // over the limit at start: emptied first
	if b, _ := os.ReadFile(path); string(b) != "three\n" {
		t.Fatalf("log = %q, want it started over", b)
	}
}
