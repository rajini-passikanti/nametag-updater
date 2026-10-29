// Package updater keeps a running executable up to date with the newest
// signed release published on an update server.
package updater

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"nametag-updater/internal/fsutil"
	"nametag-updater/internal/manifest"
)

const (
	maxFailedBoots = 2
	maxBackoff     = time.Hour
)

// Config configures an Updater.
type Config struct {
	// ServerURL is the update server's base URL. It must be https, except
	// for loopback addresses used in development and tests.
	ServerURL string
	// Channel selects a release track such as "stable" or "beta".
	Channel string
	// Interval is the average time between update checks.
	Interval time.Duration
	// HealthyAfter is how long a new version must run before it is
	// considered good and the previous version stops being a fallback.
	HealthyAfter time.Duration
	// StateDir holds updater state and the update lock.
	StateDir string
	// Executable is the absolute, symlink-free path of the binary to replace.
	Executable string
	// Current is the version of the running binary.
	Current manifest.Version
	// Keys are the trusted release-signing keys.
	Keys manifest.KeyRing

	// ShouldApply, if set, is consulted before installing a non-mandatory
	// update. Returning false defers it to a later check.
	ShouldApply func() bool
	// Reporter, if set, receives update events for telemetry.
	Reporter Reporter
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// HTTPClient defaults to a client with TLS 1.2+ and same-host redirects.
	HTTPClient *http.Client
}

// Updater checks for, verifies, and installs new releases.
type Updater struct {
	cfg  Config
	base *url.URL
	log  *slog.Logger

	// skip holds versions that failed verification in this process, so a
	// bad release is not downloaded again on every check.
	skip map[manifest.Version]bool

	identity os.FileInfo

	// Seams for tests. checkpoint is called between install steps so tests
	// can inspect the disk as it would be after a crash at that point.
	selfTest   func(ctx context.Context, path string, want manifest.Version) error
	now        func() time.Time
	checkpoint func(step string)
	swap       func(string, string) (bool, error)
	launch     func(string, []string) error
}

// Result describes the outcome of one update check.
type Result struct {
	// Installed is true when a new version replaced the executable. The
	// caller should stop its work and Relaunch.
	Installed bool
	// Deferred is true when ShouldApply asked to postpone an update.
	Deferred bool
	Version  manifest.Version
}

// New validates cfg and returns an Updater.
func New(cfg Config) (*Updater, error) {
	if len(cfg.Keys) == 0 {
		return nil, manifest.ErrNoKeys
	}
	base, err := parseServerURL(cfg.ServerURL)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(cfg.Executable) {
		return nil, fmt.Errorf("executable path %q must be absolute", cfg.Executable)
	}
	if cfg.Interval <= 0 {
		return nil, errors.New("interval must be positive")
	}
	if cfg.HealthyAfter <= 0 {
		return nil, errors.New("healthy-after must be positive")
	}
	if cfg.Channel == "" {
		cfg.Channel = "stable"
	}
	if cfg.StateDir == "" {
		cfg.StateDir = filepath.Join(filepath.Dir(cfg.Executable), ".updater")
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = defaultHTTPClient()
	}
	identity, err := os.Stat(cfg.Executable)
	if err != nil {
		return nil, fmt.Errorf("stat executable: %w", err)
	}
	return &Updater{
		cfg:        cfg,
		identity:   identity,
		swap:       swapExecutable,
		launch:     Relaunch,
		base:       base,
		log:        cfg.Logger,
		skip:       map[manifest.Version]bool{},
		selfTest:   runSelfTest,
		now:        time.Now,
		checkpoint: func(string) {},
	}, nil
}

func parseServerURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid server URL %q", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return nil, fmt.Errorf("server URL %q must use https", raw)
		}
	default:
		return nil, fmt.Errorf("server URL %q must use https", raw)
	}
	return u, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Run checks for updates until ctx is cancelled or an update is installed.
// After it returns an installed version, the caller should stop its work and
// call Relaunch so the new binary takes over.
func (u *Updater) Run(ctx context.Context) (manifest.Version, error) {
	failures := 0
	timer := time.NewTimer(jitter(min(u.cfg.Interval, 10*time.Second)))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return manifest.Version{}, ctx.Err()
		case <-timer.C:
		}

		res, err := u.CheckOnce(ctx)
		next := u.cfg.Interval
		switch {
		case res.Installed:
			if err != nil {
				u.log.Warn("replacement committed with durability error", "err", err)
			}
			return res.Version, nil
		case err != nil && ctx.Err() != nil:
			return manifest.Version{}, ctx.Err()
		case errors.Is(err, ErrStaleProcess):
			return manifest.Version{}, err
		case err != nil:
			failures++
			next = backoff(u.cfg.Interval, failures)
			u.log.Warn("update check failed", "err", err, "retry_in", next.Round(time.Second))
		default:
			failures = 0
		}
		timer.Reset(jitter(next))
	}
}

