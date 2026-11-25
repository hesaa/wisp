package ui

import (
	"fmt"
	"math"
	"runtime"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"

	"wa-go/internal/theme"
)

// Colour emoji. GDI's DrawText, which draws all other text, renders Segoe UI Emoji as monochrome
// outlines; only DirectWrite draws its colour glyphs. So text that holds an emoji is drawn by
// Direct2D/DirectWrite onto the same device context (drawText falls back to GDI if that fails).
// The factories load on the first emoji drawn, not at startup. UI thread only.

// hasEmoji reports whether s holds a character Segoe UI Emoji draws in colour: the pictograph
// planes, the dingbat and symbol blocks, or anything the emoji selector (U+FE0F) follows.
func hasEmoji(s string) bool {
	for _, r := range s {
		switch {
		case r < 0x203c:
		case r >= 0x1f000 && r <= 0x1faff, r >= 0x2600 && r <= 0x27bf, r == 0xfe0f,
			r >= 0x231a && r <= 0x23ff, r >= 0x2b00 && r <= 0x2bff, r == 0x203c, r == 0x2049:
			return true
		}
	}
	return false
}

var (
	d2d1                = windows.NewLazySystemDLL("d2d1.dll")
	dwrite              = windows.NewLazySystemDLL("dwrite.dll")
	gdi32               = windows.NewLazySystemDLL("gdi32.dll")
	pD2D1CreateFactory  = d2d1.NewProc("D2D1CreateFactory")
	pDWriteCreateFactry = dwrite.NewProc("DWriteCreateFactory")
	pGetClipBox         = gdi32.NewProc("GetClipBox")

	iidD2D1Factory   = windows.GUID{Data1: 0x06152247, Data2: 0x6f50, Data3: 0x465a, Data4: [8]byte{0x92, 0x45, 0x11, 0x8b, 0xfd, 0x3b, 0x60, 0x07}}
	iidDWriteFactory = windows.GUID{Data1: 0xb859ee5a, Data2: 0xd838, Data3: 0x4b5b, Data4: [8]byte{0xa2, 0xe8, 0x1a, 0xdc, 0x7d, 0x93, 0xdb, 0x48}}
)

// Vtable slots of the COM methods used (d2d1.h, dwrite.h).
const (
	comRelease = 2

	d2dFactoryCreateDCRenderTarget = 16
	rtCreateSolidColorBrush        = 8
	rtDrawTextLayout               = 28
	rtBeginDraw                    = 48
	rtEndDraw                      = 49
	rtBindDC                       = 57

	dwFactoryCreateTextFormat   = 15
	dwFactoryCreateTextLayout   = 18
	dwFactoryCreateEllipsisSign = 20
	layoutSetFontFamilyName     = 31
	fmtSetTextAlignment         = 3
	fmtSetParagraphAlignment    = 4
	fmtSetWordWrapping          = 5
	fmtSetTrimming              = 9

	d2dRecreateTarget = 0x8899000c // D2DERR_RECREATE_TARGET
)

// com calls method slot of COM object obj and turns a failed HRESULT into an error.
// The directive makes pointers converted to uintptr in a call's arguments escape to the heap and stay
// alive for the call: a stack-allocated out-parameter would otherwise go stale if the stack moved.
//
//go:uintptrescapes
func com(obj unsafe.Pointer, slot int, args ...uintptr) error {
	vt := *(**[64]uintptr)(obj)
	hr, _, _ := syscall.SyscallN(vt[slot], append([]uintptr{uintptr(obj)}, args...)...)
	if int32(hr) < 0 {
		return fmt.Errorf("ui: COM method %d failed: 0x%08x", slot, uint32(hr))
	}
	return nil
}

type textFormatKey struct {
	px             float32
	style          walk.FontStyle
	align, valign  uintptr
	wrap, ellipsis bool
}

// colour is the Direct2D state, made on first use and kept for the run.
var colour struct {
	d2d, dw, rt unsafe.Pointer
	formats     map[textFormatKey]unsafe.Pointer // a handful: the sizes and alignments the UI uses
}

