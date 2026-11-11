package ui

import (
	"errors"

	"github.com/lxn/walk"

	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
)

// linkSteps are the numbered instructions left of the code.
var linkSteps = []string{
	"1. Open WhatsApp on your phone",
	"2. Tap Settings, then Linked devices",
	"3. Tap Link a device",
	"4. Point your phone at this screen",
}

// LinkPage is the full-window page that links a phone: the logo, the steps and a QR panel. It
// pairs only while shown. UI thread only.
type LinkPage struct {
	*walk.Composite
	*kit
	qr       *QRPanel
	cancel   *button
	onCancel func()
	logo     *walk.Icon // nil: a glyph stands in
	shown    bool
}

// NewLinkPage creates the page in parent, hidden.
func NewLinkPage(parent walk.Container, p theme.Palette, pair PairFunc) (*LinkPage, error) {
	c, err := walk.NewComposite(parent)
	if err != nil {
		return nil, err
	}
	g := &LinkPage{Composite: c, kit: &kit{pal: p}}
	l := walk.NewHBoxLayout()
	if err := errors.Join(setBox(c, l, walk.Margins{HNear: 64, VNear: 32, HFar: 64, VFar: 32}), l.SetSpacing(48), g.themed(c, false)); err != nil {
		return nil, err
	}
	left, err := walk.NewComposite(c)
	if err != nil {
		return nil, err
	}
	ll := walk.NewVBoxLayout()
	if err := errors.Join(setBox(left, ll, walk.Margins{}), ll.SetSpacing(8), g.themed(left, false)); err != nil {
		return nil, err
	}
	if _, err := walk.NewVSpacer(left); err != nil {
		return nil, err
	}
	g.logo, err = walk.NewIconFromResourceWithSize("APP", walk.Size{Width: 128, Height: 128})
	logErr("link page logo", err) // the glyph stands in
	if err := g.newLogo(left); err != nil {
		return nil, err
	}
	if _, err := g.newLabel(left, "Link your phone to Wisp", 20, walk.FontBold, toneText, 56); err != nil {
		return nil, err
	}
	for _, s := range linkSteps {
		if _, err := g.newLabel(left, s, 11, 0, toneText, 30); err != nil {
			return nil, err
		}
	}
	if _, err := g.newLabel(left, "Your phone doesn't need to stay online after linking.", 10, 0, toneSecondary, 44); err != nil {
		return nil, err
	}
	row, err := g.newRow(left, fieldH)
	if err != nil {
		return nil, err
	}
	if g.cancel, err = g.newButton(row, "Cancel", btnPlain, 100, func() {
		if g.onCancel != nil {
			g.onCancel()
		}
	}); err != nil {
		return nil, err
	}
	if _, err := walk.NewHSpacer(row); err != nil {
		return nil, err
	}
	g.cancel.SetVisible(false)
	if _, err := walk.NewVSpacer(left); err != nil {
		return nil, err
	}
	if g.qr, err = NewQRPanel(c, p, pair); err != nil {
		return nil, err
	}
	if err := errors.Join(g.qr.SetMinMaxSize(walk.Size{Width: 340}, walk.Size{Width: 340}), l.SetStretchFactor(left, 1)); err != nil {
		return nil, err
	}
	c.SetVisible(false)
	return g, nil
}

func (g *LinkPage) newLogo(parent walk.Container) error {
	var cw *walk.CustomWidget
	cw, err := newCanvas(parent, func(c *walk.Canvas, _ walk.Rectangle) error {
		b := cw.ClientBoundsPixels()
		bg, err := brushFor(g.pal.Sidebar)
		if err != nil {
			return err
		}
		box := walk.Rectangle{X: 0, Y: 0, Width: b.Height, Height: b.Height}
		if g.logo == nil {
			return errors.Join(c.FillRectanglePixels(bg, b), icons.Draw(c, icons.IconChat, box, theme.Color(g.pal.Accent)))
		}
		return errors.Join(c.FillRectanglePixels(bg, b), c.DrawImageStretchedPixels(g.logo, box))
	})
	if err != nil {
		return err
	}
	g.canvases = append(g.canvases, cw)
	return errors.Join(cw.SetMinMaxSize(walk.Size{Height: 64}, walk.Size{Height: 64}), cw.Accessibility().SetRole(walk.AccRoleGraphic),
		cw.Accessibility().SetName("Wisp logo"))
}

// OnCancel sets what the Cancel button does.
func (g *LinkPage) OnCancel(fn func()) { g.onCancel = fn }

// Show makes the page visible and starts pairing; with cancellable, a Cancel button is offered.
// Showing it again only updates Cancel.
func (g *LinkPage) Show(cancellable bool) {
	g.cancel.SetVisible(cancellable)
	if g.shown {
		return
	}
	g.shown = true
	g.SetVisible(true)
	g.qr.Start()
	switch {
	case cancellable:
		focusOn(g.cancel)
	case g.qr.retry.Visible():
		focusOn(g.qr.retry)
	}
}

// Hide hides the page and stops pairing.
func (g *LinkPage) Hide() {
	if !g.shown {
		return
	}
	g.shown = false
	g.qr.Stop()
	g.SetVisible(false)
}

// SetPalette recolours the page and its QR panel.
func (g *LinkPage) SetPalette(p theme.Palette) error {
	return errors.Join(g.setPalette(p), g.qr.SetPalette(p))
}
