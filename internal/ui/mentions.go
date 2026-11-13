package ui

import (
	"cmp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// The composer's group mentions: which @token is being typed, who it may mean, and what the sent
// text carries. Pure, so it is tested without a window; nothing here reads the disk or the network.

// GroupMember is a participant of the open group the composer can mention.
type GroupMember struct {
	JID   string // as the group addresses them: what the mention carries
	Name  string // the mention's text after the @: full name, else push name, else the number
	Push  string // push name, "" when unknown
	Phone string // phone number digits, "" when unknown
}

// mentionAll is the list's first row: @all notifies every participant (whatsapp.Manager.SendText).
var mentionAll = GroupMember{Name: "all"}

// mentionQuery is the text typed after the @ of the mention the caret is in, given the box's text
// before the caret. ok is false when no mention is being typed: the @ must start a word.
func mentionQuery(before string) (query string, ok bool) {
	i := strings.LastIndex(before, "@")
	if i < 0 {
		return "", false
	}
	if r, _ := utf8.DecodeLastRuneInString(before[:i]); i > 0 && !unicode.IsSpace(r) {
		return "", false
	}
	query = before[i+1:]
	return query, !strings.ContainsFunc(query, unicode.IsSpace)
}

// filterMentions lists @all, then the members whose name, push name or number contains query.
func filterMentions(members []GroupMember, query string) []GroupMember {
	q := strings.ToLower(query)
	digits := strings.TrimPrefix(q, "+")
	out := []GroupMember{mentionAll}
	for _, m := range members {
		if strings.Contains(strings.ToLower(m.Name), q) || strings.Contains(strings.ToLower(m.Push), q) ||
			digits != "" && strings.Contains(m.Phone, digits) {
			out = append(out, m)
		}
	}
	return out
}

// mentionPicker is the mention list's state.
type mentionPicker struct {
	members []GroupMember // the open group's, sorted by name; none outside groups
	items   []GroupMember // the rows listed now; none: the list is closed
	sel     int           // the highlighted row
}

// refresh lists the matches of the mention being typed before the caret, or closes the list.
func (p *mentionPicker) refresh(before string) {
	p.close()
	q, ok := mentionQuery(before)
	if !ok || len(p.members) == 0 {
		return
	}
	p.items = filterMentions(p.members, q)
	// a typed name selects its first match, so Enter completes it; @all stays one Up away
	if q != "" && len(p.items) > 1 && !strings.HasPrefix("all", strings.ToLower(q)) {
		p.sel = 1
	}
}

func (p *mentionPicker) close() { p.items, p.sel = nil, 0 }

// move moves the highlight d rows, stopping at the ends.
func (p *mentionPicker) move(d int) { p.sel = max(0, min(len(p.items)-1, p.sel+d)) }

// complete replaces the mention being typed (the end of before) with the highlighted row's and
// closes the list. It returns the box's new text and the caret after the mention, in UTF-16 units.
func (p *mentionPicker) complete(before, after string) (text string, caret int) {
	if len(p.items) == 0 {
		return before + after, len(utf16.Encode([]rune(before)))
	}
	before = before[:strings.LastIndex(before, "@")+1] + p.items[p.sel].Name + " "
	p.close()
	return before + after, len(utf16.Encode([]rune(before)))
}

// wireMentions turns the @Name tokens of text into WhatsApp's @<number> and returns it with the JIDs
// mentioned, each once. @all stays as it is.
// shortcut: of two members with the same name the first is mentioned; carry the picked JIDs with
// the box's text if that shows.
func wireMentions(text string, members []GroupMember) (string, []string) {
	byLen := slices.Clone(members) // longest first: "@Ann Lee" is not "@Ann"
	slices.SortStableFunc(byLen, func(a, b GroupMember) int { return cmp.Compare(len(b.Name), len(a.Name)) })
	var jids []string
	for _, m := range byLen {
		user, _, _ := strings.Cut(m.JID, "@")
		if m.Name == "" || user == "" {
			continue
		}
		token := "@" + m.Name
		for from := 0; ; {
			i := strings.Index(text[from:], token)
			if i < 0 {
				break
			}
			i += from
			end := i + len(token)
			prev, _ := utf8.DecodeLastRuneInString(text[:i])
			next, _ := utf8.DecodeRuneInString(text[end:])
			if (i > 0 && !unicode.IsSpace(prev)) || (end < len(text) && (unicode.IsLetter(next) || unicode.IsDigit(next))) {
				from = end
				continue
			}
			text = text[:i] + "@" + user + text[end:]
			from = i + 1 + len(user)
			if !slices.Contains(jids, m.JID) {
				jids = append(jids, m.JID)
			}
		}
	}
	return text, jids
}
