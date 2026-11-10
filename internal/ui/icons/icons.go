// Package icons draws monochrome Segoe Fluent-style line icons with
// anti-aliased GDI+ strokes. Icons are stroke data on a 24x24 grid.
//
// To add an icon: add a constant, then add an entry to definitions built
// from poly / ellipse / arc ops (coordinates in 0..24 grid units).
package icons

import (
	"fmt"
	"image"
	"math"
	"runtime"
	"unsafe"

	"github.com/lxn/walk"
)

type Icon int

const (
	IconChat Icon = iota + 1
	IconStatus
	IconSettings
	IconSearch
	IconAttach
	IconEmoji
	IconSend
	IconDoubleCheck
)

const (
	grid        = 24
	strokeUnits = 1.5
)

type opKind int

const (
	opPoly    opKind = iota // v: x0,y0,x1,y1,...
	opEllipse               // v: x,y,w,h (bounding box)
	opArc                   // v: x,y,w,h,startDeg,sweepDeg (clockwise from +x)
)

type op struct {
	kind opKind
	v    []float32
}

func poly(v ...float32) op    { return op{opPoly, v} }
func ellipse(v ...float32) op { return op{opEllipse, v} }
func arc(v ...float32) op     { return op{opArc, v} }

// gear returns a closed 8-tooth gear outline centred on the grid.
func gear() op {
	var v []float32
	const teeth, ro, ri = 8, 9.5, 7.5
	for i := 0; i < teeth; i++ {
		a := float64(i) * 2 * math.Pi / teeth
		for _, p := range [][2]float64{{-0.34, ri}, {-0.2, ro}, {0.2, ro}, {0.34, ri}} {
			t := a + p[0]*math.Pi*2/teeth
			v = append(v, float32(12+p[1]*math.Cos(t)), float32(12+p[1]*math.Sin(t)))
		}
	}
	return poly(append(v, v[0], v[1])...)
}

var definitions = map[Icon][]op{
	IconChat: {poly(7, 3.5, 17, 3.5, 20.5, 7, 20.5, 13, 17, 16.5, 12, 16.5, 7.5, 20.5, 7.5, 16.5, 7, 16.5, 3.5, 13, 3.5, 7, 7, 3.5)},
	IconStatus: {
		arc(3, 3, 18, 18, -80, 100),
		arc(3, 3, 18, 18, 40, 100),
		arc(3, 3, 18, 18, 160, 100),
	},
	IconSettings: {gear(), ellipse(9, 9, 6, 6)},
	IconSearch:   {ellipse(3.5, 3.5, 12, 12), poly(14, 14, 20.5, 20.5)},
	IconAttach: {
		arc(8, 12.5, 8, 8, 0, 180),
		poly(8, 16.5, 8, 7),
		arc(8, 3.5, 7, 7, 180, 180),
		poly(15, 7, 15, 15.5),
		arc(9, 12.5, 6, 6, 0, 180),
		poly(9, 15.5, 9, 8.5),
	},
	IconEmoji: {
		ellipse(3, 3, 18, 18),
		poly(9, 9.5, 9, 10),
		poly(15, 9.5, 15, 10),
		arc(7, 7, 10, 10, 35, 110),
	},
	IconSend: {
		poly(21, 3, 3, 10.5, 9.5, 13.5, 21, 3),
		poly(9.5, 13.5, 10, 20, 13.5, 15.5),
	},
	IconDoubleCheck: {
		poly(2, 12.5, 6.5, 17, 15, 7),
		poly(10.5, 15.5, 12, 17, 21.5, 7),
	},
}

func argb(c walk.Color) uint32 {
	return 0xFF000000 | uint32(c.R())<<16 | uint32(c.G())<<8 | uint32(c.B())
}

