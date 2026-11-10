package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"wa-go/internal/ui"
)

// giphyServer answers every https request as if it were the host in the URL; hits counts requests.
func giphyServer(t *testing.T, h http.HandlerFunc) (*http.Client, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	rt := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // the test certificate names 127.0.0.1, not GIPHY
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		},
	}
	return newGIFClient(rt, 2*time.Second), &hits
}

// tempDir points os.CreateTemp at an empty directory and returns it.
func tempDir(t *testing.T) string {
	d := t.TempDir()
	t.Setenv("TMP", d)
	t.Setenv("TEMP", d)
	return d
}

func leftovers(t *testing.T, dir string) int {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(es)
}

const mp4URL = "https://media1.giphy.com/media/v1.x/abc/giphy-downsized-small.mp4"

func TestFetchGIFSavesTheMP4(t *testing.T) {
	dir := tempDir(t)
	body := bytes.Repeat([]byte("mp4"), 1000)
	c, _ := giphyServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "media1.giphy.com" {
			t.Errorf("host %q", r.Host)
		}
		w.Write(body)
	})
	path, err := fetchGIF(context.Background(), c, mp4URL, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("saved %d bytes (err %v), want %d", len(got), err, len(body))
	}
	if err := os.Remove(path); err != nil || leftovers(t, dir) != 0 {
		t.Fatalf("temp file not where expected: %v", err)
	}
}

func TestFetchGIFRechecksTheAllowlist(t *testing.T) {
	tempDir(t)
	c, hits := giphyServer(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("x")) })
	for _, u := range []string{"http://media1.giphy.com/x.mp4", "https://evil.example/x.mp4", "https://media1.giphy.com.evil.example/x.mp4", "file:///C:/x.mp4"} {
		if _, err := fetchGIF(context.Background(), c, u, 1<<20); err == nil {
			t.Errorf("%s was fetched", u)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("%d requests made for refused URLs", hits.Load())
	}
}

func TestFetchGIFFollowsRedirectsOnlyWithinGiphy(t *testing.T) {
	tempDir(t)
	c, _ := giphyServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/inside":
			http.Redirect(w, r, "https://media2.giphy.com/ok.mp4", http.StatusFound)
		case "/outside":
			http.Redirect(w, r, "https://evil.example/x.mp4", http.StatusFound)
		default:
			w.Write([]byte("ok"))
		}
	})
	path, err := fetchGIF(context.Background(), c, "https://media1.giphy.com/inside", 1<<20)
	if err != nil {
		t.Fatalf("redirect within GIPHY: %v", err)
	}
	os.Remove(path)
	if _, err := fetchGIF(context.Background(), c, "https://media1.giphy.com/outside", 1<<20); err == nil {
		t.Fatal("redirect off GIPHY was followed")
	}
}

func TestFetchGIFCapsTheSize(t *testing.T) {
	dir := tempDir(t)
	const limit = 1000
	c, _ := giphyServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/declared" {
			w.Header().Set("Content-Length", strconv.Itoa(limit+1))
		}
		w.Write(bytes.Repeat([]byte("x"), limit/2))
		w.(http.Flusher).Flush() // no Content-Length: streamed
		w.Write(bytes.Repeat([]byte("x"), limit/2+1))
	})
	for _, p := range []string{"/streamed", "/declared"} {
		if _, err := fetchGIF(context.Background(), c, "https://media1.giphy.com"+p, limit); err == nil {
			t.Errorf("%s: %d bytes accepted over a %d byte cap", p, limit+1, limit)
		}
	}
	if n := leftovers(t, dir); n != 0 {
		t.Fatalf("%d temp files left behind", n)
	}
	path, err := fetchGIF(context.Background(), c, "https://media1.giphy.com/streamed", limit+1)
	if err != nil {
		t.Fatalf("exactly at the cap: %v", err)
	}
	os.Remove(path)
}

func TestFetchGIFTimesOutAndRejectsErrors(t *testing.T) {
	dir := tempDir(t)
	c, _ := giphyServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		select { // slower than the client allows
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	})
	c.Timeout = 100 * time.Millisecond
	start := time.Now()
	if _, err := fetchGIF(context.Background(), c, "https://media1.giphy.com/slow", 1<<20); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("slow download: err %v after %v", err, time.Since(start))
	}
	if _, err := fetchGIF(context.Background(), c, "https://media1.giphy.com/missing", 1<<20); err == nil {
		t.Fatal("a 404 was saved")
	}
	if n := leftovers(t, dir); n != 0 {
		t.Fatalf("%d temp files left behind", n)
	}
}

func TestPickedGIFsGoThroughTheProxy(t *testing.T) {
	if gifHTTP.Transport != ui.GIFTransport {
		t.Fatal("picked GIFs are downloaded without the proxy in Settings")
	}
}
