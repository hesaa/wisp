# Wisp: Link & Phone Detection, Chat Deep-Linking, and Link Previews — Design

**Date:** 2026-11-26  
**Status:** Approved  
**Builds on:** `2026-11-10-whatsapp-client-design.md`, `2026-11-11-wisp-people-previews-design.md`, `2026-11-13-wisp-mentions-replies-design.md`  
**Binding constraints:** Idle RAM < 50 MB with one account, 0% idle CPU, no browser runtimes, no new third-party dependencies, monochrome stroke icons only (no UI emojis), dark and light themes, keyboard operable everywhere, nothing shown or fetched behind lock screen.

---

## 1. Goal

Add two connected capabilities to Wisp's chat experience:

1. **Autodetect Links & Phone Numbers with Action Popups:**
   - Detect `http://` / `https://` URLs and phone numbers (international `+...` and local `0...` formats) inside chat message text and render them inline as interactive underlined spans.
   - Clicking a **phone number** opens a compact action menu:
     - If the number is registered on WhatsApp (`IsOnWhatsApp`), show **Chat with \<formatted number\>** and **Copy phone number**.
     - If the number is not registered on WhatsApp, omit the **Chat with** item and show only **Copy phone number**.
     - Choosing **Chat with** jumps directly to that contact's existing chat room, or opens an empty new chat ready to compose if there are no messages yet.
   - Clicking a **link** (or its preview card) opens a compact action menu with **Open link** (opens the default browser) and **Copy link**.

2. **Full-Auto Link Previews with Settings Toggle:**
   - Render link preview cards inside message bubbles above the message text:
     - **Rich card** (thumbnail banner + title + description + domain) when preview metadata includes an image.
     - **Text-only card** (title + description + domain) when preview metadata exists without a thumbnail.
     - **Link only** (no card) when no preview exists or when **Link previews** is turned off in Settings.
   - **Composer preview (Input Bar):** While typing or pasting a link in the input box, automatically fetch a preview for the first URL and show a dismissible preview strip (`X` button) above the input box. Unless dismissed or disabled in Settings, sending attaches the preview metadata to the outgoing WhatsApp `ExtendedTextMessage`.
   - **Incoming auto-fetch:** Render preview metadata already attached to incoming `ExtendedTextMessage` messages; if a message viewed in the open chat has a link without attached preview metadata (and **Link previews** is enabled), fetch OpenGraph/HTML metadata lazily off the UI thread and cache it locally. *(My call)* Trigger missing-preview auto-fetching lazily via `ChatView.settle()` gated by `App.fetchAllowed()` (visible, not minimized, not locked)—matching how `ChatView` handles visible media—for both incoming and `FromMe` messages (e.g., history sync or messages sent from a linked phone) rather than eagerly on background `onMessage` events.
   - **Settings toggle:** Add a **Link previews** toggle (on by default) in **Chats & Presence**. Turning it off hides preview cards in chat bubbles and disables both composer and incoming background fetching.

---

## 2. Non-Goals & Anti-Goals

### Non-Goals
- **Multiple preview cards per message:** At most one preview card is displayed per message (matching WhatsApp Desktop: the matched/first URL), even if the message contains multiple URLs; all URLs in the body remain individually clickable.
- **Ambiguous bare numbers without `+` or `0`:** Plain numbers such as `123,456`, decimals (`0.123`), or numbers embedded inside URLs or words are not detected as phone numbers.
- **Custom link scheme execution:** Only `http://` and `https://` URLs are detected and opened in the browser; `file://`, `javascript:`, `cmd:`, etc. are never auto-linked or passed to `ShellExecute`.
- **Post-send preview editing:** Once a message is sent, its attached preview is not modified.

### Anti-Goals
- **UI-thread blocking:** Never run `IsOnWhatsApp`, OpenGraph HTTP requests, or image decoding synchronously on the UI thread.
- **Repeated network chatter:** Scrolling a chat must never re-fetch a URL already fetched (whether it succeeded or had no preview); cache negative results as well as positive ones.
- **SSRF / unbounded downloads:** Background link fetching must reject loopback, link-local, and private/RFC1918 IP addresses, enforce strict response size and timeout caps, and route through `KeyProxy` when configured.

---

## 3. Detection & Phone Normalization

### URL & Phone Span Rules
Implement a single pure-Go detector (shared by `ChatView` layout/hit-testing, `InputBar`, and preview fetching) returning ordered non-overlapping spans with kind (`SpanURL` or `SpanPhone`), byte/UTF-16 offsets, raw text, and normalized target:

- **URL detection:**
  - Matches `http://` and `https://` URLs up to whitespace or `<>"'`.
  - Strips trailing sentence punctuation (`.`, `,`, `;`, `:`, `!`, `?`, and unmatched `)`, `]`, `}`) while preserving balanced parentheses inside URLs (e.g., Wikipedia links).
  - URL spans take precedence over any digit sequences inside them.
  - *(My call)* `wa.me/<number>` links are detected as regular `SpanURL` links (**Open link** / **Copy link**).

