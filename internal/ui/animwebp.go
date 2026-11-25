package ui

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"os"
	"time"

	"github.com/lxn/walk"
	"golang.org/x/image/draw"

	"wa-go/internal/media"
)

// Animated stickers, decoded by media.DecodeAnimatedWebP.

// errStill is returned for an image that is not animated.
var errStill = media.ErrStill

const (
	maxAnimFile   = 8 << 20 // sticker files are a few hundred KB
	maxAnimBytes  = 6 << 20 // decoded frames kept per sticker; a longer animation loops early
	minFrameDelay = 20 * time.Millisecond
)

// frameSource gives an animation's frames in order, over and over. Used off the UI thread.
type frameSource interface {
	// next is the next frame as top-down 32-bit BGRX pixels and how long it shows. The pixels stay
	// valid until the call after the next one.
	next() (pix []byte, w, h int, show time.Duration, err error)
	close()
}

// loopFrames is an animation decoded whole: a sticker's.
type loopFrames struct {
	pix    [][]byte
	shows  []time.Duration
	w, h   int
	cursor int
}

func (l *loopFrames) next() ([]byte, int, int, time.Duration, error) {
	i := l.cursor
	l.cursor = (i + 1) % len(l.pix)
	return l.pix[i], l.w, l.h, l.shows[i], nil
}

func (l *loopFrames) close() {}

var errEnough = errors.New("ui: enough frames")

// firstWebPFrame is the first picture of the WebP at path that x/image/webp alone cannot decode (see
// media.FirstWebPFrame).
func firstWebPFrame(path string) (*image.RGBA, error) {
	data, err := readSticker(path)
	if err != nil {
		return nil, err
	}
	return media.FirstWebPFrame(data)
}

// readSticker reads the sticker file at path, refusing one far bigger than stickers are.
func readSticker(path string) ([]byte, error) {
	if st, err := os.Stat(path); err != nil {
		return nil, err
	} else if st.Size() > maxAnimFile {
		return nil, fmt.Errorf("ui: sticker of %d bytes is too big to decode", st.Size())
	}
	return os.ReadFile(path)
}

// bgrxOver draws src scaled to fill dst over bg and returns a copy of dst's pixels as the
// top-down BGRX GDI takes.
func bgrxOver(dst *image.RGBA, src image.Image, bg walk.Color) []byte {
	for i := 0; i < len(dst.Pix); i += 4 {
		dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = bg.R(), bg.G(), bg.B(), 0xff
	}
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	pix := bytes.Clone(dst.Pix)
	for i := 0; i < len(pix); i += 4 {
		pix[i], pix[i+2] = pix[i+2], pix[i]
	}
	return pix
}

// playWebP hands show each frame of the animated WebP in data, at its own size over bg as BGRX,
// when it is due, round and round until stop is closed (then nil). Decoding a frame at a time, it
// holds one frame's memory however long the animation. A still WebP gives errStill. Off the UI thread.
func playWebP(data []byte, bg walk.Color, stop <-chan struct{}, show func(pix []byte)) error {
	var dst *image.RGBA
	due := time.Now()
	for {
		err := media.DecodeAnimatedWebP(data, func(canvas *image.NRGBA, delay time.Duration) error {
			if dst == nil {
				dst = image.NewRGBA(canvas.Bounds())
			}
			pix := bgrxOver(dst, canvas, bg)
			if time.Until(due) < 0 {
				due = time.Now() // decoding fell behind: no catching up in a burst
			}
			select {
			case <-stop:
				return errEnough
			case <-time.After(time.Until(due)):
			}
			due = due.Add(delay)
			show(pix)
			return nil
		})
		if errors.Is(err, errEnough) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// newWebPFrames decodes the animated sticker at path into frames w x h pixels, its transparent
// parts filled with bg (the bubble's colour). A still sticker gives errStill.
func newWebPFrames(path string, w, h int, bg walk.Color) (*loopFrames, error) {
	data, err := readSticker(path)
	if err != nil {
		return nil, err
	}
	l := &loopFrames{w: w, h: h}
	scaled := image.NewRGBA(image.Rect(0, 0, w, h))
	err = media.DecodeAnimatedWebP(data, func(canvas *image.NRGBA, show time.Duration) error {
		pix := bgrxOver(scaled, canvas, bg)
		l.pix = append(l.pix, pix)
		l.shows = append(l.shows, show)
		if len(l.pix)*len(scaled.Pix) >= maxAnimBytes {
			return errEnough
		}
		return nil
	})
	if err != nil && !errors.Is(err, errEnough) {
		return nil, err
	}
	if len(l.pix) < 2 {
		return nil, errStill
	}
	return l, nil
}
