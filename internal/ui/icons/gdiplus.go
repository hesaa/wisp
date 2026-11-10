package icons

import (
	"fmt"
	"math"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Minimal GDI+ flat API binding. Float arguments are passed as raw bits: the
// Go runtime mirrors the first four syscall args into XMM0-3 on amd64.

var (
	gdip               = windows.NewLazySystemDLL("gdiplus.dll")
	pStartup           = gdip.NewProc("GdiplusStartup")
	pCreateFromHDC     = gdip.NewProc("GdipCreateFromHDC")
	pGetImageGraphics  = gdip.NewProc("GdipGetImageGraphicsContext")
	pCreateBitmapScan0 = gdip.NewProc("GdipCreateBitmapFromScan0")
	pDisposeImage      = gdip.NewProc("GdipDisposeImage")
	pDeleteGraphics    = gdip.NewProc("GdipDeleteGraphics")
	pSetSmoothingMode  = gdip.NewProc("GdipSetSmoothingMode")
	pCreatePen1        = gdip.NewProc("GdipCreatePen1")
	pDeletePen         = gdip.NewProc("GdipDeletePen")
	pSetPenStartCap    = gdip.NewProc("GdipSetPenStartCap")
	pSetPenEndCap      = gdip.NewProc("GdipSetPenEndCap")
	pSetPenLineJoin    = gdip.NewProc("GdipSetPenLineJoin")
	pDrawLines         = gdip.NewProc("GdipDrawLines")
	pDrawEllipse       = gdip.NewProc("GdipDrawEllipse")
	pDrawArc           = gdip.NewProc("GdipDrawArc")
	startupOnce        sync.Once
	startupErr         error
)

const (
	smoothingAntiAlias = 4
	lineCapRound       = 2
	lineJoinRound      = 2
	unitWorld          = 0
	pixelFormat32ARGB  = 0x26200A
)

func f32(v float32) uintptr { return uintptr(math.Float32bits(v)) }

// call invokes a GDI+ proc and converts a non-zero Status into an error.
func call(p *windows.LazyProc, args ...uintptr) error {
	r, _, _ := p.Call(args...)
	if r != 0 {
		return fmt.Errorf("icons: %s failed with GDI+ status %d", p.Name, r)
	}
	return nil
}

func startup() error {
	startupOnce.Do(func() {
		in := struct {
			version        uint32
			debugCallback  uintptr
			suppressThread int32
			suppressCodecs int32
		}{version: 1}
		var token uintptr
		// Never shut down: GDI+ lives for the process lifetime.
		startupErr = call(pStartup, uintptr(unsafe.Pointer(&token)), uintptr(unsafe.Pointer(&in)), 0)
	})
	return startupErr
}

type gpPoint struct{ x, y float32 }

func newPen(argb uint32, width float32) (uintptr, error) {
	var pen uintptr
	if err := call(pCreatePen1, uintptr(argb), f32(width), unitWorld, uintptr(unsafe.Pointer(&pen))); err != nil {
		return 0, err
	}
	for _, p := range []*windows.LazyProc{pSetPenStartCap, pSetPenEndCap} {
		if err := call(p, pen, lineCapRound); err != nil {
			call(pDeletePen, pen)
			return 0, err
		}
	}
	if err := call(pSetPenLineJoin, pen, lineJoinRound); err != nil {
		call(pDeletePen, pen)
		return 0, err
	}
	return pen, nil
}
