package store

import "testing"

func TestSettingsPerAccountAndGlobal(t *testing.T) {
	db := openTest(t)
	const other = "6289999999999"
	if v, ok, err := db.GetSetting(acct, "k"); v != "" || ok || err != nil {
		t.Fatalf("unset: %q %v %v", v, ok, err)
	}
	for _, s := range []struct{ acct, val string }{{acct, "a"}, {other, "b"}, {"", "global"}, {acct, "a2"}} {
		if err := db.SetSetting(s.acct, "k", s.val); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []struct{ acct, want string }{{acct, "a2"}, {other, "b"}, {"", "global"}} {
		if v, ok, err := db.GetSetting(s.acct, "k"); v != s.want || !ok || err != nil {
			t.Errorf("account %q: %q %v %v, want %q", s.acct, v, ok, err, s.want)
		}
	}
	if err := db.SetSetting(other, "empty", ""); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := db.GetSetting(other, "empty"); v != "" || !ok || err != nil {
		t.Fatalf("empty value: %q %v %v (set to empty is not unset)", v, ok, err)
	}
}
