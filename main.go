// Command sheetserve serves Google Sheets tabs as a REST API and an MCP endpoint.
package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
)

const version = "0.1.0"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "keygen" {
		key := "sk_" + rand.Text()
		fmt.Printf("key:  %s   (give this to the client; it is not stored)\nhash: %s   (put this in config.yaml)\n", key, hashKey(key))
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
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	store, err := newGoogleStore(context.Background())
	if err != nil {
		return fmt.Errorf("google sheets client: %w", err)
	}
	api := &API{cfg: cfg, svc: newService(cfg, store)}
	srv := &http.Server{Addr: cfg.Listen, Handler: api.routes(), ReadHeaderTimeout: 10 * time.Second}
	slog.Info("listening", "addr", cfg.Listen, "version", version)
	return srv.ListenAndServe()
}
