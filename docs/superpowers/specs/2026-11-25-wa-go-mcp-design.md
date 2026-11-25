# `wa-go-mcp` — Built-in MCP Support for Wisp

**Date:** 2026-11-25
**Status:** Ready for user approval

## Intent & Why

Allow an AI assistant to look up WhatsApp contacts and groups, read recent chat history for context, and send text messages or media files (for example: *"write a handoff and report, send the message to Joe and Group `Foo` on WhatsApp"*).

Because the user keeps Wisp (`wisp.exe`) running on their PC alongside WhatsApp on their phone, MCP support is built into Wisp rather than running as a separate standalone WhatsApp client. This avoids using an extra Linked Device slot, avoids fighting over `session.db`, and keeps Wisp's UI, local database, and phone in sync.

## Goals

- **Built into Wisp:** Powered by the running `wisp.exe` process, using its live `whatsmeow` connections and `wisp.db` store.
- **Independent MCP toggle in Settings:** Users can turn MCP access on or off directly in Wisp Settings, completely separate from Wisp's UI App lock (PIN). Locking Wisp's window does not turn off MCP.
- **Read & search:** Search contacts, groups, and chats; read recent message history from a chat.
- **Send messages & media:** Send text messages and media files (documents, images, etc.) to a contact or group.
- **Safe by default:** Always pause for user confirmation before sending any message or file, and surface ambiguous recipient matches instead of guessing.

## Non-Goals & Anti-Goals

- **No standalone mode:** `wa-go-mcp` does not run its own independent WhatsApp session when Wisp is closed.
- **No destructive or admin actions:** MCP must not delete messages or chats, change Wisp settings, or log out WhatsApp accounts.
- **No silent sends:** Never send a message or file without explicit user confirmation, even when the recipient name is unambiguous.
- **No blind guessing:** If a recipient query (e.g. `"Joe"`) matches multiple contacts or groups, return the candidates rather than picking one automatically.

## User-Confirmed Decisions

1. **Built into Wisp:** Integrated into Wisp with a Settings option, not a standalone Linked Device app.
2. **Separate MCP on/off toggle from Wisp App lock:** Wisp's App lock (PIN to hide the UI) does *not* turn off or block MCP. MCP has its own simple on/off toggle in Wisp Settings.
3. **In-chat confirmation & disambiguation:** Send confirmation happens in the AI chat (two-step preview/prepare then confirm). Before sending, the AI shows the target recipient, account, and message/file and pauses for the user's okay. If a name like `"Joe"` matches multiple contacts or groups, the tool returns the candidates so the AI asks the user which `"Joe"` they meant. This also works cleanly when Wisp is minimized or App-locked.

## Design Calls (Defaults)

These are sensible defaults chosen for v1; the user can override any of them:

- **Local bridge + `stdio` compatibility:** `wisp.exe` hosts a local authenticated bridge when MCP is enabled, paired with a lightweight `stdio` MCP entrypoint (or Streamable HTTP endpoint) so standard AI clients (Antigravity, Claude Desktop, Cursor) can connect easily. Wisp Settings shows the status and lets the user copy the client config snippet.
- **Two-step send flow:** Sending uses a two-step MCP flow (`prepare_send` -> `confirm_send` with a short-lived token/ID) rather than a single direct send tool, so the AI assistant previews the recipient, account, and content/file to the user in chat before executing the send.
- **Multi-account support:** Read/search tools can query across linked accounts (tagging each result with its account) or filter to a specific account. Send tools explicitly identify which account will send.
- **Media scope:** Sending supports local file paths (documents, images, etc.) using Wisp's existing `whatsmeow` media upload/send paths. Reading history returns text content and metadata (type, filename, caption, sender, timestamp) for media messages.
- **Disabled / offline errors:** If Wisp is not running, or the MCP toggle is turned off in Settings, MCP tool calls return a clear, actionable message telling the user to open Wisp or turn MCP on in Settings.

## Left to the Builder

- Choosing the Go MCP library/protocol implementation and local IPC mechanism (e.g., localhost HTTP with a generated token vs. Windows named pipe + `cmd/wisp-mcp` stdio shim).
- Designing the exact MCP tool names, schemas, and error payloads.
- Designing the Settings UI section layout in `internal/ui` to match existing Wisp Settings pages.
- Implementing concurrency-safe access between the MCP bridge, `internal/whatsapp` manager, and `internal/store`.
