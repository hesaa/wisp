package ui

import (
	"errors"
	"strconv"
	"strings"

	"github.com/lxn/walk"

	"wa-go/internal/store"
	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
)

// Albums, as WhatsApp shows them: four or more photos without captions sent by one sender, each
// within a minute of the last, are one bubble of the first four in a 2x2 grid, "+N" on the fourth
// for the rest. The bubble is the first photo's; the others are laid out hidden under it, and
// clicking a tile opens that photo in the viewer (where ← and → step through all of them).
// shortcut: videos break an album rather than join it; add them if albums of both show up.

const (
	albumMin    = 4
	albumWindow = 60 // seconds between photos of one album
	albumGap    = 2  // between tiles, 1/96 inch
)

func albumable(m *store.Message) bool {
	return m.Type == "image" && strings.TrimSpace(m.Body) == "" && m.QuotedID == ""
}

// joins reports whether photo m, sent right after prev, belongs to prev's album.
func joins(prev, m *store.Message) bool {
	return albumable(prev) && albumable(m) && prev.FromMe == m.FromMe && prev.SenderJID == m.SenderJID &&
		m.Timestamp >= prev.Timestamp && m.Timestamp-prev.Timestamp <= albumWindow
}

// albumLen is how many messages from msgs[i] on are an album led by it, 0 when it leads none.
func albumLen(msgs []store.Message, i int) int {
	if !albumable(&msgs[i]) || (i > 0 && joins(&msgs[i-1], &msgs[i])) {
		return 0
	}
	n := 1
	for i+n < len(msgs) && joins(&msgs[i+n-1], &msgs[i+n]) {
		n++
	}
	if n < albumMin {
		return 0
	}
	return n
}

// albumTiles are the four tiles of an album's grid r.
func albumTiles(r walk.Rectangle, gap int) [4]walk.Rectangle {
	w, h := (r.Width-gap)/2, (r.Height-gap)/2
	var t [4]walk.Rectangle
	for k := range t {
		t[k] = walk.Rectangle{X: r.X + k%2*(w+gap), Y: r.Y + k/2*(h+gap), Width: w, Height: h}
	}
	t[1].Width, t[3].Width = r.Width-w-gap, r.Width-w-gap // odd pixels go to the right and bottom
	t[2].Height, t[3].Height = r.Height-h-gap, r.Height-h-gap
	return t
}

// albumPill is the "download all" button in the middle of grid r.
func albumPill(r walk.Rectangle, dpi int) walk.Rectangle {
	s := scaler(dpi)
	w, h := min(s(150), r.Width-s(16)), s(56)
	return walk.Rectangle{X: r.X + (r.Width-w)/2, Y: r.Y + (r.Height-h)/2, Width: w, Height: h}
}

// albumHit is the member under (x, y) of an album bubble b (y from the bubble's top): the tile's
// index, or all true on the download pill (shown while pill is true); ok is false off the grid.
func albumHit(b bubble, x, y, dpi int, pill bool) (k int, all, ok bool) {
	if !in(b.thumb, x, y) {
		return 0, false, false
	}
	if pill && in(albumPill(b.thumb, dpi), x, y) {
		return 0, true, true
	}
	for k, t := range albumTiles(b.thumb, scaler(dpi)(albumGap)) {
		if in(t, x, y) {
			return k, false, true
		}
	}
	return 0, false, false // the gap between tiles
}

// albumLabel is what the download pill says: the album's size, then how many photos.
func albumLabel(bytes uint64, n int) (size, count string) {
	return humanSize(bytes), plural(n, "photo", "photos")
}

// albumMissing reports whether any photo of album is still to download: the grid then shows the
// download pill.
func albumMissing(album []store.Message) bool {
	for _, m := range album {
		if needsDownload(m) {
			return true
		}
	}
	return false
}

// paintAlbum paints album bubble b's grid at r: the first four photos, "+N" over the fourth for the
// rest, and while any is not downloaded the pill that downloads them all.
func (v *ChatView) paintAlbum(c *walk.Canvas, r walk.Rectangle, b bubble, album []store.Message) error {
	dpi, p := v.canvas.DPI(), v.pal
	s := scaler(dpi)
	white := walk.RGB(255, 255, 255)
	var errs []error
	for k, t := range albumTiles(r, s(albumGap)) {
		if bmp := v.thumbFor(&album[k], b.albumPreview[k], t.Width, t.Height); bmp != nil {
			errs = append(errs, c.DrawImageStretchedPixels(bmp, t))
		} else {
			errs = append(errs, icons.FillRounded(c, t, s(4), theme.Color(p.ChatBg)))
		}
		if k == 3 && len(album) > 4 {
			errs = append(errs, icons.FillARGB(c, t, 0x99000000),
				drawText(c, dpi, "+"+strconv.Itoa(len(album)-4), 16, 0, white, t, centerText))
		}
	}
	if albumMissing(album) {
		pill := albumPill(r, dpi)
		size, count := albumLabel(b.albumBytes, len(album))
		icon := square(pill.X+s(30), pill.Y+pill.Height/2, s(24))
		text := walk.Rectangle{X: icon.X + icon.Width + s(10), Y: pill.Y + s(8), Width: pill.X + pill.Width - icon.X - icon.Width - s(18), Height: s(20)}
		errs = append(errs, icons.FillRoundedARGB(c, pill, pill.Height/2, 0xcc000000),
			icons.Draw(c, icons.IconDownload, icon, white),
			drawText(c, dpi, size, bodyPt, walk.FontBold, white, text, leftText))
		text.Y += s(20)
		errs = append(errs, drawText(c, dpi, count, bodyPt, 0, white, text, leftText))
	}
	return errors.Join(errs...)
}
