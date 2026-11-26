package whatsapp

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"wa-go/internal/linkify"
	"wa-go/internal/store"
)

func TestVideoNoteMarkSurvivesStorageAndIsStripped(t *testing.T) {
	orig := &waE2E.VideoMessage{Caption: proto.String("hey")}
	_, _, media, _ := content(&waE2E.Message{PtvMessage: orig})
	if len(orig.ProtoReflect().GetUnknown()) != 0 {
		t.Fatal("incoming message was modified")
	}
	meta, err := proto.Marshal(media)
	if err != nil {
		t.Fatal(err)
	}
	var stored waE2E.VideoMessage
	if err := proto.Unmarshal(meta, &stored); err != nil || !IsVideoNote(&stored) {
		t.Fatalf("mark lost: %v", err)
	}
	if len(stored.ProtoReflect().GetUnknown()) != 0 || IsVideoNote(&stored) || stored.GetCaption() != "hey" {
		t.Fatal("mark not stripped, or content lost")
	}
	if IsVideoNote(&waE2E.VideoMessage{}) {
		t.Fatal("plain video taken for a note")
	}
	q := quotedMessage(&store.Message{Type: TypeVideo, MediaMeta: meta})
	if q.GetPtvMessage() == nil || q.GetVideoMessage() != nil || len(q.GetPtvMessage().ProtoReflect().GetUnknown()) != 0 {
		t.Fatalf("quote of a video note: %v", q)
	}
}

func TestContentMapsFormerlyUnsupportedKinds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		m        *waE2E.Message
		typ      string
		body     string
		wantSkip bool
	}{
		{"location", &waE2E.Message{LocationMessage: &waE2E.LocationMessage{Name: proto.String("Monas")}}, TypeText, "Location: Monas", false},
		{"bare location", &waE2E.Message{LocationMessage: &waE2E.LocationMessage{}}, TypeText, "Location", false},
		{"contact", &waE2E.Message{ContactMessage: &waE2E.ContactMessage{DisplayName: proto.String("Budi")}}, TypeText, "Contact: Budi", false},
		{"poll v3", &waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{Name: proto.String("Lunch?")}}, TypeText, "Poll: Lunch?", false},
		{"video call", &waE2E.Message{CallLogMesssage: &waE2E.CallLogMessage{IsVideo: proto.Bool(true)}}, TypeText, "Video call", false},
		{"button reply", &waE2E.Message{ButtonsResponseMessage: &waE2E.ButtonsResponseMessage{
			Response: &waE2E.ButtonsResponseMessage_SelectedDisplayText{SelectedDisplayText: "Yes"}}}, TypeText, "Yes", false},
		{"ptv", &waE2E.Message{PtvMessage: &waE2E.VideoMessage{Caption: proto.String("hey")}}, TypeVideo, "hey", false},
		{"wrapped text", &waE2E.Message{GroupMentionedMessage: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{Conversation: proto.String("hi")}}}, TypeText, "hi", false},
		{"enc reaction", &waE2E.Message{EncReactionMessage: &waE2E.EncReactionMessage{}}, "signal", "", true},
		{"unknown", &waE2E.Message{}, TypeUnsupported, "", false},
	} {
		typ, body, _, skip := content(tc.m)
		if typ != tc.typ || body != tc.body || skip != tc.wantSkip {
			t.Errorf("%s: got (%q, %q, skip=%v), want (%q, %q, skip=%v)", tc.name, typ, body, skip, tc.typ, tc.body, tc.wantSkip)
		}
	}
}

func TestRefetchPlanCoversEveryUnsupportedMessage(t *testing.T) {
	now := time.Unix(5000, 0)
	row := func(chat, id, typ string, ts int64) store.Message {
		return store.Message{ChatJID: chat, WireChatJID: "", ID: id, Type: typ, Timestamp: ts}
	}
	rows := []store.Message{ // by chat, newest first
		row("1@s.whatsapp.net", "u1", TypeUnsupported, 9), row("1@s.whatsapp.net", "t1", TypeText, 8),
		row("1@s.whatsapp.net", "u2", TypeUnsupported, 7), row("1@s.whatsapp.net", "t2", TypeText, 6),
		row("2@s.whatsapp.net", "t3", TypeText, 9), row("2@s.whatsapp.net", "u3", TypeUnsupported, 8),
	}
	got := refetchPlan(rows, 2, now)
	if len(got) != 3 {
		t.Fatalf("requests %+v", got)
	}
	// u1 is its chat's newest: made-up anchor at now, covering u1, t1
	if got[0].anchor.Chat.User != "1" || got[0].anchor.ID == "" || !got[0].anchor.Timestamp.Equal(now) || got[0].count != 2 {
		t.Fatalf("first %+v", got[0])
	}
	if got[1].anchor.ID != "t1" || got[1].anchor.Timestamp.Unix() != 8 { // u2, the message after it
		t.Fatalf("second %+v", got[1])
	}
	if got[2].anchor.ID != "t3" || got[2].anchor.Chat.User != "2" {
		t.Fatalf("third %+v", got[2])
	}
}

