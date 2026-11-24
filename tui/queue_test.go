package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
)

func TestQueuedInputsStayVisibleUntilPlacement(t *testing.T) {
	m, state := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	m.backend = admitOnlyBackend{}
	snap := sdk.State{Thread: sdk.Thread{ID: "1"}, Run: &sdk.Run{TaskID: "20"}}
	feed := func() {
		m.receiveFeed(feedMsg{owner: "1", ok: true, update: sdk.StateUpdate{Snapshot: sdk.Snapshot{State: snap}}})
	}
	feed()
	viewHas := func(want string) string {
		t.Helper()
		body := ansi.Strip(assertFrame(t, m))
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
		return body
	}
	const text = "use the cache"
	m.editor.set(text)
	cmd := m.sendPrompt(sdk.Steer)
	viewHas("sending steer · " + text)
	m.finishWork(cmd().(workMsg))
	viewHas("steer admitted · " + text)

	// A later follow-up and an unrelated commit must not hide the steer.
	snap.Queue = []sdk.QueueItem{
		{SubmissionID: "42", Mode: "steer", Text: text},
		{SubmissionID: "43", Mode: "followUp", Text: "then review"},
	}
	snap.Window = sdk.TranscriptWindow{LastEntryID: ptr(sdk.ID("3")), Entries: []sdk.Entry{entry(3, "assistant", "previous step")}}
	feed()
	// A local admission timeout must not expire a snapshot-backed queued item.
	m.Update(admissionExpired{"1", "42", state.life})
	m.editor.set("next draft")
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	body := viewHas("steer queued · " + text)
	if strings.Count(body, text) != 1 || !strings.Contains(body, "follow-up queued · then review") || !strings.Contains(body, "next draft") {
		t.Fatalf("queue duplicated or clipped the draft:\n%s", body)
	}
	if len(state.admittedInputs) != 0 || len(state.ids) != 3 {
		t.Fatal("queue preview was not replaced by the watch, or entered history before placement")
	}

	// Placement is one snapshot: remove the pending row and draw the real
	// transcript entry. The queued follow-up and the draft are unaffected.
	snap.Run.SubmissionIDs = []sdk.ID{"42"}
	snap.Queue = snap.Queue[1:]
	snap.Window = sdk.TranscriptWindow{LastEntryID: ptr(sdk.ID("4")), Entries: []sdk.Entry{entry(4, "user", text)}}
	feed()
	body = viewHas("│ " + text)
	if strings.Contains(body, "steer queued") || strings.Count(body, text) != 1 || !strings.Contains(body, "follow-up queued") || m.editor.value() != "next draft" {
		t.Fatalf("bad handoff to the transcript:\n%s", body)
	}
	// Withdrawal is not placement and cannot add another user entry.
	snap.Queue = nil
	feed()
	if strings.Contains(ansi.Strip(assertFrame(t, m)), "then review") || len(state.ids) != 4 {
		t.Fatal("withdrawn follow-up remained visible or entered history")
	}
}

type placementFixture struct {
	admitOnlyBackend
	row sdk.Submission
}

func (b placementFixture) GetSubmission(context.Context, sdk.ID, sdk.ID) (sdk.Submission, error) {
	return b.row, nil
}

func TestUnseenAdmissionClearsOnVerifiedPlacement(t *testing.T) {
	m, state := demo(t, Options{})
	state.rememberAdmission(sdk.QueueItem{SubmissionID: "42", Mode: "steer", Text: "fast steer"})
	row := sdk.Submission{ThreadID: "1", SubmissionID: "42", Status: "queued"}
	m.backend = placementFixture{row: row}
	if msg := m.probeQueuePlacement("1", "42", state.life)(); msg != nil {
		t.Fatal("a queue receipt is not placement")
	}
	row.Status, row.EntryID = "placed", ptr(sdk.ID("3"))
	m.backend = placementFixture{row: row}
	m.Update(m.probeQueuePlacement("1", "42", state.life)())
	if len(state.admittedInputs) != 0 {
		t.Fatal("coalesced placement left a local preview")
	}
}
