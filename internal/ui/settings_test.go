package ui

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"

	"wa-go/internal/store"
	"wa-go/internal/theme"
	"wa-go/internal/whatsapp"
)

// What RunSettings is handed in main.go.
var (
	_ SettingsStore   = (*store.DB)(nil)
	_ SettingsManager = (*whatsapp.Manager)(nil)
)

type privacyCall struct {
	account string
	name    types.PrivacySettingType
	value   types.PrivacySetting
}

type timerCall struct {
	account string
	d       time.Duration
}

// fakePrivacy is WhatsApp's side of the privacy settings: what each account's server holds.
type fakePrivacy struct {
	calls  []privacyCall
	timers []timerCall
	server map[string]types.PrivacySettings
	fail   error // every call fails with it
}

func (f *fakePrivacy) SetPrivacySetting(_ context.Context, account string, name types.PrivacySettingType, value types.PrivacySetting) (types.PrivacySettings, error) {
	f.calls = append(f.calls, privacyCall{account, name, value})
	return f.server[account], f.fail
}

func (f *fakePrivacy) PrivacySettings(_ context.Context, account string) (types.PrivacySettings, error) {
	return f.server[account], f.fail
}

func (f *fakePrivacy) SetDefaultDisappearingTimer(_ context.Context, account string, d time.Duration) error {
	f.timers = append(f.timers, timerCall{account, d})
	return f.fail
}

type settingsHarness struct {
	ctl     *settingsCtl
	db      *store.DB
	wa      *fakePrivacy
	changed []string
	themes  []theme.ThemeMode
}

var errNotDone = errors.New("done was not called")

func newSettingsHarness(t *testing.T) *settingsHarness {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "wa.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := &settingsHarness{db: db, wa: &fakePrivacy{server: map[string]types.PrivacySettings{}}}
	h.ctl = &settingsCtl{
		store:     db,
		wa:        h.wa,
		bg:        func(work func() error, done func(error)) { done(work()) },
		onTheme:   func(m theme.ThemeMode) { h.themes = append(h.themes, m) },
		onChanged: func(key string) { h.changed = append(h.changed, key) },
	}
	return h
}

func (h *settingsHarness) privacy(account, key, value string) error {
	got := errNotDone
	h.ctl.setPrivacy(account, key, value, func(err error) { got = err })
	return got
}

func (h *settingsHarness) global(key, value string) error {
	got := errNotDone
	h.ctl.setGlobal(key, value, func(err error) { got = err })
	return got
}

// stored is the settings table's value of key for account, "<unset>" when there is none.
func (h *settingsHarness) stored(t *testing.T, account, key string) string {
	t.Helper()
	v, ok, err := h.db.GetSetting(account, key)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return "<unset>"
	}
	return v
}

func TestPrivacyGoesToWhatsAppThenTheTable(t *testing.T) {
	h := newSettingsHarness(t)
	if err := h.privacy(acctA, KeyReadReceipts, "none"); err != nil {
		t.Fatal(err)
	}
	if err := h.privacy(acctA, KeyLastSeen, "contacts"); err != nil {
		t.Fatal(err)
	}
	if err := h.privacy(acctA, KeyProfilePhoto, "none"); err != nil {
		t.Fatal(err)
	}
	want := []privacyCall{
		{acctA, types.PrivacySettingTypeReadReceipts, types.PrivacySettingNone},
		{acctA, types.PrivacySettingTypeLastSeen, types.PrivacySettingContacts},
		{acctA, types.PrivacySettingTypeProfile, types.PrivacySettingNone},
	}
	if !slices.Equal(h.wa.calls, want) {
		t.Fatalf("calls %+v, want %+v", h.wa.calls, want)
	}
	if rr, ls := h.stored(t, acctA, KeyReadReceipts), h.stored(t, acctA, KeyLastSeen); rr != "none" || ls != "contacts" {
		t.Fatalf("table: read receipts %q, last seen %q", rr, ls)
	}
	for _, key := range []string{KeyReadReceipts, KeyLastSeen, KeyProfilePhoto} {
		if v := h.stored(t, acctB, key); v != "<unset>" {
			t.Errorf("account B's %s became %q", key, v)
		}
		if v := h.stored(t, "", key); v != "<unset>" {
			t.Errorf("global %s became %q", key, v)
		}
	}
	if len(h.changed) != 0 {
		t.Fatalf("app-global change callbacks for per-account settings: %v", h.changed)
	}
}

