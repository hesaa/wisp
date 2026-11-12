package ui

import (
	"cmp"
	"errors"
	"fmt"
	"image"
	"slices"
	"strings"

	"github.com/lxn/walk"
	"wa-go/internal/store"
	"wa-go/internal/theme"
	"wa-go/internal/ui/icons"
)

type InfoMember struct {
	JID   string
	Admin bool
}

type GroupInfo struct {
	Description string
	Members     []InfoMember
}

type rowKind int

const (
	rowTitle rowKind = iota
	rowHeader
	rowPhone
	rowSection
	rowText
	rowMemberCount
	rowMember
	rowLoading
	rowError
)

type infoRow struct {
	kind  rowKind
	text  string
	sub   string
	jid   string
	admin bool
}

// sortMembers sorts members: admins first, then by name case-insensitively.
func sortMembers(ms []InfoMember, d *Directory) []InfoMember {
	res := slices.Clone(ms)
	slices.SortStableFunc(res, func(a, b InfoMember) int {
		if a.Admin != b.Admin {
			if a.Admin {
				return -1
			}
			return 1
		}
		var nameA, nameB string
		if d != nil {
			nameA = d.Name(a.JID, "")
			nameB = d.Name(b.JID, "")
		} else {
			nameA = a.JID
			nameB = b.JID
		}
		return cmp.Compare(strings.ToLower(nameA), strings.ToLower(nameB))
	})
	return res
}

// infoRows computes the ordered rows to draw for chat details.
func infoRows(chat store.Chat, d *Directory, about string, g *GroupInfo, loading, failed bool) []infoRow {
	isGroup := strings.HasSuffix(chat.JID, "@g.us") || g != nil
	var rows []infoRow

	if isGroup {
		rows = append(rows, infoRow{kind: rowTitle, text: "Group info"})
		subject := chat.Name
		if d != nil {
			subject = d.Name(chat.JID, chat.Name)
		}
		rows = append(rows, infoRow{kind: rowHeader, text: subject, jid: chat.JID})

		if g != nil && g.Description != "" {
			rows = append(rows, infoRow{kind: rowSection, text: "Description"})
			rows = append(rows, infoRow{kind: rowText, text: g.Description})
		}
		if g != nil {
			rows = append(rows, infoRow{kind: rowMemberCount, text: fmt.Sprintf("%d members", len(g.Members))})
			sorted := sortMembers(g.Members, d)
			for _, m := range sorted {
				name := m.JID
				if d != nil {
					name = d.Name(m.JID, "")
				}
				rows = append(rows, infoRow{kind: rowMember, text: name, jid: m.JID, admin: m.Admin})
			}
		}
	} else {
		rows = append(rows, infoRow{kind: rowTitle, text: "Contact info"})
		name := chat.Name
		if d != nil {
			name = d.Name(chat.JID, chat.Name)
		}
		rows = append(rows, infoRow{kind: rowHeader, text: name, jid: chat.JID})

		var phone string
		if d != nil {
			if p, ok := d.Person(chat.JID); ok && p.PNJID != "" && !maskedName(p.PNJID) {
				phone = p.PNJID
			}
		}
		if phone == "" && strings.HasSuffix(chat.JID, "@s.whatsapp.net") && !maskedName(chat.JID) {
			phone = chat.JID
		}
		if phone != "" {
			digits, _, _ := strings.Cut(phone, "@")
			rows = append(rows, infoRow{kind: rowPhone, text: "+" + digits})
		}

		if about != "" {
			rows = append(rows, infoRow{kind: rowSection, text: "About"})
			rows = append(rows, infoRow{kind: rowText, text: about})
		}
	}

	if loading {
		rows = append(rows, infoRow{kind: rowLoading, text: "Loading…"})
	}
	if failed {
		rows = append(rows, infoRow{kind: rowError, text: "Couldn't load the details."})
	}

	return rows
}

// InfoView is a composite displaying contact or group details replacing the chat view.
type InfoView struct {
	*walk.Composite
	pal        theme.Palette
	dir        *Directory
	header     *walk.CustomWidget
	canvas     *walk.CustomWidget
	scroll     vscroll
	chat       store.Chat
	openJID    string
	open       bool
	loading    bool
	failed     bool
	about      string
	group      *GroupInfo
	picture    image.Image
	pictureBmp *walk.Bitmap
	selMember  int

	onClose    func()
	onRetry    func(jid string)
	onOpenChat func(jid string)

	// Hit-test state updated on paint
	retryRect  walk.Rectangle
	memberHits []memberHit
	contentH   int
}

type memberHit struct {
	rect walk.Rectangle
	jid  string
}

