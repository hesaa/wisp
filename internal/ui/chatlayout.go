package ui

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/lxn/walk"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"wa-go/internal/linkify"
	"wa-go/internal/store"
	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
	"wa-go/internal/whatsapp"
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
	stickerSide  = 140 // stickers a smaller one
	cardW, cardH = 280, 64
	nameH        = 18 // sender name line in groups
	metaH        = 15 // time and ticks
	metaGap      = 10 // between a text's last line and its time
	tickW        = 16
	runGap       = 2  // between messages of one sender in a row
	newRunGap    = 8  // between runs
	listPad      = 8  // above the first message and below the last
	quoteH       = 38 // quoted-message card of a reply: author over one line of the original
	quoteGap     = 4  // under the card
	quotePadL    = 10 // card text from the card's left edge, past the 3 px accent bar
	quotePadR    = 8
	quoteMaxLen  = 120 // runes of the original kept for the card's line

	linkCardMinW  = 280
	linkCardPadX  = 10
	linkCardPadY  = 8
	linkCardGap   = 6
	linkThumbMinH = 80
	linkThumbMaxH = 150

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
	kindText   bubbleKind = iota // text, deleted and unsupported messages
	kindImage                    // a thumbnail box, caption below
	kindCard                     // video, audio, document, sticker without preview: icon, name, size
	kindSystem                   // centered system notices (e.g. member removed)
)

// mediaInfo is what a bubble shows of a message's MediaMeta.
type mediaInfo struct {
	size    uint64
	name    string // document file name
	w, h    int    // pixel size of an image, video or sticker; 0 when unknown
	preview []byte // embedded thumbnail (JPEG; PNG for stickers); not kept in layouts
	gif     bool   // a video sent as a GIF
	voice   bool   // audio recorded in the app (a voice message)
	note    bool   // a round video note
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
		return mediaInfo{size: x.GetFileLength(), w: int(x.GetWidth()), h: int(x.GetHeight()), preview: x.GetJPEGThumbnail(), gif: x.GetGifPlayback(), note: whatsapp.IsVideoNote(&x)}, err
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
	case "system":
		return kindSystem
	case "image":
		return kindImage
	case "sticker": // its file is a picture: shown once downloaded, its embedded preview until then
		return kindImage
	case "video": // a preview picture, or once downloaded the thumbnail Windows makes of the file
		if len(info.preview) > 0 || m.MediaPath != "" {
			return kindImage
		}
		return kindCard
	case "audio", "document":
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
	case "system":
		return m.Body, nil, false
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
		if info.gif {
			return "GIF", size
		}
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
	avatar   bool      // group incoming message starting a run shows sender avatar
	preview  bool      // MediaMeta has an embedded thumbnail
	play     bool      // video or GIF preview
	gifBadge bool      // GIF preview
	info     mediaInfo // without preview bytes
	box      walk.Rectangle
	name     walk.Rectangle   // sender name, incoming group messages starting a run
	text     walk.Rectangle   // body, caption or notice: widest line by lines × line height
	units      []uint16         // the text, UTF-16
	mentions   [][2]int         // UTF-16 spans of rewritten mentions
	linkSpans  [][2]int         // UTF-16 spans of detected URLs
	phoneSpans [][2]int         // UTF-16 spans of detected phone numbers
	styles     []walk.FontStyle // each unit's formatting (formatText), nil for none
	indents    []int            // each line's indent in pixels (wrapBlocks), nil for none
	// an album (album.go): how many photos it shows, from this message on, whether each has an
	// embedded preview, and their total size; hidden for the photos after the first
	album        int
	albumPreview []bool
	albumBytes   uint64
	hidden       bool
	lines        []textLine     // the text's lines, top to bottom
	thumb        walk.Rectangle // image box
	card         walk.Rectangle // media card
	time         walk.Rectangle
	ticks        walk.Rectangle // outgoing only
	// a reply's card of the message it answers, above everything but the sender's name
	quotedBox                   walk.Rectangle
	quotedSender, quotedSnippet string
	// link preview card, below quotedBox and above text; zero when absent or disabled
	linkCard, linkThumb, linkTitle, linkDesc, linkDomain walk.Rectangle
	linkTitleText, linkDescText, linkDomainText          string
	linkTargetURL                                        string
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
	dir          *Directory
	group        bool                    // names the senders of incoming messages
	linkPreviews bool                    // render link preview cards in bubbles
	senderName   func(jid string) string // a sender's display name, "" when unknown; may be nil
	width, dpi   int                     // what the cache was laid out for
	lineH        int                     // height of one line of body text
	cache        map[string]bubble
	tops         []int // each message's top, pixels from the top of the content
	total        int   // content height
}

