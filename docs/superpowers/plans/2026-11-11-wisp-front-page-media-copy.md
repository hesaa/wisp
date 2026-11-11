# Wisp link page, app lock, in-app media and text selection — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a full-window link page, an optional PIN app lock with reset, in-app photo/video/GIF viewing, and free text selection plus whole-message copy to Wisp.

**Architecture:** The main window switches between three top-level views (chats, link page, lock page); inside the chat column it switches between the chat view and a new media view. Pure logic (PIN hashing, back-off, page choice, line breaking, hit-testing, copy formatting, zoom math) lives in small tested functions; walk widgets stay thin. Video uses Windows' `mfplay.dll` through a small COM wrapper.

**Tech Stack:** Go 1.26, lxn/walk + lxn/win, GDI (`GetTextExtentExPointW`), Media Foundation MFPlay, `crypto/pbkdf2`, whatsmeow.

**Spec:** `docs/superpowers/specs/2026-11-11-wisp-front-page-media-copy-design.md` (and the binding base spec `docs/superpowers/specs/2026-11-10-whatsapp-client-design.md`).

## Global Constraints

- Idle memory with one account must not grow (Task Manager "Memory" ≈ 26–28 MB today; base target < 50 MB); idle CPU stays 0% — no timers or goroutines run while the link page, lock page, media view or player are not in use.
- No browser runtimes, no new third-party modules (standard library, walk/win, x/image, whatsmeow only); `mfplay.dll` ships with Windows 10/11.
- UI icons stay monochrome line icons (`internal/ui/icons`); no emoji as buttons. Dark and light themes for every new view; the QR code is always black on white.
- Every new control is reachable and operable by keyboard.
- Logs never contain a PIN, the stored PIN hash, API keys, or message text.
- Tests never touch the real `%LOCALAPPDATA%`; use `t.TempDir()`.
- Copy is sentence case, no "please", no "successfully". Exact strings from the spec are used verbatim.
- Settings key for the lock: `lock.pin`, value `pbkdf2-sha256$<iterations>$<salt b64>$<hash b64>`, 600 000 iterations, 16-byte salt, 32-byte key; empty means off.
- PIN: 4–8 ASCII digits. Back-off: after every 5th wrong try wait 30 s, doubling (60 s, 120 s, …); not persisted.
- `gofmt` clean (strip CR first: the checkout uses `core.autocrlf=true`); `go vet ./...` clean; `go test ./...` green after every task.

## Review Focus

1. Wisp locks while a video plays or a photo is open → the media view closes, playback stops, nothing from the chat stays visible (test in Task 11).
2. Copying text that contains emoji (surrogate pairs) and `\r\n` line breaks → the clipboard text is exactly the selected substring, no broken characters (test in Task 8).
3. Opening a photo whose cached file was removed by Clear Cache → the viewer asks for the download instead of failing (test in Task 10).
4. The phone logs out one of two accounts → the chats stay; it logs out the last one → the link page appears (test in Task 3).
5. Reset Wisp while offline → every logout fails, yet local data is still wiped and the user is told the phone may still list the device (test in Task 6).

---

### Task 1: Commit the SetFocus fix already in the working tree

**Files:** already modified: `internal/ui/keys.go` (`focusOn` moved here), `internal/ui/{chatview,inputbar,newchat,pickers,qrdialog,rail,settingsdialog,sidebar,statusview}.go`, `internal/app/{app,chat}.go`.

**Interfaces:**
- Produces: `func focusOn(w walk.Window)` in `internal/ui/keys.go`; `func (b *InputBar) Focus()`; `func (s *Sidebar) FocusSearch()` (no error). Later tasks use `focusOn` for every focus change.

- [ ] **Step 1:** Run `go vet ./... && go test ./internal/ui ./internal/app` — expected: all `ok`.
- [ ] **Step 2:** `git grep -n "SetFocus()" -- "*.go"` — expected: no `logErr(..., x.SetFocus())` call left.
- [ ] **Step 3: Commit**
```bash
git add internal/ui internal/app
git commit -m "fix: focus changes no longer log walk's false SetFocus error"
```

### Task 2: `internal/lock` — PIN hashing, updates and back-off

**Files:**
- Create: `internal/lock/lock.go`, `internal/lock/lock_test.go`

**Interfaces:**
- Produces:
  - `var ErrPINFormat, ErrWrongPIN, ErrStored error`
  - `func ValidPIN(pin string) error` — `ErrPINFormat` unless 4–8 ASCII digits.
  - `func Hash(pin string) (string, error)` — `pbkdf2-sha256$600000$<base64.RawStdEncoding salt>$<base64.RawStdEncoding key>`, salt from `crypto/rand`, `pbkdf2.Key(sha256.New, pin, salt, 600000, 32)`.
  - `func Verify(stored, pin string) (bool, error)` — parses iterations from `stored`; `ErrStored` when malformed; constant-time compare (`crypto/subtle`).
  - `func Update(stored, current, next string) (string, error)` — when `stored != ""`, `current` must verify (`ErrWrongPIN` otherwise); `next == ""` returns `""` (lock off); else `ValidPIN(next)` then `Hash(next)`.
  - `type Backoff struct` with `func (b *Backoff) Wait(now time.Time) time.Duration`, `func (b *Backoff) Failed(now time.Time)`, `func (b *Backoff) Succeeded()`.

