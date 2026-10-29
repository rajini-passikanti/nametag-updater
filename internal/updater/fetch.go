package updater

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"nametag-updater/internal/fsutil"
	"nametag-updater/internal/manifest"
)

const (
	manifestTimeout = 30 * time.Second
	downloadTimeout = 10 * time.Minute
	maxRedirects    = 3
)

var (
	errSizeMismatch = errors.New("artifact size does not match manifest")
	errHashMismatch = errors.New("artifact sha256 does not match manifest")
)

func defaultHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	t.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: t, CheckRedirect: sameOriginRedirects}
}

func sameOriginRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("refusing redirect to another origin: %s", req.URL.Redacted())
	}
	return nil
}

// fetchManifest returns the verified manifest for this client's channel, or
// nil if the channel has no release yet.
func (u *Updater) fetchManifest(ctx context.Context, installID string) (*manifest.Manifest, error) {
	ctx, cancel := context.WithTimeout(ctx, manifestTimeout)
	defer cancel()

	endpoint := u.base.JoinPath("v1", "manifest")
	endpoint.RawQuery = url.Values{
		"channel":    {u.cfg.Channel},
		"version":    {u.cfg.Current.String()},
		"os":         {runtime.GOOS},
		"arch":       {runtime.GOARCH},
		"install_id": {installID},
	}.Encode()

	resp, err := u.get(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		u.log.Debug("no release published", "channel", u.cfg.Channel)
		return nil, nil
	default:
		return nil, fmt.Errorf("fetch manifest: unexpected status %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, manifest.MaxEnvelopeSize+1))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	return manifest.Verify(body, u.cfg.Keys)
}

// download streams art into a temporary file beside the executable,
// enforcing the signed size and digest. It returns the temp file's path.
func (u *Updater) download(ctx context.Context, art manifest.Artifact) (path string, err error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	resp, err := u.get(ctx, u.base.JoinPath(art.Path))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %s", resp.Status)
	}

	// The temp file must be in the executable's directory so the final
	// rename stays on one filesystem, and keep its extension so Windows
	// will run it for the self-test.
	exe := u.cfg.Executable
	f, err := os.CreateTemp(filepath.Dir(exe), fsutil.TempPrefix(exe)+"*"+filepath.Ext(exe))
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, art.Size+1))
	if err != nil {
		return "", fmt.Errorf("write artifact: %w", err)
	}
	if n != art.Size {
		return "", fmt.Errorf("%w: got at least %d bytes, want %d", errSizeMismatch, n, art.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, art.SHA256) {
		return "", fmt.Errorf("%w: got %s", errHashMismatch, got)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("sync artifact: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close artifact: %w", err)
	}
	return f.Name(), nil
}

func (u *Updater) get(ctx context.Context, target *url.URL) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "nametag-updater/"+u.cfg.Current.String())
	return u.cfg.HTTPClient.Do(req)
}