// NewInfoView creates a new details view composite.
func NewInfoView(parent walk.Container, p theme.Palette) (*InfoView, error) {
	v := &InfoView{pal: p, selMember: -1}
	var err error
	if v.Composite, err = walk.NewComposite(parent); err != nil {
		return nil, err
	}
	layout := walk.NewVBoxLayout()
	if err := errors.Join(layout.SetMargins(walk.Margins{}), layout.SetSpacing(0), v.SetLayout(layout), setBg(v.Composite, p.ChatBg)); err != nil {
		return nil, err
	}

	if v.header, err = newCanvas(v.Composite, v.paintHeader); err != nil {
		return nil, err
	}
	if err := v.header.SetMinMaxSize(walk.Size{Height: 50}, walk.Size{Height: 50}); err != nil {
		return nil, err
	}
	v.header.MouseDown().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton {
			s := scaler(v.header.DPI())
			if x <= s(48) {
				v.Close()
			}
		}
	})

	if v.canvas, err = newCanvas(v.Composite, v.paint); err != nil {
		return nil, err
	}
	if err := errors.Join(fillRest(v.Composite, v.canvas), focusable(v.canvas, walk.AccRolePane, "Details", v.key)); err != nil {
		return nil, err
	}
	v.scroll.attach(v.canvas, func() int { return v.contentH }, func() int { return scaler(v.canvas.DPI())(40) })
	v.canvas.MouseDown().Attach(v.mouseDown)

	return v, nil
}

func (v *InfoView) Open(chat store.Chat, d *Directory) {
	v.open = true
	v.openJID = chat.JID
	v.chat = chat
	v.dir = d
	v.about = ""
	v.group = nil
	if v.pictureBmp != nil {
		v.pictureBmp.Dispose()
		v.pictureBmp = nil
	}
	v.picture = nil
	v.loading = true
	v.failed = false
	v.selMember = -1
	v.scroll.off = 0
	if v.header != nil && v.canvas != nil {
		logErr("info repaint", errors.Join(v.header.Invalidate(), v.canvas.Invalidate()))
	}
}

func (v *InfoView) SetAbout(jid, about string) {
	if !v.open || jid != v.openJID {
		return
	}
	v.about = about
	v.loading = false
	v.failed = false
	if v.canvas != nil {
		logErr("info repaint", v.canvas.Invalidate())
	}
}

func (v *InfoView) SetGroup(jid string, g GroupInfo) {
	if !v.open || jid != v.openJID {
		return
	}
	v.group = &g
	v.loading = false
	v.failed = false
	if v.canvas != nil {
		logErr("info repaint", v.canvas.Invalidate())
	}
}

func (v *InfoView) SetPicture(jid string, img image.Image) {
	if !v.open || jid != v.openJID {
		return
	}
	if v.pictureBmp != nil {
		v.pictureBmp.Dispose()
		v.pictureBmp = nil
	}
	v.picture = img
	if img != nil && v.canvas != nil {
		masked := circleMask(img)
		v.pictureBmp, _ = walk.NewBitmapFromImageForDPI(masked, v.canvas.DPI())
	}
	if v.canvas != nil {
		logErr("info repaint", v.canvas.Invalidate())
	}
}

func (v *InfoView) SetError(jid string) {
	if !v.open || jid != v.openJID {
		return
	}
	v.failed = true
	v.loading = false
	if v.canvas != nil {
		logErr("info repaint", v.canvas.Invalidate())
	}
}

func (v *InfoView) Close() {
	v.open = false
	v.openJID = ""
	if v.pictureBmp != nil {
		v.pictureBmp.Dispose()
		v.pictureBmp = nil
	}
	if v.onClose != nil {
		v.onClose()
	}
}

func (v *InfoView) OnClose(fn func())           { v.onClose = fn }
func (v *InfoView) OnRetry(fn func(jid string)) { v.onRetry = fn }
func (v *InfoView) OnOpenChat(fn func(jid string)) {
	v.onOpenChat = fn
}
func (v *InfoView) SetDirectory(d *Directory) {
	v.dir = d
	if v.canvas != nil {
		logErr("info repaint", v.canvas.Invalidate())
	}
}
// Invalidate repaints the info view: header, canvas, and composite.
func (v *InfoView) Invalidate() error {
	if v == nil {
		return nil
	}
	var errs []error
	if v.header != nil {
		errs = append(errs, v.header.Invalidate())
	}
	if v.canvas != nil {
		errs = append(errs, v.canvas.Invalidate())
	}
	if v.Composite != nil {
		errs = append(errs, v.Composite.Invalidate())
	}
	return errors.Join(errs...)
}

