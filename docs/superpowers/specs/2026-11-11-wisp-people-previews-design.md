# Wisp: people, pictures, details and video previews — design

**Date:** 2026-11-11
**Status:** approved in conversation, awaiting written-spec review
**Builds on:** `2026-11-10-whatsapp-client-design.md` and `2026-11-11-wisp-front-page-media-copy-design.md`
(binding constraints still apply: idle RAM < 50 MB with one account, 0% idle CPU, no browser
runtimes, no new third-party modules, monochrome UI icons, dark and light themes, multi-account,
every control operable by keyboard, nothing shown or fetched behind the lock page).

## Goal

Five gaps found in the first live test:

1. **Names** — unsaved contacts and group members show as bare internal IDs or numbers.
2. **Mentions** — a mention shows as `@35893729566783` instead of `@Name`.
3. **Pictures** — every avatar is initials; no profile or group pictures.
4. **Details** — there is no way to see a contact's or a group's details.
5. **Video and GIF previews** — a video or GIF message is a plain "Video" card.

## Non-goals

- Colour emoji (its own design, next: it replaces the text engine).
- Editing anything: no rename, no group admin actions, no mute, no leave group, no block.
- GIFs or videos playing inside the chat list of messages (they play in the media view only).
- Recovering the mention list of messages stored before this change (see §2).
- Showing a phone number WhatsApp does not reveal.

---

## 1. People store

**Why.** Wisp keeps names only in `chats.name`, so a group member without a direct chat has no
name, the name a person set for themself (push name) is dropped for group senders, and an internal
ID (`@lid`) whose phone number is unknown is shown bare.

**Data.** Schema version 2 adds, per account:

```sql
CREATE TABLE people (
  account_id  TEXT NOT NULL,
  jid         TEXT NOT NULL,      -- as WhatsApp addressed them: …@s.whatsapp.net, …@lid or …@g.us
  pn_jid      TEXT NOT NULL DEFAULT '',  -- phone-number JID when jid is a LID and it is known
  full_name   TEXT NOT NULL DEFAULT '',  -- address-book name
  push_name   TEXT NOT NULL DEFAULT '',  -- the name they set themselves
  avatar_id   TEXT NOT NULL DEFAULT '',  -- WhatsApp's picture ID; '' unknown, '-' has none
  avatar_ts   INTEGER NOT NULL DEFAULT 0, -- when the picture was last checked (unix s)
  PRIMARY KEY (account_id, jid)
);
ALTER TABLE messages ADD COLUMN mentions TEXT NOT NULL DEFAULT ''; -- space-separated JIDs
```

The migration only adds; existing rows keep working. Groups get a row too (for their picture).

**Sources.** A row is created or updated (only the fields the source knows) from: every incoming
message's sender and push name, in direct chats and groups; the address book (`AllContacts`); group
member lists when a group's details are loaded; LID-to-phone mappings whenever whatsmeow learns one;
history sync. Updates are batched with the message batch that carried them; no extra goroutine.

**Lookup.** `store.Person(accountID, jid)` returns the row for `jid`, or the row whose `pn_jid` is
`jid`, merged: a LID row and its phone-number row count as one person, and the address-book name of
either wins. Old messages are never rewritten; names are resolved when drawn. The app keeps the
people of the open account in memory as it does chat names today and refreshes them on change.

## 2. Names and mentions

**Name rule** — one pure function, used by the chat list, chat header, group sender line, mentions,
details view, notifications and whole-message copy:

1. address-book name;
2. else the push name, shown as `~Name`;
3. else the phone number, shown as `+<digits>`;
4. else `Unknown`.

An internal ID is never shown. Groups use their subject, else `Group`.

**Mentions.** `MentionedJID` from the message's context info is stored in `messages.mentions` for
text and captions. When a bubble's text is built, each `@<user part>` of a mentioned JID becomes
`@<name by the rule>` (for rule 2 without the `~`). The rewritten text is what is wrapped, selected
and copied. Mentions are drawn in the theme's accent colour.

Messages stored before this change have no mention list. For them, an `@` followed by 5–20 digits
is rewritten only when those digits are the user part of a known person; otherwise the text stays
as it is.

## 3. Pictures

**Where.** Every place that draws an avatar today (chat list, stories strip, chat header, account
rail, status viewer, settings account list) plus two new ones: the details view, and in group chats
a 28 px picture left of the first bubble of each run of messages from the same sender. Initials in
the coloured circle stay as the fallback and while a picture loads.

**Fetching.** When an avatar is drawn for a person whose picture is due, Wisp asks WhatsApp for
the small preview (`GetProfilePictureInfo`, preview, passing the known `avatar_id`), downloads it
through the configured proxy, scales it to 96 px, and saves it as
`<cache>\<account>\avatars\<sha256 of jid>.jpg`. One fetch runs at a time per account; requests for
rows scrolled away are dropped.

