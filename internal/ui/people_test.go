package ui

import (
	"slices"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"wa-go/internal/store"
)

func TestNameRule(t *testing.T) {
	// FullName wins; "~Sinta" for push name only; stored "Budi" used; "+62812…" for a bare PN jid; LID with PNJID → "+<pn digits>"; LID alone → "Unknown"; group → stored, else "Group"
	d := NewDirectory([]store.Person{
		{JID: "111@s.whatsapp.net", FullName: "Sinta Full", PushName: "Sinta Push"},
		{JID: "222@s.whatsapp.net", PushName: "Sinta"},
		{JID: "555@lid", PNJID: "6281234567890@s.whatsapp.net"},
		{JID: "666@lid"},
	})

	// 1. FullName wins over push name and stored name
	if got := d.Name("111@s.whatsapp.net", "Budi"); got != "Sinta Full" {
		t.Fatalf("FullName wins: got %q, want 'Sinta Full'", got)
	}

	// 2. "~Sinta" for push name only (wins over stored name)
	if got := d.Name("222@s.whatsapp.net", "Budi"); got != "~Sinta" {
		t.Fatalf("PushName only: got %q, want '~Sinta'", got)
	}

	// 3. Stored "Budi" used when neither FullName nor PushName is known
	if got := d.Name("333@lid", "Budi"); got != "Budi" {
		t.Fatalf("stored name: got %q, want 'Budi'", got)
	}

	// 4. "+62812…" for a bare PN jid
	if got := d.Name("6281234567890@s.whatsapp.net", ""); got != "+6281234567890" {
		t.Fatalf("bare PN: got %q, want '+6281234567890'", got)
	}

	// 5. LID with PNJID → "+<pn digits>"
	if got := d.Name("555@lid", ""); got != "+6281234567890" {
		t.Fatalf("LID with PNJID: got %q, want '+6281234567890'", got)
	}

	// 6. LID alone → "Unknown"
	if got := d.Name("666@lid", ""); got != "Unknown" {
		t.Fatalf("LID alone in directory: got %q, want 'Unknown'", got)
	}
	if got := d.Name("999@lid", ""); got != "Unknown" {
		t.Fatalf("LID alone not in directory: got %q, want 'Unknown'", got)
	}

	// 6b. LID with non-phone PNJID (e.g. another LID) ignored, never formatted as +<digits> → "Unknown"
	dLIDPN := NewDirectory([]store.Person{
		{JID: "777@lid", PNJID: "888@lid"},
	})
	if got := dLIDPN.Name("777@lid", ""); got != "Unknown" {
		t.Fatalf("LID with non-phone PNJID: got %q, want 'Unknown'", got)
	}
	if p, ok := dLIDPN.Person("777@lid"); !ok || p.PNJID != "" {
		t.Fatalf("Person with non-phone PNJID: ok=%v, PNJID=%q, want ''", ok, p.PNJID)
	}

	// 7. Group → stored, else "Group"
	if got := d.Name("12345-67890@g.us", "My Group"); got != "My Group" {
		t.Fatalf("group with stored: got %q, want 'My Group'", got)
	}
	if got := d.Name("12345-67890@g.us", ""); got != "Group" {
		t.Fatalf("group without stored: got %q, want 'Group'", got)
	}

	// Nil directory knows nobody
	var nilDir *Directory
	if got := nilDir.Name("6281234567890@s.whatsapp.net", ""); got != "+6281234567890" {
		t.Fatalf("nil dir bare PN: got %q, want '+6281234567890'", got)
	}
	if got := nilDir.Name("999@lid", ""); got != "Unknown" {
		t.Fatalf("nil dir LID: got %q, want 'Unknown'", got)
	}
	if got := nilDir.Name("12345@g.us", ""); got != "Group" {
		t.Fatalf("nil dir group: got %q, want 'Group'", got)
	}
}

