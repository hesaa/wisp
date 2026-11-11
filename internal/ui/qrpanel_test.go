package ui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lxn/walk"
	"go.mau.fi/whatsmeow"
)

// matrixFromRects paints dark back into an n×n matrix of module m on paper; it fails on a rectangle
// that is not whole modules.
func matrixFromRects(t *testing.T, n, m int, paper walk.Rectangle, dark []walk.Rectangle) [][]bool {
	t.Helper()
	got := make([][]bool, n)
	for i := range got {
		got[i] = make([]bool, n)
	}
	for _, r := range dark {
		x, y := r.X-paper.X-qrQuiet*m, r.Y-paper.Y-qrQuiet*m
		if x%m != 0 || y%m != 0 || r.Width%m != 0 || r.Width <= 0 || r.Height != m {
			t.Fatalf("rect %+v is not whole %dpx modules", r, m)
		}
		for i := 0; i < r.Width/m; i++ {
			got[y/m][x/m+i] = true
		}
	}
	return got
}

func TestQRModulesAreWholePixelsInsideAQuietZone(t *testing.T) {
	const n = 21
	black := func(x, y int) bool { return y == 0 || (x+y)%3 == 0 }
	paper, dark := qrRects(n, black, walk.Rectangle{X: 10, Y: 20, Width: 300, Height: 340})
	// 300 px across 21 modules plus 4 quiet modules a side: 10 px modules, 290 px of paper, centred
	if want := (walk.Rectangle{X: 15, Y: 45, Width: 290, Height: 290}); paper != want {
		t.Fatalf("paper = %+v, want %+v", paper, want)
	}
	got := matrixFromRects(t, n, 10, paper, dark)
	for y := range n {
		for x := range n {
			if got[y][x] != black(x, y) {
				t.Fatalf("module (%d,%d) dark = %v, want %v", x, y, got[y][x], black(x, y))
			}
		}
	}
	if dark[0].Width != n*10 {
		t.Errorf("a dark row should be one rectangle, got %+v", dark[0])
	}
}

func TestQRModulesNeverShrinkBelowAPixel(t *testing.T) {
	paper, dark := qrRects(21, func(int, int) bool { return true }, walk.Rectangle{Width: 10, Height: 10})
	if paper.Width != 29 || paper.Height != 29 || len(dark) != 21 {
		t.Fatalf("paper %+v with %d runs; want 29 px of 1 px modules, one run per row", paper, len(dark))
	}
}

func TestQRStepTurnsChannelItemsIntoViews(t *testing.T) {
	for _, c := range []struct {
		item                whatsmeow.QRChannelItem
		code                string
		failed, done, shown bool
		status              string
	}{
		{item: whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "2@abc"}, code: "2@abc", shown: true},
		{item: whatsmeow.QRChannelSuccess, done: true, shown: true},
		{item: whatsmeow.QRChannelTimeout, failed: true, shown: true, status: "expired"},
		{item: whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventError, Error: errors.New("bad pairing")}, failed: true, shown: true, status: "bad pairing"},
		{item: whatsmeow.QRChannelClientOutdated, failed: true, shown: true, status: "too old"},
		{item: whatsmeow.QRChannelScannedWithoutMultidevice, failed: true, shown: true, status: "multi-device"},
		{item: whatsmeow.QRChannelErrUnexpectedEvent, failed: true, shown: true, status: "failed"},
		{item: whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventPasskeyRequest}},
	} {
		v, ok := qrStep(c.item)
		if ok != c.shown || v.code != c.code || v.failed != c.failed || v.done != c.done || !strings.Contains(v.status, c.status) {
			t.Errorf("qrStep(%q) = %+v, %v", c.item.Event, v, ok)
		}
	}
}

// runPairing runs pairing over items (closed afterwards unless open) and returns the views shown.
func runPairing(t *testing.T, ctx context.Context, items []whatsmeow.QRChannelItem, open bool, pairErr error) []qrView {
	t.Helper()
	ch := make(chan whatsmeow.QRChannelItem, len(items))
	for _, it := range items {
		ch <- it
	}
	if !open {
		close(ch)
	}
	var views []qrView
	done := make(chan struct{})
	go func() {
		defer close(done)
		pairing(ctx, func(context.Context) (<-chan whatsmeow.QRChannelItem, error) {
			if pairErr != nil {
				return nil, pairErr
			}
			return ch, nil
		}, func(f func()) { f() }, func(v qrView) { views = append(views, v) })
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pairing did not return")
	}
	return views
}

func TestPairingShowsEachCodeThenTheOutcome(t *testing.T) {
	code := func(c string) whatsmeow.QRChannelItem {
		return whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: c, Timeout: time.Minute}
	}
	views := runPairing(t, context.Background(), []whatsmeow.QRChannelItem{code("one"), {Event: whatsmeow.QRChannelEventPasskeyRequest}, code("two"), whatsmeow.QRChannelSuccess}, false, nil)
	if len(views) != 3 || views[0].code != "one" || views[1].code != "two" || !views[2].done {
		t.Fatalf("views = %+v; want code one, code two, done", views)
	}
}

func TestPairingFailures(t *testing.T) {
	if v := runPairing(t, context.Background(), nil, false, errors.New("no network")); len(v) != 1 || !v[0].failed || !strings.Contains(v[0].status, "no network") {
		t.Errorf("pair error: views = %+v", v)
	}
	if v := runPairing(t, context.Background(), []whatsmeow.QRChannelItem{{Event: whatsmeow.QRChannelEventCode, Code: "c"}}, false, nil); len(v) != 2 || !v[1].failed {
		t.Errorf("channel closed without an outcome: views = %+v; want the code, then a failure", v)
	}
	if v := runPairing(t, context.Background(), []whatsmeow.QRChannelItem{whatsmeow.QRChannelTimeout}, false, nil); len(v) != 1 || !v[0].failed {
		t.Errorf("timeout: views = %+v; want exactly one failure", v)
	}
}

func TestCancelledPairingShowsNothingAndReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// the channel stays open: pairing must not wait for it once cancelled
	if v := runPairing(t, ctx, []whatsmeow.QRChannelItem{{Event: whatsmeow.QRChannelEventCode, Code: "c"}}, true, nil); len(v) != 0 {
		t.Fatalf("views after cancel = %+v", v)
	}
	if v := runPairing(t, ctx, nil, false, errors.New("context canceled")); len(v) != 0 {
		t.Fatalf("pair error after cancel shown: %+v", v)
	}
}