func TestPrivacyFailureLeavesTheTable(t *testing.T) {
	h := newSettingsHarness(t)
	if err := h.privacy(acctA, KeyLastSeen, "all"); err != nil {
		t.Fatal(err)
	}
	h.wa.fail = errors.New("not connected")
	if err := h.privacy(acctA, KeyLastSeen, "none"); !errors.Is(err, h.wa.fail) {
		t.Fatalf("got %v, want the server's error", err)
	}
	if err := h.privacy(acctA, KeyDisappearing, "86400"); !errors.Is(err, h.wa.fail) {
		t.Fatalf("timer: got %v, want the server's error", err)
	}
	if v := h.stored(t, acctA, KeyLastSeen); v != "all" {
		t.Fatalf("last seen %q after a failed change, want all", v)
	}
	if v := h.stored(t, acctA, KeyDisappearing); v != "<unset>" {
		t.Fatalf("timer %q after a failed change", v)
	}
}

func TestPrivacyRejectsWhatWhatsAppWouldNot(t *testing.T) {
	h := newSettingsHarness(t)
	for _, c := range []struct{ account, key, value string }{
		{acctA, KeyReadReceipts, "contacts"}, // read receipts are all or none
		{acctA, KeyLastSeen, "everyone"},
		{acctA, KeyDisappearing, "3600"}, // not one of WhatsApp's timers
		{acctA, KeyGhostTyping, "true"},  // app-global, not a privacy setting
		{"", KeyLastSeen, "all"},         // no account
	} {
		if err := h.privacy(c.account, c.key, c.value); err == nil || err == errNotDone {
			t.Errorf("%+v: %v", c, err)
		}
	}
	if len(h.wa.calls)+len(h.wa.timers) != 0 {
		t.Fatalf("rejected changes reached WhatsApp: %+v %+v", h.wa.calls, h.wa.timers)
	}
}

func TestStealthIsLocalOnly(t *testing.T) {
	h := newSettingsHarness(t)
	for _, key := range []string{KeyGhostTyping, KeyAppearOffline} {
		if v, err := Setting(h.db, "", key); v != "false" || err != nil {
			t.Fatalf("%s default %q %v", key, v, err)
		}
		if err := h.global(key, "true"); err != nil {
			t.Fatal(err)
		}
		if v := h.stored(t, "", key); v != "true" {
			t.Fatalf("%s stored %q", key, v)
		}
		if v := h.stored(t, acctA, key); v != "<unset>" {
			t.Fatalf("%s stored per account: %q", key, v)
		}
	}
	if len(h.wa.calls)+len(h.wa.timers) != 0 {
		t.Fatalf("stealth flags reached WhatsApp: %+v %+v", h.wa.calls, h.wa.timers)
	}
	if !slices.Equal(h.changed, []string{KeyGhostTyping, KeyAppearOffline}) {
		t.Fatalf("change callbacks %v", h.changed)
	}
}

func TestDisappearingDefaultSetsTheTimer(t *testing.T) {
	h := newSettingsHarness(t)
	if v, err := Setting(h.db, acctA, KeyDisappearing); v != "0" || err != nil {
		t.Fatalf("default %q %v", v, err)
	}
	if err := h.privacy(acctA, KeyDisappearing, "604800"); err != nil {
		t.Fatal(err)
	}
	if err := h.privacy(acctB, KeyDisappearing, "0"); err != nil {
		t.Fatal(err)
	}
	want := []timerCall{{acctA, 7 * 24 * time.Hour}, {acctB, 0}}
	if !slices.Equal(h.wa.timers, want) || len(h.wa.calls) != 0 {
		t.Fatalf("timers %+v, privacy calls %+v", h.wa.timers, h.wa.calls)
	}
	if a, b := h.stored(t, acctA, KeyDisappearing), h.stored(t, acctB, KeyDisappearing); a != "604800" || b != "0" {
		t.Fatalf("stored A %q B %q", a, b)
	}
}

func TestRefreshMirrorsTheServerPerAccount(t *testing.T) {
	h := newSettingsHarness(t)
	h.wa.server[acctA] = types.PrivacySettings{LastSeen: types.PrivacySettingContacts, ReadReceipts: types.PrivacySettingNone, Profile: types.PrivacySettingNone}
	h.wa.server[acctB] = types.PrivacySettings{LastSeen: types.PrivacySettingAll}
	if err := h.db.SetSetting(acctB, KeyLastSeen, "none"); err != nil {
		t.Fatal(err)
	}
	var vals map[string]string
	got := errNotDone
	h.ctl.refresh(acctA, func(v map[string]string, err error) { vals, got = v, err })
	if got != nil {
		t.Fatal(got)
	}
	for key, want := range map[string]string{KeyLastSeen: "contacts", KeyReadReceipts: "none", KeyProfilePhoto: "none"} {
		if v := h.stored(t, acctA, key); v != want {
			t.Errorf("A's %s: table %q, want %q", key, v, want)
		}
		if vals[key] != want {
			t.Errorf("A's %s: shown %q, want %q", key, vals[key], want)
		}
	}
	if vals[KeyDisappearing] != "0" || vals[KeyTheme] != "system" {
		t.Errorf("refresh answer lacks the other values: %v", vals)
	}
	if v := h.stored(t, acctB, KeyLastSeen); v != "none" {
		t.Fatalf("B's last seen %q after refreshing A", v)
	}

	h.wa.fail = errors.New("not connected")
	h.ctl.refresh(acctB, func(v map[string]string, err error) { vals, got = v, err })
	if !errors.Is(got, h.wa.fail) || vals != nil {
		t.Fatalf("failed refresh: %v %v", vals, got)
	}
	if v := h.stored(t, acctB, KeyLastSeen); v != "none" {
		t.Fatalf("B's last seen %q after a failed refresh", v)
	}
}

