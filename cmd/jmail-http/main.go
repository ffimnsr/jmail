package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	if len(cfg.token) > 0 && len(cfg.token) < 24 {
		slog.Warn("JMAIL_HTTP_TOKEN is short; use at least 32 random bytes, for example `openssl rand -hex 32`",
			"length", len(cfg.token))
	}
	if cfg.allowAnyFrom {
		slog.Warn("JMAIL_HTTP_ALLOWED_FROM is '*': messages from any domain are accepted, and only mail from JMAIL_ORIGIN or JMAIL_DOMAINS is DKIM-signed",
			"domains", cfg.domains)
	}

	srv := newServer(cfg)
	httpSrv := &http.Server{
		Addr:    cfg.listen,
		Handler: srv.routes(),
		// The body is bounded by JMAIL_HTTP_MAX_BYTES instead of by time alone, so
		// these are generous but finite: a stalled client cannot pin a goroutine.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	drain := cfg.drain

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("jmail-http listening",
			"version", version,
			"addr", cfg.listen,
			"submit_to", cfg.smtpAddr,
			"origin", cfg.origin,
			"domains", cfg.domains,
			"allowed_from", cfg.allowedDomains(),
			"send_api", cfg.token != "",
			"supabase_hooks", len(cfg.supabaseHooks),
			"max_bytes", cfg.maxBytes,
			"rate", cfg.rate,
			"log_rate", cfg.logRate,
			"idempotency_dir", cfg.idempotencyDir,
			"drain_timeout", drain.String(),
		)
		serveErr <- httpSrv.ListenAndServe()
	}()

	exitCode := 0
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server stopped", "error", err)
			exitCode = 1
		}
	case <-ctx.Done():
		slog.Info("signal received, shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed, closing connections", "error", err)
		if closeErr := httpSrv.Close(); closeErr != nil {
			slog.Error("forced close failed", "error", closeErr)
		}
		exitCode = 1
	}

	slog.Info("stopped")
	srv.close()
	os.Exit(exitCode)
}
