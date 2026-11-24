package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const maxQueueRows = 3
const maxAdmissionPreviews = 8

// These bridges are only for replies that beat the watch. An observed queue
// item lives in the snapshot instead and never expires on a local timer.
func (t *threadState) rememberAdmission(item sdk.QueueItem) {
	if len(t.admittedInputs) == maxAdmissionPreviews {
		t.admittedInputs = slices.Delete(t.admittedInputs, 0, 1)
	}
	t.admittedInputs = append(t.admittedInputs, item)
}

func (t *threadState) forgetAdmission(id sdk.ID) {
	t.admittedInputs = slices.DeleteFunc(t.admittedInputs, func(item sdk.QueueItem) bool { return item.SubmissionID == id })
}

type queueResolved admissionExpired

func (m *Model) probeQueuePlacement(owner, id sdk.ID, life uint64) tea.Cmd {
	backend, ok := m.backend.(interface {
		GetSubmission(context.Context, sdk.ID, sdk.ID) (sdk.Submission, error)
	})
	if !ok {
		return nil
	}
	// The watch can coalesce the entire queued-to-placed transition. Check
	// the individual receipt without guessing from nearby transcript text.
	ctx := m.ctx
	return func() tea.Msg {
		call, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		row, err := backend.GetSubmission(call, owner, id)
		if err == nil && row.ThreadID == owner && row.SubmissionID == id && (row.EntryID != nil || row.Terminal()) {
			return queueResolved{owner, id, life}
		}
		return nil
	}
}

func (t *threadState) queueRows(w, limit int) []displayRow {
	limit = min(maxQueueRows, limit)
	if limit <= 0 {
		return nil
	}
	seen, admitted := len(t.queue), len(t.admittedInputs)
	total := seen + admitted
	if t.sendingInput.Mode != "" {
		total++
	}
	if total == 0 {
		return nil
	}
	rows := make([]displayRow, 0, min(limit, total))
	start := max(0, total-limit)
	if start > 0 && limit > 1 {
		start++ // Reserve one row for the hidden entries, without scanning them.
		rows = append(rows, displayRow{text: paint(x.Muted, fmt.Sprintf("… %d earlier pending", start))})
	}
	for i := start; i < total; i++ {
		var item sdk.QueueItem
		var lead string
		switch {
		case i < seen:
			item = t.queue[i]
			lead = queueMode(item.Mode) + " queued"
		case i < seen+admitted:
			item = t.admittedInputs[i-seen]
			lead = queueMode(item.Mode) + " admitted"
		default:
			item = t.sendingInput
			lead = "sending " + queueMode(item.Mode)
		}
		if limit == 1 && start > 0 {
			lead += fmt.Sprintf(" (+%d)", start)
		}
		lead = "↳ " + lead + " · "
		preview := oneLine(limitText(item.Text, 512))
		if strings.TrimSpace(preview) == "" {
			preview = "(no text)"
		}
		rows = append(rows, displayRow{text: paint(x.Muted, lead) + ansi.Truncate(preview, max(0, w-stringWidth(lead)), "…")})
	}
	return rows
}

func queueMode(mode string) string {
	switch sdk.BusyMode(mode) {
	case sdk.Steer:
		return "steer"
	case sdk.FollowUp:
		return "follow-up"
	default:
		return "input"
	}
}
