package linkpreview

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/gif"
	_ "image/png"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
	"golang.org/x/net/html"
	"golang.org/x/net/proxy"

	"wa-go/internal/linkify"
	"wa-go/internal/whatsapp"
)

const (
	requestTimeout   = 8 * time.Second
	maxRedirects     = 5
	maxHTMLBytes     = 512 * 1024
	maxImageBytes    = 2 * 1024 * 1024
	maxImagePixels   = 4_000_000
	maxThumbWidth    = 320
	maxThumbHeight   = 320
	thumbJPEGQuality = 75
	fetchUserAgent   = "WhatsApp/2.24.0"
)

// Result holds OpenGraph/HTML link preview metadata extracted from a URL.
type Result struct {
	CanonicalURL string
	Title        string
	Description  string
	Thumb        []byte
	Found        bool
}

// Fetcher fetches OpenGraph link previews with proxy support, SSRF protection,
// and strict timeout and memory caps.
type Fetcher struct {
	mu        sync.RWMutex
	proxyAddr string
	proxyURL  *url.URL
	client    *http.Client
	transport *http.Transport

	// Test hooks; nil uses production implementations.
	ipBlocked func(net.IP) bool
	lookupIP  func(ctx context.Context, host string) ([]net.IPAddr, error)
	dialRaw   func(ctx context.Context, network, addr string) (net.Conn, error)
}

// NewFetcher returns a Fetcher configured to route requests through proxyAddr
// ("" for a direct connection, or a socks5://, http://, or https:// URL).
func NewFetcher(proxyAddr string) (*Fetcher, error) {
	f := &Fetcher{}
	if err := f.SetProxy(proxyAddr); err != nil {
		return nil, err
	}
	return f, nil
}

// SetProxy reconfigures the proxy used for subsequent Fetch calls.
func (f *Fetcher) SetProxy(proxyAddr string) error {
	if err := whatsapp.ValidateProxy(proxyAddr); err != nil {
		return err
	}
	var parsed *url.URL
	if proxyAddr != "" {
		u, err := url.Parse(proxyAddr)
		if err != nil {
			return err
		}
		parsed = u
	}

	client, tr, err := f.buildClient(parsed)
	if err != nil {
		return err
	}

	f.mu.Lock()
	oldTr := f.transport
	f.proxyAddr = proxyAddr
	f.proxyURL = parsed
	f.client = client
	f.transport = tr
	f.mu.Unlock()

	if oldTr != nil {
		oldTr.CloseIdleConnections()
	}
	return nil
}

func (f *Fetcher) buildClient(proxyURL *url.URL) (*http.Client, *http.Transport, error) {
	tr := &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   requestTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	}

	proxied := proxyURL != nil
	if !proxied {
		tr.Proxy = nil
		tr.DialContext = f.directDial
	} else if proxyURL.Scheme == "socks5" {
		d, err := proxy.FromURL(proxyURL, contextDialerFunc(f.rawDial))
		if err != nil {
			return nil, nil, fmt.Errorf("linkpreview: socks5 proxy: %w", err)
		}
		tr.Proxy = nil
		if cd, ok := d.(proxy.ContextDialer); ok {
			tr.DialContext = cd.DialContext
		} else {
			tr.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
				return d.Dial(network, addr)
			}
		}
	} else {
		tr.Proxy = http.ProxyURL(proxyURL)
		tr.DialContext = f.rawDial
	}

	client := &http.Client{
		Transport: tr,
		Timeout:   requestTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return errors.New("linkpreview: too many redirects")
			}
			return validateTargetURL(req.URL, proxied, f.checkIPBlocked)
		},
	}
	return client, tr, nil
}

type contextDialerFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func (fn contextDialerFunc) Dial(network, addr string) (net.Conn, error) {
	return fn(context.Background(), network, addr)
}

func (fn contextDialerFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return fn(ctx, network, addr)
}

