package ui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"

	"wa-go/internal/lock"
	"wa-go/internal/theme"
	"wa-go/internal/whatsapp"
)

// Keys of the store's settings table. The privacy ones are per account; the rest are app-global
// (account ""). Booleans are "true" or "false"; read them with Setting, which knows the defaults.
const (
	KeyLastSeen     = "privacy.last_seen"            // whatsmeow privacy value: all, contacts, contact_blacklist, none
	KeyReadReceipts = "privacy.read_receipts"        // all or none
	KeyProfilePhoto = "privacy.profile_photo"        // as last seen
	KeyDisappearing = "privacy.disappearing_default" // seconds: 0 (off), 86400, 604800 or 7776000

	KeyGhostTyping        = "stealth.ghost_typing"      // never send typing indicators
	KeyAppearOffline      = "stealth.appear_offline"    // always send presence unavailable
	KeyAutoDownloadImages = "media.autodownload_images" // fetch incoming images without a click
	KeyGiphyAPIKey        = "gif.giphy_api_key"         // "" uses the built-in key
	KeyProxy              = "net.proxy"                 // see whatsapp.ValidateProxy; "" is a direct connection
	KeyNotify             = "notify.enabled"            // toasts for new messages
	KeyNotifyPrivate      = "notify.private"            // toasts without sender or text
	KeyNotifySound        = "notify.sound"              // toasts with the notification sound
	KeyTheme              = "ui.theme"                  // system, dark or light
	KeyLockPIN            = "lock.pin"                  // the app-lock PIN's hash (see package lock); "" is lock off
)

// option is one value of a multiple-choice setting with its label.
type option struct{ label, value string }

var (
	visibilityOptions = []option{{"Everyone", "all"}, {"My contacts", "contacts"}, {"Contacts except…", "contact_blacklist"}, {"Nobody", "none"}}
	timerOptions      = []option{{"Off", "0"}, {"24 hours", "86400"}, {"7 days", "604800"}, {"90 days", "7776000"}}
	themeOptions      = []option{{"System default", "system"}, {"Dark", "dark"}, {"Light", "light"}}
)

// privacyKeys are the per-account settings, with the values WhatsApp accepts for each.
var privacyKeys = map[string][]option{
	KeyLastSeen: visibilityOptions, KeyProfilePhoto: visibilityOptions, KeyDisappearing: timerOptions,
	KeyReadReceipts: {{"On", "all"}, {"Off", "none"}},
}

// privacyTypes maps the keys WhatsApp keeps as privacy settings to their whatsmeow type.
var privacyTypes = map[string]types.PrivacySettingType{
	KeyLastSeen: types.PrivacySettingTypeLastSeen, KeyReadReceipts: types.PrivacySettingTypeReadReceipts,
	KeyProfilePhoto: types.PrivacySettingTypeProfile,
}

// settingNames are the settings' names as the dialog shows them.
var settingNames = map[string]string{
	KeyLastSeen: "Last seen", KeyReadReceipts: "Read receipts", KeyProfilePhoto: "Profile photo",
	KeyDisappearing: "Default message timer", KeyGhostTyping: "Ghost typing", KeyAppearOffline: "Appear offline",
	KeyAutoDownloadImages: "Auto-download images", KeyGiphyAPIKey: "GIPHY API key", KeyProxy: "Proxy",
	KeyNotify: "Notifications", KeyNotifyPrivate: "Private notifications", KeyNotifySound: "Notification sound",
	KeyTheme: "Theme", KeyLockPIN: "App lock",
}

// defaults are the values of settings never saved. Privacy ones stand in until WhatsApp's answer.
var defaults = map[string]string{
	KeyLastSeen: "all", KeyReadReceipts: "all", KeyProfilePhoto: "all", KeyDisappearing: "0",
	KeyGhostTyping: "false", KeyAppearOffline: "false", KeyAutoDownloadImages: "true", KeyGiphyAPIKey: "",
	KeyProxy: "", KeyNotify: "true", KeyNotifyPrivate: "false", KeyNotifySound: "true", KeyTheme: "system", KeyLockPIN: "",
}

