# Wisp Links, Phone Detection & Link Previews Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Autodetect URLs and phone numbers in Wisp chat messages with WhatsApp-style Copy/Open/Chat-with popups, full-auto link preview cards (composer + incoming lazy fetch), and a Settings toggle.

**Architecture:** A pure-Go `internal/linkify` package detects URLs/phones and normalizes local `0...` numbers using the active account's E.164 country code; `internal/store` adds schema V4 message link columns and a `link_previews` cache; `internal/whatsapp` parses/sends `ExtendedTextMessage` preview fields and caches `IsOnWhatsApp` checks; `internal/linkpreview` fetches OpenGraph/HTML metadata and downscaled JPEG thumbnails with proxy-aware SSRF guards; and `internal/ui` + `internal/app` wire bubble cards, inline click/keyboard popups, composer banners, and Settings.

**Tech Stack:** Go 1.26, `github.com/lxn/walk`, `github.com/lxn/win`, `go.mau.fi/whatsmeow`, `modernc.org/sqlite`, standard library `net/http`, `golang.org/x/net/html` / stdlib HTML tokenizing, `image/jpeg`.

**Spec:** [2026-11-26-wisp-links-phones-previews-design.md](file:///d:/project/wa-go/docs/superpowers/specs/2026-11-26-wisp-links-phones-previews-design.md)

## Global Constraints

- Idle RAM < 50 MB with one account; 0% idle CPU.
- No browser runtimes; no new third-party dependencies in `go.mod`.
- Monochrome stroke icons only (no UI emojis).
- Support both dark and light themes (`theme.Palette`).
- Keyboard operable everywhere (tab/arrow/Enter/Esc and context menu).
- Nothing shown or fetched behind the lock screen (`App.fetchAllowed()` / `!a.locked`).
- Never block the UI thread on network, database, or image-decoding work.

## Review Focus

- **URLs with underscores/formatting neighbors:** `https://example.com/foo_bar_baz` alongside `*bold*` must keep `_bar_` unformatted and preserve exact UTF-16 click hit-boxes (pinned in Task 6).
- **Non-phone digit sequences:** `123,456`, `0.8123456`, `v1.0.234567`, and digits inside URLs like `https://x.test/p/0812345678` must never emit phone spans (pinned in Task 1).
- **SSRF via HTTP redirect:** A public URL redirecting (`302`) to `http://127.0.0.1/` or `http://169.254.169.254/` must be rejected in both direct and proxied modes (pinned in Task 4).
- **Composer async race & dismiss:** Slow fetches completing after `X` dismiss, send, or chat switch must be ignored and never attach stale metadata (pinned in Task 7).
- **Phone lookup failure & empty-chat open:** `IsOnWhatsApp` network errors must degrade cleanly to **Copy phone number** only, and **Chat with** on an unseen JID must open a blank compose-ready chat (pinned in Tasks 6 & 8).

---

## File Structure

- **Create:** `internal/linkify/detect.go`, `internal/linkify/detect_test.go` — URL/phone span detector, E.164 country-code extraction, local `0...` normalization, display domain helper.
- **Create:** `internal/store/linkpreviews.go`, `internal/store/linkpreviews_test.go` — `LinkPreview` cache CRUD (`GetLinkPreview`, `PutLinkPreview`, `ClearLinkPreviews`).
- **Modify:** `internal/store/db.go`, `internal/store/messages.go`, `internal/store/upgrade_test.go`, `internal/store/messages_test.go` — Schema V4 migration and `Message.Link*` persistence.
- **Modify:** `internal/whatsapp/client.go`, `internal/whatsapp/manager.go`, `internal/whatsapp/content_test.go`, `internal/whatsapp/manager_test.go` — `ExtendedTextMessage` preview extract/send, `IsOnWhatsApp` on `waClient`, `Manager.CheckPhones`.
- **Create:** `internal/linkpreview/fetch.go`, `internal/linkpreview/fetch_test.go` — Proxy-aware HTTP + SSRF-safe OpenGraph/HTML metadata + JPEG thumbnail fetcher.
- **Modify:** `internal/ui/settings.go`, `internal/ui/settingsdialog.go`, `internal/ui/settings_test.go` — `KeyLinkPreviews` constant, default, and **Chats & Presence** toggle.
- **Modify:** `internal/ui/icons/icons.go`, `internal/ui/icons/icons_test.go` — `IconCopy` and `IconExternal` stroke icons.
- **Create:** `internal/ui/linkpopup.go`, `internal/ui/linkpopup_test.go` — Borderless themed action popup (`Chat with` / `Copy` / `Open link` / async checking state + keyboard navigation).
- **Modify:** `internal/ui/format.go`, `internal/ui/chatlayout.go`, `internal/ui/chatview.go`, `internal/ui/chatview_test.go` — Inline link/phone spans, preview card layout & paint, click-vs-drag hit-testing, context menu keyboard items, lazy `settle()` preview request.
- **Modify:** `internal/ui/inputbar.go`, `internal/ui/inputbar_test.go` — Debounced first-URL preview banner with `X` dismiss and preview attachment on send.
- **Modify:** `internal/app/app.go`, `internal/app/chat.go`, `internal/app/events.go` — Wire fetcher, proxy updates, `IsOnWhatsApp` checks, `ShellExecute` URL open, `openChat` from phone, `ClearLinkPreviews` on cache clear.
- **Create:** `internal/app/linkpreview_test.go` — App-level integration tests for preview caching, `KeyLinkPreviews` toggle, lock gating, and phone chat opening.

---

### Task 1: URL & Phone Detection and E.164 Normalization (`internal/linkify`)

**Files:**
- Create: `internal/linkify/detect.go`
- Test: `internal/linkify/detect_test.go`

**Interfaces:**
- Produces:
  - `type SpanKind int` (`SpanURL`, `SpanPhone`)
  - `type Span struct { Kind SpanKind; Start, End int; Text string }` (byte offsets in input string)
  - `func Detect(s string) []Span`
  - `func FirstURL(s string) string`
  - `func AccountCountryCode(accountID string) string`
  - `func NormalizePhone(raw, accountID string) (e164, jid string, ok bool)`
  - `func FormatPhoneDisplay(e164 string) string`
  - `func DisplayDomain(rawURL string) string`

- [ ] **Step 1: Write the failing test**

Create `internal/linkify/detect_test.go` with `TestDetectURLsAndPhones`, `TestDetectIgnoresNonPhones`, `TestNormalizePhoneAndAccountCountryCode`, and `TestDisplayDomain`:
```go
func TestDetectURLsAndPhones(t *testing.T) {
	got := Detect("visit https://en.wikipedia.org/wiki/Go_(programming_language). and https://example.com/a_b?id=08123456789! or call +62 882-0007-83234 or 0812 1222 133 33")
	// Assert exact spans:
	// 1. SpanURL: "https://en.wikipedia.org/wiki/Go_(programming_language)" (balanced parens kept, trailing '.' stripped)
	// 2. SpanURL: "https://example.com/a_b?id=08123456789" (trailing '!' stripped, internal digits NOT emitted as SpanPhone)
	// 3. SpanPhone: "+62 882-0007-83234"
	// 4. SpanPhone: "0812 1222 133 33"
}

func TestDetectIgnoresNonPhones(t *testing.T) {
	// Review Focus: commas, decimals, words, multiline splits, non-http schemes
	for _, s := range []string{"testers 123,456", "v1.0.234567", "0.8123456", "abc081234567", "0812\n3456789", "file:///C:/08123456789"} {
		if spans := Detect(s); len(spans) != 0 {
			t.Fatalf("Detect(%q) = %+v, want empty", s, spans)
		}
	}
}

func TestNormalizePhoneAndAccountCountryCode(t *testing.T) {
	if cc := AccountCountryCode("628123456789"); cc != "62" { ... }
	if cc := AccountCountryCode("15550100999"); cc != "1" { ... }
	if cc := AccountCountryCode("886912345678"); cc != "886" { ... }
	e164, jid, ok := NormalizePhone("0812 1222 133 33", "628123456789")
	// expect e164 == "+62812122213333", jid == "62812122213333@s.whatsapp.net", ok == true
	e164, jid, ok = NormalizePhone("+1 (555) 010-0999", "628123456789")
	// expect e164 == "+15550100999", jid == "15550100999@s.whatsapp.net", ok == true
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/linkify/...`
Expected: FAIL (package does not exist).

- [ ] **Step 3: Implement `internal/linkify/detect.go`**

Implement:
- `Detect(s string) []Span`: scan `http://` / `https://` URLs first (stripping trailing `.,;:!?` and unmatched `)]}`), then scan phone candidates outside URL spans (`+` + 7..15 digits with ` -()`, or `0` + 7..14 more digits i.e. 8..15 total with ` -`), requiring clean boundaries (preceding rune not alphanumeric/`.`, `,`, `+`, `/`, `_`; following rune not alphanumeric/`.`, `,`). Reject numbers crossing `\n` or `\r`.
- `AccountCountryCode(accountID string) string`: match leading digits against the prefix-free ITU-T E.164 calling code table (`1`, `7`, 2-digit codes `20,27,30..34,36,39,40..49,51..58,60..66,81,82,84,86,90..95,98`, and 3-digit codes `211..998`).
- `NormalizePhone(raw, accountID string) (e164, jid string, ok bool)`: strip separators (` +-()`); if `raw` starts with `+`, validate 7..15 digits and first digit `'1'..'9'`; if `raw` starts with `0`, require `len >= 8` and non-empty `AccountCountryCode(accountID)` to build `cc + digits[1:]`. Return `"+"+d`, `d+"@s.whatsapp.net"`, `true`.
- `FirstURL(s string) string`, `FormatPhoneDisplay(e164 string) string`, and `DisplayDomain(rawURL string) string`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./internal/linkify/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```powershell
git add internal/linkify/detect.go internal/linkify/detect_test.go; if ($?) { git commit -m "feat(linkify): add URL/phone detector and E.164 normalization" }
```

---

### Task 2: Store Schema V4 & Link Preview Cache (`internal/store`)

**Files:**
- Modify: `internal/store/db.go:12-131`
- Modify: `internal/store/messages.go:25-137,188-240`
- Create: `internal/store/linkpreviews.go`
- Test: `internal/store/linkpreviews_test.go`, `internal/store/upgrade_test.go`, `internal/store/messages_test.go`

**Interfaces:**
- Produces:
  - `store.Message` fields: `LinkURL`, `LinkCanonical`, `LinkTitle`, `LinkDesc string`, `LinkThumb []byte`
  - `type LinkPreview struct { URL, Status, Canonical, Title, Desc string; Thumb []byte; FetchedAt int64 }`
  - `func (d *DB) GetLinkPreview(url string) (LinkPreview, bool, error)`
  - `func (d *DB) PutLinkPreview(p LinkPreview) error`
  - `func (d *DB) ClearLinkPreviews() error`

- [ ] **Step 1: Write the failing tests**

In `internal/store/linkpreviews_test.go` and `internal/store/upgrade_test.go`:
- `TestMigrateV3ToV4`: open a schema V3 database with an existing message row, re-open with `store.Open`, verify `PRAGMA user_version` is `4`, old message reads with empty `Link*` fields, and `link_previews` table exists.
- `TestMessageLinkFieldsRoundTrip`: insert a `store.Message` with `LinkURL`, `LinkCanonical`, `LinkTitle`, `LinkDesc`, `LinkThumb` and verify `GetChatMessages` & `GetMessage` return identical values (including upgrading an `'unsupported'` message).
- `TestLinkPreviewsCacheCRUD`: `PutLinkPreview` for `'ok'` and `'none'`, verify `GetLinkPreview` returns it, upsert updates fields, `ClearLinkPreviews` deletes all rows.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/...`
Expected: FAIL (new fields and methods undefined).

- [ ] **Step 3: Implement schema V4, `Message` link columns, and `linkpreviews.go`**

- Update `schemaVersion = 4` in `internal/store/db.go`, add `schemaV4` (`ALTER TABLE messages` for the 5 `link_*` columns + `CREATE TABLE link_previews`), and run it in `migrate()` when `v < 4`.
- Update `Message`, `messageCols`, `scanMessages`, `insertChunk` (insert + upgrade) in `internal/store/messages.go`. Ensure `COALESCE(?, x'')` for `link_thumb` so nil `[]byte` binds safely.
- Create `internal/store/linkpreviews.go` with `LinkPreview`, `GetLinkPreview`, `PutLinkPreview`, and `ClearLinkPreviews`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/store/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```powershell
git add internal/store/db.go internal/store/messages.go internal/store/linkpreviews.go internal/store/linkpreviews_test.go internal/store/upgrade_test.go; if ($?) { git commit -m "feat(store): add schema v4 link preview columns and cache" }
```

---

### Task 3: WhatsApp Protocol Fields & `IsOnWhatsApp` (`internal/whatsapp`)

**Files:**
- Modify: `internal/whatsapp/client.go:51-92,750-845`
- Modify: `internal/whatsapp/manager.go:260-294` (plus `CheckPhones`)
- Test: `internal/whatsapp/content_test.go`, `internal/whatsapp/manager_test.go`

**Interfaces:**
- Consumes: `store.Message.Link*` (Task 2), `linkify.FirstURL` (Task 1)
- Produces:
  - `type LinkPreview struct { MatchedText, CanonicalURL, Title, Description string; Thumb []byte }`
  - `type PhoneStatus struct { Registered bool; JID string }`
  - `func (m *Manager) SendTextWithPreview(ctx context.Context, accountID, chatJID, text string, reply *store.Message, mentions []string, preview *LinkPreview) (store.Message, error)`
  - `func (m *Manager) CheckPhones(ctx context.Context, accountID string, phones []string) (map[string]PhoneStatus, error)`

- [ ] **Step 1: Write the failing tests**

- In `internal/whatsapp/content_test.go`: `TestToStoreMessageExtractsLinkPreview`: build an `events.Message` with `ExtendedTextMessage` carrying `Text`, `MatchedText`, `CanonicalURL`, `Title`, `Description`, `JPEGThumbnail`; assert `toStoreMessage` populates `LinkURL`, `LinkCanonical`, `LinkTitle`, `LinkDesc`, `LinkThumb` (and falls back `LinkURL` to `CanonicalURL` or `linkify.FirstURL` when `MatchedText` is empty).
- In `internal/whatsapp/manager_test.go`:
  - Add `IsOnWhatsApp` to `fakeClient` (returns stub `[]types.IsOnWhatsAppResponse` and records calls).
  - `TestSendTextWithPreview`: call `SendTextWithPreview`, assert `ExtendedTextMessage` carries all preview fields + `PreviewType_NONE`, and stored `store.Message` keeps them.
  - `TestCheckPhonesCachesSessionResults`: call `CheckPhones` twice for the same `+62...` number, verify `fakeClient.IsOnWhatsApp` is called once and returns `{Registered: true, JID: "62...@s.whatsapp.net"}`; verify unregistered number returns `{Registered: false}`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/whatsapp/...`
Expected: FAIL.

- [ ] **Step 3: Implement `ExtendedTextMessage` extraction/sending and `CheckPhones`**

- Add `IsOnWhatsApp(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error)` to `waClient` in `internal/whatsapp/client.go`.
- In `toStoreMessage`: when unwrapped `ExtendedTextMessage != nil` has preview metadata (`MatchedText`, `CanonicalURL`, `Title`, `Description`, or `JPEGThumbnail`), populate `msg.Link*`.
- In `internal/whatsapp/manager.go`: implement `SendTextWithPreview` (keeping `SendText` calling `SendTextWithPreview(..., nil)`) and `CheckPhones` with a mutex-guarded session map keyed by `accountID + "\x00" + phone`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/whatsapp/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```powershell
git add internal/whatsapp/client.go internal/whatsapp/manager.go internal/whatsapp/content_test.go internal/whatsapp/manager_test.go; if ($?) { git commit -m "feat(whatsapp): support ExtendedTextMessage link previews and CheckPhones" }
```

---

### Task 4: Proxy-Aware OpenGraph Link Preview Fetcher (`internal/linkpreview`)

**Files:**
- Create: `internal/linkpreview/fetch.go`
- Test: `internal/linkpreview/fetch_test.go`

**Interfaces:**
- Consumes: `whatsapp.ValidateProxy` (or transport builder matching `internal/app` proxy config)
- Produces:
  - `type Result struct { CanonicalURL, Title, Description string; Thumb []byte; Found bool }`
  - `type Fetcher struct { ... }`
  - `func NewFetcher(proxyAddr string) (*Fetcher, error)`
  - `func (f *Fetcher) SetProxy(proxyAddr string) error`
  - `func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (Result, error)`

- [ ] **Step 1: Write the failing tests**

Create `internal/linkpreview/fetch_test.go`:
- `TestFetchExtractsOpenGraphAndThumbnail`: `httptest.Server` serving HTML with `og:title`, `og:description`, relative `og:image`, and a PNG/JPEG image; verify `Result.Found == true`, `Title`, `Description`, and `Thumb` is valid JPEG <= 320px wide.
- `TestFetchTextOnlyWithoutImage`: HTML with `<title>` and `<meta name="description">` only; verify `Found == true`, `Thumb == nil`.
- `TestFetchNoMetadataReturnsFoundFalse`: HTML with no title/description/image; verify `Found == false, err == nil`.
- `TestFetchBlocksSSRFDirectAndRedirects`:
  - Direct mode rejecting `http://127.0.0.1:...` / `http://169.254.169.254/...` and rejecting a public test URL that returns `302` to `http://127.0.0.1/` (Review Focus).
  - Proxied mode (`SetProxy` to local `httptest.Server` proxy) allowing dial to the `127.0.0.1` proxy while rejecting target URLs pointing to `localhost`, `.local`, or literal private IPs.
- `TestFetchCapsOversizedHTMLAndImage`: 1 MB HTML body stops at 512 KB without OOM; >2 MB image is skipped (card degrades to text-only).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/linkpreview/...`
Expected: FAIL.

- [ ] **Step 3: Implement `internal/linkpreview/fetch.go`**

- Build proxy-aware `http.Client` (using `golang.org/x/net/proxy` for `socks5://` and `http.ProxyURL` for `http(s)://`, matching `internal/whatsapp/manager.go` / `gifHTTP`). Allow injecting IP-checker hook for unit tests where `httptest.Server` listens on `127.0.0.1`.
- Implement SSRF rules per spec: direct mode checks resolved IPs in `DialContext` (`isBlockedIP`: loopback, private, link-local, unspecified, multicast); proxied mode skips local DNS lookup and validates target/redirect hosts against `localhost`, `.local`, and literal `isBlockedIP`. Enforce max 5 redirects via `CheckRedirect`, 8s timeout, 512 KB HTML cap, 2 MB image cap, and JPEG downscaling via `golang.org/x/image/draw` + `image/jpeg` (already in `go.mod`).
- Parse meta tags cleanly using `golang.org/x/net/html` tokenizer (already in `go.mod`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/linkpreview/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```powershell
git add internal/linkpreview/fetch.go internal/linkpreview/fetch_test.go; if ($?) { git commit -m "feat(linkpreview): add proxy-aware SSRF-safe OpenGraph fetcher" }
```

---

### Task 5: Settings Toggle (`KeyLinkPreviews`) (`internal/ui/settings*`)

**Files:**
- Modify: `internal/ui/settings.go:28-42,103-118`
- Modify: `internal/ui/settingsdialog.go:397-410`
- Test: `internal/ui/settings_test.go`

**Interfaces:**
- Produces: `const KeyLinkPreviews = "chat.link_previews"` (default `"true"`, name `"Link previews"`)

- [ ] **Step 1: Write the failing test**

In `internal/ui/settings_test.go`: assert `Setting(db, "", KeyLinkPreviews)` defaults to `"true"` and persists `"false"` via `SetSetting`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ui -run Test.*LinkPreview`
Expected: FAIL.

- [ ] **Step 3: Add `KeyLinkPreviews` and toggle in `buildChats`**

- Add `KeyLinkPreviews = "chat.link_previews"` to `settings.go`, `settingNames`, and `defaults` (`"true"`).
- In `settingsdialog.go` `buildChats`: add `newToggle(pg, "Link previews", "Show preview cards for links and fetch previews while typing.", "true", "false")` and `d.bind(secChats, KeyLinkPreviews, linkPreviews)`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/ui -run Test.*Setting`
Expected: PASS.

- [ ] **Step 5: Commit**

```powershell
git add internal/ui/settings.go internal/ui/settingsdialog.go internal/ui/settings_test.go; if ($?) { git commit -m "feat(settings): add Link previews toggle under Chats & Presence" }
```

---

### Task 6: Icons, Bubble Preview Cards, Inline Link/Phone Spans & Action Popup (`internal/ui`)

**Files:**
- Modify: `internal/ui/icons/icons.go`, `internal/ui/icons/icons_test.go`
- Modify: `internal/ui/format.go:38-85`
- Modify: `internal/ui/chatlayout.go:127-160,236-270,390-510`
- Create: `internal/ui/linkpopup.go`, `internal/ui/linkpopup_test.go`
- Modify: `internal/ui/chatview.go:250-320,612-645,907-1050,1174-1240,1485-1580`
- Test: `internal/ui/chatview_test.go`

**Interfaces:**
- Consumes: `linkify.Detect`, `linkify.DisplayDomain`, `store.Message.Link*`
- Produces:
  - `icons.IconCopy`, `icons.IconExternal`
  - `func (v *ChatView) SetLinkPreviews(enabled bool)`
  - `func (v *ChatView) ApplyLinkPreview(msgID string, url, canonical, title, desc string, thumb []byte)`
  - `func (v *ChatView) DismissPopup()`
  - `func (v *ChatView) OnNeedLinkPreview(fn func(m store.Message))`
  - `func (v *ChatView) OnOpenLink(fn func(url string))`
  - `func (v *ChatView) OnCheckPhone(fn func(raw string, done func(registered bool, display, jid string)))`
  - `func (v *ChatView) OnOpenPhoneChat(fn func(jid, display string))`

- [ ] **Step 1: Write the failing tests**

- `internal/ui/icons/icons_test.go`: add `IconCopy` and `IconExternal` to the rendered icons test.
- `internal/ui/chatview_test.go`:
  - `TestBubbleLinkPreviewCardAndToggle`: message with `LinkTitle`/`LinkDesc`/`LinkThumb` lays out `linkCard` + `linkThumb` above `text`; message without `LinkThumb` lays out `linkCard` with zero `linkThumb`; `SetLinkPreviews(false)` collapses `linkCard` to zero.
  - `TestBubbleInlineLinksAndPhonesWithFormatting` (Review Focus): message `"*bold* https://example.com/foo_bar_baz +62 882-0007-83234"` keeps `foo_bar_baz` unformatted, adjusts UTF-16 `linkSpans` and `phoneSpans` offsets accurately after `*` removal, and `spanAt` hits both spans and misses non-span text.
  - `TestClickVsDragOnLink`: down+up within drag threshold opens link callback; down+drag+up selects text without firing link click.
- `internal/ui/linkpopup_test.go`:
  - `TestLinkPopupItems`: link mode exposes `Open link` and `Copy link`; phone mode starts with `Checking WhatsApp...` + `Copy phone number`, inserts `Chat with +62 ...` when registered=true, removes checking row leaving only `Copy phone number` when registered=false or error (Review Focus).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/ui/...`
Expected: FAIL.

- [ ] **Step 3: Implement icons, layout, popup, painting, and click/keyboard handling**

- Add `IconCopy` and `IconExternal` 24x24 vector stroke ops in `internal/ui/icons/icons.go`.
- In `chatlayout.go`:
  - Detect `linkify.Detect(rewritten)` on message text, convert byte offsets to UTF-16 spans, pass combined `(mentions + links + phones)` into `formatText` so formatting markers never break URLs/phones and spans shift together, then store `b.linkSpans` and `b.phoneSpans` separately on `bubble`.
  - Add preview card geometry (`b.linkCard`, `b.linkThumb`, `b.linkTitle`, `b.linkDesc`, `b.linkDomain`, `b.linkTitleText`, `b.linkDescText`, `b.linkDomainText`, `b.linkTargetURL`) when `l.linkPreviews` is true and the message has preview metadata (preferring `m.LinkTitle`/`m.LinkDesc`/`m.LinkThumb`, or preview cache injected via `SetChat` / `ApplyLinkPreview`), positioned below `quotedBox` and above `text`.
- In `chatview.go`:
  - Paint `b.linkCard` (tinted card background, optional banner thumbnail decoded via `decodeCapped`, bold title, secondary description, muted domain) and draw `linkSpans` (`TickRead`, underlined — darkened slightly on light theme if needed for contrast) and `phoneSpans` (`Accent`, underlined) in text runs.
  - Track `pressSpan` on `mouseDown` and trigger popup on `mouseUp` when `!v.dragging` (pointer within system drag threshold) and there is no multi-char selection, or on single-click inside `b.linkCard`.
  - Expose detected links/phones on the focused message in `buildMenu` so keyboard users can activate them via context menu / `Apps` key.
  - In `settle()`: when `v.linkPreviews` and `v.onNeedLinkPreview != nil`, request previews once per message ID (`v.linkRequested[m.ID]`) for visible messages with `linkify.FirstURL(m.Body) != ""` and empty `LinkTitle`/`LinkDesc`.
- In `internal/ui/linkpopup.go`: build the borderless themed `walk.Dialog` popup using `kit`, monochrome `icons`, arrow/Enter/Esc keyboard handling, and safe async update guard (`closed` flag so late `OnCheckPhone` callbacks after dismissal are no-ops).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/ui/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```powershell
git add internal/ui/icons/icons.go internal/ui/icons/icons_test.go internal/ui/format.go internal/ui/chatlayout.go internal/ui/chatview.go internal/ui/chatview_test.go internal/ui/linkpopup.go internal/ui/linkpopup_test.go; if ($?) { git commit -m "feat(ui): render link preview cards, clickable links/phones, and action popup" }
```

---

### Task 7: Input Bar Composer Preview Banner (`internal/ui/inputbar.go`)

**Files:**
- Modify: `internal/ui/inputbar.go:42-220,380-560`
- Test: `internal/ui/inputbar_test.go`

**Interfaces:**
- Consumes: `linkify.FirstURL`, `linkify.DisplayDomain`, `icons.IconClose`
- Produces:
  - `type ComposerLinkPreview struct { URL, Canonical, Title, Desc string; Thumb []byte }`
  - `func (b *InputBar) SetLinkPreviewsEnabled(enabled bool)`
  - `func (b *InputBar) OnNeedLinkPreview(fn func(url string))`
  - `func (b *InputBar) SetLinkPreview(url string, p *ComposerLinkPreview)`
  - `func (b *InputBar) OnSendWithPreview(fn func(text string, preview *ComposerLinkPreview))` (or `ActiveLinkPreview() *ComposerLinkPreview` read synchronously inside `OnSend`)

- [ ] **Step 1: Write the failing tests**

In `internal/ui/inputbar_test.go`:
- `TestComposerLinkPreviewDebounceAndDismiss` (Review Focus):
  1. Typing `"check https://example.com"` schedules one `onNeedLinkPreview("https://example.com")` after debounce.
  2. `SetLinkPreview("https://example.com", ...)` shows the preview banner; clicking `X` dismisses it and clears active preview.
  3. Further edits that keep `"https://example.com"` as first URL do NOT re-request or re-show the dismissed preview; changing the URL to `"https://other.dev"` clears the dismissal and fetches the new URL.
  4. Stale `SetLinkPreview("https://old.dev", ...)` after text changed or `Reset()` is ignored.
  5. `SetLinkPreviewsEnabled(false)` hides any open link banner and stops firing `onNeedLinkPreview`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ui -run TestComposerLinkPreview`
Expected: FAIL.

- [ ] **Step 3: Implement composer link preview strip in `internal/ui/inputbar.go`**

- Add a `linkBanner` custom widget above `edit` (separate from `replyBanner` so replying with a link still shows both cleanly), painted with optional thumbnail on the left, title/desc/domain, and `icons.IconClose` `X` hit target on the right.
- Debounce text changes (~400ms via `b.after`), track `previewURL`, `dismissedURL`, and `activePreview *ComposerLinkPreview`.
- Snapshot `ActiveLinkPreview()` on `sendNow()` before `Reset()` clears it.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/ui -run TestComposerLinkPreview`
Expected: PASS.

- [ ] **Step 5: Commit**

```powershell
git add internal/ui/inputbar.go internal/ui/inputbar_test.go; if ($?) { git commit -m "feat(ui): add dismissible composer link preview banner in InputBar" }
```

---

### Task 8: App Wiring, Browser Opening, Phone Deep-Linking & Lock Safety (`internal/app`)

**Files:**
- Modify: `internal/app/app.go:280-360,490-550`
- Modify: `internal/app/chat.go:176-210,380-430`
- Create: `internal/app/linkpreview_test.go`

**Interfaces:**
- Consumes: `linkpreview.Fetcher`, `store` preview methods, `whatsapp.Manager.CheckPhones`, `whatsapp.Manager.SendTextWithPreview`, `ChatView` & `InputBar` callbacks.
- Produces: Complete end-to-end wiring inside `App`.

- [ ] **Step 1: Write the failing tests**

Create `internal/app/linkpreview_test.go`:
- `TestOpenLinkRejectsNonHTTPAndAllowsHTTPS`: validate scheme guard before `ShellExecute`.
- `TestResolvePhoneUsesLocalContactsBeforeNetwork`: if contact/chat already exists in `a.v.dirs` / `a.side`, resolve immediately without `CheckPhones`; otherwise call `CheckPhones` off UI thread and handle errors by reporting `registered = false` (Review Focus).
- `TestOpenNewChatFromPhoneCreatesBlankComposePane`: calling `openChat` for a registered JID with 0 messages opens an empty chat pane ready to type (Review Focus).
- `TestLinkPreviewWorkerRespectsSettingAndLock`: when `KeyLinkPreviews == "false"` or `a.locked == true`, preview requests are skipped; when `store.GetLinkPreview` has a valid cached `'ok'` or non-expired `'none'` (<24h), no HTTP fetch runs.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/app -run "Test(OpenLink|ResolvePhone|OpenNewChat|LinkPreview)"`
Expected: FAIL.

- [ ] **Step 3: Wire everything in `internal/app/app.go` and `internal/app/chat.go`**

- Instantiate `a.linkFetch` (`*linkpreview.Fetcher`) with `a.cfg[ui.KeyProxy]`, and update its proxy when `KeyProxy` changes in Settings.
- Initialize `a.chat.SetLinkPreviews(a.cfg[ui.KeyLinkPreviews] != "false")` and `a.bar.SetLinkPreviewsEnabled(a.cfg[ui.KeyLinkPreviews] != "false")`, updating both on `KeyLinkPreviews` settings changes.
- Wire `a.chat.OnOpenLink`: parse URL, allow only `http` / `https`, call `windows.ShellExecute(0, "open", ...)`.
- Wire `a.chat.OnCheckPhone`: normalize with `linkify.NormalizePhone(raw, a.v.active)`; if invalid, return `false`; if JID is already in contacts/chats, return `true` immediately; else if `!a.locked`, run `a.mgr.CheckPhones` in a goroutine and deliver via `a.mw.Synchronize`.
- Wire `a.chat.OnOpenPhoneChat`: call `a.openChat(a.v.active, jid, display)` (which opens existing chats or blank compose panes).
- Wire `a.chat.OnNeedLinkPreview` and `a.bar.OnNeedLinkPreview`: check `a.db.GetLinkPreview(url)` off UI thread (`'ok'` or `'none'` within 24h); if cache miss and `a.fetchAllowed()` (or for composer, `!a.locked && linkPreviews`), fetch via `a.linkFetch.Fetch`, store `'ok'` or `'none'` via `a.db.PutLinkPreview`, and push result to `ChatView.ApplyLinkPreview` or `InputBar.SetLinkPreview`.
- Also enrich loaded chat pages (`loadChat` / `loadPrevious` / incoming `onMessage`) with cached `link_previews` entries when `m.LinkTitle == "" && m.LinkDesc == ""` so previously fetched previews render immediately on reopen without re-fetching.
- Update `a.send` in `internal/app/chat.go` to pass `a.bar.ActiveLinkPreview()` into `a.mgr.SendTextWithPreview`.
- Update `cacheCleared()` in `internal/app/chat.go` to include `a.db.ClearLinkPreviews()`.
- In `App.lock()` (`app.go`), call `a.chat.DismissPopup()` alongside `endMenu.Call()`.

- [ ] **Step 4: Run full test suite and build**

Run: `go test ./...`
Expected: PASS across all packages with zero failures.

- [ ] **Step 5: Commit**

```powershell
git add internal/app/app.go internal/app/chat.go internal/app/linkpreview_test.go; if ($?) { git commit -m "feat(app): wire link/phone actions, link previews, and settings" }
```
