package app

import (
	"testing"
	"time"

	"wa-go/internal/ui"
)

func TestAppOnHidden_ImmediateLock(t *testing.T) {
	a := &App{cfg: map[string]string{ui.KeyLockPIN: "somehash", ui.KeyLockTimeout: "0"}}
	a.onHidden()
	if !a.locked {
		t.Error("expected a.locked == true for immediate timeout")
	}
	if !a.hiddenAt.IsZero() {
		t.Error("expected hiddenAt to be zero for immediate timeout")
	}
}

func TestAppOnHidden_WithTimeout(t *testing.T) {
	a := &App{cfg: map[string]string{ui.KeyLockPIN: "somehash", ui.KeyLockTimeout: "5"}}
	a.onHidden()
	defer func() {
		if a.lockTimer != nil {
			a.lockTimer.Stop()
		}
	}()
	if a.locked {
		t.Error("expected a.locked == false when timeout > 0")
	}
	if a.hiddenAt.IsZero() {
		t.Error("expected hiddenAt to be non-zero when timeout > 0")
	}
	if a.lockTimer == nil {
		t.Error("expected lockTimer to be armed")
	}
}

func TestAppOnRestored_BeforeTimeout(t *testing.T) {
	a := &App{
		cfg:      map[string]string{ui.KeyLockPIN: "somehash", ui.KeyLockTimeout: "5"},
		hiddenAt: time.Now().Add(-1 * time.Minute),
	}
	a.onRestored()
	if a.locked {
		t.Error("expected a.locked == false when restored before timeout")
	}
	if !a.hiddenAt.IsZero() {
		t.Error("expected hiddenAt to be cleared after restore")
	}
}

func TestAppOnRestored_AfterTimeout(t *testing.T) {
	a := &App{
		cfg:      map[string]string{ui.KeyLockPIN: "somehash", ui.KeyLockTimeout: "5"},
		hiddenAt: time.Now().Add(-6 * time.Minute),
	}
	a.onRestored()
	if !a.locked {
		t.Error("expected a.locked == true when restored after timeout")
	}
	if !a.hiddenAt.IsZero() {
		t.Error("expected hiddenAt to be cleared after restore")
	}
}
