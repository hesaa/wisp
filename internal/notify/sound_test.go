package notify

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSoundFile(t *testing.T) {
	fakeWin := filepath.Join("C:", "Windows")
	existing := map[string]bool{
		filepath.Join(fakeWin, "Media", "Windows Notify Messaging.wav"): true,
		filepath.Join(fakeWin, "Media", "Windows Notify Email.wav"):     true,
		filepath.Join(fakeWin, "Media", "Windows Notify Calendar.wav"):  true,
		filepath.Join(fakeWin, "Media", "Windows Message Nudge.wav"):    true,
	}
	exists := func(p string) bool { return existing[p] }

	cases := []struct {
		name     string
		wantFile string
	}{
		{"message", "Windows Notify Messaging.wav"},
		{"mail", "Windows Notify Email.wav"},
		{"reminder", "Windows Notify Calendar.wav"},
		{"sms", "Windows Message Nudge.wav"},
		{"MESSAGE", "Windows Notify Messaging.wav"},
		{"default", ""},
		{"bogus", ""},
		{"", ""},
	}

	for _, c := range cases {
		got := SoundFile(c.name, fakeWin, exists)
		if c.wantFile == "" {
			if got != "" {
				t.Errorf("SoundFile(%q) = %q, want empty", c.name, got)
			}
		} else {
			want := filepath.Join(fakeWin, "Media", c.wantFile)
			if got != want {
				t.Errorf("SoundFile(%q) = %q, want %q", c.name, got, want)
			}
		}
	}

	// Missing file returns empty string
	existsNone := func(string) bool { return false }
	if got := SoundFile("mail", fakeWin, existsNone); got != "" {
		t.Errorf("SoundFile with missing file = %q, want empty", got)
	}
}

func TestFormatSoundChoice(t *testing.T) {
	fakeWin := filepath.Join("C:", "Windows")
	mailWav := filepath.Join(fakeWin, "Media", "Windows Notify Email.wav")

	origWinDir := winDirEnv
	origExists := pathExists
	t.Cleanup(func() {
		winDirEnv = origWinDir
		pathExists = origExists
	})

	winDirEnv = func() string { return fakeWin }
	pathExists = func(p string) bool {
		return strings.EqualFold(p, mailWav)
	}

	in := msg()

	// 1. Sound off -> Silent: true, Sound: ""
	nOff, ok := Format(in, Settings{Enabled: true, Sound: false, SoundName: "mail"})
	if !ok || !nOff.Silent || nOff.Sound != "" {
		t.Errorf("sound off: got silent=%v sound=%q, want silent=true sound=''", nOff.Silent, nOff.Sound)
	}

	// 2. Sound on + "mail" file present -> Silent: true, Sound: mailWav
	nMail, ok := Format(in, Settings{Enabled: true, Sound: true, SoundName: "mail"})
	if !ok || !nMail.Silent || nMail.Sound != mailWav {
		t.Errorf("sound mail: got silent=%v sound=%q, want silent=true sound=%q", nMail.Silent, nMail.Sound, mailWav)
	}

	// 3. Sound on + "default" -> Silent: false, Sound: ""
	nDef, ok := Format(in, Settings{Enabled: true, Sound: true, SoundName: "default"})
	if !ok || nDef.Silent || nDef.Sound != "" {
		t.Errorf("sound default: got silent=%v sound=%q, want silent=false sound=''", nDef.Silent, nDef.Sound)
	}

	// 4. Sound on + file missing -> Silent: false, Sound: ""
	nMiss, ok := Format(in, Settings{Enabled: true, Sound: true, SoundName: "reminder"})
	if !ok || nMiss.Silent || nMiss.Sound != "" {
		t.Errorf("sound missing file: got silent=%v sound=%q, want silent=false sound=''", nMiss.Silent, nMiss.Sound)
	}
}

func TestDispatcherPlaysSound(t *testing.T) {
	fakeWin := filepath.Join("C:", "Windows")
	mailWav := filepath.Join(fakeWin, "Media", "Windows Notify Email.wav")

	origWinDir := winDirEnv
	origExists := pathExists
	origPlay := playSound
	t.Cleanup(func() {
		winDirEnv = origWinDir
		pathExists = origExists
		playSound = origPlay
	})

	var played string
	playSound = func(path string) error {
		played = path
		return nil
	}
	winDirEnv = func() string { return fakeWin }
	pathExists = func(p string) bool { return strings.EqualFold(p, mailWav) }

	fb := &fakeBalloon{}
	d := newDispatcher(fb)

	// Notify with sound enabled and custom sound
	s := Settings{Enabled: true, Sound: true, SoundName: "mail"}
	if err := d.Notify(msg(), s); err != nil {
		t.Fatal(err)
	}
	if fb.calls != 1 || !fb.silent {
		t.Fatalf("expected silent balloon with custom WAV, got %+v", fb)
	}
	if played != mailWav {
		t.Fatalf("expected played %q, got %q", mailWav, played)
	}

	// Notify with sound disabled -> no sound played
	played = ""
	sOff := Settings{Enabled: true, Sound: false, SoundName: "mail"}
	if err := d.Notify(msg(), sOff); err != nil {
		t.Fatal(err)
	}
	if played != "" {
		t.Fatalf("expected no sound played when sound is off, got %q", played)
	}
}