- **Phone number detection:**
  - Matches candidate phone numbers bounded by non-alphanumeric / non-`.` / non-`,` boundaries so numbers inside words, URLs, decimals (`0.8123456`), or comma-formatted numbers (`123,456`) are ignored.
  - Accepted prefixes:
    1. **International (`+`):** starts with `+`, followed by digits and optional separators (` `, `-`, `(`, `)`) totaling **7 to 15 digits**, first digit `1`–`9`.
    2. **Local (`0`):** starts with `0`, followed by digits and optional spaces/dashes totaling **8 to 15 digits** (e.g. `081234567`, `0812 1222 133 33`).
  - *(My call)* Consecutive digits separated by newlines do not form a single phone number.

### Local `0...` to E.164 Conversion
- WhatsApp's `IsOnWhatsApp` and JIDs require an international E.164 number with country calling code.
- Extract the active account's country calling code from `accountID` (which is the account's own phone digits, e.g. `62812...` → `62`) using the standard prefix-free ITU-T E.164 country calling code table.
- For a local `0...` span: strip the leading `0` and prepend `+<accountCountryCode>` when checking `IsOnWhatsApp` and building the target JID (`<digits>@s.whatsapp.net`).
- Display text in the popup keeps a readable international format when resolved (e.g. `Chat with +62 812-3456-7890` or `Chat with +6281234567890`), while **Copy phone number** copies the phone number text from the message (or normalized number — *(my call)*: copy the exact matched phone text as shown in the message so copying `0812 1222 133 33` pastes what the user clicked).

---

## 4. Data Storage (`internal/store`)

### Schema V4 Migration
Advance `schemaVersion` from `3` to `4` in `internal/store/db.go` with additive changes:

1. **Columns on `messages`** (for preview metadata received from or sent via WhatsApp):
   ```sql
   ALTER TABLE messages ADD COLUMN link_url TEXT NOT NULL DEFAULT '';
   ALTER TABLE messages ADD COLUMN link_canonical TEXT NOT NULL DEFAULT '';
   ALTER TABLE messages ADD COLUMN link_title TEXT NOT NULL DEFAULT '';
   ALTER TABLE messages ADD COLUMN link_desc TEXT NOT NULL DEFAULT '';
   ALTER TABLE messages ADD COLUMN link_thumb BLOB NOT NULL DEFAULT x'';
   ```
   `store.Message` gains `LinkURL`, `LinkCanonical`, `LinkTitle`, `LinkDesc`, and `LinkThumb []byte`, included in `messageCols`, `scanMessages`, `InsertMessage`, `InsertMessagesBatch`, and the unsupported-message upgrade statement.

2. **Table `link_previews`** (local cache for auto-fetched OpenGraph previews, keyed by URL):
   ```sql
   CREATE TABLE link_previews (
       url        TEXT PRIMARY KEY,
       status     TEXT NOT NULL DEFAULT '', -- 'ok' or 'none'
       canonical  TEXT NOT NULL DEFAULT '',
       title      TEXT NOT NULL DEFAULT '',
       desc       TEXT NOT NULL DEFAULT '',
       thumb      BLOB NOT NULL DEFAULT x'',
       fetched_at INTEGER NOT NULL DEFAULT 0
   );
   ```
   - `GetLinkPreview(url)` returns the cached entry if present.
   - `PutLinkPreview(entry)` upserts `'ok'` (with metadata + JPEG thumbnail bytes) or `'none'` (negative cache when a site returned no usable metadata or failed).
   - *(My call)* Cap negative `'none'` cache TTL at 24 hours (re-checkable on later session/view) and treat `'ok'` entries as persistent until cleared via Settings **Clear cache**. Also include `DELETE FROM link_previews` inside cache-clearing if appropriate *(my call: add a `ClearLinkPreviews()` helper called on **Clear cache**)*.

---

## 5. WhatsApp Protocol Layer (`internal/whatsapp`)

### Inbound Link Previews (`internal/whatsapp/client.go`)
Update `extractContent` / `toStoreMessage` when `m.GetExtendedTextMessage() != nil`:
- Extract `MatchedText`, `CanonicalURL`, `Title`, `Description`, and `JPEGThumbnail` into `store.Message`'s `Link*` fields.
- If `MatchedText` is empty while `CanonicalURL` or `Title` is non-empty, fall back to `CanonicalURL` (or the first detected URL in `Text`) as `LinkURL`.

