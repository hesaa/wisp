package ui

import (
	"sort"
	"strings"
	"time"
	"unicode"

	"wa-go/internal/store"
)

const (
	avatarRefresh = 7 * 24 * time.Hour
	avatarRetry   = 10 * time.Minute
)

// Directory indexes people known to an account, merging LID and phone number rows.
// A nil *Directory is valid and knows nobody.
type Directory struct {
	byJID map[string]store.Person
}

// NewDirectory builds a Directory index from the account's people rows.
func NewDirectory(people []store.Person) *Directory {
	d := &Directory{
		byJID: make(map[string]store.Person),
	}
	if len(people) == 0 {
		return d
	}

	raw := make(map[string]store.Person, len(people))
	links := make(map[string]string)

	for _, p := range people {
		if p.JID == "" {
			continue
		}
		raw[p.JID] = p
		if p.PNJID != "" && p.PNJID != p.JID {
			links[p.JID] = p.PNJID
			links[p.PNJID] = p.JID
		}
	}

	// For every JID present in raw, merge with linked row if available.
	for jid, p := range raw {
		otherJID := links[jid]
		if otherJID != "" {
			if other, ok := raw[otherJID]; ok {
				d.byJID[jid] = mergePerson(p, other)
				continue
			}
		}
		d.byJID[jid] = mergePerson(p, store.Person{})
	}

	// For JIDs linked through PNJID that had no row of their own in raw.
	for jid, otherJID := range links {
		if _, ok := d.byJID[jid]; !ok {
			if other, ok := raw[otherJID]; ok {
				base := store.Person{JID: jid}
				if strings.HasSuffix(jid, "@s.whatsapp.net") {
					base.PNJID = jid
				}
				d.byJID[jid] = mergePerson(base, other)
			}
		}
	}

	return d
}

func pickPNJID(a, b string) string {
	if strings.HasSuffix(a, "@s.whatsapp.net") {
		return a
	}
	if strings.HasSuffix(b, "@s.whatsapp.net") {
		return b
	}
	return ""
}

// mergePerson merges two rows for the same person: a non-empty field of either fills
// the result; FullName of either wins.
func mergePerson(base, other store.Person) store.Person {
	res := base
	if res.JID == "" {
		res.JID = other.JID
	}
	if strings.HasSuffix(res.JID, "@s.whatsapp.net") {
		res.PNJID = res.JID
	} else {
		res.PNJID = pickPNJID(res.PNJID, other.PNJID)
		if res.PNJID == "" && strings.HasSuffix(other.JID, "@s.whatsapp.net") {
			res.PNJID = other.JID
		}
	}
	if res.FullName == "" {
		res.FullName = other.FullName
	}
	if res.PushName == "" {
		res.PushName = other.PushName
	}
	if res.AvatarID == "" {
		res.AvatarID = other.AvatarID
	}
	if res.AvatarTS == 0 || (other.AvatarTS > res.AvatarTS) {
		res.AvatarTS = other.AvatarTS
	}
	return res
}

func cleanJID(jid string) string {
	user, server, ok := strings.Cut(jid, "@")
	if !ok {
		return jid
	}
	user, _, _ = strings.Cut(user, ":")
	return user + "@" + server
}

