package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"wa-go/internal/ui"
)

// maxGIFBytes caps a picked GIF's MP4; GIPHY's messaging renditions are well under 1 MB.
const maxGIFBytes = 8 << 20

var errNotGiphy = errors.New("app: not a GIPHY media URL")

// newGIFClient is the client that downloads picked GIFs: a total timeout, and redirects followed
// only to GIPHY (rt nil: the default transport).
func newGIFClient(rt http.RoundTripper, timeout time.Duration) *http.Client {
	return &http.Client{Transport: rt, Timeout: timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || !ui.AllowedGIFURL(req.URL.String()) {
			return fmt.Errorf("app: refusing a GIF redirect to host %q", req.URL.Hostname())
		}
		return nil
	}}
}

// fetchGIF downloads the GIPHY MP4 at rawURL into a temp file of at most limit bytes and returns
// its path, which the caller removes. The URL is checked against the GIPHY allowlist again first;
// nothing is fetched when it fails.
func fetchGIF(ctx context.Context, c *http.Client, rawURL string, limit int64) (string, error) {
	if !ui.AllowedGIFURL(rawURL) {
		return "", errNotGiphy
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	tooBig := fmt.Errorf("app: GIF larger than %d bytes", limit)
	switch {
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("app: GIF download: %s", resp.Status)
	case resp.ContentLength > limit:
		return "", tooBig
	}
	f, err := os.CreateTemp("", "wa-go-gif-*.mp4")
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > limit {
		err = tooBig
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
