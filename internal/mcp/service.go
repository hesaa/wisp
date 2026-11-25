// Package mcp implements Wisp's built-in MCP tool service for searching contacts/chats,
// reading message history, and sending text or media messages with explicit confirmation.
package mcp

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"wa-go/internal/store"
	"wa-go/internal/whatsapp"
)

const (
	defaultHistoryLimit = 20
	maxHistoryLimit     = 100
	defaultSearchLimit  = 50
	maxSearchLimit      = 200
	confirmTTL          = 5 * time.Minute
	statusBroadcastJID  = "status@broadcast"
)

// Store is the subset of *store.DB consumed by the MCP service.
type Store interface {
	ListChats(accountID string) ([]store.Chat, error)
	People(accountID string) ([]store.Person, error)
	GetChatMessages(accountID, chatJID string, page int) ([]store.Message, error)
}

// Sender is the subset of *whatsapp.Manager consumed by the MCP service.
type Sender interface {
	Accounts() []string
	SendText(ctx context.Context, accountID, chatJID, text string, reply *store.Message, mentions []string) (store.Message, error)
	SendMedia(ctx context.Context, accountID, chatJID, path, caption string, reply *store.Message) (store.Message, error)
}

var (
	_ Store  = (*store.DB)(nil)
	_ Sender = (*whatsapp.Manager)(nil)
)

// ToolDef describes an MCP tool exposed by Service.
type ToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// TextContent is one text block inside a ToolResult.
type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolResult is the MCP tools/call response payload.
type ToolResult struct {
	Content []TextContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type pendingSend struct {
	Account   string
	ChatJID   string
	Text      string
	MediaPath string
	Caption   string
	ExpiresAt time.Time
}

// Service implements Wisp's MCP tools on top of a Store and Sender.
type Service struct {
	st  Store
	wa  Sender
	now func() time.Time

	mu      sync.Mutex
	pending map[string]pendingSend
}

// NewService creates a Service backed by st and wa.
func NewService(st Store, wa Sender) *Service {
	return &Service{
		st:      st,
		wa:      wa,
		now:     time.Now,
		pending: make(map[string]pendingSend),
	}
}

// Tools returns the MCP tool definitions supported by Service.
func (s *Service) Tools() []ToolDef {
	return []ToolDef{
		{
			Name:        "list_accounts",
			Description: "List all registered WhatsApp account IDs in Wisp.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "search_chats_and_contacts",
			Description: "Search WhatsApp chats, groups, and contacts by name, phone number, or JID across one or all accounts.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Case-insensitive search text matching name, phone number, or JID. Empty returns recent chats and contacts.",
					},
					"account": map[string]any{
						"type":        "string",
						"description": "Optional WhatsApp account ID to filter results to.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of results to return (default 50, max 200).",
					},
				},
			},
		},
		{
			Name:        "get_chat_history",
			Description: "Read recent messages (newest first) from a WhatsApp chat, including media metadata.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"chat_jid"},
				"properties": map[string]any{
					"chat_jid": map[string]any{
						"type":        "string",
						"description": "Chat or group JID (e.g. 15551234567@s.whatsapp.net or 120363000111@g.us).",
					},
					"account": map[string]any{
						"type":        "string",
						"description": "Optional WhatsApp account ID. Required if multiple accounts share the chat.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of messages to return (default 20, max 100).",
					},
				},
			},
		},
		{
			Name:        "prepare_send",
			Description: "Stage a text or media message for sending. Resolves recipient and returns a short-lived confirm_token or an ambiguous candidate list without sending.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"recipient"},
				"properties": map[string]any{
					"recipient": map[string]any{
						"type":        "string",
						"description": "Recipient name, phone number, or exact JID.",
					},
					"text": map[string]any{
						"type":        "string",
						"description": "Text message body (or caption if media_path is provided and caption is omitted).",
					},
					"media_path": map[string]any{
						"type":        "string",
						"description": "Local file path of an image or document to send.",
					},
					"caption": map[string]any{
						"type":        "string",
						"description": "Optional caption when sending media_path.",
					},
					"account": map[string]any{
						"type":        "string",
						"description": "Optional WhatsApp account ID to send from.",
					},
				},
			},
		},
		{
			Name:        "confirm_send",
			Description: "Execute a previously prepared send using its single-use confirm_token after explicit user confirmation.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"confirm_token"},
				"properties": map[string]any{
					"confirm_token": map[string]any{
						"type":        "string",
						"description": "Single-use confirmation token returned by prepare_send.",
					},
				},
			},
		},
	}
}

