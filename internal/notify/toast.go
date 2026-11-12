// Package notify shows message alerts as Windows toasts (tray balloons) and owns the tray icon.
package notify

import "unicode/utf16"

// Input describes one incoming message, with names already resolved by the caller.
type Input struct {
	AccountID    string
	AccountLabel string
	ChatJID      string
	SenderName   string
	ChatName     string
	IsGroup      bool
	Type         string // store.Message.Type
	Body         string // text, or media caption
	FileName     string // document file name, if known
	FromMe       bool
}

// Settings are the user preferences that shape an alert.
type Settings struct {
	Enabled      bool
	PrivateMode  bool // hide sender/chat name and message text
	Sound        bool
	SoundName    string
	MultiAccount bool // more than one account connected: title names the account
}

// Notification is the text to display.
type Notification struct {
	Title, Body string
	Silent      bool
	Sound       string
}

var mediaWord = map[string]string{
	"image": "Photo", "video": "Video", "audio": "Voice message",
	"document": "Document", "sticker": "Sticker",
}

// Format decides whether to alert and with what text. Pure.
func Format(in Input, s Settings) (Notification, bool) {
	if !s.Enabled || in.FromMe || in.ChatJID == "status@broadcast" ||
		in.Type == "revoked" || in.Type == "unsupported" {
		return Notification{}, false
	}
	n := Notification{}
	if !s.Sound {
		n.Silent = true
		n.Sound = ""
	} else {
		soundPath := SoundFile(s.SoundName, winDirEnv(), pathExists)
		if soundPath != "" {
			n.Silent = true
			n.Sound = soundPath
		} else {
			n.Silent = false
			n.Sound = ""
		}
	}
	if s.PrivateMode {
		n.Title, n.Body = "WhatsApp", "New message"
	} else {
		n.Title = in.ChatName
		if in.IsGroup {
			n.Title = in.SenderName + " @ " + in.ChatName
		} else if n.Title == "" {
			n.Title = in.SenderName
		}
		n.Body = in.Body
		if w, ok := mediaWord[in.Type]; ok {
			n.Body = w
			if in.Type == "document" && in.FileName != "" {
				n.Body += ": " + in.FileName
			} else if in.Body != "" && in.Type != "document" {
				n.Body += ": " + in.Body
			}
		}
	}
	if s.MultiAccount && in.AccountLabel != "" {
		n.Title += " (" + in.AccountLabel + ")"
	}
	return n, true
}

// truncateUTF16 cuts s to at most max UTF-16 units without splitting a rune.
func truncateUTF16(s string, max int) string {
	units := 0
	for i, r := range s {
		w := utf16.RuneLen(r)
		if w < 0 {
			w = 1 // invalid rune becomes U+FFFD, one unit
		}
		if units+w > max {
			return s[:i]
		}
		units += w
	}
	return s
}

type balloon interface {
	show(title, body string, silent bool) error
}

// Dispatcher formats messages and hands them to a balloon, remembering the last chat alerted.
// Use from the UI thread only.
type Dispatcher struct {
	b             balloon
	account, chat string
	has           bool
}

func newDispatcher(b balloon) *Dispatcher { return &Dispatcher{b: b} }

// Notify shows an alert if Format allows it.
func (d *Dispatcher) Notify(in Input, s Settings) error {
	n, ok := Format(in, s)
	if !ok {
		return nil
	}
	if err := d.b.show(n.Title, n.Body, n.Silent); err != nil {
		return err
	}
	if n.Sound != "" {
		_ = playSound(n.Sound)
	}
	d.account, d.chat, d.has = in.AccountID, in.ChatJID, true
	return nil
}

// Last returns the (account, chat) of the most recent alert, for handling a click on it.
func (d *Dispatcher) Last() (accountID, chatJID string, ok bool) {
	return d.account, d.chat, d.has
}
