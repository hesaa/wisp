package ui

import (
	"strconv"
	"testing"

	"github.com/lxn/walk"

	"wa-go/internal/store"
)

func photos(n int, from string, at int64) []store.Message {
	var ms []store.Message
	for k := range n {
		ms = append(ms, store.Message{ID: from + strconv.Itoa(k), Type: "image", SenderJID: from, Timestamp: at + int64(k)*10})
	}
	return ms
}

func TestAlbumGrouping(t *testing.T) {
	five := photos(5, alice, 1000)
	three := photos(3, alice, 1000)
	captioned := photos(4, alice, 1000)
	captioned[2].Body = "look"
	late := photos(4, alice, 1000)
	late[3].Timestamp += albumWindow
	mixed := append(photos(2, alice, 1000), photos(2, "bob", 1020)...)
	for _, c := range []struct {
		name string
		msgs []store.Message
		want []int // albumLen of each
	}{
		{"five photos", five, []int{5, 0, 0, 0, 0}},
		{"three are too few", three, []int{0, 0, 0}},
		{"a caption breaks it", captioned, []int{0, 0, 0, 0}},
		{"over a minute apart", late, []int{0, 0, 0, 0}},
		{"two senders", mixed, []int{0, 0, 0, 0}},
	} {
		for i, w := range c.want {
			if got := albumLen(c.msgs, i); got != w {
				t.Errorf("%s: albumLen(%d) = %d, want %d", c.name, i, got, w)
			}
		}
	}
}

func TestAlbumLayout(t *testing.T) {
	msgs := append(photos(5, alice, 1000), store.Message{ID: "t", Type: "text", Body: "after", SenderJID: alice, Timestamp: 2000})
	l := layOut(t, false, msgs...)
	lead := l.cache[msgs[0].ID]
	if lead.album != 5 || len(lead.albumPreview) != 4 || lead.thumb.Width != lead.thumb.Height || lead.thumb.Width == 0 {
		t.Fatalf("lead %+v", lead)
	}
	for _, m := range msgs[1:5] {
		if b := l.cache[m.ID]; !b.hidden || b.box.Height != 0 || l.tops[1] != l.tops[0]+lead.box.Height {
			t.Errorf("photo %s not hidden under the album", m.ID)
		}
	}
	if l.tops[5] != l.tops[4]+runGap {
		t.Errorf("the text after sits at %d, want %d", l.tops[5], l.tops[4]+runGap)
	}
	// a sixth photo grows the album, laid out again
	more := append(photos(6, alice, 1000), msgs[5])
	l.update(more, viewW, 96, &monoMeasurer{})
	if b := l.cache[more[0].ID]; b.album != 6 {
		t.Errorf("album of %d after a sixth photo", b.album)
	}
}

func TestAlbumHit(t *testing.T) {
	b := bubble{thumb: walk.Rectangle{X: 100, Y: 0, Width: 202, Height: 202}}
	for _, c := range []struct {
		x, y      int
		pill      bool
		k         int
		all, isOK bool
	}{
		{110, 10, false, 0, false, true},
		{290, 10, false, 1, false, true},
		{110, 190, false, 2, false, true},
		{290, 190, false, 3, false, true},
		{201, 101, true, 0, true, true},    // the pill
		{201, 101, false, 0, false, false}, // no pill: the gap
		{50, 10, false, 0, false, false},
	} {
		k, all, ok := albumHit(b, c.x, c.y, 96, c.pill)
		if k != c.k || all != c.all || ok != c.isOK {
			t.Errorf("(%d,%d) pill %v: %d %v %v", c.x, c.y, c.pill, k, all, ok)
		}
	}
}

func TestSaveFilter(t *testing.T) {
	if got := saveFilter(".jpg"); got != "JPEG image (*.jpg)|*.jpg|All files (*.*)|*.*" {
		t.Errorf("%q", got)
	}
	if got := saveFilter(".xyz"); got != "XYZ file (*.xyz)|*.xyz|All files (*.*)|*.*" {
		t.Errorf("%q", got)
	}
}
