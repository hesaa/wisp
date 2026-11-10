# wa-go

A small, native Windows desktop client for WhatsApp, written in Go. It links to your phone as a
companion device (like WhatsApp Web) and can run several WhatsApp accounts side by side, each on
the account rail at the left. It keeps your chats in a local database and stays light on memory
and CPU; closing the window keeps it running in the tray.

## Prerequisites

- Windows 10 or 11, x64
- Go 1.26 or newer (only to build it)

## Build

```bat
scripts\build.bat
```

This writes `wa-go.exe` at the repository root. GIF search uses GIPHY; the build embeds a GIPHY API
key taken from the `WA_GO_GIPHY_KEY` environment variable, or else from a `giphy.key` file at the
repository root (git-ignored; one line, plain ASCII, no BOM). Without either, it builds without a
key and you can enter one later in Settings › Media & Storage.

## Run and link a phone

Start `wa-go.exe`. On the first run the pairing window shows a QR code: on your phone open
WhatsApp › Settings › Linked devices › Link a device and scan it. Add more accounts later with the
**+** button on the rail. Only one wa-go runs at a time; starting it again brings the open one to the
front.

## Your data

Everything lives in `%LOCALAPPDATA%\wa-go\`:

| Path        | What                                     |
|-------------|------------------------------------------|
| `wa-go.db`  | your chats, messages and settings         |
| `session.db`| the linked-device sessions (keys)         |
| `cache\`    | downloaded media (Settings can clear it)  |
| `wa-go.log` | a small log, without message text or keys |

To start over, quit wa-go from the tray (Exit), then delete that folder. Also remove the device
under Linked devices on your phone (or use Log out in Settings first).

## Privacy

- **Online status**: your accounts show online only while wa-go is the window you are using. Switch
  to another program and they go offline after about 10 seconds; minimising, closing to the tray,
  locking the PC or quitting takes them offline at once.
- **Appear offline** (Settings) keeps you offline all the time and also hides your typing.
- **Ghost typing** (Settings) never shows you as typing.
- **Read receipts**: with read receipts off in Privacy, reading a chat or a status here tells only
  your own devices (so they stay in sync); senders see no blue ticks.
- **Disappearing messages** are honoured locally: they and their downloaded media are deleted from
  this PC when their time is up (checked at start-up and whenever a chat opens), and replies you send
  in such a chat disappear too.
- Downloaded files are marked as coming from the Internet, so Windows and Office open them with the
  usual protections.

## Known limitations

- No voice or video calls, and you cannot post a status (you can view them).
- Avatars are initials, not profile photos.
- Reactions are not shown.
- The GIPHY key built into `wa-go.exe` can be extracted from the file: do not share builds made with
  your personal key.
