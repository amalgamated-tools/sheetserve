// Package otelkeys defines string constants for all structured log field keys
// used in slog calls throughout the codebase. Centralizing keys
// here ensures consistent field naming across all packages and enables
// reliable log querying and alerting.
package otelkeys

// This file defines common attribute keys for structured logging and telemetry.
// Keep these in alphabetical order and snake_case to match OpenTelemetry conventions.
// Avoid redundant key prefixes; prefer concise, domain-meaningful names, and rely on
// component/source metadata for origin context when available.
const (
	Address                  string = "address"
	AppType                  string = "app_type"
	Component                string = "component"
	Count                    string = "count"
	Environment              string = "environment"
	Error                    string = "error"
	Exporter                 string = "exporter"
	Kind                     string = "kind"
	Method                   string = "method"
	Name                     string = "name"
	OtelSetupLogs            string = "otel.setup_logs"
	OtelSetupLogsDuration    string = "otel.setup_logs.duration"
	OtelSetupLogsSuccess     string = "otel.setup_logs.success"
	OtelSetupMetrics         string = "otel.setup_metrics"
	OtelSetupMetricsDuration string = "otel.setup_metrics.duration"
	OtelSetupMetricsSuccess  string = "otel.setup_metrics.success"
	OtelSetupRuntime         string = "otel.setup_runtime"
	OtelSetupRuntimeDuration string = "otel.setup_runtime.duration"
	OtelSetupRuntimeSuccess  string = "otel.setup_runtime.success"
	OtelSetupTracing         string = "otel.setup_tracing"
	OtelSetupTracingDuration string = "otel.setup_tracing.duration"
	OtelSetupTracingSuccess  string = "otel.setup_tracing.success"
	Owner                    string = "owner"
	Path                     string = "path"
	Port                     string = "port"
	RemoteAddr               string = "remote_addr"
	RequestID                string = "request_id"
	StatusCode               string = "status_code"
	URL                      string = "url"
	UserAgent                string = "user_agent"
	Version                  string = "version"
)