- [ ] **Step 1: Write the failing tests**
```go
func TestHashVerifyRoundTrip(t *testing.T)        // Hash("2580") → Verify(h,"2580")==true; prefix "pbkdf2-sha256$600000$"; two Hash calls differ
func TestVerifyRejectsWrongPIN(t *testing.T)      // Verify(Hash("2580"),"2581")==false, err nil
func TestVerifyMalformedStored(t *testing.T)      // "", "plain", "pbkdf2-sha256$x$AA$AA", "md5$1$AA$AA" → ErrStored
func TestValidPIN(t *testing.T)                   // "1234","12345678" ok; "123","123456789","12a4","１２３４"," 1234" → ErrPINFormat
func TestUpdate(t *testing.T)                     // ("", "", "1234") → hash of 1234; (h,"1234","5678") → verifies 5678; (h,"0000","5678") → ErrWrongPIN; (h,"1234","") → ""; ("", "", "12") → ErrPINFormat
func TestBackoff(t *testing.T)                    // 4×Failed → Wait 0; 5th → 30s; at +30s → 0; 10th → 60s; 15th → 120s; Succeeded → 0 and counting restarts
```
- [ ] **Step 2:** `go test ./internal/lock` — expected: FAIL (package does not compile).
- [ ] **Step 3:** Implement `internal/lock/lock.go` with the interfaces above (Go 1.24+ `crypto/pbkdf2`).
- [ ] **Step 4:** `go test ./internal/lock` — expected: `ok`.
- [ ] **Step 5: Commit** `git add internal/lock && git commit -m "feat(lock): PIN hashing, updates and back-off"`

### Task 3: Link page replaces the pairing dialog

**Files:**
- Rename: `internal/ui/qrdialog.go` → `internal/ui/qrpanel.go`; `internal/ui/qrdialog_test.go` → `internal/ui/qrpanel_test.go` (`git mv`)
- Create: `internal/ui/linkpage.go`
- Modify: `internal/app/app.go` (build, `pair`, new `showPage`), `internal/app/events.go` (`StatePaired`, `StateLoggedOut`), `internal/app/logic.go`, `internal/app/logic_test.go`

**Interfaces:**
- Consumes: existing pure `qrStep`, `pairing(ctx, pair, deliver, show)`, `qrRects`, `PairFunc` (unchanged, tests unchanged).
- Produces:
  - `type QRPanel struct{ *walk.Composite }` — `func NewQRPanel(parent walk.Container, p theme.Palette, pair PairFunc) (*QRPanel, error)`, `Start()`, `Stop()`, `SetPalette(p theme.Palette) error`. Holds the code painting, status line and **Try again** of the old dialog.
  - `type LinkPage struct{ *walk.Composite }` — `func NewLinkPage(parent walk.Container, p theme.Palette, pair PairFunc) (*LinkPage, error)`, `Show(cancellable bool)` (shows, starts pairing), `Hide()` (stops pairing), `OnCancel(fn func())`, `SetPalette(p theme.Palette) error`.
  - In `internal/app/logic.go`: `type page int`, `const (pageChats page = iota; pageLink; pageLock)`, `func pageFor(accounts int, locked, adding bool) page`.
  - `func (a *App) showPage()` — shows exactly one of {rail+sidebar+chat column, link page, lock page (Task 5)} for `pageFor(len(accounts), a.locked, a.adding)`.
  - `RunQRDialog` and `qrDialog` are deleted.

