package ui

import (
	"cmp"
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
	"golang.org/x/sys/windows"

	"wa-go/internal/media"
	"wa-go/internal/store"
	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
)

const (
	mediaBarH = 56   // top bar, 1/96 inch
	maxZoom   = 4    // times the fitted size
	zoomStep  = 1.25 // per Ctrl +/- or wheel notch
	videoBarH = 48   // a video's control bar, 1/96 inch
	videoPoll = 250 * time.Millisecond
	videoWait = 5 * time.Second // for playback to start before the video counts as unplayable
	videoSkip = 5 * time.Second // per Shift+← or Shift+→
)

// every runs tick every d on a goroutine until the returned channel is closed.
func every(d time.Duration, tick func()) chan struct{} {
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(d)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				tick()
			}
		}
	}()
	return stop
}

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
	mediaBroken     // could not be decoded
	mediaVideo      // the player runs
	mediaUnplayable // the player failed: Open with…
)

// MediaView shows a chat's photos and stickers in the chat column: a top bar (back, sender and time,
// zoom, Save as…, Open with…) over the picture, fitted, zoomed 1×–4× by the wheel around the
// pointer, Ctrl + / −, or a double-click, and dragged when zoomed. ← and → step through the chat's
// photos and videos, 0 fits, Esc closes. The picture is decoded only while shown. Videos play in
// video, over a control bar (Space plays or pauses, ← → seek 5 s, m mutes); GIFs play muted and
// loop, without the bar. UI thread only.
type MediaView struct {
	*walk.Composite
	*kit
	canvas             *walk.CustomWidget
	video, ctrl        *walk.CustomWidget // the player's window and a video's control bar
	who, when, zoomLbl *label
	save, with         *button

	play     mediaPlayer // nil while no video is open
	stop     chan struct{}
	gif      bool
	muted    bool
	started  bool // the player has played or paused
	opened   time.Time
	st       playerState
	pos, dur time.Duration
	gifAt    time.Duration // how far a GIF has got (see gifReached); not rewound when mfplay stops it
	seeking  bool          // the seek bar is dragged

	list      []store.Message
	i         int
	sender    func(store.Message) string
	state     mediaState
	failure   string
	bmp       *walk.Bitmap
	frame     []byte        // an animated sticker's frame on show (BGRX, size), over bmp; nil when still
	animStop  chan struct{} // closed to end the sticker's animation
	size      walk.Size     // the picture's pixels
	gen       int           // per picture shown: a decode finished for another is dropped
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
	return v.buildVideo()
}

// buildVideo adds the player's window and the control bar, hidden.
func (v *MediaView) buildVideo() error {
	var err error
	if v.video, err = walk.NewCustomWidgetPixels(v.Composite, 0, v.paintVideo); err != nil {
		return err
	}
	vw := v.video
	vw.SetPaintMode(walk.PaintNoErase) // the player draws the frames
	if err := errors.Join(fillRest(v.Composite, vw), focusable(vw, walk.AccRoleAnimation, "Video", nil)); err != nil {
		return err
	}
	v.keys(vw, v.videoKey)
	vw.MouseDown().Attach(func(_, _ int, b walk.MouseButton) {
		if b == walk.LeftButton {
			focusOn(vw)
			if !v.gif {
				v.toggle()
			}
		}
	})
	vw.SizeChanged().Attach(func() { v.updateVideo() })
	if v.ctrl, err = newCanvas(v.Composite, v.paintCtrl); err != nil {
		return err
	}
	v.canvases = append(v.canvases, v.ctrl)
	vw.FocusedChanged().Attach(func() { logErr("video bar repaint", v.ctrl.Invalidate()) }) // its focus shows there
	if err := v.ctrl.SetMinMaxSize(walk.Size{Height: videoBarH}, walk.Size{Height: videoBarH}); err != nil {
		return err
	}
	v.ctrl.MouseDown().Attach(v.ctrlDown)
	v.ctrl.MouseMove().Attach(func(x, _ int, b walk.MouseButton) {
		if v.seeking = v.seeking && b&walk.LeftButton != 0; v.seeking {
			v.seekTo(x)
		}
	})
	v.ctrl.MouseUp().Attach(func(_, _ int, _ walk.MouseButton) { v.seeking = false })
	vw.SetVisible(false)
	v.ctrl.SetVisible(false)
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
	focusOn(v.area())
}

var (
	kernel32Dll               = windows.NewLazySystemDLL("kernel32.dll")
	pSetProcessWorkingSetSize = kernel32Dll.NewProc("SetProcessWorkingSetSize")
)