// CallTool dispatches an MCP tool call by name.
func (s *Service) CallTool(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
	switch name {
	case "list_accounts":
		return s.callListAccounts(), nil
	case "search_chats_and_contacts":
		return s.callSearchChatsAndContacts(args), nil
	case "get_chat_history":
		return s.callGetChatHistory(args), nil
	case "prepare_send":
		return s.callPrepareSend(args), nil
	case "confirm_send":
		return s.callConfirmSend(ctx, args), nil
	default:
		msg := fmt.Sprintf("unknown tool %q", name)
		return errResult(msg), fmt.Errorf("mcp: %s", msg)
	}
}

func (s *Service) callListAccounts() ToolResult {
	accounts := s.wa.Accounts()
	if accounts == nil {
		accounts = []string{}
	}
	return jsonResult(map[string]any{"accounts": accounts})
}

func (s *Service) callSearchChatsAndContacts(raw json.RawMessage) ToolResult {
	var args struct {
		Query   string `json:"query"`
		Account string `json:"account"`
		Limit   int    `json:"limit"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return errResult("invalid arguments: " + err.Error())
	}

	accounts, err := s.targetAccounts(args.Account)
	if err != nil {
		return errResult(err.Error())
	}

	limit := args.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	} else if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	entries, err := s.collectDirectory(accounts)
	if err != nil {
		return errResult(err.Error())
	}

	q := strings.TrimSpace(args.Query)
	results := make([]directoryEntry, 0)
	for _, e := range entries {
		if matchesQuery(e, q) {
			results = append(results, e)
			if len(results) >= limit {
				break
			}
		}
	}

	return jsonResult(map[string]any{"results": results})
}

func (s *Service) callGetChatHistory(raw json.RawMessage) ToolResult {
	var args struct {
		ChatJID string `json:"chat_jid"`
		Account string `json:"account"`
		Limit   int    `json:"limit"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return errResult("invalid arguments: " + err.Error())
	}
	chatJID := strings.TrimSpace(args.ChatJID)
	if chatJID == "" {
		return errResult("chat_jid is required")
	}

	account, err := s.resolveAccountForChat(args.Account, chatJID)
	if err != nil {
		return errResult(err.Error())
	}

	limit := args.Limit
	if limit <= 0 {
		limit = defaultHistoryLimit
	} else if limit > maxHistoryLimit {
		limit = maxHistoryLimit
	}

	var msgs []store.Message
	seen := make(map[string]bool)
	for page := 0; len(msgs) < limit; page++ {
		batch, err := s.st.GetChatMessages(account, chatJID, page)
		if err != nil {
			return errResult(fmt.Sprintf("load chat messages: %v", err))
		}
		if len(batch) == 0 {
			break
		}
		added := 0
		for _, m := range batch {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			msgs = append(msgs, m)
			added++
			if len(msgs) >= limit {
				break
			}
		}
		if added == 0 {
			break
		}
	}

	people, err := s.st.People(account)
	if err != nil {
		return errResult(fmt.Sprintf("load contacts: %v", err))
	}
	names := buildPersonNameMap(people)

	type historyMessage struct {
		ID            string `json:"id"`
		SenderJID     string `json:"sender_jid"`
		SenderName    string `json:"sender_name"`
		FromMe        bool   `json:"from_me"`
		Timestamp     string `json:"timestamp"`
		TimestampUnix int64  `json:"timestamp_unix"`
		Type          string `json:"type"`
		Body          string `json:"body"`
		MediaFilename string `json:"media_filename"`
		MediaMime     string `json:"media_mime"`
		Caption       string `json:"caption"`
	}

	out := make([]historyMessage, 0, len(msgs))
	for _, m := range msgs {
		senderName := resolveSenderName(m, names)
		filename, mimeType, caption := extractMediaMeta(m)
		ts := ""
		if m.Timestamp != 0 {
			ts = time.Unix(m.Timestamp, 0).UTC().Format(time.RFC3339)
		}
		out = append(out, historyMessage{
			ID:            m.ID,
			SenderJID:     m.SenderJID,
			SenderName:    senderName,
			FromMe:        m.FromMe,
			Timestamp:     ts,
			TimestampUnix: m.Timestamp,
			Type:          m.Type,
			Body:          m.Body,
			MediaFilename: filename,
			MediaMime:     mimeType,
			Caption:       caption,
		})
	}

	return jsonResult(map[string]any{
		"account":  account,
		"chat_jid": chatJID,
		"order":    "newest_first",
		"messages": out,
	})
}

