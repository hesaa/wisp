package ui

import (
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/lxn/walk"
)

// textLine is one line of laid-out text: UTF-16 offsets [start, end) into the text, without the
// newline that ended it. Trailing spaces belong to the line.
type textLine struct{ start, end int }

// wrapText breaks units into lines at most maxW pixels wide, as DrawText's word break does: at
// newlines ("\r\n" counts as one), after the last space or other break opportunity (breakBetween)
// that fits, and inside a word wider than the line, never inside a character (cluster); every line
// holds at least one. Trailing spaces never push a line past maxW. adv is called once, with all of
// units, and returns the cumulative width after each unit.
func wrapText(units []uint16, maxW int, adv func(u []uint16) []int) []textLine {
	if len(units) == 0 {
		return nil
	}
	x := adv(units)
	var lines []textLine
	start, brk := 0, 0 // brk: the line's last break opportunity; at or before start when it has none
	prev := rune(' ')  // the character before i
	for i := 0; i < len(units); {
		switch u := units[i]; {
		case u == '\n':
			end := i
			if end > start && units[end-1] == '\r' {
				end--
			}
			lines = append(lines, textLine{start, end})
			i++
			start = i
		case u == ' ':
			i++
			brk, prev = i, ' '
		case u == '\r' && i+1 < len(units) && units[i+1] == '\n':
			i++ // part of the newline, not of the line's width
		default:
			r, n := cluster(units, i)
			if i > start && breakBetween(prev, r) {
				brk = i
			}
			if i > start && widthOf(x, start, i+n) > maxW {
				if brk <= start {
					brk = i
				}
				lines = append(lines, textLine{start, brk})
				start = brk
				continue // cluster i again, on the new line
			}
			i += n
			prev = r
		}
	}
	return append(lines, textLine{start, len(units)})
}

// cluster is the character at units[i] with what stays with it: combining marks and variation
// selectors, skin tones, emoji tags, ZWJ joins and the second half of a flag. It returns the
// character's first rune and the cluster's length in units.
func cluster(units []uint16, i int) (rune, int) {
	first, n := runeAt(units, i)
	for i+n < len(units) {
		r, m := runeAt(units, i+n)
		switch {
		case unicode.In(r, unicode.Mn, unicode.Me), r >= 0x1f3fb && r <= 0x1f3ff, r >= 0xe0020 && r <= 0xe007f:
		case r == 0x200d: // zero width joiner: the next character joins too
			if j := i + n + m; j < len(units) && units[j] > ' ' {
				_, k := runeAt(units, j)
				m += k
			}
		case flag(first) && flag(r) && n == 2:
		default:
			return first, n
		}
		n += m
	}
	return first, n
}

// runeAt is the rune at units[i] and its length in units; a lone surrogate is its own rune.
func runeAt(units []uint16, i int) (rune, int) {
	if i+1 < len(units) {
		if r := utf16.DecodeRune(rune(units[i]), rune(units[i+1])); r != unicode.ReplacementChar {
			return r, 2
		}
	}
	return rune(units[i]), 1
}

// breakBetween reports whether DrawText may break a line between characters p and c besides at
// spaces: around CJK characters and emoji, but never before closing punctuation, between two flags,
// or before an emoji that follows a letter or opening punctuation.
func breakBetween(p, c rune) bool {
	switch {
	case unicode.In(c, unicode.Pe, unicode.Pf) || strings.ContainsRune(",.!?:;、。，．！？：；々", c) || flag(p) && flag(c):
		return false
	case cjk(p) || cjk(c) || emoji(p):
		return true
	case emoji(c):
		return !unicode.In(p, unicode.L, unicode.M, unicode.Ps, unicode.Pi)
	}
	return false
}

// cjk reports whether r is a CJK ideograph, kana or hangul syllable.
func cjk(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana) || r >= 0xac00 && r <= 0xd7a3
}

// emoji reports whether r is a pictograph DrawText breaks around: those of the supplementary planes
// but the enclosed letters. shortcut: BMP emoji DrawText also breaks around (☺ ✌ ☕ ⚽ ⌚) are not
// included, nor are its gaps (💯 🎵 🔗); add Windows' table if they matter.
func emoji(r rune) bool { return r >= 0x1f000 && r <= 0x1faff && (r < 0x1f100 || r > 0x1f1e5) }

