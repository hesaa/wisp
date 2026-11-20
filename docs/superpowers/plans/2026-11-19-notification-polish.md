# Notification & Action Center Polish Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Polish the native Windows notification experience with an inline "Mark as read" button, silent audio control, rich photo hero previews, chat grouping, and automatic dismissal of read notifications from Action Center.

**Architecture:** Enhances `BuildToastXML` to include `<audio silent="true"/>`, `<action content="Mark as read"...>`, and `<image placement="hero"...>`. Implements `IToastNotification2` `Tag` & `Group` properties and `ToastNotificationHistory` removal in `internal/notify/winrt.go`. Routes `wisp://markread` via Win32 `WM_COPYDATA` to mark chats read and clear Action Center notifications on read.

**Tech Stack:** Go, WinRT COM (`Windows.UI.Notifications.ToastNotification2`, `ToastNotificationHistory`), Windows Toast XML.

**Spec:** `docs/superpowers/specs/2026-11-19-notification-polish-design.md`

## Global Constraints

- No external dependencies added to `go.mod`.
- Complete privacy protection: when `PrivateMode` is true or app is locked, "Mark as read" action and hero images are omitted.
- Missing or invalid media paths must gracefully omit the hero image without failing the toast.

## Review Focus

1. **Audio Double-Chime**: Muted or custom WAV notifications must never trigger Windows' default system sound.
2. **Mark as Read Concurrency**: Rapid clicks on "Mark as read" must not conflict with ongoing database writes or UI state.
3. **Lock State Safety**: When locked, clicking "Mark as read" or submitting quick replies must be rejected.
4. **Action Center Cleanup**: Reading a chat in Wisp must remove its toasts from Action Center history without crashing if no notifications were pending.
5. **XML Sanitization**: Escaping must continue to protect all dynamic fields in the Toast XML.

---

### Task 1: Toast XML Polish (Audio Silence, "Mark as Read", Hero Images)

**Files:**
- Modify: `internal/notify/toast.go`
- Modify: `internal/notify/winrt.go`
- Modify: `internal/notify/winrt_test.go`

- [x] **Step 1: Write unit tests for new XML elements**
  Add unit tests in `internal/notify/winrt_test.go` verifying:
  - `<audio silent="true"/>` when `n.Silent == true`
  - `<action content="Mark as read" activationType="protocol" arguments="wisp://markread?..." />` when `n.CanReply == true`
  - `<image placement="hero" src="..."/>` when `n.ImagePath != ""`
  - Private mode suppresses the hero image and the "Mark as read" action.
  Run `go test -v ./internal/notify` to verify failure before implementation.

- [x] **Step 2: Implement XML enhancements in `internal/notify/winrt.go`**
  Add `ImagePath string` to `notify.Input` and `notify.Notification`.
  Update `BuildToastXML` to format audio, mark read action, and hero image.

- [x] **Step 3: Run tests and verify**
  Run `go test -v ./internal/notify` and verify all tests pass.

---

### Task 2: WinRT Tagging, Grouping & History Clearing

**Files:**
- Modify: `internal/notify/winrt.go`
- Modify: `internal/notify/toast.go`
- Modify: `internal/notify/winrt_test.go`

- [x] **Step 1: Write unit tests for Tag, Group, and History Clear**
  Test setting Tag (`ChatJID`) and Group (`AccountID`) on notifications, and test `ClearChatToasts(accountID, chatJID, appID)`.

- [x] **Step 2: Implement `put_Tag`, `put_Group`, and `ClearChatToasts` in `internal/notify/winrt.go`**
  Query `IToastNotification2` on the created toast object and set `Tag` and `Group`.
  Implement `ClearChatToasts` using `ToastNotificationManager.get_History` -> `Remove(tag, group, appID)`.

- [x] **Step 3: Run tests and verify**
  Run `go test -v ./internal/notify` and ensure tests pass.

---

### Task 3: App Integration (Media Previews, Mark as Read & Auto-Dismiss)

**Files:**
- Modify: `internal/app/events.go`
- Modify: `internal/app/winmsg.go`
- Modify: `internal/app/chat.go`
- Modify: `internal/app/winmsg_test.go`

- [x] **Step 1: Write unit tests for `markread` protocol handling**
  Test `parseProtocolAction("wisp://markread?acct=...&jid=...")` and handler behavior.

- [x] **Step 2: Wire `markread` protocol action to `a.markChatRead`**
  In `handleProtocolRaw`, dispatch `kind == "markread"` to `a.mw.Synchronize(func() { a.markChatRead(c) })` and clear the toast from Action Center.

- [x] **Step 3: Auto-clear Action Center when opening or reading a chat in Wisp**
  In `a.markChatRead` and `a.openChat`, call `notify.ClearChatToasts(acct, jid, "Wisp.App")`.

- [x] **Step 4: Attach image paths for incoming media messages**
  In `a.toast(m)`, if `m.Type == "image"` and `m.MediaPath` exists on disk, set `in.ImagePath = m.MediaPath`.

- [x] **Step 5: Run tests and verify**
  Run `go test -v ./internal/... ./cmd/...` and ensure all tests pass.

---

### Task 4: End-to-End Build and Verification

**Files:**
- Target: `wisp.exe`

- [x] **Step 1: Run full test suite across packages**
  Run `go test -count=1 ./...`.

- [x] **Step 2: Rebuild release binary**
  Run `scripts\build.bat`.

- [x] **Step 3: Launch updated `wisp.exe` and test live notification**
  Verify the toast displays with silent audio control, mark-as-read button, and Action Center auto-clear.
