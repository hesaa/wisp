package ui

import (
	"container/list"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"sync"

	"github.com/lxn/walk"
	"wa-go/internal/media"
)

var (
	avatarsMu     sync.RWMutex
	globalAvatars *Avatars
)

// SetAvatars sets the package-level Avatars cache used by drawAvatar.
func SetAvatars(a *Avatars) {
	avatarsMu.Lock()
	defer avatarsMu.Unlock()
	globalAvatars = a
}

func getAvatars() *Avatars {
	avatarsMu.RLock()
	defer avatarsMu.RUnlock()
	return globalAvatars
}

type avatarEntry struct {
	key string
	bmp *walk.Bitmap
}

// Avatars manages an LRU of 64 decoded avatar bitmaps.
type Avatars struct {
	cache     *media.Cache
	accountID string
	mu        sync.Mutex
	onWant    func(jid string)
	wanted    map[string]bool
	items     map[string]*list.Element
	order     *list.List
}

// NewAvatars creates a new Avatars cache.
func NewAvatars(c *media.Cache) *Avatars {
	return &Avatars{
		cache:  c,
		wanted: make(map[string]bool),
		items:  make(map[string]*list.Element),
		order:  list.New(),
	}
}

// SetAccount changes the active account.
func (a *Avatars) SetAccount(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.accountID != id {
		a.accountID = id
		a.wanted = make(map[string]bool)
	}
}

// OnWant registers the callback fired when an avatar picture is needed.
// Fired at most once per jid until Invalidate(jid).
func (a *Avatars) OnWant(fn func(jid string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onWant = fn
}

// Invalidate clears any cached bitmap and wanted flag for jid.
func (a *Avatars) Invalidate(jid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.wanted, jid)
	key := a.accountID + ":" + jid
	if el, ok := a.items[key]; ok {
		a.order.Remove(el)
		delete(a.items, key)
		if e, ok := el.Value.(avatarEntry); ok && e.bmp != nil {
			e.bmp.Dispose()
		}
	}
}

// Dispose disposes all bitmaps in the cache.
func (a *Avatars) Dispose() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for el := a.order.Front(); el != nil; el = el.Next() {
		if e, ok := el.Value.(avatarEntry); ok && e.bmp != nil {
			e.bmp.Dispose()
		}
	}
	a.items = make(map[string]*list.Element)
	a.order.Init()
	a.wanted = make(map[string]bool)
}

// Get returns the decoded avatar bitmap for jid at dpi, or nil if not cached.
// If the file does not exist, OnWant is fired once for that jid.
func (a *Avatars) Get(jid string, dpi int) *walk.Bitmap {
	if a == nil || jid == "" {
		return nil
	}
	a.mu.Lock()
	key := a.accountID + ":" + jid
	if el, ok := a.items[key]; ok {
		a.order.MoveToFront(el)
		bmp := el.Value.(avatarEntry).bmp
		a.mu.Unlock()
		return bmp
	}

	var path string
	if a.cache != nil && a.accountID != "" {
		path = a.cache.AvatarPath(a.accountID, jid)
	}

	if path != "" {
		if f, err := os.Open(path); err == nil {
			img, err := jpeg.Decode(f)
			f.Close()
			if err == nil {
				masked := circleMask(img)
				bmp, err := walk.NewBitmapFromImageForDPI(masked, dpi)
				if err == nil {
					if a.order.Len() >= 64 {
						oldest := a.order.Back()
						if oldest != nil {
							a.order.Remove(oldest)
							oe := oldest.Value.(avatarEntry)
							delete(a.items, oe.key)
							if oe.bmp != nil {
								oe.bmp.Dispose()
							}
						}
					}
					el := a.order.PushFront(avatarEntry{key: key, bmp: bmp})
					a.items[key] = el
					a.mu.Unlock()
					return bmp
				}
			}
		}
	}

	// File not on disk or failed to decode
	var wantFn func(jid string)
	if !a.wanted[jid] {
		a.wanted[jid] = true
		wantFn = a.onWant
	}
	a.mu.Unlock()

	if wantFn != nil {
		wantFn(jid)
	}
	return nil
}

// Len returns the number of bitmaps currently cached in the LRU.
func (a *Avatars) Len() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.order.Len()
}

// circleMask clips img to an anti-aliased circle with transparent corners.
func circleMask(img image.Image) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	cx, cy := float64(w)/2.0-0.5, float64(h)/2.0-0.5
	r := min(float64(w), float64(h)) / 2.0

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			d := math.Hypot(float64(x)-cx, float64(y)-cy)
			if d > r+0.5 {
				continue
			}
			alpha := uint8(255)
			if d > r-0.5 {
				alpha = uint8(255 * (r + 0.5 - d))
			}
			c := img.At(b.Min.X+x, b.Min.Y+y)
			cr, cg, cb, _ := c.RGBA()
			out.SetRGBA(x, y, color.RGBA{
				R: uint8(uint32(cr) * uint32(alpha) / 65535),
				G: uint8(uint32(cg) * uint32(alpha) / 65535),
				B: uint8(uint32(cb) * uint32(alpha) / 65535),
				A: alpha,
			})
		}
	}
	return out
}
