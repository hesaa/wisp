# WhatsApp Native Desktop Client (Go + Walk Win32) Design Document

**Status:** Final Draft — Awaiting Approval  
**Date:** 2026-11-10  
**Target Path:** `D:\project\wa-go`  

---

## 1. Intent & Problem Statement

Official and third-party WhatsApp desktop applications are built on Chromium or Electron, frequently consuming between 400 MB to 1 GB of RAM and ongoing background CPU cycles.

This project delivers an ultra-lightweight, 100% native Windows desktop WhatsApp client targeting:
* **Idle Memory:** ~35–55 MB RAM total (under 65 MB even with two active accounts connected).
* **CPU Usage:** 0.0% at idle, near-instant launch speed.
* **Single Binary:** A standalone, native Windows `.exe` without requiring external runtimes, Node.js, or browser engines.
* **Clean Visuals & Dark Mode:** Clean, sharp vector line icons (Segoe / Fluent style). Full native Dark Mode and Light Mode support (with automatic Windows system theme detection via DWM API). No color emojis used as UI buttons.

---

## 2. Scope & Core Requirements

### 2.1 In Scope
1. **Multi-Account Support:** Simultaneous pairing and background connection for multiple phone numbers. A vertical navigation rail allows one-click switching while all accounts remain connected and alert in real time.
2. **Persistent Message History (Paginated):** Local SQLite storage saves conversation threads and messages across restarts. Queries are strictly paginated (last 40–50 messages loaded into RAM per active chat; older messages loaded on scroll) to preserve low memory.
3. **Rich Media & Attachments:**
   * Images: In-app thumbnail rendering inside speech bubbles with click-to-view.
   * Videos & Audio: File size badge, thumbnail preview, and one-click opening in the default Windows media player via `ShellExecute`.
   * Documents (PDF, Word, ZIP): In-bubble file card with size, icon, and download/open action.
   * Media is streamed directly to a disk cache (`%LOCALAPPDATA%\wa-go\cache\`) rather than buffered in RAM.
4. **Status / Stories:** Browse contacts' temporary status updates via `status@broadcast` through a top avatar strip in the sidebar, opening a lightweight popup viewer for photos and captions.
5. **Windows Notifications & Background Lifecycle:**
   * Native Windows 10/11 desktop toast notifications with sender name, message snippet, and click-to-open.
   * Clicking the window close button (`X`) minimizes to the Windows System Tray to keep notifications alive, while marking WhatsApp presence as `PresenceUnavailable` (offline) so contacts do not see you online 24/7.
   * Context menu on tray icon provides **Open** and **Exit** (full shutdown).
6. **Input Features:** Quick emoji picker, GIF search (via lightweight public API), and file attachment picker.
7. **Initiating New Chats:** A "New Chat" vector action in the sidebar header to search contacts or enter an unsaved phone number directly.
8. **Settings Window:** Account management, privacy & security toggles (read receipts, last seen), stealth options (ghost typing mode, appear offline), proxy settings, notification toggles, cache cleanup, and **Theme selection** (System Default / Dark / Light).
9. **Dark Mode & Theming:**
   * Full palette support: Dark background (`#0b141a`), sidebar & rail (`#111b21`), panel headers (`#202c33`), dark green outgoing bubbles (`#005c4b`), and incoming message bubbles (`#202c33`).
   * Window title bar themed using Windows `DwmSetWindowAttribute(hwnd, DWMWA_USE_IMMERSIVE_DARK_MODE, ...)` so the standard Windows title bar matches the dark theme.

### 2.2 Out of Scope (Non-Goals)
* **Voice & Video Calling:** Excluded. WhatsApp's proprietary WebRTC signaling and media codec pipelines require heavy third-party media stacks that inflate binary size and RAM by orders of magnitude.
* **Status Posting / Uploading:** Viewing contacts' stories is in scope; publishing new stories from this desktop client is deferred.
* **Cross-Platform Support:** Specifically built for Windows (Win32 API) to maximize efficiency and minimize runtime abstraction layers.

### 2.3 Anti-Goals
* **No Electron / CEF / WebView2:** No web browsers, headless Chromium instances, or web wrappers.
* **No Emoji as UI Buttons:** All UI navigation, action, and tool buttons must use crisp vector line icons or Segoe Fluent Glyphs.

---

## 3. Technology Stack & Architecture

### 3.1 Core Components
* **Language:** Go (Golang) 1.23+
* **WhatsApp Protocol Engine:** [`go.mau.fi/whatsmeow`](https://github.com/tulir/whatsmeow)
  * Implements WhatsApp Multi-Device (MD) protocol over WebSockets.
  * Direct Noise handshake, Protobuf serialization, Signal Protocol (libsignal) end-to-end encryption.
* **GUI Toolkit:** [`github.com/lxn/walk`](https://github.com/lxn/walk)
  * Direct wrapper around native Windows Win32 API and Common Controls.
  * Custom owner-drawn widgets for speech bubbles and media cards using GDI+ / Direct2D.
* **Database Engine:** SQLite with `modernc.org/sqlite` (pure Go SQLite driver, no C compiler required on Windows).
  * Stores session credentials (`whatsmeow/store/sqlstore`).
  * Custom local tables for conversation threads, message history, and cached contact names.
* **Resource Linking:** Windows Application Manifest (`app.manifest`) compiled into `.syso` via `go-winres` for Windows Common Controls v6 styling and Per-Monitor V2 DPI awareness.

### 3.2 System Architecture Diagram

```
┌────────────────────────────────────────────────────────────────────────┐
│                        wa-go.exe (Single Process)                     │
│                                                                        │
│   ┌────────────────────────────────────────────────────────────────┐   │
│   │                      Walk Win32 GUI Thread                     │   │
│   │  - Main Window (Rail + Sidebar + Chat Pane)                    │   │
│   │  - Custom Drawn Bubbles (GDI+ / Direct2D)                      │   │
│   │  - System Tray (Shell_NotifyIcon) & Toast Dispatcher           │   │
│   └───────────────────────▲────────────────────────────────────────┘   │
│                           │ mw.Synchronize()                           │
│   ┌───────────────────────┴────────────────────────────────────────┐   │
│   │                 App Manager / Dispatcher (Go)                  │   │
│   │  - Multi-Account Coordinator                                   │   │
│   │  - Message Store (SQLite: 50-message paginated queries)        │   │
│   │  - Disk Media Streamer (%LOCALAPPDATA%\wa-go\cache)            │   │
│   │  - GIF & Emoji Providers                                       │   │
│   └───────────────────────▲────────────────────────────────────────┘   │
│                           │ Channels & Events                          │
│   ┌───────────────────────┴────────────────────────────────────────┐   │
│   │                 whatsmeow Client Workers (Goroutines)          │   │
│   │  - Account 1 Client Worker  ◄──┐                               │   │
│   │  - Account 2 Client Worker  ◄──┼── Independent WebSockets      │   │
│   │  - HistorySync Ingestion Pipeline (Chunked writes to SQLite)   │   │
│   └────────────────────────────────┼───────────────────────────────┘   │
└────────────────────────────────────┼───────────────────────────────────┘
                                     ▼
                        WhatsApp Multi-Device Servers
```

---

## 4. Detailed Component Design

### 4.1 UI Layout Specification (Three-Column Structure)
1. **Navigation Rail (56px width, Dark `#111b21`):**
   * Account profile avatars with green status dots and unread counter badges.
   * Add account (`+`) button to trigger QR code pairing dialog.
   * Divider line.
   * Vector navigation icons: Chats, Status/Stories, Settings (at bottom).
2. **Left Sidebar (280px width, Light `#ffffff`):**
   * **Stories Row:** Horizontal scrollable avatar strip displaying contacts with active status updates.
   * **Header Actions:** "New Chat" vector icon (contact picker / direct number input).
   * **Search Bar:** Clean input box with vector search icon for filtering conversations.
   * **Chat List:** Virtualized / efficient list of conversations showing contact avatar, display name, timestamp, last message snippet, and green unread badge.
3. **Right Chat View (Flexible width, WhatsApp Warm `#efeae2`):**
   * **Header (`#f0f2f5`):** Contact avatar, name, online/last seen status, search and options vector buttons.
   * **Message Canvas:** Custom painted message bubbles:
     * Outgoing: Soft green (`#d9fdd3`), right-aligned, timestamp + delivery tick vectors.
     * Incoming: Clean white (`#ffffff`), left-aligned, timestamp.
     * Media Bubbles: Scaled image thumbnail (cached locally), document icon cards with file size and download button.
   * **Bottom Input Bar (`#f0f2f5`):**
     * Vector icon triggers for Emoji Picker, GIF search dialog, and Attachment paperclip.
     * Single/Multi-line text input field.
     * Green Send button with vector paper-plane glyph.

### 4.2 SQLite Message Persistence & Pagination
* **Database Schema:**
  * `chats`: `jid` (PK), `account_id`, `name`, `last_message_ts`, `unread_count`.
  * `messages`: `id` (PK), `chat_jid`, `account_id`, `sender_jid`, `timestamp`, `message_type`, `body`, `media_path`, `status`.
* **Pagination Pattern:** Active chat queries run `SELECT * FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT 50 OFFSET ?`. Only the active page is instantiated in RAM.
* **HistorySync Ingestion:** `events.HistorySync` messages received upon pairing are processed in batches of 100 via background SQLite transactions to prevent I/O blocking and avoid memory spikes.

### 4.3 Media Pipeline & Memory Safeguards
* **No Full Media in RAM:** When an image, video, or document message is received, only metadata (mime type, thumbnail, file size) is kept in memory.
* **On-Demand Streaming:** Media files are only downloaded when the user views the message or clicks "Download".
* **Disk Caching:** Media is saved to `%LOCALAPPDATA%\wa-go\cache\<account_id>\media\` with hashed filenames.
* **External Launch:** Double-clicking video or document cards invokes `windows.ShellExecute` to delegate playback to default Windows players (e.g. Windows Media Player, Acrobat, VLC).

### 4.4 Desktop Notifications & Background Tray
* Implemented via Windows Toast API / `Shell_NotifyIcon`.
* Notifications include contact name, avatar if cached, message snippet, and click-to-activate behavior that brings the `wa-go` window to the foreground and opens the relevant chat.
* Minimizing to tray sets `client.SendPresence(types.PresenceUnavailable)` so the user does not remain marked as active 24/7.

---

## 5. Summary of Implementation Calls Made
* **Database Driver:** `modernc.org/sqlite` (pure Go, CGO-free, seamless Windows compilation).
* **Tray Behavior:** Minimize-to-tray on close with offline presence toggle and full Exit menu option.
* **New Chat Flow:** Dedicated vector button in sidebar header with contact search and unsaved phone number support.
* **Status Viewer:** Lightweight popup overlay with photo slide carousel.
* **Icons:** Strictly monochrome vector line glyphs (Segoe Fluent style).

---

## 6. Verification & Acceptance Criteria
1. **Compilation:** Compiles cleanly to `wa-go.exe` on Windows with standard Go toolchain.
2. **Resource Benchmark:**
   * Idle RAM under 50 MB with 1 active account.
   * Idle RAM under 65 MB with 2 active accounts.
   * 0.0% background CPU when idle.
3. **Multi-Account:** Able to pair at least 2 accounts via QR code and switch between them smoothly.
4. **Media Handling:** Successfully sends and receives images and PDF documents; images render inline without memory leaks.
5. **Message History:** Existing chats and past messages reload cleanly from SQLite on restart without noticeable startup lag.
6. **UI Quality:** Clean vector line icons rendered sharply without any colored emoji buttons.
