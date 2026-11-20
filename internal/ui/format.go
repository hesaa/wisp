package ui

import (
	"slices"
	"unicode"
	"unicode/utf16"

	"github.com/lxn/walk"
)

// WhatsApp's text formatting: *bold*, _italic_, ~strikethrough~, `inline code` and ```monospace```
// (which may span lines), and paragraphs starting "> " (a quote), "* " or "- " (a bulleted item) or
// "1. " (a numbered one). The markers are hidden, and the text between drawn in that style.

// Style bits walk lacks. font draws fontMono in Consolas and ignores the others, which the chat
// draws itself: an indent and bar for paragraphs, a shaded box for inline code.
const (
	fontList  walk.FontStyle = 0x10 // a list item: indented
	fontQuote walk.FontStyle = 0x20 // a quote: indented behind a bar
	fontCode  walk.FontStyle = 0x40 // inline code (with fontMono): on a shaded box
	fontMono  walk.FontStyle = 0x80
)

// Paragraph indents, 1/96 inch.
const (
	quoteIndent = 12
	quoteBarW   = 3
	listIndent  = 12
)

type fchar struct {
	r    rune
	st   walk.FontStyle
	at   int  // UTF-16 offset in the text before formatting
	lock bool // inside a mention or code: never a marker
}

// formatText hides text's formatting markers. It returns the text shown, spans (UTF-16 spans of
// text, the mentions) moved to match, and the style of each UTF-16 unit of the text shown, nil when
// there was nothing to format.
func formatText(text string, spans [][2]int) (string, [][2]int, []walk.FontStyle) {
	cs := make([]fchar, 0, len(text))
	at := 0
	for _, r := range text {
		cs = append(cs, fchar{r: r, at: at})
		at += utf16.RuneLen(r)
	}
	for _, sp := range spans {
		for i := range cs {
			if cs[i].at >= sp[0] && cs[i].at < sp[1] {
				cs[i].lock = true
			}
		}
	}
	n := len(cs)
	cs = markBlocks(cs)
	cs = pairMarkers(cs, "```", fontMono, true)
	cs = pairMarkers(cs, "`", fontMono|fontCode, true)
	for _, m := range []struct {
		mark string
		st   walk.FontStyle
	}{{"*", walk.FontBold}, {"_", walk.FontItalic}, {"~", walk.FontStrikeOut}} {
		cs = pairMarkers(cs, m.mark, m.st, false)
	}
	if len(cs) == n && !slices.ContainsFunc(cs, func(c fchar) bool { return c.st != 0 }) {
		return text, spans, nil
	}
	var out []rune
	var styles []walk.FontStyle
	moved := make([][2]int, len(spans))
	for i := range moved {
		moved[i] = [2]int{-1, -1}
	}
	for _, c := range cs {
		u := len(styles)
		for i, sp := range spans {
			if c.at >= sp[0] && c.at < sp[1] {
				if moved[i][0] < 0 {
					moved[i][0] = u
				}
				moved[i][1] = u + utf16.RuneLen(c.r)
			}
		}
		out = append(out, c.r)
		for range utf16.RuneLen(c.r) {
			styles = append(styles, c.st)
		}
	}
	var kept [][2]int
	for _, sp := range moved {
		if sp[0] >= 0 {
			kept = append(kept, sp)
		}
	}
	return string(out), kept, styles
}

// markBlocks styles the paragraphs of cs that start as a quote ("> ", hidden) or a list item ("* "
// and "- " shown as "• ", "1. " as it is).
func markBlocks(cs []fchar) []fchar {
	at := func(i int, s string) bool {
		for _, r := range s {
			if i >= len(cs) || cs[i].r != r || cs[i].lock {
				return false
			}
			i++
		}
		return true
	}
	for i := 0; i < len(cs); i++ {
		if i > 0 && cs[i-1].r != '\n' {
			continue
		}
		st := walk.FontStyle(0)
		switch d := i; {
		case at(i, "> "):
			cs, st = append(cs[:i], cs[i+2:]...), fontQuote
		case at(i, "* ") || at(i, "- "):
			cs[i].r, cs[i].lock, st = '•', true, fontList
		default:
			for d < len(cs) && cs[d].r >= '0' && cs[d].r <= '9' {
				d++
			}
			if d > i && at(d, ". ") {
				st = fontList
			}
		}
		for j := i; st != 0 && j < len(cs) && cs[j].r != '\n'; j++ {
			cs[j].st |= st
		}
	}
	return cs
}

