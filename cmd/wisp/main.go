// Command wisp builds Wisp, a native Windows WhatsApp client for several accounts at once.
//
// Data lives in %LOCALAPPDATA%\Wisp: wisp.db (messages and settings), session.db (whatsmeow's
// paired devices), cache\ (media) and wisp.log.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"unsafe"

	"github.com/lxn/walk"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"wa-go/internal/app"
	"wa-go/internal/media"
	"wa-go/internal/store"
	"wa-go/internal/whatsapp"
)

// appUserModelID registers Wisp with the Windows shell so toasts and tray balloons display.
const appUserModelID = "Wisp.App"

var (
	shell32                                 = windows.NewLazySystemDLL("shell32.dll")
	pSetCurrentProcessExplicitAppUserModelID = shell32.NewProc("SetCurrentProcessExplicitAppUserModelID")
)

func setAppID(id string) error {
	p, err := windows.UTF16PtrFromString(id)
	if err != nil {
		return err
	}
	r, _, _ := pSetCurrentProcessExplicitAppUserModelID.Call(uintptr(unsafe.Pointer(p)))
	if r != 0 {
		return windows.Errno(r)
	}
	return nil
}

// ensureShortcut creates a Start Menu shortcut stamped with appUserModelID if missing,
// which Windows 10/11 requires to display desktop toast notifications.
func ensureShortcut() {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return
	}
	lnkPath := filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Wisp.lnk")
	if _, err := os.Stat(lnkPath); err == nil {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	script := fmt.Sprintf(`
$w = New-Object -ComObject WScript.Shell
$s = $w.CreateShortcut('%s')
$s.TargetPath = '%s'
$s.Description = 'Wisp'
$s.Save()
$code = @"
using System;
using System.Runtime.InteropServices;
using System.Runtime.InteropServices.ComTypes;
public static class Helper {
    [ComImport, Guid("00021401-0000-0000-C000-000000000046")] public class ShellLink {}
    [ComImport, InterfaceType(ComInterfaceType.InterfaceIsIUnknown), Guid("886d8eeb-8cf2-4446-8d02-cdba1dbdcf99")]
    public interface IPropertyStore {
        void GetCount(out uint c); void GetAt(uint i, out PROPERTYKEY k); void GetValue(ref PROPERTYKEY k, ref object v);
        void SetValue(ref PROPERTYKEY k, ref object v); void Commit();
    }
    [StructLayout(LayoutKind.Sequential, Pack = 4)] public struct PROPERTYKEY { public Guid f; public uint p; }
    public static void Set(string path, string id) {
        ShellLink link = new ShellLink();
        ((IPersistFile)link).Load(path, 0);
        IPropertyStore store = (IPropertyStore)link;
        PROPERTYKEY key = new PROPERTYKEY { f = new Guid("9F4C2855-9F79-4B39-A8D0-E1D42DE1D5F3"), p = 5 };
        object val = id;
        store.SetValue(ref key, ref val);
        store.Commit();
        ((IPersistFile)link).Save(path, true);
    }
}
"@
Add-Type -TypeDefinition $code -ErrorAction SilentlyContinue
[Helper]::Set('%s', '%s')
`, lnkPath, exe, lnkPath, appUserModelID)
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script)
	_ = cmd.Run()
}

// ensureProtocol registers the wisp:// custom protocol scheme in HKCU\Software\Classes\wisp
// so toast notification clicks and inline quick replies can invoke the running app.
func ensureProtocol() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Classes\wisp`, registry.ALL_ACCESS)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.SetStringValue("", "URL:Wisp Protocol")
	_ = k.SetStringValue("URL Protocol", "")

	cmdKey, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Classes\wisp\shell\open\command`, registry.ALL_ACCESS)
	if err != nil {
		return
	}
	defer cmdKey.Close()
	_ = cmdKey.SetStringValue("", fmt.Sprintf(`"%s" "%%1"`, exe))
}

type copyDataStruct struct {
	dwData uintptr
	cbData uint32
	lpData uintptr
}

const wmCopyData = 0x004A

var (
	user32              = windows.NewLazySystemDLL("user32.dll")
	findWindowW         = user32.NewProc("FindWindowW")
	sendMessageTimeoutW = user32.NewProc("SendMessageTimeoutW")
)

// forwardToRunningInstance sends an argument string to Wisp's main window via WM_COPYDATA.
func forwardToRunningInstance(arg string) {
	title, _ := windows.UTF16PtrFromString("Wisp")
	hwnd, _, _ := findWindowW.Call(0, uintptr(unsafe.Pointer(title)))
	if hwnd == 0 {
		return
	}
	raw := []byte(arg)
	cds := copyDataStruct{
		dwData: 0,
		cbData: uint32(len(raw)),
		lpData: uintptr(unsafe.Pointer(&raw[0])),
	}
	var res uintptr
	// SMTO_ABORTIFHUNG = 0x0002, timeout 3000 ms
	sendMessageTimeoutW.Call(hwnd, wmCopyData, 0, uintptr(unsafe.Pointer(&cds)), 0x0002, 3000, uintptr(unsafe.Pointer(&res)))
}

