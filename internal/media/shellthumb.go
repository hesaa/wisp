package media

import (
	"fmt"
	"image"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Thumbnails Windows makes: of video (and GIFs, which WhatsApp sends as MP4), which this app has no
// decoder for. It is the picture Explorer shows for the file, from the codecs Windows ships.

var (
	shell32                      = windows.NewLazySystemDLL("shell32.dll")
	ole32                        = windows.NewLazySystemDLL("ole32.dll")
	gdi32                        = windows.NewLazySystemDLL("gdi32.dll")
	user32                       = windows.NewLazySystemDLL("user32.dll")
	pSHCreateItemFromParsingName = shell32.NewProc("SHCreateItemFromParsingName")
	pCoInitializeEx              = ole32.NewProc("CoInitializeEx")
	pCoUninitialize              = ole32.NewProc("CoUninitialize")
	pGetObject                   = gdi32.NewProc("GetObjectW")
	pGetDIBits                   = gdi32.NewProc("GetDIBits")
	pDeleteObject                = gdi32.NewProc("DeleteObject")
	pGetDC                       = user32.NewProc("GetDC")
	pReleaseDC                   = user32.NewProc("ReleaseDC")

	iidShellItemImageFactory = windows.GUID{Data1: 0xbcc18b79, Data2: 0xba16, Data3: 0x442f, Data4: [8]byte{0x80, 0xc4, 0x8a, 0x59, 0xc3, 0x0c, 0x46, 0x3b}}
)

// comCall calls method slot of COM object obj. The directive makes pointers converted to uintptr in
// the arguments escape to the heap and stay alive for the call.
//
//go:uintptrescapes
func comCall(obj unsafe.Pointer, slot int, args ...uintptr) uintptr {
	vt := *(**[8]uintptr)(obj)
	hr, _, _ := syscall.SyscallN(vt[slot], append([]uintptr{uintptr(obj)}, args...)...)
	return hr
}

// shellThumbnail is Windows' thumbnail of the file at path, fitted within side x side pixels. It
// fails for a file Windows has no picture of (it never returns the file type's icon).
func shellThumbnail(path string, side int) (image.Image, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return nil, err
	}
	runtime.LockOSThread() // COM is set up per thread
	defer runtime.UnlockOSThread()
	if hr, _, _ := pCoInitializeEx.Call(0, 2); int32(hr) >= 0 { // apartment threaded; a failure means the thread already has COM
		defer pCoUninitialize.Call()
	}
	var factory unsafe.Pointer
	if hr, _, _ := pSHCreateItemFromParsingName.Call(uintptr(unsafe.Pointer(name)), 0,
		uintptr(unsafe.Pointer(&iidShellItemImageFactory)), uintptr(unsafe.Pointer(&factory))); int32(hr) < 0 {
		return nil, fmt.Errorf("media: shell item of %s: 0x%08x", filepath.Base(abs), uint32(hr))
	}
	defer comCall(factory, 2) // Release
	var hbmp uintptr
	const thumbnailOnly = 0x8 // SIIGBF_THUMBNAILONLY
	// GetImage takes the size {cx, cy} by value: an 8-byte struct, one register
	if hr := comCall(factory, 3, uintptr(uint32(side))|uintptr(uint32(side))<<32, thumbnailOnly, uintptr(unsafe.Pointer(&hbmp))); int32(hr) < 0 {
		return nil, fmt.Errorf("media: Windows has no thumbnail of %s: 0x%08x", filepath.Base(abs), uint32(hr))
	}
	defer pDeleteObject.Call(hbmp)

	var bm struct {
		Type, Width, Height, WidthBytes int32
		Planes, BitsPixel               uint16
		Bits                            uintptr
	}
	if n, _, _ := pGetObject.Call(hbmp, unsafe.Sizeof(bm), uintptr(unsafe.Pointer(&bm))); n == 0 || bm.Width <= 0 || bm.Height <= 0 {
		return nil, fmt.Errorf("media: thumbnail of %s has no size", filepath.Base(abs))
	}
	w, h := int(bm.Width), int(bm.Height)
	header := struct { // BITMAPINFOHEADER: 32-bit pixels, top row first
		Size          uint32
		Width, Height int32
		Planes, Bits  uint16
		Compression   uint32
		_             [20]byte
	}{Size: 40, Width: bm.Width, Height: -bm.Height, Planes: 1, Bits: 32}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	dc, _, _ := pGetDC.Call(0)
	n, _, _ := pGetDIBits.Call(dc, hbmp, 0, uintptr(h), uintptr(unsafe.Pointer(&img.Pix[0])), uintptr(unsafe.Pointer(&header)), 0)
	pReleaseDC.Call(0, dc)
	if n == 0 {
		return nil, fmt.Errorf("media: reading the thumbnail of %s failed", filepath.Base(abs))
	}
	for i := 0; i < len(img.Pix); i += 4 { // BGRA to opaque RGBA
		img.Pix[i], img.Pix[i+2], img.Pix[i+3] = img.Pix[i+2], img.Pix[i], 0xff
	}
	return img, nil
}
