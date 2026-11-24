package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

type configThreadChange struct {
	thread   sdk.ID
	model    *sdk.Model
	thinking string
}

type configFixture struct {
	modelMenuFixture
	settings      sdk.SettingsSnapshot
	changes       []sdk.SettingsChange
	threadChanges []configThreadChange
}

func (f *configFixture) Hello(context.Context) (sdk.Hello, error) {
	return sdk.Hello{Capabilities: map[string]uint64{"modelCatalog": sdk.ModelCatalogDraftVersion, "settings": sdk.SettingsVersion}}, nil
}

func (f *configFixture) SearchModelCatalog(_ context.Context, thread sdk.ID, query string) (sdk.ModelCatalog, error) {
	if query != "" && !strings.Contains("fixture/two", query) {
		return sdk.ModelCatalog{Default: f.settings.Model, Models: []sdk.ModelCatalogRow{}}, nil
	}
	catalog := sdk.ModelCatalog{Default: f.settings.Model, Models: []sdk.ModelCatalogRow{
		{Model: sdk.Model{Provider: "fixture", ModelID: "one"}, Label: "Fixture one", Available: true, ThinkingLevels: []string{"off", "low", "high"}},
		{Model: sdk.Model{Provider: "fixture", ModelID: "two"}, Label: "Fixture two", Available: true, ThinkingLevels: []string{"off", "low", "high"}},
	}}
	if thread != "" {
		current := sdk.Model{Provider: "fixture", ModelID: "one"}
		catalog.Current = &current
	}
	return catalog, nil
}

func (f *configFixture) NewCatalogThread(context.Context, string, sdk.Model, string) (sdk.Thread, error) {
	return sdk.Thread{}, sdk.ErrProtocol
}

func (f *configFixture) ConfigureCatalogThread(_ context.Context, thread sdk.ID, model *sdk.Model, thinking string) error {
	var copied *sdk.Model
	if model != nil {
		value := *model
		copied = &value
	}
	f.threadChanges = append(f.threadChanges, configThreadChange{thread: thread, model: copied, thinking: thinking})
	return nil
}

func (f *configFixture) Settings(context.Context) (sdk.SettingsSnapshot, error) {
	return f.settings, nil
}

func (f *configFixture) ConfigureSettings(_ context.Context, change sdk.SettingsChange) (sdk.SettingsSnapshot, error) {
	f.changes = append(f.changes, change)
	if change.Model != nil {
		f.settings.Model = *change.Model
	}
	if change.ThinkingLevel != "" {
		f.settings.ThinkingLevel = change.ThinkingLevel
	}
	if change.Reasoning != "" {
		f.settings.Reasoning = change.Reasoning
	}
	return f.settings, nil
}

func configModel() sdk.SettingsSnapshot {
	return sdk.SettingsSnapshot{Model: sdk.Model{Provider: "fixture", ModelID: "one"}, ThinkingLevel: "high", Reasoning: sdk.ReasoningFolded}
}

