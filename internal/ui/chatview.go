package ui

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // embedded previews and cached thumbnails
	_ "image/png"  // sticker previews
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // sticker files, in the media view
	"golang.org/x/sys/windows"

	"wa-go/internal/media"
	"wa-go/internal/store"
	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
)

const (
	chatHeaderH    = 60  // 1/96 inch
	loadAheadPx    = 120 // ask for older messages this close to the top (1/96 inch)
	thumbCacheSize = 24  // decoded thumbnails kept
	// maxPreviewPixels bounds what is decoded for painting: embedded previews and cached thumbnails
	// are at most a few hundred pixels square; anything bigger is refused, not decoded.
	maxPreviewPixels = 1 << 20
	// msgFormat is how gdiMeasurer.measure wraps text; message text is broken by wrapText instead.
	msgFormat = walk.TextWordbreak | walk.TextNoPrefix
	// autoScrollTick is how often a selection dragged past the top or bottom edge scrolls the view.
	autoScrollTick = 30 * time.Millisecond
)

var (
	getDoubleClickTime = windows.NewLazySystemDLL("user32.dll").NewProc("GetDoubleClickTime")
	getCapture         = windows.NewLazySystemDLL("user32.dll").NewProc("GetCapture")
	thumbSlot          = make(chan struct{}, 1) // one full-size image decoded at a time
)

// ChatView is the chat pane: a header with the chat's avatar, name and number over a custom-painted,
// scrolling canvas of message bubbles. Feed it with SetChat, PrependPage, Upsert and SetMediaPath;
// it asks for older pages and media through its callbacks. The canvas is a tab stop:
// Up/Down/PgUp/PgDn/Home/End move the focused message, Enter or Space download or open its media.
// Dragging over message text selects it (a double-click selects a word, a triple-click the
// message's text); Ctrl+C copies it, Esc or a click elsewhere clears it. UI-thread only.
type ChatView struct {
	*walk.Composite
	header *walk.CustomWidget
	canvas *walk.CustomWidget
	scroll vscroll
	pal    theme.Palette
	cache  *media.Cache

	chatModel
	dir         *Directory
	name        string // the chat's own name, "" when it has none
	lay         chatLayout
	stale       bool              // msgs changed since the last layout
	view        int               // canvas height the scroll offset was last fitted to
	cdpi        int               // DPI of the paint canvas; text is measured with its fonts
	focus       string            // keyboard-focused message ID
	requested   map[string]bool   // images whose download was requested on sight
	autoFetch   bool              // request images as they come into view (media.autodownload_images)
	thumbFiles  map[string]string // Cache.Thumbnail of a downloaded image; "" when it has none
	thumbBusy   map[string]bool
	bitmaps     *lru[thumbBitmap]
	gen         int // bumped per chat: thumbnails finished for an older chat are dropped
	lastClickID string
	lastClickAt time.Time
	clicks      int             // presses in a row on lastClickID, each within the double-click time
	selA, selB  textMark        // anchor and caret of the text selection; selA.id is "" for none
	dragging    bool            // the left button went down on text and is held: moves extend the selection
	drag        walk.Point      // where the pointer is while dragging
	ticking     bool            // an auto-scroll tick is pending
	pickAnchor  string          // where a range of picks starts (Shift+click, Shift+Up/Down); "" for none
	picked      map[string]bool // messages picked for copying (Ctrl+click, Shift+click, Shift+Up/Down)
	notice      string          // shown in the header for a while instead of the number
	noticeGen   int             // bumped per notice: only the latest one's timer clears it
	glow        string          // message briefly highlighted after a jump to it
	glowGen     int             // bumped per glow: only the latest one's timer clears it
	jumping     string          // quoted message a jump is paging back to; "" for none
	jumpPages   int             // older pages asked for on its behalf

	onLoadPrevious func()
	onNeedMedia    func(store.Message)
	onOpenMedia    func(store.Message)
	onOpenInfo     func()
	onReply        func(store.Message)
	hasMessage     func(id string) bool
}

func (v *ChatView) OnOpenInfo(fn func()) {
	v.onOpenInfo = fn
}

// chatModel is the open chat's messages and paging state, kept apart from the window so its rules
// are tested on their own. Answers and events carry their chat: those for another chat are ignored.
type chatModel struct {
	account, chat      string
	msgs               []store.Message // oldest first
	loading, exhausted bool            // an older page was asked for; there is none
}

// open shows a chat from its newest page (newest first); a short page is all of its history.
func (m *chatModel) open(accountID, chatJID string, firstPage []store.Message) {
	m.account, m.chat = accountID, chatJID
	m.msgs = slices.Clone(firstPage)
	slices.Reverse(m.msgs)
	m.loading, m.exhausted = false, len(firstPage) < store.PageSize
}

func (m *chatModel) is(accountID, chatJID string) bool {
	return accountID == m.account && chatJID == m.chat
}

func (m *chatModel) index(id string) int {
	return slices.IndexFunc(m.msgs, func(x store.Message) bool { return x.ID == id })
}

// prepend answers a request for the page before the loaded messages with page (newest first) and
// returns how many messages it added above them. A short page ends the history. Messages already
// loaded are skipped: pages shift as new messages arrive.
func (m *chatModel) prepend(accountID, chatJID string, page []store.Message) int {
	if !m.is(accountID, chatJID) {
		return 0 // a late answer for a chat no longer open
	}
	m.loading = false
	if len(page) < store.PageSize {
		m.exhausted = true
	}
	var older []store.Message
	for i := len(page) - 1; i >= 0; i-- {
		if m.index(page[i].ID) < 0 {
			older = append(older, page[i])
		}
	}
	m.msgs = append(older, m.msgs...)
	return len(older)
}

// jumpMaxPages bounds the older pages a jump to a quoted message loads: they all stay in memory.
const jumpMaxPages = 20

// jumpStep says how a jump to message id goes on after pages older pages were loaded for it: i is
// its index once loaded (-1 before), more whether to load another.
func (m *chatModel) jumpStep(id string, pages int) (i int, more bool) {
	if i = m.index(id); i >= 0 {
		return i, false
	}
	return -1, !m.exhausted && pages < jumpMaxPages
}

// centerOn is the scroll offset that centres something h high at top in a view.
func centerOn(top, h, view int) int { return top + h/2 - view/2 }

// noMore answers a request for an older page of a chat that has none.
func (m *chatModel) noMore(accountID, chatJID string) {
	if m.is(accountID, chatJID) {
		m.loading, m.exhausted = false, true
	}
}

// upsert replaces a loaded message or adds a new one by time, and reports whether the chat changed.
// A replacement without a media path keeps the known one (events don't carry downloads). An unknown
// message older than the loaded ones, while older pages remain, is left to arrive with its page:
// placed now it would sit above a gap and break the order once the page comes.
func (m *chatModel) upsert(msg store.Message) bool {
	if !m.is(msg.AccountID, msg.ChatJID) {
		return false
	}
	if i := m.index(msg.ID); i >= 0 {
		if msg.MediaPath == "" {
			msg.MediaPath = m.msgs[i].MediaPath
		}
		m.msgs[i] = msg
		return true
	}
	if !m.exhausted && len(m.msgs) > 0 && msg.Timestamp < m.msgs[0].Timestamp {
		return false
	}
	at := len(m.msgs)
	for at > 0 && m.msgs[at-1].Timestamp > msg.Timestamp {
		at--
	}
	m.msgs = slices.Insert(m.msgs, at, msg)
	return true
}

// textMark is an end of the text selection: an offset into the text of the message with this ID,
// so it stays put as messages are added around it.
type textMark struct {
	id  string
	off int
}

// thumbBitmap is a thumbnail decoded and scaled for its box.
type thumbBitmap struct {
	bmp  *walk.Bitmap // nil: the source could not be decoded
	src  string       // thumbnail file, "" for the embedded preview
	w, h int
}

