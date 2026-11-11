// Command wisp builds Wisp, a native Windows WhatsApp client for several accounts at once.
//
// Data lives in %LOCALAPPDATA%\Wisp: wisp.db (messages and settings), session.db (whatsmeow's
// paired devices), cache\ (media) and wisp.log.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/lxn/walk"
	"golang.org/x/sys/windows"

	"wa-go/internal/app"
	"wa-go/internal/media"
	"wa-go/internal/store"
	"wa-go/internal/whatsapp"
)

// giphyKey is the built-in GIPHY API key, set at build time with -ldflags "-X main.giphyKey=…";
// a key saved in Settings wins. Never logged.
var giphyKey string

// logLimit: a log larger than this at startup starts over.
const logLimit = 1 << 20

func main() {
	if err := run(); err != nil {
		walk.MsgBox(nil, "Wisp", "Wisp could not start: "+err.Error(), walk.MsgBoxIconError|walk.MsgBoxOK)
		os.Exit(1)
	}
}

// run starts Wisp and returns once it exits; an error is logged before the log closes.
func run() (err error) {
	base, err := os.UserCacheDir() // %LOCALAPPDATA%
	if err != nil {
		return err
	}
	dir := filepath.Join(base, "Wisp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if f, ferr := openLog(filepath.Join(dir, "wisp.log"), logLimit); ferr == nil {
		log.SetOutput(f)
		defer func() {
			if err != nil {
				log.Printf("wisp: %v", err)
			}
			f.Close()
		}()
	} // else: a GUI build has nowhere to log, the app still runs

	show, first, err := claimInstance(dir)
	if err != nil {
		return err
	}
	if !first {
		log.Printf("wisp: already running; brought it to the front")
		return nil
	}
	defer windows.CloseHandle(show)

	db, err := store.Open(filepath.Join(dir, "wisp.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	root, err := media.DefaultRoot()
	if err != nil {
		return err
	}
	cache := media.New(root)
	a, err := app.New(db, cache, giphyKey)
	if err != nil {
		return err
	}
	mgr, err := whatsapp.New(context.Background(), filepath.Join(dir, "session.db"), db, cache)
	if err != nil {
		return err
	}
	a.Attach(mgr)
	go func() { // another Wisp asked for the window
		for {
			if ev, err := windows.WaitForSingleObject(show, windows.INFINITE); ev != windows.WAIT_OBJECT_0 {
				log.Printf("wisp: instance signal: %v", err)
				return
			}
			a.Restore()
		}
	}()
	return a.Run()
}

// instanceName names the per-session event that keeps one Wisp per data folder: two would share
// the database and connect the same accounts twice.
func instanceName(dataDir string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(dataDir))))
	return `Local\wisp-` + hex.EncodeToString(sum[:16])
}

// claimInstance makes this the running Wisp for dataDir and returns the event other instances set
// to bring it to the front. When one already runs, it is signalled instead and first is false.
func claimInstance(dataDir string) (show windows.Handle, first bool, err error) {
	name, err := windows.UTF16PtrFromString(instanceName(dataDir))
	if err != nil {
		return 0, false, err
	}
	show, err = windows.CreateEvent(nil, 0, 0, name) // auto-reset, unset
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		defer windows.CloseHandle(show)
		allowSetForegroundWindow.Call(asfwAny) // we have the foreground: let the running one take it
		return 0, false, windows.SetEvent(show)
	}
	if err != nil {
		return 0, false, err
	}
	return show, true, nil
}

var allowSetForegroundWindow = windows.NewLazySystemDLL("user32.dll").NewProc("AllowSetForegroundWindow")

const asfwAny = ^uintptr(0) // ASFW_ANY

// openLog opens the log at path for appending, emptying it first when it is over limit bytes.
func openLog(path string, limit int64) (*os.File, error) {
	flag := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fi, err := os.Stat(path); err == nil && fi.Size() > limit {
		flag |= os.O_TRUNC
	}
	return os.OpenFile(path, flag, 0o600)
}
