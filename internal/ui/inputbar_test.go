package ui

import (
	"slices"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/lxn/walk"

	"wa-go/internal/store"
)

func TestEnterSendsShiftEnterBreaksTheLine(t *testing.T) {
	for _, c := range []struct {
		k     walk.Key
		shift bool
		want  bool
	}{{walk.KeyReturn, false, true}, {walk.KeyReturn, true, false}, {walk.KeyA, false, false}, {walk.KeyTab, false, false}} {
		if got := enterSends(c.k, c.shift); got != c.want {
			t.Errorf("enterSends(%v, shift %v) = %v", c.k, c.shift, got)
		}
	}
}

func TestOutgoingTextIsTrimmedAndNeverBlank(t *testing.T) {
	for in, want := range map[string]string{
		"  hello \r\n":       "hello",
		"line one\r\nline 2": "line one\r\nline 2", // inner line breaks stay
		"\t👍 ":               "👍",
		"":                   "",
		" \r\n\t \r\n":       "",
	} {
		got, ok := outgoingText(in)
		if got != want || ok != (want != "") {
			t.Errorf("outgoingText(%q) = %q, %v", in, got, ok)
		}
	}
}

// fakeBox stands in for the edit control.
type fakeBox struct{ text string }

func (b *fakeBox) Text() string           { return b.text }
func (b *fakeBox) SetText(s string) error { b.text = s; return nil }

// fakeTimers records one-shot timers so tests fire them by hand instead of sleeping.
type fakeTimers struct{ timers []*fakeTimer }

type fakeTimer struct {
	d       time.Duration
	f       func()
	stopped bool
}

func (ft *fakeTimers) after(d time.Duration, f func()) func() bool {
	t := &fakeTimer{d: d, f: f}
	ft.timers = append(ft.timers, t)
	return func() bool { was := !t.stopped; t.stopped = true; return was }
}

// live counts timers neither fired nor stopped.
func (ft *fakeTimers) live() int {
	n := 0
	for _, t := range ft.timers {
		if !t.stopped {
			n++
		}
	}
	return n
}

// fire runs timer i as its expiry would (it is spent afterwards).
func (ft *fakeTimers) fire(i int) {
	ft.timers[i].stopped = true
	ft.timers[i].f()
}

func TestSendClearsTheBoxAndFiresOnce(t *testing.T) {
	ft := &fakeTimers{}
	var sent []string
	var typing []bool
	box := &fakeBox{}
	c := &composer{box: box, onSend: func(s string) { sent = append(sent, s) },
		typing: typingSignal{after: ft.after, fire: func(on bool) { typing = append(typing, on) }}}

	box.text = "  see you at 7 \r\n"
	c.edited()
	if !c.send() || box.text != "" {
		t.Fatalf("send: box %q", box.text)
	}
	c.edited() // the clearing is an edit too
	if c.send() {
		t.Error("second send of the cleared box sent")
	}
	if !slices.Equal(sent, []string{"see you at 7"}) {
		t.Errorf("sent %q", sent)
	}
	if !slices.Equal(typing, []bool{true, false}) || ft.live() != 0 {
		t.Errorf("typing %v, %d timers left", typing, ft.live())
	}

	box.text = " \r\n "
	if c.send() || box.text != " \r\n " || len(sent) != 1 {
		t.Errorf("blank text: sent %q, box %q", sent, box.text)
	}
}

func TestTypingSignalComposingThenPausedWhenIdle(t *testing.T) {
	ft := &fakeTimers{}
	var got []bool
	s := typingSignal{after: ft.after, fire: func(on bool) { got = append(got, on) }}

	s.edited() // first edit after idle: composing
	s.edited() // still typing: no signal, the idle timer restarts
	if !slices.Equal(got, []bool{true}) || ft.live() != 1 || ft.timers[1].d != typingIdle {
		t.Fatalf("while typing: %v, %d live timers, timer %v", got, ft.live(), ft.timers[1].d)
	}
	ft.fire(1) // typingIdle without an edit: paused
	if !slices.Equal(got, []bool{true, false}) || ft.live() != 0 {
		t.Fatalf("after idle: %v, %d live timers", got, ft.live())
	}
	ft.timers[0].f() // a stale expiry that raced its Stop: ignored
	s.stop()         // already idle: nothing to say
	if !slices.Equal(got, []bool{true, false}) {
		t.Fatalf("stale expiry or idle stop signalled: %v", got)
	}

	s.edited()
	ft.fire(1) // an expiry queued before the edit restarted the timer: ignored
	s.stop()   // sent or cleared: paused at once
	if !slices.Equal(got, []bool{true, false, true, false}) || ft.live() != 0 {
		t.Fatalf("stop: %v, %d live timers", got, ft.live())
	}
}