func (v *InfoView) SetPalette(p theme.Palette) error {
	v.pal = p
	var errs []error
	if v.Composite != nil {
		errs = append(errs, setBg(v.Composite, p.ChatBg))
	}
	if v.header != nil {
		errs = append(errs, v.header.Invalidate())
	}
	if v.canvas != nil {
		errs = append(errs, v.canvas.Invalidate())
	}
	return errors.Join(errs...)
}

func (v *InfoView) key(k walk.Key) bool {
	switch k {
	case walk.KeyEscape:
		v.Close()
		return true
	case walk.KeyUp:
		members := v.members()
		if len(members) > 0 {
			if v.selMember > 0 {
				v.selMember--
			} else {
				v.selMember = 0
			}
			logErr("info repaint", v.canvas.Invalidate())
			return true
		}
	case walk.KeyDown:
		members := v.members()
		if len(members) > 0 {
			if v.selMember < len(members)-1 {
				v.selMember++
			} else {
				v.selMember = len(members) - 1
			}
			logErr("info repaint", v.canvas.Invalidate())
			return true
		}
	case walk.KeyReturn, walk.KeySpace:
		if v.failed {
			if v.onRetry != nil && v.openJID != "" {
				v.onRetry(v.openJID)
				return true
			}
		}
		members := v.members()
		if v.selMember >= 0 && v.selMember < len(members) {
			target := members[v.selMember].JID
			v.Close()
			if v.onOpenChat != nil {
				v.onOpenChat(target)
			}
			return true
		}
	}
	return false
}

func (v *InfoView) members() []InfoMember {
	if v.group != nil {
		return sortMembers(v.group.Members, v.dir)
	}
	return nil
}

func (v *InfoView) mouseDown(x, y int, button walk.MouseButton) {
	if button != walk.LeftButton || v.scroll.press(x, y) {
		return
	}
	// Check Retry click
	if v.failed && in(v.retryRect, x, y) {
		if v.onRetry != nil && v.openJID != "" {
			v.onRetry(v.openJID)
		}
		return
	}
	// Check Member click
	for _, m := range v.memberHits {
		if in(m.rect, x, y) {
			v.Close()
			if v.onOpenChat != nil {
				v.onOpenChat(m.jid)
			}
			return
		}
	}
}

func (v *InfoView) paintHeader(c *walk.Canvas, _ walk.Rectangle) error {
	dpi, b, p := v.header.DPI(), v.header.ClientBoundsPixels(), v.pal
	s := scaler(dpi)
	bg, err := brushFor(p.Header)
	if err != nil {
		return err
	}
	if err := c.FillRectanglePixels(bg, b); err != nil {
		return err
	}

	titleText := "Contact info"
	if strings.HasSuffix(v.openJID, "@g.us") || v.group != nil {
		titleText = "Group info"
	}

	backBox := walk.Rectangle{X: s(8), Y: (b.Height - s(32)) / 2, Width: s(32), Height: s(32)}
	titleBox := walk.Rectangle{X: s(48), Y: 0, Width: b.Width - s(56), Height: b.Height}

	return errors.Join(
		icons.Draw(c, icons.IconBack, backBox, theme.Color(p.Text)),
		drawText(c, dpi, titleText, 12, walk.FontBold, theme.Color(p.Text), titleBox, leftText),
	)
}

