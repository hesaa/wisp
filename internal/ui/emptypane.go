package ui

import (
	"errors"

	"github.com/lxn/walk"

	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
)

// EmptyPane is the chat column's resting state while no chat is open: a chat glyph, the app's name
// and a quiet hint on the chat background. UI thread only.
type EmptyPane struct {
	*walk.CustomWidget
	pal  theme.Palette
	hint string
}

// NewEmptyPane creates the pane in parent with hint under the app's name.
func NewEmptyPane(parent walk.Container, p theme.Palette, hint string) (*EmptyPane, error) {
	e := &EmptyPane{pal: p}
	cw, err := newCanvas(parent, e.paint)
	if err != nil {
		return nil, err
	}
	e.CustomWidget = cw
	if err := cw.Accessibility().SetRole(walk.AccRoleStatictext); err != nil {
		return nil, err
	}
	e.SetHint(hint)
	return e, nil
}

// SetHint replaces the line under the app's name.
func (e *EmptyPane) SetHint(hint string) {
	e.hint = hint
	logErr("empty pane", errors.Join(e.Accessibility().SetName(hint), e.Invalidate()))
}

// SetPalette recolours the pane.
func (e *EmptyPane) SetPalette(p theme.Palette) error {
	e.pal = p
	return e.Invalidate()
}

func (e *EmptyPane) paint(c *walk.Canvas, _ walk.Rectangle) error {
	b, dpi, p := e.ClientBoundsPixels(), e.DPI(), e.pal
	s := scaler(dpi)
	bg, err := brushFor(p.ChatBg)
	if err != nil {
		return err
	}
	cx, cy := b.Width/2, b.Height/2
	return errors.Join(c.FillRectanglePixels(bg, b),
		icons.Draw(c, icons.IconChat, square(cx, cy-s(52), s(64)), theme.Color(p.TextSecondary)),
		drawText(c, dpi, "wa-go", 20, 0, theme.Color(p.Text), walk.Rectangle{X: s(24), Y: cy - s(12), Width: b.Width - s(48), Height: s(40)}, centerText),
		drawText(c, dpi, e.hint, 10, 0, theme.Color(p.TextSecondary), walk.Rectangle{X: s(24), Y: cy + s(32), Width: b.Width - s(48), Height: s(24)}, centerText))
}
