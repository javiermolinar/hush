package sdk

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	WorkspaceFilesVersion    = 1
	workspaceFilesCapability = "workspaceFiles"
	workspaceFilesPrefix     = "ext.workspaceFiles."
)

// WorkspaceFileScope describes the two independently authorized roots for a
// persisted thread. Neither path is sent by the client.
type WorkspaceFileScope struct{ Workspace, Home bool }

type WorkspaceFileEntry struct {
	Name      string `json:"name"`
	Directory bool   `json:"directory"`
}

type WorkspaceFilePage struct {
	Entries    []WorkspaceFileEntry `json:"entries"`
	NextCursor string               `json:"nextCursor,omitempty"`
}

// WorkspaceFileScope checks current browse grants without opening a listing.
func (c *Client) WorkspaceFileScope(ctx context.Context, thread ID) (WorkspaceFileScope, error) {
	if !thread.Valid() {
		return WorkspaceFileScope{}, ErrProtocol
	}
	var zero WorkspaceFileScope
	out, err := waitOnCapability[struct {
		Workspace *bool `json:"workspace"`
		Home      *bool `json:"home"`
	}](ctx, c, workspaceFilesPrefix+"scope", map[string]ID{"threadId": thread}, workspaceFilesCapability, WorkspaceFilesVersion)
	if err != nil {
		return zero, err
	}
	if out.Workspace == nil || out.Home == nil {
		return zero, ErrProtocol
	}
	return WorkspaceFileScope{*out.Workspace, *out.Home}, nil
}

func validBrowseName(s string) bool {
	return s != "" && len(s) <= 255 && utf8.ValidString(s) && s[0] != '.' &&
		!strings.ContainsAny(s, "/\\\"@:") && strings.IndexFunc(s, unicode.IsControl) < 0
}

// ListWorkspaceFiles lists immediate children of a relative directory in the
// authorized workspace or backend home. Its passive connection closes on cancel.
// The opaque cursor may be reused only with the same request fields and boot.
func (c *Client) ListWorkspaceFiles(ctx context.Context, thread ID, root, directory string, limit int, cursor string) (WorkspaceFilePage, error) {
	var zero WorkspaceFilePage
	if !thread.Valid() || root != "workspace" && root != "home" || len(directory) > 2048 ||
		strings.Count(directory, "/") >= 32 || limit < 1 || limit > 100 || cursor != "" && !directoryCursor.MatchString(cursor) {
		return zero, ErrProtocol
	}
	if directory != "" {
		for name := range strings.SplitSeq(directory, "/") {
			if !validBrowseName(name) {
				return zero, ErrProtocol
			}
		}
	}
	params := map[string]any{"threadId": thread, "root": root, "directory": directory, "limit": limit}
	if cursor != "" {
		params["cursor"] = cursor
	}
	out, err := waitOnCapability[struct {
		Entries []struct {
			Name      string `json:"name"`
			Directory *bool  `json:"directory"`
		} `json:"entries"`
		NextCursor string `json:"nextCursor,omitempty"`
	}](ctx, c, workspaceFilesPrefix+"list", params, workspaceFilesCapability, WorkspaceFilesVersion)
	if err != nil {
		return zero, err
	}
	if out.Entries == nil || len(out.Entries) > limit || out.NextCursor != "" &&
		(out.NextCursor == cursor || !directoryCursor.MatchString(out.NextCursor) || len(out.Entries) != limit) {
		return zero, ErrProtocol
	}
	result := WorkspaceFilePage{Entries: make([]WorkspaceFileEntry, 0, len(out.Entries)), NextCursor: out.NextCursor}
	seen := make(map[string]bool, len(out.Entries))
	for _, item := range out.Entries {
		if item.Directory == nil || !validBrowseName(item.Name) || seen[item.Name] {
			return zero, ErrProtocol
		}
		seen[item.Name] = true
		result.Entries = append(result.Entries, WorkspaceFileEntry{item.Name, *item.Directory})
	}
	return result, nil
}
