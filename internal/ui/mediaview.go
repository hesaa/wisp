package ui

import (
	"errors"
	"image"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lxn/walk"
	"github.com/lxn/win"

	"wa-go/internal/media"
	"wa-go/internal/store"
	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
)

const (
	mediaBarH = 56   // top bar, 1/96 inch
	maxZoom   = 4    // times the fitted size
	zoomStep  = 1.25 // per Ctrl +/- or wheel notch
)

// mediaSteps are the nearest photos, stickers or videos before and after msgs[i]; -1 when none.
func mediaSteps(msgs []store.Message, i int) (prev, next int) {
	visual := func(m store.Message) bool { return m.Type == "image" || m.Type == "sticker" || m.Type == "video" }
	prev, next = -1, -1
	for j := i - 1; j >= 0 && prev < 0; j-- {
		if visual(msgs[j]) {
			prev = j
		}
	}
	for j := i + 1; j < len(msgs) && next < 0; j++ {
		if visual(msgs[j]) {
			next = j
		}
	}
	return prev, next
}

// fitRect is where an img-sized picture shows in area at zoom (1: fitted, letterboxed, never above
// 1:1), moved by pan from the centre; pan is clamped so the picture never leaves a gap it could cover.
func fitRect(img walk.Size, area walk.Rectangle, zoom float64, pan walk.Point) walk.Rectangle {
	if img.Width <= 0 || img.Height <= 0 || area.Width <= 0 || area.Height <= 0 {
		return walk.Rectangle{X: area.X + area.Width/2, Y: area.Y + area.Height/2}
	}
	s := min(1, float64(area.Width)/float64(img.Width), float64(area.Height)/float64(img.Height)) * zoom
	w, h := int(math.Round(float64(img.Width)*s)), int(math.Round(float64(img.Height)*s))
	place := func(at, size, n, pan int) int {
		if n <= size {
			return at + (size-n)/2
		}
		return max(at+size-n, min(at, at+(size-n)/2+pan))
	}
	return walk.Rectangle{X: place(area.X, area.Width, w, pan.X), Y: place(area.Y, area.Height, h, pan.Y), Width: w, Height: h}
}

// zoomAround multiplies zoom by factor (kept within 1…4) and returns the pan that keeps the picture
// point under cursor where it is; shown is where the picture is now.
func zoomAround(zoom, factor float64, cursor walk.Point, shown walk.Rectangle, area walk.Rectangle) (float64, walk.Point) {
	nz := max(1, min(maxZoom, zoom*factor))
	if nz == 1 || shown.Width <= 0 || shown.Height <= 0 {
		return nz, walk.Point{}
	}
	at := func(c, x, n, a, size int) int {
		f := float64(c-x) / float64(n) // where the cursor is on the picture, 0…1
		nn := float64(n) * nz / zoom
		return int(math.Round(float64(c) - f*nn - (float64(a) + (float64(size)-nn)/2)))
	}
	return nz, walk.Point{X: at(cursor.X, shown.X, shown.Width, area.X, area.Width), Y: at(cursor.Y, shown.Y, shown.Height, area.Y, area.Height)}
}

// mediaWhen is a message's time as the media view's top bar shows it.
func mediaWhen(ts int64, now time.Time) string {
	t := time.Unix(ts, 0).In(now.Location())
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	switch {
	case !t.Before(today):
		return "Today at " + t.Format("15:04")
	case !t.Before(today.AddDate(0, 0, -1)):
		return "Yesterday at " + t.Format("15:04")
	}
	return t.Format("02/01/2006 at 15:04")
}

type mediaState int

const (
	mediaDownloading mediaState = iota
	mediaFailed                 // the download failed: the error and Retry
	mediaLoading                // decoding
	mediaShown
	mediaTooLarge
	mediaBroken // could not be decoded
	mediaVideo  // played with Open with… until the player comes
)

// MediaView shows a chat's photos and stickers in the chat column: a top bar (back, sender and time,
// zoom, Save as…, Open with…) over the picture, fitted, zoomed 1×–4× by the wheel around the
// pointer, Ctrl + / −, or a double-click, and dragged when zoomed. ← and → step through the chat's
// photos and videos, 0 fits, Esc closes. The picture is decoded only while shown. UI thread only.
type MediaView struct {
	*walk.Composite
	*kit
	canvas             *walk.CustomWidget
	who, when, zoomLbl *label
	save, with         *button

	list      []store.Message
	i         int
	sender    func(store.Message) string
	state     mediaState
	failure   string
	bmp       *walk.Bitmap
	size      walk.Size // the picture's pixels
	gen       int       // per picture shown: a decode finished for another is dropped
	zoom      float64
	pan       walk.Point
	dragging  bool
	dragAt    walk.Point
	lastClick time.Time

	onClose     func()
	onNeedMedia func(store.Message)
	onOpenWith  func(path string)
}

