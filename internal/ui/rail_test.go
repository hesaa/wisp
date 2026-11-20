package ui

import "testing"

func TestRailModelChatsUnread(t *testing.T) {
	m := NewRailModel()
	if got := m.ChatsUnread(); got != 0 {
		t.Fatalf("ChatsUnread() = %d, want 0", got)
	}

	redrawn := 0
	m.redraw = func() { redrawn++ }

	m.SetChatsUnread(3)
	if got := m.ChatsUnread(); got != 3 {
		t.Fatalf("ChatsUnread() = %d, want 3", got)
	}
	if redrawn != 1 {
		t.Fatalf("redrawn = %d, want 1", redrawn)
	}

	// Setting same value does not redraw
	m.SetChatsUnread(3)
	if redrawn != 1 {
		t.Fatalf("redrawn = %d on no-op, want 1", redrawn)
	}

	m.SetChatsUnread(0)
	if got := m.ChatsUnread(); got != 0 {
		t.Fatalf("ChatsUnread() = %d, want 0", got)
	}
	if redrawn != 2 {
		t.Fatalf("redrawn = %d, want 2", redrawn)
	}
}
