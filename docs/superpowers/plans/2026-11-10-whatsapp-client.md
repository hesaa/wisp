# WhatsApp Native Desktop Client Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build an ultra-efficient, 100% native Windows desktop WhatsApp client (`wa-go.exe`) using Go, `lxn/walk` (Win32), and `whatsmeow`, with multi-account support, paginated message persistence, media streaming, system tray integration, and complete Dark/Light theming, consuming under 55 MB idle RAM.

**Architecture:** A single native executable process composed of three layers: (1) a Win32 UI message loop driven by `lxn/walk` with GDI+/Direct2D custom-painted bubbles; (2) an asynchronous application dispatcher coordinating local SQLite storage and media disk caching; and (3) concurrent `whatsmeow` background worker goroutines maintaining independent Multi-Device WebSocket connections for each paired account.

**Tech Stack:** Go 1.23+, `go.mau.fi/whatsmeow`, `github.com/lxn/walk`, `modernc.org/sqlite` (pure Go SQLite), `github.com/tc-hib/go-winres` (Windows Manifest & DPI resources).

**Spec:** [`D:\project\wa-go\docs\superpowers\specs\2026-11-10-whatsapp-client-design.md`](file:///D:/project/wa-go/docs/superpowers/specs/2026-11-10-whatsapp-client-design.md)

---

## Global Constraints

* Target OS: Windows 10 & 11 (64-bit).
* RAM target: Idle memory strictly under 55 MB RAM with 1 account, under 65 MB with 2 accounts.
* CPU target: 0.0% CPU usage when idle or minimized to tray.
* No browser runtimes: Zero Electron, Chromium, CEF, or WebView2 components.
* No emoji icons: All interface navigation, tool, and action buttons must use monochrome vector line icons (Segoe / Fluent style).
* Database: Use `modernc.org/sqlite` (pure Go, CGO-free for seamless Windows compilation).
* Memory management: Media files must never be buffered entirely in RAM; stream directly to `%LOCALAPPDATA%\wa-go\cache\`.
* Database pagination: Chat message queries must strictly fetch 50 messages per page.

---

## Review Focus

1. **Large HistorySync payload crashing RAM:** When WhatsApp sends a multi-megabyte `HistorySync` archive upon initial pairing, the decompression pipeline must stream through a bounded buffer and insert in batches of 100 into SQLite rather than loading the whole payload into memory.
2. **Win32 Thread Safety Violations:** Any event arriving from background `whatsmeow` goroutines must be wrapped inside `mainWindow.Synchronize(func() { ... })` before touching any Walk control.
3. **Ghost Presence Leaks:** Minimizing the window to the tray must immediately broadcast `types.PresenceUnavailable` so contacts do not see the account online 24/7.
4. **Media Cache Spikes:** High-resolution image thumbnails decoded for display must be downsampled immediately before painting, and temporary byte buffers released for garbage collection.
5. **Multi-Account State Collisions:** Incoming message events must carry their corresponding `AccountID` so message routing, active view updating, and unread counters never cross-contaminate between different phone numbers.

---

## File Structure & Responsibilities

```
D:\project\wa-go\
├── cmd\
│   └── wa-go\
│       ├── main.go               # Application entry point & lifecycle
│       └── winres\
│           └── winres.json       # Manifest, icon, and DPI awareness definition
├── internal\
│   ├── theme\
│   │   ├── theme.go              # Dark/Light palettes & DWM immersive dark mode API
│   │   └── theme_test.go
│   ├── ui\
│   │   ├── icons\
│   │   │   ├── icons.go          # Vector monochrome line icons & glyph paths
│   │   │   └── icons_test.go
│   │   ├── rail.go               # Left account switcher rail
│   │   ├── sidebar.go            # Stories row, search bar, and chat list
│   │   ├── chatview.go           # Custom bubble canvas & scroll pagination
│   │   ├── inputbar.go           # Input box, attachment, emoji, and GIF buttons
│   │   ├── statusview.go         # Stories / Status popup viewer
│   │   └── settingsdialog.go     # Tabbed settings dialog (Privacy, Stealth, Proxy, etc.)
│   ├── store\
│   │   ├── db.go                 # SQLite schema, migrations, connection pool
│   │   ├── messages.go           # 50-item paginated queries, thread updates
│   │   └── messages_test.go
│   ├── media\
│   │   ├── cache.go              # Disk cache streamer, thumbnails, size cleanup
│   │   └── cache_test.go
│   ├── notify\
│   │   ├── toast.go              # Windows Toast notification dispatcher
│   │   ├── tray.go               # Win32 system tray icon & context menu
│   │   └── toast_test.go
│   └── whatsapp\
│       ├── client.go             # whatsmeow wrapper (Noise, QR, Signal crypto)
│       ├── manager.go            # Multi-account coordinator & event router
│       └── manager_test.go
├── docs\
│   └── superpowers\
│       ├── specs\
│       └── plans\
├── go.mod
└── go.sum
```

---

## Tasks

### Task 1: Project Scaffolding & Windows Resource Manifest

**Files:**
- Create: `D:\project\wa-go\go.mod`
- Create: `D:\project\wa-go\cmd\wa-go\winres\winres.json`
- Create: `D:\project\wa-go\cmd\wa-go\main_smoke_test.go`

- [ ] Step 1: Initialize `go.mod` for `wa-go` with Go 1.23.
- [ ] Step 2: Install primary dependencies: `github.com/lxn/walk`, `go.mau.fi/whatsmeow`, `modernc.org/sqlite`, and `github.com/tc-hib/go-winres`.
- [ ] Step 3: Create `winres.json` configuring Windows Common Controls v6 (`comctl32.dll`) and Per-Monitor V2 DPI awareness.
- [ ] Step 4: Write failing smoke test `main_smoke_test.go` asserting package compilation and manifest presence.
- [ ] Step 5: Run `go test ./cmd/wa-go/...` to verify failure, then generate `.syso` resources via `go-winres make`.
- [ ] Step 6: Re-run test to confirm passing.
- [ ] Step 7: Commit task changes.

---

### Task 2: Theme Engine & Windows DWM Immersive Dark Mode

**Files:**
- Create: `D:\project\wa-go\internal\theme\theme.go`
- Create: `D:\project\wa-go\internal\theme\theme_test.go`

- [ ] Step 1: Write failing unit test `theme_test.go` asserting Dark and Light palettes have valid hex codes for backgrounds, text, and message bubbles, and that `GetPalette(ThemeDark)` returns correct values.
- [ ] Step 2: Run `go test ./internal/theme/...` to verify test fails.
- [ ] Step 3: Implement `theme.go` defining `ThemeMode` (`ThemeSystem`, `ThemeDark`, `ThemeLight`), `Palette` struct, and Windows API binding calling `dwmapi.dll:DwmSetWindowAttribute` with `DWMWA_USE_IMMERSIVE_DARK_MODE (20)` for the native title bar.
- [ ] Step 4: Run `go test ./internal/theme/...` to verify test passes.
- [ ] Step 5: Commit task changes.

---

### Task 3: Vector Line Icon Drawing Utilities

**Files:**
- Create: `D:\project\wa-go\internal\ui\icons\icons.go`
- Create: `D:\project\wa-go\internal\ui\icons\icons_test.go`

- [ ] Step 1: Write failing test in `icons_test.go` asserting that vector definitions exist for `IconChat`, `IconStatus`, `IconSettings`, `IconSearch`, `IconAttach`, `IconEmoji`, `IconSend`, and `IconDoubleCheck`.
- [ ] Step 2: Run `go test ./internal/ui/icons/...` to verify test fails.
- [ ] Step 3: Implement `icons.go` with GDI+ / Direct2D path drawing functions that render sharp, scalable monochrome vector line glyphs (Segoe Fluent style) onto any Walk canvas or bitmap target.
- [ ] Step 4: Run `go test ./internal/ui/icons/...` to verify test passes.
- [ ] Step 5: Commit task changes.

---

### Task 4: SQLite Message Store & 50-Item Paginated Queries

**Files:**
- Create: `D:\project\wa-go\internal\store\db.go`
- Create: `D:\project\wa-go\internal\store\messages.go`
- Create: `D:\project\wa-go\internal\store\messages_test.go`

- [ ] Step 1: Write failing tests in `messages_test.go` testing:
  - Inserting 120 messages for a chat thread.
  - Querying page 0 (offset 0, limit 50) returns the 50 most recent messages in reverse timestamp order.
  - Querying page 1 (offset 50, limit 50) returns the next 50 messages.
  - Updating read status and calculating unread badge counts per chat.
- [ ] Step 2: Run `go test ./internal/store/...` to verify test fails.
- [ ] Step 3: Implement `db.go` with `modernc.org/sqlite` connection initialization, WAL mode (`PRAGMA journal_mode=WAL`), and schema creation (`accounts`, `chats`, `messages`, `settings`).
- [ ] Step 4: Implement `messages.go` with batched transaction inserts (`InsertMessagesBatch`), paginated selects (`GetChatMessages`), and unread updates.
- [ ] Step 5: Run `go test ./internal/store/...` to confirm tests pass.
- [ ] Step 6: Commit task changes.

---

### Task 5: Media Streaming Engine & Disk Cache

**Files:**
- Create: `D:\project\wa-go\internal\media\cache.go`
- Create: `D:\project\wa-go\internal\media\cache_test.go`

- [ ] Step 1: Write failing test in `cache_test.go` verifying that streaming an image payload writes to `%LOCALAPPDATA%\wa-go\cache\<account>\media\<hash>.jpg`, generates a 200x200 thumbnail without loading the full video/doc in memory, and calculates total cache size.
- [ ] Step 2: Run `go test ./internal/media/...` to confirm test fails.
- [ ] Step 3: Implement `cache.go` with streaming `io.Copy` to disk, image thumbnail extraction (using standard Go `image/jpeg` and `image/png`), `GetCacheSizeBytes()`, and `ClearMediaCache()`.
- [ ] Step 4: Add helper `OpenInDefaultApp(filePath string)` invoking `windows.ShellExecute` to delegate video and document viewing to native Windows apps.
- [ ] Step 5: Run `go test ./internal/media/...` to confirm tests pass.
- [ ] Step 6: Commit task changes.

---

### Task 6: WhatsApp Connection Manager & Multi-Account Coordinator

**Files:**
- Create: `D:\project\wa-go\internal\whatsapp\client.go`
- Create: `D:\project\wa-go\internal\whatsapp\manager.go`
- Create: `D:\project\wa-go\internal\whatsapp\manager_test.go`

- [ ] Step 1: Write failing tests in `manager_test.go` verifying:
  - Registering two accounts in the manager.
  - Event dispatch routing incoming messages to the correct account handler.
  - Toggling presence to `PresenceUnavailable` sets offline state for all active accounts.
- [ ] Step 2: Run `go test ./internal/whatsapp/...` to confirm test fails.
- [ ] Step 3: Implement `client.go` wrapping `whatsmeow.Client`, handling QR code channels, setting up `sqlstore` device containers, and handling incoming `events.Message` and `events.Receipt`.
- [ ] Step 4: Implement `manager.go` orchestrating concurrent account connections, broadcasting connection status, and delegating incoming events to registered UI listeners.
- [ ] Step 5: Run `go test ./internal/whatsapp/...` to verify test passes.
- [ ] Step 6: Commit task changes.

---

### Task 7: Windows Toast Notifications & System Tray Lifecycle

**Files:**
- Create: `D:\project\wa-go\internal\notify\toast.go`
- Create: `D:\project\wa-go\internal\notify\tray.go`
- Create: `D:\project\wa-go\internal\notify\toast_test.go`

- [ ] Step 1: Write failing test in `toast_test.go` asserting notification formatting, privacy suppression (hiding message text and sender name when Private Mode is enabled), and sound toggle logic.
- [ ] Step 2: Run `go test ./internal/notify/...` to confirm test fails.
- [ ] Step 3: Implement `toast.go` dispatching native Windows 10/11 toast alerts.
- [ ] Step 4: Implement `tray.go` wrapping `walk.NotifyIcon` with an unread badge indicator, double-click to restore window, and a right-click context menu with **Open WhatsApp Lite** and **Exit**.
- [ ] Step 5: Run `go test ./internal/notify/...` to confirm tests pass.
- [ ] Step 6: Commit task changes.

---

### Task 8: UI — Account Rail & Sidebar Navigation

**Files:**
- Create: `D:\project\wa-go\internal\ui\rail.go`
- Create: `D:\project\wa-go\internal\ui\sidebar.go`
- Create: `D:\project\wa-go\internal\ui\sidebar_test.go`

- [ ] Step 1: Write failing tests in `sidebar_test.go` asserting:
  - Chat filtering logic filters conversations case-insensitively by contact name or message snippet.
  - Unread badge counter updates when a new message arrives.
  - Account switching fires the active account change event.
- [ ] Step 2: Run `go test ./internal/ui/...` to confirm test fails.
- [ ] Step 3: Implement `rail.go` (56px width) with account profile circles, active green indicator, unread badge, `+` Add Account button, and navigation icons (Chats, Status, Settings).
- [ ] Step 4: Implement `sidebar.go` (280px width) with top Status/Stories avatar strip, New Chat vector button dialog, search input box, and a scrollable list view of recent chats with real-time updates.
- [ ] Step 5: Run `go test ./internal/ui/...` to confirm tests pass.
- [ ] Step 6: Commit task changes.

---

### Task 9: UI — Chat Pane & Custom Message Bubbles (GDI+/Direct2D)

**Files:**
- Create: `D:\project\wa-go\internal\ui\chatview.go`
- Create: `D:\project\wa-go\internal\ui\inputbar.go`
- Create: `D:\project\wa-go\internal\ui\chatview_test.go`

- [ ] Step 1: Write failing test in `chatview_test.go` verifying bubble layout geometry calculations:
  - Right-aligned for outgoing messages with soft green background (`#d9fdd3` in light, `#005c4b` in dark).
  - Left-aligned for incoming messages (`#ffffff` in light, `#202c33` in dark).
  - Image thumbnail placement and PDF/document attachment card layout.
  - Pagination boundary detection (scrolling to top triggers `LoadPreviousPage` signal).
- [ ] Step 2: Run `go test ./internal/ui/...` to confirm test fails.
- [ ] Step 3: Implement `chatview.go` using a custom-drawn `walk.CustomWidget` rendering message bubbles with high-performance double-buffering.
- [ ] Step 4: Implement `inputbar.go` with vector buttons for Emoji, GIF search popup, attachment clip, multi-line edit box, and green Send button.
- [ ] Step 5: Run `go test ./internal/ui/...` to confirm tests pass.
- [ ] Step 6: Commit task changes.

---

### Task 10: UI — Status Popup Viewer & Settings Dialog

**Files:**
- Create: `D:\project\wa-go\internal\ui\statusview.go`
- Create: `D:\project\wa-go\internal\ui\settingsdialog.go`
- Create: `D:\project\wa-go\internal\ui\settings_test.go`

- [ ] Step 1: Write failing test in `settings_test.go` verifying that updating privacy settings (e.g. read receipts, last seen) and stealth settings (disable typing indicator) correctly updates both the local SQLite `settings` table and triggers `whatsmeow.Client.SetPrivacySetting`.
- [ ] Step 2: Run `go test ./internal/ui/...` to confirm test fails.
- [ ] Step 3: Implement `statusview.go` as a lightweight modal dialog displaying contact story photos and captions with timer progression.
- [ ] Step 4: Implement `settingsdialog.go` with the 6 tabbed panels:
  - **Privacy & Security:** Read receipts, last seen, profile photo, disappearing messages.
  - **Chats & Presence:** Ghost mode (hide typing indicator), always appear offline.
  - **Media & Storage:** Auto-download rules, cache size display, "Clear Cache" action.
  - **Network & Proxy:** Custom SOCKS5/HTTP proxy configuration.
  - **Accounts & Sessions:** Active accounts list, Add Account QR trigger, Logout button.
  - **Notifications & Theme:** Windows Toast toggles, privacy mode, and Theme selector (System Default / Dark / Light).
- [ ] Step 5: Run `go test ./internal/ui/...` to confirm tests pass.
- [ ] Step 6: Commit task changes.

---

### Task 11: End-to-End Integration, Resource Benchmark & Binary Build

**Files:**
- Create: `D:\project\wa-go\cmd\wa-go\main.go`
- Create: `D:\project\wa-go\scripts\build.bat`

- [ ] Step 1: Implement `main.go` connecting Walk `MainWindow`, Manager, Store, Media Cache, Notification Tray, and Theme Engine.
- [ ] Step 2: Hook the window close event (`WM_CLOSE`) to minimize to tray and broadcast `types.PresenceUnavailable`.
- [ ] Step 3: Create `scripts\build.bat` building the optimized native binary: `go build -ldflags="-H windowsgui -s -w" -o wa-go.exe ./cmd/wa-go`.
- [ ] Step 4: Execute `scripts\build.bat` and verify that `wa-go.exe` compiles cleanly without warnings.
- [ ] Step 5: Run automated test suite: `go test -v ./...`.
- [ ] Step 6: Launch `wa-go.exe`, verify QR code generation, switch between Dark and Light mode, verify vector icons, and measure idle memory with Task Manager / PowerShell (`Get-Process wa-go | Select-Object WorkingSet64`) to confirm it stays well within the **35–55 MB RAM target**.
- [ ] Step 7: Commit task changes.
