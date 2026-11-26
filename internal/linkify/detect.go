package linkify

import (
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SpanKind identifies the kind of detected inline span.
type SpanKind int

const (
	SpanURL SpanKind = iota
	SpanPhone
)

// Span is a detected URL or phone number with byte offsets into the source string.
type Span struct {
	Kind  SpanKind
	Start int
	End   int
	Text  string
}

// Detect finds non-overlapping http/https URLs and international/local phone numbers
// in s, ordered by Start byte offset.
func Detect(s string) []Span {
	if s == "" {
		return nil
	}

	urls := detectURLs(s)
	phones := detectPhones(s, urls)

	if len(urls) == 0 && len(phones) == 0 {
		return nil
	}
	out := make([]Span, 0, len(urls)+len(phones))
	out = append(out, urls...)
	out = append(out, phones...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Start < out[j].Start
	})
	return out
}

// FirstURL returns the first http/https URL detected in s, or "".
func FirstURL(s string) string {
	for _, sp := range Detect(s) {
		if sp.Kind == SpanURL {
			return sp.Text
		}
	}
	return ""
}

func detectURLs(s string) []Span {
	var spans []Span
	for i := 0; i < len(s); {
		prefixLen := httpPrefixLen(s[i:])
		if prefixLen == 0 {
			i++
			continue
		}
		if i > 0 {
			prev, _ := utf8.DecodeLastRuneInString(s[:i])
			if isAlphaNum(prev) {
				i += prefixLen
				continue
			}
		}

		end := i + prefixLen
		for end < len(s) {
			b := s[end]
			if b <= 0x20 || b == 0x7f || b == '<' || b == '>' || b == '"' || b == '`' {
				break
			}
			if b >= utf8.RuneSelf {
				r, size := utf8.DecodeRuneInString(s[end:])
				if unicode.IsSpace(r) {
					break
				}
				end += size
				continue
			}
			end++
		}

		end = trimURLTrailing(s, i, end)
		if end <= i+prefixLen || isHostEmpty(s[i+prefixLen:end]) {
			i += prefixLen
			continue
		}

		spans = append(spans, Span{
			Kind:  SpanURL,
			Start: i,
			End:   end,
			Text:  s[i:end],
		})
		i = end
	}
	return spans
}

func httpPrefixLen(s string) int {
	if len(s) >= 8 && strings.EqualFold(s[:8], "https://") {
		return 8
	}
	if len(s) >= 7 && strings.EqualFold(s[:7], "http://") {
		return 7
	}
	return 0
}

func isHostEmpty(rest string) bool {
	return rest == "" || rest[0] == '/' || rest[0] == '?' || rest[0] == '#'
}

func trimURLTrailing(s string, start, end int) int {
	for end > start {
		last := s[end-1]
		switch last {
		case '.', ',', ';', ':', '!', '?':
			end--
		case ')', ']', '}':
			open, closeCh := matchingBracket(last)
			if countUnmatchedClose(s[start:end], open, closeCh) > 0 {
				end--
			} else {
				return end
			}
		default:
			return end
		}
	}
	return end
}

func matchingBracket(closeCh byte) (byte, byte) {
	switch closeCh {
	case ')':
		return '(', ')'
	case ']':
		return '[', ']'
	default:
		return '{', '}'
	}
}

func countUnmatchedClose(sub string, openCh, closeCh byte) int {
	balance := 0
	for i := 0; i < len(sub); i++ {
		switch sub[i] {
		case openCh:
			balance++
		case closeCh:
			balance--
		}
	}
	if balance < 0 {
		return -balance
	}
	return 0
}

