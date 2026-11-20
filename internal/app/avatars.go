package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"wa-go/internal/store"
	"wa-go/internal/ui"
	"wa-go/internal/whatsapp"
)

type pictureResult struct {
	id        string
	data      []byte
	unchanged bool
	err       error
}

type avatarFetcher interface {
	ProfilePicture(ctx context.Context, accountID, jid, knownID string, full bool) (whatsapp.Picture, bool, error)
	FetchPicture(ctx context.Context, rawURL string) ([]byte, error)
}

func fetchAvatarResult(ctx context.Context, mgr avatarFetcher, account, jid, knownID string) pictureResult {
	if mgr == nil {
		return pictureResult{err: errors.New("no manager")}
	}
	pic, ok, err := mgr.ProfilePicture(ctx, account, jid, knownID, false)
	if err != nil {
		return pictureResult{err: err}
	}
	if !ok {
		return pictureResult{unchanged: true}
	}
	data, err := mgr.FetchPicture(ctx, pic.URL)
	if err != nil {
		return pictureResult{err: err}
	}
	return pictureResult{id: pic.ID, data: data}
}

func fetchAllowed(visible, iconic, locked bool) bool {
	return visible && !iconic && !locked
}

func avatarStep(p store.Person, now time.Time, res pictureResult) (id string, ts int64, save bool) {
	if res.err != nil {
		if errors.Is(res.err, whatsapp.ErrNoPicture) {
			return "-", now.Unix(), false
		}
		return p.AvatarID, ui.AvatarFailedTS(now), false
	}
	if res.unchanged {
		return p.AvatarID, now.Unix(), false
	}
	if res.data != nil {
		return res.id, now.Unix(), true
	}
	return p.AvatarID, now.Unix(), false
}

// avatarNeedsRepaint reports whether an avatar update changes the visible bitmap on screen,
// requiring UI widget repainting.
func avatarNeedsRepaint(oldID, newID string, save bool) bool {
	if save {
		return true
	}
	if oldID != "" && newID == "-" {
		return true
	}
	return false
}

type avatarQueue struct {
	mu       sync.Mutex
	pending  map[string][]string
	inFlight map[string]bool
	fetch    func(account, jid string) pictureResult
	wanted   func(account, jid string) bool
	onDrop   func(account, jid string)
	onDone   func(account, jid string, res pictureResult)
}

func newAvatarQueue() *avatarQueue {
	return &avatarQueue{
		pending:  make(map[string][]string),
		inFlight: make(map[string]bool),
	}
}

func (q *avatarQueue) want(account, jid string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.pending[account]
	found := false
	for i, j := range items {
		if j == jid {
			items = append(append(items[:i], items[i+1:]...), jid)
			q.pending[account] = items
			found = true
			break
		}
	}
	if !found {
		q.pending[account] = append(q.pending[account], jid)
	}
	if !q.inFlight[account] {
		q.startNext(account)
	}
}

func (q *avatarQueue) startNext(account string) {
	for len(q.pending[account]) > 0 {
		n := len(q.pending[account])
		jid := q.pending[account][n-1]
		q.pending[account] = q.pending[account][:n-1]

		if q.wanted != nil && !q.wanted(account, jid) {
			if q.onDrop != nil {
				q.onDrop(account, jid)
			}
			continue
		}

		q.inFlight[account] = true
		go func(acc, j string) {
			var res pictureResult
			if q.fetch != nil {
				res = q.fetch(acc, j)
			}
			if q.onDone != nil {
				q.onDone(acc, j, res)
			}
			q.mu.Lock()
			q.inFlight[acc] = false
			q.startNext(acc)
			q.mu.Unlock()
		}(account, jid)
		return
	}
}
