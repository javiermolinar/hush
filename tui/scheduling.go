package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

type scheduleBackend interface {
	ListSchedules(context.Context, sdk.ID) ([]sdk.ScheduleItem, error)
	AddSchedule(context.Context, sdk.ScheduleInput) (sdk.ScheduleItem, error)
	ChangeSchedule(context.Context, sdk.ScheduleChange) (sdk.ScheduleItem, error)
}

func scheduleJob(backend scheduleBackend, id sdk.ID, op x.ManageSchedule) func(context.Context) x.ClientResult {
	return func(ctx context.Context) x.ClientResult {
		if !id.Valid() || op.Add != nil && op.Change != nil {
			return x.ClientResult{Err: sdk.ErrProtocol}
		}
		ctx, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		if op.Add != nil {
			add := *op.Add
			add.ThreadID = id
			row, err := backend.AddSchedule(ctx, add)
			return x.ClientResult{Schedule: row, Err: err}
		}
		if op.Change != nil {
			change := *op.Change
			change.ThreadID = id
			row, err := backend.ChangeSchedule(ctx, change)
			return x.ClientResult{Schedule: row, Err: err}
		}
		rows, err := backend.ListSchedules(ctx, id)
		return x.ClientResult{Schedules: rows, Err: err}
	}
}

func scheduleMenu(c x.Context, v x.Invocation) x.Outcome {
	if !c.Thread.ID.Valid() {
		return showNote("Open the requesting thread before scheduling", x.Warning)
	}
	args, err := editArgs(strings.TrimSpace(v.Args))
	if strings.TrimSpace(v.Args) == "" {
		args, err = nil, nil
	}
	if err == nil && (len(args) == 0 || len(args) == 1 && args[0] == "list") {
		return x.Outcome{x.Client{Op: x.ManageSchedule{}, Done: scheduleList}}
	}
	const help = `Use /schedule add TARGET RFC3339 "prompt" [after THREAD/SUBMISSION] [allow-late]`
	if err != nil || len(args) < 4 || args[0] != "add" || !sdk.ID(args[1]).Valid() || strings.TrimSpace(args[3]) == "" {
		return showNote(help, x.Warning)
	}
	when, err := sdk.ParseScheduleTime(args[2])
	if err != nil {
		return showNote(err.Error(), x.Warning)
	}
	input := sdk.ScheduleInput{TargetThreadID: sdk.ID(args[1]), ScheduleTime: when, Text: args[3]}
	for rest := args[4:]; len(rest) != 0; {
		switch {
		case rest[0] == "allow-late" && !input.AllowLate:
			input.AllowLate, rest = true, rest[1:]
		case rest[0] == "after" && len(rest) >= 2 && input.Prerequisite == nil:
			input.Prerequisite, err = sdk.ParseSchedulePrerequisite(rest[1])
			if err != nil || input.Prerequisite == nil {
				return showNote("Use after THREAD/SUBMISSION", x.Warning)
			}
			rest = rest[2:]
		default:
			return showNote(help, x.Warning)
		}
	}
	message := scheduleWhen(when, input.TargetThreadID, input.Prerequisite)
	title := "Schedule this input?"
	if input.AllowLate {
		title = "Schedule this input (allow late)?"
	}
	return x.Outcome{x.ConfirmDialog{Placement: x.Overlay, Title: title, Message: message,
		Done: func(_ x.Context, decision x.Reply[bool]) x.Outcome {
			if !decision.Accepted || !decision.Value {
				return nil
			}
			return x.Outcome{x.Client{Op: x.ManageSchedule{Add: &input}, Done: scheduleChanged}}
		}}}
}

func scheduleWhen(when sdk.ScheduleTime, target sdk.ID, dep *sdk.SchedulePrerequisite) string {
	line := when.String() + " → #" + string(target)
	if dep != nil {
		line += fmt.Sprintf(" · after #%s submission #%s", dep.ThreadID, dep.SubmissionID)
	}
	return line
}

