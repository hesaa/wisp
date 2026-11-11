package ui

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/lxn/walk"
	"github.com/lxn/win"

	"wa-go/internal/ui/icons"
)

// focusOn gives w the keyboard focus. walk's SetFocus reports a failure whenever no window had the
// focus before (so whenever a dialog opens), so the outcome is checked instead.
func focusOn(w walk.Window) {
	if win.SetFocus(w.Handle()); win.GetFocus() != w.Handle() {
		logErr("focus", errors.New("ui: SetFocus failed"))
	}
}

// navigate returns where keyboard focus moves from item cur (-1: none) among n items for key k:
// Up/Left and Down/Right step, PgUp/PgDn move page items, Home/End jump to the ends. With wrap,
// stepping past an end continues at the other one; paging and Home/End never wrap. It returns -1
// when there are no items, and ok false for keys it does not handle.
func navigate(cur, n, page int, k walk.Key, wrap bool) (next int, ok bool) {
	cur = min(cur, n-1)
	step := k == walk.KeyUp || k == walk.KeyLeft || k == walk.KeyDown || k == walk.KeyRight
	switch k {
	case walk.KeyUp, walk.KeyLeft:
		next = cur - 1
	case walk.KeyDown, walk.KeyRight:
		next = cur + 1
	case walk.KeyPrior:
		next = cur - max(1, page)
	case walk.KeyNext:
		next = cur + max(1, page)
	case walk.KeyHome:
		next = 0
	case walk.KeyEnd:
		next = n - 1
	default:
		return cur, false
	}
	switch {
	case n <= 0:
		return -1, true
	case wrap && step && next < 0:
		return n - 1, true
	case wrap && step && next >= n:
		return 0, true
	}
	return max(0, min(next, n-1)), true
}

// scrollIntoView returns the scroll offset that brings [start, start+size) into a view of length
// view now scrolled to off, moving as little as possible; an item longer than the view shows its start.
func scrollIntoView(off, start, size, view int) int {
	switch {
	case start < off:
		return start
	case start+size > off+view:
		return min(start, start+size-view)
	}
	return off
}

// span is a scrolled one-dimensional list: n items of size pixels laid out from pad, seen through a
// view scrolled to off.
type span struct{ off, pad, size, n int }

// at is the item under view position pos, -1 for none.
func (s span) at(pos int) int {
	p := pos + s.off - s.pad
	if p < 0 || s.size <= 0 || p/s.size >= s.n {
		return -1
	}
	return p / s.size
}

// firstFull is the first item wholly in view (-1 when there are none): where focus rests when a
// widget gains it with nothing focused, so gaining focus never scrolls.
func (s span) firstFull() int {
	if s.n <= 0 || s.size <= 0 {
		return -1
	}
	return min(max(0, (s.off-s.pad+s.size-1)/s.size), s.n-1)
}

// keyHook routes the keys of a custom-painted widget to Go.
type keyHook struct {
	prev  uintptr // the window procedure walk installed
	onKey func(walk.Key) bool
	esc   func() bool // whether onKey takes Esc now (takeEscape); nil: never
	// menu is asked before a context menu opens, with the screen point it was asked for ((-1, -1) from the
	// keyboard); it gives where to open it, or false for none (takeContextMenu); nil: as asked.
	menu func(x, y int32) (int32, int32, bool)
}

var (
	keyHooks   = map[win.HWND]*keyHook{} // UI thread only
	keyHookPtr = syscall.NewCallback(keyHookProc)
)

func keyHookProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	h := keyHooks[hwnd]
	switch msg {
	case win.WM_GETDLGCODE:
		// walk's message loop runs IsDialogMessage, which spends arrows and Enter on moving focus
		// between controls, and typed characters on mnemonics, unless the focused window asks for
		// them; Tab keeps working, and so does Esc (Cancel) unless the widget takes it now.
		if wParam == win.VK_RETURN || wParam == win.VK_ESCAPE && h.esc != nil && h.esc() {
			return win.DLGC_WANTARROWS | win.DLGC_WANTCHARS | win.DLGC_WANTALLKEYS
		}
		return win.DLGC_WANTARROWS | win.DLGC_WANTCHARS
	case win.WM_CONTEXTMENU:
		if h.menu != nil {
			x, y, ok := h.menu(win.GET_X_LPARAM(lParam), win.GET_Y_LPARAM(lParam))
			if !ok {
				return 0
			}
			lParam = uintptr(uint32(uint16(x)) | uint32(uint16(y))<<16)
		}
	case win.WM_KEYDOWN: // handled here, not via walk's KeyDown, which drops auto-repeat
		if h.onKey(walk.Key(wParam)) {
			showFocusCues(hwnd)
			return 0
		}
	case win.WM_UPDATEUISTATE: // focus cues were shown or hidden: repaint the indicator
		r := win.CallWindowProc(h.prev, hwnd, msg, wParam, lParam)
		win.InvalidateRect(hwnd, nil, false)
		return r
	case win.WM_NCDESTROY:
		delete(keyHooks, hwnd)
	}
	return win.CallWindowProc(h.prev, hwnd, msg, wParam, lParam)
}

