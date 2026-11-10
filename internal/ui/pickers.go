package ui

import (
	"cmp"
	"context"
	"errors"
	"strings"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"

	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
)

// emojis is the picker's one grid, ten by ten. Symbols that default to text style carry U+FE0F so
// phones show them as emoji.
var emojis = []string{
	"😀", "😃", "😄", "😁", "😆", "😅", "😂", "🤣", "😊", "😇",
	"🙂", "🙃", "😉", "😌", "😍", "🥰", "😘", "😋", "😛", "😜",
	"🤪", "🤨", "🤓", "😎", "🤩", "🥳", "😏", "😒", "😞", "😔",
	"😟", "😕", "🙁", "😣", "😫", "😩", "🥺", "😢", "😭", "😤",
	"😠", "😡", "🤯", "😳", "😱", "😨", "😰", "🤗", "🤔", "🤭",
	"🤫", "😶", "😐", "😑", "😬", "🙄", "😴", "🤤", "😷", "🤒",
	"👍", "👎", "👌", "✌️", "🤞", "🤟", "🤘", "👏", "🙌", "🙏",
	"👋", "💪", "👀", "🤝", "✋", "👉", "👈", "👆", "👇", "☝️",
	"❤️", "🧡", "💛", "💚", "💙", "💜", "🖤", "💔", "💯", "🔥",
	"✨", "🎉", "🎂", "🎁", "🌹", "🌸", "☀️", "🌙", "⭐", "✅",
}

// Popup sizes in 1/96 inch.
const (
	gridPad    = 6 // around a grid's cells
	emojiCell  = 36
	gifCellW   = 150
	gifCellH   = 112
	gifColumns = 3
	gifFooter  = 22
)

const (
	gifDebounce = 400 * time.Millisecond // a search starts this long after the last keystroke
	gifNoKey    = "GIF search needs a GIPHY API key (Settings › Media & Storage)"
)

// pickEmoji opens the emoji popup; a picked emoji goes in at the caret (replacing any selection)
// and the focus returns to the box.
func (b *InputBar) pickEmoji() {
	if b.closePopup() {
		return
	}
	pal := b.pal
	side := 10*emojiCell + 2*gridPad + 2
	p, err := b.newPopup(b.emoji, "Emoji", side, side)
	if err != nil {
		logErr("emoji popup", err)
		return
	}
	defer p.Dispose()
	grid, err := newCellGrid(p.Dialog, &pal, "Emoji", 10, emojiCell, emojiCell)
	if err != nil {
		logErr("emoji popup", err)
		return
	}
	grid.n = len(emojis)
	grid.paintCell = func(c *walk.Canvas, i int, r walk.Rectangle) error {
		return drawText(c, grid.DPI(), emojis[i], 16, 0, theme.Color(pal.Text), r, centerText)
	}
	grid.describe = func(i int) string { return emojis[i] }
	picked := -1
	grid.pick = func(i int) {
		picked = i
		p.Accept()
	}
	p.Starting().Attach(func() { logErr("emoji focus", grid.SetFocus()) })
	b.runPopup(p)
	if picked >= 0 {
		b.edit.ReplaceSelectedText(emojis[picked], true)
		logErr("focus", b.edit.SetFocus())
	}
}

// gifPicker is the open GIF popup. UI-thread only.
type gifPicker struct {
	bar      *InputBar
	search   *searchBox
	grid     *cellGrid
	life     context.Context    // ends when the popup closes
	ctx      context.Context    // the shown search's: ends when another replaces it
	cancel   context.CancelFunc // of ctx
	gen      int                // per search: answers to an older one are dropped
	typed    int                // per keystroke: a debounce expiry already queued is dropped
	debounce func() bool        // the pending search while typing; nil otherwise
	results  []gifResult
	bmps     []*walk.Bitmap // previews by result, nil until fetched
}

