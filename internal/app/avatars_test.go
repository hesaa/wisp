package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wa-go/internal/store"
	"wa-go/internal/ui"
	"wa-go/internal/whatsapp"
)

func TestFetchAllowed(t *testing.T) {
	if fetchAllowed(false, false, false) {
		t.Error("fetchAllowed should be false when hidden")
	}
	if fetchAllowed(true, true, false) {
		t.Error("fetchAllowed should be false when minimised (iconic)")
	}
	if fetchAllowed(true, false, true) {
		t.Error("fetchAllowed should be false when locked")
	}
	if !fetchAllowed(true, false, false) {
		t.Error("fetchAllowed should be true when visible, not iconic, not locked")
	}
}

func TestAvatarStep(t *testing.T) {
	now := time.Unix(1700000000, 0)
	p := store.Person{JID: "user@s.whatsapp.net", AvatarID: "old_id", AvatarTS: 1600000000}

	// 1. Unchanged: keep id, ts = now, save = false
	id, ts, save := avatarStep(p, now, pictureResult{unchanged: true})
	if id != "old_id" || ts != now.Unix() || save {
		t.Errorf("unchanged: got (%q, %d, %v), want (%q, %d, false)", id, ts, save, "old_id", now.Unix())
	}

	// 2. New picture: new id, ts = now, save = true
	id, ts, save = avatarStep(p, now, pictureResult{id: "new_id", data: []byte{1, 2, 3}})
	if id != "new_id" || ts != now.Unix() || !save {
		t.Errorf("new picture: got (%q, %d, %v), want (%q, %d, true)", id, ts, save, "new_id", now.Unix())
	}

	// 3. ErrNoPicture: id = "-", ts = now, save = false
	id, ts, save = avatarStep(p, now, pictureResult{err: whatsapp.ErrNoPicture})
	if id != "-" || ts != now.Unix() || save {
		t.Errorf("ErrNoPicture: got (%q, %d, %v), want (%q, %d, false)", id, ts, save, "-", now.Unix())
	}

	// 4. Other error: keep id, ts = avatarFailedTS(now), save = false
	errNet := errors.New("network error")
	id, ts, save = avatarStep(p, now, pictureResult{err: errNet})
	if id != "old_id" || ts != ui.AvatarFailedTS(now) || save {
		t.Errorf("network error: got (%q, %d, %v), want (%q, %d, false)", id, ts, save, "old_id", ui.AvatarFailedTS(now))
	}
	// After error, not due for 10 minutes
	pAfter := store.Person{JID: p.JID, AvatarID: id, AvatarTS: ts}
	if ui.AvatarDue(pAfter, now.Add(9*time.Minute)) {
		t.Errorf("person should not be due 9 minutes after failure")
	}
	if !ui.AvatarDue(pAfter, now.Add(11*time.Minute)) {
		t.Errorf("person should be due 11 minutes after failure")
	}
}

func TestAvatarQueueOneAtATimeNoLoop(t *testing.T) {
	q := newAvatarQueue()

	var (
		inFlight    int32
		maxInFlight int32
		calls       int32
		mu          sync.Mutex
		calledJIDs  []string
		blockCh     = make(chan struct{})
	)

	q.fetch = func(account, jid string) pictureResult {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			cur := atomic.LoadInt32(&maxInFlight)
			if n <= cur || atomic.CompareAndSwapInt32(&maxInFlight, cur, n) {
				break
			}
		}
		atomic.AddInt32(&calls, 1)
		mu.Lock()
		calledJIDs = append(calledJIDs, jid)
		mu.Unlock()

		if jid == "user1" {
			<-blockCh
		}
		atomic.AddInt32(&inFlight, -1)
		if jid == "fail_user" {
			return pictureResult{err: errors.New("fail")}
		}
		return pictureResult{unchanged: true}
	}

	q.wanted = func(account, jid string) bool {
		return jid != "cancelled_user"
	}

	// 1. Submit 3 wants: user1 (blocks), user2, user3
	q.want("acc1", "user1")
	q.want("acc1", "user2")
	q.want("acc1", "user3")

	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&inFlight) != 1 {
		t.Errorf("inFlight = %d, want 1", atomic.LoadInt32(&inFlight))
	}

	// 2. Unblock user1
	close(blockCh)

	// Wait for queue to drain
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&inFlight) > 0 || atomic.LoadInt32(&calls) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for queue: calls=%d", atomic.LoadInt32(&calls))
		}
		time.Sleep(10 * time.Millisecond)
	}

	if max := atomic.LoadInt32(&maxInFlight); max != 1 {
		t.Errorf("maxInFlight was %d, want 1 (one fetch at a time per account)", max)
	}

	// 3. A want for a jid no longer wanted is dropped
	var droppedJID string
	q.onDrop = func(account, jid string) {
		droppedJID = jid
	}
	q.want("acc1", "cancelled_user")
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	for _, j := range calledJIDs {
		if j == "cancelled_user" {
			t.Errorf("cancelled_user was fetched but was no longer wanted")
		}
	}
	mu.Unlock()
	if droppedJID != "cancelled_user" {
		t.Errorf("droppedJID = %q, want 'cancelled_user'", droppedJID)
	}

	// 4. Failing fake is called once per want, not repeatedly in a loop
	callsBefore := atomic.LoadInt32(&calls)
	q.want("acc1", "fail_user")
	time.Sleep(100 * time.Millisecond)
	callsAfter := atomic.LoadInt32(&calls)
	if diff := callsAfter - callsBefore; diff != 1 {
		t.Errorf("failing fake called %d times, want exactly 1", diff)
	}
}

type fakeFetcher struct {
	pic      whatsapp.Picture
	ok       bool
	err      error
	data     []byte
	fetchErr error
}

func (f *fakeFetcher) ProfilePicture(_ context.Context, accountID, jid, knownID string, full bool) (whatsapp.Picture, bool, error) {
	return f.pic, f.ok, f.err
}

func (f *fakeFetcher) FetchPicture(_ context.Context, rawURL string) ([]byte, error) {
	return f.data, f.fetchErr
}

func TestFetchAvatarResult(t *testing.T) {
	ctx := context.Background()

	// 1. Picture found (ok = true) -> downloads data
	f1 := &fakeFetcher{
		pic:  whatsapp.Picture{ID: "pic999", URL: "http://example.com/pic.jpg"},
		ok:   true,
		data: []byte{0xff, 0xd8, 0xff},
	}
	res1 := fetchAvatarResult(ctx, f1, "acc", "user1", "")
	if res1.err != nil || res1.unchanged || res1.id != "pic999" || len(res1.data) == 0 {
		t.Errorf("expected picture downloaded, got %+v", res1)
	}

	// 2. Unchanged (ok = false, err = nil) -> unchanged = true, no fetch
	f2 := &fakeFetcher{
		ok:  false,
		err: nil,
	}
	res2 := fetchAvatarResult(ctx, f2, "acc", "user2", "pic999")
	if res2.err != nil || !res2.unchanged || res2.data != nil {
		t.Errorf("expected unchanged = true, got %+v", res2)
	}

	// 3. Error from ProfilePicture -> reports error
	f3 := &fakeFetcher{
		err: whatsapp.ErrNoPicture,
	}
	res3 := fetchAvatarResult(ctx, f3, "acc", "user3", "")
	if !errors.Is(res3.err, whatsapp.ErrNoPicture) {
		t.Errorf("expected ErrNoPicture, got %+v", res3)
	}
}
