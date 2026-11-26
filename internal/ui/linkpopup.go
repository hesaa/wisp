package ui

import (
	"cmp"
	"errors"
	"slices"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"

	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
)

const (
	popupRowH    = 34
	popupPadX    = 12
	popupPadY    = 4
	popupIconSz  = 16
	popupIconGap = 10
	popupMinW    = 210
	popupMaxW    = 340
)

type popupAction int

const (
	popupOpenLink popupAction = iota
	popupCopyLink
	popupChecking
	popupChatWith
	popupCopyPhone
)

type popupItem struct {
	action   popupAction
	icon     icons.Icon
	label    string
	disabled bool
	url      string
	raw      string
	jid      string
	display  string
}

// linkPopupModel holds the action items and keyboard state of the link/phone popup.
// Pure Go so it is tested without a Win32 dialog.
type linkPopupModel struct {
	raw    string
	items  []popupItem
	cur    int
	moved  bool
	closed bool
}

func newLinkPopupModel(url string) *linkPopupModel {
	return &linkPopupModel{
		raw: url,
		items: []popupItem{
			{action: popupOpenLink, icon: icons.IconExternal, label: "Open link", url: url},
			{action: popupCopyLink, icon: icons.IconCopy, label: "Copy link", url: url},
		},
		cur: 0,
	}
}

func newPhonePopupModel(raw string) *linkPopupModel {
	return &linkPopupModel{
		raw: raw,
		items: []popupItem{
			{action: popupChecking, icon: icons.IconChat, label: "Checking WhatsApp...", disabled: true, raw: raw},
			{action: popupCopyPhone, icon: icons.IconCopy, label: "Copy phone number", raw: raw},
		},
		cur: 1,
	}
}

// applyPhoneCheck updates the phone popup when WhatsApp registration lookup finishes.
// Returns false when the popup was already closed (no-op).
func (m *linkPopupModel) applyPhoneCheck(registered bool, display, jid string) bool {
	if m.closed {
		return false
	}
	if registered && jid != "" {
		disp := cmp.Or(display, m.raw)
		chatItem := popupItem{
			action:  popupChatWith,
			icon:    icons.IconChat,
			label:   "Chat with " + disp,
			raw:     m.raw,
			jid:     jid,
			display: disp,
		}
		if len(m.items) > 0 && (m.items[0].action == popupChecking || m.items[0].action == popupChatWith) {
			m.items[0] = chatItem
		} else {
			m.items = slices.Insert(m.items, 0, chatItem)
		}
		if !m.moved || m.cur < 0 || m.cur >= len(m.items) || m.items[m.cur].disabled {
			m.cur = 0
		}
		return true
	}
	m.items = slices.DeleteFunc(m.items, func(it popupItem) bool { return it.action == popupChecking })
	m.cur = m.firstEnabled()
	return true
}

func (m *linkPopupModel) firstEnabled() int {
	for i, it := range m.items {
		if !it.disabled {
			return i
		}
	}
	return -1
}

// move moves the keyboard selection among enabled items by delta (-1 or +1).
func (m *linkPopupModel) move(delta int) {
	if len(m.items) == 0 || delta == 0 {
		return
	}
	step := 1
	if delta < 0 {
		step = -1
	}
	for i := m.cur + step; i >= 0 && i < len(m.items); i += step {
		if !m.items[i].disabled {
			m.cur = i
			m.moved = true
			return
		}
	}
}

func (m *linkPopupModel) moveEnd(last bool) {
	if !last {
		for i := 0; i < len(m.items); i++ {
			if !m.items[i].disabled {
				m.cur = i
				m.moved = true
				return
			}
		}
		return
	}
	for i := len(m.items) - 1; i >= 0; i-- {
		if !m.items[i].disabled {
			m.cur = i
			m.moved = true
			return
		}
	}
}

func (m *linkPopupModel) activate() (popupItem, bool) {
	if m.cur < 0 || m.cur >= len(m.items) || m.items[m.cur].disabled {
		return popupItem{}, false
	}
	return m.items[m.cur], true
}

func (m *linkPopupModel) close() { m.closed = true }

// linkPopup is the borderless themed dialog showing link or phone actions.
type linkPopup struct {
	dlg   *walk.Dialog
	k     *kit
	list  *walk.CustomWidget
	model *linkPopupModel
	hover int
	esc   bool
	picked *popupItem
}

