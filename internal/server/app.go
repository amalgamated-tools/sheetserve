// Command sheetserve serves Google Sheets tabs as a REST API and an MCP endpoint.
package server

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/amalgamated-tools/sheetserve/internal/api"
	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/hosted"
	"github.com/amalgamated-tools/sheetserve/internal/service"
	"github.com/amalgamated-tools/sheetserve/internal/store"
)

func Main() {
	if len(os.Args) > 1 && os.Args[1] == "keygen" {
		key := config.NewKey()
		fmt.Printf("key:  %s   (give this to the client; it is not stored)\nhash: %s   (put this in config.yaml)\n", key, config.HashKey(key))
		return
	}
	path := flag.String("config", cmp.Or(os.Getenv("SHEETSERVE_CONFIG"), "config.yaml"), "path to config file")
	flag.Parse()

	if err := run(*path); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	var h http.Handler
	if len(cfg.Spreadsheets) == 0 {
		if h, err = hosted.New(cfg); err != nil {
			return err
		}
	} else {
		sheetStore, err := store.NewGoogleStore(context.Background())
		if err != nil {
			return fmt.Errorf("google sheets client: %w", err)
		}
		h = api.New(cfg, service.New(cfg, sheetStore)).Routes()
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	slog.Info("listening", "addr", cfg.Listen, "version", api.Version, "hosted", len(cfg.Spreadsheets) == 0)
	return srv.ListenAndServe()
}
