package media

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// tinyWebP is a 1x1 lossless WebP.
const tinyWebP = "UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA=="

func TestThumbnailOfAStickerIsAPNG(t *testing.T) {
	c, _ := newCache(t)
	raw, err := base64.StdEncoding.DecodeString(tinyWebP)
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.Save("acc", ".webp", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	tp, err := c.Thumbnail(p)
	if err != nil {
		t.Fatalf("Thumbnail of a WebP: %v", err)
	}
	if filepath.Ext(tp) != ".png" { // PNG keeps the sticker's transparency
		t.Fatalf("thumbnail %q, want a PNG", tp)
	}
	f, err := os.Open(tp)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if cfg, format, err := image.DecodeConfig(f); err != nil || format != "png" || cfg.Width != 1 || cfg.Height != 1 {
		t.Fatalf("thumbnail is %s %dx%d (%v)", format, cfg.Width, cfg.Height, err)
	}
}

// webpChunk is a RIFF chunk: id, little-endian length, body, padded to an even size.
func webpChunk(id string, body []byte) []byte {
	b := append([]byte(id), byte(len(body)), byte(len(body)>>8), byte(len(body)>>16), byte(len(body)>>24))
	b = append(b, body...)
	if len(body)%2 == 1 {
		b = append(b, 0)
	}
	return b
}

// Stickers x/image/webp cannot decode still get a thumbnail: an animated WebP of a single frame,
// and a still in the extended format flagged as having alpha (and EXIF) over a lossless picture,
// which that package refuses although the format allows it.
func TestThumbnailOfStickersXImageRefuses(t *testing.T) {
	still, err := base64.StdEncoding.DecodeString(tinyWebP)
	if err != nil {
		t.Fatal(err)
	}
	vp8l := still[12:] // its VP8L chunk, padded
	for name, chunks := range map[string][][]byte{
		"one-frame animation": {
			webpChunk("VP8X", []byte{0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0}), // animated, 1x1
			webpChunk("ANIM", make([]byte, 6)),
			webpChunk("ANMF", append([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 100, 0, 0, 0}, vp8l...)),
		},
		"extended still with alpha and EXIF": {
			webpChunk("VP8X", []byte{0x18, 0, 0, 0, 0, 0, 0, 0, 0, 0}), // alpha and EXIF flags, 1x1
			vp8l,
			webpChunk("EXIF", []byte("II*\x00")),
		},
	} {
		body := []byte("WEBP")
		for _, c := range chunks {
			body = append(body, c...)
		}
		raw := append([]byte{'R', 'I', 'F', 'F', byte(len(body)), byte(len(body) >> 8), 0, 0}, body...)
		c, _ := newCache(t)
		p, err := c.Save("acc", ".webp", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		tp, err := c.Thumbnail(p)
		if err != nil {
			t.Errorf("%s: Thumbnail: %v", name, err)
			continue
		}
		f, err := os.Open(tp)
		if err != nil {
			t.Fatal(err)
		}
		cfg, format, err := image.DecodeConfig(f)
		f.Close()
		if err != nil || format != "png" || cfg.Width != 1 || cfg.Height != 1 {
			t.Errorf("%s: thumbnail is %s %dx%d (%v)", name, format, cfg.Width, cfg.Height, err)
		}
	}
}

// Windows makes the thumbnail of what this app cannot decode (video): any file type Explorer shows
// a picture for goes the same way, so a PNG stands in for a video here.
func TestShellThumbnailFitsTheSide(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 400, 200))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	for x := 0; x < 400; x++ {
		for y := 0; y < 200; y++ {
			img.Set(x, y, color.RGBA{200, 30, 30, 255})
		}
	}
	path := filepath.Join(t.TempDir(), "red.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	f.Close()
	got, err := shellThumbnail(path, 100)
	if err != nil {
		t.Fatalf("shellThumbnail: %v", err)
	}
	b := got.Bounds()
	if b.Dx() > 100 || b.Dy() > 100 || b.Dx() < 50 {
		t.Fatalf("thumbnail %dx%d, want it to fit 100x100", b.Dx(), b.Dy())
	}
	r, g, bl, _ := got.At(b.Dx()/2, b.Dy()/2).RGBA()
	if r>>8 < 150 || g>>8 > 90 || bl>>8 > 90 {
		t.Fatalf("centre pixel %d,%d,%d, want the picture's red", r>>8, g>>8, bl>>8)
	}
}
