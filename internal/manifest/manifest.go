// Package manifest defines the signed release manifest and the rules a client
// uses to decide whether a release may be installed.
package manifest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// MaxEnvelopeSize bounds how much a client reads before verifying.
	MaxEnvelopeSize = 64 << 10

	// ArtifactPathPrefix is the only location artifacts may be served from,
	// relative to the update server's base URL.
	ArtifactPathPrefix = "/v1/artifacts/"

	// signingContext binds signatures to this purpose so a key reused
	// elsewhere cannot have its signatures replayed as manifests.
	signingContext = "nametag-updater-manifest-v1\n"
)

var (
	ErrNoKeys       = errors.New("no trusted signing keys configured")
	ErrTooLarge     = errors.New("manifest exceeds size limit")
	ErrUnknownKey   = errors.New("manifest signed by an unknown key")
	ErrBadSignature = errors.New("manifest signature is invalid")
)

// Envelope is the wire format: the manifest bytes exactly as signed, plus the
// signature over them. Clients parse Payload only after verifying Sig.
type Envelope struct {
	KeyID   string `json:"key_id"`
	Payload []byte `json:"payload"`
	Sig     []byte `json:"sig"`
}

// Manifest describes one release. Unknown fields are ignored so that older
// clients keep working when fields are added.
type Manifest struct {
	Version    Version             `json:"version"`
	ReleasedAt time.Time           `json:"released_at"`
	ExpiresAt  time.Time           `json:"expires_at"`
	MinVersion *Version            `json:"min_version,omitempty"`
	Revoked    []Version           `json:"revoked,omitempty"`
	Artifacts  map[string]Artifact `json:"artifacts"`
}

// Artifact is one platform's executable, keyed in Manifest.Artifacts by
// "GOOS/GOARCH".
type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Sign validates m and returns a signed envelope ready to publish.
func Sign(m *Manifest, keyID string, priv ed25519.PrivateKey) ([]byte, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	env := Envelope{KeyID: keyID, Payload: payload, Sig: ed25519.Sign(priv, signedMessage(payload))}
	return json.MarshalIndent(env, "", "  ")
}

// Verify checks the envelope signature against keys and only then parses and
// validates the manifest it carries.
func Verify(data []byte, keys KeyRing) (*Manifest, error) {
	if len(keys) == 0 {
		return nil, ErrNoKeys
	}
	if len(data) > MaxEnvelopeSize {
		return nil, ErrTooLarge
	}
	env, err := decodeEnvelope(data)
	if err != nil {
		return nil, err
	}
	pub, ok := keys[env.KeyID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKey, env.KeyID)
	}
	if !ed25519.Verify(pub, signedMessage(env.Payload), env.Sig) {
		return nil, ErrBadSignature
	}
	return parsePayload(env.Payload)
}

// ReadUnverified parses an envelope without checking its signature. It is for
// tooling and status pages only and must never drive an install decision.
func ReadUnverified(data []byte) (*Manifest, error) {
	env, err := decodeEnvelope(data)
	if err != nil {
		return nil, err
	}
	return parsePayload(env.Payload)
}

func decodeEnvelope(data []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, fmt.Errorf("decode manifest envelope: %w", err)
	}
	return env, nil
}

func parsePayload(payload []byte) (*Manifest, error) {
	var m Manifest
	if err := json.NewDecoder(bytes.NewReader(payload)).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func signedMessage(payload []byte) []byte {
	return append([]byte(signingContext), payload...)
}

var platformPattern = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+$`)

func (m *Manifest) validate() error {
	if m.Version.IsZero() {
		return errors.New("invalid manifest: version is required")
	}
	if m.ExpiresAt.IsZero() {
		return errors.New("invalid manifest: expires_at is required")
	}
	if len(m.Artifacts) == 0 {
		return errors.New("invalid manifest: no artifacts")
	}
	for platform, a := range m.Artifacts {
		if !platformPattern.MatchString(platform) {
			return fmt.Errorf("invalid manifest: bad platform %q", platform)
		}
		if err := a.validate(); err != nil {
			return fmt.Errorf("invalid manifest: artifact %s: %w", platform, err)
		}
	}
	return nil
}

func (a Artifact) validate() error {
	if !strings.HasPrefix(a.Path, ArtifactPathPrefix) ||
		strings.Contains(a.Path, "..") ||
		strings.Contains(a.Path, "//") ||
		strings.ContainsAny(a.Path, `\?#%`) {
		return fmt.Errorf("path %q is not under %s", a.Path, ArtifactPathPrefix)
	}
	if a.Size <= 0 {
		return fmt.Errorf("size %d must be positive", a.Size)
	}
	if b, err := hex.DecodeString(a.SHA256); err != nil || len(b) != sha256.Size {
		return fmt.Errorf("sha256 %q is not a hex SHA-256 digest", a.SHA256)
	}
	return nil
}