// pickGIF opens the GIF search popup; a picked GIF goes to OnSendGIF and the focus returns to the box.
func (b *InputBar) pickGIF() {
	if b.closePopup() {
		return
	}
	pal := b.pal
	p, err := b.newPopup(b.gif, "GIF search", gifColumns*gifCellW+2*gridPad+2, 48+3*gifCellH+2*gridPad+gifFooter+2)
	if err != nil {
		logErr("GIF popup", err)
		return
	}
	defer p.Dispose()
	life, end := context.WithCancel(context.Background())
	g := &gifPicker{bar: b, life: life}
	g.ctx, g.cancel = context.WithCancel(life)
	defer func() {
		end()
		g.close()
	}()
	var footer *walk.CustomWidget
	if g.search, err = newSearchBox(p.Dialog, &pal, "Search GIPHY"); err == nil {
		g.grid, err = newCellGrid(p.Dialog, &pal, "GIFs", gifColumns, gifCellW, gifCellH)
	}
	if err == nil { // the attribution GIPHY's API terms ask for
		footer, err = newCanvas(p.Dialog, func(c *walk.Canvas, _ walk.Rectangle) error {
			r := footer.ClientBoundsPixels()
			bg, err := brushFor(pal.Sidebar)
			if err != nil {
				return err
			}
			r.Width -= footer.IntFrom96DPI(10)
			return errors.Join(c.FillRectanglePixels(bg, footer.ClientBoundsPixels()),
				drawText(c, footer.DPI(), "Powered by GIPHY", 8, walk.FontBold, theme.Color(pal.TextSecondary), r, rightText))
		})
	}
	if err == nil {
		err = footer.SetMinMaxSize(walk.Size{Height: gifFooter}, walk.Size{Height: gifFooter})
	}
	if err = errors.Join(err, fillRest(p.Dialog, g.grid), g.search.applyPalette()); err != nil {
		logErr("GIF popup", err)
		return
	}
	g.grid.paintCell = g.paintCell
	g.grid.describe = func(i int) string { return cmp.Or(g.results[i].desc, "GIF") }
	picked := ""
	g.grid.pick = func(i int) {
		picked = g.results[i].mp4
		p.Accept()
	}
	g.show("Type to search GIFs")
	if b.apiKey() == "" {
		g.show(gifNoKey)
	}
	g.search.edit.TextChanged().Attach(g.typedKey)
	g.search.edit.KeyDown().Attach(func(k walk.Key) {
		switch {
		case k == walk.KeyReturn:
			g.run()
		case k == walk.KeyDown && g.grid.n > 0:
			logErr("focus", g.grid.SetFocus())
			showFocusCues(g.grid.Handle())
		}
	})
	p.Starting().Attach(func() { logErr("GIF focus", g.search.edit.SetFocus()) })
	b.runPopup(p)
	if picked != "" && b.onSendGIF != nil {
		b.onSendGIF(picked)
		logErr("focus", b.edit.SetFocus())
	}
}

// typedKey restarts the debounce: the search runs gifDebounce after the last keystroke.
func (g *gifPicker) typedKey() {
	if g.debounce != nil {
		g.debounce()
	}
	g.typed++
	typed := g.typed
	g.debounce = g.bar.after(gifDebounce, func() {
		if typed == g.typed && g.life.Err() == nil {
			g.run()
		}
	})
}

// run searches for the box's text now, replacing the shown results.
func (g *gifPicker) run() {
	if g.debounce != nil {
		g.debounce()
		g.debounce = nil
	}
	g.typed++
	q, key := strings.TrimSpace(g.search.edit.Text()), g.bar.apiKey()
	switch {
	case key == "":
		g.show(gifNoKey)
		return
	case q == "":
		g.show("Type to search GIFs")
		return
	}
	g.show("Searching…")
	ctx, gen := g.ctx, g.gen
	go func() {
		res, err := g.bar.gifs.search(ctx, key, q)
		g.bar.Synchronize(func() {
			if gen != g.gen || g.life.Err() != nil {
				return
			}
			switch {
			case errors.Is(err, errNoGIFKey):
				g.show(gifNoKey)
			case err != nil:
				logErr("GIF search", err)
				g.show("GIF search failed")
			case len(res) == 0:
				g.show("No GIFs found")
			default:
				g.results, g.bmps = res, make([]*walk.Bitmap, len(res))
				g.grid.setItems(len(res))
				g.fetchPreviews(ctx)
			}
		})
	}()
}

// show replaces the results with msg and starts a new search generation: older answers and
// previews are dropped and their requests cancelled.
func (g *gifPicker) show(msg string) {
	g.cancel()
	g.ctx, g.cancel = context.WithCancel(g.life)
	g.gen++
	g.release()
	g.results, g.grid.empty = nil, msg
	g.grid.setItems(0)
}

