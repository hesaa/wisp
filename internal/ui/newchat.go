package ui

import (
	"errors"
	"strings"

	"github.com/lxn/walk"
	"github.com/lxn/win"

	"wa-go/internal/store"
	"wa-go/internal/theme"
	"wa-go/internal/whatsapp"
)

// RunNewChat shows the modal New Chat picker over owner: the account's chats and address-book
// contacts filtered as the user types, plus a "start a new chat" row when the input is a phone
// number. Clicking a row, Enter on the focused row, or Enter in the search box (the first visible
// row) calls onPick with its chat JID; Down moves from the search box into the list; Esc or closing
// calls nothing. UI-thread only.
func RunNewChat(owner walk.Form, p theme.Palette, chats []store.Chat, contacts []whatsapp.Contact, onPick func(jid string), d ...*Directory) error {
	var dir *Directory
	if len(d) > 0 {
		dir = d[0]
	}
	dlg, err := walk.NewDialog(owner)
	if err != nil {
		return err
	}
	defer dlg.Dispose()
	if err := errors.Join(setBg(dlg, p.Sidebar), dlg.SetTitle("New chat"), setBox(dlg, walk.NewVBoxLayout(), walk.Margins{VNear: 8}),
		dlg.SetMinMaxSize(walk.Size{Width: 360, Height: 520}, walk.Size{})); err != nil {
		return err
	}
	pal := p
	search, err := newSearchBox(dlg, &pal, "Search name or number")
	if err != nil {
		return err
	}
	list, err := newChatList(dlg, &pal, "Contacts")
	if err != nil {
		return err
	}
	list.dir = dir
	if err := fillRest(dlg, list); err != nil {
		return err
	}
	// walk routes Esc (IDCANCEL) only to a cancel button; this one stays hidden
	cancel, err := walk.NewPushButton(dlg)
	if err != nil {
		return err
	}
	cancel.SetVisible(false)
	cancel.Clicked().Attach(dlg.Cancel)
	if err := errors.Join(dlg.SetCancelButton(cancel), search.applyPalette()); err != nil {
		return err
	}

	cands := newChatCandidates(chats, contacts)
	show := func() { list.setRows(newChatRows(cands, search.edit.Text(), dir), "", true) }
	show()
	search.edit.TextChanged().Attach(show)
	picked := ""
	pick := func(jid string) {
		if jid == "" {
			return // the country-code hint
		}
		picked = jid
		dlg.Accept()
	}
	list.onClick = pick
	searchKeys(search.edit, list)
	dlg.Starting().Attach(func() { focusOn(search.edit) })
	// focus cues follow the user's setting and last input (hidden after a click until a key is used)
	dlg.SendMessage(win.WM_CHANGEUISTATE, win.UIS_INITIALIZE, 0)
	// cosmetic: Windows builds without dark title bars keep a light one
	logErr("new chat title bar", theme.ApplyTitleBar(uintptr(dlg.Handle()), isDark(p)))

	if dlg.Run() == walk.DlgCmdOK && picked != "" {
		onPick(picked)
	}
	return nil
}

func isDark(p theme.Palette) bool {
	c := theme.Color(p.Sidebar)
	return int(c.R())*299+int(c.G())*587+int(c.B())*114 < 128*1000
}

// newChatCandidates merges chats and contacts into picker rows, chats first, one row per JID, with
// the phone number as the second line of named rows.
func newChatCandidates(chats []store.Chat, contacts []whatsapp.Contact) []store.Chat {
	out := make([]store.Chat, 0, len(chats)+len(contacts))
	at := make(map[string]int, cap(out))
	add := func(jid, name string) {
		if i, ok := at[jid]; ok {
			if out[i].Name == "" {
				out[i].Name = name
			}
			return
		}
		at[jid] = len(out)
		out = append(out, store.Chat{JID: jid, Name: name})
	}
	for _, c := range chats {
		add(c.JID, c.Name)
	}
	for _, c := range contacts {
		add(c.JID, c.Name)
	}
	for i, c := range out {
		if user, server, _ := strings.Cut(c.JID, "@"); c.Name != "" && server == "s.whatsapp.net" {
			out[i].Snippet = "+" + user
		}
	}
	return out
}

// newChatRows filters candidates by name or number (ignoring case). When the query is a phone number
// that no candidate has, a row to start a chat with it comes first.
func newChatRows(cands []store.Chat, query string, d ...*Directory) []store.Chat {
	var dir *Directory
	if len(d) > 0 {
		dir = d[0]
	}
	q := strings.ToLower(strings.TrimSpace(query))
	digits, numeric := phoneDigits(query)
	jid, isPhone := phoneJID(query)
	var out []store.Chat
	known := false
	for _, c := range cands {
		user, _, _ := strings.Cut(c.JID, "@")
		known = known || c.JID == jid
		if strings.Contains(strings.ToLower(dir.Name(c.JID, c.Name)), q) || strings.Contains(c.Snippet, q) ||
			numeric && digits != "" && strings.Contains(user, digits) {
			out = append(out, c)
		}
	}
	switch {
	case isPhone && !known:
		out = append([]store.Chat{{JID: jid, Snippet: "Start a new chat"}}, out...) // unnamed: shows as +number
	case numeric && len(digits) >= 7 && digits[0] == '0': // no JID: nothing to pick
		out = append([]store.Chat{{Name: strings.TrimSpace(query), Snippet: "Include the country code, e.g. 62812…"}}, out...)
	}
	return out
}

// phoneDigits returns the digits of s with spaces, +, -, and parentheses dropped; numeric is false
// when s holds anything else.
func phoneDigits(s string) (digits string, numeric bool) {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case strings.ContainsRune(" +-()", r):
		default:
			return "", false
		}
	}
	return b.String(), true
}

// phoneJID turns a typed phone number into a user JID: 7 to 15 digits once separators are dropped,
// country code first (a leading 0 is a local number, which WhatsApp cannot deliver to).
func phoneJID(s string) (string, bool) {
	d, ok := phoneDigits(s)
	if !ok || len(d) < 7 || len(d) > 15 || d[0] == '0' {
		return "", false
	}
	return d + "@s.whatsapp.net", true
}
