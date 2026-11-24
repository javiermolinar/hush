package tui

import (
	"encoding/json"
	"runtime"
	"strings"

	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const coreID = "hush/core"
const paletteID x.ActionID = coreID + ".palette"

func builtins(workspaceTrust bool) x.Registration {
	local := func(id, title, direct, leader, slash string, op x.LocalOp) x.Action {
		a := x.Action{ID: x.ActionID(coreID + "." + id), Title: title, Binding: x.Binding{Direct: direct, Leader: leader},
			Run: func(x.Context, x.Invocation) x.Outcome { return x.Outcome{x.Local{Op: op}} }}
		if slash != "" {
			a.Slash = &x.Slash{Name: slash}
		}
		return a
	}
	r := x.Registration{ID: coreID, APIVersion: x.APIVersion, Actions: []x.Action{
		local("palette", "Command palette", "", "p", "hotkeys", x.OpenPalette),
		local("details", "Toggle details", "ctrl+o", "o", "", x.ToggleDetails),
		local("edit", "Edit prompt", "ctrl+g", "e", "", x.EditPrompt),
		{ID: gitDiffID, Title: "Review worktree changes", Binding: x.Binding{Leader: "i d"}, Requires: x.GitDiff,
			Slash: &x.Slash{Name: "diff"}, Available: func(c x.Context) bool { return c.ActiveThreadID.Valid() },
			Run: func(x.Context, x.Invocation) x.Outcome { return x.Outcome{x.Local{Op: openGitDiffOp}} }},
		local("copy", "Copy last answer", "", "y", "copy", x.CopyLastAnswer),
		local("code", "Copy last code block", "", "c", "", x.CopyLastCode),
		local("queue", "Recall queued messages", "alt+up", "q", "", x.RecallQueued),
		{ID: coreID + ".close", Title: "Close tab", Binding: x.Binding{Direct: "ctrl+w", Leader: "w"}, Run: func(_ x.Context, v x.Invocation) x.Outcome {
			return x.Outcome{x.Local{Op: x.CloseTab, ThreadID: sdk.ID(v.Target)}}
		}},
		local("quit", "Quit", "", "d", "quit", x.Quit),
	}}
	if workspaceTrust {
		r.Actions = append(r.Actions, local("trust", "Workspace trust", "", "i t", "trust", openWorkspaceTrustOp))
	}
	if runtime.GOOS == "darwin" {
		r.Actions = append(r.Actions, local("paste-image", "Paste clipboard image", "ctrl+v", "i p", "", pasteImageOp))
	}
	// The native core exposes entry kinds, not renderer names. Register the
	// common turn discriminator; other kinds use the same portable fallback.
	for _, kind := range []string{"pi.user"} {
		r.Entries = append(r.Entries, x.EntryRenderer{Kind: kind, Render: func(ctx x.RenderContext, e x.Entry) (x.Content, bool) {
			return entryContent(ctx, e), !entryHasTool(e)
		}})
	}
	for _, spec := range []struct{ name, arg string }{{"bash", "command"}, {"read", "path"}, {"edit", "path"}, {"write", "path"}} {
		r.Tools = append(r.Tools, x.ToolRenderer{Name: spec.name, Render: func(ctx x.RenderContext, t x.Tool) (x.Content, bool) {
			if ctx.Live {
				return nil, false // the prompt rule already shows the current tool and timing
			}
			return renderTool(ctx, t, spec.arg), true
		}})
	}
	return r
}

func entryHasTool(e x.Entry) bool {
	for _, msg := range e.Record.Messages {
		if msg.Role == "toolResult" {
			return true
		}
		for _, b := range msg.Blocks {
			if b.Type == "toolCall" {
				return true
			}
		}
	}
	return false
}

func checkpointMarker(kind string) bool {
	return kind == "hush.checkpoint" || kind == "hush.known-effect"
}

func entryContent(ctx x.RenderContext, e x.Entry) x.Content {
	var out x.Content
	for _, msg := range e.Record.Messages {
		if checkpointMarker(e.Record.Kind) {
			for _, b := range msg.Blocks {
				if b.Type == "text" && b.Text != "" {
					out = append(out, x.Block{Text: b.Text, Tone: x.Muted})
				}
			}
			continue
		}
		out = append(out, messageBlocks(ctx, msg.Role, msg.Blocks)...)
		if msg.Error != "" {
			out = append(out, x.Block{Text: msg.Error, Tone: x.Danger})
		}
	}
	if len(e.Record.Messages) == 0 && e.Record.Kind != "model" {
		out = append(out, x.Block{Text: "─  " + e.Record.Kind, Tone: x.Muted})
	}
	return out
}

func messageBlocks(ctx x.RenderContext, role string, blocks []sdk.Block) x.Content {
	var out x.Content
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			switch role {
			case "assistant":
				out = append(out, x.Block{Text: b.Text, Format: x.Markdown})
			case "user":
				out = append(out, x.Block{Text: b.Text, Format: x.UserText})
			default:
				out = append(out, x.Block{Text: b.Text, Tone: x.Muted})
			}
		case "thinking":
			if b.Text != "" && ctx.ReasoningExpanded {
				out = append(out, x.Block{Text: b.Text, Format: x.Markdown, Tone: x.Muted})
			}
		}
	}
	return out
}

func (m *Model) blocksContent(ctx x.RenderContext, role string, blocks []sdk.Block) x.Content {
	return messageBlocks(ctx, role, blocks)
}

func (m *Model) toolRenderer(ctx x.RenderContext, t x.Tool) (x.Content, bool) {
	if h, ok := m.host.tools[t.Name]; ok {
		return h.Render(ctx, t)
	}
	return nil, false
}

func (m *Model) toolContent(ctx x.RenderContext, t x.Tool) x.Content {
	if rows, handled := m.toolRenderer(ctx, t); handled {
		return rows
	}
	return renderTool(ctx, t, "")
}

func renderTool(ctx x.RenderContext, t x.Tool, arg string) x.Content {
	symbol, tone := "·", x.Muted
	if t.Live {
		symbol, tone = "◌", x.Accent
	}
	if t.Failed {
		symbol, tone = "✗", x.Danger
	}
	label := symbol + " " + t.Name
	if t.Status == "result" {
		label += " result"
	}
	var args map[string]json.RawMessage
	if arg != "" && json.Unmarshal([]byte(t.ArgumentsJSON), &args) == nil {
		var detail string
		_ = json.Unmarshal(args[arg], &detail)
		if detail != "" {
			if arg == "path" {
				detail = "@" + detail
			}
			label += " " + detail
		}
	}
	var out x.Content
	if t.Status != "done" || arg == "path" {
		out = x.Content{{Text: label, Tone: tone}}
	}
	output := t.ResultText
	if output == "" && t.Status == "done" {
		out = append(out, x.Block{Text: "No retained output", Tone: x.Muted})
	}
	if t.Live {
		output = t.Output
	}
	if output != "" && (ctx.Expanded || t.Live || t.Failed) {
		cap := 6
		if ctx.Expanded {
			cap = toolPreviewLines
		}
		if t.Live {
			lines := strings.Split(output, "\n")
			output = strings.Join(lines[max(0, len(lines)-cap):], "\n")
		} else {
			lines := strings.SplitN(output, "\n", cap+1)
			output = strings.Join(lines[:min(len(lines), cap)], "\n")
		}
		format := x.Text
		if t.Name == "edit" && (strings.HasPrefix(output, "@@") || strings.HasPrefix(output, "diff ")) {
			format = x.Diff
		}
		out = append(out, x.Block{Text: output, Format: format})
	}
	return out
}
