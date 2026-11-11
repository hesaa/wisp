package app

import (
	"fmt"
	"syscall"

	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

// The main window's messages walk does not publish, both for presence: WM_ACTIVATEAPP (the user
// switched to Wisp, any of its windows, or away to another program) and session lock notifications.

const (
	wmWTSSessionChange   = 0x02B1
	wtsSessionLock       = 0x7
	wtsSessionUnlock     = 0x8
	notifyForThisSession = 0
)

var (
	wtsapi32      = windows.NewLazySystemDLL("wtsapi32.dll")
	wtsRegister   = wtsapi32.NewProc("WTSRegisterSessionNotification")
	wtsUnregister = wtsapi32.NewProc("WTSUnRegisterSessionNotification")

	hooked     *App    // the app whose main window is subclassed; UI thread only
	hookedPrev uintptr // the window procedure walk installed
	hookPtr    = syscall.NewCallback(mainWndProc)
)

func mainWndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case win.WM_ACTIVATEAPP:
		hooked.presence.activated(wParam != 0)
	case wmWTSSessionChange:
		switch wParam {
		case wtsSessionLock:
			hooked.presence.sessionLocked(true)
		case wtsSessionUnlock:
			hooked.presence.sessionLocked(false)
		}
	case win.WM_DESTROY:
		wtsUnregister.Call(uintptr(hwnd))
	}
	return win.CallWindowProc(hookedPrev, hwnd, msg, wParam, lParam)
}

// watchActivation subclasses the main window for WM_ACTIVATEAPP and registers it for session lock
// notifications.
func (a *App) watchActivation() error {
	h := a.mw.Handle()
	hooked = a
	if hookedPrev = win.SetWindowLongPtr(h, win.GWLP_WNDPROC, hookPtr); hookedPrev == 0 {
		return fmt.Errorf("app: subclassing the main window failed")
	}
	if r, _, err := wtsRegister.Call(uintptr(h), notifyForThisSession); r == 0 {
		return fmt.Errorf("app: session lock notifications: %w", err)
	}
	return nil
}