// focusable makes cw a tab stop with accessible role and name: a focus change repaints it (for its
// focus indicator), and key presses, auto-repeat included, go to onKey, which reports whether it
// handled the key. The widget's own click handler calls SetFocus, after it has resolved the item
// under the cursor (walk runs MouseDown handlers in attach order and WM_SETFOCUS synchronously).
func focusable(cw *walk.CustomWidget, role walk.AccRole, name string, onKey func(walk.Key) bool) error {
	h := cw.Handle()
	keyHooks[h] = &keyHook{prev: win.GetWindowLongPtr(h, win.GWLP_WNDPROC), onKey: onKey}
	if win.SetWindowLongPtr(h, win.GWLP_WNDPROC, keyHookPtr) == 0 {
		delete(keyHooks, h)
		return fmt.Errorf("ui: subclassing a custom widget failed")
	}
	if win.SetWindowLong(h, win.GWL_STYLE, win.GetWindowLong(h, win.GWL_STYLE)|win.WS_TABSTOP) == 0 {
		return fmt.Errorf("ui: SetWindowLong(GWL_STYLE) failed")
	}
	cw.FocusedChanged().Attach(func() { logErr("focus repaint", cw.Invalidate()) })
	acc := cw.Accessibility()
	return errors.Join(acc.SetRole(role), acc.SetName(name))
}

// takeEscape has Esc go to cw's onKey while when() reports true, not to the dialog as Cancel.
func takeEscape(cw *walk.CustomWidget, when func() bool) { keyHooks[cw.Handle()].esc = when }

// takeContextMenu lets where decide where cw's context menu opens, or whether it does (keyHook.menu).
func takeContextMenu(cw *walk.CustomWidget, where func(x, y int32) (int32, int32, bool)) {
	keyHooks[cw.Handle()].menu = where
}

// focusCues reports whether w should paint its focus indicator: it has the focus and Windows shows
// focus cues, which it hides until the keyboard is used (so clicks don't leave rings behind).
func focusCues(w walk.Window) bool {
	return w.Focused() && w.SendMessage(win.WM_QUERYUISTATE, 0, 0)&win.UISF_HIDEFOCUS == 0
}

// showFocusCues turns focus cues on for hwnd's window, as native controls do on keyboard use; the
// top-level window then sends WM_UPDATEUISTATE to every child.
func showFocusCues(hwnd win.HWND) {
	win.SendMessage(hwnd, win.WM_CHANGEUISTATE, win.UIS_CLEAR|win.UISF_HIDEFOCUS<<16, 0)
}

// announce names w for screen readers as label plus its focused item, if any; walk raises the
// name-change event they follow.
func announce(w walk.Window, label, item string) {
	if item != "" {
		label += ": " + item
	}
	logErr("accessible name", w.AsWindowBase().Accessibility().SetName(label))
}

// focusRing paints a keyboard-focus ring of diameter d and width w around (cx, cy), with bg inside;
// paint the item over it.
func focusRing(c *walk.Canvas, cx, cy, d, w int, color, bg walk.Color) error {
	return errors.Join(icons.Fill(c, square(cx, cy, d), color), icons.Fill(c, square(cx, cy, d-2*w), bg))
}

// focusRect outlines r with a w-pixel border in hex.
func focusRect(c *walk.Canvas, r walk.Rectangle, w int, hex string) error {
	b, err := brushFor(hex)
	if err != nil {
		return err
	}
	return errors.Join(
		c.FillRectanglePixels(b, walk.Rectangle{X: r.X, Y: r.Y, Width: r.Width, Height: w}),
		c.FillRectanglePixels(b, walk.Rectangle{X: r.X, Y: r.Y + r.Height - w, Width: r.Width, Height: w}),
		c.FillRectanglePixels(b, walk.Rectangle{X: r.X, Y: r.Y, Width: w, Height: r.Height}),
		c.FillRectanglePixels(b, walk.Rectangle{X: r.X + r.Width - w, Y: r.Y, Width: w, Height: r.Height}))
}
