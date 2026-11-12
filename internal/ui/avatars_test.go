package ui

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"wa-go/internal/media"
)

func TestAvatarsWantOncePerJID(t *testing.T) {
	root := t.TempDir()
	c := media.New(root)
	a := NewAvatars(c)
	defer a.Dispose()
	a.SetAccount("acc1")

	wants := map[string]int{}
	a.OnWant(func(jid string) {
		wants[jid]++
	})

	// drawing twice asks once
	_ = a.Get("jid1", 96)
	_ = a.Get("jid1", 96)
	if wants["jid1"] != 1 {
		t.Errorf("wants[jid1] = %d, want 1", wants["jid1"])
	}

	// after Invalidate asks again
	a.Invalidate("jid1")
	_ = a.Get("jid1", 96)
	if wants["jid1"] != 2 {
		t.Errorf("after Invalidate wants[jid1] = %d, want 2", wants["jid1"])
	}

	// LRU never holds more than 64
	for i := 0; i < 70; i++ {
		jid := fmt.Sprintf("user%d@s.whatsapp.net", i)
		img := image.NewRGBA(image.Rect(0, 0, 96, 96))
		img.Set(0, 0, color.RGBA{uint8(i), 100, 100, 255})
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, nil); err != nil {
			t.Fatal(err)
		}
		p := c.AvatarPath("acc1", jid)
		if p != "" {
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_ = a.Get(jid, 96)
	}

	if n := a.Len(); n > 64 {
		t.Errorf("LRU size = %d, want <= 64", n)
	}
}

func TestAvatarHarness200RowsMemory(t *testing.T) {
	root := t.TempDir()
	c := media.New(root)
	a := NewAvatars(c)
	defer a.Dispose()
	a.SetAccount("acc1")

	// Seed 200 people, first 100 have avatar pictures on disk, remaining 100 missing (showing initials)
	for i := 0; i < 100; i++ {
		jid := fmt.Sprintf("contact%d@s.whatsapp.net", i)
		img := image.NewRGBA(image.Rect(0, 0, 96, 96))
		img.Set(0, 0, color.RGBA{uint8(i), 120, 200, 255})
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, nil); err != nil {
			t.Fatal(err)
		}
		p := c.AvatarPath("acc1", jid)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	runtime.GC()
	var mBefore runtime.MemStats
	runtime.ReadMemStats(&mBefore)

	// Simulate scrolling 200 rows
	for i := 0; i < 200; i++ {
		jid := fmt.Sprintf("contact%d@s.whatsapp.net", i)
		bmp := a.Get(jid, 96)
		if i < 100 && bmp == nil {
			t.Errorf("expected bitmap for seeded avatar %d", i)
		}
		if i >= 100 && bmp != nil {
			t.Errorf("expected nil bitmap (initials) for unseeded avatar %d", i)
		}
	}

	if a.Len() > 64 {
		t.Errorf("LRU size = %d, want <= 64", a.Len())
	}

	runtime.GC()
	var mAfter runtime.MemStats
	runtime.ReadMemStats(&mAfter)

	growth := int64(mAfter.Alloc) - int64(mBefore.Alloc)
	if growth > 4*1024*1024 {
		t.Errorf("memory growth %d bytes exceeded 4 MB", growth)
	}
}
