package ui

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"

	"wa-go/internal/store"
	"wa-go/internal/theme"
)

// Playing stickers and GIFs in the chat. A bubble on screen whose media is animated gets an
// animation: a goroutine that decodes its frames and hands each to the UI thread, which repaints
// just that picture. Painting is what keeps an animation alive: one whose bubble was not painted
// for animIdle (scrolled away, another chat, the window minimised or locked) stops and frees its
// decoder, and starts again from the first frame when the bubble is next painted. So nothing runs
// while nothing animated is on show.

const (
	maxAnims = 4               // playing at once; the rest stay still
	animIdle = 1 * time.Second // unpainted this long, an animation stops
)

var pStretchDIBits = gdi32.NewProc("StretchDIBits")

// animatable reports whether a message's media plays in its bubble: a downloaded WebP sticker (it
// may turn out to be a still one) or a downloaded GIF.
func animatable(typ, path string, gif bool) bool {
	switch {
	case path == "":
		return false
	case typ == "sticker":
		return strings.EqualFold(filepath.Ext(path), ".webp")
	}
	return typ == "video" && gif
}

// animation is one playing sticker or GIF. Its fields belong to the UI thread; the goroutine only
// reads stop.
type animation struct {
	pix  []byte // the frame on show, top-down BGRX; nil before the first
	w, h int
	seen time.Time     // when its bubble was last painted
	stop chan struct{} // closed to end the goroutine
}

// animFor is the animation of message m, whose picture is being painted at r, started if it has
// none and may have one; nil when it does not play.
func (v *ChatView) animFor(m *store.Message, gif bool, r walk.Rectangle) *animation {
	if a := v.anims[m.ID]; a != nil {
		a.seen = time.Now()
		return a
	}
	if v.still[m.ID] || len(v.anims) >= maxAnims || !animatable(m.Type, m.MediaPath, gif) {
		return nil
	}
	a := &animation{seen: time.Now(), stop: make(chan struct{})}
	v.anims[m.ID] = a
	go v.runAnim(a, m.ID, m.MediaPath, m.Type == "sticker", r.Width, r.Height, theme.Color(bubbleColor(v.pal, m.FromMe)))
	return a
}

// runAnim decodes and times a's frames until it is stopped. Off the UI thread.
func (v *ChatView) runAnim(a *animation, id, path string, sticker bool, w, h int, bg walk.Color) {
	var src frameSource
	var err error
	if sticker {
		src, err = newWebPFrames(path, w, h, bg)
	} else {
		runtime.LockOSThread() // the video reader lives on one thread
		defer runtime.UnlockOSThread()
		src, err = newVideoFrames(path)
	}
	gaveUp := func(err error) {
		v.canvas.Synchronize(func() {
			if v.anims[id] != a {
				return
			}
			v.stopAnim(id)
			v.still[id] = true // not asked again for this chat: its still picture stays
			if !errors.Is(err, errStill) {
				logErr("animation of message "+id, err)
			}
		})
	}
	if err != nil {
		gaveUp(err)
		return
	}
	defer src.close()
	due := time.Now()
	for {
		pix, fw, fh, show, err := src.next()
		if err != nil {
			gaveUp(err)
			return
		}
		if wait := time.Until(due); wait > 0 {
			select {
			case <-a.stop:
				return
			case <-time.After(wait):
			}
		} else {
			select {
			case <-a.stop:
				return
			default:
			}
			due = time.Now() // decoding fell behind: no catching up in a burst
		}
		due = due.Add(show)
		v.canvas.Synchronize(func() {
			if v.anims[id] != a {
				return
			}
			if time.Since(a.seen) > animIdle {
				v.stopAnim(id)
				return
			}
			a.pix, a.w, a.h = pix, fw, fh
			v.repaintThumb(id)
		})
	}
}

// stopAnim ends message id's animation, if it has one.
func (v *ChatView) stopAnim(id string) {
	if a := v.anims[id]; a != nil {
		close(a.stop)
		delete(v.anims, id)
	}
}

// stopAnims ends every animation and forgets which media turned out still: for another chat.
func (v *ChatView) stopAnims() {
	for id := range v.anims {
		v.stopAnim(id)
	}
	clear(v.still)
}

// repaintThumb repaints only the picture of message id.
func (v *ChatView) repaintThumb(id string) {
	i := v.index(id)
	if i < 0 || len(v.lay.tops) != len(v.msgs) {
		return
	}
	t := v.lay.cache[id].thumb
	y := int32(v.screenY(i) + t.Y)
	win.InvalidateRect(v.canvas.Handle(), &win.RECT{Left: int32(t.X), Top: y, Right: int32(t.X + t.Width), Bottom: y + int32(t.Height)}, false)
}

// blitFrame draws a w x h frame of top-down BGRX pixels to fill r on hdc, cropped at its middle to
// r's shape as thumbnails are.
func blitFrame(hdc win.HDC, r walk.Rectangle, pix []byte, w, h int) error {
	if w <= 0 || h <= 0 || len(pix) < w*h*4 || r.Width <= 0 || r.Height <= 0 {
		return nil
	}
	sx, sy, sw, sh := 0, 0, w, h
	if w*r.Height > h*r.Width { // wider than the box: lose the sides
		sw = max(1, h*r.Width/r.Height)
		sx = (w - sw) / 2
	} else { // lose top and bottom, equally: so it does not matter which end rows count from
		sh = max(1, w*r.Height/r.Width)
		sy = (h - sh) / 2
	}
	header := win.BITMAPINFOHEADER{BiSize: 40, BiWidth: int32(w), BiHeight: -int32(h), BiPlanes: 1, BiBitCount: 32, BiCompression: win.BI_RGB}
	old := win.SetStretchBltMode(hdc, win.HALFTONE) // averaged, not dropped, pixels when shrinking
	defer win.SetStretchBltMode(hdc, old)
	const srcCopy = 0x00cc0020
	if n, _, _ := pStretchDIBits.Call(uintptr(hdc), uintptr(r.X), uintptr(r.Y), uintptr(r.Width), uintptr(r.Height),
		uintptr(sx), uintptr(sy), uintptr(sw), uintptr(sh), uintptr(unsafe.Pointer(&pix[0])), uintptr(unsafe.Pointer(&header)), 0, srcCopy); n == 0 {
		return fmt.Errorf("ui: StretchDIBits failed")
	}
	return nil
}
