package notify

import (
	"fmt"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	return s
}

// BuildToastXML formats a Windows 10/11 adaptive toast notification XML.
func BuildToastXML(n Notification) string {
	var b strings.Builder
	launchURI := ""
	if n.AccountID != "" && n.ChatJID != "" {
		launchURI = fmt.Sprintf("wisp://open?acct=%s&amp;jid=%s",
			url.QueryEscape(n.AccountID), url.QueryEscape(n.ChatJID))
	}

	if launchURI != "" {
		b.WriteString(fmt.Sprintf(`<toast launch="%s" activationType="protocol">`, launchURI))
	} else {
		b.WriteString(`<toast>`)
	}

	b.WriteString(`<visual><binding template="ToastGeneric">`)
	b.WriteString(fmt.Sprintf(`<text hint-maxLines="1">%s</text>`, xmlEscape(n.Title)))
	b.WriteString(fmt.Sprintf(`<text>%s</text>`, xmlEscape(n.Body)))

	if n.UnreadCount > 1 {
		b.WriteString(fmt.Sprintf(`<text placement="attribution">%d unread messages</text>`, n.UnreadCount))
	}
	if n.AvatarPath != "" {
		b.WriteString(fmt.Sprintf(`<image placement="appLogoOverride" hint-crop="circle" src="%s"/>`, xmlEscape(n.AvatarPath)))
	}
	if n.CanReply && n.ImagePath != "" {
		b.WriteString(fmt.Sprintf(`<image placement="hero" src="%s"/>`, xmlEscape(n.ImagePath)))
	}
	b.WriteString(`</binding></visual>`)

	if n.Silent {
		b.WriteString(`<audio silent="true"/>`)
	}

	if n.CanReply && n.AccountID != "" && n.ChatJID != "" {
		replyArgs := fmt.Sprintf("wisp://reply?acct=%s&amp;jid=%s",
			url.QueryEscape(n.AccountID), url.QueryEscape(n.ChatJID))
		markReadArgs := fmt.Sprintf("wisp://markread?acct=%s&amp;jid=%s",
			url.QueryEscape(n.AccountID), url.QueryEscape(n.ChatJID))
		b.WriteString(`<actions>`)
		b.WriteString(`<input id="replyText" type="text" placeHolderContent="Type a reply"/>`)
		b.WriteString(fmt.Sprintf(`<action content="Send" activationType="protocol" arguments="%s" hint-inputId="replyText"/>`, replyArgs))
		b.WriteString(fmt.Sprintf(`<action content="Mark as read" activationType="protocol" arguments="%s"/>`, markReadArgs))
		b.WriteString(`</actions>`)
	}

	b.WriteString(`</toast>`)
	return b.String()
}

var (
	combase                 = windows.NewLazySystemDLL("combase.dll")
	pRoInitialize           = combase.NewProc("RoInitialize")
	pRoActivateInstance     = combase.NewProc("RoActivateInstance")
	pRoGetActivationFactory = combase.NewProc("RoGetActivationFactory")
	pWindowsCreateString    = combase.NewProc("WindowsCreateString")
	pWindowsDeleteString    = combase.NewProc("WindowsDeleteString")

	roOnce sync.Once
)

func initWinRT() {
	roOnce.Do(func() {
		// RO_INIT_MULTITHREADED = 1
		pRoInitialize.Call(1)
	})
}

type hstring uintptr

func createHString(s string) (hstring, error) {
	u16, err := windows.UTF16FromString(s)
	if err != nil {
		return 0, err
	}
	var hs hstring
	r, _, _ := pWindowsCreateString.Call(
		uintptr(unsafe.Pointer(&u16[0])),
		uintptr(len(u16)-1),
		uintptr(unsafe.Pointer(&hs)),
	)
	if r != 0 {
		return 0, syscall.Errno(r)
	}
	return hs, nil
}

func deleteHString(hs hstring) {
	pWindowsDeleteString.Call(uintptr(hs))
}

