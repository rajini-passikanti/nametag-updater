// Command app is the deployed program. It does its work (here, a heartbeat)
// while a background updater keeps it on the newest signed release.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"nametag-updater/internal/manifest"
	"nametag-updater/internal/updater"
	"nametag-updater/internal/version"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		showVersion  = flag.Bool("version", false, "print the version and exit")
		selfTest     = flag.Bool("self-test", false, "check that this binary can run, then exit")
		serverURL    = flag.String("server", envOr("UPDATER_SERVER", "http://127.0.0.1:8080"), "update server base URL")
		channel      = flag.String("channel", "stable", "release channel")
		interval     = flag.Duration("interval", 5*time.Minute, "average time between update checks")
		healthyAfter = flag.Duration("healthy-after", 30*time.Second, "uptime after which a new version is confirmed good")
		heartbeat    = flag.Duration("heartbeat", 10*time.Second, "interval between heartbeat log lines")
		stateDir     = flag.String("state-dir", "", "updater state directory (default: .updater next to the executable)")
	)
	flag.Parse()
	if *heartbeat <= 0 || *healthyAfter <= 0 {
		fmt.Fprintln(os.Stderr, "heartbeat and healthy-after must be positive")
		return 2
	}

	current, err := manifest.ParseVersion(version.Version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build error:", err)
		return 1
	}
	if *showVersion {
		fmt.Println(current)
		return 0
	}
	keys, keyErr := manifest.ParseKeyRing(version.PublicKeys)
	if keyErr == nil && len(keys) == 0 {
		keyErr = manifest.ErrNoKeys
	}
	if *selfTest {
		if keyErr != nil {
			fmt.Fprintln(os.Stderr, "self-test failed:", keyErr)
			return 1
		}
		fmt.Println(updater.SelfTestOutput(current))
		return 0
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("version", current.String())
	slog.SetDefault(logger)
	if keyErr != nil {
		logger.Error("refusing to start without trusted signing keys", "err", keyErr)
		return 1
	}
	exe, err := updater.Executable()
	if err != nil {
		logger.Error("locate executable", "err", err)
		return 1
	}

	u, err := updater.New(updater.Config{
		ServerURL:    *serverURL,
		Channel:      *channel,
		Interval:     *interval,
		HealthyAfter: *healthyAfter,
		StateDir:     *stateDir,
		Executable:   exe,
		Current:      current,
		Keys:         keys,
		Logger:       logger,
	})
	if err != nil {
		logger.Error("configure updater", "err", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	instance, err := u.LockInstance(ctx)
	if err != nil {
		logger.Error("acquire application instance lock", "err", err)
		return 1
	}
	defer instance.Unlock()
	logger.Info("starting", "pid", os.Getpid(), "exe", exe, "server", *serverURL)

	rolledBack, err := u.Startup(ctx)
	if err != nil {
		logger.Error("updater startup", "err", err)
		if errors.Is(err, updater.ErrStaleProcess) {
			return 0
		}
	}
	if err == nil && rolledBack {
		return relaunch(logger, exe)
	}

	startupOK := err == nil
	for {
		workCtx, stopWork := context.WithCancel(ctx)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); work(workCtx, logger, *heartbeat) }()

		if !startupOK {
			// Preserve the workload, but do not mutate safety state after failed
			// recovery or unreadable state. An operator can repair and restart.
			logger.Warn("automatic updates disabled; repair and restart required")
			<-ctx.Done()
			stopWork()
			wg.Wait()
			return 0
		}
		installed, err := u.Run(ctx)
		stopWork()
		wg.Wait()
		if err != nil {
			logger.Info("shutting down")
			return 0
		}
		logger.Info("restarting into new version", "new_version", installed)
		if err := u.Handover(ctx, os.Args[1:]); err == nil {
			return 0
		} else {
			logger.Error("handover failed; resuming workload", "err", err)
			if errors.Is(err, updater.ErrRecoveryRequired) {
				startupOK = false
			}
		}
	}
}

// work stands in for the program's real job.
func work(ctx context.Context, logger *slog.Logger, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			logger.Info("heartbeat")
		}
	}
}

func relaunch(logger *slog.Logger, exe string) int {
	if err := updater.Relaunch(exe, os.Args[1:]); err != nil {
		logger.Error("relaunch failed", "err", err)
		return 1
	}
	return 0
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