func TestEditingToBlankPausesTyping(t *testing.T) {
	ft := &fakeTimers{}
	var got []bool
	box := &fakeBox{text: "h"}
	c := &composer{box: box, typing: typingSignal{after: ft.after, fire: func(on bool) { got = append(got, on) }}}
	c.edited()
	box.text = ""
	c.edited()
	if !slices.Equal(got, []bool{true, false}) || ft.live() != 0 {
		t.Fatalf("typing %v, %d live timers", got, ft.live())
	}
}

func TestGridKeys(t *testing.T) {
	// 10 cells in rows of 4: 0-3, 4-7, 8-9
	for _, c := range []struct {
		cur  int
		k    walk.Key
		want int
	}{
		{-1, walk.KeyRight, 0}, {-1, walk.KeyDown, 0},
		{5, walk.KeyRight, 6}, {5, walk.KeyLeft, 4}, {0, walk.KeyLeft, 0}, {9, walk.KeyRight, 9},
		{5, walk.KeyUp, 1}, {1, walk.KeyUp, 1}, {1, walk.KeyDown, 5},
		{6, walk.KeyDown, 9}, // the last row is short: lands on its last cell
		{9, walk.KeyDown, 9}, {5, walk.KeyHome, 0}, {5, walk.KeyEnd, 9},
		{1, walk.KeyNext, 9}, {9, walk.KeyPrior, 1},
	} {
		got, ok := gridKey(c.cur, 10, 4, 8, c.k)
		if !ok || got != c.want {
			t.Errorf("gridKey(%d, %v) = %d, %v; want %d", c.cur, c.k, got, ok, c.want)
		}
	}
	if _, ok := gridKey(3, 10, 4, 8, walk.KeyA); ok {
		t.Error("letter handled")
	}
	if got, ok := gridKey(-1, 0, 4, 8, walk.KeyDown); !ok || got != -1 {
		t.Errorf("empty grid: %d, %v", got, ok)
	}
}

