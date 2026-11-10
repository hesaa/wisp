package ui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGiphySearchURL(t *testing.T) {
	u, err := url.Parse(giphyURL("https://api.giphy.com/v1/gifs/search", "cats & dogs?#", "K3Y"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != "https://api.giphy.com/v1/gifs/search" {
		t.Errorf("endpoint %s", u)
	}
	want := url.Values{"api_key": {"K3Y"}, "q": {"cats & dogs?#"}, "limit": {"20"}, "rating": {"pg-13"}, "lang": {"en"}}
	if got := u.Query(); fmt.Sprint(got) != fmt.Sprint(want) { // no bundle: it drops the still renditions
		t.Errorf("query %v, want %v", got, want)
	}
	if !strings.Contains(u.RawQuery, "q=cats+%26+dogs%3F%23") {
		t.Errorf("query not escaped: %s", u.RawQuery)
	}
}

func TestOnlyGiphyHTTPSURLsAreFetched(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://media0.giphy.com/media/x/200w_s.gif": true,
		"https://MEDIA4.Giphy.com/media/x/200w_s.gif": true,
		"https://i.giphy.com/x.gif":                   true,
		"https://giphy.com/gifs/x":                    true,
		"https://api.giphy.com/v1/gifs/search":        true,
		"http://media0.giphy.com/media/x/200w_s.gif":  false, // not TLS
		"https://media0.giphy.com@evil.com/x.gif":     false, // userinfo trick: the host is evil.com
		"https://user:pw@media0.giphy.com/x.gif":      false,
		"https://giphy.com.evil.com/x.gif":            false,
		"https://media0.giphy.com.evil.com/x.gif":     false,
		"https://evilgiphy.com/x.gif":                 false,
		"https://evil.com/media0.giphy.com/x.gif":     false,
		"https://giphy.co/x.gif":                      false,
		"https://151.101.2.2/x.gif":                   false, // IP literal
		"https://[2001:db8::1]/x.gif":                 false,
		"https://media0.giphy.com:8443/x.gif":         false,
		"file:///C:/Windows/win.ini":                  false,
		"//media0.giphy.com/x.gif":                    false,
		"media0.giphy.com/x.gif":                      false,
		"https://media0.giphy.com\\@evil.com/x.gif":   false,
		"": false,
		"https://media0.giphy.com/x.gif\x00.evil.com": false,
		"https://media0.gİphy.com/x.gif":              false, // U+0130 lower-cases to ASCII i, but is dialled as punycode
		"https://gİphy.com/x":                         false,
		"https://media0.xn--giphy-8fd.com/x.gif":      false, // that punycode spelled out
	} {
		if got := allowedGIFURL(raw); got != want {
			t.Errorf("allowedGIFURL(%q) = %v", raw, got)
		}
	}
}

func TestParseGiphyRealAnswer(t *testing.T) {
	b, err := os.ReadFile("testdata/giphy-search.json") // a live answer, trimmed to the renditions used
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseGiphy(b)
	if err != nil {
		t.Fatal(err)
	}
	const cid = "https://media1.giphy.com/media/v1.Y2lkPTRhNDRkNTc0bDgzeDQ0NGxkaTBkdmlvazg5c3NwcG03MXl4bzh3aXJzdzlqb2xqZyZlcD12MV9naWZzX3NlYXJjaCZjdD1n/"
	if len(got) != 2 || got[0].preview != cid+"3ohhwLgxEYOFwqgI3S/200w_s.gif" || got[0].mp4 != cid+"3ohhwLgxEYOFwqgI3S/giphy-downsized-small.mp4" ||
		got[0].desc != "" || got[1].desc != "Heart Thank You GIF" || !strings.HasSuffix(got[1].mp4, "/giphy-downsized-small.mp4") {
		t.Errorf("parsed %+v", got)
	}
}

func giphyJSON(results ...string) string {
	return `{"data":[` + strings.Join(results, ",") + `],"meta":{"status":200,"msg":"OK"}}`
}

func giphyResult(title, still, mp4 string) string {
	return fmt.Sprintf(`{"id":"x","title":%q,"images":{"fixed_width_still":{"url":%q,"width":"200","height":"162","size":"15312"},"downsized_small":{"mp4":%q,"mp4_size":"62651"},"looping":{}}}`, title, still, mp4)
}

func TestParseGiphyFallsBackAndKeepsOnlyAllowedResults(t *testing.T) {
	body := giphyJSON(
		giphyResult("ok", "https://media0.giphy.com/a/200w_s.gif", "https://media0.giphy.com/a/small.mp4"),
		giphyResult("evil still", "https://evil.example/a.gif", "https://media0.giphy.com/b/small.mp4"),
		giphyResult("evil video", "https://media0.giphy.com/c/200w_s.gif", "http://media0.giphy.com/c/small.mp4"),
		`{"title":"no video","images":{"fixed_width_still":{"url":"https://media0.giphy.com/d/200w_s.gif"}}}`,
		`{"title":"fallbacks\r\nused","images":{"fixed_width_small_still":{"url":"https://media2.giphy.com/e/100w_s.gif"},"fixed_width":{"mp4":"https://media2.giphy.com/e/200w.mp4"}}}`,
		`{"title":"odd shapes","images":{"fixed_width_still":{"url":7},"fixed_width_small_still":{"url":"https://i.giphy.com/f.gif"},"downsized_small":[],"fixed_width":{"mp4":"https://i.giphy.com/f.mp4"},"looping":null,"hd":"x"}}`,
	)
	got, err := parseGiphy([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []gifResult{
		{desc: "ok", preview: "https://media0.giphy.com/a/200w_s.gif", mp4: "https://media0.giphy.com/a/small.mp4"},
		{desc: "fallbacks used", preview: "https://media2.giphy.com/e/100w_s.gif", mp4: "https://media2.giphy.com/e/200w.mp4"},
		{desc: "odd shapes", preview: "https://i.giphy.com/f.gif", mp4: "https://i.giphy.com/f.mp4"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestParseGiphyCapsAndRejectsMalformed(t *testing.T) {
	many := make([]string, 50)
	for i := range many {
		many[i] = giphyResult("g", fmt.Sprintf("https://media0.giphy.com/%d/200w_s.gif", i), fmt.Sprintf("https://media0.giphy.com/%d/small.mp4", i))
	}
	if got, err := parseGiphy([]byte(giphyJSON(many...))); err != nil || len(got) != gifResults {
		t.Errorf("50 results: %d kept, %v; want %d", len(got), err, gifResults)
	}
	for _, bad := range []string{``, `{`, `[]`, `{}`, `{"data":null}`, `{"data":{}}`, `{"data":[{"images":[1,2]}]}`, `<html>rate limited</html>`,
		`{"meta":{"status":401,"msg":"Unauthorized"}}`} {
		if _, err := parseGiphy([]byte(bad)); err == nil {
			t.Errorf("parseGiphy(%q) accepted", bad)
		}
	}
	if got, err := parseGiphy([]byte(`{"data":[]}`)); err != nil || len(got) != 0 {
		t.Errorf("no results: %v, %v", got, err)
	}
}

// giphyServer answers searches with body and counts requests; q records the last query.
func giphyServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var hits atomic.Int32
	var q atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		q.Store(r.URL.Query())
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &q
}

func testGiphy(srv *httptest.Server) *giphy {
	g := newGiphy()
	g.base = srv.URL + "/v1/gifs/search"
	return g
}

func TestGiphySearch(t *testing.T) {
	srv, hits, q := giphyServer(t, http.StatusOK, giphyJSON(giphyResult("Cat Jump GIF", "https://media0.giphy.com/a/200w_s.gif", "https://media0.giphy.com/a/small.mp4")))
	got, err := testGiphy(srv).search(context.Background(), "K3Y", "cat jump")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].mp4 != "https://media0.giphy.com/a/small.mp4" || hits.Load() != 1 {
		t.Fatalf("results %+v after %d requests", got, hits.Load())
	}
	sent := q.Load().(url.Values)
	if sent.Get("q") != "cat jump" || sent.Get("api_key") != "K3Y" || sent.Get("rating") != "pg-13" || sent.Has("bundle") {
		t.Errorf("request query %v", sent)
	}
}

func TestGiphyWithoutAKeyMakesNoRequest(t *testing.T) {
	srv, hits, _ := giphyServer(t, http.StatusOK, giphyJSON())
	for _, key := range []string{"", "   "} {
		if _, err := testGiphy(srv).search(context.Background(), key, "cats"); !errors.Is(err, errNoGIFKey) {
			t.Errorf("key %q: error %v, want errNoGIFKey", key, err)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("%d requests made without a key", hits.Load())
	}
}

func TestGiphyFailuresAreErrorsThatHideTheKey(t *testing.T) {
	huge := `{"data":[],"pad":"` + strings.Repeat("x", maxSearchJSON) + `"}`
	for name, c := range map[string]struct {
		status int
		body   string
	}{
		"rate limited": {http.StatusTooManyRequests, `{"meta":{"status":429,"msg":"Too Many Requests"}}`},
		"bad key":      {http.StatusUnauthorized, `{"meta":{"status":401,"msg":"Unauthorized"}}`},
		"oversized":    {http.StatusOK, huge},
		"garbage":      {http.StatusOK, `not json`},
	} {
		srv, _, _ := giphyServer(t, c.status, c.body)
		_, err := testGiphy(srv).search(context.Background(), "S3CRET", "cats")
		if err == nil || strings.Contains(err.Error(), "S3CRET") {
			t.Errorf("%s: error %v", name, err)
		}
	}
	srv, _, _ := giphyServer(t, http.StatusOK, "")
	g := testGiphy(srv)
	srv.Close() // connection refused: net/http's error names the URL, which holds the key
	if _, err := g.search(context.Background(), "S3CRET", "cats"); err == nil || strings.Contains(err.Error(), "S3CRET") {
		t.Errorf("unreachable: error %v", err)
	}
	// a refused redirect that carries the key: the error names no more than the host
	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/v1/gifs/search?api_key=S3CRET&q=cats", http.StatusFound)
	}))
	defer hop.Close()
	if _, err := testGiphy(hop).search(context.Background(), "S3CRET", "cats"); err == nil || strings.Contains(err.Error(), "S3CRET") {
		t.Errorf("refused redirect: error %v", err)
	}
}

// redirectTo sends every request to srv, as if srv were the host in the URL.
type redirectTo struct{ srv *httptest.Server }

func (r redirectTo) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(r.srv.URL)
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host = u.Scheme, u.Host
	return http.DefaultTransport.RoundTrip(out)
}

func gifBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	pal := color.Palette{color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}}
	frame := func(c uint8) *image.Paletted {
		img := image.NewPaletted(image.Rect(0, 0, w, h), pal)
		for i := range img.Pix {
			img.Pix[i] = c
		}
		return img
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, &gif.GIF{Image: []*image.Paletted{frame(0), frame(1)}, Delay: []int{10, 10}}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestGiphyPreviewDecodesTheFirstFrameOfAllowedURLsOnly(t *testing.T) {
	small, wide := gifBytes(t, 40, 30), gifBytes(t, 4000, 400)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/huge-dims.gif":
			w.Write(wide)
		case "/huge-file.gif":
			w.Write(append(small, make([]byte, maxGIFPreview)...))
		case "/hop":
			http.Redirect(w, r, "https://evil.example/x.gif", http.StatusFound)
		default: // any other path, on any host the client would reach: a good GIF
			w.Write(small)
		}
	}))
	defer srv.Close()
	g := newGiphy()
	g.http.Transport = redirectTo{srv}
	ctx := context.Background()

	img, err := g.preview(ctx, "https://media0.giphy.com/200w_s.gif", 20, 10)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 20 || b.Dy() != 10 {
		t.Errorf("preview %v, want 20x10", b)
	}
	if p := img.RGBAAt(10, 5); p.R != 255 || p.B != 0 {
		t.Errorf("preview pixel %v: not the first (red) frame", p)
	}
	for _, u := range []string{"https://media0.giphy.com/huge-dims.gif", "https://media0.giphy.com/huge-file.gif", "https://media0.giphy.com/hop"} {
		if _, err := g.preview(ctx, u, 20, 10); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
	before := hits.Load()
	for _, u := range []string{"http://media0.giphy.com/200w_s.gif", "https://evil.example/200w_s.gif", "https://media0.giphy.com@evil.example/200w_s.gif"} {
		if _, err := g.preview(ctx, u, 20, 10); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
	if hits.Load() != before {
		t.Errorf("%d requests for disallowed URLs", hits.Load()-before)
	}
}
