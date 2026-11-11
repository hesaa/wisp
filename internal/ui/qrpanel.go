package ui

import (
	"context"
	"errors"

	"github.com/lxn/walk"
	"go.mau.fi/whatsmeow"
	"rsc.io/qr"

	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
)

// PairFunc starts linking a phone, as whatsapp.Manager.Pair does: the channel carries the QR codes
// and then the outcome. It blocks on the network, so the panel calls it off the UI thread.
type PairFunc func(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error)

// qrSide is the code's square in 1/96 inch: 5 px modules for a typical code at 96 DPI.
const qrSide = 310

const (
	qrQuiet   = 4  // the white margin around a QR code, in modules, that scanners need
	qrTypical = 53 // modules across a WhatsApp code (version 9); the placeholder takes its size
)

// qrView is what the QR panel shows at one moment.
type qrView struct {
	code   string // QR payload to draw; "" for none
	status string // the line under the steps
	failed bool   // offer Try again
	done   bool   // linked: the app swaps the page
}

// qrStep is the view for an item of the pairing channel; ok is false for items the panel does not
// handle (passkey prompts).
func qrStep(it whatsmeow.QRChannelItem) (v qrView, ok bool) {
	failed := func(status string) (qrView, bool) { return qrView{failed: true, status: status}, true }
	switch it.Event {
	case whatsmeow.QRChannelEventCode:
		return qrView{code: it.Code}, true
	case whatsmeow.QRChannelSuccess.Event:
		return qrView{done: true}, true
	case whatsmeow.QRChannelTimeout.Event:
		return failed("The code expired before it was scanned. Try again.")
	case whatsmeow.QRChannelEventError:
		if it.Error == nil {
			return failed("Linking failed. Try again.")
		}
		return failed("Linking failed: " + it.Error.Error())
	case whatsmeow.QRChannelClientOutdated.Event:
		return failed("This version of Wisp is too old to link a phone.")
	case whatsmeow.QRChannelScannedWithoutMultidevice.Event:
		return failed("Turn on multi-device on your phone, then try again.")
	case whatsmeow.QRChannelErrUnexpectedEvent.Event:
		return failed("Linking failed. Try again.")
	}
	return qrView{}, false
}

// pairing runs one pairing attempt; call it off the UI thread. It calls pair and hands each view to
// show through deliver (the UI thread's Synchronize) until the outcome, the channel closing (a
// failure) or ctx ending; nothing is shown once ctx has ended.
func pairing(ctx context.Context, pair PairFunc, deliver func(func()), show func(qrView)) {
	send := func(v qrView) {
		if ctx.Err() == nil {
			deliver(func() { show(v) })
		}
	}
	ch, err := pair(ctx)
	if err != nil {
		send(qrView{failed: true, status: "Could not connect to WhatsApp: " + err.Error()})
		return
	}
	for {
		select {
		case <-ctx.Done():
			return // whatsmeow closes the channel and disconnects the pairing client itself
		case it, open := <-ch:
			if !open {
				send(qrView{failed: true, status: "Linking stopped. Try again."})
				return
			}
			v, ok := qrStep(it)
			if !ok {
				continue
			}
			send(v)
			if v.done || v.failed {
				return
			}
		}
	}
}

// qrRects lays out an n×n QR matrix (black reports a dark module) centred in box: modules are a
// whole number of pixels (at least one), so their edges stay crisp. It returns the white paper,
// quiet zone included, and the dark modules, one rectangle per horizontal run.
func qrRects(n int, black func(x, y int) bool, box walk.Rectangle) (paper walk.Rectangle, dark []walk.Rectangle) {
	m := max(1, min(box.Width, box.Height)/(n+2*qrQuiet))
	side := m * (n + 2*qrQuiet)
	paper = walk.Rectangle{X: box.X + (box.Width-side)/2, Y: box.Y + (box.Height-side)/2, Width: side, Height: side}
	ox, oy := paper.X+qrQuiet*m, paper.Y+qrQuiet*m
	for y := range n {
		for x := 0; x < n; {
			if !black(x, y) {
				x++
				continue
			}
			start := x
			for x < n && black(x, y) {
				x++
			}
			dark = append(dark, walk.Rectangle{X: ox + start*m, Y: oy + y*m, Width: (x - start) * m, Height: m})
		}
	}
	return paper, dark
}

// QRPanel pairs a phone through a PairFunc: it shows each QR code as it comes (always dark on white,
// for scanners), a status line, and on a timeout or error says why and offers Try again. It pairs
// only between Start and Stop. UI thread only.
type QRPanel struct {
	*walk.Composite
	*kit
	pair   PairFunc
	code   *qr.Code // nil: no code to show
	canvas *walk.CustomWidget
	status *label
	retry  *button
	cancel context.CancelFunc // ends the running attempt
	gen    int                // per attempt: views of an earlier one are dropped
}