// fetchPreviews fetches and decodes the results' first frames off the UI thread, four at a time;
// each is turned into a bitmap on the UI thread as it arrives.
func (g *gifPicker) fetchPreviews(ctx context.Context) {
	gen, dpi := g.gen, g.grid.DPI()
	w, h := walk.IntFrom96DPI(gifCellW-4, dpi), walk.IntFrom96DPI(gifCellH-4, dpi)
	slots := make(chan struct{}, 4)
	for i, r := range g.results {
		go func() {
			slots <- struct{}{}
			img, err := g.bar.gifs.preview(ctx, r.preview, w, h)
			<-slots
			if ctx.Err() != nil {
				return
			}
			g.bar.Synchronize(func() {
				if gen != g.gen || g.life.Err() != nil {
					return
				}
				var bmp *walk.Bitmap
				if err == nil {
					bmp, err = walk.NewBitmapFromImageForDPI(img, dpi)
				}
				if err != nil {
					logErr("GIF preview", err)
					return
				}
				g.bmps[i] = bmp
				logErr("GIF repaint", g.grid.Invalidate())
			})
		}()
	}
}

func (g *gifPicker) paintCell(c *walk.Canvas, i int, r walk.Rectangle) error {
	s := scaler(g.grid.DPI())
	in := walk.Rectangle{X: r.X + s(2), Y: r.Y + s(2), Width: r.Width - s(4), Height: r.Height - s(4)}
	if bmp := g.bmps[i]; bmp != nil {
		return c.DrawImageStretchedPixels(bmp, in)
	}
	return icons.FillRounded(c, in, s(6), theme.Color(g.grid.pal.Header))
}

// release disposes of the previews.
func (g *gifPicker) release() {
	for _, bmp := range g.bmps {
		if bmp != nil {
			bmp.Dispose()
		}
	}
	g.bmps = nil
}

// close stops the debounce timer and disposes of the previews; the popup's life has ended, so
// late answers are dropped.
func (g *gifPicker) close() {
	if g.debounce != nil {
		g.debounce()
	}
	g.cancel()
	g.release()
}

// popup is a borderless modal dialog shown above (else below) the button that opened it.
type popup struct {
	*walk.Dialog
	esc bool // closed with Esc
}

// newPopup makes a w x h (1/96 inch) popup for opener with a 1px border; run it with runPopup.
func (b *InputBar) newPopup(opener *walk.CustomWidget, title string, w, h int) (*popup, error) {
	owner := opener.Form()
	dlg, err := walk.NewDialogWithFixedSize(owner)
	if err != nil {
		return nil, err
	}
	p := &popup{Dialog: dlg}
	hwnd := dlg.Handle()
	style := uint32(win.GetWindowLong(hwnd, win.GWL_STYLE))
	if win.SetWindowLong(hwnd, win.GWL_STYLE, int32(style&^(win.WS_CAPTION|win.WS_SYSMENU)|win.WS_POPUP)) == 0 ||
		!win.SetWindowPos(hwnd, 0, 0, 0, 0, 0, win.SWP_NOMOVE|win.SWP_NOSIZE|win.SWP_NOZORDER|win.SWP_FRAMECHANGED) {
		dlg.Dispose()
		return nil, errors.New("ui: making the popup borderless failed")
	}
	// walk routes Esc (IDCANCEL) only to a cancel button; this one stays hidden
	cancel, err := walk.NewPushButton(dlg)
	if err == nil {
		cancel.SetVisible(false)
		cancel.Clicked().Attach(func() {
			p.esc = true
			dlg.Cancel()
		})
		err = errors.Join(dlg.SetCancelButton(cancel), setBg(dlg, b.pal.TextSecondary), // the border
			setBox(dlg, walk.NewVBoxLayout(), walk.Margins{HNear: 1, VNear: 1, HFar: 1, VFar: 1}),
			dlg.SetMinMaxSize(walk.Size{Width: w, Height: h}, walk.Size{Width: w, Height: h}), dlg.SetTitle(title))
	}
	if err != nil {
		dlg.Dispose()
		return nil, err
	}
	dlg.Starting().Attach(func() {
		var r win.RECT
		var mi win.MONITORINFO
		mi.CbSize = uint32(unsafe.Sizeof(mi))
		if win.GetWindowRect(opener.Handle(), &r) && win.GetMonitorInfo(win.MonitorFromWindow(opener.Handle(), win.MONITOR_DEFAULTTONEAREST), &mi) {
			anchor := walk.Rectangle{X: int(r.Left), Y: int(r.Top), Width: int(r.Right - r.Left), Height: int(r.Bottom - r.Top)}
			wa := mi.RcWork
			logErr("popup position", dlg.SetBoundsPixels(placePopup(anchor, dlg.SizePixels(),
				walk.Rectangle{X: int(wa.Left), Y: int(wa.Top), Width: int(wa.Right - wa.Left), Height: int(wa.Bottom - wa.Top)})))
		}
		// a click elsewhere in the window closes the popup (below), so the window stays usable
		win.EnableWindow(owner.Handle(), true)
	})
	dlg.Deactivating().Attach(func() {
		b.Synchronize(func() { // after the activation settles; never close inside WM_ACTIVATE
			if !dlg.IsDisposed() && win.GetActiveWindow() == owner.Handle() {
				dlg.Cancel()
			}
		})
	})
	// focus cues follow the user's setting and last input (hidden after a click until a key is used)
	dlg.SendMessage(win.WM_CHANGEUISTATE, win.UIS_INITIALIZE, 0)
	return p, nil
}

