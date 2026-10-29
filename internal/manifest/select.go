package manifest

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

var (
	ErrNotNewer   = errors.New("release is not newer than the installed version")
	ErrExpired    = errors.New("manifest has expired")
	ErrRevoked    = errors.New("release is revoked")
	ErrNoArtifact = errors.New("release has no artifact for this platform")
)

// Installed describes the client deciding whether to take a release.
type Installed struct {
	Version Version
	// HighWater is the newest version this install has ever confirmed
	// healthy. Releases at or below it are treated as replays.
	HighWater Version
	// Platform is "GOOS/GOARCH".
	Platform string
}

// Select returns the artifact to install, or an error saying why this
// release does not apply. ErrNotNewer is the normal "up to date" case.
func (m *Manifest) Select(in Installed, now time.Time) (Artifact, error) {
	if !now.Before(m.ExpiresAt) {
		return Artifact{}, fmt.Errorf("%w at %s", ErrExpired, m.ExpiresAt.Format(time.RFC3339))
	}
	floor := in.Version
	if floor.Less(in.HighWater) {
		floor = in.HighWater
	}
	if m.Version.Compare(floor) <= 0 {
		return Artifact{}, ErrNotNewer
	}
	if m.IsRevoked(m.Version) {
		return Artifact{}, fmt.Errorf("%w: %s", ErrRevoked, m.Version)
	}
	a, ok := m.Artifacts[in.Platform]
	if !ok {
		return Artifact{}, fmt.Errorf("%w: %s", ErrNoArtifact, in.Platform)
	}
	return a, nil
}

// IsRevoked reports whether v is listed as revoked.
func (m *Manifest) IsRevoked(v Version) bool {
	return slices.Contains(m.Revoked, v)
}

// Mandatory reports whether the installed version must update immediately,
// overriding any request from the application to defer.
func (m *Manifest) Mandatory(installed Version) bool {
	if m.IsRevoked(installed) {
		return true
	}
	return m.MinVersion != nil && installed.Less(*m.MinVersion)
}
