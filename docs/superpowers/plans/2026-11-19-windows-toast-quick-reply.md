# Windows Toast Notifications with Avatar and Quick Reply Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver native Windows 10/11 rich toast notifications with circular avatars, unread message counts, and interactive inline quick-reply boxes.

**Architecture:** Generates adaptive Windows Toast XML with circular sender avatars, unread attribution, and interactive quick-reply actions. Dispatches toasts natively via WinRT COM (`combase.dll`) under `Wisp.App` AppUserModelID. Handles toast clicks and quick-reply submissions through a registered `wisp://` protocol scheme routed to the primary running instance via Win32 `WM_COPYDATA`.

**Tech Stack:** Go, Win32 API (`user32.dll`, `shell32.dll`), WinRT COM (`combase.dll`), Windows Toast XML.

**Spec:** `docs/superpowers/specs/2026-11-19-windows-toast-quick-reply-design.md`

## Global Constraints

- No external third-party dependencies added to `go.mod`.
- Sub-millisecond toast dispatch (no child `powershell.exe` process spawning).
- Complete privacy protection: when `PrivateMode` is true or the app is locked with PIN, message body is hidden, avatars are omitted, and quick reply is disabled.
- Graceful fallback to shell tray balloon if WinRT COM is unavailable or fails.

## Review Focus

1. **Protocol Injection Safety**: Malformed or unexpected query parameters in `wisp://` URLs must never panic or crash the application.
2. **Avatar Path Handling**: Missing, corrupted, or non-existent avatar paths must not break toast display; toasts must render cleanly with or without avatars.
3. **App Lock Isolation**: When Wisp is locked, quick reply actions must never allow unauthorized message transmission.
4. **Duplicate Notification Reset**: Multiple rapid messages from the same sender must each pop a fresh toast without Windows suppression.
5. **Special Character Escaping**: XML special characters (`&`, `<`, `>`, `"`, `'`) in sender names and message bodies must be strictly escaped to avoid invalid XML syntax errors.

---

### Task 1: WinRT Toast XML Builder & COM Dispatcher

**Files:**
- Create: `internal/notify/winrt.go`
- Create: `internal/notify/winrt_test.go`
- Modify: `internal/notify/toast.go`
- Modify: `internal/notify/tray.go`

- [x] **Step 1: Write unit tests for Toast XML generation**
  Test XML construction with title, body, circular avatar (`<image placement="appLogoOverride" hint-crop="circle"...>`), unread attribution, and quick-reply action button (`hint-inputId="replyText"`). Test XML escaping for special characters and omission of reply/avatar in private mode.
  Run `go test -v ./internal/notify` to confirm test failures before implementation.

- [x] **Step 2: Implement WinRT Toast XML generation and COM dispatch in `internal/notify/winrt.go`**
  Implement `BuildToastXML(in Input, s Settings) string` with XML entity escaping.
  Implement `ShowWinRTToast(xml string, appID string) error` using `combase.dll` (`RoInitialize`, `RoActivateInstance`, `RoGetActivationFactory`).
  Implement fallback to tray balloon when WinRT call fails.

- [x] **Step 3: Update `Input` and `notify.Dispatcher`**
  Add `AvatarPath string`, `UnreadCount int` to `notify.Input`.
  Connect `notify.Dispatcher` to call WinRT toast when available.

- [x] **Step 4: Run tests and verify**
  Run `go test -v ./internal/notify` and ensure all tests pass.

---

### Task 2: Protocol Scheme Registration & IPC via WM_COPYDATA

**Files:**
- Modify: `cmd/wisp/main.go`
- Modify: `internal/app/winmsg.go`
- Create: `internal/app/winmsg_test.go`

- [x] **Step 1: Write unit tests for protocol URL parsing**
  Test parsing `wisp://open?acct=...&jid=...` and `wisp://reply?acct=...&jid=...&replyText=...` in `internal/app/winmsg_test.go`.
  Run `go test -v ./internal/app` to confirm tests fail.

- [x] **Step 2: Implement WM_COPYDATA receiver in `internal/app/winmsg.go`**
  Subclass `mainWndProc` to handle `win.WM_COPYDATA` (0x004A).
  Extract string payload and route to `handleProtocolURL(url string)`.

- [x] **Step 3: Register `wisp://` URI scheme and forward arguments in `cmd/wisp/main.go`**
  Add `ensureProtocol()` to register `HKCU\Software\Classes\wisp` pointing to `wisp.exe "%1"`.
  In `run()`, when an instance already runs and `len(os.Args) > 1`, forward `os.Args[1]` to the running instance via `SendMessage(hwnd, WM_COPYDATA, ...)` and exit.

- [x] **Step 4: Run tests and verify**
  Run `go test -v ./cmd/wisp ./internal/app` and verify tests pass.

---

### Task 3: Quick Reply Sending & App Lock Integration

**Files:**
- Modify: `internal/app/events.go`
- Modify: `internal/app/chat.go`
- Modify: `internal/app/app.go`

- [x] **Step 1: Write unit tests for quick reply dispatch**
  Test that receiving a reply action when locked does nothing, and when unlocked calls `SendText` with the target account and JID.

- [x] **Step 2: Populate AvatarPath and UnreadCount in `internal/app/events.go`**
  In `a.toast(m)`, check `a.cache.AvatarPath(m.AccountID, m.SenderJID)` (and chat JID for 1:1 chats); if the file exists on disk, set `in.AvatarPath`. Set `in.UnreadCount = a.chatUnread(m.ChatJID)`.

- [x] **Step 3: Implement `sendQuickReply(acct, jid, text)` in `internal/app/chat.go`**
  Send text message using `a.mgr.SendText(context.Background(), acct, jid, text, nil, nil)`.
  If the active chat is `(acct, jid)`, ensure the message reflects in UI.

- [x] **Step 4: Run tests and verify**
  Run `go test -v ./internal/app` and confirm all tests pass.

---

### Task 4: End-to-End Build and Verification

**Files:**
- Target: `wisp.exe`

- [x] **Step 1: Run full test suite across packages**
  Run `go test -count=1 ./...` to ensure no regressions.

- [x] **Step 2: Rebuild release binary**
  Run `scripts\build.bat` to compile updated `wisp.exe`.

- [x] **Step 3: Launch updated `wisp.exe` and test toast appearance**
  Trigger test toast and verify circular avatar, text layout, and quick reply action.
