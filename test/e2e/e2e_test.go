// Package e2e builds real binaries and checks that a running program replaces
// itself with a newly published release.
package e2e

import (
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"nametag-updater/internal/manifest"
	"nametag-updater/internal/release"
	"nametag-updater/internal/server"
)

type harness struct {
	t        *testing.T
	dir      string
	exe      string // installed program
	releases string
	logPath  string
	pubKeys  string
	priv     ed25519.PrivateKey
	srv      *httptest.Server
}

func newHarness(t *testing.T) *harness {
	if testing.Short() {
		t.Skip("builds binaries and runs processes")
	}
	dir := t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(nil)
	h := &harness{
		t:        t,
		dir:      dir,
		exe:      filepath.Join(dir, "install", "app"+exeExt()),
		releases: filepath.Join(dir, "releases"),
		logPath:  filepath.Join(dir, "app.log"),
		pubKeys:  manifest.PublicKeyEntry("k1", pub),
		priv:     priv,
	}
	h.srv = httptest.NewServer(server.New(h.releases, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(h.srv.Close)
	t.Cleanup(h.killApps) // runs before TempDir removal, which Windows needs
	return h
}

func exeExt() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func artifactName() string {
	return "app-" + runtime.GOOS + "-" + runtime.GOARCH + exeExt()
}

// build compiles cmd/app at version into a fresh dist directory.
func (h *harness) build(version string) string {
	h.t.Helper()
	dist := filepath.Join(h.dir, "dist", version)
	ldflags := "-X nametag-updater/internal/version.Version=" + version +
		" -X nametag-updater/internal/version.PublicKeys=" + h.pubKeys
	cmd := exec.Command("go", "build", "-ldflags", ldflags, "-o", filepath.Join(dist, artifactName()), "nametag-updater/cmd/app")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("go build %s: %v\n%s", version, err, out)
	}
	return dist
}

func (h *harness) install(dist string) {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(dist, artifactName()))
	if err != nil {
		h.t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(h.exe), 0o755)
	if err := os.WriteFile(h.exe, data, 0o755); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) publish(version, dist string) {
	h.t.Helper()
	v, _ := manifest.ParseVersion(version)
	_, err := release.Publish(release.Options{
		Version: v, DistDir: dist, ReleasesDir: h.releases, Channel: "stable",
		KeyID: "k1", PrivateKey: h.priv, TTL: time.Hour, Now: time.Now(),
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

// start launches the installed program. Relaunched versions inherit the same
// log file, so the log shows the whole history.
func (h *harness) start() <-chan error {
	return h.startWithHealth("1s")
}

func (h *harness) startWithHealth(healthyAfter string) <-chan error {
	h.t.Helper()
	logFile, err := os.OpenFile(h.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command(h.exe,
		"--server", h.srv.URL,
		"--interval", "300ms",
		"--healthy-after", healthyAfter,
		"--heartbeat", "200ms")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	logFile.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done
}

func (h *harness) state() struct {
	Pending *struct {
		Version string
		Boots   int
	}
	Blocked []string
} {
	h.t.Helper()
	var state struct {
		Pending *struct {
			Version string
			Boots   int
		}
		Blocked []string
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(h.exe), ".updater", "state.json"))
	if err == nil {
		_ = json.Unmarshal(data, &state)
	}
	return state
}

func TestExternalRestartsRecoverUnconfirmedRelease(t *testing.T) {
	h := newHarness(t)
	h.install(h.build("1.0.0"))
	done := h.startWithHealth("1h")
	h.waitFor("initial workload", func() bool { return strings.Contains(h.log(), "heartbeat version=1.0.0") })
	h.publish("1.1.0", h.build("1.1.0"))
	h.waitFor("first unconfirmed boot", func() bool {
		s := h.state()
		return s.Pending != nil && s.Pending.Version == "1.1.0" && s.Pending.Boots == 1
	})
	if err := <-done; err != nil {
		t.Fatalf("initial handover: %v", err)
	}
	// The harness is the external restart mechanism. Kill the actual v2
	// process, then launch it twice more to exercise Startup recovery.
	matches := pidPattern.FindAllStringSubmatch(h.log(), -1)
	if len(matches) < 2 {
		t.Fatal("replacement PID missing")
	}
	pid, _ := strconv.Atoi(matches[len(matches)-1][1])
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	done = h.startWithHealth("1h")
	h.waitFor("second unconfirmed boot", func() bool { s := h.state(); return s.Pending != nil && s.Pending.Boots == 2 })
	matches = pidPattern.FindAllStringSubmatch(h.log(), -1)
	pid, _ = strconv.Atoi(matches[len(matches)-1][1])
	process, err = os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("killed candidate unexpectedly exited successfully")
	}
	done = h.startWithHealth("1h")
	h.waitFor("rollback", func() bool { return strings.Contains(h.log(), "new version kept failing; rolled back") })
	if err := <-done; err != nil {
		t.Fatalf("rollback relaunch: %v", err)
	}
	h.waitFor("restored workload", func() bool {
		return strings.Count(h.log(), "starting version=1.0.0") == 2 && strings.Contains(h.log()[strings.LastIndex(h.log(), "starting version=1.0.0"):], "heartbeat version=1.0.0")
	})
	if got := h.installedVersion(); got != "1.0.0" {
		t.Fatalf("restored version = %s", got)
	}
	s := h.state()
	if s.Pending != nil || len(s.Blocked) != 1 || s.Blocked[0] != "1.1.0" {
		t.Fatalf("recovered state = %+v", s)
	}
}

func TestMissingBackupDoesNotRelaunchForever(t *testing.T) {
	h := newHarness(t)
	h.install(h.build("1.1.0"))
	stateDir := filepath.Join(filepath.Dir(h.exe), ".updater")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state := `{"high_water":"1.0.0","pending":{"version":"1.1.0","from":"1.0.0","boots":2}}`
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	done := h.start()
	h.waitFor("workload after failed recovery", func() bool { return strings.Contains(h.log(), "heartbeat version=1.1.0") })
	if !strings.Contains(h.log(), "automatic updates disabled; repair and restart required") {
		t.Fatal("recovery error did not disable updates")
	}
	select {
	case err := <-done:
		t.Fatalf("app exited after failed recovery: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if n := strings.Count(h.log(), "msg=starting"); n != 1 {
		t.Fatalf("unexpected restart loop: %d starts", n)
	}
	if p := h.state().Pending; p == nil || p.Boots != 2 {
		t.Fatalf("recovery state lost: %+v", p)
	}
}

func (h *harness) log() string {
	b, _ := os.ReadFile(h.logPath)
	return string(b)
}

func (h *harness) installedVersion() string {
	out, err := exec.Command(h.exe, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s\nlog:\n%s", what, h.log())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

var pidPattern = regexp.MustCompile(`pid=(\d+)`)

// killApps stops every process the log says was started, including the ones
// launched by the program itself during an update.
func (h *harness) killApps() {
	for _, m := range pidPattern.FindAllStringSubmatch(h.log(), -1) {
		pid, _ := strconv.Atoi(m[1])
		if p, err := os.FindProcess(pid); err == nil {
			p.Kill()
		}
	}
	time.Sleep(200 * time.Millisecond) // let Windows release file handles
}

func TestRunningProgramUpdatesItself(t *testing.T) {
	h := newHarness(t)
	h.install(h.build("1.0.0"))
	done := h.start()
	h.waitFor("1.0.0 heartbeat", func() bool { return strings.Contains(h.log(), "heartbeat version=1.0.0") })

	h.publish("1.1.0", h.build("1.1.0"))

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("1.0.0 exited with error: %v\n%s", err, h.log())
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("1.0.0 did not hand over to 1.1.0\n%s", h.log())
	}
	if got := h.installedVersion(); got != "1.1.0" {
		t.Fatalf("installed version = %q, want 1.1.0", got)
	}
	h.waitFor("1.1.0 heartbeat", func() bool { return strings.Contains(h.log(), "heartbeat version=1.1.0") })
	h.waitFor("1.1.0 confirmed", func() bool { return strings.Contains(h.log(), "update confirmed healthy") })
}

func TestBrokenReleaseIsNeverInstalled(t *testing.T) {
	h := newHarness(t)
	h.install(h.build("1.0.0"))
	done := h.start()

	// Correctly signed, but not a runnable program for this platform.
	dist := filepath.Join(h.dir, "dist", "1.1.0")
	os.MkdirAll(dist, 0o755)
	os.WriteFile(filepath.Join(dist, artifactName()), []byte("this is not an executable"), 0o755)
	h.publish("1.1.0", dist)

	h.waitFor("self-test failure", func() bool { return strings.Contains(h.log(), "self-test of 1.1.0 failed") })
	select {
	case err := <-done:
		t.Fatalf("1.0.0 exited (%v) but should keep running\n%s", err, h.log())
	case <-time.After(time.Second):
	}
	if got := h.installedVersion(); got != "1.0.0" {
		t.Fatalf("installed version = %q, want 1.0.0", got)
	}
}