func TestPopupSitsAboveItsButtonInsideTheScreen(t *testing.T) {
	work := walk.Rectangle{X: 0, Y: 0, Width: 1920, Height: 1040}
	size := walk.Size{Width: 400, Height: 380}
	for _, c := range []struct {
		name   string
		anchor walk.Rectangle
		want   walk.Rectangle
	}{
		{"above, left-aligned", walk.Rectangle{X: 300, Y: 980, Width: 42, Height: 42}, walk.Rectangle{X: 300, Y: 600, Width: 400, Height: 380}},
		{"pushed in from the right edge", walk.Rectangle{X: 1800, Y: 980, Width: 42, Height: 42}, walk.Rectangle{X: 1520, Y: 600, Width: 400, Height: 380}},
		{"below when no room above", walk.Rectangle{X: 300, Y: 100, Width: 42, Height: 42}, walk.Rectangle{X: 300, Y: 142, Width: 400, Height: 380}},
		{"pinned to the bottom when neither fits", walk.Rectangle{X: 300, Y: 300, Width: 42, Height: 700}, walk.Rectangle{X: 300, Y: 660, Width: 400, Height: 380}},
	} {
		if got := placePopup(c.anchor, size, work); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestEmojiGridHasAHundredDistinctEmoji(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range emojis {
		if e == "" || !utf8.ValidString(e) || seen[e] {
			t.Errorf("bad or repeated emoji %q", e)
		}
		seen[e] = true
	}
	if len(emojis) != 100 {
		t.Errorf("%d emoji, want 100 (a 10 x 10 grid)", len(emojis))
	}
}

// echoBox reports its changes as the edit control does: from inside SetText.
type echoBox struct {
	fakeBox
	onSet func()
}

func (b *echoBox) SetText(s string) error {
	b.text = s
	if b.onSet != nil {
		b.onSet()
	}
	return nil
}

func TestRestorePutsAFailedMessageBackOnlyIntoABlankBox(t *testing.T) {
	ft := &fakeTimers{}
	var typing []bool
	box := &echoBox{fakeBox: fakeBox{text: "  "}}
	c := &composer{box: box, typing: typingSignal{after: ft.after, fire: func(on bool) { typing = append(typing, on) }}}
	box.onSet = c.edited
	if !c.restore("hello") || box.Text() != "hello" {
		t.Fatalf("blank box: text %q", box.Text())
	}
	if len(typing) != 0 || ft.live() != 0 {
		t.Fatalf("restoring signalled typing %v (%d timers)", typing, ft.live())
	}
	box.SetText("new draft")
	if c.restore("hello") || box.Text() != "new draft" {
		t.Fatalf("a new draft was replaced: %q", box.Text())
	}
}

func TestInputBarReplyBannerState(t *testing.T) {
	c := &composer{box: &fakeBox{}}
	if _, ok := c.replyTarget(); ok {
		t.Fatal("a fresh composer has a reply target")
	}
	c.setReply(store.Message{ID: "m1", Body: "hello"}, "Alice")
	if m, ok := c.replyTarget(); !ok || m.ID != "m1" || c.replyAuthor != "Alice" {
		t.Fatalf("reply target = %+v %v, author %q", m, ok, c.replyAuthor)
	}
	c.clearReply()
	if _, ok := c.replyTarget(); ok || c.replyAuthor != "" {
		t.Fatal("ClearReply left the target")
	}
}

var testMembers = []GroupMember{
	{JID: "111@lid", Name: "Budi Santoso", Phone: "62811"},
	{JID: "62822@s.whatsapp.net", Name: "Puteri", Push: "putri99", Phone: "62822"},
	{JID: "62833@s.whatsapp.net", Name: "62833", Phone: "62833"},
}

func names(ms []GroupMember) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

func TestMentionAutocompleteFiltering(t *testing.T) {
	for q, want := range map[string][]string{
		"P":     {"all", "Puteri"},
		"z":     {"all"},
		"":      {"all", "Budi Santoso", "Puteri", "62833"},
		"SANT":  {"all", "Budi Santoso"},
		"putri": {"all", "Puteri"},                // push name
		"6281":  {"all", "Budi Santoso"},          // phone digits
		"+6283": {"all", "62833"},
	} {
		if got := names(filterMentions(testMembers, q)); !slices.Equal(got, want) {
			t.Errorf("filter %q = %v, want %v", q, got, want)
		}
	}
	for before, want := range map[string]string{"@": "", "hi @Pu": "Pu", "line\r\n@x": "x"} {
		if q, ok := mentionQuery(before); !ok || q != want {
			t.Errorf("mentionQuery(%q) = %q %v, want %q", before, q, ok, want)
		}
	}
	for _, before := range []string{"", "hello", "mail me@x", "@Pu teri", "@all "} {
		if q, ok := mentionQuery(before); ok {
			t.Errorf("mentionQuery(%q) = %q: no mention is being typed", before, q)
		}
	}
}

func TestMentionPickerKeysAndCompletion(t *testing.T) {
	p := mentionPicker{}
	if p.refresh("@"); len(p.items) != 0 {
		t.Fatal("the list opened outside a group")
	}
	p.members = testMembers
	p.refresh("hi @")
	if len(p.items) != 4 || p.sel != 0 {
		t.Fatalf("items %v sel %d, want everyone with @all selected", names(p.items), p.sel)
	}
	p.move(-1)
	p.move(1)
	p.move(1)
	p.move(9)
	if p.sel != 3 {
		t.Fatalf("sel = %d, want clamped to the last row", p.sel)
	}
	// typing a name selects its first match, not @all, so Enter completes what was typed
	p.refresh("hi @Pu")
	if p.sel != 1 {
		t.Fatalf("sel = %d, want the first member match", p.sel)
	}
	text, caret := p.complete("hi @Pu", " there")
	if text != "hi @Puteri  there" || caret != len("hi @Puteri ") || len(p.items) != 0 {
		t.Fatalf("complete = %q caret %d, list %v", text, caret, names(p.items))
	}
	// the caret counts UTF-16 units, as the edit control does
	p.refresh("😀 @al")
	if text, caret = p.complete("😀 @al", ""); text != "😀 @all " || caret != 8 {
		t.Fatalf("complete = %q caret %d", text, caret)
	}
	p.refresh("@z")
	if p.close(); len(p.items) != 0 {
		t.Fatal("close left the list open")
	}
}

func TestWireMentionsReplaceNamesWithNumbers(t *testing.T) {
	text, jids := wireMentions("@Budi Santoso and @Puteri, not mail@Puteri or @Puterium; @all @Puteri", testMembers)
	if want := "@111 and @62822, not mail@Puteri or @Puterium; @all @62822"; text != want {
		t.Fatalf("wire text = %q, want %q", text, want)
	}
	if !slices.Equal(jids, []string{"111@lid", "62822@s.whatsapp.net"}) {
		t.Fatalf("mentioned = %v", jids)
	}
	if text, jids = wireMentions("plain @all", nil); text != "plain @all" || len(jids) != 0 {
		t.Fatalf("no members: %q %v", text, jids)
	}
}
