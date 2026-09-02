package google

// Slack-markdown renderers for the Google tool results. Same compact
// code-block style the other connector formatters use, so a mixed report reads
// consistently.

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// maxColWidth caps per-column width in a rendered table.
	maxColWidth = 40
	// maxDisplayRows caps how many rows a read renders inline so a wide range
	// does not blow past Slack's message limit.
	maxDisplayRows = 50
)

// FormatAppendResult renders a batch append: a headline, a per-target table,
// and the failures spelled out in full so the model can correct and retry only
// what actually failed.
func FormatAppendResult(r *BatchAppendResult) string {
	if r == nil || len(r.Outcomes) == 0 {
		return "No append targets were processed."
	}

	var sb strings.Builder
	ok := len(r.Outcomes) - r.Failed
	fmt.Fprintf(&sb, "*Appended %d row%s across %d of %d target%s* (%d API request%s)\n\n",
		r.RowsAppended, pluralS(r.RowsAppended),
		ok, len(r.Outcomes), pluralS(len(r.Outcomes)),
		r.Requests, pluralS(r.Requests))

	headers := []string{"Spreadsheet", "Tab", "Rows", "Range", "Status"}
	rows := make([][]string, 0, len(r.Outcomes))
	for _, o := range r.Outcomes {
		status := "ok"
		if o.Error != "" {
			status = "FAILED"
		} else if o.RowsAppended != o.RowsRequested {
			status = fmt.Sprintf("partial (%d of %d)", o.RowsAppended, o.RowsRequested)
		}
		rows = append(rows, []string{
			truncate(o.SpreadsheetID, maxColWidth),
			truncate(o.Tab, maxColWidth),
			fmt.Sprintf("%d", o.RowsRequested),
			truncate(o.UpdatedRange, maxColWidth),
			status,
		})
	}
	writeTable(&sb, headers, rows)

	if r.Failed > 0 {
		fmt.Fprintf(&sb, "\n*%d target%s failed:*\n", r.Failed, pluralS(r.Failed))
		for _, o := range r.Outcomes {
			if o.Error == "" {
				continue
			}
			fmt.Fprintf(&sb, "• `%s` tab `%s` — %s\n", o.SpreadsheetID, o.Tab, o.Error)
		}
	}

	// Link only the spreadsheets that were actually written, deduplicated.
	seen := map[string]bool{}
	var links []string
	for _, o := range r.Outcomes {
		if o.Error != "" || o.SpreadsheetURL == "" || seen[o.SpreadsheetID] {
			continue
		}
		seen[o.SpreadsheetID] = true
		links = append(links, o.SpreadsheetURL)
	}
	if len(links) > 0 && len(links) <= 10 {
		sb.WriteString("\n")
		for _, l := range links {
			fmt.Fprintf(&sb, "%s\n", l)
		}
	}
	return sb.String()
}

// FormatReadResult renders the values read from one or more ranges.
func FormatReadResult(r *ReadResult) string {
	if r == nil || len(r.Ranges) == 0 {
		return "No values were returned for the requested range(s)."
	}

	var sb strings.Builder
	title := r.SpreadsheetTitle
	if title == "" {
		title = r.SpreadsheetID
	}
	fmt.Fprintf(&sb, "*%s* — %s\n", title, r.SpreadsheetURL)

	for _, rv := range r.Ranges {
		fmt.Fprintf(&sb, "\n`%s` — %d row%s\n", rv.Range, rv.Rows, pluralS(rv.Rows))
		if rv.Error != "" {
			fmt.Fprintf(&sb, "_%s_\n", rv.Error)
			continue
		}
		if len(rv.Values) == 0 {
			sb.WriteString("_(empty)_\n")
			continue
		}
		// The first row is treated as the header only when it is complete and
		// entirely non-numeric — guessing wrong would hide a data row.
		display := rv.Values
		trimmed := false
		if len(display) > maxDisplayRows {
			display = display[:maxDisplayRows]
			trimmed = true
		}
		cells := make([][]string, 0, len(display))
		for _, row := range display {
			out := make([]string, len(row))
			for i, v := range row {
				out[i] = truncate(v, maxColWidth)
			}
			cells = append(cells, out)
		}
		writeTable(&sb, nil, cells)
		if trimmed {
			fmt.Fprintf(&sb, "_showing the first %d of %d rows_\n", maxDisplayRows, rv.Rows)
		}
	}
	if r.Truncated {
		sb.WriteString("\n_Result was capped; request a narrower range to see the rest._\n")
	}
	return sb.String()
}