A picture is **due** (pure function) when it was never checked, when WhatsApp announced a change
(picture event → `avatar_ts = 0`), or when the last check is more than 7 days old. "No picture" and
"not allowed to see it" are remembered as `avatar_id = '-'` and retried by the same 7-day rule.
A failed fetch is retried the next time the row is drawn after 10 minutes, not in a loop.

**Memory.** Decoded pictures live in one LRU of 64 bitmaps shared by all views (about 2.4 MB at
most), disposed on eviction. No timer; nothing is fetched while the window is hidden or locked.

**Large picture.** The details view asks for the full-size picture while it is open, shows it at up
to 320 px, and drops it on close; it is not saved to disk.

**Clear Cache** also deletes `avatars\` and resets `avatar_ts`, so pictures come back on demand.

## 4. Details view

**Where.** Clicking the chat header, or Enter while it has focus (it becomes a tab stop), replaces
the chat view and input bar in the chat column with the details view — the same mechanism as the
media view, and a child composite, not a dialog. A back arrow or Esc returns to the chat at the
same scroll position. Wisp locking, switching chat or account, hiding or minimising the window all
close it.

**Contact.** Title `Contact info`. Large picture; name by the name rule; phone number as `+<digits>`
(line omitted when WhatsApp hides it); `About` with the about text (section omitted when empty).

**Group.** Title `Group info`. Large picture; subject; `Description` (omitted when empty);
`N members`; the member list — picture, name by the name rule, `Admin` on the right for admins —
sorted admins first, then by name. ↑ ↓ move through members; Enter or a click opens that member's
chat (as New chat does). The list draws only visible rows.

**Loading.** The view opens at once with what the people store already knows and shows
`Loading…` under it while it asks WhatsApp (`GetUserInfo` for about, `GetGroupInfo` for a group).
On failure: `Couldn't load the details.` with a `Retry` button. A reply that arrives after the view
closed or moved to another chat is dropped. Group member lists feed the people store.

## 5. Video and GIF previews

A video or GIF message whose stored metadata has a preview picture is drawn like a photo bubble,
with a play mark (line icon in a dark disc) in the centre; a GIF also shows a `GIF` badge in the
top-left corner. Without a preview it stays the card it is today, labelled `GIF` or `Video`. A
click or Enter opens the player as now. Nothing is downloaded or decoded for the preview beyond the
embedded picture, under the existing 1 MP cap and thumbnail LRU.

GIFs sent from Wisp carry a preview, width and height from the picked GIF's still image, so they
show the same way on both sides.

---

## Components

| File | Change |
|---|---|
| `internal/store/db.go`, `people.go` (new) | schema v2 migration; `UpsertPeople`, `Person`, `People`, avatar fields |
| `internal/store/messages.go` | `mentions` column read and written |
| `internal/whatsapp/client.go`, `manager.go` | collect people and mentions from messages; wrap `GetProfilePictureInfo`, `GetUserInfo`, `GetGroupInfo` details; picture event; GIF preview on send |
| `internal/media/cache.go` | `avatars\` folder: save, path, clear |
| `internal/ui/people.go` (new) | name rule, mention rewriting, `avatarDue` (pure, tested) |
| `internal/ui/avatars.go` (new) | shared picture LRU; `drawAvatar` draws a picture when there is one |
| `internal/ui/chatlayout.go`, `chatview.go` | group sender pictures, mention colour, video/GIF preview bubbles, header click and focus |
| `internal/ui/infoview.go` (new) | details view |
| `internal/app` | people in memory, avatar fetch queue, open/close details, lock and page rules |

## Error handling

- Any WhatsApp query failing leaves the initials / stored names in place; nothing blocks the UI.
- Logs name the operation and the error only — never a name, number, about text or message text.
- A corrupt or oversized downloaded picture is discarded and counts as a failed fetch.

## Testing

- Unit tests: name rule (all four steps, LID + phone merge), mention rewriting (stored list, legacy
  digits, unknown ID left alone, surrogate pairs around it), `avatarDue`, migration v1 → v2 on a
  copy of a v1 database in `t.TempDir()`, member sort, video preview layout choice.
- Views: a harness window on a scratch profile with seeded people and pictures, driven only by
  messages to its own window (the live-check rules of the previous plan apply unchanged).
- With the user's linked account, by the user: pictures appear and refresh, about text, group
  description and members, mentions in a real group, a received and a sent GIF.

## Risks

- **WhatsApp rate limits** on picture and user-info queries: one query at a time per account, only
  for visible rows, weekly refresh. If WhatsApp still throttles, fetches fail quietly and retry later.
- **Hidden phone numbers:** some group members have neither a visible number nor a push name until
  they write; they show as `Unknown`.
- **Group sender pictures** change bubble layout in groups (bubbles shift right by 36 px); the
  wrapping and selection code from the previous plan is reused, not changed.
