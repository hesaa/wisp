# Wisp: Group Mentions, @all, and Quoted Message Replies — Design

**Date:** 2026-11-13  
**Status:** In Review (Brainstormed with User)  
**Builds on:** `2026-11-10-whatsapp-client-design.md`, `2026-11-11-wisp-front-page-media-copy-design.md`, `2026-11-11-wisp-people-previews-design.md`  
**Binding constraints:** Idle RAM < 50 MB with one account, 0% idle CPU, no browser runtimes, no new third-party dependencies, monochrome UI icons, dark and light themes, keyboard operable everywhere, nothing shown or fetched behind lock screen.

---

## 1. Goal

Deliver the two missing collaborative interaction capabilities in Wisp:

1. **Quoted Message Replies:**
   - Ability to reply to any specific message from the message history (via context menu, `Ctrl+R`, or double-click).
   - Display a docked, dismissible reply banner above the message input bar showing the target sender and snippet.
   - Transmit WhatsApp-compliant quotation metadata (`waE2E.ContextInfo` containing `StanzaId`, `Participant`, and `QuotedMessage`).
   - Render quoted message preview cards inside message bubbles (colored vertical bar, author name, 1–2 line snippet).
   - Clicking a quoted preview inside a bubble smoothly scrolls to and briefly highlights the original message.
   - Parse and display incoming quoted messages received from other WhatsApp clients.

2. **Group Mentions & `@all`:**
   - In group chats, typing `@` into the input bar triggers an autocomplete popup directly above the composer.
   - The top item in the autocomplete popup is `@all` ("Notify all participants"), followed by group members matching the query.
   - Real-time search by contact name, push name, or phone digits.
   - Keyboard navigation: `↑` / `↓` moves selection, `Enter` or `Tab` inserts the mention token, `Esc` cancels.
   - When sent, messages with `@all` attach all group participant JIDs to `ContextInfo.MentionedJID` so every member receives a mention notification.
   - Messages with individual `@Name` tokens attach the corresponding member JIDs to `ContextInfo.MentionedJID`.
   - Mentions in chat bubbles continue to render in theme accent color (`Palette.Accent`), extended to support `@all` / `@everyone`.

---

## 2. Non-goals & Anti-goals

### Non-goals
- **Rich Text WYSIWYG Editing:** Composer remains lightweight plain text; mentions are plain text tokens (e.g. `@Alice`) mapped to JIDs upon send.
- **Mentions in 1:1 Chats:** Autocomplete popup only triggers in group chats (`@g.us`), where tagging is meaningful.
- **Post-Send Quote Editing:** Once a message is sent, its quote cannot be altered.
- **Message Reactions:** Reactions (emoji tapbacks) are a separate future feature.

### Anti-goals
- **Typing Lag:** Autocomplete filtering must operate purely in-memory against cached group participants without database queries or network requests during keystrokes.
- **Protocol Desync:** Quoted messages must strictly follow WhatsApp protobuf E2E standards (`waE2E.ContextInfo`) so official WhatsApp mobile and desktop apps render replies accurately.
- **Bloat:** The SQLite schema update must be an instant `ALTER TABLE ADD COLUMN` with minimal storage footprint.

---

## 3. Data Storage (Store Layer)

### Schema V3 Migration
Schema version advances from `2` to `3`. SQLite `ALTER TABLE ADD COLUMN` with empty string defaults preserves existing rows:

```sql
ALTER TABLE messages ADD COLUMN quoted_id TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN quoted_sender TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN quoted_body TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN quoted_type TEXT NOT NULL DEFAULT '';
```

### `store.Message` Struct Extension
`internal/store/messages.go` gains four fields:

```go
type Message struct {
    ...
    // Quoted message metadata (empty if not a reply)
    QuotedID     string // WhatsApp stanza ID of the message being quoted
    QuotedSender string // JID of the sender of the quoted message
    QuotedBody   string // Snippet or text of the quoted message
    QuotedType   string // Type of the quoted message (text, image, video, audio, document)
}
```