func (s *Service) callPrepareSend(raw json.RawMessage) ToolResult {
	var args struct {
		Recipient string `json:"recipient"`
		Text      string `json:"text"`
		MediaPath string `json:"media_path"`
		Caption   string `json:"caption"`
		Account   string `json:"account"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return errResult("invalid arguments: " + err.Error())
	}

	recipient := strings.TrimSpace(args.Recipient)
	if recipient == "" {
		return errResult("recipient is required")
	}

	mediaPath := strings.TrimSpace(args.MediaPath)
	caption := strings.TrimSpace(args.Caption)
	if mediaPath != "" {
		info, err := os.Stat(mediaPath)
		if err != nil {
			return errResult(fmt.Sprintf("invalid media_path %q: %v", mediaPath, err))
		}
		if !info.Mode().IsRegular() {
			return errResult(fmt.Sprintf("invalid media_path %q: must be a regular file", mediaPath))
		}
		if caption == "" {
			caption = strings.TrimSpace(args.Text)
		}
	} else if strings.TrimSpace(args.Text) == "" {
		return errResult("either text or media_path is required")
	}

	accounts, err := s.targetAccounts(args.Account)
	if err != nil {
		return errResult(err.Error())
	}

	match, candidates, err := s.resolveRecipient(accounts, recipient)
	if err != nil {
		return errResult(err.Error())
	}
	if len(candidates) > 1 {
		return jsonResult(map[string]any{
			"status":     "ambiguous",
			"query":      recipient,
			"candidates": candidates,
			"message":    fmt.Sprintf("Multiple contacts or groups match %q. Ask the user to choose one and call prepare_send again with the exact JID (and account if needed).", recipient),
		})
	}

	token, err := randomHexToken(16)
	if err != nil {
		return errResult(fmt.Sprintf("generate confirmation token: %v", err))
	}

	now := s.now()
	expiresAt := now.Add(confirmTTL)
	pending := pendingSend{
		Account:   match.Account,
		ChatJID:   match.JID,
		Text:      args.Text,
		MediaPath: mediaPath,
		Caption:   caption,
		ExpiresAt: expiresAt,
	}

	s.mu.Lock()
	for k, v := range s.pending {
		if now.After(v.ExpiresAt) {
			delete(s.pending, k)
		}
	}
	s.pending[token] = pending
	s.mu.Unlock()

	resp := map[string]any{
		"status":        "needs_confirmation",
		"confirm_token": token,
		"expires_at":    expiresAt.UTC().Format(time.RFC3339),
		"account":       match.Account,
		"recipient": map[string]any{
			"jid":  match.JID,
			"name": match.Name,
			"type": match.Type,
		},
	}
	if mediaPath != "" {
		resp["media_path"] = mediaPath
		if caption != "" {
			resp["caption"] = caption
		}
	} else {
		resp["text"] = args.Text
	}
	return jsonResult(resp)
}

func (s *Service) callConfirmSend(ctx context.Context, raw json.RawMessage) ToolResult {
	var args struct {
		ConfirmToken string `json:"confirm_token"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return errResult("invalid arguments: " + err.Error())
	}
	token := strings.TrimSpace(args.ConfirmToken)
	if token == "" {
		return errResult("confirm_token is required")
	}

	s.mu.Lock()
	pending, ok := s.pending[token]
	if ok {
		delete(s.pending, token)
	}
	s.mu.Unlock()

	if !ok {
		return errResult("unknown or already used confirmation token; call prepare_send first")
	}
	if s.now().After(pending.ExpiresAt) {
		return errResult("confirmation token has expired; call prepare_send again")
	}

	var (
		sent store.Message
		err  error
	)
	if pending.MediaPath != "" {
		sent, err = s.wa.SendMedia(ctx, pending.Account, pending.ChatJID, pending.MediaPath, pending.Caption, nil)
	} else {
		sent, err = s.wa.SendText(ctx, pending.Account, pending.ChatJID, pending.Text, nil, nil)
	}
	if err != nil {
		return errResult(fmt.Sprintf("send failed: %v", err))
	}

	ts := ""
	if sent.Timestamp != 0 {
		ts = time.Unix(sent.Timestamp, 0).UTC().Format(time.RFC3339)
	}
	return jsonResult(map[string]any{
		"status":     "sent",
		"account":    pending.Account,
		"chat_jid":   pending.ChatJID,
		"message_id": sent.ID,
		"timestamp":  ts,
		"type":       sent.Type,
		"body":       sent.Body,
	})
}