var (
	xmlDocIOIID     = windows.GUID{Data1: 0x6cd0e74e, Data2: 0xee65, Data3: 0x4489, Data4: [8]byte{0x9e, 0xbf, 0xca, 0x43, 0xe8, 0x7b, 0xa6, 0x37}}
	toastFactoryIID = windows.GUID{Data1: 0x04124b20, Data2: 0x82c6, Data3: 0x4229, Data4: [8]byte{0xb1, 0x09, 0xfd, 0x9e, 0xd4, 0x66, 0x2b, 0x53}}
	mgrStaticsIID   = windows.GUID{Data1: 0x50ac103f, Data2: 0xd235, Data3: 0x4598, Data4: [8]byte{0xbb, 0xef, 0x98, 0xfe, 0x4d, 0x1a, 0x3a, 0xd4}}
	toast2IID       = windows.GUID{Data1: 0x9dfb9fd1, Data2: 0x143a, Data3: 0x490e, Data4: [8]byte{0x90, 0xbf, 0xb9, 0xfb, 0xa7, 0x13, 0x2d, 0xe7}}
	mgrStatics2IID  = windows.GUID{Data1: 0x7ab93c52, Data2: 0x0e48, Data3: 0x4750, Data4: [8]byte{0xba, 0x9d, 0x1a, 0x41, 0x13, 0x98, 0x18, 0x47}}
)

