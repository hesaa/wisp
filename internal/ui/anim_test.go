package ui

import (
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"os"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"

	"wa-go/internal/media"
)

// testdata/anim.webp: 8x4, three frames (40, 120, 80 ms). All red; then a green square appears on
// the right (the file stores only that square); then everything is transparent but a blue dot.
func TestAnimatedWebPFramesAreComposited(t *testing.T) {
	data, err := os.ReadFile("testdata/anim.webp")
	if err != nil {
		t.Fatal(err)
	}
	type shot struct {
		left, right color.NRGBA
		delay       time.Duration
	}
	var got []shot
	err = media.DecodeAnimatedWebP(data, func(canvas *image.NRGBA, delay time.Duration) error {
		if b := canvas.Bounds(); b.Dx() != 8 || b.Dy() != 4 {
			t.Fatalf("canvas %v, want 8x4", b)
		}
		got = append(got, shot{canvas.NRGBAAt(0, 0), canvas.NRGBAAt(5, 1), delay})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	red, green, blue, none := color.NRGBA{255, 0, 0, 255}, color.NRGBA{0, 255, 0, 255}, color.NRGBA{0, 0, 255, 255}, color.NRGBA{}
	want := []shot{{red, red, 40 * time.Millisecond}, {red, green, 120 * time.Millisecond}, {blue, none, 80 * time.Millisecond}}
	if len(got) != len(want) {
		t.Fatalf("%d frames, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].right.A == 0 {
			got[i].right = none // colour under full transparency does not matter
		}
		if got[i] != want[i] {
			t.Errorf("frame %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	still, _ := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
	if err := media.DecodeAnimatedWebP(still, func(*image.NRGBA, time.Duration) error { return nil }); !errors.Is(err, errStill) {
		t.Fatalf("a still WebP: %v, want errStill", err)
	}
}

func TestPlayWebPLoopsUntilStopped(t *testing.T) {
	data, err := os.ReadFile("testdata/anim.webp")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var frames [][]byte
	err = playWebP(data, walk.RGB(10, 20, 30), stop, func(pix []byte) {
		if frames = append(frames, pix); len(frames) == 4 { // three frames, then the first again
			close(stop)
		}
	})
	if err != nil || len(frames) != 4 {
		t.Fatalf("playWebP: %v after %d frames", err, len(frames))
	}
	at := (1*8 + 5) * 4 // (5, 1), on the right; BGRX at the sticker's own 8x4
	// Red; then transparent, so the background's; then red again.
	for i, want := range map[int]byte{0: 255, 2: 10, 3: 255} {
		if got := frames[i][at+2]; got != want {
			t.Errorf("frame %d red %d, want %d", i, got, want)
		}
	}
	still, _ := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
	if err := playWebP(still, 0, stop, nil); !errors.Is(err, errStill) {
		t.Errorf("a still WebP: %v, want errStill", err)
	}
}

func TestStickerFramesAreScaledOverTheBubbleColour(t *testing.T) {
	src, err := newWebPFrames("testdata/anim.webp", 16, 8, walk.RGB(10, 20, 30))
	if err != nil {
		t.Fatal(err)
	}
	defer src.close()
	var last []byte
	for i := 0; i < 4; i++ { // three frames, then the first again
		pix, w, h, delay, err := src.next()
		if err != nil || w != 16 || h != 8 || len(pix) != 16*8*4 || delay <= 0 {
			t.Fatalf("frame %d: %dx%d, %d bytes, delay %v, err %v", i, w, h, len(pix), delay, err)
		}
		at := (4*16 + 12) * 4 // a pixel in the right half: BGRA
		switch i {
		case 0, 3:
			if pix[at] != 0 || pix[at+2] != 255 {
				t.Fatalf("frame %d right pixel BGRA %v, want red", i, pix[at:at+4])
			}
		case 2: // transparent there: the bubble colour shows
			if pix[at] != 30 || pix[at+1] != 20 || pix[at+2] != 10 {
				t.Fatalf("frame 2 right pixel BGRA %v, want the bubble colour", pix[at:at+4])
			}
		}
		last = pix
	}
	_ = last
}

// testdata/tiny.mp4: 64x48 H.264, 0.3 s of red then 0.3 s of blue at 10 frames a second, with a green
// box in the top left corner throughout.
func TestVideoFramesPlayAndLoop(t *testing.T) {
	runtime.LockOSThread() // Media Foundation is set up per thread
	defer runtime.UnlockOSThread()
	src, err := newVideoFrames("testdata/tiny.mp4")
	if err != nil {
		t.Fatalf("newVideoFrames: %v", err)
	}
	defer src.close()
	seq := ""
	for i := 0; i < 16; i++ {
		pix, w, h, delay, err := src.next()
		if err != nil || w != 64 || h != 48 || len(pix) < 64*48*4 {
			t.Fatalf("frame %d: %dx%d, %d bytes, err %v", i, w, h, len(pix), err)
		}
		if delay < 10*time.Millisecond || delay > 250*time.Millisecond {
			t.Fatalf("frame %d: delay %v", i, delay)
		}
		if corner := (4*64 + 4) * 4; pix[corner+1] < 150 || pix[corner] > 100 || pix[corner+2] > 100 {
			t.Fatalf("frame %d: top left pixel BGRA %v, want the green box (is the picture upside down?)", i, pix[corner:corner+4])
		}
		at := (24*64 + 32) * 4 // the centre, BGRA
		switch {
		case pix[at+2] > 150 && pix[at] < 100:
			seq += "r"
		case pix[at] > 150 && pix[at+2] < 100:
			seq += "b"
		default:
			seq += "?"
		}
	}
	if seq != "rrrbbbrrrbbbrrrb" {
		t.Fatalf("frames %q, want red then blue, looping", seq)
	}
}

func TestOnlyDownloadedStickersAndGIFsAnimate(t *testing.T) {
	for _, c := range []struct {
		typ, path string
		gif, want bool
	}{
		{"sticker", "c/s.webp", false, true}, {"sticker", "", false, false}, {"sticker", "c/s.png", false, false},
		{"video", "c/g.mp4", true, true}, {"video", "c/v.mp4", false, false}, {"video", "", true, false},
		{"image", "c/a.jpg", false, false},
	} {
		if got := animatable(c.typ, c.path, c.gif); got != c.want {
			t.Errorf("animatable(%s, %q, gif %v) = %v", c.typ, c.path, c.gif, got)
		}
	}
}

func TestBlitFrameCropsToTheBox(t *testing.T) {
	// a 4x2 frame: red left half, blue right half; drawn into a square box it loses its sides evenly
	pix := make([]byte, 4*2*4)
	for i := 0; i < 8; i++ {
		if i%4 < 2 {
			pix[i*4+2] = 255
		} else {
			pix[i*4] = 255
		}
	}
	const side = 40
	hdc := win.CreateCompatibleDC(0)
	defer win.DeleteDC(hdc)
	var bi win.BITMAPINFO
	bi.BmiHeader = win.BITMAPINFOHEADER{BiSize: uint32(unsafe.Sizeof(bi.BmiHeader)), BiWidth: side, BiHeight: -side, BiPlanes: 1, BiBitCount: 32, BiCompression: win.BI_RGB}
	var bits unsafe.Pointer
	bmp := win.CreateDIBSection(hdc, &bi.BmiHeader, win.DIB_RGB_COLORS, &bits, 0, 0)
	defer win.DeleteObject(win.HGDIOBJ(bmp))
	defer win.SelectObject(hdc, win.SelectObject(hdc, win.HGDIOBJ(bmp)))
	if err := blitFrame(hdc, walk.Rectangle{Width: side, Height: side}, pix, 4, 2); err != nil {
		t.Fatal(err)
	}
	win.GdiFlush()
	out := unsafe.Slice((*byte)(bits), side*side*4)
	left, right := (20*side+5)*4, (20*side+34)*4
	if out[left+2] < 200 || out[left] > 60 || out[right] < 200 || out[right+2] > 60 {
		t.Fatalf("left BGRA %v, right %v: want red and blue halves", out[left:left+4], out[right:right+4])
	}
}

// testdata/odd.mp4: 250x140 (not a multiple of 16, so the decoder pads its rows), red with a green
// box in the top left corner and a white one in the bottom right.
func TestVideoFramesOfAnOddWidthAreNotSheared(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	src, err := newVideoFrames("testdata/odd.mp4")
	if err != nil {
		t.Fatalf("newVideoFrames: %v", err)
	}
	defer src.close()
	pix, w, h, _, err := src.next()
	if err != nil || w != 250 || h != 140 || len(pix) != 250*140*4 {
		t.Fatalf("frame %dx%d, %d bytes, err %v", w, h, len(pix), err)
	}
	at := func(x, y int) []byte { return pix[(y*w+x)*4 : (y*w+x)*4+3] } // B, G, R
	if p := at(10, 10); p[1] < 150 || p[2] > 100 {
		t.Errorf("top left %v, want green", p)
	}
	if p := at(240, 130); p[0] < 180 || p[1] < 180 || p[2] < 180 {
		t.Errorf("bottom right %v, want white", p)
	}
	for _, xy := range [][2]int{{125, 70}, {10, 130}, {240, 10}, {125, 135}} {
		if p := at(xy[0], xy[1]); p[2] < 150 || p[1] > 100 || p[0] > 100 {
			t.Errorf("pixel %v = %v, want red", xy, p)
		}
	}
}

func TestRowStepOfPaddedPictures(t *testing.T) {
	for _, c := range [][4]int{
		{64 * 48 * 4, 64, 48, 256},      // as it says
		{250 * 140 * 4, 250, 140, 1000}, // cropped by the processor
		{256 * 144 * 4, 250, 140, 1024}, // the decoder's padded picture
		{100, 250, 140, 0},
	} {
		if got := rowStep(c[0], c[1], c[2]); got != c[3] {
			t.Errorf("rowStep(%d, %d, %d) = %d, want %d", c[0], c[1], c[2], got, c[3])
		}
	}
}