- [ ] **Step 1: Write the failing tests** in `internal/app/logic_test.go`
```go
func TestPageFor(t *testing.T) {
	cases := []struct{ accounts int; locked, adding bool; want page }{
		{0, false, false, pageLink}, {1, false, false, pageChats}, {1, false, true, pageLink},
		{0, true, false, pageLock}, {2, true, true, pageLock}, {2, false, false, pageChats},
	}
	// table loop: pageFor(...) == want
}
// Review Focus 4: two accounts, one logged out → pageChats; the last logged out → pageLink
func TestPageAfterLogouts(t *testing.T) // pageFor(1,false,false)==pageChats; pageFor(0,false,false)==pageLink
```
- [ ] **Step 2:** `go test ./internal/app -run 'TestPage'` — expected: FAIL (`pageFor` undefined).
- [ ] **Step 3:** Implement `pageFor`; move the dialog's widget code into `QRPanel`; build `LinkPage` with exactly: heading `Link your phone to Wisp`; steps `1. Open WhatsApp on your phone` / `2. Tap Settings, then Linked devices` / `3. Tap Link a device` / `4. Point your phone at this screen`; note `Your phone doesn't need to stay online after linking.`; caption `The code refreshes on its own`; button `Cancel` (only when cancellable). Add the link page to `mw`'s layout (stretch 1000, hidden); `App.pair` sets `a.adding = true` and calls `showPage`; Cancel and `StatePaired` set `a.adding = false` then `showPage`; `refreshAccounts` ends with `showPage`; `Attach` no longer calls `pair` (zero accounts already shows the link page). Remove `RunQRDialog`.
- [ ] **Step 4:** `go test ./internal/ui ./internal/app` — expected: `ok` (qr tests unchanged and passing).
- [ ] **Step 5:** Build (`scripts\build.bat`), start `wisp.exe` with `LOCALAPPDATA` set to a scratch folder, capture its window (own process only, move the window off-screen first so the live code cannot be scanned) — expected: the link page fills the window, QR black on white, in dark and light themes.
- [ ] **Step 6: Commit** `git commit -m "feat: full-window link page replaces the pairing dialog"`

### Task 4: App lock settings (set, change, turn off)

**Files:**
- Create: `internal/ui/pindialog.go`
- Modify: `internal/ui/settings.go` (key + pure flow), `internal/ui/settingsdialog.go` (`buildPrivacy`), `internal/ui/settings_test.go`, `internal/app/app.go` (`globalKeys`)

**Interfaces:**
- Consumes: `lock.Update`, `lock.ErrWrongPIN`, `lock.ErrPINFormat` (Task 2).
- Produces:
  - `const KeyLockPIN = "lock.pin"` in `internal/ui/settings.go`; added to `app.globalKeys`.
  - `type pinMode int` with `pinSet` (values: new, confirm), `pinChange` (current, new, confirm), `pinOff` (current).
  - `func pinUpdate(stored string, mode pinMode, values []string) (newStored, msg string, ok bool)` — pure; messages: mismatch `The PINs don't match.`, wrong current `That PIN isn't right.`, format `Use 4 to 8 digits.`, success `App lock is on.` / `PIN changed.` / `App lock is off.`
  - `func askPIN(owner walk.Form, p theme.Palette, title string, labels ...string) ([]string, bool, error)` — modal, one masked digits-only `LineEdit` per label (`ES_NUMBER`, max length 8), OK/Cancel, Esc cancels.

- [ ] **Step 1: Write the failing tests** in `internal/ui/settings_test.go`
```go
func TestPinUpdate(t *testing.T) {
	// pinSet: ["1234","1234"] → ok, stored verifies "1234", msg "App lock is on."
	// pinSet: ["1234","1235"] → !ok, msg "The PINs don't match."
	// pinSet: ["12","12"] → !ok, msg "Use 4 to 8 digits."
	// pinChange on stored(1234): ["0000","5678","5678"] → !ok "That PIN isn't right."; ["1234","5678","5678"] → ok, verifies "5678", "PIN changed."
	// pinOff on stored(1234): ["1234"] → ok, newStored "", "App lock is off."; ["9999"] → !ok "That PIN isn't right."
}
```
- [ ] **Step 2:** `go test ./internal/ui -run TestPinUpdate` — expected: FAIL (undefined).
- [ ] **Step 3:** Implement `pinUpdate` and `askPIN`; in `buildPrivacy` add an "App lock" row: state label `Off`/`On`, buttons `Set PIN` (when off) or `Change PIN` + `Turn off` (when on), and the note `The lock hides Wisp's window; it doesn't encrypt Wisp's files.` Each button: `askPIN` → `pinUpdate` → on ok store `KeyLockPIN` (account `""`) and call `OnSettingsChanged(KeyLockPIN)`; show `msg` on the section's status line.
- [ ] **Step 4:** `go test ./internal/ui ./internal/app` — expected: `ok`.
- [ ] **Step 5: Commit** `git commit -m "feat: app lock PIN in Settings"`

### Task 5: Lock page, when it locks, and what locking hides

**Files:**
- Create: `internal/ui/lockpage.go`
- Modify: `internal/app/app.go` (`closing`, `sizeChanged`, `restore`, `Attach`, `settingChanged`), `internal/app/winmsg.go` (session lock), `internal/app/logic.go` (`presenceCtl`), `internal/app/events.go` (`toast`), `internal/app/logic_test.go`

**Interfaces:**
- Consumes: `lock.Verify`, `lock.Backoff` (Task 2); `pageLock`, `showPage` (Task 3); `KeyLockPIN` (Task 4).
- Produces:
  - `type LockPage struct{ *walk.Composite }` — `func NewLockPage(parent walk.Container, p theme.Palette) (*LockPage, error)`, `OnUnlock(fn func(pin string))`, `OnReset(fn func())`, `Say(text string)`, `Wait(d time.Duration)` (disables the box, shows `Try again in N s`, re-enables itself), `Clear()`, `Focus()`, `SetPalette(p theme.Palette) error`. Texts: `Wisp is locked`, `Enter your PIN to see your chats`, `Unlock`, `Forgot your PIN?` + link `Reset Wisp`.
  - `func (a *App) lockNow()` — no-op unless `a.cfg[ui.KeyLockPIN] != ""`; sets `a.locked`, calls `a.closeMedia()`, `showPage`, `a.presence.appLock(true)`.
  - `func (a *App) closeMedia()` — created here as an empty stub; Task 10 gives it its body.
  - `func (a *App) unlock(pin string)`.
  - `presenceCtl` gains `appLocked bool` and `func (p *presenceCtl) appLock(locked bool)`; `stateOf` receives `p.locked || p.appLocked`.
  - `func toastSettings(cfg map[string]string, locked bool, accounts int) notify.Settings` — `PrivateMode` forced true while locked.

