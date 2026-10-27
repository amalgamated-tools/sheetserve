package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/amalgamated-tools/sheetserve/internal/api"
	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/otel"
	"github.com/amalgamated-tools/sheetserve/internal/server"
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
		slog.ErrorContext(ctx, "error occurred", slog.Any("error", err))
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

	h, err := server.NewHandler(cfg)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: ":" + cfg.Listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	slog.Info("listening", "addr", ":"+cfg.Listen, "version", api.Version)
	return srv.ListenAndServe()
}
