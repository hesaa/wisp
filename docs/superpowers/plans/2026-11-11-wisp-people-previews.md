# Wisp people, pictures, details view and previews — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show people by name and picture everywhere, resolve mentions, add a contact/group details view, video/GIF previews, a chat list preview line for non-text messages, and a notification sound choice.

**Architecture:** A `people` table per account (schema v2) is filled from messages, the address book and group lists; a pure `ui.Directory` index applies one name rule everywhere. Pictures are fetched on demand through one queue per account, saved small on disk, and drawn by the existing `drawAvatar` through a shared bitmap LRU. The details view is a child composite in the chat column, switched like the media view.

**Tech Stack:** Go 1.26, lxn/walk + lxn/win, GDI/GDI+, whatsmeow, SQLite, `winmm.dll!PlaySoundW`.

**Spec:** `docs/superpowers/specs/2026-11-11-wisp-people-previews-design.md` (binding), with the base specs `2026-11-10-whatsapp-client-design.md` and `2026-11-11-wisp-front-page-media-copy-design.md`.

**Branch:** create `feat/people-previews` from `feat/front-page-media-copy` before Task 1.

## Global Constraints

- Idle memory with one account must not grow beyond the picture LRU (64 bitmaps of 96 px, about 2.4 MB at most); idle CPU stays 0% — no timers, no polling goroutines; nothing is fetched while the window is hidden, minimised or locked.
- No browser runtimes, no new third-party modules (standard library, walk/win, x/image, whatsmeow only).
- UI icons stay monochrome line icons (`internal/ui/icons`); dark and light themes for everything new; every new control reachable and operable by keyboard.
- Nothing is shown, opened, fetched or sent behind the lock page; views in the chat column are child composites, never dialogs; focus changes go through `focusOn`.
- Logs name the operation and the error only — never a name, phone number, about text, message text, PIN or key.
- Tests never touch the real `%LOCALAPPDATA%`; use `t.TempDir()`. No test or live check writes the system clipboard, opens a file dialog, launches another program, sends input to a window it did not create, or links a real account.
- Copy is sentence case, no "please", no "successfully". Exact strings from the spec are used verbatim: `Unknown`, `Group`, `Contact info`, `Group info`, `About`, `Description`, `N members`, `Admin`, `Loading…`, `Couldn't load the details.`, `Retry`, `GIF`, `Photo`, `Video`, `Sticker`, `Voice message`, `Audio`, `Document: <name>`, `This message was deleted`, `Unsupported message`, `You: `, `Sound`, `Default`, `Message`, `Mail`, `Reminder`, `SMS`.
- An internal ID (`@lid` user part) is never shown to the user.
- `gofmt` clean (strip CR first: the checkout uses `core.autocrlf=true`); `go vet ./...` clean; `go test ./...` green after every task.

## Review Focus

1. A database written by the previous version (schema v1, with chats and messages) is opened → it migrates without losing a row and old chats still list (test in Task 1).
2. A person known only by an internal ID later gets a phone number and an address-book name → every place shows the address-book name, with no duplicate person (test in Task 2).
3. A message whose text contains `@` plus digits that is not a mention (a pasted number, an email-like string) → the text is left exactly as written (test in Task 2).
4. WhatsApp refuses or fails a picture request (privacy, offline, rate limit) → initials stay, nothing retries in a loop, the UI never blocks (test in Task 6).
5. Wisp locks, or the user switches chat, while the details view is loading → the view closes and the late reply is dropped (test in Task 7).

---

### Task 1: Store — people table, mentions column, last-message fields

**Files:**
- Create: `internal/store/people.go`, `internal/store/people_test.go`
- Modify: `internal/store/db.go` (schema v2), `internal/store/messages.go` (`Message.Mentions`, `Chat` last-message fields, `ListChats`), their tests