// CheckOnce performs a single update check and, if a valid newer release is
// available, installs it.
func (u *Updater) CheckOnce(ctx context.Context) (Result, error) {
	lock, err := fsutil.TryLock(u.lockPath())
	if errors.Is(err, fsutil.ErrLocked) {
		u.log.Debug("another process is updating this install; skipping check")
		return Result{}, nil
	}
	if err != nil {
		return Result{}, err
	}
	defer lock.Unlock()

	if err := u.checkIdentity(); err != nil {
		return Result{}, err
	}
	st, err := u.ensureInstallID()
	if err != nil {
		return Result{}, err
	}
	if st.Pending != nil {
		// Preserve the confirmed-good backup until health confirmation or recovery.
		return Result{Deferred: true, Version: st.Pending.Version}, nil
	}
	if u.cfg.Current.Less(st.HighWater) {
		return Result{}, ErrStaleProcess
	}
	m, err := u.fetchManifest(ctx, st.InstallID)
	if err != nil || m == nil {
		return Result{}, err
	}

	art, err := m.Select(manifest.Installed{
		Version:   u.cfg.Current,
		HighWater: st.HighWater,
		Platform:  platform(),
	}, u.now())
	if errors.Is(err, manifest.ErrNotNewer) {
		u.log.Debug("up to date", "latest", m.Version)
		return Result{}, nil
	}
	if err != nil {
		u.report(ctx, Event{Kind: EventRejected, From: u.cfg.Current, To: m.Version, Err: err})
		return Result{}, fmt.Errorf("release %s rejected: %w", m.Version, err)
	}
	if st.isBlocked(m.Version) || u.skip[m.Version] {
		u.log.Debug("skipping release that failed earlier", "latest", m.Version)
		return Result{}, nil
	}
	if !m.Mandatory(u.cfg.Current) && u.cfg.ShouldApply != nil && !u.cfg.ShouldApply() {
		u.log.Info("update available but deferred by application", "to", m.Version)
		return Result{Deferred: true, Version: m.Version}, nil
	}

	committed, err := u.install(ctx, m.Version, art)
	if err != nil {
		u.report(ctx, Event{Kind: EventFailed, From: u.cfg.Current, To: m.Version, Err: err})
		return Result{Installed: committed, Version: m.Version}, err
	}
	u.log.Info("update installed", "from", u.cfg.Current, "to", m.Version)
	u.report(ctx, Event{Kind: EventInstalled, From: u.cfg.Current, To: m.Version})
	return Result{Installed: true, Version: m.Version}, nil
}

// install downloads and verifies art, proves it runs, and swaps it in.
func (u *Updater) install(ctx context.Context, v manifest.Version, art manifest.Artifact) (bool, error) {
	exe := u.cfg.Executable
	info, err := os.Stat(exe)
	if err != nil {
		return false, fmt.Errorf("stat executable: %w", err)
	}
	tmp, err := u.download(ctx, art)
	if err != nil {
		u.skipIfPermanent(v, err)
		return false, fmt.Errorf("download %s: %w", v, err)
	}
	defer os.Remove(tmp) // no-op once the file has been renamed into place
	u.checkpoint("downloaded")

	if err := os.Chmod(tmp, info.Mode().Perm()); err != nil {
		return false, fmt.Errorf("set permissions on new executable: %w", err)
	}
	if err := u.selfTest(ctx, tmp, v); err != nil {
		u.skip[v] = true
		return false, fmt.Errorf("self-test of %s failed: %w", v, err)
	}
	u.checkpoint("self-tested")

	fromDigest, err := executableDigest(exe)
	if err != nil {
		return false, err
	}
	if err := u.updateState(func(s *State) {
		s.Pending = &Pending{Version: v, From: u.cfg.Current, SHA256: art.SHA256, FromSHA256: fromDigest}
	}); err != nil {
		return false, err
	}
	u.checkpoint("pending-recorded")
	if committed, err := u.replaceExecutable(exe, tmp, info.Mode().Perm()); err != nil {
		if committed {
			return true, fmt.Errorf("replacement committed: %w", err)
		}
		// A failed Windows restoration can leave the path missing. Clear
		// recovery state only when the original executable is still present.
		if digest, derr := executableDigest(exe); derr == nil && digest == fromDigest {
			if serr := u.updateState(func(s *State) { s.Pending = nil }); serr != nil {
				u.log.Error("could not clear pending update", "err", serr)
			}
		}
		return false, fmt.Errorf("replace executable: %w", err)
	}
	return true, nil
}

func (u *Updater) skipIfPermanent(v manifest.Version, err error) {
	if errors.Is(err, errHashMismatch) || errors.Is(err, errSizeMismatch) {
		u.skip[v] = true
	}
}

func (u *Updater) ensureInstallID() (State, error) {
	var out State
	err := u.updateState(func(s *State) {
		if s.InstallID == "" {
			s.InstallID = newInstallID()
		}
		out = *s
	})
	return out, err
}

// updateState requires the installation lock. Never reset unreadable state:
// preserving its bytes keeps recovery information available to the operator.
func (u *Updater) updateState(fn func(*State)) error {
	s, err := loadState(u.statePath())
	if err != nil {
		return err
	}
	fn(&s)
	return saveState(u.statePath(), s)
}

func (u *Updater) report(ctx context.Context, e Event) {
	if u.cfg.Reporter != nil {
		u.cfg.Reporter.Report(ctx, e)
	}
}

func (u *Updater) statePath() string { return filepath.Join(u.cfg.StateDir, "state.json") }
func (u *Updater) lockPath() string  { return filepath.Join(u.cfg.StateDir, "update.lock") }

func platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// jitter spreads d by ±20% so a fleet of clients does not poll in lockstep.
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

func backoff(interval time.Duration, failures int) time.Duration {
	d := interval << min(failures, 6)
	return min(d, max(maxBackoff, interval))
}

// ErrStaleProcess means another process replaced this process's executable.
// Such a process must not modify the newer installation or its safety state.
var ErrStaleProcess = errors.New("executable changed; this process cannot update the installation")

// checkIdentity requires the installation lock.
func (u *Updater) checkIdentity() error {
	info, err := os.Stat(u.cfg.Executable)
	if err != nil {
		return err
	}
	if !os.SameFile(u.identity, info) {
		return ErrStaleProcess
	}
	return nil
}