// runPopup shows p until it closes; after Esc the focus goes back to the button that opened it.
func (b *InputBar) runPopup(p *popup) {
	b.popup = p.Dialog
	defer func() { b.popup = nil }()
	opener := win.GetFocus()
	p.Run()
	if p.esc {
		win.SetFocus(opener)
	}
}

// closePopup closes the open popup, if any, and reports whether there was one: a click on a popup
// button while a popup is open only closes it.
func (b *InputBar) closePopup() bool {
	if b.popup == nil {
		return false
	}
	b.popup.Cancel()
	return true
}

// placePopup puts a popup of size above anchor, left-aligned with it, else below it, else at the
// bottom of work; always inside work. All in screen pixels.
func placePopup(anchor walk.Rectangle, size walk.Size, work walk.Rectangle) walk.Rectangle {
	r := walk.Rectangle{X: anchor.X, Y: anchor.Y - size.Height, Width: size.Width, Height: size.Height}
	bottom := work.Y + work.Height
	if r.Y < work.Y {
		r.Y = anchor.Y + anchor.Height
		if r.Y+r.Height > bottom {
			r.Y = bottom - r.Height
		}
	}
	r.X = max(work.X, min(r.X, work.X+work.Width-r.Width))
	r.Y = max(work.Y, r.Y)
	return r
}

// cellGrid is a focusable, scrolling grid of n cells, cols to a row, painted by paintCell: arrows,
// PgUp/PgDn and Home/End move the focus, Enter, Space or a click pick a cell. With no cells it
// shows empty.
type cellGrid struct {
	*walk.CustomWidget
	vscroll
	pal          *theme.Palette
	name         string // accessible name
	n, cols      int
	cellW, cellH int // 1/96 inch
	focus        int // -1 for none
	empty        string
	paintCell    func(c *walk.Canvas, i int, r walk.Rectangle) error
	describe     func(i int) string
	pick         func(i int)
}

func newCellGrid(parent walk.Container, pal *theme.Palette, name string, cols, cellW, cellH int) (*cellGrid, error) {
	g := &cellGrid{pal: pal, name: name, cols: cols, cellW: cellW, cellH: cellH, focus: -1}
	cw, err := newCanvas(parent, g.paint)
	if err != nil {
		return nil, err
	}
	g.CustomWidget = cw
	if err := focusable(cw, walk.AccRoleList, name, g.key); err != nil {
		return nil, err
	}
	g.attach(cw, func() int { r := g.rows(); return 2*r.pad + r.n*r.size }, func() int { return g.rows().size })
	cw.MouseDown().Attach(g.mouseDown)
	cw.FocusedChanged().Attach(func() {
		if r := g.rows().firstFull(); cw.Focused() && g.focus < 0 && r >= 0 {
			g.setFocus(r * g.cols) // no scrolling: a click may be resolving its cell against this offset
		}
	})
	tipOnHover(cw, func(x, y int) string {
		if i := g.at(x, y); i >= 0 {
			return g.describe(i)
		}
		return ""
	})
	return g, nil
}

// rows is the grid's rows as a scrolled span.
func (g *cellGrid) rows() span {
	s := scaler(g.DPI())
	return span{off: g.off, pad: s(gridPad), size: s(g.cellH), n: (g.n + g.cols - 1) / g.cols}
}

