package google

// Drive surface: discovering what the service account can reach, resolving
// files by name inside it, and the in-code scope guard every Sheets and
// download call runs through first.
//
// Scope model
//
// The service account has no Drive of its own — it can only see what somebody
// explicitly shared with it. That share IS the boundary, and by default this
// package simply adopts it: at startup it asks Drive "what has been shared with
// me?" and treats those folders and shared drives (plus their subtrees) as the
// working set. Nothing else exists as far as the connector is concerned.
//
// Two consequences worth being precise about:
//
//   - With discovery (the default), the in-code guard is NOT an extra boundary
//     beyond the Drive ACL — it mirrors it. Its value is that it fails fast with
//     an actionable message instead of a bare 404, and that it gives the model a
//     definite answer to "which folders can I use".
//   - With pinned folder IDs configured, the guard IS an extra boundary: the
//     connector refuses anything outside those folders even when the service
//     account was separately granted access to it. Use pins when the account's
//     shares are broader than what this agent should touch.
//
// When exactly one root is shared, tools resolve it implicitly — the model never
// has to name a folder. When several are shared, drive_list_folders enumerates
// them and searches span all of them unless one is named.

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// MimeSpreadsheet is the Drive MIME type of a Google Sheets file.
const MimeSpreadsheet = "application/vnd.google-apps.spreadsheet"

// mimeFolder is the Drive MIME type of a folder.
const mimeFolder = "application/vnd.google-apps.folder"

// maxFindResults caps how many files one drive_find_file call returns.
const maxFindResults = 200

// maxFolderTreeDepth bounds how deep a root's subtree is walked when resolving
// descendants. A shared reporting folder is a handful of levels at most; the cap
// stops a pathological tree from turning one tool call into a crawl.
const maxFolderTreeDepth = 6

// maxRoots caps how many shared folders / drives are adopted, so an account that
// has accumulated a large number of shares cannot make every scope check
// expensive.
const maxRoots = 50

// parentsChunk bounds how many parent IDs go into one Drive query. Drive limits
// query length, so a wide subtree is searched in several chunked requests rather
// than one that fails.
const parentsChunk = 40

// errNoSharesReason is the model-facing phrasing for "no folder has been shared
// with this connector". It points at the operator surface instead of printing the
// service-account address into a channel.
const errNoSharesReason = "no Google Drive folder has been shared with this connector yet, so there is nothing to read. " +
	"This is a deployment gap, not a missing file: an administrator needs to share a folder with the connector's service account " +
	"(the address is on the arbetern integrations page). Report it as a configuration gap rather than retrying"

// RootKind describes how a root became reachable.
const (
	RootKindShared      = "shared folder"
	RootKindSharedDrive = "shared drive"
	RootKindPinned      = "pinned folder"
)

// Root is a top-level Drive location the service account can reach: a folder
// somebody shared with it, a shared drive it is a member of, or a folder that
// was pinned by configuration.
type Root struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// DriveFile is a file the connector can reach.
type DriveFile struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	MimeType     string   `json:"mimeType"`
	ModifiedTime string   `json:"modifiedTime"`
	Size         string   `json:"size"` // absent for Google-native files, which have no byte size
	WebViewLink  string   `json:"webViewLink"`
	Parents      []string `json:"parents"`
	// SharedWithMeTime is set when the file was shared DIRECTLY with the service
	// account (as opposed to being reachable through a shared folder). Note that
	// `sharedWithMe` is a query term only — it is not a field on the File
	// resource, and asking for it in a field selector is a 400.
	SharedWithMeTime string `json:"sharedWithMeTime"`
}

// IsSharedWithMe reports whether the file was shared directly with the service
// account rather than inherited through a shared folder.
func (f DriveFile) IsSharedWithMe() bool { return strings.TrimSpace(f.SharedWithMeTime) != "" }

// IsSpreadsheet reports whether the file is a Google Sheet (as opposed to an
// uploaded .xlsx or some other file that happens to match the name).
func (f DriveFile) IsSpreadsheet() bool { return f.MimeType == MimeSpreadsheet }

// IsFolder reports whether the file is a Drive folder.
func (f DriveFile) IsFolder() bool { return f.MimeType == mimeFolder }