- [ ] **Step 1: Write the failing tests** in `internal/app/logic_test.go`
```go
func TestToastSettingsPrivateWhileLocked(t *testing.T) // cfg{notify.private:"false"}, locked → PrivateMode true; unlocked → false; Sound/MultiAccount carried through
func TestPresenceUnavailableWhileAppLocked(t *testing.T) // with the existing fake window (visible, active), appLock(true) sends PresenceUnavailable at once; appLock(false) sends PresenceAvailable
```
- [ ] **Step 2:** `go test ./internal/app -run 'TestToastSettings|TestPresenceUnavailableWhileAppLocked'` — expected: FAIL.
- [ ] **Step 3:** Implement. Lock moments: at start (`Attach`, before the window is first shown, when a PIN is set); when the window is hidden to the tray (`closing`) or minimised (`sizeChanged` to iconic) — locking at hide time means a restore never flashes chats; on session lock (`wtsSessionLock` also calls `lockNow`). `unlock`: `backoff.Wait(now) > 0` → `Wait(d)`; `lock.Verify` true → `Succeeded`, `a.locked = false`, `showPage`, `appLock(false)`, `a.side.FocusSearch()`; false → `Failed`, `Clear`, `Say("That PIN isn't right.")`, and `Wait` if the back-off now applies. `toast` uses `toastSettings`. Turning the lock on in Settings does not lock immediately.
- [ ] **Step 4:** `go test ./internal/app ./internal/ui` — expected: `ok`.
- [ ] **Step 5:** Live check on a scratch profile (with no account the link page hides Settings, so seed the PIN): a throwaway program opens the scratch `wisp.db` with `store.Open` and stores `lock.Hash("1234")` under `lock.pin`; start `wisp.exe` → lock page; wrong PIN ×5 → `Try again in 30 s`; `1234` → link page; close to tray and restore → lock page again.
- [ ] **Step 6: Commit** `git commit -m "feat: lock page with PIN, locking on hide, start and session lock"`

### Task 6: Reset Wisp

**Files:**
- Create: `cmd/wisp/reset.go`, `cmd/wisp/reset_test.go`
- Modify: `cmd/wisp/main.go` (`run`, `main`), `internal/app/app.go` (`Run`, new `reset`), `internal/app/logic.go`, `internal/app/logic_test.go`, `internal/ui/settingsdialog.go` (export `confirm` as `Confirm`)

**Interfaces:**
- Consumes: `LockPage.OnReset` (Task 5), `Backend.Logout` (exists).
- Produces:
  - `func Confirm(owner walk.Form, p theme.Palette, question, text, action string) (bool, error)` (renamed from `confirm`; callers updated).
  - `func logoutAll(ctx context.Context, accounts []string, logout func(context.Context, string) error) (failed []string)` in `internal/app/logic.go`.
  - `func (a *App) Run() (reset bool, err error)` — returns `true` when the run ended through Reset Wisp.
  - `func dataDir() (string, error)` (extracted from `run`), `func run() (reset bool, err error)`.
  - `func wipeData(dir string) error` — removes `wisp.db`, `wisp.db-wal`, `wisp.db-shm`, `session.db`, `session.db-wal`, `session.db-shm` and the `cache` folder; keeps `wisp.log`; missing entries are fine.
  - `func relaunch() error` — `os.Executable()` then `exec.Command(path).Start()`.

- [ ] **Step 1: Write the failing tests**
```go
// cmd/wisp/reset_test.go
func TestWipeDataKeepsOnlyTheLog(t *testing.T) // temp dir with every listed file + cache\a\b.jpg + wisp.log → only wisp.log remains, content unchanged
func TestWipeDataWithNothingThere(t *testing.T) // empty dir → nil error
// internal/app/logic_test.go — Review Focus 5
func TestLogoutAllReportsFailuresAndTriesEvery(t *testing.T) // accounts a,b,c; logout fails for a and c (offline) → failed == [a c]; b was still called
```
- [ ] **Step 2:** `go test ./cmd/wisp ./internal/app -run 'TestWipeData|TestLogoutAll'` — expected: FAIL.
- [ ] **Step 3:** Implement. `a.reset()`: `Confirm(a.mw, a.pal, "Reset Wisp?", "This logs out every account on this PC and deletes your chats, media and settings here. Your phone keeps its chats.", "Reset")`; on yes `logoutAll` with `netTimeout` per account; if any failed, alert `Some accounts couldn't be logged out. Remove "Wisp" under Linked devices on your phone.`; then mark reset and `exit()`. In `main`: after `run` returns with `reset` (every defer done: databases closed, instance handle released) → `wipeData(dir)` then `relaunch()`; show either error in a message box.
- [ ] **Step 4:** `go test ./cmd/wisp ./internal/app` — expected: `ok`.
- [ ] **Step 5:** Live check on a scratch profile only (never the real folder), PIN seeded as in Task 5: lock page → Reset Wisp → confirm → the app restarts on the link page and the scratch folder holds only `wisp.log` plus the new files of the fresh start.
- [ ] **Step 6: Commit** `git commit -m "feat: Reset Wisp from the lock page"`

