package store

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"

	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
)

// Store is the raw spreadsheet backend. Rows are 1-based sheet row numbers; columns are 0-based.
type Store interface {
	Read(ctx context.Context, spreadsheetID, tab string) ([][]any, error)
	Append(ctx context.Context, spreadsheetID, tab string, row []any) error
	SetCells(ctx context.Context, spreadsheetID, tab string, row int, cells map[int]any) error
	DeleteRow(ctx context.Context, spreadsheetID, tab string, row int) error
	Tabs(ctx context.Context, spreadsheetID string) ([]string, error)
}

// Tab is a tab for a new spreadsheet, with the header row it starts with.
type Tab struct {
	Name    string
	Columns []string
}

// Creator makes new spreadsheets. A Store that can't create one doesn't implement it.
type Creator interface {
	Create(ctx context.Context, title string, tabs []Tab) (spreadsheetID string, err error)
}

type googleStore struct {
	svc  *sheets.Service
	mu   sync.Mutex
	gids map[string]int64 // "spreadsheetID/tab" -> sheetId, needed for row deletes
}

// newGoogleStore authenticates with Application Default Credentials
// (GOOGLE_APPLICATION_CREDENTIALS pointing at a service account JSON file) unless opts say otherwise.
func NewGoogleStore(ctx context.Context, opts ...option.ClientOption) (*googleStore, error) {
	svc, err := sheets.NewService(ctx, append([]option.ClientOption{option.WithScopes(sheets.SpreadsheetsScope)}, opts...)...)
	if err != nil {
		return nil, err
	}
	return &googleStore{svc: svc, gids: map[string]int64{}}, nil
}

func (g *googleStore) Read(ctx context.Context, id, tab string) (_ [][]any, err error) {
	defer logCall(ctx, "read", id, tab, time.Now(), &err)
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

func (g *googleStore) Append(ctx context.Context, id, tab string, row []any) (err error) {
	defer logCall(ctx, "append", id, tab, time.Now(), &err)
	_, err = g.svc.Spreadsheets.Values.Append(id, quoteTab(tab)+"!A1", &sheets.ValueRange{Values: [][]any{row}}).
		ValueInputOption("RAW").InsertDataOption("INSERT_ROWS").Context(ctx).Do()
	return err
}

func (g *googleStore) SetCells(ctx context.Context, id, tab string, row int, cells map[int]any) (err error) {
	defer logCall(ctx, "set_cells", id, tab, time.Now(), &err)
	req := &sheets.BatchUpdateValuesRequest{ValueInputOption: "RAW"}
	for col, v := range cells {
		req.Data = append(req.Data, &sheets.ValueRange{
			Range:  quoteTab(tab) + "!" + colName(col) + strconv.Itoa(row),
			Values: [][]any{{v}},
		})
	}
	_, err = g.svc.Spreadsheets.Values.BatchUpdate(id, req).Context(ctx).Do()
	return err
}

func (g *googleStore) DeleteRow(ctx context.Context, id, tab string, row int) (err error) {
	defer logCall(ctx, "delete_row", id, tab, time.Now(), &err)
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

// Tabs returns the spreadsheet's tab titles in sheet order.
func (g *googleStore) Tabs(ctx context.Context, id string) (_ []string, err error) {
	defer logCall(ctx, "tabs", id, "", time.Now(), &err)
	ss, err := g.svc.Spreadsheets.Get(id).Fields("sheets.properties.title").Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, sh := range ss.Sheets {
		out = append(out, sh.Properties.Title)
	}
	return out, nil
}

// Create makes a spreadsheet with tabs in order, each with its header row written and frozen.
func (g *googleStore) Create(ctx context.Context, title string, tabs []Tab) (_ string, err error) {
	defer logCall(ctx, "create", "", "", time.Now(), &err)
	ss := &sheets.Spreadsheet{Properties: &sheets.SpreadsheetProperties{Title: title}}
	for _, t := range tabs {
		var header []*sheets.CellData
		for _, c := range t.Columns {
			header = append(header, &sheets.CellData{UserEnteredValue: &sheets.ExtendedValue{StringValue: &c}})
		}
		ss.Sheets = append(ss.Sheets, &sheets.Sheet{
			Properties: &sheets.SheetProperties{Title: t.Name, GridProperties: &sheets.GridProperties{FrozenRowCount: 1}},
			Data:       []*sheets.GridData{{RowData: []*sheets.RowData{{Values: header}}}},
		})
	}
	created, err := g.svc.Spreadsheets.Create(ss).Fields("spreadsheetId").Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return created.SpreadsheetId, nil
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

// logCall logs a Google Sheets call at debug once it returns, as
// defer logCall(ctx, op, id, tab, time.Now(), &err). An empty id or tab is left out.
func logCall(ctx context.Context, op, id, tab string, start time.Time, err *error) {
	if !slog.Default().Enabled(ctx, slog.LevelDebug) {
		return
	}
	attrs := []slog.Attr{slog.String(otelkeys.Op, op), slog.Int64(otelkeys.DurationMs, time.Since(start).Milliseconds())}
	if id != "" {
		attrs = append(attrs, slog.String(otelkeys.SpreadsheetID, id))
	}
	if tab != "" {
		attrs = append(attrs, slog.String(otelkeys.Tab, tab))
	}
	if *err != nil {
		attrs = append(attrs, slog.Any(otelkeys.Error, *err))
	}
	slog.LogAttrs(ctx, slog.LevelDebug, "google sheets call", attrs...)
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
