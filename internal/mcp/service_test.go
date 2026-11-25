package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"wa-go/internal/store"
)

type fakeStore struct {
	chats    map[string][]store.Chat
	people   map[string][]store.Person
	messages map[string]map[string][]store.Message // account -> chatJID -> newest-first messages
	pageSize int
}

func (f *fakeStore) ListChats(accountID string) ([]store.Chat, error) {
	return slices.Clone(f.chats[accountID]), nil
}

func (f *fakeStore) People(accountID string) ([]store.Person, error) {
	return slices.Clone(f.people[accountID]), nil
}

func (f *fakeStore) GetChatMessages(accountID, chatJID string, page int) ([]store.Message, error) {
	msgs := f.messages[accountID][chatJID]
	ps := f.pageSize
	if ps <= 0 {
		ps = store.PageSize
	}
	if page < 0 {
		page = 0
	}
	start := page * ps
	if start >= len(msgs) {
		return []store.Message{}, nil
	}
	end := min(start+ps, len(msgs))
	return slices.Clone(msgs[start:end]), nil
}

type sendTextCall struct {
	accountID string
	chatJID   string
	text      string
}

type sendMediaCall struct {
	accountID string
	chatJID   string
	path      string
	caption   string
}

type fakeSender struct {
	accounts   []string
	textCalls  []sendTextCall
	mediaCalls []sendMediaCall
	sendErr    error
}

func (f *fakeSender) Accounts() []string {
	return slices.Clone(f.accounts)
}

func (f *fakeSender) SendText(_ context.Context, accountID, chatJID, text string, _ *store.Message, _ []string) (store.Message, error) {
	if f.sendErr != nil {
		return store.Message{}, f.sendErr
	}
	f.textCalls = append(f.textCalls, sendTextCall{accountID: accountID, chatJID: chatJID, text: text})
	return store.Message{
		ID:        "sent-text-1",
		AccountID: accountID,
		ChatJID:   chatJID,
		Timestamp: 1700000100,
		Type:      "text",
		Body:      text,
		FromMe:    true,
		Status:    store.StatusSent,
	}, nil
}

func (f *fakeSender) SendMedia(_ context.Context, accountID, chatJID, path, caption string, _ *store.Message) (store.Message, error) {
	if f.sendErr != nil {
		return store.Message{}, f.sendErr
	}
	f.mediaCalls = append(f.mediaCalls, sendMediaCall{accountID: accountID, chatJID: chatJID, path: path, caption: caption})
	return store.Message{
		ID:        "sent-media-1",
		AccountID: accountID,
		ChatJID:   chatJID,
		Timestamp: 1700000200,
		Type:      "document",
		Body:      caption,
		FromMe:    true,
		Status:    store.StatusSent,
	}, nil
}

func callJSON(t *testing.T, svc *Service, name string, args any, out any) ToolResult {
	t.Helper()
	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			t.Fatalf("marshal args: %v", err)
		}
		raw = b
	}
	res, err := svc.CallTool(context.Background(), name, raw)
	if err != nil {
		t.Fatalf("CallTool(%s) unexpected error: %v", name, err)
	}
	if out != nil && !res.IsError && len(res.Content) > 0 {
		if err := json.Unmarshal([]byte(res.Content[0].Text), out); err != nil {
			t.Fatalf("unmarshal %s result %q: %v", name, res.Content[0].Text, err)
		}
	}
	return res
}

func TestListAccounts(t *testing.T) {
	svc := NewService(&fakeStore{}, &fakeSender{accounts: []string{"15550001", "15550002"}})
	if len(svc.Tools()) != 5 {
		t.Fatalf("got %d tools, want 5", len(svc.Tools()))
	}
	var out struct {
		Accounts []string `json:"accounts"`
	}
	res := callJSON(t, svc, "list_accounts", map[string]any{}, &out)
	if res.IsError {
		t.Fatalf("list_accounts error: %v", res.Content)
	}
	if !slices.Equal(out.Accounts, []string{"15550001", "15550002"}) {
		t.Fatalf("accounts = %v, want [15550001 15550002]", out.Accounts)
	}
}

