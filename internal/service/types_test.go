package service

import (
	"encoding/json"
	"testing"

	"github.com/amalgamated-tools/sheetserve/internal/db"
)

func TestCheckValue(t *testing.T) {
	for _, c := range []struct {
		typ      string
		required bool
		v        any
		ok       bool
	}{
		{"", false, map[string]any{"a": 1}, true},
		{"string", false, "x", true},
		{"string", false, "", true},
		{"string", true, "", false},
		{"string", true, nil, false},
		{"number", false, json.Number("2.5"), true},
		{"number", false, 2.5, true}, // MCP arguments decode numbers as float64
		{"number", false, "2.5", false},
		{"number", false, nil, true},
		{"number", false, "", false},
		{"integer", false, json.Number("3"), true},
		{"integer", false, json.Number("3.0"), true},
		{"integer", false, 3.0, true},
		{"integer", false, 3.5, false},
		{"boolean", false, true, true},
		{"boolean", false, "true", false},
		{"date", false, "2026-01-31", true},
		{"date", false, "2026-02-31", false},
		{"date", false, "1/31/2026", false},
		{"datetime", false, "2026-01-31T09:30:00Z", true},
		{"datetime", false, "2026-01-31", false},
	} {
		err := checkValue("col", db.Column{Type: c.typ, Required: c.required}, c.v)
		if (err == nil) != c.ok {
			t.Errorf("%s required=%v %#v: %v", c.typ, c.required, c.v, err)
		}
	}
}

func TestReadValue(t *testing.T) {
	for _, c := range []struct {
		typ  string
		in   any
		want any
	}{
		{"", "", ""},
		{"", 4.0, 4.0},
		{"string", 4.0, "4"},
		{"string", 0.1, "0.1"},
		{"string", true, "true"},
		{"string", "", nil},
		{"number", " 4.5 ", 4.5},
		{"number", "n/a", "n/a"}, // typed by hand; comes back as it is
		{"integer", 4.0, int64(4)},
		{"integer", "12", int64(12)},
		{"integer", 4.5, 4.5},
		{"boolean", "TRUE", true},
		{"boolean", "1", "1"},
		{"date", "2026-01-31", "2026-01-31"},
		{"date", nil, nil},
	} {
		if got := readValue(c.typ, c.in); got != c.want {
			t.Errorf("readValue(%q, %#v) = %#v, want %#v", c.typ, c.in, got, c.want)
		}
	}
}

func TestEnumAndDefaults(t *testing.T) {
	size := db.Column{Type: "enum", Options: []string{"S", "M"}}
	for v, ok := range map[any]bool{"S": true, "M": true, "L": false, "s": false, 1.0: false} {
		if err := checkValue("size", size, v); (err == nil) != ok {
			t.Errorf("checkValue(size, %#v): %v", v, err)
		}
	}
	if err := checkValue("size", size, "L"); err == nil || err.Error() != `size must be one of S, M, got "L"` {
		t.Errorf("enum message: %v", err)
	}
	for _, c := range []struct {
		col db.Column
		ok  bool
	}{
		{db.Column{Type: "enum"}, false},
		{db.Column{Type: "enum", Options: []string{"S"}, Default: "S"}, true},
		{db.Column{Type: "enum", Options: []string{"S"}, Default: "XL"}, false},
		{db.Column{Type: "integer", Default: "3"}, true},
		{db.Column{Type: "integer", Default: "3.5"}, false},
		{db.Column{Type: "integer", Default: "three"}, false},
		{db.Column{Type: "boolean", Default: "TRUE"}, true},
		{db.Column{Type: "boolean", Default: "yes"}, false},
		{db.Column{Type: "date", Default: "2026-01-31"}, true},
		{db.Column{Type: "date", Default: "soon"}, false},
		{db.Column{Default: "anything"}, true},
	} {
		if err := CheckColumn("col", c.col); (err == nil) != c.ok {
			t.Errorf("CheckColumn(%+v): %v", c.col, err)
		}
	}
	for _, c := range []struct {
		col  db.Column
		want any
	}{
		{db.Column{Type: "integer", Default: "3"}, json.Number("3")},
		{db.Column{Type: "boolean", Default: "TRUE"}, true},
		{db.Column{Type: "enum", Options: []string{"S"}, Default: "S"}, "S"},
	} {
		if got, ok := DefaultValue(c.col); !ok || got != c.want {
			t.Errorf("DefaultValue(%+v) = %#v, want %#v", c.col, got, c.want)
		}
	}
}
