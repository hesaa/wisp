package ui

import (
	"syscall"
	"testing"
	"time"

	"github.com/lxn/win"
)

func TestGifPlayerPlaysWithoutGpuDrivers(t *testing.T) {
	// Create hidden test window
	hwnd := win.CreateWindowEx(0, syscall.StringToUTF16Ptr("STATIC"), nil, win.WS_POPUP, 0, 0, 320, 240, 0, 0, 0, nil)
	if hwnd == 0 {
		t.Fatal("CreateWindowEx failed")
	}
	defer win.DestroyWindow(hwnd)

	gp, err := newGifPlayer(hwnd, "testdata/tiny.mp4")
	if err != nil {
		t.Fatalf("newGifPlayer failed: %v", err)
	}
	defer gp.Shutdown()

	st, err := gp.State()
	if err != nil || st != playerPlaying {
		t.Fatalf("gp.State() = %v, %v; want playerPlaying", st, err)
	}

	// Verify UpdateVideo works without crashing
	if err := gp.UpdateVideo(); err != nil {
		t.Errorf("UpdateVideo failed: %v", err)
	}

	// Verify that EVR (Enhanced Video Renderer) and Intel GPU compiler dlls are NOT loaded
	if h := win.GetModuleHandle(syscall.StringToUTF16Ptr("evr.dll")); h != 0 {
		t.Errorf("evr.dll is loaded; gifPlayer should not use EVR hardware acceleration")
	}
	if h := win.GetModuleHandle(syscall.StringToUTF16Ptr("igc64.dll")); h != 0 {
		t.Errorf("igc64.dll (Intel Graphics Compiler) is loaded; gifPlayer should use software Source Reader")
	}

	time.Sleep(100 * time.Millisecond)
}
