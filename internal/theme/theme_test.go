package theme

import (
	"regexp"
	"testing"

	"github.com/lxn/walk"
)

var hexRe = regexp.MustCompile(`^#[0-9a-f]{6}$`)

func TestPalettesHaveValidHex(t *testing.T) {
	for _, mode := range []ThemeMode{ThemeDark, ThemeLight} {
		p := GetPalette(mode)
		for name, c := range map[string]string{
			"ChatBg": p.ChatBg, "Rail": p.Rail, "Sidebar": p.Sidebar, "Header": p.Header,
			"OutgoingBubble": p.OutgoingBubble, "IncomingBubble": p.IncomingBubble,
			"Text": p.Text, "TextSecondary": p.TextSecondary, "Accent": p.Accent, "TickRead": p.TickRead,
			"Selection": p.Selection,
		} {
			if !hexRe.MatchString(c) {
				t.Errorf("mode %v %s = %q, not #rrggbb", mode, name, c)
			}
		}
	}
}

func TestDarkAndLightValues(t *testing.T) {
	d := GetPalette(ThemeDark)
	if d.ChatBg != "#0b141a" || d.Sidebar != "#111b21" || d.Rail != "#111b21" ||
		d.Header != "#202c33" || d.OutgoingBubble != "#005c4b" || d.IncomingBubble != "#202c33" {
		t.Errorf("dark palette wrong: %+v", d)
	}
	l := GetPalette(ThemeLight)
	if l.Rail != "#111b21" || l.Sidebar != "#ffffff" || l.ChatBg != "#efeae2" ||
		l.Header != "#f0f2f5" || l.OutgoingBubble != "#d9fdd3" || l.IncomingBubble != "#ffffff" {
		t.Errorf("light palette wrong: %+v", l)
	}
	if d.TickRead != "#53bdeb" || l.TickRead != "#53bdeb" {
		t.Errorf("read tick colour: dark %q light %q, want #53bdeb", d.TickRead, l.TickRead)
	}
	if d.Selection != "#27649b" || l.Selection != "#b3d4f5" {
		t.Errorf("selection colour: dark %q light %q", d.Selection, l.Selection)
	}
}

func TestSystemResolvesToDarkOrLight(t *testing.T) {
	if m := Resolve(ThemeSystem); m != ThemeDark && m != ThemeLight {
		t.Errorf("Resolve(ThemeSystem) = %v", m)
	}
	if Resolve(ThemeDark) != ThemeDark || Resolve(ThemeLight) != ThemeLight {
		t.Error("explicit modes must resolve to themselves")
	}
	if GetPalette(ThemeSystem).ChatBg == "" {
		t.Error("GetPalette(ThemeSystem) empty")
	}
}

func TestColor(t *testing.T) {
	c := Color("#005c4b")
	if c != walk.RGB(0x00, 0x5c, 0x4b) {
		t.Errorf("Color = %v", c)
	}
}
