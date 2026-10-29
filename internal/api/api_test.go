package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/api/googleapi"

	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/service"
	"github.com/amalgamated-tools/sheetserve/internal/store"
	"github.com/amalgamated-tools/sheetserve/internal/testutil/oteltest"
)

// fakeStore keeps tabs in memory, keyed by "spreadsheetID/tab".
type fakeStore struct{ tabs map[string][][]any }

func (f *fakeStore) Read(_ context.Context, id, tab string) ([][]any, error) {
	return f.tabs[id+"/"+tab], nil
}

func (f *fakeStore) Append(_ context.Context, id, tab string, row []any) error {
	f.tabs[id+"/"+tab] = append(f.tabs[id+"/"+tab], row)
	return nil
}

func (f *fakeStore) SetCells(_ context.Context, id, tab string, row int, cells map[int]any) error {
	rows := f.tabs[id+"/"+tab]
	if row < 1 || row > len(rows) {
		return fmt.Errorf("row %d out of range", row)
	}
	for c, v := range cells {
		rows[row-1][c] = v
	}
	return nil
}

func (f *fakeStore) DeleteRow(_ context.Context, id, tab string, row int) error {
	k := id + "/" + tab
	f.tabs[k] = slices.Delete(f.tabs[k], row-1, row)
	return nil
}

func (f *fakeStore) Tabs(_ context.Context, id string) ([]string, error) {
	var out []string
	for k := range f.tabs {
		if tab, ok := strings.CutPrefix(k, id+"/"); ok {
			out = append(out, tab)
		}
	}
	slices.Sort(out)
	return out, nil
}

