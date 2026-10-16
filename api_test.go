package main

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
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
	for c, v := range cells {
		f.tabs[id+"/"+tab][row-1][c] = v
	}
	return nil
}

func (f *fakeStore) DeleteRow(_ context.Context, id, tab string, row int) error {
	k := id + "/" + tab
	f.tabs[k] = slices.Delete(f.tabs[k], row-1, row)
	return nil
}

func newTestAPI(t *testing.T) http.Handler {
	t.Helper()
	cfg := &Config{
		CacheTTL: time.Minute, // exercises cache invalidation on writes
		Spreadsheets: map[string]Spreadsheet{"shop": {ID: "SS", Tabs: map[string]TabRule{
			"Products": {Ops: []string{"read"}, Hidden: []string{"cost"}},
			"Orders":   {Ops: allOps},
		}}},
		Keys: []Key{
			{Name: "admin", Hash: hashKey("admin-key"), Tabs: []string{"*"}},
			{Name: "web", Hash: hashKey("web-key"), Tabs: []string{"shop/*"}, Ops: []string{"read"}},
		},
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{tabs: map[string][][]any{
		"SS/Products": {{"id", "name", "cost"}, {"p1", "mug", 3.5}},
		"SS/Orders":   {{"id", "item", "qty"}},
	}}
	return (&API{cfg: cfg, svc: newService(cfg, store)}).routes()
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

func TestColName(t *testing.T) {
	for i, want := range map[int]string{0: "A", 25: "Z", 26: "AA", 51: "AZ", 52: "BA", 701: "ZZ", 702: "AAA"} {
		if got := colName(i); got != want {
			t.Errorf("colName(%d) = %s, want %s", i, got, want)
		}
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
	cfg := &Config{
		Spreadsheets: map[string]Spreadsheet{"shop": {ID: "SS", Tabs: map[string]TabRule{
			"A B": {Ops: []string{"read"}},
			"A_B": {Ops: []string{"read"}, Hidden: []string{"id"}},
		}}},
		Keys: []Key{{Name: "admin", Hash: hashKey("admin-key"), Tabs: []string{"*"}}},
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{tabs: map[string][][]any{
		"SS/A B": {{"id", "limit", "name", "name"}},
		"SS/A_B": {{"id", "name"}},
	}}
	_, body := call(t, (&API{cfg: cfg, svc: newService(cfg, store)}).routes(), "GET", "/openapi.json", "admin-key", "")
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
	for _, col := range []string{"id", "name"} {
		want = append(want, col)
		for _, op := range filterOps {
			want = append(want, col+"["+op+"]")
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

// A tab that fails to load keeps its name reserved, so the next tab can't take over its tools.
func TestFailedTabKeepsName(t *testing.T) {
	cfg := &Config{
		Spreadsheets: map[string]Spreadsheet{"shop": {ID: "SS", Tabs: map[string]TabRule{
			"A B": {Ops: []string{"read"}},
			"A_B": {Ops: []string{"read"}},
		}}},
		Keys: []Key{{Name: "admin", Hash: hashKey("admin-key"), Tabs: []string{"*"}}},
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{tabs: map[string][][]any{"SS/A_B": {{"id", "name"}}}} // "A B" has no id column
	a := &API{cfg: cfg, svc: newService(cfg, store)}
	var names []string
	a.eachTab(context.Background(), &cfg.Keys[0], func(name, _, _ string, _, _ []string) { names = append(names, name) })
	if !slices.Equal(names, []string{"shop_A_B_2"}) {
		t.Fatalf("names %v, want [shop_A_B_2]", names)
	}
}

func TestListSortAndFilterOps(t *testing.T) {
	cfg := &Config{
		Spreadsheets: map[string]Spreadsheet{"shop": {ID: "SS", Tabs: map[string]TabRule{
			"Orders": {Ops: []string{"read"}, Hidden: []string{"cost"}},
		}}},
		Keys: []Key{{Name: "admin", Hash: hashKey("admin-key"), Tabs: []string{"*"}}},
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{tabs: map[string][][]any{"SS/Orders": {
		{"id", "item", "qty", "cost", "a[b]"},
		{"o1", "Green Tea", "10", "1", "x"},
		{"o2", "mug", "9", "2", "y"},
		{"o3", "tea pot", "2", "3", "z"},
	}}}
	h := (&API{cfg: cfg, svc: newService(cfg, store)}).routes()
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
	cfg := &Config{CORSOrigins: []string{"https://app.example"}}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	h := (&API{cfg: cfg, svc: newService(cfg, &fakeStore{})}).routes()
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
	if rec := req("GET", "https://app.example"); rec.Code != 401 || rec.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Errorf("actual request should carry CORS headers: %d %v", rec.Code, rec.Header())
	}
	if rec := req("OPTIONS", "https://evil.example"); rec.Code == 204 || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("unlisted origin allowed: %d %v", rec.Code, rec.Header())
	}
}