Queries `GetMessage`, `GetChatMessages`, `InsertMessage`, and `InsertMessagesBatch` are updated to read and write these columns.

---

## 4. WhatsApp Protocol Layer (`internal/whatsapp`)

### 1. Inbound Quoted Messages
In `toStoreMessage(id string, info types.MessageInfo, m *waE2E.Message)`:
When `ci := contextInfo(m)` is present:
- `msg.QuotedID = ci.GetStanzaId()`
- If `msg.QuotedID != ""`:
  - `msg.QuotedSender = ci.GetParticipant()`
  - In 1:1 direct chats where WhatsApp protocol omits `Participant`: if `ci.GetParticipant()` is empty, `QuotedSender` defaults to `info.Chat.String()`. When persisting or rendering, if the quoted message exists in the local store, its exact sender (`FromMe` vs partner) is verified from the stored message record.
  - If `qm := ci.GetQuotedMessage(); qm != nil`:
    - `qType, qBody, _, _ := content(qm)`
    - `msg.QuotedType = qType`
    - `msg.QuotedBody = qBody`

### 2. Outbound Quoted Replies
`SendText` and `SendMedia` take an optional `replyTo *store.Message`:
```go
type SendOpts struct {
    ReplyTo  *store.Message
    Mentions []string // explicit JIDs or populated via @all
}
```
When `ReplyTo != nil`:
- Construct `waE2E.ContextInfo`:
  - `StanzaId = proto.String(opts.ReplyTo.ID)`
  - `Participant = proto.String(opts.ReplyTo.SenderJID)`
  - `QuotedMessage = buildQuotedMessage(opts.ReplyTo)`:
    - Text: `&waE2E.Message{Conversation: proto.String(opts.ReplyTo.Body)}`
    - Media: sets corresponding media stub with thumbnail/caption if available.

### 3. Outbound Mentions & Wire Format
- **UI to Wire Translation:** While the composer displays user-friendly `@Name` tokens selected from the autocomplete popup, the outbound message text transmitted over WhatsApp protocol wire converts them to WhatsApp's standard `@<phone_or_lid_digits>` syntax. This ensures:
  1. Official WhatsApp mobile and desktop apps render mentions as interactive, formatted contact chips.
  2. Wisp's existing `rewriteMentions` logic seamlessly highlights and resolves them back to `@Name` in chat bubbles.
- **`@all` Handling:**
  - If text contains `@all` or `@everyone` (case-insensitive word boundary) in a group (`@g.us`):
    - Wisp retrieves group members from the participant cache.
    - All participant JIDs are added to `ContextInfo.MentionedJID`.
    - The literal text `@all` is preserved on the wire; `rewriteMentions` highlights `@all` in `Palette.Accent`.
  - For individual mentions, matched member JIDs are added to `ContextInfo.MentionedJID`.
  - Deduplicate all JIDs in `ci.MentionedJID`.

---

## 5. UI Layer (`internal/ui`)

### 1. Composer Quoted Reply Banner (`InputBar`)
- A docked banner sitting immediately above the text input field in `InputBar`:
  - Left border: 3px solid `Palette.Accent`.
  - Header: `"Replying to <Name>"` in bold `Palette.Accent`.
  - Subtitle: 1-line truncated message preview in muted text color.
  - Right: `×` close button (clicking it or pressing `Esc` when text is empty cancels reply).
- Methods on `InputBar`:
  - `SetReply(m store.Message, authorName string)`
  - `ClearReply()`
  - `ReplyTarget() (store.Message, bool)`

