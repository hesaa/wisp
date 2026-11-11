package ui

import (
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

// playerState mirrors MFP_MEDIAPLAYER_STATE.
type playerState int

const (
	playerEmpty playerState = iota
	playerStopped
	playerPlaying
	playerPaused
	playerShutdown
)

// mediaPlayer is what the media view plays a video with; tests use a fake.
type mediaPlayer interface {
	Play() error
	Pause() error
	Seek(time.Duration) error
	Position() (time.Duration, error)
	Duration() (time.Duration, error)
	State() (playerState, error)
	SetMute(bool) error
	UpdateVideo() error
	Shutdown()
}

// clockText is d as m:ss, or h:mm:ss from an hour.
func clockText(d time.Duration) string {
	s := int(d / time.Second)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// seekAt is the position at x on a seek bar width pixels wide for a video total long.
func seekAt(x, width int, total time.Duration) time.Duration {
	if width <= 0 {
		return 0
	}
	return max(0, min(total, time.Duration(float64(total)*float64(x)/float64(width))))
}

// gifRestart reports whether a GIF has played to its end and must start again.
func gifRestart(st playerState, pos, total time.Duration) bool {
	return st == playerStopped && total > 0 && pos >= total-100*time.Millisecond
}

var pMFPCreateMediaPlayer = windows.NewLazySystemDLL("mfplay.dll").NewProc("MFPCreateMediaPlayer")

// IMFPMediaPlayer vtable slots, in mfplay.h order (Windows SDK 10.0.26100.0).
const (
	slotRelease     = 2
	slotPlay        = 3
	slotPause       = 4
	slotSetPosition = 7
	slotGetPosition = 8
	slotGetDuration = 9
	slotGetState    = 13
	slotSetMute     = 24
	slotUpdateVideo = 32
	slotShutdown    = 38
)

// comObj is a COM object: a pointer to its vtable.
type comObj struct{ vtbl *[slotShutdown + 1]uintptr }

// propVariant is a PROPVARIANT holding a VT_I8 or VT_UI8 (the union is two pointers wide).
type propVariant struct {
	vt  uint16
	_   [3]uint16
	val int64
	_   uintptr
}

const vtI8, vtUI8 = 20, 21

var guidNull windows.GUID // MFP_POSITIONTYPE_100NS

// comCall calls o's method at slot and turns a failed HRESULT into an error.
//
//go:uintptrescapes
func comCall(o *comObj, slot int, args ...uintptr) error {
	if o == nil {
		return errors.New("ui: player is shut down")
	}
	r, _, _ := syscall.SyscallN(o.vtbl[slot], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	if int32(r) < 0 {
		return fmt.Errorf("ui: IMFPMediaPlayer method %d: HRESULT %#x", slot, uint32(r))
	}
	return nil
}

// player plays a file into a window through mfplay.dll's IMFPMediaPlayer, with no callback object:
// its state is polled. UI thread only (COM is initialised there by walk).
type player struct{ com *comObj }

// newPlayer starts playing path into hwnd.
func newPlayer(hwnd win.HWND, path string) (*player, error) {
	url, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	if err := pMFPCreateMediaPlayer.Find(); err != nil {
		return nil, err
	}
	var o *comObj
	r, _, _ := pMFPCreateMediaPlayer.Call(uintptr(unsafe.Pointer(url)), 1, 0, 0, uintptr(hwnd), uintptr(unsafe.Pointer(&o)))
	if int32(r) < 0 || o == nil {
		return nil, fmt.Errorf("ui: MFPCreateMediaPlayer: HRESULT %#x", uint32(r))
	}
	return &player{com: o}, nil
}

func (p *player) Play() error  { return comCall(p.com, slotPlay) }
func (p *player) Pause() error { return comCall(p.com, slotPause) }

func (p *player) Seek(d time.Duration) error {
	v := propVariant{vt: vtI8, val: int64(d / 100)}
	return comCall(p.com, slotSetPosition, uintptr(unsafe.Pointer(&guidNull)), uintptr(unsafe.Pointer(&v)))
}

func (p *player) Position() (time.Duration, error) { return p.time(slotGetPosition) }
func (p *player) Duration() (time.Duration, error) { return p.time(slotGetDuration) }

// time reads a position-like value (GetPosition, GetDuration) in 100 ns units.
func (p *player) time(slot int) (time.Duration, error) {
	var v propVariant
	if err := comCall(p.com, slot, uintptr(unsafe.Pointer(&guidNull)), uintptr(unsafe.Pointer(&v))); err != nil {
		return 0, err
	}
	if v.vt != vtI8 && v.vt != vtUI8 { // GetDuration answers VT_UI8
		return 0, fmt.Errorf("ui: IMFPMediaPlayer method %d: variant type %d", slot, v.vt)
	}
	return time.Duration(v.val) * 100, nil
}

func (p *player) State() (playerState, error) {
	var st int32
	err := comCall(p.com, slotGetState, uintptr(unsafe.Pointer(&st)))
	return playerState(st), err
}

func (p *player) SetMute(on bool) error {
	b := uintptr(0)
	if on {
		b = 1
	}
	return comCall(p.com, slotSetMute, b)
}

// UpdateVideo repaints the video; called when its window paints or resizes.
func (p *player) UpdateVideo() error { return comCall(p.com, slotUpdateVideo) }

// Shutdown stops playback and releases the player; later calls fail. Safe to call twice.
func (p *player) Shutdown() {
	if p.com == nil {
		return
	}
	logErr("player shutdown", comCall(p.com, slotShutdown))
	syscall.SyscallN(p.com.vtbl[slotRelease], uintptr(unsafe.Pointer(p.com))) // returns the count, not an HRESULT
	p.com = nil
}
