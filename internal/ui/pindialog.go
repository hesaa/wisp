package ui

import (
	"errors"

	"github.com/lxn/walk"
	"github.com/lxn/win"

	"wa-go/internal/theme"
)

// askPIN asks for PINs in a small modal dialog over owner, themed in p: one masked, digits-only
// field per label, then Cancel and OK. Tab moves between the fields, Enter in one is OK and Esc
// cancels. It reports the entered texts in label order, and whether OK was chosen. The texts are
// handed back and kept nowhere else.
func askPIN(owner walk.Form, p theme.Palette, title string, labels ...string) ([]string, bool, error) {
	dlg, err := walk.NewDialogWithFixedSize(owner)
	if err != nil {
		return nil, false, err
	}
	defer dlg.Dispose()
	k := &kit{pal: p}
	h := 150 + 62*len(labels)
	l := walk.NewVBoxLayout()
	if err := errors.Join(dlg.SetTitle(title), setBox(dlg, l, walk.Margins{HNear: 24, VNear: 20, HFar: 24, VFar: 20}), l.SetSpacing(8),
		k.themed(dlg, false), dlg.SetMinMaxSize(walk.Size{Width: 400, Height: h}, walk.Size{Width: 400, Height: h})); err != nil {
		return nil, false, err
	}
	edits := make([]*walk.LineEdit, len(labels))
	out := make([]string, len(labels))
	accept := func() { // read the fields first: closing the dialog destroys them, and Text is "" after that
		for i, e := range edits {
			out[i] = e.Text()
		}
		dlg.Accept()
	}
	for i, name := range labels {
		if _, err := k.newLabel(dlg, name, 10, 0, toneSecondary, 22); err != nil {
			return nil, false, err
		}
		if edits[i], err = pinField(k, dlg, name, accept); err != nil {
			return nil, false, err
		}
	}
	spacer, err := walk.NewVSpacer(dlg)
	if err != nil {
		return nil, false, err
	}
	row, err := k.newRow(dlg, fieldH)
	if err != nil {
		return nil, false, err
	}
	gap, err := walk.NewHSpacer(row)
	if err != nil {
		return nil, false, err
	}
	if _, err := k.newButton(row, "Cancel", btnPlain, 100, dlg.Cancel); err != nil {
		return nil, false, err
	}
	if _, err := k.newButton(row, "OK", btnPrimary, 100, accept); err != nil {
		return nil, false, err
	}
	if err := errors.Join(fillRest(dlg, spacer), fillRest(row, gap), escCloses(dlg)); err != nil {
		return nil, false, err
	}
	dlg.Starting().Attach(func() { focusOn(edits[0]) })
	dlg.SendMessage(win.WM_CHANGEUISTATE, win.UIS_INITIALIZE, 0)
	logErr("PIN dialog title bar", titleBar(dlg, isDark(p)))
	if dlg.Run() != walk.DlgCmdOK {
		return nil, false, nil
	}
	return out, true, nil
}

// pinField is a masked field taking up to 8 digits; Enter in it calls enter.
func pinField(k *kit, parent walk.Container, name string, enter func()) (*walk.LineEdit, error) {
	pill, err := walk.NewComposite(parent)
	if err != nil {
		return nil, err
	}
	l := walk.NewHBoxLayout()
	if err := errors.Join(setBox(pill, l, walk.Margins{HNear: 12, HFar: 12}), l.SetAlignment(walk.AlignHNearVCenter), k.themed(pill, true)); err != nil {
		return nil, err
	}
	if _, err := walk.NewVSpacerFixed(pill, fieldH); err != nil {
		return nil, err
	}
	edit, err := walk.NewLineEdit(pill)
	if err != nil {
		return nil, err
	}
	edit.SetFont(font(10, 0))
	edit.SetTextColor(theme.Color(k.pal.Text))
	edit.SetPasswordMode(true)
	edit.SetMaxLength(8)
	k.edits = append(k.edits, edit)
	style := win.GetWindowLong(edit.Handle(), win.GWL_STYLE)
	if win.SetWindowLong(edit.Handle(), win.GWL_STYLE, style|win.ES_NUMBER) == 0 {
		logErr("PIN field style", errors.New("ui: SetWindowLong(GWL_STYLE) failed"))
	}
	edit.KeyDown().Attach(func(key walk.Key) {
		if key == walk.KeyReturn {
			enter()
		}
	})
	return edit, errors.Join(noBorder(edit), edit.Accessibility().SetName(name), k.themed(edit, true))
}