// NewMediaView creates the view in parent, hidden.
func NewMediaView(parent walk.Container, p theme.Palette) (*MediaView, error) {
	c, err := walk.NewComposite(parent)
	if err != nil {
		return nil, err
	}
	v := &MediaView{Composite: c, kit: &kit{pal: p}, zoom: 1}
	if err := v.build(); err != nil {
		c.Dispose()
		return nil, err
	}
	c.SetVisible(false)
	return v, nil
}

func (v *MediaView) build() error {
	if err := errors.Join(setBox(v.Composite, walk.NewVBoxLayout(), walk.Margins{}), v.themed(v.Composite, false)); err != nil {
		return err
	}
	bar, err := v.newRow(v.Composite, mediaBarH)
	if err != nil {
		return err
	}
	// walk stretches a row's items to its height: the margins keep them button-high
	if err := bar.Layout().SetMargins(walk.Margins{HNear: 8, HFar: 12, VNear: (mediaBarH - fieldH) / 2, VFar: (mediaBarH - fieldH) / 2}); err != nil {
		return err
	}
	var back *walk.CustomWidget
	back, err = v.control(bar, walk.Size{Width: fieldH, Height: fieldH}, walk.AccRolePushbutton, "Back",
		func(c *walk.Canvas, _ walk.Rectangle) error { return v.paintBack(c, back) }, nil, func(int, int) func() { return v.closed })
	if err != nil {
		return err
	}
	v.keys(back, func(k walk.Key) bool {
		if k == walk.KeySpace || k == walk.KeyReturn {
			v.closed()
			return true
		}
		return false
	})
	col, err := walk.NewComposite(bar)
	if err != nil {
		return err
	}
	if err := errors.Join(setBox(col, walk.NewVBoxLayout(), walk.Margins{}), v.themed(col, false), fillRest(bar, col)); err != nil {
		return err
	}
	if v.who, err = v.newLabel(col, "", 11, walk.FontBold, toneText, fieldH/2); err != nil {
		return err
	}
	if v.when, err = v.newLabel(col, "", 9, 0, toneSecondary, fieldH/2); err != nil {
		return err
	}
	if v.zoomLbl, err = v.newLabel(bar, "", 10, 0, toneSecondary, fieldH); err != nil {
		return err
	}
	v.zoomLbl.mid = true
	if err := v.zoomLbl.SetMinMaxSize(walk.Size{Width: 56, Height: fieldH}, walk.Size{Width: 56, Height: fieldH}); err != nil {
		return err
	}
	if v.save, err = v.newButton(bar, "Save as…", btnPlain, 100, v.saveAs); err != nil {
		return err
	}
	if v.with, err = v.newButton(bar, "Open with…", btnPlain, 110, v.openWith); err != nil {
		return err
	}
	v.keys(v.save.CustomWidget, nil)
	v.keys(v.with.CustomWidget, nil)
	if v.canvas, err = newCanvas(v.Composite, v.paint); err != nil {
		return err
	}
	cw := v.canvas
	v.canvases = append(v.canvases, cw)
	if err := errors.Join(fillRest(v.Composite, cw), focusable(cw, walk.AccRoleGraphic, "Photo", nil)); err != nil {
		return err
	}
	v.keys(cw, func(k walk.Key) bool {
		if k == walk.KeySpace || k == walk.KeyReturn {
			v.act()
			return true
		}
		return false
	})
	cw.MouseDown().Attach(v.mouseDown)
	cw.MouseMove().Attach(v.mouseMove)
	cw.MouseUp().Attach(func(_, _ int, b walk.MouseButton) {
		if b == walk.LeftButton {
			v.dragging = false
		}
	})
	cw.MouseWheel().Attach(v.wheel)
	cw.SizeChanged().Attach(func() { v.reframe(v.zoom, v.pan) })
	return nil
}

