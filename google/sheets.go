package google

// Sheets surface: batched appends and range reads.
//
// On batching — the Sheets API allows 60 write requests per minute per user and
// 300 per minute per project, and one workflow tick can target many
// spreadsheets, so the unit of work here is deliberately a BATCH, never a row:
//
//   - Every row destined for the same (spreadsheet, tab) pair is sent in ONE
//     values:append request. 50 rows across 15 spreadsheets is 15 requests, not
//     50, and it is one tool call — which also matters for the 200-round
//     per-tick tool budget in config/config.go.
//   - Reads of several ranges in one spreadsheet collapse into one
//     values:batchGet.
//   - Targets are issued concurrently with a bounded worker pool, under the
//     client-side per-minute budget in client.go, with 429/5xx retried by
//     apiRequest.
//
// Note on values:batchUpdate: it writes to FIXED ranges and has no append
// semantics, so it cannot serve "add rows after whatever is already there"
// without first reading each tab's row count — which costs an extra request per
// tab and races any concurrent writer. values:append is the correct primitive
// for appending and is already one request per tab, so that is what BatchAppend
// uses. BatchWriteRanges below is the batchUpdate path, used when the caller
// names exact ranges.

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
)

const (
	// appendConcurrency bounds how many spreadsheets are written in parallel.
	// Small on purpose: the per-minute budget is the real throttle, and a wide
	// fan-out just queues behind it while holding more sockets open.
	appendConcurrency = 4

	// maxBatchRows caps the rows one tool call may write across all targets, so
	// a runaway model cannot turn a single call into a multi-megabyte upload.
	maxBatchRows = 5000

	// maxBatchTargets caps the (spreadsheet, tab) pairs one call may touch.
	maxBatchTargets = 100

	// maxReadRanges caps the ranges one read call may request.
	maxReadRanges = 50

	// maxReadCells caps the cells returned to the model from one read.
	maxReadCells = 20000
)

// AppendTarget is one destination for a batch append: rows go to a tab of a
// spreadsheet.
type AppendTarget struct {
	SpreadsheetID string     `json:"spreadsheet_id"`
	Tab           string     `json:"tab"`
	Rows          [][]string `json:"rows"`
}

// AppendOutcome reports what happened for one target.
type AppendOutcome struct {
	SpreadsheetID  string `json:"spreadsheet_id"`
	SpreadsheetURL string `json:"spreadsheet_url,omitempty"`
	Tab            string `json:"tab"`
	RowsRequested  int    `json:"rows_requested"`
	RowsAppended   int    `json:"rows_appended"`
	UpdatedRange   string `json:"updated_range,omitempty"`
	Error          string `json:"error,omitempty"`
}

// BatchAppendResult aggregates a whole batch append.
type BatchAppendResult struct {
	Outcomes     []AppendOutcome `json:"outcomes"`
	Requests     int             `json:"requests"`      // Google API requests actually issued
	RowsAppended int             `json:"rows_appended"` // total across successful targets
	Failed       int             `json:"failed"`
}

