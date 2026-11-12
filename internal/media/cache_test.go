package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newCache(t *testing.T) (*Cache, string) {
	t.Helper()
	root := t.TempDir()
	return New(root), root
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

type repeatReader struct{ n int64 }

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.n {
		p = p[:r.n]
	}
	for i := range p {
		p[i] = byte(i)
	}
	r.n -= int64(len(p))
	return len(p), nil
}

func TestSaveStreamsToHashedPath(t *testing.T) {
	c, root := newCache(t)
	data := jpegBytes(t, 50, 50)
	p, err := c.Save("628123", ".jpg", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	want := filepath.Join(root, "628123", "media", hex.EncodeToString(sum[:])+".jpg")
	if p != want {
		t.Fatalf("path %q want %q", p, want)
	}
	got, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("content mismatch: %v", err)
	}
	// duplicate returns existing path, leaves no temp files
	p2, err := c.Save("628123", ".jpg", bytes.NewReader(data))
	if err != nil || p2 != p {
		t.Fatalf("dup: %q %v", p2, err)
	}
	ents, _ := os.ReadDir(filepath.Dir(p))
	if len(ents) != 1 {
		t.Fatalf("expected 1 file, got %d", len(ents))
	}
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestSaveErrorLeavesNoFile(t *testing.T) {
	c, root := newCache(t)
	if _, err := c.Save("a1", ".mp4", io.MultiReader(strings.NewReader("abc"), failReader{})); err == nil {
		t.Fatal("expected error")
	}
	ents, _ := os.ReadDir(filepath.Join(root, "a1", "media"))
	if len(ents) != 0 {
		t.Fatalf("leftover files: %v", ents)
	}
}

func TestSaveMemoryBounded(t *testing.T) {
	c, _ := newCache(t)
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	if _, err := c.Save("a1", ".mp4", &repeatReader{n: 64 << 20}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&b)
	if d := b.TotalAlloc - a.TotalAlloc; d > 8<<20 {
		t.Fatalf("allocated %d bytes", d)
	}
}

func TestInvalidAccountAndExt(t *testing.T) {
	c, _ := newCache(t)
	for _, acc := range []string{"", "..", "a/b", `a\b`, "C:x", "a..b"} {
		if _, err := c.Save(acc, ".jpg", strings.NewReader("x")); err == nil {
			t.Errorf("account %q accepted", acc)
		}
	}
	for _, ext := range []string{"", "jpg", ".", "..", ".j/g", `.a\b`, ".toolongextension", ".j pg", "./x"} {
		if _, err := c.Save("acc", ext, strings.NewReader("x")); err == nil {
			t.Errorf("ext %q accepted", ext)
		}
	}
}

func TestThumbnail(t *testing.T) {
	c, _ := newCache(t)
	p, err := c.Save("acc", ".jpg", bytes.NewReader(jpegBytes(t, 1600, 900)))
	if err != nil {
		t.Fatal(err)
	}
	tp, err := c.Thumbnail(p)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(tp)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 200 || cfg.Height != 112 {
		t.Fatalf("thumb %dx%d", cfg.Width, cfg.Height)
	}
}

func TestThumbnailRefusals(t *testing.T) {
	c, _ := newCache(t)
	// header claiming 60000x60000 PNG
	var hdr bytes.Buffer
	hdr.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], 60000)
	binary.BigEndian.PutUint32(ihdr[4:], 60000)
	ihdr[8], ihdr[9] = 8, 2
	binary.Write(&hdr, binary.BigEndian, uint32(13))
	hdr.WriteString("IHDR")
	hdr.Write(ihdr)
	binary.Write(&hdr, binary.BigEndian, crc32.ChecksumIEEE(append([]byte("IHDR"), ihdr...)))
	p, err := c.Save("acc", ".png", &hdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Thumbnail(p); !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("want ErrImageTooLarge, got %v", err)
	}
	vp, _ := c.Save("acc", ".mp4", strings.NewReader("not an image"))
	if _, err := c.Thumbnail(vp); !errors.Is(err, ErrNoThumbnail) {
		t.Fatalf("want ErrNoThumbnail, got %v", err)
	}
	// .jpg that is not an image: decode error, not panic
	bp, _ := c.Save("acc", ".jpg", strings.NewReader("garbage"))
	if _, err := c.Thumbnail(bp); err == nil {
		t.Fatal("expected error")
	}
}

func TestCacheSizeAndClear(t *testing.T) {
	c, root := newCache(t)
	if n, err := New(filepath.Join(root, "missing")).GetCacheSizeBytes(); n != 0 || err != nil {
		t.Fatalf("missing root: %d %v", n, err)
	}
	if err := New(filepath.Join(root, "missing")).ClearMediaCache(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Save("a1", ".txt", strings.NewReader("12345")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Save("a2", ".txt", strings.NewReader("1234567")); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "a1", "keep.db")
	os.WriteFile(other, []byte("xx"), 0o600)
	if n, _ := c.GetCacheSizeBytes(); n != 14 {
		t.Fatalf("size %d", n)
	}
	outside := filepath.Join(filepath.Dir(root), "sentinel-"+filepath.Base(root))
	os.WriteFile(outside, []byte("s"), 0o600)
	defer os.Remove(outside)
	if err := c.ClearMediaCache(); err != nil {
		t.Fatal(err)
	}
	if n, _ := c.GetCacheSizeBytes(); n != 2 {
		t.Fatalf("size after clear %d", n)
	}
	for _, f := range []string{other, outside} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("%s removed", f)
		}
	}
}

