package ui

import (
	"slices"
	"strings"
	"testing"

	"wa-go/internal/store"
	"wa-go/internal/theme"
)

func TestInfoRowsContact(t *testing.T) {
	d := NewDirectory([]store.Person{
		{JID: "628123456789@s.whatsapp.net", PNJID: "628123456789@s.whatsapp.net", FullName: "Alice Smith"},
		{JID: "hidden@lid", FullName: "Hidden Contact"},
	})

	chat := store.Chat{JID: "628123456789@s.whatsapp.net", Name: "Alice Smith"}

	// 1. Full details: title, name, phone, about
	rows := infoRows(chat, d, "Hey there!", nil, false, false)
	kinds := map[rowKind]bool{}
	var texts []string
	for _, r := range rows {
		kinds[r.kind] = true
		texts = append(texts, r.text)
	}

	if !kinds[rowTitle] || rows[0].text != "Contact info" {
		t.Errorf("expected title 'Contact info', got %v", rows[0].text)
	}
	if !slices.Contains(texts, "Alice Smith") {
		t.Errorf("expected name 'Alice Smith' in rows: %v", texts)
	}
	if !slices.Contains(texts, "+628123456789") {
		t.Errorf("expected phone '+628123456789' in rows: %v", texts)
	}
	if !slices.Contains(texts, "About") || !slices.Contains(texts, "Hey there!") {
		t.Errorf("expected About section and text in rows: %v", texts)
	}

	// 2. Hidden number and empty about: neither phone line nor about section
	chatHidden := store.Chat{JID: "hidden@lid", Name: "Hidden Contact"}
	rowsHidden := infoRows(chatHidden, d, "", nil, false, false)
	for _, r := range rowsHidden {
		if r.kind == rowPhone {
			t.Errorf("unexpected phone line in hidden contact: %v", r.text)
		}
		if r.kind == rowSection && r.text == "About" {
			t.Errorf("unexpected About section in empty about contact")
		}
	}

	// 3. Loading
	rowsLoading := infoRows(chat, d, "", nil, true, false)
	hasLoading := false
	for _, r := range rowsLoading {
		if r.kind == rowLoading && strings.Contains(r.text, "Loading") {
			hasLoading = true
		}
	}
	if !hasLoading {
		t.Errorf("expected loading row, got rows: %v", rowsLoading)
	}

	// 4. Failed
	rowsFailed := infoRows(chat, d, "", nil, false, true)
	hasFailed := false
	for _, r := range rowsFailed {
		if r.kind == rowError && strings.Contains(r.text, "Couldn't load the details.") {
			hasFailed = true
		}
	}
	if !hasFailed {
		t.Errorf("expected error row with retry, got rows: %v", rowsFailed)
	}
}

func TestInfoRowsGroup(t *testing.T) {
	d := NewDirectory([]store.Person{
		{JID: "111@s.whatsapp.net", FullName: "Charlie"},
		{JID: "222@s.whatsapp.net", FullName: "Alice"},
		{JID: "333@s.whatsapp.net", FullName: "Bob"},
	})

	g := &GroupInfo{
		Description: "Our project discussion",
		Members: []InfoMember{
			{JID: "111@s.whatsapp.net", Admin: false},
			{JID: "222@s.whatsapp.net", Admin: true},
			{JID: "333@s.whatsapp.net", Admin: false},
		},
	}

	chat := store.Chat{JID: "group1@g.us", Name: "Team Alpha"}
	rows := infoRows(chat, d, "", g, false, false)

	if len(rows) == 0 || rows[0].kind != rowTitle || rows[0].text != "Group info" {
		t.Fatalf("expected title 'Group info', got %+v", rows)
	}

	var hasSubject, hasDesc, hasCount bool
	var memberRows []infoRow
	for _, r := range rows {
		if r.text == "Team Alpha" {
			hasSubject = true
		}
		if r.text == "Description" {
			hasDesc = true
		}
		if r.kind == rowMemberCount && r.text == "3 members" {
			hasCount = true
		}
		if r.kind == rowMember {
			memberRows = append(memberRows, r)
		}
	}

	if !hasSubject {
		t.Errorf("missing group subject Team Alpha")
	}
	if !hasDesc {
		t.Errorf("missing Description section")
	}
	if !hasCount {
		t.Errorf("missing '3 members' row")
	}
	if len(memberRows) != 3 {
		t.Fatalf("got %d member rows, want 3", len(memberRows))
	}

	// Members sorted: Alice (admin), Bob, Charlie
	if memberRows[0].text != "Alice" || !memberRows[0].admin {
		t.Errorf("first member should be admin Alice, got %+v", memberRows[0])
	}
	if memberRows[1].text != "Bob" {
		t.Errorf("second member should be Bob, got %+v", memberRows[1])
	}
	if memberRows[2].text != "Charlie" {
		t.Errorf("third member should be Charlie, got %+v", memberRows[2])
	}
}

func TestSortMembers(t *testing.T) {
	d := NewDirectory([]store.Person{
		{JID: "1@s.whatsapp.net", FullName: "david"},
		{JID: "2@s.whatsapp.net", FullName: "Bob"},
		{JID: "3@s.whatsapp.net", FullName: "alice"},
		{JID: "4@s.whatsapp.net", FullName: "Charlie"},
	})

	members := []InfoMember{
		{JID: "1@s.whatsapp.net", Admin: false}, // david
		{JID: "2@s.whatsapp.net", Admin: true},  // Bob (admin)
		{JID: "3@s.whatsapp.net", Admin: false}, // alice
		{JID: "4@s.whatsapp.net", Admin: true},  // Charlie (admin)
	}

	sorted := sortMembers(members, d)
	// Admins first: Bob, Charlie (alphabetical case-insensitive)
	// Non-admins: alice, david
	want := []string{"Bob", "Charlie", "alice", "david"}
	for i, m := range sorted {
		name := d.Name(m.JID, "")
		if name != want[i] {
			t.Errorf("sorted[%d] = %q, want %q", i, name, want[i])
		}
	}
	if !sorted[0].Admin || !sorted[1].Admin || sorted[2].Admin || sorted[3].Admin {
		t.Errorf("admin flags incorrect in sorted members: %+v", sorted)
	}
}

func TestInfoViewIgnoresOtherChat(t *testing.T) {
	v := &InfoView{pal: theme.GetPalette(theme.ThemeDark)}
	v.Open(store.Chat{JID: "chatA@s.whatsapp.net", Name: "Chat A"}, nil)

	// Calls for chatB should change nothing
	v.SetAbout("chatB@s.whatsapp.net", "About B")
	if v.about != "" {
		t.Errorf("about changed for other chat: %q", v.about)
	}
	v.SetGroup("groupB@g.us", GroupInfo{Description: "Desc B"})
	if v.group != nil {
		t.Errorf("group changed for other chat")
	}
	v.SetError("chatB@s.whatsapp.net")
	if v.failed {
		t.Errorf("failed set for other chat")
	}

	// After Close, calls for chatA should change nothing
	v.Close()
	v.SetAbout("chatA@s.whatsapp.net", "About A")
	if v.about != "" {
		t.Errorf("about changed after Close: %q", v.about)
	}
}