// SettingsStore is the part of *store.DB the settings use.
type SettingsStore interface {
	GetSetting(accountID, key string) (value string, ok bool, err error)
	SetSetting(accountID, key, value string) error
}

// PrivacyClient is the part of *whatsapp.Manager the privacy settings use.
type PrivacyClient interface {
	SetPrivacySetting(ctx context.Context, accountID string, name types.PrivacySettingType, value types.PrivacySetting) (types.PrivacySettings, error)
	PrivacySettings(ctx context.Context, accountID string) (types.PrivacySettings, error)
	SetDefaultDisappearingTimer(ctx context.Context, accountID string, d time.Duration) error
}

// Setting returns accountID's value for key ("" for app-global keys), or its default when unset.
func Setting(s SettingsStore, accountID, key string) (string, error) {
	v, ok, err := s.GetSetting(accountID, key)
	if err != nil || !ok {
		return defaults[key], err
	}
	return v, nil
}

// ParseTheme reads a KeyTheme value; anything unknown is ThemeSystem.
func ParseTheme(v string) theme.ThemeMode {
	switch v {
	case "dark":
		return theme.ThemeDark
	case "light":
		return theme.ThemeLight
	}
	return theme.ThemeSystem
}

// errNotMirrored: WhatsApp took a privacy change but the settings table could not record it.
var errNotMirrored = errors.New("saved on WhatsApp, but not on this PC")

// settingsShared is what every Settings dialog shares: one worker, so a dialog opened while an
// earlier one's change is still being saved loads only after it; and the last change that failed
// after its dialog had closed, for the next dialog to report (UI thread only).
type settingsShared struct {
	jobs   serial
	missed string
}

// onWorker makes a settingsCtl.bg: work runs on jobs, one at a time in call order, and its done is
// handed to deliver (the UI thread's Synchronize), so dones arrive in that order too.
func onWorker(jobs *serial, deliver func(func())) func(work func() error, done func(error)) {
	return func(work func() error, done func(error)) {
		jobs.do(func() {
			err := work()
			deliver(func() { done(err) })
		})
	}
}

// settingsCtl is the Settings dialog without its window: it writes privacy settings to WhatsApp
// first and then to the settings table, and app-global settings to the table only.
type settingsCtl struct {
	store     SettingsStore
	wa        PrivacyClient
	bg        func(work func() error, done func(error)) // work off the UI thread, then done on it, in call order
	onTheme   func(theme.ThemeMode)                     // a theme was picked (before it is saved)
	onChanged func(key string)                          // an app-global setting was saved
	shared    *settingsShared
	closed    func() bool // the dialog has closed: a change failing now is kept for the next one
}

// finish wraps the done of a change to key (of accountID, "" for app-global): a failure arriving
// after the dialog closed is logged and kept for the next dialog, which takeMissed hands it to.
func (c *settingsCtl) finish(accountID, key string, done func(error)) func(error) {
	return func(err error) {
		if err != nil && c.closed() {
			who := ""
			if accountID != "" {
				who = "+" + accountID + ": "
			}
			c.shared.missed = who + "a change to " + settingNames[key] + " made before Settings closed failed: " + err.Error()
			logErr("settings", errors.New(c.shared.missed))
		}
		done(err)
	}
}

// takeMissed returns, once, the last change that failed after its dialog had closed ("" for none).
func (c *settingsCtl) takeMissed() string {
	m := c.shared.missed
	c.shared.missed = ""
	return m
}

// values are the shown values of the app-global settings and, unless accountID is "", of its
// privacy settings.
func (c *settingsCtl) values(accountID string) (map[string]string, error) {
	out := make(map[string]string, len(defaults))
	var errs []error
	for key := range defaults {
		id := ""
		if _, ok := privacyKeys[key]; ok {
			if accountID == "" {
				continue
			}
			id = accountID
		}
		v, err := Setting(c.store, id, key)
		out[key] = v
		errs = append(errs, err)
	}
	return out, errors.Join(errs...)
}

