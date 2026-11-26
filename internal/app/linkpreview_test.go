package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wa-go/internal/linkpreview"
	"wa-go/internal/store"
	"wa-go/internal/ui"
	"wa-go/internal/whatsapp"
)

func TestOpenLinkRejectsNonHTTPAndAllowsHTTPS(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://example.com/a": true, "http://example.com": true, "HTTPS://EXAMPLE.com": true,
		"file:///C:/Windows/notepad.exe": false, "javascript:alert(1)": false, "ms-msdt:/x": false,
		"example.com": false, "//example.com": false, "": false, "https://": false,
	} {
		if _, ok := openableURL(raw); ok != want {
			t.Errorf("openableURL(%q) = %v, want %v", raw, ok, want)
		}
	}
}

type phoneBackend struct {
	Backend
	calls int
	res   map[string]whatsapp.PhoneStatus
	err   error
}

func (b *phoneBackend) CheckPhones(_ context.Context, _ string, phones []string) (map[string]whatsapp.PhoneStatus, error) {
	b.calls++
	return b.res, b.err
}

func phoneApp(b Backend) *App {
	return &App{mgr: b, bg: context.Background(), cfg: map[string]string{}, v: view{active: acctA,
		names: map[string]map[string]string{acctA: {alice: "Alice"}}, dirs: map[string]*ui.Directory{}}}
}

func resolve(a *App, raw string) (reg bool, display, jid string) {
	got := make(chan struct{})
	a.resolvePhone(raw, func(r bool, d, j string) { reg, display, jid = r, d, j; close(got) })
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		panic("resolvePhone never answered")
	}
	return
}

func TestResolvePhoneUsesLocalContactsBeforeNetwork(t *testing.T) {
	b := &phoneBackend{res: map[string]whatsapp.PhoneStatus{"+6283333333333": {Registered: true, JID: "6283333333333@s.whatsapp.net"}}}
	a := phoneApp(b)

	if reg, disp, jid := resolve(a, "+62 811 1111 1111"); !reg || jid != alice || disp != "Alice" || b.calls != 0 {
		t.Fatalf("known chat: %v %q %q calls=%d", reg, disp, jid, b.calls)
	}
	if reg, disp, jid := resolve(a, "+62 833 3333 3333"); !reg || jid != "6283333333333@s.whatsapp.net" || disp != "+6283333333333" || b.calls != 1 {
		t.Fatalf("unknown number: %v %q %q calls=%d", reg, disp, jid, b.calls)
	}
	b.err = errors.New("offline")
	if reg, _, _ := resolve(a, "+62 844 4444 4444"); reg {
		t.Fatal("a failed check must report not registered")
	}
	if reg, _, _ := resolve(a, "12"); reg {
		t.Fatal("invalid number must not be registered")
	}
	a.locked = true
	calls := b.calls
	if reg, _, _ := resolve(a, "+62 855 5555 5555"); reg || b.calls != calls {
		t.Fatal("no network check behind the lock")
	}
}

func TestLinkPreviewWorkerRespectsSettingAndLock(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "wa.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Now().Unix()
	for _, p := range []store.LinkPreview{
		{URL: "https://ok", Status: "ok", Title: "T", FetchedAt: now - 99999999},
		{URL: "https://fresh-none", Status: "none", FetchedAt: now - 60},
	} {
		if err := db.PutLinkPreview(p); err != nil {
			t.Fatal(err)
		}
	}
	fetches := 0
	fetch := func(context.Context, string) (linkpreview.Result, error) { fetches++; return linkpreview.Result{}, nil }

	if p, ok := previewLookup(context.Background(), db, "https://ok", true, fetch); !ok || p.Title != "T" || fetches != 0 {
		t.Fatalf("cached ok: %+v %v fetches=%d", p, ok, fetches)
	}
	if _, ok := previewLookup(context.Background(), db, "https://fresh-none", true, fetch); ok || fetches != 0 {
		t.Fatalf("fresh none must not refetch or show: fetches=%d", fetches)
	}
	if _, ok := previewLookup(context.Background(), db, "https://miss", false, fetch); ok || fetches != 0 {
		t.Fatal("fetching not allowed: no fetch")
	}
	if _, ok := previewLookup(context.Background(), db, "https://miss", true, fetch); ok || fetches != 1 {
		t.Fatalf("miss with nothing found: fetches=%d", fetches)
	}
	if p, found, _ := db.GetLinkPreview("https://miss"); !found || p.Status != "none" {
		t.Fatalf("a miss is cached as none: %+v", p)
	}

	// setting off or locked: needPreview never starts a lookup
	for _, a := range []*App{
		{cfg: map[string]string{ui.KeyLinkPreviews: "false"}},
		{cfg: map[string]string{}, locked: true},
	} {
		a.db = db
		a.fetchLink = func(context.Context, string) (linkpreview.Result, error) {
			t.Fatal("fetched")
			return linkpreview.Result{}, nil
		}
		a.needPreview("https://ok", true, func(store.LinkPreview) { t.Fatal("delivered") })
		a.needPreview("https://ok", false, func(store.LinkPreview) { t.Fatal("delivered") })
	}
}

