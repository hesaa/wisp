package notify

import (
	"strings"
	"testing"
)

func TestBuildToastXML_Basic(t *testing.T) {
	n := Notification{
		Title:       "Alice",
		Body:        "Hello there!",
		AvatarPath:  `C:\cache\avatar.jpg`,
		UnreadCount: 4,
		AccountID:   "62812345",
		ChatJID:     "alice@s.whatsapp.net",
		CanReply:    true,
	}
	xml := BuildToastXML(n)
	if !strings.Contains(xml, `<image placement="appLogoOverride" hint-crop="circle" src="C:\cache\avatar.jpg"/>`) {
		t.Errorf("missing circular avatar element: %s", xml)
	}
	if !strings.Contains(xml, `<text placement="attribution">4 unread messages</text>`) {
		t.Errorf("missing unread attribution: %s", xml)
	}
	if !strings.Contains(xml, `wisp://open?acct=62812345&amp;jid=alice%40s.whatsapp.net`) {
		t.Errorf("missing launch uri: %s", xml)
	}
	if !strings.Contains(xml, `<input id="replyText" type="text" placeHolderContent="Type a reply"/>`) {
		t.Errorf("missing reply input: %s", xml)
	}
	if !strings.Contains(xml, `arguments="wisp://reply?acct=62812345&amp;jid=alice%40s.whatsapp.net" hint-inputId="replyText"`) {
		t.Errorf("missing send action with hint-inputId: %s", xml)
	}
	if !strings.Contains(xml, `action content="Mark as read" activationType="protocol" arguments="wisp://markread?acct=62812345&amp;jid=alice%40s.whatsapp.net"`) {
		t.Errorf("missing mark as read action: %s", xml)
	}
}

func TestBuildToastXML_AudioSilent(t *testing.T) {
	nSilent := Notification{Title: "A", Body: "B", Silent: true}
	xmlSilent := BuildToastXML(nSilent)
	if !strings.Contains(xmlSilent, `<audio silent="true"/>`) {
		t.Errorf("expected <audio silent=\"true\"/>: %s", xmlSilent)
	}

	nSound := Notification{Title: "A", Body: "B", Silent: false}
	xmlSound := BuildToastXML(nSound)
	if strings.Contains(xmlSound, `<audio silent="true"/>`) {
		t.Errorf("expected sound not to be silent: %s", xmlSound)
	}
}

func TestBuildToastXML_HeroImage(t *testing.T) {
	n := Notification{
		Title:      "A",
		Body:       "B",
		ImagePath:  `C:\cache\photo.jpg`,
		AccountID:  "acc1",
		ChatJID:    "chat1",
		CanReply:   true,
	}
	xml := BuildToastXML(n)
	if !strings.Contains(xml, `<image placement="hero" src="C:\cache\photo.jpg"/>`) {
		t.Errorf("missing hero image: %s", xml)
	}

	nPrivate := Notification{
		Title:      "A",
		Body:       "B",
		ImagePath:  `C:\cache\photo.jpg`,
		CanReply:   false, // private mode
	}
	xmlPrivate := BuildToastXML(nPrivate)
	if strings.Contains(xmlPrivate, `<image`) {
		t.Errorf("private mode must not contain hero image: %s", xmlPrivate)
	}
}

func TestBuildToastXML_PrivateMode(t *testing.T) {
	n := Notification{
		Title:       "WhatsApp",
		Body:        "New message",
		AvatarPath:  "",
		UnreadCount: 0,
		AccountID:   "62812345",
		ChatJID:     "alice@s.whatsapp.net",
		CanReply:    false, // private mode or locked
	}
	xml := BuildToastXML(n)
	if strings.Contains(xml, `<image`) {
		t.Errorf("private mode must not contain image: %s", xml)
	}
	if strings.Contains(xml, `<actions>`) {
		t.Errorf("private mode must not contain quick reply actions: %s", xml)
	}
	if strings.Contains(xml, `attribution`) {
		t.Errorf("private mode must not contain attribution: %s", xml)
	}
}

func TestBuildToastXML_Escaping(t *testing.T) {
	n := Notification{
		Title:     `Tom & "Jerry" <friends>`,
		Body:      `It's a "test" & demo!`,
		AccountID: "123",
		ChatJID:   "group@g.us",
		CanReply:  false,
	}
	xml := BuildToastXML(n)
	if !strings.Contains(xml, `Tom &amp; &quot;Jerry&quot; &lt;friends&gt;`) {
		t.Errorf("title not escaped: %s", xml)
	}
	if !strings.Contains(xml, `It&apos;s a &quot;test&quot; &amp; demo!`) {
		t.Errorf("body not escaped: %s", xml)
	}
}

func TestDispatcher_WinRT(t *testing.T) {
	fb := &fakeBalloon{}
	d := newDispatcher(fb)
	var gotXML, gotAppID, gotTag, gotGroup string
	d.showToast = func(xml, appID, tag, group string) error {
		gotXML = xml
		gotAppID = appID
		gotTag = tag
		gotGroup = group
		return nil
	}
	in := Input{
		AccountID:  "acc1",
		ChatJID:    "chat1@s.whatsapp.net",
		SenderName: "Bob",
		ChatName:   "Bob",
		Type:       "text",
		Body:       "hello",
		AvatarPath: "C:\\avatar.jpg",
	}
	if err := d.Notify(in, on); err != nil {
		t.Fatal(err)
	}
	if fb.calls != 0 {
		t.Errorf("balloon should not be called when winrt succeeds; calls = %d", fb.calls)
	}
	if gotAppID != "Wisp.App" {
		t.Errorf("got appID %q, want Wisp.App", gotAppID)
	}
	if gotTag != "chat1@s.whatsapp.net" {
		t.Errorf("got tag %q, want chat1@s.whatsapp.net", gotTag)
	}
	if gotGroup != "acc1" {
		t.Errorf("got group %q, want acc1", gotGroup)
	}
	if !strings.Contains(gotXML, "C:\\avatar.jpg") {
		t.Errorf("expected avatar path in xml: %s", gotXML)
	}
	acc, chat, ok := d.Last()
	if !ok || acc != "acc1" || chat != "chat1@s.whatsapp.net" {
		t.Errorf("d.Last() = (%q, %q, %v)", acc, chat, ok)
	}
}
