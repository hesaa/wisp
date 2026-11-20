package ui

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"golang.org/x/image/draw"

	"wa-go/internal/store"
	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
)

const (
	slideDuration = 5 * time.Second        // each status shows this long
	statusTick    = 100 * time.Millisecond // how often the progress bar repaints while a slide runs
	// maxFullPixels bounds a downloaded status image before it is decoded (~100 MB transient); it
	// matches the media cache's limit.
	maxFullPixels = 25_000_000

	statusW, statusH = 440, 700 // window size, 1/96 inch
	statusTextPt     = 20       // a text status's body
)

// statusSeq is the order of a sender's statuses and the clock of the one on show: which slide, how
// much of its time has passed, and whether that is running. It holds no window and no timer; the
// clock is injected.
type statusSeq struct {
	n, idx int
	banked time.Duration // time shown before the last pause
	since  time.Time     // when the slide last started running; meaningless while paused
	halted bool
	now    func() time.Time
	seen   []bool
}

func newStatusSeq(n int, now func() time.Time) *statusSeq {
	s := &statusSeq{n: n, now: now, seen: make([]bool, n)}
	s.restart()
	return s
}

func (s *statusSeq) index() int   { return s.idx }
func (s *statusSeq) paused() bool { return s.halted }

// restart starts the current slide's time over; a paused sequence stays paused.
func (s *statusSeq) restart() { s.banked, s.since = 0, s.now() }

func (s *statusSeq) elapsed() time.Duration {
	e := s.banked
	if !s.halted {
		e += s.now().Sub(s.since)
	}
	return min(e, slideDuration)
}

func (s *statusSeq) remaining() time.Duration { return slideDuration - s.elapsed() }

func (s *statusSeq) pause() {
	if !s.halted {
		s.banked, s.halted = s.elapsed(), true
	}
}

func (s *statusSeq) resume() {
	if s.halted {
		s.since, s.halted = s.now(), false
	}
}

// progress is how full slide i's bar is: 1 for slides already shown, 0 for those to come.
func (s *statusSeq) progress(i int) float64 {
	switch {
	case i < s.idx:
		return 1
	case i > s.idx:
		return 0
	}
	return float64(s.elapsed()) / float64(slideDuration)
}

// next moves to the following slide; at the last one it stays and reports end.
func (s *statusSeq) next() (end bool) {
	if s.idx+1 >= s.n {
		return true
	}
	s.idx++
	s.restart()
	return false
}

// prev moves to the previous slide; on the first it starts that slide over.
func (s *statusSeq) prev() {
	s.idx = max(0, s.idx-1)
	s.restart()
}

// markSeen reports whether the current slide is shown for the first time.
func (s *statusSeq) markSeen() bool {
	first := !s.seen[s.idx]
	s.seen[s.idx] = true
	return first
}

type slideKind int

const (
	slideText  slideKind = iota // text, and anything that is not an image or video
	slideImage                  // photo with an optional caption
	slideVideo                  // thumbnail and an Open action
)

func slideKindOf(typ string) slideKind {
	switch typ {
	case "image":
		return slideImage
	case "video":
		return slideVideo
	}
	return slideText
}

// StatusCallbacks connect the viewer to the app; each may be nil. They run on the UI thread.
type StatusCallbacks struct {
	// OnNeedMedia asks for a status's file to be downloaded: once when an image or video status
	// without one is first shown, and again when the user opens such a video. The answer comes back
	// through the setter Opened gave.
	OnNeedMedia func(store.Message)
	// OnOpenMedia opens a downloaded video in the system's player.
	OnOpenMedia func(path string)
	// OnViewed is called once per status, the first time it is shown (the read receipt's moment).
	OnViewed func(store.Message)
	// Opened is called once the viewer exists and before it runs, with the function that hands it a
	// downloaded file's path while it is open (call it on the UI thread). It does nothing once the viewer is closed.
	Opened func(setMediaPath func(id, path string))
}

