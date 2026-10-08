package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// Store is the raw spreadsheet backend. Rows are 1-based sheet row numbers; columns are 0-based.
type Store interface {
	Read(ctx context.Context, spreadsheetID, tab string) ([][]any, error)
	Append(ctx context.Context, spreadsheetID, tab string, row []any) error
	SetCells(ctx context.Context, spreadsheetID, tab string, row int, cells map[int]any) error
	DeleteRow(ctx context.Context, spreadsheetID, tab string, row int) error
}

type googleStore struct {
	svc  *sheets.Service
	mu   sync.Mutex
	gids map[string]int64 // "spreadsheetID/tab" -> sheetId, needed for row deletes
}

// newGoogleStore authenticates with Application Default Credentials
// (GOOGLE_APPLICATION_CREDENTIALS pointing at a service account JSON file).
func newGoogleStore(ctx context.Context) (*googleStore, error) {
	svc, err := sheets.NewService(ctx, option.WithScopes(sheets.SpreadsheetsScope))
	if err != nil {
		return nil, err
	}
	return &googleStore{svc: svc, gids: map[string]int64{}}, nil
}

func (g *googleStore) Read(ctx context.Context, id, tab string) ([][]any, error) {
	resp, err := g.svc.Spreadsheets.Values.Get(id, quoteTab(tab)).
		ValueRenderOption("UNFORMATTED_VALUE").
		DateTimeRenderOption("FORMATTED_STRING").
		Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return resp.Values, nil
}

// Writes use RAW input so client data is never parsed as a formula.

func (g *googleStore) Append(ctx context.Context, id, tab string, row []any) error {
	_, err := g.svc.Spreadsheets.Values.Append(id, quoteTab(tab)+"!A1", &sheets.ValueRange{Values: [][]any{row}}).
		ValueInputOption("RAW").InsertDataOption("INSERT_ROWS").Context(ctx).Do()
	return err
}

func (g *googleStore) SetCells(ctx context.Context, id, tab string, row int, cells map[int]any) error {
	req := &sheets.BatchUpdateValuesRequest{ValueInputOption: "RAW"}
	for col, v := range cells {
		req.Data = append(req.Data, &sheets.ValueRange{
			Range:  quoteTab(tab) + "!" + colName(col) + strconv.Itoa(row),
			Values: [][]any{{v}},
		})
	}
	_, err := g.svc.Spreadsheets.Values.BatchUpdate(id, req).Context(ctx).Do()
	return err
}

func (g *googleStore) DeleteRow(ctx context.Context, id, tab string, row int) error {
	gid, err := g.gid(ctx, id, tab)
	if err != nil {
		return err
	}
	_, err = g.svc.Spreadsheets.BatchUpdate(id, &sheets.BatchUpdateSpreadsheetRequest{
		Requests: []*sheets.Request{{DeleteDimension: &sheets.DeleteDimensionRequest{
			Range: &sheets.DimensionRange{
				SheetId: gid, Dimension: "ROWS", StartIndex: int64(row - 1), EndIndex: int64(row),
				ForceSendFields: []string{"SheetId"}, // the first tab's id is 0
			},
		}}},
	}).Context(ctx).Do()
	return err
}

func (g *googleStore) gid(ctx context.Context, id, tab string) (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if v, ok := g.gids[id+"/"+tab]; ok {
		return v, nil
	}
	ss, err := g.svc.Spreadsheets.Get(id).Fields("sheets.properties(sheetId,title)").Context(ctx).Do()
	if err != nil {
		return 0, err
	}
	for _, sh := range ss.Sheets {
		g.gids[id+"/"+sh.Properties.Title] = sh.Properties.SheetId
	}
	v, ok := g.gids[id+"/"+tab]
	if !ok {
		return 0, fmt.Errorf("tab %q not found in spreadsheet", tab)
	}
	return v, nil
}

func quoteTab(tab string) string { return "'" + strings.ReplaceAll(tab, "'", "''") + "'" }

// colName converts a 0-based column index to A1 letters: 0→A, 25→Z, 26→AA.
func colName(i int) string {
	s := ""
	for i++; i > 0; i = (i - 1) / 26 {
		s = string(rune('A'+(i-1)%26)) + s
	}
	return s
}
