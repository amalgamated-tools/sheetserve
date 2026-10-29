package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/otel"
	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
	"github.com/amalgamated-tools/sheetserve/internal/server"
	"golang.org/x/sync/errgroup"
)

// ShutdownTimeout bounds telemetry flush on exit.
const ShutdownTimeout = 10 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// After the first signal/cancel, restore default signal handling so a
	// second SIGINT/SIGTERM force-terminates the process.
	go func() {
		<-ctx.Done()
		stop()
	}()

	if err := realMain(ctx); err != nil {
		slog.ErrorContext(ctx, "error occurred", slog.Any(otelkeys.Error, err))
		os.Exit(1)
	}

	os.Exit(0)
}

func realMain(ctx context.Context) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	shutdownTelemetry, err := otel.SetupRuntimeTelemetry(ctx, ShutdownTimeout)
	if err != nil {
		// if this errors, we will log it in the Main exit path
		return fmt.Errorf("failed to set up telemetry: %w", err)
	}
	defer shutdownTelemetry()

	d, err := db.Open(cfg.DatabaseURL, cfg.DecryptedSecret)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	s, err := server.New(
		ctx,
		server.WithConfig(cfg),
		server.WithDB(d),
		server.WithPort(cfg.Port),
	)
	if err != nil {
		return err
	}
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		slog.InfoContext(gctx, "Starting HTTP server")
		return s.Run(gctx)
	})

	err = g.Wait()
	if err != nil {
		slog.ErrorContext(ctx, "error occurred", slog.Any(otelkeys.Error, err))
		return err
	}

	return nil
}