// statusCtl is the viewer's behaviour without its window: it walks a sequence of statuses, arms the
// one-shot timer that advances them, and decides when media is requested, a status counts as viewed
// and a decoded image is still wanted. The window supplies redraw, done, load and shown. UI thread only.
type statusCtl struct {
	seq   *statusSeq
	msgs  []store.Message
	cb    StatusCallbacks
	after func(d time.Duration, f func()) (stop func() bool) // one-shot timer; f runs on the UI thread

	redraw func()
	done   func()                            // closes the viewer after the last status
	load   func(m *store.Message, gen int64) // makes the slide's pictures; gen names the slide for imageReady
	shown  func()                            // a slide came up

	stop   func() bool // cancels the pending timer, nil for none
	timer  int         // per timer: an expiry queued before it was stopped is ignored
	gen    atomic.Int64
	asked  map[string]bool
	closed bool

	pic, thumb *walk.Bitmap // the shown slide's pictures: the sized photo, the embedded preview
	aspect     walk.Size    // the picture's shape as far as known before the photo is decoded
}

func newStatusCtl(msgs []store.Message, cb StatusCallbacks, now func() time.Time, after func(time.Duration, func()) func() bool) *statusCtl {
	return &statusCtl{seq: newStatusSeq(len(msgs), now), msgs: slices.Clone(msgs), cb: cb, after: after, asked: map[string]bool{},
		redraw: func() {}, done: func() {}, load: func(*store.Message, int64) {}, shown: func() {}}
}

func (c *statusCtl) index() int          { return c.seq.index() }
func (c *statusCtl) slideGen() int64     { return c.gen.Load() }
func (c *statusCtl) cur() *store.Message { return &c.msgs[c.seq.index()] }

func (c *statusCtl) start() { c.show() }

// show brings the current slide up: drops the last one's pictures and in-flight decodes, reports
// the first view, asks for missing media, and arms the timer.
func (c *statusCtl) show() {
	gen := c.gen.Add(1)
	c.dispose()
	m := c.cur()
	if c.seq.markSeen() && c.cb.OnViewed != nil {
		c.cb.OnViewed(*m)
	}
	if slideKindOf(m.Type) != slideText {
		if m.MediaPath == "" && !c.asked[m.ID] {
			c.asked[m.ID] = true
			if c.cb.OnNeedMedia != nil {
				c.cb.OnNeedMedia(*m)
			}
		}
		c.load(m, gen)
	}
	c.shown()
	c.redraw()
	c.arm()
}

// arm sets the one timer for the running slide: its next repaint, or its end when that is nearer.
func (c *statusCtl) arm() {
	c.cancel()
	if c.closed || c.seq.paused() {
		return
	}
	c.timer++
	id := c.timer
	c.stop = c.after(min(c.seq.remaining(), statusTick), func() {
		if id != c.timer || c.closed {
			return
		}
		if c.seq.remaining() <= 0 {
			c.next()
			return
		}
		c.redraw()
		c.arm()
	})
}

func (c *statusCtl) cancel() {
	if c.stop != nil {
		c.stop()
		c.stop = nil
	}
	c.timer++
}

func (c *statusCtl) next() {
	if c.closed {
		return
	}
	if c.seq.next() {
		c.close()
		c.done()
		return
	}
	c.show()
}

func (c *statusCtl) prev() {
	if !c.closed {
		c.seq.prev()
		c.show()
	}
}

func (c *statusCtl) togglePause() {
	if c.closed {
		return
	}
	if c.seq.paused() {
		c.seq.resume()
	} else {
		c.seq.pause()
	}
	c.arm()
	c.redraw()
}

// activate is the slide's action: opening a video, or asking for its file.
func (c *statusCtl) activate() {
	if c.closed {
		return
	}
	m := c.cur()
	switch {
	case slideKindOf(m.Type) != slideVideo:
	case m.MediaPath != "":
		if c.cb.OnOpenMedia != nil {
			c.cb.OnOpenMedia(m.MediaPath)
		}
	case c.cb.OnNeedMedia != nil:
		c.cb.OnNeedMedia(*m)
	}
}

// SetMediaPath records that status id was downloaded to path; the shown image is then decoded.
func (c *statusCtl) SetMediaPath(id, path string) {
	if c.closed {
		return
	}
	for i := range c.msgs {
		if c.msgs[i].ID != id {
			continue
		}
		c.msgs[i].MediaPath = path
		if i == c.seq.index() {
			if slideKindOf(c.msgs[i].Type) == slideImage {
				c.load(&c.msgs[i], c.slideGen())
			}
			c.redraw()
		}
	}
}