// NewChatView creates the chat pane in parent. cache makes thumbnails of downloaded images;
// senderName gives a group member's display name ("" when unknown; the number is shown then).
func NewChatView(parent walk.Container, p theme.Palette, cache *media.Cache, senderName func(jid string) string) (*ChatView, error) {
	comp, err := walk.NewComposite(parent)
	if err != nil {
		return nil, err
	}
	v := &ChatView{Composite: comp, pal: p, cache: cache, cdpi: canvasDPI(), autoFetch: true,
		picked: map[string]bool{}, requested: map[string]bool{}, thumbFiles: map[string]string{}, thumbBusy: map[string]bool{},
		bitmaps: newLRU(thumbCacheSize, func(t thumbBitmap) {
			if t.bmp != nil {
				t.bmp.Dispose()
			}
		}),
	}
	v.lay.senderName = senderName
	if err := v.build(); err != nil {
		comp.Dispose()
		return nil, err
	}
	return v, nil
}

func (v *ChatView) build() error {
	var err error
	if err = setBox(v.Composite, walk.NewVBoxLayout(), walk.Margins{}); err != nil {
		return err
	}
	if v.header, err = newCanvas(v.Composite, v.paintHeader); err != nil {
		return err
	}
	if v.canvas, err = newCanvas(v.Composite, v.paint); err != nil {
		return err
	}
	cw := v.canvas
	if err := errors.Join(v.header.SetMinMaxSize(walk.Size{Height: chatHeaderH}, walk.Size{Height: chatHeaderH}),
		fillRest(v.Composite, cw), focusable(v.header, walk.AccRolePushbutton, "Chat details", v.headerKey), focusable(cw, walk.AccRoleList, "Messages", v.key)); err != nil {
		return err
	}
	v.header.MouseDown().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton && v.onOpenInfo != nil && v.chat != "" {
			v.onOpenInfo()
		}
	})
	cw.MouseDown().Attach(v.mouseDown)
	cw.MouseMove().Attach(v.mouseMove)
	cw.MouseUp().Attach(func(_, _ int, b walk.MouseButton) {
		if b == walk.LeftButton {
			v.dragging = false
		}
	})
	takeEscape(cw, func() bool { _, _, ok := v.selection(); return ok || len(v.picked) > 0 })
	takeContextMenu(cw, v.menuAt)
	v.scroll.attach(cw, func() int { return v.lay.total }, func() int { return cw.IntFrom96DPI(100) })
	v.scroll.moved = v.settle
	cw.SizeChanged().Attach(func() {
		logErr("chat layout", v.relayout(false))
		v.settle()
	})
	cw.FocusedChanged().Attach(func() {
		if cw.Focused() && v.focusIndex() < 0 {
			if i := v.lastFull(); i >= 0 {
				v.setFocus(i) // no scrolling: a click may be resolving its message against this offset
			}
		}
	})
	cw.Disposing().Attach(v.bitmaps.clear)
	if err := v.buildMenu(); err != nil {
		return err
	}
	return v.SetPalette(v.pal)
}

// OnLoadPrevious sets the handler called when the view nears the top of the loaded messages. It
// loads the next older page of the chat open at that moment and answers with PrependPage or
// NoMoreHistory naming that chat; it fires once until answered for the open chat.
func (v *ChatView) OnLoadPrevious(fn func()) { v.onLoadPrevious = fn }

// OnNeedMedia sets the handler that downloads a message's media and then calls SetMediaPath. It
// fires once when an image comes into view and again on each click of undownloaded media, so it
// may come for a download already under way.
func (v *ChatView) OnNeedMedia(fn func(store.Message)) { v.onNeedMedia = fn }

// OnOpenMedia sets the handler that opens media, downloaded or not (Enter; a click on a photo,
// sticker or video's picture; a double-click elsewhere and on documents and audio): it shows it in
// the media view, or opens its file, or asks for the download first.
func (v *ChatView) OnOpenMedia(fn func(store.Message)) { v.onOpenMedia = fn }

// OnReply sets the handler of a reply started on a message: Ctrl+R on the focused one, Reply in
// the context menu, or a double-click on a text bubble beside its text.
func (v *ChatView) OnReply(fn func(store.Message)) { v.onReply = fn }

// OnHasMessage sets the lookup of whether the open chat has message id stored, loaded or not: a click
// on a reply's quote pages back to an unloaded original only when it has.
func (v *ChatView) OnHasMessage(fn func(id string) bool) { v.hasMessage = fn }

func (v *ChatView) triggerReply(m store.Message) {
	if v.onReply != nil && m.Type != "revoked" && m.Type != "unsupported" {
		v.onReply(m)
	}
}

// replyFocused starts a reply to the focused message.
func (v *ChatView) replyFocused() {
	if i := v.focusIndex(); i >= 0 {
		v.triggerReply(v.msgs[i])
	}
}

// jumpToMessage shows message id (the original of a reply) centred and briefly highlighted, loading
// older pages up to it when it is stored but not loaded.
func (v *ChatView) jumpToMessage(id string) {
	if v.index(id) < 0 && (v.hasMessage == nil || !v.hasMessage(id)) {
		v.flash("Original message isn't available")
		return
	}
	v.jumping, v.jumpPages = id, 0
	v.chase()
}

// chase goes on with a jump: it shows the message once loaded, else asks for the next older page
// (PrependPage and NoMoreHistory come back here).
func (v *ChatView) chase() {
	if v.jumping == "" {
		return
	}
	i, more := v.jumpStep(v.jumping, v.jumpPages)
	switch {
	case i >= 0:
		v.jumping = ""
		logErr("chat layout", v.relayout(false))
		if len(v.lay.tops) != len(v.msgs) {
			return
		}
		id := v.msgs[i].ID
		v.setFocus(i)
		v.scroll.scrollTo(centerOn(v.lay.tops[i], v.lay.cache[id].box.Height, v.view))
		v.glow = id
		v.glowGen++
		gen := v.glowGen
		time.AfterFunc(400*time.Millisecond, func() {
			v.canvas.Synchronize(func() {
				if gen == v.glowGen && !v.canvas.IsDisposed() {
					v.glow = ""
					logErr("chat repaint", v.canvas.Invalidate())
				}
			})
		})
	case !more || v.onLoadPrevious == nil:
		v.jumping = ""
		if v.exhausted {
			v.flash("Original message isn't available")
		} else {
			v.flash("Original message is too far back")
		}
	case !v.loading:
		v.loading = true
		v.jumpPages++
		v.onLoadPrevious()
	}
}

// Messages are the loaded messages, oldest first; the caller must not change them.
func (v *ChatView) Messages() []store.Message { return v.msgs }

// Focus gives the messages the keyboard.
func (v *ChatView) Focus() { focusOn(v.canvas) }

// SetChat shows a chat from its newest page (store.GetChatMessages page 0, newest first), scrolled
// to the end. name is the chat's own name ("" for none).
func (v *ChatView) SetChat(accountID, chatJID, name string, isGroup bool, firstPage []store.Message) {
	v.open(accountID, chatJID, firstPage)
	v.name = name
	v.lay.reset()
	v.lay.group = isGroup
	v.bitmaps.clear()
	clear(v.requested)
	clear(v.thumbFiles)
	clear(v.thumbBusy)
	v.gen++
	v.focus, v.view, v.scroll.off, v.stale = "", 0, 0, true
	v.selA, v.selB, v.dragging = textMark{}, textMark{}, false
	clear(v.picked)
	v.pickAnchor = ""
	v.jumping, v.glow = "", ""
	logErr("chat layout", v.relayout(false))
	logErr("chat repaint", errors.Join(v.header.Invalidate(), v.canvas.Invalidate()))
	v.settle()
}

