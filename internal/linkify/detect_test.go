package linkify

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDetectURLsAndPhones(t *testing.T) {
	input := "visit https://en.wikipedia.org/wiki/Go_(programming_language). and https://example.com/a_b?id=08123456789! or call +62 882-0007-83234 or 0812 1222 133 33"
	got := Detect(input)

	want := []Span{
		{
			Kind:  SpanURL,
			Start: 6,
			End:   61,
			Text:  "https://en.wikipedia.org/wiki/Go_(programming_language)",
		},
		{
			Kind:  SpanURL,
			Start: 67,
			End:   105,
			Text:  "https://example.com/a_b?id=08123456789",
		},
		{
			Kind:  SpanPhone,
			Start: 115,
			End:   133,
			Text:  "+62 882-0007-83234",
		},
		{
			Kind:  SpanPhone,
			Start: 137,
			End:   153,
			Text:  "0812 1222 133 33",
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Detect() =\n  %+v\nwant:\n  %+v", got, want)
	}
	for _, sp := range got {
		if input[sp.Start:sp.End] != sp.Text {
			t.Fatalf("span %+v: input[%d:%d]=%q != Text %q", sp, sp.Start, sp.End, input[sp.Start:sp.End], sp.Text)
		}
	}

	if first := FirstURL(input); first != "https://en.wikipedia.org/wiki/Go_(programming_language)" {
		t.Fatalf("FirstURL() = %q", first)
	}
	if first := FirstURL("call +62 812-3456-7890 only"); first != "" {
		t.Fatalf("FirstURL(no url) = %q, want empty", first)
	}
}

func TestDetectOrderingAndEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []Span
	}{
		{
			name: "phone before url with utf8 prefix",
			in:   "café +1 (555) 010-0999 then (http://example.com/foo_(1)).",
			want: []Span{
				{Kind: SpanPhone, Start: 6, End: 23, Text: "+1 (555) 010-0999"},
				{Kind: SpanURL, Start: 30, End: 56, Text: "http://example.com/foo_(1)"},
			},
		},
		{
			name: "balanced brackets and trailing punctuation",
			in:   "[https://example.com/a[b]{c}(d)]; next (0812-3456-7890)?",
			want: []Span{
				{Kind: SpanURL, Start: 1, End: 31, Text: "https://example.com/a[b]{c}(d)"},
				{Kind: SpanPhone, Start: 40, End: 54, Text: "0812-3456-7890"},
			},
		},
		{
			name: "url wrapped in quotes and angle brackets",
			in:   `"<https://example.com/path>"`,
			want: []Span{
				{Kind: SpanURL, Start: 2, End: 26, Text: "https://example.com/path"},
			},
		},
		{
			name: "single quote terminates url",
			in:   "'https://example.com/a'b",
			want: []Span{
				{Kind: SpanURL, Start: 1, End: 22, Text: "https://example.com/a"},
			},
		},
		{
			name: "trailing formatting markers stripped",
			in:   "*https://x.com* _https://y.com/p_ ~https://z.com~.",
			want: []Span{
				{Kind: SpanURL, Start: 1, End: 14, Text: "https://x.com"},
				{Kind: SpanURL, Start: 17, End: 32, Text: "https://y.com/p"},
				{Kind: SpanURL, Start: 35, End: 48, Text: "https://z.com"},
			},
		},
		{
			name: "trailing marks without an opening mark stay in the url",
			in:   "see https://e.com/_x_ and https://e.com/a*",
			want: []Span{
				{Kind: SpanURL, Start: 4, End: 21, Text: "https://e.com/_x_"},
				{Kind: SpanURL, Start: 26, End: 42, Text: "https://e.com/a*"},
			},
		},
		{
			name: "min length boundaries",
			in:   "+1234567 and 01234567 and +123456789012345",
			want: []Span{
				{Kind: SpanPhone, Start: 0, End: 8, Text: "+1234567"},
				{Kind: SpanPhone, Start: 13, End: 21, Text: "01234567"},
				{Kind: SpanPhone, Start: 26, End: 42, Text: "+123456789012345"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Detect(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Detect(%q) =\n  %+v\nwant:\n  %+v", tc.in, got, tc.want)
			}
			for _, sp := range got {
				if tc.in[sp.Start:sp.End] != sp.Text {
					t.Fatalf("span %+v: input[%d:%d]=%q != Text %q", sp, sp.Start, sp.End, tc.in[sp.Start:sp.End], sp.Text)
				}
			}
		})
	}
}

