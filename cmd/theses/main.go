package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "time/tzdata" // so TZ works on a runtime image with no zone files

	"github.com/davidtorcivia/theses/internal/backup"
	"github.com/davidtorcivia/theses/internal/config"
	"github.com/davidtorcivia/theses/internal/server"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

// Version is stamped at build time: -ldflags="-X main.Version=<sha>".
var Version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "theses:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadEnv()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	// Before anything opens the data directory, because this is the one moment
	// nothing in it is in use.
	if err := backup.Sweep(cfg.DataDir, log); err != nil {
		return err
	}

	db, err := store.Open(filepath.Join(cfg.DataDir, "theses.db"))
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	set, err := settings.Open(ctx, db, cfg.SecretKey)
	if err != nil {
		return err
	}
	srv, err := server.New(cfg, db, set, log, Version)
	if err != nil {
		return err
	}

	// Registered after the database is opened, so it runs before the database
	// closes: a restore started from the settings page outlives the request and
	// must not have the file pulled out from under it half way.
	defer srv.Backups().Stop()

	// The outbox worker stops with ctx. A send caught by the cancellation
	// leaves its row untouched and goes out again on the next start; the
	// shutdown path waits here so the goroutine is gone before the process is.
	mailDone := make(chan struct{})
	go func() {
		defer close(mailDone)
		srv.Mail().Run(ctx)
	}()

	// The scheduler stops with ctx too. It holds no row and writes nothing until
	// the configured minute, so a cancellation between two ticks costs nothing.
	backupDone := make(chan struct{})
	go func() {
		defer close(backupDone)
		srv.Backups().Schedule(ctx)
	}()

	httpSrv := &http.Server{
		Addr:    cfg.Bind,
		Handler: srv,
		// Without these a connection that stops sending, or stops reading, holds
		// a goroutine and its memory until the process ends.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	done := make(chan error, 1)
	go func() {
		log.Info("theses listening", "version", Version, "bind", cfg.Bind, "base_url", cfg.BaseURL, "dev", cfg.Dev)
		err := httpSrv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdown); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		<-mailDone
		<-backupDone
		return <-done
	}
}
