package ui

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lxn/walk"
	"github.com/lxn/win"

	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
)

// Sizes in 1/96 inch.
const (
	attachRowH    = 60
	attachThumb   = 44
	attachRowsMax = 5 // rows the preview shows before it scrolls
)

var (
	pictureExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".bmp": true, ".gif": true}
	videoExts   = map[string]bool{".mp4": true, ".mov": true, ".mkv": true, ".avi": true, ".webm": true, ".3gp": true}
	audioExts   = map[string]bool{".mp3": true, ".m4a": true, ".aac": true, ".ogg": true, ".opus": true, ".wav": true}
)

// attachList paints the preview's rows: a picture's thumbnail or the file type's icon, the name, and
// its size and type or why it will not be sent.
type attachList struct {
	cw     *walk.CustomWidget
	k      *kit
	items  []attachItem
	scroll vscroll
	thumbs map[int]*walk.Bitmap // decoded on first paint; nil: none, the icon stays
}

// previewAttachments shows items in a modal dialog over owner and reports whether Send was chosen.
// Files that cannot be sent are listed with the reason and left out; with none sendable there is
// only Close. Enter sends, Esc cancels, Up and Down scroll a long list.
func previewAttachments(owner walk.Form, p theme.Palette, items []attachItem) (bool, error) {
	dlg, list, err := buildAttachPreview(owner, p, items)
	if err != nil {
		return false, err
	}
	defer dlg.Dispose()
	defer list.dispose()
	return dlg.Run() == walk.DlgCmdOK && len(sendablePaths(items)) > 0, nil
}

// buildAttachPreview makes the preview dialog, not yet shown, and its list.
func buildAttachPreview(owner walk.Form, p theme.Palette, items []attachItem) (dlg *walk.Dialog, list *attachList, err error) {
	if dlg, err = walk.NewDialogWithFixedSize(owner); err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			dlg.Dispose()
		}
	}()
	k := &kit{pal: p}
	ok := len(sendablePaths(items))
	h := 160 + min(len(items), attachRowsMax)*attachRowH
	l := walk.NewVBoxLayout()
	if err := errors.Join(dlg.SetTitle("Wisp"), setBox(dlg, l, walk.Margins{HNear: 20, VNear: 16, HFar: 20, VFar: 16}), l.SetSpacing(8),
		k.themed(dlg, false), dlg.SetMinMaxSize(walk.Size{Width: 460, Height: h}, walk.Size{Width: 460, Height: h})); err != nil {
		return nil, nil, err
	}
	title := attachTitle(ok, len(items)-ok)
	if n := len(items) - ok; n > 0 && ok > 0 {
		title += " (" + plural(n, "other", "others") + " can't be sent)"
	}
	if _, err := k.newLabel(dlg, title, 12, walk.FontBold, toneText, 28); err != nil {
		return nil, nil, err
	}
	list = &attachList{k: k, items: items, thumbs: map[int]*walk.Bitmap{}}
	if list.cw, err = newCanvas(dlg, list.paint); err != nil {
		return nil, nil, err
	}
	names := make([]string, len(items))
	for i, it := range items {
		names[i] = it.name
		if it.blocked != "" {
			names[i] += " (not sent: " + it.blocked + ")"
		}
	}
	row := func() int { return list.cw.IntFrom96DPI(attachRowH) }
	list.scroll.attach(list.cw, func() int { return len(items) * row() }, row)
	list.cw.MouseDown().Attach(func(x, y int, b walk.MouseButton) {
		if b == walk.LeftButton {
			list.scroll.press(x, y)
		}
	})
	if err := errors.Join(fillRest(dlg, list.cw), focusable(list.cw, walk.AccRoleList, strings.Join(names, ", "), func(key walk.Key) bool {
		switch key {
		case walk.KeyDown:
			list.scroll.scrollTo(list.scroll.off + row())
		case walk.KeyUp:
			list.scroll.scrollTo(list.scroll.off - row())
		default:
			return false
		}
		return true
	})); err != nil {
		return nil, nil, err
	}
	buttons, err := k.newRow(dlg, fieldH)
	if err != nil {
		return nil, nil, err
	}
	gap, err := walk.NewHSpacer(buttons)
	if err != nil {
		return nil, nil, err
	}
	var first walk.Window
	if ok == 0 {
		b, err := k.newButton(buttons, "Close", btnPlain, 100, dlg.Cancel)
		if err != nil {
			return nil, nil, err
		}
		first = b
	} else {
		if _, err := k.newButton(buttons, "Cancel", btnPlain, 100, dlg.Cancel); err != nil {
			return nil, nil, err
		}
		b, err := k.newButton(buttons, "Send", btnPrimary, 100, dlg.Accept)
		if err != nil {
			return nil, nil, err
		}
		first = b
	}
	if err := errors.Join(fillRest(buttons, gap), escCloses(dlg)); err != nil {
		return nil, nil, err
	}
	dlg.Starting().Attach(func() { focusOn(first) }) // Enter sends at once
	dlg.SendMessage(win.WM_CHANGEUISTATE, win.UIS_INITIALIZE, 0)
	logErr("attachment preview title bar", theme.ApplyTitleBar(uintptr(dlg.Handle()), isDark(p)))
	return dlg, list, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func (l *attachList) dispose() {
	for _, b := range l.thumbs {
		if b != nil {
			b.Dispose()
		}
	}
}

