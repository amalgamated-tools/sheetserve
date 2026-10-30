// Package server wires sign-in, each user's API and the dashboard into one HTTP server.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/codes"
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
	smiddleware "github.com/amalgamated-tools/sheetserve/internal/server/middleware"
	"github.com/amalgamated-tools/sheetserve/internal/store"
	"github.com/amalgamated-tools/sheetserve/internal/tenancy"
	"github.com/amalgamated-tools/sheetserve/internal/web"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/justinas/alice"
)

const (
	// UserAgentHeader is the header name for the user agent.
	UserAgentHeader = "User-Agent"
	// HTTPReadHeaderTimeout is the maximum duration for reading the request headers.
	HTTPReadHeaderTimeout = 10 * time.Second
	// HTTPWriteTimeout is the maximum duration before timing out writes of the response.
	// Set high enough to accommodate large uploads (e.g. 100 MB Calibre database imports).
	HTTPWriteTimeout = 120 * time.Second
	// HTTPReadTimeout is the maximum duration for reading the entire request, including the body.
	// Set high enough to accommodate large uploads on slow connections.
	HTTPReadTimeout = 120 * time.Second
	// HTTPIdleTimeout is the maximum amount of time to wait for the next request when keep-alives are enabled.
	HTTPIdleTimeout = 30 * time.Second
	// HTTPRequestTimeout is the maximum duration for handling a single HTTP request.
	HTTPRequestTimeout = 10 * time.Second
	// ShutdownGracePeriod is the time we allow for graceful shutdown of the http server.
	// Must fit inside Render's SIGTERM-to-SIGKILL window (30s by default; see
	// maxShutdownDelaySeconds), with headroom for the worker drain and webhook
	// cleanup that share it. Requests still in flight when it expires are cut off.
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
	limiter *store.Limiter // keeps every user's Google calls under the Sheets API quotas
	dash    *web.Dashboard

	httpServer    *http.Server
	port          int
	router        *chi.Mux
	shutdownFuncs []ShutdownFunc
}

