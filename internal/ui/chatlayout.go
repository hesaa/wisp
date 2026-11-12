package ui

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/lxn/walk"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"wa-go/internal/store"
	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
)

// The chat pane's message layout: a pure function of the messages, the view width and DPI and a text
// measurer, so it is tested without a window. Sizes are 1/96 inch, scaled to the DPI.
const (
	bubbleEdge   = 18  // between bubbles and the sides of the view
	bubbleMaxPct = 65  // widest bubble, percent of the view width
	bubblePadX   = 9   // text padding
	bubblePadY   = 6   // text padding, top and bottom
	bubbleInset  = 3   // around thumbnails and cards
	bubbleRadius = 8   // corner radius
	thumbSide    = 240 // thumbnails fit a square this big
	cardW, cardH = 280, 64
	nameH        = 18 // sender name line in groups
	metaH        = 15 // time and ticks
	metaGap      = 10 // between a text's last line and its time
	tickW        = 16
	runGap       = 2 // between messages of one sender in a row
	newRunGap    = 8 // between runs
	listPad      = 8 // above the first message and below the last

	bodyPt, namePt, metaPt = 10, 9, 8
)

// measurer measures text for layout.
type measurer interface {
	// measure is the size, in pixels, of text in Segoe UI at pt points word-wrapped at maxW pixels.
	measure(text string, pt int, style walk.FontStyle, maxW int) (w, h int)
	// advances is the width, in pixels, of units in Segoe UI at pt points after each unit.
	advances(units []uint16, pt int, style walk.FontStyle) []int
}

type bubbleKind int

const (
	kindText  bubbleKind = iota // text, deleted and unsupported messages
	kindImage                   // a thumbnail box, caption below
	kindCard                    // video, audio, document, sticker without preview: icon, name, size
)

// mediaInfo is what a bubble shows of a message's MediaMeta.
type mediaInfo struct {
	size    uint64
	name    string // document file name
	w, h    int    // pixel size of an image, video or sticker; 0 when unknown
	preview []byte // embedded thumbnail (JPEG; PNG for stickers); not kept in layouts
	gif     bool   // a video sent as a GIF
	voice   bool   // audio recorded in the app (a voice message)
}

// parseMedia reads a message's MediaMeta, the marshaled media sub-message of its type.
func parseMedia(m *store.Message) (mediaInfo, error) {
	if len(m.MediaMeta) == 0 {
		return mediaInfo{}, nil
	}
	switch m.Type {
	case "image":
		var x waE2E.ImageMessage
		err := proto.Unmarshal(m.MediaMeta, &x)
		return mediaInfo{size: x.GetFileLength(), w: int(x.GetWidth()), h: int(x.GetHeight()), preview: x.GetJPEGThumbnail()}, err
	case "video":
		var x waE2E.VideoMessage
		err := proto.Unmarshal(m.MediaMeta, &x)
		return mediaInfo{size: x.GetFileLength(), w: int(x.GetWidth()), h: int(x.GetHeight()), preview: x.GetJPEGThumbnail(), gif: x.GetGifPlayback()}, err
	case "audio":
		var x waE2E.AudioMessage
		err := proto.Unmarshal(m.MediaMeta, &x)
		return mediaInfo{size: x.GetFileLength(), voice: x.GetPTT()}, err
	case "document":
		var x waE2E.DocumentMessage
		err := proto.Unmarshal(m.MediaMeta, &x)
		return mediaInfo{size: x.GetFileLength(), name: x.GetFileName(), preview: x.GetJPEGThumbnail()}, err
	case "sticker":
		var x waE2E.StickerMessage
		err := proto.Unmarshal(m.MediaMeta, &x)
		return mediaInfo{size: x.GetFileLength(), w: int(x.GetWidth()), h: int(x.GetHeight()), preview: x.GetPngThumbnail()}, err
	}
	return mediaInfo{}, nil
}

func kindOf(m *store.Message, info mediaInfo) bubbleKind {
	switch m.Type {
	case "image":
		return kindImage
	case "sticker":
		if len(info.preview) > 0 {
			return kindImage
		}
		return kindCard
	case "video", "audio", "document":
		return kindCard
	}
	return kindText
}

