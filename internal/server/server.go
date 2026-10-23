// Command sheetserve serves Google Sheets tabs as a REST API and an MCP endpoint.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/justinas/alice"
	"golang.org/x/sync/errgroup"

	"github.com/amalgamated-tools/sheetserve/internal/config"
)

const (
	// UserAgentHeader is the header name for the user agent.
	UserAgentHeader = "User-Agent"
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

type Server struct {
	Address string

	cfg           *config.Config
	environment   string
	httpServer    *http.Server
	port          int
	router        *chi.Mux
	shutdownFuncs []ShutdownFunc
}

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
	s.setupRoutes(ctx)
	return s, nil
}

func (s *Server) setupRoutes(ctx context.Context) {
	// newctx, span := otel.StartSpan(otel.WithComponent(ctx, otel.ComponentHTTP), instrumentationName, otelkeys.ServerSetupRoutes)
	// defer span.End()
	// defer otel.ObserveDuration(newctx, otelkeys.ServerSetupRoutesDuration)()

	// slog.DebugContext(newctx, "Setting up routes")
	// s.router.Use(middleware.Heartbeat("/healthz")) // this must be before anything else
	// s.router.Mount("/debug", middleware.Profiler())

	// // everything under /slack that is a POST should call the slack middleware.
	// s.router.Mount("/slack", s.slackRoutes())

	// s.router.Get("/render/oauth/callback", http.HandlerFunc(s.handleRenderOAuthCallback))
	// s.router.Mount("/render", s.renderRoutes())

	// span.SetStatus(codes.Ok, "")
}

// middlewareChain wraps next in the server's middleware. sentryhttp reuses
// the per-request hub RequestIDHandler put on the context, and Repanic hands
// the panic back to middleware.Recoverer upstream, which answers 500.
func middlewareChain(next http.Handler) http.Handler {
	return alice.New(
		// smiddleware.RequestIDHandler,
		// smiddleware.Trace,
		// smiddleware.Metrics,
		middleware.Recoverer,
		middleware.URLFormat,
		// smiddleware.LoggingMiddleware,
		// sentryHandler.Handle,
	).Then(next)
}

// Run starts the HTTP server, applies request-ID, tracing, and logging
// middleware, and blocks until ctx is cancelled or a fatal server error
// occurs. A graceful shutdown is attempted with a ShutdownGracePeriod timeout.
func (s *Server) Run(ctx context.Context) error {
	newctx := ctx
	// newctx, span := otel.StartSpan(otel.WithComponent(ctx, otel.ComponentHTTP), instrumentationName, otelkeys.ServerRun)
	// defer span.End()
	// defer otel.ObserveDuration(newctx, otelkeys.ServerRunDuration)()

	slog.InfoContext(newctx, "Running server", slog.String("address", s.Address))

	ctx, cancel := context.WithCancel(newctx)
	defer cancel()

	chain := middlewareChain(s.router)

	s.httpServer = &http.Server{
		Addr:         s.Address,
		Handler:      chain,
		WriteTimeout: HTTPWriteTimeout,
		ReadTimeout:  HTTPReadTimeout,
		IdleTimeout:  HTTPIdleTimeout,
		// // Request contexts derive from BaseContext (not from Run's ctx), so
		// // tag it here to scope handler and access logs to the http component.
		// BaseContext: func(net.Listener) context.Context {
		// 	return otel.WithComponent(context.Background(), otel.ComponentHTTP)
		// },
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
	newctx := ctx
	// newctx, span := otel.StartSpan(otel.WithComponent(ctx, otel.ComponentHTTP), instrumentationName, otelkeys.ServerShutdown)
	// defer span.End()
	// defer otel.ObserveDuration(newctx, otelkeys.ServerShutdownDuration)()

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
	// span.SetStatus(codes.Ok, "")

	return nil
}
