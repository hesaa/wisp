package app

import (
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

type avatarQueue struct {
	mu       sync.Mutex
	pending  map[string][]string
	inFlight map[string]bool
	fetch    func(account, jid string) pictureResult
	wanted   func(account, jid string) bool
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
