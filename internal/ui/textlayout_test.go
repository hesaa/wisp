package ui

import (
	"slices"
	"testing"
	"unicode/utf16"
)

// tenPx is a fake adv: every unit 10px wide, so a surrogate pair is 20px.
func tenPx(u []uint16) []int {
	x := make([]int, len(u))
	for i := range x {
		x[i] = 10 * (i + 1)
	}
	return x
}

// wrapped is text broken by wrapText at maxW with tenPx, as strings.
func wrapped(t *testing.T, text string, maxW int) []string {
	t.Helper()
	u := utf16.Encode([]rune(text))
	var out []string
	for _, l := range wrapText(u, maxW, tenPx) {
		if l.start > 0 && utf16.IsSurrogate(rune(u[l.start])) && u[l.start] >= 0xdc00 {
			t.Errorf("%q: a line starts on a low surrogate at %d", text, l.start)
		}
		out = append(out, string(utf16.Decode(u[l.start:l.end])))
	}
	return out
}

func wantLines(t *testing.T, text string, maxW int, want ...string) {
	t.Helper()
	if got := wrapped(t, text, maxW); !slices.Equal(got, want) {
		t.Errorf("%q at %dpx: lines %q, want %q", text, maxW, got, want)
	}
}

func TestWrapTextBreaksAfterSpaces(t *testing.T) {
	wantLines(t, "aaa bbb ccc", 60, "aaa ", "bbb ", "ccc")
	wantLines(t, "aaa bbb ccc", 70, "aaa bbb ", "ccc") // "aaa bbb" fits exactly; its trailing space need not
	wantLines(t, "aaa   bbb", 40, "aaa   ", "bbb")     // a run of spaces stays on the line it ends
}

func TestWrapTextHonoursNewlines(t *testing.T) {
	wantLines(t, "a\nb\r\nc", 100, "a", "b", "c")
	wantLines(t, "a\n\nb", 100, "a", "", "b")
	wantLines(t, "aa bb\r\ncc", 50, "aa bb", "cc") // the \r takes no room on a full line
}

func TestWrapTextSplitsLongWords(t *testing.T) {
	wantLines(t, "abcdefghij", 40, "abcd", "efgh", "ij")
	wantLines(t, "x abcdefghij", 40, "x ", "abcd", "efgh", "ij")
}

func TestWrapTextKeepsSurrogatePairs(t *testing.T) {
	wantLines(t, "ab😀cd", 30, "ab", "😀", "cd") // split before the pair, then break after the emoji
	wantLines(t, "😀😀", 10, "😀", "😀")           // a pair wider than the line still makes a line
}

func TestWrapTextBreaksBetweenEmoji(t *testing.T) {
	wantLines(t, "😀😀😀😀😀😀", 60, "😀😀😀", "😀😀😀")
	wantLines(t, "ok 😀😀😀", 70, "ok 😀😀", "😀") // as DrawText: the run breaks, not the space
	wantLines(t, "x ok😀", 50, "x ", "ok😀")   // but not between a letter and an emoji
	wantLines(t, "😀ok", 30, "😀", "ok")       // after an emoji it may
	wantLines(t, "😀😀!", 40, "😀", "😀!")       // never before closing punctuation
}

func TestWrapTextKeepsEmojiSequencesWhole(t *testing.T) {
	zwj, vs16, keycap := string(rune(0x200d)), string(rune(0xfe0f)), string(rune(0x20e3))
	family := "👨" + zwj + "👩" + zwj + "👧" + zwj + "👦" // 11 units, 110px
	wantLines(t, family+family, 120, family, family)
	wantLines(t, family+family, 50, family, family) // wider than the line, still whole
	wantLines(t, "👍🏽👍🏽", 30, "👍🏽", "👍🏽")            // skin tone
	heart := "❤" + vs16
	wantLines(t, heart+heart, 10, heart, heart) // variation selector
	wantLines(t, "1"+vs16+keycap+"#"+keycap, 20, "1"+vs16+keycap, "#"+keycap)
	wantLines(t, "🇮🇩🇯🇵", 30, "🇮🇩", "🇯🇵") // flags: no break between them, but never split
}