func detectPhones(s string, urlSpans []Span) []Span {
	var spans []Span
	uIdx := 0
	for i := 0; i < len(s); {
		for uIdx < len(urlSpans) && i >= urlSpans[uIdx].End {
			uIdx++
		}
		if uIdx < len(urlSpans) && i >= urlSpans[uIdx].Start {
			i = urlSpans[uIdx].End
			continue
		}

		b := s[i]
		if b != '+' && b != '0' {
			i++
			continue
		}
		if i > 0 {
			prev, _ := utf8.DecodeLastRuneInString(s[:i])
			if !isCleanPhonePrev(prev) {
				i++
				continue
			}
		}

		urlLimit := len(s)
		if uIdx < len(urlSpans) {
			urlLimit = urlSpans[uIdx].Start
		}

		end, skipTo, ok := scanPhoneCandidate(s, i, urlLimit)
		if !ok {
			if skipTo > i {
				i = skipTo
			} else {
				i++
			}
			continue
		}

		if end < len(s) {
			next, _ := utf8.DecodeRuneInString(s[end:])
			if !isCleanPhoneNext(next) {
				i++
				continue
			}
		}

		spans = append(spans, Span{
			Kind:  SpanPhone,
			Start: i,
			End:   end,
			Text:  s[i:end],
		})
		i = end
	}
	return spans
}

func scanPhoneCandidate(s string, start, limit int) (end, skipTo int, ok bool) {
	intl := s[start] == '+'
	j := start
	digits := 0
	lastDigitEnd := -1

	if intl {
		j++
		if j >= limit || s[j] < '1' || s[j] > '9' {
			return 0, 0, false
		}
	}

	prevSep := false
	for j < limit {
		c := s[j]
		if c >= '0' && c <= '9' {
			digits++
			j++
			lastDigitEnd = j
			prevSep = false
			continue
		}
		if c == '\n' || c == '\r' {
			break
		}
		if intl {
			if c == ' ' || c == '-' || c == '(' || c == ')' {
				j++
				continue
			}
		} else if c == ' ' || c == '-' {
			if prevSep {
				break
			}
			prevSep = true
			j++
			continue
		}
		break
	}

	if lastDigitEnd < 0 {
		return 0, 0, false
	}
	if digits > 15 {
		return 0, lastDigitEnd, false
	}
	minDigits := 8
	if intl {
		minDigits = 7
	}
	if digits < minDigits {
		return 0, 0, false
	}
	return lastDigitEnd, 0, true
}

func isAlphaNum(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func isCleanPhonePrev(r rune) bool {
	if isAlphaNum(r) {
		return false
	}
	switch r {
	case '.', ',', '+', '/', '_':
		return false
	}
	return true
}

func isCleanPhoneNext(r rune) bool {
	if isAlphaNum(r) {
		return false
	}
	switch r {
	case '.', ',':
		return false
	}
	return true
}

// AccountCountryCode extracts the leading ITU-T E.164 country calling code from accountID,
// or "" if no valid code matches.
func AccountCountryCode(accountID string) string {
	digits := leadingDigits(accountID)
	if digits == "" {
		return ""
	}
	if digits[0] == '1' || digits[0] == '7' {
		return digits[:1]
	}
	if len(digits) >= 2 {
		if twoDigitCallingCodes[digits[:2]] {
			return digits[:2]
		}
	}
	if len(digits) >= 3 {
		if threeDigitCallingCodes[digits[:3]] {
			return digits[:3]
		}
	}
	return ""
}

func leadingDigits(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "+")
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

// NormalizePhone normalizes an international (+...) or local (0...) phone number using
// accountID's E.164 country calling code, returning "+<digits>", "<digits>@s.whatsapp.net", true.
func NormalizePhone(raw, accountID string) (e164, jid string, ok bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", false
	}

	switch trimmed[0] {
	case '+':
		d, valid := stripPhoneSeparators(trimmed[1:])
		if !valid || len(d) < 7 || len(d) > 15 || d[0] < '1' || d[0] > '9' {
			return "", "", false
		}
		return "+" + d, d + "@s.whatsapp.net", true

	case '0':
		d, valid := stripPhoneSeparators(trimmed)
		if !valid || len(d) < 8 || len(d) > 15 {
			return "", "", false
		}
		cc := AccountCountryCode(accountID)
		if cc == "" {
			return "", "", false
		}
		full := cc + d[1:]
		if len(full) > 15 {
			return "", "", false
		}
		return "+" + full, full + "@s.whatsapp.net", true

	default:
		return "", "", false
	}
}

func stripPhoneSeparators(s string) (string, bool) {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == ' ' || c == '-' || c == '(' || c == ')':
		default:
			return "", false
		}
	}
	return b.String(), true
}

