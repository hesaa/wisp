package ui

import "github.com/lxn/walk"

// vscroll scrolls a custom-painted widget vertically over content total() pixels tall, with a
// draggable thumb along its right edge; the mouse wheel moves it wheelStep() pixels per notch. The
// widget's own MouseDown handler calls press first and handles the click only when press didn't.
type vscroll struct {
	cw        *walk.CustomWidget
	total     func() int
	wheelStep func() int
	moved     func() // after off changed by scrolling; may be nil
	off       int    // scroll position, pixels
	dragY     int    // mouse y where a thumb drag started; -1 when not dragging
	dragFrom  int    // off when the drag started
}

func (v *vscroll) attach(cw *walk.CustomWidget, total, wheelStep func() int) {
	v.cw, v.total, v.wheelStep, v.dragY = cw, total, wheelStep, -1
	cw.MouseMove().Attach(func(_, y int, b walk.MouseButton) {
		if v.dragY >= 0 && b&walk.LeftButton != 0 {
			v.scrollTo(v.dragFrom + v.thumbToOffset(y-v.dragY))
		}
	})
	cw.MouseUp().Attach(func(int, int, walk.MouseButton) { v.dragY = -1 })
	cw.MouseWheel().Attach(func(_, _ int, b walk.MouseButton) {
		v.scrollTo(v.off - walk.MouseWheelEventDelta(b)*v.wheelStep()/120)
	})
}

func (v *vscroll) view() int { return v.cw.ClientBoundsPixels().Height }

func (v *vscroll) clamp(off int) int { return max(0, min(off, v.total()-v.view())) }

func (v *vscroll) scrollTo(off int) {
	if off = v.clamp(off); off != v.off {
		v.off = off
		if v.moved != nil {
			v.moved()
		}
		logErr("scroll repaint", v.cw.Invalidate())
	}
}

// thumb is the scroll thumb, a zero rectangle when all content fits.
func (v *vscroll) thumb() walk.Rectangle {
	b := v.cw.ClientBoundsPixels()
	total := v.total()
	if total <= b.Height {
		return walk.Rectangle{}
	}
	th := max(v.cw.IntFrom96DPI(32), b.Height*b.Height/total)
	return walk.Rectangle{X: b.Width - v.cw.IntFrom96DPI(8), Y: v.off * (b.Height - th) / (total - b.Height), Width: v.cw.IntFrom96DPI(5), Height: th}
}

// thumbToOffset converts a thumb movement to a scroll movement.
func (v *vscroll) thumbToOffset(dy int) int {
	h, th := v.view(), v.thumb().Height
	if h <= th {
		return 0
	}
	return dy * (v.total() - h) / (h - th)
}

// press starts a thumb drag, or pages to a track click, for a left click at (x, y); it reports
// whether the click was on the scroll bar.
func (v *vscroll) press(x, y int) bool {
	t := v.thumb()
	if t.Height <= 0 || x < t.X-v.cw.IntFrom96DPI(4) {
		return false
	}
	if y < t.Y || y >= t.Y+t.Height { // track click: centre the thumb there
		v.scrollTo(v.off + v.thumbToOffset(y-t.Y-t.Height/2))
	}
	v.dragY, v.dragFrom = y, v.off
	return true
}
