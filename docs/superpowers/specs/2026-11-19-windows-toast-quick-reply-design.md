# Windows Toast Notifications with Avatar, Quick Reply & Action Center Settings — Design

**Date:** 2026-11-19  
**Status:** In Review  
**Builds on:** `2026-11-10-whatsapp-client-design.md`, `2026-11-11-wisp-front-page-media-copy-design.md`  
**Binding constraints:** 0% idle CPU, no new external dependencies, no third-party runtimes, zero-delay toast popups (< 20 ms), privacy mode & app lock safety.

---

## 1. Goal

Deliver native Windows 10/11 rich toast notifications matching modern messaging clients (as shown in reference screenshot):
1. **Visual Toast Banner**:
   - Circular sender profile picture / avatar (`<image placement="appLogoOverride" hint-crop="circle"/>`).
   - Sender and chat names (`<text hint-maxLines="1">...</text>`).
   - Message text preview or media description.
   - Attribution line showing unread messages (e.g. `4 unread messages`).
2. **Interactive Inline Quick Reply**:
   - An inline text box: `<input id="replyText" type="text" placeHolderContent="Type a reply"/>`.
   - A "Send" action button: `<action content="Send" activationType="protocol" arguments="wisp://reply?..." hint-inputId="replyText"/>`.
3. **Protocol Activation & Single-Instance IPC**:
   - Register `wisp://` protocol in Windows user registry (`HKCU\Software\Classes\wisp`).
   - Clicking the toast body triggers `wisp://open?acct=...&jid=...`, which restores Wisp and brings up that specific chat.
   - Clicking "Send" on the quick reply triggers `wisp://reply?acct=...&jid=...&replyText=...`, which forwards the reply to the running Wisp process via Win32 `WM_COPYDATA` and sends the text over WhatsApp in the background.
4. **Windows Settings Integration**:
   - Registered under AppUserModelID `Wisp.App` with matching Start Menu shortcut.
   - Shows in Windows 10 Settings > System > Notifications & actions as an individual entry with "Banners, Sounds" configuration toggles.
5. **Privacy & Lock Screen Safety**:
   - When private mode (`notify.private`) is enabled or the app is locked with PIN, message body is hidden (`New message`), avatar is omitted, and the quick-reply input box is omitted to prevent unauthorized sending.

---

## 2. Architecture & Tech Stack

- **WinRT Toast Dispatcher**: Direct COM invocation of `Windows.UI.Notifications.ToastNotificationManager` and `Windows.Data.Xml.Dom.XmlDocument` via `combase.dll` (`RoInitialize`, `RoActivateInstance`, `RoGetActivationFactory`). Eliminates PowerShell child-process overhead.
- **Protocol & IPC**:
  - `HKCU\Software\Classes\wisp` registered at startup.
  - Inter-process communication uses Win32 `WM_COPYDATA` to send protocol URIs from child/secondary instances to `Wisp`'s main window handle without network sockets.
- **Avatars**: Reuses existing cached JPEG avatar paths from `internal/media/Cache.AvatarPath(accountID, jid)`.
