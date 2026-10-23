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
	"github.com/amalgamated-tools/sheetserve/internal/otel"
	"github.com/amalgamated-tools/sheetserve/internal/server"
	"golang.org/x/sync/errgroup"
)

// ShutdownTimeout bounds telemetry flush on exit.
const ShutdownTimeout = 10 * time.Second

// Func is the server's startup logic, invoked once config, logging, telemetry,
// and the database are ready. Server-specific setup belongs here, not in Main.
type Func func(ctx context.Context, httpServer *server.Server) error

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
		slog.ErrorContext(ctx, "error occurred", slog.Any("error", err))
		os.Exit(1)
	}

	os.Exit(0)
}

func realMain(ctx context.Context) error {
	cfg, err := config.FromEnv()
	if err != nil {
		slog.ErrorContext(ctx, "failed to load config", slog.Any("error", err))
		return err
	}
	shutdownTelemetry, err := otel.SetupRuntimeTelemetry(ctx, ShutdownTimeout)
	if err != nil {
		// if this errors, we will log it in the Main exit path
		return fmt.Errorf("failed to set up telemetry: %w", err)
	}
	defer shutdownTelemetry()
	_ = cfg

	// slog.InfoContext(ctx, "Initializing HTTP server")
	httpServer, err := server.New(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to initialize HTTP server", slog.Any("error", err))
		return err
	}

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		slog.InfoContext(gctx, "Starting HTTP server")
		return httpServer.Run(gctx)
	})

	err = g.Wait()
	if err != nil {
		slog.ErrorContext(ctx, "error occurred", slog.Any("error", err))
		return err
	}

	// return 0
	return nil
}
