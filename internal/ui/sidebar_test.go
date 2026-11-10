package ui

import (
	"slices"
	"testing"
	"time"

	"github.com/lxn/walk"

	"wa-go/internal/store"
	"wa-go/internal/whatsapp"
)

const (
	acctA = "6281111111111"
	acctB = "6282222222222"
	alice = "6283333333333@s.whatsapp.net"
	bob   = "6284444444444@s.whatsapp.net"
	group = "120363000000000001@g.us"
)

func sampleChats() []store.Chat {
	return []store.Chat{
		{JID: alice, Name: "Alice", LastMessageTS: 300, Snippet: "See you tomorrow"},
		{JID: bob, Name: "Bob", LastMessageTS: 200, Snippet: "Invoice ATTACHED", Unread: 2},
		{JID: group, Name: "Family", LastMessageTS: 100, Snippet: "dinner?"},
	}
}

func jids(rows []store.Chat) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.JID)
	}
	return out
}

func TestFilterByNameOrSnippetCaseInsensitive(t *testing.T) {
	m := NewSidebarModel()
	m.SetChats(acctA, sampleChats())
	for q, want := range map[string][]string{
		"":         {alice, bob, group},
		"ALI":      {alice},
		"fam":      {group},
		"attached": {bob},   // snippet
		"Tomorrow": {alice}, // snippet, mixed case
		"zzz":      nil,
	} {
		m.SetFilter(q)
		if got := jids(m.Rows()); !slices.Equal(got, want) {
			t.Errorf("filter %q: %v, want %v", q, got, want)
		}
	}
}

func TestIncomingMessageBumpsUnreadAndMovesToTop(t *testing.T) {
	m := NewSidebarModel()
	m.SetChats(acctA, sampleChats())
	if !m.ApplyMessage(store.Message{AccountID: acctA, ChatJID: group, Timestamp: 400, Body: "on my way", Status: store.StatusReceived}) {
		t.Fatal("message for the active account not applied")
	}
	top := m.Rows()[0]
	if top.JID != group || top.Unread != 1 || top.Snippet != "on my way" || top.LastMessageTS != 400 || top.Name != "Family" {
		t.Fatalf("top row %+v", top)
	}
	if m.Unread() != 3 {
		t.Fatalf("total unread %d, want 3", m.Unread())
	}
	// a chat not listed yet appears on top
	m.ApplyMessage(store.Message{AccountID: acctA, ChatJID: "6285555555555@s.whatsapp.net", Timestamp: 500, Body: "hi", Status: store.StatusReceived})
	if top := m.Rows()[0]; top.JID != "6285555555555@s.whatsapp.net" || top.Unread != 1 || len(m.Rows()) != 4 {
		t.Fatalf("new chat row %+v (rows %d)", top, len(m.Rows()))
	}
	m.MarkRead(bob)
	if m.Unread() != 2 {
		t.Fatalf("total unread %d after reading Bob, want 2", m.Unread())
	}
}

func TestFromMeDoesNotBumpUnread(t *testing.T) {
	m := NewSidebarModel()
	m.SetChats(acctA, sampleChats())
	m.ApplyMessage(store.Message{AccountID: acctA, ChatJID: group, Timestamp: 400, Body: "ok", FromMe: true, Status: store.StatusSent})
	if top := m.Rows()[0]; top.JID != group || top.Unread != 0 || top.Snippet != "ok" {
		t.Fatalf("top row %+v", top)
	}
}

func TestOtherAccountMessageLeavesListAlone(t *testing.T) {
	m := NewSidebarModel()
	m.SetChats(acctA, sampleChats())
	if m.ApplyMessage(store.Message{AccountID: acctB, ChatJID: group, Timestamp: 400, Body: "x", Status: store.StatusReceived}) {
		t.Fatal("message for another account applied")
	}
	if !slices.Equal(m.Rows(), sampleChats()) {
		t.Fatalf("rows changed: %+v", m.Rows())
	}
}

func TestStatusBroadcastNeverARow(t *testing.T) {
	m := NewSidebarModel()
	m.SetChats(acctA, append(sampleChats(), store.Chat{JID: statusBroadcast, LastMessageTS: 900}))
	if m.ApplyMessage(store.Message{AccountID: acctA, ChatJID: statusBroadcast, Timestamp: 999, Body: "story", Status: store.StatusReceived}) {
		t.Fatal("story applied as a chat")
	}
	if slices.Contains(jids(m.Rows()), statusBroadcast) || m.Unread() != 2 {
		t.Fatalf("rows %v unread %d", jids(m.Rows()), m.Unread())
	}
}

