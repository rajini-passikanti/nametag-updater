package manifest

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func testKey(t *testing.T) (KeyRing, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return KeyRing{"k1": pub}, priv
}

func testManifest() *Manifest {
	return &Manifest{
		Version:    Version{1, 2, 0},
		ReleasedAt: now,
		ExpiresAt:  now.Add(24 * time.Hour),
		Artifacts: map[string]Artifact{
			"linux/amd64": {
				Path:   "/v1/artifacts/1.2.0/app-linux-amd64",
				SHA256: strings.Repeat("ab", 32),
				Size:   1024,
			},
		},
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	keys, priv := testKey(t)
	env, err := Sign(testManifest(), "k1", priv)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Verify(env, keys)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != (Version{1, 2, 0}) || m.Artifacts["linux/amd64"].Size != 1024 {
		t.Fatalf("round trip mismatch: %+v", m)
	}
}

func TestVerifyRejects(t *testing.T) {
	keys, priv := testKey(t)
	otherKeys, _ := testKey(t)
	good, err := Sign(testManifest(), "k1", priv)
	if err != nil {
		t.Fatal(err)
	}

	tampered := func() []byte {
		var env Envelope
		json.Unmarshal(good, &env)
		env.Payload = bytes.Replace(env.Payload, []byte(`"1.2.0"`), []byte(`"9.9.9"`), 1)
		b, _ := json.Marshal(env)
		return b
	}()
	unknownKeyID := func() []byte {
		var env Envelope
		json.Unmarshal(good, &env)
		env.KeyID = "k2"
		b, _ := json.Marshal(env)
		return b
	}()

	tests := []struct {
		name string
		data []byte
		keys KeyRing
		want error
	}{
		{"tampered payload", tampered, keys, ErrBadSignature},
		{"wrong key", good, otherKeys, ErrBadSignature},
		{"unknown key id", unknownKeyID, keys, ErrUnknownKey},
		{"no trusted keys", good, KeyRing{}, ErrNoKeys},
		{"oversized", make([]byte, MaxEnvelopeSize+1), keys, ErrTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Verify(tt.data, tt.keys); !errors.Is(err, tt.want) {
				t.Fatalf("Verify error = %v, want %v", err, tt.want)
			}
		})
	}

	t.Run("not json", func(t *testing.T) {
		if _, err := Verify([]byte("garbage"), keys); err == nil {
			t.Fatal("expected error")
		}
	})
}

// A signed payload is only trusted after validation, so even a correctly
// signed manifest with a dangerous artifact path is refused.
func TestVerifyRejectsInvalidSignedPayload(t *testing.T) {
	keys, priv := testKey(t)
	for name, path := range map[string]string{
		"traversal":      "/v1/artifacts/../../etc/passwd",
		"absolute url":   "https://evil.example/app",
		"outside prefix": "/static/app",
		"encoded":        "/v1/artifacts/1.2.0/%2e%2e/app",
	} {
		t.Run(name, func(t *testing.T) {
			payload, _ := json.Marshal(map[string]any{
				"version":    "1.2.0",
				"expires_at": now.Add(time.Hour),
				"artifacts": map[string]any{
					"linux/amd64": map[string]any{"path": path, "sha256": strings.Repeat("ab", 32), "size": 1},
				},
			})
			env, _ := json.Marshal(Envelope{KeyID: "k1", Payload: payload, Sig: ed25519.Sign(priv, signedMessage(payload))})
			if _, err := Verify(env, keys); err == nil {
				t.Fatal("expected invalid manifest error")
			}
		})
	}
}

func TestVerifyIgnoresUnknownFields(t *testing.T) {
	keys, priv := testKey(t)
	payload, _ := json.Marshal(map[string]any{
		"version":        "1.2.0",
		"expires_at":     now.Add(time.Hour),
		"rollout_cohort": 7,
		"artifacts": map[string]any{
			"linux/amd64": map[string]any{"path": "/v1/artifacts/1.2.0/app", "sha256": strings.Repeat("ab", 32), "size": 1, "future": true},
		},
	})
	env, _ := json.Marshal(Envelope{KeyID: "k1", Payload: payload, Sig: ed25519.Sign(priv, signedMessage(payload))})
	if _, err := Verify(env, keys); err != nil {
		t.Fatalf("unknown fields should be ignored: %v", err)
	}
}

func TestSignatureIsBoundToContext(t *testing.T) {
	keys, priv := testKey(t)
	payload, _ := json.Marshal(testManifest())
	// Signed without the domain-separation prefix, as another system using
	// the same key might do.
	env, _ := json.Marshal(Envelope{KeyID: "k1", Payload: payload, Sig: ed25519.Sign(priv, payload)})
	if _, err := Verify(env, keys); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("got %v, want ErrBadSignature", err)
	}
}

func TestSelect(t *testing.T) {
	v := func(s string) Version {
		parsed, err := ParseVersion(s)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	tests := []struct {
		name      string
		edit      func(*Manifest)
		installed Installed
		at        time.Time
		want      error
	}{
		{"newer", nil, Installed{Version: v("1.1.0"), Platform: "linux/amd64"}, now, nil},
		{"same version", nil, Installed{Version: v("1.2.0"), Platform: "linux/amd64"}, now, ErrNotNewer},
		{"downgrade", nil, Installed{Version: v("2.0.0"), Platform: "linux/amd64"}, now, ErrNotNewer},
		{"below high water", nil, Installed{Version: v("1.0.0"), HighWater: v("1.3.0"), Platform: "linux/amd64"}, now, ErrNotNewer},
		{"expired", nil, Installed{Version: v("1.1.0"), Platform: "linux/amd64"}, now.Add(48 * time.Hour), ErrExpired},
		{"revoked", func(m *Manifest) { m.Revoked = []Version{v("1.2.0")} }, Installed{Version: v("1.1.0"), Platform: "linux/amd64"}, now, ErrRevoked},
		{"no artifact", nil, Installed{Version: v("1.1.0"), Platform: "plan9/amd64"}, now, ErrNoArtifact},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testManifest()
			if tt.edit != nil {
				tt.edit(m)
			}
			_, err := m.Select(tt.installed, tt.at)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Select error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestMandatory(t *testing.T) {
	m := testManifest()
	min := Version{1, 1, 0}
	m.MinVersion = &min
	m.Revoked = []Version{{1, 1, 5}}

	for installed, want := range map[Version]bool{
		{1, 0, 9}: true,  // below minimum
		{1, 1, 5}: true,  // revoked
		{1, 1, 6}: false, // supported
	} {
		if got := m.Mandatory(installed); got != want {
			t.Errorf("Mandatory(%s) = %v, want %v", installed, got, want)
		}
	}
}