// FormatPhoneDisplay formats an E.164 phone number for readable UI display (e.g. "+62 812-3456-7890").
func FormatPhoneDisplay(e164 string) string {
	digits := leadingDigits(e164)
	if digits == "" {
		return e164
	}
	cc := AccountCountryCode(digits)
	if cc == "" {
		return "+" + digits
	}
	rest := digits[len(cc):]
	switch {
	case len(rest) == 0:
		return "+" + cc
	case len(rest) <= 4:
		return "+" + cc + " " + rest
	case len(rest) <= 7:
		return "+" + cc + " " + rest[:3] + "-" + rest[3:]
	case len(rest) <= 10:
		return "+" + cc + " " + rest[:3] + "-" + rest[3:6] + "-" + rest[6:]
	default:
		return "+" + cc + " " + rest[:3] + "-" + rest[3:7] + "-" + rest[7:]
	}
}

// DisplayDomain extracts a lowercase host without a leading "www." prefix from rawURL.
func DisplayDomain(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return ""
	}
	return strings.TrimPrefix(host, "www.")
}

var twoDigitCallingCodes = map[string]bool{
	"20": true, "27": true,
	"30": true, "31": true, "32": true, "33": true, "34": true, "36": true, "39": true,
	"40": true, "41": true, "43": true, "44": true, "45": true, "46": true, "47": true, "48": true, "49": true,
	"51": true, "52": true, "53": true, "54": true, "55": true, "56": true, "57": true, "58": true,
	"60": true, "61": true, "62": true, "63": true, "64": true, "65": true, "66": true,
	"81": true, "82": true, "84": true, "86": true,
	"90": true, "91": true, "92": true, "93": true, "94": true, "95": true, "98": true,
}

var threeDigitCallingCodes = map[string]bool{
	"211": true, "212": true, "213": true, "216": true, "218": true,
	"220": true, "221": true, "222": true, "223": true, "224": true, "225": true, "226": true, "227": true, "228": true, "229": true,
	"230": true, "231": true, "232": true, "233": true, "234": true, "235": true, "236": true, "237": true, "238": true, "239": true,
	"240": true, "241": true, "242": true, "243": true, "244": true, "245": true, "246": true, "247": true, "248": true, "249": true,
	"250": true, "251": true, "252": true, "253": true, "254": true, "255": true, "256": true, "257": true, "258": true,
	"260": true, "261": true, "262": true, "263": true, "264": true, "265": true, "266": true, "267": true, "268": true, "269": true,
	"290": true, "291": true, "297": true, "298": true, "299": true,
	"350": true, "351": true, "352": true, "353": true, "354": true, "355": true, "356": true, "357": true, "358": true, "359": true,
	"370": true, "371": true, "372": true, "373": true, "374": true, "375": true, "376": true, "377": true, "378": true, "379": true,
	"380": true, "381": true, "382": true, "383": true, "385": true, "386": true, "387": true, "389": true,
	"420": true, "421": true, "423": true,
	"500": true, "501": true, "502": true, "503": true, "504": true, "505": true, "506": true, "507": true, "508": true, "509": true,
	"590": true, "591": true, "592": true, "593": true, "594": true, "595": true, "596": true, "597": true, "598": true, "599": true,
	"670": true, "672": true, "673": true, "674": true, "675": true, "676": true, "677": true, "678": true, "679": true,
	"680": true, "681": true, "682": true, "683": true, "685": true, "686": true, "687": true, "688": true, "689": true,
	"690": true, "691": true, "692": true,
	"800": true, "808": true,
	"850": true, "852": true, "853": true, "855": true, "856": true,
	"870": true, "878": true,
	"880": true, "881": true, "882": true, "883": true, "886": true,
	"960": true, "961": true, "962": true, "963": true, "964": true, "965": true, "966": true, "967": true, "968": true,
	"970": true, "971": true, "972": true, "973": true, "974": true, "975": true, "976": true, "977": true, "979": true,
	"991": true, "992": true, "993": true, "994": true, "995": true, "996": true, "998": true,
}
