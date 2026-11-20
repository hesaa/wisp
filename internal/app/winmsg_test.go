package app

import (
	"testing"
)

func TestParseProtocolAction_Open(t *testing.T) {
	action, ok := parseProtocolAction("wisp://open?acct=62812345&jid=alice%40s.whatsapp.net")
	if !ok {
		t.Fatal("expected ok = true")
	}
	if action.kind != "open" {
		t.Errorf("got kind %q, want open", action.kind)
	}
	if action.accountID != "62812345" {
		t.Errorf("got accountID %q", action.accountID)
	}
	if action.chatJID != "alice@s.whatsapp.net" {
		t.Errorf("got chatJID %q", action.chatJID)
	}
}

func TestParseProtocolAction_Reply(t *testing.T) {
	action, ok := parseProtocolAction("wisp://reply?acct=62812345&jid=alice%40s.whatsapp.net&replyText=Hello+World%21")
	if !ok {
		t.Fatal("expected ok = true")
	}
	if action.kind != "reply" {
		t.Errorf("got kind %q, want reply", action.kind)
	}
	if action.accountID != "62812345" {
		t.Errorf("got accountID %q", action.accountID)
	}
	if action.chatJID != "alice@s.whatsapp.net" {
		t.Errorf("got chatJID %q", action.chatJID)
	}
	if action.text != "Hello World!" {
		t.Errorf("got text %q, want 'Hello World!'", action.text)
	}
}

func TestParseProtocolAction_MarkRead(t *testing.T) {
	action, ok := parseProtocolAction("wisp://markread?acct=62812345&jid=alice%40s.whatsapp.net")
	if !ok {
		t.Fatal("expected ok = true")
	}
	if action.kind != "markread" {
		t.Errorf("got kind %q, want markread", action.kind)
	}
	if action.accountID != "62812345" {
		t.Errorf("got accountID %q", action.accountID)
	}
	if action.chatJID != "alice@s.whatsapp.net" {
		t.Errorf("got chatJID %q", action.chatJID)
	}
}

func TestParseProtocolAction_Malformed(t *testing.T) {
	cases := []string{
		"",
		"http://open?acct=1",
		"wisp://unknown?acct=1",
		"not-a-url",
		"://bad",
	}
	for _, c := range cases {
		if action, ok := parseProtocolAction(c); ok {
			t.Errorf("expected ok = false for %q; got %+v", c, action)
		}
	}
}

func TestSendQuickReply_Locked(t *testing.T) {
	a := &App{locked: true}
	// If locked, sendQuickReply must return immediately without touching mgr
	a.sendQuickReply("acc1", "chat1", "hello")
}