func TestNameMergesLIDAndPhone(t *testing.T) {
	// Review Focus 2: rows {A@lid, PNJID P} and {P, FullName "Sinta"} → Name(A@lid) == Name(P) == "Sinta"; Person(A@lid) has FullName "Sinta"
	const (
		lid = "alice@lid"
		pn  = "6281111111111@s.whatsapp.net"
	)
	p1 := store.Person{JID: lid, PNJID: pn, PushName: "Ali"}
	p2 := store.Person{JID: pn, FullName: "Sinta", AvatarID: "av99", AvatarTS: 12345}

	d := NewDirectory([]store.Person{p1, p2})

	if gotA, gotP := d.Name(lid, ""), d.Name(pn, ""); gotA != "Sinta" || gotP != "Sinta" {
		t.Fatalf("Name(A@lid) = %q, Name(P) = %q, want both 'Sinta'", gotA, gotP)
	}

	personA, okA := d.Person(lid)
	if !okA {
		t.Fatalf("Person(%q) not found", lid)
	}
	if personA.FullName != "Sinta" {
		t.Fatalf("Person(%q).FullName = %q, want 'Sinta'", lid, personA.FullName)
	}
	if personA.PushName != "Ali" {
		t.Fatalf("Person(%q).PushName = %q, want 'Ali'", lid, personA.PushName)
	}
	if personA.AvatarID != "av99" || personA.AvatarTS != 12345 {
		t.Fatalf("Person(%q) avatar = %s/%d, want av99/12345", lid, personA.AvatarID, personA.AvatarTS)
	}

	personP, okP := d.Person(pn)
	if !okP {
		t.Fatalf("Person(%q) not found", pn)
	}
	if personP.FullName != "Sinta" {
		t.Fatalf("Person(%q).FullName = %q, want 'Sinta'", pn, personP.FullName)
	}
	if personP.PushName != "Ali" {
		t.Fatalf("Person(%q).PushName = %q, want 'Ali'", pn, personP.PushName)
	}
	if personP.AvatarID != "av99" || personP.AvatarTS != 12345 {
		t.Fatalf("Person(%q) avatar = %s/%d, want av99/12345", pn, personP.AvatarID, personP.AvatarTS)
	}
}

func TestNonPhonePNJIDIgnored(t *testing.T) {
	// PNJID must strictly be a phone-number JID ending in @s.whatsapp.net.
	// Non-phone JIDs (like @lid or @g.us) are ignored and never formatted as +<digits>.
	d := NewDirectory([]store.Person{
		{JID: "12345678901234@lid", PNJID: "98765432109876@lid"},
		{JID: "22345678901234@lid", PNJID: "group123@g.us"},
		{JID: "32345678901234@lid", PNJID: "invalid-jid"},
	})

	for _, jid := range []string{"12345678901234@lid", "22345678901234@lid", "32345678901234@lid"} {
		if name := d.Name(jid, ""); name != "Unknown" {
			t.Errorf("Name(%q) = %q, want 'Unknown'", jid, name)
		}
		if mention := d.Mention(jid); mention != "Unknown" {
			t.Errorf("Mention(%q) = %q, want 'Unknown'", jid, mention)
		}
		p, ok := d.Person(jid)
		if !ok {
			t.Errorf("Person(%q) not found", jid)
		}
		if p.PNJID != "" {
			t.Errorf("Person(%q).PNJID = %q, want ''", jid, p.PNJID)
		}
	}
}

func TestMaskedNameSkipped(t *testing.T) {
	// maskedName("+62••••••32") true, ("+62 812 3456") false, ("Sinta •") false; Name(lid, "+62••••••32") == "Unknown"
	if !maskedName("+62••••••32") {
		t.Fatalf("maskedName('+62••••••32') want true")
	}
	if maskedName("+62 812 3456") {
		t.Fatalf("maskedName('+62 812 3456') want false")
	}
	if maskedName("Sinta •") {
		t.Fatalf("maskedName('Sinta •') want false")
	}

	// Other mask characters
	if !maskedName("+62···32") {
		t.Fatalf("maskedName with middle dot want true")
	}
	if !maskedName("+62***32") {
		t.Fatalf("maskedName with asterisk want true")
	}
	if !maskedName("+62...32") {
		t.Fatalf("maskedName with dot want true")
	}
	if !maskedName("+62∙∙∙32") {
		t.Fatalf("maskedName with bullet operator want true")
	}

	d := NewDirectory(nil)
	if got := d.Name("unknown@lid", "+62••••••32"); got != "Unknown" {
		t.Fatalf("Name(lid, masked) = %q, want 'Unknown'", got)
	}
}

