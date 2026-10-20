package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"google.golang.org/api/sheets/v4"
)

// TestLive runs the Service against a real spreadsheet. It needs:
//
//	GOOGLE_APPLICATION_CREDENTIALS=service-account.json
//	SHEETSERVE_LIVE_SHEET_ID=<spreadsheet shared with the service account as Editor>
//
// It creates a scratch tab (with a space and an apostrophe in its name, to test quoting) and deletes it afterwards.
func TestLive(t *testing.T) {
	sheetID := os.Getenv("SHEETSERVE_LIVE_SHEET_ID")
	if sheetID == "" {
		t.Skip("set SHEETSERVE_LIVE_SHEET_ID and GOOGLE_APPLICATION_CREDENTIALS to run")
	}
	ctx := context.Background()
	store, err := newGoogleStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tab := fmt.Sprintf("it's sheetserve %d", time.Now().UnixNano())

	// Set up the scratch tab with a header row.
	resp, err := store.svc.Spreadsheets.BatchUpdate(sheetID, &sheets.BatchUpdateSpreadsheetRequest{
		Requests: []*sheets.Request{{AddSheet: &sheets.AddSheetRequest{Properties: &sheets.SheetProperties{Title: tab}}}},
	}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("add scratch tab: %v", err)
	}
	gid := resp.Replies[0].AddSheet.Properties.SheetId
	t.Cleanup(func() {
		store.svc.Spreadsheets.BatchUpdate(sheetID, &sheets.BatchUpdateSpreadsheetRequest{
			Requests: []*sheets.Request{{DeleteSheet: &sheets.DeleteSheetRequest{SheetId: gid}}},
		}).Context(context.Background()).Do()
	})
	setRow := func(rng string, row []any, input string) {
		t.Helper()
		_, err := store.svc.Spreadsheets.Values.Update(sheetID, quoteTab(tab)+"!"+rng, &sheets.ValueRange{Values: [][]any{row}}).
			ValueInputOption(input).Context(ctx).Do()
		if err != nil {
			t.Fatalf("set %s: %v", rng, err)
		}
	}
	setRow("A1", []any{"id", "item", "qty", "total", "secret"}, "RAW")

	cfg := &Config{Spreadsheets: map[string]Spreadsheet{"live": {ID: sheetID, Tabs: map[string]TabRule{
		tab: {Ops: allOps, Hidden: []string{"secret"}},
	}}}}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	svc := newService(cfg, store)

	// Create: lands in columns A.., number stays a number, formula-looking input stays text.
	a, err := svc.Create(ctx, "live", tab, map[string]any{"item": "=1+1", "qty": 2})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	b, err := svc.Create(ctx, "live", tab, map[string]any{"item": "tea", "qty": 7})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	idA, idB := a["id"].(string), b["id"].(string)
	got, err := svc.Get(ctx, "live", tab, idA)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got["item"] != "=1+1" || got["qty"] != float64(2) {
		t.Fatalf("created row read back as %v; want item \"=1+1\" (RAW, not evaluated) and qty 2 (number)", got)
	}
	if _, ok := got["secret"]; ok {
		t.Fatalf("hidden column returned: %v", got)
	}

	// A formula in another column must survive an update of qty.
	setRow("D2", []any{"=C2*10"}, "USER_ENTERED")
	got, err = svc.Update(ctx, "live", tab, idA, map[string]any{"qty": 5})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	formula, err := store.svc.Spreadsheets.Values.Get(sheetID, quoteTab(tab)+"!D2").ValueRenderOption("FORMULA").Context(ctx).Do()
	if err != nil || len(formula.Values) == 0 || formula.Values[0][0] != "=C2*10" {
		t.Fatalf("formula in D2 not preserved: %v %v", formula, err)
	}
	got, _ = svc.Get(ctx, "live", tab, idA)
	if got["total"] != float64(50) {
		t.Fatalf("total = %v, want 50 (formula recomputed from updated qty)", got["total"])
	}

	// Delete the first row; the second must still be found and the first must be gone.
	if err := svc.Delete(ctx, "live", tab, idA); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Get(ctx, "live", tab, idA); err != errNotFound {
		t.Fatalf("deleted row still readable: %v", err)
	}
	rows, err := svc.List(ctx, "live", tab, map[string]string{"item": "tea"}, "", 0, 0)
	if err != nil || len(rows) != 1 || rows[0]["id"] != idB || rows[0]["qty"] != float64(7) {
		t.Fatalf("list after delete: %v %v", rows, err)
	}
}
