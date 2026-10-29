package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/service"
	"github.com/amalgamated-tools/sheetserve/internal/store"
	"google.golang.org/api/option"
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
	gs, err := store.NewGoogleStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sheetsSvc, err := sheets.NewService(ctx, option.WithScopes(sheets.SpreadsheetsScope))
	if err != nil {
		t.Fatal(err)
	}
	tab := fmt.Sprintf("it's sheetserve %d", time.Now().UnixNano())

	// Set up the scratch tab with a header row.
	resp, err := sheetsSvc.Spreadsheets.BatchUpdate(sheetID, &sheets.BatchUpdateSpreadsheetRequest{
		Requests: []*sheets.Request{{AddSheet: &sheets.AddSheetRequest{Properties: &sheets.SheetProperties{Title: tab}}}},
	}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("add scratch tab: %v", err)
	}
	gid := resp.Replies[0].AddSheet.Properties.SheetId
	t.Cleanup(func() {
		if _, err := sheetsSvc.Spreadsheets.BatchUpdate(sheetID, &sheets.BatchUpdateSpreadsheetRequest{
			Requests: []*sheets.Request{{DeleteSheet: &sheets.DeleteSheetRequest{SheetId: gid}}},
		}).Context(context.Background()).Do(); err != nil {
			t.Errorf("delete scratch tab: %v", err)
		}
	})
	setRow := func(rng string, row []any, input string) {
		t.Helper()
		_, err := sheetsSvc.Spreadsheets.Values.Update(sheetID, quotedTab(tab)+"!"+rng, &sheets.ValueRange{Values: [][]any{row}}).
			ValueInputOption(input).Context(ctx).Do()
		if err != nil {
			t.Fatalf("set %s: %v", rng, err)
		}
	}
	setRow("A1", []any{"id", "item", "qty", "total", "secret"}, "RAW")

	cfg := &db.Settings{Spreadsheets: map[string]db.SheetRule{"live": {ID: sheetID, Tabs: map[string]db.TabRule{
		tab: {Ops: db.AllOps, Hidden: []string{"secret"}},
	}}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	svc := service.New(cfg, gs, 0)

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
	_, err = svc.Update(ctx, "live", tab, idA, map[string]any{"qty": 5})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	formula, err := sheetsSvc.Spreadsheets.Values.Get(sheetID, quotedTab(tab)+"!D2").ValueRenderOption("FORMULA").Context(ctx).Do()
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
	var notFound *service.APIError
	if _, err := svc.Get(ctx, "live", tab, idA); !errors.As(err, &notFound) || notFound.Status != http.StatusNotFound {
		t.Fatalf("deleted row still readable: %v", err)
	}
	rows, _, err := svc.List(ctx, "live", tab, map[string]string{"item": "tea"}, "", 0, 0)
	if err != nil || len(rows) != 1 || rows[0]["id"] != idB || rows[0]["qty"] != float64(7) {
		t.Fatalf("list after delete: %v %v", rows, err)
	}
}

func quotedTab(tab string) string { return "'" + strings.ReplaceAll(tab, "'", "''") + "'" }
