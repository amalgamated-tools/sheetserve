package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

type obj = map[string]any

// openAPIHandler serves an OpenAPI 3.1 spec covering only what the caller's key can reach.
func (a *API) openAPIHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	key := a.cfg.keyFor(bearer(r))
	status, keyName, reason := http.StatusOK, "", ""
	if key == nil {
		status, reason = http.StatusUnauthorized, "missing or invalid API key"
		writeJSON(w, status, map[string]string{"error": reason})
	} else {
		keyName = key.Name
		writeJSON(w, status, a.openAPI(r.Context(), key))
	}
	slog.InfoContext(r.Context(), "request", "method", r.Method, "path", r.URL.Path, "op", "openapi", "key", keyName,
		"status", status, "reason", reason, "duration_ms", time.Since(start).Milliseconds())
}

func (a *API) openAPI(ctx context.Context, key *Key) obj {
	schemas := obj{"Error": object(obj{"error": obj{"type": "string"}}, "error")}
	paths := obj{}
	a.eachTab(ctx, key, func(name, sheet, tab string, ops, cols []string) {
		// Rows carry every visible column (id included) with raw cell values, so leave types open.
		row, fields := obj{}, obj{}
		for _, c := range cols {
			row[c] = obj{}
			if c != "id" {
				fields[c] = obj{}
			}
		}
		schemas[name] = object(row, cols...)
		ref := obj{"$ref": "#/components/schemas/" + name}
		input := obj{"type": "object", "properties": fields, "additionalProperties": false}

		base := "/v1/" + url.PathEscape(sheet) + "/" + url.PathEscape(tab)
		coll, item := obj{}, obj{}
		idParam := []any{obj{"name": "id", "in": "path", "required": true, "schema": obj{"type": "string"}}}
		for _, op := range ops {
			switch op {
			case "read":
				params := []any{
					obj{"name": "limit", "in": "query", "schema": obj{"type": "integer", "minimum": 0}},
					obj{"name": "offset", "in": "query", "schema": obj{"type": "integer", "minimum": 0}},
					obj{"name": "sort", "in": "query", "description": "column to sort by; prefix with - for descending",
						"schema": obj{"type": "string", "enum": sortValues(cols)}},
				}
				for _, c := range cols {
					if c == "limit" || c == "offset" || c == "sort" { // API.list reads these only as list options
						continue
					}
					params = append(params, obj{"name": c, "in": "query", "description": "exact match", "schema": obj{"type": "string"}})
					for _, filterOp := range filterOps {
						if slices.Contains(cols, c+"["+filterOp+"]") { // the exact column wins; see Service.List
							continue
						}
						params = append(params, obj{"name": c + "[" + filterOp + "]", "in": "query",
							"description": "filter using " + filterOp, "schema": obj{"type": "string"}})
					}
				}
				list := operation(name+"_list", "List rows in "+sheet+"/"+tab, params, nil, "200", obj{"type": "array", "items": ref})
				list["description"] = "Filter with col=value for an exact match, or col[op]=value where op is one of " +
					strings.Join(filterOps, ", ") + ". gt/gte/lt/lte compare numbers numerically, else as strings, with numbers before strings; contains ignores case."
				coll["get"] = list
				item["get"] = operation(name+"_get", "Get one row by id", idParam, nil, "200", ref)
			case "create":
				coll["post"] = operation(name+"_create", "Add a row; the id is assigned by the server", nil, input, "201", ref)
			case "update":
				item["patch"] = operation(name+"_update", "Change columns of one row", idParam, input, "200", ref)
			case "delete":
				item["delete"] = operation(name+"_delete", "Delete one row by id", idParam, nil, "200",
					object(obj{"deleted": obj{"type": "string"}}, "deleted"))
			}
		}
		if len(coll) > 0 {
			paths[base] = coll
		}
		if len(item) > 0 {
			paths[base+"/{id}"] = item
		}
	})
	return obj{
		"openapi":    "3.1.0",
		"info":       obj{"title": "sheetserve", "version": version},
		"servers":    []any{obj{"url": "/"}},
		"security":   []any{obj{"bearerAuth": []any{}}},
		"paths":      paths,
		"components": obj{"schemas": schemas, "securitySchemes": obj{"bearerAuth": obj{"type": "http", "scheme": "bearer"}}},
	}
}

func sortValues(cols []string) []any {
	var out []any
	for _, c := range cols {
		out = append(out, c)
		if !slices.Contains(cols, "-"+c) { // a column named -c wins; see Service.List
			out = append(out, "-"+c)
		}
	}
	return out
}

func operation(id, summary string, params []any, body obj, status string, result obj) obj {
	errResp := obj{"description": "error", "content": obj{"application/json": obj{"schema": obj{"$ref": "#/components/schemas/Error"}}}}
	op := obj{
		"operationId": id,
		"summary":     summary,
		"responses": obj{
			status: obj{"description": "ok", "content": obj{"application/json": obj{"schema": result}}},
			"4XX":  errResp,
			"5XX":  errResp,
		},
	}
	if len(params) > 0 {
		op["parameters"] = params
	}
	if body != nil {
		op["requestBody"] = obj{"required": true, "content": obj{"application/json": obj{"schema": body}}}
	}
	return op
}
