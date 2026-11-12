package ui

import (
	"cmp"
	"fmt"
	"strings"
	"time"

	"wa-go/internal/store"
)

// mediaLabel is what stands for a media message's content in copied text, "" for other messages.
func mediaLabel(m store.Message, info mediaInfo) string {
	switch m.Type {
	case "image":
		return "[Photo]"
	case "sticker":
		return "[Sticker]"
	case "video":
		if info.gif {
			return "[GIF]"
		}
		return "[Video]"
	case "audio":
		if info.voice {
			return "[Voice message]"
		}
		return "[Audio]"
	case "document":
		return "[Document: " + cmp.Or(info.name, m.Body, "Document") + "]"
	}
	return ""
}

// messageText is message m as copied: its text or caption, else the label of its media. Notices
// ("This message was deleted") copy nothing, as they cannot be selected either.
func messageText(m store.Message, d ...*Directory) string {
	var dir *Directory
	if len(d) > 0 {
		dir = d[0]
	}
	info, _ := parseMedia(&m) // a damaged MediaMeta only loses the file name and flags
	text, _, muted := bubbleText(&m, info, dir)
	if muted {
		return ""
	}
	return cmp.Or(text, mediaLabel(m, info))
}

// copyMessages is msgs (in chat order) as Ctrl+C copies them: one message gives its text, several one
// "[15:04, 02/01/2006] Sender: text" line each, joined with "\r\n". sender names the sender of a
// message not from the user, who is "You". Messages with nothing to copy are left out.
func copyMessages(msgs []store.Message, sender func(store.Message) string, loc *time.Location, d ...*Directory) string {
	var dir *Directory
	if len(d) > 0 {
		dir = d[0]
	}
	if len(msgs) == 1 {
		return messageText(msgs[0], dir)
	}
	var lines []string
	for _, m := range msgs {
		text := messageText(m, dir)
		if text == "" {
			continue
		}
		who := "You"
		if !m.FromMe {
			who = sender(m)
		}
		lines = append(lines, fmt.Sprintf("[%s] %s: %s", time.Unix(m.Timestamp, 0).In(loc).Format("15:04, 02/01/2006"), who, text))
	}
	return strings.Join(lines, "\r\n")
}
