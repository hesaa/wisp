package ui

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lxn/walk"

	"wa-go/internal/media"
	"wa-go/internal/store"
)

func TestMediaStepsSkipsNonMedia(t *testing.T) {
	msgs := []store.Message{{Type: "text"}, {Type: "image"}, {Type: "document"}, {Type: "video"}, {Type: "text"}}
	if p, n := mediaSteps(msgs, 1); p != -1 || n != 3 {
		t.Errorf("from 1: (%d, %d), want (-1, 3)", p, n)
	}
	if p, n := mediaSteps(msgs, 3); p != 1 || n != -1 {
		t.Errorf("from 3: (%d, %d), want (1, -1)", p, n)
	}
	stickers := []store.Message{{Type: "sticker"}, {Type: "audio"}, {Type: "image"}}
	if p, n := mediaSteps(stickers, 2); p != 0 || n != -1 {
		t.Errorf("stickers: (%d, %d), want (0, -1)", p, n)
	}
}

func TestFitRect(t *testing.T) {
	area := walk.Rectangle{Width: 800, Height: 400}
	cases := []struct {
		name string
		img  walk.Size
		zoom float64
		pan  walk.Point
		want walk.Rectangle
	}{
		{"fit, letterboxed", walk.Size{Width: 1600, Height: 1200}, 1, walk.Point{}, walk.Rectangle{X: 133, Y: 0, Width: 533, Height: 400}},
		{"small: no upscale", walk.Size{Width: 100, Height: 50}, 1, walk.Point{}, walk.Rectangle{X: 350, Y: 175, Width: 100, Height: 50}},
		{"fit ignores pan", walk.Size{Width: 1600, Height: 1200}, 1, walk.Point{X: 500, Y: 500}, walk.Rectangle{X: 133, Y: 0, Width: 533, Height: 400}},
		{"2x centred", walk.Size{Width: 1600, Height: 1200}, 2, walk.Point{}, walk.Rectangle{X: -133, Y: -200, Width: 1067, Height: 800}},
		{"2x pan clamped high", walk.Size{Width: 1600, Height: 1200}, 2, walk.Point{X: 1000, Y: 1000}, walk.Rectangle{X: 0, Y: 0, Width: 1067, Height: 800}},
		{"2x pan clamped low", walk.Size{Width: 1600, Height: 1200}, 2, walk.Point{X: -1000, Y: -1000}, walk.Rectangle{X: -267, Y: -400, Width: 1067, Height: 800}},
	}
	for _, c := range cases {
		if got := fitRect(c.img, area, c.zoom, c.pan); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestZoomAroundKeepsCursorPoint(t *testing.T) {
	area := walk.Rectangle{Width: 800, Height: 400}
	img := walk.Size{Width: 1600, Height: 1200}
	for _, cur := range []walk.Point{{X: 400, Y: 200}, {X: 300, Y: 100}} {
		shown := fitRect(img, area, 1, walk.Point{})
		// the image point under the cursor, in image pixels
		ix := float64(cur.X-shown.X) * float64(img.Width) / float64(shown.Width)
		iy := float64(cur.Y-shown.Y) * float64(img.Height) / float64(shown.Height)
		z, pan := zoomAround(1, 2, cur, shown, area)
		if z != 2 {
			t.Fatalf("zoom %v, want 2", z)
		}
		after := fitRect(img, area, z, pan)
		sx := float64(after.X) + ix*float64(after.Width)/float64(img.Width)
		sy := float64(after.Y) + iy*float64(after.Height)/float64(img.Height)
		if math.Abs(sx-float64(cur.X)) > 1.5 || math.Abs(sy-float64(cur.Y)) > 1.5 {
			t.Errorf("cursor %v: the point moved to (%.1f, %.1f)", cur, sx, sy)
		}
	}
	shown := fitRect(img, area, 1, walk.Point{})
	if z, _ := zoomAround(3, 2, walk.Point{X: 400, Y: 200}, shown, area); z != 4 {
		t.Errorf("zoom past 4: %v", z)
	}
	if z, pan := zoomAround(1.5, 0.5, walk.Point{X: 400, Y: 200}, shown, area); z != 1 || pan != (walk.Point{}) {
		t.Errorf("zoom below 1: %v, pan %v", z, pan)
	}
}

// Review focus: a photo whose cached file was removed (Clear Cache) asks for the download again.
func TestOpenMissingFileAsksDownload(t *testing.T) {
	dir := t.TempDir()
	kept := filepath.Join(dir, "kept.jpg")
	if err := os.WriteFile(kept, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path string
		want bool
	}{
		{"", true},
		{filepath.Join(dir, "cleared.jpg"), true},
		{kept, false},
	}
	for _, c := range cases {
		if got := needsDownload(store.Message{Type: "image", MediaPath: c.path}); got != c.want {
			t.Errorf("needsDownload(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestMediaWhen(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.Local)
	cases := map[time.Time]string{
		time.Date(2026, 11, 11, 8, 5, 0, 0, time.Local):   "Today at 08:05",
		time.Date(2026, 11, 10, 23, 59, 0, 0, time.Local): "Yesterday at 23:59",
		time.Date(2026, 10, 2, 15, 4, 0, 0, time.Local):   "02/10/2026 at 15:04",
	}
	for at, want := range cases {
		if got := mediaWhen(at.Unix(), now); got != want {
			t.Errorf("mediaWhen(%v) = %q, want %q", at, got, want)
		}
	}
}

func TestDecodeFull(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "a.png")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	img, err := decodeFull(p)
	if err != nil || img.Bounds().Dx() != 4 || img.Bounds().Dy() != 3 {
		t.Fatalf("decodeFull: %v, %v", img, err)
	}
	if _, err := decodeCapped(bytes.NewReader(buf.Bytes()), 11); !errors.Is(err, media.ErrImageTooLarge) {
		t.Errorf("over the cap: %v, want ErrImageTooLarge", err)
	}
}

func TestClickOpens(t *testing.T) {
	photo := bubble{thumb: walk.Rectangle{X: 10, Y: 10, Width: 200, Height: 150}, text: walk.Rectangle{X: 10, Y: 165, Width: 200, Height: 20}}
	card := bubble{card: walk.Rectangle{X: 10, Y: 10, Width: 240, Height: 60}}
	cases := []struct {
		name    string
		typ     string
		b       bubble
		x, y    int
		n       int
		mods    walk.Modifiers
		dragged bool
		want    bool
	}{
		{"plain click on the photo", "image", photo, 50, 50, 1, 0, false, true},
		{"plain click on a sticker", "sticker", photo, 50, 50, 1, 0, false, true},
		{"plain click on a video card", "video", card, 50, 30, 1, 0, false, true},
		{"ctrl picks", "image", photo, 50, 50, 1, walk.ModControl, false, false},
		{"shift picks", "image", photo, 50, 50, 1, walk.ModShift, false, false},
		{"caption text", "image", photo, 50, 170, 1, 0, false, false},
		{"outside the media", "image", photo, 230, 50, 1, 0, false, false},
		{"end of a drag", "image", photo, 50, 50, 1, 0, true, false},
		{"document waits for a double-click", "document", card, 50, 30, 1, 0, false, false},
		{"audio waits for a double-click", "audio", card, 50, 30, 1, 0, false, false},
		{"double-click on the photo opens once", "image", photo, 50, 50, 2, 0, false, false},
		{"double-click on a video card opens once", "video", card, 50, 30, 2, 0, false, false},
		{"double-click on a document", "document", card, 50, 30, 2, 0, false, true},
		{"double-click on audio", "audio", card, 50, 30, 2, 0, false, true},
		{"double-click beside a photo", "image", photo, 230, 50, 2, 0, false, true},
	}
	for _, c := range cases {
		if got := clickOpens(c.typ, c.b, c.x, c.y, c.n, c.mods, c.dragged); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
