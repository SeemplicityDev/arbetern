package google

// Provisioning surface: copying a file that is already in scope, so a new sheet
// starts life as an exact copy of a template instead of an empty grid somebody
// has to build by hand.
//
// Why a copy and not a fresh spreadsheet. A workflow that appends to these
// sheets reads each tab's header row to learn the column order, deliberately, so
// that inserting a column does not silently shift the data. The cost of that
// choice is that two sheets whose headers differ produce differently shaped data
// from the same writer and nothing errors. A copy cannot drift: tabs, headers
// and their order all come from one template, which is also where a person can
// see and edit them.
//
// Two guards make this safe to run on a schedule:
//
//   - Both ends are checked against the shared folders before the request is
//     sent: the source has to resolve inside them, and the destination has to be
//     one of them.
//   - A name that already exists in the destination is REFUSED, not copied.
//     Drive would otherwise create a second file with the same name, after which
//     resolving that name returns whichever copy comes back first. Refusing is
//     what makes a provisioning job idempotent — it can run daily and create
//     only what is missing.

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// ScopeDriveFull is the Drive scope files.copy requires.
const ScopeDriveFull = "https://www.googleapis.com/auth/drive"

// maxCopyMatches caps how many same-named candidates are fetched when resolving
// the source; enough to report the ambiguity, not enough to be a listing.
const maxCopyMatches = 10

// ErrNameTaken reports that the destination folder already holds a file of the
// requested name, so nothing was copied.
//
// It is a distinct type because it is the expected outcome of re-running a
// provisioning job, not a failure: the caller should read it as "already
// provisioned" and move on to the next account.
type ErrNameTaken struct {
	Name     string
	FolderID string
	FileID   string
}

func (e *ErrNameTaken) Error() string {
	return fmt.Sprintf("a file named %q already exists in the destination folder, so nothing was copied "+
		"(the existing file is %s). Drive would otherwise have created a second file with the same name, after which "+
		"resolving that name returns whichever copy comes back first. Treat this as already provisioned rather than retrying",
		e.Name, e.FileID)
}

// CanCopyFiles reports whether the configured scopes permit files.copy.
//
// The default Drive scope is read-only and copying needs write access to Drive
// itself: drive.file is not enough, because it only ever sees files this
// connector created, never a template somebody shared with it. Copying is
// therefore opt-in — a deployment that wants it sets GOOGLE_SCOPES with the full
// Drive scope, and the tool is not advertised until it does.
func (c *Client) CanCopyFiles() bool {
	if c == nil {
		return false
	}
	for _, s := range strings.Fields(c.scopes) {
		if s == ScopeDriveFull {
			return true
		}
	}
	return false
}

// CopyFileOptions parameterises CopyFile.
type CopyFileOptions struct {
	// SourceName is the exact name of the file to copy, resolved inside the
	// shared folders the way FindFiles resolves any other name.
	SourceName string
	// SourceFolderID narrows the source lookup to one folder, for when the same
	// template name exists in more than one.
	SourceFolderID string
	// NewName is the copy's name. It must not already exist in the destination.
	NewName string
	// FolderID is the destination folder. Empty means the single shared folder,
	// when exactly one is reachable.
	FolderID string
}

// CopyFileResult reports a completed copy. File.ID is what a provisioning
// caller needs next.
type CopyFileResult struct {
	File       DriveFile `json:"file"`
	Source     DriveFile `json:"source"`
	FolderID   string    `json:"folder_id"`
	FolderName string    `json:"folder_name"`
}