**Interfaces:**
- Produces:
  - Schema v2 exactly as in the spec §1 (`people` table; `messages.mentions TEXT NOT NULL DEFAULT ''`); `schemaVersion = 2`; the `v < 2` step only adds.
  - `type Person struct{ JID, PNJID, FullName, PushName, AvatarID string; AvatarTS int64 }`
  - `func (d *DB) UpsertPeople(accountID string, people []Person) error` — one transaction; inserts missing rows; a non-empty `PNJID`, `FullName` or `PushName` overwrites, an empty one leaves the stored value; never touches the avatar columns.
  - `func (d *DB) People(accountID string) ([]Person, error)`
  - `func (d *DB) SetAvatar(accountID, jid, avatarID string, ts int64) error` — upserts the row; sets both avatar columns.
  - `func (d *DB) ResetAvatars() error` — `avatar_ts = 0` for every row of every account.
  - `store.Message.Mentions string` (space-separated JIDs), written by `InsertMessage`/`InsertMessagesBatch`, read by every message query.
  - `store.Chat` gains `LastType, LastSender string; LastFromMe bool; LastMeta []byte` — the newest message's `message_type`, `sender_jid`, `from_me`, `media_meta`; `Snippet` stays its body.

- [ ] **Step 1: Write the failing tests**
```go
func TestMigrateV1ToV2KeepsRows(t *testing.T)   // Review Focus 1: build a v1 file with the old DDL (copy it into the test), 2 chats + 3 messages, user_version 1; Open → ListChats has 2, GetChatMessages has 3 with Mentions "", user_version 2, people table exists
func TestUpsertPeopleMergesFields(t *testing.T) // {jid A, PushName "Sinta"} then {jid A, PNJID "62…@s.whatsapp.net"} → one row with both; then {jid A, PushName ""} leaves "Sinta"
func TestSetAvatarAndReset(t *testing.T)        // SetAvatar on an unknown jid creates the row; UpsertPeople afterwards keeps avatar_id; ResetAvatars zeroes avatar_ts only
func TestMentionsRoundTrip(t *testing.T)        // insert with Mentions "a@lid b@s.whatsapp.net" → read back equal
func TestListChatsLastMessageFields(t *testing.T) // newest message is an own image without body → Snippet "", LastType "image", LastFromMe true, LastMeta equal
```
- [ ] **Step 2:** `go test ./internal/store -run 'TestMigrateV1|TestUpsertPeople|TestSetAvatar|TestMentions|TestListChatsLast'` — expected: FAIL (undefined).
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test ./internal/store` — expected: `ok`.
- [ ] **Step 5: Commit** `git commit -m "feat(store): people table, mentions and last-message fields (schema v2)"`

### Task 2: Pure rules — directory, names, mentions, preview line, picture refresh

**Files:**
- Create: `internal/ui/people.go`, `internal/ui/people_test.go`

**Interfaces:**
- Consumes: `store.Person`, `store.Chat` last-message fields (Task 1); `parseMedia`, `mediaLabel` (existing, `internal/ui`).
- Produces (all pure):
  - `type Directory struct` — `func NewDirectory(people []store.Person) *Directory`; a nil `*Directory` is valid and knows nobody.
  - `func (d *Directory) Person(jid string) (store.Person, bool)` — the row for `jid` merged with the row linked through `PNJID` in either direction (a LID row and its phone-number row are one person; a non-empty field of either fills the result; `FullName` of either wins).
  - `func (d *Directory) Name(jid, stored string) string` — the spec's name rule. `stored` is `chats.name`. Group (`@g.us`): `stored`, else `Group`. Otherwise: `FullName`; else `~`+`PushName`; else `stored` when it is not empty and not masked; else `+<digits>` of the phone-number JID (`jid` itself when it is `@s.whatsapp.net`, else `PNJID`); else `Unknown`.
  - `func (d *Directory) Mention(jid string) string` — as `Name(jid, "")` without the leading `~`.
  - `func maskedName(s string) bool` — true for a `+`, then only digits, spaces and at least one of `•`, `·`, `*`, `.`, `∙`.
  - `func rewriteMentions(body, mentions string, d *Directory) (text string, spans [][2]int)` — for each JID in `mentions`, every `@<user part>` in `body` becomes `@<Mention>`; with `mentions == ""`, an `@` followed by 5–20 digits (not preceded by a letter or digit, not followed by one) is rewritten only when `d.Person` knows `<digits>@lid` or `<digits>@s.whatsapp.net`. `spans` are the UTF-16 start/end offsets of each rewritten mention in `text`, in order.
  - `func snippetLine(c store.Chat, d *Directory) string` — spec §6: text or caption, else the label (`Photo`, `Video`, `GIF`, `Sticker`, `Voice message`, `Audio`, `Document: <name>`, `This message was deleted`, `Unsupported message`); prefix `You: ` when `LastFromMe`; in a group, `<Name(LastSender, "")>: ` otherwise; a chat with no message gives `""`.
  - `const avatarRefresh = 7 * 24 * time.Hour`, `const avatarRetry = 10 * time.Minute`
  - `func avatarDue(p store.Person, now time.Time) bool` — `AvatarTS == 0`, or `now − AvatarTS ≥ avatarRefresh`.
  - `func avatarFailedTS(now time.Time) int64` — the `AvatarTS` to store after a failed fetch so it is due again after `avatarRetry`.

- [ ] **Step 1: Write the failing tests**
```go
func TestNameRule(t *testing.T)            // FullName wins; "~Sinta" for push name only; stored "Budi" used; "+62812…" for a bare PN jid; LID with PNJID → "+<pn digits>"; LID alone → "Unknown"; group → stored, else "Group"
func TestNameMergesLIDAndPhone(t *testing.T) // Review Focus 2: rows {A@lid, PNJID P} and {P, FullName "Sinta"} → Name(A@lid) == Name(P) == "Sinta"; Person(A@lid) has FullName "Sinta"
func TestMaskedNameSkipped(t *testing.T)   // maskedName("+62••••••32") true, ("+62 812 3456") false, ("Sinta •") false; Name(lid, "+62••••••32") == "Unknown"
func TestRewriteMentions(t *testing.T)     // stored list: "@35893729566783 jumat" + that LID known as push "Sinta" → "@Sinta jumat", spans [[0,6]]; two mentions; emoji before the mention keeps UTF-16 offsets right
func TestRewriteLeavesNonMentions(t *testing.T) // Review Focus 3: no list: "call @6281234567890 now" unknown → unchanged; "a@6281234567890" unchanged; "@1234" (4 digits) unchanged; known digits → rewritten
func TestSnippetLine(t *testing.T)         // each message kind → its label; caption wins; "You: Photo"; group other → "Sinta: hi"; deleted; empty chat → ""
func TestAvatarDue(t *testing.T)           // never checked → true; checked 6 days ago → false; 7 days → true; after avatarFailedTS(now): false at +9 min, true at +10 min
```
- [ ] **Step 2:** `go test ./internal/ui -run 'TestNameRule|TestNameMerges|TestMaskedName|TestRewrite|TestSnippetLine|TestAvatarDue'` — expected: FAIL.
- [ ] **Step 3:** Implement `people.go`.
- [ ] **Step 4:** `go test ./internal/ui` — expected: `ok`.
- [ ] **Step 5: Commit** `git commit -m "feat(ui): name rule, mention rewriting, preview line and picture refresh rule"`

### Task 3: WhatsApp layer — collect people and mentions, new queries, GIF preview on send

**Files:**
- Modify: `internal/whatsapp/client.go`, `internal/whatsapp/manager.go`, their tests (the existing fake `waClient` gains the new methods)

**Interfaces:**
- Consumes: `store.UpsertPeople`, `store.Person`, `Message.Mentions`, `store.SetAvatar` (Task 1).
- Produces:
  - `toStoreMessage` fills `Mentions` from the context info's `MentionedJID` of text (`ExtendedTextMessage`) and of captions.
  - People are upserted with the batch that carries them: for every incoming message its sender (as addressed, LID or phone) with `PushName` (direct chats and groups) and, when `SenderAlt`/`PNForLID` gives it, `PNJID`; from `AllContacts` the `FullName` (once per connect); from history sync the conversations' phone JIDs. After a batch that changed people the manager emits `PeopleEvent{AccountID string}`.
  - `nameDM` and history sync no longer store a masked name (`+`, digits and bullet/dot characters) in `chats.name`; `func maskedName(s string) bool` in `internal/whatsapp` with the same rule as Task 2's.
  - `type Picture struct{ ID, URL string }`; `func (m *Manager) ProfilePicture(ctx context.Context, accountID, jid, knownID string, full bool) (Picture, bool, error)` — wraps `GetProfilePictureInfo` (`Preview: !full`, `ExistingID: knownID`); `ok == false` with a nil error means unchanged (`knownID` still current); `ErrNoPicture` when there is none or it is hidden.
  - `var ErrNoPicture = errors.New("whatsapp: no profile picture")`
  - `func (m *Manager) FetchPicture(ctx context.Context, url string) ([]byte, error)` — HTTP GET through the configured proxy, at most 2 MB, 20 s timeout.
  - `func (m *Manager) About(ctx context.Context, accountID, jid string) (string, error)` — `GetUserInfo` status text.
  - `type Member struct{ JID string; Admin bool }`, `type GroupDetails struct{ Subject, Description string; Members []Member }`; `func (m *Manager) GroupDetails(ctx context.Context, accountID, jid string) (GroupDetails, error)` — `GetGroupInfo`; also upserts the members (with phone JIDs when the group info carries them).
  - `events.Picture` → `store.SetAvatar(account, jid, "", 0)` then `AvatarEvent{AccountID, JID string}`.
  - `SendGIF(ctx, accountID, chatJID, mp4Path string, preview []byte, w, h int)` — sets `JPEGThumbnail`, `Width`, `Height` when `preview != nil`; the one caller (GIF picker send path in `internal/app`) passes the picked GIF's still image re-encoded as JPEG (≤ 320 px wide) and its size, or nil.

- [ ] **Step 1: Write the failing tests** (with the package's existing fake client)
```go
func TestMentionsStoredFromText(t *testing.T)     // ExtendedTextMessage with MentionedJID [a@lid, b@s.whatsapp.net] → Message.Mentions "a@lid b@s.whatsapp.net"; an image caption with ContextInfo likewise
func TestGroupSenderPushNameStored(t *testing.T)  // group message from a LID with PushName "Sinta" and SenderAlt P → People has {jid LID, PNJID P, PushName "Sinta"}; PeopleEvent emitted once
func TestMaskedNameNotStored(t *testing.T)        // DM whose push name is "+62••••••32" → chats.name stays ""
func TestProfilePictureUnchangedAndMissing(t *testing.T) // fake returns nil info for the known ID → ok false; fake returns whatsmeow's not-set / unauthorized errors → ErrNoPicture
func TestGroupDetailsUpsertsMembers(t *testing.T) // fake group with 2 members, one admin → Members in order, People has both
func TestSendGIFCarriesPreview(t *testing.T)      // the sent VideoMessage has GifPlayback, JPEGThumbnail, Width, Height
```
- [ ] **Step 2:** `go test ./internal/whatsapp -run 'TestMentionsStored|TestGroupSender|TestMaskedName|TestProfilePicture|TestGroupDetails|TestSendGIF'` — expected: FAIL.
- [ ] **Step 3:** Implement; `go build ./...` must pass (update the `SendGIF` caller).
- [ ] **Step 4:** `go test ./internal/whatsapp ./internal/app` — expected: `ok`.
- [ ] **Step 5: Commit** `git commit -m "feat(whatsapp): collect people and mentions, picture and details queries, GIF preview on send"`

### Task 4: Names everywhere, mentions drawn, preview line

**Files:**
- Modify: `internal/app/events.go`, `internal/app/logic.go`, `internal/app/chat.go` (people in memory, `PeopleEvent`), `internal/ui/sidebar.go` (`displayName`, preview line, search), `internal/ui/chatlayout.go` + `internal/ui/chatview.go` (sender line, mentions, header), `internal/ui/copytext.go`, `internal/ui/newchat.go`, `internal/notify` callers, tests

**Interfaces:**
- Consumes: `ui.Directory`, `rewriteMentions`, `snippetLine` (Task 2); `store.People`, `PeopleEvent` (Tasks 1, 3).
- Produces:
  - The app loads `store.People` for each account into a `*ui.Directory` when accounts load and on every `PeopleEvent`, and hands it to the views: `func (m *SidebarModel) SetDirectory(d *Directory)`, `func (v *ChatView) SetDirectory(d *Directory)` (both repaint; the chat view re-lays out its bubbles).
  - `displayName(c store.Chat)` becomes `d.Name(c.JID, c.Name)` at every caller; `labelOf`/`chatName`/`senderName` in `internal/app` use the directory, so notifications and whole-message copy follow the rule.
  - The chat list row draws `snippetLine`; chat list search matches the name by the rule and the preview line.
  - `bubbleText` returns the rewritten text; `bubble` gains `mentions [][2]int` (UTF-16 spans in its text) drawn in `Palette.Accent`; wrapping, selection and copy work on the rewritten text.
  - The group sender line and the chat header title use `d.Name`; the header subtitle shows `+<digits>` only when the title is not already that number.

- [ ] **Step 1: Write the failing tests**
```go
func TestSidebarUsesNameRuleAndPreviewLine(t *testing.T) // model with a LID chat, stored "" and a directory knowing push "Sinta": row title "~Sinta", line "Photo" for a last image; search "sinta" and search "photo" both find it
func TestBubbleTextRewritesMentions(t *testing.T)        // a message with Mentions and a directory → bubble text "@Sinta jumat", mentions [[0,6]]; selecting the whole text and copying gives "@Sinta jumat"
func TestNoInternalIDShown(t *testing.T)                 // for chats/senders that are @lid with nothing known, title, sender line and notification title are "Unknown", never the digits
```
- [ ] **Step 2:** run them — expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test ./internal/ui ./internal/app ./internal/notify` — expected: `ok`.
- [ ] **Step 5: Commit** `git commit -m "feat: names by one rule everywhere, mentions resolved, chat list preview line"`

