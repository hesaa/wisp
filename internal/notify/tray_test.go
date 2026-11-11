package notify

import (
	"image/color"
	"testing"
)

// The badge sits on the teal logo, so a white ring has to separate the teal badge from it.
func TestBadgeHasWhiteRing(t *testing.T) {
	img := badgeImage("3")
	white := color.RGBA{0xff, 0xff, 0xff, 0xff}
	accent := color.RGBA{0x00, 0xa8, 0x84, 0xff}
	mid := badgePx / 2
	if c := img.RGBAAt(0, 0); c.A != 0 {
		t.Errorf("corner = %v, want transparent", c)
	}
	for _, x := range []int{0, 1, badgePx - 1} { // the ring is 2 px wide at the circle's edge
		if c := img.RGBAAt(x, mid); c != white {
			t.Errorf("edge pixel (%d,%d) = %v, want the white ring", x, mid, c)
		}
	}
	if c := img.RGBAAt(3, mid); c != accent { // inside, left of the glyph
		t.Errorf("inside = %v, want the accent", c)
	}
}