func TestAccountSwitchFiresActiveChanged(t *testing.T) {
	m := NewRailModel()
	var got []string
	m.OnActiveChanged(func(id string) { got = append(got, id) })
	m.SetAccounts([]string{acctA, acctB})
	if m.Active() != acctA || !slices.Equal(got, []string{acctA}) {
		t.Fatalf("after SetAccounts active=%q events=%v", m.Active(), got)
	}
	m.SetActive(acctB)
	if m.Active() != acctB || !slices.Equal(got, []string{acctA, acctB}) {
		t.Fatalf("after switch active=%q events=%v", m.Active(), got)
	}
	m.SetActive(acctB)     // same account
	m.SetActive("unknown") // not an account
	if m.Active() != acctB || len(got) != 2 {
		t.Fatalf("no-op switches fired: active=%q events=%v", m.Active(), got)
	}
	m.SetAccounts([]string{acctA}) // active account removed: falls back to the first
	if m.Active() != acctA || len(got) != 3 {
		t.Fatalf("after removal active=%q events=%v", m.Active(), got)
	}
}

func TestPhoneJID(t *testing.T) {
	for in, want := range map[string]string{
		"+62 812-3456-7890": "6281234567890@s.whatsapp.net",
		"(021) 555 0123":    "0215550123@s.whatsapp.net",
		"1234567":           "1234567@s.whatsapp.net",
		"123456789012345":   "123456789012345@s.whatsapp.net",
	} {
		if got, ok := phoneJID(in); !ok || got != want {
			t.Errorf("phoneJID(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "123456", "1234567890123456", "Alice", "62812x4567", "+", "62.812.3456"} {
		if got, ok := phoneJID(in); ok {
			t.Errorf("phoneJID(%q) = %q, want invalid", in, got)
		}
	}
}

func TestNewChatRows(t *testing.T) {
	cands := newChatCandidates(
		[]store.Chat{{JID: alice, Name: "Alice"}, {JID: group, Name: "Family"}},
		[]whatsapp.Contact{{JID: alice, Name: "Alice Smith"}, {JID: bob, Name: "Bob"}},
	)
	if got := jids(cands); !slices.Equal(got, []string{alice, group, bob}) {
		t.Fatalf("candidates %v (chats first, contacts deduped)", got)
	}
	if got := jids(newChatRows(cands, "BO")); !slices.Equal(got, []string{bob}) {
		t.Fatalf("by name: %v", got)
	}
	if got := jids(newChatRows(cands, "833333")); !slices.Equal(got, []string{alice}) {
		t.Fatalf("by number: %v", got)
	}
	if got := jids(newChatRows(cands, "+62 844")); !slices.Equal(got, []string{bob}) {
		t.Fatalf("by formatted partial number: %v", got)
	}
	if got := jids(newChatRows(cands, "+1 (555) 010-0999")); !slices.Equal(got, []string{"15550100999@s.whatsapp.net"}) {
		t.Fatalf("typed number: %v", got)
	}
	if got := jids(newChatRows(cands, "+62 833 3333 3333")); !slices.Equal(got, []string{alice}) {
		t.Fatalf("typed number of a known chat must not add a second row: %v", got)
	}
}

func TestRelTime(t *testing.T) {
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.Local) // a Tuesday
	at := func(d, h int) int64 { return time.Date(2026, 11, d, h, 4, 0, 0, time.Local).Unix() }
	for ts, want := range map[int64]string{
		0:         "",
		at(10, 9): "09:04",
		at(9, 23): "Yesterday",
		at(4, 9):  "Wednesday",
		at(3, 9):  "03/11/2026",
	} {
		if got := relTime(ts, now); got != want {
			t.Errorf("relTime(%d) = %q, want %q", ts, got, want)
		}
	}
}

func TestNavigate(t *testing.T) {
	for _, c := range []struct {
		cur, n, page int
		k            walk.Key
		wrap         bool
		want         int
	}{
		{-1, 5, 3, walk.KeyDown, false, 0}, // first Down focuses the first item
		{2, 5, 3, walk.KeyDown, false, 3},
		{4, 5, 3, walk.KeyDown, false, 4}, // no wrap: stays on the last
		{4, 5, 3, walk.KeyDown, true, 0},
		{0, 5, 3, walk.KeyUp, false, 0},
		{0, 5, 3, walk.KeyUp, true, 4},
		{-1, 5, 3, walk.KeyUp, true, 4},
		{1, 5, 3, walk.KeyNext, false, 4},
		{3, 5, 3, walk.KeyNext, true, 4}, // paging never wraps
		{3, 5, 3, walk.KeyPrior, true, 0},
		{3, 5, 0, walk.KeyPrior, false, 2}, // a page is at least one item
		{2, 5, 3, walk.KeyHome, false, 0},
		{2, 5, 3, walk.KeyEnd, false, 4},
		{1, 3, 1, walk.KeyRight, false, 2},
		{1, 3, 1, walk.KeyLeft, false, 0},
		{9, 3, 1, walk.KeyUp, false, 1}, // stale index past a shrunk list: from the last item
		{-1, 0, 3, walk.KeyDown, false, -1},
	} {
		if got, ok := navigate(c.cur, c.n, c.page, c.k, c.wrap); !ok || got != c.want {
			t.Errorf("navigate(%d, n=%d, page=%d, key %d, wrap=%v) = %d, %v; want %d", c.cur, c.n, c.page, c.k, c.wrap, got, ok, c.want)
		}
	}
	if _, ok := navigate(0, 5, 3, walk.KeyA, false); ok {
		t.Error("a letter key was handled")
	}
}

func TestScrollIntoView(t *testing.T) {
	for _, c := range []struct{ off, start, size, view, want int }{
		{100, 144, 72, 200, 100}, // already visible
		{100, 72, 72, 200, 72},   // above: its top at the top
		{100, 288, 72, 200, 160}, // below: its bottom at the bottom
		{500, 0, 300, 200, 0},    // taller than the view: its start shows
		{0, 216, 300, 200, 216},
	} {
		if got := scrollIntoView(c.off, c.start, c.size, c.view); got != c.want {
			t.Errorf("scrollIntoView(%+v) = %d", c, got)
		}
	}
}

func TestClickTargetsTheItemUnderTheCursor(t *testing.T) {
	// chat list after one wheel notch: offset 108 with 72px rows, row 1 half scrolled off the top
	list := span{off: 108, size: 72, n: 10}
	for y, want := range map[int]int{0: 1, 35: 1, 36: 2, 50: 2, 107: 2, 108: 3} {
		if got := list.at(y); got != want {
			t.Errorf("list click at y=%d: row %d, want %d", y, got, want)
		}
	}
	if got := (span{off: 0, size: 72, n: 2}).at(150); got != -1 {
		t.Errorf("click below the last row: %d, want -1", got)
	}
	// stories strip scrolled one notch: offset 68, 8px left padding, 68px slots
	strip := span{off: 68, pad: 8, size: 68, n: 8}
	for x, want := range map[int]int{0: 0, 7: 0, 8: 1, 100: 2} {
		if got := strip.at(x); got != want {
			t.Errorf("strip click at x=%d: story %d, want %d", x, got, want)
		}
	}
	if got := (span{pad: 8, size: 68, n: 3}).at(3); got != -1 {
		t.Errorf("click in the strip's left padding: %d, want -1", got)
	}
	// focus gained with nothing focused rests on the first fully visible item, so the scroll
	// position the user saw (and is clicking into) does not move
	for _, c := range []struct {
		s    span
		view int
		want int
	}{{list, 400, 2}, {strip, 280, 1}, {span{off: 0, size: 72, n: 5}, 400, 0}, {span{off: 144, size: 72, n: 3}, 400, 2}} {
		i := c.s.firstFull()
		if i != c.want || scrollIntoView(c.s.off, c.s.pad+i*c.s.size, c.s.size, c.view) != c.s.off {
			t.Errorf("%+v: firstFull %d (want %d) moves the offset", c.s, i, c.want)
		}
	}
	if got := (span{size: 72}).firstFull(); got != -1 {
		t.Errorf("empty list firstFull %d", got)
	}
}

// inkWidth draws "Yesterday" at 9pt for a widget at widgetDPI on a canvas that reports canvasDPI
// (walk's buffered canvas reports the system DPI) and returns the drawn width in pixels.
func inkWidth(t *testing.T, canvasDPI, widgetDPI int) int {
	t.Helper()
	bmp, err := walk.NewBitmapForDPI(walk.Size{Width: 200, Height: 60}, canvasDPI)
	if err != nil {
		t.Fatal(err)
	}
	defer bmp.Dispose()
	c, err := walk.NewCanvasFromImage(bmp)
	if err != nil {
		t.Fatal(err)
	}
	// white on the bitmap's black: walk's rectangle fills need a window-initialised walk
	err = drawText(c, widgetDPI, "Yesterday", 9, 0, walk.RGB(255, 255, 255), walk.Rectangle{Width: 200, Height: 60}, leftText)
	c.Dispose()
	if err != nil {
		t.Fatal(err)
	}
	img, err := bmp.ToImage()
	if err != nil {
		t.Fatal(err)
	}
	left, right := 200, -1
	for y := 0; y < 60; y++ {
		for x := 0; x < 200; x++ {
			if p := img.RGBAAt(x, y); p.G > 128 {
				left, right = min(left, x), max(right, x)
			}
		}
	}
	return right - left + 1
}

func TestTextFollowsWidgetDPI(t *testing.T) {
	base := inkWidth(t, 96, 96)
	if got := inkWidth(t, 144, 96); got < base-2 || got > base+2 {
		t.Errorf("system DPI 144, window at 96: text %dpx wide, want ~%d (sized for the window)", got, base)
	}
	if got := inkWidth(t, 96, 144); got*100 < base*135 || got*100 > base*165 {
		t.Errorf("window at 144 on a 96 canvas: text %dpx wide, want ~1.5x %d", got, base)
	}
}

func TestInitialsAndDisplayName(t *testing.T) {
	for _, c := range []struct{ name, jid, want string }{
		{"Alice Smith", alice, "AS"},
		{"mom ❤", alice, "M"},
		{"", alice, "33"},
		{"❤❤", bob, "44"},
	} {
		if got := initials(c.name, c.jid); got != c.want {
			t.Errorf("initials(%q) = %q, want %q", c.name, got, c.want)
		}
	}
	if got := displayName(store.Chat{JID: alice}); got != "+6283333333333" {
		t.Errorf("displayName fallback %q", got)
	}
}