// driveFileFields is the field selector used everywhere a file is fetched, kept
// in one place so metadata is uniform across list, get and download paths.
//
// Every entry must be a real field on the Drive v3 File resource. Query TERMS
// are not fields: `sharedWithMe` is legal inside `q` but selecting it returns
// "Invalid field selection sharedWithMe" and fails the whole request — the
// corresponding field is `sharedWithMeTime`. Same trap applies to `owners`
// vs `ownedByMe` and to any other q-only predicate.
const driveFileFields = "id,name,mimeType,modifiedTime,size,webViewLink,parents,sharedWithMeTime"

// ── Root discovery ─────────────────────────────────────────────────────────

// Roots returns the top-level locations the connector can reach, cached for
// rootsCacheTTL. When folder IDs were pinned by configuration those are the
// roots; otherwise Drive is asked what has been shared with the service
// account.
func (c *Client) Roots(ctx context.Context) ([]Root, error) {
	if c == nil {
		return nil, fmt.Errorf("google client is not configured")
	}
	c.rootsMu.Lock()
	if c.roots != nil && time.Since(c.rootsFetched) < rootsCacheTTL {
		out := append([]Root(nil), c.roots...)
		c.rootsMu.Unlock()
		return out, nil
	}
	c.rootsMu.Unlock()

	roots, err := c.discoverRoots(ctx)
	if err != nil {
		// Serve a stale snapshot rather than failing a tick over a transient
		// Drive hiccup — the set of shares changes on human timescales.
		c.rootsMu.Lock()
		stale := append([]Root(nil), c.roots...)
		c.rootsMu.Unlock()
		if len(stale) > 0 {
			return stale, nil
		}
		return nil, err
	}

	c.rootsMu.Lock()
	c.roots = roots
	c.rootsFetched = time.Now()
	// A changed root set invalidates every cached subtree and scope verdict.
	c.treeMu.Lock()
	c.folderTree = nil
	c.treeMu.Unlock()
	c.rootsMu.Unlock()
	return append([]Root(nil), roots...), nil
}

// discoverRoots resolves the root set from configuration or from Drive.
func (c *Client) discoverRoots(ctx context.Context) ([]Root, error) {
	if len(c.pinnedFolders) > 0 {
		var roots []Root
		for _, id := range c.pinnedFolders {
			f, err := c.getFile(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("a pinned folder is not reachable — it may not be shared with this connector: %w", err)
			}
			if !f.IsFolder() {
				return nil, fmt.Errorf("pinned id %s is a %s, not a folder", id, f.MimeType)
			}
			roots = append(roots, Root{ID: f.ID, Name: f.Name, Kind: RootKindPinned})
		}
		return roots, nil
	}

	var roots []Root
	seen := map[string]bool{}

	// Folders shared directly with the service account.
	q := url.Values{}
	q.Set("q", fmt.Sprintf("sharedWithMe = true and trashed = false and mimeType = %s", quoteDriveValue(mimeFolder)))
	q.Set("fields", "files(id,name)")
	q.Set("pageSize", fmt.Sprintf("%d", maxRoots))
	q.Set("orderBy", "name")
	q.Set("supportsAllDrives", "true")
	q.Set("includeItemsFromAllDrives", "true")

	var shared struct {
		Files []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"files"`
	}
	if err := c.apiRequest(ctx, "GET", driveBase+"/files?"+q.Encode(), nil, &shared, false); err != nil {
		return nil, fmt.Errorf("listing the folders shared with this connector: %w", err)
	}
	for _, f := range shared.Files {
		if f.ID == "" || seen[f.ID] {
			continue
		}
		seen[f.ID] = true
		roots = append(roots, Root{ID: f.ID, Name: f.Name, Kind: RootKindShared})
	}

	// Shared drives the account is a member of. A shared drive's ID doubles as
	// the ID of its top-level folder, so it slots into the same tree walk.
	dq := url.Values{}
	dq.Set("pageSize", "100")
	dq.Set("fields", "drives(id,name)")
	var drives struct {
		Drives []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"drives"`
	}
	if err := c.apiRequest(ctx, "GET", driveBase+"/drives?"+dq.Encode(), nil, &drives, false); err != nil {
		// Not fatal: a deployment with no shared drives, or a scope that omits
		// them, must still work off plain shared folders.
		logf("[google] could not list shared drives (continuing with %d shared folder(s)): %v", len(roots), err)
	} else {
		for _, d := range drives.Drives {
			if d.ID == "" || seen[d.ID] {
				continue
			}
			seen[d.ID] = true
			roots = append(roots, Root{ID: d.ID, Name: d.Name, Kind: RootKindSharedDrive})
		}
	}

	sort.SliceStable(roots, func(i, j int) bool { return roots[i].Name < roots[j].Name })
	if len(roots) > maxRoots {
		roots = roots[:maxRoots]
	}
	return roots, nil
}