// PrependPage answers OnLoadPrevious for accountID's chat chatJID with the next older page (newest
// first, as store.GetChatMessages returns it), added above the loaded messages with the messages on
// screen kept where they are. A short page ends the history. An answer for a chat that is no longer
// open, empty or not, is ignored.
func (v *ChatView) PrependPage(accountID, chatJID string, page []store.Message) {
	logErr("chat layout", v.relayout(false)) // the total to anchor against
	if v.prepend(accountID, chatJID, page) > 0 {
		v.stale = true
		logErr("chat layout", v.relayout(true))
		logErr("chat repaint", v.canvas.Invalidate())
	}
	v.settle()
	v.chase()
}

// NoMoreHistory answers OnLoadPrevious for accountID's chat chatJID when there is nothing older; an
// answer for a chat that is no longer open is ignored.
func (v *ChatView) NoMoreHistory(accountID, chatJID string) {
	v.noMore(accountID, chatJID)
	v.chase()
}

// Upsert adds a new message of the open chat or replaces a changed one (status, edit, deletion); an
// empty MediaPath keeps the downloaded one. The view follows new messages only when it was showing
// the end. Other chats' messages are ignored, and so are unknown ones older than the loaded messages
// while older pages remain: they come with their page.
func (v *ChatView) Upsert(msg store.Message) {
	old := ""
	if i := v.index(msg.ID); i >= 0 {
		old = v.msgs[i].MediaPath
	}
	if !v.upsert(msg) {
		return
	}
	if msg.MediaPath != "" && msg.MediaPath != old {
		delete(v.thumbFiles, msg.ID) // made from the old download
	}
	v.lay.forget(msg.ID)
	v.stale = true
	logErr("chat layout", v.relayout(false))
	logErr("chat repaint", v.canvas.Invalidate())
	v.settle()
}

// Rename shows a new name for accountID's chat chatJID (a group was named) when it is open.
func (v *ChatView) Rename(accountID, chatJID, name string) {
	if v.is(accountID, chatJID) && name != v.name {
		v.name = name
		logErr("chat header repaint", v.header.Invalidate())
	}
}

// SetMediaPath records that message id's media was downloaded to path.
func (v *ChatView) SetMediaPath(id, path string) {
	i := v.index(id)
	if i < 0 {
		return
	}
	v.msgs[i].MediaPath = path
	delete(v.thumbFiles, id)
	v.lay.forget(id)
	v.stale = true
	logErr("chat layout", v.relayout(false))
	logErr("chat repaint", v.canvas.Invalidate())
	v.settle()
}

// SetAutoDownload turns requesting images as they come into view on or off (on at first); a click
// on undownloaded media requests it either way.
func (v *ChatView) SetAutoDownload(on bool) {
	v.autoFetch = on
	if on {
		v.settle() // the images already in view
	}
}

// fetchOnSight reports whether an undownloaded image that came into view is requested now: only
// with auto-download on, and once per chat (recorded in requested).
func fetchOnSight(id string, auto bool, requested map[string]bool) bool {
	if !auto || requested[id] {
		return false
	}
	requested[id] = true
	return true
}

// SetPalette recolours the pane.
func (v *ChatView) SetPalette(p theme.Palette) error {
	v.pal = p
	return errors.Join(setBg(v.Composite, p.ChatBg), v.header.Invalidate(), v.canvas.Invalidate())
}

func (v *ChatView) focusIndex() int {
	if v.focus == "" {
		return -1
	}
	return v.index(v.focus)
}

// SetDirectory sets the directory index for name resolution and repaints the view.
func (v *ChatView) SetDirectory(d *Directory) {
	v.dir = d
	v.lay.dir = d
	v.lay.cache = nil
	logErr("chat repaint", v.canvas.Invalidate())
	logErr("chat header repaint", v.header.Invalidate())
}

// Invalidate repaints the chat view: header, message canvas, and composite.
func (v *ChatView) Invalidate() error {
	if v == nil {
		return nil
	}
	var errs []error
	if v.header != nil {
		errs = append(errs, v.header.Invalidate())
	}
	if v.canvas != nil {
		errs = append(errs, v.canvas.Invalidate())
	}
	if v.Composite != nil {
		errs = append(errs, v.Composite.Invalidate())
	}
	return errors.Join(errs...)
}

func (v *ChatView) Directory() *Directory {
	return v.dir
}

// title is the chat's name by the name rule.
func (v *ChatView) title() string { return v.dir.Name(v.chat, v.name) }

// relayout brings the layout up to date with the messages and the canvas size. The view stays on
// the end when it showed the end; prepended (older messages added) keeps the messages on screen.
func (v *ChatView) relayout(prepended bool) error {
	b, dpi := v.canvas.ClientBoundsPixels(), v.canvas.DPI()
	if b.Width <= 0 || b.Height <= 0 ||
		!v.stale && v.lay.cache != nil && b.Width == v.lay.width && dpi == v.lay.dpi && b.Height == v.view {
		return nil
	}
	oldTotal, oldView := v.lay.total, v.view
	ms := &gdiMeasurer{dpi: dpi, cdpi: v.cdpi}
	v.lay.update(v.msgs, b.Width, dpi, ms)
	if err := ms.close(); err != nil {
		v.lay.cache = nil // measure again next time
		return err
	}
	off := followBottom(v.scroll.off, oldTotal, oldView, v.lay.total, b.Height)
	if prepended {
		off = anchorAfterPrepend(v.scroll.off, oldTotal, v.lay.total, b.Height)
	}
	v.stale, v.view = false, b.Height
	v.scroll.off = v.scroll.clamp(off)
	return nil
}

// pad is the space above the messages when they don't fill the view: they sit at its bottom.
func (v *ChatView) pad() int { return max(0, v.view-v.lay.total) }

// screenY is message i's top in canvas pixels.
func (v *ChatView) screenY(i int) int { return v.pad() + v.lay.tops[i] - v.scroll.off }

// visible is the range [first, last) of messages at least partly on screen.
func (v *ChatView) visible() (first, last int) {
	tops := v.lay.tops
	if len(tops) != len(v.msgs) { // not laid out yet
		return 0, 0
	}
	top := v.scroll.off - v.pad()
	first = sort.Search(len(tops), func(i int) bool { return tops[i]+v.lay.cache[v.msgs[i].ID].box.Height > top })
	last = sort.Search(len(tops), func(i int) bool { return tops[i] >= top+v.view })
	return first, last
}

// lastFull is the newest message wholly on screen (else the newest partly on screen; -1 for none):
// where focus rests when the canvas gains it with nothing focused, so gaining focus never scrolls.
func (v *ChatView) lastFull() int {
	first, last := v.visible()
	for i := last - 1; i >= first; i-- {
		if y := v.screenY(i); y >= 0 && y+v.lay.cache[v.msgs[i].ID].box.Height <= v.view {
			return i
		}
	}
	return last - 1
}

// hit is the message whose bubble is at canvas point (x, y), -1 for none.
func (v *ChatView) hit(x, y int) int {
	first, last := v.visible()
	for i := first; i < last; i++ {
		b := v.lay.cache[v.msgs[i].ID].box
		if top := v.screenY(i); y >= top && y < top+b.Height && x >= b.X && x < b.X+b.Width {
			return i
		}
	}
	return -1
}

// settle acts on what the view now shows: asks for the previous page near the top, for the media of
// images in sight, and makes thumbnails of downloaded images in sight. Never called while painting:
// the handlers may change the messages.
func (v *ChatView) settle() {
	if v.chat == "" || len(v.lay.tops) != len(v.msgs) {
		return
	}
	if v.onLoadPrevious != nil && shouldLoadPrevious(v.scroll.off, v.canvas.IntFrom96DPI(loadAheadPx), v.loading, v.exhausted) {
		v.loading = true
		v.onLoadPrevious() // may answer right away
	}
	first, last := v.visible()
	for _, m := range slices.Clone(v.msgs[first:last]) {
		switch {
		case m.Type != "image":
		case m.MediaPath == "":
			if v.onNeedMedia != nil && fetchOnSight(m.ID, v.autoFetch, v.requested) {
				v.onNeedMedia(m)
			}
		default:
			if _, done := v.thumbFiles[m.ID]; !done && !v.thumbBusy[m.ID] {
				v.makeThumb(m.ID, m.MediaPath)
			}
		}
	}
}

