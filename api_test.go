package main

import (
	"context"
	"encoding/json"
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
