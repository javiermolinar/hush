package tui

import (
	"context"
	"fmt"

	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

var subagentFixture = sdk.SubagentUpdate{
	Usage: sdk.Usage{TotalTokens: 12000, CostUSD: .08},
	Workers: []sdk.Subagent{
		{ID: "11", Title: "Check the retry path", CWD: "/repo", ReadOnly: true, Status: "reading", Usage: sdk.Usage{TotalTokens: 8000, CostUSD: .05}},
		{ID: "12", Title: "Check cancellation", CWD: "/repo", ReadOnly: true, Status: "ok", Usage: sdk.Usage{TotalTokens: 4000, CostUSD: .03}, Text: "Cancellation leaves the original result intact."},
	},
}

type subagentDockBackend struct {
	Backend
	updates chan sdk.SubagentUpdate
}

func (b *subagentDockBackend) Hello(context.Context) (sdk.Hello, error) {
	return sdk.Hello{Capabilities: map[string]uint64{"subagents": sdk.SubagentOverviewVersion}}, nil
}

func (b *subagentDockBackend) WatchSubagents(context.Context, sdk.ID) <-chan sdk.SubagentUpdate {
	return b.updates
}

func TestSubagentDockLiveLogAndSpend(t *testing.T) {
	m, state := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	b := &subagentDockBackend{Backend: inertBackend{}, updates: make(chan sdk.SubagentUpdate, 1)}
	m.backend = b
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.editor.set("Unsent draft")
	state.info.Usage = sdk.Usage{TotalTokens: 32000, CostUSD: .24}
	state.info.ContextUse = x.ContextUse{Known: true, Tokens: 18000, Window: 500000}
	b.updates <- subagentFixture
	_, watch := m.Update(m.menuProbe()())
	if watch == nil {
		t.Fatal("watch was not started")
	}
	_, next := m.Update(watch())
	frame := ansi.Strip(assertFrame(t, m))
	for _, want := range []string{"Subagents 2 · 1 running", "44.0k tok / $0.32", "3%"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q:\n%s", want, frame)
		}
	}
	if got := m.footerThread(state).ContextUse.Tokens; got != 18000 {
		t.Fatalf("ctx changed with spend: %d", got)
	}
	rule := m.frame.ruleY
	hits := m.frame.hits[rule]
	i := slices.IndexFunc(hits, func(hit spanHit) bool { return hit.action.ID == subagentsID })
	if i < 0 {
		t.Fatal("subagent badge has no hit target")
	}
	click := func(x, y int) { m.Update(tea.MouseClickMsg(tea.Mouse{X: x, Y: y, Button: tea.MouseLeft})) }
	click(hits[i].first, rule)
	roster := m.focusedDialog()
	frame = ansi.Strip(assertFrame(t, m))
	if roster == nil || roster.subagent == nil || !strings.Contains(frame, "Reading files") || m.frame.dialogY+m.frame.dialogHeight != rule {
		t.Fatalf("roster not docked or no live status:\n%s", frame)
	}
	i = slices.Index(m.frame.dialogChoices, 0)
	if i < 0 {
		t.Fatal("no worker row hit")
	}
	click(m.frame.dialogX+1, m.frame.dialogY+i)
	detail := m.focusedDialog()
	if detail == roster || detail.subagent.id != "11" {
		t.Fatal("click did not open worker log")
	}
	frame = ansi.Strip(assertFrame(t, m))
	if !strings.Contains(frame, "No public output yet") {
		t.Fatal(frame)
	}
	update := subagentFixture
	update.Workers = slices.Clone(update.Workers)
	update.Workers[0].Status = "responding"
	update.Workers[0].Text = "Read the retry path.\x1b]52;c;clipboard\a\nChecking the error return."
	b.updates <- update
	m.Update(next())
	raw := assertFrame(t, m)
	if !strings.Contains(ansi.Strip(raw), "Checking the error return.") || strings.Contains(raw, "\x1b]52;") {
		t.Fatalf("live text missing or unsanitized:\n%s", raw)
	}
	press(m, tea.KeyEscape, 0)
	if m.focusedDialog() != roster {
		t.Fatal("Escape did not return to roster")
	}
	press(m, tea.KeyEscape, 0)
	assertFrame(t, m)
	press(m, 'x', tea.ModCtrl)
	press(m, 'i', 0)
	press(m, 'g', 0)
	if d := m.focusedDialog(); d == nil || d.subagent == nil || d.subagent.id.Valid() {
		t.Fatal("leader did not open subagents")
	}
	lease := m.subagents.feedLease
	m.switchThread("2")
	if lease.ctx.Err() == nil || m.focusedDialog() != nil || m.subagentBadge(80).text != "" {
		t.Fatal("old tab retained the live worker view")
	}
	m.Update(subagentMsg{lease: lease.token, update: update, ok: true})
	if m.footerThread(m.threads["2"]).SubagentUsage != (sdk.Usage{}) {
		t.Fatal("stale update became spend on another thread")
	}
	m.switchThread("1")
	if m.editor.value() != "Unsent draft" {
		t.Fatal("inspection changed the draft")
	}
}