func newTestAPI(t *testing.T) http.Handler {
	t.Helper()
	cfg := &db.Settings{
		Spreadsheets: map[string]db.SheetRule{"shop": {ID: "SS", Tabs: map[string]db.TabRule{
			"Products": {Ops: []string{"read"}, Hidden: []string{"cost"}},
			"Orders":   {Ops: db.AllOps},
		}}},
		Keys: []db.Key{
			{Name: "admin", Hash: db.HashKey("admin-key"), Tabs: []string{"*"}},
			{Name: "web", Hash: db.HashKey("web-key"), Tabs: []string{"shop/*"}, Ops: []string{"read"}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{tabs: map[string][][]any{
		"SS/Products": {{"id", "name", "cost"}, {"p1", "mug", 3.5}},
		"SS/Orders":   {{"id", "item", "qty"}},
	}}
	return New(cfg, service.New(cfg, store, time.Minute)).Mux() // the cache TTL exercises invalidation on writes
}

func call(t *testing.T, h http.Handler, method, path, key, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func TestAPI(t *testing.T) {
	h := newTestAPI(t)
	steps := []struct {
		name, method, path, key, body string
		status                        int
		want                          string // substring of the response body
	}{
		{"no key", "GET", "/v1/shop/Products", "", "", 401, "invalid API key"},
		{"bad key", "GET", "/v1/shop/Products", "nope", "", 401, "invalid API key"},
		{"unknown tab", "GET", "/v1/shop/Nope", "admin-key", "", 404, "unknown tab"},
		{"hidden column stripped", "GET", "/v1/shop/Products", "web-key", "", 200, `[{"id":"p1","name":"mug"}]`},
		{"filter on hidden column refused", "GET", "/v1/shop/Products?cost=3.5", "web-key", "", 400, "unknown column"},
		{"filter", "GET", "/v1/shop/Products?name=tea", "web-key", "", 200, `[]`},
		{"tab is read-only", "POST", "/v1/shop/Products", "admin-key", `{"name":"x"}`, 403, "create not allowed"},
		{"key is read-only", "POST", "/v1/shop/Orders", "web-key", `{"item":"x"}`, 403, "create not allowed"},
		{"unknown field", "POST", "/v1/shop/Orders", "admin-key", `{"nope":1}`, 400, "unknown column"},
		{"client cannot set id", "POST", "/v1/shop/Orders", "admin-key", `{"id":"x"}`, 400, "assigned by the server"},
		{"bad body", "POST", "/v1/shop/Orders", "admin-key", `[1]`, 400, "JSON object"},
		{"warm cache", "GET", "/v1/shop/Orders", "web-key", "", 200, `[]`},
		{"create", "POST", "/v1/shop/Orders", "admin-key", `{"item":"mug","qty":2}`, 201, `"item":"mug"`},
		{"create invalidates cache", "GET", "/v1/shop/Orders?item=mug", "web-key", "", 200, `"qty":2`},
	}
	for _, s := range steps {
		status, body := call(t, h, s.method, s.path, s.key, s.body)
		if status != s.status || !strings.Contains(body, s.want) {
			t.Fatalf("%s: got %d %s, want %d containing %s", s.name, status, body, s.status, s.want)
		}
	}

	_, body := call(t, h, "GET", "/v1/shop/Orders", "admin-key", "")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(body), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("list orders: %s", body)
	}
	id := rows[0]["id"].(string)

	for _, s := range []struct {
		method, path, body string
		status             int
		want               string
	}{
		{"PATCH", "/v1/shop/Orders/" + id, `{"qty":5}`, 200, `"qty":5`},
		{"GET", "/v1/shop/Orders/" + id, "", 200, `"qty":5`},
		{"PATCH", "/v1/shop/Orders/missing", `{"qty":1}`, 404, "row not found"},
		{"DELETE", "/v1/shop/Orders/" + id, "", 200, `"deleted"`},
		{"GET", "/v1/shop/Orders/" + id, "", 404, "row not found"},
	} {
		status, body := call(t, h, s.method, s.path, "admin-key", s.body)
		if status != s.status || !strings.Contains(body, s.want) {
			t.Fatalf("%s %s: got %d %s, want %d containing %s", s.method, s.path, status, body, s.status, s.want)
		}
	}
}

func TestMCP(t *testing.T) {
	h := newTestAPI(t)
	if status, _ := call(t, h, "POST", "/mcp", "", `{}`); status != 401 {
		t.Fatalf("no key: got %d, want 401", status)
	}

	_, body := call(t, h, "POST", "/mcp", "web-key", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var list struct {
		Result struct {
			Tools []struct{ Name string } `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("tools/list: %v: %s", err, body)
	}
	var names []string
	for _, tool := range list.Result.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	want := []string{"shop_Orders_get", "shop_Orders_list", "shop_Products_get", "shop_Products_list"}
	if !slices.Equal(names, want) {
		t.Fatalf("read-only key tools: got %v, want %v", names, want)
	}
	if strings.Contains(body, "cost") {
		t.Fatalf("hidden column leaked into tool schema: %s", body)
	}

	for _, tc := range []struct {
		arguments string
		want      string
	}{
		{`{"offset":-1}`, "invalid offset"},
		{`{"limit":-1}`, "invalid limit"},
	} {
		_, body = call(t, h, "POST", "/mcp", "web-key",
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"shop_Orders_list","arguments":`+tc.arguments+`}}`)
		if !strings.Contains(body, `"isError":true`) || !strings.Contains(body, tc.want) {
			t.Fatalf("negative pagination %s: expected MCP error containing %q, got %s", tc.arguments, tc.want, body)
		}
	}

	_, body = call(t, h, "POST", "/mcp", "admin-key",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"shop_Orders_create","arguments":{"item":"tea"}}}`)
	if !strings.Contains(body, `\"item\":\"tea\"`) || strings.Contains(body, `"isError":true`) {
		t.Fatalf("create via mcp: %s", body)
	}
}

func TestOpenAPI(t *testing.T) {
	h := newTestAPI(t)
	if status, _ := call(t, h, "GET", "/openapi.json", "", ""); status != 401 {
		t.Fatalf("no key: got %d, want 401", status)
	}
	status, body := call(t, h, "GET", "/openapi.json", "web-key", "")
	var spec struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal([]byte(body), &spec); status != 200 || err != nil || spec.OpenAPI != "3.1.0" {
		t.Fatalf("spec: %d %v %s", status, err, body)
	}
	for path, want := range map[string][]string{
		"/v1/shop/Products":      {"get"},
		"/v1/shop/Products/{id}": {"get"},
		"/v1/shop/Orders":        {"get"}, // tab allows create, but the key is read-only
		"/v1/shop/Orders/{id}":   {"get"},
	} {
		got := slices.Sorted(maps.Keys(spec.Paths[path]))
		if !slices.Equal(got, want) {
			t.Errorf("%s: methods %v, want %v", path, got, want)
		}
	}
	if strings.Contains(body, "cost") {
		t.Fatalf("hidden column leaked into spec: %s", body)
	}

	_, body = call(t, h, "GET", "/openapi.json", "admin-key", "")
	for _, want := range []string{`"post"`, `"patch"`, `"delete"`, `"qty"`} {
		if !strings.Contains(body, want) {
			t.Errorf("admin spec missing %s", want)
		}
	}
}

// Tabs whose names sanitize alike, a hidden id, and columns that clash with pagination
// or repeat must still produce unique operations and parameters.
func TestOpenAPIEdgeCases(t *testing.T) {
	cfg := &db.Settings{
		Spreadsheets: map[string]db.SheetRule{"shop": {ID: "SS", Tabs: map[string]db.TabRule{
			"A B": {Ops: []string{"read"}},
			"A_B": {Ops: []string{"read"}, Hidden: []string{"id"}},
		}}},
		Keys: []db.Key{{Name: "admin", Hash: db.HashKey("admin-key"), Tabs: []string{"*"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{tabs: map[string][][]any{
		"SS/A B": {{"id", "limit", "name", "name", "name[eq]"}},
		"SS/A_B": {{"id", "name"}},
	}}
	_, body := call(t, New(cfg, service.New(cfg, store, 0)).Mux(), "GET", "/openapi.json", "admin-key", "")
	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Parameters  []struct {
				Name string `json:"name"`
			} `json:"parameters"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `json:"properties"`
				Required   []string       `json:"required"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal([]byte(body), &spec); err != nil {
		t.Fatal(err)
	}
	list1, list2 := spec.Paths["/v1/shop/A%20B"]["get"], spec.Paths["/v1/shop/A_B"]["get"]
	if list1.OperationID != "shop_A_B_list" || list2.OperationID != "shop_A_B_2_list" {
		t.Fatalf("operation ids: %q %q", list1.OperationID, list2.OperationID)
	}
	var names []string
	for _, p := range list1.Parameters {
		names = append(names, p.Name)
	}
	want := []string{"limit", "offset", "sort"}
	for _, col := range []string{"id", "name", "name[eq]"} {
		want = append(want, col)
		for _, op := range service.FilterOperators() {
			if col+"["+op+"]" != "name[eq]" { // listed once, as its own column
				want = append(want, col+"["+op+"]")
			}
		}
	}
	if !slices.Equal(names, want) {
		t.Errorf("params %v, want %v", names, want)
	}
	if s := spec.Components.Schemas["shop_A_B"]; !slices.Contains(s.Required, "id") || s.Properties["id"] == nil {
		t.Errorf("visible id not in schema: %+v", s)
	}
	if s := spec.Components.Schemas["shop_A_B_2"]; slices.Contains(s.Required, "id") || s.Properties["id"] != nil {
		t.Errorf("hidden id advertised: %+v", s)
	}
}

// tabFailStore fails every read of one tab.
type tabFailStore struct {
	*fakeStore
	tab string
}

func (f tabFailStore) Read(ctx context.Context, id, tab string) ([][]any, error) {
	if tab == f.tab {
		return nil, fmt.Errorf("read %s: unavailable", tab)
	}
	return f.fakeStore.Read(ctx, id, tab)
}

// A tab that fails to load keeps its name reserved, so the next tab can't take over its tools.
func TestFailedTabKeepsName(t *testing.T) {
	cfg := &db.Settings{
		Spreadsheets: map[string]db.SheetRule{"shop": {ID: "SS", Tabs: map[string]db.TabRule{
			"A B": {Ops: []string{"read"}},
			"A_B": {Ops: []string{"read"}},
		}}},
		Keys: []db.Key{{Name: "admin", Hash: db.HashKey("admin-key"), Tabs: []string{"*"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	store := tabFailStore{&fakeStore{tabs: map[string][][]any{"SS/A_B": {{"id", "name"}}}}, "A B"}
	a := New(cfg, service.New(cfg, store, 0))
	var names []string
	a.eachTab(context.Background(), &cfg.Keys[0], func(name, _, _, _ string, _, _ []string) { names = append(names, name) })
	if !slices.Equal(names, []string{"shop_A_B_2"}) {
		t.Fatalf("names %v, want [shop_A_B_2]", names)
	}
}

func TestListSortAndFilterOps(t *testing.T) {
	cfg := &db.Settings{
		Spreadsheets: map[string]db.SheetRule{"shop": {ID: "SS", Tabs: map[string]db.TabRule{
			"Orders": {Ops: []string{"read"}, Hidden: []string{"cost"}},
		}}},
		Keys: []db.Key{{Name: "admin", Hash: db.HashKey("admin-key"), Tabs: []string{"*"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{tabs: map[string][][]any{"SS/Orders": {
		{"id", "item", "qty", "cost", "a[b]", "-n"},
		{"o1", "Green Tea", "10", "1", "x", "3"},
		{"o2", "mug", "9", "2", "y", "1"},
		{"o3", "tea pot", "2", "3", "z", "2"},
	}}}
	h := New(cfg, service.New(cfg, store, 0)).Mux()
	ids := func(body string) string {
		var rows []map[string]any
		if err := json.Unmarshal([]byte(body), &rows); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r["id"].(string))
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		query  string
		status int
		want   string // ids, or a substring of the error
	}{
		{"sort=qty", 200, "o3,o2,o1"}, // numeric, not "10" < "2"
		{"sort=-qty", 200, "o1,o2,o3"},
		{"sort=item", 200, "o1,o2,o3"},
		{"qty[gt]=2&sort=qty", 200, "o2,o1"},
		{"qty[lte]=9&qty[ne]=2", 200, "o2"},
		{"item[contains]=TEA&sort=-qty&limit=1&offset=1", 200, "o3"},
		{"a[b]=y", 200, "o2"}, // a column whose name looks like an operator still matches exactly
		{"a[b][ne]=x", 200, "o2,o3"},
		{"sort=-n", 200, "o2,o3,o1"},  // the column named -n, ascending
		{"sort=--n", 200, "o1,o3,o2"}, // ...and descending
		{"item[contains]=e&limit=1&offset=1", 200, "o3"},
		{"qty[like]=1", 400, "unknown operator"},
		{"cost[gt]=0", 400, "unknown column"},
		{"sort=-cost", 400, "unknown column"},
	} {
		status, body := call(t, h, "GET", "/v1/shop/Orders?"+c.query, "admin-key", "")
		got := body
		if status == 200 {
			got = ids(body)
		}
		if status != c.status || (status == 200 && got != c.want) || !strings.Contains(got, c.want) {
			t.Errorf("%s: got %d %s, want %d %s", c.query, status, got, c.status, c.want)
		}
	}
}

func TestCORS(t *testing.T) {
	h := CORS([]string{"https://app.example"}, New(&db.Settings{}, service.New(&db.Settings{}, &fakeStore{}, 0)).Mux())
	req := func(method, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/v1/shop/Orders", nil)
		r.Header.Set("Origin", origin)
		r.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	if rec := req("OPTIONS", "https://app.example"); rec.Code != 204 ||
		rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example" ||
		!strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Errorf("preflight: %d %v", rec.Code, rec.Header())
	}
	if rec := req("GET", "https://app.example"); rec.Code != 401 || rec.Header().Get("Access-Control-Allow-Origin") == "" ||
		rec.Header().Get("Access-Control-Expose-Headers") != "X-Total-Count" {
		t.Errorf("actual request should carry CORS headers: %d %v", rec.Code, rec.Header())
	}
	if rec := req("OPTIONS", "https://evil.example"); rec.Code == 204 || rec.Header().Get("Access-Control-Allow-Origin") != "" ||
		rec.Header().Get("Vary") != "Origin" {
		t.Errorf("unlisted origin allowed: %d %v", rec.Code, rec.Header())
	}
}

// failStore fails every read with err.
type failStore struct {
	fakeStore
	err error
}

func (f *failStore) Read(context.Context, string, string) ([][]any, error) { return nil, f.err }

// Google's rate limit reaches callers as 429 with Retry-After; other Google errors stay 502.
func TestGoogleRateLimit(t *testing.T) {
	cfg := &db.Settings{
		Spreadsheets: map[string]db.SheetRule{"shop": {ID: "SS", Tabs: map[string]db.TabRule{"Orders": {Ops: []string{"read"}}}}},
		Keys:         []db.Key{{Name: "admin", Hash: db.HashKey("admin-key"), Tabs: []string{"*"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for code, want := range map[int]int{429: 429, 404: 502} {
		store := &failStore{err: fmt.Errorf("read: %w", &googleapi.Error{Code: code, Message: "nope"})}
		h := New(cfg, service.New(cfg, store, 0)).Mux()
		req := httptest.NewRequest("GET", "/v1/shop/Orders", nil)
		req.Header.Set("Authorization", "Bearer admin-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if retry := rec.Header().Get("Retry-After"); rec.Code != want || (want == 429) != (retry == "60") {
			t.Errorf("Google %d: got %d, Retry-After %q: %s", code, rec.Code, retry, rec.Body)
		}
	}
}

// A tab can name its key column. Without one it can only be listed, and its tools and spec say so;
// a chosen key is required on create, unique, and fixed; a key that matches two rows is refused.
func TestKeyColumns(t *testing.T) {
	cfg := &db.Settings{
		Spreadsheets: map[string]db.SheetRule{"shop": {ID: "SS", Tabs: map[string]db.TabRule{
			"Log":    {Ops: db.AllOps},
			"People": {Ops: db.AllOps, Key: "email"},
			"Dupes":  {Ops: []string{"read"}},
		}}},
		Keys: []db.Key{{Name: "admin", Hash: db.HashKey("admin-key"), Tabs: []string{"*"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{tabs: map[string][][]any{
		"SS/Log":    {{"at", "msg"}, {"1", "boot"}},
		"SS/People": {{"email", "name"}},
		"SS/Dupes":  {{"id", "v"}, {"x", 1}, {"x", 2}},
	}}
	h := New(cfg, service.New(cfg, store, 0)).Mux()

	for _, s := range []struct {
		name, method, path, body string
		status                   int
		want                     string
	}{
		{"keyless list", "GET", "/v1/shop/Log", "", 200, `"msg":"boot"`},
		{"keyless get", "GET", "/v1/shop/Log/1", "", 403, `has no \"id\" column, so it can only be listed`},
		{"keyless create", "POST", "/v1/shop/Log", `{"msg":"x"}`, 403, "can only be listed"},
		{"keyless update", "PATCH", "/v1/shop/Log/1", `{"msg":"x"}`, 403, "can only be listed"},
		{"keyless delete", "DELETE", "/v1/shop/Log/1", "", 403, "can only be listed"},
		{"key required", "POST", "/v1/shop/People", `{"name":"Ann"}`, 400, "email is the tab's key and is required"},
		{"create with key", "POST", "/v1/shop/People", `{"email":"ann@example.com","name":"Ann"}`, 201, `"email":"ann@example.com"`},
		{"key unique", "POST", "/v1/shop/People", `{"email":"ann@example.com"}`, 409, "already exists"},
		{"get by key", "GET", "/v1/shop/People/ann@example.com", "", 200, `"name":"Ann"`},
		{"key fixed", "PATCH", "/v1/shop/People/ann@example.com", `{"email":"b@example.com"}`, 400, "can't be changed"},
		{"update by key", "PATCH", "/v1/shop/People/ann@example.com", `{"name":"Anne"}`, 200, `"name":"Anne"`},
		{"delete by key", "DELETE", "/v1/shop/People/ann@example.com", "", 200, `"deleted"`},
		{"duplicate key", "GET", "/v1/shop/Dupes/x", "", 409, "more than one row"},
	} {
		status, body := call(t, h, s.method, s.path, "admin-key", s.body)
		if status != s.status || !strings.Contains(body, s.want) {
			t.Errorf("%s: got %d %s, want %d containing %s", s.name, status, body, s.status, s.want)
		}
	}

	_, body := call(t, h, "POST", "/mcp", "admin-key", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var list struct {
		Result struct {
			Tools []struct {
				Name        string
				InputSchema struct{ Required []string } `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("tools/list: %v: %s", err, body)
	}
	var names []string
	for _, tool := range list.Result.Tools {
		if strings.HasPrefix(tool.Name, "shop_Log") {
			names = append(names, tool.Name)
		}
		if tool.Name == "shop_People_create" && !slices.Equal(tool.InputSchema.Required, []string{"email"}) {
			t.Errorf("People create requires %v, want [email]", tool.InputSchema.Required)
		}
	}
	if !slices.Equal(names, []string{"shop_Log_list"}) {
		t.Errorf("keyless tools: %v, want [shop_Log_list]", names)
	}

	_, body = call(t, h, "GET", "/openapi.json", "admin-key", "")
	var spec struct{ Paths map[string]map[string]any }
	if err := json.Unmarshal([]byte(body), &spec); err != nil {
		t.Fatal(err)
	}
	if got := slices.Collect(maps.Keys(spec.Paths["/v1/shop/Log"])); !slices.Equal(got, []string{"get"}) {
		t.Errorf("keyless collection ops: %v, want [get]", got)
	}
	if _, ok := spec.Paths["/v1/shop/Log/{id}"]; ok {
		t.Error("keyless tab has an item path")
	}
}

// Tab names reach handlers decoded, whether or not the request path needed escaping.
func TestEscapedTabNames(t *testing.T) {
	cfg := &db.Settings{
		Spreadsheets: map[string]db.SheetRule{"shop": {ID: "SS", Tabs: map[string]db.TabRule{
			"Q1/Q2": {Ops: []string{"read"}}, "100%": {Ops: []string{"read"}}, "A B": {Ops: []string{"read"}},
		}}},
		Keys: []db.Key{{Name: "admin", Hash: db.HashKey("admin-key"), Tabs: []string{"*"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{tabs: map[string][][]any{
		"SS/Q1/Q2": {{"id"}, {"r1"}}, "SS/100%": {{"id"}, {"r2"}}, "SS/A B": {{"id"}, {"r3"}},
	}}
	h := New(cfg, service.New(cfg, store, 0)).Mux()
	for path, want := range map[string]string{
		"/v1/shop/Q1%2FQ2": "r1", "/v1/shop/100%25": "r2", "/v1/shop/A%20B": "r3", "/v1/shop/A%20B/r3": "r3",
	} {
		if status, body := call(t, h, "GET", path, "admin-key", ""); status != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("GET %s: %d %s", path, status, body)
		}
	}
}

// Routes mounted without middleware that supplies an API fail closed instead of panicking.
func TestRoutesWithoutAPI(t *testing.T) {
	r := chi.NewRouter()
	Routes(r)
	if status, body := call(t, r, "GET", "/v1/shop/Orders", "admin-key", ""); status != http.StatusInternalServerError {
		t.Errorf("got %d %s", status, body)
	}
}

// With a cache TTL, the first read misses, the next hits, and a write drops the entry so the
// read after it misses again.
func TestCacheLogs(t *testing.T) {
	logs := oteltest.CaptureLogs(t)
	h := newTestAPI(t)
	call(t, h, "GET", "/v1/shop/Orders", "admin-key", "")
	call(t, h, "GET", "/v1/shop/Orders", "admin-key", "")
	if s, body := call(t, h, "POST", "/v1/shop/Orders", "admin-key", `{"item":"mug"}`); s != 201 {
		t.Fatalf("create: %d %s", s, body)
	}
	call(t, h, "GET", "/v1/shop/Orders", "admin-key", "")

	if got := len(logs("cache miss")); got != 2 {
		t.Errorf("cache misses: %d, want 2", got)
	}
	if hits := logs("cache hit"); len(hits) != 1 || hits[0]["sheet"] != "shop" || hits[0]["tab"] != "Orders" || hits[0]["age_ms"] == nil {
		t.Errorf("cache hits: %v", hits)
	}
	if got := len(logs("cache invalidate")); got != 1 {
		t.Errorf("cache invalidations: %d, want 1", got)
	}
}

// Column types shape the MCP tool schemas, and MCP arguments, whose numbers decode as float64, are
// checked against them like REST bodies.
func TestTypedColumnsMCP(t *testing.T) {
	cfg := &db.Settings{
		Spreadsheets: map[string]db.SheetRule{"shop": {ID: "SS", Tabs: map[string]db.TabRule{
			"Orders": {Ops: db.AllOps, Columns: map[string]db.Column{
				"id": {Type: "string"}, "item": {Type: "string", Required: true}, "qty": {Type: "integer"}, "due": {Type: "date"},
			}},
		}}},
		Keys: []db.Key{{Name: "admin", Hash: db.HashKey("admin-key"), Tabs: []string{"*"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{tabs: map[string][][]any{"SS/Orders": {{"id", "item", "qty", "due", "note"}}}}
	h := New(cfg, service.New(cfg, store, 0)).Mux()

	_, body := call(t, h, "POST", "/mcp", "admin-key", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var list struct {
		Result struct {
			Tools []struct {
				Name        string
				InputSchema struct {
					Properties map[string]any `json:"properties"`
					Required   []string       `json:"required"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("tools/list: %v: %s", err, body)
	}
	var create *struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	for i, tool := range list.Result.Tools {
		if tool.Name == "shop_Orders_create" {
			create = &list.Result.Tools[i].InputSchema
		}
	}
	if create == nil {
		t.Fatalf("no create tool: %s", body)
	}
	got, _ := json.Marshal(create.Properties)
	if want := `{"due":{"format":"date","type":["string","null"]},"item":{"type":"string"},"note":{},"qty":{"type":["integer","null"]}}`; string(got) != want {
		t.Errorf("create properties: %s, want %s", got, want)
	}
	if !slices.Equal(create.Required, []string{"item"}) {
		t.Errorf("create requires %v, want [item]", create.Required)
	}

	for args, want := range map[string]string{
		`{"item":"tea","qty":2.5}`: `qty must be an integer, got 2.5`,
		`{"qty":2}`:                `item is required`,
	} {
		_, body = call(t, h, "POST", "/mcp", "admin-key",
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"shop_Orders_create","arguments":`+args+`}}`)
		if !strings.Contains(body, `"isError":true`) || !strings.Contains(body, want) {
			t.Errorf("create %s: %s, want %s", args, body, want)
		}
	}
	_, body = call(t, h, "POST", "/mcp", "admin-key",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"shop_Orders_create","arguments":{"item":"tea","qty":2,"due":"2026-02-01"}}}`)
	if strings.Contains(body, `"isError":true`) || !strings.Contains(body, `\"qty\":2`) {
		t.Errorf("create via mcp: %s", body)
	}
}

// An enum's values and a column's default go into the schemas, and a required column with a default
// isn't required on create.
func TestEnumAndDefaultSchemas(t *testing.T) {
	cfg := &db.Settings{
		Spreadsheets: map[string]db.SheetRule{"shop": {ID: "SS", Tabs: map[string]db.TabRule{
			"Orders": {Ops: db.AllOps, Columns: map[string]db.Column{
				"status": {Type: "enum", Options: []string{"new", "paid"}, Required: true, Default: "new"},
				"size":   {Type: "enum", Options: []string{"S", "M"}},
				"qty":    {Type: "integer", Required: true},
			}},
		}}},
		Keys: []db.Key{{Name: "admin", Hash: db.HashKey("admin-key"), Tabs: []string{"*"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	h := New(cfg, service.New(cfg, &fakeStore{tabs: map[string][][]any{"SS/Orders": {{"id", "status", "size", "qty"}}}}, 0)).Mux()
	_, spec := call(t, h, "GET", "/openapi.json", "admin-key", "")
	for _, want := range []string{
		`"status":{"default":"new","enum":["new","paid"],"type":"string"}`,
		`"size":{"enum":["S","M",null],"type":["string","null"]}`,
		`"name":"status","schema":{"enum":["new","paid"],"type":"string"}`,
		`"name":"size[gt]","schema":{"type":"string"}`,
		`"required":["qty"]`,
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("spec lacks %s: %s", want, spec)
		}
	}
}

// A list sends how many rows matched its filters, before limit and offset, as X-Total-Count.
func TestTotalCount(t *testing.T) {
	cfg := &db.Settings{
		Spreadsheets: map[string]db.SheetRule{"shop": {ID: "SS", Tabs: map[string]db.TabRule{"Orders": {Ops: []string{"read"}}}}},
		Keys:         []db.Key{{Name: "admin", Hash: db.HashKey("admin-key"), Tabs: []string{"*"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{tabs: map[string][][]any{"SS/Orders": {{"id", "item"}, {"a", "tea"}, {"b", "mug"}, {"c", "tea"}, {"d", "tea"}}}}
	h := New(cfg, service.New(cfg, store, 0)).Mux()
	for path, want := range map[string]struct {
		total string
		rows  int
	}{
		"/v1/shop/Orders":                           {"4", 4},
		"/v1/shop/Orders?limit=2":                   {"4", 2},
		"/v1/shop/Orders?item=tea&limit=1&offset=1": {"3", 1},
		"/v1/shop/Orders?item=tea&offset=9":         {"3", 0},
	} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer admin-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var rows []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil || rec.Header().Get("X-Total-Count") != want.total || len(rows) != want.rows {
			t.Errorf("%s: total %q, %d rows (%v), want %s and %d", path, rec.Header().Get("X-Total-Count"), len(rows), err, want.total, want.rows)
		}
	}
	if _, spec := call(t, h, "GET", "/openapi.json", "admin-key", ""); !strings.Contains(spec, `"headers":{"X-Total-Count":`) {
		t.Errorf("spec has no X-Total-Count: %s", spec)
	}
}

// limitedStore refuses every read the way the server's Google quota limiter does.
type limitedStore struct{ fakeStore }

func (*limitedStore) Read(context.Context, string, string) ([][]any, error) {
	return nil, &store.RateLimitError{RetryAfter: 2500 * time.Millisecond}
}

// A call the limiter refuses is a 429 whose Retry-After says when there's room again.
func TestRateLimited(t *testing.T) {
	cfg := &db.Settings{
		Spreadsheets: map[string]db.SheetRule{"shop": {ID: "SS", Tabs: map[string]db.TabRule{"Orders": {Ops: []string{"read"}}}}},
		Keys:         []db.Key{{Name: "admin", Hash: db.HashKey("admin-key"), Tabs: []string{"*"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	h := New(cfg, service.New(cfg, &limitedStore{}, 0)).Mux()
	req := httptest.NewRequest("GET", "/v1/shop/Orders", nil)
	req.Header.Set("Authorization", "Bearer admin-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "3" || !strings.Contains(rec.Body.String(), "quota reached; retry in 3s") {
		t.Errorf("got %d, Retry-After %q: %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
}
