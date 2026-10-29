package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/service"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpHandler serves a stateless MCP endpoint whose tools are generated per key:
// one set of tools per tab the key can reach, with column names from the header row.
func (a *API) mcpHandler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return a.mcpServer(r.Context(), a.settings.KeyFor(bearer(r)))
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.settings.KeyFor(bearer(r)) == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid API key"})
			return
		}
		h.ServeHTTP(w, r)
	})
}

// TODO: the server is rebuilt per request (stateless mode), reading each tab's header from
// the cache; with cache_ttl 0 every MCP call re-reads every reachable tab. Cache headers separately if that hurts.
func (a *API) mcpServer(ctx context.Context, key *db.Key) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "sheetserve", Version: Version}, nil)
	a.eachTab(ctx, key, func(name, sheet, tab, rowKey string, ops, cols []string) {
		a.addTabTools(srv, key, name, sheet, tab, rowKey, ops, cols)
	})
	return srv
}

// eachTab calls fn, in sorted order, for every tab key can reach, with a name unique among
// those tabs, its key column ("" when it has none), the ops it may perform and its distinct
// visible columns (including the key). A tab without a key column offers only read, which then
// means listing. Tabs whose header can't be read are skipped.
func (a *API) eachTab(ctx context.Context, key *db.Key, fn func(name, sheet, tab, rowKey string, ops, cols []string)) {
	used := map[string]bool{}
	for _, sheet := range slices.Sorted(maps.Keys(a.settings.Spreadsheets)) {
		for _, tab := range slices.Sorted(maps.Keys(a.settings.Spreadsheets[sheet].Tabs)) {
			ops := slices.DeleteFunc(slices.Clone(db.AllOps), func(op string) bool { return !a.settings.Can(key, sheet, tab, op) })
			if len(ops) == 0 {
				continue
			}
			// "shop/A B" and "shop/A_B" both sanitize to shop_A_B; suffix later ones. Names are
			// claimed before loading so a tab that fails to load can't hand its name to another.
			base := toolSafe.ReplaceAllString(sheet+"_"+tab, "_")
			name := base
			for i := 2; used[name]; i++ {
				name = base + "_" + strconv.Itoa(i)
			}
			used[name] = true
			columns, err := a.svc.VisibleColumns(ctx, sheet, tab)
			var rowKey string
			if err == nil {
				rowKey, err = a.svc.Key(ctx, sheet, tab)
			}
			if err != nil {
				slog.WarnContext(ctx, "skipping tab", "sheet", sheet, "tab", tab, "err", err)
				continue
			}
			if rowKey == "" {
				ops = slices.DeleteFunc(ops, func(op string) bool { return op != "read" })
				if len(ops) == 0 {
					continue
				}
			}
			var cols []string
			for _, c := range columns {
				if !slices.Contains(cols, c) {
					cols = append(cols, c)
				}
			}
			fn(name, sheet, tab, rowKey, ops, cols)
		}
	}
}

type idArgs struct {
	ID string `json:"id"`
}

// writeFields returns the input schemas for creating and updating a row of a tab keyed by rowKey,
// with the column types from columnTypes. The server assigns ids, so an "id" key is never sent;
// any other key is required on create and can't be changed by an update.
func writeFields(rowKey string, cols []string, types map[string]db.Column) (create, update map[string]any) {
	createCols, updateCols := map[string]any{}, map[string]any{}
	var required []string
	for _, c := range cols {
		if c != rowKey || rowKey != "id" {
			createCols[c] = valueSchema(types[c])
			if types[c].Required && (types[c].Default == "" || c == rowKey) {
				required = append(required, c) // a default fills a required column the client leaves out
			}
		}
		if c != rowKey {
			updateCols[c] = valueSchema(types[c])
		}
	}
	create, update = object(createCols, required...), object(updateCols)
	create["additionalProperties"], update["additionalProperties"] = false, false
	return create, update
}

// columnTypes returns the types of sheet/tab's columns by name, with its key column rowKey marked
// required, since every row has one.
func (a *API) columnTypes(sheet, tab, rowKey string) map[string]db.Column {
	types := maps.Clone(a.settings.Spreadsheets[sheet].Tabs[tab].Columns)
	if types == nil {
		types = map[string]db.Column{}
	}
	if rowKey != "" {
		k := types[rowKey]
		k.Required = true
		types[rowKey] = k
	}
	return types
}

