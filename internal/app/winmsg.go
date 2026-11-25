package app

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"syscall"
	"unsafe"

	"github.com/lxn/win"
	"golang.org/x/sys/windows"

	"wa-go/internal/notify"
	"wa-go/internal/ui"
)

// The main window's messages walk does not publish, both for presence: WM_ACTIVATEAPP (the user
// switched to Wisp, any of its windows, or away to another program) and session lock notifications.

const (
	wmCopyData           = 0x004A
	wmWTSSessionChange   = 0x02B1
	wtsSessionLock       = 0x7
	wtsSessionUnlock     = 0x8
	notifyForThisSession = 0
)

type copyDataStruct struct {
	dwData uintptr
	cbData uint32
	lpData uintptr
}

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
	case wmCopyData:
		cds := (*copyDataStruct)(unsafe.Pointer(lParam))
		if cds != nil && cds.cbData > 0 {
			buf := make([]byte, cds.cbData)
			copy(buf, unsafe.Slice((*byte)(unsafe.Pointer(cds.lpData)), cds.cbData))
			hooked.handleProtocolRaw(string(buf))
			return 1
		}
	case win.WM_ACTIVATEAPP:
		hooked.presence.activated(wParam != 0)
	case win.WM_SYSCOMMAND:
		if wParam&0xFFF0 == win.SC_MINIMIZE {
			if parseLockTimeout(hooked.cfg[ui.KeyLockTimeout]) == 0 {
				hooked.lockNow() // while the window still paints: the taskbar preview and Alt+Tab keep the lock page
				// UpdateWindow, children included: the lock page is a child window
				win.RedrawWindow(hwnd, nil, 0, win.RDW_INVALIDATE|win.RDW_UPDATENOW|win.RDW_ALLCHILDREN)
			}
		}
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
	endMenu           = user32.NewProc("EndMenu")
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

var dwmSetWindowAttribute = windows.NewLazySystemDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute")

const dwmwaForceIconicRepresentation = 7

// forceIconic makes the taskbar preview and Alt+Tab show the icon instead of the window while on
// (a PIN is set): a window minimised by Win+D never repaints the lock page first.
func forceIconic(h win.HWND, on bool) error {
	if err := dwmSetWindowAttribute.Find(); err != nil {
		return err
	}
	v := int32(0) // BOOL
	if on {
		v = 1
	}
	r, _, _ := dwmSetWindowAttribute.Call(uintptr(h), dwmwaForceIconicRepresentation, uintptr(unsafe.Pointer(&v)), unsafe.Sizeof(v))
	if int32(r) < 0 {
		return fmt.Errorf("app: DwmSetWindowAttribute: HRESULT %#x", uint32(r))
	}
	return nil
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

type protocolAction struct {
	kind      string // "open" or "reply"
	accountID string
	chatJID   string
	text      string
}

func parseProtocolAction(raw string) (protocolAction, bool) {
	if raw == "" {
		return protocolAction{}, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "wisp" {
		return protocolAction{}, false
	}
	kind := u.Host
	if kind == "" {
		kind = strings.TrimPrefix(u.Path, "/")
	}
	kind = strings.ToLower(strings.Trim(kind, "/"))
	if kind != "open" && kind != "reply" && kind != "markread" {
		return protocolAction{}, false
	}
	q := u.Query()
	action := protocolAction{
		kind:      kind,
		accountID: q.Get("acct"),
		chatJID:   q.Get("jid"),
	}
	if kind == "reply" {
		text := q.Get("replyText")
		if text == "" {
			text = q.Get("text")
		}
		if text == "" {
			text = q.Get("msg")
		}
		action.text = text
	}
	return action, true
}

func (a *App) handleProtocolRaw(raw string) {
	action, ok := parseProtocolAction(raw)
	if !ok {
		return
	}
	switch action.kind {
	case "open":
		a.toastClicked(action.accountID, action.chatJID)
	case "reply":
		if action.text != "" {
			a.sendQuickReply(action.accountID, action.chatJID, action.text)
		}
	case "markread":
		if !a.locked && action.accountID != "" && action.chatJID != "" {
			a.mw.Synchronize(func() {
				a.markChatRead(chatRef{action.accountID, action.chatJID})
			})
			_ = notify.ClearChatToasts("Wisp.App", action.chatJID, action.accountID)
		}
	}
}

func (a *App) sendQuickReply(acct, jid, text string) {
	if a.locked {
		return // locked with PIN, ignore quick replies
	}
	if acct == "" || jid == "" || text == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), netTimeout)
		defer cancel()
		if _, err := a.mgr.SendText(ctx, acct, jid, text, nil, nil); err != nil {
			logErr("quick reply", err)
		}
	}()
}

