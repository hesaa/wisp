package main

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestInstanceNamePerDataFolder(t *testing.T) {
	a, b := instanceName(`C:\Users\x\AppData\Local\wa-go`), instanceName(`c:\users\x\appdata\local\wa-go\`)
	if a != b {
		t.Fatalf("same folder, different names: %q %q", a, b)
	}
	if !strings.HasPrefix(a, `Local\wa-go-`) || strings.ContainsAny(a[len(`Local\`):], `\:`) {
		t.Fatalf("name %q: want the session namespace and no path characters", a)
	}
	if a == instanceName(`D:\scratch\wa-go`) {
		t.Fatal("two data folders share one instance")
	}
}

func TestSecondInstanceSignalsTheFirst(t *testing.T) {
	dir := t.TempDir()
	show, first, err := claimInstance(dir)
	if err != nil || !first {
		t.Fatalf("first claim: first=%v err=%v", first, err)
	}
	defer windows.CloseHandle(show)
	if ev, _ := windows.WaitForSingleObject(show, 0); ev != uint32(windows.WAIT_TIMEOUT) {
		t.Fatal("signalled before a second instance ran")
	}
	if _, again, err := claimInstance(dir); err != nil || again {
		t.Fatalf("second claim: first=%v err=%v", again, err)
	}
	if ev, _ := windows.WaitForSingleObject(show, 0); ev != windows.WAIT_OBJECT_0 {
		t.Fatal("the running instance was not asked to come to the front")
	}
}