func TestDetectIgnoresNonPhones(t *testing.T) {
	// Review Focus: commas, decimals, words, multiline splits, non-http schemes
	for _, s := range []string{
		"testers 123,456",
		"v1.0.234567",
		"0.8123456",
		"abc081234567",
		"081234567abc",
		"0812\n3456789",
		"0812\r\n3456789",
		"file:///C:/08123456789",
		"ftp://example.com/08123456789",
		"javascript:alert(123456)",
		"+123456",
		"0123456",
		"+0123456789",
		"0123456789012345",
		"+1234567890123456",
		"+1 234-056789012345",
		"0812-012345678901",
		"1+628123456789",
		"foo_08123456789",
		"08123456.789",
		"08123456,789",
		"https://",
		"http://",
	} {
		if spans := Detect(s); len(spans) != 0 {
			t.Fatalf("Detect(%q) = %+v, want empty", s, spans)
		}
	}
}

func TestNormalizePhoneAndAccountCountryCode(t *testing.T) {
	ccCases := []struct {
		accountID string
		want      string
	}{
		{"628123456789", "62"},
		{"15550100999", "1"},
		{"79161234567", "7"},
		{"447911123456", "44"},
		{"420123456789", "420"},
		{"886912345678", "886"},
		{"998901234567", "998"},
		{"", ""},
		{"000123456", ""},
	}
	for _, tc := range ccCases {
		if cc := AccountCountryCode(tc.accountID); cc != tc.want {
			t.Fatalf("AccountCountryCode(%q) = %q, want %q", tc.accountID, cc, tc.want)
		}
	}

	e164, jid, ok := NormalizePhone("0812 1222 133 33", "628123456789")
	if !ok || e164 != "+62812122213333" || jid != "62812122213333@s.whatsapp.net" {
		t.Fatalf("NormalizePhone local = (%q, %q, %v)", e164, jid, ok)
	}

	e164, jid, ok = NormalizePhone("+1 (555) 010-0999", "628123456789")
	if !ok || e164 != "+15550100999" || jid != "15550100999@s.whatsapp.net" {
		t.Fatalf("NormalizePhone intl = (%q, %q, %v)", e164, jid, ok)
	}

	if _, _, ok := NormalizePhone("0123456", "628123456789"); ok {
		t.Fatal("NormalizePhone short local number unexpectedly succeeded")
	}
	if _, _, ok := NormalizePhone("0812345678", ""); ok {
		t.Fatal("NormalizePhone without account country code unexpectedly succeeded")
	}
	if _, _, ok := NormalizePhone("+012345678", "628123456789"); ok {
		t.Fatal("NormalizePhone +0... unexpectedly succeeded")
	}
	if _, _, ok := NormalizePhone("628123456789", "628123456789"); ok {
		t.Fatal("NormalizePhone bare number without + or 0 unexpectedly succeeded")
	}

	if disp := FormatPhoneDisplay("+6281234567890"); disp != "+62 812-3456-7890" {
		t.Fatalf("FormatPhoneDisplay(+6281234567890) = %q", disp)
	}
	if disp := FormatPhoneDisplay("+15550100999"); disp != "+1 555-010-0999" {
		t.Fatalf("FormatPhoneDisplay(+15550100999) = %q", disp)
	}
}

func TestDisplayDomain(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"https://www.example.com/foo/bar?x=1", "example.com"},
		{"https://en.wikipedia.org/wiki/Go", "en.wikipedia.org"},
		{"http://EXAMPLE.COM:8080/test", "example.com"},
		{"not a url", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := DisplayDomain(tc.raw); got != tc.want {
			t.Fatalf("DisplayDomain(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestSanitizePreview(t *testing.T) {
	title, desc := SanitizePreview("  Hello\n\t world\xff  ", strings.Repeat("é", 1000))
	if title != "Hello world\uFFFD" {
		t.Fatalf("title = %q", title)
	}
	if !utf8.ValidString(desc) || utf8.RuneCountInString(desc) != MaxPreviewDescRunes {
		t.Fatalf("desc runes = %d, valid = %v", utf8.RuneCountInString(desc), utf8.ValidString(desc))
	}
	long, _ := SanitizePreview(strings.Repeat("a ", 500), "")
	if n := utf8.RuneCountInString(long); n == 0 || n > MaxPreviewTitleRunes {
		t.Fatalf("title runes = %d, want 1..%d", n, MaxPreviewTitleRunes)
	}
	if strings.HasSuffix(long, " ") {
		t.Fatalf("clamped title has trailing space: %q", long)
	}
}
