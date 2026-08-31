package google

// Downloading and reading file content.
//
// Drive has two distinct download paths and picking the wrong one is the usual
// cause of a confusing 403:
//
//   - Uploaded/binary files (CSV, TXT, JSON, PDF, XLSX, images) are fetched with
//     `files.get?alt=media`, which streams the raw bytes.
//   - Google-native files (Docs, Sheets, Slides) have NO byte content at all and
//     must be converted on the fly with `files.export?mimeType=…`. Asking for
//     alt=media on a Doc fails.
//
// exportMIME below encodes that choice so a caller never has to.
//
// Everything here streams. The body is consumed through an io.LimitReader and a
// caller-supplied cap, so a 400 MB video shared into the folder cannot be pulled
// into memory: the read stops at the cap, the rest of the body is drained and
// discarded, and the result is marked truncated. Nothing is ever written to disk.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// defaultReadBytes is how much of a file is returned when the caller does
	// not say. Mirrors the http_get tool's default so a model's intuition about
	// one transfers to the other.
	defaultReadBytes = 200_000

	// maxReadBytes is the hard ceiling on what one read may return to the model.
	maxReadBytes = 1 << 20 // 1 MiB

	// maxStreamBytes bounds what a single download will pull off the wire even
	// while skipping to an offset, so a huge file cannot tie up a tick.
	maxStreamBytes = 64 << 20 // 64 MiB

	// binarySniffBytes is how much of the head is examined to decide whether the
	// content is text.
	binarySniffBytes = 8000
)

// exportFormats lists the text conversions offered for each Google-native type,
// most useful first. The first entry is the default.
var exportFormats = map[string][]string{
	"application/vnd.google-apps.document":     {"text/markdown", "text/plain", "application/pdf"},
	"application/vnd.google-apps.spreadsheet":  {"text/csv", "application/pdf"},
	"application/vnd.google-apps.presentation": {"text/plain", "application/pdf"},
	"application/vnd.google-apps.script":       {"application/vnd.google-apps.script+json"},
	"application/vnd.google-apps.drawing":      {"image/svg+xml", "application/pdf"},
}

// nativePrefix marks a Google-native MIME type, which has no byte content.
const nativePrefix = "application/vnd.google-apps."

// FileContent is the outcome of reading a file.
type FileContent struct {
	File        DriveFile `json:"file"`
	Content     string    `json:"content"`
	ContentType string    `json:"content_type"` // the MIME type actually delivered
	Exported    bool      `json:"exported"`     // whether a Google-native file was converted
	ExportedAs  string    `json:"exported_as"`  // conversion target, when Exported
	BytesRead   int       `json:"bytes_read"`   // bytes of content returned
	OffsetBytes int       `json:"offset_bytes"` // where this window started
	Truncated   bool      `json:"truncated"`    // more content follows
	TotalBytes  int64     `json:"total_bytes"`  // file size when Drive reports one (0 for native files)
	IsBinary    bool      `json:"is_binary"`    // content is not text; Content is empty
	BinaryNote  string    `json:"binary_note"`  // why no text was returned
	WebViewLink string    `json:"web_view_link"`
}

// ReadFileOptions parameterises ReadFile.
type ReadFileOptions struct {
	// MaxBytes caps the content returned (defaults to defaultReadBytes, ceiling
	// maxReadBytes).
	MaxBytes int
	// OffsetBytes skips this many bytes, for paging through a large file.
	OffsetBytes int
	// ExportMIME forces the conversion target for a Google-native file (e.g.
	// "text/csv" for a Sheet). Ignored for uploaded files.
	ExportMIME string
}