// bubbleText is a bubble's text (body, caption or notice), its mention spans, and whether it is a muted notice.
func bubbleText(m *store.Message, info mediaInfo, d ...*Directory) (string, [][2]int, bool) {
	var dir *Directory
	if len(d) > 0 {
		dir = d[0]
	}
	switch m.Type {
	case "revoked":
		return "This message was deleted", nil, true
	case "unsupported":
		return "Unsupported message", nil, true
	case "audio", "sticker":
		return "", nil, false
	case "document":
		if info.name == "" || m.Body == info.name { // the body is the file name, shown on the card
			return "", nil, false
		}
	}
	trimmed := strings.TrimRight(m.Body, " \t\r\n")
	rewritten, spans := rewriteMentions(trimmed, m.Mentions, dir)
	return rewritten, spans, false
}

// selectableUnits is the text of message m's bubble b a selection may take: its text or caption,
// none for a notice ("This message was deleted").
func selectableUnits(m *store.Message, b bubble) []uint16 {
	if _, _, muted := bubbleText(m, b.info); muted {
		return nil
	}
	return b.units
}

// cardLabels are a media card's title and its size line.
func cardLabels(m *store.Message, info mediaInfo) (title, sub string) {
	size := humanSize(info.size)
	switch m.Type {
	case "document":
		title = cmp.Or(info.name, m.Body, "Document")
		ext := strings.ToUpper(strings.TrimPrefix(filepath.Ext(title), "."))
		return title, strings.Join(slices.DeleteFunc([]string{size, ext}, func(s string) bool { return s == "" }), " · ")
	case "video":
		return "Video", size
	case "audio":
		return "Audio", size
	}
	return "Sticker", size
}