func newLinkPopup(owner walk.Form, p theme.Palette, anchor walk.Point, m *linkPopupModel) (*linkPopup, error) {
	dlg, err := walk.NewDialogWithFixedSize(owner)
	if err != nil {
		return nil, err
	}
	hwnd := dlg.Handle()
	style := uint32(win.GetWindowLong(hwnd, win.GWL_STYLE))
	if win.SetWindowLong(hwnd, win.GWL_STYLE, int32(style&^(win.WS_CAPTION|win.WS_SYSMENU)|win.WS_POPUP)) == 0 ||
		!win.SetWindowPos(hwnd, 0, 0, 0, 0, 0, win.SWP_NOMOVE|win.SWP_NOSIZE|win.SWP_NOZORDER|win.SWP_FRAMECHANGED) {
		dlg.Dispose()
		return nil, errors.New("ui: making the link popup borderless failed")
	}

	lp := &linkPopup{
		dlg:   dlg,
		k:     &kit{pal: p},
		model: m,
		hover: -1,
	}

	cancel, err := walk.NewPushButton(dlg)
	if err == nil {
		cancel.SetVisible(false)
		cancel.Clicked().Attach(func() {
			lp.esc = true
			lp.model.close()
			dlg.Cancel()
		})
		err = errors.Join(
			dlg.SetCancelButton(cancel),
			setBg(dlg, p.TextSecondary),
			setBox(dlg, walk.NewVBoxLayout(), walk.Margins{HNear: 1, VNear: 1, HFar: 1, VFar: 1}),
		)
	}
	if err != nil {
		dlg.Dispose()
		return nil, err
	}

	w, h := lp.logicalSize()
	if err := dlg.SetMinMaxSize(walk.Size{Width: w, Height: h}, walk.Size{Width: w, Height: h}); err != nil {
		dlg.Dispose()
		return nil, err
	}

	cw, err := lp.k.control(dlg, walk.Size{}, walk.AccRoleMenuPopup, "Actions", lp.paint, lp.key, lp.hit)
	if err != nil {
		dlg.Dispose()
		return nil, err
	}
	lp.list = cw
	if err := fillRest(dlg, cw); err != nil {
		dlg.Dispose()
		return nil, err
	}
	cw.MouseMove().Attach(func(x, y int, _ walk.MouseButton) {
		h := lp.rowAt(y)
		if h >= 0 && lp.model.items[h].disabled {
			h = -1
		}
		if h != lp.hover {
			lp.hover = h
			logErr("link popup repaint", cw.Invalidate())
		}
	})

	dlg.Starting().Attach(func() {
		lp.reposition(owner.Handle(), anchor)
		focusOn(cw)
		win.EnableWindow(owner.Handle(), true)
	})
	dlg.Deactivating().Attach(func() {
		owner.Synchronize(func() {
			if !dlg.IsDisposed() && win.GetActiveWindow() == owner.Handle() {
				lp.model.close()
				dlg.Cancel()
			}
		})
	})
	dlg.Closing().Attach(func(_ *bool, _ walk.CloseReason) {
		lp.model.close()
	})
	dlg.SendMessage(win.WM_CHANGEUISTATE, win.UIS_INITIALIZE, 0)
	return lp, nil
}

func (lp *linkPopup) logicalSize() (int, int) {
	ms := &gdiMeasurer{dpi: 96, cdpi: 96}
	maxText := 0
	for _, it := range lp.model.items {
		tw, _ := ms.measure(it.label, bodyPt, 0, 1<<20)
		maxText = max(maxText, tw)
	}
	_ = ms.close()
	w := max(popupMinW, min(popupMaxW, 2*popupPadX+popupIconSz+popupIconGap+maxText+8))
	h := max(1, len(lp.model.items))*popupRowH + 2*popupPadY + 2
	return w, h
}

func (lp *linkPopup) reposition(owner win.HWND, anchor walk.Point) {
	var mi win.MONITORINFO
	mi.CbSize = uint32(unsafe.Sizeof(mi))
	hMon := win.MonitorFromWindow(owner, win.MONITOR_DEFAULTTONEAREST)
	if !win.GetMonitorInfo(hMon, &mi) {
		return
	}
	wa := mi.RcWork
	work := walk.Rectangle{X: int(wa.Left), Y: int(wa.Top), Width: int(wa.Right - wa.Left), Height: int(wa.Bottom - wa.Top)}
	sz := lp.dlg.SizePixels()
	logErr("link popup position", lp.dlg.SetBoundsPixels(placePopupAtPoint(anchor, sz, work)))
}

// placePopupAtPoint positions a popup of size at screen pixel point pt, flipping left/up to stay inside work.
func placePopupAtPoint(pt walk.Point, size walk.Size, work walk.Rectangle) walk.Rectangle {
	r := walk.Rectangle{X: pt.X, Y: pt.Y, Width: size.Width, Height: size.Height}
	if r.X+r.Width > work.X+work.Width {
		r.X = pt.X - r.Width
	}
	if r.Y+r.Height > work.Y+work.Height {
		r.Y = pt.Y - r.Height
	}
	r.X = max(work.X, min(r.X, work.X+work.Width-r.Width))
	r.Y = max(work.Y, min(r.Y, work.Y+work.Height-r.Height))
	return r
}

