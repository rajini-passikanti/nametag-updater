// Command release generates signing keys and publishes signed releases.
//
//	release keygen  --key-id k1 --out keys
//	release publish --version 1.1.0 --dist dist/1.1.0 --key keys/k1.key --releases releases
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"nametag-updater/internal/manifest"
	"nametag-updater/internal/release"
)

const usage = `usage:
  release keygen  [--key-id k1] [--out keys]
  release publish --version X.Y.Z --dist DIR --key FILE [--releases releases]
                  [--channel stable] [--ttl 720h] [--min-version X.Y.Z] [--revoke X.Y.Z,...]
  release refresh --key FILE [--releases releases] [--channel stable] [--ttl 720h]
                  [--trust FILE.pub]
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "publish":
		err = publish(os.Args[2:])
	case "refresh":
		err = refresh(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	keyID := fs.String("key-id", "k1", "identifier embedded in signatures")
	out := fs.String("out", "keys", "output directory")
	fs.Parse(args)

	keyFile, pubEntry, err := manifest.GenerateKey(*keyID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(keyFile, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		return err
	}
	privPath := filepath.Join(*out, *keyID+".key")
	pubPath := filepath.Join(*out, *keyID+".pub")
	if err := writeNew(privPath, data, 0o600); err != nil {
		return err
	}
	if err := writeNew(pubPath, []byte(pubEntry+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("private key: %s (keep offline; never commit)\npublic key:  %s\n", privPath, pubPath)
	return nil
}

// writeNew refuses to overwrite, so a signing key is never lost by accident.
func writeNew(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s already exists; refusing to overwrite", path)
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func publish(args []string) error {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	versionFlag := fs.String("version", "", "release version (required)")
	dist := fs.String("dist", "", "directory of built artifacts (default dist/<version>)")
	keyPath := fs.String("key", "keys/k1.key", "signing key file")
	releases := fs.String("releases", "releases", "releases directory served by update-server")
	channel := fs.String("channel", "stable", "release channel")
	ttl := fs.Duration("ttl", 30*24*time.Hour, "how long the manifest stays valid")
	minVersion := fs.String("min-version", "", "clients older than this update even if deferring")
	revoke := fs.String("revoke", "", "comma-separated versions that must update immediately")
	fs.Parse(args)

	v, err := manifest.ParseVersion(*versionFlag)
	if err != nil {
		return err
	}
	if *dist == "" {
		*dist = filepath.Join("dist", v.String())
	}
	keyID, priv, err := manifest.LoadPrivateKey(*keyPath)
	if err != nil {
		return err
	}
	opts := release.Options{
		Version:     v,
		DistDir:     *dist,
		ReleasesDir: *releases,
		Channel:     *channel,
		KeyID:       keyID,
		PrivateKey:  priv,
		TTL:         *ttl,
		Now:         time.Now(),
	}
	if *minVersion != "" {
		mv, err := manifest.ParseVersion(*minVersion)
		if err != nil {
			return fmt.Errorf("--min-version: %w", err)
		}
		opts.MinVersion = &mv
	}
	for _, s := range strings.Split(*revoke, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		rv, err := manifest.ParseVersion(s)
		if err != nil {
			return fmt.Errorf("--revoke: %w", err)
		}
		opts.Revoked = append(opts.Revoked, rv)
	}

	m, err := release.Publish(opts)
	if err != nil {
		return err
	}
	platforms := make([]string, 0, len(m.Artifacts))
	for p := range m.Artifacts {
		platforms = append(platforms, p)
	}
	sort.Strings(platforms)
	fmt.Printf("published %s to channel %q (key %s, expires %s)\n  platforms: %s\n",
		m.Version, *channel, keyID, m.ExpiresAt.Format(time.RFC3339), strings.Join(platforms, ", "))
	return nil
}

func refresh(args []string) error {
	fs := flag.NewFlagSet("refresh", flag.ExitOnError)
	keyPath := fs.String("key", "keys/k1.key", "signing key file")
	releases := fs.String("releases", "releases", "releases directory served by update-server")
	channel := fs.String("channel", "stable", "release channel")
	ttl := fs.Duration("ttl", 30*24*time.Hour, "new validity period, from now")
	trust := fs.String("trust", "", "public key file that signed the current manifest (default: the signing key)")
	fs.Parse(args)

	keyID, priv, err := manifest.LoadPrivateKey(*keyPath)
	if err != nil {
		return err
	}
	opts := release.RefreshOptions{
		ReleasesDir: *releases,
		Channel:     *channel,
		KeyID:       keyID,
		PrivateKey:  priv,
		TTL:         *ttl,
		Now:         time.Now(),
	}
	if *trust != "" {
		data, err := os.ReadFile(*trust)
		if err != nil {
			return err
		}
		if opts.Trusted, err = manifest.ParseKeyRing(strings.TrimSpace(string(data))); err != nil {
			return fmt.Errorf("--trust: %w", err)
		}
	}
	m, err := release.Refresh(opts)
	if err != nil {
		return err
	}
	fmt.Printf("re-signed %s on channel %q with key %s; now expires %s\n",
		m.Version, *channel, keyID, m.ExpiresAt.Format(time.RFC3339))
	return nil
}