// keys sends cw's keys to own first (nil: none), then to the view's keys; Esc included.
func (v *MediaView) keys(cw *walk.CustomWidget, own func(walk.Key) bool) {
	h := keyHooks[cw.Handle()]
	inner := h.onKey
	h.onKey = func(k walk.Key) bool {
		return own != nil && own(k) || inner != nil && inner(k) || v.key(k)
	}
	h.esc = func() bool { return true }
}

// OnClose sets what the back arrow and Esc do; the handler hides the view and calls Close.
func (v *MediaView) OnClose(fn func()) { v.onClose = fn }

// OnNeedMedia sets the handler that downloads a message's media and answers with SetMediaPath or
// SetDownloadError.
func (v *MediaView) OnNeedMedia(fn func(store.Message)) { v.onNeedMedia = fn }

// OnOpenWith sets what Open with… does with the shown file (the default program).
func (v *MediaView) OnOpenWith(fn func(path string)) { v.onOpenWith = fn }

// Open shows list[i] (a chat's loaded messages, oldest first) and takes the keyboard; sender names a
// message's sender.
func (v *MediaView) Open(list []store.Message, i int, sender func(store.Message) string) {
	v.list, v.sender = slices.Clone(list), sender
	v.show(i)
	focusOn(v.canvas)
}

// Close drops the picture and the messages and gives the memory back.
func (v *MediaView) Close() {
	v.gen++
	v.drop()
	v.list, v.sender, v.dragging = nil, nil, false
	debug.FreeOSMemory()
}

func (v *MediaView) drop() {
	if v.bmp != nil {
		v.bmp.Dispose()
		v.bmp = nil
	}
	v.size, v.zoom, v.pan = walk.Size{}, 1, walk.Point{}
}

// SetMediaPath records that message id was downloaded to path; the shown one is then decoded.
func (v *MediaView) SetMediaPath(id, path string) {
	for j := range v.list {
		if v.list[j].ID == id {
			v.list[j].MediaPath = path
			if j == v.i && (v.state == mediaDownloading || v.state == mediaFailed) {
				v.show(j)
			}
		}
	}
}

// SetDownloadError reports that message id's download failed; shown, it says so with Retry.
func (v *MediaView) SetDownloadError(id string, err error) {
	if m, ok := v.current(); ok && m.ID == id && v.state == mediaDownloading {
		v.state, v.failure = mediaFailed, "Download failed: "+err.Error()
		v.update()
	}
}

// SetPalette recolours the view.
func (v *MediaView) SetPalette(p theme.Palette) error { return v.setPalette(p) }

func (v *MediaView) current() (store.Message, bool) {
	if v.i < 0 || v.i >= len(v.list) {
		return store.Message{}, false
	}
	return v.list[v.i], true
}

func (v *MediaView) closed() {
	if v.onClose != nil {
		v.onClose()
	}
}

// show brings list[i] up: asks for its download, or decodes it off the UI thread.
func (v *MediaView) show(i int) {
	v.gen++
	v.drop()
	v.i, v.dragging = i, false
	m, ok := v.current()
	switch {
	case !ok:
		return
	case needsDownload(m):
		v.state = mediaDownloading
		if v.onNeedMedia != nil {
			v.onNeedMedia(m)
		}
	case m.Type == "video":
		v.state = mediaVideo
	default:
		v.state = mediaLoading
		gen, path, dpi := v.gen, m.MediaPath, v.canvas.DPI()
		go func() {
			thumbSlot <- struct{}{} // one full-size image decoded at a time
			img, err := decodeFull(path)
			<-thumbSlot
			v.canvas.Synchronize(func() { v.decoded(gen, m.ID, img, err, dpi) })
		}()
	}
	v.update()
}

func (v *MediaView) decoded(gen int, id string, img *image.RGBA, err error, dpi int) {
	if gen != v.gen || v.canvas.IsDisposed() {
		return // stepped away or closed meanwhile; img goes with the next collection
	}
	var bmp *walk.Bitmap
	if err == nil {
		bmp, err = walk.NewBitmapFromImageForDPI(img, dpi)
	}
	switch {
	case errors.Is(err, media.ErrImageTooLarge):
		v.state = mediaTooLarge
	case err != nil:
		logErr("photo "+id, err)
		v.state = mediaBroken
	default:
		v.bmp, v.size, v.state = bmp, walk.Size{Width: img.Bounds().Dx(), Height: img.Bounds().Dy()}, mediaShown
	}
	v.update()
}

