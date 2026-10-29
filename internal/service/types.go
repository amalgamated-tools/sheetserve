package service

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/amalgamated-tools/sheetserve/internal/db"
)

// typeNames say what a value of each column type looks like, for error messages.
var typeNames = map[string]string{
	"string":   "a string",
	"number":   "a number",
	"integer":  "an integer",
	"boolean":  "true or false",
	"date":     "a date like 2026-01-31",
	"datetime": "a date and time like 2026-01-31T09:30:00Z",
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
	case "string":
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