func (v *InfoView) paint(c *walk.Canvas, _ walk.Rectangle) error {
	dpi, b, p := v.canvas.DPI(), v.canvas.ClientBoundsPixels(), v.pal
	s := scaler(dpi)
	bg, err := brushFor(p.ChatBg)
	if err != nil {
		return err
	}
	if err := c.FillRectanglePixels(bg, b); err != nil {
		return err
	}

	if !v.open {
		return nil
	}

	rows := infoRows(v.chat, v.dir, v.about, v.group, v.loading, v.failed)
	v.memberHits = v.memberHits[:0]
	v.retryRect = walk.Rectangle{}

	y := -v.scroll.off + s(20)
	padX := s(24)
	contentW := b.Width - 2*padX
	textCol := theme.Color(p.Text)
	secCol := theme.Color(p.TextSecondary)

	var errs []error

	// 1. Large 80 px avatar
	avSize := s(80)
	avRect := walk.Rectangle{X: (b.Width - avSize) / 2, Y: y, Width: avSize, Height: avSize}
	if v.pictureBmp != nil {
		errs = append(errs, c.DrawImageStretchedPixels(v.pictureBmp, avRect))
	} else {
		name := v.chat.Name
		if v.dir != nil {
			name = v.dir.Name(v.chat.JID, v.chat.Name)
		}
		errs = append(errs, drawAvatar(c, dpi, avRect, name, v.chat.JID, 24))
	}
	y += avSize + s(16)

	// Draw subsequent rows
	for _, r := range rows {
		switch r.kind {
		case rowHeader:
			rH := s(28)
			errs = append(errs, drawText(c, dpi, r.text, 14, walk.FontBold, textCol, walk.Rectangle{X: padX, Y: y, Width: contentW, Height: rH}, centerText))
			y += rH + s(4)
		case rowPhone:
			rH := s(20)
			errs = append(errs, drawText(c, dpi, r.text, 10, 0, secCol, walk.Rectangle{X: padX, Y: y, Width: contentW, Height: rH}, centerText))
			y += rH + s(16)
		case rowSection:
			rH := s(22)
			errs = append(errs, drawText(c, dpi, r.text, 10, walk.FontBold, theme.Color(p.Accent), walk.Rectangle{X: padX, Y: y, Width: contentW, Height: rH}, leftText))
			y += rH + s(4)
		case rowText:
			rH := s(22)
			errs = append(errs, drawText(c, dpi, r.text, 10, 0, textCol, walk.Rectangle{X: padX, Y: y, Width: contentW, Height: rH}, leftText))
			y += rH + s(16)
		case rowMemberCount:
			rH := s(22)
			errs = append(errs, drawText(c, dpi, r.text, 10, walk.FontBold, secCol, walk.Rectangle{X: padX, Y: y, Width: contentW, Height: rH}, leftText))
			y += rH + s(8)
		case rowMember:
			rowH := s(44)
			rowRect := walk.Rectangle{X: padX, Y: y, Width: contentW, Height: rowH}
			v.memberHits = append(v.memberHits, memberHit{rect: rowRect, jid: r.jid})

			// Highlight if selected
			memberIdx := len(v.memberHits) - 1
			if memberIdx == v.selMember {
				selBrush, _ := brushFor(p.Header)
				if selBrush != nil {
					_ = c.FillRectanglePixels(selBrush, rowRect)
				}
			}

			// Avatar (32 px)
			mAv := walk.Rectangle{X: rowRect.X + s(4), Y: rowRect.Y + (rowH-s(32))/2, Width: s(32), Height: s(32)}
			errs = append(errs, drawAvatar(c, dpi, mAv, r.text, r.jid, 10))

			// Name
			nameW := contentW - s(48)
			if r.admin {
				nameW -= s(60)
			}
			nameRect := walk.Rectangle{X: rowRect.X + s(44), Y: rowRect.Y, Width: nameW, Height: rowH}
			errs = append(errs, drawText(c, dpi, r.text, 10, 0, textCol, nameRect, leftText))

			// Admin badge
			if r.admin {
				badgeW, badgeH := s(48), s(18)
				badgeRect := walk.Rectangle{X: rowRect.X + contentW - badgeW - s(4), Y: rowRect.Y + (rowH-badgeH)/2, Width: badgeW, Height: badgeH}
				_ = icons.FillRounded(c, badgeRect, s(4), theme.Color(p.Header))
				errs = append(errs, drawText(c, dpi, "Admin", 8, walk.FontBold, secCol, badgeRect, centerText))
			}
			y += rowH + s(2)
		case rowLoading:
			rH := s(30)
			errs = append(errs, drawText(c, dpi, r.text, 10, walk.FontItalic, secCol, walk.Rectangle{X: padX, Y: y, Width: contentW, Height: rH}, centerText))
			y += rH + s(10)
		case rowError:
			rH := s(24)
			errs = append(errs, drawText(c, dpi, r.text, 10, 0, secCol, walk.Rectangle{X: padX, Y: y, Width: contentW, Height: rH}, centerText))
			y += rH + s(8)
			btnW, btnH := s(80), s(28)
			btnRect := walk.Rectangle{X: (b.Width - btnW) / 2, Y: y, Width: btnW, Height: btnH}
			v.retryRect = btnRect
			_ = icons.FillRounded(c, btnRect, s(4), theme.Color(p.Accent))
			errs = append(errs, drawText(c, dpi, "Retry", 9, walk.FontBold, walk.RGB(255, 255, 255), btnRect, centerText))
			y += btnH + s(16)
		}
	}

	v.contentH = y + v.scroll.off + s(40)
	if t := v.scroll.thumb(); t.Height > 0 {
		errs = append(errs, icons.Fill(c, t, theme.Color(p.TextSecondary)))
	}
	return errors.Join(errs...)
}