// Close drops the picture or shuts the player down, drops the messages and gives the memory back.
func (v *MediaView) Close() {
	v.gen++
	v.drop()
	v.list, v.sender, v.dragging = nil, nil, false
	debug.FreeOSMemory()
	if h, err := windows.GetCurrentProcess(); err == nil {
		_, _, _ = pSetProcessWorkingSetSize.Call(uintptr(h), ^uintptr(0), ^uintptr(0))
	}
}

func (v *MediaView) drop() {
	if v.bmp != nil {
		v.bmp.Dispose()
		v.bmp = nil
	}
	if v.animStop != nil {
		close(v.animStop)
		v.animStop = nil
	}
	v.frame = nil
	v.size, v.zoom, v.pan = walk.Size{}, 1, walk.Point{}
	v.stopVideo()
}

// stopVideo stops the timer and shuts the player down, if one runs.
func (v *MediaView) stopVideo() {
	if v.stop != nil {
		close(v.stop)
		v.stop = nil
	}
	if v.play != nil {
		v.play.Shutdown()
		v.play = nil
	}
	v.seeking = false
}

// area is the window showing the current message: the player's while a video plays.
func (v *MediaView) area() *walk.CustomWidget {
	if v.play != nil {
		return v.video
	}
	return v.canvas
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
	info, err := parseMedia(&m)
	v.gif = ok && m.Type == "video" && err == nil && info.gif
	switch {
	case !ok:
		return
	case needsDownload(m):
		v.state = mediaDownloading
		if v.onNeedMedia != nil {
			v.onNeedMedia(m)
		}
	case m.Type == "video":
		v.startVideo(m)
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
		if m, ok := v.current(); ok && m.ID == id && animatable(m.Type, m.MediaPath, false) {
			v.playSticker(m.MediaPath)
		}
	}
	v.update()
}

// playSticker animates the sticker at path over its still first frame, decoding a frame at a time
// so a big sticker costs no more memory than one, until drop. A still sticker stays as it is.
func (v *MediaView) playSticker(path string) {
	stop := make(chan struct{})
	v.animStop = stop
	bg := theme.Color(v.pal.ChatBg)
	go func() {
		data, err := readSticker(path)
		if err == nil {
			err = playWebP(data, bg, stop, func(pix []byte) {
				v.canvas.Synchronize(func() {
					if v.animStop == stop {
						v.frame = pix
						logErr("sticker repaint", v.canvas.Invalidate())
					}
				})
			})
		}
		if err != nil && !errors.Is(err, errStill) {
			v.canvas.Synchronize(func() { logErr("sticker animation", err) })
		}
	}()
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
	switch {
	case m.Type == "sticker":
		kind = "Sticker"
	case m.Type == "video" && v.gif:
		kind = "GIF"
	case m.Type == "video":
		kind = "Video"
	}
	focused := v.canvas.Focused() || v.video.Focused()
	playing := v.play != nil
	v.canvas.SetVisible(!playing)
	v.video.SetVisible(playing)
	v.ctrl.SetVisible(playing && !v.gif)
	if focused {
		focusOn(v.area()) // the focused window may have gone
	}
	announce(v.area(), kind+" from "+v.sender(m)+", "+mediaWhen(m.Timestamp, time.Now()), v.stateText())
	v.reframe(v.zoom, v.pan)
}

// startVideo creates the player for m (downloaded) and the timer that polls it.
func (v *MediaView) startVideo(m store.Message) {
	v.muted, v.started, v.opened, v.st, v.pos, v.dur, v.gifAt = v.gif, false, time.Now(), playerEmpty, 0, 0, 0
	var p mediaPlayer
	var err error
	if v.gif {
		p, err = newGifPlayer(v.video.Handle(), m.MediaPath)
	} else {
		p, err = newPlayer(v.video.Handle(), m.MediaPath)
	}
	if err != nil {
		logErr("video "+m.ID, err)
		v.state = mediaUnplayable
		return
	}
	v.play, v.state = p, mediaVideo
	if v.gif {
		logErr("mute", p.SetMute(true))
	}
	v.stop = every(videoPoll, func() { v.video.Synchronize(v.poll) })
}

