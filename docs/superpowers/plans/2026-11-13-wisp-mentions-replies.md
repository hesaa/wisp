# Wisp: Group Mentions, @all, and Quoted Message Replies — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement quoted message replies (context menu, composer reply banner, in-bubble quote preview cards, jump-to-quote on click) and group mentions with `@all` autocomplete and WhatsApp mention context tags.

**Architecture:**
- **Store:** Schema v3 adds `quoted_id`, `quoted_sender`, `quoted_body`, and `quoted_type` columns to the `messages` table.
- **WhatsApp:** `toStoreMessage` parses incoming `ContextInfo` (`QuotedMessage`, `StanzaId`, `Participant`). `SendText`/`SendMedia` constructs outgoing `ContextInfo` with quotes and translates UI `@Name` / `@all` mentions to wire tokens and `MentionedJID`.
- **UI:** `InputBar` gains a dismissible docked reply banner and an inline mention autocomplete popup for group chats. `ChatView` and `ChatLayout` render quoted preview blocks inside bubbles, support right-click / `Ctrl+R` to reply, and jump to the quoted message on click.
- **App:** Wires `ChatView.OnReply` to `InputBar.SetReply`, caches and feeds group participants to the composer, and forwards reply targets to the send pipeline.

**Tech Stack:** Go 1.26, lxn/walk + lxn/win, SQLite, whatsmeow (protobuf waE2E).

**Spec:** `docs/superpowers/specs/2026-11-13-wisp-mentions-replies-design.md` (binding).

---

## Global Constraints

- Schema version is 3; migrations must be additive and preserve all existing v1 and v2 records.
- Idle RAM stays within existing bounds (<50 MB with 1 account); 0% idle CPU; no polling loops.
- Autocomplete filtering operates in-memory; zero disk I/O or network requests per keystroke.
- No new third-party modules (standard library, walk/win, whatsmeow only).
- Dark and light theme support across all new UI components (reply banner, autocomplete popup, quote cards).
- Full keyboard operability: `Ctrl+R` on focused message to reply, `Esc` to cancel reply or dismiss popup, `↑`/`↓`/`Enter` in autocomplete.
- `go vet ./...` clean; `go test -count=1 ./...` green after every task.

---

## Review Focus

1. **Schema V2 to V3 Migration:** Existing databases migrate seamlessly without loss; `QuotedID`, `QuotedSender`, `QuotedBody`, `QuotedType` are empty strings for pre-existing rows (Task 1).
2. **Inbound & Outbound Quoted Message Protocol:** Inbound `ContextInfo.QuotedMessage` parses accurately; outbound `SendText` embeds valid `StanzaId`, `Participant`, and `QuotedMessage` protobuf matching WhatsApp protocol standards (Task 2).
3. **`@all` and `@Name` Mention Expansion:** In groups, `@all` correctly populates all participant JIDs into `ContextInfo.MentionedJID`; individual mentions map UI names to `@<digits>` wire format (Task 2).
4. **Quoted Bubble Layout & Hit-Testing:** Chat bubbles with quotes calculate height correctly; clicking the quoted preview box jumps smoothly to the original message in view (Task 3).
5. **Composer State & Lifecycle:** Switching chats, switching accounts, or app locking immediately clears pending reply targets and dismisses mention popups (Task 4 & 5).

---

### Task 1: Store — Schema V3 & Quoted Message Fields

**Files:**
- Modify: `internal/store/db.go`, `internal/store/messages.go`, `internal/store/db_test.go`, `internal/store/messages_test.go`

**Interfaces:**
- Schema version 3:
  - `ALTER TABLE messages ADD COLUMN quoted_id TEXT NOT NULL DEFAULT '';`
  - `ALTER TABLE messages ADD COLUMN quoted_sender TEXT NOT NULL DEFAULT '';`
  - `ALTER TABLE messages ADD COLUMN quoted_body TEXT NOT NULL DEFAULT '';`
  - `ALTER TABLE messages ADD COLUMN quoted_type TEXT NOT NULL DEFAULT '';`
- `store.Message` struct fields:
  - `QuotedID string`
  - `QuotedSender string`
  - `QuotedBody string`
  - `QuotedType string`
- `InsertMessage`, `InsertMessagesBatch`, `GetMessage`, `GetChatMessages` read/write quoted columns.

- [ ] **Step 1: Write the failing tests**
  - `TestMigrateV2ToV3KeepsRows(t *testing.T)`: Verify migration from v2 database to v3 preserves existing messages and sets default empty strings for quoted fields.
  - `TestQuotedMessageRoundTrip(t *testing.T)`: Insert message with `QuotedID="msg123"`, `QuotedSender="alice@s.whatsapp.net"`, `QuotedBody="hello"`, `QuotedType="text"`; retrieve via `GetMessage` and `GetChatMessages` and assert equality.
