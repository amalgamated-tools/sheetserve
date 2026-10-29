package service

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/store"
)

// typeNames say what a value of each column type looks like, for error messages.
var typeNames = map[string]string{
	"string":   "a string",
	"number":   "a number",
	"integer":  "an integer",
	"boolean":  "true or false",
	"date":     "a date like 2026-01-31",
	"datetime": "a date and time like 2026-01-31T09:30:00Z",
	"enum":     "one of its values",
}

// checkValue returns why v can't go in column name, or nil. null or "" empties the cell, which a
// required column refuses and a column of a non-string type takes only as null.
func checkValue(name string, c db.Column, v any) error {
	if v == nil || v == "" {
		switch {
		case c.Required:
			return BadRequest("%s is required", name)
		case v == "" && c.Type != "" && c.Type != "string":
			return BadRequest("%s must be %s, or null to empty it", name, typeNames[c.Type])
		}
		return nil
	}
	ok := true
	switch c.Type {
	case "string":
		_, ok = v.(string)
	case "number":
		_, ok = toFloat(v)
	case "integer":
		f, isNum := toFloat(v)
		ok = isNum && f == math.Trunc(f)
	case "boolean":
		_, ok = v.(bool)
	case "date", "datetime":
		layout := map[string]string{"date": time.DateOnly, "datetime": time.RFC3339}[c.Type]
		s, isString := v.(string)
		_, err := time.Parse(layout, s)
		ok = isString && err == nil
	case "enum":
		s, isString := v.(string)
		if !isString || !slices.Contains(c.Options, s) {
			got, _ := json.Marshal(v)
			return BadRequest("%s must be one of %s, got %s", name, strings.Join(c.Options, ", "), got)
		}
	}
	if !ok {
		got, _ := json.Marshal(v)
		return BadRequest("%s must be %s, got %s", name, typeNames[c.Type], got)
	}
	return nil
}

// toFloat returns a JSON number's value. REST bodies decode numbers as json.Number, MCP arguments
// as float64.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	}
	return 0, false
}

// readValue converts a cell to its column's type. Sheets returns a blank cell as "" and a number
// typed in as text as a string. A value that doesn't convert, such as text someone typed into a
// number column by hand, comes back as it is.
func readValue(typ string, v any) any {
	if typ == "" {
		return v
	}
	if v == nil || v == "" {
		return nil
	}
	s, isString := v.(string)
	s = strings.TrimSpace(s)
	switch typ {
	case "string", "enum":
		if f, ok := v.(float64); ok {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
		if !isString {
			return fmt.Sprint(v)
		}
	case "number":
		if f, err := strconv.ParseFloat(s, 64); isString && err == nil {
			return f
		}
	case "integer":
		if f, ok := v.(float64); ok && f == math.Trunc(f) && math.Abs(f) < 1<<53 {
			return int64(f)
		}
		if n, err := strconv.ParseInt(s, 10, 64); isString && err == nil {
			return n
		}
	case "boolean":
		if b, err := strconv.ParseBool(s); isString && err == nil && (strings.EqualFold(s, "true") || strings.EqualFold(s, "false")) {
			return b
		}
	}
	return v
}

// sheetsEpoch is day 0 of Sheets' serial dates.
var sheetsEpoch = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)

// fromSerial returns a Sheets serial date as ISO 8601 text: a date, or a date and time in UTC.
func fromSerial(days float64, withTime bool) string {
	t := sheetsEpoch.Add(time.Duration(math.Round(days*24*60*60)) * time.Second)
	if withTime {
		return t.Format(time.RFC3339)
	}
	return t.Format(time.DateOnly)
}

// normalize returns a value checkValue accepted as the API returns it: a date and time in UTC.
func normalize(typ string, v any) any {
	if s, ok := v.(string); ok && typ == "datetime" && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return v
}

// toStore returns a cell value for the store: a date or datetime column's text as a store.Date, so
// it becomes a date cell.
func toStore(typ string, v any) any {
	s, ok := v.(string)
	if !ok || s == "" {
		return v
	}
	switch typ {
	case "date":
		if t, err := time.Parse(time.DateOnly, s); err == nil {
			return store.Date{Time: t}
		}
	case "datetime":
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return store.Date{Time: t, WithTime: true}
		}
	}
	return v
}

// filterValue returns a filter's value as List compares it, or why it can't be in a column of type
// typ. contains matches text, so it takes any value.
func filterValue(c db.Column, key, op, v string) (string, error) {
	if op == "contains" {
		return v, nil
	}
	switch typ := c.Type; typ {
	case "number", "integer":
		if _, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err != nil {
			return "", BadRequest("%s must be a number, got %q", key, v)
		}
		return strings.TrimSpace(v), nil
	case "boolean":
		if !strings.EqualFold(v, "true") && !strings.EqualFold(v, "false") {
			return "", BadRequest("%s must be true or false, got %q", key, v)
		}
		return strings.ToLower(v), nil
	case "enum":
		if (op == "eq" || op == "ne") && v != "" && !slices.Contains(c.Options, v) {
			return "", BadRequest("%s must be one of %s, got %q", key, strings.Join(c.Options, ", "), v)
		}
	case "date", "datetime":
		if v == "" {
			return v, nil // matches an empty cell
		}
		if err := checkValue(key, db.Column{Type: typ}, v); err != nil {
			return "", BadRequest("%s must be %s, got %q", key, typeNames[c.Type], v)
		}
		return normalize(typ, v).(string), nil
	}
	return v, nil
}

// DefaultValue returns the value a new row gets in column c when it sends none: c.Default as its
// type's JSON value.
func DefaultValue(c db.Column) (any, bool) {
	if c.Default == "" {
		return nil, false
	}
	switch c.Type {
	case "number", "integer":
		return json.Number(c.Default), true
	case "boolean":
		return strings.EqualFold(c.Default, "true"), true
	}
	return c.Default, true
}

// CheckColumn returns why column name's settings don't fit together, or nil: an enum's values, and
// a default that its type, or its values, would refuse.
func CheckColumn(name string, c db.Column) error {
	if c.Type == "enum" && len(c.Options) == 0 {
		return BadRequest("%s: list the values an enum allows", name)
	}
	if c.Default == "" {
		return nil
	}
	if c.Type == "boolean" && !strings.EqualFold(c.Default, "true") && !strings.EqualFold(c.Default, "false") {
		return BadRequest("%s: the default must be true or false", name)
	}
	if c.Type == "number" || c.Type == "integer" {
		if _, err := strconv.ParseFloat(c.Default, 64); err != nil {
			return BadRequest("%s: the default must be %s", name, typeNames[c.Type])
		}
	}
	v, _ := DefaultValue(c)
	if err := checkValue(name, c, v); err != nil {
		return BadRequest("%s: the default doesn't fit: %s", name, err)
	}
	return nil
}
