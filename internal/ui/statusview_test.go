package ui

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"testing"
	"time"

	"github.com/lxn/walk"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"wa-go/internal/store"
)

// fakeClock is a hand-wound clock and a one-shot timer wheel in one: timers fall due on the fake
// clock, and runFor winds it forward firing them on the way, so nothing sleeps.
type fakeClock struct {
	t       time.Time
	pending []*clockTimer
}

type clockTimer struct {
	due            time.Time
	f              func()
	stopped, fired bool
}

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) after(d time.Duration, f func()) func() bool {
	t := &clockTimer{due: c.t.Add(d), f: f}
	c.pending = append(c.pending, t)
	return func() bool { was := !t.stopped && !t.fired; t.stopped = true; return was }
}

// live is the number of timers that would still fire.
func (c *fakeClock) live() (n int) {
	for _, t := range c.pending {
		if !t.stopped && !t.fired {
			n++
		}
	}
	return n
}

// runFor winds the clock by d, firing the timers that fall due on the way, earliest first.
func (c *fakeClock) runFor(d time.Duration) {
	end := c.t.Add(d)
	for {
		var next *clockTimer
		for _, t := range c.pending {
			if !t.stopped && !t.fired && (next == nil || t.due.Before(next.due)) {
				next = t
			}
		}
		if next == nil || next.due.After(end) {
			c.t = end
			return
		}
		c.t, next.fired = next.due, true
		next.f()
	}
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func TestSeqBoundsAndProgress(t *testing.T) {
	c := newClock()
	s := newStatusSeq(3, c.now)
	if s.index() != 0 || s.progress(0) != 0 || s.progress(1) != 0 {
		t.Fatalf("start: index %d progress %v %v", s.index(), s.progress(0), s.progress(1))
	}
	c.t = c.t.Add(slideDuration / 4)
	if s.progress(0) != 0.25 || s.progress(1) != 0 {
		t.Fatalf("a quarter in: %v %v", s.progress(0), s.progress(1))
	}
	if s.prev(); s.index() != 0 || s.progress(0) != 0 {
		t.Fatalf("prev on the first slide restarts it: index %d progress %v", s.index(), s.progress(0))
	}
	if end := s.next(); end || s.index() != 1 || s.progress(0) != 1 || s.progress(1) != 0 || s.progress(2) != 0 {
		t.Fatalf("next: end %v index %d progress %v %v %v", end, s.index(), s.progress(0), s.progress(1), s.progress(2))
	}
	s.prev()
	if s.index() != 0 {
		t.Fatalf("prev: index %d", s.index())
	}
	s.next()
	s.next()
	if end := s.next(); !end || s.index() != 2 {
		t.Fatalf("next past the last slide: end %v index %d (it stays on the last)", end, s.index())
	}
	c.t = c.t.Add(10 * slideDuration)
	if s.progress(2) != 1 {
		t.Fatalf("progress is capped at 1, got %v", s.progress(2))
	}
}

func TestSeqPauseFreezesAndResumeContinues(t *testing.T) {
	c := newClock()
	s := newStatusSeq(2, c.now)
	c.t = c.t.Add(2 * time.Second)
	s.pause()
	c.t = c.t.Add(time.Hour)
	if !s.paused() || s.remaining() != 3*time.Second || s.progress(0) != 0.4 {
		t.Fatalf("paused: remaining %v progress %v", s.remaining(), s.progress(0))
	}
	s.resume()
	c.t = c.t.Add(time.Second)
	if s.paused() || s.remaining() != 2*time.Second {
		t.Fatalf("resumed: remaining %v", s.remaining())
	}
	s.pause()
	s.next() // navigating while paused keeps it paused, at the start of the new slide
	c.t = c.t.Add(time.Minute)
	if !s.paused() || s.progress(1) != 0 || s.remaining() != slideDuration {
		t.Fatalf("next while paused: paused %v progress %v", s.paused(), s.progress(1))
	}
}

func TestSeqMarkSeenOncePerSlide(t *testing.T) {
	s := newStatusSeq(2, newClock().now)
	if !s.markSeen() || s.markSeen() {
		t.Fatal("slide 0 is first seen once")
	}
	s.next()
	if !s.markSeen() {
		t.Fatal("slide 1 is first seen when shown")
	}
	s.prev()
	if s.markSeen() {
		t.Fatal("slide 0 seen again after going back")
	}
}

// ctlRig is a controller on a fake clock that counts what it was asked to do.
type ctlRig struct {
	*statusCtl
	clock           *fakeClock
	viewed, needed  []string
	opened          []string
	redraws, finish int
	loads           []string // "<id>@<gen>"
}

func newRig(msgs ...store.Message) *ctlRig {
	r := &ctlRig{clock: newClock()}
	cb := StatusCallbacks{
		OnViewed:    func(m store.Message) { r.viewed = append(r.viewed, m.ID) },
		OnNeedMedia: func(m store.Message) { r.needed = append(r.needed, m.ID) },
		OnOpenMedia: func(p string) { r.opened = append(r.opened, p) },
	}
	r.statusCtl = newStatusCtl(msgs, cb, r.clock.now, r.clock.after)
	r.redraw = func() { r.redraws++ }
	r.done = func() { r.finish++ }
	r.load = func(m *store.Message, gen int64) { r.loads = append(r.loads, m.ID+"@"+string(rune('0'+gen))) }
	return r
}

func img(id string) store.Message { return store.Message{ID: id, Type: "image"} }
func txt(id string) store.Message { return store.Message{ID: id, Type: "text", Body: "hi"} }

func TestCtlAutoAdvancesAfterFiveSecondsAndClosesAfterTheLast(t *testing.T) {
	r := newRig(txt("a"), txt("b"))
	r.start()
	if r.index() != 0 || r.clock.live() != 1 {
		t.Fatalf("start: index %d, %d timers", r.index(), r.clock.live())
	}
	r.clock.runFor(slideDuration - time.Millisecond)
	if r.index() != 0 {
		t.Fatalf("advanced early, index %d", r.index())
	}
	r.clock.runFor(time.Millisecond)
	if r.index() != 1 || r.finish != 0 {
		t.Fatalf("after 5 s: index %d, finished %d", r.index(), r.finish)
	}
	r.clock.runFor(slideDuration)
	if r.finish != 1 || r.clock.live() != 0 {
		t.Fatalf("after the last slide: finished %d, %d timers left", r.finish, r.clock.live())
	}
	if r.clock.runFor(10 * time.Second); r.finish != 1 {
		t.Fatal("closed more than once")
	}
}

func TestCtlPauseStopsTimersAndResumeFinishesTheSlide(t *testing.T) {
	r := newRig(txt("a"), txt("b"))
	r.start()
	r.clock.runFor(2 * time.Second)
	r.togglePause()
	if r.clock.live() != 0 {
		t.Fatalf("%d timers run while paused", r.clock.live())
	}
	r.clock.runFor(time.Hour)
	if r.index() != 0 {
		t.Fatal("advanced while paused")
	}
	r.togglePause()
	r.clock.runFor(3*time.Second - time.Millisecond)
	if r.index() != 0 {
		t.Fatalf("advanced before the remaining 3 s, index %d", r.index())
	}
	r.clock.runFor(time.Millisecond)
	if r.index() != 1 {
		t.Fatalf("index %d after the remaining 3 s", r.index())
	}
}

func TestCtlNavigationRearmsOneTimerAndIgnoresStaleOnes(t *testing.T) {
	r := newRig(txt("a"), txt("b"), txt("c"))
	r.start()
	stale := r.clock.pending[0]
	r.next()
	r.next()
	if r.index() != 2 || r.clock.live() != 1 {
		t.Fatalf("index %d with %d timers", r.index(), r.clock.live())
	}
	stale.f() // a timer queued before the click that fires anyway
	if r.index() != 2 {
		t.Fatalf("a stale timer moved the slide to %d", r.index())
	}
	r.prev()
	if r.index() != 1 {
		t.Fatalf("prev: %d", r.index())
	}
}

func TestCtlCloseStopsEverything(t *testing.T) {
	r := newRig(txt("a"), txt("b"))
	r.start()
	r.close()
	if r.clock.live() != 0 {
		t.Fatalf("%d timers after close", r.clock.live())
	}
	r.next()
	r.togglePause()
	r.clock.runFor(time.Minute)
	if r.clock.live() != 0 || r.index() != 0 || len(r.viewed) != 1 {
		t.Fatalf("a closed viewer moved: %d timers, index %d, viewed %v", r.clock.live(), r.index(), r.viewed)
	}
}

func TestCtlViewedOncePerStatusEvenWhenGoingBack(t *testing.T) {
	r := newRig(txt("a"), txt("b"), txt("c"))
	r.start()
	r.next()
	r.prev()
	r.next()
	r.next()
	r.prev()
	r.prev()
	if got := strings.Join(r.viewed, ","); got != "a,b,c" {
		t.Fatalf("viewed %q, want each status once in first-shown order", got)
	}
}

func TestCtlNeedMediaOncePerMessageAndOnlyWithoutAFile(t *testing.T) {
	have := img("have")
	have.MediaPath = `C:\cache\have.jpg`
	vid := store.Message{ID: "vid", Type: "video"}
	r := newRig(img("a"), have, vid, txt("t"))
	r.start()
	r.next()
	r.next()
	r.next()
	r.prev()
	r.prev()
	r.prev() // back to "a": no second request
	r.next()
	if got := strings.Join(r.needed, ","); got != "a,vid" {
		t.Fatalf("media asked for %q, want a,vid once each (not the downloaded image or the text)", got)
	}
}

func TestCtlSetMediaPathLoadsTheCurrentImageOnly(t *testing.T) {
	r := newRig(img("a"), img("b"))
	r.start()
	if len(r.loads) != 1 {
		t.Fatalf("loads on show: %v", r.loads)
	}
	r.SetMediaPath("b", "p-b") // not the shown slide: stored, nothing loaded
	if len(r.loads) != 1 {
		t.Fatalf("loaded for another slide: %v", r.loads)
	}
	r.SetMediaPath("a", "p-a")
	if len(r.loads) != 2 || !strings.HasPrefix(r.loads[1], "a@") {
		t.Fatalf("loads %v", r.loads)
	}
	r.next() // "b" has its path now: loaded at once, not requested
	if len(r.loads) != 3 || !strings.HasPrefix(r.loads[2], "b@") || len(r.needed) != 1 {
		t.Fatalf("loads %v, needed %v", r.loads, r.needed)
	}
	r.SetMediaPath("nobody", "x") // unknown id: ignored
}

func TestCtlStaleDecodeIsDropped(t *testing.T) {
	r := newRig(img("a"), img("b"))
	r.start()
	gen := r.slideGen()
	r.next()
	if r.imageReady(gen, nil) {
		t.Fatal("a decode begun for the previous slide was accepted")
	}
	if !r.imageReady(r.slideGen(), nil) {
		t.Fatal("the current slide's decode was dropped")
	}
	r.close()
	if r.imageReady(r.slideGen(), nil) {
		t.Fatal("a decode landing after close was accepted")
	}
}

func TestCtlVideoActionOpensOrRequests(t *testing.T) {
	v := store.Message{ID: "v", Type: "video"}
	r := newRig(v)
	r.start()
	r.activate()
	if len(r.opened) != 0 || strings.Join(r.needed, ",") != "v,v" {
		t.Fatalf("not downloaded: opened %v, needed %v", r.opened, r.needed)
	}
	r.SetMediaPath("v", `C:\cache\v.mp4`)
	r.activate()
	if len(r.opened) != 1 || r.opened[0] != `C:\cache\v.mp4` {
		t.Fatalf("downloaded: opened %v", r.opened)
	}
	t1 := newRig(txt("t"))
	t1.start()
	t1.activate() // nothing to open on a text status
	if len(t1.opened)+len(t1.needed) != 0 {
		t.Fatal("a text status has no action")
	}
}

func TestStatusKinds(t *testing.T) {
	for _, c := range []struct {
		typ  string
		want slideKind
	}{{"image", slideImage}, {"video", slideVideo}, {"text", slideText}, {"unsupported", slideText}, {"", slideText}} {
		if got := slideKindOf(c.typ); got != c.want {
			t.Errorf("%q: %v want %v", c.typ, got, c.want)
		}
	}
}

func jpegBytes(w, h int) []byte {
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			im.Set(x, y, color.RGBA{uint8(x), uint8(y), 99, 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, im, nil); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func TestFitImageKeepsAspectNeverUpscalesAndRefusesHugeImages(t *testing.T) {
	big := jpegBytes(400, 200)
	got, err := fitImage(bytes.NewReader(big), 100, 100, maxFullPixels)
	if err != nil || got.Bounds().Dx() != 100 || got.Bounds().Dy() != 50 {
		t.Fatalf("400x200 into 100x100: %v, %v", got.Bounds(), err)
	}
	got, err = fitImage(bytes.NewReader(big), 1000, 1000, maxFullPixels)
	if err != nil || got.Bounds().Dx() != 400 || got.Bounds().Dy() != 200 {
		t.Fatalf("small image upscaled: %v, %v", got.Bounds(), err)
	}
	if _, err := fitImage(bytes.NewReader(big), 100, 100, 400*200-1); err == nil {
		t.Fatal("an image over the pixel cap was decoded")
	}
	if _, err := fitImage(bytes.NewReader([]byte("not an image")), 100, 100, maxFullPixels); err == nil {
		t.Fatal("garbage decoded")
	}
}

func TestFitRect(t *testing.T) {
	area := walk.Rectangle{X: 10, Y: 20, Width: 400, Height: 300}
	for _, c := range []struct {
		w, h int
		want walk.Rectangle
	}{
		{800, 400, walk.Rectangle{X: 10, Y: 70, Width: 400, Height: 200}},  // wide: letterboxed
		{300, 600, walk.Rectangle{X: 135, Y: 20, Width: 150, Height: 300}}, // tall: pillarboxed
		{0, 0, area}, // unknown size fills
	} {
		if got := fitRect(c.w, c.h, area); got != c.want {
			t.Errorf("%dx%d: %v want %v", c.w, c.h, got, c.want)
		}
	}
}

func TestSlidePreviewSurvivesHostileMediaMeta(t *testing.T) {
	good := &waE2E.ImageMessage{JPEGThumbnail: jpegBytes(8, 8), Width: proto.Uint32(8), Height: proto.Uint32(8)}
	meta, err := proto.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]store.Message{
		"valid":    {Type: "image", MediaMeta: meta},
		"garbage":  {Type: "image", MediaMeta: []byte{0xff, 0xff, 0xff, 0x01}},
		"empty":    {Type: "video"},
		"not-jpeg": {Type: "image", MediaMeta: mustMarshal(t, &waE2E.ImageMessage{JPEGThumbnail: []byte("nope")})},
		"huge":     {Type: "image", MediaMeta: mustMarshal(t, &waE2E.ImageMessage{JPEGThumbnail: jpegBytes(1200, 1000)})},
	} {
		pv, w, h := slidePreview(&m)
		if (name == "valid") != (pv != nil) {
			t.Errorf("%s: preview %v", name, pv != nil)
		}
		if name == "valid" && (w != 8 || h != 8) {
			t.Errorf("valid: size %dx%d", w, h)
		}
	}
}

func mustMarshal(t *testing.T, m proto.Message) []byte {
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStatusGeometryHits(t *testing.T) {
	s := func(v int) int { return v }
	g := statusGeometry(400, 700, s, true, true, 5)
	if g.body.Y < g.top.Height || g.caption.Y < g.body.Y+g.body.Height-1 || g.caption.Height <= 0 {
		t.Fatalf("bands overlap: top %v body %v caption %v", g.top, g.body, g.caption)
	}
	if len(g.segs) != 5 || g.segs[0].X < 0 || g.segs[4].X+g.segs[4].Width > 400 {
		t.Fatalf("segments %v", g.segs)
	}
	for i := 1; i < len(g.segs); i++ {
		if g.segs[i].X <= g.segs[i-1].X+g.segs[i-1].Width {
			t.Fatalf("segments touch or overlap: %v", g.segs)
		}
	}
	mid := func(r walk.Rectangle) (int, int) { return r.X + r.Width/2, r.Y + r.Height/2 }
	x, y := mid(g.close)
	if g.hit(x, y) != hitClose {
		t.Error("close button")
	}
	x, y = mid(g.pause)
	if g.hit(x, y) != hitPause {
		t.Error("pause button")
	}
	if g.hit(50, g.body.Y+g.body.Height/2) != hitPrev || g.hit(350, g.body.Y+g.body.Height/2) != hitNext {
		t.Error("halves of the slide go back and forward")
	}
	x, y = mid(g.action)
	if g.hit(x, y) != hitAction {
		t.Error("the video action wins over the half it sits in")
	}
	if g.hit(200, g.top.Height-1) != hitNone || g.hit(5, 5) != hitNone {
		t.Error("the header is not a slide")
	}
	if g.hit(5, g.caption.Y+5) != hitNone {
		t.Error("the caption is not a slide")
	}
	if noAct := statusGeometry(400, 700, s, false, false, 1); noAct.action.Width != 0 || noAct.hit(200, noAct.body.Y+noAct.body.Height/2) != hitNext {
		t.Error("no video, no action: the whole slide pages")
	}
	if noCap := statusGeometry(400, 700, s, false, false, 1); noCap.caption.Height != 0 || noCap.body.Height <= g.body.Height {
		t.Errorf("without a caption the slide takes the room: %v vs %v", noCap.body, g.body)
	}
}

func TestStoryTime(t *testing.T) {
	now := time.Date(2026, 11, 11, 15, 0, 0, 0, time.Local)
	at := func(h, m int, dayBack int) int64 {
		return time.Date(2026, 11, 11-dayBack, h, m, 0, 0, time.Local).Unix()
	}
	for _, c := range []struct {
		ts   int64
		want string
	}{{at(14, 5, 0), "Today, 14:05"}, {at(23, 0, 1), "Yesterday"}, {0, ""}} {
		if got := storyTime(c.ts, now); got != c.want {
			t.Errorf("%v: %q want %q", c.ts, got, c.want)
		}
	}
}