// pairMarkers finds mark ... mark pairs in cs left to right, drops the markers and gives the text
// between style st. code pairs need only text between them, may span lines and lock what they
// hold (inline code keeps to one line); the others must hug their text (no space inside the markers), keep to one line and stand
// apart from letters and digits outside, as WhatsApp's do: snake_case_names stay as they are.
func pairMarkers(cs []fchar, mark string, st walk.FontStyle, code bool) []fchar {
	m := []rune(mark)
	isMark := func(i int) bool {
		if i < 0 || i+len(m) > len(cs) {
			return false
		}
		for k, r := range m {
			if cs[i+k].r != r || cs[i+k].lock {
				return false
			}
		}
		return true
	}
	word := func(i int) bool { // a letter or digit at i
		return i >= 0 && i < len(cs) && (unicode.IsLetter(cs[i].r) || unicode.IsDigit(cs[i].r))
	}
	space := func(i int) bool { return i < 0 || i >= len(cs) || unicode.IsSpace(cs[i].r) }
	for i := 0; i < len(cs); i++ {
		if !isMark(i) || (!code && (word(i-1) || space(i+len(m)))) || (mark == "`" && isMark(i+1)) {
			continue // "``" does not start inline code: the rest of an unpaired ```
		}
		in := i + len(m) // first character inside
		for j := in + 1; j < len(cs); j++ {
			if len(m) == 1 && cs[j].r == '\n' {
				break
			}
			if !isMark(j) || (!code && (space(j-1) || word(j+len(m)))) {
				continue
			}
			for k := in; k < j; k++ {
				cs[k].st |= st
				cs[k].lock = cs[k].lock || code
			}
			cs = append(cs[:j], cs[j+len(m):]...)
			cs = append(cs[:i], cs[in:]...)
			i = j - len(m) - 1 // resume after the closing marker's place
			break
		}
	}
	return cs
}

// styledAdv is the x after each unit of units, each run of one style measured in it over base.
func styledAdv(ms measurer, units []uint16, styles []walk.FontStyle, base walk.FontStyle) []int {
	if styles == nil {
		return ms.advances(units, bodyPt, base)
	}
	x, off := make([]int, len(units)), 0
	for i := 0; i < len(units); {
		j := i
		for j < len(units) && styles[j] == styles[i] {
			j++
		}
		for k, a := range ms.advances(units[i:j], bodyPt, base|styles[i]) {
			x[i+k] = off + a
		}
		off, i = x[j-1], j
	}
	return x
}

// lineStyles is the formatting of line ln of bubble b, nil for none.
func lineStyles(b bubble, ln textLine) []walk.FontStyle {
	if b.styles == nil {
		return nil
	}
	return b.styles[ln.start:ln.end]
}

// blockIndent is the indent, in pixels, of the line starting at unit at of text styled styles.
func blockIndent(styles []walk.FontStyle, at int, s func(int) int) int {
	switch {
	case at >= len(styles):
		return 0
	case styles[at]&fontQuote != 0:
		return s(quoteIndent)
	case styles[at]&fontList != 0:
		return s(listIndent)
	}
	return 0
}

// wrapBlocks is wrapText for text with indented paragraphs (blockIndent): each is wrapped to maxW
// less its indent, a list item's later lines further in, under its text rather than its bullet
// ("• " or "1. "). It returns the lines, each line's indent in pixels (nil when none has one) and
// x, all of units' cumulative advances.
func wrapBlocks(units []uint16, maxW int, styles []walk.FontStyle, s func(int) int, adv func(u []uint16) []int) ([]textLine, []int, []int) {
	x := adv(units)
	same := func([]uint16) []int { return x }
	if !slices.ContainsFunc(styles, func(st walk.FontStyle) bool { return st&(fontQuote|fontList) != 0 }) {
		return wrapText(units, maxW, same), nil, x
	}
	var lines []textLine
	var indents []int
	for start := 0; ; {
		end := start
		for end < len(units) && units[end] != '\n' {
			end++
		}
		pend := end
		if pend > start && units[pend-1] == '\r' {
			pend--
		}
		ind, from := blockIndent(styles, start, s), start // from: where the wrapped text starts
		if start < len(styles) && styles[start]&fontList != 0 {
			if k := slices.Index(units[start:pend], ' '); k >= 0 {
				from = start + k + 1 // past the bullet or number and its space
			}
		}
		hang, base := widthOf(x, start, from), widthOf(x, 0, from)
		ls := wrapText(units[from:pend], maxW-ind-hang, func(u []uint16) []int {
			r := make([]int, len(u))
			for k := range u {
				r[k] = x[from+k] - base
			}
			return r
		})
		if len(ls) == 0 {
			ls = []textLine{{}}
		}
		for k, ln := range ls {
			if k == 0 {
				lines, indents = append(lines, textLine{start, from + ln.end}), append(indents, ind)
			} else {
				lines, indents = append(lines, textLine{from + ln.start, from + ln.end}), append(indents, ind+hang)
			}
		}
		if end == len(units) {
			return lines, indents, x
		}
		start = end + 1
	}
}

// lineIndent is line i's indent from wrapBlocks' indents.
func lineIndent(indents []int, i int) int {
	if i < len(indents) {
		return indents[i]
	}
	return 0
}
