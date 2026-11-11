package ui

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"wa-go/internal/store"
)

func TestCopyMessagesOne(t *testing.T) {
	got := copyMessages([]store.Message{{Type: "text", Body: "hello\nworld", SenderJID: alice}}, func(store.Message) string { return "Sinta" }, time.UTC)
	if got != "hello\nworld" {
		t.Errorf("got %q", got)
	}
}

func TestCopyMessagesMany(t *testing.T) {
	at := time.Date(2026, 11, 11, 8, 30, 0, 0, time.UTC).Unix()
	msgs := []store.Message{
		{Type: "text", Body: "hi", Timestamp: at},
		{Type: "revoked", Timestamp: at + 30}, // nothing to copy
		{Type: "text", Body: "ok", Timestamp: at + 60, FromMe: true},
	}
	got := copyMessages(msgs, func(store.Message) string { return "Sinta" }, time.UTC)
	if want := "[08:30, 11/11/2026] Sinta: hi\r\n[08:31, 11/11/2026] You: ok"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMediaLabels(t *testing.T) {
	for _, c := range []struct {
		m    store.Message
		want string
	}{
		{store.Message{Type: "image"}, "[Photo]"},
		{store.Message{Type: "sticker"}, "[Sticker]"},
		{store.Message{Type: "video", MediaMeta: meta(t, &waE2E.VideoMessage{})}, "[Video]"},
		{store.Message{Type: "video", MediaMeta: meta(t, &waE2E.VideoMessage{GifPlayback: proto.Bool(true)})}, "[GIF]"},
		{store.Message{Type: "audio", MediaMeta: meta(t, &waE2E.AudioMessage{PTT: proto.Bool(true)})}, "[Voice message]"},
		{store.Message{Type: "audio", MediaMeta: meta(t, &waE2E.AudioMessage{})}, "[Audio]"},
		{store.Message{Type: "document", Body: "invoice.pdf", MediaMeta: meta(t, &waE2E.DocumentMessage{FileName: proto.String("invoice.pdf")})}, "[Document: invoice.pdf]"},
	} {
		if got := messageText(c.m); got != c.want {
			t.Errorf("%s: %q, want %q", c.m.Type, got, c.want)
		}
	}
}

func TestCaptionWinsOverLabel(t *testing.T) {
	if got := messageText(store.Message{Type: "image", Body: "look"}); got != "look" {
		t.Errorf("got %q", got)
	}
}
