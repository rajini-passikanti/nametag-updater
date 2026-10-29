package release

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nametag-updater/internal/fsutil"
	"nametag-updater/internal/manifest"
	"nametag-updater/internal/server"
)

func TestPromotionRequiresIdenticalArtifacts(t *testing.T) {
	for _, change := range []string{"identical", "different bytes", "missing artifact", "additional artifact"} {
		t.Run(change, func(t *testing.T) {
			o, keys := setup(t)
			o.Channel = "beta"
			if _, err := Publish(o); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(server.ManifestPath(o.ReleasesDir, "beta"))
			if err != nil {
				t.Fatal(err)
			}
			artifact := filepath.Join(server.ArtifactDir(o.ReleasesDir, o.Version), "app-linux-amd64")
			before, err := os.ReadFile(artifact)
			if err != nil {
				t.Fatal(err)
			}
			o.Channel = "stable"
			switch change {
			case "different bytes":
				if err := os.WriteFile(filepath.Join(o.DistDir, "app-linux-amd64"), []byte("replacement"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "missing artifact":
				if err := os.Remove(filepath.Join(o.DistDir, "app-windows-amd64.exe")); err != nil {
					t.Fatal(err)
				}
			case "additional artifact":
				if err := os.WriteFile(filepath.Join(o.DistDir, "app-darwin-arm64"), []byte("extra"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			_, err = Publish(o)
			if (err == nil) != (change == "identical") {
				t.Fatalf("promotion error = %v", err)
			}
			after, err := os.ReadFile(artifact)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("original artifact changed: %q, %v", after, err)
			}
			current, err := os.ReadFile(server.ManifestPath(o.ReleasesDir, "beta"))
			if err != nil || !bytes.Equal(original, current) {
				t.Fatal("beta manifest changed")
			}
			if _, err := manifest.Verify(current, keys); err != nil {
				t.Fatal(err)
			}
			if change != "identical" {
				if _, err := os.Stat(server.ManifestPath(o.ReleasesDir, "stable")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("conflicting manifest exists: %v", err)
				}
			}
		})
	}
}

func TestConcurrentPublishersCannotReuseVersion(t *testing.T) {
	o, keys := setup(t)
	other := o
	other.DistDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(other.DistDir, "app-linux-amd64"), []byte("conflicting build"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, candidate := range []Options{o, other} {
		wg.Add(1)
		go func(candidate Options) { defer wg.Done(); <-start; _, err := Publish(candidate); results <- err }(candidate)
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful conflicting publications = %d", successes)
	}
	data, err := os.ReadFile(server.ManifestPath(o.ReleasesDir, o.Channel))
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Verify(data, keys)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range m.Artifacts {
		digest, size, err := hashFile(filepath.Join(server.ArtifactDir(o.ReleasesDir, o.Version), filepath.Base(a.Path)))
		if err != nil || digest != a.SHA256 || size != a.Size {
			t.Fatalf("published artifact differs from signed manifest: %v", err)
		}
	}
}

func TestPublishAndRefreshShareLock(t *testing.T) {
	o, _ := setup(t)
	if _, err := Publish(o); err != nil {
		t.Fatal(err)
	}
	lock, err := lockReleases(o.ReleasesDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	o.Version.Patch++
	if _, err := Publish(o); !errors.Is(err, fsutil.ErrLocked) {
		t.Fatalf("publish while locked = %v", err)
	}
	if _, err := Refresh(RefreshOptions{ReleasesDir: o.ReleasesDir, Channel: o.Channel, KeyID: o.KeyID, PrivateKey: o.PrivateKey, TTL: time.Hour, Now: o.Now}); !errors.Is(err, fsutil.ErrLocked) {
		t.Fatalf("refresh while locked = %v", err)
	}
}