// ReadFile streams a file's content and returns a bounded window of it as text.
//
// The target is scope-checked first, so a file outside what has been shared with
// the service account is refused before any bytes move. Google-native files are
// exported to text; genuinely binary content (a PDF, an image, an archive) is
// reported with its type and size rather than dumped as mojibake, because a
// model given 200 KB of decoded JPEG will hallucinate about it.
func (c *Client) ReadFile(ctx context.Context, fileID string, opts ReadFileOptions) (*FileContent, error) {
	if c == nil {
		return nil, fmt.Errorf("google client is not configured")
	}
	fileID = strings.TrimSpace(fileID)
	if err := c.withinScope(ctx, fileID); err != nil {
		return nil, err
	}

	meta, err := c.getFile(ctx, fileID)
	if err != nil {
		return nil, err
	}
	if meta.IsFolder() {
		return nil, fmt.Errorf("%s is a folder, not a file — list its contents with drive_find_file", meta.Name)
	}

	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultReadBytes
	}
	if maxBytes > maxReadBytes {
		maxBytes = maxReadBytes
	}
	offset := max(opts.OffsetBytes, 0)

	endpoint, exportAs, err := c.downloadURL(meta, opts.ExportMIME)
	if err != nil {
		return nil, err
	}

	out := &FileContent{
		File:        *meta,
		Exported:    exportAs != "",
		ExportedAs:  exportAs,
		OffsetBytes: offset,
		WebViewLink: meta.WebViewLink,
	}
	if n, perr := strconv.ParseInt(meta.Size, 10, 64); perr == nil {
		out.TotalBytes = n
	}

	// A Range header is the cheap way to skip ahead, and Drive honours it for
	// alt=media. Export conversions do not support it, so those are skipped by
	// reading and discarding.
	rangeHeader := ""
	skip := offset
	if offset > 0 && exportAs == "" {
		rangeHeader = fmt.Sprintf("bytes=%d-%d", offset, offset+maxBytes)
		skip = 0
	}

	body, contentType, err := c.stream(ctx, endpoint, rangeHeader)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	out.ContentType = contentType
	if out.ContentType == "" {
		out.ContentType = firstNonEmpty(exportAs, meta.MimeType)
	}

	reader := bufio.NewReader(io.LimitReader(body, maxStreamBytes))
	if skip > 0 {
		if _, err := io.CopyN(io.Discard, reader, int64(skip)); err != nil && err != io.EOF {
			return nil, fmt.Errorf("skipping to offset %d: %w", skip, err)
		}
	}

	// Read one byte past the cap so "there is more" is knowable without
	// buffering the remainder.
	buf, err := io.ReadAll(io.LimitReader(reader, int64(maxBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", meta.Name, err)
	}
	if len(buf) > maxBytes {
		buf = buf[:maxBytes]
		out.Truncated = true
	}
	// Drain whatever is left so the connection can be reused, but never more
	// than the stream cap already imposes.
	_, _ = io.Copy(io.Discard, reader)

	if isBinary(buf, out.ContentType) {
		out.IsBinary = true
		out.BinaryNote = binaryNote(meta, out.ContentType)
		out.BytesRead = 0
		return out, nil
	}

	text := string(buf)
	if out.Truncated {
		// A cut mid-rune would otherwise emit a replacement char at the seam.
		text = strings.ToValidUTF8(trimPartialRune(text), "")
	}
	out.Content = text
	out.BytesRead = len(text)
	return out, nil
}

// downloadURL picks the right endpoint for the file: an export conversion for a
// Google-native type, a raw media download for anything else. It returns the URL
// and the export target (empty when not exporting).
func (c *Client) downloadURL(meta *DriveFile, wantMIME string) (endpoint, exportAs string, err error) {
	esc := url.PathEscape(meta.ID)
	wantMIME = strings.TrimSpace(wantMIME)

	if !strings.HasPrefix(meta.MimeType, nativePrefix) {
		// Uploaded file: raw bytes. An export MIME is meaningless here, so it is
		// ignored rather than turned into an error the model has to unpick.
		q := url.Values{}
		q.Set("alt", "media")
		q.Set("supportsAllDrives", "true")
		return fmt.Sprintf("%s/files/%s?%s", driveBase, esc, q.Encode()), "", nil
	}

	formats := exportFormats[meta.MimeType]
	target := wantMIME
	if target == "" {
		if len(formats) == 0 {
			return "", "", fmt.Errorf("%q is a %s, which Drive cannot export to text; open it with the link instead",
				meta.Name, shortMime(meta.MimeType))
		}
		target = formats[0]
	} else if len(formats) > 0 && !containsStr(formats, target) {
		return "", "", fmt.Errorf("%q cannot be exported as %s; supported: %s",
			meta.Name, target, strings.Join(formats, ", "))
	}

	q := url.Values{}
	q.Set("mimeType", target)
	q.Set("supportsAllDrives", "true")
	return fmt.Sprintf("%s/files/%s/export?%s", driveBase, esc, q.Encode()), target, nil
}

// stream issues an authenticated GET and hands back the still-open body, so the
// caller reads incrementally instead of the whole file landing in memory.
//
// It deliberately does not go through apiRequest: that decodes a JSON body and
// closes it, which is exactly wrong for a download. The budget and the
// retry/backoff policy are shared, but a retry here has to re-issue the request
// rather than replay a buffer.
func (c *Client) stream(ctx context.Context, fullURL, rangeHeader string) (io.ReadCloser, string, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoff(attempt, 0)); err != nil {
				return nil, "", err
			}
		}
		if err := c.reads.wait(ctx); err != nil {
			return nil, "", err
		}
		token, err := c.token(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("acquiring google access token: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
		if err != nil {
			return nil, "", fmt.Errorf("building request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("User-Agent", userAgent)
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			lastErr = fmt.Errorf("google download failed: %w", err)
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp.Body, resp.Header.Get("Content-Type"), nil
		}

		// An error body is small; read it for the message, then decide.
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8000))
		_ = resp.Body.Close()
		if retryableStatus(resp.StatusCode) && attempt < maxRetries {
			lastErr = apiError(resp.StatusCode, errBody)
			logf("[google] HTTP %d downloading %s — retrying (attempt %d/%d)",
				resp.StatusCode, redactURL(fullURL), attempt+1, maxRetries)
			continue
		}
		return nil, "", apiError(resp.StatusCode, errBody)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("google download exhausted %d attempts", maxRetries+1)
	}
	return nil, "", lastErr
}