### 2. Mention Autocomplete Dropdown (`InputBar` / Group Chat)
- Active only when current chat is a group (`strings.HasSuffix(chatJID, "@g.us")`).
- **Participant Caching:** Group participants are maintained in-memory in `whatsapp.Manager` / `app` (populated on group open via `GetGroupInfo` or `store.People` upsert, and updated on group participant events). Keystroke filtering never performs disk I/O.
- Triggered when user enters `@` preceded by whitespace or at the start of input.
- Displays a floating/docked popup directly above the composer:
  - Top Item: `@all` with an announcement badge ("Notify all participants").
  - Member Items: Sorted list of group participants matching the filter text after `@`.
    - Shows member avatar / initials, full name, push name, and phone digits.
- Keyboard Handling in Input:
  - `Down` / `Up`: moves highlighted index in popup.
  - `Enter` / `Tab`: selects highlighted item, replaces `@filter` with `@Name `, hides popup.
  - `Esc`: dismisses popup without inserting.

### 3. Chat Canvas & Bubbles (`ChatView` & `ChatLayout`)
- **Right-Click Context Menu:**
  - Added item: `"Reply\tCtrl+R"`.
  - Keyboard shortcut: `Ctrl+R` on focused message starts reply.
  - Double-click on a message bubble starts reply.
- **Bubble Layout (`chatlayout.go`):**
  - If `m.QuotedID != ""`:
    - Reserve a quoted card block at top of bubble:
      - Height: ~36px (author line + 1 line snippet + padding).
      - Left accent bar: width 3px.
      - Box dimensions stored in `bubble.quotedBox` for hit-testing.
- **Bubble Painting (`chatview.go`):**
  - Draw background tint for quoted box (`Palette.Selection` or 10% accent tint).
  - Draw accent colored left strip.
  - Draw quoted author name in `Palette.Accent`.
  - Draw quoted snippet in muted text.
- **Jump to Quoted Message on Click:**
  - In `mouseDown`: hit-test `bubble.quotedBox`.
  - If clicked: search loaded `v.msgs` for `m.QuotedID`.
  - If found in memory: smoothly scroll view to center the original message, and flash its background for 400ms.
  - If not in loaded pages: query `store.DB.GetMessage(acct, chatJID, m.QuotedID)` and asynchronously load the page window around that timestamp; if the message is missing from database (e.g. wiped or not synced), display a brief status hint.

---

## 6. App Architecture & Event Flow (`internal/app`)

1. **Reply Action:**
   `chatView.OnReply(func(m store.Message))` -> fetches display name via `dir.Name(m.SenderJID, "")` -> calls `inputBar.SetReply(m, name)` -> focuses input editor.
2. **Clearing Reply:**
   - Cleared automatically when message is successfully sent.
   - Cleared on chat switch, account switch, or app lock.
3. **Sending:**
   `chat.go`: `onSendText` checks `inputBar.ReplyTarget()`. Passes target message and text to `mgr.SendText(ctx, account, chatJID, text, reply)`.
4. **Group Participants for Mentions:**
   `Directory` or `mgr.GroupParticipants(account, groupJID)` provides the participant list for the autocomplete popup.

---

## 7. Testing Strategy

1. **Store Tests (`internal/store`):**
   - Schema migration from v2 to v3 preserves existing messages.
   - Round-trip test for `QuotedID`, `QuotedSender`, `QuotedBody`, `QuotedType`.
2. **WhatsApp Tests (`internal/whatsapp`):**
   - Inbound `ContextInfo` with `QuotedMessage` parses to `store.Message` with quoted fields.
   - Outbound `SendText` with `ReplyTo` properly encodes `waE2E.ContextInfo` with `StanzaId`, `Participant`, and `QuotedMessage`.
   - Outbound `@all` in group populates `MentionedJID` with all group member JIDs.
3. **UI / Logic Tests (`internal/ui`):**
   - Autocomplete filter matching by name, push name, and phone digits.
   - Autocomplete keyboard navigation (`Up`, `Down`, `Enter`, `Esc`).
   - Mention token replacement in text.
   - Bubble layout sizing with and without quoted preview block.
   - Click hit-test on quoted preview box.
