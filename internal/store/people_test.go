package store

import (
	"testing"
)

func TestUpsertPeopleMergesFields(t *testing.T) {
	db := openTest(t)
	const acct = "6281234567890"

	// Step 1: {jid A, PushName "Sinta"}
	if err := db.UpsertPeople(acct, []Person{{JID: "A", PushName: "Sinta"}}); err != nil {
		t.Fatalf("UpsertPeople step 1: %v", err)
	}
	people, err := db.People(acct)
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	if len(people) != 1 || people[0].JID != "A" || people[0].PushName != "Sinta" || people[0].PNJID != "" {
		t.Fatalf("step 1 unexpected: %+v", people)
	}

	// Step 2: {jid A, PNJID "62…@s.whatsapp.net"} -> one row with both
	const pnJID = "6281234567890@s.whatsapp.net"
	if err := db.UpsertPeople(acct, []Person{{JID: "A", PNJID: pnJID}}); err != nil {
		t.Fatalf("UpsertPeople step 2: %v", err)
	}
	people, err = db.People(acct)
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	if len(people) != 1 {
		t.Fatalf("expected 1 row, got %d", len(people))
	}
	if people[0].PushName != "Sinta" || people[0].PNJID != pnJID {
		t.Fatalf("step 2 merge failed: %+v", people[0])
	}

	// Step 3: {jid A, PushName ""} leaves "Sinta"
	if err := db.UpsertPeople(acct, []Person{{JID: "A", PushName: ""}}); err != nil {
		t.Fatalf("UpsertPeople step 3: %v", err)
	}
	people, err = db.People(acct)
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	if len(people) != 1 || people[0].PushName != "Sinta" || people[0].PNJID != pnJID {
		t.Fatalf("step 3 empty push_name overwrote value: %+v", people[0])
	}

	// Step 4: FullName overwrites non-empty and leaves on empty
	if err := db.UpsertPeople(acct, []Person{{JID: "A", FullName: "Sinta Dewi"}}); err != nil {
		t.Fatalf("UpsertPeople step 4: %v", err)
	}
	people, err = db.People(acct)
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	if people[0].FullName != "Sinta Dewi" || people[0].PushName != "Sinta" || people[0].PNJID != pnJID {
		t.Fatalf("step 4 failed: %+v", people[0])
	}
	if err := db.UpsertPeople(acct, []Person{{JID: "A", FullName: ""}}); err != nil {
		t.Fatalf("UpsertPeople step 4b: %v", err)
	}
	people, err = db.People(acct)
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	if people[0].FullName != "Sinta Dewi" {
		t.Fatalf("step 4b empty full_name overwrote value: %+v", people[0])
	}
}

func TestSetAvatarAndReset(t *testing.T) {
	db := openTest(t)
	const acct = "6281234567890"

	// SetAvatar on an unknown jid creates the row
	if err := db.SetAvatar(acct, "unknown@lid", "av123", 1000); err != nil {
		t.Fatalf("SetAvatar: %v", err)
	}
	people, err := db.People(acct)
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	if len(people) != 1 || people[0].JID != "unknown@lid" || people[0].AvatarID != "av123" || people[0].AvatarTS != 1000 {
		t.Fatalf("after SetAvatar unexpected: %+v", people)
	}

	// UpsertPeople afterwards keeps avatar_id and avatar_ts
	if err := db.UpsertPeople(acct, []Person{{JID: "unknown@lid", PushName: "Name"}}); err != nil {
		t.Fatalf("UpsertPeople: %v", err)
	}
	people, err = db.People(acct)
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	if len(people) != 1 || people[0].PushName != "Name" || people[0].AvatarID != "av123" || people[0].AvatarTS != 1000 {
		t.Fatalf("after UpsertPeople avatar fields not kept: %+v", people)
	}

	// Also add another account
	if err := db.SetAvatar("other-acct", "user2", "av456", 2000); err != nil {
		t.Fatalf("SetAvatar other-acct: %v", err)
	}

	// ResetAvatars zeroes avatar_ts only
	if err := db.ResetAvatars(); err != nil {
		t.Fatalf("ResetAvatars: %v", err)
	}

	people, err = db.People(acct)
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	if len(people) != 1 || people[0].AvatarID != "av123" || people[0].AvatarTS != 0 {
		t.Fatalf("ResetAvatars acct unexpected: %+v", people[0])
	}

	otherPeople, err := db.People("other-acct")
	if err != nil {
		t.Fatalf("People other-acct: %v", err)
	}
	if len(otherPeople) != 1 || otherPeople[0].AvatarID != "av456" || otherPeople[0].AvatarTS != 0 {
		t.Fatalf("ResetAvatars other-acct unexpected: %+v", otherPeople[0])
	}
}
