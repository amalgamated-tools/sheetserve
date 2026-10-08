package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpHandler serves a stateless MCP endpoint whose tools are generated per key:
// one set of tools per tab the key can reach, with column names from the header row.
func (a *API) mcpHandler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return a.mcpServer(r.Context(), a.cfg.keyFor(bearer(r)))
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.keyFor(bearer(r)) == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid API key"})
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ponytail: the server is rebuilt per request (stateless mode), reading each tab's header from
// the cache; with cache_ttl 0 every MCP call re-reads every reachable tab. Cache headers separately if that hurts.
func (a *API) mcpServer(ctx context.Context, key *Key) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "sheetserve", Version: version}, nil)
	for _, sheet := range slices.Sorted(maps.Keys(a.cfg.Spreadsheets)) {
		for _, tab := range slices.Sorted(maps.Keys(a.cfg.Spreadsheets[sheet].Tabs)) {
			ops := slices.DeleteFunc(slices.Clone(allOps), func(op string) bool { return !a.cfg.can(key, sheet, tab, op) })
			if len(ops) == 0 {
				continue
			}
			snap, err := a.svc.load(ctx, sheet, tab, false)
			if err != nil {
				slog.Warn("mcp: skipping tab", "sheet", sheet, "tab", tab, "err", err)
				continue
			}
			cols := slices.DeleteFunc(a.svc.visibleColumns(snap, sheet, tab), func(c string) bool { return c == "id" })
			a.addTabTools(srv, key, sheet, tab, ops, cols)
		}
	}
	return srv
}

type idArgs struct {
	ID string `json:"id"`
}

func (a *API) addTabTools(srv *mcp.Server, key *Key, sheet, tab string, ops, cols []string) {
	name := toolSafe.ReplaceAllString(sheet+"_"+tab, "_")
	where := sheet + "/" + tab
	columns := map[string]any{}
	for _, c := range cols {
		columns[c] = map[string]any{}
	}
	fields := map[string]any{"type": "object", "properties": columns, "additionalProperties": false}
	idSchema := object(map[string]any{"id": map[string]any{"type": "string"}}, "id")

	for _, op := range ops {
		switch op {
		case "read":
			filter := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}}
			for _, c := range cols {
				filter["properties"].(map[string]any)[c] = map[string]any{"type": "string"}
			}
			addTool(srv, key, name+"_list", "List rows in "+where+". filter matches columns exactly.",
				object(map[string]any{"filter": filter, "limit": map[string]any{"type": "integer"}, "offset": map[string]any{"type": "integer"}}),
				func(ctx context.Context, in struct {
					Filter map[string]string `json:"filter"`
					Limit  int               `json:"limit"`
					Offset int               `json:"offset"`
				}) (any, error) {
					return a.svc.List(ctx, sheet, tab, in.Filter, in.Limit, in.Offset)
				})
			addTool(srv, key, name+"_get", "Get one row in "+where+" by id.", idSchema,
				func(ctx context.Context, in idArgs) (any, error) { return a.svc.Get(ctx, sheet, tab, in.ID) })
		case "create":
			addTool(srv, key, name+"_create", "Add a row to "+where+". The id is assigned by the server.", fields,
				func(ctx context.Context, in map[string]any) (any, error) { return a.svc.Create(ctx, sheet, tab, in) })
		case "update":
			addTool(srv, key, name+"_update", "Change columns of one row in "+where+".",
				object(map[string]any{"id": map[string]any{"type": "string"}, "fields": fields}, "id", "fields"),
				func(ctx context.Context, in struct {
					ID     string         `json:"id"`
					Fields map[string]any `json:"fields"`
				}) (any, error) {
					return a.svc.Update(ctx, sheet, tab, in.ID, in.Fields)
				})
		case "delete":
			addTool(srv, key, name+"_delete", "Delete one row in "+where+" by id.", idSchema,
				func(ctx context.Context, in idArgs) (any, error) {
					return map[string]string{"deleted": in.ID}, a.svc.Delete(ctx, sheet, tab, in.ID)
				})
		}
	}
}

var toolSafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func addTool[In any](srv *mcp.Server, key *Key, name, desc string, schema map[string]any, fn func(context.Context, In) (any, error)) {
	srv.AddTool(&mcp.Tool{Name: name, Description: desc, InputSchema: schema},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var in In
			var out any
			err := error(badRequest("invalid arguments"))
			if args := req.Params.Arguments; len(args) == 0 || json.Unmarshal(args, &in) == nil {
				out, err = fn(ctx, in)
			}
			reason := ""
			if err != nil {
				_, out, reason = errorResponse(err)
			}
			slog.Info("mcp call", "tool", name, "key", key.Name, "reason", reason)
			b, _ := json.Marshal(out)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, IsError: err != nil}, nil
		})
}