func TestGoldenSubagentRoster(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	m, _ := demo(t, Options{Fills: FillOn, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.menuFeaturesFromBackend |= x.Subagents
	m.subagents = subagentFeed{feedLease: feedLease{owner: m.active}, update: subagentFixture, running: 1}
	m.openSubagents(m.route(m.active, coreID))
	golden(t, "subagent-roster", m)
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	preset, _ := presetTheme("panther")
	for _, mode := range []surfaceMode{surfaceTransparent, surfaceTinted, surfaceSolid} {
		preset.surface = mode
		m.applyTheme(preset)
		for _, width := range []int{42, 80} {
			m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
			frame := ansi.Strip(assertFrame(t, m))
			if !strings.Contains(frame, "Subagents") || m.frame.dialogY+m.frame.dialogHeight != m.frame.ruleY {
				t.Fatalf("%s %d: dock not anchored:\n%s", mode, width, frame)
			}
		}
	}
}

func TestSubagentPausedLogKeepsAnchorAndMarksMissingWorker(t *testing.T) {
	m, _ := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.menuFeaturesFromBackend |= x.Subagents
	var lines []string
	for i := 0; i < 32; i++ {
		lines = append(lines, fmt.Sprintf("Line %02d: %s", i, strings.Repeat("内容 ", 30)))
	}
	row := sdk.Subagent{ID: "11", Title: "Check retries", CWD: "/repo", Status: "responding", Text: strings.Join(lines, "\n")}
	m.subagents = subagentFeed{feedLease: feedLease{owner: m.active}, update: sdk.SubagentUpdate{Workers: []sdk.Subagent{row}}, ready: true}
	m.openSubagents(m.route(m.active, coreID))
	assertFrame(t, m)
	press(m, tea.KeyEnter, 0)
	d := m.focusedDialog()
	assertFrame(t, m)
	press(m, tea.KeyHome, 0)
	assertFrame(t, m)
	press(m, tea.KeyPgDown, 0)
	assertFrame(t, m)
	p := d.subagent
	anchor := p.viewer.reviewAnchor()
	if p.follow || anchor == 0 {
		t.Fatal("log did not scroll away from start")
	}
	row.Text += "\nDone reading."
	m.subagents.update.Workers[0] = row
	m.refreshSubagent(d)
	assertFrame(t, m)
	if got := p.viewer.reviewAnchor(); got != anchor {
		t.Fatalf("append moved anchor: %d → %d", anchor, got)
	}
	m.Update(tea.WindowSizeMsg{Width: 52, Height: 24})
	assertFrame(t, m)
	v := p.viewer
	if v.top <= 0 || v.reviewAnchor() > anchor || v.reviewStarts[v.top+1] <= anchor {
		t.Fatalf("resize lost anchor %d: row %d at %d", anchor, v.top, v.reviewAnchor())
	}
	m.subagents.update.Workers = nil
	m.refreshSubagent(d)
	frame := ansi.Strip(assertFrame(t, m))
	if !strings.Contains(frame, "No longer followed") || strings.Contains(frame, "Responding") || p.row.Text != row.Text {
		t.Fatalf("missing worker did not retain its log as inactive:\n%s", frame)
	}
}

func TestUsageUpdatesAfterSubagentBaseline(t *testing.T) {
	m, state := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.menuFeaturesFromBackend |= x.Subagents
	state.info.Usage = sdk.Usage{TotalTokens: 100, CostUSD: .12}
	ctx, cancel := context.WithCancel(m.ctx)
	defer cancel()
	m.subagents.feedLease = feedLease{m.token(), m.active, ctx, cancel}
	budget := maxEffects
	m.openUsage(m.route(m.active, coreID), state, &budget)
	d := m.focusedDialog()
	if !strings.Contains(d.output.raw, "Parent only") || strings.Contains(d.output.raw, "Total tokens:") {
		t.Fatal(d.output.raw)
	}
	m.Update(subagentMsg{lease: m.subagents.token, ok: true, update: sdk.SubagentUpdate{Usage: sdk.Usage{TotalTokens: 50, CostUSD: .08}}})
	if m.focusedDialog() != d || !strings.Contains(d.output.raw, "Total tokens: 150\nSpend: $0.20") || strings.Contains(d.output.raw, "Parent only") {
		t.Fatal(d.output.raw)
	}
}