// update refreshes the top bar and the picture after a change of message or state.
func (v *MediaView) update() {
	m, ok := v.current()
	if !ok {
		return
	}
	v.who.set(v.sender(m), toneText)
	v.when.set(mediaWhen(m.Timestamp, time.Now()), toneSecondary)
	has := !needsDownload(m)
	v.save.SetVisible(has)
	v.with.SetVisible(has)
	kind := "Photo"
	switch m.Type {
	case "sticker":
		kind = "Sticker"
	case "video":
		kind = "Video"
	}
	announce(v.canvas, kind+" from "+v.sender(m)+", "+mediaWhen(m.Timestamp, time.Now()), v.stateText())
	v.reframe(v.zoom, v.pan)
}

// stateText is what the view says instead of a picture; "" while one shows.
func (v *MediaView) stateText() string {
	switch v.state {
	case mediaDownloading:
		return "Downloading…"
	case mediaFailed:
		return v.failure
	case mediaTooLarge:
		return "Too large to show here"
	case mediaBroken:
		return "Can't show this picture"
	}
	return ""
}

// reframe sets zoom and pan, the pan clamped to the picture, and repaints.
func (v *MediaView) reframe(zoom float64, pan walk.Point) {
	v.zoom = zoom
	area := v.canvas.ClientBoundsPixels()
	r := fitRect(v.size, area, zoom, pan)
	v.pan = walk.Point{X: r.X - (area.X + (area.Width-r.Width)/2), Y: r.Y - (area.Y + (area.Height-r.Height)/2)}
	pct := ""
	if v.state == mediaShown {
		pct = strconv.Itoa(int(math.Round(zoom*100))) + "%"
	}
	if pct != v.zoomLbl.text {
		v.zoomLbl.set(pct, toneSecondary)
	}
	logErr("media repaint", v.canvas.Invalidate())
}

// zoomBy zooms around cursor (canvas pixels).
func (v *MediaView) zoomBy(factor float64, cursor walk.Point) {
	if v.state != mediaShown {
		return
	}
	area := v.canvas.ClientBoundsPixels()
	v.reframe(zoomAround(v.zoom, factor, cursor, fitRect(v.size, area, v.zoom, v.pan), area))
}

func (v *MediaView) centre() walk.Point {
	b := v.canvas.ClientBoundsPixels()
	return walk.Point{X: b.Width / 2, Y: b.Height / 2}
}

// key handles the view's keys wherever its focus is.
func (v *MediaView) key(k walk.Key) bool {
	ctrl := walk.ControlDown()
	switch {
	case k == walk.KeyEscape:
		v.closed()
	case k == walk.KeyLeft || k == walk.KeyRight:
		prev, next := mediaSteps(v.list, v.i)
		if k == walk.KeyRight {
			prev = next
		}
		if prev >= 0 {
			v.show(prev)
			focusOn(v.canvas) // Save as… may have gone
		}
	case ctrl && (k == walk.KeyOEMPlus || k == walk.KeyAdd):
		v.zoomBy(zoomStep, v.centre())
	case ctrl && (k == walk.KeyOEMMinus || k == walk.KeySubtract):
		v.zoomBy(1/zoomStep, v.centre())
	case k == walk.Key0 || k == walk.KeyNumpad0:
		v.reframe(1, walk.Point{})
	default:
		return false
	}
	return true
}

// act is Enter on the picture: Retry after a failed download, play a video.
func (v *MediaView) act() {
	switch v.state {
	case mediaFailed:
		v.show(v.i)
	case mediaVideo:
		v.openWith()
	}
}

func (v *MediaView) retryRect() walk.Rectangle {
	b, s := v.canvas.ClientBoundsPixels(), scaler(v.canvas.DPI())
	return walk.Rectangle{X: (b.Width - s(110)) / 2, Y: b.Height/2 + s(16), Width: s(110), Height: s(fieldH)}
}

func (v *MediaView) mouseDown(x, y int, b walk.MouseButton) {
	if b != walk.LeftButton {
		return
	}
	focusOn(v.canvas)
	switch v.state {
	case mediaFailed:
		if in(v.retryRect(), x, y) {
			v.act()
		}
		return
	case mediaVideo:
		v.act()
		return
	case mediaShown:
	default:
		return
	}
	now := time.Now()
	ms, _, _ := getDoubleClickTime.Call()
	if now.Sub(v.lastClick) <= time.Duration(ms)*time.Millisecond {
		v.lastClick = time.Time{}
		if v.zoom > 1 {
			v.reframe(1, walk.Point{})
		} else {
			v.zoomBy(2, walk.Point{X: x, Y: y})
		}
		return
	}
	v.lastClick = now
	v.dragging, v.dragAt = v.zoom > 1, walk.Point{X: x, Y: y}
}

