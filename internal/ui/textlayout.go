package ui

import (
	"unicode"
	"unicode/utf16"
)

// textLine is one line of laid-out text: UTF-16 offsets [start, end) into the text, without the
// newline that ended it. Trailing spaces belong to the line.
type textLine struct{ start, end int }

// wrapText breaks units into lines at most maxW pixels wide, as DrawText's word break does: at
// newlines ("\r\n" counts as one), after the last space that fits, and inside a word wider than the
// line, never between the halves of a surrogate pair; every line holds at least one character.
// Trailing spaces never push a line past maxW. adv is called once, with all of units, and returns
// the cumulative width after each unit.
func wrapText(units []uint16, maxW int, adv func(u []uint16) []int) []textLine {
	if len(units) == 0 {
		return nil
	}
	x := adv(units)
	var lines []textLine
	start, brk := 0, 0 // brk: just after the line's last space; at or before start when it has none
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
			brk = i
		case u == '\r' && i+1 < len(units) && units[i+1] == '\n':
			i++ // part of the newline, not of the line's width
		default:
			n := 1
			if i+1 < len(units) && utf16.DecodeRune(rune(u), rune(units[i+1])) != unicode.ReplacementChar {
				n = 2
			}
			if i > start && widthOf(x, start, i+n) > maxW {
				if brk <= start {
					brk = i
				}
				lines = append(lines, textLine{start, brk})
				start = brk
				continue // unit i again, on the new line
			}
			i += n
		}
	}
	return append(lines, textLine{start, len(units)})
}

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
