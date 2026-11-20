# Notification & Action Center Polish — Design

**Date:** 2026-11-19  
**Status:** In Review  
**Builds on:** `2026-11-19-windows-toast-quick-reply-design.md`  
**Binding constraints:** 0% idle CPU, no new external dependencies, no third-party runtimes, zero lag, full lock screen and privacy safety.

---

## 1. Goal

Deliver 5 core polish capabilities to the Windows notification experience:
1. **"Mark as Read" Quick Action Button**:
   - An action button on the toast banner (`[Mark as read]`) with `wisp://markread?acct=...&jid=...`.
   - Clicking it sends WhatsApp read receipts and clears unread status in Wisp and Windows Action Center without needing to focus or restore the window.
2. **Audio Cleanliness (`<audio silent="true"/>`)**:
   - When `n.Silent` is true (either notifications sound is disabled in Settings or a custom WAV sound is played), explicitly specify `<audio silent="true"/>` in Toast XML so Windows' default system chime does not fire.
3. **Photo / Media Previews in Banners**:
   - When receiving an image message (`Type == "image"`) whose file or thumbnail exists in cache, display a rich hero image (`<image placement="hero" src="..."/>`) in the toast banner.
4. **Conversation Tagging & Grouping (`Tag` & `Group`)**:
   - Tag toasts with `Tag = ChatJID` (or message ID) and `Group = AccountID` using WinRT `IToastNotification2` (`put_Tag`, `put_Group`).
   - Replaces older unread toasts from the same conversation in Action Center, preventing notification spam.
5. **Auto-Clear Action Center on Read**:
   - When a conversation is opened or marked as read in Wisp, remove that conversation's notifications from the Windows Action Center via `ToastNotificationHistory.Remove(tag, group, "Wisp.App")` or `RemoveGroup`.