func TestRewriteMentions(t *testing.T) {
	// stored list: "@35893729566783 jumat" + that LID known as push "Sinta" → "@Sinta jumat", spans [[0,6]]; two mentions; emoji before the mention keeps UTF-16 offsets right
	d := NewDirectory([]store.Person{
		{JID: "35893729566783@lid", PushName: "Sinta"},
		{JID: "6281234567890@s.whatsapp.net", FullName: "Budi"},
	})

	// 1. Single mention with push name (Mention strips leading ~)
	text, spans := rewriteMentions("@35893729566783 jumat", "35893729566783@lid", d)
	if text != "@Sinta jumat" {
		t.Fatalf("text = %q, want '@Sinta jumat'", text)
	}
	expectedSpans := [][2]int{{0, 6}}
	if !slices.Equal(spans, expectedSpans) {
		t.Fatalf("spans = %v, want %v", spans, expectedSpans)
	}

	// 2. Two mentions
	text2, spans2 := rewriteMentions("@35893729566783 and @6281234567890", "35893729566783@lid 6281234567890@s.whatsapp.net", d)
	if text2 != "@Sinta and @Budi" {
		t.Fatalf("text2 = %q, want '@Sinta and @Budi'", text2)
	}
	expectedSpans2 := [][2]int{{0, 6}, {11, 16}}
	if !slices.Equal(spans2, expectedSpans2) {
		t.Fatalf("spans2 = %v, want %v", spans2, expectedSpans2)
	}

	// 3. Emoji before the mention keeps UTF-16 offsets right
	// "🎉" is U+1F389, represented as 2 UTF-16 code units (surrogate pair).
	// "🎉 " has UTF-16 length 2 + 1 = 3.
	// "@Sinta" has UTF-16 length 6.
	// Span should be [3, 9].
	text3, spans3 := rewriteMentions("🎉 @35893729566783", "35893729566783@lid", d)
	if text3 != "🎉 @Sinta" {
		t.Fatalf("text3 = %q, want '🎉 @Sinta'", text3)
	}
	expectedSpans3 := [][2]int{{3, 9}}
	if !slices.Equal(spans3, expectedSpans3) {
		t.Fatalf("spans3 = %v, want %v", spans3, expectedSpans3)
	}

	// 4. Repeated mention in same body
	text4, spans4 := rewriteMentions("@35893729566783 and again @35893729566783", "35893729566783@lid", d)
	if text4 != "@Sinta and again @Sinta" {
		t.Fatalf("text4 = %q, want '@Sinta and again @Sinta'", text4)
	}
	expectedSpans4 := [][2]int{{0, 6}, {17, 23}}
	if !slices.Equal(spans4, expectedSpans4) {
		t.Fatalf("spans4 = %v, want %v", spans4, expectedSpans4)
	}

	// 5. Nil directory with mentions list falls back gracefully (LID becomes Unknown)
	text5, spans5 := rewriteMentions("@35893729566783", "35893729566783@lid", nil)
	if text5 != "@Unknown" {
		t.Fatalf("text5 = %q, want '@Unknown'", text5)
	}
	expectedSpans5 := [][2]int{{0, 8}}
	if !slices.Equal(spans5, expectedSpans5) {
		t.Fatalf("spans5 = %v, want %v", spans5, expectedSpans5)
	}

	// 6. Empty body
	if text6, spans6 := rewriteMentions("", "35893729566783@lid", d); text6 != "" || len(spans6) != 0 {
		t.Fatalf("empty body: text=%q, spans=%v", text6, spans6)
	}
}

func TestRewriteLeavesNonMentions(t *testing.T) {
	// Review Focus 3: no list: "call @6281234567890 now" unknown → unchanged; "a@6281234567890" unchanged; "@1234" (4 digits) unchanged; known digits → rewritten
	dEmpty := NewDirectory(nil)

	// Unknown 5-20 digits with no mentions list → unchanged
	if text, spans := rewriteMentions("call @6281234567890 now", "", dEmpty); text != "call @6281234567890 now" || len(spans) != 0 {
		t.Fatalf("unknown digits unchanged: text=%q, spans=%v", text, spans)
	}

	// Preceded by letter ("a@6281234567890") → unchanged
	if text, spans := rewriteMentions("a@6281234567890", "", dEmpty); text != "a@6281234567890" || len(spans) != 0 {
		t.Fatalf("preceded by letter: text=%q, spans=%v", text, spans)
	}

	// "@1234" (4 digits, less than 5) → unchanged
	if text, spans := rewriteMentions("@1234", "", dEmpty); text != "@1234" || len(spans) != 0 {
		t.Fatalf("4 digits: text=%q, spans=%v", text, spans)
	}

	// Known digits → rewritten
	dKnown := NewDirectory([]store.Person{
		{JID: "6281234567890@s.whatsapp.net", FullName: "Budi"},
	})
	text, spans := rewriteMentions("call @6281234567890 now", "", dKnown)
	if text != "call @Budi now" {
		t.Fatalf("known digits text = %q, want 'call @Budi now'", text)
	}
	expectedSpans := [][2]int{{5, 10}}
	if !slices.Equal(spans, expectedSpans) {
		t.Fatalf("known digits spans = %v, want %v", spans, expectedSpans)
	}
}

