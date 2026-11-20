package ui

import (
	"unsafe"

	"github.com/lxn/win"

	"wa-go/internal/theme"
)

// The message box's scroll bar: past inputMaxLines lines a thin thumb like the chat's (vscroll)
// along the box's right edge, in a margin text never reaches, dragged to scroll; the wheel scrolls
// three lines a notch. The box is an edit control, so the thumb is painted over it after each
// redraw (enterHook.over) and the mouse is handled before it (enterHook.mouse).

const (
	boxBarW       = 10 // the margin the thumb sits in, 1/96 inch
	boxThumbW     = 5
	boxThumbMinH  = 20
	ecRightMargin = 2 // EM_SETMARGINS
)

// boxBar is the box's scroll state: lines in all, how many show, the first shown, the client
// rectangle and the thumb in it; ok is false while every line shows.
func (b *InputBar) boxBar() (n, vis, first int, client, thumb win.RECT, ok bool) {
	h := b.edit.Handle()
	n = int(win.SendMessage(h, win.EM_GETLINECOUNT, 0, 0))
	win.GetClientRect(h, &client)
	lh, ch := lineHeight(h), int(client.Bottom-client.Top)
	if lh <= 0 || ch <= 0 {
		return n, 0, 0, client, thumb, false
	}
	vis = max(1, ch/lh)
	if n <= vis {
		return n, vis, 0, client, thumb, false
	}
	first = int(win.SendMessage(h, win.EM_GETFIRSTVISIBLELINE, 0, 0))
	s := b.edit.IntFrom96DPI
	th := max(s(boxThumbMinH), ch*vis/n)
	y := min(first, n-vis) * (ch - th) / (n - vis)
	right := int(client.Right) - (s(boxBarW)-s(boxThumbW))/2
	thumb = win.RECT{Left: int32(right - s(boxThumbW)), Top: int32(y), Right: int32(right), Bottom: int32(y + th)}
	return n, vis, first, client, thumb, true
}

// paintBar paints the margin, and the thumb in it while the box scrolls.
func (b *InputBar) paintBar() {
	h := b.edit.Handle()
	hdc := win.GetDC(h)
	if hdc == 0 {
		return
	}
	defer win.ReleaseDC(h, hdc)
	_, _, _, client, thumb, ok := b.boxBar()
	strip := client
	strip.Left = client.Right - int32(b.edit.IntFrom96DPI(boxBarW))
	fill := func(hex string, paint func(br uintptr)) {
		c := theme.Color(hex)
		br, _, _ := pCreateSolidBr.Call(uintptr(c))
		if br == 0 {
			return
		}
		defer win.DeleteObject(win.HGDIOBJ(br))
		paint(br)
	}
	fill(b.pal.Sidebar, func(br uintptr) { pFillRect.Call(uintptr(hdc), uintptr(unsafe.Pointer(&strip)), br) })
	if !ok {
		return
	}
	fill(b.pal.TextSecondary, func(br uintptr) {
		oldBr := win.SelectObject(hdc, win.HGDIOBJ(br))
		oldPen := win.SelectObject(hdc, win.GetStockObject(win.NULL_PEN))
		w := thumb.Right - thumb.Left
		win.RoundRect(hdc, thumb.Left, thumb.Top, thumb.Right+1, thumb.Bottom+1, w, w) // a NULL_PEN shape is a pixel short
		win.SelectObject(hdc, oldPen)
		win.SelectObject(hdc, oldBr)
	})
}

// scrollBox scrolls the box so line first is its top one.
func (b *InputBar) scrollBox(first int) {
	n, vis, now, _, _, ok := b.boxBar()
	if !ok {
		return
	}
	if first = max(0, min(first, n-vis)); first != now {
		b.edit.SendMessage(win.EM_LINESCROLL, 0, uintptr(first-now))
	}
}

// boxMouse takes the wheel and drags of the thumb before the box sees them, and reports whether it
// did.
func (b *InputBar) boxMouse(msg uint32, wParam, lParam uintptr) bool {
	x, y := int32(int16(lParam)), int32(int16(lParam>>16))
	switch msg {
	case win.WM_MOUSEWHEEL:
		_, _, first, _, _, ok := b.boxBar()
		if ok {
			b.scrollBox(first - wheelLines(&b.wheelRem, int(int16(wParam>>16))))
		}
		return ok
	case win.WM_LBUTTONDOWN:
		n, vis, first, client, thumb, ok := b.boxBar()
		if !ok || x < client.Right-int32(b.edit.IntFrom96DPI(boxBarW)) {
			return false
		}
		if y < thumb.Top || y >= thumb.Bottom { // track click: centre the thumb there
			span := int(client.Bottom-client.Top) - int(thumb.Bottom-thumb.Top)
			if span > 0 {
				first += (int(y) - int(thumb.Top+thumb.Bottom)/2) * (n - vis) / span
				b.scrollBox(first)
			}
		}
		_, _, b.dragFirst, _, _, _ = b.boxBar()
		b.dragY, b.dragging = int(y), true
		win.SetCapture(b.edit.Handle())
		return true
	case win.WM_MOUSEMOVE:
		if !b.dragging {
			return false
		}
		if n, vis, _, client, thumb, ok := b.boxBar(); ok {
			if span := int(client.Bottom-client.Top) - int(thumb.Bottom-thumb.Top); span > 0 {
				b.scrollBox(b.dragFirst + (int(y)-b.dragY)*(n-vis)/span)
			}
		}
		return true
	case win.WM_LBUTTONUP, win.WM_CAPTURECHANGED:
		if !b.dragging {
			return false
		}
		b.dragging = false
		if msg == win.WM_LBUTTONUP {
			win.ReleaseCapture()
		}
		return true
	}
	return false
}

// wheelLines adds a wheel delta to *rem and returns the whole lines it now makes, three a notch
// (120); the rest stays in *rem for the next step.
func wheelLines(rem *int, delta int) int {
	*rem += delta * 3
	n := *rem / 120
	*rem -= n * 120
	return n
}

// setBoxMargin keeps the box's text out of the thumb's margin.
func (b *InputBar) setBoxMargin() {
	if m := b.edit.IntFrom96DPI(boxBarW); m != b.marginPx {
		b.marginPx = m
		b.edit.SendMessage(win.EM_SETMARGINS, ecRightMargin, uintptr(m)<<16)
	}
}