// askCustomTimeout opens a modal dialog to enter a custom timeout in minutes.
func askCustomTimeout(owner walk.Form, p theme.Palette, currentVal string) (string, bool, error) {
	dlg, err := walk.NewDialogWithFixedSize(owner)
	if err != nil {
		return "", false, err
	}
	defer dlg.Dispose()
	k := &kit{pal: p}
	h := 180
	l := walk.NewVBoxLayout()
	if err := errors.Join(dlg.SetTitle("Auto-lock timeout"), setBox(dlg, l, walk.Margins{HNear: 24, VNear: 20, HFar: 24, VFar: 20}), l.SetSpacing(8),
		k.themed(dlg, false), dlg.SetMinMaxSize(walk.Size{Width: 380, Height: h}, walk.Size{Width: 380, Height: h})); err != nil {
		return "", false, err
	}
	if _, err := k.newLabel(dlg, "Timeout in minutes (0 for immediately):", 10, 0, toneSecondary, 22); err != nil {
		return "", false, err
	}
	pill, err := walk.NewComposite(dlg)
	if err != nil {
		return "", false, err
	}
	pl := walk.NewHBoxLayout()
	if err := errors.Join(setBox(pill, pl, walk.Margins{HNear: 12, HFar: 12}), pl.SetAlignment(walk.AlignHNearVCenter), k.themed(pill, true)); err != nil {
		return "", false, err
	}
	if _, err := walk.NewVSpacerFixed(pill, fieldH); err != nil {
		return "", false, err
	}
	edit, err := walk.NewLineEdit(pill)
	if err != nil {
		return "", false, err
	}
	edit.SetFont(font(10, 0))
	edit.SetTextColor(theme.Color(k.pal.Text))
	edit.SetMaxLength(5)
	k.edits = append(k.edits, edit)
	style := win.GetWindowLong(edit.Handle(), win.GWL_STYLE)
	if win.SetWindowLong(edit.Handle(), win.GWL_STYLE, style|win.ES_NUMBER) == 0 {
		logErr("timeout field style", errors.New("ui: SetWindowLong(GWL_STYLE) failed"))
	}
	edit.SetText(currentVal)
	var out string
	accept := func() {
		out = edit.Text()
		dlg.Accept()
	}
	edit.KeyDown().Attach(func(key walk.Key) {
		if key == walk.KeyReturn {
			accept()
		}
	})

	spacer, err := walk.NewVSpacer(dlg)
	if err != nil {
		return "", false, err
	}
	row, err := k.newRow(dlg, fieldH)
	if err != nil {
		return "", false, err
	}
	gap, err := walk.NewHSpacer(row)
	if err != nil {
		return "", false, err
	}
	if _, err := k.newButton(row, "Cancel", btnPlain, 100, dlg.Cancel); err != nil {
		return "", false, err
	}
	if _, err := k.newButton(row, "OK", btnPrimary, 100, accept); err != nil {
		return "", false, err
	}
	if err := errors.Join(fillRest(dlg, spacer), fillRest(row, gap), escCloses(dlg), noBorder(edit), k.themed(edit, true)); err != nil {
		return "", false, err
	}
	dlg.Starting().Attach(func() {
		focusOn(edit)
		edit.SetTextSelection(0, -1)
	})
	dlg.SendMessage(win.WM_CHANGEUISTATE, win.UIS_INITIALIZE, 0)
	logErr("timeout dialog title bar", titleBar(dlg, isDark(p)))
	if dlg.Run() != walk.DlgCmdOK {
		return "", false, nil
	}
	norm, err := validateCustomTimeout(out)
	if err != nil {
		return "", false, err
	}
	return norm, true, nil
}
