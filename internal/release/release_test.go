package release

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nametag-updater/internal/manifest"
	"nametag-updater/internal/server"
)

func setup(t *testing.T) (Options, manifest.KeyRing) {
	t.Helper()
	dist := t.TempDir()
	for _, name := range []string{"app-linux-amd64", "app-windows-amd64.exe", "README.txt"} {
		if err := os.WriteFile(filepath.Join(dist, name), []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	return Options{
		Version:     manifest.Version{Major: 1, Minor: 1},
		DistDir:     dist,
		ReleasesDir: t.TempDir(),
		Channel:     "stable",
		KeyID:       "k1",
		PrivateKey:  priv,
		TTL:         time.Hour,
		Now:         time.Now(),
	}, manifest.KeyRing{"k1": pub}
}

func TestPublishWritesVerifiableRelease(t *testing.T) {
	opts, keys := setup(t)
	if _, err := Publish(opts); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(server.ManifestPath(opts.ReleasesDir, "stable"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Verify(data, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Artifacts) != 2 {
		t.Fatalf("artifacts = %v, want linux and windows only", m.Artifacts)
	}
	win := m.Artifacts["windows/amd64"]
	if win.Path != "/v1/artifacts/1.1.0/app-windows-amd64.exe" || win.Size != int64(len("app-windows-amd64.exe")) {
		t.Fatalf("windows artifact = %+v", win)
	}
	if _, err := os.Stat(filepath.Join(server.ArtifactDir(opts.ReleasesDir, opts.Version), "app-linux-amd64")); err != nil {
		t.Fatalf("artifact not copied: %v", err)
	}
}

func TestPublishRefusesNonNewerVersion(t *testing.T) {
	opts, _ := setup(t)
	if _, err := Publish(opts); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(opts); err == nil {
		t.Fatal("republishing the same version should fail")
	}
	opts.Version = manifest.Version{Major: 1}
	if _, err := Publish(opts); err == nil {
		t.Fatal("publishing an older version should fail")
	}
}

func TestHaltingARolloutAndFixingForward(t *testing.T) {
	opts, keys := setup(t)
	v110, v120, v121 := opts.Version, manifest.Version{Major: 1, Minor: 2}, manifest.Version{Major: 1, Minor: 2, Patch: 1}
	if _, err := Publish(opts); err != nil {
		t.Fatal(err)
	}
	opts.Version = v120
	if _, err := Publish(opts); err != nil {
		t.Fatal(err)
	}

	// Halt 1.2.0 the way the runbook says: restore the 1.1.0 manifest.
	archived, err := os.ReadFile(HistoryPath(opts.ReleasesDir, "stable", v110))
	if err != nil {
		t.Fatalf("1.1.0 manifest not in history: %v", err)
	}
	if _, err := manifest.Verify(archived, keys); err != nil {
		t.Fatalf("archived manifest no longer verifies: %v", err)
	}
	os.WriteFile(server.ManifestPath(opts.ReleasesDir, "stable"), archived, 0o644)

	// 1.2.0 already went live, so its number can never point at new bytes.
	opts.Version = v120
	if _, err := Publish(opts); err == nil {
		t.Fatal("republished 1.2.0 after it had been live")
	}
	opts.Version = v121
	if _, err := Publish(opts); err != nil {
		t.Fatalf("fix-forward release after a halt: %v", err)
	}
}

func TestRefreshExtendsExpiry(t *testing.T) {
	opts, keys := setup(t)
	if _, err := Publish(opts); err != nil {
		t.Fatal(err)
	}
	refresh := RefreshOptions{
		ReleasesDir: opts.ReleasesDir, Channel: "stable", KeyID: "k1", PrivateKey: opts.PrivateKey,
		TTL: 30 * 24 * time.Hour, Now: opts.Now.Add(time.Hour),
	}
	m, err := Refresh(refresh)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(server.ManifestPath(opts.ReleasesDir, "stable"))
	got, err := manifest.Verify(data, keys)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != opts.Version || !got.ExpiresAt.Equal(m.ExpiresAt) || !got.ExpiresAt.After(opts.Now.Add(29*24*time.Hour)) {
		t.Fatalf("refreshed manifest = %+v", got)
	}

	// A tampered artifact must not be re-signed.
	os.WriteFile(filepath.Join(server.ArtifactDir(opts.ReleasesDir, opts.Version), "app-linux-amd64"), []byte("evil"), 0o644)
	if _, err := Refresh(refresh); err == nil {
		t.Fatal("refresh re-signed a tampered artifact")
	}
}

func TestRefreshRequiresTrustedManifest(t *testing.T) {
	opts, _ := setup(t)
	if _, err := Publish(opts); err != nil {
		t.Fatal(err)
	}
	_, otherKey, _ := ed25519.GenerateKey(nil)
	_, err := Refresh(RefreshOptions{
		ReleasesDir: opts.ReleasesDir, Channel: "stable", KeyID: "k2", PrivateKey: otherKey,
		TTL: time.Hour, Now: time.Now(),
	})
	if err == nil {
		t.Fatal("refresh re-signed a manifest it could not verify")
	}
}

func TestPublishValidatesInput(t *testing.T) {
	opts, _ := setup(t)
	empty := opts
	empty.DistDir = t.TempDir()
	if _, err := Publish(empty); err == nil {
		t.Fatal("expected error for dist dir without artifacts")
	}
	badChannel := opts
	badChannel.Channel = "../x"
	if _, err := Publish(badChannel); err == nil {
		t.Fatal("expected error for invalid channel")
	}
	selfRevoke := opts
	selfRevoke.Revoked = []manifest.Version{opts.Version}
	if _, err := Publish(selfRevoke); err == nil {
		t.Fatal("expected error when revoking the version being published")
	}
}