### Task 5: Video and GIF preview bubbles

**Files:**
- Modify: `internal/ui/chatlayout.go` (`kindOf`, layout), `internal/ui/chatview.go` (paint), `internal/ui/icons/icons.go` (play mark if none fits), tests

**Interfaces:**
- Consumes: `mediaInfo.preview`, `mediaInfo.gif` (existing).
- Produces: `kindOf` returns `kindImage` for a video whose `info.preview` is not empty; `bubble` gains `play bool` (video or GIF preview) and `gifBadge bool`; the paint draws the preview through the existing `thumbFor` path, a play mark (line icon on a 40 px dark disc, 60% black) centred, and for a GIF a `GIF` badge (9 pt, white on 60% black, 4 px radius) 8 px from the top-left corner. Without a preview the card stays, its title `GIF` for a GIF. `clickOpens` still opens on the thumbnail. Nothing is downloaded for the preview.

- [ ] **Step 1: Write the failing tests**
```go
func TestVideoWithPreviewIsImageBubble(t *testing.T) // video + preview → kindImage, play true, gifBadge false; gif → gifBadge true; video without preview → kindCard; GIF card title "GIF"
func TestClickOpensVideoPreview(t *testing.T)        // a plain single click on the preview of a video bubble → clickOpens true
```
- [ ] **Step 2:** run — expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test ./internal/ui` — expected: `ok`.
- [ ] **Step 5: Commit** `git commit -m "feat(ui): video and GIF messages show their preview picture"`

### Task 6: Pictures — cache folder, fetch queue, shared LRU, drawn everywhere

**Files:**
- Create: `internal/ui/avatars.go`, `internal/ui/avatars_test.go`, `internal/app/avatars.go`, `internal/app/avatars_test.go`
- Modify: `internal/media/cache.go` (+ test), `internal/ui/rail.go` (`drawAvatar`), `internal/ui/chatlayout.go` + `chatview.go` (group sender pictures), `internal/app/app.go`, `events.go`, `chat.go` (`cacheCleared`)

**Interfaces:**
- Consumes: `Manager.ProfilePicture`, `FetchPicture`, `ErrNoPicture`, `AvatarEvent` (Task 3); `store.SetAvatar`, `ResetAvatars` (Task 1); `avatarDue`, `avatarFailedTS`, `Directory.Person` (Task 2).
- Produces:
  - `func (c *Cache) AvatarPath(accountID, jid string) string` — `<root>\<account>\avatars\<sha256 hex of jid>.jpg`; `func (c *Cache) SaveAvatar(accountID, jid string, data []byte) (string, error)` — decodes (JPEG/PNG/WebP, refuses more than 4 MP), scales to 96 px, writes JPEG atomically; `ClearMediaCache` also removes every `avatars\` folder.
  - `internal/ui`: `type Avatars struct` — `func NewAvatars(c *media.Cache) *Avatars`; `SetAccount(id string)`; `OnWant(fn func(jid string))` — called at most once per jid until `Invalidate(jid)`; `Invalidate(jid string)`; `Dispose()`. An LRU of 64 decoded bitmaps keyed by account+jid, disposed on eviction. `drawAvatar` gains the `*Avatars` (package-level `SetAvatars(a *Avatars)` like `SetGIFProxy`): it draws the cached picture clipped to the circle when the file exists, else initials, and calls `want(jid)`.
  - Group chats: `bubble` gains `avatar bool` for the first bubble of each run of incoming messages from one sender; incoming bubbles in a group are inset 36 px; the 28 px picture sits left of that bubble, top-aligned.
  - `internal/app`: `type avatarQueue` (window-free, tested with fakes) — `want(account, jid string)`, one fetch in flight per account, newest request first, a request is dropped when `wanted(jid)` no longer holds; `func avatarStep(p store.Person, now time.Time, res pictureResult) (id string, ts int64, save bool)` (pure): unchanged → keep id, `ts = now`; new picture → new id, `ts = now`, save; `ErrNoPicture` → `"-"`, `now`; other error → keep id, `avatarFailedTS(now)`.
  - The queue runs only while `mediaStays`-style conditions hold: window visible, not minimised, not locked (`func fetchAllowed(visible, iconic, locked bool) bool`). On success or `AvatarEvent` the app calls `Avatars.Invalidate(jid)` and repaints the views. `cacheCleared` calls `ResetAvatars` and drops the LRU.

- [ ] **Step 1: Write the failing tests**
```go
func TestSaveAvatarScalesAndClears(t *testing.T)    // 640×640 PNG → file at AvatarPath, decodes to 96×96; ClearMediaCache removes it; a 5 MP image is refused
func TestAvatarStep(t *testing.T)                   // Review Focus 4: the four outcomes above; after an error the person is not due for 10 minutes
func TestAvatarQueueOneAtATimeNoLoop(t *testing.T)  // Review Focus 4: 3 wants with a blocking fake → 1 in flight; a failing fake is called once per want, not again until due; a want for a jid no longer wanted is dropped
func TestFetchAllowed(t *testing.T)                 // false when hidden, minimised or locked
func TestAvatarsWantOncePerJID(t *testing.T)        // drawing twice asks once; after Invalidate asks again; LRU never holds more than 64
func TestGroupRunsGetOneAvatar(t *testing.T)        // incoming A,A,B,A in a group → avatar flags true,false,true,true; own messages and direct chats: none, no inset
```
- [ ] **Step 2:** run — expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test ./internal/media ./internal/ui ./internal/app` — expected: `ok`.
- [ ] **Step 5:** Live check in a throwaway harness window on a scratch profile: seeded people and avatar files in `t.TempDir()`-style folders show as pictures in a chat list and a group chat; a missing file shows initials; working set after scrolling 200 rows stays within 4 MB of before. Delete the harness.
- [ ] **Step 6: Commit** `git commit -m "feat: profile and group pictures"`