// poll follows the player: whether it started in time, a GIF's loop, the control bar.
func (v *MediaView) poll() {
	if v.play == nil {
		return // closed meanwhile
	}
	st, err := v.play.State()
	if err == nil && !v.started && (st == playerPlaying || st == playerPaused) {
		v.started = true
		logErr("mute", v.play.SetMute(v.muted))
		v.updateVideo()
	}
	if err != nil || !v.started && time.Since(v.opened) > videoWait {
		if m, ok := v.current(); ok {
			logErr("video "+m.ID, cmp.Or(err, errors.New("playback did not start")))
		}
		v.stopVideo()
		v.state = mediaUnplayable
		v.update()
		return
	}
	pos, dur := v.pos, v.dur
	if d, err := v.play.Position(); err == nil {
		pos = d
	}
	if d, err := v.play.Duration(); err == nil {
		dur = d
	}
	v.gifAt = gifReached(st, pos, v.gifAt)
	if v.gif && v.started && gifRestart(st, v.gifAt, dur) {
		logErr("gif loop", errors.Join(v.play.Seek(0), v.play.Play()))
		v.gifAt = 0 // until it plays again: no second restart before a whole loop
	}
	if st != v.st || pos != v.pos || dur != v.dur {
		v.st, v.pos, v.dur = st, pos, dur
		logErr("video bar repaint", v.ctrl.Invalidate())
	}
}

// toggle plays or pauses a video.
func (v *MediaView) toggle() {
	if v.play == nil {
		return
	}
	if v.st == playerPlaying {
		logErr("pause", v.play.Pause())
		v.st = playerPaused
	} else {
		logErr("play", v.play.Play())
		v.st = playerPlaying
	}
	logErr("video bar repaint", v.ctrl.Invalidate())
}

func (v *MediaView) toggleMute() {
	if v.play != nil {
		v.muted = !v.muted
		logErr("mute", v.play.SetMute(v.muted))
		logErr("video bar repaint", v.ctrl.Invalidate())
	}
}

// seek moves a video to d, kept within it.
func (v *MediaView) seek(d time.Duration) {
	if v.play == nil || v.dur <= 0 {
		return
	}
	v.pos = max(0, min(v.dur-100*time.Millisecond, d)) // seeking to the very end stalls mfplay
	logErr("seek", v.play.Seek(v.pos))
	logErr("video bar repaint", v.ctrl.Invalidate())
}

// updateVideo repaints the video; before playback starts there is none to paint.
func (v *MediaView) updateVideo() {
	if v.play != nil && v.started {
		logErr("video repaint", v.play.UpdateVideo())
	}
}

// videoKey handles a video's keys on the player's window: Space, Shift+← and Shift+→ (seek), m; the
// rest, and all of a GIF's, go on to the view's (← and → step to the previous or next media).
func (v *MediaView) videoKey(k walk.Key) bool {
	if v.play == nil || v.gif {
		return false
	}
	shift := walk.ShiftDown()
	switch {
	case k == walk.KeySpace:
		v.toggle()
	case k == walk.KeyLeft && shift:
		v.seek(v.pos - videoSkip)
	case k == walk.KeyRight && shift:
		v.seek(v.pos + videoSkip)
	case k == walk.KeyM:
		v.toggleMute()
	default:
		return false
	}
	return true
}

// ctrlParts lays the control bar out: play/pause, the seek bar, the clock and mute (pixels).
func (v *MediaView) ctrlParts() (play, track, clock, mute walk.Rectangle) {
	b, s := v.ctrl.ClientBoundsPixels(), scaler(v.ctrl.DPI())
	play = walk.Rectangle{X: s(4), Width: s(44), Height: b.Height}
	mute = walk.Rectangle{X: b.Width - s(48), Width: s(44), Height: b.Height}
	clock = walk.Rectangle{X: mute.X - s(116), Width: s(112), Height: b.Height}
	track = walk.Rectangle{X: play.X + play.Width + s(8), Y: b.Height/2 - s(2), Height: s(4)}
	track.Width = max(0, clock.X-s(8)-track.X)
	return play, track, clock, mute
}

func (v *MediaView) ctrlDown(x, y int, b walk.MouseButton) {
	if b != walk.LeftButton || v.play == nil {
		return
	}
	focusOn(v.video)
	play, track, _, mute := v.ctrlParts()
	slack := scaler(v.ctrl.DPI())(8)
	switch {
	case in(play, x, y):
		v.toggle()
	case in(mute, x, y):
		v.toggleMute()
	case x >= track.X-slack && x <= track.X+track.Width+slack:
		v.seeking = true
		v.seekTo(x)
	}
}