func TestConfigDockAndDefaultModel(t *testing.T) {
	m, _ := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	fixture := &configFixture{settings: configModel()}
	m.backend = fixture
	m.Update(m.menuProbe()())
	m.Update(tea.WindowSizeMsg{Width: 84, Height: 22})
	m.Update(m.loadSettings()())
	m.editor.set("keep this draft")
	m.finishWork(m.runAction(m.host.byID[x.ActionID(menusID+".config")], "")().(workMsg))
	d := m.focusedDialog()
	if d == nil || d.placement != x.Sheet || d.config == nil || len(m.overlays) != 0 {
		t.Fatal("configuration did not open as a dock")
	}
	d.config.levels[fixture.settings.Model] = []string{"off", "low", "high"}
	if got := m.editor.value(); got != "keep this draft" {
		t.Fatalf("configuration changed the draft: %q", got)
	}
	golden(t, "menu-config", m)

	press(m, tea.KeyTab, 0)  // defaults
	press(m, tea.KeyDown, 0) // model
	cmd := press(m, tea.KeyEnter, 0)
	if d.modelSearch == nil || !d.config.pickingModel || cmd == nil {
		t.Fatal("inline default model search did not open")
	}
	m.Update(cmd())
	golden(t, "menu-config-model", m)
	press(m, tea.KeyDown, 0)
	cmd = press(m, tea.KeyEnter, 0)
	m.finishWork(cmd().(workMsg))
	if len(fixture.changes) != 1 || fixture.changes[0].Model == nil || fixture.changes[0].Model.ModelID != "two" || fixture.changes[0].ThinkingLevel != "high" {
		t.Fatalf("unexpected one-step default change: %+v", fixture.changes)
	}
	if d != m.focusedDialog() || d.config.pickingModel || d.config.pending || d.config.settings.Model.ModelID != "two" {
		t.Fatal("model change did not return to the configuration dock")
	}

	press(m, tea.KeyUp, 0) // thinking
	cmd = press(m, tea.KeyLeft, 0)
	m.finishWork(cmd().(workMsg))
	if len(fixture.changes) != 2 || fixture.settings.ThinkingLevel != "low" {
		t.Fatalf("thinking did not change in place: %+v", fixture.changes)
	}
	cmd = press(m, 'u', 0)
	m.finishWork(cmd().(workMsg))
	if fixture.settings.ThinkingLevel != "high" || d.config.undo != nil {
		t.Fatal("undo did not restore the previous default")
	}
	press(m, tea.KeyEscape, 0)
	if m.focusedDialog() != nil || m.editor.value() != "keep this draft" || !strings.Contains(assertFrame(t, m), "keep this draft") {
		t.Fatal("Escape did not restore the unchanged input")
	}
}

func TestConfigAccessibleFromMainPalette(t *testing.T) {
	m, _ := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	fixture := &configFixture{settings: configModel()}
	m.backend = fixture
	m.Update(m.menuProbe()())
	m.Update(m.loadSettings()())
	m.Update(tea.WindowSizeMsg{Width: 84, Height: 22})
	m.runAction(m.host.byID[paletteID], "")
	palette := m.focusedDialog()
	palette.query = "configuration"
	palette.filter()
	cmd := m.acceptChoice(palette)
	m.finishWork(cmd().(workMsg))
	if m.sheet == nil || m.sheet.config == nil || len(m.overlays) != 0 || m.focusedDialog() == palette {
		t.Fatal("configuration dock did not open from the main palette")
	}
	press(m, tea.KeyEscape, 0)
	if m.focusedDialog() != nil {
		t.Fatal("Escape did not restore the input")
	}
}

func testConfigFrameGeometry(t *testing.T) {
	for _, size := range [][2]int{{40, 12}, {64, 16}, {120, 30}} {
		m, _ := demo(t, Options{Fills: FillOff, ReducedMotion: true})
		fixture := &configFixture{settings: configModel()}
		m.backend = fixture
		m.Update(m.menuProbe()())
		m.Update(m.loadSettings()())
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m.finishWork(m.runAction(m.host.byID[x.ActionID(menusID+".config")], "")().(workMsg))
		frame := assertFrame(t, m)
		if again := assertFrame(t, m); again != frame {
			t.Fatalf("config frame changed at %v", size)
		}
		press(m, tea.KeyDown, 0)
		if cmd := press(m, tea.KeyEnter, 0); cmd != nil {
			m.Update(cmd())
		}
		frame = assertFrame(t, m)
		if again := assertFrame(t, m); again != frame {
			t.Fatalf("config model frame changed at %v", size)
		}
	}
}

