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
	Action              string = "action"
	AgeMs               string = "age_ms"
	Columns             string = "columns"
	Component           string = "component"
	DurationMs          string = "duration_ms"
	Environment         string = "environment"
	Error               string = "error"
	ExceptionStacktrace string = "exception_stacktrace"
	Exporter            string = "exporter"
	Hidden              string = "hidden"
	KeyColumn           string = "key_column"
	KeyName             string = "key_name"
	Kind                string = "kind"
	Method              string = "method"
	Name                string = "name"
	Op                  string = "op"
	Ops                 string = "ops"
	Path                string = "path"
	RemoteAddr          string = "remote_addr"
	RequestID           string = "request_id"
	Role                string = "role"
	Sheet               string = "sheet"
	SpreadsheetID       string = "spreadsheet_id"
	StatusCode          string = "status_code"
	Tab                 string = "tab"
	Tabs                string = "tabs"
	TargetUserID        string = "target_user_id"
	TraceID             string = "trace_id"
	URL                 string = "url"
	UserAgent           string = "user_agent"
	UserEmail           string = "user_email"
	UserID              string = "user_id"
	Version             string = "version"
)