// New connects sign-in, each user's API, and the dashboard that edits them, with users,
// spreadsheets and keys kept in d.
func New(ctx context.Context, opts ...Option) (*Server, error) {
	s := &Server{
		router: chi.NewRouter(),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.port == 0 {
		s.port = 8085
	}
	s.Address = net.JoinHostPort("0.0.0.0", strconv.Itoa(s.port))

	s.auth = auth.New(s.db, &oauth2.Config{
		ClientID:     s.cfg.GoogleClientID,
		ClientSecret: s.cfg.GoogleClientSecret,
		Endpoint:     google.Endpoint,
		RedirectURL:  strings.TrimSuffix(s.cfg.BaseURL, "/") + "/auth/callback",
		Scopes:       []string{"openid", "email", "https://www.googleapis.com/auth/drive.file"},
	})
	s.limiter = store.NewLimiter(s.cfg.ProjectQuota, s.cfg.UserQuota, s.cfg.KeyQuota)
	s.tenants = &tenancy.Tenants{DB: s.db, Global: s.cfg, NewStore: s.newStore}
	s.auth.OnSignIn = s.tenants.Invalidate // pick up the new refresh token
	s.dash = &web.Dashboard{
		DB: s.db, Auth: s.auth, Tenants: s.tenants,
		PickerKey: s.cfg.GooglePickerAPIKey, AppID: s.cfg.GoogleProjectNumber,
		OperatorName: s.cfg.OperatorName, OperatorContact: s.cfg.OperatorContact, BaseURL: s.cfg.BaseURL,
	}
	s.setupRoutes(ctx)
	return s, nil
}

// newStore reaches Google Sheets with userID's own credentials, within the server's quotas.
func (s *Server) newStore(userID int64) (store.Store, error) {
	ts, err := s.auth.TokenSource(userID)
	if err != nil {
		return nil, err
	}
	g, err := store.NewGoogleStore(context.Background(), option.WithTokenSource(ts))
	if err != nil {
		return nil, err
	}
	return store.Limit(g, s.limiter, userID), nil
}

// setupRoutes sends each request to sign-in, the per-user API, or the dashboard.
func (s *Server) setupRoutes(ctx context.Context) {
	newctx, span := otel.StartSpan(otel.WithComponent(ctx, otel.ComponentServer), instrumentationName, otelkeys.ServerSetupRoutes)
	defer span.End()
	defer otel.ObserveDuration(newctx, otelkeys.ServerSetupRoutesDuration)()

	slog.DebugContext(newctx, "Setting up routes")
	s.router.Use(middleware.Heartbeat("/healthz")) // this must be before anything else
	s.router.Use(func(next http.Handler) http.Handler { return api.CORS(s.cfg.CORSOrigins, next) })
	s.router.Mount("/debug", middleware.Profiler())

	s.auth.Routes(s.router)
	s.dash.Routes(s.router)
	s.router.Group(func(r chi.Router) {
		r.Use(s.tenants.Middleware)
		api.Routes(r)
	})
}

// middlewareChain wraps next in the server's middleware. sentryhttp reuses
// the per-request hub RequestIDHandler put on the context, and Repanic hands
// the panic back to middleware.Recoverer upstream, which answers 500.
func middlewareChain(next http.Handler) http.Handler {
	return alice.New(
		smiddleware.RequestIDHandler,
		smiddleware.Trace,
		smiddleware.Metrics,
		middleware.Recoverer,
		smiddleware.LoggingMiddleware,
	).Then(next)
}

// Run serves HTTP and blocks until ctx is cancelled or a fatal server error
// occurs. A graceful shutdown is attempted with a ShutdownGracePeriod timeout.
func (s *Server) Run(ctx context.Context) error {
	newctx, span := otel.StartSpan(otel.WithComponent(ctx, otel.ComponentServer), instrumentationName, otelkeys.ServerRun)
	defer span.End()
	defer otel.ObserveDuration(newctx, otelkeys.ServerRunDuration)()

	slog.InfoContext(newctx, "Running server", slog.String(otelkeys.ServerAddress, s.Address))

	ctx, cancel := context.WithCancel(newctx)
	defer cancel()

	chain := middlewareChain(s.router)
	s.httpServer = &http.Server{
		Addr:              s.Address,
		Handler:           chain,
		ReadHeaderTimeout: HTTPReadHeaderTimeout,
		WriteTimeout:      HTTPWriteTimeout,
		ReadTimeout:       HTTPReadTimeout,
		IdleTimeout:       HTTPIdleTimeout,
		// Request contexts derive from BaseContext (not from Run's ctx), so
		// tag it here to scope handler and access logs to the http component.
		BaseContext: func(net.Listener) context.Context {
			return otel.WithComponent(context.Background(), otel.ComponentServer)
		},
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
			// otel.RecordError(span, "server shutdown failed", err)
			return fmt.Errorf("server shutdown failed: %w", err)
		}
		// span.SetStatus(codes.Ok, "")

		return nil

	case err := <-errCh:
		// otel.RecordError(span, "http server error", err)
		s.shutdownFuncs = append(s.shutdownFuncs, func(ctx context.Context) error {
			slog.InfoContext(ctx, "Shutting down HTTP server due to error")
			return fmt.Errorf("HTTP server error: %w", err)
		})
		cancel()
		<-ctx.Done()
		shutdownErr := s.shutdown(ctx)
		if shutdownErr != nil {
			// otel.RecordError(span, "server shutdown failed after server error", shutdownErr)
			return fmt.Errorf("server shutdown failed after server error: %w", shutdownErr)
		}

		return shutdownErr
	}
}

func (s *Server) shutdown(ctx context.Context) error {
	newctx, span := otel.StartSpan(otel.WithComponent(ctx, otel.ComponentServer), instrumentationName, otelkeys.ServerShutdown)
	defer span.End()
	defer otel.ObserveDuration(newctx, otelkeys.ServerShutdownDuration)()

	// The incoming ctx is typically already cancelled (that's what triggered
	// shutdown), so detach from it before applying the grace-period timeout —
	// otherwise httpServer.Shutdown would give up immediately instead of
	// draining in-flight requests.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(newctx), ShutdownGracePeriod)
	defer cancel()

	shutdownGroup, ctx := errgroup.WithContext(ctx)

	for _, shutdownFn := range s.shutdownFuncs {
		shutdownGroup.Go(func() error {
			return shutdownFn(ctx)
		})
	}

	if err := shutdownGroup.Wait(); err != nil {
		// otel.RecordError(span, "shutdown functions failed", err)
		return fmt.Errorf("shutdown functions failed: %w", err)
	}
	span.SetStatus(codes.Ok, "")

	return nil
}
