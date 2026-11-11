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
	wantLines(t, "ab😀cd", 30, "ab", "😀c", "d")
	wantLines(t, "😀😀", 10, "😀", "😀") // a pair wider than the line still makes a line
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