### Task 7: Details view

**Files:**
- Create: `internal/ui/infoview.go`, `internal/ui/infoview_test.go`
- Modify: `internal/ui/chatview.go` (header click, focus, Enter), `internal/app/app.go`, `chat.go`, `logic.go` (+ test)

**Interfaces:**
- Consumes: `Manager.About`, `GroupDetails`, `ProfilePicture(full)`, `FetchPicture` (Task 3); `Directory` (Task 2); `drawAvatar` (Task 6); `focusOn`, `mediaStays`, `closeMedia` (existing).
- Produces:
  - `func (v *ChatView) OnOpenInfo(fn func())` — fired by a click on the header or Enter/Space while it has focus; the header is a tab stop with a visible focus ring.
  - `type InfoView struct{ *walk.Composite }` — `func NewInfoView(parent walk.Container, p theme.Palette) (*InfoView, error)`, `Open(chat store.Chat, d *Directory)`, `SetAbout(jid, about string)`, `SetGroup(jid string, g GroupInfo)`, `SetPicture(jid string, img image.Image)`, `SetError(jid string)`, `Close()`, `OnClose(fn func())`, `OnRetry(fn func(jid string))`, `OnOpenChat(fn func(jid string))`, `SetDirectory(d *Directory)`, `SetPalette(p theme.Palette) error`. Every `Set…` for a jid other than the open one is ignored.
  - `type GroupInfo struct{ Description string; Members []InfoMember }`, `type InfoMember struct{ JID string; Admin bool }`
  - `func sortMembers(ms []InfoMember, d *Directory) []InfoMember` — admins first, then by `Name` case-insensitively.
  - `func infoRows(chat store.Chat, d *Directory, about string, g *GroupInfo, loading, failed bool) []infoRow` (pure) — the rows the view draws, in the spec's order with its omissions (no phone line when hidden, no `About`/`Description` section when empty, `Loading…`, `Couldn't load the details.` + `Retry`).
  - `internal/app`: `openInfo()` / `closeInfo()`; the chat column shows exactly one of chat+input bar, media view, details view; `closeInfo` runs wherever `closeMedia` runs (lock, chat switch, account switch, window hidden or minimised, link page). Replies carry the jid and a generation; a reply for a closed or changed view is dropped. Keys: Esc and the back arrow close; ↑ ↓ move through members; Enter or a click on a member opens that chat (as New chat does) and closes the view.

- [ ] **Step 1: Write the failing tests**
```go
func TestInfoRowsContact(t *testing.T)   // title "Contact info"; name; "+62812…" line; "About" + text; with a hidden number and empty about: neither line; loading → "Loading…"; failed → "Couldn't load the details." and Retry
func TestInfoRowsGroup(t *testing.T)     // title "Group info"; subject; "Description"; "3 members"; members sorted admins first then by name, "Admin" marked
func TestSortMembers(t *testing.T)
func TestInfoViewIgnoresOtherChat(t *testing.T) // Review Focus 5: Open(A); SetAbout(B, …), SetGroup(B, …), SetError(B) change nothing; after Close, SetAbout(A, …) changes nothing
func TestInfoClosesWithTheChats(t *testing.T)   // Review Focus 5, in internal/app: the pure rule that keeps the media view (mediaStays) also decides the details view; locking → closed, generation bumped so a late reply is dropped
```
- [ ] **Step 2:** run — expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test ./internal/ui ./internal/app` — expected: `ok`.
- [ ] **Step 5:** Live check in a harness window (scratch profile, seeded contact and a 300-member group, fake replies): open, keyboard through members, Retry, Esc; dark and light. Delete the harness.
- [ ] **Step 6: Commit** `git commit -m "feat: contact and group details view"`

