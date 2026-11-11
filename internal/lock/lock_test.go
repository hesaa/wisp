package lock

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	h, err := Hash("2580")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$600000$") {
		t.Fatalf("prefix: %q", h)
	}
	ok, err := Verify(h, "2580")
	if err != nil || !ok {
		t.Fatalf("Verify = %v, %v", ok, err)
	}
	h2, _ := Hash("2580")
	if h == h2 {
		t.Fatal("two hashes of the same PIN are identical")
	}
}

func TestVerifyRejectsWrongPIN(t *testing.T) {
	h, _ := Hash("2580")
	ok, err := Verify(h, "2581")
	if ok || err != nil {
		t.Fatalf("Verify = %v, %v", ok, err)
	}
}

func TestVerifyMalformedStored(t *testing.T) {
	for _, s := range []string{"", "plain", "pbkdf2-sha256$x$AA$AA", "md5$1$AA$AA", "pbkdf2-sha256$0$AA$AA", "pbkdf2-sha256$1$!!$AA", "pbkdf2-sha256$1$AA$AA"} {
		if _, err := Verify(s, "1234"); !errors.Is(err, ErrStored) {
			t.Errorf("Verify(%q) err = %v, want ErrStored", s, err)
		}
	}
}

func TestValidPIN(t *testing.T) {
	for _, p := range []string{"1234", "12345678"} {
		if err := ValidPIN(p); err != nil {
			t.Errorf("ValidPIN(%q) = %v", p, err)
		}
	}
	for _, p := range []string{"123", "123456789", "12a4", "１２３４", " 1234"} {
		if err := ValidPIN(p); !errors.Is(err, ErrPINFormat) {
			t.Errorf("ValidPIN(%q) = %v, want ErrPINFormat", p, err)
		}
	}
}

func TestUpdate(t *testing.T) {
	h, err := Update("", "", "1234")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := Verify(h, "1234"); !ok {
		t.Fatal("new hash does not verify 1234")
	}
	h2, err := Update(h, "1234", "5678")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := Verify(h2, "5678"); !ok {
		t.Fatal("changed hash does not verify 5678")
	}
	if _, err := Update(h, "0000", "5678"); !errors.Is(err, ErrWrongPIN) {
		t.Fatalf("wrong current: %v", err)
	}
	if off, err := Update(h, "1234", ""); err != nil || off != "" {
		t.Fatalf("turn off = %q, %v", off, err)
	}
	if _, err := Update("", "", "12"); !errors.Is(err, ErrPINFormat) {
		t.Fatalf("short pin: %v", err)
	}
}

func TestBackoff(t *testing.T) {
	var b Backoff
	now := time.Unix(1000, 0)
	fail := func(n int) {
		for i := 0; i < n; i++ {
			b.Failed(now)
		}
	}
	fail(4)
	if w := b.Wait(now); w != 0 {
		t.Fatalf("after 4: %v", w)
	}
	fail(1)
	if w := b.Wait(now); w != 30*time.Second {
		t.Fatalf("after 5: %v", w)
	}
	if w := b.Wait(now.Add(30 * time.Second)); w != 0 {
		t.Fatalf("after 30s: %v", w)
	}
	fail(5)
	if w := b.Wait(now); w != 60*time.Second {
		t.Fatalf("after 10: %v", w)
	}
	fail(5)
	if w := b.Wait(now); w != 120*time.Second {
		t.Fatalf("after 15: %v", w)
	}
	b.Succeeded()
	if w := b.Wait(now); w != 0 {
		t.Fatalf("after success: %v", w)
	}
	fail(4)
	if w := b.Wait(now); w != 0 {
		t.Fatalf("counting did not restart: %v", w)
	}
}