func TestSearchChatsAndContacts_MatchesByNameAndJID(t *testing.T) {
	st := &fakeStore{
		chats: map[string][]store.Chat{
			"acct1": {
				{JID: "15551112222@s.whatsapp.net", Name: "Alice Chat", LastMessageTS: 100},
				{JID: "120363000111@g.us", Name: "Project Alpha", LastMessageTS: 200},
			},
			"acct2": {
				{JID: "15559998888@s.whatsapp.net", Name: "Alpha Support", LastMessageTS: 150},
			},
		},
		people: map[string][]store.Person{
			"acct1": {
				// Same JID as chat above: must deduplicate and prefer FullName.
				{JID: "15551112222@s.whatsapp.net", FullName: "Alice Wonderland", PushName: "Ali"},
				{JID: "15553334444@s.whatsapp.net", FullName: "Bob Builder", PushName: "Bobby"},
			},
		},
	}
	wa := &fakeSender{accounts: []string{"acct1", "acct2"}}
	svc := NewService(st, wa)

	// Case-insensitive name search across both accounts, including chat+person deduplication.
	var out struct {
		Results []struct {
			Account string `json:"account"`
			JID     string `json:"jid"`
			Name    string `json:"name"`
			Type    string `json:"type"`
		} `json:"results"`
	}
	res := callJSON(t, svc, "search_chats_and_contacts", map[string]any{"query": "ALPHA"}, &out)
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.Content)
	}
	if len(out.Results) != 2 {
		t.Fatalf("got %d results, want 2: %+v", len(out.Results), out.Results)
	}
	if out.Results[0].Account != "acct1" || out.Results[0].JID != "120363000111@g.us" || out.Results[0].Type != "group" {
		t.Fatalf("unexpected first result: %+v", out.Results[0])
	}
	if out.Results[1].Account != "acct2" || out.Results[1].JID != "15559998888@s.whatsapp.net" {
		t.Fatalf("unexpected second result: %+v", out.Results[1])
	}

	// Deduplication + preferred FullName when matching Alice by JID substring.
	out.Results = nil
	res = callJSON(t, svc, "search_chats_and_contacts", map[string]any{"query": "5551112222", "account": "acct1"}, &out)
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.Content)
	}
	if len(out.Results) != 1 {
		t.Fatalf("expected deduplicated 1 result for Alice, got %d: %+v", len(out.Results), out.Results)
	}
	if out.Results[0].Name != "Alice Wonderland" {
		t.Fatalf("expected preferred FullName 'Alice Wonderland', got %q", out.Results[0].Name)
	}

	// Match person-only contact by name.
	out.Results = nil
	res = callJSON(t, svc, "search_chats_and_contacts", map[string]any{"query": "builder"}, &out)
	if res.IsError || len(out.Results) != 1 || out.Results[0].JID != "15553334444@s.whatsapp.net" {
		t.Fatalf("unexpected builder search result: res=%+v out=%+v", res, out)
	}
}

