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

func TestRangeBetween(t *testing.T) {
	var msgs []store.Message
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		msgs = append(msgs, store.Message{ID: id})
	}
	ids := func(m []store.Message) string {
		s := ""
		for _, x := range m {
			s += x.ID
		}
		return s
	}
	// Shift+Down three times from c (anchor 2): the range grows
	for i, want := range []string{"cd", "cde", "cdef"} {
		if got := ids(rangeBetween(msgs, 2, 3+i)); got != want {
			t.Errorf("step %d: %q, want %q", i, got, want)
		}
	}
	if got := ids(rangeBetween(msgs, 2, 4)); got != "cde" { // reversing shrinks
		t.Errorf("shrink: %q", got)
	}
	if got := ids(rangeBetween(msgs, 4, 1)); got != "bcde" { // anchor after the end
		t.Errorf("reverse: %q", got)
	}
	if got := ids(rangeBetween(msgs, 3, 3)); got != "d" {
		t.Errorf("single: %q", got)
	}
}
