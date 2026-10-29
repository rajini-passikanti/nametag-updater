package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"nametag-updater/internal/manifest"
)

func newTestServer(t *testing.T) (string, *httptest.Server) {
	t.Helper()
	root := t.TempDir()
	srv := httptest.NewServer(New(root, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(srv.Close)
	return root, srv
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestManifestEndpoint(t *testing.T) {
	root, srv := newTestServer(t)

	if resp := get(t, srv.URL+"/v1/manifest"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no manifest: status %d, want 404", resp.StatusCode)
	}

	path := ManifestPath(root, "stable")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(`{"key_id":"k1"}`), 0o644)

	resp := get(t, srv.URL+"/v1/manifest?channel=stable&version=1.0.0&os=linux&arch=amd64")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}

	if resp := get(t, srv.URL+"/v1/manifest?channel=../secrets"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad channel: status %d, want 400", resp.StatusCode)
	}
}

func TestArtifactEndpoint(t *testing.T) {
	root, srv := newTestServer(t)
	dir := ArtifactDir(root, manifest.Version{Major: 1, Minor: 1})
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "app-linux-amd64"), []byte("binary"), 0o644)
	os.WriteFile(filepath.Join(root, "secret"), []byte("do not serve"), 0o644)

	resp := get(t, srv.URL+"/v1/artifacts/1.1.0/app-linux-amd64")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "binary" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}

	for _, path := range []string{
		"/v1/artifacts/1.1.0/..%2f..%2fsecret",
		"/v1/artifacts/..%2f..%2f/secret",
		"/v1/artifacts/latest/app-linux-amd64",
		"/v1/artifacts/1.1.0/.hidden",
	} {
		resp := get(t, srv.URL+path)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s: served with status 200", path)
		}
	}
	if resp := get(t, srv.URL+"/v1/artifacts/1.1.0/missing"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing artifact: status %d, want 404", resp.StatusCode)
	}
}

func TestStatusEndpoint(t *testing.T) {
	_, srv := newTestServer(t)
	resp := get(t, srv.URL+"/status")
	var body struct {
		Channels map[string]ChannelStatus `json:"channels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Channels == nil {
		t.Fatal("channels missing from status")
	}
}
