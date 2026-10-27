// Command sheetserve serves Google Sheets tabs as a REST API and an MCP endpoint.
package server

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/amalgamated-tools/sheetserve/internal/api"
	"github.com/amalgamated-tools/sheetserve/internal/config"
)

func Main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	h, err := NewHandler(cfg)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	slog.Info("listening", "addr", cfg.Listen, "version", api.Version)
	return srv.ListenAndServe()
}