### Task 7: Own line breaking for message text

**Files:**
- Create: `internal/ui/textlayout.go`, `internal/ui/textlayout_test.go`
- Modify: `internal/ui/chatlayout.go` (`measurer`, `bubble`, `layout`), `internal/ui/chatview.go` (`gdiMeasurer`, `paintBubble`), `internal/ui/chatview_test.go` (fake measurer)

**Interfaces:**
- Produces:
  - `type textLine struct{ start, end int }` — UTF-16 offsets into the bubble text, `end` exclusive; a line excludes the trailing `\n` / `\r\n` that ended it; trailing spaces belong to the line but are not counted in its width.
  - `func wrapText(units []uint16, maxW int, adv func(u []uint16) []int) []textLine` — `adv` returns the cumulative pixel width after each unit (`GetTextExtentExPointW` semantics).
  - `measurer` gains `advances(units []uint16, pt int, style walk.FontStyle) []int`.
  - `bubble` gains `units []uint16` and `lines []textLine` (set for any bubble with text); `b.text` keeps its rectangle (width = widest line, height = lines × line height).
- Algorithm (not determined by the signature): walk the units; break at `\n` (drop a preceding `\r`); when the next unit would pass `maxW`, break after the last space that fits; with no space, break before the unit that overflows but never between a high and a low surrogate; every line holds at least one character (a whole surrogate pair).

- [ ] **Step 1: Write the failing tests** (fake `adv`: every unit 10 px, a surrogate pair 20 px)
```go
func TestWrapTextBreaksAfterSpaces(t *testing.T)   // "aaa bbb ccc", maxW 70 → "aaa "|"bbb "|"ccc"
func TestWrapTextHonoursNewlines(t *testing.T)     // "a\nb\r\nc" → lines "a","b","c"; "a\n\nb" keeps an empty middle line
func TestWrapTextSplitsLongWords(t *testing.T)     // "abcdefghij", maxW 40 → "abcd","efgh","ij"
func TestWrapTextKeepsSurrogatePairs(t *testing.T) // "ab😀cd", maxW 30 → no line starts on a low surrogate
func TestWrapTextEmpty(t *testing.T)               // "" → no lines
func TestLayoutTextSizeFromLines(t *testing.T)     // layout() with the fake measurer: b.text.Height == len(b.lines)*lineH, width == widest line
```
- [ ] **Step 2:** `go test ./internal/ui -run 'TestWrapText|TestLayoutTextSize'` — expected: FAIL.
- [ ] **Step 3:** Implement `wrapText`; `gdiMeasurer.advances` with `GetTextExtentExPointW` (gdi32) on the measuring canvas's DC with the bubble font; `layout` uses `wrapText` instead of `measure` for body text; `paintBubble` draws each line with single-line formatting (`walk.TextSingleLine|walk.TextNoPrefix`) at `y + i*lineH`. Names, times and card labels keep `measure`/`drawText`.
- [ ] **Step 4:** `go test ./internal/ui` — expected: `ok` (existing layout tests still pass).
- [ ] **Step 5:** Visual check: render the same sample chat (short, long, multi-line, emoji, long URL) before and after with a throwaway preview test gated by an env var, compare PNGs — expected: identical wrapping except long words now wrap instead of overflowing. Delete the throwaway test.
- [ ] **Step 6: Commit** `git commit -m "refactor(ui): wrap and draw message text line by line"`

### Task 8: Free text selection by dragging

**Files:**
- Modify: `internal/ui/textlayout.go`, `internal/ui/textlayout_test.go`, `internal/ui/chatview.go`, `internal/theme/theme.go` (palette `Selection`)

**Interfaces:**
- Consumes: `textLine`, `bubble.units`, `bubble.lines`, `measurer.advances` (Task 7).
- Produces:
  - `type textPos struct{ msg, off int }` (message index, UTF-16 offset); `func orderPos(a, b textPos) (from, to textPos)`.
  - `func hitOffset(adv []int, x int) int` — boundary nearest to `x` (midpoint rule), never inside a surrogate pair.
  - `func wordAt(units []uint16, off int) (start, end int)` — letters/digits (any script, `unicode.IsLetter`/`IsDigit`) form a word; otherwise the single character.
  - `func selectedText(texts [][]uint16, from, to textPos) string` — the selected part of each message, joined with `"\r\n"`.
  - `theme.Palette.Selection string` — dark `#2b5f56`, light `#b9e3d8`.
  - ChatView: drag selection state, highlight painting, auto-scroll while dragging (a 30 ms timer that exists only during a drag past the top or bottom edge), double-click word, triple-click message, Esc / plain click clears, Ctrl+C and context **Copy** (Task 9) copy `selectedText`.