// thumb is item i's picture side x side pixels, nil when it is not a picture this app decodes.
// shortcut: decoded on the UI thread at the first paint; move it off the thread if pasting a
// dozen large photos ever makes the preview slow to open.
func (l *attachList) thumb(i, side, dpi int) *walk.Bitmap {
	if b, done := l.thumbs[i]; done {
		return b
	}
	l.thumbs[i] = nil
	it := l.items[i]
	if it.blocked != "" || !(pictureExts[strings.ToLower(filepath.Ext(it.path))]) {
		return nil
	}
	f, err := os.Open(it.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	img, err := coverImage(f, side, side)
	if err != nil {
		return nil // an unreadable picture still goes as a file: the icon stands in
	}
	if b, err := walk.NewBitmapFromImageForDPI(img, dpi); err == nil {
		l.thumbs[i] = b
	}
	return l.thumbs[i]
}

func (l *attachList) paint(c *walk.Canvas, ub walk.Rectangle) error {
	bg, err := brushFor(l.k.pal.Sidebar)
	if err != nil {
		return err
	}
	err = errors.Join(c.FillRectanglePixels(bg, ub), l.paintRows(c, l.cw.ClientBoundsPixels().Width, l.cw.DPI(), ub))
	if t := l.scroll.thumb(); t.Height > 0 {
		err = errors.Join(err, icons.Fill(c, t, theme.Color(l.k.pal.TextSecondary)))
	}
	return err
}

// paintRows paints the rows in sight of the update area ub, over a background already filled, for a
// list width pixels wide at dpi.
func (l *attachList) paintRows(c *walk.Canvas, width, dpi int, ub walk.Rectangle) error {
	p := l.k.pal
	s := scaler(dpi)
	var errs []error
	secondary := theme.Color(p.TextSecondary)
	for i, it := range l.items {
		y := i*s(attachRowH) - l.scroll.off
		if y+s(attachRowH) <= ub.Y || y >= ub.Y+ub.Height {
			continue
		}
		box := walk.Rectangle{X: s(2), Y: y + (s(attachRowH)-s(attachThumb))/2, Width: s(attachThumb), Height: s(attachThumb)}
		ext := strings.ToLower(filepath.Ext(it.path))
		if bmp := l.thumb(i, box.Width, dpi); bmp != nil {
			errs = append(errs, c.DrawImageStretchedPixels(bmp, box))
		} else {
			icon := icons.IconDocument
			switch {
			case videoExts[ext]:
				icon = icons.IconVideo
			case audioExts[ext]:
				icon = icons.IconAudio
			}
			errs = append(errs, icons.FillRounded(c, box, s(6), theme.Color(p.Header)),
				icons.Draw(c, icon, square(box.X+box.Width/2, box.Y+box.Height/2, s(26)), secondary))
		}
		x := box.X + box.Width + s(12)
		line := walk.Rectangle{X: x, Y: y + s(9), Width: width - x - s(12), Height: s(22)}
		nameColor, sub, subColor := theme.Color(p.Text), humanSize(uint64(it.size)), secondary
		if kind := strings.ToUpper(strings.TrimPrefix(ext, ".")); kind != "" && !it.temp {
			sub += " · " + kind
		}
		if it.blocked != "" {
			nameColor, sub, subColor = secondary, "Not sent: "+it.blocked, l.k.color(toneError)
		}
		errs = append(errs, drawText(c, dpi, it.name, bodyPt, 0, nameColor, line, leftText))
		line.Y += s(22)
		errs = append(errs, drawText(c, dpi, sub, namePt, 0, subColor, line, leftText))
	}
	return errors.Join(errs...)
}