// DefaultRoot returns the single root when exactly one is reachable, so callers
// can act without asking which folder to use. It returns nil when there are
// none or several.
func (c *Client) DefaultRoot(ctx context.Context) *Root {
	roots, err := c.Roots(ctx)
	if err != nil || len(roots) != 1 {
		return nil
	}
	r := roots[0]
	return &r
}

// scopeTree returns every folder ID inside scope: each root plus its
// descendants, cached for rootsCacheTTL.
func (c *Client) scopeTree(ctx context.Context) (map[string]bool, error) {
	c.treeMu.Lock()
	if c.folderTree != nil && time.Since(c.treeFetched) < rootsCacheTTL {
		out := c.folderTree
		c.treeMu.Unlock()
		return out, nil
	}
	c.treeMu.Unlock()

	roots, err := c.Roots(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(roots))
	var frontier []string
	for _, r := range roots {
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		frontier = append(frontier, r.ID)
	}

	for depth := 0; depth < maxFolderTreeDepth && len(frontier) > 0; depth++ {
		next, err := c.childFolders(ctx, frontier, seen)
		if err != nil {
			return nil, fmt.Errorf("walking the shared folder tree: %w", err)
		}
		frontier = next
	}

	c.treeMu.Lock()
	c.folderTree = seen
	c.treeFetched = time.Now()
	c.treeMu.Unlock()
	return seen, nil
}

// childFolders lists the immediate subfolders of the given parents, recording
// them in seen and returning the newly discovered ones. Parents are queried in
// chunks so a wide tree does not build a query Drive rejects for length.
func (c *Client) childFolders(ctx context.Context, parents []string, seen map[string]bool) ([]string, error) {
	var next []string
	for _, chunk := range chunkStrings(parents, parentsChunk) {
		q := url.Values{}
		q.Set("q", fmt.Sprintf("trashed = false and mimeType = %s and %s",
			quoteDriveValue(mimeFolder), parentsClause(chunk)))
		q.Set("fields", "files(id)")
		q.Set("pageSize", "500")
		q.Set("supportsAllDrives", "true")
		q.Set("includeItemsFromAllDrives", "true")

		var resp struct {
			Files []struct {
				ID string `json:"id"`
			} `json:"files"`
		}
		if err := c.apiRequest(ctx, "GET", driveBase+"/files?"+q.Encode(), nil, &resp, false); err != nil {
			return nil, err
		}
		for _, f := range resp.Files {
			if f.ID == "" || seen[f.ID] {
				continue
			}
			seen[f.ID] = true
			next = append(next, f.ID)
		}
	}
	return next, nil
}

// chunkStrings splits s into slices of at most n elements.
func chunkStrings(s []string, n int) [][]string {
	if n <= 0 || len(s) <= n {
		return [][]string{s}
	}
	var out [][]string
	for i := 0; i < len(s); i += n {
		out = append(out, s[i:min(i+n, len(s))])
	}
	return out
}

// ── Finding files ──────────────────────────────────────────────────────────

// FindFilesResult is the outcome of a drive_find_file call.
type FindFilesResult struct {
	Files     []DriveFile `json:"files"`
	Roots     []Root      `json:"roots"`     // the folders that were searched
	Query     string      `json:"query"`     // human-readable description of what was searched for
	Recursive bool        `json:"recursive"` // whether subfolders were included
	Truncated bool        `json:"truncated"` // more matches existed than were returned
	Folders   int         `json:"folders"`   // number of folders the query covered
	Pinned    bool        `json:"pinned"`    // whether the root set came from configuration
}

