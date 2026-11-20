package notify

import (
	"errors"
	"image"
	"image/color"
	"strconv"
	"unicode/utf16"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"wa-go/internal/theme"
)

// Tray wraps the notification-area icon: menu, restore click, unread badge, and toasts.
type Tray struct {
	ni      *walk.NotifyIcon
	hwnd    win.HWND
	appName string
	base    walk.Image
	// Lazily built badge icons, index 1-9 = count, 10 = "9+". Never disposed before Dispose():
	// walk's icon cache keeps every Icon handed to SetIcon (and any per-DPI copy), so a fixed
	// set bounds that growth.
	badges [10 + 1]*walk.Icon
	count  int
	d      *Dispatcher
}

// NewTray creates a visible tray icon. onOpen restores the window (left click or menu "Open"),
// onExit quits. base is owned by the caller. Walk does not publish double-click, so a single
// left click restores. Call on the UI thread.
func NewTray(form walk.Form, base walk.Image, appName string, onOpen, onExit func()) (*Tray, error) {
	ni, err := walk.NewNotifyIcon(form)
	if err != nil {
		return nil, err
	}
	t := &Tray{ni: ni, hwnd: form.Handle(), appName: appName, base: base}
	t.d = newDispatcher(t)
	fail := func(err error) (*Tray, error) { ni.Dispose(); return nil, err }

	for _, item := range []struct {
		text string
		fn   func()
	}{{"Open", onOpen}, {"Exit", onExit}} {
		a := walk.NewAction()
		if err := a.SetText(item.text); err != nil {
			return fail(err)
		}
		a.Triggered().Attach(item.fn)
		if err := ni.ContextMenu().Actions().Add(a); err != nil {
			return fail(err)
		}
	}
	ni.MouseUp().Attach(func(_, _ int, b walk.MouseButton) {
		if b == walk.LeftButton {
			onOpen()
		}
	})
	if err := ni.SetIcon(base); err != nil {
		return fail(err)
	}
	if err := ni.SetToolTip(appName); err != nil {
		return fail(err)
	}
	if err := ni.SetVisible(true); err != nil {
		return fail(err)
	}
	return t, nil
}

// Notify shows a toast per the formatting rules.
func (t *Tray) Notify(in Input, s Settings) error { return t.d.Notify(in, s) }

// OnToastClick calls fn with the (account, chat) of the latest toast when it is clicked.
func (t *Tray) OnToastClick(fn func(accountID, chatJID string)) {
	t.ni.MessageClicked().Attach(func() {
		if a, c, ok := t.d.Last(); ok {
			fn(a, c)
		}
	})
}

// SetUnread updates icon badge and tooltip; does nothing if the count is unchanged.
func (t *Tray) SetUnread(n int) error {
	if n < 0 {
		n = 0
	}
	if n == t.count {
		return nil
	}
	tip := t.appName
	if n > 0 {
		tip += " - " + strconv.Itoa(n) + " unread"
	}
	img := t.base
	if n > 0 {
		i := min(n, 10)
		if t.badges[i] == nil {
			ic, err := badgedIcon(t.base, badgeLabel(n))
			if err != nil {
				return err
			}
			t.badges[i] = ic
		}
		img = t.badges[i]
	}
	if err := t.ni.SetIcon(img); err != nil {
		return err
	}
	t.count = n
	return t.ni.SetToolTip(tip)
}

// Dispose removes the tray icon and frees all generated icons.
func (t *Tray) Dispose() error {
	err := t.ni.Dispose()
	for i, ic := range t.badges {
		if ic != nil {
			ic.Dispose()
			t.badges[i] = nil
		}
	}
	return err
}