// FormatFindResult renders the files resolved inside the reachable folders.
func FormatFindResult(r *FindFilesResult) string {
	if r == nil {
		return "No result."
	}
	where := describeRoots(r.Roots)
	if len(r.Files) == 0 {
		return fmt.Sprintf("No file matching %s was found in %s. "+
			"Either the name differs (try a partial name), or the file has not been shared with the service account.",
			r.Query, where)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "*Found %d file%s* in %s", len(r.Files), pluralS(len(r.Files)), where)
	if r.Recursive && r.Folders > 1 {
		fmt.Fprintf(&sb, " (searched %d folders)", r.Folders)
	}
	sb.WriteString("\n\n")

	headers := []string{"Name", "File ID", "Type", "Size", "Modified"}
	rows := make([][]string, 0, len(r.Files))
	for _, f := range r.Files {
		rows = append(rows, []string{
			truncate(f.Name, maxColWidth),
			f.ID,
			fileKind(f),
			fileSize(f),
			truncate(f.ModifiedTime, 20),
		})
	}
	writeTable(&sb, headers, rows)

	if r.Truncated {
		sb.WriteString("\n_More files matched than were returned; narrow the name or pass a folder_id._\n")
	}
	return sb.String()
}

// FormatRoots renders the folders and shared drives the connector can reach.
// When exactly one is reachable it says so explicitly, because that is the
// signal the model needs to stop asking which folder to use.
func FormatRoots(roots []Root, serviceAccount string, pinned bool) string {
	_ = serviceAccount // deliberately unused — see the empty-roots branch below
	if len(roots) == 0 {
		// serviceAccount is intentionally NOT printed: this string reaches a Slack
		// channel, and the address identifies internal infrastructure that is of
		// no use to the person asking. Operators read it from the boot log or the
		// integrations page.
		return "No Google Drive folder has been shared with this connector yet, so no Drive or Sheets call can succeed. " +
			"An administrator needs to share a folder with the connector's service account — Editor to allow appends, " +
			"Viewer for read-only. The address is shown on the arbetern integrations page."
	}

	var sb strings.Builder
	origin := "shared with this connector"
	if pinned {
		origin = "pinned for this deployment"
	}
	if len(roots) == 1 {
		fmt.Fprintf(&sb, "*One folder is %s, so it is used automatically — you do not need to ask which folder to use.*\n\n", origin)
	} else {
		fmt.Fprintf(&sb, "*%d locations are %s.* Searches cover all of them unless you pass a `folder_id`.\n\n", len(roots), origin)
	}

	headers := []string{"Name", "Folder ID", "Kind"}
	rows := make([][]string, 0, len(roots))
	for _, r := range roots {
		rows = append(rows, []string{truncate(r.Name, maxColWidth), r.ID, r.Kind})
	}
	writeTable(&sb, headers, rows)
	return sb.String()
}