// FindFilesOptions parameterises FindFiles.
type FindFilesOptions struct {
	// Names are exact file names to resolve. Batched into a single Drive query.
	Names []string
	// NameContains is a substring match, used when Names is empty.
	NameContains string
	// FolderID restricts the search to one root (or any folder in scope) instead
	// of every reachable root.
	FolderID string
	// MimeType restricts results to one MIME type (e.g. MimeSpreadsheet).
	MimeType string
	// SpreadsheetsOnly is shorthand for MimeType = MimeSpreadsheet.
	SpreadsheetsOnly bool
	// Recursive includes descendant folders.
	Recursive bool
	// IncludeFolders keeps folders in the results (they are excluded by default,
	// since a folder is rarely what a "find the file" call is after).
	IncludeFolders bool
	// Limit caps the returned files (defaults to maxFindResults).
	Limit int
}

// FindFiles resolves files by name across everything the connector can reach.
// Every name in opts.Names is resolved by ONE Drive query per parent chunk
// rather than one query per name, so a workflow tick that needs 15 spreadsheets
// spends a single request in the common case.
func (c *Client) FindFiles(ctx context.Context, opts FindFilesOptions) (*FindFilesResult, error) {
	if c == nil {
		return nil, fmt.Errorf("google client is not configured")
	}
	limit := opts.Limit
	if limit <= 0 || limit > maxFindResults {
		limit = maxFindResults
	}

	roots, err := c.Roots(ctx)
	if err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("%s", errNoSharesReason)
	}

	// Decide which folders to search.
	var parents []string
	searchRoots := roots
	if id := strings.TrimSpace(opts.FolderID); id != "" {
		if err := c.folderInScope(ctx, id); err != nil {
			return nil, err
		}
		searchRoots = []Root{{ID: id, Name: c.rootName(roots, id), Kind: RootKindShared}}
		if opts.Recursive {
			parents, err = c.subtreeOf(ctx, id)
			if err != nil {
				return nil, err
			}
		} else {
			parents = []string{id}
		}
	} else if opts.Recursive {
		tree, err := c.scopeTree(ctx)
		if err != nil {
			return nil, err
		}
		parents = make([]string, 0, len(tree))
		for f := range tree {
			parents = append(parents, f)
		}
	} else {
		for _, r := range roots {
			parents = append(parents, r.ID)
		}
	}

	// Build the name predicate once; it is reused across parent chunks.
	var namePred, describe string
	switch {
	case len(opts.Names) > 0:
		var ors, shown []string
		for _, n := range opts.Names {
			if n = strings.TrimSpace(n); n == "" {
				continue
			}
			ors = append(ors, fmt.Sprintf("name = %s", quoteDriveValue(n)))
			shown = append(shown, n)
		}
		if len(ors) == 0 {
			return nil, fmt.Errorf("no non-empty file name was supplied")
		}
		namePred = "(" + strings.Join(ors, " or ") + ")"
		describe = "name in [" + strings.Join(shown, ", ") + "]"
	case strings.TrimSpace(opts.NameContains) != "":
		term := strings.TrimSpace(opts.NameContains)
		namePred = fmt.Sprintf("name contains %s", quoteDriveValue(term))
		describe = "name contains " + term
	default:
		describe = "all files"
	}

	mime := strings.TrimSpace(opts.MimeType)
	if mime == "" && opts.SpreadsheetsOnly {
		mime = MimeSpreadsheet
	}
	if mime != "" {
		describe += ", type " + shortMime(mime)
	}

	out := &FindFilesResult{
		Roots:     searchRoots,
		Query:     describe,
		Recursive: opts.Recursive,
		Folders:   len(parents),
		Pinned:    len(c.pinnedFolders) > 0,
	}

	seenFile := map[string]bool{}
	for _, chunk := range chunkStrings(parents, parentsChunk) {
		if len(out.Files) > limit {
			break
		}
		clauses := []string{"trashed = false", parentsClause(chunk)}
		if namePred != "" {
			clauses = append(clauses, namePred)
		}
		if mime != "" {
			clauses = append(clauses, fmt.Sprintf("mimeType = %s", quoteDriveValue(mime)))
		} else if !opts.IncludeFolders {
			clauses = append(clauses, fmt.Sprintf("mimeType != %s", quoteDriveValue(mimeFolder)))
		}

		q := url.Values{}
		q.Set("q", strings.Join(clauses, " and "))
		q.Set("fields", "files("+driveFileFields+")")
		q.Set("pageSize", fmt.Sprintf("%d", min(limit+1, 1000)))
		q.Set("orderBy", "name")
		// Shared drives are opted into explicitly; without these the query
		// silently misses a folder that lives on a shared drive.
		q.Set("supportsAllDrives", "true")
		q.Set("includeItemsFromAllDrives", "true")

		var resp struct {
			Files []DriveFile `json:"files"`
		}
		if err := c.apiRequest(ctx, "GET", driveBase+"/files?"+q.Encode(), nil, &resp, false); err != nil {
			return nil, err
		}
		for _, f := range resp.Files {
			if seenFile[f.ID] {
				continue
			}
			seenFile[f.ID] = true
			out.Files = append(out.Files, f)
		}
	}

	sort.SliceStable(out.Files, func(i, j int) bool { return out.Files[i].Name < out.Files[j].Name })
	if len(out.Files) > limit {
		out.Files = out.Files[:limit]
		out.Truncated = true
	}
	// Every hit came back from a parents-constrained query, so cache the
	// in-scope verdict — a find-then-append flow then costs no extra lookup.
	for _, f := range out.Files {
		c.cacheScope(f.ID, true)
	}
	return out, nil
}

