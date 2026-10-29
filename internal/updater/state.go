package updater

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

	"nametag-updater/internal/fsutil"
	"nametag-updater/internal/manifest"
)

// State is what the updater remembers across restarts.
type State struct {
	// InstallID is a random, stable identifier for this install. It lets the
	// server do staged rollouts without identifying the user.
	InstallID string `json:"install_id"`

	// HighWater is the newest version confirmed healthy on this install.
	// It only moves forward, which blocks downgrade and replay attacks.
	HighWater manifest.Version `json:"high_water"`

	// Blocked lists versions that were rolled back on this install.
	Blocked []manifest.Version `json:"blocked,omitempty"`

	// Pending is set while a freshly installed version has not yet been
	// confirmed healthy.
	Pending *Pending `json:"pending,omitempty"`
}

// Pending tracks an installed but unconfirmed version.
type Pending struct {
	Version    manifest.Version `json:"version"`
	From       manifest.Version `json:"from"`
	Boots      int              `json:"boots"`
	SHA256     string           `json:"sha256,omitempty"`
	FromSHA256 string           `json:"from_sha256,omitempty"`
}

func (s *State) isBlocked(v manifest.Version) bool {
	return slices.Contains(s.Blocked, v)
}

func loadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read state: %w", err)
	}
	var s *State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("parse state %s: %w", path, err)
	}
	if s == nil {
		return State{}, fmt.Errorf("parse state %s: expected an object", path)
	}
	if p := s.Pending; p != nil && (p.Boots < 0 || !p.From.Less(p.Version)) {
		return State{}, fmt.Errorf("parse state %s: invalid pending update", path)
	}
	return *s, nil
}

func saveState(path string, s State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, data, 0o600)
}

func newInstallID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b)
}
