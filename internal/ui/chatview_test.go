package ui

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/lxn/walk"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"wa-go/internal/store"
	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
)

// monoMeasurer measures text as fixed-width runes (7px at 10pt) on 20px lines (at 10pt), wrapping
// at maxW like the real measurer; calls counts how often it was asked.
type monoMeasurer struct{ calls int }

func (m *monoMeasurer) measure(text string, pt int, _ walk.FontStyle, maxW int) (int, int) {
	m.calls++
	cw, lh := pt*7/10, pt*2
	per := max(1, maxW/cw)
	lines, widest := 0, 0
	for _, l := range strings.Split(text, "\n") {
		n := utf8.RuneCountInString(l)
		lines += max(1, (n+per-1)/per)
		widest = max(widest, min(n, per))
	}
	return widest * cw, lines * lh
}

const viewW = 800 // at 96 DPI: bubbles are at most 520 wide, 18 from the sides

func layOut(t *testing.T, group bool, msgs ...store.Message) *chatLayout {
	t.Helper()
	l := &chatLayout{group: group, senderName: func(jid string) string {
		if jid == alice {
			return "Alice"
		}
		return ""
	}}
	l.update(msgs, viewW, 96, &monoMeasurer{})
	if len(l.tops) != len(msgs) {
		t.Fatalf("%d tops for %d messages", len(l.tops), len(msgs))
	}
	return l
}