// BatchAppend appends rows to one or more (spreadsheet, tab) targets. Targets
// naming the same spreadsheet and tab are merged so their rows travel in a
// single request, preserving the order they were supplied in.
//
// Every distinct spreadsheet is checked against the scoped folder before any
// write is issued: a batch containing one out-of-scope target fails that target
// and still writes the rest, rather than silently writing outside the folder or
// discarding valid work.
func (c *Client) BatchAppend(ctx context.Context, targets []AppendTarget, valueInputRaw bool) (*BatchAppendResult, error) {
	if c == nil {
		return nil, fmt.Errorf("google client is not configured")
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("at least one target with rows is required")
	}

	// Merge by (spreadsheet, tab), keeping first-seen order stable so the
	// rendered result matches what the caller asked for.
	type key struct{ sheet, tab string }
	merged := make(map[key]*AppendTarget, len(targets))
	var order []key
	totalRows := 0
	for i, t := range targets {
		sheetID := strings.TrimSpace(t.SpreadsheetID)
		tab := strings.TrimSpace(t.Tab)
		if sheetID == "" {
			return nil, fmt.Errorf("target %d is missing spreadsheet_id", i+1)
		}
		if tab == "" {
			return nil, fmt.Errorf("target %d (spreadsheet %s) is missing tab", i+1, sheetID)
		}
		if len(t.Rows) == 0 {
			return nil, fmt.Errorf("target %d (spreadsheet %s, tab %s) has no rows", i+1, sheetID, tab)
		}
		totalRows += len(t.Rows)
		if totalRows > maxBatchRows {
			return nil, fmt.Errorf("batch exceeds the %d-row limit for a single call; split it across calls", maxBatchRows)
		}
		k := key{sheetID, tab}
		if cur, ok := merged[k]; ok {
			cur.Rows = append(cur.Rows, t.Rows...)
			continue
		}
		rows := make([][]string, len(t.Rows))
		copy(rows, t.Rows)
		merged[k] = &AppendTarget{SpreadsheetID: sheetID, Tab: tab, Rows: rows}
		order = append(order, k)
	}
	if len(order) > maxBatchTargets {
		return nil, fmt.Errorf("batch touches %d (spreadsheet, tab) targets, above the limit of %d; split it across calls", len(order), maxBatchTargets)
	}

	// Scope-check each distinct spreadsheet once.
	scopeErr := make(map[string]error)
	for _, k := range order {
		if _, done := scopeErr[k.sheet]; done {
			continue
		}
		scopeErr[k.sheet] = c.withinScope(ctx, k.sheet)
	}

	result := &BatchAppendResult{Outcomes: make([]AppendOutcome, len(order))}
	valueInputOption := "USER_ENTERED"
	if valueInputRaw {
		valueInputOption = "RAW"
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		sem  = make(chan struct{}, appendConcurrency)
		reqs int
	)
	for idx, k := range order {
		t := merged[k]
		out := AppendOutcome{
			SpreadsheetID: t.SpreadsheetID,
			Tab:           t.Tab,
			RowsRequested: len(t.Rows),
		}
		if err := scopeErr[t.SpreadsheetID]; err != nil {
			out.Error = err.Error()
			result.Outcomes[idx] = out
			result.Failed++
			continue
		}

		wg.Add(1)
		go func(idx int, t *AppendTarget, out AppendOutcome) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			resp, err := c.appendValues(ctx, t.SpreadsheetID, t.Tab, t.Rows, valueInputOption)
			mu.Lock()
			reqs++
			if err != nil {
				// The outcome carries the sanitized message (it reaches a Slack
				// channel); the full Google diagnostic goes to the log only.
				logf("[google] append failed for spreadsheet %s tab %q: %s", t.SpreadsheetID, t.Tab, ErrorDetail(err))
				out.Error = err.Error()
				result.Failed++
			} else {
				out.RowsAppended = resp.Updates.UpdatedRows
				out.UpdatedRange = resp.Updates.UpdatedRange
				out.SpreadsheetURL = SpreadsheetURL(t.SpreadsheetID)
				result.RowsAppended += resp.Updates.UpdatedRows
			}
			result.Outcomes[idx] = out
			mu.Unlock()
		}(idx, t, out)
	}
	wg.Wait()
	result.Requests = reqs
	return result, nil
}

// appendResponse is the values:append reply.
type appendResponse struct {
	SpreadsheetID string `json:"spreadsheetId"`
	TableRange    string `json:"tableRange"`
	Updates       struct {
		UpdatedRange   string `json:"updatedRange"`
		UpdatedRows    int    `json:"updatedRows"`
		UpdatedColumns int    `json:"updatedColumns"`
		UpdatedCells   int    `json:"updatedCells"`
	} `json:"updates"`
}

