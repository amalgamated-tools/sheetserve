// Package slogcheck provides a go/analysis Analyzer that enforces the
// project's structured-logging conventions on [log/slog] attribute
// constructors:
//
//   - Keys must be constants from the otelkeys package, never raw string
//     literals or constants from other packages. Centralized keys keep field
//     naming consistent and log queries reliable.
//
//   - [log/slog.Any] must not be used where a typed attribute constructor
//     exists (e.g. [log/slog.String] for string values). slog.Any should only
//     be used when no typed constructor applies — the primary legitimate use
//     case is logging error values.
package slogcheck

import (
	"errors"
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// Analyzer is the slogcheck analysis.Analyzer.
var Analyzer = &analysis.Analyzer{
	Name:     "slogcheck",
	Doc:      "enforces otelkeys constants as slog attribute keys and typed constructors over slog.Any",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

// keyPackageName is the package whose constants are the only permitted slog
// attribute keys.
const keyPackageName = "otelkeys"

// typedAlternative maps the string representation of a type to the slog
// constructor that should be used instead of slog.Any.
var typedAlternative = map[string]string{
	"string":        "slog.String",
	"int":           "slog.Int",
	"int64":         "slog.Int64",
	"uint64":        "slog.Uint64",
	"float64":       "slog.Float64",
	"bool":          "slog.Bool",
	"time.Time":     "slog.Time",
	"time.Duration": "slog.Duration",
}

// attrConstructors is the set of log/slog package-level functions whose first
// argument is an attribute key.
var attrConstructors = map[string]bool{
	"String":   true,
	"Int":      true,
	"Int64":    true,
	"Uint64":   true,
	"Float64":  true,
	"Bool":     true,
	"Time":     true,
	"Duration": true,
	"Any":      true,
	"Group":    true,
}

var (
	// ErrLookupFailed is returned when the 'error' interface cannot be found in the universe.
	ErrLookupFailed = errors.New("failed to look up 'error' interface in universe")
	// ErrMissingInspectResult is returned when the required inspect.Analyzer dependency result is missing.
	ErrMissingInspectResult = errors.New("missing required inspect.Analyzer dependency result")
)

func run(pass *analysis.Pass) (any, error) {
	errorIface, ok := types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
	if !ok {
		return nil, ErrLookupFailed
	}

	insp, ok := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	if !ok {
		return nil, ErrMissingInspectResult
	}
	nodeFilter := []ast.Node{(*ast.CallExpr)(nil)}

	insp.Preorder(nodeFilter, func(n ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}

		name, ok := slogFuncName(pass, call)
		if !ok || !attrConstructors[name] || len(call.Args) == 0 {
			return
		}

		checkKey(pass, name, call.Args[0])

		if name == "Any" {
			checkAnyValue(pass, errorIface, call)
		}
	})

	//nolint:nilnil // analyzers that do not export results to other passes return nil, nil on success
	return nil, nil
}

// checkKey reports the key argument of a slog attribute constructor unless it
// is a constant from the otelkeys package.
func checkKey(pass *analysis.Pass, funcName string, key ast.Expr) {
	var obj types.Object
	switch e := key.(type) {
	case *ast.SelectorExpr:
		obj = pass.TypesInfo.Uses[e.Sel]
	case *ast.Ident: // dot-imported constant
		obj = pass.TypesInfo.Uses[e]
	}

	if cnst, ok := obj.(*types.Const); ok {
		if pkg := cnst.Pkg(); pkg != nil && pkg.Name() == keyPackageName {
			return
		}
	}

	pass.Reportf(key.Pos(), "slog.%s key must be a constant from the %s package", funcName, keyPackageName)
}

// checkAnyValue reports slog.Any calls whose value argument has a more
// specific typed constructor available. Error values are exempt.
func checkAnyValue(pass *analysis.Pass, errorIface *types.Interface, call *ast.CallExpr) {
	// slog.Any(key, value) — check the value argument (index 1).
	if len(call.Args) < 2 {
		return
	}
	valueArg := call.Args[1]
	t := pass.TypesInfo.TypeOf(valueArg)
	if t == nil {
		return
	}

	// Allow error types — slog.Any is the canonical way to log errors.
	if types.Implements(t, errorIface) {
		return
	}
	// Also allow T when *T implements error (e.g. a non-pointer value
	// whose pointer receiver satisfies the error interface).
	if _, ok := t.(*types.Pointer); !ok && types.Implements(types.NewPointer(t), errorIface) {
		return
	}

	// Normalize untyped basic types (e.g. untyped string from a literal)
	// to their default typed form so the map lookup matches.
	lookup := t
	if basic, ok := t.(*types.Basic); ok && basic.Info()&types.IsUntyped != 0 {
		lookup = types.Default(t)
	}

	// Report if the type has a direct slog constructor.
	if alt, found := typedAlternative[lookup.String()]; found {
		pass.Reportf(call.Pos(), "use %s instead of slog.Any for %s values", alt, lookup)
	}
}

// slogFuncName returns the name of the log/slog package-level function being
// called, if call is such an invocation.
func slogFuncName(pass *analysis.Pass, call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	obj := pass.TypesInfo.Uses[sel.Sel]
	if obj == nil {
		return "", false
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return "", false
	}
	pkg := fn.Pkg()
	if pkg == nil || pkg.Path() != "log/slog" {
		return "", false
	}

	return fn.Name(), true
}
