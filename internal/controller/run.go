package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"lwd/internal/nodeclient"
	"lwd/internal/registry"
	"lwd/internal/secrets"
	"lwd/internal/store"
)

// ShutdownGrace is how long a stopping controller waits for in-flight
// requests. It covers a full node deploy so an operator's SIGTERM does not
// orphan a deployment record; a second signal (handled by the caller) kills
// the process immediately.
const ShutdownGrace = nodeclient.DeployTimeout + 30*time.Second

// Run listens on cfg.Listen and serves until ctx is cancelled.
func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	if cfg.APIToken == "" {
		return errors.New("LWD_API_TOKEN is required")
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	return Serve(ctx, cfg, ln, log)
}

// Serve opens the store, applies migrations, fails deployments orphaned by a
// previous process, and serves the /v1 API on ln until ctx is cancelled,
// then shuts down gracefully. It takes ownership of ln.
func Serve(ctx context.Context, cfg Config, ln net.Listener, log *slog.Logger) error {
	defer ln.Close()
	if cfg.APIToken == "" {
		return errors.New("LWD_API_TOKEN is required")
	}
	ciph, err := secrets.NewCipher(cfg.SecretKeyFile)
	if err != nil {
		return fmt.Errorf("secret key %s: %w", cfg.SecretKeyFile, err)
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	if n, err := st.FailInterrupted(ctx); err != nil {
		return err
	} else if n > 0 {
		log.Warn("marked deployments interrupted by a previous controller as failed", "count", n)
	}

	c := New(st, ciph, registry.NewRemote(cfg.InsecureRegistries), log)
	srv := &http.Server{
		Handler:           c.Handler(cfg.APIToken, cfg.ReadToken),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	bctx, stopBackups := context.WithCancel(ctx)
	defer stopBackups()
	go c.backupLoop(bctx, cfg.BackupHour, time.Minute)
	log.Info("daily database backups scheduled", "hour", cfg.BackupHour)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("lwd controller listening", "addr", ln.Addr().String())

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down; waiting for in-flight requests", "grace", ShutdownGrace)
	sctx, cancel := context.WithTimeout(context.Background(), ShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