// ShowWinRTToast dispatches an adaptive toast notification using Windows Runtime COM APIs.
func ShowWinRTToast(xmlContent string, appID string, tag, group string) error {
	initWinRT()

	xmlDocStr, err := createHString("Windows.Data.Xml.Dom.XmlDocument")
	if err != nil {
		return err
	}
	defer deleteHString(xmlDocStr)

	var xmlDoc uintptr
	r, _, _ := pRoActivateInstance.Call(uintptr(xmlDocStr), uintptr(unsafe.Pointer(&xmlDoc)))
	if r != 0 {
		return syscall.Errno(r)
	}
	vtableDoc := *(**[10]uintptr)(unsafe.Pointer(xmlDoc))
	defer syscall.SyscallN(vtableDoc[2], xmlDoc) // Release

	var xmlDocIO uintptr
	r, _, _ = syscall.SyscallN(vtableDoc[0], xmlDoc, uintptr(unsafe.Pointer(&xmlDocIOIID)), uintptr(unsafe.Pointer(&xmlDocIO)))
	if r != 0 {
		return syscall.Errno(r)
	}
	vtableDocIO := *(**[10]uintptr)(unsafe.Pointer(xmlDocIO))
	defer syscall.SyscallN(vtableDocIO[2], xmlDocIO) // Release

	xmlHS, err := createHString(xmlContent)
	if err != nil {
		return err
	}
	defer deleteHString(xmlHS)

	// LoadXml is method 6 on IXmlDocumentIO
	r, _, _ = syscall.SyscallN(vtableDocIO[6], xmlDocIO, uintptr(xmlHS))
	if r != 0 {
		return syscall.Errno(r)
	}

	toastFactoryStr, err := createHString("Windows.UI.Notifications.ToastNotification")
	if err != nil {
		return err
	}
	defer deleteHString(toastFactoryStr)

	var toastFactory uintptr
	r, _, _ = pRoGetActivationFactory.Call(uintptr(toastFactoryStr), uintptr(unsafe.Pointer(&toastFactoryIID)), uintptr(unsafe.Pointer(&toastFactory)))
	if r != 0 {
		return syscall.Errno(r)
	}
	vtableTF := *(**[10]uintptr)(unsafe.Pointer(toastFactory))
	defer syscall.SyscallN(vtableTF[2], toastFactory) // Release

	var toast uintptr
	// CreateToastNotification is method 6 on IToastNotificationFactory
	r, _, _ = syscall.SyscallN(vtableTF[6], toastFactory, xmlDoc, uintptr(unsafe.Pointer(&toast)))
	if r != 0 {
		return syscall.Errno(r)
	}
	vtableToast := *(**[10]uintptr)(unsafe.Pointer(toast))
	defer syscall.SyscallN(vtableToast[2], toast) // Release

	if tag != "" || group != "" {
		var toast2 uintptr
		r2, _, _ := syscall.SyscallN(vtableToast[0], toast, uintptr(unsafe.Pointer(&toast2IID)), uintptr(unsafe.Pointer(&toast2)))
		if r2 == 0 && toast2 != 0 {
			vtableT2 := *(**[12]uintptr)(unsafe.Pointer(toast2))
			if tag != "" {
				if tagHS, err := createHString(tag); err == nil {
					syscall.SyscallN(vtableT2[6], toast2, uintptr(tagHS))
					deleteHString(tagHS)
				}
			}
			if group != "" {
				if groupHS, err := createHString(group); err == nil {
					syscall.SyscallN(vtableT2[8], toast2, uintptr(groupHS))
					deleteHString(groupHS)
				}
			}
			syscall.SyscallN(vtableT2[2], toast2) // Release
		}
	}

	mgrStr, err := createHString("Windows.UI.Notifications.ToastNotificationManager")
	if err != nil {
		return err
	}
	defer deleteHString(mgrStr)

	var mgr uintptr
	r, _, _ = pRoGetActivationFactory.Call(uintptr(mgrStr), uintptr(unsafe.Pointer(&mgrStaticsIID)), uintptr(unsafe.Pointer(&mgr)))
	if r != 0 {
		return syscall.Errno(r)
	}
	vtableMgr := *(**[10]uintptr)(unsafe.Pointer(mgr))
	defer syscall.SyscallN(vtableMgr[2], mgr) // Release

	appIDHS, err := createHString(appID)
	if err != nil {
		return err
	}
	defer deleteHString(appIDHS)

	var notifier uintptr
	// CreateToastNotifierWithId is method 7 on IToastNotificationManagerStatics
	r, _, _ = syscall.SyscallN(vtableMgr[7], mgr, uintptr(appIDHS), uintptr(unsafe.Pointer(&notifier)))
	if r != 0 {
		return syscall.Errno(r)
	}
	vtableNotifier := *(**[10]uintptr)(unsafe.Pointer(notifier))
	defer syscall.SyscallN(vtableNotifier[2], notifier) // Release

	// Show is method 6 on IToastNotifier
	r, _, _ = syscall.SyscallN(vtableNotifier[6], notifier, toast)
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

// ClearChatToasts removes notifications for tag (chatJID) and group (accountID) from Action Center.
func ClearChatToasts(appID, tag, group string) error {
	initWinRT()

	mgrStr, err := createHString("Windows.UI.Notifications.ToastNotificationManager")
	if err != nil {
		return err
	}
	defer deleteHString(mgrStr)

	var mgrStatics2 uintptr
	r, _, _ := pRoGetActivationFactory.Call(uintptr(mgrStr), uintptr(unsafe.Pointer(&mgrStatics2IID)), uintptr(unsafe.Pointer(&mgrStatics2)))
	if r != 0 {
		return syscall.Errno(r)
	}
	vtableMgr2 := *(**[10]uintptr)(unsafe.Pointer(mgrStatics2))
	defer syscall.SyscallN(vtableMgr2[2], mgrStatics2) // Release

	var history uintptr
	// get_History is method 6 on IToastNotificationManagerStatics2
	r, _, _ = syscall.SyscallN(vtableMgr2[6], mgrStatics2, uintptr(unsafe.Pointer(&history)))
	if r != 0 {
		return syscall.Errno(r)
	}
	vtableHistory := *(**[15]uintptr)(unsafe.Pointer(history))
	defer syscall.SyscallN(vtableHistory[2], history) // Release

	appIDHS, err := createHString(appID)
	if err != nil {
		return err
	}
	defer deleteHString(appIDHS)

	if tag != "" && group != "" {
		tagHS, err := createHString(tag)
		if err != nil {
			return err
		}
		defer deleteHString(tagHS)
		groupHS, err := createHString(group)
		if err != nil {
			return err
		}
		defer deleteHString(groupHS)
		// Method 8 is Remove(tag, group, appId)
		syscall.SyscallN(vtableHistory[8], history, uintptr(tagHS), uintptr(groupHS), uintptr(appIDHS))
	} else if group != "" {
		groupHS, err := createHString(group)
		if err != nil {
			return err
		}
		defer deleteHString(groupHS)
		// Method 7 is RemoveGroup(group, appId)
		syscall.SyscallN(vtableHistory[7], history, uintptr(groupHS), uintptr(appIDHS))
	} else {
		// Method 12 is Clear(appId)
		syscall.SyscallN(vtableHistory[12], history, uintptr(appIDHS))
	}
	return nil
}
