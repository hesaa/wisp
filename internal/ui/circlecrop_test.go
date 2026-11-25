package ui

import (
	"image"
	"image/color"
	"testing"

	"github.com/lxn/walk"
)

func TestCircleCropPaintsOnlyOutsideTheCircle(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 20, 20))
	for i := range img.Pix {
		img.Pix[i] = 0xff // white
	}
	circleCrop(img, walk.RGB(0, 0, 0))
	if got := img.RGBAAt(0, 0); got != (color.RGBA{0, 0, 0, 0xff}) {
		t.Fatalf("corner %v, want background", got)
	}
	if got := img.RGBAAt(10, 10); got != (color.RGBA{0xff, 0xff, 0xff, 0xff}) {
		t.Fatalf("centre %v, want untouched", got)
	}
}