// rootName returns a known root's display name, or the raw ID.
func (c *Client) rootName(roots []Root, id string) string {
	for _, r := range roots {
		if r.ID == id {
			return r.Name
		}
	}
	return id
}

// subtreeOf returns a folder plus its descendant folder IDs.
func (c *Client) subtreeOf(ctx context.Context, folderID string) ([]string, error) {
	seen := map[string]bool{folderID: true}
	frontier := []string{folderID}
	for depth := 0; depth < maxFolderTreeDepth && len(frontier) > 0; depth++ {
		next, err := c.childFolders(ctx, frontier, seen)
		if err != nil {
			return nil, err
		}
		frontier = next
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out, nil
}

// parentsClause renders the Drive `in parents` disjunction for a folder set.
func parentsClause(parents []string) string {
	ors := make([]string, 0, len(parents))
	for _, p := range parents {
		ors = append(ors, fmt.Sprintf("%s in parents", quoteDriveValue(p)))
	}
	return "(" + strings.Join(ors, " or ") + ")"
}

// quoteDriveValue renders a value as a Drive query string literal, escaping the
// backslashes and single quotes that would otherwise break out of it.
func quoteDriveValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `'`, `\'`)
	return "'" + v + "'"
}

// getFile fetches one file's metadata, including its parents.
func (c *Client) getFile(ctx context.Context, fileID string) (*DriveFile, error) {
	q := url.Values{}
	q.Set("fields", driveFileFields)
	q.Set("supportsAllDrives", "true")

	var f DriveFile
	path := driveBase + "/files/" + url.PathEscape(fileID) + "?" + q.Encode()
	if err := c.apiRequest(ctx, "GET", path, nil, &f, false); err != nil {
		return nil, err
	}
	return &f, nil
}

// ── Scope enforcement ──────────────────────────────────────────────────────

// ErrOutOfScope describes a target the connector will not touch.
//
// Reason is phrased for the model and therefore for a Slack channel: it never
// names the service-account address or the GCP project. Those identify internal
// infrastructure, are of no use to whoever asked the question, and travel further
// than the channel once a reply is screenshotted. Operators get them from the
// boot log and the integrations page instead.
type ErrOutOfScope struct {
	FileID string
	Reason string
}

func (e *ErrOutOfScope) Error() string {
	return fmt.Sprintf("that file is out of scope for this connector: %s. "+
		"Resolve the file with drive_find_file, or list what is reachable with drive_list_folders, and use only the IDs those return",
		e.Reason)
}

