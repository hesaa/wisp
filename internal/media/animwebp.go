package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"time"

	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

// Animated WebP. x/image/webp decodes only still WebP, so the animation container is read here:
// each frame in it is a still WebP bitstream, decoded by that package and composited onto the
// canvas as the format says (offset, blend, dispose).

// ErrStill is returned for an image that is not animated.
var ErrStill = errors.New("media: not an animated image")

const (
	maxAnimFrames = 240
	maxAnimPixels = 1 << 22 // canvas size refused above this
	minFrameDelay = 20 * time.Millisecond
)

func u24(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }

// DecodeAnimatedWebP calls each with the whole picture after every frame of the animated WebP in
// data, and how long that frame shows. canvas is reused: it is valid only during the call. A still
// WebP gives ErrStill.
func DecodeAnimatedWebP(data []byte, each func(canvas *image.NRGBA, delay time.Duration) error) error {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return errors.New("media: not a WebP file")
	}
	var canvas *image.NRGBA
	var clear image.Rectangle // the last frame's area, when it is disposed of before the next
	frames := 0
	for p := 12; p+8 <= len(data) && frames < maxAnimFrames; {
		id, n := string(data[p:p+4]), int(binary.LittleEndian.Uint32(data[p+4:]))
		body := p + 8
		if n < 0 || body+n > len(data) {
			return errors.New("media: truncated WebP")
		}
		chunk := data[body : body+n]
		switch id {
		case "VP8X":
			if n < 10 {
				return errors.New("media: bad WebP header")
			}
			if chunk[0]&0x02 == 0 {
				return ErrStill
			}
			w, h := 1+u24(chunk[4:]), 1+u24(chunk[7:])
			if w*h > maxAnimPixels {
				return fmt.Errorf("media: refusing to animate a %dx%d image", w, h)
			}
			canvas = image.NewNRGBA(image.Rect(0, 0, w, h))
		case "ANMF":
			if canvas == nil || n < 16 {
				return errors.New("media: bad WebP frame")
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
		return ErrStill
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
			return nil, errors.New("media: truncated WebP frame")
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
		return nil, errors.New("media: WebP frame without a picture")
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
	file = append(file, body.Bytes()...)
	// the picture's own header has its size: refused undecoded when huge, as decodeLimited does
	if cfg, err := webp.DecodeConfig(bytes.NewReader(file)); err != nil {
		return nil, err
	} else if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxAnimPixels {
		return nil, fmt.Errorf("media: refusing to decode a %dx%d WebP picture: %w", cfg.Width, cfg.Height, ErrImageTooLarge)
	}
	return webp.Decode(bytes.NewReader(file))
}

// errFirst stops DecodeAnimatedWebP after the first frame.
var errFirst = errors.New("media: first frame")

// FirstWebPFrame is the first picture of a WebP in data that x/image/webp alone cannot decode: an
// animation's (an animated sticker may hold only one frame), or a still in the extended (VP8X)
// format, which that package refuses when it is lossless and flagged as having alpha.
func FirstWebPFrame(data []byte) (*image.RGBA, error) {
	var first *image.RGBA
	err := DecodeAnimatedWebP(data, func(canvas *image.NRGBA, _ time.Duration) error {
		first = image.NewRGBA(canvas.Bounds())
		draw.Draw(first, first.Bounds(), canvas, canvas.Bounds().Min, draw.Src)
		return errFirst
	})
	if first != nil {
		return first, nil
	}
	if !errors.Is(err, ErrStill) || len(data) < 30 || string(data[12:16]) != "VP8X" {
		return nil, err
	}
	// the chunks after VP8X hold the picture (and maybe EXIF, ICCP or XMP, which are skipped)
	img, err := decodeWebPFrame(data[12:], 1+u24(data[24:]), 1+u24(data[27:]))
	if err != nil {
		return nil, err
	}
	still := image.NewRGBA(img.Bounds())
	draw.Draw(still, still.Bounds(), img, img.Bounds().Min, draw.Src)
	return still, nil
}