func (s *Service) targetAccounts(requested string) ([]string, error) {
	accounts := s.wa.Accounts()
	req := strings.TrimSpace(requested)
	if req != "" {
		if !slices.Contains(accounts, req) {
			return nil, fmt.Errorf("unknown account %q", req)
		}
		return []string{req}, nil
	}
	if len(accounts) == 0 {
		return nil, errors.New("no WhatsApp accounts are connected")
	}
	return accounts, nil
}

func (s *Service) resolveAccountForChat(requested, chatJID string) (string, error) {
	accounts, err := s.targetAccounts(requested)
	if err != nil {
		return "", err
	}
	if len(accounts) == 1 {
		return accounts[0], nil
	}
	var matched []string
	for _, acct := range accounts {
		chats, err := s.st.ListChats(acct)
		if err != nil {
			return "", fmt.Errorf("list chats for %s: %w", acct, err)
		}
		found := false
		for _, c := range chats {
			if c.JID == chatJID {
				found = true
				break
			}
		}
		if !found {
			msgs, err := s.st.GetChatMessages(acct, chatJID, 0)
			if err != nil {
				return "", fmt.Errorf("check chat messages for %s: %w", acct, err)
			}
			found = len(msgs) > 0
		}
		if found {
			matched = append(matched, acct)
		}
	}
	switch len(matched) {
	case 1:
		return matched[0], nil
	case 0:
		return "", fmt.Errorf("multiple accounts exist and chat %q was not found; specify account", chatJID)
	default:
		return "", fmt.Errorf("chat %q exists in multiple accounts (%s); specify account", chatJID, strings.Join(matched, ", "))
	}
}

type directoryEntry struct {
	Account       string `json:"account"`
	JID           string `json:"jid"`
	PNJID         string `json:"pn_jid,omitempty"`
	Name          string `json:"name"`
	FullName      string `json:"-"`
	PushName      string `json:"push_name,omitempty"`
	ChatName      string `json:"-"`
	AltJID        string `json:"-"`
	Type          string `json:"type"`
	LastMessageTS int64  `json:"last_message_ts,omitempty"`
	Unread        int    `json:"unread,omitempty"`
}

