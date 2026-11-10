# WhatsApp Native Desktop Client (`wa-go`) — Session Handoff

**Date:** 2026-11-10  
**Project Path:** `D:\project\wa-go`  
**Current Phase:** Implementation Ready (Planning Complete)  

---

## 1. Project Summary & Goals

Build an ultra-efficient, 100% native Windows desktop WhatsApp client (`wa-go.exe`) using Go, `lxn/walk` (Win32 API), and `whatsmeow`.
* **RAM Footprint:** Strictly **35–55 MB idle RAM** (under 65 MB with two active accounts connected).
* **CPU Footprint:** **0.0% CPU** at idle or sitting in the system tray.
* **Packaging:** Single standalone `.exe` without Chromium, Electron, Node.js, or WebView2.
* **Aesthetics:** Clean monochrome vector line icons (Segoe / Fluent style). **Strictly no colored emojis used as UI buttons.** Full Dark Mode & Light Mode support.

---

## 2. Key Architecture Decisions

1. **GUI Framework:** `github.com/lxn/walk` (Win32). Direct Windows API wrapper with custom-painted message bubbles via GDI+/Direct2D.
2. **Protocol Engine:** `go.mau.fi/whatsmeow` (Multi-Device protocol directly over WebSockets with Noise and Signal Protocol crypto).
3. **Multi-Account:** Managed via `internal/whatsapp/manager.go` running concurrent background client goroutines for multiple phone numbers with an account rail on the far left.
4. **Database & Pagination:** `modernc.org/sqlite` (pure Go, CGO-free). All message queries are strictly paginated to **50 items per viewport** to keep memory low.
5. **Media Pipeline:** Downloaded media streams directly to disk at `%LOCALAPPDATA%\wa-go\cache\` rather than buffering in RAM. Videos and documents are launched in default Windows applications via `windows.ShellExecute`.
6. **Background Lifecycle:** Window close (`X`) minimizes to the Windows System Tray and broadcasts `types.PresenceUnavailable` (offline) so the user does not stay online 24/7.
7. **Theming:** Full Dark Mode and Light Mode support with Windows `dwmapi.dll` (`DwmSetWindowAttribute`) immersive dark mode title bar integration.

---

## 3. Core Documentation References

* **Design Specification:** [`D:\project\wa-go\docs\superpowers\specs\2026-11-10-whatsapp-client-design.md`](file:///D:/project/wa-go/docs/superpowers/specs/2026-11-10-whatsapp-client-design.md)
* **Implementation Plan (11 Tasks):** [`D:\project\wa-go\docs\superpowers\plans\2026-11-10-whatsapp-client.md`](file:///D:/project/wa-go/docs/superpowers/plans/2026-11-10-whatsapp-client.md)

---

## 4. Instructions for the New Session

1. Open workspace at `D:\project\wa-go`.
2. Read the implementation plan: `docs/superpowers/plans/2026-11-10-whatsapp-client.md`.
3. Use the execution skill:
   - **Recommended:** `superpowers:subagent-driven-development` (or `superpowers:executing-plans`).
4. Begin with **Task 1: Project Scaffolding & Windows Resource Manifest** (`go.mod`, `winres.json`, and initial smoke tests).
