// Command update-server serves signed release manifests and artifacts.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"nametag-updater/internal/server"
)

func main() {
	var (
		addr     = flag.String("addr", "127.0.0.1:8080", "listen address")
		releases = flag.String("releases", "releases", "releases directory written by 'release publish'")
		certFile = flag.String("tls-cert", "", "TLS certificate file (enables HTTPS)")
		keyFile  = flag.String("tls-key", "", "TLS private key file")
	)
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           server.New(*releases, logger).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      10 * time.Minute, // artifacts can be large
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", *addr, "releases", *releases, "tls", *certFile != "")
		if *certFile != "" {
			errc <- srv.ListenAndServeTLS(*certFile, *keyFile)
		} else {
			errc <- srv.ListenAndServe()
		}
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "err", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "err", err)
			os.Exit(1)
		}
	}
}
