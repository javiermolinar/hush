package sdk

import (
	"context"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	WorkspaceFilesVersion            = 1
	WorkspaceFileSearchVersion       = 2
	WorkspaceFileProjectPathsVersion = 3
	WorkspaceFileSearchLimit         = 32
	WorkspaceFileSearchQueryBytes    = 255
	workspaceFilesCapability         = "workspaceFiles"
	workspaceFilesPrefix             = "ext.workspaceFiles."
	workspaceFileSearchMethod        = workspaceFilesPrefix + "search"
	githubDirectory                  = ".github"
)

// WorkspaceFileScope describes the two independently authorized roots for a
// target. The host never returns their paths.
type WorkspaceFileScope struct{ Workspace, Home bool }

// WorkspaceFileTarget selects the workspace whose grants authorize a file
// request. Set exactly one field: an existing thread, or, with workspaceFiles:2
// or later, a prospective thread's absolute working directory (the CWD NewThread
// would record). Neither creates a thread. Send CWD only for a
// workspace the client shares with the backend host.
type WorkspaceFileTarget struct {
	ThreadID ID
	CWD      string
}

func (t WorkspaceFileTarget) valid() bool {
	if t.ThreadID != "" {
		return t.CWD == "" && t.ThreadID.Valid()
	}
	return filepath.IsAbs(t.CWD) && utf8.ValidString(t.CWD) && len(t.CWD) <= 4096 && strings.IndexFunc(t.CWD, unicode.IsControl) < 0
}

// params returns the target's wire fields and the versions that accept them.
func (t WorkspaceFileTarget) params() (map[string]any, []uint64) {
	if t.ThreadID != "" {
		return map[string]any{"threadId": t.ThreadID}, []uint64{WorkspaceFilesVersion, WorkspaceFileSearchVersion, WorkspaceFileProjectPathsVersion}
	}
	return map[string]any{"cwd": t.CWD}, []uint64{WorkspaceFileSearchVersion, WorkspaceFileProjectPathsVersion}
}

type WorkspaceFileEntry struct {
	Name      string `json:"name"`
	Directory bool   `json:"directory"`
}

type WorkspaceFilePage struct {
	Entries    []WorkspaceFileEntry `json:"entries"`
	NextCursor string               `json:"nextCursor,omitempty"`
}

// WorkspaceFileScope checks current browse grants without opening a listing.
func (c *Client) WorkspaceFileScope(ctx context.Context, target WorkspaceFileTarget) (WorkspaceFileScope, error) {
	if !target.valid() {
		return WorkspaceFileScope{}, ErrProtocol
	}
	var zero WorkspaceFileScope
	params, versions := target.params()
	out, err := waitOnCapability[struct {
		Workspace *bool `json:"workspace"`
		Home      *bool `json:"home"`
	}](ctx, c, workspaceFilesPrefix+"scope", params, workspaceFilesCapability, versions...)
	if err != nil {
		return zero, err
	}
	if out.Workspace == nil || out.Home == nil {
		return zero, ErrProtocol
	}
	return WorkspaceFileScope{*out.Workspace, *out.Home}, nil
}

func validBrowseName(s string, includeGitHub bool) bool {
	if includeGitHub && s == githubDirectory {
		return true
	}
	return s != "" && len(s) <= 255 && utf8.ValidString(s) && s[0] != '.' &&
		!strings.ContainsAny(s, "/\\\"@:") && strings.IndexFunc(s, unicode.IsControl) < 0
}