### Outbound Link Previews (`internal/whatsapp/manager.go`)
- Extend `SendText` (or add a `LinkPreview` struct parameter) so when a link preview is present on send, the outgoing `waE2E.Message` uses `waE2E.ExtendedTextMessage` with:
  - `Text`: message text
  - `MatchedText`: matched URL from composer
  - `CanonicalURL`: canonical URL (if any)
  - `Title`: preview title
  - `Description`: preview description
  - `JPEGThumbnail`: JPEG thumbnail bytes (when present)
  - `PreviewType`: `waE2E.ExtendedTextMessage_NONE.Enum()` (standard WhatsApp link preview type)
- Persist the same `Link*` fields on the locally stored outgoing `store.Message` so the sender immediately sees the sent preview card without re-fetching.

### WhatsApp Registration Lookup (`IsOnWhatsApp`)
- Add `CheckPhones(ctx context.Context, accountID string, phones []string) (map[string]PhoneStatus, error)` on `whatsapp.Manager` calling `cli.IsOnWhatsApp(ctx, phones)` (where `PhoneStatus` holds `IsRegistered bool` and `JID string`).
- Add `IsOnWhatsApp` to the `waClient` interface in `internal/whatsapp/client.go` so it is unit-testable with `fakeClient`.
- Cache results in memory per `(accountID, normalizedE164)` for the session (`Manager`) so clicking the same phone number again is instant. Also treat numbers already present in `store` chats or `whatsapp` contacts as immediately known without waiting on a network round-trip.

---

## 6. Link Preview Fetcher (`internal/app` or dedicated `internal/linkpreview` package)

- Runs off the UI thread, respecting `KeyProxy` (using the same proxy dialer/transport configuration as `gifHTTP`).
- **Safety & resource limits:**
  - Only `http` and `https` schemes.
  - *(My call)* **Proxy-aware SSRF protection:**
    - **Direct connections (no `KeyProxy`):** Custom `DialContext` inspects resolved IPs before dialing and blocks loopback (`127.0.0.0/8`, `::1`), private (`10/8`, `172.16/12`, `192.168/16`, `fc00::/7`), link-local (`169.254/16`, `fe80::/10`), and unspecified/multicast addresses, including across HTTP redirects (max 5 redirects).
    - **Proxied connections (`KeyProxy` set, e.g. `socks5://127.0.0.1:1080`):** Allow dialing the configured proxy host itself (which is commonly localhost), skip local DNS lookup (preventing DNS leaks outside the proxy), and reject target/redirect URLs whose host is `localhost`, `.local`, or a literal loopback/private/link-local IP.
  - Request timeout: 8 seconds; HTML body read capped at 512 KB; image fetch capped at 2 MB.
  - Thumbnail image is decoded safely (bounded pixel count, matching `maxPreviewPixels`), downscaled to a compact banner thumbnail (max width ~320px), and encoded to JPEG (e.g. quality 75, ~15–40 KB) so `JPEGThumbnail` stays small for WhatsApp and SQLite.
