// Package media stores downloaded WhatsApp media on disk (streamed, never
// buffered whole in RAM), makes small thumbnails and opens files safely.
package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	_ "image/png" // register PNG decoder
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register WebP decoder
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

// DefaultRoot returns %LOCALAPPDATA%\Wisp\cache.
func DefaultRoot() (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "Wisp", "cache"), nil
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

// Thumbnail writes a thumbnail (fit within 200x200) next to a cached file and returns its path:
// <name>_thumb.jpg of a JPEG or PNG; <name>_thumb.png, which keeps transparency, of a WebP sticker;
// <name>_thumb.jpg as Windows pictures it of a video. One already made is returned as it is. Other
// types return ErrNoThumbnail without reading the file.
func (c *Cache) Thumbnail(path string) (string, error) {
	resolved, err := c.inRoot(path)
	if err != nil {
		return "", err
	}
	ext := strings.ToLower(filepath.Ext(resolved))
	out, video := strings.TrimSuffix(resolved, filepath.Ext(resolved))+"_thumb.jpg", false
	switch ext {
	case ".jpg", ".jpeg", ".png":
	case ".webp":
		out = strings.TrimSuffix(out, ".jpg") + ".png"
	case ".mp4", ".mov", ".mkv", ".avi", ".webm", ".3gp":
		video = true
	default:
		return "", ErrNoThumbnail
	}
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}
	var dst image.Image
	if video {
		if dst, err = shellThumbnail(resolved, thumbMax); err != nil {
			err = fmt.Errorf("%w: %v", ErrNoThumbnail, err) // not a video Windows can picture
		}
	} else {
		dst, err = scaledDown(resolved)
	}
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(out), ".tmp-*")
	if err != nil {
		return "", err
	}
	if ext == ".webp" {
		err = png.Encode(tmp, dst)
	} else {
		err = jpeg.Encode(tmp, dst, &jpeg.Options{Quality: 80})
	}
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

// decodeLimited decodes the image file at path; one of more than maxPixels is refused undecoded.
func decodeLimited(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return nil, ErrImageTooLarge
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	src, _, err := image.Decode(f)
	return src, err
}

// firstFrameOf is FirstWebPFrame of the WebP file at path, refusing a file far bigger than stickers
// are.
func firstFrameOf(path string) (image.Image, error) {
	if st, err := os.Stat(path); err != nil {
		return nil, err
	} else if st.Size() > 8<<20 {
		return nil, ErrImageTooLarge
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return FirstWebPFrame(data)
}

// scaledDown decodes the image file at path, fitted within thumbMax x thumbMax; one of more than
// maxPixels is refused undecoded.
func scaledDown(path string) (image.Image, error) {
	src, err := decodeLimited(path)
	if err != nil && strings.EqualFold(filepath.Ext(path), ".webp") {
		if first, ferr := firstFrameOf(path); ferr == nil {
			src, err = first, nil // an animated sticker, or a still one x/image/webp refuses
		}
	}
	if err != nil {
		return nil, err
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
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
	return dst, nil
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

// AvatarPath returns the path where jid's avatar is stored: <root>\<accountID>\avatars\<sha256 hex of jid>.jpg.
func (c *Cache) AvatarPath(accountID, jid string) string {
	h := sha256.Sum256([]byte(jid))
	return filepath.Join(c.root, accountID, "avatars", hex.EncodeToString(h[:])+".jpg")
}

// SaveAvatar decodes (JPEG/PNG/WebP, refuses > 4 MP), scales to 96 px, and writes JPEG atomically.
func (c *Cache) SaveAvatar(accountID, jid string, data []byte) (string, error) {
	if !accountRe.MatchString(accountID) {
		return "", fmt.Errorf("media: invalid account id %q", accountID)
	}
	if jid == "" {
		return "", errors.New("media: empty jid")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 4_000_000 {
		return "", ErrImageTooLarge
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	dst := image.NewRGBA(image.Rect(0, 0, 96, 96))
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	side := min(w, h)
	crop := image.Rect(b.Min.X+(w-side)/2, b.Min.Y+(h-side)/2, b.Min.X+(w+side)/2, b.Min.Y+(h+side)/2)
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, crop, draw.Src, nil)

	out := c.AvatarPath(accountID, jid)
	dir := filepath.Dir(out)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	err = jpeg.Encode(tmp, dst, &jpeg.Options{Quality: 85})
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

// ClearMediaCache deletes <root>\*\media\ and <root>\*\avatars\ directories.
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
		if fi, err := os.Lstat(m); err == nil && fi.IsDir() {
			if err := os.RemoveAll(m); err != nil {
				errs = append(errs, err)
			}
		}
		av := filepath.Join(c.root, a.Name(), "avatars")
		if fi, err := os.Lstat(av); err == nil && fi.IsDir() {
			if err := os.RemoveAll(av); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Remove deletes a cached file and its thumbnail (a disappearing message went). A path outside the
// cache is refused; one already gone is no error.
func (c *Cache) Remove(path string) error {
	resolved, err := c.inRoot(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	thumb := strings.TrimSuffix(resolved, filepath.Ext(resolved)) + "_thumb.jpg"
	return errors.Join(removeGone(resolved), removeGone(thumb))
}

// removeGone deletes path; one already gone is no error.
func removeGone(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// MarkFromInternet gives a downloaded file the Mark of the Web (an NTFS Zone.Identifier stream for
// the Internet zone), so Windows, SmartScreen and Office's Protected View treat it as untrusted.
func MarkFromInternet(path string) error {
	return os.WriteFile(path+":Zone.Identifier", []byte("[ZoneTransfer]\r\nZoneId=3\r\n"), 0o600)
}

// Missing reports whether a recorded download's file is gone (Clear Cache, or deleted by hand), so
// it shows as downloadable again. No path is not missing: it was never downloaded.
func Missing(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
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