func TestWrapTextBreaksBetweenCJK(t *testing.T) {
	wantLines(t, "漢字漢字漢字", 40, "漢字漢字", "漢字")
	wantLines(t, "ab 漢字漢字漢字", 60, "ab 漢字漢", "字漢字") // fills the line instead of moving the run
	wantLines(t, "かなカナ한글", 30, "かなカ", "ナ한글")
	wantLines(t, "漢字漢。", 30, "漢字", "漢。") // no line starts with closing punctuation
}

func TestTextWidthCountsTrailingSpacesThatFit(t *testing.T) {
	for _, c := range []struct {
		text       string
		maxW, want int
	}{
		{"aaa bbb ccc", 60, 40}, // "aaa " | "bbb " | "ccc": the spaces fit
		{"aaa bbb ccc", 70, 70}, // "aaa bbb " | "ccc": the space does not
		{"aaa   bbb", 50, 50},   // "aaa   " | "bbb": two of three spaces fit
		{"a\nbb", 100, 20},
		{"😀😀", 10, 20}, // a character wider than the line
	} {
		u := utf16.Encode([]rune(c.text))
		if got := textWidth(u, tenPx(u), wrapText(u, c.maxW, tenPx), c.maxW); got != c.want {
			t.Errorf("%q at %dpx: width %d, want %d", c.text, c.maxW, got, c.want)
		}
	}
}

func TestWrapTextEmpty(t *testing.T) {
	if lines := wrapText(nil, 100, tenPx); len(lines) != 0 {
		t.Errorf("empty text: lines %v", lines)
	}
}

func TestHitOffsetMidpoints(t *testing.T) {
	adv := []int{10, 20, 30}
	for x, want := range map[int]int{4: 0, 6: 1, 31: 3, -5: 0, 5: 0} { // a tie keeps the earlier boundary
		if got := hitOffset(adv, x); got != want {
			t.Errorf("hitOffset(%v, %d) = %d, want %d", adv, x, got, want)
		}
	}
	if got := hitOffset(nil, 7); got != 0 {
		t.Errorf("hitOffset of an empty line = %d", got)
	}
}

func TestHitOffsetLandsOnWholeCharacters(t *testing.T) {
	// monoMeasurer gives a surrogate pair's width to its first half: lineAdv must still keep hits off it
	u := utf16.Encode([]rune("a😀b👍🏽c" + "👨\u200d👩"))
	starts := map[int]bool{len(u): true}
	for i := 0; i < len(u); {
		starts[i] = true
		_, n := cluster(u, i)
		i += n
	}
	adv := lineAdv(&monoMeasurer{}, u, nil, 0)
	for x := -5; x < 80; x++ {
		if off := hitOffset(adv, x); !starts[off] {
			t.Errorf("x=%d hits offset %d, inside a character", x, off)
		}
	}
	if got := hitOffset(adv, 10); got != 1 { // the emoji spans 7 to 14: before its middle
		t.Errorf("x=10 hits %d, want 1", got)
	}
	if got := hitOffset(adv, 11); got != 3 { // past its middle: after it
		t.Errorf("x=11 hits %d, want 3", got)
	}
}