func scheduleList(_ x.Context, result x.ClientResult) x.Outcome {
	if result.Err != nil {
		return clientNote(result, "")
	}
	if len(result.Schedules) == 0 {
		return showNote("No scheduled occurrences in this thread", x.Muted)
	}
	rows := result.Schedules
	choices := make([]x.Choice, len(rows))
	for i, row := range rows {
		state := strings.ReplaceAll(row.State, "_", " ")
		if row.State == "needs_attention" {
			state = "Needs you"
		}
		choices[i] = x.Choice{ID: row.ID, Label: state + " · " + scheduleWhen(row.ScheduleTime, row.TargetThreadID, row.Prerequisite),
			Description: row.ID}
	}
	return x.Outcome{x.SelectDialog{Title: "Scheduled occurrences", Placement: x.Sheet, Choices: choices,
		Done: func(_ x.Context, selection x.Reply[string]) x.Outcome {
			if selection.Accepted {
				for _, row := range rows {
					if row.ID == selection.Value {
						return scheduleActions(row)
					}
				}
			}
			return nil
		}}}
}

func scheduleActions(row sdk.ScheduleItem) x.Outcome {
	if row.State != "pending" && row.State != "needs_attention" {
		return scheduleChanged(x.Context{}, x.ClientResult{Schedule: row})
	}
	choices := []x.Choice{{ID: "reschedule", Label: "Reschedule"}, {ID: "cancel", Label: "Cancel occurrence"}}
	if row.State == "needs_attention" {
		choices = append([]x.Choice{{ID: "run", Label: "Run now (if eligible)"}}, choices...)
	}
	message := scheduleWhen(row.ScheduleTime, row.TargetThreadID, row.Prerequisite)
	title := "Scheduled occurrence"
	if row.Reason != "" {
		title = "Needs you: " + strings.ReplaceAll(row.Reason, "_", " ")
	}
	return x.Outcome{x.SelectDialog{Title: title + " · " + message, Choices: choices,
		Done: func(_ x.Context, selection x.Reply[string]) x.Outcome {
			if !selection.Accepted {
				return nil
			}
			change := sdk.ScheduleChange{ID: row.ID, Op: selection.Value}
			submit := func() x.Outcome {
				return x.Outcome{x.Client{Op: x.ManageSchedule{Change: &change}, Done: scheduleChanged}}
			}
			if selection.Value == "reschedule" {
				return x.Outcome{x.InputDialog{Title: "New due time (RFC3339)", Initial: row.ScheduleTime.String(),
					Done: func(_ x.Context, reply x.Reply[string]) x.Outcome {
						if !reply.Accepted {
							return nil
						}
						when, err := sdk.ParseScheduleTime(strings.TrimSpace(reply.Value))
						if err != nil {
							return showNote(err.Error(), x.Warning)
						}
						change.Time = when
						return submit()
					}}}
			}
			return x.Outcome{x.ConfirmDialog{Title: "Confirm " + selection.Value + "?", Message: message,
				Done: func(_ x.Context, decision x.Reply[bool]) x.Outcome {
					if decision.Accepted && decision.Value {
						return submit()
					}
					return nil
				}}}
		}}}
}

func scheduleChanged(_ x.Context, r x.ClientResult) x.Outcome {
	if r.Err != nil {
		return clientNote(r, "")
	}
	line := "Schedule: " + strings.ReplaceAll(r.Schedule.State, "_", " ")
	if r.Schedule.SubmissionID.Valid() {
		line += " · admitted submission #" + string(r.Schedule.SubmissionID)
	} else if r.Schedule.State == "pending" {
		line += " · " + r.Schedule.ScheduleTime.String()
	}
	if r.Schedule.Reason != "" && r.Schedule.Reason != "cancelled" {
		line += " · " + strings.ReplaceAll(r.Schedule.Reason, "_", " ")
	}
	return showNote(line, x.Good)
}