// humanSize formats a file size like "245 KB"; "" for an unknown (zero) size.
func humanSize(n uint64) string {
	switch {
	case n == 0:
		return ""
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%d KB", (n+512)>>10)
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
}

func bubbleColor(p theme.Palette, out bool) string {
	if out {
		return p.OutgoingBubble
	}
	return p.IncomingBubble
}

// tickFor is the delivery tick of an outgoing message in status; ok is false when it has none.
func tickFor(p theme.Palette, status string) (icon icons.Icon, color string, ok bool) {
	switch status {
	case store.StatusSent:
		return icons.IconCheck, p.TextSecondary, true
	case store.StatusDelivered:
		return icons.IconDoubleCheck, p.TextSecondary, true
	case store.StatusRead:
		return icons.IconDoubleCheck, p.TickRead, true
	}
	return 0, "", false
}

func clock(ts int64) string { return time.Unix(ts, 0).Format("15:04") }

// fitBox sizes a w x h image to fit a side x side square, no wider than maxW, keeping its aspect;
// unknown sizes give the square. The short side stays at least a quarter side (the painter crops).
func fitBox(w, h, side, maxW int) (int, int) {
	if w <= 0 || h <= 0 {
		w, h = 1, 1
	}
	bw, bh := side, side
	if w >= h {
		bh = side * h / w
	} else {
		bw = side * w / h
	}
	if bw > maxW {
		bw, bh = maxW, bh*maxW/bw
	}
	lo := min(side/4, maxW)
	return max(bw, lo), max(bh, lo)
}

// bubble is one message's laid-out geometry in pixels: x from the view's left, y from the bubble's
// top (box.Y is 0). Zero rectangles are parts it does not have.
type bubble struct {
	kind     bubbleKind
	out      bool
	named    bool      // shows the sender's name
	preview  bool      // MediaMeta has an embedded thumbnail
	info     mediaInfo // without preview bytes
	box      walk.Rectangle
	name     walk.Rectangle // sender name, incoming group messages starting a run
	text     walk.Rectangle // body, caption or notice: widest line by lines × line height
	units    []uint16       // the text, UTF-16
	mentions [][2]int       // UTF-16 spans of rewritten mentions
	lines    []textLine     // the text's lines, top to bottom
	thumb    walk.Rectangle // image box
	card     walk.Rectangle // media card
	time     walk.Rectangle
	ticks    walk.Rectangle // outgoing only
}

// cardParts splits a media card into its type icon, title, size line and download button.
func cardParts(card walk.Rectangle, dpi int) (icon, title, sub, download walk.Rectangle) {
	s := scaler(dpi)
	icon = walk.Rectangle{X: card.X + s(10), Y: card.Y + (card.Height-s(40))/2, Width: s(40), Height: s(40)}
	download = walk.Rectangle{X: card.X + card.Width - s(42), Y: card.Y + (card.Height-s(32))/2, Width: s(32), Height: s(32)}
	x := icon.X + icon.Width + s(10)
	w := download.X - s(8) - x
	return icon, walk.Rectangle{X: x, Y: card.Y + s(12), Width: w, Height: s(22)},
		walk.Rectangle{X: x, Y: card.Y + s(34), Width: w, Height: s(18)}, download
}

// chatLayout lays out a chat's messages and keeps each bubble, by message ID, until the width or
// DPI changes or the message is forgotten, so text is measured once, not on every paint.
type chatLayout struct {
	dir        *Directory
	group      bool                    // names the senders of incoming messages
	senderName func(jid string) string // a sender's display name, "" when unknown; may be nil
	width, dpi int                     // what the cache was laid out for
	lineH      int                     // height of one line of body text
	cache      map[string]bubble
	tops       []int // each message's top, pixels from the top of the content
	total      int   // content height
}

// reset drops everything, for another chat.
func (l *chatLayout) reset() { *l = chatLayout{dir: l.dir, senderName: l.senderName} }

// forget drops a changed message's bubble, to be laid out again.
func (l *chatLayout) forget(id string) { delete(l.cache, id) }

func (l *chatLayout) sender(jid string) string {
	name := ""
	if l.senderName != nil {
		name = l.senderName(jid)
	}
	return l.dir.Name(jid, name)
}

// update lays msgs (oldest first) out top to bottom for a view width pixels wide at dpi,
// measuring only bubbles not cached for that width and DPI.
func (l *chatLayout) update(msgs []store.Message, width, dpi int, ms measurer) {
	if l.cache == nil || width != l.width || dpi != l.dpi {
		l.width, l.dpi, l.lineH, l.cache = width, dpi, 0, map[string]bubble{}
	}
	s := scaler(dpi)
	l.tops = l.tops[:0]
	y := s(listPad)
	for i := range msgs {
		m := &msgs[i]
		run := i > 0 && msgs[i-1].FromMe == m.FromMe && msgs[i-1].SenderJID == m.SenderJID
		named := l.group && !m.FromMe && !run
		b, ok := l.cache[m.ID]
		if !ok || b.named != named {
			b = l.layout(m, named, ms)
			l.cache[m.ID] = b
		}
		switch {
		case i == 0:
		case run:
			y += s(runGap)
		default:
			y += s(newRunGap)
		}
		l.tops = append(l.tops, y)
		y += b.box.Height
	}
	l.total = y + s(listPad)
}

// layout lays one message out; its bubble's x is final, its y relative to the bubble's top.
func (l *chatLayout) layout(m *store.Message, named bool, ms measurer) bubble {
	s := scaler(l.dpi)
	info, err := parseMedia(m)
	logErr("media details of message "+m.ID, err)
	b := bubble{kind: kindOf(m, info), out: m.FromMe, named: named, preview: len(info.preview) > 0}
	info.preview = nil
	b.info = info
	if l.lineH == 0 {
		_, l.lineH = ms.measure("M", bodyPt, 0, 1<<20)
	}
	maxW := l.width * bubbleMaxPct / 100
	padX := s(bubblePadX)
	text, spans, muted := bubbleText(m, info, l.dir)
	b.mentions = spans
	style := walk.FontStyle(0)
	if muted {
		style = walk.FontItalic
	}
	timeW, _ := ms.measure(clock(m.Timestamp), metaPt, 0, 1<<20)
	metaW := timeW
	if m.FromMe {
		metaW += s(tickW) + s(3)
	}

	// Build top to bottom with x relative to the bubble's left edge.
	y, nameW, boxW := s(bubblePadY), 0, 0
	if named {
		nameW, _ = ms.measure(l.sender(m.SenderJID), namePt, walk.FontBold, maxW-2*padX)
		b.name = walk.Rectangle{X: padX, Y: y, Height: s(nameH)}
		y += s(nameH)
	}
	if b.kind != kindText {
		if !named {
			y = s(bubbleInset)
		}
		media := walk.Rectangle{X: s(bubbleInset), Y: y}
		if b.kind == kindImage {
			media.Width, media.Height = fitBox(info.w, info.h, s(thumbSide), maxW-2*s(bubbleInset))
			b.thumb = media
		} else {
			media.Width, media.Height = min(s(cardW), maxW-2*s(bubbleInset)), s(cardH)
			b.card = media
		}
		y += media.Height
		boxW = media.Width + 2*s(bubbleInset)
		maxW = boxW // a caption wraps to the media's width
		if text != "" {
			y += s(bubblePadY)
		}
	}
	metaY := y + s(2) // own row under media without a caption
	if text != "" {
		var x []int
		b.units = utf16.Encode([]rune(text))
		b.lines = wrapText(b.units, maxW-2*padX, func(u []uint16) []int { x = ms.advances(u, bodyPt, style); return x })
		tw, th := textWidth(b.units, x, b.lines, maxW-2*padX), len(b.lines)*l.lineH
		b.text = walk.Rectangle{X: padX, Y: y, Width: tw, Height: th}
		inline := len(b.lines) == 1 && tw+s(metaGap)+metaW <= maxW-2*padX
		if inline {
			metaY = y + th - s(metaH)
			tw += s(metaGap) + metaW
		} else {
			metaY = y + th
		}
		y = max(y+th, metaY+s(metaH))
		boxW = max(boxW, tw+2*padX)
	} else {
		y = metaY + s(metaH)
	}
	y += s(bubblePadY)
	boxW = max(boxW, nameW+2*padX, metaW+2*padX)
	if named {
		b.name.Width = min(nameW, boxW-2*padX)
	}
	right := boxW - padX
	if m.FromMe {
		b.ticks = walk.Rectangle{X: right - s(tickW), Y: metaY, Width: s(tickW), Height: s(metaH)}
		right = b.ticks.X - s(3)
	}
	b.time = walk.Rectangle{X: right - timeW, Y: metaY, Width: timeW, Height: s(metaH)}

	b.box = walk.Rectangle{X: s(bubbleEdge), Width: boxW, Height: y}
	if m.FromMe {
		b.box.X = l.width - s(bubbleEdge) - boxW
	}
	for _, r := range []*walk.Rectangle{&b.name, &b.text, &b.thumb, &b.card, &b.time, &b.ticks} {
		if r.Width > 0 {
			r.X += b.box.X
		}
	}
	return b
}

// shouldLoadPrevious decides whether a view scrolled to off (pixels from the top) asks for the
// previous page: near the top, not already loading, history not exhausted.
func shouldLoadPrevious(off, threshold int, loading, exhausted bool) bool {
	return !loading && !exhausted && off <= threshold
}

// anchorAfterPrepend is the scroll offset that keeps the messages on screen where they were after
// older messages grew the content from oldTotal to newTotal. Content shorter than the view sits at
// its bottom.
func anchorAfterPrepend(off, oldTotal, newTotal, view int) int {
	pad := func(total int) int { return max(0, view-total) }
	return off + newTotal - oldTotal + pad(newTotal) - pad(oldTotal)
}

// followBottom is the scroll offset after the content or view changed size: on the end when the
// view showed the end, else where it was.
func followBottom(off, oldTotal, oldView, newTotal, newView int) int {
	if off >= oldTotal-oldView {
		return max(0, newTotal-newView)
	}
	return off
}

// lru keeps the max most recently used values; evict releases each value it drops.
type lru[V any] struct {
	max   int
	keys  []string // least recently used first
	vals  map[string]V
	evict func(V)
}

func newLRU[V any](size int, evict func(V)) *lru[V] {
	return &lru[V]{max: size, vals: map[string]V{}, evict: evict}
}

func (l *lru[V]) touch(k string) {
	i := slices.Index(l.keys, k)
	l.keys = append(slices.Delete(l.keys, i, i+1), k)
}

func (l *lru[V]) get(k string) (V, bool) {
	v, ok := l.vals[k]
	if ok {
		l.touch(k)
	}
	return v, ok
}

// put stores v under k, releasing a value it replaces and the least recently used beyond max.
func (l *lru[V]) put(k string, v V) {
	if old, ok := l.vals[k]; ok {
		l.evict(old)
		l.touch(k)
	} else {
		l.keys = append(l.keys, k)
	}
	l.vals[k] = v
	for len(l.keys) > l.max {
		oldest := l.keys[0]
		l.keys = slices.Delete(l.keys, 0, 1)
		l.evict(l.vals[oldest])
		delete(l.vals, oldest)
	}
}

func (l *lru[V]) clear() {
	for _, k := range l.keys {
		l.evict(l.vals[k])
	}
	l.keys = l.keys[:0]
	clear(l.vals)
}