// valueSchema is the JSON schema of a value in a column of type c, with its default. A typed column
// that isn't required can be empty, which reads as null. An untyped column can hold anything.
func valueSchema(c db.Column) map[string]any {
	s := map[string]any{}
	if d, ok := service.DefaultValue(c); ok {
		s["default"] = d
	}
	typ, format := c.Type, ""
	switch c.Type {
	case "":
		return s
	case "date":
		typ, format = "string", "date"
	case "datetime":
		typ, format = "string", "date-time"
	case "enum":
		typ = "string"
		values := []any{}
		for _, o := range c.Options {
			values = append(values, o)
		}
		if !c.Required {
			values = append(values, nil)
		}
		s["enum"] = values
	}
	s["type"] = typ
	if !c.Required {
		s["type"] = []any{typ, "null"}
	}
	if format != "" {
		s["format"] = format
	}
	return s
}

// createNote tells a client who sets a new row's key.
func createNote(rowKey string) string {
	if rowKey == "id" {
		return "The id is assigned by the server."
	}
	return rowKey + " is required and must be unique."
}

func (a *API) addTabTools(srv *mcp.Server, key *db.Key, name, sheet, tab, rowKey string, ops, cols []string) {
	where := sheet + "/" + tab
	createFields, updateFields := writeFields(rowKey, cols, a.columnTypes(sheet, tab, rowKey))
	idSchema := object(map[string]any{"id": map[string]any{"type": "string", "description": "the row's " + rowKey}}, "id")

	for _, op := range ops {
		switch op {
		case "read":
			filter := map[string]any{
				"type": "object", "properties": map[string]any{},
				"additionalProperties": map[string]any{"type": "string"},
			}
			for _, c := range cols {
				filter["properties"].(map[string]any)[c] = map[string]any{"type": "string"}
			}
			addTool(srv, key, name+"_list", "List rows in "+where+". A filter key \"col\" matches exactly; \"col[op]\" uses op, one of "+
				strings.Join(service.FilterOperators(), ", ")+". gt/gte/lt/lte compare numbers numerically, else as strings, with numbers before strings; contains ignores case. "+
				"sort is a column name, prefixed with - for descending.",
				object(map[string]any{
					"filter": filter, "sort": map[string]any{"type": "string"},
					"limit": map[string]any{"type": "integer"}, "offset": map[string]any{"type": "integer"},
				}),
				func(ctx context.Context, in struct {
					Filter map[string]string `json:"filter"`
					Sort   string            `json:"sort"`
					Limit  int               `json:"limit"`
					Offset int               `json:"offset"`
				},
				) (any, error) {
					rows, _, err := a.svc.List(ctx, sheet, tab, in.Filter, in.Sort, in.Limit, in.Offset)
					return rows, err
				})
			if rowKey != "" {
				addTool(srv, key, name+"_get", "Get one row in "+where+" by its "+rowKey+".", idSchema,
					func(ctx context.Context, in idArgs) (any, error) { return a.svc.Get(ctx, sheet, tab, in.ID) })
			}
		case "create":
			addTool(srv, key, name+"_create", "Add a row to "+where+". "+createNote(rowKey), createFields,
				func(ctx context.Context, in map[string]any) (any, error) { return a.svc.Create(ctx, sheet, tab, in) })
		case "update":
			addTool(srv, key, name+"_update", "Change columns of one row in "+where+", found by its "+rowKey+".",
				object(map[string]any{"id": idSchema["properties"].(map[string]any)["id"], "fields": updateFields}, "id", "fields"),
				func(ctx context.Context, in struct {
					ID     string         `json:"id"`
					Fields map[string]any `json:"fields"`
				},
				) (any, error) {
					return a.svc.Update(ctx, sheet, tab, in.ID, in.Fields)
				})
		case "delete":
			addTool(srv, key, name+"_delete", "Delete one row in "+where+" by its "+rowKey+".", idSchema,
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

func addTool[In any](srv *mcp.Server, key *db.Key, name, desc string, schema map[string]any, fn func(context.Context, In) (any, error)) {
	srv.AddTool(&mcp.Tool{Name: name, Description: desc, InputSchema: schema},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var in In
			var out any
			err := error(service.BadRequest("invalid arguments"))
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
