package linkpreview

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func makeTestPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 40, G: 140, B: 220, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func newTestFetcher(t *testing.T) *Fetcher {
	t.Helper()
	f, err := NewFetcher("")
	if err != nil {
		t.Fatalf("NewFetcher: %v", err)
	}
	// Allow httptest.Server loopback connections in non-SSRF tests.
	f.ipBlocked = func(net.IP) bool { return false }
	return f
}

func TestFetchExtractsOpenGraphAndThumbnail(t *testing.T) {
	pngBytes := makeTestPNG(640, 320)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/article":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<!doctype html>
<html>
<head>
  <title>Fallback Title</title>
  <meta property="og:title" content="Wisp &amp; Go Preview">
  <meta property="og:description" content="Fast desktop client">
  <meta property="og:image" content="/assets/banner.png">
  <meta property="og:url" content="/canonical-article">
</head>
<body>hello</body>
</html>`)
		case "/assets/banner.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	f := newTestFetcher(t)
	res, err := f.Fetch(context.Background(), srv.URL+"/article")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !res.Found {
		t.Fatal("expected Found == true")
	}
	if res.Title != "Wisp & Go Preview" {
		t.Fatalf("Title = %q, want %q", res.Title, "Wisp & Go Preview")
	}
	if res.Description != "Fast desktop client" {
		t.Fatalf("Description = %q, want %q", res.Description, "Fast desktop client")
	}
	if res.CanonicalURL != srv.URL+"/canonical-article" {
		t.Fatalf("CanonicalURL = %q, want %q", res.CanonicalURL, srv.URL+"/canonical-article")
	}
	if len(res.Thumb) == 0 {
		t.Fatal("expected non-empty JPEG Thumb")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(res.Thumb))
	if err != nil {
		t.Fatalf("DecodeConfig(Thumb): %v", err)
	}
	if format != "jpeg" {
		t.Fatalf("Thumb format = %q, want jpeg", format)
	}
	if cfg.Width > 320 || cfg.Width <= 0 {
		t.Fatalf("Thumb width = %d, want 1..320", cfg.Width)
	}
	if _, err := jpeg.Decode(bytes.NewReader(res.Thumb)); err != nil {
		t.Fatalf("jpeg.Decode(Thumb): %v", err)
	}
}

func TestFetchTextOnlyWithoutImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html>
<html>
<head>
  <title>  Plain Page Title  </title>
  <meta name="description" content="Plain meta description">
</head>
<body>text</body>
</html>`)
	}))
	defer srv.Close()

	f := newTestFetcher(t)
	res, err := f.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !res.Found {
		t.Fatal("expected Found == true")
	}
	if res.Title != "Plain Page Title" {
		t.Fatalf("Title = %q, want %q", res.Title, "Plain Page Title")
	}
	if res.Description != "Plain meta description" {
		t.Fatalf("Description = %q, want %q", res.Description, "Plain meta description")
	}
	if res.Thumb != nil {
		t.Fatalf("Thumb = %d bytes, want nil", len(res.Thumb))
	}
}

func TestFetchNoMetadataReturnsFoundFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><html><head></head><body><p>Nothing here</p></body></html>`)
	}))
	defer srv.Close()

	f := newTestFetcher(t)
	res, err := f.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v, want nil", err)
	}
	if res.Found {
		t.Fatalf("Found = true (%+v), want false", res)
	}
}

func TestFetchBlocksSSRFDirectAndRedirects(t *testing.T) {
	t.Run("direct_blocks_loopback_and_link_local", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			fmt.Fprint(w, `<title>secret</title>`)
		}))
		defer srv.Close()

		f, err := NewFetcher("")
		if err != nil {
			t.Fatalf("NewFetcher: %v", err)
		}
		for _, target := range []string{
			srv.URL,
			"http://169.254.169.254/latest/meta-data/",
			"http://[::1]:8080/",
			"http://[::ffff:127.0.0.1]:8080/",
			"http://10.0.0.1/admin",
			"file:///etc/passwd",
		} {
			if _, err := f.Fetch(context.Background(), target); err == nil {
				t.Errorf("Fetch(%q) succeeded, want SSRF/scheme block", target)
			}
		}
		if hits.Load() != 0 {
			t.Fatalf("loopback server hit %d times, want 0", hits.Load())
		}
	})

	t.Run("direct_blocks_redirect_to_loopback", func(t *testing.T) {
		var targetHits atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			targetHits.Add(1)
			fmt.Fprint(w, `<title>internal</title>`)
		}))
		defer target.Close()

		redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/secret", http.StatusFound)
		}))
		defer redir.Close()

		redirAddr := redir.Listener.Addr().String()
		_, redirPort, err := net.SplitHostPort(redirAddr)
		if err != nil {
			t.Fatalf("SplitHostPort: %v", err)
		}

		f, err := NewFetcher("")
		if err != nil {
			t.Fatalf("NewFetcher: %v", err)
		}
		// Simulate "public.example" resolving to a public IP (93.184.216.34)
		// and route that public IP:port to our local redirect server.
		// The redirect target (127.0.0.1) still hits the real isBlockedIP check.
		f.lookupIP = func(ctx context.Context, host string) ([]net.IPAddr, error) {
			if host == "public.example" {
				return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
			}
			return net.DefaultResolver.LookupIPAddr(ctx, host)
		}
		baseDial := (&net.Dialer{}).DialContext
		f.dialRaw = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == net.JoinHostPort("93.184.216.34", redirPort) {
				return baseDial(ctx, network, redirAddr)
			}
			return baseDial(ctx, network, addr)
		}
		// Rebuild transport so it uses the injected lookupIP/dialRaw hooks.
		if err := f.SetProxy(""); err != nil {
			t.Fatalf("SetProxy: %v", err)
		}

		if _, err := f.Fetch(context.Background(), "http://public.example:"+redirPort+"/go"); err == nil {
			t.Fatal("Fetch across 302 to 127.0.0.1 succeeded, want SSRF block")
		}
		if targetHits.Load() != 0 {
			t.Fatalf("internal redirect target hit %d times, want 0", targetHits.Load())
		}
	})

	t.Run("direct_blocks_more_than_5_redirects", func(t *testing.T) {
		var hops atomic.Int32
		var srvURL string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := hops.Add(1)
			http.Redirect(w, r, fmt.Sprintf("%s/hop/%d", srvURL, n), http.StatusFound)
		}))
		defer srv.Close()
		srvURL = srv.URL

		f := newTestFetcher(t)
		if _, err := f.Fetch(context.Background(), srvURL+"/start"); err == nil {
			t.Fatal("expected error when exceeding 5 redirects")
		}
		if hops.Load() > 6 {
			t.Fatalf("server saw %d hops, expected <= 6 (1 initial + 5 redirects)", hops.Load())
		}
	})

	t.Run("proxied_allows_local_proxy_blocks_local_targets_and_redirects", func(t *testing.T) {
		var proxyHits atomic.Int32
		var lastTarget string
		proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proxyHits.Add(1)
			lastTarget = r.URL.String()
			switch r.URL.Path {
			case "/ok":
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, `<meta property="og:title" content="Via Proxy">`)
			case "/redir-local":
				http.Redirect(w, r, "http://localhost/internal", http.StatusFound)
			case "/redir-dotlocal":
				http.Redirect(w, r, "http://printer.local/admin", http.StatusFound)
			case "/redir-private":
				http.Redirect(w, r, "http://10.0.0.5/secret", http.StatusFound)
			default:
				http.NotFound(w, r)
			}
		}))
		defer proxySrv.Close()

		f, err := NewFetcher("")
		if err != nil {
			t.Fatalf("NewFetcher: %v", err)
		}
		if err := f.SetProxy(proxySrv.URL); err != nil {
			t.Fatalf("SetProxy(%q): %v", proxySrv.URL, err)
		}

		// Public domain succeeds via the 127.0.0.1 proxy without local DNS lookup.
		res, err := f.Fetch(context.Background(), "http://nonexistent.example.invalid/ok")
		if err != nil {
			t.Fatalf("proxied Fetch failed: %v", err)
		}
		if !res.Found || res.Title != "Via Proxy" {
			t.Fatalf("proxied Fetch = %+v, want Found=true Title='Via Proxy'", res)
		}
		if proxyHits.Load() != 1 {
			t.Fatalf("proxyHits = %d, want 1", proxyHits.Load())
		}

		// Local / private / .local targets must be rejected before hitting the proxy.
		hitsBefore := proxyHits.Load()
		for _, blocked := range []string{
			"http://localhost/admin",
			"http://sub.localhost/admin",
			"http://printer.local/status",
			"http://local/status",
			"http://127.0.0.1:8080/",
			"http://10.1.2.3/",
			"http://192.168.1.1/",
			"http://169.254.169.254/",
			"http://[::1]/",
		} {
			if _, err := f.Fetch(context.Background(), blocked); err == nil {
				t.Errorf("proxied Fetch(%q) succeeded, want block", blocked)
			}
		}
		if proxyHits.Load() != hitsBefore {
			t.Fatalf("proxy was hit for blocked local targets (last=%q)", lastTarget)
		}

		// Redirects to localhost / .local / private IP must be rejected by CheckRedirect.
		for _, redirPath := range []string{"/redir-local", "/redir-dotlocal", "/redir-private"} {
			before := proxyHits.Load()
			if _, err := f.Fetch(context.Background(), "http://nonexistent.example.invalid"+redirPath); err == nil {
				t.Errorf("proxied redirect %s succeeded, want block", redirPath)
			}
			// Only the initial request should hit the proxy, never the redirected target.
			if got := proxyHits.Load() - before; got != 1 {
				t.Fatalf("redirect %s caused %d proxy hits, want 1 (last=%q)", redirPath, got, lastTarget)
			}
		}
	})

	t.Run("socks5_proxy_sends_hostname_without_local_dns_and_blocks_local_targets", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		defer ln.Close()

		var socksHits atomic.Int32
		var requestedHost string
		done := make(chan struct{})
		go func() {
			defer close(done)
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			socksHits.Add(1)

			// SOCKS5 auth greeting: ver, nmethods, methods...
			hdr := make([]byte, 2)
			if _, err := io.ReadFull(conn, hdr); err != nil {
				return
			}
			methods := make([]byte, hdr[1])
			if _, err := io.ReadFull(conn, methods); err != nil {
				return
			}
			_, _ = conn.Write([]byte{0x05, 0x00}) // no auth

			// SOCKS5 CONNECT request: ver, cmd, rsv, atyp
			req := make([]byte, 4)
			if _, err := io.ReadFull(conn, req); err != nil {
				return
			}
			if req[3] == 0x03 { // domain name
				lnBuf := make([]byte, 1)
				if _, err := io.ReadFull(conn, lnBuf); err != nil {
					return
				}
				hostBuf := make([]byte, lnBuf[0]+2) // domain + 2-byte port
				if _, err := io.ReadFull(conn, hostBuf); err != nil {
					return
				}
				requestedHost = string(hostBuf[:lnBuf[0]])
			}
			_, _ = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

			// Read HTTP request and respond with OpenGraph HTML.
			buf := make([]byte, 1024)
			_, _ = conn.Read(buf)
			body := `<meta property="og:title" content="SOCKS5 Preview">`
			fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		}()

		f, err := NewFetcher("socks5://" + ln.Addr().String())
		if err != nil {
			t.Fatalf("NewFetcher(socks5): %v", err)
		}

		// Local targets must be rejected before dialing the SOCKS5 proxy.
		for _, blocked := range []string{"http://localhost/x", "http://box.local/x", "http://127.0.0.1/x"} {
			if _, err := f.Fetch(context.Background(), blocked); err == nil {
				t.Errorf("socks5 Fetch(%q) succeeded, want block", blocked)
			}
		}
		if socksHits.Load() != 0 {
			t.Fatalf("SOCKS5 proxy dialed %d times for blocked hosts, want 0", socksHits.Load())
		}

		res, err := f.Fetch(context.Background(), "http://nonexistent.socks5.invalid/article")
		if err != nil {
			t.Fatalf("socks5 Fetch: %v", err)
		}
		<-done
		if requestedHost != "nonexistent.socks5.invalid" {
			t.Fatalf("SOCKS5 requested host = %q, want %q", requestedHost, "nonexistent.socks5.invalid")
		}
		if !res.Found || res.Title != "SOCKS5 Preview" {
			t.Fatalf("socks5 result = %+v, want Found=true Title='SOCKS5 Preview'", res)
		}
	})

	t.Run("invalid_proxy_rejected_without_clobbering", func(t *testing.T) {
		f, err := NewFetcher("http://127.0.0.1:8080")
		if err != nil {
			t.Fatalf("NewFetcher: %v", err)
		}
		if err := f.SetProxy("ftp://bad:21"); err == nil {
			t.Fatal("expected error for invalid proxy")
		}
		if _, err := NewFetcher("socks4://127.0.0.1:1080"); err == nil {
			t.Fatal("expected error from NewFetcher for invalid proxy")
		}
		// Existing valid proxy should remain intact.
		u, _ := url.Parse("http://127.0.0.1:8080")
		if f.proxyURL == nil || f.proxyURL.String() != u.String() {
			t.Fatalf("proxyURL = %v, want %v", f.proxyURL, u)
		}
	})
}

func TestFetchCapsOversizedHTMLAndImage(t *testing.T) {
	t.Run("html_stops_at_512kb", func(t *testing.T) {
		pad := strings.Repeat("a", 1024*1024) // 1 MB
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			switch r.URL.Path {
			case "/head-first":
				fmt.Fprintf(w, `<head><meta property="og:title" content="Before Cap"></head><body>%s</body>`, pad)
			case "/head-after":
				fmt.Fprintf(w, `<body>%s</body><head><meta property="og:title" content="After Cap"></head>`, pad)
			}
		}))
		defer srv.Close()

		f := newTestFetcher(t)
		res1, err := f.Fetch(context.Background(), srv.URL+"/head-first")
		if err != nil {
			t.Fatalf("Fetch head-first: %v", err)
		}
		if !res1.Found || res1.Title != "Before Cap" {
			t.Fatalf("head-first = %+v, want Found=true Title='Before Cap'", res1)
		}

		res2, err := f.Fetch(context.Background(), srv.URL+"/head-after")
		if err != nil {
			t.Fatalf("Fetch head-after: %v", err)
		}
		if res2.Found {
			t.Fatalf("head-after = %+v, want Found=false because metadata is past 512 KB cap", res2)
		}
	})

	t.Run("oversized_image_degrades_to_text_only", func(t *testing.T) {
		oversized := make([]byte, 2*1024*1024+100)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/page":
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, `<head>
  <meta property="og:title" content="Big Image Page">
  <meta property="og:description" content="Still has text">
  <meta property="og:image" content="/huge.png">
</head>`)
			case "/huge.png":
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write(oversized)
			}
		}))
		defer srv.Close()

		f := newTestFetcher(t)
		res, err := f.Fetch(context.Background(), srv.URL+"/page")
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if !res.Found {
			t.Fatal("expected Found == true")
		}
		if res.Title != "Big Image Page" || res.Description != "Still has text" {
			t.Fatalf("unexpected text metadata: %+v", res)
		}
		if res.Thumb != nil {
			t.Fatalf("expected Thumb == nil when image > 2 MB, got %d bytes", len(res.Thumb))
		}
	})
}
