package ui

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // GIPHY's still previews are single-frame GIFs; only the first frame is decoded
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"wa-go/internal/whatsapp"
)

const (
	giphySearch   = "https://api.giphy.com/v1/gifs/search"
	gifResults    = 20      // asked for, and the most kept from an answer
	maxSearchJSON = 1 << 20 // a search answer larger than this is refused
	maxGIFPreview = 512 << 10
)

var errNoGIFKey = errors.New("ui: GIF search needs a GIPHY API key")

// gifResult is one GIF found on GIPHY; both URLs passed AllowedGIFURL.
type gifResult struct {
	desc    string // GIPHY's title, for screen readers and tooltips; may be ""
	preview string // a still: a small single-frame GIF
	mp4     string // what is sent: a small MP4 made for messaging
}

// giphy searches GIPHY for GIFs. Its answers are untrusted: they are size-limited, and only https
// URLs on GIPHY's own hosts are ever fetched, redirects included.
type giphy struct {
	http *http.Client
	base string // search endpoint
}

// GIFTransport carries every GIPHY request (searches, previews, picked GIFs) through the proxy set
// with SetGIFProxy, as WhatsApp's own traffic goes; environment proxies are not used.
var GIFTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = func(*http.Request) (*url.URL, error) {
		if p := gifProxy.Load(); p != nil {
			return p.url, p.err
		}
		return nil, nil
	}
	return t
}()

// gifProxy is GIF traffic's proxy: url nil for a direct connection, err for an unusable address.
var gifProxy atomic.Pointer[struct {
	url *url.URL
	err error
}]

// SetGIFProxy routes GIF traffic through the proxy at addr, the net.proxy setting ("" for a direct
// connection). An address whatsapp.ValidateProxy refuses fails every GIF request instead of going
// direct.
func SetGIFProxy(addr string) {
	p := &struct {
		url *url.URL
		err error
	}{}
	if p.err = whatsapp.ValidateProxy(addr); p.err == nil && addr != "" {
		p.url, p.err = url.Parse(addr)
	}
	gifProxy.Store(p)
}

func newGiphy() *giphy {
	return &giphy{base: giphySearch, http: &http.Client{
		Transport: GIFTransport,
		Timeout:   15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || !AllowedGIFURL(req.URL.String()) {
				// the host only: the target's query may repeat the API key
				return fmt.Errorf("ui: refusing a GIF redirect to host %q", req.URL.Hostname())
			}
			return nil
		},
	}}
}

// giphyURL is the search request for query. No bundle: GIPHY's messaging bundle drops the stills.
func giphyURL(base, query, key string) string {
	return base + "?" + url.Values{
		"api_key": {key}, "q": {query}, "limit": {fmt.Sprint(gifResults)}, "rating": {"pg-13"}, "lang": {"en"},
	}.Encode()
}

// AllowedGIFURL reports whether raw is an https URL on giphy.com or a subdomain of it (the API,
// media0-4, i), on the default port and without user info.
func AllowedGIFURL(raw string) bool {
	u, err := url.Parse(raw)
	// ASCII hosts only: a non-ASCII letter can lower-case to ASCII (U+0130 to i) while net/http
	// dials the host's punycode, a different domain
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" ||
		strings.IndexFunc(u.Host, func(r rune) bool { return r >= utf8.RuneSelf }) >= 0 {
		return false
	}
	h := strings.ToLower(u.Host)
	return h == "giphy.com" || strings.HasSuffix(h, ".giphy.com")
}

// search asks GIPHY for GIFs matching query with the user's API key; without one it fails with
// errNoGIFKey and makes no request.
func (g *giphy) search(ctx context.Context, key, query string) ([]gifResult, error) {
	if key = strings.TrimSpace(key); key == "" {
		return nil, errNoGIFKey
	}
	body, err := g.get(ctx, giphyURL(g.base, query, key), maxSearchJSON)
	if err != nil {
		return nil, err
	}
	return parseGiphy(body)
}

// preview fetches a result's still and returns its first frame scaled to cover w x h.
func (g *giphy) preview(ctx context.Context, u string, w, h int) (*image.RGBA, error) {
	if !AllowedGIFURL(u) {
		return nil, fmt.Errorf("ui: refusing to fetch GIF preview %q", u)
	}
	body, err := g.get(ctx, u, maxGIFPreview)
	if err != nil {
		return nil, err
	}
	return coverImage(bytes.NewReader(body), w, h)
}

// get returns the body of a 200 answer to u, refusing one over limit bytes. Errors never carry the
// URL: a search URL holds the API key.
func (g *giphy) get(ctx context.Context, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.New("ui: bad GIF request URL")
	}
	resp, err := g.http.Do(req)
	if ue := (*url.Error)(nil); errors.As(err, &ue) {
		err = fmt.Errorf("ui: GIF request: %w", ue.Err)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ui: GIF request: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("ui: GIF request: %w", err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("ui: GIF answer over %d bytes", limit)
	}
	return b, nil
}

// parseGiphy reads a search answer: up to gifResults results with an allowed still (fixed_width_still,
// else fixed_width_small_still) and an allowed MP4 (downsized_small, else fixed_width); the rest are
// dropped. Only those renditions are decoded, so odd shapes elsewhere in a result don't matter.
func parseGiphy(b []byte) ([]gifResult, error) {
	var r struct {
		Data []struct {
			Title  string                     `json:"title"`
			Images map[string]json.RawMessage `json:"images"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("ui: GIF search answer: %w", err)
	}
	if r.Data == nil { // an error object, or "data": null
		return nil, errors.New("ui: GIF search answer has no results list")
	}
	var out []gifResult
	for _, res := range r.Data {
		rendition := func(name string) (v struct{ URL, MP4 string }) {
			json.Unmarshal(res.Images[name], &v) // a missing or malformed rendition stays empty
			return v
		}
		g := gifResult{desc: oneLine(res.Title),
			preview: cmp.Or(rendition("fixed_width_still").URL, rendition("fixed_width_small_still").URL),
			mp4:     cmp.Or(rendition("downsized_small").MP4, rendition("fixed_width").MP4)}
		if AllowedGIFURL(g.preview) && AllowedGIFURL(g.mp4) {
			out = append(out, g)
		}
		if len(out) == gifResults {
			break
		}
	}
	return out, nil
}
