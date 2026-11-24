// Package inbox is an example of a separately linked hush extension. It uses
// only the public extension API and SDK value types.
package inbox

import (
	"strconv"
	"strings"

	"github.com/javiermolinar/hush/sdk"
	"github.com/javiermolinar/hush/tui/extension"
)

const id = "example.org/inbox"
const open extension.ActionID = id + ".open"

// New constructs a registration for an explicit build to pass to the shell.
func New() extension.Registration {
	return extension.Registration{
		ID:         id,
		APIVersion: "0.1.0", // version this extension was authored against
		Actions: []extension.Action{{
			ID:          open,
			Title:       "Thread inbox",
			Description: "Check queued inputs or send a follow-up",
			Binding:     extension.Binding{Direct: "alt+i", Leader: "i b"},
			Slash:       &extension.Slash{Name: "inbox", Complete: complete},
			Available:   func(ctx extension.Context) bool { return ctx.Thread.ID != "" },
			Run:         run,
		}},
		Segments: []extension.Segment{{
			Key: "queue", Slot: extension.StatusRight, Order: 20, Priority: 10,
			Render: func(ctx extension.RenderContext) extension.Line {
				if ctx.Thread.Queued == 0 {
					return nil
				}
				return extension.Line{{
					Text:   queueLabel(ctx.Context),
					Tone:   extension.Accent,
					Action: extension.ActionRef{ID: open, Args: "status"},
					Hint:   "Show inbox status",
				}}
			},
		}},
		Activity: []extension.Summarizer{{Key: "tool-count", Summarize: summarizeTools}},
	}
}

func complete(_ extension.Context, req extension.CompletionRequest) []extension.Completion {
	var items []extension.Completion
	for _, arg := range [...]string{"status", "follow-up"} {
		if len(items) >= req.Limit {
			break
		}
		if strings.HasPrefix(arg, req.Args[:req.Cursor]) {
			items = append(items, extension.Completion{
				Label:   arg,
				Replace: extension.Replacement{Start: 0, End: len(req.Args), Text: arg},
			})
		}
	}
	return items
}

func queueLabel(ctx extension.Context) string {
	return strconv.Itoa(ctx.Thread.Queued) + " queued"
}

func run(ctx extension.Context, inv extension.Invocation) extension.Outcome {
	switch strings.TrimSpace(inv.Args) {
	case "status":
		if ctx.Connection != extension.Connected {
			return extension.Outcome{extension.Notice{Tone: extension.Warning, Text: "Inbox status may be stale while disconnected."}}
		}
		return extension.Outcome{extension.Notice{Text: queueLabel(ctx)}}
	case "follow-up":
		return followUp(ctx)
	case "":
		return extension.Outcome{extension.SelectDialog{
			Placement: extension.Overlay,
			Title:     "Thread inbox",
			Choices: []extension.Choice{
				{ID: "status", Label: "Show queue status"},
				{ID: "follow-up", Label: "Send a follow-up", Disabled: ctx.Connection != extension.Connected},
			},
			Done: func(_ extension.Context, reply extension.Reply[string]) extension.Outcome {
				if !reply.Accepted {
					return nil
				}
				return extension.Outcome{extension.Dispatch{Action: extension.ActionRef{ID: open, Args: reply.Value}}}
			},
		}}
	default:
		return extension.Outcome{extension.Notice{Tone: extension.Warning, Text: "Use /inbox status or /inbox follow-up."}}
	}
}

func followUp(ctx extension.Context) extension.Outcome {
	if ctx.Connection != extension.Connected {
		return extension.Outcome{extension.Notice{Tone: extension.Warning, Text: "Connect before sending a follow-up."}}
	}
	return extension.Outcome{extension.InputDialog{
		Title:       "Send a follow-up",
		Placeholder: "Queue while busy; start a run when idle",
		Multiline:   true,
		Done: func(_ extension.Context, reply extension.Reply[string]) extension.Outcome {
			if !reply.Accepted || strings.TrimSpace(reply.Value) == "" {
				return nil
			}
			return extension.Outcome{extension.Client{
				// Zero ThreadID stays tied to the thread that opened the dialog.
				Op: extension.Submit{Text: reply.Value, WhenBusy: sdk.FollowUp},
				Done: func(_ extension.Context, result extension.ClientResult) extension.Outcome {
					if result.Err != nil {
						return extension.Outcome{extension.Notice{Tone: extension.Warning, Text: "No follow-up receipt: " + result.Err.Error()}}
					}
					return extension.Outcome{extension.Notice{Tone: extension.Good, Text: "Follow-up accepted."}}
				},
			}}
		},
	}}
}

func summarizeTools(_ extension.RenderContext, activity extension.Activity) (extension.Line, bool) {
	if activity.Omitted != 0 || len(activity.Items) < 2 {
		return nil, false
	}
	for _, item := range activity.Items {
		if item.Kind != extension.ActivityTool || item.Label != "read" {
			return nil, false
		}
	}
	return extension.Line{{Text: "read-only checks", Tone: extension.Muted}}, true
}
