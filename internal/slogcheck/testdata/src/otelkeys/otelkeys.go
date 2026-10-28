// Package otelkeys is a test fixture mirroring the real otelkeys package:
// slogcheck permits slog attribute keys only when they are constants from a
// package with this name.
package otelkeys

const (
	Error  string = "error"
	Key    string = "key"
	Struct string = "struct"
)
