package ui

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/lxn/walk"
	"github.com/lxn/win"

	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
)

// Sizes in 1/96 inch.
const (
	attachStripH  = 76
	attachTile    = 52
	attachStep    = 60 // from one tile's left edge to the next
	attachEdge    = 84 // kept clear of tiles at both ends of the strip; the Send button is in the right one
	attachCaption = 1024
)

var (
	pictureExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".bmp": true, ".gif": true}
	videoExts   = map[string]bool{".mp4": true, ".mov": true, ".mkv": true, ".avi": true, ".webm": true, ".3gp": true}
	audioExts   = map[string]bool{".mp3": true, ".m4a": true, ".aac": true, ".ogg": true, ".opus": true, ".wav": true}
)

// attachPreview is the dialog shown before files are sent: the chosen file large (its picture, or a
// card saying what it is or why it cannot be sent), a caption for it, and a strip with a tile per
// file, a tile that adds more, and the Send button.
type attachPreview struct {
	dlg          *walk.Dialog
	k            *kit
	items        []attachItem
	sel          int
	stage, strip *walk.CustomWidget
	caption      *walk.LineEdit
	thumbs       map[string]*walk.Bitmap // by path, decoded on first paint; nil: none, the icon stays
	big          *walk.Bitmap            // the chosen picture, fitted to the stage
	bigFor       string
	bigArea      walk.Size
	bigSize      walk.Size
}

// previewAttachments shows items in a modal dialog over owner. It returns the files still listed
// when it closed, with their captions, and whether Send was chosen. Files that cannot be sent say
// why and are left out. Enter sends; Esc or the cross closes, after asking; in the strip Left and
// Right choose a file, Delete (or the cross on its tile) takes it out and Insert adds more.
func previewAttachments(owner walk.Form, p theme.Palette, items []attachItem) ([]attachItem, bool, error) {
	v, err := buildAttachPreview(owner, p, items)
	if err != nil {
		return items, false, err
	}
	defer v.dlg.Dispose()
	defer v.dispose()
	send := v.dlg.Run() == walk.DlgCmdOK && len(sendable(v.items)) > 0
	return v.items, send, nil
}

