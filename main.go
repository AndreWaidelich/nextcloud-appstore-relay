package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AndreWaidelich/nextcloud-appstore-relay/internal/catalog"
	"github.com/AndreWaidelich/nextcloud-appstore-relay/internal/config"
	"github.com/AndreWaidelich/nextcloud-appstore-relay/internal/relaylog"
	"github.com/AndreWaidelich/nextcloud-appstore-relay/internal/server"
	"github.com/AndreWaidelich/nextcloud-appstore-relay/internal/tarball"
)

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		os.Stderr.WriteString("config error: " + err.Error() + "\n")
		os.Exit(2)
	}
	log := relaylog.New(cfg.LogLevel)
	log.Info("starting nextcloud-appstore-relay",
		"upstream", cfg.Upstream,
		"public_url", cfg.PublicURL,
		"cache_dir", cfg.CacheDir,
		"json_ttl", cfg.JSONTTL.String(),
		"listen", cfg.Listen,
	)

	cat, err := catalog.New(cfg.Upstream, cfg.PublicURL, cfg.CacheDir, cfg.JSONTTL, cfg.UpstreamTimeout, log)
	if err != nil {
		log.Error("catalog init failed", "err", err.Error())
		os.Exit(1)
	}
	tb, err := tarball.New(cfg.CacheDir, cfg.TarballTimeout, log)
	if err != nil {
		log.Error("tarball store init failed", "err", err.Error())
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cat.Start(ctx)

	srv := server.New(cat, tb, log)
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No overall write timeout: large tarball downloads can exceed any
		// fixed budget. The per-request context handles cancellation.
	}

	go func() {
		log.Info("http server listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "err", err.Error())
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutdown requested, draining http server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	log.Info("bye")
}
