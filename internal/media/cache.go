// Package media stores downloaded WhatsApp media on disk (streamed, never
// buffered whole in RAM), makes small thumbnails and opens files safely.
package media

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // register PNG decoder
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/image/draw"
	"golang.org/x/sys/windows"
)

const (
	thumbMax = 200
	// Decoded size is ~4 bytes/pixel; 25 MP (~100 MB transient) is the largest we will risk.
	maxPixels = 25_000_000
)

var (
	ErrNoThumbnail   = errors.New("media: no thumbnail for this type")
	ErrImageTooLarge = errors.New("media: image dimensions too large")

	accountRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	extRe     = regexp.MustCompile(`^\.[A-Za-z0-9]{1,8}$`)

	openAllow = map[string]bool{}
)

func init() {
	for _, e := range strings.Fields("jpg jpeg png gif webp bmp mp4 mov mkv avi webm 3gp mp3 m4a aac ogg opus wav pdf txt csv doc docx xls xlsx ppt pptx odt ods odp rtf zip rar 7z") {
		openAllow["."+e] = true
	}
}

// Cache is a disk cache rooted at a directory: <root>\<accountID>\media\<sha256><ext>.
type Cache struct{ root string }

// New returns a cache using root (created lazily on first save).
func New(root string) *Cache { return &Cache{root: root} }

// DefaultRoot returns %LOCALAPPDATA%\wa-go\cache.
func DefaultRoot() (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "wa-go", "cache"), nil
}

// Save streams r to disk, content-addressed by SHA-256, and returns the final path.
func (c *Cache) Save(accountID, ext string, r io.Reader) (string, error) {
	if !accountRe.MatchString(accountID) {
		return "", fmt.Errorf("media: invalid account id %q", accountID)
	}
	if !extRe.MatchString(ext) {
		return "", fmt.Errorf("media: invalid extension %q", ext)
	}
	dir := filepath.Join(c.root, accountID, "media")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	final := filepath.Join(dir, hex.EncodeToString(h.Sum(nil))+ext)
	if _, serr := os.Stat(final); serr == nil {
		return final, nil // identical content already cached
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return "", err
	}
	return final, nil
}

// Thumbnail writes <name>_thumb.jpg (fit within 200x200) next to a cached JPEG/PNG
// and returns its path. Other types return ErrNoThumbnail without reading the file.
func (c *Cache) Thumbnail(path string) (string, error) {
	resolved, err := c.inRoot(path)
	if err != nil {
		return "", err
	}
	ext := strings.ToLower(filepath.Ext(resolved))
	if ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
		return "", ErrNoThumbnail
	}
	f, err := os.Open(resolved)
	if err != nil {
		return "", err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return "", err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return "", ErrImageTooLarge
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	src, _, err := image.Decode(f)
	if err != nil {
		return "", err
	}
	w, h := cfg.Width, cfg.Height
	if w > thumbMax || h > thumbMax {
		if w >= h {
			h = max(1, h*thumbMax/w)
			w = thumbMax
		} else {
			w = max(1, w*thumbMax/h)
			h = thumbMax
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	src = nil // let the full-size image be collected before encoding

	out := strings.TrimSuffix(resolved, filepath.Ext(resolved)) + "_thumb.jpg"
	tmp, err := os.CreateTemp(filepath.Dir(out), ".tmp-*")
	if err != nil {
		return "", err
	}
	err = jpeg.Encode(tmp, dst, &jpeg.Options{Quality: 80})
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), out)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return out, nil
}

// GetCacheSizeBytes sums regular-file sizes under the root; a missing root is 0.
func (c *Cache) GetCacheSizeBytes() (int64, error) {
	var total int64
	err := filepath.WalkDir(c.root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) && total == 0 {
		return 0, nil
	}
	return total, err
}

// ClearMediaCache deletes <root>\*\media\ directories only.
func (c *Cache) ClearMediaCache() error {
	accounts, err := os.ReadDir(c.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range accounts {
		if !a.IsDir() {
			continue
		}
		m := filepath.Join(c.root, a.Name(), "media")
		if fi, err := os.Lstat(m); err != nil || !fi.IsDir() { // skip missing, files, symlinks/junctions
			continue
		}
		if err := os.RemoveAll(m); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// OpenInDefaultApp launches a cached file with its Windows default handler,
// refusing paths outside the cache root and non-allowlisted extensions.
func (c *Cache) OpenInDefaultApp(filePath string) error {
	resolved, err := c.checkOpenable(filePath)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(resolved)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, windows.StringToUTF16Ptr("open"), p, nil, nil, windows.SW_SHOWNORMAL)
}

// checkOpenable returns the resolved path if it is inside the root and allowlisted.
func (c *Cache) checkOpenable(filePath string) (string, error) {
	resolved, err := c.inRoot(filePath)
	if err != nil {
		return "", err
	}
	if ext := strings.ToLower(filepath.Ext(resolved)); !openAllow[ext] {
		return "", fmt.Errorf("media: refusing to open %q files", ext)
	}
	return resolved, nil
}

// inRoot resolves symlinks/.. and returns the real path if it lies inside the root.
func (c *Cache) inRoot(path string) (string, error) {
	// Reject NTFS alternate data streams ("file.exe:x.pdf") outright.
	if strings.Contains(path[len(filepath.VolumeName(path)):], ":") {
		return "", fmt.Errorf("media: %q contains ':'", path)
	}
	root, err := filepath.EvalSymlinks(c.root)
	if err != nil {
		return "", err
	}
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("media: %q is outside the cache", path)
	}
	return p, nil
}