// meta marshals a media sub-message as the store keeps it.
func meta(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func inside(in, out walk.Rectangle) bool {
	return in.X >= out.X && in.Y >= out.Y && in.X+in.Width <= out.X+out.Width && in.Y+in.Height <= out.Y+out.Height
}

func below(a, b walk.Rectangle) bool { return a.Y >= b.Y+b.Height }

func TestBubbleAlignmentAndColour(t *testing.T) {
	l := layOut(t, false,
		store.Message{ID: "out", Type: "text", Body: "hello there", FromMe: true, Status: store.StatusSent, Timestamp: 100},
		store.Message{ID: "in", Type: "text", Body: "hi", SenderJID: alice, Status: store.StatusRead, Timestamp: 200},
	)
	out, in := l.cache["out"], l.cache["in"]
	if right := out.box.X + out.box.Width; right != viewW-18 {
		t.Errorf("outgoing bubble ends at x=%d, want right-aligned at %d", right, viewW-18)
	}
	if in.box.X != 18 {
		t.Errorf("incoming bubble starts at x=%d, want left-aligned at 18", in.box.X)
	}
	for _, b := range []bubble{out, in} {
		if !inside(b.text, b.box) || !inside(b.time, b.box) || b.box.Width >= 520 {
			t.Errorf("bubble %+v: parts outside or too wide for two words", b)
		}
	}
	if !out.out || in.out {
		t.Error("direction not recorded")
	}
	if l.tops[1] <= l.tops[0]+out.box.Height-1 || l.total <= l.tops[1]+in.box.Height {
		t.Errorf("tops %v total %d: bubbles overlap or overflow", l.tops, l.total)
	}
	for _, c := range []struct {
		mode    theme.ThemeMode
		out, in string
	}{{theme.ThemeLight, "#d9fdd3", "#ffffff"}, {theme.ThemeDark, "#005c4b", "#202c33"}} {
		p := theme.GetPalette(c.mode)
		if got := bubbleColor(p, out.out); got != c.out {
			t.Errorf("mode %v outgoing colour %s, want %s", c.mode, got, c.out)
		}
		if got := bubbleColor(p, in.out); got != c.in {
			t.Errorf("mode %v incoming colour %s, want %s", c.mode, got, c.in)
		}
	}
}

func TestLongTextWrapsAtMaxWidth(t *testing.T) {
	long := strings.Repeat("lorem ipsum ", 40)
	l := layOut(t, false,
		store.Message{ID: "long", Type: "text", Body: long, SenderJID: alice, Timestamp: 100},
		store.Message{ID: "short", Type: "text", Body: "ok", SenderJID: alice, Timestamp: 100},
	)
	lb, sb := l.cache["long"], l.cache["short"]
	if lb.box.Width > 520 || lb.box.Width < 500 {
		t.Errorf("long bubble %d wide, want close to the 65%% cap of 520", lb.box.Width)
	}
	if lb.text.Height <= 20 || !inside(lb.text, lb.box) {
		t.Errorf("long text %+v not wrapped inside %+v", lb.text, lb.box)
	}
	if !below(lb.time, lb.text) || !inside(lb.time, lb.box) {
		t.Errorf("time %+v of a wrapped text should get its own row under %+v", lb.time, lb.text)
	}
	// a short single line keeps its time on the same line, right of the text
	if sb.time.Y+sb.time.Height != sb.text.Y+sb.text.Height || sb.time.X < sb.text.X+sb.text.Width {
		t.Errorf("short text %+v, time %+v: time not beside the text", sb.text, sb.time)
	}
	if sb.box.Height >= lb.box.Height || sb.box.Width > 120 {
		t.Errorf("short bubble %+v too big", sb.box)
	}
}

func TestImageThumbnailBoxWithCaptionBelow(t *testing.T) {
	l := layOut(t, false,
		store.Message{ID: "wide", Type: "image", Body: "sunset at the beach", SenderJID: alice, MediaMeta: meta(t, &waE2E.ImageMessage{Width: proto.Uint32(800), Height: proto.Uint32(600)})},
		store.Message{ID: "nodims", Type: "image", FromMe: true, Status: store.StatusRead},
	)
	w, n := l.cache["wide"], l.cache["nodims"]
	if w.kind != kindImage || w.thumb.Width != 240 || w.thumb.Height != 180 {
		t.Fatalf("800x600 image box %+v, want 240x180", w.thumb)
	}
	if w.thumb.X != w.box.X+3 || w.thumb.Y != w.box.Y+3 || w.box.Width != 246 {
		t.Errorf("thumbnail %+v not inset 3px in bubble %+v", w.thumb, w.box)
	}
	if !below(w.text, w.thumb) || !inside(w.text, w.box) || w.text.X != w.box.X+9 {
		t.Errorf("caption %+v not under the thumbnail %+v inside %+v", w.text, w.thumb, w.box)
	}
	if n.thumb.Width != 240 || n.thumb.Height != 240 || n.text != (walk.Rectangle{}) {
		t.Errorf("image without dimensions or caption: box %+v text %+v, want a 240 square, no text", n.thumb, n.text)
	}
	if !below(n.time, n.thumb) || !inside(n.ticks, n.box) || n.box.X+n.box.Width != viewW-18 {
		t.Errorf("outgoing image: time %+v ticks %+v box %+v", n.time, n.ticks, n.box)
	}
}

func TestFitBox(t *testing.T) {
	for _, c := range []struct{ w, h, maxW, ww, wh int }{
		{800, 600, 500, 240, 180},
		{600, 800, 500, 180, 240},
		{0, 0, 500, 240, 240},
		{4000, 100, 500, 240, 60}, // panorama: kept at least a quarter tall, cropped when painted
		{800, 600, 120, 120, 90},  // narrow view
		{100, 50, 500, 240, 120},  // small images are shown at the common size
	} {
		if w, h := fitBox(c.w, c.h, 240, c.maxW); w != c.ww || h != c.wh {
			t.Errorf("fitBox(%d, %d, max %d) = %dx%d, want %dx%d", c.w, c.h, c.maxW, w, h, c.ww, c.wh)
		}
	}
}

func TestDocumentCardLayout(t *testing.T) {
	pdf := meta(t, &waE2E.DocumentMessage{FileName: proto.String("report.pdf"), FileLength: proto.Uint64(250880)})
	l := layOut(t, false,
		store.Message{ID: "doc", Type: "document", Body: "report.pdf", SenderJID: alice, MediaMeta: pdf},
		store.Message{ID: "cap", Type: "document", Body: "numbers for Q3", FromMe: true, Status: store.StatusDelivered, MediaMeta: pdf},
	)
	d, c := l.cache["doc"], l.cache["cap"]
	if d.kind != kindCard || d.card.Width != 280 || d.card.Height != 64 || d.card.X != d.box.X+3 || d.card.Y != d.box.Y+3 {
		t.Fatalf("document card %+v in bubble %+v, want 280x64 inset 3", d.card, d.box)
	}
	if d.text != (walk.Rectangle{}) || !below(d.time, d.card) || !inside(d.time, d.box) {
		t.Errorf("file name only: text %+v (want none), time %+v under the card", d.text, d.time)
	}
	if !below(c.text, c.card) || !below(c.time, c.card) || !inside(c.text, c.box) || c.box.X+c.box.Width != viewW-18 {
		t.Errorf("captioned document: card %+v text %+v time %+v box %+v", c.card, c.text, c.time, c.box)
	}
	icon, title, sub, dl := cardParts(d.card, 96)
	if !inside(icon, d.card) || !inside(title, d.card) || !inside(sub, d.card) || !inside(dl, d.card) ||
		title.X < icon.X+icon.Width || !below(sub, title) || dl.X < title.X+title.Width {
		t.Errorf("card parts icon %+v title %+v sub %+v download %+v in %+v", icon, title, sub, dl, d.card)
	}
	for _, x := range []struct {
		m          store.Message
		info       mediaInfo
		title, sub string
	}{
		{store.Message{Type: "document", Body: "report.pdf"}, mediaInfo{name: "report.pdf", size: 250880}, "report.pdf", "245 KB · PDF"},
		{store.Message{Type: "document", Body: "notes"}, mediaInfo{}, "notes", ""},
		{store.Message{Type: "video"}, mediaInfo{size: 3565158}, "Video", "3.4 MB"},
		{store.Message{Type: "audio"}, mediaInfo{size: 900}, "Audio", "900 B"},
		{store.Message{Type: "sticker"}, mediaInfo{}, "Sticker", ""},
	} {
		if title, sub := cardLabels(&x.m, x.info); title != x.title || sub != x.sub {
			t.Errorf("%s card labels %q, %q; want %q, %q", x.m.Type, title, sub, x.title, x.sub)
		}
	}
}

func TestMediaKinds(t *testing.T) {
	for _, c := range []struct {
		typ  string
		info mediaInfo
		want bubbleKind
	}{
		{"text", mediaInfo{}, kindText},
		{"image", mediaInfo{}, kindImage},
		{"video", mediaInfo{preview: []byte{1}}, kindCard},
		{"audio", mediaInfo{}, kindCard},
		{"document", mediaInfo{}, kindCard},
		{"sticker", mediaInfo{}, kindCard},
		{"sticker", mediaInfo{preview: []byte{1}}, kindImage},
		{"revoked", mediaInfo{}, kindText},
		{"unsupported", mediaInfo{}, kindText},
	} {
		if got := kindOf(&store.Message{Type: c.typ}, c.info); got != c.want {
			t.Errorf("%s (preview %v): kind %v, want %v", c.typ, c.info.preview != nil, got, c.want)
		}
	}
}

func TestParseMediaMeta(t *testing.T) {
	doc := meta(t, &waE2E.DocumentMessage{FileName: proto.String("q3.xlsx"), FileLength: proto.Uint64(4096), JPEGThumbnail: []byte{0xff, 0xd8}})
	img := meta(t, &waE2E.ImageMessage{Width: proto.Uint32(1600), Height: proto.Uint32(1200), FileLength: proto.Uint64(5)})
	if info, err := parseMedia(&store.Message{Type: "document", MediaMeta: doc}); err != nil || info.name != "q3.xlsx" || info.size != 4096 || len(info.preview) != 2 {
		t.Errorf("document meta %+v, %v", info, err)
	}
	if info, err := parseMedia(&store.Message{Type: "image", MediaMeta: img}); err != nil || info.w != 1600 || info.h != 1200 || info.size != 5 {
		t.Errorf("image meta %+v, %v", info, err)
	}
	if info, err := parseMedia(&store.Message{Type: "text"}); err != nil || info.size != 0 {
		t.Errorf("no meta: %+v, %v", info, err)
	}
	if _, err := parseMedia(&store.Message{Type: "image", MediaMeta: []byte{0xff, 0xff, 0xff}}); err == nil {
		t.Error("garbage meta accepted")
	}
}

func TestHumanSize(t *testing.T) {
	for n, want := range map[uint64]string{0: "", 900: "900 B", 2048: "2 KB", 250880: "245 KB", 1048576: "1.0 MB", 3565158: "3.4 MB", 3 << 30: "3.0 GB"} {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestTicksPerStatus(t *testing.T) {
	for _, mode := range []theme.ThemeMode{theme.ThemeLight, theme.ThemeDark} {
		p := theme.GetPalette(mode)
		for _, c := range []struct {
			status string
			icon   icons.Icon
			color  string
			ok     bool
		}{
			{store.StatusSent, icons.IconCheck, p.TextSecondary, true},
			{store.StatusDelivered, icons.IconDoubleCheck, p.TextSecondary, true},
			{store.StatusRead, icons.IconDoubleCheck, "#53bdeb", true},
			{store.StatusReceived, 0, "", false},
			{"", 0, "", false},
		} {
			icon, color, ok := tickFor(p, c.status)
			if icon != c.icon || color != c.color || ok != c.ok {
				t.Errorf("mode %v status %q: tick %v %q %v, want %v %q %v", mode, c.status, icon, color, ok, c.icon, c.color, c.ok)
			}
		}
	}
	l := layOut(t, false,
		store.Message{ID: "in", Type: "text", Body: "x", SenderJID: alice, Status: store.StatusRead},
		store.Message{ID: "out", Type: "text", Body: "x", FromMe: true, Status: store.StatusRead},
	)
	if l.cache["in"].ticks != (walk.Rectangle{}) || l.cache["out"].ticks.Width == 0 {
		t.Errorf("ticks: incoming %+v (want none), outgoing %+v", l.cache["in"].ticks, l.cache["out"].ticks)
	}
}

func TestDeletedAndUnsupportedText(t *testing.T) {
	for _, c := range []struct {
		m     store.Message
		info  mediaInfo
		text  string
		muted bool
	}{
		{store.Message{Type: "revoked", Body: "anything"}, mediaInfo{}, "This message was deleted", true},
		{store.Message{Type: "unsupported"}, mediaInfo{}, "Unsupported message", true},
		{store.Message{Type: "text", Body: "hello\r\n\n"}, mediaInfo{}, "hello", false},
		{store.Message{Type: "image", Body: "caption"}, mediaInfo{}, "caption", false},
		{store.Message{Type: "document", Body: "a.pdf"}, mediaInfo{name: "a.pdf"}, "", false},
		{store.Message{Type: "document", Body: "see this"}, mediaInfo{name: "a.pdf"}, "see this", false},
		{store.Message{Type: "audio"}, mediaInfo{}, "", false},
	} {
		if text, muted := bubbleText(&c.m, c.info); text != c.text || muted != c.muted {
			t.Errorf("%s %q: %q muted=%v, want %q %v", c.m.Type, c.m.Body, text, muted, c.text, c.muted)
		}
	}
	l := layOut(t, false, store.Message{ID: "r", Type: "revoked", Body: "This message was deleted", SenderJID: alice})
	if r := l.cache["r"]; r.kind != kindText || r.text.Width == 0 {
		t.Errorf("revoked bubble %+v has no notice", r)
	}
}

func TestGroupSenderNameOnFirstOfRun(t *testing.T) {
	msgs := []store.Message{
		{ID: "a1", Type: "text", Body: "hi all", SenderJID: alice},
		{ID: "a2", Type: "text", Body: "dinner?", SenderJID: alice},
		{ID: "b1", Type: "text", Body: "yes", SenderJID: bob},
		{ID: "m1", Type: "text", Body: "me too", FromMe: true, Status: store.StatusSent},
		{ID: "a3", Type: "text", Body: "ok", SenderJID: alice},
	}
	l := layOut(t, true, msgs...)
	for id, named := range map[string]bool{"a1": true, "a2": false, "b1": true, "m1": false, "a3": true} {
		b := l.cache[id]
		if b.named != named || (b.name.Width > 0) != named {
			t.Errorf("%s: named %v name rect %+v, want named %v", id, b.named, b.name, named)
		}
		if named && (!below(b.text, b.name) || !inside(b.name, b.box)) {
			t.Errorf("%s: name %+v not above text %+v", id, b.name, b.text)
		}
	}
	gap := func(i int) int { return l.tops[i] - l.tops[i-1] - l.cache[msgs[i-1].ID].box.Height }
	if gap(1) >= gap(2) {
		t.Errorf("gap inside a run %d, between runs %d: want runs grouped", gap(1), gap(2))
	}
	if bobName, aliceName := l.sender(bob), l.sender(alice); bobName != "+6284444444444" || aliceName != "Alice" {
		t.Errorf("sender names %q, %q", bobName, aliceName)
	}
	// a 1:1 chat never names the sender
	if one := layOut(t, false, msgs...); one.cache["a1"].named {
		t.Error("1:1 chat names its sender")
	}
}

func TestLayoutIsCachedNotRemeasured(t *testing.T) {
	msgs := []store.Message{
		{ID: "1", Type: "text", Body: "one", SenderJID: alice},
		{ID: "2", Type: "text", Body: "two", FromMe: true, Status: store.StatusSent},
	}
	l := &chatLayout{}
	m := &monoMeasurer{}
	l.update(msgs, viewW, 96, m)
	if m.calls == 0 {
		t.Fatal("nothing measured")
	}
	m.calls = 0
	l.update(msgs, viewW, 96, m)
	if m.calls != 0 {
		t.Errorf("same width and DPI re-measured %d times", m.calls)
	}
	l.forget("2")
	l.update(msgs, viewW, 96, m)
	if m.calls == 0 || m.calls > 3 {
		t.Errorf("one changed message: %d measurements", m.calls)
	}
	m.calls = 0
	l.update(msgs, 600, 96, m)
	if m.calls < 2 {
		t.Errorf("width change re-measured only %d texts", m.calls)
	}
	// DPI scales the geometry
	l.update(msgs, 1200, 144, m)
	if b := l.cache["1"]; b.box.X != 27 {
		t.Errorf("incoming bubble at 144 DPI starts at %d, want 27", b.box.X)
	}
}

func TestShouldLoadPrevious(t *testing.T) {
	for _, c := range []struct {
		off, threshold     int
		loading, exhausted bool
		want               bool
	}{
		{0, 64, false, false, true},
		{64, 64, false, false, true},
		{65, 64, false, false, false},
		{0, 64, true, false, false}, // already asked: once until the page arrives
		{0, 64, false, true, false}, // no more history
	} {
		if got := shouldLoadPrevious(c.off, c.threshold, c.loading, c.exhausted); got != c.want {
			t.Errorf("shouldLoadPrevious(%+v) = %v", c, got)
		}
	}
}

func TestAnchorAfterPrepend(t *testing.T) {
	for _, c := range []struct{ off, oldTotal, newTotal, view, want int }{
		{10, 2000, 3000, 500, 1010}, // the message at the top stays put
		{0, 300, 400, 500, 0},       // still shorter than the view: bottom-aligned, nothing moves
		{0, 300, 900, 500, 400},     // grows past the view: old messages keep their place at the bottom
	} {
		got := anchorAfterPrepend(c.off, c.oldTotal, c.newTotal, c.view)
		if got != c.want {
			t.Errorf("anchorAfterPrepend(%+v) = %d", c, got)
		}
		// same screen position for an old message at content y: pad + y - off before and after
		pad := func(total int) int { return max(0, c.view-total) }
		y := 100
		if before, after := pad(c.oldTotal)+y-c.off, pad(c.newTotal)+(y+c.newTotal-c.oldTotal)-got; before != after {
			t.Errorf("%+v: message moved from %d to %d", c, before, after)
		}
	}
}

func TestFollowBottomOnlyWhenAtBottom(t *testing.T) {
	for _, c := range []struct{ off, oldTotal, oldView, newTotal, newView, want int }{
		{1500, 2000, 500, 2100, 500, 1600}, // at the bottom: follows the new message
		{1000, 2000, 500, 2100, 500, 1000}, // reading history: stays
		{1499, 2000, 500, 2100, 500, 1499},
		{0, 300, 500, 700, 500, 200},       // short chat grows past the view
		{1500, 2000, 500, 2000, 400, 1600}, // window got shorter while at the bottom
		{0, 0, 0, 2000, 500, 1500},         // first layout of a chat opens at the bottom
	} {
		if got := followBottom(c.off, c.oldTotal, c.oldView, c.newTotal, c.newView); got != c.want {
			t.Errorf("followBottom(%+v) = %d", c, got)
		}
	}
}

func TestThumbnailLRUEvicts(t *testing.T) {
	var evicted []int
	l := newLRU(24, func(v int) { evicted = append(evicted, v) })
	key := func(i int) string { return string(rune('A' + i)) }
	for i := 0; i < 24; i++ {
		l.put(key(i), i)
	}
	if _, ok := l.get(key(0)); !ok { // now the most recent
		t.Fatal("entry 0 missing")
	}
	for i := 24; i < 30; i++ {
		l.put(key(i), i)
	}
	if len(evicted) != 6 || evicted[0] != 1 || evicted[5] != 6 {
		t.Errorf("evicted %v, want 1..6 (0 was used recently)", evicted)
	}
	if _, ok := l.get(key(1)); ok {
		t.Error("evicted entry still served")
	}
	l.put(key(0), 100) // replacing releases the old value
	if evicted[len(evicted)-1] != 0 {
		t.Errorf("replaced value not released: %v", evicted)
	}
	l.clear()
	if len(evicted) != 31 {
		t.Errorf("clear released %d of 24", len(evicted)-7)
	}
	if _, ok := l.get(key(29)); ok {
		t.Error("cleared entry still served")
	}
}

func TestGDIMeasurerMatchesDrawnText(t *testing.T) {
	// window at 144 DPI on a canvas reporting 96 (walk's buffered canvas reports the system DPI):
	// measured width must match what drawText draws, or wrapped lines would be clipped
	g := &gdiMeasurer{dpi: 144, cdpi: 96}
	w, h := g.measure("Yesterday", 9, 0, 1000)
	_, h2 := g.measure("Yesterday\nToday", 9, 0, 1000)
	long, longH := g.measure(strings.Repeat("x", 300), 10, 0, 200) // one word: broken, not overflowing
	if err := g.close(); err != nil {
		t.Fatal(err)
	}
	if ink := inkWidth(t, 96, 144); w < ink || w > ink+4 {
		t.Errorf("measured %dpx, drawn ink %dpx", w, ink)
	}
	if h2 != 2*h {
		t.Errorf("two lines %dpx high, one line %dpx", h2, h)
	}
	if long > 200 || longH < 3*h {
		t.Errorf("an unbreakable 300-char word measured %dx%d at max width 200", long, longH)
	}
}

// page is a store page (newest first) of n messages of chat, timestamps from ts up, ids prefix0..
func page(chat, prefix string, ts int64, n int) []store.Message {
	var out []store.Message
	for i := n - 1; i >= 0; i-- {
		out = append(out, store.Message{ID: prefix + strconv.Itoa(i), AccountID: acctA, ChatJID: chat, SenderJID: chat, Type: "text", Timestamp: ts + int64(i)})
	}
	return out
}

func TestLateHistoryAnswersForAnotherChatAreIgnored(t *testing.T) {
	var m chatModel
	m.open(acctA, alice, page(alice, "a", 1000, store.PageSize))
	m.loading = true // Alice's previous page is being fetched...
	m.open(acctA, bob, page(bob, "b", 1000, store.PageSize))
	m.loading = true // ...when Bob's chat opens and asks too
	if n := m.prepend(acctA, alice, nil); n != 0 || !m.loading || m.exhausted {
		t.Errorf("late empty page for Alice: added %d, Bob loading %v exhausted %v", n, m.loading, m.exhausted)
	}
	if n := m.prepend(acctA, alice, page(alice, "x", 1, 3)); n != 0 || len(m.msgs) != store.PageSize {
		t.Errorf("late page for Alice added %d messages to Bob's chat", n)
	}
	m.noMore(acctA, alice)
	m.noMore(acctB, bob) // same chat JID, other account
	if !m.loading || m.exhausted {
		t.Errorf("NoMoreHistory for another chat: Bob loading %v exhausted %v", m.loading, m.exhausted)
	}
	if n := m.prepend(acctA, bob, page(bob, "old", 900, 10)); n != 10 || m.loading || !m.exhausted {
		t.Errorf("Bob's short page: added %d, loading %v exhausted %v", n, m.loading, m.exhausted)
	}
	m.loading = true
	m.exhausted = false
	m.noMore(acctA, bob)
	if m.loading || !m.exhausted {
		t.Errorf("Bob's NoMoreHistory: loading %v exhausted %v", m.loading, m.exhausted)
	}
}

func TestUpsertOfMessageOlderThanTheLoadedWindow(t *testing.T) {
	var m chatModel
	m.open(acctA, alice, page(alice, "a", 1000, store.PageSize)) // more history exists
	old := store.Message{ID: "old7", AccountID: acctA, ChatJID: alice, Type: "revoked", Timestamp: 907}
	if m.upsert(old) || len(m.msgs) != store.PageSize {
		t.Fatal("a revision of a message above the loaded window was inserted above the gap")
	}
	// it arrives with its page, in order
	m.prepend(acctA, alice, page(alice, "old", 900, store.PageSize))
	for i := 1; i < len(m.msgs); i++ {
		if m.msgs[i].Timestamp < m.msgs[i-1].Timestamp {
			t.Fatalf("out of order at %d: %d after %d", i, m.msgs[i].Timestamp, m.msgs[i-1].Timestamp)
		}
	}
	// newer messages and ones inside the window are placed by time
	if !m.upsert(store.Message{ID: "new", AccountID: acctA, ChatJID: alice, Timestamp: 5000}) || m.msgs[len(m.msgs)-1].ID != "new" {
		t.Error("new message not appended")
	}
	if !m.upsert(store.Message{ID: "mid", AccountID: acctA, ChatJID: alice, Timestamp: 1020}) || m.msgs[m.index("mid")-1].Timestamp != 1020 {
		t.Error("message inside the window not placed by time")
	}
	// once the history is exhausted everything is loaded, so an older message really is the oldest
	var short chatModel
	short.open(acctA, alice, page(alice, "a", 1000, 5))
	if !short.upsert(old) || short.msgs[0].ID != "old7" {
		t.Error("older message of an exhausted history not inserted first")
	}
	if m.upsert(store.Message{ID: "z", AccountID: acctA, ChatJID: bob, Timestamp: 5000}) {
		t.Error("message of another chat applied")
	}
}

func TestUpsertKeepsTheDownloadedMediaPath(t *testing.T) {
	var m chatModel
	img := store.Message{ID: "img", AccountID: acctA, ChatJID: alice, Type: "image", FromMe: true, Status: store.StatusSent, Timestamp: 1, MediaPath: `C:\cache\a.jpg`}
	m.open(acctA, alice, []store.Message{img})
	receipt := img
	receipt.Status, receipt.MediaPath = store.StatusRead, "" // built from an event: no path
	if !m.upsert(receipt) {
		t.Fatal("status change not applied")
	}
	if got := m.msgs[0]; got.Status != store.StatusRead || got.MediaPath != img.MediaPath {
		t.Errorf("after the receipt: status %q path %q, want read and the downloaded path", got.Status, got.MediaPath)
	}
	moved := img
	moved.MediaPath = `C:\cache\b.jpg`
	m.upsert(moved)
	if m.msgs[0].MediaPath != moved.MediaPath {
		t.Errorf("a new path was not taken: %q", m.msgs[0].MediaPath)
	}
}

func TestImagesAreFetchedOnSightOnlyWithAutoDownload(t *testing.T) {
	requested := map[string]bool{}
	if fetchOnSight("a", false, requested) || requested["a"] {
		t.Fatal("auto-download off: an image in view was requested")
	}
	if !fetchOnSight("a", true, requested) {
		t.Fatal("auto-download on: the image in view was not requested")
	}
	if fetchOnSight("a", true, requested) {
		t.Fatal("the same image was requested twice on sight")
	}
}
