package headless

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/javiermolinar/hush/sdk"
	"github.com/spf13/pflag"
)

type checkpointOptions struct {
	thread, at                         sdk.ID
	op, mark, mode, key, label, effect string
}

func (p *checkpointOptions) flags(f *pflag.FlagSet) {
	f.StringVar(&p.mark, "checkpoint", "", "")
	f.StringVar(&p.mode, "restore", "", "")
}

func (p *checkpointOptions) parse(f *pflag.FlagSet, args []string) error {
	if len(args) < 2 || !p.thread.Valid() {
		return errors.New("checkpoint and rewind require an action and --thread")
	}
	allowed := []string{"thread", "socket", "backend", "profile", "json"}
	argc := 2
	p.op = args[0] + "." + args[1]
	switch p.op {
	case "checkpoint.list":
	case "checkpoint.bookmark", "checkpoint.files":
		allowed = append(allowed, "at-entry", "title")
		if f.Changed("at-entry") && !p.at.Valid() {
			return errors.New("--at-entry must be a valid durable ID")
		}
	case "checkpoint.effect":
		allowed = append(allowed, "text")
		if strings.TrimSpace(p.effect) == "" {
			return errors.New("checkpoint effect requires --text")
		}
	case "rewind.preview":
		allowed = append(allowed, "checkpoint", "at-entry", "restore")
		if (p.mark == "") == (p.at == "") || p.at != "" && !p.at.Valid() {
			return errors.New("rewind preview needs exactly one of --checkpoint or --at-entry")
		}
	case "rewind.confirm":
		allowed = append(allowed, "restore")
		argc = 3
	case "rewind.status":
		argc = 3
	default:
		return errors.New("use checkpoint list, bookmark, files, effect; or rewind preview, confirm, status")
	}
	if (p.op == "rewind.preview" || p.op == "rewind.confirm") && p.mode != sdk.RewindTranscript && p.mode != sdk.RewindFiles {
		return errors.New("specify --restore files or --restore transcript")
	}
	if len(args) != argc {
		return errors.New("rewind confirm/status require one operation ID; other actions take only flags")
	}
	if argc == 3 {
		p.key = args[2]
	}
	var invalid string
	f.Visit(func(v *pflag.Flag) {
		if invalid == "" && !slices.Contains(allowed, v.Name) {
			invalid = v.Name
		}
	})
	if invalid != "" {
		return fmt.Errorf("--%s is not valid for %s", invalid, p.op)
	}
	return nil
}

func (p checkpointOptions) run(ctx context.Context, c *sdk.Client, json bool, out io.Writer) error {
	switch p.op {
	case "checkpoint.list":
		rows, err := c.ListCheckpoints(ctx, p.thread)
		if err != nil {
			return err
		}
		if json {
			return writeValue(out, true, struct {
				Items []sdk.Checkpoint `json:"items"`
			}{rows}, "")
		}
		for _, r := range rows {
			if _, err := fmt.Fprintln(out, checkpointLine(r)); err != nil {
				return err
			}
		}
		return nil
	case "checkpoint.bookmark", "checkpoint.files":
		r, err := c.RecordCheckpoint(ctx, sdk.CheckpointInput{ThreadID: p.thread, AtEntryID: p.at,
			Kind: strings.TrimPrefix(p.op, "checkpoint."), Label: p.label})
		if err != nil {
			return err
		}
		return writeValue(out, json, r, checkpointLine(r))
	case "checkpoint.effect":
		r, err := c.RecordKnownEffect(ctx, p.thread, p.effect)
		if err != nil {
			return err
		}
		return writeValue(out, json, struct {
			EntryID sdk.ID `json:"entryId"`
		}{r}, "Recorded external effect at entry #"+string(r))
	case "rewind.preview":
		r, err := c.PreviewRewind(ctx, sdk.RewindInput{ThreadID: p.thread, AtEntryID: p.at,
			CheckpointID: p.mark, Mode: p.mode})
		if err != nil {
			return err
		}
		return writeValue(out, json, r, strings.Join(r.Lines(), "\n"))
	}
	var r sdk.RewindResult
	var err error
	if p.op == "rewind.confirm" {
		r, err = c.ConfirmRewind(ctx, sdk.RewindDecision{ThreadID: p.thread, ID: p.key, Mode: p.mode})
	} else {
		r, err = c.RewindStatus(ctx, p.thread, p.key)
	}
	if err != nil {
		return err
	}
	line := fmt.Sprintf("%s %s", r.ID, r.State)
	if r.ForkThreadID.Valid() {
		line += " · new fork #" + string(r.ForkThreadID)
	}
	if r.Reason != "" {
		line += " · " + r.Reason
	}
	return writeValue(out, json, r, line)
}

func checkpointLine(r sdk.Checkpoint) string {
	return fmt.Sprintf("%s %s entry #%s · %s", r.ID, r.Kind, r.AtEntryID, r.Label)
}