// show implements balloon. Mirrors walk's showMessage but can set NIIF_NOSOUND,
// which walk's public API cannot.
func (t *Tray) show(title, body string, silent bool) error {
	// Clear any previous notification first so Windows triggers repeated alerts
	// with identical text (e.g. in private mode where every notification is "WhatsApp: New message").
	reset := win.NOTIFYICONDATA{HWnd: t.hwnd, UFlags: win.NIF_INFO}
	reset.CbSize = uint32(unsafe.Sizeof(reset) - unsafe.Sizeof(win.HICON(0)))
	_ = win.Shell_NotifyIcon(win.NIM_MODIFY, &reset)

	nid := win.NOTIFYICONDATA{HWnd: t.hwnd, UFlags: win.NIF_INFO, DwInfoFlags: win.NIIF_INFO | win.NIIF_LARGE_ICON}
	nid.CbSize = uint32(unsafe.Sizeof(nid) - unsafe.Sizeof(win.HICON(0)))
	if silent {
		nid.DwInfoFlags |= win.NIIF_NOSOUND
	}
	// -1 leaves room for the NUL terminator.
	copy(nid.SzInfoTitle[:], utf16.Encode([]rune(truncateUTF16(title, len(nid.SzInfoTitle)-1))))
	copy(nid.SzInfo[:], utf16.Encode([]rune(truncateUTF16(body, len(nid.SzInfo)-1))))
	if !win.Shell_NotifyIcon(win.NIM_MODIFY, &nid) {
		return errors.New("notify: Shell_NotifyIcon(NIF_INFO) failed")
	}
	return nil
}

func badgeLabel(n int) string {
	switch {
	case n <= 0:
		return ""
	case n > 9:
		return "9+"
	}
	return strconv.Itoa(n)
}

const iconPx, badgePx = 32, 16

// badgedIcon draws base plus a white-ringed accent-green circle with label at the bottom right.
func badgedIcon(base walk.Image, label string) (*walk.Icon, error) {
	bmp, err := walk.NewBitmapFromImageWithSize(base, walk.Size{Width: iconPx, Height: iconPx})
	if err != nil {
		return nil, err
	}
	defer bmp.Dispose()
	badge, err := walk.NewBitmapFromImage(badgeImage(label))
	if err != nil {
		return nil, err
	}
	defer badge.Dispose()
	cv, err := walk.NewCanvasFromImage(bmp)
	if err != nil {
		return nil, err
	}
	err = cv.DrawBitmapWithOpacityPixels(badge, walk.Rectangle{X: iconPx - badgePx, Y: iconPx - badgePx, Width: badgePx, Height: badgePx}, 0xff)
	cv.Dispose()
	if err != nil {
		return nil, err
	}
	return walk.NewIconFromBitmap(bmp)
}

// 3x5 glyphs, one string per row, for "0"-"9" and "+".
var glyphs = map[rune][5]string{
	'0': {"111", "101", "101", "101", "111"}, '1': {"010", "110", "010", "010", "111"},
	'2': {"111", "001", "111", "100", "111"}, '3': {"111", "001", "111", "001", "111"},
	'4': {"101", "101", "111", "001", "001"}, '5': {"111", "100", "111", "001", "111"},
	'6': {"111", "100", "111", "101", "111"}, '7': {"111", "001", "010", "010", "010"},
	'8': {"111", "101", "111", "101", "111"}, '9': {"111", "101", "111", "001", "111"},
	'+': {"000", "010", "111", "010", "000"},
}

// badgeImage renders the circle and label without GDI (alpha-correct), glyphs scaled 2x.
func badgeImage(label string) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, badgePx, badgePx))
	accent := theme.Color("#00a884")
	green := color.RGBA{R: accent.R(), G: accent.G(), B: accent.B(), A: 0xff}
	r := float64(badgePx) / 2
	ring := r - 2 // 2 px here, 1 px once the tray shrinks the icon to 16 px: keeps the badge off the teal logo
	for y := 0; y < badgePx; y++ {
		for x := 0; x < badgePx; x++ {
			dx, dy := float64(x)+0.5-r, float64(y)+0.5-r
			switch d := dx*dx + dy*dy; {
			case d <= ring*ring:
				img.SetRGBA(x, y, green)
			case d <= r*r:
				img.SetRGBA(x, y, color.RGBA{255, 255, 255, 255})
			}
		}
	}
	w := len(label)*7 - 1 // 2*3 px glyph + 1 gap
	x0, y0 := (badgePx-w)/2, (badgePx-10)/2
	for i, ch := range label {
		for row, bits := range glyphs[ch] {
			for col, c := range bits {
				if c != '1' {
					continue
				}
				for dy := 0; dy < 2; dy++ {
					for dx := 0; dx < 2; dx++ {
						img.SetRGBA(x0+i*7+col*2+dx, y0+row*2+dy, color.RGBA{255, 255, 255, 255})
					}
				}
			}
		}
	}
	return img
}