func TestGetChatHistory_ReturnsTextAndMediaMetadata(t *testing.T) {
	docMeta, err := proto.Marshal(&waE2E.DocumentMessage{
		FileName: proto.String("handoff.pdf"),
		Mimetype: proto.String("application/pdf"),
		Caption:  proto.String("Q4 handoff"),
	})
	if err != nil {
		t.Fatalf("marshal doc: %v", err)
	}
	imgMeta, err := proto.Marshal(&waE2E.ImageMessage{
		Mimetype: proto.String("image/png"),
		Caption:  proto.String("chart"),
	})
	if err != nil {
		t.Fatalf("marshal img: %v", err)
	}

	chatJID := "120363000111@g.us"
	st := &fakeStore{
		pageSize: 2,
		chats: map[string][]store.Chat{
			"acct1": {{JID: chatJID, Name: "Group Foo"}},
		},
		people: map[string][]store.Person{
			"acct1": {
				{JID: "15551112222@s.whatsapp.net", FullName: "Alice"},
			},
		},
		messages: map[string]map[string][]store.Message{
			"acct1": {
				chatJID: {
					{
						ID:        "m3",
						ChatJID:   chatJID,
						AccountID: "acct1",
						SenderJID: "15551112222@s.whatsapp.net",
						Timestamp: 1700000300,
						Type:      "document",
						Body:      "Q4 handoff",
						MediaMeta: docMeta,
					},
					{
						ID:        "m2",
						ChatJID:   chatJID,
						AccountID: "acct1",
						SenderJID: "",
						FromMe:    true,
						Timestamp: 1700000200,
						Type:      "image",
						Body:      "chart",
						MediaMeta: imgMeta,
					},
					{
						ID:        "m1",
						ChatJID:   chatJID,
						AccountID: "acct1",
						SenderJID: "15551112222@s.whatsapp.net",
						Timestamp: 1700000100,
						Type:      "text",
						Body:      "hello world",
					},
				},
			},
		},
	}
	wa := &fakeSender{accounts: []string{"acct1"}}
	svc := NewService(st, wa)

	var out struct {
		Account  string `json:"account"`
		ChatJID  string `json:"chat_jid"`
		Messages []struct {
			ID            string `json:"id"`
			SenderJID     string `json:"sender_jid"`
			SenderName    string `json:"sender_name"`
			FromMe        bool   `json:"from_me"`
			Timestamp     string `json:"timestamp"`
			Type          string `json:"type"`
			Body          string `json:"body"`
			MediaFilename string `json:"media_filename"`
			MediaMime     string `json:"media_mime"`
			Caption       string `json:"caption"`
		} `json:"messages"`
	}

	// Request limit=3 across pageSize=2 (tests multi-page fetching + metadata).
	res := callJSON(t, svc, "get_chat_history", map[string]any{
		"chat_jid": chatJID,
		"limit":    3,
	}, &out)
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.Content)
	}
	if out.Account != "acct1" || len(out.Messages) != 3 {
		t.Fatalf("got account=%q len=%d, want acct1 and 3", out.Account, len(out.Messages))
	}

	mDoc := out.Messages[0]
	if mDoc.ID != "m3" || mDoc.SenderName != "Alice" || mDoc.Type != "document" ||
		mDoc.MediaFilename != "handoff.pdf" || mDoc.MediaMime != "application/pdf" || mDoc.Caption != "Q4 handoff" {
		t.Fatalf("unexpected document message: %+v", mDoc)
	}
	if mDoc.Timestamp == "" {
		t.Fatalf("expected non-empty timestamp")
	}

	mImg := out.Messages[1]
	if mImg.ID != "m2" || !mImg.FromMe || mImg.Type != "image" || mImg.MediaMime != "image/png" || mImg.Caption != "chart" {
		t.Fatalf("unexpected image message: %+v", mImg)
	}

	// Verify limit=2 truncates cleanly.
	out.Messages = nil
	res = callJSON(t, svc, "get_chat_history", map[string]any{
		"chat_jid": chatJID,
		"limit":    2,
	}, &out)
	if res.IsError || len(out.Messages) != 2 {
		t.Fatalf("expected 2 messages with limit=2, got err=%v len=%d", res.IsError, len(out.Messages))
	}
}

func TestPrepareSend_AmbiguousRecipient(t *testing.T) {
	st := &fakeStore{
		people: map[string][]store.Person{
			"acct1": {
				{JID: "15550001111@s.whatsapp.net", FullName: "Joe Smith"},
				{JID: "15550002222@s.whatsapp.net", FullName: "Joe Brown"},
			},
		},
	}
	wa := &fakeSender{accounts: []string{"acct1"}}
	svc := NewService(st, wa)

	var out struct {
		Status       string `json:"status"`
		ConfirmToken string `json:"confirm_token"`
		Candidates   []struct {
			Account string `json:"account"`
			JID     string `json:"jid"`
			Name    string `json:"name"`
		} `json:"candidates"`
	}
	res := callJSON(t, svc, "prepare_send", map[string]any{
		"recipient": "Joe",
		"text":      "Hey Joe!",
	}, &out)
	if res.IsError {
		t.Fatalf("expected structured ambiguous result, got error: %v", res.Content)
	}
	if out.Status != "ambiguous" {
		t.Fatalf("status = %q, want ambiguous", out.Status)
	}
	if out.ConfirmToken != "" {
		t.Fatalf("confirm_token should be empty on ambiguous match, got %q", out.ConfirmToken)
	}
	if len(out.Candidates) != 2 {
		t.Fatalf("got %d candidates, want 2: %+v", len(out.Candidates), out.Candidates)
	}
	if len(wa.textCalls) != 0 || len(wa.mediaCalls) != 0 {
		t.Fatalf("no sends should occur during prepare_send")
	}
}