// ── text / binary handling ─────────────────────────────────────────────────

// textualTypes are content types treated as text regardless of what the byte
// sniff concludes.
var textualTypes = []string{
	"text/", "application/json", "application/xml", "application/csv",
	"application/x-ndjson", "application/yaml", "application/x-yaml",
	"application/javascript", "+json", "+xml", "+yaml",
}

// binaryTypes are content types never treated as text even if they sniff clean.
var binaryTypes = []string{
	"application/pdf", "image/", "audio/", "video/", "application/zip",
	"application/octet-stream", "application/vnd.openxmlformats",
	"application/vnd.ms-", "application/x-tar", "application/gzip",
	"application/vnd.oasis.opendocument",
}

// isBinary decides whether the payload should be handed to the model as text.
// The content type is trusted first; when it is unhelpful the head is sniffed
// for NUL bytes and invalid UTF-8, which is what actually distinguishes a CSV
// from a JPEG.
func isBinary(buf []byte, contentType string) bool {
	ct := strings.ToLower(contentType)
	for _, b := range binaryTypes {
		if strings.Contains(ct, b) {
			return true
		}
	}
	for _, t := range textualTypes {
		if strings.Contains(ct, t) {
			return false
		}
	}
	if len(buf) == 0 {
		return false
	}
	head := buf
	if len(head) > binarySniffBytes {
		head = head[:binarySniffBytes]
	}
	if bytesContainsZero(head) {
		return true
	}
	// Trim a trailing partial rune before validating, so a clean cut is not
	// mistaken for corruption.
	if !utf8.Valid([]byte(trimPartialRune(string(head)))) {
		return true
	}
	return false
}

func bytesContainsZero(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}

// trimPartialRune drops a trailing incomplete UTF-8 sequence.
func trimPartialRune(s string) string {
	for i := 0; i < utf8.UTFMax && i < len(s); i++ {
		trial := s[:len(s)-i]
		if r, size := utf8.DecodeLastRuneInString(trial); r != utf8.RuneError || size > 1 {
			return trial
		}
	}
	if len(s) < utf8.UTFMax {
		return ""
	}
	return s
}

// binaryNote explains why no text came back, and what to do instead.
func binaryNote(meta *DriveFile, contentType string) string {
	size := ""
	if n, err := strconv.ParseInt(meta.Size, 10, 64); err == nil && n > 0 {
		size = " (" + humanBytes(n) + ")"
	}
	base := fmt.Sprintf("%q is %s%s, which is binary — arbetern does not extract text from it",
		meta.Name, shortMime(firstNonEmpty(contentType, meta.MimeType)), size)
	switch {
	case strings.Contains(strings.ToLower(contentType), "pdf"):
		return base + ". If the same content exists as a Google Doc or a text/CSV file in the folder, read that instead; otherwise share the link with the user."
	case strings.Contains(strings.ToLower(contentType), "openxmlformats"):
		return base + ". Ask for it to be converted to a Google Sheet or Doc in Drive (Open with → Google Sheets/Docs), which can then be read as text."
	default:
		return base + ". Share the file link with the user rather than guessing at its contents."
	}
}

// humanBytes renders a byte count compactly.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ExportFormatsFor returns the text conversions available for a MIME type, for
// error messages and tool descriptions.
func ExportFormatsFor(mimeType string) []string { return exportFormats[mimeType] }