func TestWordAt(t *testing.T) {
	for _, c := range []struct {
		text       string
		off        int
		start, end int
	}{
		{"halo dunia!", 6, 5, 10},
		{"halo dunia!", 4, 4, 5},    // a space is its own character
		{"halo dunia!", 10, 10, 11}, // so is punctuation
		{"halo dunia!", 11, 10, 11}, // past the end: the last character
		{"kopi☕susu", 4, 4, 5},      // the emoji alone
		{"kopi☕susu", 3, 0, 4},
		{"kopi☕susu", 5, 5, 9},
		{"naïve", 2, 0, 5},
		{"nai\u0308ve", 2, 0, 6}, // a combining mark stays with its letter
		{"ab😀cd", 3, 2, 4},       // inside a surrogate pair: the pair
		{"Rp 1500 ok", 5, 3, 7},  // digits form words too
		{"東京タワー", 1, 0, 5},
	} {
		u := utf16.Encode([]rune(c.text))
		if s, e := wordAt(u, c.off); s != c.start || e != c.end {
			t.Errorf("wordAt(%q, %d) = (%d, %d), want (%d, %d)", c.text, c.off, s, e, c.start, c.end)
		}
	}
	if s, e := wordAt(nil, 0); s != 0 || e != 0 {
		t.Errorf("wordAt of no text = (%d, %d)", s, e)
	}
}

func TestSelectedTextAcrossMessages(t *testing.T) {
	enc := func(s string) []uint16 { return utf16.Encode([]rune(s)) }
	texts := [][]uint16{enc("Hi 😀 there"), enc("line1\r\nline2")}
	if got := selectedText(texts, textPos{0, 3}, textPos{1, 5}); got != "😀 there\r\nline1" {
		t.Errorf("across two messages: %q", got)
	}
	if got := selectedText(texts, textPos{1, 0}, textPos{1, 12}); got != "line1\r\nline2" {
		t.Errorf("a CRLF inside a message: %q", got)
	}
	texts = [][]uint16{enc("one\ntwo"), nil, enc("three")} // a photo without caption between
	if got := selectedText(texts, textPos{0, 4}, textPos{2, 5}); got != "two\r\nthree" {
		t.Errorf("skipping a message without text: %q", got)
	}
	if got := selectedText(texts, textPos{0, 0}, textPos{0, 7}); got != "one\r\ntwo" {
		t.Errorf("a lone LF becomes CRLF: %q", got)
	}
	if got := selectedText(texts, textPos{0, 7}, textPos{2, 0}); got != "" {
		t.Errorf("nothing between the end of one text and the start of another: %q", got)
	}
}

func TestOrderPos(t *testing.T) {
	for _, c := range [][4]textPos{
		{{2, 1}, {0, 4}, {0, 4}, {2, 1}}, // dragged backwards
		{{1, 5}, {1, 2}, {1, 2}, {1, 5}},
		{{0, 4}, {2, 1}, {0, 4}, {2, 1}},
	} {
		if from, to := orderPos(c[0], c[1]); from != c[2] || to != c[3] {
			t.Errorf("orderPos(%v, %v) = %v, %v", c[0], c[1], from, to)
		}
	}
}

func TestSelectionOffsetsStayOnCharacters(t *testing.T) {
	// offsets left inside a character by an edit of the message snap to its start
	u := utf16.Encode([]rune("a😀b")) // a, the pair at 1-2, b at 3
	for _, c := range []struct {
		from, to   int
		start, end int
	}{
		{0, 2, 0, 1},  // the end inside the pair: the emoji is left out
		{2, 4, 1, 4},  // the start inside it: the emoji is taken whole
		{0, 99, 0, 4}, // past the end of a shortened text
		{2, 2, 1, 1},
	} {
		if s, e := selSpan(textPos{0, c.from}, textPos{0, c.to}, 0, u); s != c.start || e != c.end {
			t.Errorf("selSpan %d..%d = %d..%d, want %d..%d", c.from, c.to, s, e, c.start, c.end)
		}
	}
	family := utf16.Encode([]rune("x👨\u200d👩y")) // the family sequence is units 1-5
	if s, e := selSpan(textPos{0, 0}, textPos{0, 4}, 0, family); s != 0 || e != 1 {
		t.Errorf("an end inside an emoji sequence: %d..%d, want 0..1", s, e)
	}
	texts := [][]uint16{utf16.Encode([]rune("Hi 😀")), u}
	if got := selectedText(texts, textPos{0, 4}, textPos{1, 2}); got != "😀\r\na" {
		t.Errorf("copied across stale offsets: %q", got)
	}
}