// reload makes the shown slide's pictures again, for a changed window size.
func (c *statusCtl) reload() {
	if m := c.cur(); !c.closed && slideKindOf(m.Type) != slideText {
		c.load(m, c.gen.Add(1))
	}
}

// imageReady takes the decoded photo of slide gen; it reports false, and frees bmp, when the viewer
// has moved on or closed since the decode began.
func (c *statusCtl) imageReady(gen int64, bmp *walk.Bitmap) bool {
	if c.closed || gen != c.slideGen() {
		disposeBitmap(bmp)
		return false
	}
	if c.pic != bmp {
		disposeBitmap(c.pic)
	}
	if bmp != nil { // the sized photo replaces the preview
		disposeBitmap(c.thumb)
		c.thumb = nil
	}
	c.pic = bmp
	c.redraw()
	return true
}

func (c *statusCtl) dispose() {
	disposeBitmap(c.pic)
	disposeBitmap(c.thumb)
	c.pic, c.thumb, c.aspect = nil, nil, walk.Size{}
}

func disposeBitmap(b *walk.Bitmap) {
	if b != nil {
		b.Dispose()
	}
}

// close stops the timer and frees the pictures; the controller then ignores everything.
func (c *statusCtl) close() {
	c.closed = true
	c.cancel()
	c.gen.Add(1)
	c.dispose()
}

// fitShape is the largest rectangle of a w x h picture's shape that fits area, centred in it; an
// unknown size fills the area.
func fitShape(w, h int, area walk.Rectangle) walk.Rectangle {
	if w <= 0 || h <= 0 || area.Width <= 0 || area.Height <= 0 {
		return area
	}
	nw, nh := area.Width, h*area.Width/w
	if w*area.Height <= h*area.Width {
		nw, nh = w*area.Height/h, area.Height
	}
	return walk.Rectangle{X: area.X + (area.Width-nw)/2, Y: area.Y + (area.Height-nh)/2, Width: nw, Height: nh}
}

// fitImage decodes a JPEG or PNG of at most limit pixels and scales it down to fit w x h, keeping its
// shape; a smaller image keeps its size.
func fitImage(r io.ReadSeeker, w, h, limit int) (*image.RGBA, error) {
	src, err := decodeCapped(r, limit)
	if err != nil {
		return nil, err
	}
	sb := src.Bounds()
	dw, dh := sb.Dx(), sb.Dy()
	if dw > w || dh > h {
		fit := fitShape(dw, dh, walk.Rectangle{Width: w, Height: h})
		dw, dh = max(1, fit.Width), max(1, fit.Height)
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, sb, draw.Src, nil)
	return dst, nil
}

// slidePreview decodes the status's embedded thumbnail, with the size of the full picture when its
// metadata states one; nil when there is no usable thumbnail. The metadata comes off the network:
// anything unreadable or oversized just means no preview.
func slidePreview(m *store.Message) (pv image.Image, w, h int) {
	info, err := parseMedia(m)
	if err != nil {
		logErr("status "+m.ID+" metadata", err)
		return nil, 0, 0
	}
	if len(info.preview) == 0 {
		return nil, 0, 0
	}
	im, err := decodeCapped(bytes.NewReader(info.preview), maxPreviewPixels)
	if err != nil {
		logErr("status "+m.ID+" preview", err)
		return nil, 0, 0
	}
	if w, h = info.w, info.h; w <= 0 || h <= 0 {
		w, h = im.Bounds().Dx(), im.Bounds().Dy()
	}
	return im, w, h
}

// storyTime is a status's age as the header shows it.
func storyTime(ts int64, now time.Time) string {
	s := relTime(ts, now)
	if strings.Contains(s, ":") {
		return "Today, " + s
	}
	return s
}

type statusHit int

const (
	hitNone statusHit = iota
	hitClose
	hitPause
	hitAction
	hitPrev
	hitNext
)

// statusGeom is the viewer's layout in pixels: a header (progress segments, sender, buttons), the
// slide, and below it the caption when there is one.
type statusGeom struct {
	top, body, caption       walk.Rectangle
	segs                     []walk.Rectangle
	avatar, close, pause     walk.Rectangle
	prevBtn, nextBtn, action walk.Rectangle
	margin                   int
}

