// Package theme holds the dark/light palettes and the Windows dark title bar call.
package theme

import (
	"fmt"
	"strconv"
	"unsafe"

	"github.com/lxn/walk"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

type ThemeMode int

const (
	ThemeSystem ThemeMode = iota
	ThemeDark
	ThemeLight
)

// Palette colours are "#rrggbb" strings; use Color to get a walk.Color.
type Palette struct {
	ChatBg         string
	Rail           string
	Sidebar        string
	Header         string // panel headers and bottom input bar
	OutgoingBubble string
	IncomingBubble string
	Text           string
	TextSecondary  string
	Accent         string // unread badges, Send button
	TickRead       string // read-receipt double tick (WhatsApp read-blue)
}

var dark = Palette{
	ChatBg: "#0b141a", Rail: "#111b21", Sidebar: "#111b21", Header: "#202c33",
	OutgoingBubble: "#005c4b", IncomingBubble: "#202c33",
	Text: "#e9edef", TextSecondary: "#8696a0", Accent: "#00a884", TickRead: "#53bdeb",
}

var light = Palette{
	ChatBg: "#efeae2", Rail: "#111b21", Sidebar: "#ffffff", Header: "#f0f2f5",
	OutgoingBubble: "#d9fdd3", IncomingBubble: "#ffffff",
	Text: "#111b21", TextSecondary: "#667781", Accent: "#00a884", TickRead: "#53bdeb",
}

// Resolve turns ThemeSystem into ThemeDark or ThemeLight; other modes pass through.
// Missing or unreadable registry value falls back to Light.
func Resolve(m ThemeMode) ThemeMode {
	if m != ThemeSystem {
		return m
	}
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return ThemeLight
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	if err == nil && v == 0 {
		return ThemeDark
	}
	return ThemeLight
}

func GetPalette(m ThemeMode) Palette {
	if Resolve(m) == ThemeDark {
		return dark
	}
	return light
}

// Color converts a "#rrggbb" palette entry; malformed input yields black.
func Color(hex string) walk.Color {
	if len(hex) != 7 || hex[0] != '#' {
		return walk.RGB(0, 0, 0)
	}
	n, err := strconv.ParseUint(hex[1:], 16, 24)
	if err != nil {
		return walk.RGB(0, 0, 0)
	}
	return walk.RGB(byte(n>>16), byte(n>>8), byte(n))
}

var procSetWindowAttribute = windows.NewLazySystemDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute")

// ApplyTitleBar switches the native title bar of hwnd to dark or light.
// Attribute 20 is DWMWA_USE_IMMERSIVE_DARK_MODE; builds before Windows 10 20H1 use 19.
func ApplyTitleBar(hwnd uintptr, dark bool) error {
	var v int32
	if dark {
		v = 1
	}
	var hr uintptr
	for _, attr := range []uintptr{20, 19} {
		hr, _, _ = procSetWindowAttribute.Call(hwnd, attr, uintptr(unsafe.Pointer(&v)), 4)
		if hr == 0 {
			return nil
		}
	}
	return fmt.Errorf("DwmSetWindowAttribute failed: HRESULT 0x%x", hr)
}