- **Metadata extraction:**
  - Parse `<meta property="og:title">`, `<meta property="og:description">`, `<meta property="og:image">`, `<meta property="og:url">`, falling back to `<meta name="description">`, `<title>`, and `twitter:title` / `twitter:description` / `twitter:image`.
  - Resolve relative image/canonical URLs against the final response URL.
  - Extract display domain (host without leading `www.` if desired, or `url.Host`, matching WhatsApp's `www.tasteskill.dev` / `google.com` style).
  - A preview counts as existing if at least a non-empty `Title` or `Description` (or `Image`) was parsed; otherwise store status `'none'`.

---

## 7. Settings (`internal/ui/settings.go`, `settingsdialog.go`)

- Add constant `KeyLinkPreviews = "chat.link_previews"` with default `"true"` and label `"Link previews"`.
- In `buildChats` (**Chats & Presence** section), add a toggle:
  - Title: **Link previews**
  - Subtitle: **Show preview cards for links and fetch previews while typing.**
- Saving the toggle notifies `App` (`OnSettingsChanged(KeyLinkPreviews)`):
  - Updates `ChatView` and `InputBar` immediately.
  - When toggled off: `ChatView` re-layouts without preview cards (plain underlined links only), `InputBar` clears/hides any active composer preview strip, and background link fetching stops.
  - When toggled on: `ChatView` re-layouts to show stored/cached preview cards and triggers background fetch for visible messages missing previews.

---

## 8. UI & Interaction (`internal/ui`, `internal/app`)

### Message Bubble Rendering (`ChatLayout` & `ChatView`)
- **Inline spans:** Detect URLs and phone numbers in `bubbleText` alongside `@mentions` and `formatText` (`*bold*`, etc.). *(My call)* Render clickable links in `Palette.TickRead` (`#53bdeb`) underlined, and phone numbers in `Palette.Accent` (`#00a884`) underlined (or `Palette.TickRead` for both if preferred — let's use `Palette.TickRead` for links and `Palette.Accent` for phone numbers, matching the approved mockup, or `Palette.Accent` for links in light theme if contrast requires: *(my call)* ensure sufficient contrast in both dark and light `theme.Palette`).
- **Preview card inside bubble:**
  - Placed inside the bubble **above** the message body text (and below any quoted-reply card if the message is a reply), matching the approved mockup.
  - Tinted inner container (`rgba` / blended dark or light surface within the bubble, rounded corners).
  - **Rich card** (when `thumb` decodes): top image banner (cropped/fit to bubble card width, fixed max aspect height) + title (bold, max 2 lines) + description (secondary color, max 2 lines) + domain (muted, 1 line).
  - **Text-only card** (when no `thumb`): title + description + domain inside the same tinted inner container.
  - **No card** when `LinkPreviews` is false or no preview exists for the message.

### Click, Selection, & Keyboard Interaction (`ChatView`)
- **Left-click vs text selection:**
  - On `mouseDown` over bubble text: record hit span (if any) and drag origin, and start selection tracking as today (`pressText`).
  - On `mouseUp`: if the user did **not** drag (pointer moved within system drag threshold) and there is no multi-character selection, and both down & up land on the same URL/phone span (or inside the preview card on a single click), open the **Item Action Popup** anchored at the click point!
  - Dragging across a link/phone still selects text normally; double-clicking non-link/non-phone text still replies; right-clicking anywhere on a message still opens the message context menu (`Reply`, `Copy`, `Select`).
- **Action Popup UI:**
  - Lightweight borderless/themed popup menu matching Wisp's palette (`Sidebar`/`Header` background, `Text`, `TextSecondary`, subtle 1px border) with monochrome stroke icons (`icons` package, no emojis):
  - **For a link (or preview card):**
    1. `[External-link icon]` **Open link** — validates `http`/`https` and opens in default browser (`ShellExecute` `"open"`).
    2. `[Copy icon]` **Copy link** — copies the URL to clipboard.
  - **For a phone number:**
    - On open, check session cache / local contacts / chats first; if unknown, show **Copy phone number** immediately with a subtle muted `"Checking WhatsApp..."` row (or disabled placeholder) while `IsOnWhatsApp` runs off the UI thread.
    - When `IsOnWhatsApp` returns **registered**: replace/enable the top row as `[Chat icon] Chat with +<number>`. Clicking it closes the popup and calls `App.openChat(activeAccount, jid, contactName)`, which opens the existing chat room or opens an empty new chat ready to compose if no messages exist.
    - When `IsOnWhatsApp` returns **not registered** (or the lookup fails / number cannot be normalized): remove the top row so only `[Copy icon] Copy phone number` remains.
  - Keyboard operable inside the popup: `↑`/`↓` moves highlight, `Enter` activates, `Esc` or click outside dismisses.
  - **Keyboard access from chat list (`ChatView`):** *(My call)* Add detected link/phone actions to the focused message's right-click / keyboard `Apps` context menu (e.g. `Open link` / `Chat with +...` / `Copy ...` when the focused message contains links or numbers) so keyboard-only users can trigger them without a mouse.

### Composer Preview Banner (`InputBar`)
- When `KeyLinkPreviews` is `"true"`:
  - Debounce text changes (~400ms) and detect the first URL in `InputBar.Text()`.
  - If the first URL changed (and wasn't dismissed by `X`), ask `App` to fetch its preview off the UI thread (ignoring stale responses if the input text changed or chat switched).
  - Show a compact horizontal preview card above the text box (alongside/independent of `replyBanner`): small square/landscape thumbnail on the left (if present), bold title + 1-line description + domain, and a monochrome `X` close button on the right.
  - Clicking `X` hides the banner and marks that URL as dismissed for the current draft so it does not re-appear until the URL changes or the message is sent/cleared.
  - Sending clears the banner and attaches the active preview to `SendText`.

### Lock Screen & Background Fetch Safety
- Missing-preview auto-fetches run only when `App.fetchAllowed()` is true (window visible, not minimized to tray, not locked). While `a.locked` is true, no link previews are fetched, no `IsOnWhatsApp` requests are triggered, and any open action popup is dismissed on lock (`endMenu` / popup close).

---

## 9. Left to the Builder

- Exact pixel padding, corner radii, and tint colors for the preview card and action popup within `theme.Palette`.
- Exact decomposition of helpers across `internal/ui`, `internal/app`, `internal/whatsapp`, and `internal/store`.
- Unit tests covering: URL & phone detection edge cases (including `123,456`, decimals, URLs with digits, `0...` local numbers, E.164 country code extraction), schema V4 migration and `link_previews` cache, `ExtendedTextMessage` round-trip, `IsOnWhatsApp` mocked client behavior, SSRF IP blocking & HTML meta parsing, and `ChatView` / `InputBar` layout & hit-testing.