func TestOpenChatGuardAllowsUnseenJID(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "wa.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a := phoneApp(nil)
	const fresh = "6283333333333@s.whatsapp.net" // in no chat list or directory
	if !a.canOpenChat(acctA, fresh) {
		t.Fatal("an unseen JID must be openable")
	}
	if page, err := db.GetChatMessages(acctA, fresh, 0); err != nil || len(page) != 0 {
		t.Fatalf("an unseen chat opens as an empty page: %v %v", page, err)
	}
	if a.canOpenChat(acctB, fresh) || a.canOpenChat(acctA, "") {
		t.Fatal("other account / empty JID must not open")
	}
	a.locked = true
	if a.canOpenChat(acctA, fresh) {
		t.Fatal("nothing opens behind the lock")
	}
}

func TestLinkPreviewCacheHitSkipsFetch(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "wa.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Now().Unix()
	for _, p := range []store.LinkPreview{
		{URL: "https://ok", Status: "ok", Title: "T", FetchedAt: now - 99999999},
	} {
		if err := db.PutLinkPreview(p); err != nil {
			t.Fatal(err)
		}
	}
	var fetches atomic.Int32
	a := &App{db: db, bg: context.Background(), cfg: map[string]string{}}
	a.fetchLink = func(context.Context, string) (linkpreview.Result, error) {
		fetches.Add(1)
		return linkpreview.Result{}, nil
	}
	got := make(chan string, 1)
	a.needPreview("https://ok", true, func(p store.LinkPreview) { got <- p.Title })
	select {
	case title := <-got:
		if title != "T" {
			t.Fatalf("title %q", title)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cached preview never delivered")
	}
	if n := fetches.Load(); n != 0 {
		t.Fatalf("fetched %d times on a cache hit", n)
	}
}

func previewTestApp(t *testing.T, fetch func(context.Context, string) (linkpreview.Result, error)) *App {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "wa.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &App{db: db, bg: context.Background(), cfg: map[string]string{}, fetchLink: fetch}
}

func TestNeedPreviewSharesOneLookupPerURL(t *testing.T) {
	release := make(chan struct{})
	var fetches atomic.Int32
	a := previewTestApp(t, func(_ context.Context, u string) (linkpreview.Result, error) {
		fetches.Add(1)
		<-release
		return linkpreview.Result{Found: true, Title: "T " + u}, nil
	})
	got := make(chan string, 2)
	a.needPreview("https://same", true, func(p store.LinkPreview) { got <- "first " + p.Title })
	a.needPreview("https://same", true, func(p store.LinkPreview) { got <- "second " + p.Title })
	time.Sleep(100 * time.Millisecond) // room for a second lookup to start, were one started
	close(release)
	for range 2 {
		select {
		case s := <-got:
			if s != "first T https://same" && s != "second T https://same" {
				t.Fatalf("delivered %q", s)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a waiter never got the shared result")
		}
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("fetched %d times for one URL, want 1", n)
	}
}

func TestNeedPreviewRunsAtMostTwoLookups(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	var cur, peak atomic.Int32
	a := previewTestApp(t, func(context.Context, string) (linkpreview.Result, error) {
		n := cur.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		started <- struct{}{}
		<-release
		cur.Add(-1)
		return linkpreview.Result{}, nil
	})
	for _, u := range []string{"https://a", "https://b", "https://c", "https://d"} {
		a.needPreview(u, true, func(store.LinkPreview) {})
	}
	for range 2 {
		<-started
	}
	select {
	case <-started:
		t.Fatal("a third lookup started while two were under way")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	for range 2 {
		<-started
	}
	if p := peak.Load(); p != 2 {
		t.Fatalf("peak concurrent lookups = %d, want 2", p)
	}
}

func TestSetLinkProxyFailsClosed(t *testing.T) {
	a := &App{}
	a.setLinkProxy("")
	if a.linkFetch == nil {
		t.Fatal("no fetcher for a direct connection")
	}
	a.setLinkProxy("ftp://bad:21") // SetProxy refuses it: the old (direct) config must not be used
	if _, err := a.fetchLink(context.Background(), "https://example.com"); err == nil || !strings.Contains(err.Error(), "no usable proxy") {
		t.Fatalf("bad proxy: fetch err = %v, want the fail-closed stub", err)
	}
	b := &App{}
	b.setLinkProxy("socks4://127.0.0.1:1080") // NewFetcher refuses it
	if _, err := b.fetchLink(context.Background(), "https://example.com"); err == nil || !strings.Contains(err.Error(), "no usable proxy") {
		t.Fatalf("bad proxy at start: fetch err = %v, want the fail-closed stub", err)
	}
}