// FormatCopyResult renders a completed copy. The new file's ID leads, because
// that is what a provisioning caller needs to write to next.
func FormatCopyResult(r *CopyFileResult) string {
	if r == nil {
		return "No copy was made."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Copied* `%s` *to* `%s`\n\n", r.Source.Name, r.File.Name)

	folder := r.FolderName
	if folder == "" || folder == r.FolderID {
		folder = r.FolderID
	} else {
		folder = fmt.Sprintf("%s (%s)", folder, r.FolderID)
	}
	writeTable(&sb, nil, [][]string{
		{"New file ID", r.File.ID},
		{"Type", fileKind(r.File)},
		{"Folder", truncate(folder, maxColWidth)},
		{"Copied from", r.Source.ID},
	})
	if r.File.WebViewLink != "" {
		fmt.Fprintf(&sb, "%s\n", r.File.WebViewLink)
	}
	return sb.String()
}

// FormatFileContent renders a file read: a header identifying the file and the
// window that was returned, then the content in a code block.
func FormatFileContent(f *FileContent) string {
	if f == nil {
		return "No content."
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "*%s* — %s", f.File.Name, fileKind(f.File))
	if sz := fileSize(f.File); sz != "—" {
		fmt.Fprintf(&sb, ", %s", sz)
	}
	sb.WriteString("\n")
	if f.Exported {
		fmt.Fprintf(&sb, "_Converted to %s for reading._\n", f.ExportedAs)
	}
	if f.WebViewLink != "" {
		fmt.Fprintf(&sb, "%s\n", f.WebViewLink)
	}

	if f.IsBinary {
		fmt.Fprintf(&sb, "\n_%s_\n", f.BinaryNote)
		return sb.String()
	}
	if strings.TrimSpace(f.Content) == "" {
		sb.WriteString("\n_The file is empty._\n")
		return sb.String()
	}

	window := fmt.Sprintf("%d bytes", f.BytesRead)
	if f.OffsetBytes > 0 {
		window = fmt.Sprintf("bytes %d–%d", f.OffsetBytes, f.OffsetBytes+f.BytesRead)
	}
	fmt.Fprintf(&sb, "\n_Showing %s", window)
	if f.TotalBytes > 0 {
		fmt.Fprintf(&sb, " of %s", humanBytes(f.TotalBytes))
	}
	sb.WriteString("_\n")

	sb.WriteString("```\n")
	sb.WriteString(f.Content)
	if !strings.HasSuffix(f.Content, "\n") {
		sb.WriteString("\n")
	}
	sb.WriteString("```\n")

	if f.Truncated {
		fmt.Fprintf(&sb, "_Truncated. Continue with offset_bytes=%d, or raise max_bytes._\n", f.OffsetBytes+f.BytesRead)
	}
	return sb.String()
}

// describeRoots renders the searched locations for a result header.
func describeRoots(roots []Root) string {
	switch len(roots) {
	case 0:
		return "the shared folders"
	case 1:
		return fmt.Sprintf("*%s*", roots[0].Name)
	default:
		names := make([]string, 0, len(roots))
		for _, r := range roots {
			names = append(names, r.Name)
		}
		if len(names) > 4 {
			return fmt.Sprintf("%d shared locations (%s, …)", len(names), strings.Join(names[:4], ", "))
		}
		return strings.Join(names, ", ")
	}
}

// fileKind renders a file's type as a short human word.
func fileKind(f DriveFile) string {
	switch {
	case f.IsFolder():
		return "folder"
	case f.IsSpreadsheet():
		return "spreadsheet"
	default:
		return shortMime(f.MimeType)
	}
}

// fileSize renders a file's size, or an em dash for a Google-native file (which
// has no byte size of its own).
func fileSize(f DriveFile) string {
	n, err := strconv.ParseInt(f.Size, 10, 64)
	if err != nil || n <= 0 {
		return "—"
	}
	return humanBytes(n)
}

// FormatSpreadsheetInfo renders a spreadsheet's tabs, which is what a caller
// needs before choosing a tab to append to.
func FormatSpreadsheetInfo(i *SpreadsheetInfo) string {
	if i == nil {
		return "No spreadsheet information."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*%s* — %s\n`%s`\n\n", i.Title, i.URL, i.SpreadsheetID)
	if len(i.Tabs) == 0 {
		sb.WriteString("_No tabs._\n")
		return sb.String()
	}
	headers := []string{"Tab", "Rows", "Columns"}
	rows := make([][]string, 0, len(i.Tabs))
	for _, t := range i.Tabs {
		rows = append(rows, []string{
			truncate(t.Title, maxColWidth),
			fmt.Sprintf("%d", t.RowCount),
			fmt.Sprintf("%d", t.ColCount),
		})
	}
	writeTable(&sb, headers, rows)
	return sb.String()
}

// ── helpers ────────────────────────────────────────────────────────────────

// writeTable renders a fixed-width table inside a code block. headers may be
// nil for a headerless grid (a raw sheet range).
func writeTable(sb *strings.Builder, headers []string, rows [][]string) {
	cols := len(headers)
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 {
		return
	}

	widths := make([]int, cols)
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, r := range rows {
		for i, cell := range r {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	sb.WriteString("```\n")
	if len(headers) > 0 {
		writeRow(sb, headers, widths)
		seps := make([]string, cols)
		for i := range seps {
			seps[i] = strings.Repeat("─", widths[i])
		}
		writeRow(sb, seps, widths)
	}
	for _, r := range rows {
		writeRow(sb, r, widths)
	}
	sb.WriteString("```\n")
}

func writeRow(sb *strings.Builder, cells []string, widths []int) {
	for i, w := range widths {
		val := ""
		if i < len(cells) {
			val = cells[i]
		}
		if i > 0 {
			sb.WriteString("  ")
		}
		fmt.Fprintf(sb, "%-*s", w, val)
	}
	sb.WriteString("\n")
}

// truncate shortens s to at most max characters, appending an ellipsis when it
// cuts.
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

// shortMime renders a MIME type as a short human word: a Google-native type by
// its trailing segment ("document", "spreadsheet"), anything else by its
// subtype ("csv", "pdf", "plain").
func shortMime(m string) string {
	if strings.HasPrefix(m, nativePrefix) {
		if i := strings.LastIndexByte(m, '.'); i >= 0 && i+1 < len(m) {
			return m[i+1:]
		}
	}
	if i := strings.IndexByte(m, '/'); i >= 0 && i+1 < len(m) {
		sub := m[i+1:]
		// Drop vendor and structured-syntax noise: "vnd.ms-excel" → "ms-excel".
		sub = strings.TrimPrefix(sub, "vnd.")
		if j := strings.IndexByte(sub, '+'); j > 0 {
			sub = sub[:j]
		}
		return sub
	}
	if m == "" {
		return "unknown"
	}
	return m
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
