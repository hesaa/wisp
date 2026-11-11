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
			hooked.lockNow()
		case wtsSessionUnlock:
			hooked.presence.sessionLocked(false)
		}
	case win.WM_DESTROY:
		wtsUnregister.Call(uintptr(hwnd))
	}
	return win.CallWindowProc(hookedPrev, hwnd, msg, wParam, lParam)
}

var (
	user32            = windows.NewLazySystemDLL("user32.dll")
	enumThreadWindows = user32.NewProc("EnumThreadWindows")
	modalPtr          = syscall.NewCallback(collectModal)
	modalOwner        win.HWND // closeModals' arguments and result; UI thread only
	modals            []win.HWND
)

func collectModal(h, _ uintptr) uintptr {
	// hidden ones count: Windows hides a minimised window's owned windows before it tells the window
	var buf [64]uint16
	n, _ := win.GetClassName(win.HWND(h), &buf[0], len(buf))
	if w := win.HWND(h); modalClass(windows.UTF16ToString(buf[:max(n, 0)])) {
		for o := win.GetWindow(w, win.GW_OWNER); o != 0; o = win.GetWindow(o, win.GW_OWNER) {
			if o == modalOwner {
				modals = append(modals, w)
				break
			}
		}
	}
	return 1
}

// closeModals asks every dialog this thread has that is owned (directly or through another
// dialog) by owner to close: the Settings, New chat and status dialogs, their PIN, file and message
// boxes. They close once control returns to their message loops.
func closeModals(owner win.HWND) {
	modalOwner, modals = owner, nil
	enumThreadWindows.Call(uintptr(windows.GetCurrentThreadId()), modalPtr, 0)
	for _, h := range modals {
		win.PostMessage(h, win.WM_CLOSE, 0, 0)
	}
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