// giphyKey is the built-in GIPHY API key, set at build time with -ldflags "-X main.giphyKey=…";
// a key saved in Settings wins. Never logged.
var giphyKey string

// logLimit: a log larger than this at startup starts over.
const logLimit = 1 << 20

func main() {
	// Set a 64 MB soft memory limit so the Go runtime aggressively reclaims and returns
	// memory to Windows before the heap can grow unchecked.
	debug.SetMemoryLimit(64 * 1024 * 1024)
	_ = setAppID(appUserModelID)
	go ensureShortcut()
	go ensureProtocol()
	reset, err := run()
	if err != nil {
		walk.MsgBox(nil, "Wisp", "Wisp could not start: "+err.Error(), walk.MsgBoxIconError|walk.MsgBoxOK)
		os.Exit(1)
	}
	if !reset {
		return
	}
	// every defer of run is done: databases closed, instance handle released, log closed
	dir, err := dataDir()
	if err == nil {
		err = wipeData(dir)
	}
	if err == nil {
		err = relaunch()
	}
	if err != nil {
		walk.MsgBox(nil, "Wisp", "Wisp could not reset: "+err.Error(), walk.MsgBoxIconError|walk.MsgBoxOK)
		os.Exit(1)
	}
}

// dataDir is Wisp's data folder, %LOCALAPPDATA%\Wisp.
func dataDir() (string, error) {
	base, err := os.UserCacheDir() // %LOCALAPPDATA%
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "Wisp"), nil
}

// run starts Wisp and returns once it exits; reset says it ended through Reset Wisp. An error is
// logged before the log closes.
func run() (reset bool, err error) {
	dir, err := dataDir()
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	if f, ferr := openLog(filepath.Join(dir, "wisp.log"), logLimit); ferr == nil {
		log.SetOutput(f)
		defer func() {
			if err != nil {
				log.Printf("wisp: %v", err)
			}
			f.Close()
		}()
	} // else: a GUI build has nowhere to log, the app still runs

	show, first, err := claimInstance(dir)
	if err != nil {
		return false, err
	}
	if !first {
		if len(os.Args) > 1 {
			forwardToRunningInstance(os.Args[1])
		}
		log.Printf("wisp: already running; brought it to the front")
		return false, nil
	}
	defer windows.CloseHandle(show)

	db, err := store.Open(filepath.Join(dir, "wisp.db"))
	if err != nil {
		return false, err
	}
	defer db.Close()
	root, err := media.DefaultRoot()
	if err != nil {
		return false, err
	}
	cache := media.New(root)
	a, err := app.New(db, cache, giphyKey)
	if err != nil {
		return false, err
	}
	mgr, err := whatsapp.New(context.Background(), filepath.Join(dir, "session.db"), db, cache)
	if err != nil {
		return false, err
	}
	a.Attach(mgr)
	go func() { // another Wisp asked for the window
		for {
			if ev, err := windows.WaitForSingleObject(show, windows.INFINITE); ev != windows.WAIT_OBJECT_0 {
				log.Printf("wisp: instance signal: %v", err)
				return
			}
			a.Restore()
		}
	}()
	return a.Run()
}

// instanceName names the per-session event that keeps one Wisp per data folder: two would share
// the database and connect the same accounts twice.
func instanceName(dataDir string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(dataDir))))
	return `Local\wisp-` + hex.EncodeToString(sum[:16])
}

// claimInstance makes this the running Wisp for dataDir and returns the event other instances set
// to bring it to the front. When one already runs, it is signalled instead and first is false.
func claimInstance(dataDir string) (show windows.Handle, first bool, err error) {
	name, err := windows.UTF16PtrFromString(instanceName(dataDir))
	if err != nil {
		return 0, false, err
	}
	show, err = windows.CreateEvent(nil, 0, 0, name) // auto-reset, unset
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		defer windows.CloseHandle(show)
		allowSetForegroundWindow.Call(asfwAny) // we have the foreground: let the running one take it
		return 0, false, windows.SetEvent(show)
	}
	if err != nil {
		return 0, false, err
	}
	return show, true, nil
}

var allowSetForegroundWindow = windows.NewLazySystemDLL("user32.dll").NewProc("AllowSetForegroundWindow")

const asfwAny = ^uintptr(0) // ASFW_ANY

// openLog opens the log at path for appending, emptying it first when it is over limit bytes.
func openLog(path string, limit int64) (*os.File, error) {
	flag := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fi, err := os.Stat(path); err == nil && fi.Size() > limit {
		flag |= os.O_TRUNC
	}
	return os.OpenFile(path, flag, 0o600)
}