func colourInit() error {
	c := &colour
	if c.d2d == nil {
		if hr, _, _ := pD2D1CreateFactory.Call(0, uintptr(unsafe.Pointer(&iidD2D1Factory)), 0, uintptr(unsafe.Pointer(&c.d2d))); int32(hr) < 0 {
			return fmt.Errorf("ui: D2D1CreateFactory: 0x%08x", uint32(hr))
		}
	}
	if c.dw == nil {
		if hr, _, _ := pDWriteCreateFactry.Call(0, uintptr(unsafe.Pointer(&iidDWriteFactory)), uintptr(unsafe.Pointer(&c.dw))); int32(hr) < 0 {
			return fmt.Errorf("ui: DWriteCreateFactory: 0x%08x", uint32(hr))
		}
		c.formats = map[textFormatKey]unsafe.Pointer{}
	}
	if c.rt == nil {
		// B8G8R8A8, alpha ignored (what a GDI surface is), 96 DPI so its units are pixels.
		// D2D1_RENDER_TARGET_TYPE_SOFTWARE (1) prevents Direct2D from initializing
		// Direct3D 11 and loading GPU vendor display drivers (e.g. Intel igc64/igd10,
		// NVIDIA/AMD) which consume 60-100+ MB of working set and commit memory.
		props := struct {
			typ, format, alpha uint32
			dpiX, dpiY         float32
			usage, minLevel    uint32
		}{typ: 1, format: 87, alpha: 3, dpiX: 96, dpiY: 96}
		err := com(c.d2d, d2dFactoryCreateDCRenderTarget, uintptr(unsafe.Pointer(&props)), uintptr(unsafe.Pointer(&c.rt)))
		runtime.KeepAlive(&props)
		return err
	}
	return nil
}

// textFormat is the cached DirectWrite format of Segoe UI at px pixels for a DrawText format.
func textFormat(px float32, style walk.FontStyle, format walk.DrawTextFormat) (unsafe.Pointer, error) {
	k := textFormatKey{px: px, style: style, wrap: format&walk.TextWordbreak != 0, ellipsis: format&walk.TextEndEllipsis != 0}
	switch {
	case format&walk.TextCenter != 0:
		k.align = 2
	case format&walk.TextRight != 0:
		k.align = 1
	}
	if format&walk.TextVCenter != 0 {
		k.valign = 2
	}
	if f, ok := colour.formats[k]; ok {
		return f, nil
	}
	family, locale := syscall.StringToUTF16Ptr("Segoe UI"), syscall.StringToUTF16Ptr("")
	weight, slant := uintptr(400), uintptr(0)
	if style&walk.FontBold != 0 {
		weight = 700
	}
	if style&walk.FontItalic != 0 {
		slant = 2
	}
	var f unsafe.Pointer
	// the size is a float passed on the stack (the seventh argument): its bits go in as they are
	err := com(colour.dw, dwFactoryCreateTextFormat, uintptr(unsafe.Pointer(family)), 0, weight, slant, 5,
		uintptr(math.Float32bits(px)), uintptr(unsafe.Pointer(locale)), uintptr(unsafe.Pointer(&f)))
	runtime.KeepAlive(family)
	runtime.KeepAlive(locale)
	if err != nil {
		return nil, err
	}
	wrap := uintptr(1) // no wrap
	if k.wrap {
		wrap = 0
	}
	for _, set := range [][2]uintptr{{fmtSetTextAlignment, k.align}, {fmtSetParagraphAlignment, k.valign}, {fmtSetWordWrapping, wrap}} {
		if err := com(f, int(set[0]), set[1]); err != nil {
			com(f, comRelease)
			return nil, err
		}
	}
	if k.ellipsis {
		var sign unsafe.Pointer
		trim := [3]uint32{1} // by character, no delimiter
		err := com(colour.dw, dwFactoryCreateEllipsisSign, uintptr(f), uintptr(unsafe.Pointer(&sign)))
		if err == nil {
			err = com(f, fmtSetTrimming, uintptr(unsafe.Pointer(&trim)), uintptr(sign))
			com(sign, comRelease) // the format holds it now
		}
		runtime.KeepAlive(&trim)
		if err != nil {
			com(f, comRelease)
			return nil, err
		}
	}
	colour.formats[k] = f
	return f, nil
}

