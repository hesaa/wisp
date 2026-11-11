package ui

import (
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/lxn/win"
)

func TestClockText(t *testing.T) {
	cases := map[time.Duration]string{
		5 * time.Second:    "0:05",
		62 * time.Second:   "1:02",
		3723 * time.Second: "1:02:03",
		0:                  "0:00",
	}
	for d, want := range cases {
		if got := clockText(d); got != want {
			t.Errorf("clockText(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestSeekAt(t *testing.T) {
	total := 45 * time.Second
	cases := map[int]time.Duration{50: 11250 * time.Millisecond, -3: 0, 250: total}
	for x, want := range cases {
		if got := seekAt(x, 200, total); got != want {
			t.Errorf("seekAt(%d, 200, 45s) = %v, want %v", x, got, want)
		}
	}
}

func TestGifRestart(t *testing.T) {
	total := 3 * time.Second
	cases := []struct {
		name string
		st   playerState
		pos  time.Duration
		want bool
	}{
		{"stopped at the end", playerStopped, total, true},
		{"stopped just before the end", playerStopped, total - 50*time.Millisecond, true},
		{"playing", playerPlaying, total, false},
		{"stopped at 0, never played", playerStopped, 0, false},
	}
	for _, c := range cases {
		if got := gifRestart(c.st, c.pos, total); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// A poll late enough to judge the stopped GIF short of its end only delays the restart: the next
// stopped poll restarts it.
func TestGifRestartAfterLatePoll(t *testing.T) {
	total := 3 * time.Second
	at := gifReached(playerPlaying, total-400*time.Millisecond, 0) // the last poll while it played
	at = gifReached(playerStopped, 0, at)                          // late: mfplay already rewound
	if gifRestart(playerStopped, at, total) {
		t.Fatalf("restarted at %v, before the end could be told", at)
	}
	at = gifReached(playerStopped, 0, at)
	if !gifRestart(playerStopped, at, total) {
		t.Errorf("not restarted at %v on the poll after the late one", at)
	}
	if got := gifReached(playerPlaying, time.Second, at); got != time.Second {
		t.Errorf("playing again: %v, want 1s", got)
	}
}

// Gated live check: plays a local H.264 MP4 through mfplay.dll in a hidden window.
func TestPlayerPlaysMP4(t *testing.T) {
	path := os.Getenv("WISP_TEST_MP4")
	if path == "" {
		t.Skip("set WISP_TEST_MP4 to a local H.264 MP4")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if hr := win.CoInitializeEx(nil, win.COINIT_APARTMENTTHREADED); hr < 0 {
		t.Fatalf("CoInitializeEx: %#x", uint32(hr))
	}
	defer win.CoUninitialize()
	hwnd := win.CreateWindowEx(0, syscall.StringToUTF16Ptr("STATIC"), nil, win.WS_POPUP, 0, 0, 320, 240, 0, 0, 0, nil)
	if hwnd == 0 {
		t.Fatal("CreateWindowEx failed")
	}
	defer win.DestroyWindow(hwnd)
	p, err := newPlayer(hwnd, path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown()
	wait := func(want playerState) {
		t.Helper()
		var msg win.MSG
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			for win.PeekMessage(&msg, 0, 0, 0, win.PM_REMOVE) {
				win.TranslateMessage(&msg)
				win.DispatchMessage(&msg)
			}
			if st, err := p.State(); err != nil {
				t.Fatal(err)
			} else if st == want {
				return
			}
		}
		st, _ := p.State()
		t.Fatalf("state %d after 5 s, want %d", st, want)
	}
	wait(playerPlaying)
	if err := p.SetMute(true); err != nil {
		t.Errorf("SetMute: %v", err)
	}
	if d, err := p.Duration(); err != nil || d <= 0 {
		t.Errorf("Duration: %v, %v", d, err)
	}
	if err := p.Pause(); err != nil {
		t.Fatal(err)
	}
	wait(playerPaused)
	if err := p.Seek(time.Second); err != nil {
		t.Errorf("Seek: %v", err)
	}
	if _, err := p.Position(); err != nil {
		t.Errorf("Position: %v", err)
	}
	if err := p.UpdateVideo(); err != nil {
		t.Errorf("UpdateVideo: %v", err)
	}
	p.Shutdown()
	if _, err := p.State(); err == nil {
		t.Error("State after Shutdown: no error")
	}
}
