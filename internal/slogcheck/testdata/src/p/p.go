// Package p contains slogcheck test fixtures.
package p

import (
	"errors"
	"log/slog"
	"time"

	"otelkeys"
)

// errorValue implements the error interface.
type errorValue struct{ msg string }

func (e *errorValue) Error() string { return e.msg }

// myStruct is a complex type with no direct slog equivalent.
type myStruct struct{ X int }

// localKey is a constant, but not from the otelkeys package.
const localKey = "local"

func examples() {
	var (
		s   string
		i   int
		i64 int64
		u64 uint64
		f64 float64
		b   bool
		t   time.Time
		d   time.Duration
	)

	// Primitive types — all should be flagged.
	slog.Any(otelkeys.Key, s)   // want `use slog\.String instead of slog\.Any for string values`
	slog.Any(otelkeys.Key, i)   // want `use slog\.Int instead of slog\.Any for int values`
	slog.Any(otelkeys.Key, i64) // want `use slog\.Int64 instead of slog\.Any for int64 values`
	slog.Any(otelkeys.Key, u64) // want `use slog\.Uint64 instead of slog\.Any for uint64 values`
	slog.Any(otelkeys.Key, f64) // want `use slog\.Float64 instead of slog\.Any for float64 values`
	slog.Any(otelkeys.Key, b)   // want `use slog\.Bool instead of slog\.Any for bool values`
	slog.Any(otelkeys.Key, t)   // want `use slog\.Time instead of slog\.Any for time\.Time values`
	slog.Any(otelkeys.Key, d)   // want `use slog\.Duration instead of slog\.Any for time\.Duration values`

	// Untyped literals — should also be flagged.
	slog.Any(otelkeys.Key, "value") // want `use slog\.String instead of slog\.Any for string values`
	slog.Any(otelkeys.Key, 1)       // want `use slog\.Int instead of slog\.Any for int values`
	slog.Any(otelkeys.Key, true)    // want `use slog\.Bool instead of slog\.Any for bool values`
	slog.Any(otelkeys.Key, 3.14)    // want `use slog\.Float64 instead of slog\.Any for float64 values`

	// Untyped constant — should also be flagged.
	const untypedStr = "hello"
	slog.Any(otelkeys.Key, untypedStr) // want `use slog\.String instead of slog\.Any for string values`

	// error values — must not be flagged.
	err := errors.New("oops")
	slog.Any(otelkeys.Error, err)
	var ev *errorValue
	slog.Any(otelkeys.Error, ev)
	// Non-pointer value where *T implements error — must not be flagged.
	var evVal errorValue
	slog.Any(otelkeys.Error, evVal)

	// Complex struct — no direct slog equivalent, must not be flagged.
	slog.Any(otelkeys.Struct, &myStruct{X: 1})
}

func keyExamples() {
	// Raw string literal keys — all should be flagged.
	slog.String("key", "value")       // want `slog\.String key must be a constant from the otelkeys package`
	slog.Int("key", 1)                // want `slog\.Int key must be a constant from the otelkeys package`
	slog.Int64("key", 1)              // want `slog\.Int64 key must be a constant from the otelkeys package`
	slog.Uint64("key", 1)             // want `slog\.Uint64 key must be a constant from the otelkeys package`
	slog.Float64("key", 1.0)          // want `slog\.Float64 key must be a constant from the otelkeys package`
	slog.Bool("key", true)            // want `slog\.Bool key must be a constant from the otelkeys package`
	slog.Time("key", time.Time{})     // want `slog\.Time key must be a constant from the otelkeys package`
	slog.Duration("key", time.Second) // want `slog\.Duration key must be a constant from the otelkeys package`
	slog.Group("key")                 // want `slog\.Group key must be a constant from the otelkeys package`
	err := errors.New("oops")
	slog.Any("error", err) // want `slog\.Any key must be a constant from the otelkeys package`

	// Constants from other packages or scopes — should be flagged too.
	slog.String(localKey, "value") // want `slog\.String key must be a constant from the otelkeys package`
	const inline = "inline"
	slog.String(inline, "value") // want `slog\.String key must be a constant from the otelkeys package`

	// Variables — should be flagged.
	keyVar := "key"
	slog.String(keyVar, "value") // want `slog\.String key must be a constant from the otelkeys package`

	// otelkeys constants — must not be flagged.
	slog.String(otelkeys.Key, "value")
	slog.Int(otelkeys.Key, 1)
	slog.Bool(otelkeys.Key, true)
	slog.Group(otelkeys.Key, slog.String(otelkeys.Key, "value"))
	slog.Any(otelkeys.Error, err)
}
