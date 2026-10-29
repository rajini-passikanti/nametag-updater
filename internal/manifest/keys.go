package manifest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// KeyRing maps a key ID to a trusted release-signing public key.
type KeyRing map[string]ed25519.PublicKey

var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// ParseKeyRing parses "id:base64[,id:base64...]" as produced by
// PublicKeyEntry. An empty string yields an empty ring.
func ParseKeyRing(s string) (KeyRing, error) {
	ring := KeyRing{}
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, encoded, ok := strings.Cut(entry, ":")
		if !ok || !keyIDPattern.MatchString(id) {
			return nil, fmt.Errorf("invalid key ring entry %q: want id:base64", entry)
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid public key for key id %q", id)
		}
		if _, dup := ring[id]; dup {
			return nil, fmt.Errorf("duplicate key id %q", id)
		}
		ring[id] = ed25519.PublicKey(raw)
	}
	return ring, nil
}

// PublicKeyEntry formats a public key as a key ring entry.
func PublicKeyEntry(keyID string, pub ed25519.PublicKey) string {
	return keyID + ":" + base64.StdEncoding.EncodeToString(pub)
}

// PrivateKeyFile is the on-disk format of a release-signing key.
type PrivateKeyFile struct {
	KeyID      string `json:"key_id"`
	PrivateKey []byte `json:"private_key"`
}

// GenerateKey creates a new signing key and returns its file form along with
// the public key ring entry to embed in client builds.
func GenerateKey(keyID string) (PrivateKeyFile, string, error) {
	if !keyIDPattern.MatchString(keyID) {
		return PrivateKeyFile{}, "", fmt.Errorf("invalid key id %q", keyID)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return PrivateKeyFile{}, "", fmt.Errorf("generate key: %w", err)
	}
	return PrivateKeyFile{KeyID: keyID, PrivateKey: priv}, PublicKeyEntry(keyID, pub), nil
}

// LoadPrivateKey reads a key written by GenerateKey.
func LoadPrivateKey(path string) (string, ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("read signing key: %w", err)
	}
	var f PrivateKeyFile
	if err := json.Unmarshal(data, &f); err != nil {
		return "", nil, fmt.Errorf("parse signing key %s: %w", path, err)
	}
	if !keyIDPattern.MatchString(f.KeyID) || len(f.PrivateKey) != ed25519.PrivateKeySize {
		return "", nil, fmt.Errorf("signing key %s is malformed", path)
	}
	return f.KeyID, ed25519.PrivateKey(f.PrivateKey), nil
}
