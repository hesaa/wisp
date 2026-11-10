package icons

import (
	"testing"

	"github.com/lxn/walk"
)

var allIcons = []Icon{IconChat, IconStatus, IconSettings, IconSearch, IconAttach, IconEmoji, IconSend, IconDoubleCheck}

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
