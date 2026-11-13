package ui

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/lxn/walk"

	"wa-go/internal/theme"
)

// The preview's list, painted onto a bitmap (a test binary cannot open walk dialogs: it has no
// visual-styles manifest). With WISP_PREVIEW_PNG set the painted list is saved there, to look at.
func TestAttachPreviewListPaints(t *testing.T) {
	dir := t.TempDir()
	pic := filepath.Join(dir, "holiday photo.png")
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for i := 0; i < 64*48; i++ {
		img.Set(i%64, i/64, color.RGBA{R: uint8(i % 64 * 4), G: 120, B: uint8(i / 64 * 5), A: 255})
	}
	f, err := os.Create(pic)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	f.Close()
	for _, name := range []string{"report.pdf", "setup.exe", "clip.mp4"} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, 2048), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	items := []attachItem{newAttachItem(pic, false), newAttachItem(filepath.Join(dir, "report.pdf"), false),
		newAttachItem(filepath.Join(dir, "setup.exe"), false), newAttachItem(filepath.Join(dir, "clip.mp4"), false)}
	if items[2].blocked == "" || items[0].blocked != "" {
		t.Fatalf("blocked: %q, %q", items[0].blocked, items[2].blocked)
	}

	list := &attachList{k: &kit{pal: theme.GetPalette(theme.ThemeDark)}, items: items, thumbs: map[int]*walk.Bitmap{}}
	defer list.dispose()
	const w, h = 420, 4 * attachRowH
	bmp, err := walk.NewBitmapForDPI(walk.Size{Width: w, Height: h}, 96)
	if err != nil {
		t.Fatal(err)
	}
	defer bmp.Dispose()
	c, err := walk.NewCanvasFromImage(bmp)
	if err != nil {
		t.Fatal(err)
	}
	err = list.paintRows(c, w, 96, walk.Rectangle{Width: w, Height: h})
	c.Dispose()
	if err != nil {
		t.Fatalf("painting the list: %v", err)
	}
	if list.thumbs[0] == nil {
		t.Error("the picture got no thumbnail")
	}
	if list.thumbs[2] != nil || list.thumbs[1] != nil {
		t.Error("a document or a blocked file got a thumbnail")
	}
	if out := os.Getenv("WISP_PREVIEW_PNG"); out != "" {
		shot, err := bmp.ToImage()
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(out)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, shot); err != nil {
			t.Fatal(err)
		}
	}
}