func (v *MediaView) seekTo(x int) {
	_, track, _, _ := v.ctrlParts()
	v.seek(seekAt(x-track.X, track.Width, v.dur))
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
	case mediaUnplayable:
		return "Windows can't play this video"
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
			focusOn(v.area()) // Save as… may have gone
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

// act is Enter on the picture: Retry after a failed download.
func (v *MediaView) act() {
	if v.state == mediaFailed {
		v.show(v.i)
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
	ext := filepath.Ext(src)
	dlg := walk.FileDialog{Title: "Save as", Flags: win.OFN_OVERWRITEPROMPT, FilePath: "Wisp " + time.Unix(m.Timestamp, 0).Format("2006-01-02 15.04.05") + ext}
	if ext != "" {
		dlg.Filter, dlg.FilterIndex = saveFilter(ext), 1
	}
	ok, err := dlg.ShowSave(v.Form())
	if err != nil || !ok || v.list == nil {
		logErr("save as", err)
		return // cancelled or closed by the lock
	}
	// walk sets no default extension: a name typed without one gets the file's, if its type is chosen
	if ext != "" && dlg.FilterIndex == 1 && filepath.Ext(dlg.FilePath) == "" {
		dlg.FilePath += ext
		if _, err := os.Stat(dlg.FilePath); err == nil && walk.MsgBox(v.Form(), "Save as",
			filepath.Base(dlg.FilePath)+" already exists.\nDo you want to replace it?", walk.MsgBoxIconWarning|walk.MsgBoxYesNo) != walk.DlgCmdYes {
			return
		}
	}
	if strings.EqualFold(filepath.Clean(dlg.FilePath), filepath.Clean(src)) {
		return // the cached file itself
	}
	if err := copyFile(src, dlg.FilePath); err != nil {
		logErr("save as", err)
		walk.MsgBox(v.Form(), "Wisp", "This file was not saved: "+err.Error(), walk.MsgBoxIconWarning|walk.MsgBoxOK)
	}
}

// saveFilter is Save as's type list for a file ending ext (".jpg"): that type, then all files.
func saveFilter(ext string) string {
	name := strings.ToUpper(strings.TrimPrefix(ext, "."))
	if kind := saveKinds[strings.ToLower(ext)]; kind != "" {
		name = kind
	} else {
		name += " file"
	}
	return name + " (*" + ext + ")|*" + ext + "|All files (*.*)|*.*"
}

var saveKinds = map[string]string{".jpg": "JPEG image", ".jpeg": "JPEG image", ".png": "PNG image", ".webp": "WebP image",
	".gif": "GIF image", ".mp4": "MP4 video", ".ogg": "Ogg audio", ".opus": "Opus audio", ".mp3": "MP3 audio", ".pdf": "PDF document"}

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
	switch {
	case v.state == mediaShown && v.frame != nil:
		errs = append(errs, blitFrame(c.HDC(), fitRect(v.size, b, v.zoom, v.pan), v.frame, v.size.Width, v.size.Height))
	case v.state == mediaShown:
		errs = append(errs, c.DrawImageStretchedPixels(v.bmp, fitRect(v.size, b, v.zoom, v.pan)))
	case v.state == mediaLoading:
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

func (v *MediaView) paintVideo(c *walk.Canvas, _ walk.Rectangle) error {
	if v.play != nil && v.started {
		return v.play.UpdateVideo()
	}
	return fillBand(c, v.video.ClientBoundsPixels(), v.pal.ChatBg) // until the first frame
}

// paintCtrl paints the control bar; it also shows the player window's keyboard focus.
func (v *MediaView) paintCtrl(c *walk.Canvas, _ walk.Rectangle) error {
	p, b, dpi := v.pal, v.ctrl.ClientBoundsPixels(), v.ctrl.DPI()
	s := scaler(dpi)
	play, track, clock, mute := v.ctrlParts()
	pp, snd := icons.IconPlay, icons.IconVolume
	if v.st == playerPlaying {
		pp = icons.IconPause
	}
	if v.muted {
		snd = icons.IconMute
	}
	text := theme.Color(p.Text)
	errs := []error{fillBand(c, b, p.Sidebar),
		icons.Draw(c, pp, square(play.X+play.Width/2, b.Height/2, s(24)), text),
		icons.Draw(c, snd, square(mute.X+mute.Width/2, b.Height/2, s(24)), text),
		drawText(c, dpi, clockText(v.pos)+" / "+clockText(v.dur), 10, 0, v.color(toneSecondary), clock, centerText)}
	if track.Width > 0 {
		x := track.X
		if v.dur > 0 {
			x += int(float64(track.Width) * float64(min(v.pos, v.dur)) / float64(v.dur))
		}
		errs = append(errs, icons.Fill(c, track, theme.Color(p.TextSecondary)))
		if x > track.X {
			errs = append(errs, icons.Fill(c, walk.Rectangle{X: track.X, Y: track.Y, Width: x - track.X, Height: track.Height}, theme.Color(p.Accent)))
		}
		errs = append(errs, icons.Fill(c, square(x, b.Height/2, s(12)), theme.Color(p.Accent)))
	}
	if focusCues(v.video) {
		errs = append(errs, focusRect(c, b, s(2), p.Accent))
	}
	return errors.Join(errs...)
}