// buildAttachPreview makes the preview dialog, not yet shown.
func buildAttachPreview(owner walk.Form, p theme.Palette, items []attachItem) (v *attachPreview, err error) {
	dlg, err := walk.NewDialogWithFixedSize(owner)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			dlg.Dispose()
		}
	}()
	v = &attachPreview{dlg: dlg, k: &kit{pal: p}, items: items, thumbs: map[string]*walk.Bitmap{}}
	k := v.k
	l := walk.NewVBoxLayout()
	size := walk.Size{Width: 760, Height: 640}
	if err := errors.Join(dlg.SetTitle("Send files"), setBox(dlg, l, walk.Margins{}), l.SetSpacing(0),
		k.themed(dlg, false), dlg.SetMinMaxSize(size, size)); err != nil {
		return nil, err
	}

	if v.stage, err = newCanvas(dlg, func(c *walk.Canvas, _ walk.Rectangle) error {
		return v.paintStage(c, v.stage.ClientBoundsPixels(), v.stage.DPI())
	}); err != nil {
		return nil, err
	}
	k.canvases = append(k.canvases, v.stage)
	v.stage.MouseDown().Attach(func(x, y int, b walk.MouseButton) {
		box := closeBox(v.stage.DPI())
		if b == walk.LeftButton && x < box.X+2*box.Width && y < box.Y+2*box.Height {
			dlg.Cancel()
		}
	})
	if err := fillRest(dlg, v.stage); err != nil {
		return nil, err
	}

	row, err := k.newRow(dlg, fieldH)
	if err != nil {
		return nil, err
	}
	if _, err := walk.NewHSpacerFixed(row, 72); err != nil {
		return nil, err
	}
	pill, err := walk.NewComposite(row)
	if err != nil {
		return nil, err
	}
	pl := walk.NewHBoxLayout()
	if err := errors.Join(setBox(pill, pl, walk.Margins{HNear: 12, HFar: 12}), pl.SetAlignment(walk.AlignHNearVCenter),
		k.themed(pill, true), fillRest(row, pill)); err != nil {
		return nil, err
	}
	if _, err := walk.NewVSpacerFixed(pill, fieldH); err != nil {
		return nil, err
	}
	if v.caption, err = walk.NewLineEdit(pill); err != nil {
		return nil, err
	}
	v.caption.SetFont(font(10, 0))
	v.caption.SetTextColor(theme.Color(p.Text))
	v.caption.SetMaxLength(attachCaption)
	k.edits = append(k.edits, v.caption)
	if err := errors.Join(noBorder(v.caption), cueBanner(v.caption, "Type a message"),
		v.caption.Accessibility().SetName("Caption"), k.themed(v.caption, true)); err != nil {
		return nil, err
	}
	v.caption.TextChanged().Attach(func() {
		if v.sel < len(v.items) {
			v.items[v.sel].caption = v.caption.Text()
		}
	})
	v.caption.KeyDown().Attach(func(key walk.Key) {
		if key == walk.KeyReturn {
			v.send()
		}
	})
	if _, err := walk.NewHSpacerFixed(row, 72); err != nil {
		return nil, err
	}
	if _, err := walk.NewVSpacerFixed(dlg, 14); err != nil {
		return nil, err
	}

	if v.strip, err = k.control(dlg, walk.Size{Height: attachStripH}, walk.AccRoleList, "", func(c *walk.Canvas, _ walk.Rectangle) error {
		return v.paintStrip(c, v.strip.ClientBoundsPixels(), v.strip.DPI(), focusCues(v.strip))
	}, func(key walk.Key) bool {
		switch key {
		case walk.KeyLeft:
			v.choose(v.sel - 1)
		case walk.KeyRight:
			v.choose(v.sel + 1)
		case walk.KeyDelete:
			v.remove(v.sel)
		case walk.KeyInsert:
			v.add()
		case walk.KeyReturn:
			v.send()
		default:
			return false
		}
		return true
	}, func(x, y int) func() {
		tiles, send := stripTiles(v.strip.ClientBoundsPixels().Width, len(v.items), v.sel, scaler(v.strip.DPI()))
		at := walk.Point{X: x, Y: y}
		if inRect(send, at) {
			return v.send
		}
		if v.sel < len(v.items) && inRect(removeBox(tiles[v.sel], scaler(v.strip.DPI())), at) {
			return func() { v.remove(v.sel) }
		}
		for i, t := range tiles {
			if inRect(t, at) && tileShown(t, v.strip.ClientBoundsPixels().Width, scaler(v.strip.DPI())) {
				if i == len(v.items) {
					return v.add
				}
				return func() { v.choose(i) }
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	v.name()

	if err := escCloses(dlg); err != nil {
		return nil, err
	}
	// Esc, the cross and the title bar's close button all end here: files ready to go are not
	// dropped without asking.
	dlg.Closing().Attach(func(canceled *bool, _ walk.CloseReason) {
		if dlg.Result() == walk.DlgCmdOK || len(sendable(v.items)) == 0 {
			return
		}
		yes, err := Confirm(dlg, v.k.pal, "Discard selection?", "The files you chose will not be sent.", "Discard")
		logErr("discard selection", err)
		*canceled = err == nil && !yes
	})
	dlg.Starting().Attach(func() { focusOn(v.caption) }) // type a caption, Enter sends
	dlg.SendMessage(win.WM_CHANGEUISTATE, win.UIS_INITIALIZE, 0)
	logErr("attachment preview title bar", theme.ApplyTitleBar(uintptr(dlg.Handle()), isDark(p)))
	return v, nil
}

func inRect(r walk.Rectangle, p walk.Point) bool {
	return p.X >= r.X && p.X < r.X+r.Width && p.Y >= r.Y && p.Y < r.Y+r.Height
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func (v *attachPreview) dispose() {
	for _, b := range v.thumbs {
		if b != nil {
			b.Dispose()
		}
	}
	if v.big != nil {
		v.big.Dispose()
	}
}

// name tells assistive technology what the strip holds and how to work it.
func (v *attachPreview) name() {
	names := make([]string, len(v.items))
	for i, it := range v.items {
		names[i] = it.name
		if it.blocked != "" {
			names[i] += " (not sent: " + it.blocked + ")"
		}
	}
	text := plural(len(sendable(v.items)), "file", "files") + " to send: " + strings.Join(names, ", ") +
		". Left and Right choose, Delete removes, Insert adds, Enter sends."
	logErr("attachment strip name", v.strip.Accessibility().SetName(text))
}

func (v *attachPreview) repaint() {
	logErr("attachment preview repaint", errors.Join(v.stage.Invalidate(), v.strip.Invalidate()))
}

// choose shows file i large, with its caption.
func (v *attachPreview) choose(i int) {
	if i < 0 || i >= len(v.items) {
		return
	}
	v.sel = i
	logErr("attachment caption", v.caption.SetText(v.items[i].caption))
	v.repaint()
}

// remove takes file i out; with none left the dialog closes.
func (v *attachPreview) remove(i int) {
	if i < 0 || i >= len(v.items) {
		return
	}
	it := v.items[i]
	if it.temp {
		logErr("remove pasted picture", os.Remove(it.path))
	}
	if b := v.thumbs[it.path]; b != nil {
		b.Dispose()
	}
	delete(v.thumbs, it.path)
	v.items = slices.Delete(v.items, i, i+1)
	if len(v.items) == 0 {
		v.dlg.Cancel()
		return
	}
	v.name()
	v.choose(min(v.sel, len(v.items)-1))
}

// add asks for more files and shows the first of them.
func (v *attachPreview) add() {
	fd := walk.FileDialog{Title: "Attach files"}
	ok, err := fd.ShowOpenMultiple(v.dlg)
	if err != nil || !ok || len(fd.FilePaths) == 0 {
		logErr("attach", err)
		return
	}
	first := len(v.items)
	for _, p := range fd.FilePaths {
		v.items = append(v.items, newAttachItem(p, false))
	}
	v.name()
	v.choose(first)
}

func (v *attachPreview) send() {
	if len(sendable(v.items)) > 0 {
		v.dlg.Accept()
	}
}

// thumb is a file's picture side x side pixels, nil when it is not a picture this app decodes.
// shortcut: decoded on the UI thread at the first paint; move it off the thread if pasting a
// dozen large photos ever makes the preview slow to open.
func (v *attachPreview) thumb(it attachItem, side, dpi int) *walk.Bitmap {
	if b, done := v.thumbs[it.path]; done {
		return b
	}
	v.thumbs[it.path] = nil
	if it.blocked != "" || !pictureExts[strings.ToLower(filepath.Ext(it.path))] {
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
		v.thumbs[it.path] = b
	}
	return v.thumbs[it.path]
}

// large is the chosen file's picture scaled down to fit area, and its size in pixels; nil when it
// has none to show. Only the chosen file's is kept.
func (v *attachPreview) large(it attachItem, area walk.Size, dpi int) (*walk.Bitmap, walk.Size) {
	if v.bigFor == it.path && v.bigArea == area {
		return v.big, v.bigSize
	}
	if v.big != nil {
		v.big.Dispose()
	}
	v.big, v.bigFor, v.bigArea = nil, it.path, area
	if it.blocked != "" || !pictureExts[strings.ToLower(filepath.Ext(it.path))] {
		return nil, walk.Size{}
	}
	f, err := os.Open(it.path)
	if err != nil {
		return nil, walk.Size{}
	}
	defer f.Close()
	img, err := fitImage(f, area.Width, area.Height, maxFullPixels)
	if err != nil {
		return nil, walk.Size{}
	}
	if b, err := walk.NewBitmapFromImageForDPI(img, dpi); err == nil {
		v.big, v.bigSize = b, walk.Size{Width: img.Bounds().Dx(), Height: img.Bounds().Dy()}
	}
	return v.big, v.bigSize
}

// fillBack fills the whole of r, square corners included (icons.FillRounded rounds them by at least
// a pixel, so it fills past r; a walk brush would do, but cannot be used in tests).
func fillBack(c *walk.Canvas, r walk.Rectangle, color walk.Color) error {
	return icons.FillRounded(c, inset(r, -2), 1, color)
}

// closeBox is where the stage draws its cross.
func closeBox(dpi int) walk.Rectangle {
	s := scaler(dpi)
	return square(s(28), s(28), s(16))
}

func kindIcon(ext string) icons.Icon {
	switch {
	case videoExts[ext]:
		return icons.IconVideo
	case audioExts[ext]:
		return icons.IconAudio
	}
	return icons.IconDocument
}

// paintStage paints the area r above the caption: the cross, the chosen file's name and the file.
func (v *attachPreview) paintStage(c *walk.Canvas, r walk.Rectangle, dpi int) error {
	p := v.k.pal
	s := scaler(dpi)
	text, secondary := theme.Color(p.Text), theme.Color(p.TextSecondary)
	errs := []error{fillBack(c, r, theme.Color(p.Sidebar)), icons.Draw(c, icons.IconClose, closeBox(dpi), text)}
	if v.sel >= len(v.items) {
		return errors.Join(errs...)
	}
	it := v.items[v.sel]
	errs = append(errs, drawText(c, dpi, it.name, bodyPt, walk.FontBold, text,
		walk.Rectangle{X: s(64), Y: s(12), Width: r.Width - s(128), Height: s(32)}, centerText))
	area := walk.Rectangle{X: s(40), Y: s(60), Width: r.Width - s(80), Height: r.Height - s(80)}
	if bmp, size := v.large(it, area.Size(), dpi); bmp != nil {
		at := walk.Rectangle{X: area.X + (area.Width-size.Width)/2, Y: area.Y + (area.Height-size.Height)/2, Width: size.Width, Height: size.Height}
		return errors.Join(append(errs, c.DrawImageStretchedPixels(bmp, at))...)
	}
	card := walk.Rectangle{Width: min(s(420), area.Width), Height: min(s(280), area.Height)}
	card.X, card.Y = area.X+(area.Width-card.Width)/2, area.Y+(area.Height-card.Height)/2
	ext := strings.ToLower(filepath.Ext(it.path))
	title, sub, subColor := "No preview available", humanSize(uint64(it.size)), secondary
	if kind := strings.ToUpper(strings.TrimPrefix(ext, ".")); kind != "" && !it.temp {
		sub += " · " + kind
	}
	if it.blocked != "" {
		title, sub, subColor = "Can't be sent", it.blocked, v.k.color(toneError)
	}
	line := walk.Rectangle{X: card.X + s(16), Y: card.Y + card.Height - s(98), Width: card.Width - s(32), Height: s(32)}
	errs = append(errs, icons.FillRounded(c, card, s(12), theme.Color(p.Header)),
		icons.Draw(c, kindIcon(ext), square(card.X+card.Width/2, card.Y+(card.Height-s(98))/2+s(8), s(80)), text),
		drawText(c, dpi, title, 15, 0, text, line, centerText))
	line.Y, line.Height = line.Y+s(34), s(22)
	return errors.Join(append(errs, drawText(c, dpi, sub, namePt, 0, subColor, line, centerText))...)
}

// stripTiles places the strip's tiles in a strip width pixels wide: one per file and a last one that
// adds more, centred, and the Send button. With more than fit, they shift to keep tile sel in sight.
func stripTiles(width, n, sel int, s func(int) int) (tiles []walk.Rectangle, send walk.Rectangle) {
	side, step, edge := s(attachTile), s(attachStep), s(attachEdge)
	total, avail := n*step+side, width-2*edge
	x := edge + (avail-total)/2
	if total > avail {
		x = edge - min(max(sel*step+side/2-avail/2, 0), total-avail)
	}
	tiles = make([]walk.Rectangle, n+1)
	for i := range tiles {
		tiles[i] = walk.Rectangle{X: x + i*step, Y: (s(attachStripH) - side) / 2, Width: side, Height: side}
	}
	return tiles, square(width-s(46), s(attachStripH)/2, s(56))
}

// removeBox is the button on the chosen tile t's top right corner that takes its file out.
func removeBox(t walk.Rectangle, s func(int) int) walk.Rectangle {
	return square(t.X+t.Width-s(3), t.Y+s(3), s(18))
}

// tileShown reports whether tile t is in the part of the strip tiles are painted in.
func tileShown(t walk.Rectangle, width int, s func(int) int) bool {
	return t.X >= s(attachEdge)-s(attachTile)/2 && t.X+t.Width <= width-s(attachEdge)+s(attachTile)/2
}

// paintStrip paints the strip r: a tile per file, the chosen one ringed, the tile that adds more,
// and the Send button with the number of files it will send.
func (v *attachPreview) paintStrip(c *walk.Canvas, r walk.Rectangle, dpi int, cues bool) error {
	p := v.k.pal
	s := scaler(dpi)
	bg, text, secondary, accent := theme.Color(p.Sidebar), theme.Color(p.Text), theme.Color(p.TextSecondary), theme.Color(p.Accent)
	errs := []error{fillBack(c, r, bg), icons.Fill(c, walk.Rectangle{Width: r.Width, Height: 1}, theme.Color(p.Header))}
	tiles, send := stripTiles(r.Width, len(v.items), v.sel, s)
	for i, t := range tiles {
		if !tileShown(t, r.Width, s) {
			continue
		}
		if i == len(v.items) { // the tile that adds more
			errs = append(errs, icons.FillRounded(c, t, s(6), secondary), icons.FillRounded(c, inset(t, 1), s(5), bg),
				icons.Draw(c, icons.IconAdd, inset(t, s(16)), text))
			continue
		}
		it := v.items[i]
		in := t
		if i == v.sel {
			ring := accent
			if cues {
				ring = text
			}
			in = inset(t, s(3))
			errs = append(errs, icons.FillRounded(c, t, s(7), ring), icons.FillRounded(c, inset(t, s(2)), s(5), bg))
		}
		if bmp := v.thumb(it, s(attachTile), dpi); bmp != nil {
			errs = append(errs, c.DrawImageStretchedPixels(bmp, in))
			continue
		}
		fg := secondary
		if it.blocked != "" {
			fg = v.k.color(toneError)
		}
		errs = append(errs, icons.FillRounded(c, in, s(5), theme.Color(p.Header)),
			icons.Draw(c, kindIcon(strings.ToLower(filepath.Ext(it.path))), inset(t, s(13)), fg))
	}
	if v.sel < len(v.items) && tileShown(tiles[v.sel], r.Width, s) { // over the tiles beside it
		x := removeBox(tiles[v.sel], s)
		errs = append(errs, icons.Fill(c, x, text), icons.Draw(c, icons.IconClose, inset(x, s(4)), bg))
	}
	n := len(sendable(v.items))
	fill := accent
	if n == 0 {
		fill = theme.Color(p.Header)
	}
	badge := square(send.X+send.Width-s(8), send.Y+s(8), s(22))
	return errors.Join(append(errs, icons.FillRounded(c, send, send.Width/2, fill),
		icons.Draw(c, icons.IconSend, inset(send, s(16)), walk.RGB(255, 255, 255)),
		icons.FillRounded(c, badge, badge.Width/2, text),
		drawText(c, dpi, strconv.Itoa(n), namePt, walk.FontBold, bg, badge, centerText))...)
}
