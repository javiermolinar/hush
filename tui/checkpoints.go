package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

type checkpointBackend interface {
	ListCheckpoints(context.Context, sdk.ID) ([]sdk.Checkpoint, error)
	RecordCheckpoint(context.Context, sdk.CheckpointInput) (sdk.Checkpoint, error)
	RecordKnownEffect(context.Context, sdk.ID, string) (sdk.ID, error)
	PreviewRewind(context.Context, sdk.RewindInput) (sdk.RewindPreview, error)
	ConfirmRewind(context.Context, sdk.RewindDecision) (sdk.RewindResult, error)
	RewindStatus(context.Context, sdk.ID, string) (sdk.RewindResult, error)
}

func checkpointJob(backend checkpointBackend, id sdk.ID, op x.ManageCheckpoint) func(context.Context) x.ClientResult {
	operations := 0
	for _, supplied := range []bool{op.Record != nil, op.Effect != "", op.Preview != nil, op.Confirm != nil, op.StatusID != ""} {
		if supplied {
			operations++
		}
	}
	return func(ctx context.Context) x.ClientResult {
		if !id.Valid() || operations > 1 {
			return x.ClientResult{Err: sdk.ErrProtocol}
		}
		call, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		switch {
		case op.Record != nil:
			input := *op.Record
			input.ThreadID = id
			v, err := backend.RecordCheckpoint(call, input)
			return x.ClientResult{Checkpoint: v, Err: err}
		case op.Effect != "":
			_, err := backend.RecordKnownEffect(call, id, op.Effect)
			return x.ClientResult{Err: err}
		case op.Preview != nil:
			input := *op.Preview
			input.ThreadID = id
			v, err := backend.PreviewRewind(call, input)
			return x.ClientResult{RewindPreview: v, Err: err}
		case op.Confirm != nil:
			decision := *op.Confirm
			decision.ThreadID = id
			v, err := backend.ConfirmRewind(call, decision)
			if err == nil && v.State == "staging" {
				v = waitRewind(call, backend, id, v)
			}
			return x.ClientResult{Rewind: v, Err: err}
		case op.StatusID != "":
			v, err := backend.RewindStatus(call, id, op.StatusID)
			return x.ClientResult{Rewind: v, Err: err}
		default:
			rows, err := backend.ListCheckpoints(call, id)
			return x.ClientResult{Checkpoints: rows, Err: err}
		}
	}
}

func waitRewind(ctx context.Context, backend checkpointBackend, thread sdk.ID, accepted sdk.RewindResult) sdk.RewindResult {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return accepted
		case <-t.C:
			r, err := backend.RewindStatus(ctx, thread, accepted.ID)
			if err != nil {
				return accepted // no replay or assumption about settlement
			}
			if r.State != "staging" {
				return r
			}
		}
	}
}

func checkpointMenu(c x.Context, v x.Invocation) x.Outcome {
	if !c.Thread.ID.Valid() {
		return showNote("Open a thread with committed history first", x.Warning)
	}
	op, text, _ := strings.Cut(strings.TrimSpace(v.Args), " ")
	text = strings.TrimSpace(text)
	switch op {
	case "effect":
		if text != "" {
			return x.Outcome{x.Client{Op: x.ManageCheckpoint{Effect: text}, Done: func(_ x.Context, r x.ClientResult) x.Outcome {
				return clientNote(r, "Recorded known external effect")
			}}}
		}
	case "bookmark", "files":
		return recordCheckpoint(op, text)
	case "":
		return x.Outcome{x.SelectDialog{Placement: x.Sheet, Title: "Record at the latest committed entry", Choices: []x.Choice{
			{ID: "bookmark", Label: "Transcript bookmark", Description: "No files captured"},
			{ID: "files", Label: "File checkpoint", Description: "Committed HEAD only"},
		}, Done: func(_ x.Context, r x.Reply[string]) x.Outcome {
			if !r.Accepted {
				return nil
			}
			return x.Outcome{x.InputDialog{Title: "Checkpoint label (optional)", Placement: x.Sheet,
				Done: func(_ x.Context, label x.Reply[string]) x.Outcome {
					if label.Accepted {
						return recordCheckpoint(r.Value, strings.TrimSpace(label.Value))
					}
					return nil
				}}}
		}}}
	}
	return showNote("Use /checkpoint bookmark [label], files [label], or effect TEXT", x.Warning)
}

func recordCheckpoint(kind, label string) x.Outcome {
	save := x.Client{Op: x.ManageCheckpoint{Record: &sdk.CheckpointInput{Kind: kind, Label: label}},
		Done: func(_ x.Context, r x.ClientResult) x.Outcome {
			return clientNote(r, "Recorded "+r.Checkpoint.Kind+" checkpoint at #"+string(r.Checkpoint.AtEntryID))
		}}
	if kind == "files" {
		return x.Outcome{x.ConfirmDialog{Title: "Pin committed HEAD?", Message: "Staged, unstaged and untracked changes are not captured.",
			Done: func(_ x.Context, r x.Reply[bool]) x.Outcome {
				if r.Accepted && r.Value {
					return x.Outcome{save}
				}
				return nil
			}}}
	}
	return x.Outcome{save}
}

