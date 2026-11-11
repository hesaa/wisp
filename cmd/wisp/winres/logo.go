//go:build ignore

// logo renders the Wisp app icon, a teal speech bubble with a white rounded "w", into the PNGs that
// winres.json packs into the exe. Regenerate after changing it, from cmd/wisp/winres:
//
//	go run logo.go
//	cd .. && go tool go-winres make --arch amd64
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"math"
	"os"

	"golang.org/x/image/vector"
)

var (
	teal  = color.RGBA{0x00, 0xa8, 0x84, 0xff} // the app's accent (internal/theme)
	white = color.RGBA{0xff, 0xff, 0xff, 0xff}
)

// The sizes Windows picks from at 100-200% scaling: 16-32 small icons (title bar, tray), 32-64 large,
// 256 for Explorer's big views. Each is rendered at its own size, not scaled from another.
var sizes = []int{256, 64, 48, 40, 32, 24, 20, 16}

// The "w" stroke in the 96-unit design space.
var wPoints = [][2]float32{{26, 34}, {37, 58}, {48, 41}, {59, 58}, {70, 34}}

const wStroke = 9

func main() {
	for _, s := range sizes {
		img := render(s)
		check(img, s)
		f, err := os.Create(fmt.Sprintf("icon-%d.png", s))
		if err != nil {
			log.Fatal(err)
		}
		if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(f, img); err != nil {
			log.Fatal(err)
		}
		if err := f.Close(); err != nil {
			log.Fatal(err)
		}
	}
}

func render(s int) *image.RGBA {
	k := float32(s) / 96
	img := image.NewRGBA(image.Rect(0, 0, s, s))
	paint(img, teal, mask(s, func(z *vector.Rasterizer) { bubble(z, k) }))

	half := float32(wStroke) / 2
	if minPx := minStrokePx(s); wStroke*k < minPx { // keep the w legible instead of fading at small sizes
		half = minPx / k / 2
	}
	ink := image.NewAlpha(img.Bounds())
	for i, p := range wPoints {
		union(ink, mask(s, func(z *vector.Rasterizer) { circle(z, p[0]*k, p[1]*k, half*k) }))
		if i > 0 {
			q := wPoints[i-1]
			union(ink, mask(s, func(z *vector.Rasterizer) { segment(z, q, p, half, k) }))
		}
	}
	paint(img, white, ink)
	return img
}

// minStrokePx is the thinnest the w may get: 2 px at 16 px, growing to 2.5 px at 24 px.
func minStrokePx(s int) float32 {
	if s > 24 {
		return 0
	}
	return 2 + float32(s-16)/16
}

// bubble is the icon's outline: a rounded body with a tail at the bottom left.
func bubble(z *vector.Rasterizer, k float32) {
	z.MoveTo(32*k, 8*k)
	z.LineTo(64*k, 8*k)
	z.CubeTo(80*k, 8*k, 90*k, 18*k, 90*k, 34*k)
	z.LineTo(90*k, 54*k)
	z.CubeTo(90*k, 70*k, 80*k, 80*k, 64*k, 80*k)
	z.LineTo(40*k, 80*k)
	z.LineTo(18*k, 92*k)
	z.LineTo(22*k, 76*k)
	z.CubeTo(11*k, 72*k, 6*k, 64*k, 6*k, 54*k)
	z.LineTo(6*k, 34*k)
	z.CubeTo(6*k, 18*k, 16*k, 8*k, 32*k, 8*k)
	z.ClosePath()
}

func circle(z *vector.Rasterizer, cx, cy, r float32) {
	c := 0.5523 * r // cubic approximation of a quarter circle
	z.MoveTo(cx+r, cy)
	z.CubeTo(cx+r, cy+c, cx+c, cy+r, cx, cy+r)
	z.CubeTo(cx-c, cy+r, cx-r, cy+c, cx-r, cy)
	z.CubeTo(cx-r, cy-c, cx-c, cy-r, cx, cy-r)
	z.CubeTo(cx+c, cy-r, cx+r, cy-c, cx+r, cy)
	z.ClosePath()
}

// segment is the straight part of a stroke from a to b with half-width h (caps are the circles).
func segment(z *vector.Rasterizer, a, b [2]float32, h, k float32) {
	dx, dy := b[0]-a[0], b[1]-a[1]
	l := float32(math.Hypot(float64(dx), float64(dy)))
	nx, ny := -dy/l*h, dx/l*h
	z.MoveTo((a[0]+nx)*k, (a[1]+ny)*k)
	z.LineTo((b[0]+nx)*k, (b[1]+ny)*k)
	z.LineTo((b[0]-nx)*k, (b[1]-ny)*k)
	z.LineTo((a[0]-nx)*k, (a[1]-ny)*k)
	z.ClosePath()
}

// mask rasterizes one shape on its own: shapes are unioned afterwards, so overlapping ones of
// opposite winding cannot cancel out.
func mask(s int, shape func(*vector.Rasterizer)) *image.Alpha {
	z := vector.NewRasterizer(s, s)
	shape(z)
	m := image.NewAlpha(image.Rect(0, 0, s, s))
	z.Draw(m, m.Bounds(), image.Opaque, image.Point{})
	return m
}

func union(dst, src *image.Alpha) {
	for i, a := range src.Pix {
		dst.Pix[i] = max(dst.Pix[i], a)
	}
}

func paint(img *image.RGBA, c color.RGBA, m *image.Alpha) {
	draw.DrawMask(img, img.Bounds(), image.NewUniform(c), image.Point{}, m, image.Point{}, draw.Over)
}

// check fails the run if a rendering lost its shape: transparent corner, teal body, white w.
func check(img *image.RGBA, s int) {
	at := func(x, y float32) color.RGBA {
		return img.RGBAAt(int(x*float32(s)/96), int(y*float32(s)/96))
	}
	if c := at(1, 1); c.A != 0 {
		log.Fatalf("%d px: corner not transparent: %v", s, c)
	}
	if c := at(48, 20); !near(c, teal) {
		log.Fatalf("%d px: body above the w is %v, want teal", s, c)
	}
	if c := at(48, 42); !near(c, white) {
		log.Fatalf("%d px: middle of the w is %v, want white", s, c)
	}
}

// near allows for anti-aliasing at the edges of a pixel.
func near(c, want color.RGBA) bool {
	d := func(a, b uint8) bool { return max(int(a)-int(b), int(b)-int(a)) < 16 }
	return d(c.R, want.R) && d(c.G, want.G) && d(c.B, want.B) && d(c.A, want.A)
}
