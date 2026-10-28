// Command slogcheck runs the slogcheck analyzer over the provided packages.
// Usage:
//
//	go run ./cmd/slogcheck ./...
package main

import (
	"github.com/amalgamated-tools/sheetserve/internal/slogcheck"
	"golang.org/x/tools/go/analysis/singlechecker"
)

func main() {
	singlechecker.Main(slogcheck.Analyzer)
}
