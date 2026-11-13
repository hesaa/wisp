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

func TestStripTilesStayCentredOrFollowTheChosenFile(t *testing.T) {
	s := scaler(96)
	const width = 760
	tiles, send := stripTiles(width, 2, 0, s)
	if len(tiles) != 3 {
		t.Fatalf("2 files gave %d tiles, want 3 (the last adds more)", len(tiles))
	}
	if left, right := tiles[0].X, width-(tiles[2].X+tiles[2].Width); left != right {
		t.Errorf("tiles not centred: %d left, %d right", left, right)
	}
	if send.X+send.Width > width || send.X < tiles[2].X+tiles[2].Width {
		t.Errorf("Send button %v overlaps the tiles or leaves the strip", send)
	}
	if x := removeBox(tiles[1], s); x.Y < 0 || x.X+x.Width > tiles[2].X+tiles[2].Width/2 || x.X+x.Width <= tiles[1].X+tiles[1].Width-s(18) {
		t.Errorf("remove button %v is off the strip or off its tile's corner %v", x, tiles[1])
	}
	for _, sel := range []int{0, 17, 39} { // far more files than fit
		tiles, _ := stripTiles(width, 40, sel, s)
		if !tileShown(tiles[sel], width, s) {
			t.Errorf("with 40 files the chosen tile %d is out of sight at %v", sel, tiles[sel])
		}
	}
}

// The preview's stage and strip, painted onto bitmaps (a test binary cannot open walk dialogs: it has
// no visual-styles manifest). With WISP_PREVIEW_PNG set to a directory they are saved there, to look at.
func TestAttachPreviewPaints(t *testing.T) {
	dir := t.TempDir()
	pic := filepath.Join(dir, "holiday photo.png")
	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 640; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x / 3), G: 120, B: uint8(y / 2), A: 255})
		}
	}
	f, err := os.Create(pic)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	f.Close()
	for _, name := range []string{"README.md", "setup.exe", "clip.mp4"} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, 2048), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	items := []attachItem{newAttachItem(pic, false), newAttachItem(filepath.Join(dir, "README.md"), false),
		newAttachItem(filepath.Join(dir, "setup.exe"), false), newAttachItem(filepath.Join(dir, "clip.mp4"), false)}
	if items[2].blocked == "" || items[0].blocked != "" {
		t.Fatalf("blocked: %q, %q", items[0].blocked, items[2].blocked)
	}

	for _, th := range []theme.ThemeMode{theme.ThemeDark, theme.ThemeLight} {
		v := &attachPreview{k: &kit{pal: theme.GetPalette(th)}, items: items, thumbs: map[string]*walk.Bitmap{}}
		for sel := range items {
			v.sel = sel
			for part, size := range map[string]walk.Size{"stage": {Width: 760, Height: 470}, "strip": {Width: 760, Height: attachStripH}} {
				bmp, err := walk.NewBitmapForDPI(size, 96)
				if err != nil {
					t.Fatal(err)
				}
				c, err := walk.NewCanvasFromImage(bmp)
				if err != nil {
					t.Fatal(err)
				}
				r := walk.Rectangle{Width: size.Width, Height: size.Height}
				if part == "stage" {
					err = v.paintStage(c, r, 96)
				} else {
					err = v.paintStrip(c, r, 96, false)
				}
				c.Dispose()
				if err != nil {
					t.Fatalf("painting the %s for %s: %v", part, items[sel].name, err)
				}
				if out := os.Getenv("WISP_PREVIEW_PNG"); out != "" {
					savePNG(t, bmp, filepath.Join(out, map[theme.ThemeMode]string{theme.ThemeDark: "dark", theme.ThemeLight: "light"}[th]+"-"+part+"-"+items[sel].name+".png"))
				}
				bmp.Dispose()
			}
			if got := v.big != nil; got != (sel == 0) {
				t.Errorf("%s: has a large picture = %v", items[sel].name, got)
			}
		}
		if v.thumbs[pic] == nil {
			t.Error("the picture got no thumbnail")
		}
		if v.thumbs[items[1].path] != nil || v.thumbs[items[2].path] != nil {
			t.Error("a document or a blocked file got a thumbnail")
		}
		v.dispose()
	}
}

func savePNG(t *testing.T, bmp *walk.Bitmap, path string) {
	t.Helper()
	shot, err := bmp.ToImage()
	if err != nil {
		t.Logf("%s not saved: %v", path, err) // walk's ToImage fails now and then; the shot is only for looking at
		return
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, shot); err != nil {
		t.Fatal(err)
	}
}
