// Command otelkeyscheck runs the otelkeyscheck analyzer over the provided packages.
// Usage:
//
//	go run ./cmd/otelkeyscheck ./...
package main

import (
	"github.com/amalgamated-tools/sheetserve/internal/otelkeyscheck"
	"golang.org/x/tools/go/analysis/singlechecker"
)

func main() {
	singlechecker.Main(otelkeyscheck.Analyzer)
}