// drawColorText draws text like DrawText does with format, in Segoe UI at px pixels, over what hdc
// already shows in r (logical pixels), with emoji in colour.
func drawColorText(hdc win.HDC, text string, px float32, style walk.FontStyle, color walk.Color, r walk.Rectangle, format walk.DrawTextFormat) error {
	if err := colourInit(); err != nil {
		return err
	}
	tf, err := textFormat(px, style, format)
	if err != nil {
		return err
	}
	// Direct2D draws to its own surface and copies it to the DC: bind it to the visible part of r
	// only (logical pixels, as GDI takes them), and lay the text out relative to that part.
	var clip win.RECT
	if kind, _, _ := pGetClipBox.Call(uintptr(hdc), uintptr(unsafe.Pointer(&clip))); kind <= 1 { // error, or nothing visible
		return nil
	}
	left, top := max(int32(r.X), clip.Left), max(int32(r.Y), clip.Top)
	right, bottom := min(int32(r.X+r.Width), clip.Right), min(int32(r.Y+r.Height), clip.Bottom)
	if left >= right || top >= bottom {
		return nil
	}
	bound := win.RECT{Left: left, Top: top, Right: right, Bottom: bottom}
	rt := colour.rt
	if err := com(rt, rtBindDC, uintptr(hdc), uintptr(unsafe.Pointer(&bound))); err != nil {
		return err
	}
	rgba := [4]float32{float32(color.R()) / 255, float32(color.G()) / 255, float32(color.B()) / 255, 1}
	var brush unsafe.Pointer
	if err := com(rt, rtCreateSolidColorBrush, uintptr(unsafe.Pointer(&rgba)), 0, uintptr(unsafe.Pointer(&brush))); err != nil {
		return err
	}
	defer com(brush, comRelease)
	units := utf16.Encode([]rune(text))
	if len(units) == 0 {
		return nil
	}
	// A layout, so that every emoji can be given the emoji font. Left to font fallback, a symbol the
	// text font has a plain glyph of its own for (♥, ☺) is drawn with that one. Sizes are floats
	// passed on the stack: their bits go in as they are.
	var tl unsafe.Pointer
	if err := com(colour.dw, dwFactoryCreateTextLayout, uintptr(unsafe.Pointer(&units[0])), uintptr(len(units)), uintptr(tf),
		uintptr(math.Float32bits(float32(r.Width))), uintptr(math.Float32bits(float32(r.Height))), uintptr(unsafe.Pointer(&tl))); err != nil {
		return err
	}
	defer com(tl, comRelease)
	for i := 0; i < len(units); {
		_, k := cluster(units, i)
		if hasEmoji(string(utf16.Decode(units[i : i+k]))) {
			// the range {start, length} is an 8-byte struct passed by value: one register
			if err := com(tl, layoutSetFontFamilyName, uintptr(unsafe.Pointer(emojiFamily)), uintptr(i)|uintptr(k)<<32); err != nil {
				return err
			}
		}
		i += k
	}
	// the origin {x, y} is two floats passed by value: one register too
	origin := uintptr(math.Float32bits(float32(int32(r.X)-left))) | uintptr(math.Float32bits(float32(int32(r.Y)-top)))<<32
	const clipToRect, colourFont = 2, 4
	vt := *(**[64]uintptr)(rt)
	syscall.SyscallN(vt[rtBeginDraw], uintptr(rt)) // BeginDraw and DrawTextLayout return nothing: EndDraw reports
	syscall.SyscallN(vt[rtDrawTextLayout], uintptr(rt), origin, uintptr(tl), uintptr(brush), clipToRect|colourFont)
	hr, _, _ := syscall.SyscallN(vt[rtEndDraw], uintptr(rt), 0, 0)
	runtime.KeepAlive(&bound)
	runtime.KeepAlive(&rgba)
	runtime.KeepAlive(units)
	if uint32(hr) == d2dRecreateTarget { // the display device changed: make a new target next time
		com(rt, comRelease)
		colour.rt = nil
	}
	if int32(hr) < 0 {
		return fmt.Errorf("ui: Direct2D EndDraw: 0x%08x", uint32(hr))
	}
	return nil
}

var (
	user32          = windows.NewLazySystemDLL("user32.dll")
	pFillRect       = user32.NewProc("FillRect")
	pHideCaret      = user32.NewProc("HideCaret")
	pShowCaret      = user32.NewProc("ShowCaret")
	pCreateSolidBr  = gdi32.NewProc("CreateSolidBrush")
	pRestoreDC      = gdi32.NewProc("RestoreDC")
	pGetSysColorRGB = user32.NewProc("GetSysColor")
)