- [ ] **Step 2: Run tests to confirm failure**
  - Run: `go test -count=1 ./internal/store -run 'TestMigrateV2ToV3|TestQuotedMessage'`
  - Expected: Fail (fields/migrations undefined).
- [ ] **Step 3: Implement minimal code**
  - Update `internal/store/db.go` to schema version 3 with `ALTER TABLE` statements.
  - Update `internal/store/messages.go` struct definition, SQL queries, scans, and insert statements.
- [ ] **Step 4: Run tests to confirm pass**
  - Run: `go test -count=1 ./internal/store`
  - Expected: Pass.
- [ ] **Step 5: Commit**
  - Commit message: `feat(store): schema v3 quoted message fields and migration`

---

### Task 2: WhatsApp Layer — Inbound/Outbound Quotes & Mentions Translation

**Files:**
- Modify: `internal/whatsapp/client.go`, `internal/whatsapp/manager.go`, `internal/whatsapp/manager_test.go`

**Interfaces:**
- `toStoreMessage`:
  - When `ci := contextInfo(m)` has `ci.GetStanzaId() != ""`:
    - `msg.QuotedID = ci.GetStanzaId()`
    - `msg.QuotedSender = ci.GetParticipant()` (or in 1:1 DMs fallback to `info.Chat.String()`)
    - If `qm := ci.GetQuotedMessage(); qm != nil`:
      - `msg.QuotedType, msg.QuotedBody, _, _ = content(qm)`
- `SendReply`:
  - `SendText(ctx, account, chatJID, text string, reply *store.Message, mentions []string) (store.Message, error)`
  - Outbound `waE2E.ContextInfo`:
    - When `reply != nil`: sets `StanzaId`, `Participant`, and `QuotedMessage`.
    - When `text` contains `@all` or `@everyone` in a group: resolves participant JIDs and adds to `ContextInfo.MentionedJID`.
    - Translates UI `@Name` tokens to wire `@<digits>` format and adds corresponding JIDs to `ContextInfo.MentionedJID`.
- `Manager.GroupParticipants(accountID, groupJID string) ([]types.JID, error)`

- [ ] **Step 1: Write the failing tests**
  - `TestInboundQuotedMessageStored(t *testing.T)`: Simulate message with `ContextInfo{StanzaId: "xyz", QuotedMessage: ...}`; verify `toStoreMessage` returns correct `QuotedID`, `QuotedSender`, `QuotedBody`.
  - `TestOutboundReplyContextInfo(t *testing.T)`: Call `SendText` with a reply target; verify fake client receives message with `waE2E.ContextInfo` containing `StanzaId`, `Participant`, and `QuotedMessage`.
  - `TestOutboundMentionAllExpansion(t *testing.T)`: Call `SendText` in group with `@all`; verify `ContextInfo.MentionedJID` contains all group member JIDs.
- [ ] **Step 2: Run tests to confirm failure**
  - Run: `go test -count=1 ./internal/whatsapp -run 'TestInboundQuotedMessage|TestOutboundReply|TestOutboundMention'`
  - Expected: Fail.
- [ ] **Step 3: Implement minimal code**
  - In `client.go`, parse quoted fields in `toStoreMessage`.
  - In `manager.go`, implement quote metadata construction and mention resolution in `send()`.
- [ ] **Step 4: Run tests to confirm pass**
  - Run: `go test -count=1 ./internal/whatsapp`
  - Expected: Pass.
- [ ] **Step 5: Commit**
  - Commit message: `feat(whatsapp): parse inbound quotes, encode outbound replies and @all mentions`

---

### Task 3: UI Chat Canvas — Quoted Bubble Layout, Paint & Jump-to-Quote

**Files:**
- Modify: `internal/ui/chatlayout.go`, `internal/ui/chatview.go`, `internal/ui/chatview_test.go`

**Interfaces:**
- `bubble` gains:
  - `quotedBox walk.Rectangle` (empty if not a reply)
  - `quotedSender string`
  - `quotedSnippet string`
- `chatlayout.go`:
  - If `m.QuotedID != ""`:
    - Layout reserves quoted card at top of bubble (height ~36-40px, full inner bubble width).
    - Stores `quotedBox` relative to bubble bounds.
- `chatview.go`:
  - Paints quoted box: translucent accent background tint, 3px solid accent line on left, quoted author in accent color, snippet in muted color.
  - Right-click context menu: adds `"Reply\tCtrl+R"`.
  - Keyboard handling: `Ctrl+R` when a message is focused/picked calls `v.triggerReply(msg)`.
  - Double click on bubble calls `v.triggerReply(msg)`.
  - Click on `bubble.quotedBox` in `mouseDown`: calls `v.jumpToMessage(m.QuotedID)`.
  - `jumpToMessage(id string)`: centers the target message in view and flashes its background.