func (v *MediaView) mouseMove(x, y int, b walk.MouseButton) {
	if !v.dragging || b&walk.LeftButton == 0 {
		v.dragging = false
		return
	}
	pan := walk.Point{X: v.pan.X + x - v.dragAt.X, Y: v.pan.Y + y - v.dragAt.Y}
	v.dragAt = walk.Point{X: x, Y: y}
	v.reframe(v.zoom, pan)
}

// wheel zooms around the pointer; the wheel message carries screen coordinates.
func (v *MediaView) wheel(x, y int, b walk.MouseButton) {
	pt := win.POINT{X: int32(x), Y: int32(y)}
	win.ScreenToClient(v.canvas.Handle(), &pt)
	v.zoomBy(math.Pow(zoomStep, float64(walk.MouseWheelEventDelta(b))/120), walk.Point{X: int(pt.X), Y: int(pt.Y)})
}

func (v *MediaView) openWith() {
	if m, ok := v.current(); ok && !needsDownload(m) && v.onOpenWith != nil {
		v.onOpenWith(m.MediaPath)
	}
}

// saveAs copies the cached file to where the user picks.
func (v *MediaView) saveAs() {
	m, ok := v.current()
	if !ok || needsDownload(m) {
		return
	}
	src := m.MediaPath
	dlg := walk.FileDialog{Title: "Save as", Flags: win.OFN_OVERWRITEPROMPT, FilePath: "Wisp " + time.Unix(m.Timestamp, 0).Format("2006-01-02 15.04.05") + filepath.Ext(src)}
	ok, err := dlg.ShowSave(v.Form())
	if err != nil || !ok || v.list == nil || strings.EqualFold(filepath.Clean(dlg.FilePath), filepath.Clean(src)) {
		logErr("save as", err)
		return // cancelled, closed by the lock, or the cached file itself
	}
	if err := copyFile(src, dlg.FilePath); err != nil {
		logErr("save as", err)
		walk.MsgBox(v.Form(), "Wisp", "This file was not saved: "+err.Error(), walk.MsgBoxIconWarning|walk.MsgBoxOK)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		logErr("mark of the web", media.MarkFromInternet(dst)) // it came from the internet, as the cached file did
	}
	return err
}

func (v *MediaView) paintBack(c *walk.Canvas, cw *walk.CustomWidget) error {
	p, r := v.pal, cw.ClientBoundsPixels()
	s := scaler(cw.DPI())
	errs := []error{fillBand(c, r, p.Sidebar)}
	if focusCues(cw) {
		errs = append(errs, focusRing(c, r.Width/2, r.Height/2, r.Width, s(2), theme.Color(p.Text), theme.Color(p.Sidebar)))
	}
	return errors.Join(append(errs, icons.Draw(c, icons.IconBack, square(r.Width/2, r.Height/2, s(24)), theme.Color(p.Text)))...)
}

func (v *MediaView) paint(c *walk.Canvas, _ walk.Rectangle) error {
	cw, p := v.canvas, v.pal
	b, dpi := cw.ClientBoundsPixels(), cw.DPI()
	s := scaler(dpi)
	errs := []error{fillBand(c, b, p.ChatBg)}
	switch v.state {
	case mediaShown:
		errs = append(errs, c.DrawImageStretchedPixels(v.bmp, fitRect(v.size, b, v.zoom, v.pan)))
	case mediaVideo:
		errs = append(errs, icons.Draw(c, icons.IconPlay, square(b.Width/2, b.Height/2, s(64)), theme.Color(p.Text)))
	case mediaLoading:
	default:
		msg := walk.Rectangle{X: s(16), Y: b.Height/2 - s(20), Width: b.Width - s(32), Height: s(28)}
		errs = append(errs, drawText(c, dpi, v.stateText(), 11, 0, v.color(toneText), msg, centerText))
		if v.state == mediaFailed {
			r := v.retryRect()
			errs = append(errs, icons.Fill(c, r, theme.Color(p.Accent)),
				drawText(c, dpi, "Retry", 10, walk.FontBold, walk.RGB(255, 255, 255), r, centerText))
		}
	}
	if focusCues(cw) {
		errs = append(errs, focusRect(c, b, s(2), p.Accent))
	}
	return errors.Join(errs...)
}
