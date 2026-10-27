// Package server wires sign-in, each user's API and the dashboard into one HTTP server.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/option"

	"github.com/amalgamated-tools/sheetserve/internal/api"
	"github.com/amalgamated-tools/sheetserve/internal/auth"
	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/otel"
	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
	"github.com/amalgamated-tools/sheetserve/internal/store"
	"github.com/amalgamated-tools/sheetserve/internal/tenancy"
	"github.com/amalgamated-tools/sheetserve/internal/web"
)

const (
	// HTTPReadHeaderTimeout is the maximum duration for reading the request headers.
	HTTPReadHeaderTimeout = 10 * time.Second
	// HTTPWriteTimeout is the maximum duration before timing out writes of the response.
	HTTPWriteTimeout = 120 * time.Second
	// HTTPReadTimeout is the maximum duration for reading the entire request, including the body.
	HTTPReadTimeout = 120 * time.Second
	// HTTPIdleTimeout is the maximum amount of time to wait for the next request when keep-alives are enabled.
	HTTPIdleTimeout = 30 * time.Second
	// ShutdownGracePeriod is the time we allow for graceful shutdown of the http server.
	// Must fit inside Render's SIGTERM-to-SIGKILL window (30s by default; see
	// maxShutdownDelaySeconds). Requests still in flight when it expires are cut off.
	ShutdownGracePeriod = 20 * time.Second
)

// ShutdownFunc is a function that takes a context and returns an error
type ShutdownFunc func(context.Context) error

// Server owns sign-in, each user's API and the dashboard, and serves them over HTTP.
type Server struct {
	Address string

	cfg     *config.Config
	db      *db.DB
	auth    *auth.Google
	tenants *tenancy.Tenants
	dash    *web.Dashboard

	handler       http.Handler
	httpServer    *http.Server
	shutdownFuncs []ShutdownFunc
}

// New connects sign-in, each user's API, and the dashboard that edits them, with users,
// spreadsheets and keys kept in d.
func New(ctx context.Context, cfg *config.Config, d *db.DB) (*Server, error) {
	s := &Server{
		Address: net.JoinHostPort("", cfg.Listen),
		cfg:     cfg,
		db:      d,
	}
	s.auth = auth.New(d, &oauth2.Config{
		ClientID:     cfg.GoogleClientID,
		ClientSecret: cfg.GoogleClientSecret,
		Endpoint:     google.Endpoint,
		RedirectURL:  strings.TrimSuffix(cfg.BaseURL, "/") + "/auth/callback",
		Scopes:       []string{"openid", "email", "https://www.googleapis.com/auth/drive.file"},
	})
	s.tenants = &tenancy.Tenants{DB: d, Global: cfg, NewStore: s.newStore}
	s.auth.OnSignIn = s.tenants.Invalidate // pick up the new refresh token
	s.dash = &web.Dashboard{
		DB: d, Auth: s.auth, Tenants: s.tenants,
		PickerKey: cfg.GooglePickerAPIKey, AppID: cfg.GoogleProjectNumber,
	}
	s.handler = s.routes(ctx)
	return s, nil
}

// newStore reaches Google Sheets with userID's own credentials.
func (s *Server) newStore(userID int64) (store.Store, error) {
	ts, err := s.auth.TokenSource(userID)
	if err != nil {
		return nil, err
	}
	return store.NewGoogleStore(context.Background(), option.WithTokenSource(ts))
}

// routes sends each request to sign-in, the per-user API, or the dashboard.
func (s *Server) routes(ctx context.Context) http.Handler {
	ctx, span := otel.StartSpan(otel.WithComponent(ctx, otel.ComponentServer), instrumentationName, otelkeys.ServerSetupRoutes)
	defer span.End()
	defer otel.ObserveDuration(ctx, otelkeys.ServerSetupRoutesDuration)()

	slog.DebugContext(ctx, "Setting up routes")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /login", s.auth.Login)
	mux.HandleFunc("GET /auth/callback", s.auth.Callback)
	mux.Handle("/v1/", s.tenants)
	mux.Handle("/mcp", s.tenants)
	mux.Handle("GET /openapi.json", s.tenants)
	mux.Handle("/", s.dash.Handler())
	return api.CORS(s.cfg.CORSOrigins, mux)
}

// Run serves HTTP and blocks until ctx is cancelled or a fatal server error
// occurs. A graceful shutdown is attempted with a ShutdownGracePeriod timeout.
func (s *Server) Run(ctx context.Context) error {
	slog.InfoContext(ctx, "Running server", slog.String("address", s.Address), slog.String("version", api.Version))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.httpServer = &http.Server{
		Addr:              s.Address,
		Handler:           s.handler,
		ReadHeaderTimeout: HTTPReadHeaderTimeout,
		WriteTimeout:      HTTPWriteTimeout,
		ReadTimeout:       HTTPReadTimeout,
		IdleTimeout:       HTTPIdleTimeout,
	}

	errCh := make(chan error, 1)

	go func() {
		err := s.httpServer.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	s.shutdownFuncs = append(s.shutdownFuncs, s.httpServer.Shutdown)

	select {
	case <-ctx.Done():
		if err := s.shutdown(ctx); err != nil {
			return fmt.Errorf("server shutdown failed: %w", err)
		}
		return nil

	case err := <-errCh:
		return errors.Join(fmt.Errorf("HTTP server error: %w", err), s.shutdown(ctx))
	}
}

func (s *Server) shutdown(ctx context.Context) error {
	// The incoming ctx is typically already cancelled (that's what triggered
	// shutdown), so detach from it before applying the grace-period timeout —
	// otherwise httpServer.Shutdown would give up immediately instead of
	// draining in-flight requests.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ShutdownGracePeriod)
	defer cancel()

	shutdownGroup, ctx := errgroup.WithContext(ctx)

	for _, shutdownFn := range s.shutdownFuncs {
		shutdownGroup.Go(func() error {
			return shutdownFn(ctx)
		})
	}

	if err := shutdownGroup.Wait(); err != nil {
		return fmt.Errorf("shutdown functions failed: %w", err)
	}
	return nil
}