func (s *Service) collectDirectory(accounts []string) ([]directoryEntry, error) {
	var all []directoryEntry
	for _, acct := range accounts {
		chats, err := s.st.ListChats(acct)
		if err != nil {
			return nil, fmt.Errorf("list chats for %s: %w", acct, err)
		}
		people, err := s.st.People(acct)
		if err != nil {
			return nil, fmt.Errorf("list contacts for %s: %w", acct, err)
		}

		byJID := make(map[string]*directoryEntry)
		var order []string
		getOrCreate := func(jid string) *directoryEntry {
			if e, ok := byJID[jid]; ok {
				return e
			}
			e := &directoryEntry{Account: acct, JID: jid}
			byJID[jid] = e
			order = append(order, jid)
			return e
		}

		knownJIDs := make(map[string]bool, len(chats)+len(people))
		for _, c := range chats {
			if c.JID == "" || c.JID == statusBroadcastJID {
				continue
			}
			knownJIDs[c.JID] = true
			e := getOrCreate(c.JID)
			if c.Name != "" {
				e.ChatName = c.Name
			}
			e.LastMessageTS = c.LastMessageTS
			e.Unread = c.Unread
		}
		for _, p := range people {
			if p.JID != "" {
				knownJIDs[p.JID] = true
			}
		}

		for _, p := range people {
			if p.JID == "" {
				continue
			}
			key := p.JID
			if p.PNJID != "" && p.PNJID != p.JID && knownJIDs[p.PNJID] {
				key = p.PNJID
			}
			e := getOrCreate(key)
			if key != p.JID {
				e.AltJID = p.JID
			}
			if p.PNJID != "" && e.PNJID == "" {
				e.PNJID = p.PNJID
			}
			if p.FullName != "" {
				e.FullName = p.FullName
			}
			if p.PushName != "" {
				e.PushName = p.PushName
			}
		}

		for _, jid := range order {
			e := byJID[jid]
			e.Name = cmp.Or(e.FullName, e.ChatName, e.PushName)
			e.Type = entryType(e.JID)
			all = append(all, *e)
		}
	}
	return all, nil
}

func (s *Service) resolveRecipient(accounts []string, recipient string) (directoryEntry, []directoryEntry, error) {
	entries, err := s.collectDirectory(accounts)
	if err != nil {
		return directoryEntry{}, nil, err
	}

	var exact []directoryEntry
	for _, e := range entries {
		if strings.EqualFold(e.JID, recipient) ||
			(e.PNJID != "" && strings.EqualFold(e.PNJID, recipient)) ||
			(e.AltJID != "" && strings.EqualFold(e.AltJID, recipient)) {
			exact = append(exact, e)
		}
	}
	if len(exact) == 1 {
		return exact[0], nil, nil
	}
	if len(exact) > 1 {
		return directoryEntry{}, exact, nil
	}

	var matches []directoryEntry
	for _, e := range entries {
		if matchesQuery(e, recipient) {
			matches = append(matches, e)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil, nil
	}
	if len(matches) > 1 {
		return directoryEntry{}, matches, nil
	}

	if parsed, err := types.ParseJID(recipient); err == nil && parsed.User != "" && parsed.Server != "" {
		if len(accounts) == 1 {
			jid := parsed.ToNonAD().String()
			return directoryEntry{
				Account: accounts[0],
				JID:     jid,
				Name:    jid,
				Type:    entryType(jid),
			}, nil, nil
		}
		return directoryEntry{}, nil, fmt.Errorf("recipient JID %q not found in local chats/contacts and multiple accounts exist; specify account", recipient)
	}

	return directoryEntry{}, nil, fmt.Errorf("no matching chat or contact found for %q", recipient)
}

func matchesQuery(e directoryEntry, q string) bool {
	if q == "" {
		return true
	}
	ql := strings.ToLower(q)
	for _, field := range []string{e.Name, e.FullName, e.ChatName, e.PushName, e.JID, e.PNJID, e.AltJID} {
		if field != "" && strings.Contains(strings.ToLower(field), ql) {
			return true
		}
	}
	if isPhoneLike(q) {
		qDigits := digitsOnly(q)
		if len(qDigits) >= 3 {
			for _, jid := range []string{e.JID, e.PNJID} {
				if jid != "" && strings.Contains(digitsOnly(jidUser(jid)), qDigits) {
					return true
				}
			}
		}
	}
	return false
}

func isPhoneLike(s string) bool {
	digits := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '+' || r == '-' || r == ' ' || r == '(' || r == ')':
		default:
			return false
		}
	}
	return digits >= 3
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func jidUser(jid string) string {
	user, _, _ := strings.Cut(jid, "@")
	user, _, _ = strings.Cut(user, ":")
	return user
}

