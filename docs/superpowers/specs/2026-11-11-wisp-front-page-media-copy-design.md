# Wisp: link page, app lock, in-app media, text selection — design

**Date:** 2026-11-11
**Status:** approved in conversation, awaiting written-spec review
**Builds on:** `2026-11-10-whatsapp-client-design.md` (binding constraints still apply: idle RAM
< 50 MB with one account, 0% idle CPU, no browser runtimes, monochrome UI icons, dark and light
themes, multi-account).

## Goal

Four user-facing improvements to Wisp:

1. **Link page** — linking a phone happens on a full-window start page instead of a popup dialog.
2. **App lock** — an optional PIN that hides Wisp behind a lock page.
3. **In-app media** — photos, stickers, videos and GIFs open inside the chat area instead of an
   outside program.
4. **Selecting and copying** — free text selection by dragging in the chat, plus whole-message copy.

## Non-goals

- Encrypting data on disk (the lock hides the UI only).
- Windows Hello, idle auto-lock timers.
- In-app voice notes, audio or documents (they keep opening in their usual program).
- Copying images to the clipboard (text only).
- Exact selection for complex scripts (Arabic, Hebrew, Indic) — see Risks.

---

## 1. Link page

**When it shows.** Whenever no account is linked, the main window shows the link page instead of
the rail, chat list and chat column. The **+** button on the rail shows the same page for another
account, with a **Cancel** button that returns to the chats; with no account linked there is no
Cancel.

**Layout.** Left: the logo, "Link your phone to Wisp", four numbered steps (Open WhatsApp on your
phone › Settings › Linked devices › Link a device › point your phone at this screen) and the note
"Your phone doesn't need to stay online after linking." Right: the QR code, always black on white
(it must scan in both themes), "The code refreshes on its own", a status line, and **Try again**
when the codes expire. Follows the active theme otherwise.

**Behaviour.** The pairing logic of `internal/ui/qrdialog.go` (the `PairFunc` channel loop, code
rotation, timeout → "Try again", errors, cancellation through the context) moves into a reusable
QR panel used by the page. The popup dialog and `RunQRDialog` are removed; `App.pair` shows the
page. A successful scan closes the page, shows the chats and selects the new account (as the
`StatePaired` handler does today). Cancel or a closed window cancels the pairing context.

## 2. App lock

**Setting.** Settings › Privacy & Security gains **App lock**: Off by default. *Set PIN* asks for a
4–8 digit PIN twice. *Change PIN* and *Turn off* ask for the current PIN first.

**Storage.** Settings key `lock.pin` holds `pbkdf2-sha256$<iterations>$<salt b64>$<hash b64>`
(Go's `crypto/pbkdf2`, 16-byte random salt, 600 000 iterations, 32-byte key; compared in constant
time). Empty or absent means the lock is off. No PIN is ever logged or kept in memory after a check.

**When it locks.** At start-up; when the window is restored from the tray or from minimised; when
Windows locks the session (already observed for presence).

**Lock page.** Full window: logo, "Wisp is locked", "Enter your PIN to see your chats", a masked
digit-only PIN box with focus, **Unlock**, and "Forgot your PIN? **Reset Wisp**". A wrong PIN says
so and clears the box. After 5 wrong tries the box is disabled for 30 s, doubling after each
further 5 (60 s, 120 s, …); the count resets on success and is not persisted.

**While locked.** Accounts stay connected and messages are stored. Notifications are forced into
private mode ("New message", no sender or text), whatever the notification setting. The tray
badge still counts. Locked counts as "not using Wisp", so presence is unavailable (it joins the
existing `windowLocked` state). No chat content is drawn or reachable by keyboard.

