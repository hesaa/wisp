package notify

import (
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

type fakeBalloon struct {
	title, body string
	silent      bool
	calls       int
}

func (f *fakeBalloon) show(title, body string, silent bool) error {
	f.title, f.body, f.silent = title, body, silent
	f.calls++
	return nil
}

var on = Settings{Enabled: true, Sound: true}

func msg() Input {
	return Input{AccountID: "a1", AccountLabel: "Work", ChatJID: "1@s.whatsapp.net",
		SenderName: "Ann", ChatName: "Ann", Type: "text", Body: "hello"}
}

func TestFormatBasic(t *testing.T) {
	n, ok := Format(msg(), on)
	if !ok || n.Title != "Ann" || n.Body != "hello" || n.Silent {
		t.Fatalf("got %+v %v", n, ok)
	}
}

func TestFormatGroup(t *testing.T) {
	in := msg()
	in.IsGroup, in.ChatName, in.SenderName = true, "Team", "Bob"
	n, _ := Format(in, on)
	if n.Title != "Bob @ Team" {
		t.Fatalf("title %q", n.Title)
	}
}

func TestFormatSuppressed(t *testing.T) {
	cases := map[string]func(*Input, *Settings){
		"fromMe":      func(i *Input, _ *Settings) { i.FromMe = true },
		"status":      func(i *Input, _ *Settings) { i.ChatJID = "status@broadcast" },
		"revoked":     func(i *Input, _ *Settings) { i.Type = "revoked" },
		"unsupported": func(i *Input, _ *Settings) { i.Type = "unsupported" },
		"disabled":    func(_ *Input, s *Settings) { s.Enabled = false },
	}
	for name, mod := range cases {
		in, s := msg(), on
		mod(&in, &s)
		if _, ok := Format(in, s); ok {
			t.Errorf("%s: should not notify", name)
		}
	}
}

func TestFormatPrivateMode(t *testing.T) {
	in := msg()
	in.IsGroup, in.ChatName, in.SenderName, in.Body = true, "Secret Group", "Bob", "secret text"
	n, ok := Format(in, Settings{Enabled: true, PrivateMode: true, Sound: true})
	if !ok || n.Title != "WhatsApp" || n.Body != "New message" {
		t.Fatalf("got %+v", n)
	}
	in.Type = "image"
	n, _ = Format(in, Settings{Enabled: true, PrivateMode: true})
	if n.Body != "New message" {
		t.Fatalf("media leaked: %+v", n)
	}
}

func TestFormatSound(t *testing.T) {
	n, _ := Format(msg(), Settings{Enabled: true, Sound: false})
	if !n.Silent {
		t.Fatal("sound off must be silent")
	}
}

func TestFormatMultiAccount(t *testing.T) {
	n, _ := Format(msg(), Settings{Enabled: true, Sound: true, MultiAccount: true})
	if n.Title != "Ann (Work)" {
		t.Fatalf("title %q", n.Title)
	}
	n, _ = Format(msg(), Settings{Enabled: true, Sound: true, MultiAccount: true, PrivateMode: true})
	if n.Title != "WhatsApp (Work)" {
		t.Fatalf("private title %q", n.Title)
	}
}

func TestFormatMedia(t *testing.T) {
	cases := []struct{ typ, body, file, want string }{
		{"image", "", "", "Photo"},
		{"image", "sunset", "", "Photo: sunset"},
		{"video", "", "", "Video"},
		{"audio", "", "", "Voice message"},
		{"sticker", "", "", "Sticker"},
		{"document", "", "a.pdf", "Document: a.pdf"},
		{"document", "", "", "Document"},
	}
	for _, c := range cases {
		in := msg()
		in.Type, in.Body, in.FileName = c.typ, c.body, c.file
		n, _ := Format(in, on)
		if n.Body != c.want {
			t.Errorf("%s: got %q want %q", c.typ, n.Body, c.want)
		}
	}
}

func TestTruncateUTF16(t *testing.T) {
	// 😀 is a surrogate pair (2 units); limit 4 units must not split one.
	got := truncateUTF16("ab😀😀", 4)
	if got != "ab😀" || !utf8.ValidString(got) {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("é", 500)
	if n := len(utf16.Encode([]rune(truncateUTF16(long, 255)))); n != 255 {
		t.Fatalf("units %d", n)
	}
	if truncateUTF16("short", 255) != "short" {
		t.Fatal("changed short string")
	}
}

func TestDispatcher(t *testing.T) {
	fb := &fakeBalloon{}
	d := newDispatcher(fb)
	if _, _, ok := d.Last(); ok {
		t.Fatal("no last yet")
	}
	if err := d.Notify(msg(), on); err != nil {
		t.Fatal(err)
	}
	if fb.calls != 1 || fb.title != "Ann" || fb.body != "hello" || fb.silent {
		t.Fatalf("%+v", fb)
	}
	acc, chat, ok := d.Last()
	if !ok || acc != "a1" || chat != "1@s.whatsapp.net" {
		t.Fatalf("last %q %q %v", acc, chat, ok)
	}
	in := msg()
	in.FromMe, in.ChatJID = true, "2@s.whatsapp.net"
	_ = d.Notify(in, on)
	if fb.calls != 1 {
		t.Fatal("suppressed message shown")
	}
	if _, chat, _ := d.Last(); chat != "1@s.whatsapp.net" {
		t.Fatal("last overwritten by suppressed message")
	}
}

func TestBadgeLabel(t *testing.T) {
	for n, want := range map[int]string{-1: "", 0: "", 3: "3", 9: "9", 10: "9+", 500: "9+"} {
		if got := badgeLabel(n); got != want {
			t.Errorf("%d: %q want %q", n, got, want)
		}
	}
}