// paintEmojiOver paints the emoji of edit control h in colour on hdc (the control's DC, or one it was
// printed to) over the outlines the control itself drew: the control cannot draw colour fonts, so
// it keeps doing the editing and this redraws each emoji where EM_POSFROMCHAR says it is. bg and fg
// are the control's colours; a selected emoji gets the system highlight colours, as its text does.
func paintEmojiOver(h win.HWND, hdc win.HDC, bg, fg walk.Color) error {
	n := int(win.SendMessage(h, win.WM_GETTEXTLENGTH, 0, 0))
	font := win.HGDIOBJ(win.SendMessage(h, win.WM_GETFONT, 0, 0))
	var lf win.LOGFONT
	if n == 0 || font == 0 || win.GetObject(font, unsafe.Sizeof(lf), unsafe.Pointer(&lf)) == 0 {
		return nil
	}
	units := make([]uint16, n+1)
	units = units[:win.SendMessage(h, win.WM_GETTEXT, uintptr(n+1), uintptr(unsafe.Pointer(&units[0])))]
	px, lh := float32(max(lf.LfHeight, -lf.LfHeight)), lineHeight(h)
	var area win.RECT
	sendPtr(h, win.EM_GETRECT, 0, uintptr(unsafe.Pointer(&area)))
	var selA, selB uint32
	sendPtr(h, win.EM_GETSEL, uintptr(unsafe.Pointer(&selA)), uintptr(unsafe.Pointer(&selB)))
	if win.GetFocus() != h {
		selA, selB = 0, 0 // the selection shows only while the control has the focus
	}
	saved := win.SaveDC(hdc)
	defer pRestoreDC.Call(uintptr(hdc), uintptr(saved))
	win.IntersectClipRect(hdc, area.Left, area.Top, area.Right, area.Bottom)
	win.SelectObject(hdc, font) // to measure with
	for i := 0; i < len(units); {
		_, k := cluster(units, i)
		text := string(utf16.Decode(units[i : i+k]))
		pos := uint32(win.SendMessage(h, win.EM_POSFROMCHAR, uintptr(i), 0))
		x, y := int(int16(pos)), int(int16(pos>>16))
		if hasEmoji(text) && pos != 0xffffffff && y+lh > int(area.Top) && y < int(area.Bottom) {
			var m win.RECT
			win.DrawTextEx(hdc, &units[i], int32(k), &m, win.DT_CALCRECT|win.DT_SINGLELINE|win.DT_NOPREFIX, nil)
			cell := win.RECT{Left: int32(x), Top: int32(y), Right: int32(x) + m.Right - m.Left, Bottom: int32(y + lh)}
			back, ink := bg, fg
			if uint32(i) >= selA && uint32(i) < selB {
				b, f := sysColor(win.COLOR_HIGHLIGHT), sysColor(win.COLOR_HIGHLIGHTTEXT)
				back, ink = b, f
			}
			brush, _, _ := pCreateSolidBr.Call(uintptr(back))
			pFillRect.Call(uintptr(hdc), uintptr(unsafe.Pointer(&cell)), brush)
			win.DeleteObject(win.HGDIOBJ(brush))
			r := walk.Rectangle{X: x, Y: y, Width: int(cell.Right - cell.Left), Height: lh}
			if err := drawColorText(hdc, text, px, 0, ink, r, walk.TextSingleLine|walk.TextNoPrefix); err != nil {
				return err
			}
		}
		i += k
	}
	return nil
}

func sysColor(index int) walk.Color {
	c, _, _ := pGetSysColorRGB.Call(uintptr(index))
	return walk.Color(c)
}

// overEmoji repaints the emoji of the message box in colour (paintEmojiOver), right after the box
// drew itself.
func (b *InputBar) overEmoji() {
	if colourFailed || !hasEmoji(b.edit.Text()) {
		return
	}
	h := b.edit.Handle()
	hdc := win.GetDC(h)
	if hdc == 0 {
		return
	}
	defer win.ReleaseDC(h, hdc)
	pHideCaret.Call(uintptr(h)) // painting over a shown caret would leave its ghost behind
	defer pShowCaret.Call(uintptr(h))
	if err := paintEmojiOver(h, hdc, theme.Color(b.pal.Sidebar), theme.Color(b.pal.Text)); err != nil {
		colourFailed = true
		logErr("colour emoji in the message box, left as outlines", err)
	}
}

// emojiFamily is the font every emoji is drawn with.
var emojiFamily = syscall.StringToUTF16Ptr("Segoe UI Emoji")

// sendPtr is SendMessage for messages that carry pointers (see com on the directive).
//
//go:uintptrescapes
func sendPtr(h win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	return win.SendMessage(h, msg, wParam, lParam)
}