// at is the cell under (x, y), -1 for none.
func (g *cellGrid) at(x, y int) int {
	s := scaler(g.DPI())
	row, col := g.rows().at(y), span{pad: s(gridPad), size: s(g.cellW), n: g.cols}.at(x)
	if row < 0 || col < 0 || row*g.cols+col >= g.n {
		return -1
	}
	return row*g.cols + col
}

// cell is cell i's rectangle in the view.
func (g *cellGrid) cell(i int) walk.Rectangle {
	s := scaler(g.DPI())
	return walk.Rectangle{X: s(gridPad) + i%g.cols*s(g.cellW), Y: s(gridPad) + i/g.cols*s(g.cellH) - g.off, Width: s(g.cellW), Height: s(g.cellH)}
}

// setItems shows n cells from the top, none focused.
func (g *cellGrid) setItems(n int) {
	g.n, g.focus, g.off = n, -1, 0
	logErr("grid repaint", g.Invalidate())
}

func (g *cellGrid) setFocus(i int) {
	g.focus = i
	logErr("grid repaint", g.Invalidate())
	announce(g, g.name, g.describe(i))
}

func (g *cellGrid) key(k walk.Key) bool {
	if k == walk.KeyReturn || k == walk.KeySpace {
		if g.focus >= 0 && g.focus < g.n {
			g.pick(g.focus)
		}
		return true
	}
	r := g.rows()
	next, ok := gridKey(g.focus, g.n, g.cols, max(1, g.view()/r.size)*g.cols, k)
	if ok && next >= 0 {
		g.setFocus(next)
		g.scrollTo(scrollIntoView(g.off, r.pad+next/g.cols*r.size, r.size, g.view()))
	}
	return ok
}

func (g *cellGrid) mouseDown(x, y int, b walk.MouseButton) {
	if b != walk.LeftButton || g.press(x, y) {
		return
	}
	// resolve the cell against the offset the user saw, then take focus, then pick it
	i := g.at(x, y)
	if i >= 0 {
		g.setFocus(i)
	}
	logErr("focus", g.SetFocus())
	if i >= 0 {
		g.pick(i)
	}
}

func (g *cellGrid) paint(c *walk.Canvas, ub walk.Rectangle) error {
	p, b := g.pal, g.ClientBoundsPixels()
	s := scaler(g.DPI())
	g.off = g.clamp(g.off)
	bg, err := brushFor(p.Sidebar)
	if err != nil {
		return err
	}
	if err := c.FillRectanglePixels(bg, ub); err != nil {
		return err
	}
	if g.n == 0 {
		r := walk.Rectangle{X: s(24), Y: s(40), Width: b.Width - s(48), Height: b.Height - s(48)}
		return drawText(c, g.DPI(), g.empty, 10, 0, theme.Color(p.TextSecondary), r, walk.TextCenter|walk.TextWordbreak|walk.TextNoPrefix)
	}
	rows, focused := g.rows(), focusCues(g)
	first := max(0, (ub.Y+g.off-rows.pad)/rows.size)
	for i := first * g.cols; i < g.n; i++ {
		r := g.cell(i)
		if r.Y >= ub.Y+ub.Height {
			break
		}
		if err := g.paintCell(c, i, r); err != nil {
			return err
		}
		if focused && i == g.focus {
			if err := focusRect(c, r, s(2), p.Text); err != nil {
				return err
			}
		}
	}
	if t := g.thumb(); t.Height > 0 {
		return icons.Fill(c, t, theme.Color(p.TextSecondary))
	}
	return nil
}

// gridKey is where key k moves the focus from cell cur (-1: none) in a grid of n cells, cols to a
// row: Left/Right step, Up/Down move a row (onto the last cell from above a short last row),
// PgUp/PgDn move page cells, Home/End jump to the ends; nothing wraps. ok is false for other keys.
func gridKey(cur, n, cols, page int, k walk.Key) (next int, ok bool) {
	if n <= 0 || k != walk.KeyUp && k != walk.KeyDown {
		return navigate(cur, n, page, k, false)
	}
	switch {
	case cur < 0:
		return 0, true
	case k == walk.KeyUp:
		if cur < cols {
			return cur, true
		}
		return cur - cols, true
	case cur/cols == (n-1)/cols: // already on the last row
		return cur, true
	}
	return min(cur+cols, n-1), true
}
