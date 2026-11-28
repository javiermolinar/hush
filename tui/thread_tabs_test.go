package tui

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
)

func TestThreadOpenRegistersBackgroundTabWithoutLosingDraft(t *testing.T) {
	tabs := NewThreadTabs()
	m, _ := demo(t, Options{ThreadTabs: tabs, Fills: FillOff, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.editor.set("Keep the draft")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for _, id := range []sdk.ID{"9", "9"} {
		before := slices.Clone(m.tabs)
		result := make(chan error, 1)
		go func() { result <- tabs.Open(ctx, id) }()
		msg, ok := tabs.wait(ctx)().(tabOpenMsg)
		if !ok || !slices.Equal(m.tabs, before) {
			t.Fatalf("tab changed off the loop or request was lost: %v", m.tabs)
		}
		m.Update(msg)
		if r := <-result; r != nil {
			t.Fatal(r)
		}
		if m.active != "1" || m.editor.value() != "Keep the draft" || !slices.Equal(m.tabs, []sdk.ID{"1", "9"}) {
			t.Fatalf("unexpected tab, focus, or draft: %v %s %q", m.tabs, m.active, m.editor.value())
		}
	}
}

func TestThreadOpenRejectsCanceledOrFullTabBar(t *testing.T) {
	m, _ := demo(t, Options{ThreadTabs: NewThreadTabs()})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	replies := make(chan error, 1)
	m.Update(tabOpenMsg{ctx: ctx, id: "500", result: replies})
	if r := <-replies; r == nil || len(m.tabs) != 1 {
		t.Fatal(r, m.tabs)
	}
	for i := 2; i <= maxOpenTabs; i++ {
		id := sdk.ID(strconv.Itoa(i))
		m.ensureThread(id)
		m.openedTab(id)
	}
	m.Update(tabOpenMsg{ctx: t.Context(), id: "500", result: replies})
	if r := <-replies; r == nil || len(m.tabs) != maxOpenTabs || m.tabs[0] != "1" {
		t.Fatal(r, m.tabs)
	}
}

func TestThreadFocusOpensAndSwitchesKeepingTheDraft(t *testing.T) {
	tabs := NewThreadTabs()
	m, _ := demo(t, Options{ThreadTabs: tabs, Fills: FillOff, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.editor.set("Keep the draft")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- tabs.Focus(ctx, "12") }()
	msg, ok := tabs.wait(ctx)().(tabOpenMsg)
	if !ok || !msg.focus {
		t.Fatal("focus request was lost")
	}
	m.Update(msg)
	if r := <-result; r != nil {
		t.Fatal(r)
	}
	if m.active != "12" || !slices.Equal(m.tabs, []sdk.ID{"1", "12"}) || m.threads["1"].draft.text != "Keep the draft" {
		t.Fatalf("focus did not switch or lost the draft: %v %s %q", m.tabs, m.active, m.threads["1"].draft.text)
	}
}
