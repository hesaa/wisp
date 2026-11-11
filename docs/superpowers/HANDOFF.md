# Wisp — Session Handoff

**Date:** 2026-11-11 (evening)
**Project path:** `D:\project\wa-go` (Go module `wa-go`; the app is **Wisp**, exe `wisp.exe`)
**Branch:** `feat/front-page-media-copy` (from `main` at `7ffc7b7`), head `e816cec`, not merged, no remote
**Next step:** the user reviews the new plan and picks how to run it — nothing else is in flight.

---

## 1. Where things stand

Two pieces of work, in different states:

**A. Plan `2026-11-11-wisp-front-page-media-copy.md` — finished, awaiting the user's merge decision.**
All 12 tasks are done and reviewed; the final whole-branch review and its one fix wave (`a3b08b5`)
re-reviewed clean. The user is testing the build on their real account and answered "Not yet, I'll
test" to merging. Do not merge or push without their word.

A bug found by the user's test is fixed and committed: `5887049` — the PIN dialog read its fields
after closing, so Set PIN saved an empty PIN (lock off) while saying "App lock is on." It is
**unconfirmed live**: the user had not yet rebuilt and retried when this was written.

**B. Plan `2026-11-11-wisp-people-previews.md` — written, not started.**
From the user's live test: names for unsaved contacts and group members, mentions, profile/group
pictures, a contact/group details view, video/GIF previews, the chat list preview line for
non-text messages, masked `+62••••••32` names, and a notification sound choice.

- Spec (approved by the user): `docs/superpowers/specs/2026-11-11-wisp-people-previews-design.md`.
  One section changed after approval and the user has been told but has not replied: §7 (sound) —
  Wisp's notifications are tray balloons, not toasts, so the non-default sounds are played by Wisp
  with `PlaySound` from `%WINDIR%\Media`. The Settings UI is as approved.
- Plan (9 tasks): `docs/superpowers/plans/2026-11-11-wisp-people-previews.md`. The user has **not**
  yet confirmed the plan or chosen the execution method (subagent-driven was recommended; native is
  the alternative). Ask before building.
- It runs on a new branch `feat/people-previews` cut from `feat/front-page-media-copy`.

**C. Colour emoji — agreed as a separate design, not started.** The emoji picker and the bubbles
draw monochrome glyphs because all text goes through GDI. Colour needs DirectWrite/Direct2D, which
replaces the text drawing and measuring that wrapping and selection (Tasks 7–8 of plan A) rest on.
The user agreed to do it second, after B. It needs its own brainstorm → spec → plan.

## 2. How to resume

1. Open `D:\project\wa-go`; the branch is `feat/front-page-media-copy`, working tree clean except
   this file if it was not committed.
2. Ask the user: does plan B capture what they want, and subagent-driven or native? Mention the §7
   change if they have not acknowledged it.
3. On their yes: create `feat/people-previews`, then `superpowers:subagent-driven-development` (or
   `superpowers:executing-plans`) with the plan path. A new plan gets a new ledger folder under
   `.superpowers/sdd/`.
4. Plan A's ledger, briefs, reports, `final-review.md` and `final-fix-report.md` are in
   `.superpowers/sdd/2026-11-11-wisp-front-page-media-copy/` (git-ignored). Keep it until A is
   merged; it holds every ruling (R1–R17) and the deferred minors.

## 3. Still for the user to try in their own Wisp (plan A)

Every live check used a scratch profile that could not reach WhatsApp, with no real keyboard or
mouse input. Unconfirmed:

- Set PIN now saves (row shows `App lock: On`); Change PIN and Turn off (same dialog, same old bug).
- The four lock triggers: start-up, minimise, close to tray, Windows lock.
- Minimise with a PIN: taskbar preview, Alt+Tab, Win+D show no chat content (fixed from code
  reading only — the least certain item).
- Start-up with a linked account: no link page flash, no pairing attempt.
- A video or GIF open when closing to the tray: playback stops.
- Right-click menu: Copy and Select by mouse, Menu key, Shift+F10.
- A real WhatsApp video, a real WebP sticker, dragging the seek bar, the light theme.
- Copy sign-off on strings the spec did not cover: `Download failed: …`, `Can't show this picture`.

Rulings the user has been told about but may still overturn: R11 (single click opens media), R16
(← → step through media; Shift+← → seek), R17 (taskbar thumbnail is the app icon while a PIN is
set), and that a failed read of the PIN setting at start-up now stops Wisp with an error.

## 4. Things learned this session that the next one needs

- **Wisp is single-instance and closing the window only hides it to the tray.** A new build does
  nothing until the old process is exited from the tray icon; a second launch just brings the old
  one forward (`wisp.log`: "already running; brought it to the front"). The user hit this once.
- `scripts\build.bat` can replace `wisp.exe` while an old one runs, so "the exe is new" does not
  mean "the running Wisp is new". Check the process start time against the exe time.
- The lock tests seeded a PIN straight into the database, which is how the PIN dialog bug survived
  every review. Dialog input paths need a check that goes through the dialog.
- Notifications are tray balloons (`Shell_NotifyIcon`), not WinRT toasts.
- `ffmpeg` is on PATH; a short H.264 test clip can be generated in the scratchpad (R12).
- Idle memory is compared on **private bytes**, back to back against the base build (R15): working
  set swings from about 29 to 74 MB on the same build. Base: about 54 MB private on the link page.

## 5. Live-check safety rules (carry into every dispatch)

- Never the real `%LOCALAPPDATA%`: set `LOCALAPPDATA` to a scratch folder; seed
  `<scratch>\Wisp\wisp.db` with `net.proxy = socks5://127.0.0.1:1`. Never link a real account.
- Only windows of processes you started, driven by PostMessage/SendMessage to their own HWNDs —
  no SendInput, no screen-coordinate input (one misclick hit a user window in plan A, Task 6).
- **Never open a real file dialog and never launch another program** in a live check (plan A,
  Task 10 wrote stray files into the user's Documents that way; they were removed).
- Never write the system clipboard. Never read, print or log `giphy.key`.
- Do not run `scripts\build.bat` for checks; build to the session scratchpad with
  `go build -trimpath -ldflags="-H windowsgui -s -w"`.
- Any media played must be near-silent and brief; the user is at the machine.
- Never touch processes you did not start.

## 6. On the machine

- The user's Wisp may be running from `D:\project\wa-go\wisp.exe` (earlier PID 19488, started
  08:43 from an old build; they were told to exit it from the tray).
- A stray `cmd.exe` (PID 23608, from a garbled build call in plan A) may still exist; killing it
  was denied by the permission settings — leave it, the user knows.
- `wa-go2.exe` in the repo folder and `%LOCALAPPDATA%\wa-go\` are leftovers the user can delete.
- A ComDlg32 "recent files" registry entry was left by the Task 10 Save as incident (harmless).

## 7. Deferred findings worth knowing (full list in plan A's ledger)

- `settingChanged` returns early on a failed re-read, leaving the in-memory PIN state and the
  taskbar-preview flag stale.
- Reset's logouts run on the UI thread (up to 30 s per account when offline).
- Building a full-size photo bitmap runs on the UI thread (up to 25 MP).
- A blank window shows while expired messages are purged on a large database at start-up.
- `chatview.go` (~1300 lines) and `mediaview.go` (~900 lines) are large; plan B adds to both.
- Phone checks from the first plan still open: idle RAM with 2 accounts, presence as seen by a
  contact, rendering above 100% scaling, live image/PDF send and receive.
