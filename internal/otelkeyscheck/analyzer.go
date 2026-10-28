// Package otelkeyscheck provides an analyzer that enforces otelkeys conventions.
package otelkeyscheck

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/tools/go/analysis"
)

var (
	snakeCaseRE       = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	namespacedValueRE = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)*$`)
)

// Analyzer enforces ordering and value-format conventions for internal/otelkeys.
var Analyzer = &analysis.Analyzer{
	Name: "otelkeyscheck",
	Doc:  "enforces alphabetized otelkeys const blocks and key format conventions",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	if pass.Pkg == nil || pass.Pkg.Name() != "otelkeys" {
		//nolint:nilnil // framework setup failure
		return nil, nil
	}

	for _, file := range pass.Files {
		base := filepath.Base(pass.Fset.Position(file.Pos()).Filename)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			checkConstBlock(pass, gen, base)
		}
	}

	//nolint:nilnil // analyzers that do not export results to other passes return nil, nil on success
	return nil, nil
}

func checkConstBlock(pass *analysis.Pass, gen *ast.GenDecl, base string) {
	var prevName string
	var prevSortKey string
	for _, spec := range gen.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for _, name := range vs.Names {
			sortKey := strings.ToLower(name.Name)
			if prevName != "" && sortKey < prevSortKey {
				pass.Reportf(name.Pos(), "otelkeys consts in a const block must be alphabetized")
			}
			prevName = name.Name
			prevSortKey = sortKey
			checkConstValue(pass, name, base)
		}
	}
}

func checkConstValue(pass *analysis.Pass, ident *ast.Ident, base string) {
	obj, ok := pass.TypesInfo.Defs[ident].(*types.Const)
	if !ok {
		return
	}
	if obj.Val().Kind() != constant.String {
		return
	}

	value := constant.StringVal(obj.Val())
	if base == "logger_keys.go" {
		if !snakeCaseRE.MatchString(value) {
			pass.Reportf(ident.Pos(), "otelkeys value in logger_keys.go must be snake_case: %q", value)
		}

		return
	}

	if !namespacedValueRE.MatchString(value) {
		pass.Reportf(ident.Pos(), "otelkeys value must use lowercase snake_case segments (dot-separated allowed): %q", value)
	}
}