// ListWorkspaceFiles lists immediate children of a relative directory in the
// authorized workspace or backend home. Its passive connection closes on cancel.
// The opaque cursor may be reused only with the same request fields and boot.
func (c *Client) ListWorkspaceFiles(ctx context.Context, target WorkspaceFileTarget, root, directory string, limit int, cursor string) (WorkspaceFilePage, error) {
	var zero WorkspaceFilePage
	if !target.valid() || root != "workspace" && root != "home" || directory != "" && !validBrowsePath(directory, root == "workspace") ||
		limit < 1 || limit > 100 || cursor != "" && !directoryCursor.MatchString(cursor) {
		return zero, ErrProtocol
	}
	params, versions := target.params()
	if first, _, _ := strings.Cut(directory, "/"); first == githubDirectory {
		versions = []uint64{WorkspaceFileProjectPathsVersion}
	}
	params["root"], params["directory"], params["limit"] = root, directory, limit
	if cursor != "" {
		params["cursor"] = cursor
	}
	out, version, err := waitOnVersionedCapability[struct {
		Entries []struct {
			Name      string `json:"name"`
			Directory *bool  `json:"directory"`
		} `json:"entries"`
		NextCursor string `json:"nextCursor,omitempty"`
	}](ctx, c, workspaceFilesPrefix+"list", params, workspaceFilesCapability, versions...)
	if err != nil {
		return zero, err
	}
	if out.Entries == nil || len(out.Entries) > limit || out.NextCursor != "" &&
		(out.NextCursor == cursor || !directoryCursor.MatchString(out.NextCursor) || len(out.Entries) != limit) {
		return zero, ErrProtocol
	}
	result := WorkspaceFilePage{Entries: make([]WorkspaceFileEntry, 0, len(out.Entries)), NextCursor: out.NextCursor}
	seen := make(map[string]bool, len(out.Entries))
	includeGitHub := version == WorkspaceFileProjectPathsVersion && root == "workspace" && directory == ""
	for _, item := range out.Entries {
		if item.Directory == nil || !validBrowseName(item.Name, includeGitHub && *item.Directory) || seen[item.Name] {
			return zero, ErrProtocol
		}
		seen[item.Name] = true
		result.Entries = append(result.Entries, WorkspaceFileEntry{item.Name, *item.Directory})
	}
	return result, nil
}

func validBrowsePath(s string, includeGitHub bool) bool {
	if len(s) > 2048 || strings.Count(s, "/") >= 32 {
		return false
	}
	for component := range strings.SplitSeq(s, "/") {
		if !validBrowseName(component, includeGitHub) {
			return false
		}
		includeGitHub = false // only the first component may be .github
	}
	return true
}

type WorkspaceFileMatch struct {
	Path      string `json:"path"`
	Directory bool   `json:"directory"`
}

type WorkspaceFileSearchResult struct {
	Matches []WorkspaceFileMatch `json:"matches"`
	Partial bool                 `json:"partial"` // the server couldn't complete its bounded scan
}

// SearchWorkspaceFiles returns ranked paths below the target's authorized
// workspace. It requires workspaceFiles:2 or later and uses a cancelable passive connection.
func (c *Client) SearchWorkspaceFiles(ctx context.Context, target WorkspaceFileTarget, query string, limit int) (WorkspaceFileSearchResult, error) {
	var zero WorkspaceFileSearchResult
	if !target.valid() || len(query) > WorkspaceFileSearchQueryBytes || !utf8.ValidString(query) || strings.TrimSpace(query) == "" ||
		strings.ContainsAny(query, "/\\\"@:") || strings.IndexFunc(query, unicode.IsControl) >= 0 ||
		limit < 1 || limit > WorkspaceFileSearchLimit {
		return zero, ErrProtocol
	}
	params, _ := target.params()
	params["query"], params["limit"] = query, limit
	out, version, err := waitOnVersionedCapability[struct {
		Matches []struct {
			Path      string `json:"path"`
			Directory *bool  `json:"directory"`
		} `json:"matches"`
		Partial *bool `json:"partial"`
	}](ctx, c, workspaceFileSearchMethod, params, workspaceFilesCapability, WorkspaceFileSearchVersion, WorkspaceFileProjectPathsVersion)
	if err != nil {
		return zero, err
	}
	if out.Matches == nil || len(out.Matches) > limit || out.Partial == nil {
		return zero, ErrProtocol
	}
	result := WorkspaceFileSearchResult{Matches: make([]WorkspaceFileMatch, 0, len(out.Matches)), Partial: *out.Partial}
	seen := make(map[string]bool, len(out.Matches))
	includeGitHub := version == WorkspaceFileProjectPathsVersion
	for _, match := range out.Matches {
		if match.Directory == nil || !validBrowsePath(match.Path, includeGitHub) || match.Path == githubDirectory && !*match.Directory ||
			match.Path == "~" || strings.HasPrefix(match.Path, "~/") || seen[match.Path] {
			return zero, ErrProtocol
		}
		seen[match.Path] = true
		result.Matches = append(result.Matches, WorkspaceFileMatch{match.Path, *match.Directory})
	}
	return result, nil
}