// makeThumb has the cache make a downloaded image's thumbnail off the UI thread (it decodes the
// full image) and repaints with it.
func (v *ChatView) makeThumb(id, path string) {
	if v.cache == nil {
		v.thumbFiles[id] = ""
		return
	}
	v.thumbBusy[id] = true
	gen := v.gen
	go func() {
		thumbSlot <- struct{}{}
		thumb, err := v.cache.Thumbnail(path)
		<-thumbSlot
		v.canvas.Synchronize(func() {
			if gen != v.gen || v.canvas.IsDisposed() {
				return
			}
			delete(v.thumbBusy, id)
			if err != nil && !errors.Is(err, media.ErrNoThumbnail) {
				logErr("thumbnail of message "+id, err)
			}
			v.thumbFiles[id] = thumb // "" on failure: the embedded preview stays
			logErr("chat repaint", v.canvas.Invalidate())
		})
	}()
}

// thumbFor is message m's thumbnail scaled to w x h: the cached thumbnail of its download, else its
// embedded preview; nil when there is neither. Only visible bubbles ask, and the decoded bitmaps are
// kept in a small LRU.
func (v *ChatView) thumbFor(m *store.Message, preview bool, w, h int) *walk.Bitmap {
	src := v.thumbFiles[m.ID]
	if t, ok := v.bitmaps.get(m.ID); ok && t.src == src && t.w == w && t.h == h {
		return t.bmp
	}
	if src == "" && !preview {
		return nil
	}
	img, err := v.decodeThumb(m, src, w, h)
	var bmp *walk.Bitmap
	if err == nil {
		bmp, err = walk.NewBitmapFromImageForDPI(img, v.canvas.DPI())
	}
	logErr("thumbnail of message "+m.ID, err)
	v.bitmaps.put(m.ID, thumbBitmap{bmp: bmp, src: src, w: w, h: h})
	return bmp
}

func (v *ChatView) decodeThumb(m *store.Message, src string, w, h int) (*image.RGBA, error) {
	if src != "" {
		f, err := os.Open(src)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return coverImage(f, w, h)
	}
	info, err := parseMedia(m)
	if err != nil {
		return nil, err
	}
	return coverImage(bytes.NewReader(info.preview), w, h)
}