// reset drops everything, for another chat.
func (l *chatLayout) reset() {
	*l = chatLayout{dir: l.dir, senderName: l.senderName, linkPreviews: l.linkPreviews}
}

// forget drops a changed message's bubble, to be laid out again.
func (l *chatLayout) forget(id string) { delete(l.cache, id) }

func (l *chatLayout) sender(jid string) string {
	name := ""
	if l.senderName != nil {
		name = l.senderName(jid)
	}
	return l.dir.Name(jid, name)
}

var quotedKinds = map[string]string{"image": "Photo", "video": "Video", "audio": "Audio", "document": "Document", "sticker": "Sticker"}

// quoteLabels are the author and the one-line excerpt on reply m's card of the message it answers.
func (l *chatLayout) quoteLabels(m *store.Message) (sender, snippet string) {
	sender = "You"
	if user, _, _ := strings.Cut(m.QuotedSender, "@"); m.AccountID == "" || user != m.AccountID {
		sender = l.sender(m.QuotedSender)
	}
	snippet, _, _ = formatText(m.QuotedBody, nil) // *bold* and the like show without their markers
	snippet = strings.Join(strings.Fields(snippet), " ")
	if r := []rune(snippet); len(r) > quoteMaxLen {
		snippet = string(r[:quoteMaxLen]) + "…"
	}
	return sender, cmp.Or(snippet, quotedKinds[m.QuotedType], "Message")
}

