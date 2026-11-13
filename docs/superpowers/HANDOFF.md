# Wisp — Session Handoff

**Date:** 2026-11-13  
**Project path:** `D:\project\wa-go` (Go module `wa-go`, app binary `wisp.exe`)  
**Branch:** `main`, head `eb17719`, working tree clean, all tests passing  
**Next step:** Execute the implementation plan for Group Mentions, `@all`, and Quoted Replies (`docs/superpowers/plans/2026-11-13-wisp-mentions-replies.md`) via subagent-driven development or inline execution.

---

## 1. Where Things Stand

### A. Avatar & Profile Photo Display Fixes — Complete & Verified on `main`
The issue where contact, group, and account avatars were not displaying in Wisp was diagnosed, fully resolved, and verified:
1. **`2d9c14e`**: Fixed inverted `ok` check in `fetchAvatarResult()` (`internal/app/avatars.go`). Added database reset on startup (`ResetEmptyAvatars()`) to recover 412 poisoned zero-ID avatar rows.
2. **`944ca70`**: Implemented missing `Invalidate()` methods on `Sidebar`, `ChatView`, and `InfoView` composites so newly fetched avatars trigger immediate screen repainting. Normalized left rail account JIDs with `@s.whatsapp.net`.
3. **`9a8d154` & `a1feae7`**: Handled `Shell_NotifyIcon` failure gracefully so tray errors do not crash or terminate the application; deferred tray creation until window is shown.
4. **`c7048c4`**:
   - Added automatic background avatar prefetching in `reloadList()` sorted newest-first so unscrolled chats download avatars automatically.
   - Upserted group chats from `chats` into `people` table during `reloadList()`.
   - Added thread-safe `SetAvatar()` to `Directory` with `sync.RWMutex` and comprehensive unit tests.
   - Updated `whatsapp.AvatarEvent` to invalidate directory cache and queue immediate fetch.
5. **`2616f3f`**: Fixed App Lock PIN lifecycle: upon entering PIN and unlocking, Wisp now triggers `reloadList()` and `refreshAccounts()`, immediately starting the background avatar queue.
- **Verification:** All tests green (`go test -count=1 ./...`), `go vet ./...` clean, binary compiled via `.\scripts\build.bat`. Session (`session.db`) untouched and preserved.

### B. Group Mentions, `@all`, and Quoted Replies — Designed & Planned
Brainstormed, mockups reviewed with user, design approved, and implementation plan committed:
- **Spec:** `docs/superpowers/specs/2026-11-13-wisp-mentions-replies-design.md` (`8d6ab6b`)
  - Quoted message replies (right-click -> Reply, `Ctrl+R`, double-click bubble, composer banner, in-bubble quote cards, click to jump to quote).
  - Group mentions with `@all` / `@everyone` (inline autocomplete dropdown, real-time filtering, wire translation to `@<digits>`, all-member notification tagging).
- **Plan:** `docs/superpowers/plans/2026-11-13-wisp-mentions-replies.md` (`eb17719`)
  - 5 bite-sized, test-driven tasks (Store schema v3 -> WhatsApp layer -> UI Chat Canvas -> UI Input Bar -> App Integration & Verification).

---

## 2. How to Resume

1. Open `D:\project\wa-go` on branch `main`.
2. Confirm with the user their preferred execution style:
   - **Subagent-Driven Development** (recommended): fresh subagent per task with independent two-stage code review.
   - **Inline Execution**: execute task-by-task directly in the session.
3. Begin execution of Task 1 in `docs/superpowers/plans/2026-11-13-wisp-mentions-replies.md`:
   - Start with failing unit tests for schema v3 migration and quoted message fields in `internal/store/`.

---

## 3. Important Context & Decisions Made

- **Mention Wire Format:** Outbound text converts UI tokens like `@Alice` to WhatsApp standard `@<digits>` on the wire, populating `ContextInfo.MentionedJID`. This preserves compatibility with official WhatsApp mobile/web apps and Wisp's existing `rewriteMentions`.
- **`@all` Expansion:** Text containing `@all` or `@everyone` in a group populates `MentionedJID` with all participant JIDs in that group.
- **In-Memory Group Caching:** Group participants are kept in memory in `whatsapp.Manager` / `app`, populated on group chat open or `GroupDetails`, preventing keystroke latency when typing `@`.
- **Schema v3:** Additive SQLite migration (`ALTER TABLE messages ADD COLUMN ...`) adding `quoted_id`, `quoted_sender`, `quoted_body`, `quoted_type`.
- **Jump to Quote:** If the quoted message is in the loaded page (`v.msgs`), it centers and briefly highlights the bubble. If older, it queries `store.DB.GetMessage` and loads the surrounding page.

---

## 4. Key Files & Commands

- **Build binary:** `.\scripts\build.bat`
- **Run all tests:** `go test -count=1 ./...`
- **Check vet:** `go vet ./...`
- **Design document:** `docs/superpowers/specs/2026-11-13-wisp-mentions-replies-design.md`
- **Implementation plan:** `docs/superpowers/plans/2026-11-13-wisp-mentions-replies.md`
- **Database:** `%LOCALAPPDATA%\Wisp\wisp.db`
- **Session:** `%LOCALAPPDATA%\Wisp\session.db` (do NOT delete/reset)