func (lp *linkPopup) updatePhoneCheck(registered bool, display, jid string) {
	if lp.model.closed || lp.dlg.IsDisposed() {
		return
	}
	if !lp.model.applyPhoneCheck(registered, display, jid) {
		return
	}
	w, h := lp.logicalSize()
	_ = lp.dlg.SetMinMaxSize(walk.Size{Width: w, Height: h}, walk.Size{Width: w, Height: h})
	b := lp.dlg.BoundsPixels()
	var mi win.MONITORINFO
	mi.CbSize = uint32(unsafe.Sizeof(mi))
	if win.GetMonitorInfo(win.MonitorFromWindow(lp.dlg.Handle(), win.MONITOR_DEFAULTTONEAREST), &mi) {
		wa := mi.RcWork
		work := walk.Rectangle{X: int(wa.Left), Y: int(wa.Top), Width: int(wa.Right - wa.Left), Height: int(wa.Bottom - wa.Top)}
		sz := walk.Size{Width: lp.dlg.IntFrom96DPI(w), Height: lp.dlg.IntFrom96DPI(h)}
		_ = lp.dlg.SetBoundsPixels(placePopupAtPoint(walk.Point{X: b.X, Y: b.Y}, sz, work))
	}
	if lp.list != nil && !lp.list.IsDisposed() {
		logErr("link popup repaint", lp.list.Invalidate())
		if lp.model.cur >= 0 && lp.model.cur < len(lp.model.items) {
			announce(lp.list, "Actions", lp.model.items[lp.model.cur].label)
		}
	}
}

func (lp *linkPopup) rowAt(y int) int {
	s := scaler(lp.list.DPI())
	i := (y - s(popupPadY)) / s(popupRowH)
	if i < 0 || i >= len(lp.model.items) {
		return -1
	}
	return i
}

func (lp *linkPopup) key(k walk.Key) bool {
	switch k {
	case walk.KeyUp:
		lp.hover = -1
		lp.model.move(-1)
	case walk.KeyDown:
		lp.hover = -1
		lp.model.move(1)
	case walk.KeyHome:
		lp.hover = -1
		lp.model.moveEnd(false)
	case walk.KeyEnd:
		lp.hover = -1
		lp.model.moveEnd(true)
	case walk.KeyReturn, walk.KeySpace:
		if it, ok := lp.model.activate(); ok {
			lp.picked = &it
			lp.model.close()
			lp.dlg.Accept()
		}
		return true
	default:
		return false
	}
	logErr("link popup repaint", lp.list.Invalidate())
	if lp.model.cur >= 0 && lp.model.cur < len(lp.model.items) {
		announce(lp.list, "Actions", lp.model.items[lp.model.cur].label)
	}
	return true
}

func (lp *linkPopup) hit(_, y int) func() {
	i := lp.rowAt(y)
	if i < 0 || lp.model.items[i].disabled {
		return nil
	}
	return func() {
		lp.model.cur = i
		it := lp.model.items[i]
		lp.picked = &it
		lp.model.close()
		lp.dlg.Accept()
	}
}

func (lp *linkPopup) paint(c *walk.Canvas, _ walk.Rectangle) error {
	p := lp.k.pal
	b, dpi := lp.list.ClientBoundsPixels(), lp.list.DPI()
	s := scaler(dpi)
	bg, err := brushFor(p.Sidebar)
	if err != nil {
		return err
	}
	if err := c.FillRectanglePixels(bg, b); err != nil {
		return err
	}
	focused := focusCues(lp.list)
	var errs []error
	for i, it := range lp.model.items {
		row := walk.Rectangle{X: s(4), Y: s(popupPadY) + i*s(popupRowH), Width: b.Width - s(8), Height: s(popupRowH)}
		selected := !it.disabled && (i == lp.hover || (lp.hover < 0 && i == lp.model.cur) || (focused && i == lp.model.cur))
		if selected {
			errs = append(errs, icons.FillRounded(c, row, s(6), theme.Color(p.Header)))
		}
		fg := theme.Color(p.Text)
		if it.disabled {
			fg = theme.Color(p.TextSecondary)
		}
		ic := walk.Rectangle{X: row.X + s(popupPadX-4), Y: row.Y + (row.Height-s(popupIconSz))/2, Width: s(popupIconSz), Height: s(popupIconSz)}
		tx := ic.X + ic.Width + s(popupIconGap)
		tr := walk.Rectangle{X: tx, Y: row.Y, Width: max(1, row.X+row.Width-s(popupPadX-4)-tx), Height: row.Height}
		errs = append(errs,
			icons.Draw(c, it.icon, ic, fg),
			drawText(c, dpi, it.label, bodyPt, 0, fg, tr, leftText),
		)
	}
	return errors.Join(errs...)
}

func (lp *linkPopup) dismiss() {
	if lp == nil {
		return
	}
	lp.model.close()
	if !lp.dlg.IsDisposed() {
		lp.dlg.Cancel()
	}
}
