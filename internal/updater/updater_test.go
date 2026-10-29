package updater

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"nametag-updater/internal/fsutil"
	"nametag-updater/internal/manifest"
	"nametag-updater/internal/release"
	"nametag-updater/internal/server"
)

// fixture is a fake install plus a real update server over a temp directory.
type fixture struct {
	t        *testing.T
	exe      string
	releases string
	priv     ed25519.PrivateKey
	keys     manifest.KeyRing
	srv      *httptest.Server
}

var oldBinary = []byte("old binary")

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "install", "app")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	f := &fixture{
		t:        t,
		exe:      exe,
		releases: filepath.Join(dir, "releases"),
		priv:     priv,
		keys:     manifest.KeyRing{"k1": pub},
	}
	f.srv = httptest.NewServer(server.New(f.releases, discard()).Handler())
	t.Cleanup(f.srv.Close)
	return f
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func mustVersion(t *testing.T, s string) manifest.Version {
	t.Helper()
	v, err := manifest.ParseVersion(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (f *fixture) publish(version string, content []byte, edit func(*release.Options)) {
	f.t.Helper()
	dist := f.t.TempDir()
	name := "app-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dist, name), content, 0o755); err != nil {
		f.t.Fatal(err)
	}
	opts := release.Options{
		Version:     mustVersion(f.t, version),
		DistDir:     dist,
		ReleasesDir: f.releases,
		Channel:     "stable",
		KeyID:       "k1",
		PrivateKey:  f.priv,
		TTL:         time.Hour,
		Now:         time.Now(),
	}
	if edit != nil {
		edit(&opts)
	}
	if _, err := release.Publish(opts); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) updater(current string, edit func(*Config)) *Updater {
	f.t.Helper()
	cfg := Config{
		ServerURL:    f.srv.URL,
		Interval:     time.Minute,
		HealthyAfter: 10 * time.Millisecond,
		Executable:   f.exe,
		Current:      mustVersion(f.t, current),
		Keys:         f.keys,
		Logger:       discard(),
	}
	if edit != nil {
		edit(&cfg)
	}
	u, err := New(cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	u.selfTest = func(context.Context, string, manifest.Version) error { return nil }
	return u
}

func (f *fixture) exeContent() []byte {
	f.t.Helper()
	b, err := os.ReadFile(f.exe)
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

func (f *fixture) state(u *Updater) State {
	f.t.Helper()
	s, err := loadState(u.statePath())
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func TestCheckOnceInstallsNewerRelease(t *testing.T) {
	f := newFixture(t)
	f.publish("1.1.0", []byte("new binary"), nil)
	u := f.updater("1.0.0", nil)

	res, err := u.CheckOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Installed || res.Version != mustVersion(t, "1.1.0") {
		t.Fatalf("result = %+v, want installed 1.1.0", res)
	}
	if got := f.exeContent(); !bytes.Equal(got, []byte("new binary")) {
		t.Fatalf("executable = %q, want new binary", got)
	}
	if prev, _ := os.ReadFile(prevPath(f.exe)); !bytes.Equal(prev, oldBinary) {
		t.Fatalf("backup = %q, want old binary", prev)
	}
	st := f.state(u)
	if st.Pending == nil || st.Pending.Version != mustVersion(t, "1.1.0") {
		t.Fatalf("pending = %+v, want 1.1.0", st.Pending)
	}
	if st.InstallID == "" {
		t.Fatal("install id was not assigned")
	}
}

func TestCheckOnceLeavesInstallAlone(t *testing.T) {
	tests := []struct {
		name    string
		current string
		setup   func(*fixture)
		config  func(*Config)
		wantErr error
	}{
		{name: "nothing published", current: "1.0.0", setup: func(*fixture) {}},
		{name: "already newest", current: "1.1.0", setup: func(f *fixture) { f.publish("1.1.0", []byte("new"), nil) }},
		{name: "older release", current: "2.0.0", setup: func(f *fixture) { f.publish("1.1.0", []byte("new"), nil) }},
		{
			name: "tampered artifact", current: "1.0.0", wantErr: errHashMismatch,
			setup: func(f *fixture) {
				f.publish("1.1.0", []byte("new binary"), nil)
				overwriteArtifact(f, "1.1.0", []byte("evil binary")[:10])
			},
		},
		{
			name: "truncated artifact", current: "1.0.0", wantErr: errSizeMismatch,
			setup: func(f *fixture) {
				f.publish("1.1.0", []byte("new binary"), nil)
				overwriteArtifact(f, "1.1.0", []byte("new"))
			},
		},
		{
			name: "untrusted signing key", current: "1.0.0", wantErr: manifest.ErrBadSignature,
			setup: func(f *fixture) {
				_, f.priv, _ = ed25519.GenerateKey(nil)
				f.publish("1.1.0", []byte("new"), nil)
			},
		},
		{
			name: "expired manifest", current: "1.0.0", wantErr: manifest.ErrExpired,
			setup: func(f *fixture) {
				f.publish("1.1.0", []byte("new"), func(o *release.Options) { o.Now = time.Now().Add(-2 * time.Hour) })
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.setup(f)
			u := f.updater(tt.current, tt.config)
			res, err := u.CheckOnce(context.Background())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if res.Installed {
				t.Fatal("unexpected install")
			}
			if got := f.exeContent(); !bytes.Equal(got, oldBinary) {
				t.Fatalf("executable changed to %q", got)
			}
			assertNoTempFiles(t, f.exe)
		})
	}
}

func overwriteArtifact(f *fixture, version string, content []byte) {
	f.t.Helper()
	name := "app-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(server.ArtifactDir(f.releases, mustVersion(f.t, version)), name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func assertNoTempFiles(t *testing.T, exe string) {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Dir(exe))
	for _, e := range entries {
		switch e.Name() {
		case filepath.Base(exe), filepath.Base(prevPath(exe)), ".updater":
		default:
			t.Errorf("unexpected file left in install directory: %s", e.Name())
		}
	}
}

func TestSelfTestFailureKeepsCurrentVersion(t *testing.T) {
	f := newFixture(t)
	f.publish("1.1.0", []byte("broken"), nil)
	u := f.updater("1.0.0", nil)
	calls := 0
	u.selfTest = func(context.Context, string, manifest.Version) error {
		calls++
		return errors.New("exec format error")
	}

	if _, err := u.CheckOnce(context.Background()); err == nil {
		t.Fatal("expected self-test error")
	}
	if got := f.exeContent(); !bytes.Equal(got, oldBinary) {
		t.Fatalf("executable changed to %q", got)
	}
	// The broken release is not downloaded and tested again.
	if _, err := u.CheckOnce(context.Background()); err != nil {
		t.Fatalf("second check: %v", err)
	}
	if calls != 1 {
		t.Fatalf("self-test ran %d times, want 1", calls)
	}
}

func TestShouldApplyDefersUnlessMandatory(t *testing.T) {
	f := newFixture(t)
	f.publish("1.1.0", []byte("new"), nil)
	busy := func(c *Config) { c.ShouldApply = func() bool { return false } }

	res, err := f.updater("1.0.0", busy).CheckOnce(context.Background())
	if err != nil || !res.Deferred || res.Installed {
		t.Fatalf("result = %+v, %v; want deferred", res, err)
	}

	min := mustVersion(t, "1.1.0")
	f.publish("1.2.0", []byte("newer"), func(o *release.Options) { o.MinVersion = &min })
	res, err = f.updater("1.0.0", busy).CheckOnce(context.Background())
	if err != nil || !res.Installed {
		t.Fatalf("result = %+v, %v; want mandatory install", res, err)
	}
}

func TestStartupConfirmsHealthyVersion(t *testing.T) {
	f := newFixture(t)
	f.publish("1.1.0", []byte("new"), nil)
	if _, err := f.updater("1.0.0", nil).CheckOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	u := f.updater("1.1.0", nil) // the relaunched process
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rolledBack, err := u.Startup(ctx)
	if err != nil || rolledBack {
		t.Fatalf("Startup = %v, %v", rolledBack, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := f.state(u)
		if st.Pending == nil && st.HighWater == mustVersion(t, "1.1.0") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("version never confirmed: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartupRollsBackCrashingVersion(t *testing.T) {
	f := newFixture(t)
	f.publish("1.1.0", []byte("crashy binary"), nil)
	if _, err := f.updater("1.0.0", nil).CheckOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Each failed boot of 1.1.0 dies before HealthyAfter elapses.
	for boot := 1; boot <= maxFailedBoots; boot++ {
		ctx, cancel := context.WithCancel(context.Background())
		u := f.updater("1.1.0", func(c *Config) { c.HealthyAfter = time.Hour })
		if rolledBack, err := u.Startup(ctx); err != nil || rolledBack {
			t.Fatalf("boot %d: Startup = %v, %v", boot, rolledBack, err)
		}
		cancel()
	}

	u := f.updater("1.1.0", nil)
	rolledBack, err := u.Startup(context.Background())
	if err != nil || !rolledBack {
		t.Fatalf("Startup = %v, %v; want rollback", rolledBack, err)
	}
	if got := f.exeContent(); !bytes.Equal(got, oldBinary) {
		t.Fatalf("executable = %q, want old binary restored", got)
	}
	st := f.state(u)
	if st.Pending != nil || !st.isBlocked(mustVersion(t, "1.1.0")) {
		t.Fatalf("state = %+v, want 1.1.0 blocked and nothing pending", st)
	}

	// The restored 1.0.0 must not reinstall the blocked release.
	res, err := f.updater("1.0.0", nil).CheckOnce(context.Background())
	if err != nil || res.Installed {
		t.Fatalf("result = %+v, %v; want blocked release skipped", res, err)
	}
}

func TestStartupRemovesLeftoversFromInterruptedUpdate(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Dir(f.exe)
	leftovers := []string{
		fsutil.TempPrefix(f.exe) + "123",         // partial download
		fsutil.TempPrefix(prevPath(f.exe)) + "9", // partial backup
		movedAsidePrefix(f.exe) + "42",           // Windows moved-aside binary
	}
	for _, name := range leftovers {
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := f.updater("1.0.0", nil).Startup(ctx); err != nil {
		t.Fatal(err)
	}
	assertNoTempFiles(t, f.exe)
}

func TestNewRejectsInsecureConfig(t *testing.T) {
	keys := manifest.KeyRing{"k1": make(ed25519.PublicKey, ed25519.PublicKeySize)}
	exe := filepath.Join(t.TempDir(), "app")
	if err := os.WriteFile(exe, oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	base := Config{ServerURL: "https://updates.example.com", Interval: time.Minute, HealthyAfter: time.Second, Executable: exe, Keys: keys, StateDir: t.TempDir()}

	tests := map[string]func(*Config){
		"no keys":           func(c *Config) { c.Keys = nil },
		"plain http remote": func(c *Config) { c.ServerURL = "http://updates.example.com" },
		"relative exe":      func(c *Config) { c.Executable = "app" },
		"bad scheme":        func(c *Config) { c.ServerURL = "ftp://updates.example.com" },
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := base
			edit(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("expected error")
			}
		})
	}

	cfg := base
	cfg.ServerURL = "http://127.0.0.1:8080"
	if _, err := New(cfg); err != nil {
		t.Fatalf("loopback http should be allowed: %v", err)
	}
}

func TestDownloadRejectsOversizedBody(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), 100))
	}))
	defer srv.Close()
	u := f.updater("1.0.0", func(c *Config) { c.ServerURL = srv.URL })

	_, err := u.download(context.Background(), manifest.Artifact{Path: "/v1/artifacts/1.1.0/app", Size: 10})
	if !errors.Is(err, errSizeMismatch) {
		t.Fatalf("error = %v, want size mismatch", err)
	}
	assertNoTempFiles(t, f.exe)
}

func TestFetchManifestHandlesServerErrors(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	u := f.updater("1.0.0", func(c *Config) { c.ServerURL = srv.URL })
	if _, err := u.CheckOnce(context.Background()); err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestRedirectsMustStayOnOrigin(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer other.Close()
	srv := httptest.NewServer(http.RedirectHandler(other.URL+"/v1/manifest", http.StatusFound))
	defer srv.Close()

	f := newFixture(t)
	u := f.updater("1.0.0", func(c *Config) { c.ServerURL = srv.URL; c.HTTPClient = defaultHTTPClient() })
	if _, err := u.CheckOnce(context.Background()); err == nil {
		t.Fatal("expected cross-origin redirect to be refused")
	}
}

func TestBackoff(t *testing.T) {
	if got := backoff(time.Minute, 1); got != 2*time.Minute {
		t.Errorf("backoff(1) = %v", got)
	}
	if got := backoff(time.Minute, 50); got != time.Hour {
		t.Errorf("backoff(50) = %v, want capped at 1h", got)
	}
	for range 100 {
		if d := jitter(time.Minute); d < 48*time.Second || d > 72*time.Second {
			t.Fatalf("jitter out of range: %v", d)
		}
	}
}