// withinScope returns nil when the connector may touch fileID, and an
// *ErrOutOfScope otherwise. Verdicts are cached for parentsCacheTTL so a batch
// write does not re-walk the ancestry of every target.
//
// A file qualifies when any ancestor is a root (or a folder inside a root's
// subtree). When no folders are pinned, a file shared DIRECTLY with the service
// account also qualifies even though it sits in no shared folder — that share is
// an explicit grant, and refusing it would mean a spreadsheet someone shared
// one-to-one could never be used. Pinning folders turns that off: pins are the
// way to say "only these folders, regardless of what else was shared".
func (c *Client) withinScope(ctx context.Context, fileID string) error {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return fmt.Errorf("a file id is required")
	}

	c.parentsMu.Lock()
	entry, ok := c.parentsCache[fileID]
	c.parentsMu.Unlock()
	if ok && time.Since(entry.fetchedAt) < parentsCacheTTL {
		if entry.inScope {
			return nil
		}
		return &ErrOutOfScope{FileID: fileID, Reason: entry.reason}
	}

	tree, err := c.scopeTree(ctx)
	if err != nil {
		return err
	}
	if len(tree) == 0 {
		return &ErrOutOfScope{FileID: fileID, Reason: errNoSharesReason}
	}
	if tree[fileID] {
		// The target is itself a folder in scope — fine for a listing, but not a
		// file to read or write.
		return &ErrOutOfScope{FileID: fileID, Reason: "this ID is a folder, not a file"}
	}

	// Walk up from the file: a file may sit several levels below a shared
	// folder, and Drive only reports direct parents.
	cur := fileID
	pinned := len(c.pinnedFolders) > 0
	for hop := 0; hop <= maxFolderTreeDepth; hop++ {
		f, err := c.getFile(ctx, cur)
		if err != nil {
			// A 403/404 here means the account cannot see the file at all,
			// which is itself a definitive "out of scope" — but surface
			// Google's own message, it is more actionable.
			return err
		}
		for _, p := range f.Parents {
			if tree[p] {
				c.cacheScope(fileID, true)
				return nil
			}
		}
		if hop == 0 && !pinned && f.IsSharedWithMe() {
			// Shared one-to-one rather than via a folder.
			c.cacheScope(fileID, true)
			return nil
		}
		if len(f.Parents) == 0 {
			break
		}
		cur = f.Parents[0]
	}

	reason := "it is not inside any folder that has been shared with this connector"
	if pinned {
		reason = "it is not inside any of the folders this deployment is pinned to"
	}
	c.cacheScopeReason(fileID, false, reason)
	return &ErrOutOfScope{FileID: fileID, Reason: reason}
}

// folderInScope validates a folder ID the caller supplied as a search root.
func (c *Client) folderInScope(ctx context.Context, folderID string) error {
	tree, err := c.scopeTree(ctx)
	if err != nil {
		return err
	}
	if tree[folderID] {
		return nil
	}
	reason := "it is not a folder that has been shared with this connector"
	if len(c.pinnedFolders) > 0 {
		reason = "it is not one of the folders this deployment is pinned to"
	}
	return &ErrOutOfScope{FileID: folderID, Reason: reason}
}

// cacheScope records an in-scope verdict for fileID.
func (c *Client) cacheScope(fileID string, inScope bool) {
	c.cacheScopeReason(fileID, inScope, "")
}

func (c *Client) cacheScopeReason(fileID string, inScope bool, reason string) {
	c.parentsMu.Lock()
	c.parentsCache[fileID] = parentsEntry{inScope: inScope, reason: reason, fetchedAt: time.Now()}
	c.parentsMu.Unlock()
}

// VerifyAccess resolves the reachable roots once, so a deployment where nothing
// was shared with the service account says so at boot rather than surfacing as a
// puzzling empty search on the first workflow tick. Returns the roots it found.
func (c *Client) VerifyAccess(ctx context.Context) ([]Root, error) {
	if c == nil {
		return nil, fmt.Errorf("google client is not configured")
	}
	roots, err := c.Roots(ctx)
	if err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		// Operator-facing only (boot log / integrations panel), so naming the
		// address is right here — it is the actionable part for whoever deploys.
		return nil, fmt.Errorf("nothing is shared with %s — share a Drive folder with that address (Editor to allow appends)", c.ServiceAccountEmail())
	}
	return roots, nil
}
