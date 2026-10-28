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
	Component           string = "component"
	Environment         string = "environment"
	Error               string = "error"
	ExceptionStacktrace string = "exception_stacktrace"
	Exporter            string = "exporter"
	Kind                string = "kind"
	Method              string = "method"
	Name                string = "name"
	RemoteAddr          string = "remote_addr"
	RequestID           string = "request_id"
	StatusCode          string = "status_code"
	URL                 string = "url"
	UserAgent           string = "user_agent"
	Version             string = "version"
)