**Reset Wisp.** After a confirmation that names what is lost, Wisp logs out every account
(`Manager.Logout`, so phones drop the device), closes its databases, deletes `wisp.db*`,
`session.db*` and `cache\` from its data folder (not `wisp.log`), starts a new Wisp process and
exits; the new process shows the link page. A logout that fails offline still proceeds with the
local deletion and says the phone may still list the device.

**Honest limit.** Stated in Settings next to the option: the lock hides the window; it does not
encrypt Wisp's files.

## 3. In-app media

**What opens in-app.** Messages of type `image` and `sticker` (photo viewer) and `video` (player;
GIFs are `video` messages whose `MediaMeta` has `GifPlayback` set). Audio and documents keep the
current `OpenInDefaultApp` path.

**Where.** Clicking or Enter on such a message replaces the chat view and input bar in the chat
column with the media view (the rail and chat list stay). Top bar: back arrow (or Esc) returns to
the chat at the same scroll position; sender name and time; **Save as…** (file dialog, copies the
cached file); **Open with…** (the existing default-program path, as an escape hatch). ← and →
step through the photos and videos among the chat's loaded messages.

**Not downloaded yet.** The view opens in a "Downloading…" state, requests the download through
the existing `OnNeedMedia` path, and shows the media when `SetMediaPath` reports it; a failed
download shows the error and a Retry.

**Photos and stickers.** Decoded with the same Go decoders the thumbnails use, at full size, only
while open; fitted to the area; zoom 1×–4× around the pointer by mouse wheel, Ctrl + / −, or
double-click (toggles fit / 2×); drag pans when zoomed; the zoom level shows in the top bar.
Closing frees the image and calls `debug.FreeOSMemory`. Images above the existing decode size cap
show "Too large to show here" with Open with….

**Videos and GIFs.** Played by Windows' Media Foundation player (`mfplay.dll`,
`MFPCreateMediaPlayer`), rendering into a child window of the media view; no callback object —
state, position and duration are polled by a timer that runs only while a video is open
(250 ms). Videos: play/pause (Space or click), seek bar with drag, `m:ss / m:ss`, mute. GIFs:
start muted, loop (restart at the end), no control bar. The player is created on open and shut
down on close. If creation or playback fails (missing codec, "N" editions of Windows without the
Media Feature Pack), the area says "Windows can't play this video" with Open with….

**Interface change.** `ChatView.OnOpenMedia` passes the `store.Message` instead of a path, so the
app can choose viewer, player or default program.

## 4. Selecting and copying

**Text layout.** Message text and captions are no longer wrapped by a single `DrawText` call with
`msgFormat`. A line breaker (pure function) splits text into lines using measured widths: break
after spaces and at explicit newlines, split a word that is wider than the line, never split
inside a UTF-16 surrogate pair. Widths come from `GetTextExtentExPointW` for the bubble font at the
window DPI, which also yields each character's x offset. Each line is drawn on its own. Bubble
sizes and wrapping must look the same as today.

**Free selection.** Mouse down on text sets an anchor (message, character); dragging extends the
selection to the character under the pointer, across messages, auto-scrolling at the top and
bottom edges. Double-click selects a word, triple-click the message's text. The highlight is drawn
behind the selected characters of each line (theme selection colour). Only text and captions are
selectable; names, times and ticks are not. A plain click elsewhere or Esc clears it. Ctrl+C or
right-click › **Copy** copies it; parts from several messages are joined one per line.

**Whole messages.** Right-click on a message (or the Menu key / Shift+F10 on the keyboard-focused
one; Enter keeps opening media) opens a menu: **Copy**, **Select**. Ctrl+click toggles a message, Shift+click selects a range, Shift + ↑/↓
extends from the keyboard. Selected rows are tinted with an accent bar; the header shows
"N selected". Ctrl+C with no text selection copies the selected messages, or the focused one.
Format: one message → its text; several → `[HH:MM, DD/MM/YYYY] Sender: text` per line;
media → its caption, or a label (`[Photo]`, `[Video]`, `[GIF]`, `[Sticker]`, `[Voice message]`,
`[Audio]`, `[Document: name]`) when it has none.

**Clipboard.** Plain Unicode text through walk's clipboard.

---

## Architecture

| Unit | Responsibility |
|---|---|
| `internal/ui/linkpage.go` | link page; owns the QR panel (moved pairing loop from `qrdialog.go`) |
| `internal/ui/lockpage.go` | lock page: PIN entry, back-off, reset link |
| `internal/lock` (new package) | PIN hash/verify, back-off schedule — pure, no UI |
| `internal/ui/mediaview.go` | media view: top bar, photo fit/zoom/pan, next/previous, states |
| `internal/ui/player.go` | thin `mfplay.dll` COM wrapper: create, play, pause, seek, position, duration, mute, shutdown |
| `internal/ui/textlayout.go` | line breaking, per-character offsets, hit-testing (pure parts tested) |
| `internal/ui/chatview.go` | draws text through `textlayout`; selection state; context menu; Ctrl+C |
| `internal/ui/copytext.go` | builds copied text from messages / selection — pure |
| `internal/app` | chooses page (link / lock / chats); lock triggers; reset; media routing; private toasts while locked |
| Settings dialog | App lock controls in Privacy & Security |

The main window keeps one top-level layout and switches which composite is visible (link page,
lock page, or the normal rail + sidebar + chat column); inside the chat column, the chat view +
input bar or the media view.

## Constraints and resource use

- Idle memory and CPU unchanged: pages are hidden composites; the media view, decoded image and
  player exist only while open; the video timer runs only while a video is open.
- No new third-party dependencies (`crypto/pbkdf2` is standard library; `mfplay.dll` ships with
  Windows 10/11).
- Keyboard access everywhere (PIN box, Unlock, media controls, selection), as in the rest of Wisp.

## Error handling

- Pairing errors and expiry: status line + Try again (as today).
- Wrong PIN, back-off: inline message on the lock page.
- Reset failures: each step reports what could not be done; local deletion still runs.
- Media: download failure → Retry; decode failure / too large / unplayable → message + Open with….
- Clipboard failure: logged and shown as a short status in the chat header.

## Testing

- `internal/lock`: hash/verify round trip, wrong PIN, malformed stored value, back-off schedule.
- `textlayout`: line breaking (spaces, newlines, long words, surrogate pairs), offsets → hit-test.
- `copytext`: single, multiple, media with and without captions, partial selections.
- Media view: fit/zoom/pan math, next/previous ordering — pure functions.
- `player.go`: a live check playing a local MP4 (no network) in a scratch window.
- App: page selection (no accounts → link; lock on → lock at start; restore → lock), toasts
  private while locked, presence unavailable while locked — through the existing window-free
  logic tests.
- Visual check of each page and of text rendering against the current look (screenshots of our
  own window only).

## Risks

- **Complex scripts:** per-character widths from GDI are exact for Latin and emoji, but shaped
  scripts (Arabic, Hebrew, Indic) may highlight slightly off. Accepted for now.
- **Wrapping drift:** the custom line breaker must match today's `DrawText` wrapping closely; a
  visual comparison is part of the check.
- **MFPlay** is a legacy-but-supported API; codecs depend on the Windows edition.
- **Reset** deletes local data irreversibly; it is behind an explicit confirmation.