func TestProxyIsValidatedBeforeItIsSaved(t *testing.T) {
	h := newSettingsHarness(t)
	for _, addr := range []string{"socks5://127.0.0.1:1080", "  http://proxy.lan:8080  ", "https://proxy.example.com:443", ""} {
		if err := h.global(KeyProxy, addr); err != nil {
			t.Fatalf("%q: %v", addr, err)
		}
	}
	if err := h.global(KeyProxy, "http://proxy.lan:3128"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"ftp://proxy.lan:21", "socks5://proxy.lan", "proxy.lan:8080", "junk", "http://proxy.lan:8080/x"} {
		if err := h.global(KeyProxy, bad); !errors.Is(err, whatsapp.ErrBadProxy) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if v := h.stored(t, "", KeyProxy); v != "http://proxy.lan:3128" {
		t.Fatalf("stored %q: a rejected address replaced the saved one", v)
	}
	if len(h.changed) != 5 {
		t.Fatalf("change callbacks %v: want one per saved address only", h.changed)
	}
	h.global(KeyProxy, " http://proxy.lan:8080 ")
	if v := h.stored(t, "", KeyProxy); v != "http://proxy.lan:8080" {
		t.Fatalf("stored %q, want it trimmed", v)
	}
}

func TestThemeChangeFiresTheCallbackAtOnce(t *testing.T) {
	h := newSettingsHarness(t)
	if v, err := Setting(h.db, "", KeyTheme); v != "system" || err != nil {
		t.Fatalf("default %q %v", v, err)
	}
	var savedFirst []bool
	h.ctl.bg = func(work func() error, done func(error)) {
		savedFirst = append(savedFirst, len(h.themes) == 0)
		done(work())
	}
	for _, v := range []string{"dark", "light", "system"} {
		if err := h.global(KeyTheme, v); err != nil {
			t.Fatal(err)
		}
	}
	if want := []theme.ThemeMode{theme.ThemeDark, theme.ThemeLight, theme.ThemeSystem}; !slices.Equal(h.themes, want) {
		t.Fatalf("themes %v, want %v", h.themes, want)
	}
	if savedFirst[0] {
		t.Fatal("the callback waited for the save")
	}
	if v := h.stored(t, "", KeyTheme); v != "system" {
		t.Fatalf("stored %q", v)
	}
	if ParseTheme("dark") != theme.ThemeDark || ParseTheme("light") != theme.ThemeLight || ParseTheme("bogus") != theme.ThemeSystem {
		t.Fatal("ParseTheme")
	}
}

func TestSettingDefaults(t *testing.T) {
	h := newSettingsHarness(t)
	for key, want := range map[string]string{KeyAutoDownloadImages: "true", KeyNotify: "true", KeyNotifySound: "true",
		KeyNotifyPrivate: "false", KeyGiphyAPIKey: "", KeyProxy: ""} {
		if v, err := Setting(h.db, "", key); v != want || err != nil {
			t.Errorf("%s: %q %v, want %q", key, v, err, want)
		}
	}
	if err := h.global(KeyAutoDownloadImages, "false"); err != nil {
		t.Fatal(err)
	}
	if v, _ := Setting(h.db, "", KeyAutoDownloadImages); v != "false" {
		t.Fatalf("saved value %q", v)
	}
	if v, err := Setting(h.db, acctA, KeyLastSeen); v != "all" || err != nil {
		t.Fatalf("per-account default %q %v", v, err)
	}
}

func TestSerialRunsJobsOneAtATimeInOrder(t *testing.T) {
	var s serial
	var running atomic.Int32
	var got []int          // written by one job at a time; read after wg.Wait
	for round := range 2 { // the worker exits when idle and starts again for new work
		var wg sync.WaitGroup
		for i := range 50 {
			wg.Add(1)
			s.do(func() {
				defer wg.Done()
				if running.Add(1) != 1 {
					t.Error("two jobs ran at once")
				}
				got = append(got, round*50+i)
				running.Add(-1)
			})
		}
		wg.Wait()
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("job order %v", got)
		}
	}
	if len(got) != 100 {
		t.Fatalf("%d of 100 jobs ran", len(got))
	}
}