func entryType(jid string) string {
	if strings.HasSuffix(jid, "@g.us") {
		return "group"
	}
	return "contact"
}

func buildPersonNameMap(people []store.Person) map[string]string {
	m := make(map[string]string, len(people)*2)
	for _, p := range people {
		name := cmp.Or(p.FullName, p.PushName)
		if name == "" {
			continue
		}
		if p.JID != "" {
			m[p.JID] = name
		}
		if p.PNJID != "" && m[p.PNJID] == "" {
			m[p.PNJID] = name
		}
	}
	return m
}

func resolveSenderName(m store.Message, names map[string]string) string {
	if m.SenderJID != "" {
		if name := names[m.SenderJID]; name != "" {
			return name
		}
		if clean := cleanJID(m.SenderJID); clean != m.SenderJID {
			if name := names[clean]; name != "" {
				return name
			}
		}
	}
	if m.FromMe {
		return "You"
	}
	return ""
}

func cleanJID(jid string) string {
	user, server, ok := strings.Cut(jid, "@")
	if !ok {
		return jid
	}
	user, _, _ = strings.Cut(user, ":")
	return user + "@" + server
}

func extractMediaMeta(m store.Message) (filename, mimeType, caption string) {
	switch m.Type {
	case whatsapp.TypeImage:
		if len(m.MediaMeta) > 0 {
			var x waE2E.ImageMessage
			if proto.Unmarshal(m.MediaMeta, &x) == nil {
				mimeType = x.GetMimetype()
				caption = x.GetCaption()
			}
		}
		caption = cmp.Or(caption, m.Body)
	case whatsapp.TypeVideo:
		if len(m.MediaMeta) > 0 {
			var x waE2E.VideoMessage
			if proto.Unmarshal(m.MediaMeta, &x) == nil {
				mimeType = x.GetMimetype()
				caption = x.GetCaption()
			}
		}
		caption = cmp.Or(caption, m.Body)
	case whatsapp.TypeAudio:
		if len(m.MediaMeta) > 0 {
			var x waE2E.AudioMessage
			if proto.Unmarshal(m.MediaMeta, &x) == nil {
				mimeType = x.GetMimetype()
			}
		}
	case whatsapp.TypeDocument:
		if len(m.MediaMeta) > 0 {
			var x waE2E.DocumentMessage
			if proto.Unmarshal(m.MediaMeta, &x) == nil {
				filename = x.GetFileName()
				mimeType = x.GetMimetype()
				caption = x.GetCaption()
			}
		}
		if filename == "" && m.MediaPath != "" {
			filename = filepath.Base(m.MediaPath)
		}
	case whatsapp.TypeSticker:
		if len(m.MediaMeta) > 0 {
			var x waE2E.StickerMessage
			if proto.Unmarshal(m.MediaMeta, &x) == nil {
				mimeType = x.GetMimetype()
			}
		}
	}
	return filename, mimeType, caption
}

func unmarshalArgs(raw json.RawMessage, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		trimmed = []byte("{}")
	}
	return json.Unmarshal(trimmed, dst)
}

func randomHexToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func jsonResult(v any) ToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return errResult(fmt.Sprintf("marshal result: %v", err))
	}
	return ToolResult{
		Content: []TextContent{{Type: "text", Text: string(b)}},
	}
}

func errResult(msg string) ToolResult {
	return ToolResult{
		Content: []TextContent{{Type: "text", Text: msg}},
		IsError: true,
	}
}