type peerClient struct {
	fakeClient
	asked []refetchRequest
}

func (p *peerClient) BuildHistorySyncRequest(info *types.MessageInfo, count int) *waE2E.Message {
	p.asked = append(p.asked, refetchRequest{anchor: *info, count: count})
	return &waE2E.Message{}
}

func (p *peerClient) SendPeerMessage(context.Context, *waE2E.Message) (whatsmeow.SendResponse, error) {
	return whatsmeow.SendResponse{}, nil
}

func TestRefetchUnsupportedRunsOncePerAccount(t *testing.T) {
	m, db := newTestManager(t)
	u := store.Message{AccountID: acctA, ChatJID: alice.String(), ID: "u", Timestamp: 1, Type: TypeUnsupported, Status: store.StatusRead}
	if err := db.InsertMessagesBatch([]store.Message{u}); err != nil {
		t.Fatal(err)
	}
	pc := &peerClient{}
	m.refetchUnsupported(ctx, acctA, pc, 0)
	m.refetchUnsupported(ctx, acctA, pc, 0)
	if len(pc.asked) != 1 || pc.asked[0].anchor.Chat.String() != alice.String() {
		t.Fatalf("asked %+v", pc.asked)
	}
}

func TestToStoreMessageExtractsLinkPreview(t *testing.T) {
	info := types.MessageInfo{
		MessageSource: types.MessageSource{Chat: alice, Sender: alice},
		ID:            "m-link-1",
		Timestamp:     time.Unix(1700000000, 0),
	}
	thumb := []byte{0xff, 0xd8, 0xff, 0xe0}

	// Full ExtendedTextMessage preview metadata (wrapped in EphemeralMessage to verify unwrap).
	wrapped := &waE2E.Message{
		EphemeralMessage: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{
				ExtendedTextMessage: &waE2E.ExtendedTextMessage{
					Text:          proto.String("check https://example.com"),
					MatchedText:   proto.String("https://example.com"),
					Title:         proto.String("Example Domain"),
					Description:   proto.String("Summary text"),
					JPEGThumbnail: thumb,
				},
			},
		},
	}
	got, ok, err := toStoreMessage(acctA, info, wrapped)
	if err != nil || !ok {
		t.Fatalf("toStoreMessage(wrapped): ok=%v err=%v", ok, err)
	}
	if got.LinkURL != "https://example.com" || got.LinkTitle != "Example Domain" ||
		got.LinkDesc != "Summary text" || !bytes.Equal(got.LinkThumb, thumb) {
		t.Fatalf("got link fields = (%q, %q, %q, %x)", got.LinkURL, got.LinkTitle, got.LinkDesc, got.LinkThumb)
	}

	// Sender-controlled title/description are sanitized and clamped at extraction.
	hostile := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String("see https://x.example"),
			Title:       proto.String("Bad\xff\n\n  title" + strings.Repeat("t", 5000)),
			Description: proto.String(strings.Repeat("d ", 5000)),
		},
	}
	got, ok, err = toStoreMessage(acctA, info, hostile)
	if err != nil || !ok {
		t.Fatalf("toStoreMessage(hostile): ok=%v err=%v", ok, err)
	}
	if !strings.HasPrefix(got.LinkTitle, "Bad� title") || !utf8.ValidString(got.LinkTitle) ||
		utf8.RuneCountInString(got.LinkTitle) > linkify.MaxPreviewTitleRunes ||
		utf8.RuneCountInString(got.LinkDesc) > linkify.MaxPreviewDescRunes {
		t.Fatalf("hostile preview not sanitized: title %d runes %q..., desc %d runes",
			utf8.RuneCountInString(got.LinkTitle), got.LinkTitle[:min(len(got.LinkTitle), 20)], utf8.RuneCountInString(got.LinkDesc))
	}

	// Falls back LinkURL to linkify.FirstURL when MatchedText is empty.
	noMatched := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:  proto.String("see https://fallback.dev/path for details"),
			Title: proto.String("Fallback Title"),
		},
	}
	got, ok, err = toStoreMessage(acctA, info, noMatched)
	if err != nil || !ok {
		t.Fatalf("toStoreMessage(noMatched): ok=%v err=%v", ok, err)
	}
	if got.LinkURL != "https://fallback.dev/path" || got.LinkTitle != "Fallback Title" {
		t.Fatalf("fallback LinkURL = %q, Title = %q", got.LinkURL, got.LinkTitle)
	}

	// Plain ExtendedTextMessage (e.g. reply with URL in text) without preview metadata leaves Link* empty.
	plainReply := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String("replying about https://plain.example"),
			ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("q1")},
		},
	}
	got, ok, err = toStoreMessage(acctA, info, plainReply)
	if err != nil || !ok {
		t.Fatalf("toStoreMessage(plainReply): ok=%v err=%v", ok, err)
	}
	if got.LinkURL != "" || got.LinkTitle != "" || got.LinkDesc != "" || len(got.LinkThumb) != 0 {
		t.Fatalf("expected empty Link* for non-preview ExtendedTextMessage, got %+v", got)
	}
}