// update lays msgs (oldest first) out top to bottom for a view width pixels wide at dpi,
// measuring only bubbles not cached for that width and DPI.
func (l *chatLayout) update(msgs []store.Message, width, dpi int, ms measurer) {
	if l.cache == nil || width != l.width || dpi != l.dpi {
		l.width, l.dpi, l.lineH, l.cache = width, dpi, 0, map[string]bubble{}
	}
	s := scaler(dpi)
	l.tops = l.tops[:0]
	y, rest := s(listPad), 0 // rest: hidden photos still to come of the album above
	for i := range msgs {
		m := &msgs[i]
		run := i > 0 && msgs[i-1].FromMe == m.FromMe && msgs[i-1].SenderJID == m.SenderJID
		named := l.group && !m.FromMe && !run
		hidden, n := rest > 0, 0
		if hidden {
			rest--
		} else if n = albumLen(msgs, i); n > 0 {
			rest = n - 1
		}
		b, ok := l.cache[m.ID]
		if !ok || b.hidden != hidden || (!hidden && (b.named != named || b.album != n)) {
			if hidden {
				b = bubble{hidden: true}
			} else {
				b = l.layout(m, named, ms, msgs[i:i+n])
			}
			l.cache[m.ID] = b
		}
		switch {
		case hidden:
			l.tops = append(l.tops, y)
			continue
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
// album holds the album's photos when m leads one, m first.
func (l *chatLayout) layout(m *store.Message, named bool, ms measurer, album []store.Message) bubble {
	s := scaler(l.dpi)
	info, err := parseMedia(m)
	logErr("media details of message "+m.ID, err)
	b := bubble{kind: kindOf(m, info), out: m.FromMe, named: named, avatar: l.group && !m.FromMe && named, preview: len(info.preview) > 0}
	if b.kind == kindSystem {
		b.out = false
		b.named = false
		b.avatar = false
		text := m.Body
		maxW := l.width * bubbleMaxPct / 100
		tw, th := ms.measure(text, 9, 0, maxW-2*s(12))
		boxW := tw + 2*s(12)
		boxH := th + 2*s(6)
		boxX := max(0, (l.width-boxW)/2)
		b.box = walk.Rectangle{X: boxX, Width: boxW, Height: boxH}
		b.text = walk.Rectangle{X: boxX + s(12), Y: s(6), Width: tw, Height: th}
		b.units = utf16.Encode([]rune(text))
		b.lines = []textLine{{end: len(b.units)}}
		return b
	}
	if b.kind == kindImage && m.Type == "video" {
		b.play = true
		b.gifBadge = info.gif
	}
	info.preview = nil
	b.info = info
	if l.lineH == 0 {
		_, l.lineH = ms.measure("M", bodyPt, 0, 1<<20)
	}
	maxW := l.width * bubbleMaxPct / 100
	padX := s(bubblePadX)
	text, spans, muted := bubbleText(m, info, l.dir)
	if !muted {
		text, b.mentions, b.linkSpans, b.phoneSpans, b.styles = formatBubbleText(text, spans)
	} else {
		b.mentions = spans
	}
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
	quoteW := 0
	if m.QuotedID != "" {
		b.quotedSender, b.quotedSnippet = l.quoteLabels(m)
		pad := s(quotePadL + quotePadR)
		sw, _ := ms.measure(b.quotedSender, namePt, walk.FontBold, 1<<20)
		tw, _ := ms.measure(b.quotedSnippet, namePt, 0, 1<<20)
		quoteW = min(max(sw, tw), maxW-2*padX-pad) + pad
		b.quotedBox = walk.Rectangle{X: padX, Y: y, Height: s(quoteH)}
		y += s(quoteH + quoteGap)
	}
	if b.kind != kindText {
		if !named && m.QuotedID == "" {
			y = s(bubbleInset)
		}
		media := walk.Rectangle{X: s(bubbleInset), Y: y}
		if b.kind == kindImage {
			side := thumbSide
			if m.Type == "sticker" {
				side = stickerSide
			}
			media.Width, media.Height = fitBox(info.w, info.h, s(side), maxW-2*s(bubbleInset))
			if info.note { // a circle: square box, the picture cropped to it
				media.Width = min(s(side), maxW-2*s(bubbleInset))
				media.Height = media.Width
			}
			if len(album) > 0 {
				media.Width = min(s(thumbSide), maxW-2*s(bubbleInset))
				media.Height = media.Width
				b.album = len(album)
				for k := range album {
					ai, _ := parseMedia(&album[k])
					b.albumBytes += ai.size
					if k < 4 {
						b.albumPreview = append(b.albumPreview, len(ai.preview) > 0)
					}
				}
			}
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

	var textW, textH int
	var inline bool
	if text != "" {
		var x []int
		b.units = utf16.Encode([]rune(text))
		b.lines, b.indents, x = wrapBlocks(b.units, maxW-2*padX, b.styles, s, func(u []uint16) []int { return styledAdv(ms, u, b.styles, style) })
		textH = len(b.lines) * l.lineH
		for li, ln := range b.lines {
			ind := lineIndent(b.indents, li)
			textW = max(textW, ind+textWidth(b.units, x, []textLine{ln}, maxW-2*padX-ind))
		}
		inline = len(b.lines) == 1 && textW+s(metaGap)+metaW <= maxW-2*padX
		twWithMeta := textW
		if inline {
			twWithMeta += s(metaGap) + metaW
		}
		boxW = max(boxW, twWithMeta+2*padX)
	}
	boxW = max(boxW, nameW+2*padX, metaW+2*padX)
	if m.QuotedID != "" && b.kind == kindText { // media keeps its width: the card's line is cut short instead
		boxW = max(boxW, quoteW+2*padX)
	}

	hasLinkCard := b.kind == kindText && !muted && l.linkPreviews &&
		(m.LinkTitle != "" || m.LinkDesc != "" || len(m.LinkThumb) > 0)
	if hasLinkCard {
		boxW = max(boxW, min(s(linkCardMinW)+2*s(bubbleInset), maxW))
		if !named && m.QuotedID == "" {
			y = s(bubbleInset)
		}
		cw := max(1, boxW-2*s(bubbleInset))
		cy := y
		if len(m.LinkThumb) > 0 {
			th := min(s(linkThumbMaxH), max(s(linkThumbMinH), cw*9/16))
			b.linkThumb = walk.Rectangle{X: s(bubbleInset), Y: cy, Width: cw, Height: th}
			cy += th
		}
		tx := s(bubbleInset) + s(linkCardPadX)
		tw := max(1, cw-2*s(linkCardPadX))
		cy += s(linkCardPadY)
		b.linkTitleText = singleLineSpace(m.LinkTitle)
		b.linkDescText = singleLineSpace(m.LinkDesc)
		b.linkTargetURL = linkify.FirstURL(m.Body) // the sender's MatchedText only when the body shows it
		if m.LinkURL != "" && linkInBody(m.Body, m.LinkURL) {
			b.linkTargetURL = m.LinkURL
		}
		b.linkDomainText = linkify.DisplayDomain(cmp.Or(m.LinkCanonical, m.LinkURL, b.linkTargetURL))
		if b.linkTitleText != "" {
			_, l1 := ms.measure("M", namePt, walk.FontBold, 1<<20)
			_, th := ms.measure(b.linkTitleText, namePt, walk.FontBold, tw)
			th = min(th, 2*l1)
			b.linkTitle = walk.Rectangle{X: tx, Y: cy, Width: tw, Height: th}
			cy += th + s(2)
		}
		if b.linkDescText != "" {
			_, d1 := ms.measure("M", metaPt, 0, 1<<20)
			_, dh := ms.measure(b.linkDescText, metaPt, 0, tw)
			dh = min(dh, 2*d1)
			b.linkDesc = walk.Rectangle{X: tx, Y: cy, Width: tw, Height: dh}
			cy += dh + s(2)
		}
		if b.linkDomainText != "" {
			_, domH := ms.measure("M", metaPt, 0, 1<<20)
			b.linkDomain = walk.Rectangle{X: tx, Y: cy, Width: tw, Height: domH}
			cy += domH
		}
		cy += s(linkCardPadY)
		b.linkCard = walk.Rectangle{X: s(bubbleInset), Y: y, Width: cw, Height: cy - y}
		y = cy
		if text != "" {
			y += s(linkCardGap)
		}
	}

	metaY := y + s(2) // own row under media without a caption
	if text != "" {
		b.text = walk.Rectangle{X: padX, Y: y, Width: textW, Height: textH}
		if inline {
			metaY = y + textH - s(metaH)
		} else {
			metaY = y + textH
		}
		y = max(y+textH, metaY+s(metaH))
	} else {
		y = metaY + s(metaH)
	}
	y += s(bubblePadY)
	if m.QuotedID != "" {
		b.quotedBox.Width = boxW - 2*padX
	}
	if named {
		b.name.Width = min(nameW, boxW-2*padX)
	}
	right := boxW - padX
	if m.FromMe {
		b.ticks = walk.Rectangle{X: right - s(tickW), Y: metaY, Width: s(tickW), Height: s(metaH)}
		right = b.ticks.X - s(3)
	}
	b.time = walk.Rectangle{X: right - timeW, Y: metaY, Width: timeW, Height: s(metaH)}

	boxX := s(bubbleEdge)
	if l.group && !m.FromMe {
		boxX = s(bubbleEdge + 36)
	}
	b.box = walk.Rectangle{X: boxX, Width: boxW, Height: y}
	if m.FromMe {
		b.box.X = l.width - s(bubbleEdge) - boxW
	}
	for _, r := range []*walk.Rectangle{&b.name, &b.text, &b.thumb, &b.card, &b.time, &b.ticks, &b.quotedBox,
		&b.linkCard, &b.linkThumb, &b.linkTitle, &b.linkDesc, &b.linkDomain} {
		if r.Width > 0 {
			r.X += b.box.X
		}
	}
	return b
}

func singleLineSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// linkInBody reports whether link appears in body (ignoring case, so a bare-domain MatchedText
// counts): a preview's link is trusted as a click target only when the reader can see it.
func linkInBody(body, link string) bool {
	return strings.Contains(strings.ToLower(body), strings.ToLower(link))
}

// spanAt returns the link or phone span at (x, y) (x in view coords, y relative to bubble top).
func spanAt(b bubble, lineH int, ms measurer, x, y int) (kind linkify.SpanKind, raw string, span [2]int, ok bool) {
	if b.text.Width <= 0 || b.text.Height <= 0 || lineH <= 0 || (len(b.linkSpans) == 0 && len(b.phoneSpans) == 0) {
		return 0, "", [2]int{}, false
	}
	if !in(b.text, x, y) {
		return 0, "", [2]int{}, false
	}
	li := (y - b.text.Y) / lineH
	if li < 0 || li >= len(b.lines) {
		return 0, "", [2]int{}, false
	}
	ln := b.lines[li]
	lineUnits := b.units[ln.start:ln.end]
	if len(lineUnits) == 0 {
		return 0, "", [2]int{}, false
	}
	lx := x - (b.text.X + lineIndent(b.indents, li))
	if lx < 0 {
		return 0, "", [2]int{}, false
	}
	adv := lineAdv(ms, lineUnits, lineStyles(b, ln), 0)
	if len(adv) == 0 || lx >= adv[len(adv)-1] {
		return 0, "", [2]int{}, false
	}
	j := sort.Search(len(adv), func(k int) bool { return adv[k] > lx })
	if j >= len(adv) {
		return 0, "", [2]int{}, false
	}
	off := ln.start + j
	for _, sp := range b.linkSpans {
		if off >= sp[0] && off < sp[1] {
			return linkify.SpanURL, string(utf16.Decode(b.units[sp[0]:sp[1]])), sp, true
		}
	}
	for _, sp := range b.phoneSpans {
		if off >= sp[0] && off < sp[1] {
			return linkify.SpanPhone, string(utf16.Decode(b.units[sp[0]:sp[1]])), sp, true
		}
	}
	return 0, "", [2]int{}, false
}

type spanTarget struct {
	kind linkify.SpanKind
	raw  string
	span [2]int
	card bool
}

// spanPress tracks a left-button press on a clickable link/phone span or linkCard to distinguish
// a click from a text-selection drag.
type spanPress struct {
	active  bool
	dragged bool
	msgID   string
	target  spanTarget
	downX   int
	downY   int
}

func withinDragThreshold(x0, y0, x1, y1, dx, dy int) bool {
	return abs(x1-x0) <= max(1, dx) && abs(y1-y0) <= max(1, dy)
}

func (p *spanPress) beginSpan(msgID string, b bubble, lineH int, ms measurer, x, y int) bool {
	kind, raw, sp, ok := spanAt(b, lineH, ms, x, y)
	if !ok {
		*p = spanPress{}
		return false
	}
	*p = spanPress{
		active: true,
		msgID:  msgID,
		target: spanTarget{kind: kind, raw: raw, span: sp},
		downX:  x,
		downY:  y,
	}
	return true
}

func (p *spanPress) beginCard(msgID string, b bubble, x, y int) bool {
	if b.linkCard.Width <= 0 || !in(b.linkCard, x, y) || b.linkTargetURL == "" {
		*p = spanPress{}
		return false
	}
	*p = spanPress{
		active: true,
		msgID:  msgID,
		target: spanTarget{kind: linkify.SpanURL, raw: b.linkTargetURL, card: true},
		downX:  x,
		downY:  y,
	}
	return true
}

func (p *spanPress) moved(x, y, dragX, dragY int) bool {
	if !p.active {
		return false
	}
	if !p.dragged && !withinDragThreshold(p.downX, p.downY, x, y, dragX, dragY) {
		p.dragged = true
	}
	return p.dragged
}

func (p *spanPress) finish(b bubble, lineH int, ms measurer, x, y, dragX, dragY int, hasSel bool) (spanTarget, bool) {
	cur := *p
	*p = spanPress{}
	if !cur.active || cur.dragged || hasSel || !withinDragThreshold(cur.downX, cur.downY, x, y, dragX, dragY) {
		return spanTarget{}, false
	}
	if cur.target.card {
		if b.linkCard.Width <= 0 || !in(b.linkCard, x, y) || b.linkTargetURL == "" {
			return spanTarget{}, false
		}
		return cur.target, true
	}
	kind, raw, sp, ok := spanAt(b, lineH, ms, x, y)
	if !ok || kind != cur.target.kind || sp != cur.target.span || raw != cur.target.raw {
		return spanTarget{}, false
	}
	return cur.target, true
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
	if l == nil {
		return
	}
	for _, k := range l.keys {
		l.evict(l.vals[k])
	}
	l.keys = l.keys[:0]
	clear(l.vals)
}

func (l *lru[V]) del(k string) {
	if l == nil {
		return
	}
	if v, ok := l.vals[k]; ok {
		l.evict(v)
		delete(l.vals, k)
		if i := slices.Index(l.keys, k); i >= 0 {
			l.keys = slices.Delete(l.keys, i, i+1)
		}
	}
}