func TestCheckOpenable(t *testing.T) {
	c, root := newCache(t)
	touch := func(p string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	media := filepath.Join(root, "acct", "media")
	good := touch(filepath.Join(media, "ok.pdf"))
	want, _ := filepath.EvalSymlinks(good)
	if got, err := c.checkOpenable(good); err != nil || got != want {
		t.Fatalf("happy path: %q %v", got, err)
	}

	outside := touch(filepath.Join(filepath.Dir(root), "outside-"+filepath.Base(root)+".pdf"))
	sibling := touch(filepath.Join(root+"2", "x.pdf"))
	t.Cleanup(func() { os.Remove(outside); os.RemoveAll(root + "2") })
	cases := map[string]string{
		"exe in root":    touch(filepath.Join(media, "bad.exe")),
		"outside":        outside,
		"dotdot escape":  filepath.Join(media, "..", "..", "..", filepath.Base(outside)),
		"prefix sibling": sibling,
		"ADS exe:pdf":    good[:len(good)-4] + ".exe:x.pdf",
		"ADS pdf:exe":    good + ":evil.exe",
		"missing file":   filepath.Join(media, "nope.pdf"),
	}
	for name, p := range cases {
		if got, err := c.checkOpenable(p); err == nil {
			t.Errorf("%s: accepted, resolved %q", name, got)
		}
	}

	// Directory link inside the root pointing outside it.
	link := filepath.Join(media, "lnk")
	target := filepath.Dir(sibling)
	if err := os.Symlink(target, link); err != nil {
		out, jerr := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
		if jerr != nil {
			t.Skipf("cannot create symlink (%v) or junction (%v: %s); link-escape case skipped", err, jerr, out)
		}
	}
	if got, err := c.checkOpenable(filepath.Join(link, "x.pdf")); err == nil {
		t.Errorf("link escape accepted, resolved %q", got)
	}
}

func TestMarkFromInternetWritesTheZoneStream(t *testing.T) {
	c, _ := newCache(t)
	data := []byte("%PDF-1.4 hello")
	p, err := c.Save("acct", ".pdf", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkFromInternet(p); err != nil {
		t.Fatal(err)
	}
	zone, err := os.ReadFile(p + ":Zone.Identifier")
	if err != nil || string(zone) != "[ZoneTransfer]\r\nZoneId=3\r\n" {
		t.Fatalf("Zone.Identifier = %q, %v", zone, err)
	}
	if _, err := c.checkOpenable(p); err != nil {
		t.Fatalf("the marked file itself is refused: %v", err)
	}
	if n, err := c.GetCacheSizeBytes(); err != nil || n != int64(len(data)) {
		t.Fatalf("cache size = %d, %v; want %d (the stream is not counted)", n, err, len(data))
	}
	if err := c.ClearMediaCache(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marked file survived Clear: %v", err)
	}
}

func TestRemoveDeletesOnlyInsideTheCache(t *testing.T) {
	c, _ := newCache(t)
	p, err := c.Save("acct", ".jpg", bytes.NewReader(jpegBytes(t, 300, 200)))
	if err != nil {
		t.Fatal(err)
	}
	thumb, err := c.Thumbnail(p)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "keep.jpg")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(outside); err == nil {
		t.Fatal("Remove deleted a file outside the cache")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside file gone: %v", err)
	}
	if err := c.Remove(p); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{p, thumb} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived Remove: %v", f, err)
		}
	}
	if err := c.Remove(p); err != nil {
		t.Fatalf("removing a file already gone: %v", err)
	}
}

func TestMissing(t *testing.T) {
	c, _ := newCache(t)
	p, err := c.Save("acct", ".txt", strings.NewReader("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if Missing(p) || Missing("") {
		t.Fatal("an existing file (or no path) counted as missing")
	}
	os.Remove(p)
	if !Missing(p) {
		t.Fatal("a deleted download did not count as missing")
	}
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestSaveAvatarScalesAndClears(t *testing.T) {
	c, _ := newCache(t)
	pngData := pngBytes(t, 640, 640)
	jid := "alice@s.whatsapp.net"
	path, err := c.SaveAvatar("acct", jid, pngData)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := c.AvatarPath("acct", jid)
	if path != wantPath {
		t.Fatalf("AvatarPath = %q, want %q", path, wantPath)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" {
		t.Errorf("saved format = %q, want jpeg", format)
	}
	if cfg.Width != 96 || cfg.Height != 96 {
		t.Errorf("saved dimensions = %dx%d, want 96x96", cfg.Width, cfg.Height)
	}

	bigData := jpegBytes(t, 2500, 2000)
	if _, err := c.SaveAvatar("acct", "bob@s.whatsapp.net", bigData); !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("5 MP image returned err = %v, want ErrImageTooLarge", err)
	}

	if err := c.ClearMediaCache(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("avatar survived ClearMediaCache: %v", err)
	}
}