// CopyFile copies a file that is in scope into a folder that is in scope, under
// a name that is not already taken there.
func (c *Client) CopyFile(ctx context.Context, opts CopyFileOptions) (*CopyFileResult, error) {
	if c == nil {
		return nil, fmt.Errorf("google client is not configured")
	}
	sourceName := strings.TrimSpace(opts.SourceName)
	newName := strings.TrimSpace(opts.NewName)
	if sourceName == "" {
		return nil, fmt.Errorf("the name of the file to copy is required")
	}
	if newName == "" {
		return nil, fmt.Errorf("a name for the copy is required")
	}
	if !c.CanCopyFiles() {
		return nil, fmt.Errorf("this connector's Google credentials are read-only on Drive, so it cannot copy a file. " +
			"That is a deployment setting, not something to retry: report that copying needs the connector's Drive scope widened")
	}

	folder, err := c.destinationFolder(ctx, opts.FolderID)
	if err != nil {
		return nil, err
	}
	source, err := c.resolveCopySource(ctx, sourceName, opts.SourceFolderID)
	if err != nil {
		return nil, err
	}
	// Explicit even though the lookup only ever searched in-scope folders: this
	// is the same guard every write goes through, and its verdict is already
	// cached by the lookup, so it costs nothing.
	if err := c.withinScope(ctx, source.ID); err != nil {
		return nil, err
	}
	if err := c.refuseExistingName(ctx, newName, folder.ID); err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("fields", driveFileFields)
	q.Set("supportsAllDrives", "true")
	payload := map[string]any{
		"name":    newName,
		"parents": []string{folder.ID},
	}
	endpoint := driveBase + "/files/" + url.PathEscape(source.ID) + "/copy?" + q.Encode()

	var created DriveFile
	if err := c.apiRequest(ctx, "POST", endpoint, payload, &created, true); err != nil {
		return nil, err
	}
	// The copy landed in a folder that is in scope, so record the verdict rather
	// than making the first write to it walk the ancestry again.
	c.cacheScope(created.ID, true)

	return &CopyFileResult{
		File:       created,
		Source:     *source,
		FolderID:   folder.ID,
		FolderName: folder.Name,
	}, nil
}

// destinationFolder resolves where the copy should land: the folder the caller
// named, or the single shared folder when only one is reachable — the same
// implicit-folder rule the other tools follow.
func (c *Client) destinationFolder(ctx context.Context, folderID string) (*Root, error) {
	roots, err := c.Roots(ctx)
	if err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("%s", errNoSharesReason)
	}
	if id := strings.TrimSpace(folderID); id != "" {
		if err := c.folderInScope(ctx, id); err != nil {
			return nil, err
		}
		return &Root{ID: id, Name: c.rootName(roots, id), Kind: RootKindShared}, nil
	}
	if len(roots) == 1 {
		r := roots[0]
		return &r, nil
	}
	return nil, fmt.Errorf("%d folders are reachable, so there is no single obvious destination — "+
		"pass the folder_id of the one the copy belongs in (drive_list_folders names them)", len(roots))
}

// resolveCopySource resolves the template by name. Folders are included in the
// lookup only so that naming one produces a message that says why it cannot be
// copied instead of a bare "not found".
func (c *Client) resolveCopySource(ctx context.Context, name, folderID string) (*DriveFile, error) {
	found, err := c.FindFiles(ctx, FindFilesOptions{
		Names:          []string{name},
		FolderID:       folderID,
		Recursive:      true,
		IncludeFolders: true,
		Limit:          maxCopyMatches,
	})
	if err != nil {
		return nil, err
	}
	switch {
	case len(found.Files) == 0:
		return nil, fmt.Errorf("no file named %q was found in %s. The name is matched exactly, so confirm it with "+
			"drive_find_file before copying", name, describeRoots(found.Roots))
	case len(found.Files) > 1:
		ids := make([]string, 0, len(found.Files))
		for _, f := range found.Files {
			ids = append(ids, f.ID)
		}
		return nil, fmt.Errorf("%d files are named %q (%s), so it is not clear which one to copy — "+
			"pass source_folder_id to name the folder the template is in",
			len(found.Files), name, strings.Join(ids, ", "))
	}
	f := found.Files[0]
	if f.IsFolder() {
		return nil, fmt.Errorf("%q is a folder, and Drive cannot copy a folder — name the file to copy instead", name)
	}
	return &f, nil
}

// refuseExistingName fails when the destination folder already holds a file of
// that name. Only the destination's own level is searched: a same-named file in
// a sibling or child folder is a different file and no reason to refuse.
func (c *Client) refuseExistingName(ctx context.Context, name, folderID string) error {
	found, err := c.FindFiles(ctx, FindFilesOptions{
		Names:          []string{name},
		FolderID:       folderID,
		IncludeFolders: true,
		Limit:          1,
	})
	if err != nil {
		return err
	}
	if len(found.Files) == 0 {
		return nil
	}
	return &ErrNameTaken{Name: name, FolderID: folderID, FileID: found.Files[0].ID}
}