func TestSnippetLine(t *testing.T) {
	// each message kind → its label; caption wins; "You: Photo"; group other → "Sinta: hi"; deleted; empty chat → ""
	d := NewDirectory([]store.Person{
		{JID: "sinta@s.whatsapp.net", FullName: "Sinta"},
	})

	gifMeta, err := proto.Marshal(&waE2E.VideoMessage{GifPlayback: proto.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	voiceMeta, err := proto.Marshal(&waE2E.AudioMessage{PTT: proto.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	docMeta, err := proto.Marshal(&waE2E.DocumentMessage{FileName: proto.String("report.pdf")})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		chat store.Chat
		want string
	}{
		{
			name: "empty chat",
			chat: store.Chat{},
			want: "",
		},
		{
			name: "plain text",
			chat: store.Chat{LastType: "text", Snippet: "hello"},
			want: "hello",
		},
		{
			name: "formatted text shows without its markers",
			chat: store.Chat{LastType: "text", Snippet: "*Note:* _soon_ ~old~ `x` my_var"},
			want: "Note: soon old x my_var",
		},
		{
			name: "photo without caption",
			chat: store.Chat{LastType: "image"},
			want: "Photo",
		},
		{
			name: "photo with caption",
			chat: store.Chat{LastType: "image", Snippet: "beach sunset"},
			want: "beach sunset",
		},
		{
			name: "video without caption",
			chat: store.Chat{LastType: "video"},
			want: "Video",
		},
		{
			name: "video with caption",
			chat: store.Chat{LastType: "video", Snippet: "funny clip"},
			want: "funny clip",
		},
		{
			name: "gif without caption",
			chat: store.Chat{LastType: "video", LastMeta: gifMeta},
			want: "GIF",
		},
		{
			name: "sticker",
			chat: store.Chat{LastType: "sticker"},
			want: "Sticker",
		},
		{
			name: "voice message",
			chat: store.Chat{LastType: "audio", LastMeta: voiceMeta},
			want: "Voice message",
		},
		{
			name: "audio",
			chat: store.Chat{LastType: "audio"},
			want: "Audio",
		},
		{
			name: "document without caption",
			chat: store.Chat{LastType: "document", LastMeta: docMeta, Snippet: "report.pdf"},
			want: "Document: report.pdf",
		},
		{
			name: "document with caption",
			chat: store.Chat{LastType: "document", LastMeta: docMeta, Snippet: "Q3 financial report"},
			want: "Q3 financial report",
		},
		{
			name: "deleted message",
			chat: store.Chat{LastType: "revoked"},
			want: "This message was deleted",
		},
		{
			name: "unsupported message",
			chat: store.Chat{LastType: "unsupported"},
			want: "Unsupported message",
		},
		{
			name: "You prefix on outgoing media",
			chat: store.Chat{LastType: "image", LastFromMe: true},
			want: "You: Photo",
		},
		{
			name: "group other sender",
			chat: store.Chat{JID: "12345-67890@g.us", LastType: "text", Snippet: "hi", LastSender: "sinta@s.whatsapp.net", LastFromMe: false},
			want: "Sinta: hi",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := snippetLine(tc.chat, d); got != tc.want {
				t.Errorf("snippetLine() = %q, want %q", got, tc.want)
			}
		})
	}

	// Nil directory handled safely
	var nilDir *Directory
	if got := snippetLine(store.Chat{LastType: "text", Snippet: "hello"}, nilDir); got != "hello" {
		t.Fatalf("nil dir snippetLine direct: got %q, want 'hello'", got)
	}
	if got := snippetLine(store.Chat{JID: "12345-67890@g.us", LastType: "text", Snippet: "hello", LastSender: "6281234567890@s.whatsapp.net"}, nilDir); got != "+6281234567890: hello" {
		t.Fatalf("nil dir snippetLine group: got %q, want '+6281234567890: hello'", got)
	}
	if got := snippetLine(store.Chat{JID: "12345-67890@g.us", LastType: "system", Snippet: "~ Jun - HollaTeam removed you", LastSender: "6281234567890@s.whatsapp.net"}, nilDir); got != "~ Jun - HollaTeam removed you" {
		t.Fatalf("system snippet in group should not have sender prefix: got %q", got)
	}
}

func TestAvatarDue(t *testing.T) {
	// never checked → true; checked 6 days ago → false; 7 days → true; after avatarFailedTS(now): false at +9 min, true at +10 min
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

	// 1. Never checked (AvatarTS == 0) → true
	if !avatarDue(store.Person{AvatarTS: 0}, now) {
		t.Fatalf("never checked want true")
	}

	// 2. Checked 6 days ago → false
	sixDaysAgo := now.Add(-6 * 24 * time.Hour).Unix()
	if avatarDue(store.Person{AvatarTS: sixDaysAgo}, now) {
		t.Fatalf("checked 6 days ago want false")
	}

	// 3. Checked 7 days ago → true
	sevenDaysAgo := now.Add(-7 * 24 * time.Hour).Unix()
	if !avatarDue(store.Person{AvatarTS: sevenDaysAgo}, now) {
		t.Fatalf("checked 7 days ago want true")
	}

	// 4. Failed fetch retry rule: false at +9 min, true at +10 min
	failedTS := avatarFailedTS(now)
	failedPerson := store.Person{AvatarTS: failedTS}

	if avatarDue(failedPerson, now.Add(9*time.Minute)) {
		t.Fatalf("after failed fetch at +9m want false")
	}
	if !avatarDue(failedPerson, now.Add(10*time.Minute)) {
		t.Fatalf("after failed fetch at +10m want true")
	}
}

func TestDirectorySetAvatar(t *testing.T) {
	d := NewDirectory([]store.Person{
		{JID: "111@s.whatsapp.net", FullName: "Alice"},
	})

	// 1. Set avatar on existing person
	d.SetAvatar("111@s.whatsapp.net", "pic1", 12345678)
	p, ok := d.Person("111@s.whatsapp.net")
	if !ok || p.AvatarID != "pic1" || p.AvatarTS != 12345678 || p.FullName != "Alice" {
		t.Fatalf("expected updated avatar on existing person, got %+v", p)
	}

	// 2. Set avatar on new jid
	d.SetAvatar("222@s.whatsapp.net", "pic2", 98765432)
	p2, ok2 := d.Person("222@s.whatsapp.net")
	if !ok2 || p2.AvatarID != "pic2" || p2.AvatarTS != 98765432 {
		t.Fatalf("expected new person created with avatar, got %+v", p2)
	}

	// 3. Set avatar with cleanJID support
	d.SetAvatar("333:2@s.whatsapp.net", "pic3", 11111)
	p3, ok3 := d.Person("333@s.whatsapp.net")
	if !ok3 || p3.AvatarID != "pic3" || p3.AvatarTS != 11111 {
		t.Fatalf("expected cleanJID mapping to match, got %+v", p3)
	}

	// 4. Nil directory does not panic
	var nilDir *Directory
	nilDir.SetAvatar("111@s.whatsapp.net", "pic", 100)
}

func TestRewriteMentionsHighlightsAtAll(t *testing.T) {
	for body, want := range map[string][][2]int{
		"hi @all!":            {{3, 7}},
		"@Everyone meet @ALL": {{0, 9}, {15, 19}},
		"mail@all.com @allan": nil,
	} {
		if text, spans := rewriteMentions(body, "", nil); text != body || !slices.Equal(spans, want) {
			t.Errorf("rewriteMentions(%q) = %q %v, want spans %v", body, text, spans, want)
		}
	}
	// beside a member's mention, in order
	d := NewDirectory([]store.Person{{JID: "111@lid", FullName: "Budi"}})
	text, spans := rewriteMentions("@all and @111", "111@lid 222@lid", d)
	if text != "@all and @Budi" || !slices.Equal(spans, [][2]int{{0, 4}, {9, 14}}) {
		t.Errorf("with mentions: %q %v", text, spans)
	}
}
