package sdk

import (
	"context"
	"path"
	"strings"
)

const (
	GitDiffVersion    = 1
	GitDiffHead       = "head"
	GitDiffCheckpoint = "checkpoint"
	gitDiffCapability = "gitDiff"
	gitDiffPrefix     = "ext.gitDiff."
	gitDiffFileLimit  = 128
	gitDiffLineLimit  = 1500
)

// GitDiffSummary describes one live, read-only comparison on the backend.
// Revision addresses its file list; it does not pin the worktree contents.
type GitDiffSummary struct {
	Revision   string        `json:"revision"`
	Range      string        `json:"range"`
	Commit     string        `json:"commit"`
	Checkpoint bool          `json:"checkpoint"`
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
	// Cancellation closes the passive connection, rather than leaving a Git
	// command running on the chat/abort connection after switching threads.
	out, err := waitOnCapability[GitDiffSummary](ctx, c, gitDiffPrefix+"summary",
		map[string]any{"threadId": thread, "range": baseline}, gitDiffCapability, GitDiffVersion)
	if err != nil {
		return GitDiffSummary{}, err
	}
	if !scheduleID.MatchString(out.Revision) || out.Range != baseline || !gitCommit.MatchString(out.Commit) ||
		baseline == GitDiffCheckpoint && !out.Checkpoint || out.Files == nil || len(out.Files) > gitDiffFileLimit {
		return GitDiffSummary{}, ErrProtocol
	}
	seen := make(map[string]bool, len(out.Files))
	for _, f := range out.Files {
		if !f.valid() || seen[f.Path] {
			return GitDiffSummary{}, ErrProtocol
		}
		seen[f.Path] = true
	}
	return out, nil
}

// GitDiffFile fetches one zero-based file from a summary in the same thread.
func (c *Client) GitDiffFile(ctx context.Context, thread ID, revision string, file int) (GitDiffPatch, error) {
	if !thread.Valid() || !scheduleID.MatchString(revision) || file < 0 || file >= gitDiffFileLimit {
		return GitDiffPatch{}, ErrProtocol
	}
	out, err := waitOnCapability[GitDiffPatch](ctx, c, gitDiffPrefix+"file",
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
