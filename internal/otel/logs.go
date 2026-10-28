package otel

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"

	"github.com/amalgamated-tools/sheetserve/internal/version"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"charm.land/log/v2"
	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
)

const environmentVar = "ENVIRONMENT"

// SetupLogs sets up the logs for the application
func SetupLogs(ctx context.Context) (func(context.Context) error, error) {
	newctx, span := StartSpan(ctx, instrumentationName, otelkeys.OtelSetupLogs)
	defer span.End()
	defer ObserveDuration(newctx, otelkeys.OtelSetupLogsDuration)()

	exporter, err := autoexport.NewLogExporter(newctx)
	if err != nil {
		return nil, fmt.Errorf("creating log exporter: %w", err)
	}

	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
		sdklog.WithResource(resourceOrPartial(newctx)),
	)
	otel.SetLoggerProvider(provider)

	setupLogger(newctx)

	slog.DebugContext(
		newctx,
		"log export initialized",
		slog.String(otelkeys.Exporter, exporterName("OTEL_LOGS_EXPORTER")),
	)

	Incr(
		newctx,
		otelkeys.OtelSetupLogsSuccess,
	)

	return provider.Shutdown, nil
}

func setupLogger(ctx context.Context, attrs ...any) {
	format := "json"
	level := slog.LevelInfo
	charmLevel := log.InfoLevel
	addSource := false

	logFormat, ok := os.LookupEnv("LOG_FORMAT")
	if ok {
		format = logFormat
	}

	logLevel, ok := os.LookupEnv("LOG_LEVEL")
	if ok {
		switch logLevel {
		case "debug":
			level = slog.LevelDebug
			charmLevel = log.DebugLevel
			addSource = true
		case "info":
			level = slog.LevelInfo
			charmLevel = log.InfoLevel
		case "warn":
			level = slog.LevelWarn
			charmLevel = log.WarnLevel
		case "error":
			level = slog.LevelError
			charmLevel = log.ErrorLevel
		default:
			level = slog.LevelInfo
			charmLevel = log.InfoLevel
		}
	}

	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(
			os.Stdout,
			&slog.HandlerOptions{
				AddSource: addSource,
				Level:     level,
			},
		)
	} else {
		// Colorized text output for local development; disabled when stdout
		// is not a terminal (piped/redirected) or NO_COLOR is set.
		// _, noColorEnv := os.LookupEnv("NO_COLOR")
		handler = log.NewWithOptions(
			os.Stdout,
			log.Options{
				ReportCaller:    true,
				ReportTimestamp: true,
				Level:           charmLevel,
			},
		)
	}

	// Fan every record out to the OTel log bridge as well, so slog output is
	// exported as OTLP logs alongside traces and metrics. The bridge resolves
	// loggers through the global LoggerProvider, which SetupLogs installs
	// later in boot — until then (or when OTEL_LOGS_EXPORTER=none) exported
	// records go nowhere while console output is unaffected. The console
	// handler keeps its own format (text locally, json in prod); OTLP records
	// are structured regardless.
	otelHandler := minLevelHandler{
		inner: otelslog.NewHandler(instrumentationName, otelslog.WithSource(addSource)),
		level: level,
	}
	logger := slog.New(newComponentHandler(newFanoutHandler(handler, otelHandler)))

	buildVersion := version.String()

	if buildVersion == "" {
		// Try to get version from build info
		info, ok := debug.ReadBuildInfo()
		if ok {
			if info.Main.Version != "" {
				buildVersion = info.Main.Version
			}
		}
	}

	// add version to the default attributes
	attrs = append(attrs, slog.String(otelkeys.Version, buildVersion))
	// Tag every record with the normalized deployment environment, matching
	// the label traces and metrics carry.
	if env := strings.ToLower(os.Getenv(environmentVar)); env != "" {
		attrs = append(attrs, slog.String(otelkeys.Environment, env))
	}
	logger = logger.With(attrs...)

	logger.InfoContext(
		ctx,
		"Logger initialized",
	)
	slog.SetDefault(logger)
}
