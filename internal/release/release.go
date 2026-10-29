// Package release publishes signed releases into a releases directory served
// by the update server.
package release

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"nametag-updater/internal/fsutil"
	"nametag-updater/internal/manifest"
	"nametag-updater/internal/server"
)

// artifactName matches build outputs such as app-linux-amd64 and
// app-windows-amd64.exe.
var artifactName = regexp.MustCompile(`^app-([a-z0-9]+)-([a-z0-9]+)(\.exe)?$`)

// Options describes one release to publish.
type Options struct {
	Version     manifest.Version
	DistDir     string // directory of built artifacts
	ReleasesDir string // directory served by update-server
	Channel     string
	KeyID       string
	PrivateKey  ed25519.PrivateKey
	TTL         time.Duration // how long the manifest stays valid
	MinVersion  *manifest.Version
	Revoked     []manifest.Version
	Now         time.Time
}

// Publish copies the artifacts into place and then writes the signed
// manifest. The manifest is written last and atomically, so clients never
// see a manifest that points at missing or partial artifacts.
func Publish(o Options) (*manifest.Manifest, error) {
	if !server.ChannelPattern.MatchString(o.Channel) {
		return nil, fmt.Errorf("invalid channel %q", o.Channel)
	}
	if o.TTL <= 0 {
		return nil, errors.New("TTL must be positive")
	}
	if slices.Contains(o.Revoked, o.Version) {
		return nil, fmt.Errorf("cannot publish %s while also revoking it", o.Version)
	}
	lock, err := lockReleases(o.ReleasesDir)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	if err := checkNewer(o); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(o.DistDir)
	if err != nil {
		return nil, fmt.Errorf("read dist directory: %w", err)
	}
	destDir := server.ArtifactDir(o.ReleasesDir, o.Version)
	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return nil, err
	}

	stage, err := os.MkdirTemp(filepath.Dir(destDir), ".stage-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)

	m := &manifest.Manifest{
		Version:    o.Version,
		ReleasedAt: o.Now.UTC(),
		ExpiresAt:  o.Now.Add(o.TTL).UTC(),
		MinVersion: o.MinVersion,
		Revoked:    o.Revoked,
		Artifacts:  map[string]manifest.Artifact{},
	}
	for _, e := range entries {
		match := artifactName.FindStringSubmatch(e.Name())
		if match == nil || !e.Type().IsRegular() {
			continue
		}
		platform := match[1] + "/" + match[2]
		if _, exists := m.Artifacts[platform]; exists {
			return nil, fmt.Errorf("duplicate platform %s", platform)
		}
		digest, size, err := copyAndHash(filepath.Join(o.DistDir, e.Name()), filepath.Join(stage, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("publish %s: %w", e.Name(), err)
		}
		m.Artifacts[platform] = manifest.Artifact{
			Path:   path.Join(manifest.ArtifactPathPrefix, o.Version.String(), e.Name()),
			SHA256: digest,
			Size:   size,
		}
	}
	if len(m.Artifacts) == 0 {
		return nil, fmt.Errorf("no artifacts named app-<os>-<arch> found in %s", o.DistDir)
	}

	// Validate before committing any version directory. Existing version bytes
	// are never rewritten, including when promoting to a different channel.
	if _, err := manifest.Sign(m, o.KeyID, o.PrivateKey); err != nil {
		return nil, err
	}
	if err := commitArtifacts(stage, destDir); err != nil {
		return nil, err
	}
	if err := writeManifest(o.ReleasesDir, o.Channel, m, o.KeyID, o.PrivateKey); err != nil {
		return nil, err
	}
	return m, nil
}

// RefreshOptions describes re-signing a channel's current release.
type RefreshOptions struct {
	ReleasesDir string
	Channel     string
	KeyID       string
	PrivateKey  ed25519.PrivateKey
	// Trusted verifies the manifest being refreshed. It defaults to the
	// public half of PrivateKey; pass the old key here when rotating.
	Trusted manifest.KeyRing
	TTL     time.Duration
	Now     time.Time
}

// Refresh re-signs the channel's current manifest with a new expiry, so a
// channel with no new releases does not go stale. It only re-signs content
// that verifies against a trusted key and whose artifacts still match their
// digests, so it cannot be used to launder a tampered releases directory.
func Refresh(o RefreshOptions) (*manifest.Manifest, error) {
	if !server.ChannelPattern.MatchString(o.Channel) {
		return nil, fmt.Errorf("invalid channel %q", o.Channel)
	}
	if o.TTL <= 0 {
		return nil, errors.New("TTL must be positive")
	}
	lock, err := lockReleases(o.ReleasesDir)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	if o.Trusted == nil {
		o.Trusted = manifest.KeyRing{o.KeyID: o.PrivateKey.Public().(ed25519.PublicKey)}
	}
	data, err := os.ReadFile(server.ManifestPath(o.ReleasesDir, o.Channel))
	if err != nil {
		return nil, fmt.Errorf("read current manifest: %w", err)
	}
	m, err := manifest.Verify(data, o.Trusted)
	if err != nil {
		return nil, fmt.Errorf("current %s manifest does not verify: %w", o.Channel, err)
	}
	for platform, a := range m.Artifacts {
		digest, size, err := hashFile(filepath.Join(server.ArtifactDir(o.ReleasesDir, m.Version), path.Base(a.Path)))
		if err != nil {
			return nil, fmt.Errorf("artifact %s: %w", platform, err)
		}
		if digest != a.SHA256 || size != a.Size {
			return nil, fmt.Errorf("artifact %s no longer matches its manifest entry; refusing to re-sign", platform)
		}
	}
	m.ExpiresAt = o.Now.Add(o.TTL).UTC()
	if err := writeManifest(o.ReleasesDir, o.Channel, m, o.KeyID, o.PrivateKey); err != nil {
		return nil, err
	}
	return m, nil
}

// HistoryPath is where the signed manifest for version v on channel is kept.
// Every manifest that goes live is recorded here, which serves two purposes:
// a version number can never be reused, and copying an older entry back over
// the channel manifest halts a rollout (as long as that entry has not expired).
func HistoryPath(root, channel string, v manifest.Version) string {
	return filepath.Join(root, "manifests", "history", channel, v.String()+".json")
}

// writeManifest signs m, records it in the channel history, and then makes
// it the channel's current manifest.
func writeManifest(root, channel string, m *manifest.Manifest, keyID string, priv ed25519.PrivateKey) error {
	envelope, err := manifest.Sign(m, keyID, priv)
	if err != nil {
		return err
	}
	archive := HistoryPath(root, channel, m.Version)
	if err := os.MkdirAll(filepath.Dir(archive), 0o755); err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(archive, envelope, 0o644); err != nil {
		return fmt.Errorf("record manifest history: %w", err)
	}
	if err := fsutil.WriteFileAtomic(server.ManifestPath(root, channel), envelope, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// checkNewer refuses to publish a version that clients would ignore, or one
// that was already live on this channel: artifacts are served as immutable,
// so a version number must never point at different bytes.
func checkNewer(o Options) error {
	if _, err := os.Stat(HistoryPath(o.ReleasesDir, o.Channel, o.Version)); err == nil {
		return fmt.Errorf("version %s was already published on %s; versions are immutable, publish a new one", o.Version, o.Channel)
	}
	data, err := os.ReadFile(server.ManifestPath(o.ReleasesDir, o.Channel))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	current, err := manifest.ReadUnverified(data)
	if err != nil {
		return fmt.Errorf("read current manifest: %w", err)
	}
	if o.Version.Compare(current.Version) <= 0 {
		return fmt.Errorf("version %s is not newer than %s, the current %s release", o.Version, current.Version, o.Channel)
	}
	return nil
}

func copyAndHash(src, dst string) (digest string, size int64, err error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	h := sha256.New()
	err = fsutil.WriteAtomic(dst, 0o644, func(w io.Writer) error {
		size, err = io.Copy(io.MultiWriter(w, h), in)
		return err
	})
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// A busy publisher fails explicitly; callers can retry the entire command.
// Refresh uses this same lock so it cannot overwrite a concurrent publish.
func lockReleases(root string) (*fsutil.Lock, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	l, err := fsutil.TryLock(filepath.Join(root, ".publish.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock releases directory: %w", err)
	}
	return l, nil
}

// commitArtifacts makes a complete version visible at once or verifies an
// identical promotion. The caller must hold the releases-directory lock.
func commitArtifacts(stage, dest string) error {
	info, err := os.Lstat(dest)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Chmod(stage, 0o755); err != nil {
			return err
		}
		if err := fsutil.SyncDir(stage); err != nil {
			return err
		}
		if err := os.Rename(stage, dest); err != nil {
			return err
		}
		return fsutil.SyncDir(filepath.Dir(dest))
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("existing version is not a directory: %s", dest)
	}
	existing, err := os.ReadDir(dest)
	if err != nil {
		return err
	}
	candidate, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	if len(existing) != len(candidate) {
		return errors.New("version artifacts are immutable: artifact set differs")
	}
	for i, e := range existing {
		if !e.Type().IsRegular() || e.Name() != candidate[i].Name() {
			return errors.New("version artifacts are immutable: artifact set differs")
		}
		a, n, err := hashFile(filepath.Join(dest, e.Name()))
		if err != nil {
			return err
		}
		b, m, err := hashFile(filepath.Join(stage, e.Name()))
		if err != nil {
			return err
		}
		if a != b || n != m {
			return fmt.Errorf("version artifacts are immutable: %s differs", e.Name())
		}
	}
	return nil
}