func rewindMenu(c x.Context, v x.Invocation) x.Outcome {
	if !c.Thread.ID.Valid() {
		return showNote("Open a thread to rewind", x.Warning)
	}
	args := strings.Fields(v.Args)
	if len(args) == 0 {
		return x.Outcome{x.Client{Op: x.ManageCheckpoint{}, Done: rewindList}}
	}
	if len(args) == 2 {
		if args[0] == "entry" && sdk.ID(args[1]).Valid() {
			return previewRewind(sdk.RewindInput{AtEntryID: sdk.ID(args[1]), Mode: sdk.RewindTranscript})
		}
		if args[0] == "status" {
			return x.Outcome{x.Client{Op: x.ManageCheckpoint{StatusID: args[1]}, Done: rewindResult}}
		}
	}
	return showNote("Use /rewind, /rewind entry ID or /rewind status OPERATION", x.Warning)
}

func rewindList(_ x.Context, r x.ClientResult) x.Outcome {
	if r.Err != nil {
		return clientNote(r, "")
	}
	if len(r.Checkpoints) == 0 {
		return showNote("No checkpoints. Use /checkpoint or /rewind entry ID.", x.Muted)
	}
	rows := make(map[string]sdk.Checkpoint, len(r.Checkpoints))
	choices := make([]x.Choice, 0, len(r.Checkpoints))
	for _, v := range r.Checkpoints {
		key, label := v.ID, strings.ReplaceAll(v.Kind, "_", " ")
		if key == "" {
			key = "entry-" + string(v.AtEntryID)
		}
		rows[key] = v
		choices = append(choices, x.Choice{ID: key, Label: "◇ " + v.Label, Description: label + " · #" + string(v.AtEntryID)})
	}
	return x.Outcome{x.SelectDialog{Title: "Rewind from a checkpoint", Placement: x.Sheet, Choices: choices,
		Done: func(_ x.Context, selection x.Reply[string]) x.Outcome {
			v, ok := rows[selection.Value]
			if !selection.Accepted || !ok {
				return nil
			}
			input := sdk.RewindInput{CheckpointID: v.ID, Mode: sdk.RewindTranscript}
			if v.ID == "" {
				input.AtEntryID = v.AtEntryID
			}
			if v.Kind != "files" {
				return previewRewind(input)
			}
			return x.Outcome{x.SelectDialog{Title: "Choose what to rewind", Placement: x.Sheet, Choices: []x.Choice{
				{ID: sdk.RewindTranscript, Label: "Transcript only", Description: "Leave every checkout alone"},
				{ID: sdk.RewindFiles, Label: "Transcript and committed files", Description: "New worktree and branch"},
			}, Done: func(_ x.Context, mode x.Reply[string]) x.Outcome {
				if mode.Accepted {
					input.Mode = mode.Value
					return previewRewind(input)
				}
				return nil
			}}}
		}}}
}

func previewRewind(input sdk.RewindInput) x.Outcome {
	return x.Outcome{x.Client{Op: x.ManageCheckpoint{ThreadID: input.ThreadID, Preview: &input}, Done: func(_ x.Context, r x.ClientResult) x.Outcome {
		if r.Err != nil {
			return clientNote(r, "")
		}
		p := r.RewindPreview
		return x.Outcome{x.ReviewDialog{Title: "Review rewind", Text: strings.Join(p.Lines(), "\n"),
			Done: func(_ x.Context, next x.Reply[bool]) x.Outcome {
				if !next.Accepted {
					return nil
				}
				question := "Create a transcript-only fork?"
				if p.Mode == sdk.RewindFiles {
					question = "Restore into the new worktree and fork?"
				}
				return x.Outcome{x.ConfirmDialog{Title: question, Message: fmt.Sprintf("Fork thread #%s at #%s. Original stays intact.", p.ThreadID, p.AtEntryID),
					Done: func(_ x.Context, yes x.Reply[bool]) x.Outcome {
						if !yes.Accepted || !yes.Value {
							return nil
						}
						return x.Outcome{x.Client{Op: x.ManageCheckpoint{ThreadID: p.ThreadID, Confirm: &sdk.RewindDecision{ID: p.ID, Mode: p.Mode}},
							Done: func(c x.Context, r x.ClientResult) x.Outcome {
								if r.Err != nil {
									return showNote(r.Err.Error()+" · rewind "+p.ID, x.Danger)
								}
								return rewindResult(c, r)
							}}}
					}}}
			}}}
	}}}
}

func rewindResult(_ x.Context, r x.ClientResult) x.Outcome {
	if r.Err != nil {
		return clientNote(r, "")
	}
	v := r.Rewind
	if v.State == "ready" {
		return x.Outcome{x.Client{Op: x.Switch{ThreadID: v.ForkThreadID}}}
	}
	if v.State == "failed" {
		return x.Outcome{x.SelectDialog{Title: "File restore did not publish a fork", Placement: x.Sheet, Choices: []x.Choice{
			{ID: "only", Label: "Preview transcript-only fork", Description: v.Reason},
		}, Done: func(_ x.Context, r x.Reply[string]) x.Outcome {
			if r.Accepted {
				return previewRewind(sdk.RewindInput{ThreadID: v.ThreadID, AtEntryID: v.AtEntryID, Mode: sdk.RewindTranscript})
			}
			return nil
		}}}
	}
	return showNote("Still staging · /rewind status "+v.ID, x.Muted)
}
