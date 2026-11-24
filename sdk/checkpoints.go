package sdk

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const CheckpointsVersion = 1
const checkpointCapability = "checkpoints"
const checkpointPrefix = "ext.checkpoints."

const (
	RewindTranscript = "transcript"
	RewindFiles      = "files"
)

var gitCommit = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

type Checkpoint struct {
	ID        string `json:"id,omitempty"`
	AtEntryID ID     `json:"atEntryId"`
	Label     string `json:"label"`
	Kind      string `json:"kind"` // bookmark, files, or imported_label
}

type CheckpointInput struct {
	ThreadID  ID     `json:"threadId"`
	AtEntryID ID     `json:"atEntryId,omitempty"`
	Kind      string `json:"kind"`
	Label     string `json:"label,omitempty"`
}

type RewindInput struct {
	ThreadID     ID     `json:"threadId"`
	AtEntryID    ID     `json:"atEntryId,omitempty"`
	CheckpointID string `json:"checkpointId,omitempty"`
	Mode         string `json:"mode"`
}

type RewindDecision struct {
	ThreadID ID     `json:"threadId"`
	ID       string `json:"id"`
	Mode     string `json:"mode"`
}

type KnownEffect struct {
	EntryID ID     `json:"entryId"`
	Text    string `json:"text"`
}

type FileRestore struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
	Ref        string `json:"ref"`
	CWD        string `json:"cwd"`
	Branch     string `json:"branch"`
}

type RewindPreview struct {
	ID        string        `json:"id"`
	ThreadID  ID            `json:"threadId"`
	AtEntryID ID            `json:"atEntryId"`
	Mode      string        `json:"mode"`
	Files     *FileRestore  `json:"files,omitempty"`
	Reason    string        `json:"reason,omitempty"`
	Effects   []KnownEffect `json:"effects"`
}

// Lines describes the exact fork and any known irreversible effects. It
// never claims that unrecorded external effects have been enumerated.
func (p RewindPreview) Lines() []string {
	lines := []string{"Operation: " + p.ID,
		fmt.Sprintf("Fork thread #%s at entry #%s. Original remains reachable.", p.ThreadID, p.AtEntryID)}
	if p.Files != nil {
		lines = append(lines, "Restore tracked files from committed snapshot only.",
			"Repository: "+p.Files.Repository, "Commit: "+p.Files.Commit, "Ref: "+p.Files.Ref,
			"New worktree: "+p.Files.CWD, "New branch: "+p.Files.Branch)
	} else {
		lines = append(lines, "Transcript only. No files will change.")
		if p.Reason != "" {
			lines = append(lines, p.Reason)
		}
	}
	if len(p.Effects) > 0 {
		lines = append(lines, "", "Recorded external effects (not undone):")
		for _, e := range p.Effects {
			lines = append(lines, fmt.Sprintf("#%s: %s", e.EntryID, e.Text))
		}
	} else {
		lines = append(lines, "", "No external effects recorded since this entry.")
	}
	return append(lines, "Other effects may not be recorded.")
}

