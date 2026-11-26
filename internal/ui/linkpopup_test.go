package ui

import (
	"testing"

	"github.com/lxn/walk"

	"wa-go/internal/ui/icons"
)

func TestLinkPopupItems(t *testing.T) {
	// Link mode: Open link + Copy link
	lm := newLinkPopupModel("https://example.com/docs")
	if len(lm.items) != 2 {
		t.Fatalf("link mode items = %d, want 2", len(lm.items))
	}
	if lm.items[0].label != "Open link" || lm.items[0].icon != icons.IconExternal || lm.items[0].disabled {
		t.Errorf("link item 0 = %+v, want Open link with IconExternal", lm.items[0])
	}
	if lm.items[1].label != "Copy link" || lm.items[1].icon != icons.IconCopy || lm.items[1].disabled {
		t.Errorf("link item 1 = %+v, want Copy link with IconCopy", lm.items[1])
	}
	if lm.cur != 0 {
		t.Errorf("link initial cur = %d, want 0", lm.cur)
	}
	if it, ok := lm.activate(); !ok || it.action != popupOpenLink || it.url != "https://example.com/docs" {
		t.Errorf("activate(0) = %+v, %v", it, ok)
	}
	lm.move(1)
	if lm.cur != 1 {
		t.Errorf("after move(1), cur = %d, want 1", lm.cur)
	}
	if it, ok := lm.activate(); !ok || it.action != popupCopyLink || it.url != "https://example.com/docs" {
		t.Errorf("activate(1) = %+v, %v", it, ok)
	}

	// Phone mode: starts with disabled Checking WhatsApp... + Copy phone number
	pm := newPhonePopupModel("+62 882-0007-83234")
	if len(pm.items) != 2 {
		t.Fatalf("phone initial items = %d, want 2", len(pm.items))
	}
	if pm.items[0].label != "Checking WhatsApp..." || !pm.items[0].disabled {
		t.Errorf("phone item 0 = %+v, want disabled Checking WhatsApp...", pm.items[0])
	}
	if pm.items[1].label != "Copy phone number" || pm.items[1].icon != icons.IconCopy || pm.items[1].disabled {
		t.Errorf("phone item 1 = %+v, want Copy phone number", pm.items[1])
	}
	if pm.cur != 1 {
		t.Errorf("phone initial cur = %d, want 1 (skip disabled row)", pm.cur)
	}
	pm.move(-1)
	if pm.cur != 1 {
		t.Errorf("move(-1) landed on disabled row: cur = %d", pm.cur)
	}

	// Registered = true inserts Chat with <display>
	if !pm.applyPhoneCheck(true, "+62 882-0007-83234", "62882000783234@s.whatsapp.net") {
		t.Fatal("applyPhoneCheck(true) returned false")
	}
	if len(pm.items) != 2 {
		t.Fatalf("registered phone items = %d, want 2", len(pm.items))
	}
	if pm.items[0].label != "Chat with +62 882-0007-83234" || pm.items[0].disabled || pm.items[0].icon != icons.IconChat {
		t.Errorf("registered item 0 = %+v", pm.items[0])
	}
	if pm.cur != 0 {
		t.Errorf("registered cur = %d, want 0", pm.cur)
	}
	if it, ok := pm.activate(); !ok || it.action != popupChatWith || it.jid != "62882000783234@s.whatsapp.net" {
		t.Errorf("activate Chat with = %+v, %v", it, ok)
	}

	// Registered = false or error removes Checking row leaving only Copy phone number
	pmUnreg := newPhonePopupModel("08123456789")
	if !pmUnreg.applyPhoneCheck(false, "", "") {
		t.Fatal("applyPhoneCheck(false) returned false")
	}
	if len(pmUnreg.items) != 1 || pmUnreg.items[0].label != "Copy phone number" || pmUnreg.cur != 0 {
		t.Fatalf("unregistered items = %+v, cur = %d", pmUnreg.items, pmUnreg.cur)
	}
	if it, ok := pmUnreg.activate(); !ok || it.action != popupCopyPhone || it.raw != "08123456789" {
		t.Errorf("activate Copy phone = %+v, %v", it, ok)
	}

	// Closed popup ignores late async phone check
	pmClosed := newPhonePopupModel("+62 882-0007-83234")
	pmClosed.close()
	if pmClosed.applyPhoneCheck(true, "+62 882-0007-83234", "62882000783234@s.whatsapp.net") {
		t.Error("closed popup applied late phone check")
	}
	if len(pmClosed.items) != 2 || !pmClosed.items[0].disabled {
		t.Errorf("closed popup items mutated: %+v", pmClosed.items)
	}
}

func TestPlacePopupAtPointStaysInWorkArea(t *testing.T) {
	work := walk.Rectangle{X: 0, Y: 0, Width: 1920, Height: 1040}
	sz := walk.Size{Width: 240, Height: 80}

	got := placePopupAtPoint(walk.Point{X: 100, Y: 100}, sz, work)
	if got != (walk.Rectangle{X: 100, Y: 100, Width: 240, Height: 80}) {
		t.Errorf("normal placement = %+v", got)
	}

	edge := placePopupAtPoint(walk.Point{X: 1850, Y: 1010}, sz, work)
	if edge.X+edge.Width > work.Width || edge.Y+edge.Height > work.Height || edge.X < 0 || edge.Y < 0 {
		t.Errorf("edge placement escaped work area: %+v", edge)
	}
}
