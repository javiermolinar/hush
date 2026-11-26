package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestGoldenPromptWithQueueAndAttachment(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	s.info.Status = x.Running
	s.queue = []sdk.QueueItem{{Mode: string(sdk.FollowUp), Text: "Review the changes"}}
	s.attachments = []draftImage{{label: "Screenshot"}}
	m.editor.set("Keep the draft\nSecond line")
	golden(t, "prompt-queue-attachment", m)
	if cursor := m.View().Cursor; cursor == nil || cursor.Position.Y != m.frame.ruleY+2 {
		t.Fatal("the multiline prompt lost its cursor")
	}
}
