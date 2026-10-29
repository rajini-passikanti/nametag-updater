package manifest

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fuzzKey is fixed so the fuzzer can explore payloads that carry a valid
// signature, which is where the validation logic has to hold on its own.
var fuzzKey = ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))

// FuzzVerifyEnvelope feeds arbitrary bytes to Verify. It must never panic,
// and since the fuzzer cannot forge signatures, it must never accept input
// that was not signed by the trusted key.
func FuzzVerifyEnvelope(f *testing.F) {
	good, _ := Sign(testManifest(), "k1", fuzzKey)
	f.Add(good)
	f.Add([]byte(`{"key_id":"k1","payload":"e30=","sig":""}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))

	keys := KeyRing{"k1": fuzzKey.Public().(ed25519.PublicKey)}
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := Verify(data, keys)
		if err != nil {
			return
		}
		var env Envelope
		if json.Unmarshal(data, &env) != nil || !ed25519.Verify(keys[env.KeyID], signedMessage(env.Payload), env.Sig) {
			t.Fatalf("accepted an envelope without a valid signature: %q", data)
		}
		assertSafe(t, m)
	})
}

// FuzzSignedPayload signs arbitrary payloads with the trusted key, as a
// careless or compromised release pipeline might. Whatever Verify accepts
// must still satisfy the invariants the client relies on.
func FuzzSignedPayload(f *testing.F) {
	seed, _ := json.Marshal(testManifest())
	f.Add(seed)
	f.Add([]byte(`{"version":"1.0.0","expires_at":"2030-01-01T00:00:00Z","artifacts":{"linux/amd64":{"path":"/v1/artifacts/../x","sha256":"00","size":1}}}`))
	f.Add([]byte(`{"version":"0.0.0"}`))

	keys := KeyRing{"k1": fuzzKey.Public().(ed25519.PublicKey)}
	f.Fuzz(func(t *testing.T, payload []byte) {
		env, _ := json.Marshal(Envelope{KeyID: "k1", Payload: payload, Sig: ed25519.Sign(fuzzKey, signedMessage(payload))})
		m, err := Verify(env, keys)
		if err != nil {
			return
		}
		assertSafe(t, m)
		// Select must not panic on any accepted manifest.
		m.Select(Installed{Platform: "linux/amd64"}, time.Now())
		m.Mandatory(Version{})
	})
}

func assertSafe(t *testing.T, m *Manifest) {
	t.Helper()
	if m.Version.IsZero() || m.ExpiresAt.IsZero() || len(m.Artifacts) == 0 {
		t.Fatalf("accepted incomplete manifest: %+v", m)
	}
	for platform, a := range m.Artifacts {
		if !strings.HasPrefix(a.Path, ArtifactPathPrefix) || strings.Contains(a.Path, "..") || strings.Contains(a.Path, "//") {
			t.Fatalf("accepted unsafe path %q for %s", a.Path, platform)
		}
		if a.Size <= 0 || len(a.SHA256) != 64 {
			t.Fatalf("accepted bad size or digest for %s: %+v", platform, a)
		}
	}
}

// FuzzParseVersion checks that every accepted version round-trips exactly,
// so two different strings can never name the same version.
func FuzzParseVersion(f *testing.F) {
	for _, s := range []string{"1.2.3", "0.0.0", "01.2.3", "1.2", "999999999.0.1", "1.2.3-rc1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := ParseVersion(s)
		if err != nil {
			return
		}
		if v.String() != s {
			t.Fatalf("ParseVersion(%q).String() = %q", s, v.String())
		}
	})
}
