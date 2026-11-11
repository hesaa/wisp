// Package lock hashes and verifies the app-lock PIN and throttles wrong tries.
package lock

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	scheme     = "pbkdf2-sha256"
	iterations = 600000
	keyLen     = 32
	saltLen    = 16
)

var (
	ErrPINFormat = errors.New("PIN must be 4 to 8 digits")
	ErrWrongPIN  = errors.New("wrong PIN")
	ErrStored    = errors.New("stored PIN hash is malformed")
)

// ValidPIN accepts 4-8 ASCII digits only.
func ValidPIN(pin string) error {
	if len(pin) < 4 || len(pin) > 8 {
		return ErrPINFormat
	}
	for i := 0; i < len(pin); i++ {
		if pin[i] < '0' || pin[i] > '9' {
			return ErrPINFormat
		}
	}
	return nil
}

func derive(pin string, salt []byte, iter int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, pin, salt, iter, keyLen)
}

// Hash returns "pbkdf2-sha256$<iterations>$<salt>$<key>" with a fresh random salt.
func Hash(pin string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := derive(pin, salt, iterations)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("%s$%d$%s$%s", scheme, iterations, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// Verify checks pin against a stored hash; ErrStored when stored is malformed.
func Verify(stored, pin string) (bool, error) {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != scheme {
		return false, ErrStored
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false, ErrStored
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false, ErrStored
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) != keyLen {
		return false, ErrStored
	}
	got, err := derive(pin, salt, iter)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// Update returns the new stored value: "" turns the lock off. When a PIN is
// already set, current must verify.
func Update(stored, current, next string) (string, error) {
	if stored != "" {
		ok, err := Verify(stored, current)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", ErrWrongPIN
		}
	}
	if next == "" {
		return "", nil
	}
	if err := ValidPIN(next); err != nil {
		return "", err
	}
	return Hash(next)
}

// Backoff blocks tries for 30 s after every 5th wrong one, doubling each time.
// In-memory only; the zero value is ready to use.
type Backoff struct {
	fails int
	until time.Time
}

// Wait is how long the caller must still wait before the next try.
func (b *Backoff) Wait(now time.Time) time.Duration {
	if d := b.until.Sub(now); d > 0 {
		return d
	}
	return 0
}

func (b *Backoff) Failed(now time.Time) {
	b.fails++
	if b.fails%5 == 0 {
		b.until = now.Add(30 * time.Second << (b.fails/5 - 1))
	}
}

func (b *Backoff) Succeeded() { *b = Backoff{} }