// flag reports whether r is a regional indicator, half of a flag.
func flag(r rune) bool { return r >= 0x1f1e6 && r <= 0x1f1ff }

// textWidth is the width of the widest of lines as DrawText reports it: a line's trailing spaces
// count as far as they fit in maxW. x holds the cumulative advances of all of units, as wrapText's
// adv returns them.
func textWidth(units []uint16, x []int, lines []textLine, maxW int) int {
	widest := 0
	for _, ln := range lines {
		end := ln.end
		for end > ln.start && units[end-1] == ' ' && widthOf(x, ln.start, end) > maxW {
			end--
		}
		widest = max(widest, widthOf(x, ln.start, end))
	}
	return widest
}

// widthOf is the width of units [start, end) given x, their cumulative advances.
func widthOf(x []int, start, end int) int {
	switch {
	case end <= start:
		return 0
	case start == 0:
		return x[end-1]
	}
	return x[end-1] - x[start-1]
}

// textPos is a position in the chat's text: a message index and a UTF-16 offset into its text.
type textPos struct{ msg, off int }

// orderPos returns a and b in text order.
func orderPos(a, b textPos) (from, to textPos) {
	if b.msg < a.msg || b.msg == a.msg && b.off < a.off {
		return b, a
	}
	return a, b
}

// lineAdv is the x after each unit of a line of text in the body font, with the boundaries inside
// a character (a surrogate pair, an emoji sequence) moved to its start, so hits and highlight edges
// land on whole characters.
func lineAdv(ms measurer, units []uint16, style walk.FontStyle) []int {
	x := ms.advances(units, bodyPt, style)
	for i := 0; i < len(units); {
		_, n := cluster(units, i)
		for j := i; j < i+n-1; j++ {
			x[j] = widthOf(x, 0, i)
		}
		i += n
	}
	return x
}

// hitOffset is the boundary of a line nearest to x (the midpoint rule), given adv, the x after each
// unit as lineAdv returns it; a tie keeps the earlier boundary, so a hit never lands inside a
// character whose inner boundaries lineAdv moved to its start.
func hitOffset(adv []int, x int) int {
	best, bx := 0, 0
	for i, a := range adv {
		if abs(a-x) < abs(bx-x) {
			best, bx = i+1, a
		}
	}
	return best
}

func abs(n int) int { return max(n, -n) }

// wordAt is the word around offset off of units: a run of letters and digits of any script with
// their combining marks; any other character stands alone. An offset past the end means the last
// character.
func wordAt(units []uint16, off int) (start, end int) {
	off = min(off, len(units)-1)
	word := func(i int) (bool, int) {
		r, n := cluster(units, i)
		return unicode.IsLetter(r) || unicode.IsDigit(r), n
	}
	for i := 0; i < len(units); {
		w, n := word(i)
		j := i + n
		for w && j < len(units) {
			if w2, m := word(j); w2 {
				j += m
			} else {
				break
			}
		}
		if off < j {
			return i, j
		}
		i = j
	}
	return len(units), len(units)
}

// selSpan is the part [s, e) of message i's text, n units long, that lies between from and to.
func selSpan(from, to textPos, i, n int) (s, e int) {
	if i < from.msg || i > to.msg {
		return 0, 0
	}
	s, e = 0, n
	if i == from.msg {
		s = min(from.off, n)
	}
	if i == to.msg {
		e = min(to.off, n)
	}
	return s, max(s, e)
}

// selectedText is the text between from and to, texts holding each message's text by index: the
// selected part of each message, one per line, newlines as "\r\n" (the clipboard's, and what an
// edit control shows as a line break).
func selectedText(texts [][]uint16, from, to textPos) string {
	var parts []string
	for i := from.msg; i <= to.msg && i < len(texts); i++ {
		if s, e := selSpan(from, to, i, len(texts[i])); s < e {
			parts = append(parts, string(utf16.Decode(texts[i][s:e])))
		}
	}
	return strings.ReplaceAll(strings.ReplaceAll(strings.Join(parts, "\n"), "\r\n", "\n"), "\n", "\r\n")
}
