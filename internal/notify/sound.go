package notify

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	winDirEnv = func() string { return os.Getenv("WINDIR") }
	pathExists = func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	}
	playSound = Play
)

var (
	winmm       = windows.NewLazySystemDLL("winmm.dll")
	pPlaySoundW = winmm.NewProc("PlaySoundW")
)

const (
	sndAsync     = 0x0001
	sndNoDefault = 0x0002
	sndFilename  = 0x00020000
)

// SoundFile maps a logical sound name to a WAV path in the Windows Media directory.
// Returns "" for "default", unknown sound names, or missing files.
func SoundFile(name, winDir string, exists func(string) bool) string {
	if winDir == "" {
		return ""
	}
	if exists == nil {
		exists = pathExists
	}
	var file string
	switch strings.ToLower(name) {
	case "message":
		file = "Windows Notify Messaging.wav"
	case "mail":
		file = "Windows Notify Email.wav"
	case "reminder":
		file = "Windows Notify Calendar.wav"
	case "sms":
		file = "Windows Message Nudge.wav"
	default:
		return ""
	}
	path := filepath.Join(winDir, "Media", file)
	if !exists(path) {
		return ""
	}
	return path
}

// Play plays a WAV file asynchronously using winmm.dll!PlaySoundW.
func Play(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	r, _, err := pPlaySoundW.Call(uintptr(unsafe.Pointer(p)), 0, uintptr(sndFilename|sndAsync|sndNoDefault))
	if r == 0 {
		return fmt.Errorf("PlaySoundW(%q): %w", path, err)
	}
	return nil
}