### Task 8: Notification sound choice

**Files:**
- Modify: `internal/notify/toast.go`, `internal/notify/tray.go`, `internal/notify` tests, `internal/ui/settings.go`, `internal/ui/settingsdialog.go`, `internal/app/logic.go`, `internal/app/app.go` (global keys)
- Create: `internal/notify/sound.go`, `internal/notify/sound_test.go`

**Interfaces:**
- Produces:
  - `ui.KeyNotifySoundName = "notify.sound.name"`, default `"default"`, in the global keys and names (`"Sound"`).
  - `notify.Settings` gains `SoundName string`; `notify.Notification` gains `Sound string` — the file to play, `""` for none.
  - `func SoundFile(name, winDir string, exists func(string) bool) string` (pure) — `message` → `<winDir>\Media\Windows Notify Messaging.wav`, `mail` → `Windows Notify Email.wav`, `reminder` → `Windows Notify Calendar.wav`, `sms` → `Windows Message Nudge.wav`; `default`, an unknown name, or a missing file → `""`.
  - `Format`: `Sound` off → `Silent` true, `Sound` `""`; on with a file → `Silent` true and `Sound` the file; on without a file → `Silent` false (the balloon's own sound).
  - `func Play(path string) error` — `winmm.dll!PlaySoundW(path, 0, SND_FILENAME|SND_ASYNC|SND_NODEFAULT)`, the DLL loaded from the system directory on first use; the dispatcher calls it after showing a notification with `Sound != ""`.
  - Settings › Notifications: under the `Notification sound` switch a `Sound` choice (`Default`, `Message`, `Mail`, `Reminder`, `SMS`), keyboard-operable, disabled while the switch is off; choosing one saves it and plays it once (`Default` plays the Windows default notification sound via `MessageBeep(MB_ICONASTERISK)`).

- [ ] **Step 1: Write the failing tests**
```go
func TestSoundFile(t *testing.T)            // each name → its path under a fake winDir; "default", "bogus" → ""; a missing file → ""
func TestFormatSoundChoice(t *testing.T)    // sound off → Silent, no Sound; on + "mail" file present → Silent true, Sound path; on + "default" → Silent false, Sound ""
```
- [ ] **Step 2:** `go test ./internal/notify -run 'TestSoundFile|TestFormatSound'` — expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test ./internal/notify ./internal/ui ./internal/app` — expected: `ok`. No test plays a sound.
- [ ] **Step 5: Commit** `git commit -m "feat: choose the notification sound"`

### Task 9: README and whole-app verification

**Files:**
- Modify: `README.md`

- [ ] **Step 1:** README: names (`~Name`, `Unknown`, why a number can be hidden), mentions, pictures (fetched on demand, refreshed weekly, removed by Clear cache), the details view (click the chat header; Esc returns), video/GIF previews, the chat list preview line, the notification sound choice. "Known limitations": old mentions resolve only for known people; members with a hidden number and no name show as `Unknown`; GIFs do not play in the message list.
- [ ] **Step 2:** `go vet ./... && go test -count=1 ./...` — expected: every package `ok`.
- [ ] **Step 3:** Build to a scratch folder with `scripts\build.bat`'s flags; run on a scratch `LOCALAPPDATA` for 3 minutes on the link page; measure private bytes and CPU against the same build of the base branch, back to back — expected: private bytes within 2 MB of the base, CPU 0.000% over 60 s, neither `winmm.dll` nor a Media Foundation DLL loaded while idle.
- [ ] **Step 4: Commit** `git add README.md && git commit -m "docs: README for names, pictures, details, previews and notification sound"`
