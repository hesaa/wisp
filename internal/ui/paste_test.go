package ui

import (
	"image/png"
	"os"
	"slices"
	"testing"
	"unsafe"

	"github.com/lxn/win"
)

func TestAttachCheckBlocksWhatCannotBeSent(t *testing.T) {
	for _, name := range []string{"photo.jpg", "Report.PDF", "clip.mp4", "notes.txt", "archive.zip", "README", "sheet.xlsx"} {
		if why := attachCheck(name, 1234, false); why != "" {
			t.Errorf("%s is blocked: %s", name, why)
		}
	}
	for _, name := range []string{"setup.exe", "SETUP.EXE", "run.bat", "x.ps1", "a.msi", "s.lnk", "lib.dll", "m.vbs", "x.cmd", "s.scr", "app.jar"} {
		if attachCheck(name, 1234, false) == "" {
			t.Errorf("%s is not blocked", name)
		}
	}
	if attachCheck("Photos", 0, true) == "" {
		t.Error("a folder is not blocked")
	}
	if attachCheck("empty.txt", 0, false) == "" {
		t.Error("an empty file is not blocked")
	}
	if attachCheck("huge.zip", maxAttachBytes+1, false) == "" {
		t.Error("a file over the size limit is not blocked")
	}
	if attachCheck("big.zip", maxAttachBytes, false) != "" {
		t.Error("a file at the size limit is blocked")
	}
}

func TestSendablePathsKeepOrderAndLeaveBlockedOut(t *testing.T) {
	items := []attachItem{{path: "a.jpg"}, {path: "b.exe", blocked: "x"}, {path: "c.pdf"}}
	if got := sendablePaths(items); !slices.Equal(got, []string{"a.jpg", "c.pdf"}) {
		t.Fatalf("sendablePaths = %v", got)
	}
	for n, want := range map[[2]int]string{{1, 0}: "Send 1 file", {3, 0}: "Send 3 files", {2, 1}: "Send 2 files", {0, 2}: "Nothing to send"} {
		if got := attachTitle(n[0], n[1]); got != want {
			t.Errorf("attachTitle(%d, %d) = %q, want %q", n[0], n[1], got, want)
		}
	}
}

func TestPastedBitmapIsSavedAsAPNG(t *testing.T) {
	const w, h = 8, 4
	hdc := win.CreateCompatibleDC(0)
	defer win.DeleteDC(hdc)
	var bi win.BITMAPINFO
	bi.BmiHeader = win.BITMAPINFOHEADER{BiSize: uint32(unsafe.Sizeof(bi.BmiHeader)), BiWidth: w, BiHeight: -h, BiPlanes: 1, BiBitCount: 32, BiCompression: win.BI_RGB}
	var bits unsafe.Pointer
	bmp := win.CreateDIBSection(hdc, &bi.BmiHeader, win.DIB_RGB_COLORS, &bits, 0, 0)
	if bmp == 0 {
		t.Fatal("CreateDIBSection failed")
	}
	defer win.DeleteObject(win.HGDIOBJ(bmp))
	px := unsafe.Slice((*byte)(bits), w*h*4)
	for i := 0; i < len(px); i += 4 {
		px[i], px[i+1], px[i+2] = 10, 20, 250 // BGR: red
	}
	px[0], px[1], px[2] = 250, 20, 10 // the top left pixel: blue

	path, err := saveBitmapPNG(bmp, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != w || b.Dy() != h {
		t.Fatalf("saved %v, want %dx%d", b, w, h)
	}
	r, g, b, a := img.At(0, 0).RGBA()
	if r>>8 != 10 || g>>8 != 20 || b>>8 != 250 || a>>8 != 255 {
		t.Fatalf("top left = %d,%d,%d,%d, want opaque blue (not upside down, not channel-swapped)", r>>8, g>>8, b>>8, a>>8)
	}
	if r, _, b, _ := img.At(5, 3).RGBA(); r>>8 != 250 || b>>8 != 10 {
		t.Fatalf("other pixel = r %d b %d, want red", r>>8, b>>8)
	}
}
