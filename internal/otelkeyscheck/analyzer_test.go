package otelkeyscheck_test

import (
	"testing"

	"github.com/amalgamated-tools/sheetserve/internal/otelkeyscheck"
	"golang.org/x/tools/go/analysis/analysistest"
)

func TestAnalyzer(t *testing.T) {
	t.Parallel()
	analysistest.Run(t, analysistest.TestData(), otelkeyscheck.Analyzer, "otelkeys")
}
