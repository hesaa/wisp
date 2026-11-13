package ui

import (
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
)

// Sending files without the Attach button: Ctrl+V in the message box with files copied in Explorer,
// or a copied picture (a screenshot, "Copy image"), offers them in a preview (attachpreview.go)
// before anything is sent. The Attach button's picks go through the same preview.

// maxAttachBytes is the biggest file WhatsApp takes as a document.
const maxAttachBytes = 2 << 30

// blockedExts are programs and scripts: what runs when opened. WhatsApp's own apps refuse or warn
// about these, and a chat is how they spread.
var blockedExts = func() map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Fields("exe msi msix appx bat cmd com scr pif cpl dll sys drv ps1 psm1 vbs vbe js jse wsf wsh hta lnk url reg msc jar") {
		m["."+e] = true
	}
	return m
}()

// attachItem is a file offered for sending.
type attachItem struct {
	path, name string
	size       int64
	blocked    string // why it cannot be sent; "": it can
	caption    string // typed in the preview
	temp       bool   // a pasted picture saved by us: removed once sent or declined
}

// attachCheck says why a file cannot be sent, "" when it can.
func attachCheck(name string, size int64, dir bool) string {
	switch {
	case dir:
		return "Folders can't be sent"
	case blockedExts[strings.ToLower(filepath.Ext(name))]:
		return "Programs and scripts can't be sent"
	case size == 0:
		return "The file is empty"
	case size > maxAttachBytes:
		return "Larger than 2 GB"
	}
	return ""
}

func newAttachItem(path string, temp bool) attachItem {
	it := attachItem{path: path, name: filepath.Base(path), temp: temp}
	st, err := os.Stat(path)
	if err != nil {
		it.blocked = "The file can't be read"
		return it
	}
	it.size = st.Size()
	it.blocked = attachCheck(it.name, it.size, st.IsDir())
	return it
}

// Attachment is a file to send and the caption to send it with.
type Attachment struct{ Path, Caption string }

// sendable are the items that can be sent, in order.
func sendable(items []attachItem) []Attachment {
	var out []Attachment
	for _, it := range items {
		if it.blocked == "" {
			out = append(out, Attachment{Path: it.path, Caption: it.caption})
		}
	}
	return out
}

// clipboardItems are the files on the clipboard, or its picture saved as a PNG; none when it holds
// neither (or text as well as a picture: a copied spreadsheet cell is pasted as its text).
func clipboardItems(owner win.HWND) []attachItem {
	if !win.OpenClipboard(owner) {
		return nil
	}
	defer win.CloseClipboard()
	if win.IsClipboardFormatAvailable(win.CF_HDROP) {
		drop := win.HDROP(win.GetClipboardData(win.CF_HDROP))
		if drop == 0 {
			return nil
		}
		var items []attachItem
		for i := uint(0); i < win.DragQueryFile(drop, 0xffffffff, nil, 0); i++ {
			buf := make([]uint16, win.DragQueryFile(drop, i, nil, 0)+1)
			win.DragQueryFile(drop, i, &buf[0], uint(len(buf)))
			items = append(items, newAttachItem(syscall.UTF16ToString(buf), false))
		}
		return items
	}
	if win.IsClipboardFormatAvailable(win.CF_BITMAP) && !win.IsClipboardFormatAvailable(win.CF_UNICODETEXT) {
		path, err := saveBitmapPNG(win.HBITMAP(win.GetClipboardData(win.CF_BITMAP)), os.TempDir())
		if err != nil {
			logErr("pasted picture", err)
			return nil
		}
		it := newAttachItem(path, true)
		it.name = "Pasted picture"
		return []attachItem{it}
	}
	return nil
}

// saveBitmapPNG writes bitmap bmp (not freed: the clipboard owns a pasted one) as a PNG in dir and
// returns its path.
func saveBitmapPNG(bmp win.HBITMAP, dir string) (string, error) {
	var bm win.BITMAP
	if bmp == 0 || win.GetObject(win.HGDIOBJ(bmp), unsafe.Sizeof(bm), unsafe.Pointer(&bm)) == 0 {
		return "", fmt.Errorf("ui: no bitmap to save")
	}
	w, h := int(bm.BmWidth), int(bm.BmHeight)
	if w <= 0 || h <= 0 || w > maxFullPixels/h {
		return "", fmt.Errorf("ui: refusing to save a %dx%d picture", w, h)
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	var bi win.BITMAPINFO // 32-bit pixels, top row first
	bi.BmiHeader = win.BITMAPINFOHEADER{BiSize: uint32(unsafe.Sizeof(bi.BmiHeader)), BiWidth: int32(w), BiHeight: -int32(h), BiPlanes: 1, BiBitCount: 32, BiCompression: win.BI_RGB}
	dc := win.GetDC(0)
	n := win.GetDIBits(dc, bmp, 0, uint32(h), &img.Pix[0], &bi, win.DIB_RGB_COLORS)
	win.ReleaseDC(0, dc)
	if n == 0 {
		return "", fmt.Errorf("ui: reading the bitmap failed")
	}
	for i := 0; i < len(img.Pix); i += 4 { // BGRX to opaque RGBA
		img.Pix[i], img.Pix[i+2], img.Pix[i+3] = img.Pix[i+2], img.Pix[i], 0xff
	}
	f, err := os.CreateTemp(dir, "wisp-paste-*.png")
	if err != nil {
		return "", err
	}
	err = (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(f, img)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// pasteMedia handles a paste into the message box when the clipboard holds files or a picture, and
// reports whether it did: false leaves the paste to the box (text).
func (b *InputBar) pasteMedia() bool {
	items := clipboardItems(b.edit.Handle())
	if len(items) == 0 {
		return false
	}
	b.offer(items)
	return true
}

// offer shows items in the preview and, confirmed, hands the sendable ones to OnAttach.
func (b *InputBar) offer(items []attachItem) {
	items, send, err := previewAttachments(b.Form(), b.pal, items)
	logErr("attachment preview", err)
	done := func() {
		for _, it := range items {
			if it.temp {
				logErr("remove pasted picture", os.Remove(it.path))
			}
		}
	}
	if files := sendable(items); send && len(files) > 0 && b.onAttach != nil {
		b.onAttach(files, done)
	} else {
		done()
	}
	focusOn(b.edit)
}

// pickFile asks for files to send (the Attach button).
func (b *InputBar) pickFile() {
	dlg := walk.FileDialog{Title: "Attach files"}
	ok, err := dlg.ShowOpenMultiple(b.Form())
	if err != nil {
		logErr("attach", err)
		return
	}
	if !ok || len(dlg.FilePaths) == 0 {
		return
	}
	items := make([]attachItem, len(dlg.FilePaths))
	for i, p := range dlg.FilePaths {
		items[i] = newAttachItem(p, false)
	}
	b.offer(items)
}