type RewindResult struct {
	ID           string `json:"id"`
	ThreadID     ID     `json:"threadId"`
	AtEntryID    ID     `json:"atEntryId"`
	Mode         string `json:"mode"`
	State        string `json:"state"` // staging, ready or failed
	ForkThreadID ID     `json:"forkThreadId,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

func validRewindMode(s string) bool { return s == RewindFiles || s == RewindTranscript }

func checkpointText(s string, cap int) bool {
	return len(s) <= cap && utf8.ValidString(s) &&
		strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) < 0
}

func (c Checkpoint) valid() bool {
	return c.AtEntryID.Valid() && checkpointText(c.Label, 256) && c.Label != "" &&
		(c.Kind == "imported_label" && c.ID == "" || (c.Kind == "bookmark" || c.Kind == "files") && scheduleID.MatchString(c.ID))
}

func (p RewindPreview) valid(input RewindInput) bool {
	if !scheduleID.MatchString(p.ID) || p.ThreadID != input.ThreadID || !p.AtEntryID.Valid() ||
		input.AtEntryID != "" && p.AtEntryID != input.AtEntryID || !validRewindMode(p.Mode) || !checkpointText(p.Reason, 1024) ||
		p.Effects == nil || len(p.Effects) > 128 {
		return false
	}
	seen := make(map[ID]struct{}, len(p.Effects))
	for _, e := range p.Effects {
		if !e.EntryID.Valid() || e.Text == "" || !checkpointText(e.Text, 192) {
			return false
		}
		if _, exists := seen[e.EntryID]; exists {
			return false
		}
		seen[e.EntryID] = struct{}{}
	}
	if p.Files == nil {
		return p.Mode == RewindTranscript && (input.Mode == RewindTranscript) == (p.Reason == "")
	}
	if p.Mode != RewindFiles || input.Mode != RewindFiles || p.Reason != "" {
		return false
	}
	f := p.Files
	const prefix = "refs/hush/checkpoints/"
	return gitCommit.MatchString(f.Commit) && strings.HasPrefix(f.Ref, prefix) && scheduleID.MatchString(strings.TrimPrefix(f.Ref, prefix)) &&
		f.Branch == "hush/rewind/"+p.ID && path.IsAbs(f.CWD) && path.Base(f.CWD) == p.ID && checkpointText(f.CWD, 16384) &&
		path.IsAbs(f.Repository) && checkpointText(f.Repository, 16384)
}

func (r RewindResult) valid(thread ID, key, mode string) bool {
	return r.ThreadID == thread && r.ID == key && r.AtEntryID.Valid() && validRewindMode(r.Mode) &&
		(mode == "" || r.Mode == mode) && checkpointText(r.Reason, 1024) &&
		(r.State == "staging" && r.Mode == RewindFiles && r.ForkThreadID == "" && r.Reason == "" ||
			r.State == "ready" && r.ForkThreadID.Valid() && r.ForkThreadID != thread && r.Reason == "" ||
			r.State == "failed" && r.Mode == RewindFiles && r.ForkThreadID == "" && r.Reason != "")
}

func checkpointCall[T any](ctx context.Context, c *Client, op string, input any) (T, error) {
	return optionalCall[T](ctx, c, checkpointCapability, CheckpointsVersion, checkpointPrefix+op, input)
}

func (c *Client) ListCheckpoints(ctx context.Context, thread ID) ([]Checkpoint, error) {
	if !thread.Valid() {
		return nil, ErrProtocol
	}
	out, err := checkpointCall[struct {
		Items []Checkpoint `json:"items"`
	}](ctx, c, "list", map[string]ID{"threadId": thread})
	if err != nil {
		return nil, err
	}
	if out.Items == nil || len(out.Items) > 128 {
		return nil, ErrProtocol
	}
	for _, item := range out.Items {
		if !item.valid() {
			return nil, ErrProtocol
		}
	}
	return out.Items, nil
}

func (c *Client) RecordCheckpoint(ctx context.Context, input CheckpointInput) (Checkpoint, error) {
	if !input.ThreadID.Valid() || (input.AtEntryID != "" && !input.AtEntryID.Valid()) ||
		(input.Kind != "bookmark" && input.Kind != "files") || !checkpointText(input.Label, 192) {
		return Checkpoint{}, ErrProtocol
	}
	out, err := checkpointCall[Checkpoint](ctx, c, "record", input)
	if err == nil && (!out.valid() || out.Kind != input.Kind || input.AtEntryID != "" && out.AtEntryID != input.AtEntryID) {
		return Checkpoint{}, ErrProtocol
	}
	return out, err
}

func (c *Client) RecordKnownEffect(ctx context.Context, thread ID, text string) (ID, error) {
	if !thread.Valid() || strings.TrimSpace(text) == "" || !checkpointText(text, 192) {
		return "", ErrProtocol
	}
	out, err := checkpointCall[struct {
		EntryID ID `json:"entryId"`
	}](ctx, c, "effect", map[string]any{"threadId": thread, "text": text})
	if err == nil && !out.EntryID.Valid() {
		return "", ErrProtocol
	}
	return out.EntryID, err
}

func (c *Client) PreviewRewind(ctx context.Context, input RewindInput) (RewindPreview, error) {
	if !input.ThreadID.Valid() || !validRewindMode(input.Mode) ||
		!(input.AtEntryID.Valid() && input.CheckpointID == "" || input.AtEntryID == "" && scheduleID.MatchString(input.CheckpointID)) {
		return RewindPreview{}, ErrProtocol
	}
	out, err := checkpointCall[RewindPreview](ctx, c, "preview", input)
	if err == nil && !out.valid(input) {
		return RewindPreview{}, ErrProtocol
	}
	return out, err
}

func (c *Client) ConfirmRewind(ctx context.Context, input RewindDecision) (RewindResult, error) {
	if !input.ThreadID.Valid() || !scheduleID.MatchString(input.ID) || !validRewindMode(input.Mode) {
		return RewindResult{}, ErrProtocol
	}
	out, err := checkpointCall[RewindResult](ctx, c, "confirm", input)
	if err == nil && !out.valid(input.ThreadID, input.ID, input.Mode) {
		return RewindResult{}, ErrProtocol
	}
	return out, err
}

func (c *Client) RewindStatus(ctx context.Context, thread ID, id string) (RewindResult, error) {
	if !thread.Valid() || !scheduleID.MatchString(id) {
		return RewindResult{}, ErrProtocol
	}
	out, err := checkpointCall[RewindResult](ctx, c, "status", map[string]any{"threadId": thread, "id": id})
	if err == nil && !out.valid(thread, id, "") {
		return RewindResult{}, ErrProtocol
	}
	return out, err
}
