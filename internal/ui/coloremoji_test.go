package ui

import (
	"testing"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
)

func TestHasEmoji(t *testing.T) {
	for s, want := range map[string]bool{
		"":              false,
		"hello @all":    false,
		"harga 5€ → ok": false,
		"hi 😀":          true,
		"🇮🇩":            true,
		"ok ❤️":         true, // BMP heart with the emoji selector
		"☕ break":       true,
	} {
		if got := hasEmoji(s); got != want {
			t.Errorf("hasEmoji(%q) = %v, want %v", s, got, want)
		}
	}
}

// renderText draws text with drawColorText on a w x h grey bitmap and returns its BGRA pixels.
func renderText(t *testing.T, text string, w, h int) []byte {
	t.Helper()
	hdc := win.CreateCompatibleDC(0)
	defer win.DeleteDC(hdc)
	var bi win.BITMAPINFO
	bi.BmiHeader = win.BITMAPINFOHEADER{BiSize: uint32(unsafe.Sizeof(bi.BmiHeader)), BiWidth: int32(w), BiHeight: -int32(h),
		BiPlanes: 1, BiBitCount: 32, BiCompression: win.BI_RGB}
	var bits unsafe.Pointer
	bmp := win.CreateDIBSection(hdc, &bi.BmiHeader, win.DIB_RGB_COLORS, &bits, 0, 0)
	if bmp == 0 {
		t.Fatal("CreateDIBSection failed")
	}
	defer win.DeleteObject(win.HGDIOBJ(bmp))
	defer win.SelectObject(hdc, win.SelectObject(hdc, win.HGDIOBJ(bmp)))
	px := unsafe.Slice((*byte)(bits), w*h*4)
	for i := range px {
		px[i] = 0x80
	}
	// as walk's buffered paint does for an update region away from the origin
	win.SetViewportOrgEx(hdc, -100, -50, nil)
	r := walk.Rectangle{X: 100, Y: 50, Width: w, Height: h}
	if err := drawColorText(hdc, text, float32(h)*0.6, 0, walk.RGB(0, 0, 0), r, centerText); err != nil {
		t.Fatalf("drawColorText: %v", err)
	}
	win.GdiFlush()
	return append([]byte(nil), px...)
}

func TestEmojiIsDrawnInColour(t *testing.T) {
	const w, h = 64, 64
	px := renderText(t, "😀", w, h)
	vivid := 0
	for i := 0; i < len(px); i += 4 {
		b, g, r := int(px[i]), int(px[i+1]), int(px[i+2])
		if max(r, g, b)-min(r, g, b) > 90 {
			vivid++
		}
	}
	if vivid < 100 {
		t.Fatalf("%d vivid pixels: the emoji was not drawn in colour", vivid)
	}
	if px[0] != 0x80 || px[1] != 0x80 || px[2] != 0x80 {
		t.Fatalf("corner pixel %v: what was painted under the text is gone", px[:4])
	}
}