// NewQRPanel creates the panel in parent; nothing is paired until Start.
func NewQRPanel(parent walk.Container, p theme.Palette, pair PairFunc) (*QRPanel, error) {
	c, err := walk.NewComposite(parent)
	if err != nil {
		return nil, err
	}
	q := &QRPanel{Composite: c, kit: &kit{pal: p}, pair: pair}
	l := walk.NewVBoxLayout()
	if err := errors.Join(setBox(c, l, walk.Margins{}), l.SetSpacing(10), q.themed(c, false)); err != nil {
		return nil, err
	}
	if _, err := walk.NewVSpacer(c); err != nil {
		return nil, err
	}
	cw, err := newCanvas(c, q.paintCode)
	if err != nil {
		return nil, err
	}
	q.canvas = cw
	if err := errors.Join(cw.SetMinMaxSize(walk.Size{Height: qrSide}, walk.Size{Height: qrSide}),
		cw.Accessibility().SetRole(walk.AccRoleGraphic), cw.Accessibility().SetName("QR code to link your phone")); err != nil {
		return nil, err
	}
	if _, err = q.newLabel(c, "The code refreshes on its own", 10, 0, toneSecondary, 24); err != nil {
		return nil, err
	}
	if q.status, err = q.newLabel(c, "", 9, 0, toneSecondary, 40); err != nil {
		return nil, err
	}
	row, err := q.newRow(c, fieldH)
	if err != nil {
		return nil, err
	}
	if _, err := walk.NewHSpacer(row); err != nil {
		return nil, err
	}
	if q.retry, err = q.newButton(row, "Try again", btnPrimary, 110, q.Start); err != nil {
		return nil, err
	}
	if _, err := walk.NewHSpacer(row); err != nil {
		return nil, err
	}
	q.retry.SetVisible(false)
	if _, err := walk.NewVSpacer(c); err != nil {
		return nil, err
	}
	return q, nil
}

// SetPalette recolours the panel; the code itself stays dark on white.
func (q *QRPanel) SetPalette(p theme.Palette) error { return q.setPalette(p) }

// Start begins a pairing attempt, ending the one before.
func (q *QRPanel) Start() {
	q.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	q.cancel = cancel
	gen := q.gen
	q.show(qrView{status: "Connecting to WhatsApp…"})
	go pairing(ctx, q.pair, q.Synchronize, func(v qrView) {
		if gen == q.gen { // still this attempt
			q.show(v)
		}
	})
}

// Stop ends the running attempt; views it already queued are dropped.
func (q *QRPanel) Stop() {
	if q.cancel != nil {
		q.cancel()
		q.cancel = nil
	}
	q.gen++
}

func (q *QRPanel) show(v qrView) {
	q.code = nil
	if v.done {
		v = qrView{status: "Linked."}
	}
	if v.code != "" {
		c, err := qr.Encode(v.code, qr.L)
		if err != nil {
			v = qrView{failed: true, status: "The code could not be drawn: " + err.Error()}
		} else {
			q.code = c
		}
	}
	tone := toneSecondary
	if v.failed {
		tone = toneError
	}
	q.status.set(v.status, tone)
	q.retry.SetVisible(v.failed)
	logErr("pairing repaint", q.canvas.Invalidate())
	if v.failed {
		focusOn(q.retry)
	}
}

// paintCode draws the code dark on white in both themes (scanners need the contrast), or a quiet
// placeholder of the same size while there is none.
func (q *QRPanel) paintCode(c *walk.Canvas, _ walk.Rectangle) error {
	b := q.canvas.ClientBoundsPixels()
	bg, err := brushFor(q.pal.Sidebar)
	if err != nil {
		return err
	}
	if err := c.FillRectanglePixels(bg, b); err != nil {
		return err
	}
	side := min(b.Width, b.Height)
	box := walk.Rectangle{X: (b.Width - side) / 2, Y: (b.Height - side) / 2, Width: side, Height: side}
	if q.code == nil {
		paper, _ := qrRects(qrTypical, func(int, int) bool { return false }, box)
		return icons.FillRounded(c, paper, q.canvas.IntFrom96DPI(12), theme.Color(q.pal.Header))
	}
	white, err := brushFor("#ffffff")
	if err != nil {
		return err
	}
	black, err := brushFor("#000000")
	if err != nil {
		return err
	}
	paper, dark := qrRects(q.code.Size, q.code.Black, box)
	if err := c.FillRectanglePixels(white, paper); err != nil {
		return err
	}
	for _, r := range dark {
		if err := c.FillRectanglePixels(black, r); err != nil {
			return err
		}
	}
	return nil
}
