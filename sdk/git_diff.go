package sdk

import (
	"context"
	"errors"
	"path"
	"strings"
)

const (
	GitDiffVersion         = 1
	GitDiffWatchVersion    = 1
	GitDiffHead            = "head"
	GitDiffCheckpoint      = "checkpoint"
	gitDiffCapability      = "gitDiff"
	gitDiffWatchCapability = "gitDiffWatch"
	gitDiffWaitMethod      = "ext.gitDiffWatch.wait"
	gitDiffPrefix          = "ext.gitDiff."
	gitDiffFileLimit       = 128
	gitDiffLineLimit       = 1500
)

// GitDiffNeedsTrust reports the gitDiff:1 workspace-trust denial, not a
// generic invalid request or another Git error.
func GitDiffNeedsTrust(err error) bool {
	if err == nil {
		return false
	}
	var wire *ServerError
	return errors.As(err, &wire) && wire.Code == "invalid_request" &&
		wire.Message == "Workspace is not trusted by Hush. Run 'hush trust grant --cwd PATH' on the backend host."
}

// GitDiffSummary describes one live, read-only comparison on the backend.
// Revision addresses its file list; it does not pin the worktree contents.
type GitDiffSummary struct {
	Revision   string        `json:"revision"`
	Range      string        `json:"range"`
	Commit     string        `json:"commit"`
	Checkpoint bool          `json:"checkpoint"`
	Branch     string        `json:"branch,omitempty"` // local branch; empty for detached HEAD or unavailable metadata
	Files      []GitDiffFile `json:"files"`
}

type GitDiffFile struct {
	Path    string `json:"path"`
	Status  string `json:"status"`
	Added   *int64 `json:"added,omitempty"`
	Removed *int64 `json:"removed,omitempty"`
	Staged  bool   `json:"staged"`
	Text    bool   `json:"text"`
}

type GitDiffLine struct {
	Kind    string `json:"kind"`
	OldLine int    `json:"oldLine,omitempty"`
	NewLine int    `json:"newLine,omitempty"`
	Text    string `json:"text"`
}

type GitDiffPatch struct {
	Revision  string        `json:"revision"`
	File      int           `json:"file"`
	Lines     []GitDiffLine `json:"lines"`
	Truncated bool          `json:"truncated"`
}

func (f GitDiffFile) valid() bool {
	if f.Path == "" || len(f.Path) > 1024 || path.IsAbs(f.Path) || strings.ContainsRune(f.Path, 0) {
		return false
	}
	for part := range strings.SplitSeq(f.Path, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
	}
	if len(f.Status) != 1 || !strings.Contains("ADMTU?", f.Status) || f.Status == "?" && f.Staged {
		return false
	}
	const maxSafeInteger = 1<<53 - 1
	return f.Added == nil && f.Removed == nil || f.Text && f.Added != nil && f.Removed != nil &&
		*f.Added >= 0 && *f.Added <= maxSafeInteger && *f.Removed >= 0 && *f.Removed <= maxSafeInteger
}

func (l GitDiffLine) valid() bool {
	if len(l.Text) > 6<<10 || strings.ContainsRune(l.Text, '\n') || l.OldLine < 0 || l.NewLine < 0 {
		return false
	}
	switch l.Kind {
	case "context":
		return l.OldLine > 0 && l.NewLine > 0
	case "add":
		return l.OldLine == 0 && l.NewLine > 0
	case "remove":
		return l.OldLine > 0 && l.NewLine == 0
	case "hunk", "note":
		return l.OldLine == 0 && l.NewLine == 0
	}
	return false
}

// GitDiffSummary compares committed HEAD or the latest file checkpoint with
// the thread's current worktree. Offer gitDiff:1 in ClientOptions to use it.
func (c *Client) GitDiffSummary(ctx context.Context, thread ID, baseline string) (GitDiffSummary, error) {
	if !thread.Valid() || baseline != GitDiffHead && baseline != GitDiffCheckpoint {
		return GitDiffSummary{}, ErrProtocol
	}
	// The lease is exclusive and cancellation closes it, leaving the
	// command connection and other metadata requests alone.
	out, err := metadataRead[GitDiffSummary](ctx, c, gitDiffPrefix+"summary",
		map[string]any{"threadId": thread, "range": baseline}, gitDiffCapability, GitDiffVersion)
	if err != nil {
		return GitDiffSummary{}, err
	}
	if !out.valid(baseline) {
		return GitDiffSummary{}, ErrProtocol
	}
	return out, nil
}