// appendValues issues one values:append for every row bound for a single tab.
func (c *Client) appendValues(ctx context.Context, spreadsheetID, tab string, rows [][]string, valueInputOption string) (*appendResponse, error) {
	q := url.Values{}
	q.Set("valueInputOption", valueInputOption)
	// INSERT_ROWS never overwrites whatever sits below the current table;
	// OVERWRITE (the default) would clobber a footer or a second table.
	q.Set("insertDataOption", "INSERT_ROWS")
	q.Set("includeValuesInResponse", "false")
	q.Set("responseValueRenderOption", "UNFORMATTED_VALUE")

	// The append range is the tab alone: Sheets finds the end of the existing
	// table itself, which is exactly the semantics we want.
	rangeSpec := quoteTab(tab)

	payload := map[string]any{
		"range":          rangeSpec,
		"majorDimension": "ROWS",
		"values":         rows,
	}

	endpoint := fmt.Sprintf("%s/spreadsheets/%s/values/%s:append?%s",
		sheetsBase, url.PathEscape(spreadsheetID), url.PathEscape(rangeSpec), q.Encode())

	var resp appendResponse
	if err := c.apiRequest(ctx, "POST", endpoint, payload, &resp, true); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ── Reads ──────────────────────────────────────────────────────────────────

// RangeValues is one range's values.
type RangeValues struct {
	Range  string     `json:"range"`
	Values [][]string `json:"values"`
	Rows   int        `json:"rows"`
	Error  string     `json:"error,omitempty"`
}

// ReadResult is the outcome of a range read.
type ReadResult struct {
	SpreadsheetID    string        `json:"spreadsheet_id"`
	SpreadsheetURL   string        `json:"spreadsheet_url"`
	SpreadsheetTitle string        `json:"spreadsheet_title,omitempty"`
	Ranges           []RangeValues `json:"ranges"`
	Truncated        bool          `json:"truncated"`
}

// BatchRead reads one or more ranges from a single spreadsheet in ONE
// values:batchGet request. The spreadsheet is scope-checked first.
func (c *Client) BatchRead(ctx context.Context, spreadsheetID string, ranges []string, formatted bool) (*ReadResult, error) {
	if c == nil {
		return nil, fmt.Errorf("google client is not configured")
	}
	spreadsheetID = strings.TrimSpace(spreadsheetID)
	var clean []string
	for _, r := range ranges {
		if r = strings.TrimSpace(r); r != "" {
			clean = append(clean, r)
		}
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("at least one range is required (e.g. \"Sheet1!A1:D50\" or a bare tab name)")
	}
	if len(clean) > maxReadRanges {
		return nil, fmt.Errorf("read requests %d ranges, above the limit of %d", len(clean), maxReadRanges)
	}
	if err := c.withinScope(ctx, spreadsheetID); err != nil {
		return nil, err
	}

	q := url.Values{}
	for _, r := range clean {
		q.Add("ranges", r)
	}
	q.Set("majorDimension", "ROWS")
	if formatted {
		q.Set("valueRenderOption", "FORMATTED_VALUE")
	} else {
		q.Set("valueRenderOption", "UNFORMATTED_VALUE")
	}
	q.Set("dateTimeRenderOption", "FORMATTED_STRING")

	endpoint := fmt.Sprintf("%s/spreadsheets/%s/values:batchGet?%s",
		sheetsBase, url.PathEscape(spreadsheetID), q.Encode())

	var resp struct {
		SpreadsheetID string `json:"spreadsheetId"`
		ValueRanges   []struct {
			Range  string  `json:"range"`
			Values [][]any `json:"values"`
		} `json:"valueRanges"`
	}
	if err := c.apiRequest(ctx, "GET", endpoint, nil, &resp, false); err != nil {
		return nil, err
	}

	out := &ReadResult{
		SpreadsheetID:  spreadsheetID,
		SpreadsheetURL: SpreadsheetURL(spreadsheetID),
	}
	cells := 0
	for _, vr := range resp.ValueRanges {
		rv := RangeValues{Range: vr.Range}
		for _, row := range vr.Values {
			if cells >= maxReadCells {
				out.Truncated = true
				break
			}
			cellRow := make([]string, len(row))
			for i, v := range row {
				cellRow[i] = cellString(v)
			}
			cells += len(cellRow)
			rv.Values = append(rv.Values, cellRow)
		}
		rv.Rows = len(rv.Values)
		out.Ranges = append(out.Ranges, rv)
	}
	return out, nil
}

// cellString renders a JSON cell value as text. Sheets returns numbers,
// booleans and strings; a bare %v on a float64 would print 1e+06 for a
// seven-figure ARR, so integers are rendered without an exponent.
func cellString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "TRUE"
		}
		return "FALSE"
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", t), "0"), ".")
	default:
		return fmt.Sprintf("%v", t)
	}
}