- [ ] **Step 1: Write the failing tests**
  - `TestBubbleLayoutWithQuote(t *testing.T)`: Message with `QuotedID` has larger height than same message without quote; `bubble.quotedBox` is non-empty.
  - `TestHitTestQuotedBox(t *testing.T)`: Coordinates inside `quotedBox` return true for quote hit-test.
  - `TestJumpToMessageLocatesIndex(t *testing.T)`: Finding an in-memory message index by ID returns correct index and scroll target.
- [ ] **Step 2: Run tests to confirm failure**
  - Run: `go test -count=1 ./internal/ui -run 'TestBubbleLayoutWithQuote|TestHitTestQuotedBox|TestJumpToMessage'`
  - Expected: Fail.
- [ ] **Step 3: Implement minimal code**
  - Update `chatlayout.go` for quote card measurement.
  - Update `chatview.go` for drawing quote card, context menu entry, shortcuts, and click jumping.
- [ ] **Step 4: Run tests to confirm pass**
  - Run: `go test -count=1 ./internal/ui`
  - Expected: Pass.
- [ ] **Step 5: Commit**
  - Commit message: `feat(ui): render quoted message cards in chat bubbles and handle jump-to-quote`

---

### Task 4: UI Input Bar — Reply Banner & Mention Autocomplete Popup

**Files:**
- Modify: `internal/ui/inputbar.go`, `internal/ui/inputbar_test.go` (or `chatview_test.go`)

**Interfaces:**
- `InputBar` reply banner:
  - `SetReply(m store.Message, author string)`: displays docked reply banner above text edit with accent left strip, author, snippet, and `×` button.
  - `ClearReply()`: hides banner, drops target message.
  - `ReplyTarget() (store.Message, bool)`: returns active reply target if set.
- `InputBar` mention autocomplete:
  - `SetGroupParticipants(members []GroupMember)`
  - When typing `@` in text input:
    - Displays popup listing `@all` followed by matching group participants.
    - Filters in real-time as user types.
    - Arrow keys (`↑`/`↓`) select items; `Enter`/`Tab` completes mention (e.g. `@Puteri `).
    - `Esc` cancels popup.

- [ ] **Step 1: Write the failing tests**
  - `TestInputBarReplyBannerState(t *testing.T)`: Calling `SetReply` records target and author; `ClearReply` resets target; `ReplyTarget` reports correct state.
  - `TestMentionAutocompleteFiltering(t *testing.T)`: Filter `@P` against members matches `Puteri` and `@all`; filter `@z` matches nothing except `@all`.
- [ ] **Step 2: Run tests to confirm failure**
  - Run: `go test -count=1 ./internal/ui -run 'TestInputBarReply|TestMentionAutocomplete'`
  - Expected: Fail.
- [ ] **Step 3: Implement minimal code**
  - Implement reply banner composite and controls in `internal/ui/inputbar.go`.
  - Implement mention autocomplete popup and keyboard interceptors in `internal/ui/inputbar.go`.
- [ ] **Step 4: Run tests to confirm pass**
  - Run: `go test -count=1 ./internal/ui`
  - Expected: Pass.
- [ ] **Step 5: Commit**
  - Commit message: `feat(ui): input bar reply banner and group mention autocomplete popup`

---

### Task 5: App Integration & Verification

**Files:**
- Modify: `internal/app/chat.go`, `internal/app/app.go`, `internal/app/events.go`, `internal/ui/people.go` (highlight `@all`)

**Interfaces:**
- Wire `ChatView.OnReply` to `InputBar.SetReply` with resolved sender name via `Directory.Name`.
- On group chat load: populate `InputBar.SetGroupParticipants` using cached members or `mgr.GroupDetails`.
- In `onSendText`: pass `InputBar.ReplyTarget()` to `mgr.SendText`; call `InputBar.ClearReply()` after send.
- In `switchAccount`, `openChat`, `lock`: call `InputBar.ClearReply()` and dismiss mention popups.
- Update `rewriteMentions` in `internal/ui/people.go` to colorize `@all` and `@everyone` in `Palette.Accent`.

- [ ] **Step 1: Write the failing tests**
  - `TestAppReplyClearedOnChatSwitch(t *testing.T)`: Switching chats clears pending reply target.
  - `TestAppMentionsRewriteIncludesAtAll(t *testing.T)`: Body with `@all` has span for `@all` highlighted.
- [ ] **Step 2: Run tests to confirm failure**
  - Run: `go test -count=1 ./internal/app -run 'TestAppReplyCleared'`
  - Expected: Fail.
- [ ] **Step 3: Implement minimal code**
  - Connect `ChatView` and `InputBar` in `internal/app/chat.go`.
  - Pass reply target into send pipeline.
  - Clear state on chat/account switches and locks.
  - Update `rewriteMentions` in `internal/ui/people.go`.
- [ ] **Step 4: Run full test suite and build**
  - Run: `go test -count=1 ./...`
  - Run: `.\scripts\build.bat`
  - Expected: All packages pass; binary builds cleanly.
- [ ] **Step 5: Commit**
  - Commit message: `feat(app): wire reply actions, mention participant sync, and @all styling`