func (s GitDiffSummary) valid(baseline string) bool {
	if !scheduleID.MatchString(s.Revision) || s.Range != baseline || !gitCommit.MatchString(s.Commit) ||
		baseline == GitDiffCheckpoint && !s.Checkpoint || s.Files == nil || len(s.Files) > gitDiffFileLimit ||
		len(s.Branch) > 1024 || strings.ContainsFunc(s.Branch, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return false
	}
	seen := make(map[string]bool, len(s.Files))
	for _, f := range s.Files {
		if !f.valid() || seen[f.Path] {
			return false
		}
		seen[f.Path] = true
	}
	return true
}

// GitDiffUpdate is an observed HEAD summary or availability error. A
// nonempty Revision identifies the observed state; it is not a file locator.
// An Err without Revision is a failed request or lost connection.
type GitDiffUpdate struct {
	Revision string
	Summary  GitDiffSummary
	Err      error
}

// WatchGitDiff observes the thread's HEAD summary on a dedicated connection.
// Offer gitDiff:1 and gitDiffWatch:1. Cancel ctx when leaving the thread.
func (c *Client) WatchGitDiff(ctx context.Context, thread ID) <-chan GitDiffUpdate {
	w := workspaceWatch{method: gitDiffWaitMethod, capability: gitDiffWatchCapability, version: GitDiffWatchVersion,
		base: gitDiffCapability, baseVersions: []uint64{GitDiffVersion}}
	if thread.Valid() {
		w.params = map[string]any{"threadId": thread}
	}
	return watchWorkspace(ctx, c, w, decodeGitDiffUpdate, func(err error) GitDiffUpdate { return GitDiffUpdate{Err: err} })
}

func decodeGitDiffUpdate(raw any) (GitDiffUpdate, string, error) {
	v, err := decode[struct {
		Revision string          `json:"revision"`
		Summary  *GitDiffSummary `json:"summary,omitempty"`
		Error    *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}](raw)
	fields, ok := raw.(map[string]any)
	if err != nil || !ok || len(fields) != 2 || (v.Summary == nil) == (v.Error == nil) {
		return GitDiffUpdate{}, "", ErrProtocol
	}
	if v.Summary != nil && v.Summary.valid(GitDiffHead) {
		return GitDiffUpdate{Revision: v.Revision, Summary: *v.Summary}, v.Revision, nil
	}
	if v.Error != nil && v.Error.Message != "" && len(v.Error.Message) <= 512 {
		switch v.Error.Code {
		case "invalid_request", "invalid_cursor", "not_found", "limit", "internal":
			e := &ServerError{Code: v.Error.Code, Message: v.Error.Message}
			return GitDiffUpdate{Revision: v.Revision, Err: e}, v.Revision, nil
		}
	}
	return GitDiffUpdate{}, "", ErrProtocol
}

// GitDiffFile fetches one zero-based file from a summary in the same thread.
func (c *Client) GitDiffFile(ctx context.Context, thread ID, revision string, file int) (GitDiffPatch, error) {
	if !thread.Valid() || !scheduleID.MatchString(revision) || file < 0 || file >= gitDiffFileLimit {
		return GitDiffPatch{}, ErrProtocol
	}
	out, err := metadataRead[GitDiffPatch](ctx, c, gitDiffPrefix+"file",
		map[string]any{"threadId": thread, "revision": revision, "file": file}, gitDiffCapability, GitDiffVersion)
	if err != nil {
		return GitDiffPatch{}, err
	}
	if out.Revision != revision || out.File != file || len(out.Lines) == 0 || len(out.Lines) > gitDiffLineLimit {
		return GitDiffPatch{}, ErrProtocol
	}
	bytes := 0
	for _, line := range out.Lines {
		bytes += len(line.Text) + 80
		if !line.valid() || bytes > 128<<10 {
			return GitDiffPatch{}, ErrProtocol
		}
	}
	return out, nil
}
