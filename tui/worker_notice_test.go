package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
)

func TestReviewNoticeIsNotAParentAnswer(t *testing.T) {
	m := modelFor(t, Options{ThreadID: "1", ReducedMotion: true})
	s := m.threads[m.active]
	at := time.Unix(100, 0)
	report := entry(6, "assistant", "Review · Reader · succeeded · advisory\nCheck the narrow path.")
	report.Kind = workerNoticeEntry
	s.merge([]sdk.Entry{
		entry(1, "user", "hello"), entry(2, "assistant", "The previous parent answer"),
		entry(3, "user", "review"), toolCallEntry(4, "a", "read", `{}`, at),
		toolResultEntry(5, "a", "read", "first", false, at), report,
		toolCallEntry(7, "b", "read", `{}`, at), toolResultEntry(8, "b", "read", "second", false, at),
	})
	if s.lastAnswerID != "2" || !s.entries["6"].visible || s.entries["8"].burstAnchor != "5" {
		t.Fatalf("review altered the parent answer or activity: answer %s, visible %t, anchor %s",
			s.lastAnswerID, s.entries["6"].visible, s.entries["8"].burstAnchor)
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if frame := ansi.Strip(m.View().Content); !strings.Contains(frame, "Check the narrow path.") {
		t.Fatalf("review not visible:\n%s", frame)
	}
}

func TestReviewNoticeCatchUpUsesCommittedOrder(t *testing.T) {
	m := modelFor(t, Options{ThreadID: "1", ReducedMotion: true})
	makeNotice := func(id int, label string) sdk.Entry {
		e := entry(id, "assistant", label)
		e.Kind = workerNoticeEntry
		return e
	}
	baseline := makeNotice(3, "Already present")
	missing := makeNotice(8, "Backfilled review")
	latest := makeNotice(10, "Review in reconnect window")
	answer := entry(12, "assistant", "Parent synthesis")
	feed := func(before, last *sdk.ID, rows ...sdk.Entry) {
		m.receiveFeed(feedUpdate(m, sdk.Snapshot{State: sdk.State{Thread: sdk.Thread{ID: m.active},
			Window: sdk.TranscriptWindow{BeforeEntryID: before, LastEntryID: last, Entries: rows}}}))
	}
	m.beginFeed()
	feed(nil, ptr(sdk.ID("3")), entry(1, "user", "Request"), baseline)
	s := m.threads[m.active]
	original := s.entries["3"]
	if m.notice.text != "" || m.focusedDialog() != nil || s.lastAnswerID != "" {
		t.Fatal("a retained notice affected fresh attachment")
	}
	m.beginFeed()
	feed(ptr(sdk.ID("8")), ptr(sdk.ID("12")), latest, answer)
	if !s.loadingGap || s.through != "3" || s.gapUpper != "12" {
		t.Fatal("reconnect window did not preserve its gap fence")
	}
	m.receiveHistory(historyMsg{lease: m.feed.token, owner: m.active, lower: "3", upper: "12", forward: true,
		page: sdk.HistoryPage{ThreadID: m.active, Entries: []sdk.Entry{missing, latest, answer}}})
	feed(ptr(sdk.ID("8")), ptr(sdk.ID("12")), latest, answer)
	if !slices.Equal(s.ids, []sdk.ID{"1", "3", "8", "10", "12"}) || s.entries["3"] != original ||
		s.through != "12" || s.gapUpper != "" || s.lastAnswerID != "12" || m.notice.text != "" {
		t.Fatalf("catch-up duplicated or reordered notices, or altered the parent answer: %v", s.ids)
	}
}
