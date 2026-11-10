// Command wa-go is a native Windows WhatsApp client for several accounts at once.
//
// Data lives in %LOCALAPPDATA%\wa-go: wa-go.db (messages and settings), session.db (whatsmeow's
// paired devices), cache\ (media) and wa-go.log.
package main

import (
	"context"
	"log"
	"os"
	"path/filepath"

	"github.com/lxn/walk"

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
		log.Printf("wa-go: %v", err)
		walk.MsgBox(nil, "wa-go", "wa-go could not start: "+err.Error(), walk.MsgBoxIconError|walk.MsgBoxOK)
		os.Exit(1)
	}
}

func run() error {
	base, err := os.UserCacheDir() // %LOCALAPPDATA%
	if err != nil {
		return err
	}
	dir := filepath.Join(base, "wa-go")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if f, err := openLog(filepath.Join(dir, "wa-go.log"), logLimit); err == nil {
		log.SetOutput(f)
		defer f.Close()
	} // else: a GUI build has nowhere to log, the app still runs

	db, err := store.Open(filepath.Join(dir, "wa-go.db"))
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
	return a.Run()
}

// openLog opens the log at path for appending, emptying it first when it is over limit bytes.
func openLog(path string, limit int64) (*os.File, error) {
	flag := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fi, err := os.Stat(path); err == nil && fi.Size() > limit {
		flag |= os.O_TRUNC
	}
	return os.OpenFile(path, flag, 0o600)
}