// ── Spreadsheet metadata ───────────────────────────────────────────────────

// SheetTab is one tab of a spreadsheet.
type SheetTab struct {
	Title    string `json:"title"`
	SheetID  int64  `json:"sheet_id"`
	RowCount int    `json:"row_count"`
	ColCount int    `json:"col_count"`
}

// SpreadsheetInfo is a spreadsheet's title and tab list.
type SpreadsheetInfo struct {
	SpreadsheetID string     `json:"spreadsheet_id"`
	Title         string     `json:"title"`
	URL           string     `json:"url"`
	Tabs          []SheetTab `json:"tabs"`
}

// GetSpreadsheetInfo returns a spreadsheet's title and tabs, so a caller can
// discover the exact tab name before appending to it. Scope-checked.
func (c *Client) GetSpreadsheetInfo(ctx context.Context, spreadsheetID string) (*SpreadsheetInfo, error) {
	if c == nil {
		return nil, fmt.Errorf("google client is not configured")
	}
	spreadsheetID = strings.TrimSpace(spreadsheetID)
	if err := c.withinScope(ctx, spreadsheetID); err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("fields", "properties.title,sheets.properties(sheetId,title,gridProperties)")
	endpoint := fmt.Sprintf("%s/spreadsheets/%s?%s", sheetsBase, url.PathEscape(spreadsheetID), q.Encode())

	var resp struct {
		Properties struct {
			Title string `json:"title"`
		} `json:"properties"`
		Sheets []struct {
			Properties struct {
				SheetID        int64  `json:"sheetId"`
				Title          string `json:"title"`
				GridProperties struct {
					RowCount    int `json:"rowCount"`
					ColumnCount int `json:"columnCount"`
				} `json:"gridProperties"`
			} `json:"properties"`
		} `json:"sheets"`
	}
	if err := c.apiRequest(ctx, "GET", endpoint, nil, &resp, false); err != nil {
		return nil, err
	}

	info := &SpreadsheetInfo{
		SpreadsheetID: spreadsheetID,
		Title:         resp.Properties.Title,
		URL:           SpreadsheetURL(spreadsheetID),
	}
	for _, s := range resp.Sheets {
		info.Tabs = append(info.Tabs, SheetTab{
			Title:    s.Properties.Title,
			SheetID:  s.Properties.SheetID,
			RowCount: s.Properties.GridProperties.RowCount,
			ColCount: s.Properties.GridProperties.ColumnCount,
		})
	}
	sort.SliceStable(info.Tabs, func(i, j int) bool { return info.Tabs[i].Title < info.Tabs[j].Title })
	return info, nil
}

// SpreadsheetURL returns the browser URL of a spreadsheet, so a Slack report
// can link straight to what it wrote.
func SpreadsheetURL(spreadsheetID string) string {
	if strings.TrimSpace(spreadsheetID) == "" {
		return ""
	}
	return "https://docs.google.com/spreadsheets/d/" + spreadsheetID
}

// quoteTab renders a tab name for use in an A1 range. Sheets requires single
// quotes around a title containing a space or punctuation, with inner quotes
// doubled; quoting unconditionally is simpler and always valid.
func quoteTab(tab string) string {
	return "'" + strings.ReplaceAll(tab, "'", "''") + "'"
}
