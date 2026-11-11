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