func phoneDigitsFromJID(jid string) string {
	user, _, _ := strings.Cut(jid, "@")
	user, _, _ = strings.Cut(user, ":")
	var b strings.Builder
	for _, r := range user {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Person returns the row for jid merged with the row linked through PNJID in either direction.
func (d *Directory) Person(jid string) (store.Person, bool) {
	if d == nil || d.byJID == nil {
		return store.Person{}, false
	}
	p, ok := d.byJID[jid]
	if !ok {
		if clean := cleanJID(jid); clean != jid {
			p, ok = d.byJID[clean]
		}
	}
	return p, ok
}

// maskedName reports whether s is a masked number (+, then only digits, spaces and at least one
// of •, ·, *, ., ∙).
func maskedName(s string) bool {
	if !strings.HasPrefix(s, "+") {
		return false
	}
	hasMask := false
	for _, r := range s[1:] {
		switch r {
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9', ' ':
			// valid digit or space
		case '•', '·', '*', '.', '∙':
			hasMask = true
		default:
			return false
		}
	}
	return hasMask
}

// Name resolves the display name for jid according to the spec's name rule.
// Group (@g.us): stored, else Group.
// Otherwise: FullName; else ~+PushName; else stored when not empty and not masked;
// else +<digits> of the phone-number JID (jid itself when @s.whatsapp.net, else PNJID);
// else Unknown.
func (d *Directory) Name(jid, stored string) string {
	if strings.HasSuffix(jid, "@g.us") {
		if stored != "" {
			return stored
		}
		return "Group"
	}

	p, ok := d.Person(jid)
	if ok && p.FullName != "" {
		return p.FullName
	}
	if ok && p.PushName != "" {
		return "~" + p.PushName
	}
	if stored != "" && !maskedName(stored) {
		return stored
	}

	var pn string
	if strings.HasSuffix(jid, "@s.whatsapp.net") {
		pn = jid
	} else if ok && strings.HasSuffix(p.PNJID, "@s.whatsapp.net") {
		pn = p.PNJID
	}
	if pn != "" {
		digits := phoneDigitsFromJID(pn)
		if digits != "" {
			return "+" + digits
		}
	}

	return "Unknown"
}

// Mention returns Name(jid, "") without the leading ~.
func (d *Directory) Mention(jid string) string {
	return strings.TrimPrefix(d.Name(jid, ""), "~")
}

func utf16RuneLen(r rune) int {
	if r > 0xFFFF {
		return 2
	}
	return 1
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16RuneLen(r)
	}
	return n
}

type mentionCandidate struct {
	userRunes []rune
	jid       string
}

// rewriteMentions rewrites @<user part> in body to @<Mention>.
// For each JID in mentions, every @<user part> in body becomes @<Mention>.
// With mentions == "", an @ followed by 5–20 digits (not preceded by a letter or digit,
// not followed by one) is rewritten only when d.Person knows <digits>@lid or <digits>@s.whatsapp.net.
// spans are the UTF-16 start/end offsets of each rewritten mention in text, in order.
func rewriteMentions(body, mentions string, d *Directory) (string, [][2]int) {
	if body == "" {
		return "", nil
	}
	runes := []rune(body)
	if len(runes) == 0 {
		return "", nil
	}

	var spans [][2]int
	var sb strings.Builder
	u16Offset := 0

	trimmedMentions := strings.TrimSpace(mentions)
	if trimmedMentions != "" {
		fields := strings.Fields(trimmedMentions)
		var cands []mentionCandidate
		for _, jid := range fields {
			user, _, _ := strings.Cut(jid, "@")
			user, _, _ = strings.Cut(user, ":")
			if user != "" {
				cands = append(cands, mentionCandidate{
					userRunes: []rune(user),
					jid:       jid,
				})
			}
		}
		// Sort candidates by length descending so longer matches win
		sort.SliceStable(cands, func(i, j int) bool {
			return len(cands[i].userRunes) > len(cands[j].userRunes)
		})

		for i := 0; i < len(runes); {
			if runes[i] != '@' {
				sb.WriteRune(runes[i])
				u16Offset += utf16RuneLen(runes[i])
				i++
				continue
			}

			isPreceded := (i > 0 && (unicode.IsLetter(runes[i-1]) || unicode.IsDigit(runes[i-1])))
			if isPreceded {
				sb.WriteRune('@')
				u16Offset++
				i++
				continue
			}

			matched := false
			for _, cand := range cands {
				candLen := len(cand.userRunes)
				if i+1+candLen <= len(runes) && matchRunes(runes[i+1:i+1+candLen], cand.userRunes) {
					nextIdx := i + 1 + candLen
					isFollowed := (nextIdx < len(runes) && (unicode.IsLetter(runes[nextIdx]) || unicode.IsDigit(runes[nextIdx])))
					if !isFollowed {
						rep := "@" + d.Mention(cand.jid)
						repU16 := utf16Len(rep)
						spans = append(spans, [2]int{u16Offset, u16Offset + repU16})
						sb.WriteString(rep)
						u16Offset += repU16
						i = nextIdx
						matched = true
						break
					}
				}
			}
			if !matched {
				sb.WriteRune('@')
				u16Offset++
				i++
			}
		}
		return sb.String(), spans
	}

	// mentions == ""
	for i := 0; i < len(runes); {
		if runes[i] != '@' {
			sb.WriteRune(runes[i])
			u16Offset += utf16RuneLen(runes[i])
			i++
			continue
		}

		isPreceded := (i > 0 && (unicode.IsLetter(runes[i-1]) || unicode.IsDigit(runes[i-1])))
		if isPreceded {
			sb.WriteRune('@')
			u16Offset++
			i++
			continue
		}

		j := i + 1
		for j < len(runes) && runes[j] >= '0' && runes[j] <= '9' {
			j++
		}
		digitCount := j - (i + 1)
		if digitCount >= 5 && digitCount <= 20 {
			isFollowed := (j < len(runes) && (unicode.IsLetter(runes[j]) || unicode.IsDigit(runes[j])))
			if !isFollowed {
				digits := string(runes[i+1 : j])
				var matchedJID string
				if d != nil {
					if _, ok := d.Person(digits + "@lid"); ok {
						matchedJID = digits + "@lid"
					} else if _, ok := d.Person(digits + "@s.whatsapp.net"); ok {
						matchedJID = digits + "@s.whatsapp.net"
					}
				}
				if matchedJID != "" {
					rep := "@" + d.Mention(matchedJID)
					repU16 := utf16Len(rep)
					spans = append(spans, [2]int{u16Offset, u16Offset + repU16})
					sb.WriteString(rep)
					u16Offset += repU16
					i = j
					continue
				}
			}
		}

		sb.WriteRune('@')
		u16Offset++
		i++
	}

	return sb.String(), spans
}

func matchRunes(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// snippetLine produces the one-line summary for a chat in the sidebar.
// spec §6: text or caption, else the label (Photo, Video, GIF, Sticker, Voice message,
// Audio, Document: <name>, This message was deleted, Unsupported message);
// prefix You: when LastFromMe; in a group, <Name(LastSender, "")>: otherwise;
// a chat with no message gives "".
func snippetLine(c store.Chat, d *Directory) string {
	if c.LastType == "" && c.Snippet == "" {
		return ""
	}

	var content string
	switch c.LastType {
	case "revoked":
		content = "This message was deleted"
	case "unsupported":
		content = "Unsupported message"
	default:
		msg := store.Message{Type: c.LastType, Body: c.Snippet, MediaMeta: c.LastMeta}
		info, _ := parseMedia(&msg)
		text, muted := bubbleText(&msg, info)
		if muted {
			content = text
		} else if text != "" {
			content = text
		} else {
			lbl := mediaLabel(msg, info)
			content = strings.TrimSuffix(strings.TrimPrefix(lbl, "["), "]")
			if content == "" {
				if c.LastType == "text" {
					content = c.Snippet
				} else {
					content = "Unsupported message"
				}
			}
		}
	}

	var prefix string
	if c.LastFromMe {
		prefix = "You: "
	} else if strings.HasSuffix(c.JID, "@g.us") {
		prefix = d.Name(c.LastSender, "") + ": "
	}

	return prefix + content
}

// avatarDue reports whether person p's profile picture is due to be checked.
// Due when never checked (AvatarTS == 0), or now − AvatarTS ≥ avatarRefresh.
func avatarDue(p store.Person, now time.Time) bool {
	if p.AvatarTS == 0 {
		return true
	}
	return now.Sub(time.Unix(p.AvatarTS, 0)) >= avatarRefresh
}

// avatarFailedTS returns the AvatarTS to store after a failed fetch so it is
// due again after avatarRetry.
func avatarFailedTS(now time.Time) int64 {
	return now.Add(avatarRetry - avatarRefresh).Unix()
}