func TestConfigReasoningScopeAndInlineModelPicker(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	fixture := &configFixture{settings: configModel()}
	m.backend = fixture
	m.Update(m.menuProbe()())
	m.Update(m.loadSettings()())
	open := m.runAction(m.host.byID[x.ActionID(menusID+".config")], "")
	m.finishWork(open().(workMsg))
	d := m.focusedDialog()
	d.config.levels[fixture.settings.Model] = []string{"off", "low", "high"}

	cmd := press(m, tea.KeyRight, 0)
	m.finishWork(cmd().(workMsg))
	if len(fixture.threadChanges) != 1 || fixture.threadChanges[0].thinking != "off" || fixture.threadChanges[0].model != nil {
		t.Fatalf("thread thinking did not use thread scope: %+v", fixture.threadChanges)
	}
	press(m, tea.KeyDown, 0)
	press(m, tea.KeyDown, 0)
	press(m, tea.KeyRight, 0)
	if !s.reasoningDefault || m.settings.Reasoning != sdk.ReasoningFolded || len(fixture.changes) != 0 {
		t.Fatal("thread reasoning did not apply locally")
	}
	press(m, tea.KeyTab, 0)
	cmd = press(m, tea.KeyRight, 0)
	m.finishWork(cmd().(workMsg))
	if m.settings.Reasoning != sdk.ReasoningExpanded || len(fixture.changes) != 1 || fixture.changes[0].Reasoning != sdk.ReasoningExpanded {
		t.Fatalf("default reasoning setting was not persisted: %+v", fixture.changes)
	}
	golden(t, "menu-config-reasoning", m)

	press(m, tea.KeyUp, 0) // model
	cmd = press(m, '/', 0)
	m.Update(cmd())
	if !d.config.pickingModel || d.modelSearch == nil {
		t.Fatal("model picker did not reuse the dock")
	}
	press(m, tea.KeyEscape, 0)
	if d.config.pickingModel || m.focusedDialog() != d {
		t.Fatal("Escape did not return from model search to settings")
	}

	press(m, tea.KeyTab, 0) // this thread
	cmd = press(m, '/', 0)
	m.Update(cmd())
	press(m, tea.KeyDown, 0)
	cmd = press(m, 'd', tea.ModCtrl)
	m.finishWork(cmd().(workMsg))
	last := fixture.changes[len(fixture.changes)-1]
	if d.config.scope != configDefault || last.Model == nil || last.Model.ModelID != "two" {
		t.Fatalf("ctrl+d did not make the picked model the default: %+v", last)
	}
}

func TestReasoningExpandedIndependentOfToolDetails(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	thought := entry(3, "assistant", "")
	thought.Messages[0].Blocks = []sdk.Block{{Type: "thinking", Text: "Inspect the stored reasoning"}}
	s.merge([]sdk.Entry{thought})
	s.stream = detachStream(&sdk.Streaming{Blocks: []sdk.Block{{Type: "thinking", Text: "Live reasoning"}}})
	s.streamRevision++
	m.Update(tea.WindowSizeMsg{Width: 84, Height: 20})

	if rows := m.blockRows(s, len(s.visibleIDs()), 80); strings.Contains(ansi.Strip(rowsText(rows)), "Live reasoning") {
		t.Fatal("folded mode exposed live reasoning")
	}
	m.applySettings(sdk.SettingsSnapshot{Model: configModel().Model, ThinkingLevel: "high", Reasoning: sdk.ReasoningExpanded})
	index := s.shownIndex("3")
	if index < 0 {
		t.Fatal("thinking-only record stayed hidden")
	}
	rows := m.blockRows(s, index, 80)
	if !strings.Contains(ansi.Strip(rowsText(rows)), "Inspect the stored reasoning") {
		t.Fatalf("committed reasoning missing: %s", rowsText(rows))
	}
	live := m.blockRows(s, len(s.visibleIDs()), 80)
	if !strings.Contains(ansi.Strip(rowsText(live)), "Live reasoning") {
		t.Fatalf("live reasoning missing: %s", rowsText(live))
	}
	ctx := m.renderContext(s, 80, recordRows, false)
	if ctx.Expanded || !ctx.ReasoningExpanded {
		t.Fatalf("reasoning leaked into tool expansion: %+v", ctx)
	}

	m.localEffect(m.route(m.active, coreID), x.Local{Op: x.ToggleDetails}, nil)
	if !s.detailsSet || s.expanded || s.reasoningVisible() {
		t.Fatal("Ctrl+O did not temporarily collapse reasoning")
	}
	m.localEffect(m.route(m.active, coreID), x.Local{Op: x.ToggleDetails}, nil)
	if !s.expanded || !s.reasoningVisible() {
		t.Fatal("second Ctrl+O did not expand all details")
	}
}

func rowsText(rows []displayRow) string {
	var out strings.Builder
	for _, row := range rows {
		out.WriteString(row.text)
		out.WriteByte('\n')
	}
	return out.String()
}
