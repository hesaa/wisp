# Wisp

A small, native Windows desktop client for WhatsApp, written in Go. It links to your phone as a
companion device (like WhatsApp Web) and can run several WhatsApp accounts side by side, each on
the account rail at the left. It keeps your chats in a local database and stays light on memory
and CPU; closing the window keeps it running in the tray.

Wisp is an independent project, not made by or affiliated with WhatsApp or Meta.

## Prerequisites

- Windows 10 or 11, x64
- Go 1.26 or newer (only to build it)

## Build

```bat
scripts\build.bat
```

This writes `wisp.exe` at the repository root. GIF search uses GIPHY; the build embeds a GIPHY API
key taken from the `WISP_GIPHY_KEY` environment variable, or else from a `giphy.key` file at the
repository root (git-ignored; one line, plain ASCII, no BOM). Without either, it builds without a
key and you can enter one later in Settings › Media & Storage.

## Run and link a phone

Start `wisp.exe`. With no account linked, the whole window is the link page: the steps on the left
and a QR code on the right. On your phone open WhatsApp › Settings › Linked devices › Link a device
and point it at the code; the phone then lists it as "Wisp". The code refreshes on its own, and
**Try again** appears when it expires. Add more accounts later with the **+** button on the rail,
which shows the same page with a **Cancel** button. Only one Wisp runs at a time; starting it again
brings the open one to the front.

## App lock

Settings › Privacy & Security has an optional **App lock**, off by default. **Set PIN** asks for a
4–8 digit PIN twice; **Change PIN** and **Turn off** ask for the current one first.

With the lock on, Wisp shows a lock page instead of your chats at start-up, when you restore it
from the tray or from minimised, and when Windows locks. Accounts stay connected while it is locked,
but notifications show only "New message", and you count as not using Wisp, so you don't appear
online. After 5 wrong PINs the box is disabled for 30 seconds, doubling after every further 5.

**Forgot your PIN?** **Reset Wisp** on the lock page, after a confirmation, logs out every account,
deletes the chats, sessions and downloaded media from this PC and starts Wisp afresh on the link
page. It keeps `wisp.log`. If the logout can't reach WhatsApp, the phone may still list the device:
remove it under Linked devices.

The lock hides Wisp's window; it doesn't encrypt Wisp's files.

## Media and copying

Photos, stickers, videos and GIFs open inside Wisp: a single click (or Enter) on one opens it.
Audio and documents open in their usual program, on double-click or Enter.

Drag across message text to select it, then Ctrl+C to copy; Esc clears the selection. Ctrl+click,
Shift+click or Shift+↑↓ pick whole messages, and Ctrl+C copies them. Right-click, the Menu key or
Shift+F10 opens a menu with **Copy** and **Select**. Copying is text only.

## Your data

Everything lives in `%LOCALAPPDATA%\Wisp\`:

| Path         | What                                      |
|--------------|-------------------------------------------|
| `wisp.db`    | your chats, messages and settings         |
| `session.db` | the linked-device sessions (keys)         |
| `cache\`     | downloaded media (Settings can clear it)  |
| `wisp.log`   | a small log, without message text or keys |

To start over, quit Wisp from the tray (Exit), then delete that folder. Also remove the device
under Linked devices on your phone (or use Log out in Settings first).

## Privacy

- **Online status**: your accounts show online only while Wisp is the window you are using. Switch
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
- In right-to-left and shaped scripts (Arabic, Hebrew, Indic) the selection highlight may sit
  slightly off the text.
- The GIPHY key built into `wisp.exe` can be extracted from the file: do not share builds made with
  your personal key.
