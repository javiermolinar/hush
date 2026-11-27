package sdk

import (
	"context"
	"time"
	"unicode/utf8"
)

const SubagentOverviewVersion = 2
const subagentsCapability = "subagents"
const subagentOverview = "ext.subagents.overview"

// Subagent is one worker in the most recent delegated turn. Text is a
// bounded replacement log of public output. It is not a transcript of reasoning.
type Subagent struct {
	ID        ID     `json:"id"`
	Title     string `json:"title"`
	CWD       string `json:"cwd"`
	ReadOnly  bool   `json:"readOnly"`
	Status    string `json:"status"`
	Usage     Usage  `json:"usage"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
	Detail    string `json:"detail,omitempty"`
}

func (s Subagent) Active() bool {
	switch s.Status {
	case "starting", "waiting", "reading", "working", "retrying", "responding", "stopping":
		return true
	}
	return false
}

type SubagentUpdate struct {
	Workers []Subagent
	Usage   Usage // cumulative spend from all subagents in the parent thread
	Err     error
}

// WatchSubagents replaces both the roster and the cumulative usage at each
// update, including after reconnect. Don't add the updates to one another.
func (c *Client) WatchSubagents(ctx context.Context, thread ID) <-chan SubagentUpdate {
	return c.watchSubagents(ctx, thread, 10*time.Second)
}

func (c *Client) watchSubagents(ctx context.Context, thread ID, baselineTimeout time.Duration) <-chan SubagentUpdate {
	w := workspaceWatch{method: subagentOverview, capability: subagentsCapability, version: SubagentOverviewVersion,
		base: subagentsCapability, baseVersions: []uint64{SubagentOverviewVersion}, baselineTimeout: baselineTimeout}
	if thread.Valid() {
		w.params = map[string]any{"threadId": thread}
	}
	return watchWorkspace(ctx, c, w, func(raw any) (SubagentUpdate, string, error) {
		v, err := decode[struct {
			ThreadID ID         `json:"threadId"`
			Cursor   string     `json:"cursor"`
			Workers  []Subagent `json:"workers"`
			Usage    *Usage     `json:"usage"`
		}](raw)
		if err != nil {
			return SubagentUpdate{}, "", err
		}
		if v.ThreadID != thread || v.Usage == nil || v.Workers == nil || len(v.Workers) > 5 {
			return SubagentUpdate{}, "", ErrProtocol
		}
		seen := make(map[ID]bool, len(v.Workers))
		for _, row := range v.Workers {
			if !row.ID.Valid() || seen[row.ID] || row.Title == "" || len(row.Title) > 512 || row.CWD == "" || utf8.RuneCountInString(row.CWD) > 4096 ||
				len(row.Text) > 16*1024 || len(row.Detail) > 512 {
				return SubagentUpdate{}, "", ErrProtocol
			}
			switch row.Status {
			case "ok", "failed", "cancelled", "timed_out":
			default:
				if !row.Active() {
					return SubagentUpdate{}, "", ErrProtocol
				}
			}
			seen[row.ID] = true
		}
		return SubagentUpdate{Workers: v.Workers, Usage: *v.Usage}, v.Cursor, nil
	}, func(err error) SubagentUpdate { return SubagentUpdate{Err: err} })
}