func TestPrepareSend_InvalidMediaPath(t *testing.T) {
	st := &fakeStore{
		chats: map[string][]store.Chat{
			"acct1": {{JID: "120363000111@g.us", Name: "Group Foo"}},
		},
	}
	wa := &fakeSender{accounts: []string{"acct1"}}
	svc := NewService(st, wa)

	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.pdf")

	for _, p := range []string{missing, dir} {
		res := callJSON(t, svc, "prepare_send", map[string]any{
			"recipient":  "Group Foo",
			"media_path": p,
		}, nil)
		if !res.IsError {
			t.Fatalf("expected IsError=true for invalid media_path %q, got %+v", p, res)
		}
	}
}

func TestPrepareAndConfirmSend_SingleUseAndExpiry(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(reportPath, []byte("report"), 0600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	st := &fakeStore{
		chats: map[string][]store.Chat{
			"acct1": {{JID: "120363000111@g.us", Name: "Group Foo"}},
		},
	}
	wa := &fakeSender{accounts: []string{"acct1"}}
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	svc := NewService(st, wa)
	svc.now = func() time.Time { return now }

	// 1. Text prepare + confirm (single-use).
	var prep struct {
		Status       string `json:"status"`
		ConfirmToken string `json:"confirm_token"`
		Account      string `json:"account"`
	}
	res := callJSON(t, svc, "prepare_send", map[string]any{
		"recipient": "Group Foo",
		"text":      "Handoff ready",
	}, &prep)
	if res.IsError || prep.Status != "needs_confirmation" || prep.ConfirmToken == "" {
		t.Fatalf("unexpected prepare_send result: res=%+v prep=%+v", res, prep)
	}

	var conf struct {
		Status    string `json:"status"`
		MessageID string `json:"message_id"`
	}
	res = callJSON(t, svc, "confirm_send", map[string]any{"confirm_token": prep.ConfirmToken}, &conf)
	if res.IsError || conf.Status != "sent" {
		t.Fatalf("confirm_send failed: res=%+v conf=%+v", res, conf)
	}
	if len(wa.textCalls) != 1 || wa.textCalls[0].text != "Handoff ready" || wa.textCalls[0].chatJID != "120363000111@g.us" {
		t.Fatalf("unexpected textCalls: %+v", wa.textCalls)
	}

	// Second confirm_send with same token must fail and not send again.
	res = callJSON(t, svc, "confirm_send", map[string]any{"confirm_token": prep.ConfirmToken}, nil)
	if !res.IsError {
		t.Fatalf("expected error on token replay")
	}
	if len(wa.textCalls) != 1 {
		t.Fatalf("expected still 1 textCall after replay, got %d", len(wa.textCalls))
	}

	// 2. Media prepare + confirm.
	prep = struct {
		Status       string `json:"status"`
		ConfirmToken string `json:"confirm_token"`
		Account      string `json:"account"`
	}{}
	res = callJSON(t, svc, "prepare_send", map[string]any{
		"recipient":  "Group Foo",
		"media_path": reportPath,
		"text":       "See attached",
	}, &prep)
	if res.IsError || prep.Status != "needs_confirmation" {
		t.Fatalf("media prepare_send failed: %+v", res)
	}
	res = callJSON(t, svc, "confirm_send", map[string]any{"confirm_token": prep.ConfirmToken}, &conf)
	if res.IsError || len(wa.mediaCalls) != 1 || wa.mediaCalls[0].path != reportPath || wa.mediaCalls[0].caption != "See attached" {
		t.Fatalf("media confirm_send failed: res=%+v calls=%+v", res, wa.mediaCalls)
	}

	// 3. Expiry after 5 minutes.
	prep = struct {
		Status       string `json:"status"`
		ConfirmToken string `json:"confirm_token"`
		Account      string `json:"account"`
	}{}
	res = callJSON(t, svc, "prepare_send", map[string]any{
		"recipient": "Group Foo",
		"text":      "Will expire",
	}, &prep)
	if res.IsError || prep.ConfirmToken == "" {
		t.Fatalf("prepare_send failed: %+v", res)
	}
	now = now.Add(5*time.Minute + time.Second)
	res = callJSON(t, svc, "confirm_send", map[string]any{"confirm_token": prep.ConfirmToken}, nil)
	if !res.IsError {
		t.Fatalf("expected error for expired token, got %+v", res)
	}
	if !strings.Contains(strings.ToLower(res.Content[0].Text), "expired") {
		t.Fatalf("expected expiry error message, got %q", res.Content[0].Text)
	}
	if len(wa.textCalls) != 1 {
		t.Fatalf("expired token must not send, textCalls=%d", len(wa.textCalls))
	}
}

func TestGetChatHistory_MultiAccountResolution(t *testing.T) {
	st := &fakeStore{
		chats: map[string][]store.Chat{
			"acct1": {{JID: "111@s.whatsapp.net", Name: "One"}},
			"acct2": {{JID: "222@s.whatsapp.net", Name: "Two"}, {JID: "shared@g.us", Name: "Shared"}},
		},
		messages: map[string]map[string][]store.Message{
			"acct1": {
				"shared@g.us": {{ID: "a1", ChatJID: "shared@g.us", Type: "text", Body: "from 1", Timestamp: 10}},
			},
			"acct2": {
				"222@s.whatsapp.net": {{ID: "b1", ChatJID: "222@s.whatsapp.net", Type: "text", Body: "only 2", Timestamp: 20}},
				"shared@g.us":        {{ID: "b2", ChatJID: "shared@g.us", Type: "text", Body: "from 2", Timestamp: 30}},
			},
		},
	}
	wa := &fakeSender{accounts: []string{"acct1", "acct2"}}
	svc := NewService(st, wa)

	// Unambiguous across two accounts: resolves to acct2 automatically.
	var out struct {
		Account  string `json:"account"`
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	res := callJSON(t, svc, "get_chat_history", map[string]any{"chat_jid": "222@s.whatsapp.net"}, &out)
	if res.IsError || out.Account != "acct2" || len(out.Messages) != 1 || out.Messages[0].ID != "b1" {
		t.Fatalf("unexpected unambiguous history result: res=%+v out=%+v", res, out)
	}

	// Ambiguous across two accounts when account omitted: must error.
	res = callJSON(t, svc, "get_chat_history", map[string]any{"chat_jid": "shared@g.us"}, nil)
	if !res.IsError {
		t.Fatalf("expected error for chat present in multiple accounts")
	}

	// Specifying account disambiguates.
	out.Messages = nil
	res = callJSON(t, svc, "get_chat_history", map[string]any{"chat_jid": "shared@g.us", "account": "acct1"}, &out)
	if res.IsError || out.Account != "acct1" || len(out.Messages) != 1 || out.Messages[0].ID != "a1" {
		t.Fatalf("unexpected explicit account result: res=%+v out=%+v", res, out)
	}
}

func TestPrepareSend_ExactJIDPhoneAndLIDDedup(t *testing.T) {
	st := &fakeStore{
		chats: map[string][]store.Chat{
			"acct1": {{JID: "15551112222@s.whatsapp.net", Name: ""}},
		},
		people: map[string][]store.Person{
			"acct1": {
				{JID: "999888@lid", PNJID: "15551112222@s.whatsapp.net", FullName: "Alice"},
				{JID: "1555111222200@s.whatsapp.net", FullName: "Other"},
			},
		},
	}
	wa := &fakeSender{accounts: []string{"acct1"}}
	svc := NewService(st, wa)

	// LID + PNJID merged with chat -> "Alice" is unambiguous.
	var prep struct {
		Status    string `json:"status"`
		Recipient struct {
			JID  string `json:"jid"`
			Name string `json:"name"`
		} `json:"recipient"`
	}
	res := callJSON(t, svc, "prepare_send", map[string]any{
		"recipient": "Alice",
		"text":      "Hi Alice",
	}, &prep)
	if res.IsError || prep.Status != "needs_confirmation" || prep.Recipient.JID != "15551112222@s.whatsapp.net" {
		t.Fatalf("expected merged Alice entry, got res=%+v prep=%+v", res, prep)
	}

	// Formatted phone number matches Alice's PNJID (and not 1555111222200? wait: 15551112222 is prefix of 1555111222200; exact JID tests prefix disambiguation).
	res = callJSON(t, svc, "prepare_send", map[string]any{
		"recipient": "15551112222@s.whatsapp.net",
		"text":      "Exact JID",
	}, &prep)
	if res.IsError || prep.Status != "needs_confirmation" || prep.Recipient.JID != "15551112222@s.whatsapp.net" {
		t.Fatalf("expected exact JID match, got res=%+v prep=%+v", res, prep)
	}
}

func TestCallTool_UnknownTool(t *testing.T) {
	svc := NewService(&fakeStore{}, &fakeSender{})
	res, err := svc.CallTool(context.Background(), "no_such_tool", nil)
	if err == nil || !res.IsError {
		t.Fatalf("expected error and IsError=true for unknown tool, got res=%+v err=%v", res, err)
	}
}