- Mouse rule: a press inside `b.text` starts a selection; a press elsewhere keeps today's behaviour (focus, media click/double-click).

- [ ] **Step 1: Write the failing tests**
```go
func TestHitOffsetMidpoints(t *testing.T)        // adv [10,20,30]: x=4→0, x=6→1, x=31→3, x=-5→0
func TestWordAt(t *testing.T)                    // "halo dunia!" off 6 → (5,10); off 4 (space) → (4,5); "kopi☕susu" around the emoji → the emoji alone; "naïve" → whole word
func TestSelectedTextAcrossMessages(t *testing.T) // Review Focus 2: texts "Hi 😀 there", "line1\r\nline2"; from {0,3} to {1,5} → "😀 there\r\nline1" with the emoji intact
func TestOrderPos(t *testing.T)                  // backwards drag {2,1},{0,4} → from {0,4} to {2,1}
```
- [ ] **Step 2:** `go test ./internal/ui -run 'TestHitOffset|TestWordAt|TestSelectedText|TestOrderPos'` — expected: FAIL.
- [ ] **Step 3:** Implement the helpers and the ChatView interaction (mouse down/move/up on the canvas, `getDoubleClickTime` for double/triple, highlight rects per line from `advances`, clipboard via `walk.Clipboard().SetText`). A clipboard failure is logged and shown as `Couldn't copy` in the header for 2 s.
- [ ] **Step 4:** `go test ./internal/ui` — expected: `ok`.
- [ ] **Step 5:** Live check in a throwaway harness (a hidden-by-default test window hosting a `ChatView` with sample messages, gated by an env var, like Task 7's preview): drag within one message, across three, double/triple click, Esc; the clipboard text matches. Delete the harness afterwards.
- [ ] **Step 6: Commit** `git commit -m "feat(ui): select message text by dragging"`

### Task 9: Whole-message copy, picking and the context menu

**Files:**
- Create: `internal/ui/copytext.go`, `internal/ui/copytext_test.go`
- Modify: `internal/ui/chatlayout.go` (`mediaInfo`, `parseMedia`), `internal/ui/chatview.go`

**Interfaces:**
- Produces:
  - `mediaInfo` gains `gif bool` (`VideoMessage.GetGifPlayback()`) and `voice bool` (`AudioMessage.GetPTT()`).
  - `func mediaLabel(m store.Message, info mediaInfo) string` — `[Photo]`, `[Sticker]`, `[Video]`, `[GIF]`, `[Voice message]`, `[Audio]`, `[Document: <name>]`.
  - `func messageText(m store.Message) string` — `bubbleText` when there is text (caption wins), else `mediaLabel`.
  - `func copyMessages(msgs []store.Message, sender func(store.Message) string, loc *time.Location) string` — one message → `messageText`; several → `[15:04, 02/01/2006] <sender>: <text>` lines joined with `"\r\n"`, in chat order; own messages use sender `You`.
  - ChatView: `picked` set of message IDs; Ctrl+click toggles, Shift+click picks the range from the focused message, Shift+↑/↓ extends; picked rows tinted with `Palette.Selection` and a 3 px accent bar; header shows `N selected`; context menu (right-click, Menu key, Shift+F10) with `Copy` (Ctrl+C) and `Select`; Ctrl+C order: text selection, else picked messages, else the focused message; Esc clears both.

- [ ] **Step 1: Write the failing tests**
```go
func TestCopyMessagesOne(t *testing.T)    // text message → its body exactly (no prefix)
func TestCopyMessagesMany(t *testing.T)   // 2 messages at fixed UTC times → "[08:30, 11/11/2026] Sinta: hi\r\n[08:31, 11/11/2026] You: ok"
func TestMediaLabels(t *testing.T)        // image/sticker/video/gif/voice/audio/document(name "invoice.pdf") → labels above
func TestCaptionWinsOverLabel(t *testing.T) // image with caption "look" → "look"
```
- [ ] **Step 2:** `go test ./internal/ui -run 'TestCopyMessages|TestMediaLabels|TestCaptionWins'` — expected: FAIL.
- [ ] **Step 3:** Implement `copytext.go`, the `mediaInfo` flags and the ChatView picking/menu (walk `Menu` with two `Action`s; position at the pointer, or at the focused bubble for the keyboard).
- [ ] **Step 4:** `go test ./internal/ui` — expected: `ok`.
- [ ] **Step 5: Commit** `git commit -m "feat(ui): copy whole messages, pick several, context menu"`

### Task 10: In-app photo and sticker viewer

**Files:**
- Create: `internal/ui/mediaview.go`, `internal/ui/mediaview_test.go`
- Modify: `internal/ui/chatview.go` (`OnOpenMedia`, `activate`, shared full-size decode), `internal/app/app.go` (chat column, `closeMedia`), `internal/app/chat.go` (`openMedia`, `needMedia`, media path updates), `internal/app/logic.go`, `internal/app/logic_test.go`

**Interfaces:**
- Consumes: `mediaInfo` (Task 9), `focusOn` (Task 1), `lockNow` calls `closeMedia` (Task 5).
- Produces:
  - `func (v *ChatView) OnOpenMedia(fn func(store.Message))` (was `func(path string)`); `activate` passes the message.
  - `type MediaView struct{ *walk.Composite }` — `func NewMediaView(parent walk.Container, p theme.Palette) (*MediaView, error)`, `Open(list []store.Message, i int, sender func(store.Message) string)`, `Close()`, `OnClose(fn func())`, `OnNeedMedia(fn func(store.Message))`, `OnOpenWith(fn func(path string))`, `SetMediaPath(id, path string)`, `SetDownloadError(id string, err error)`, `SetPalette(p theme.Palette) error`. Top bar: back arrow, sender, `Today at 15:04` / `Yesterday at 15:04` / `02/01/2006 at 15:04`, zoom percent (photos), `Save as…`, `Open with…`; states `Downloading…`, error + `Retry`, `Too large to show here`.
  - `func mediaSteps(msgs []store.Message, i int) (prev, next int)` — nearest image/sticker/video before and after `i`, `-1` when none.
  - `func fitRect(img walk.Size, area walk.Rectangle, zoom float64, pan walk.Point) walk.Rectangle` — zoom 1 = fit (letterboxed, never upscaled past 1:1 for small images); pan clamped so the image never leaves a gap it could cover.
  - `func zoomAround(zoom, factor float64, cursor walk.Point, shown walk.Rectangle, area walk.Rectangle) (float64, walk.Point)` — clamps to 1…4, keeps the image point under the cursor fixed.
  - `func decodeFull(path string) (*image.RGBA, error)` — same decoders as thumbnails, refuses more than 25 000 000 pixels with `media.ErrImageTooLarge`.
  - In `internal/app/logic.go`: `type mediaTarget int` (`targetViewer`, `targetPlayer`, `targetExternal`) and `func targetFor(m store.Message) mediaTarget` (image, sticker → viewer; video → player; others → external).
  - `func (a *App) closeMedia()`.

- [ ] **Step 1: Write the failing tests**
```go
func TestMediaStepsSkipsNonMedia(t *testing.T)   // [text, image, doc, video, text]: from 1 → (-1, 3); from 3 → (1, -1)
func TestFitRect(t *testing.T)                  // 1600×1200 in 800×400 at zoom 1 → 533×400 centred; 100×50 → 100×50 centred (no upscale); zoom 2 → 1067×800 with pan clamped
func TestZoomAroundKeepsCursorPoint(t *testing.T) // the image point under the cursor maps to the same screen point after zooming 1→2; zoom never exceeds 4 or goes below 1
func TestTargetFor(t *testing.T)                // image, sticker → viewer; video (also gif) → player; audio, document → external
// Review Focus 3, in internal/app: a viewer opened for an image whose MediaPath is set but the file is gone asks for the download
func TestOpenMissingFileAsksDownload(t *testing.T) // via the pure decision used by openMedia: path set + media.Missing → needs download
```
- [ ] **Step 2:** `go test ./internal/ui ./internal/app -run 'TestMediaSteps|TestFitRect|TestZoomAround|TestTargetFor|TestOpenMissing'` — expected: FAIL.
- [ ] **Step 3:** Implement. The media view lives in the chat column next to the chat view and input bar (only one of the two shows). Keys: Esc closes, ← → step, Ctrl+`+`/`-` zoom, `0` fits. Mouse: wheel zooms around the pointer, double-click toggles fit/2×, drag pans when zoomed. Save as copies the cached file through a `walk.FileDialog`. `Close` drops the decoded image and calls `debug.FreeOSMemory()`. Chat switch, account switch and `lockNow` call `closeMedia`.
- [ ] **Step 4:** `go test ./internal/ui ./internal/app` — expected: `ok`.
- [ ] **Step 5:** Live check in a throwaway harness window hosting a `MediaView` over sample PNG/JPEG files generated into `t.TempDir()`: open, zoom, pan, step, Save as, Open with, Esc; the process working set returns to within 1 MB of its value before opening, a few seconds after closing. Delete the harness afterwards.
- [ ] **Step 6: Commit** `git commit -m "feat: photos and stickers open inside the chat"`

### Task 11: In-app video and GIF player

**Files:**
- Create: `internal/ui/player.go`, `internal/ui/player_test.go`
- Modify: `internal/ui/mediaview.go`, `internal/ui/mediaview_test.go`, `internal/app/chat.go`

**Interfaces:**
- Consumes: `MediaView`, `targetPlayer` (Task 10), `mediaInfo.gif` (Task 9).
- Produces:
  - `type player struct` wrapping `IMFPMediaPlayer` from `mfplay.dll!MFPCreateMediaPlayer(url, TRUE, 0, NULL, hwnd, &p)` — no callback object. Methods: `func newPlayer(hwnd win.HWND, path string) (*player, error)`, `Play() error`, `Pause() error`, `Seek(d time.Duration) error`, `Position() (time.Duration, error)`, `Duration() (time.Duration, error)`, `State() (playerState, error)`, `SetMute(bool) error`, `UpdateVideo() error`, `Shutdown()`. Vtable slots follow `mfplay.h` order (IUnknown 0–2, Play 3, Pause 4, Stop 5, FrameStep 6, SetPosition 7, GetPosition 8, GetDuration 9, …, GetState 13, …, SetMute 24, …, UpdateVideo 32, …, Shutdown 37) — verify every slot against the Windows SDK header before use. Positions use `PROPVARIANT` `VT_I8` in 100 ns units with `MFP_POSITIONTYPE_100NS` (`GUID_NULL`).
  - `type playerState int` mirroring `MFP_MEDIAPLAYER_STATE` (empty 0, stopped 1, playing 2, paused 3, shutdown 4).
  - `type mediaPlayer interface{ Play() error; Pause() error; Seek(time.Duration) error; Position() (time.Duration, error); Duration() (time.Duration, error); State() (playerState, error); SetMute(bool) error; UpdateVideo() error; Shutdown() }` — `MediaView` holds this, so tests use a fake.
  - `func clockText(d time.Duration) string` — `0:05`, `1:02`, `1:02:03`.
  - `func seekAt(x, width int, total time.Duration) time.Duration` — clamped to `[0, total]`.
  - `func gifRestart(st playerState, pos, total time.Duration) bool` — true when a GIF has stopped at (or within 100 ms of) its end.
  - Video mode in `MediaView`: a child `CustomWidget` hosts the video window; control bar (play/pause, seek bar, `m:ss / m:ss`, mute) for videos only; GIFs start muted, loop, no bar; a 250 ms timer exists only while a video is open; unplayable (creation fails, or no `playing`/`paused` state within 5 s) → `Windows can't play this video` + `Open with…`.

- [ ] **Step 1: Write the failing tests**
```go
func TestClockText(t *testing.T)   // 5s→"0:05", 62s→"1:02", 3723s→"1:02:03", 0→"0:00"
func TestSeekAt(t *testing.T)      // x=50,w=200,45s → 11.25s; x=-3 → 0; x=250 → 45s
func TestGifRestart(t *testing.T)  // stopped at end → true; playing → false; stopped at 0 (never played) → false
// Review Focus 1: closing while a video plays shuts the player down
func TestMediaViewCloseShutsPlayerDown(t *testing.T) // MediaView with a fake mediaPlayer in state playing; Close() → fake.Shutdown called once, timer stopped
// gated live check: skipped unless WISP_TEST_MP4 names a local H.264 MP4
func TestPlayerPlaysMP4(t *testing.T) // hidden window; newPlayer; poll State up to 5s → playing; Duration > 0; Pause → paused; Shutdown
```
- [ ] **Step 2:** `go test ./internal/ui -run 'TestClockText|TestSeekAt|TestGifRestart|TestMediaViewCloseShutsPlayerDown'` — expected: FAIL.
- [ ] **Step 3:** Implement `player.go` (COM calls through `syscall` with `//go:uintptrescapes` helpers as in `internal/ui/icons/gdiplus.go`; `CoInitializeEx` is already done by walk on the UI thread) and the video mode. Keys: Space play/pause, ← → seek 5 s, `m` mute, Esc closes.
- [ ] **Step 4:** `go test ./internal/ui` — expected: `ok`; then `WISP_TEST_MP4=<some local mp4> go test ./internal/ui -run TestPlayerPlaysMP4 -v` — expected: PASS (record the file's codec in the report; do not commit the file).
- [ ] **Step 5:** Live check in the Task 10 style harness with the `WISP_TEST_MP4` file: plays with sound, seek and mute work, the same file marked as a GIF loops silently with no bar, `Close` stops playback; the process uses 0% CPU over 30 s after closing.
- [ ] **Step 6: Commit** `git commit -m "feat: videos and GIFs play inside the chat"`

### Task 12: README and whole-app verification

**Files:**
- Modify: `README.md`

- [ ] **Step 1:** README: "Run and link a phone" describes the link page; new section "App lock" (set in Settings › Privacy & Security, when it locks, Reset Wisp, the honest limit sentence from Task 4); "Known limitations" drops nothing that is still true and adds that shaped scripts (Arabic, Hebrew, Indic) may highlight slightly off; a sentence that photos, stickers, videos and GIFs open inside Wisp while audio and documents open in their usual program.
- [ ] **Step 2:** `go vet ./... && go test -count=1 ./...` — expected: every package `ok`.
- [ ] **Step 3:** `scripts\build.bat`; run `wisp.exe` on a scratch `LOCALAPPDATA` for 3 minutes (link page shown, nothing linked) and sample memory/CPU as before — expected: working set within 2 MB of the 33–34 MB measured before this plan, CPU 0.000% over 60 s.
- [ ] **Step 4: Commit** `git add README.md && git commit -m "docs: README for the link page, app lock, in-app media and copying"`