func (f *Fetcher) checkIPBlocked(ip net.IP) bool {
	f.mu.RLock()
	fn := f.ipBlocked
	f.mu.RUnlock()
	if fn != nil {
		return fn(ip)
	}
	return isBlockedIP(ip)
}

func (f *Fetcher) resolveHost(ctx context.Context, host string) ([]net.IPAddr, error) {
	f.mu.RLock()
	fn := f.lookupIP
	f.mu.RUnlock()
	if fn != nil {
		return fn(ctx, host)
	}
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

func (f *Fetcher) rawDial(ctx context.Context, network, addr string) (net.Conn, error) {
	f.mu.RLock()
	fn := f.dialRaw
	f.mu.RUnlock()
	if fn != nil {
		return fn(ctx, network, addr)
	}
	return (&net.Dialer{Timeout: requestTimeout}).DialContext(ctx, network, addr)
}

func (f *Fetcher) directDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := parseIPLiteral(host); ip != nil {
		if f.checkIPBlocked(ip) {
			return nil, fmt.Errorf("linkpreview: blocked IP address %s", ip)
		}
		return f.rawDial(ctx, network, net.JoinHostPort(host, port))
	}
	ips, err := f.resolveHost(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("linkpreview: no IP addresses for host %q", host)
	}
	for _, ipAddr := range ips {
		if f.checkIPBlocked(ipAddr.IP) {
			return nil, fmt.Errorf("linkpreview: host %q resolved to blocked IP %s", host, ipAddr.IP)
		}
	}
	var lastErr error
	for _, ipAddr := range ips {
		target := ipAddr.IP.String()
		if ipAddr.Zone != "" {
			target += "%" + ipAddr.Zone
		}
		conn, err := f.rawDial(ctx, network, net.JoinHostPort(target, port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// Fetch retrieves rawURL and extracts OpenGraph/HTML metadata and a downscaled JPEG thumbnail.
// Returns Found == false with nil error if the page has no usable metadata or is non-HTML.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (Result, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return Result{}, fmt.Errorf("linkpreview: parse url: %w", err)
	}

	f.mu.RLock()
	client := f.client
	proxied := f.proxyURL != nil
	f.mu.RUnlock()

	if err := validateTargetURL(u, proxied, f.checkIPBlocked); err != nil {
		return Result{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("User-Agent", fetchUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.5")

	resp, err := client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{}, fmt.Errorf("linkpreview: HTTP %d", resp.StatusCode)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		mediaType, _, err := mime.ParseMediaType(ct)
		if err == nil && mediaType != "text/html" && mediaType != "application/xhtml+xml" {
			return Result{}, nil
		}
	}

	meta := parseHTMLMeta(io.LimitReader(resp.Body, maxHTMLBytes))
	finalURL := u
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL
	}

	var res Result
	res.Title, res.Description = linkify.SanitizePreview(meta.title(), meta.description())
	if canonical := resolveHTTPURL(finalURL, meta.ogURL); canonical != "" {
		res.CanonicalURL = canonical
	} else {
		res.CanonicalURL = finalURL.String()
	}

	if imgURL := resolveHTTPURL(finalURL, meta.image()); imgURL != "" {
		res.Thumb = f.fetchThumb(ctx, client, proxied, imgURL)
	}

	if res.Title == "" && res.Description == "" && len(res.Thumb) == 0 {
		return Result{}, nil
	}
	res.Found = true
	return res, nil
}

func (f *Fetcher) fetchThumb(ctx context.Context, client *http.Client, proxied bool, imgURL string) []byte {
	u, err := url.Parse(imgURL)
	if err != nil || validateTargetURL(u, proxied, f.checkIPBlocked) != nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", fetchUserAgent)
	req.Header.Set("Accept", "image/*,*/*;q=0.5")

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	if resp.ContentLength > maxImageBytes {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxImageBytes {
		return nil
	}
	thumb, err := encodeThumbnailJPEG(data)
	if err != nil {
		return nil
	}
	return thumb
}

func encodeThumbnailJPEG(data []byte) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxImagePixels/cfg.Height {
		return nil, errors.New("linkpreview: image dimensions exceed limit")
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, errors.New("linkpreview: empty image bounds")
	}
	if w > maxThumbWidth || h > maxThumbHeight {
		if w*maxThumbHeight >= h*maxThumbWidth {
			h = max(1, h*maxThumbWidth/w)
			w = maxThumbWidth
		} else {
			w = max(1, w*maxThumbHeight/h)
			h = maxThumbHeight
		}
	}

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)

	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: thumbJPEGQuality}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func validateTargetURL(u *url.URL, proxied bool, checkIP func(net.IP) bool) error {
	if u == nil {
		return errors.New("linkpreview: nil url")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("linkpreview: unsupported scheme %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("linkpreview: missing host")
	}
	if proxied {
		norm := strings.ToLower(strings.TrimSuffix(host, "."))
		if norm == "localhost" || strings.HasSuffix(norm, ".localhost") ||
			norm == "local" || strings.HasSuffix(norm, ".local") {
			return fmt.Errorf("linkpreview: blocked local host %q", host)
		}
	}
	if ip := parseIPLiteral(host); ip != nil && checkIP(ip) {
		return fmt.Errorf("linkpreview: blocked IP host %q", host)
	}
	return nil
}

func parseIPLiteral(host string) net.IP {
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap().WithZone("")
		return net.IP(addr.AsSlice())
	}
	return parseNonCanonicalIPv4(host)
}

// parseNonCanonicalIPv4 parses integer, hex, octal, and shorthand IPv4 notations
// (e.g. 2130706433, 0x7f000001, 0177.0.0.1, 127.1) following inet_aton / WHATWG rules.
func parseNonCanonicalIPv4(host string) net.IP {
	h := strings.TrimSuffix(host, ".")
	if h == "" {
		return nil
	}
	parts := strings.Split(h, ".")
	if len(parts) > 4 {
		return nil
	}
	var vals [4]uint64
	for i, part := range parts {
		v, ok := parseIPv4Part(part)
		if !ok {
			return nil
		}
		vals[i] = v
	}
	last := len(parts) - 1
	for i := 0; i < last; i++ {
		if vals[i] > 0xff {
			return nil
		}
	}
	maxLast := uint64(1)<<(8*(4-last)) - 1
	if vals[last] > maxLast {
		return nil
	}
	var ip4 uint32
	switch len(parts) {
	case 1:
		ip4 = uint32(vals[0])
	case 2:
		ip4 = uint32(vals[0]<<24 | vals[1])
	case 3:
		ip4 = uint32(vals[0]<<24 | vals[1]<<16 | vals[2])
	case 4:
		ip4 = uint32(vals[0]<<24 | vals[1]<<16 | vals[2]<<8 | vals[3])
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], ip4)
	return net.IP(b[:])
}

func parseIPv4Part(part string) (uint64, bool) {
	if part == "" {
		return 0, false
	}
	base := 10
	digits := part
	if strings.HasPrefix(part, "0x") || strings.HasPrefix(part, "0X") {
		base = 16
		digits = part[2:]
		if digits == "" {
			return 0, false
		}
	} else if len(part) > 1 && part[0] == '0' {
		base = 8
		digits = part[1:]
	}
	for i := 0; i < len(digits); i++ {
		c := digits[i]
		valid := (c >= '0' && c <= '9' && (base != 8 || c <= '7')) ||
			(base == 16 && ((c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')))
		if !valid {
			return 0, false
		}
	}
	v, err := strconv.ParseUint(digits, base, 33)
	if err != nil {
		return 0, false
	}
	return v, true
}

func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 0 { // 0.0.0.0/8 ("this network")
			return true
		}
		if v4[0] == 100 && v4[1]&0xc0 == 64 { // 100.64.0.0/10 (CGNAT)
			return true
		}
		ip = v4
	} else if ip16 := ip.To16(); ip16 != nil && isZeroPrefix(ip16[:12]) {
		// Also block IPv4-compatible IPv6 (e.g. ::127.0.0.1) if its embedded IPv4 is blocked,
		// while leaving :: (IsUnspecified) and ::1 (IsLoopback) handled below.
		if isBlockedIP(net.IP(ip16[12:16])) {
			return true
		}
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast() ||
		ip.Equal(net.IPv4bcast)
}

func isZeroPrefix(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func resolveHTTPURL(base *url.URL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || base == nil {
		return ""
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	resolved := base.ResolveReference(ref)
	s := strings.ToLower(resolved.Scheme)
	if (s != "http" && s != "https") || resolved.Hostname() == "" {
		return ""
	}
	return resolved.String()
}

type htmlMeta struct {
	ogTitle      string
	ogDesc       string
	ogImage      string
	ogImageAlt   string
	ogURL        string
	twitterTitle string
	twitterDesc  string
	twitterImage string
	metaDesc     string
	htmlTitle    string
}

func (m htmlMeta) title() string {
	for _, s := range []string{m.ogTitle, m.twitterTitle, m.htmlTitle} {
		if s != "" {
			return s
		}
	}
	return ""
}

func (m htmlMeta) description() string {
	for _, s := range []string{m.ogDesc, m.twitterDesc, m.metaDesc} {
		if s != "" {
			return s
		}
	}
	return ""
}

func (m htmlMeta) image() string {
	for _, s := range []string{m.ogImage, m.ogImageAlt, m.twitterImage} {
		if s != "" {
			return s
		}
	}
	return ""
}

func parseHTMLMeta(r io.Reader) htmlMeta {
	z := html.NewTokenizer(r)
	var m htmlMeta
	var inTitle, inBody bool
	var titleBuf strings.Builder

	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return m
		case html.StartTagToken, html.SelfClosingTagToken:
			nameBytes, hasAttr := z.TagName()
			tag := strings.ToLower(string(nameBytes))
			switch tag {
			case "body":
				inBody = true
			case "title":
				if tt == html.StartTagToken && !inBody && m.htmlTitle == "" {
					inTitle = true
					titleBuf.Reset()
				}
			case "meta":
				if inBody || !hasAttr {
					continue
				}
				var prop, name, content string
				for {
					k, v, more := z.TagAttr()
					key := strings.ToLower(string(k))
					val := string(v)
					switch key {
					case "property":
						prop = strings.ToLower(strings.TrimSpace(val))
					case "name":
						name = strings.ToLower(strings.TrimSpace(val))
					case "content":
						content = strings.TrimSpace(val)
					}
					if !more {
						break
					}
				}
				if content == "" {
					continue
				}
				key := prop
				if key == "" {
					key = name
				}
				switch key {
				case "og:title":
					if m.ogTitle == "" {
						m.ogTitle = content
					}
				case "og:description":
					if m.ogDesc == "" {
						m.ogDesc = content
					}
				case "og:image":
					if m.ogImage == "" {
						m.ogImage = content
					}
				case "og:image:secure_url", "og:image:url":
					if m.ogImageAlt == "" {
						m.ogImageAlt = content
					}
				case "og:url":
					if m.ogURL == "" {
						m.ogURL = content
					}
				case "twitter:title":
					if m.twitterTitle == "" {
						m.twitterTitle = content
					}
				case "twitter:description":
					if m.twitterDesc == "" {
						m.twitterDesc = content
					}
				case "twitter:image", "twitter:image:src":
					if m.twitterImage == "" {
						m.twitterImage = content
					}
				case "description":
					if m.metaDesc == "" {
						m.metaDesc = content
					}
				}
			}
		case html.TextToken:
			if inTitle {
				titleBuf.Write(z.Text())
			}
		case html.EndTagToken:
			nameBytes, _ := z.TagName()
			if strings.ToLower(string(nameBytes)) == "title" && inTitle {
				inTitle = false
				if m.htmlTitle == "" {
					m.htmlTitle = html.UnescapeString(strings.TrimSpace(titleBuf.String()))
				}
			}
		}
	}
}