func statusGeometry(w, h int, s func(int) int, hasCaption, video bool, n int) statusGeom {
	g := statusGeom{margin: s(12)}
	topH := s(68)
	g.top = walk.Rectangle{Width: w, Height: topH}
	capH := 0
	if hasCaption {
		capH = s(96)
	}
	g.caption = walk.Rectangle{Y: h - capH, Width: w, Height: capH}
	g.body = walk.Rectangle{Y: topH, Width: w, Height: max(0, h-topH-capH)}
	gap := s(3)
	sw := max(1, (w-2*g.margin-(max(n, 1)-1)*gap)/max(n, 1))
	for i := range n {
		g.segs = append(g.segs, walk.Rectangle{X: g.margin + i*(sw+gap), Y: s(10), Width: sw, Height: s(3)})
	}
	g.avatar = walk.Rectangle{X: g.margin, Y: s(22), Width: s(36), Height: s(36)}
	btn := s(32)
	g.close = walk.Rectangle{X: w - g.margin - btn, Y: s(24), Width: btn, Height: btn}
	g.pause = walk.Rectangle{X: g.close.X - s(4) - btn, Y: s(24), Width: btn, Height: btn}
	cy := g.body.Y + g.body.Height/2
	g.prevBtn = walk.Rectangle{X: s(8), Y: cy - s(18), Width: s(36), Height: s(36)}
	g.nextBtn = walk.Rectangle{X: w - s(8) - s(36), Y: cy - s(18), Width: s(36), Height: s(36)}
	if video {
		g.action = walk.Rectangle{X: (w - s(190)) / 2, Y: cy - s(22), Width: s(190), Height: s(44)}
	}
	return g
}

func in(r walk.Rectangle, x, y int) bool {
	return x >= r.X && y >= r.Y && x < r.X+r.Width && y < r.Y+r.Height
}

// hit is what a click at (x, y) means: a button, or the left or right half of the slide.
func (g statusGeom) hit(x, y int) statusHit {
	switch {
	case in(g.close, x, y):
		return hitClose
	case in(g.pause, x, y):
		return hitPause
	case in(g.action, x, y):
		return hitAction
	case !in(g.body, x, y):
		return hitNone
	case x < g.body.Width/2:
		return hitPrev
	}
	return hitNext
}

// mixColor is pct percent of a over b.
func mixColor(a, b walk.Color, pct int) walk.Color {
	m := func(x, y byte) byte { return byte((int(x)*pct + int(y)*(100-pct)) / 100) }
	return walk.RGB(m(a.R(), b.R()), m(a.G(), b.G()), m(a.B(), b.B()))
}

// statusDialog is the open viewer window around a statusCtl.
type statusDialog struct {
	*walk.Dialog
	ctl     *statusCtl
	canvas  *walk.CustomWidget
	pal     theme.Palette
	jid     string
	name    string // the sender's display name
	textFit struct{ idx, w, dpi, h int }
}

// RunStatusView shows the modal status viewer over owner: one sender's statuses, oldest first, as a
// slideshow. Each slide runs five seconds with a progress bar per status in the header; a click on
// the right or left half of the slide, or the Right and Left keys, go to the next or previous
// status, Space or the header's button pause and resume, and Esc or the close button end it, as does
// the end of the last status. Photos show their embedded preview until the file is downloaded (see
// StatusCallbacks); text statuses show on the accent colour; videos show their preview and an Open
// action. name is the sender's name ("" shows their number). UI thread only.
func RunStatusView(owner walk.Form, p theme.Palette, senderJID, name string, msgs []store.Message, cb StatusCallbacks) error {
	if len(msgs) == 0 {
		return nil
	}
	opener := win.GetFocus()
	dlg, err := walk.NewDialogWithFixedSize(owner)
	if err != nil {
		return err
	}
	defer dlg.Dispose()
	d := &statusDialog{Dialog: dlg, pal: p, jid: senderJID, name: displayName(store.Chat{JID: senderJID, Name: name})}
	d.ctl = newStatusCtl(msgs, cb, time.Now, func(dur time.Duration, f func()) func() bool {
		return time.AfterFunc(dur, func() { dlg.Synchronize(f) }).Stop
	})
	defer d.ctl.close()
	if err := d.build(); err != nil {
		return err
	}
	if cb.Opened != nil {
		cb.Opened(d.ctl.SetMediaPath)
	}
	dlg.Run()
	if opener != 0 { // the owner's focus is where Esc leaves it
		win.SetFocus(opener)
	}
	return nil
}