// decodeCapped decodes a JPEG or PNG after reading its size: one of more than limit pixels is
// refused undecoded, so a hostile file cannot make the decoder allocate gigabytes.
func decodeCapped(r io.ReadSeeker, limit int) (image.Image, error) {
	cfg, _, err := image.DecodeConfig(r)
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > limit/cfg.Height {
		return nil, fmt.Errorf("ui: refusing to decode a %dx%d image: %w", cfg.Width, cfg.Height, media.ErrImageTooLarge)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	src, _, err := image.Decode(r)
	return src, err
}

// decodeFull decodes a cached photo or sticker at full size, as decodeCapped does for thumbnails;
// one of more than maxFullPixels is refused with media.ErrImageTooLarge.
func decodeFull(path string) (*image.RGBA, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	src, err := decodeCapped(f, maxFullPixels)
	if err != nil {
		return nil, err
	}
	if rgba, ok := src.(*image.RGBA); ok {
		return rgba, nil
	}
	dst := image.NewRGBA(src.Bounds())
	draw.Draw(dst, dst.Bounds(), src, src.Bounds().Min, draw.Src)
	return dst, nil
}

// coverImage decodes a small JPEG or PNG and scales it to cover w x h, centre-cropped, so only a
// box-sized image outlives the call.
func coverImage(r io.ReadSeeker, w, h int) (*image.RGBA, error) {
	src, err := decodeCapped(r, maxPreviewPixels)
	if err != nil {
		return nil, err
	}
	sb := src.Bounds()
	if sw, sh := sb.Dx(), sb.Dy(); sw*h > sh*w {
		cw := max(1, sh*w/h)
		sb.Min.X += (sw - cw) / 2
		sb.Max.X = sb.Min.X + cw
	} else {
		ch := max(1, sw*h/w)
		sb.Min.Y += (sh - ch) / 2
		sb.Max.Y = sb.Min.Y + ch
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, sb, draw.Src, nil)
	return dst, nil
}

func (v *ChatView) setFocus(i int) {
	v.focus = v.msgs[i].ID
	logErr("chat repaint", v.canvas.Invalidate())
	announce(v.canvas, "Messages with "+v.title(), v.describe(i))
}

// focusOn moves the focus to message i and scrolls it into view.
func (v *ChatView) focusOn(i int) {
	v.setFocus(i)
	v.scroll.scrollTo(scrollIntoView(v.scroll.off, v.lay.tops[i], v.lay.cache[v.msgs[i].ID].box.Height, v.view))
}

// describe is message i for screen readers: sender, content, time and delivery state.
func (v *ChatView) describe(i int) string {
	m, b := &v.msgs[i], v.lay.cache[v.msgs[i].ID]
	who := v.senderOf(*m)
	text, _, _ := bubbleText(m, b.info, v.dir)
	switch b.kind {
	case kindImage:
		what := "Photo"
		if m.Type == "sticker" {
			what = "Sticker"
		}
		text = strings.TrimSpace(what + " " + text)
	case kindCard:
		title, sub := cardLabels(m, b.info)
		text = strings.Join(slices.DeleteFunc([]string{title, sub, text}, func(s string) bool { return s == "" }), ", ")
	}
	if b.quotedSender != "" {
		text = "in reply to " + b.quotedSender + ", " + b.quotedSnippet + ": " + text
	}
	item := who + ": " + text + ", " + clock(m.Timestamp)
	if m.FromMe && m.Status != "" {
		item += ", " + m.Status
	}
	return item
}

// senderOf names who sent m: "You", the member in a group, else the chat's other party.
func (v *ChatView) senderOf(m store.Message) string {
	switch {
	case m.FromMe:
		return "You"
	case v.lay.group:
		return v.lay.sender(m.SenderJID)
	}
	return v.title()
}

func (v *ChatView) key(k walk.Key) bool {
	v.jumping = "" // any key or click calls a jump still paging back off
	mods := walk.ModifiersDown()
	switch {
	case k == walk.KeyC && mods == walk.ModControl:
		return v.copySelection() || v.copyPicked()
	case k == walk.KeyR && mods == walk.ModControl:
		v.replyFocused()
		return true
	case k == walk.KeyEscape: // only while there is a selection or picks (takeEscape)
		v.setSel(textMark{}, textMark{})
		v.setPicks(nil)
		v.pickAnchor = ""
		return true
	}
	i := v.focusIndex()
	if mods == walk.ModShift && (k == walk.KeyUp || k == walk.KeyDown) && i >= 0 {
		next := max(0, min(len(v.msgs)-1, i+1))
		if k == walk.KeyUp {
			next = max(0, i-1)
		}
		v.pickRange(i, next)
		v.focusOn(next)
		return true
	}
	if k == walk.KeyReturn || k == walk.KeySpace {
		if i >= 0 {
			v.activate(v.msgs[i], true)
		}
		return true
	}
	first, last := v.visible()
	next, ok := navigate(i, len(v.lay.tops), max(1, last-first-1), k, false)
	if ok && next >= 0 {
		v.focusOn(next)
	}
	return ok
}

func (v *ChatView) mouseDown(x, y int, b walk.MouseButton) {
	if b != walk.LeftButton || v.scroll.press(x, y) {
		return
	}
	v.jumping = ""
	// resolve the message against the view the user saw, then take focus, then act on it
	i, prev := v.hit(x, y), v.focusIndex()
	if i >= 0 {
		v.setFocus(i)
	}
	focusOn(v.canvas)
	mods := walk.ModifiersDown()
	switch {
	case i >= 0 && mods == walk.ModControl:
		v.setSel(textMark{}, textMark{})
		v.togglePick(v.msgs[i].ID)
		v.pickAnchor = v.msgs[i].ID
		return
	case i >= 0 && mods == walk.ModShift:
		v.pickRange(max(prev, 0), i)
		return
	}
	v.setPicks(nil)
	v.pickAnchor = ""
	if i < 0 {
		v.setSel(textMark{}, textMark{})
		return
	}
	m, now := v.msgs[i], time.Now()
	ms, _, _ := getDoubleClickTime.Call()
	n := 1
	if m.ID == v.lastClickID && now.Sub(v.lastClickAt) <= time.Duration(ms)*time.Millisecond {
		n = v.clicks + 1
	}
	v.lastClickID, v.lastClickAt, v.clicks = m.ID, now, n
	bub := v.lay.cache[m.ID]
	if in(bub.quotedBox, x, y-v.screenY(i)) {
		v.setSel(textMark{}, textMark{})
		v.jumpToMessage(m.QuotedID)
		return
	}
	// a press on text selects, so a double-click on a caption picks a word and leaves the media be
	if t := v.lay.cache[m.ID].text; len(v.selText(i)) > 0 && x >= t.X && x < t.X+t.Width && y >= v.screenY(i)+t.Y && y < v.screenY(i)+t.Y+t.Height {
		v.pressText(x, y, n)
		return
	}
	v.setSel(textMark{}, textMark{})
	if n == 2 {
		v.lastClickID = ""        // a third click starts over
		if bub.kind == kindText { // on its text the double-click picks a word, on media it opens
			v.triggerReply(m)
			return
		}
	}
	v.activate(m, clickOpens(m.Type, v.lay.cache[m.ID], x, y-v.screenY(i), n, mods, v.dragging))
}

// clickOpens reports whether the nth press in a row at (x, y) (y from the bubble's top) opens a
// message, as Enter does. On the picture or card of a photo, sticker or video the first plain press
// does (not one ending a drag), and the second of a double-click must not open it again; elsewhere,
// and for documents and audio, the double-click does. Picks and text keep their own gestures.
func clickOpens(typ string, b bubble, x, y, n int, mods walk.Modifiers, dragged bool) bool {
	if (typ == "image" || typ == "sticker" || typ == "video") && (in(b.thumb, x, y) || in(b.card, x, y)) {
		return n == 1 && mods == 0 && !dragged
	}
	return n == 2
}

// pressText starts a selection for the nth press in a row on text at (x, y): the first sets the
// anchor of a drag, the second selects the word under the pointer, the third the message's text.
func (v *ChatView) pressText(x, y, n int) {
	p := v.posAt(x, y, n == 2)
	id, units := v.msgs[p.msg].ID, v.selText(p.msg)
	a, b := p.off, p.off
	switch n {
	case 1:
		v.dragging, v.drag = true, walk.Point{X: x, Y: y}
	case 2:
		a, b = wordAt(units, p.off)
	default:
		a, b = 0, len(units)
		v.lastClickID = "" // the next press starts over
	}
	v.setSel(textMark{id, a}, textMark{id, b})
}

func (v *ChatView) mouseMove(x, y int, b walk.MouseButton) {
	if !v.dragging {
		return
	}
	if b&walk.LeftButton == 0 { // released where the canvas didn't hear it
		v.dragging = false
		return
	}
	v.drag = walk.Point{X: x, Y: y}
	v.extend()
	if y < 0 || y >= v.view {
		v.armScroll()
	}
}

// extend moves the selection's caret to the text under the dragging pointer.
func (v *ChatView) extend() { v.setSel(v.selA, v.mark(v.posAt(v.drag.X, v.drag.Y, false))) }

// armScroll schedules an auto-scroll tick unless one is pending: the timer lives only while a drag
// is past an edge.
func (v *ChatView) armScroll() {
	if !v.ticking {
		v.ticking = true
		time.AfterFunc(autoScrollTick, func() { v.canvas.Synchronize(v.autoScroll) })
	}
}

// autoScroll scrolls towards a pointer dragged past the top or bottom edge, faster the further out
// it is, extends the selection and goes on until the pointer is back or the button is released.
func (v *ChatView) autoScroll() {
	v.ticking = false
	if v.canvas.IsDisposed() || !v.dragging {
		return
	}
	if h, _, _ := getCapture.Call(); win.HWND(h) != v.canvas.Handle() { // the drag ended unheard (capture taken)
		v.dragging = false
		return
	}
	if d := min(v.drag.Y, 0) + max(v.drag.Y-v.view+1, 0); d != 0 {
		v.scroll.scrollTo(v.scroll.off + d)
		v.extend()
		v.armScroll()
	}
}

// posAt is the text position at canvas point (x, y), or the nearest one: in the last message whose
// top is at or above y (the first above all), the start of its text above the text or when it has
// none to select, the end below it, else the boundary nearest to x on the line at y, or with under
// the character under x.
func (v *ChatView) posAt(x, y int, under bool) textPos {
	i := max(0, sort.Search(len(v.lay.tops), func(i int) bool { return v.screenY(i) > y })-1)
	m := &v.msgs[i]
	b := v.lay.cache[m.ID]
	top := v.screenY(i) + b.text.Y
	switch {
	case len(selectableUnits(m, b)) == 0 || y < top:
		return textPos{i, 0}
	case y >= top+len(b.lines)*v.lay.lineH:
		return textPos{i, len(b.units)}
	}
	ln := b.lines[(y-top)/v.lay.lineH]
	ms := &gdiMeasurer{dpi: v.canvas.DPI(), cdpi: v.cdpi}
	adv := lineAdv(ms, b.units[ln.start:ln.end], 0) // notices, in italics, are not selectable
	logErr("chat hit test", ms.close())
	x -= b.text.X
	if under { // the unit whose right edge is past x; past the line's end, its last
		return textPos{i, ln.start + min(sort.Search(len(adv), func(j int) bool { return adv[j] > x }), max(0, len(adv)-1))}
	}
	return textPos{i, ln.start + hitOffset(adv, x)}
}

func (v *ChatView) mark(p textPos) textMark { return textMark{v.msgs[p.msg].ID, p.off} }

// selText is the selectable text of message i (selectableUnits).
func (v *ChatView) selText(i int) []uint16 {
	return selectableUnits(&v.msgs[i], v.lay.cache[v.msgs[i].ID])
}

// setSel sets the selection's anchor and caret, repainting on a change.
func (v *ChatView) setSel(a, b textMark) {
	if a != v.selA || b != v.selB {
		v.selA, v.selB = a, b
		logErr("chat repaint", v.canvas.Invalidate())
	}
}

// selection is the selected text's ends in text order; ok is false when nothing is selected (both
// ends are then at message -1 or at one place).
func (v *ChatView) selection() (from, to textPos, ok bool) {
	from, to = orderPos(textPos{v.index(v.selA.id), v.selA.off}, textPos{v.index(v.selB.id), v.selB.off})
	return from, to, from.msg >= 0 && from != to
}

// selectionText is the selected text as Ctrl+C copies it, "" when there is none.
func (v *ChatView) selectionText() string {
	from, to, ok := v.selection()
	if !ok {
		return ""
	}
	texts := make([][]uint16, to.msg+1)
	for i := from.msg; i <= to.msg; i++ {
		texts[i] = v.selText(i)
	}
	return selectedText(texts, from, to)
}

// rangeBetween is msgs[a..b] in chat order, whichever of a and b comes first.
func rangeBetween(msgs []store.Message, a, b int) []store.Message {
	return msgs[max(0, min(a, b)) : min(len(msgs)-1, max(a, b))+1]
}

// pickRange picks the messages from the pick anchor (set to message from when there is none) to
// message to, so a run of Shift+arrows grows the range and reversing shrinks it.
func (v *ChatView) pickRange(from, to int) {
	if v.index(v.pickAnchor) < 0 {
		v.pickAnchor = v.msgs[from].ID
	}
	v.setPicks(rangeBetween(v.msgs, v.index(v.pickAnchor), to))
}

// setPicks makes msgs the picked messages (none for nil) and repaints on a change. Picking drops
// the text selection, which Ctrl+C would otherwise copy first.
func (v *ChatView) setPicks(msgs []store.Message) {
	if len(msgs) > 0 {
		v.setSel(textMark{}, textMark{})
	}
	if len(msgs) == len(v.picked) && !slices.ContainsFunc(msgs, func(m store.Message) bool { return !v.picked[m.ID] }) {
		return
	}
	clear(v.picked)
	for _, m := range msgs {
		v.picked[m.ID] = true
	}
	logErr("chat repaint", errors.Join(v.header.Invalidate(), v.canvas.Invalidate()))
}

// togglePick picks message id, or unpicks it when it is picked.
func (v *ChatView) togglePick(id string) {
	var msgs []store.Message
	for _, m := range v.msgs {
		if v.picked[m.ID] != (m.ID == id) { // the picked ones, with id flipped
			msgs = append(msgs, m)
		}
	}
	v.setPicks(msgs)
}

// copyPicked copies the picked messages, else the focused one, and reports whether there was any text
// to copy. Ctrl+C tries it after the selected text.
func (v *ChatView) copyPicked() bool {
	var msgs []store.Message
	for _, m := range v.msgs {
		if v.picked[m.ID] {
			msgs = append(msgs, m)
		}
	}
	if i := v.focusIndex(); len(msgs) == 0 && i >= 0 {
		msgs = v.msgs[i : i+1]
	}
	text := copyMessages(msgs, v.senderOf, time.Local, v.dir)
	return text != "" && v.toClipboard(text)
}

// buildMenu makes the context menu: Copy copies the selected text, else the picked or focused
// messages; Select picks the focused message.
func (v *ChatView) buildMenu() error {
	m, err := walk.NewMenu()
	if err != nil {
		return err
	}
	v.canvas.Disposing().Attach(m.Dispose)
	for _, it := range []struct {
		text string
		do   func()
	}{
		{"Reply\tCtrl+R", v.replyFocused},
		{"Copy\tCtrl+C", func() { _ = v.copySelection() || v.copyPicked() }},
		{"Select", func() {
			if i := v.focusIndex(); i >= 0 && !v.picked[v.msgs[i].ID] {
				v.togglePick(v.msgs[i].ID)
				v.pickAnchor = v.msgs[i].ID
			}
		}},
	} {
		a := walk.NewAction()
		if err := a.SetText(it.text); err != nil {
			return err
		}
		a.Triggered().Attach(it.do)
		if err := m.Actions().Add(a); err != nil {
			return err
		}
	}
	v.canvas.SetContextMenu(m)
	return nil
}

// menuAt decides where the context menu asked for at screen point (x, y) opens: at the pointer over a
// message (which it focuses, dropping picks and selection that don't include it), at the focused
// message from the keyboard (x and y are -1), nowhere else.
func (v *ChatView) menuAt(x, y int32) (int32, int32, bool) {
	h := v.canvas.Handle()
	pt := win.POINT{X: x, Y: y}
	if x == -1 && y == -1 {
		i := v.focusIndex()
		if i < 0 {
			return 0, 0, false
		}
		b := v.lay.cache[v.msgs[i].ID].box
		pt = win.POINT{X: int32(b.X + b.Width/2), Y: int32(max(0, min(v.view-1, v.screenY(i)+b.Height/2)))}
		win.ClientToScreen(h, &pt)
		return pt.X, pt.Y, true
	}
	win.ScreenToClient(h, &pt)
	i := v.hit(int(pt.X), int(pt.Y))
	if i < 0 {
		return 0, 0, false
	}
	v.setFocus(i)
	focusOn(v.canvas)
	if from, to, ok := v.selection(); !ok || i < from.msg || i > to.msg {
		v.setSel(textMark{}, textMark{})
	}
	if !v.picked[v.msgs[i].ID] {
		v.setPicks(nil)
	}
	return x, y, true
}

// copySelection puts the selected text on the clipboard and reports whether there was any to copy.
// Ctrl+C tries it first.
func (v *ChatView) copySelection() bool {
	text := v.selectionText()
	return text != "" && v.toClipboard(text)
}

// toClipboard replaces the clipboard's content with text, flashing a notice when that fails. It
// returns true: the key was handled either way.
func (v *ChatView) toClipboard(text string) bool {
	cb := walk.Clipboard()
	err := cb.Clear() // SetText alone leaves the last copy's other formats (HTML, ANSI text) behind
	if err == nil {
		err = cb.SetText(text)
	}
	if err != nil {
		logErr("copy", err) // the error only, never the text
		v.flash("Couldn't copy")
	}
	return true
}

// flash shows s in the header for two seconds.
func (v *ChatView) flash(s string) {
	v.notice = s
	v.noticeGen++
	gen := v.noticeGen
	logErr("chat header repaint", v.header.Invalidate())
	time.AfterFunc(2*time.Second, func() {
		v.header.Synchronize(func() {
			if gen == v.noticeGen && !v.header.IsDisposed() {
				v.notice = ""
				logErr("chat header repaint", v.header.Invalidate())
			}
		})
	})
}

// activate acts on a media message: opens it when open (OnOpenMedia decides about the download),
// else asks for its download when it has none.
func (v *ChatView) activate(m store.Message, open bool) {
	switch {
	case v.lay.cache[m.ID].kind == kindText:
	case open && v.onOpenMedia != nil:
		v.onOpenMedia(m)
	case needsDownload(m) && v.onNeedMedia != nil:
		v.onNeedMedia(m)
	}
}

// needsDownload reports whether m's media must be downloaded before it can be shown or opened: it
// never was, or its file is gone (Clear Cache, or deleted by hand).
func needsDownload(m store.Message) bool { return m.MediaPath == "" || media.Missing(m.MediaPath) }

func (v *ChatView) headerKey(k walk.Key) bool {
	if k == walk.KeyReturn || k == walk.KeySpace {
		if v.onOpenInfo != nil && v.chat != "" {
			v.onOpenInfo()
			return true
		}
	}
	return false
}

func (v *ChatView) paintHeader(c *walk.Canvas, _ walk.Rectangle) error {
	dpi, b, p := v.header.DPI(), v.header.ClientBoundsPixels(), v.pal
	s := scaler(dpi)
	bg, err := brushFor(p.Header)
	if err != nil {
		return err
	}
	if err := c.FillRectanglePixels(bg, b); err != nil || v.chat == "" {
		return err
	}
	var errs []error
	if focusCues(v.header) {
		errs = append(errs, focusRect(c, b, s(2), p.Accent))
	}
	av := walk.Rectangle{X: s(16), Y: (b.Height - s(40)) / 2, Width: s(40), Height: s(40)}
	x := av.X + av.Width + s(14)
	name := walk.Rectangle{X: x, Width: b.Width - x - s(16), Height: b.Height}
	var sub string
	user, server, _ := strings.Cut(v.chat, "@")
	title := v.title()
	if server == "s.whatsapp.net" && title != "+"+user {
		sub = "+" + user
	}
	if v.notice != "" {
		sub = v.notice
	}
	if n := len(v.picked); n > 0 && v.notice == "" {
		sub = fmt.Sprintf("%d selected", n)
	}
	if sub != "" {
		name.Y, name.Height = b.Height/2-s(22), s(24)
	}
	errs = append(errs,
		drawAvatar(c, dpi, av, v.name, v.chat, 13),
		drawText(c, dpi, v.title(), 12, 0, theme.Color(p.Text), name, leftText),
		drawText(c, dpi, sub, 9, 0, theme.Color(p.TextSecondary), walk.Rectangle{X: x, Y: b.Height / 2, Width: name.Width, Height: s(20)}, leftText),
	)
	return errors.Join(errs...)
}

func (v *ChatView) paint(c *walk.Canvas, ub walk.Rectangle) error {
	if cd := c.DPI(); cd != v.cdpi { // measured with other fonts than this canvas draws with
		v.cdpi, v.lay.cache = cd, nil
	}
	err := v.relayout(false)
	p, s := v.pal, scaler(v.canvas.DPI())
	bg, berr := brushFor(p.ChatBg)
	if err = errors.Join(err, berr); err != nil {
		return err
	}
	if err := c.FillRectanglePixels(bg, ub); err != nil {
		return err
	}
	focused := focusCues(v.canvas)
	from, to, _ := v.selection()
	first, last := v.visible()
	for i := first; i < last; i++ {
		m, b, dy := &v.msgs[i], v.lay.cache[v.msgs[i].ID], v.screenY(i)
		if dy+b.box.Height+s(3) <= ub.Y || dy-s(3) >= ub.Y+ub.Height {
			continue
		}
		if v.picked[m.ID] || m.ID == v.glow {
			if err := v.paintPick(c, b, dy); err != nil {
				return err
			}
		}
		var sel textLine
		sel.start, sel.end = selSpan(from, to, i, selectableUnits(m, b))
		if err := v.paintBubble(c, m, b, dy, sel); err != nil {
			return err
		}
		if focused && m.ID == v.focus {
			r := b.box
			r = walk.Rectangle{X: r.X - s(3), Y: dy - s(3), Width: r.Width + s(6), Height: r.Height + s(6)}
			if err := focusRect(c, r, s(2), p.Text); err != nil {
				return err
			}
		}
	}
	if t := v.scroll.thumb(); t.Height > 0 {
		return icons.Fill(c, t, theme.Color(p.TextSecondary))
	}
	return nil
}

// paintPick tints the row of picked message bubble b, its top at canvas y dy, with the selection
// colour and a 3 px accent bar at the left edge.
func (v *ChatView) paintPick(c *walk.Canvas, b bubble, dy int) error {
	s, w := scaler(v.canvas.DPI()), v.canvas.ClientBoundsPixels().Width
	tint, err := brushFor(v.pal.Selection)
	if err != nil {
		return err
	}
	bar, err := brushFor(v.pal.Accent)
	if err != nil {
		return err
	}
	row := walk.Rectangle{Y: dy - s(2), Width: w, Height: b.box.Height + s(4)}
	return errors.Join(c.FillRectanglePixels(tint, row), c.FillRectanglePixels(bar, walk.Rectangle{Width: s(3), Y: row.Y, Height: row.Height}))
}

var cardIcons = map[string]icons.Icon{"document": icons.IconDocument, "video": icons.IconVideo, "audio": icons.IconAudio, "sticker": icons.IconEmoji}

// paintBubble paints message m's bubble b with its top at canvas y dy, the units sel of its text
// highlighted as selected.
func (v *ChatView) paintBubble(c *walk.Canvas, m *store.Message, b bubble, dy int, sel textLine) error {
	dpi, p := v.canvas.DPI(), v.pal
	s := scaler(dpi)
	at := func(r walk.Rectangle) walk.Rectangle { r.Y += dy; return r }
	secondary := theme.Color(p.TextSecondary)
	if err := icons.FillRounded(c, at(b.box), s(bubbleRadius), theme.Color(bubbleColor(p, b.out))); err != nil {
		return err
	}
	var errs []error
	if b.avatar {
		avRect := walk.Rectangle{X: b.box.X - s(36), Y: dy, Width: s(28), Height: s(28)}
		errs = append(errs, drawAvatar(c, dpi, avRect, v.lay.sender(m.SenderJID), m.SenderJID, 9))
	}
	if b.named {
		errs = append(errs, drawText(c, dpi, v.lay.sender(m.SenderJID), namePt, walk.FontBold, avatarColor(m.SenderJID), at(b.name), leftText))
	}
	if b.quotedBox.Width > 0 {
		q, accent := at(b.quotedBox), theme.Color(p.Accent)
		line := walk.Rectangle{X: q.X + s(quotePadL), Y: q.Y + s(3), Width: q.Width - s(quotePadL+quotePadR), Height: s(16)}
		bar, err := brushFor(p.Accent)
		errs = append(errs, err,
			icons.FillRoundedARGB(c, q, s(4), 0x26000000|uint32(accent.R())<<16|uint32(accent.G())<<8|uint32(accent.B())))
		if err == nil {
			errs = append(errs, c.FillRectanglePixels(bar, walk.Rectangle{X: q.X, Y: q.Y, Width: s(3), Height: q.Height}))
		}
		errs = append(errs, drawText(c, dpi, b.quotedSender, namePt, walk.FontBold, accent, line, leftText))
		line.Y += s(16)
		errs = append(errs, drawText(c, dpi, b.quotedSnippet, namePt, 0, secondary, line, leftText))
	}
	if b.thumb.Width > 0 {
		r := at(b.thumb)
		if bmp := v.thumbFor(m, b.preview, r.Width, r.Height); bmp != nil {
			errs = append(errs, c.DrawImageStretchedPixels(bmp, r))
		} else {
			errs = append(errs, icons.FillRounded(c, r, s(4), theme.Color(p.ChatBg)))
			if !b.play && m.MediaPath == "" {
				errs = append(errs, icons.Draw(c, icons.IconDownload, square(r.X+r.Width/2, r.Y+r.Height/2, s(36)), secondary))
			}
		}
		if b.play {
			disc := square(r.X+r.Width/2, r.Y+r.Height/2, s(40))
			errs = append(errs,
				icons.FillARGB(c, disc, 0x99000000),
				icons.Draw(c, icons.IconPlay, disc, walk.RGB(255, 255, 255)),
			)
		}
		if b.gifBadge {
			badge := walk.Rectangle{X: r.X + s(8), Y: r.Y + s(8), Width: s(28), Height: s(18)}
			errs = append(errs,
				icons.FillRoundedARGB(c, badge, s(4), 0x99000000),
				drawText(c, dpi, "GIF", 9, walk.FontBold, walk.RGB(255, 255, 255), badge, centerText),
			)
		}
	}
	if b.card.Width > 0 {
		card := at(b.card)
		icon, title, sub, dl := cardParts(card, dpi)
		if m.MediaPath != "" { // no download button: the labels take its room
			title.Width = card.X + card.Width - s(12) - title.X
			sub.Width = title.Width
		}
		name, size := cardLabels(m, b.info)
		errs = append(errs,
			icons.FillRounded(c, card, s(6), theme.Color(p.ChatBg)),
			icons.Draw(c, cardIcons[m.Type], icon, secondary),
			drawText(c, dpi, name, bodyPt, 0, theme.Color(p.Text), title, leftText),
			drawText(c, dpi, size, metaPt, 0, secondary, sub, leftText))
		if m.MediaPath == "" {
			errs = append(errs, icons.Draw(c, icons.IconDownload, dl, secondary))
		}
	}
	if b.text.Width > 0 {
		_, _, muted := bubbleText(m, b.info, v.dir)
		color, style := theme.Color(p.Text), walk.FontStyle(0)
		if muted {
			color, style = secondary, walk.FontItalic
		}
		r := at(b.text)
		r.Height = v.lay.lineH
		ms := &gdiMeasurer{dpi: dpi, cdpi: v.cdpi} // measures only lines with a selection or mentions
		for _, ln := range b.lines {
			lineUnits := b.units[ln.start:ln.end]
			var adv []int
			getAdv := func() []int {
				if adv == nil {
					adv = lineAdv(ms, lineUnits, style)
				}
				return adv
			}
			if lo, hi := max(sel.start, ln.start)-ln.start, min(sel.end, ln.end)-ln.start; lo < hi {
				x := getAdv()
				hl, err := brushFor(p.Selection)
				if err == nil {
					err = c.FillRectanglePixels(hl, walk.Rectangle{X: r.X + widthOf(x, 0, lo), Y: r.Y, Width: widthOf(x, lo, hi), Height: r.Height})
				}
				errs = append(errs, err)
			}
			var lineMentions [][2]int
			if len(b.mentions) > 0 && !muted {
				for _, sp := range b.mentions {
					mLo := max(sp[0], ln.start) - ln.start
					mHi := min(sp[1], ln.end) - ln.start
					if mLo < mHi {
						lineMentions = append(lineMentions, [2]int{mLo, mHi})
					}
				}
			}
			if len(lineMentions) > 0 {
				x := getAdv()
				accentColor := theme.Color(p.Accent)
				lastU := 0
				for _, mSpan := range lineMentions {
					mLo, mHi := mSpan[0], mSpan[1]
					if mLo > lastU {
						segRect := walk.Rectangle{X: r.X + widthOf(x, 0, lastU), Y: r.Y, Width: widthOf(x, lastU, mLo), Height: r.Height}
						errs = append(errs, drawText(c, dpi, string(utf16.Decode(lineUnits[lastU:mLo])), bodyPt, style, color, segRect, walk.TextSingleLine|walk.TextNoPrefix))
					}
					segRect := walk.Rectangle{X: r.X + widthOf(x, 0, mLo), Y: r.Y, Width: widthOf(x, mLo, mHi), Height: r.Height}
					errs = append(errs, drawText(c, dpi, string(utf16.Decode(lineUnits[mLo:mHi])), bodyPt, style, accentColor, segRect, walk.TextSingleLine|walk.TextNoPrefix))
					lastU = mHi
				}
				if lastU < len(lineUnits) {
					segRect := walk.Rectangle{X: r.X + widthOf(x, 0, lastU), Y: r.Y, Width: widthOf(x, lastU, len(lineUnits)), Height: r.Height}
					errs = append(errs, drawText(c, dpi, string(utf16.Decode(lineUnits[lastU:])), bodyPt, style, color, segRect, walk.TextSingleLine|walk.TextNoPrefix))
				}
			} else {
				errs = append(errs, drawText(c, dpi, string(utf16.Decode(lineUnits)), bodyPt, style, color, r, walk.TextSingleLine|walk.TextNoPrefix))
			}
			r.Y += r.Height
		}
		errs = append(errs, ms.close())
	}
	errs = append(errs, drawText(c, dpi, clock(m.Timestamp), metaPt, 0, secondary, at(b.time), rightText))
	if icon, color, ok := tickFor(p, m.Status); ok && b.ticks.Width > 0 {
		errs = append(errs, icons.Draw(c, icon, at(b.ticks), theme.Color(color)))
	}
	return errors.Join(errs...)
}

// gdiMeasurer measures text with the fonts drawText draws it with (pt scaled from the widget's DPI to
// the paint canvas's), on a memory canvas made on first use; close releases it. The first failure
// sticks and is returned by close.
type gdiMeasurer struct {
	dpi, cdpi int
	bmp       *walk.Bitmap
	c         *walk.Canvas
	err       error
}

// ready makes the canvas on first use and reports whether measuring can go on.
func (g *gdiMeasurer) ready() bool {
	if g.err == nil && g.c == nil {
		if g.bmp, g.err = walk.NewBitmapForDPI(walk.Size{Width: 1, Height: 1}, g.cdpi); g.err == nil {
			g.c, g.err = walk.NewCanvasFromImage(g.bmp)
		}
	}
	return g.err == nil
}

func (g *gdiMeasurer) measure(text string, pt int, style walk.FontStyle, maxW int) (int, int) {
	if text == "" || !g.ready() {
		return 0, 0
	}
	r, _, err := g.c.MeasureTextPixels(text, textFont(pt, style, g.dpi, g.cdpi), walk.Rectangle{Width: maxW, Height: 1 << 20}, msgFormat|walk.TextCalcRect)
	if err != nil {
		g.err = err
		return 0, 0
	}
	return r.Width, r.Height
}

// advances is the cumulative width of units after each unit (GetTextExtentExPointW), all zero on
// failure.
func (g *gdiMeasurer) advances(units []uint16, pt int, style walk.FontStyle) []int {
	x := make([]int, len(units))
	if len(units) == 0 || !g.ready() {
		return x
	}
	f := hfont(textFont(pt, style, g.dpi, g.cdpi), g.cdpi)
	if f == 0 {
		g.err = errors.New("ui: CreateFontIndirect failed")
		return x
	}
	defer win.DeleteObject(win.HGDIOBJ(f))
	hdc := g.c.HDC()
	defer win.SelectObject(hdc, win.SelectObject(hdc, win.HGDIOBJ(f)))
	dx := make([]int32, min(len(units), advChunk))
	var size win.SIZE
	for i := 0; i < len(units); {
		n := min(len(units)-i, advChunk)
		if i+n < len(units) && units[i+n-1] >= 0xd800 && units[i+n-1] < 0xdc00 {
			n-- // a surrogate pair stays in one call
		}
		if !win.GetTextExtentExPoint(hdc, &units[i], int32(n), 0, nil, &dx[0], &size) {
			g.err = errors.New("ui: GetTextExtentExPoint failed")
			return x
		}
		base := 0
		if i > 0 {
			base = x[i-1]
		}
		for j, d := range dx[:n] {
			x[i+j] = base + int(d)
		}
		i += n
	}
	return x
}

// advChunk is the most units advances measures in one call: GDI fails on text with emoji longer
// than about 43,000 units.
const advChunk = 1 << 14

// hfont makes the GDI font walk draws f with on a canvas at dpi, as walk's Font.createForDPI does
// (walk keeps its handles private); the caller deletes it.
func hfont(f *walk.Font, dpi int) win.HFONT {
	lf := win.LOGFONT{LfHeight: -win.MulDiv(int32(f.PointSize()), int32(dpi), 72), LfWeight: win.FW_NORMAL,
		LfCharSet: win.DEFAULT_CHARSET, LfOutPrecision: win.OUT_TT_PRECIS, LfClipPrecision: win.CLIP_DEFAULT_PRECIS,
		LfQuality: win.CLEARTYPE_QUALITY, LfPitchAndFamily: win.VARIABLE_PITCH | win.FF_SWISS}
	if f.Bold() {
		lf.LfWeight = win.FW_BOLD
	}
	if f.Italic() {
		lf.LfItalic = 1
	}
	copy(lf.LfFaceName[:len(lf.LfFaceName)-1], utf16.Encode([]rune(f.Family())))
	return win.CreateFontIndirect(&lf)
}

func (g *gdiMeasurer) close() error {
	if g.c != nil {
		g.c.Dispose()
	}
	if g.bmp != nil {
		g.bmp.Dispose()
	}
	return g.err
}

// canvasDPI is the DPI walk's buffered paint canvas reports: a memory DC's (the system DPI). paint
// corrects it should the canvas report another.
func canvasDPI() int {
	hdc := win.CreateCompatibleDC(0)
	if hdc == 0 {
		return 96
	}
	defer win.DeleteDC(hdc)
	return int(win.GetDeviceCaps(hdc, win.LOGPIXELSX))
}