// setPrivacy changes accountID's privacy setting key on WhatsApp and, once WhatsApp has taken it,
// in the settings table. On failure the table keeps the old value. done gets the outcome; an error
// wrapping errNotMirrored means WhatsApp has the new value but the table does not.
func (c *settingsCtl) setPrivacy(accountID, key, value string, done func(error)) {
	opts, ok := privacyKeys[key]
	if accountID == "" || !ok || !slices.ContainsFunc(opts, func(o option) bool { return o.value == value }) {
		done(fmt.Errorf("ui: %q is not a value of %s", value, key))
		return
	}
	c.bg(func() error {
		ctx := context.Background() // not cancelled with the dialog: WhatsApp's answer must reach the table
		var err error
		if key == KeyDisappearing {
			secs, _ := strconv.Atoi(value) // one of timerOptions
			err = c.wa.SetDefaultDisappearingTimer(ctx, accountID, time.Duration(secs)*time.Second)
		} else {
			_, err = c.wa.SetPrivacySetting(ctx, accountID, privacyTypes[key], types.PrivacySetting(value))
		}
		if err != nil {
			return err
		}
		if err := c.store.SetSetting(accountID, key, value); err != nil {
			return fmt.Errorf("%w: %w", errNotMirrored, err)
		}
		return nil
	}, c.finish(accountID, key, done))
}

// refresh fetches accountID's privacy settings from WhatsApp into the settings table; done gets the
// values to show (as values does), or the error with nil values.
func (c *settingsCtl) refresh(accountID string, done func(map[string]string, error)) {
	var vals map[string]string
	c.bg(func() error {
		ps, err := c.wa.PrivacySettings(context.Background(), accountID)
		if err != nil {
			return err
		}
		for key, v := range map[string]types.PrivacySetting{KeyLastSeen: ps.LastSeen, KeyReadReceipts: ps.ReadReceipts, KeyProfilePhoto: ps.Profile} {
			if v == types.PrivacySettingUndefined {
				continue // WhatsApp did not say
			}
			if err := c.store.SetSetting(accountID, key, string(v)); err != nil {
				return err
			}
		}
		vals, err = c.values(accountID)
		return err
	}, func(err error) {
		if err != nil {
			vals = nil
		}
		done(vals, err)
	})
}

// setGlobal saves an app-global setting; never WhatsApp. A proxy address is trimmed and must pass
// whatsapp.ValidateProxy. A theme fires onTheme at once; every saved setting then fires onChanged.
func (c *settingsCtl) setGlobal(key, value string, done func(error)) {
	switch key {
	case KeyProxy:
		value = strings.TrimSpace(value)
		if err := whatsapp.ValidateProxy(value); err != nil {
			done(err)
			return
		}
	case KeyGiphyAPIKey:
		value = strings.TrimSpace(value)
	case KeyTheme:
		c.onTheme(ParseTheme(value))
	}
	c.bg(func() error { return c.store.SetSetting("", key, value) }, c.finish("", key, func(err error) {
		if err == nil {
			c.onChanged(key)
		}
		done(err)
	}))
}

// pinMode is what the PIN dialog is for.
type pinMode int

const (
	pinSet    pinMode = iota // values: new, confirm
	pinChange                // values: current, new, confirm
	pinOff                   // values: current
)

// pinUpdate applies the PIN dialog's values to the stored hash. It returns the hash to store ("" is
// lock off) and the message to show; ok is false when nothing should be stored.
func pinUpdate(stored string, mode pinMode, values []string) (newStored, msg string, ok bool) {
	current, next := "", ""
	switch mode {
	case pinSet:
		next = values[0]
		if values[1] != next {
			return "", "The PINs don't match.", false
		}
	case pinChange:
		current, next = values[0], values[1]
		if values[2] != next {
			return "", "The PINs don't match.", false
		}
	case pinOff:
		current = values[0]
	}
	newStored, err := lock.Update(stored, current, next)
	switch {
	case errors.Is(err, lock.ErrWrongPIN):
		return "", "That PIN isn't right.", false
	case errors.Is(err, lock.ErrPINFormat):
		return "", "Use 4 to 8 digits.", false
	case errors.Is(err, lock.ErrStored):
		return "", "The saved PIN is damaged. Use Reset Wisp on the lock page.", false
	case err != nil:
		return "", "Couldn't set the PIN.", false
	}
	return newStored, map[pinMode]string{pinSet: "App lock is on.", pinChange: "PIN changed.", pinOff: "App lock is off."}[mode], true
}