func (d *statusDialog) build() error {
	dlg, p := d.Dialog, d.pal
	if err := errors.Join(setBg(dlg, p.ChatBg), dlg.SetTitle("Status from "+d.name), setBox(dlg, walk.NewVBoxLayout(), walk.Margins{}),
		dlg.SetMinMaxSize(walk.Size{Width: statusW, Height: statusH}, walk.Size{Width: statusW, Height: statusH})); err != nil {
		return err
	}
	cw, err := newCanvas(dlg, d.paint)
	if err != nil {
		return err
	}
	d.canvas = cw
	n := len(d.ctl.msgs)
	if err := errors.Join(fillRest(dlg, cw), focusable(cw, walk.AccRoleClient, "Status", d.key)); err != nil {
		return err
	}
	// walk routes Esc (IDCANCEL) only to a cancel button; this one stays hidden
	cancel, err := walk.NewPushButton(dlg)
	if err != nil {
		return err
	}
	cancel.SetVisible(false)
	cancel.Clicked().Attach(dlg.Cancel)
	if err := dlg.SetCancelButton(cancel); err != nil {
		return err
	}
	c := d.ctl
	c.redraw = func() {
		if !cw.IsDisposed() {
			logErr("status repaint", cw.Invalidate())
		}
	}
	c.done = func() { dlg.Accept() }
	c.load = d.makePictures
	c.shown = func() {
		announce(cw, fmt.Sprintf("Status %d of %d from %s", c.index()+1, n, d.name), "")
	}
	cw.MouseDown().Attach(d.mouseDown)
	tipOnHover(cw, func(x, y int) string {
		switch d.geom().hit(x, y) {
		case hitClose:
			return "Close"
		case hitPause:
			if c.seq.paused() {
				return "Resume"
			}
			return "Pause"
		}
		return ""
	})
	cw.SizeChanged().Attach(func() {
		if cw.Width() > 0 {
			c.reload()
		}
	})
	dlg.Starting().Attach(func() {
		c.start()
		cw.Synchronize(func() { focusOn(cw) }) // once the window is shown
	})
	// focus cues follow the user's setting and last input (hidden after a click until a key is used)
	dlg.SendMessage(win.WM_CHANGEUISTATE, win.UIS_INITIALIZE, 0)
	// cosmetic: Windows builds without dark title bars keep a light one
	logErr("status title bar", titleBar(dlg, isDark(p)))
	return nil
}

func (d *statusDialog) geom() statusGeom {
	b, m := d.canvas.ClientBoundsPixels(), d.ctl.cur()
	return statusGeometry(b.Width, b.Height, scaler(d.canvas.DPI()), d.caption(m) != "", slideKindOf(m.Type) == slideVideo, len(d.ctl.msgs))
}

// caption is the text under a photo or video; a text status has its body as the slide itself.
func (d *statusDialog) caption(m *store.Message) string {
	if slideKindOf(m.Type) == slideText {
		return ""
	}
	return strings.TrimSpace(m.Body)
}

func (d *statusDialog) key(k walk.Key) bool {
	switch k {
	case walk.KeyRight:
		d.ctl.next()
	case walk.KeyLeft:
		d.ctl.prev()
	case walk.KeySpace:
		d.ctl.togglePause()
	case walk.KeyReturn:
		d.ctl.activate()
	default:
		return false
	}
	return true
}

// mouseDown resolves what was clicked before it takes the focus.
func (d *statusDialog) mouseDown(x, y int, b walk.MouseButton) {
	if b != walk.LeftButton {
		return
	}
	hit := d.geom().hit(x, y)
	focusOn(d.canvas)
	switch hit {
	case hitClose:
		d.Cancel()
	case hitPause:
		d.ctl.togglePause()
	case hitAction:
		d.ctl.activate()
	case hitPrev:
		d.ctl.prev()
	case hitNext:
		d.ctl.next()
	}
}

