package ui

import (
	"path/filepath"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
)

// gifPlayer plays a GIF (silent looping MP4 video) using Media Foundation's Source Reader
// (software video decoder), completely avoiding EVR (Enhanced Video Renderer) and Direct3D 11
// hardware acceleration which loads heavy GPU vendor drivers (~80 MB).
type gifPlayer struct {
	hwnd   win.HWND
	stop   chan struct{}
	done   chan struct{}
	mu     sync.Mutex
	pix    []byte
	w, h   int
	closed bool
}

func newGifPlayer(hwnd win.HWND, path string) (*gifPlayer, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	p := &gifPlayer{
		hwnd: hwnd,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	ready := make(chan error, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(p.done)

		vf, err := newVideoFrames(abs)
		if err != nil {
			ready <- err
			return
		}
		defer vf.close()

		pix, w, h, delay, err := vf.next()
		if err != nil {
			ready <- err
			return
		}

		p.mu.Lock()
		p.pix = append([]byte(nil), pix...)
		p.w, p.h = w, h
		p.mu.Unlock()
		ready <- nil
		win.InvalidateRect(p.hwnd, nil, false)

		for {
			select {
			case <-p.stop:
				return
			case <-time.After(delay):
			}

			pix, w, h, delay, err = vf.next()
			if err != nil {
				return
			}

			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				return
			}
			p.pix = append(p.pix[:0], pix...)
			p.w, p.h = w, h
			p.mu.Unlock()

			win.InvalidateRect(p.hwnd, nil, false)
		}
	}()

	if err := <-ready; err != nil {
		return nil, err
	}
	return p, nil
}

func (p *gifPlayer) Play() error                      { return nil }
func (p *gifPlayer) Pause() error                     { return nil }
func (p *gifPlayer) Seek(time.Duration) error         { return nil }
func (p *gifPlayer) Position() (time.Duration, error) { return 0, nil }
func (p *gifPlayer) Duration() (time.Duration, error) { return time.Second, nil }
func (p *gifPlayer) State() (playerState, error)      { return playerPlaying, nil }
func (p *gifPlayer) SetMute(bool) error               { return nil }

func (p *gifPlayer) UpdateVideo() error {
	p.mu.Lock()
	pix, w, h := p.pix, p.w, p.h
	p.mu.Unlock()
	if len(pix) == 0 || w <= 0 || h <= 0 {
		return nil
	}

	hdc := win.GetDC(p.hwnd)
	if hdc == 0 {
		return nil
	}
	defer win.ReleaseDC(p.hwnd, hdc)

	var rc win.RECT
	win.GetClientRect(p.hwnd, &rc)
	bounds := walk.Rectangle{Width: int(rc.Right - rc.Left), Height: int(rc.Bottom - rc.Top)}
	fit := fitRect(walk.Size{Width: w, Height: h}, bounds, 1, walk.Point{})

	bg := win.HBRUSH(win.GetStockObject(win.BLACK_BRUSH))
	if fit.X > 0 {
		leftR := win.RECT{Left: 0, Top: 0, Right: int32(fit.X), Bottom: int32(bounds.Height)}
		rightR := win.RECT{Left: int32(fit.X + fit.Width), Top: 0, Right: int32(bounds.Width), Bottom: int32(bounds.Height)}
		pFillRect.Call(uintptr(hdc), uintptr(unsafe.Pointer(&leftR)), uintptr(bg))
		pFillRect.Call(uintptr(hdc), uintptr(unsafe.Pointer(&rightR)), uintptr(bg))
	}
	if fit.Y > 0 {
		topR := win.RECT{Left: 0, Top: 0, Right: int32(bounds.Width), Bottom: int32(fit.Y)}
		botR := win.RECT{Left: 0, Top: int32(fit.Y + fit.Height), Right: int32(bounds.Width), Bottom: int32(bounds.Height)}
		pFillRect.Call(uintptr(hdc), uintptr(unsafe.Pointer(&topR)), uintptr(bg))
		pFillRect.Call(uintptr(hdc), uintptr(unsafe.Pointer(&botR)), uintptr(bg))
	}

	return blitFrame(hdc, fit, pix, w, h)
}

func (p *gifPlayer) Shutdown() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.stop)
	p.mu.Unlock()
	<-p.done
}