// render strokes the icon onto GDI+ graphics g, fitted and centred in b.
func render(g uintptr, id Icon, b walk.Rectangle, c walk.Color) error {
	ops, ok := definitions[id]
	if !ok {
		return fmt.Errorf("icons: unknown icon %d", id)
	}
	if err := call(pSetSmoothingMode, g, smoothingAntiAlias); err != nil {
		return err
	}
	size := b.Width
	if b.Height < size {
		size = b.Height
	}
	s := float32(size) / grid
	ox := float32(b.X) + (float32(b.Width)-float32(size))/2
	oy := float32(b.Y) + (float32(b.Height)-float32(size))/2
	pen, err := newPen(argb(c), strokeUnits*s)
	if err != nil {
		return err
	}
	defer call(pDeletePen, pen)

	for _, o := range ops {
		var err error
		switch o.kind {
		case opPoly:
			pts := make([]gpPoint, len(o.v)/2)
			for i := range pts {
				pts[i] = gpPoint{ox + o.v[2*i]*s, oy + o.v[2*i+1]*s}
			}
			err = call(pDrawLines, g, pen, uintptr(unsafe.Pointer(&pts[0])), uintptr(len(pts)))
		case opEllipse:
			err = call(pDrawEllipse, g, pen, f32(ox+o.v[0]*s), f32(oy+o.v[1]*s), f32(o.v[2]*s), f32(o.v[3]*s))
		case opArc:
			err = call(pDrawArc, g, pen, f32(ox+o.v[0]*s), f32(oy+o.v[1]*s), f32(o.v[2]*s), f32(o.v[3]*s), f32(o.v[4]), f32(o.v[5]))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Draw strokes the icon in colour c onto canvas, fitted into bounds (pixels).
func Draw(c *walk.Canvas, id Icon, bounds walk.Rectangle, color walk.Color) error {
	if _, ok := definitions[id]; !ok {
		return fmt.Errorf("icons: unknown icon %d", id)
	}
	if bounds.Width <= 0 || bounds.Height <= 0 {
		return fmt.Errorf("icons: empty bounds")
	}
	if err := startup(); err != nil {
		return err
	}
	var g uintptr
	if err := call(pCreateFromHDC, uintptr(c.HDC()), uintptr(unsafe.Pointer(&g))); err != nil {
		return err
	}
	defer call(pDeleteGraphics, g)
	return render(g, id, bounds, color)
}

// Bitmap returns a sizePx x sizePx bitmap of the icon on a transparent background.
func Bitmap(id Icon, sizePx int, color walk.Color) (*walk.Bitmap, error) {
	if _, ok := definitions[id]; !ok {
		return nil, fmt.Errorf("icons: unknown icon %d", id)
	}
	if sizePx <= 0 {
		return nil, fmt.Errorf("icons: invalid size %d", sizePx)
	}
	if err := startup(); err != nil {
		return nil, err
	}
	// GDI+ draws straight-alpha BGRA into our own buffer; walk's HBITMAP
	// conversion wants premultiplied, which we do afterwards.
	buf := make([]byte, sizePx*sizePx*4)
	var bmp uintptr
	if err := call(pCreateBitmapScan0, uintptr(sizePx), uintptr(sizePx), uintptr(sizePx*4), pixelFormat32ARGB, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&bmp))); err != nil {
		return nil, err
	}
	defer call(pDisposeImage, bmp)
	var g uintptr
	if err := call(pGetImageGraphics, bmp, uintptr(unsafe.Pointer(&g))); err != nil {
		return nil, err
	}
	err := render(g, id, walk.Rectangle{Width: sizePx, Height: sizePx}, color)
	if derr := call(pDeleteGraphics, g); err == nil {
		err = derr
	}
	if err != nil {
		return nil, err
	}
	runtime.KeepAlive(buf)

	img := image.NewRGBA(image.Rect(0, 0, sizePx, sizePx))
	for i := 0; i < len(buf); i += 4 {
		a := uint32(buf[i+3])
		img.Pix[i] = byte(uint32(buf[i+2]) * a / 255)
		img.Pix[i+1] = byte(uint32(buf[i+1]) * a / 255)
		img.Pix[i+2] = byte(uint32(buf[i]) * a / 255)
		img.Pix[i+3] = byte(a)
	}
	return walk.NewBitmapFromImageForDPI(img, 96)
}