// makePictures gives the slide its embedded preview now and, when its photo is on disk, decodes
// that off the UI thread, scaled to the slide area.
func (d *statusDialog) makePictures(m *store.Message, gen int64) {
	c, cw := d.ctl, d.canvas
	area := d.geom().body
	disposeBitmap(c.thumb)
	c.thumb = nil
	if pv, w, h := slidePreview(m); pv != nil {
		// the embedded preview is tiny: smoothed up to the slide, not left to blocky stretching
		if fit := fitShape(w, h, area); fit.Width > 0 && fit.Height > 0 && area.Width > 0 {
			dst := image.NewRGBA(image.Rect(0, 0, fit.Width, fit.Height))
			draw.BiLinear.Scale(dst, dst.Bounds(), pv, pv.Bounds(), draw.Src, nil)
			pv, w, h = dst, fit.Width, fit.Height
		}
		bmp, err := walk.NewBitmapFromImageForDPI(pv, cw.DPI())
		logErr("status preview", err)
		c.thumb, c.aspect = bmp, walk.Size{Width: w, Height: h}
	}
	if slideKindOf(m.Type) != slideImage || m.MediaPath == "" || area.Width <= 0 || area.Height <= 0 {
		return
	}
	id, path, dpi := m.ID, m.MediaPath, cw.DPI()
	go func() {
		thumbSlot <- struct{}{} // one full-size image decoded at a time
		var img *image.RGBA
		var err error
		if gen == c.slideGen() { // the slide may have changed while this waited
			var f *os.File
			if f, err = os.Open(path); err == nil {
				img, err = fitImage(f, area.Width, area.Height, maxFullPixels)
				f.Close()
			}
		}
		<-thumbSlot
		cw.Synchronize(func() {
			if cw.IsDisposed() || img == nil {
				if err != nil {
					logErr("status "+id+" photo", err)
				}
				return
			}
			bmp, err := walk.NewBitmapFromImageForDPI(img, dpi)
			logErr("status "+id+" photo", err)
			if bmp != nil {
				c.imageReady(gen, bmp)
			}
		})
	}()
}

func (d *statusDialog) paint(c *walk.Canvas, ub walk.Rectangle) error {
	cw, ctl, p := d.canvas, d.ctl, d.pal
	dpi, s := cw.DPI(), scaler(cw.DPI())
	b := cw.ClientBoundsPixels()
	m := ctl.cur()
	kind := slideKindOf(m.Type)
	g := statusGeometry(b.Width, b.Height, s, d.caption(m) != "", kind == slideVideo, len(ctl.msgs))
	text, secondary, header := theme.Color(p.Text), theme.Color(p.TextSecondary), theme.Color(p.Header)
	bg, err := brushFor(p.ChatBg)
	if err != nil {
		return err
	}
	errs := []error{c.FillRectanglePixels(bg, b)}

	switch kind {
	case slideText:
		errs = append(errs, d.paintText(c, g, m, dpi, s))
	default:
		errs = append(errs, d.paintPicture(c, g, m, kind, s, dpi))
	}
	if g.caption.Height > 0 {
		cr := walk.Rectangle{X: g.caption.X + s(16), Y: g.caption.Y + s(10), Width: g.caption.Width - s(32), Height: g.caption.Height - s(20)}
		errs = append(errs, fillBand(c, g.caption, p.Header),
			drawText(c, dpi, d.caption(m), 11, 0, text, cr, walk.TextLeft|walk.TextWordbreak|walk.TextEndEllipsis|walk.TextNoPrefix))
	}

	// header: progress, sender, buttons
	errs = append(errs, fillBand(c, g.top, p.Header))
	empty := mixColor(secondary, header, 40)
	for i, seg := range g.segs {
		errs = append(errs, icons.Fill(c, seg, empty))
		if w := int(float64(seg.Width) * ctl.seq.progress(i)); w >= seg.Height {
			seg.Width = w
			errs = append(errs, icons.Fill(c, seg, text))
		}
	}
	tx := g.avatar.X + g.avatar.Width + s(12)
	tw := g.pause.X - tx - s(8)
	errs = append(errs,
		drawAvatar(c, dpi, g.avatar, d.name, d.jid, 12),
		drawText(c, dpi, d.name, 11, walk.FontBold, text, walk.Rectangle{X: tx, Y: g.avatar.Y - s(1), Width: tw, Height: s(20)}, leftText),
		drawText(c, dpi, storyTime(m.Timestamp, time.Now()), 9, 0, secondary, walk.Rectangle{X: tx, Y: g.avatar.Y + s(19), Width: tw, Height: s(18)}, leftText))
	pauseIcon := icons.IconPause
	if ctl.seq.paused() {
		pauseIcon = icons.IconPlay
	}
	errs = append(errs, icons.Draw(c, pauseIcon, inset(g.pause, s(6)), text), icons.Draw(c, icons.IconClose, inset(g.close, s(6)), text))

	if ctl.index() > 0 {
		errs = append(errs, d.paintChevron(c, g.prevBtn, icons.IconChevronLeft, s))
	}
	if ctl.index() < len(ctl.msgs)-1 {
		errs = append(errs, d.paintChevron(c, g.nextBtn, icons.IconChevronRight, s))
	}
	if focusCues(cw) {
		errs = append(errs, focusRect(c, b, s(2), p.Accent))
	}
	return errors.Join(errs...)
}

