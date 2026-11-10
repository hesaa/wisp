package icons

import (
	"testing"

	"github.com/lxn/walk"
)

var allIcons = []Icon{IconChat, IconStatus, IconSettings, IconSearch, IconAttach, IconEmoji, IconSend, IconDoubleCheck,
	IconAdd, IconNewChat, IconCheck, IconDocument, IconDownload, IconVideo, IconAudio}

func TestFillCapsule(t *testing.T) {
	bmp, err := walk.NewBitmapForDPI(walk.Size{Width: 40, Height: 20}, 96)
	if err != nil {
		t.Fatal(err)
	}
	defer bmp.Dispose()
	c, err := walk.NewCanvasFromImage(bmp)
	if err != nil {
		t.Fatal(err)
	}
	if Fill(c, walk.Rectangle{Width: 0, Height: 5}, 0) == nil {
		t.Error("empty bounds accepted")
	}
	err = Fill(c, walk.Rectangle{Width: 40, Height: 20}, walk.RGB(200, 0, 0))
	c.Dispose()
	if err != nil {
		t.Fatal(err)
	}
	img, err := bmp.ToImage()
	if err != nil {
		t.Fatal(err)
	}
	red := func(x, y int) bool { p := img.RGBAAt(x, y); return p.R == 200 && p.G == 0 && p.B == 0 }
	if !red(20, 10) || !red(3, 10) || !red(36, 10) || !red(20, 0) {
		t.Error("capsule body not filled")
	}
	if red(0, 0) || red(39, 19) || red(0, 19) {
		t.Error("capsule corners filled: not rounded")
	}
}

func TestDefinitionsExistAndStayOnGrid(t *testing.T) {
	for _, id := range allIcons {
		ops, ok := definitions[id]
		if !ok || len(ops) == 0 {
			t.Fatalf("icon %d has no definition", id)
		}
		for _, o := range ops {
			coords := o.v
			if o.kind == opArc {
				coords = o.v[:4] // last two are angles in degrees
			}
			for i, v := range coords {
				if v < 0 || v > grid {
					t.Errorf("icon %d: coordinate %d = %v outside 0..%d", id, i, v, grid)
				}
			}
			if o.kind == opEllipse || o.kind == opArc {
				if o.v[0]+o.v[2] > grid || o.v[1]+o.v[3] > grid {
					t.Errorf("icon %d: ellipse bounds exceed grid", id)
				}
			}
		}
	}
}

func TestBitmapRendersSomethingButNotEverything(t *testing.T) {
	for _, id := range allIcons {
		bmp, err := Bitmap(id, 48, walk.RGB(0, 0, 0))
		if err != nil {
			t.Fatalf("icon %d: %v", id, err)
		}
		img, err := bmp.ToImage()
		bmp.Dispose()
		if err != nil {
			t.Fatalf("icon %d: ToImage: %v", id, err)
		}
		opaque, clear := 0, 0
		for i := 3; i < len(img.Pix); i += 4 {
			switch img.Pix[i] {
			case 0:
				clear++
			case 255:
				opaque++
			}
		}
		if opaque == 0 || clear == 0 {
			t.Errorf("icon %d: opaque=%d transparent=%d", id, opaque, clear)
		}
	}
}

func TestUnknownIcon(t *testing.T) {
	if _, err := Bitmap(Icon(999), 24, 0); err == nil {
		t.Fatal("expected error for unknown icon")
	}
}

func TestFillRounded(t *testing.T) {
	bmp, err := walk.NewBitmapForDPI(walk.Size{Width: 60, Height: 30}, 96)
	if err != nil {
		t.Fatal(err)
	}
	defer bmp.Dispose()
	c, err := walk.NewCanvasFromImage(bmp)
	if err != nil {
		t.Fatal(err)
	}
	err = FillRounded(c, walk.Rectangle{Width: 60, Height: 30}, 8, walk.RGB(0, 200, 0))
	c.Dispose()
	if err != nil {
		t.Fatal(err)
	}
	img, err := bmp.ToImage()
	if err != nil {
		t.Fatal(err)
	}
	green := func(x, y int) bool { p := img.RGBAAt(x, y); return p.R == 0 && p.G == 200 && p.B == 0 }
	// straight edges reach the bounds (unlike a capsule's), corners are cut
	for _, p := range [][2]int{{30, 0}, {30, 29}, {0, 15}, {59, 15}, {8, 2}, {3, 8}, {56, 26}} {
		if !green(p[0], p[1]) {
			t.Errorf("(%d,%d) not filled", p[0], p[1])
		}
	}
	for _, p := range [][2]int{{0, 0}, {59, 0}, {0, 29}, {59, 29}} {
		if green(p[0], p[1]) {
			t.Errorf("corner (%d,%d) filled: not rounded", p[0], p[1])
		}
	}
}
