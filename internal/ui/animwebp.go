package ui

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"os"
	"time"

	"github.com/lxn/walk"
	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

// Animated stickers. x/image/webp decodes only still WebP, so the animation container is read here:
// each frame in it is a still WebP bitstream, decoded by that package and composited onto the
// canvas as the format says (offset, blend, dispose).

// errStill is returned for an image that is not animated.
var errStill = errors.New("ui: not an animated image")

const (
	maxAnimFrames = 240
	maxAnimPixels = 1 << 22 // canvas size refused above this
	maxAnimFile   = 8 << 20 // sticker files are a few hundred KB
	maxAnimBytes  = 6 << 20 // decoded frames kept per sticker; a longer animation loops early
	minFrameDelay = 20 * time.Millisecond
)

func u24(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }

// decodeAnimatedWebP calls each with the whole picture after every frame of the animated WebP in
// data, and how long that frame shows. canvas is reused: it is valid only during the call. A still
// WebP gives errStill.
func decodeAnimatedWebP(data []byte, each func(canvas *image.NRGBA, delay time.Duration) error) error {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return errors.New("ui: not a WebP file")
	}
	var canvas *image.NRGBA
	var clear image.Rectangle // the last frame's area, when it is disposed of before the next
	frames := 0
	for p := 12; p+8 <= len(data) && frames < maxAnimFrames; {
		id, n := string(data[p:p+4]), int(binary.LittleEndian.Uint32(data[p+4:]))
		body := p + 8
		if n < 0 || body+n > len(data) {
			return errors.New("ui: truncated WebP")
		}
		chunk := data[body : body+n]
		switch id {
		case "VP8X":
			if n < 10 {
				return errors.New("ui: bad WebP header")
			}
			if chunk[0]&0x02 == 0 {
				return errStill
			}
			w, h := 1+u24(chunk[4:]), 1+u24(chunk[7:])
			if w*h > maxAnimPixels {
				return fmt.Errorf("ui: refusing to animate a %dx%d image", w, h)
			}
			canvas = image.NewNRGBA(image.Rect(0, 0, w, h))
		case "ANMF":
			if canvas == nil || n < 16 {
				return errors.New("ui: bad WebP frame")
			}
			x, y, w, h := 2*u24(chunk), 2*u24(chunk[3:]), 1+u24(chunk[6:]), 1+u24(chunk[9:])
			delay, flags := time.Duration(u24(chunk[12:]))*time.Millisecond, chunk[15]
			img, err := decodeWebPFrame(chunk[16:], w, h)
			if err != nil {
				return err
			}
			if !clear.Empty() {
				draw.Draw(canvas, clear, image.Transparent, image.Point{}, draw.Src)
				clear = image.Rectangle{}
			}
			r, op := image.Rect(x, y, x+w, y+h), draw.Over
			if flags&0x02 != 0 { // do not blend: the frame replaces what is under it
				op = draw.Src
			}
			draw.Draw(canvas, r, img, img.Bounds().Min, op)
			if flags&0x01 != 0 {
				clear = r
			}
			if delay < minFrameDelay { // as browsers do: "0" means "the default", not "as fast as you can"
				delay = 100 * time.Millisecond
			}
			if err := each(canvas, delay); err != nil {
				return err
			}
			frames++
		}
		p = body + n + n&1 // chunks are padded to an even size
	}
	if frames == 0 {
		return errStill
	}
	return nil
}

// decodeWebPFrame decodes one frame's data (an optional ALPH chunk and a VP8 or VP8L chunk) by
// wrapping it as the still WebP file it would be on its own.
func decodeWebPFrame(sub []byte, w, h int) (image.Image, error) {
	var alpha, bits []byte
	for p := 0; p+8 <= len(sub); {
		n := int(binary.LittleEndian.Uint32(sub[p+4:]))
		end := p + 8 + n + n&1
		if n < 0 || p+8+n > len(sub) {
			return nil, errors.New("ui: truncated WebP frame")
		}
		end = min(end, len(sub))
		switch string(sub[p : p+4]) {
		case "ALPH":
			alpha = sub[p:end]
		case "VP8 ", "VP8L":
			bits = sub[p:end]
		}
		p = end
	}
	if bits == nil {
		return nil, errors.New("ui: WebP frame without a picture")
	}
	var body bytes.Buffer
	body.WriteString("WEBP")
	if alpha != nil {
		head := []byte{'V', 'P', '8', 'X', 10, 0, 0, 0, 0x10, 0, 0, 0, byte(w - 1), byte((w - 1) >> 8), byte((w - 1) >> 16), byte(h - 1), byte((h - 1) >> 8), byte((h - 1) >> 16)}
		body.Write(head)
		body.Write(alpha)
		if len(alpha)&1 != 0 {
			body.WriteByte(0)
		}
	}
	body.Write(bits)
	if len(bits)&1 != 0 {
		body.WriteByte(0)
	}
	file := make([]byte, 8, 8+body.Len())
	copy(file, "RIFF")
	binary.LittleEndian.PutUint32(file[4:], uint32(body.Len()))
	return webp.Decode(bytes.NewReader(append(file, body.Bytes()...)))
}

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

// newWebPFrames decodes the animated sticker at path into frames w x h pixels, its transparent
// parts filled with bg (the bubble's colour). A still sticker gives errStill.
func newWebPFrames(path string, w, h int, bg walk.Color) (*loopFrames, error) {
	if st, err := os.Stat(path); err != nil {
		return nil, err
	} else if st.Size() > maxAnimFile {
		return nil, fmt.Errorf("ui: sticker of %d bytes is too big to animate", st.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	l := &loopFrames{w: w, h: h}
	scaled := image.NewRGBA(image.Rect(0, 0, w, h))
	err = decodeAnimatedWebP(data, func(canvas *image.NRGBA, show time.Duration) error {
		for i := 0; i < len(scaled.Pix); i += 4 {
			scaled.Pix[i], scaled.Pix[i+1], scaled.Pix[i+2], scaled.Pix[i+3] = bg.R(), bg.G(), bg.B(), 0xff
		}
		draw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), canvas, canvas.Bounds(), draw.Over, nil)
		pix := bytes.Clone(scaled.Pix)
		for i := 0; i < len(pix); i += 4 { // RGBA to the BGRX GDI takes
			pix[i], pix[i+2] = pix[i+2], pix[i]
		}
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
