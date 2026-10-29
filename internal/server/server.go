// Package server serves signed manifests and release artifacts. It holds no
// signing keys: clients verify everything it serves, so a compromised server
// can withhold updates but cannot push code.
package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"nametag-updater/internal/manifest"
)

// DefaultChannel is used when a client does not name one.
const DefaultChannel = "stable"

var (
	// ChannelPattern restricts channel names, which become file names.
	ChannelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	filePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// Layout of a releases directory:
//
//	manifests/<channel>.json       signed envelope for the channel's current release
//	artifacts/<version>/<file>     immutable release artifacts

// ManifestPath returns where the current manifest for channel is stored.
func ManifestPath(root, channel string) string {
	return filepath.Join(root, "manifests", channel+".json")
}

// ArtifactDir returns where artifacts for v are stored.
func ArtifactDir(root string, v manifest.Version) string {
	return filepath.Join(root, "artifacts", v.String())
}

// Server serves a releases directory over HTTP.
type Server struct {
	root string
	log  *slog.Logger
}

// New returns a Server for the releases directory at root.
func New(root string, logger *slog.Logger) *Server {
	return &Server{root: root, log: logger}
}

// Handler returns the server's HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/manifest", s.handleManifest)
	mux.HandleFunc("GET /v1/artifacts/{version}/{file}", s.handleArtifact)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /status", s.handleStatus)
	return logRequests(s.log, mux)
}

// handleManifest returns the channel's signed manifest. It is a handler rather
// than a static file so that per-client decisions (staged rollout, pinning)
// can be made from the query parameters clients already send.
func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	channel := q.Get("channel")
	if channel == "" {
		channel = DefaultChannel
	}
	if !ChannelPattern.MatchString(channel) {
		http.Error(w, "invalid channel", http.StatusBadRequest)
		return
	}
	s.log.Debug("manifest request", "channel", channel, "client_version", q.Get("version"),
		"os", q.Get("os"), "arch", q.Get("arch"), "install_id", q.Get("install_id"))

	data, err := os.ReadFile(ManifestPath(s.root, channel))
	if errors.Is(err, fs.ErrNotExist) {
		http.Error(w, "no release published on this channel", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("read manifest", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	v, err := manifest.ParseVersion(r.PathValue("version"))
	name := r.PathValue("file")
	if err != nil || !filePattern.MatchString(name) || strings.Contains(name, "..") {
		http.Error(w, "invalid artifact path", http.StatusBadRequest)
		return
	}
	f, err := os.Open(filepath.Join(ArtifactDir(s.root, v), name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	// A version's artifacts never change once published.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// ChannelStatus summarizes one channel for operators.
type ChannelStatus struct {
	Version    string    `json:"version"`
	ReleasedAt time.Time `json:"released_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Platforms  []string  `json:"platforms"`
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	paths, _ := filepath.Glob(filepath.Join(s.root, "manifests", "*.json"))
	channels := map[string]ChannelStatus{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		m, err := manifest.ReadUnverified(data)
		if err != nil {
			s.log.Warn("unreadable manifest", "path", p, "err", err)
			continue
		}
		st := ChannelStatus{Version: m.Version.String(), ReleasedAt: m.ReleasedAt, ExpiresAt: m.ExpiresAt}
		for platform := range m.Artifacts {
			st.Platforms = append(st.Platforms, platform)
		}
		channels[strings.TrimSuffix(filepath.Base(p), ".json")] = st
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{"channels": channels})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("request", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration", time.Since(start).Round(time.Microsecond))
	})
}
