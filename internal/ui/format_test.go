package ui

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/lxn/walk"
)

func TestFormatText(t *testing.T) {
	B, I, S, M := walk.FontBold, walk.FontItalic, walk.FontStrikeOut, fontMono
	cases := []struct {
		in, want string
		styled   string // a rune per shown rune: . none, b, i, s, m, x bold+italic
	}{
		{"plain text", "plain text", ""},
		{"*hi* there", "hi there", "bb......"},
		{"_a_ ~b~", "a b", "i.s"},
		{"*_both_*", "both", "xxxx"},
		{"snake_case_name", "snake_case_name", ""},
		{"2*3*4", "2*3*4", ""},
		{"- item\n2. two", "• item\n2. two", "llllll.llllll"},
		{"* not bold *", "• not bold *", "llllllllllll"},
		{"> *hi*", "hi", "qq"},
		{"*across\nlines*", "*across\nlines*", ""},
		{"`*x*`", "*x*", "ccc"},
		{"```a\n_b_```", "a\n_b_", "mmmmm"},
		{"(*ok*).", "(ok).", ".bb.."},
	}
	look := map[rune]walk.FontStyle{'.': 0, 'b': B, 'i': I, 's': S, 'm': M, 'x': B | I, 'c': M | fontCode, 'l': fontList, 'q': fontQuote | B}
	for _, c := range cases {
		got, _, styles := formatText(c.in, nil)
		if got != c.want {
			t.Errorf("%q shows %q, want %q", c.in, got, c.want)
			continue
		}
		if c.styled == "" {
			if styles != nil {
				t.Errorf("%q: styles %v, want none", c.in, styles)
			}
			continue
		}
		for i, r := range c.styled {
			if styles[i] != look[r] {
				t.Errorf("%q: unit %d styled %v, want %c", c.in, i, styles[i], r)
			}
		}
	}
}

func TestFormatTextMentions(t *testing.T) {
	// a mention's name is never a marker, and its span follows the hidden markers
	text := "*hey* @Jo_hn_ny"
	got, spans, _ := formatText(text, [][2]int{{6, 15}})
	if got != "hey @Jo_hn_ny" || len(spans) != 1 || spans[0] != [2]int{4, 13} {
		t.Errorf("got %q %v", got, spans)
	}
	u := utf16.Encode([]rune("*😀*"))
	got, _, styles := formatText(string(utf16.Decode(u)), nil)
	if got != "😀" || len(styles) != 2 || styles[1] != walk.FontBold {
		t.Errorf("emoji: %q %v", got, styles)
	}
}

func TestFormatTextSample(t *testing.T) {
	in := "> *Important Update:* Please review the _project guidelines_ before tomorrow's meeting, as the ~old deadline~ has changed.\n\n" +
		"Use the code `STATUS2026` in the portal and complete these steps:\n1. Finalize the budget\n2. Confirm the guest list\n\n" +
		"Additional notes:\n* Prepare the presentation slides\n* Test the audio setup\n\n```System ready for launch```\n```"
	want := "Important Update: Please review the project guidelines before tomorrow's meeting, as the old deadline has changed.\n\n" +
		"Use the code STATUS2026 in the portal and complete these steps:\n1. Finalize the budget\n2. Confirm the guest list\n\n" +
		"Additional notes:\n• Prepare the presentation slides\n• Test the audio setup\n\nSystem ready for launch\n```"
	got, _, styles := formatText(in, nil)
	if got != want {
		t.Fatalf("shows\n%s\nwant\n%s", got, want)
	}
	for word, st := range map[string]walk.FontStyle{
		"Important": fontQuote | walk.FontBold, "project": fontQuote | walk.FontItalic, "old deadline": fontQuote | walk.FontStrikeOut,
		"STATUS2026": fontMono | fontCode, "Finalize": fontList, "• Test": fontList, "System": fontMono, "Additional": 0,
	} {
		at := len(utf16.Encode([]rune(got[:strings.Index(got, word)])))
		if styles[at] != st {
			t.Errorf("%q styled %#x, want %#x", word, styles[at], st)
		}
	}
}

func TestWrapBlocksIndents(t *testing.T) {
	// 7px a character, 70px wide: a quote's lines wrap 12px narrower, the plain line does not
	got, _, styles := formatText("> aaaa bbbb\ncccc dddd e", nil)
	u := utf16.Encode([]rune(got))
	one := func(n int) int { return n }
	lines, indents, _ := wrapBlocks(u, 70, styles, one, func(u []uint16) []int { return (&monoMeasurer{}).advances(u, 10, 0) })
	var shown []string
	for _, ln := range lines {
		shown = append(shown, string(utf16.Decode(u[ln.start:ln.end])))
	}
	if strings.Join(shown, "|") != "aaaa |bbbb|cccc dddd |e" {
		t.Errorf("lines %q", shown)
	}
	if fmt.Sprint(indents) != fmt.Sprint([]int{quoteIndent, quoteIndent, 0, 0}) {
		t.Errorf("indents %v: a quote's lines are indented, the next paragraph's not", indents)
	}

	// a list item's later lines hang under its text: past the 12px indent and "• " (14px)
	got, _, styles = formatText("- aaaa bbbb cccc\nok", nil)
	u = utf16.Encode([]rune(got))
	lines, indents, _ = wrapBlocks(u, 70, styles, one, func(u []uint16) []int { return (&monoMeasurer{}).advances(u, 10, 0) })
	shown = nil
	for _, ln := range lines {
		shown = append(shown, string(utf16.Decode(u[ln.start:ln.end])))
	}
	if strings.Join(shown, "|") != "• aaaa |bbbb |cccc|ok" {
		t.Errorf("list lines %q", shown)
	}
	if want := []int{listIndent, listIndent + 14, listIndent + 14, 0}; fmt.Sprint(indents) != fmt.Sprint(want) {
		t.Errorf("list indents %v, want %v", indents, want)
	}
}