func fillBand(c *walk.Canvas, r walk.Rectangle, hex string) error {
	b, err := brushFor(hex)
	if err != nil {
		return err
	}
	return c.FillRectanglePixels(b, r)
}

func (d *statusDialog) paintChevron(c *walk.Canvas, r walk.Rectangle, icon icons.Icon, s func(int) int) error {
	return errors.Join(icons.Fill(c, r, theme.Color(d.pal.Header)), icons.Draw(c, icon, inset(r, s(6)), theme.Color(d.pal.Text)))
}

// paintText draws a text status centred on the accent colour.
func (d *statusDialog) paintText(c *walk.Canvas, g statusGeom, m *store.Message, dpi int, s func(int) int) error {
	bg, err := brushFor(d.pal.Accent)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(m.Body)
	if text == "" {
		text = "Status"
	}
	r := inset(g.body, s(56)) // clear of the arrows
	if r.Width <= 0 || r.Height <= 0 {
		return c.FillRectanglePixels(bg, g.body)
	}
	if t := &d.textFit; t.idx != d.ctl.index() || t.w != r.Width || t.dpi != c.DPI() || t.h == 0 { // measuring costs: once per slide and width
		gm := &gdiMeasurer{dpi: dpi, cdpi: c.DPI()}
		_, h := gm.measure(text, statusTextPt, walk.FontBold, r.Width)
		logErr("status text", gm.close())
		t.idx, t.w, t.dpi, t.h = d.ctl.index(), r.Width, c.DPI(), max(h, 1)
	}
	h := min(d.textFit.h, r.Height)
	r.Y, r.Height = r.Y+(r.Height-h)/2, h
	return errors.Join(c.FillRectanglePixels(bg, g.body),
		drawText(c, dpi, text, statusTextPt, walk.FontBold, walk.RGB(255, 255, 255), r,
			walk.TextCenter|walk.TextWordbreak|walk.TextEndEllipsis|walk.TextNoPrefix))
}

// paintPicture draws a photo or video's preview or photo fitted in the slide area, with the video action.
func (d *statusDialog) paintPicture(c *walk.Canvas, g statusGeom, m *store.Message, kind slideKind, s func(int) int, dpi int) error {
	ctl, p := d.ctl, d.pal
	var errs []error
	switch {
	case ctl.pic != nil:
		sz := ctl.pic.Size()
		errs = append(errs, c.DrawImageStretchedPixels(ctl.pic, fitShape(sz.Width, sz.Height, g.body)))
	case ctl.thumb != nil:
		errs = append(errs, c.DrawImageStretchedPixels(ctl.thumb, fitShape(ctl.aspect.Width, ctl.aspect.Height, g.body)))
	case m.MediaPath == "":
		errs = append(errs, icons.Draw(c, icons.IconDownload, square(g.body.X+g.body.Width/2, g.body.Y+g.body.Height/2, s(48)), theme.Color(p.TextSecondary)))
	}
	if kind != slideVideo || g.action.Width <= 0 {
		return errors.Join(errs...)
	}
	icon, label := icons.IconPlay, "Open video"
	if m.MediaPath == "" {
		icon, label = icons.IconDownload, "Download video"
	}
	white := walk.RGB(255, 255, 255)
	a := g.action
	errs = append(errs,
		icons.Fill(c, a, theme.Color(p.Accent)),
		icons.Draw(c, icon, square(a.X+s(26), a.Y+a.Height/2, s(24)), white),
		drawText(c, dpi, label, 11, walk.FontBold, white, walk.Rectangle{X: a.X + s(46), Y: a.Y, Width: a.Width - s(58), Height: a.Height}, leftText))
	return errors.Join(errs...)
}
