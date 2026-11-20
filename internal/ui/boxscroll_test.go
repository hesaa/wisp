package ui

import "testing"

func TestWheelLines(t *testing.T) {
	rem := 0
	if n := wheelLines(&rem, -120); n != -3 {
		t.Fatalf("a notch down: %d lines, want -3", n)
	}
	got := 0
	for range 8 { // a touchpad: eight steps of 15 make one notch
		got += wheelLines(&rem, 15)
	}
	if got != 3 || rem != 0 {
		t.Fatalf("eight touchpad steps: %d lines, rem %d; want 3, 0", got, rem)
	}
	wheelLines(&rem, 30) // 90: no line yet
	if n := wheelLines(&rem, -30); n != 0 || rem != 0 {
		t.Fatalf("back and forth: %d lines, rem %d; want 0, 0", n, rem)
	}
}
