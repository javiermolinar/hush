package tui

import (
	"context"
	"slices"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const openConfigOp x.LocalOp = "settings.open"

type settingsBackend interface {
	Settings(context.Context) (sdk.SettingsSnapshot, error)
	ConfigureSettings(context.Context, sdk.SettingsChange) (sdk.SettingsSnapshot, error)
}

type catalogConfigureBackend interface {
	ConfigureCatalogThread(context.Context, sdk.ID, *sdk.Model, string) error
}

type settingsMsg struct {
	settings sdk.SettingsSnapshot
	err      error
}

type configScope uint8

const (
	configThread configScope = iota
	configDefault
)

type configSetting uint8

const (
	configThinking configSetting = iota
	configModelSetting
	configReasoning
)

type configUndo struct {
	scope                                  configScope
	kind                                   configSetting
	settings                               sdk.SettingsSnapshot
	model                                  sdk.Model
	thinking                               string
	reasoningDefault, detailsSet, expanded bool
	liveOverride, liveExpanded             bool
}

type configPanel struct {
	scope        configScope
	selected     configSetting
	settings     sdk.SettingsSnapshot
	levels       map[sdk.Model][]string
	pickingModel bool
	pending      bool
	undo         *configUndo
}

func (m *Model) loadSettings() tea.Cmd {
	backend, ok := m.backend.(settingsBackend)
	if !ok {
		return nil
	}
	ctx := m.ctx
	return func() tea.Msg {
		call, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		settings, err := backend.Settings(call)
		return settingsMsg{settings, err}
	}
}

func (m *Model) applySettings(settings sdk.SettingsSnapshot) {
	reasoningChanged := m.settings.Reasoning != settings.Reasoning
	m.settings = settings
	if d := m.focusedDialog(); d != nil && d.config != nil {
		d.config.settings = settings
	}
	if !reasoningChanged {
		return
	}
	for _, t := range m.threads {
		t.reasoningDefault = settings.Reasoning == sdk.ReasoningExpanded
		t.detailsSet, t.expanded = false, false
		t.liveOverride = false
		t.expandEpoch++
	}
}

func (m *Model) settingsJob(operation x.Operation) (func(context.Context) x.ClientResult, bool) {
	backend, ok := m.backend.(settingsBackend)
	if !ok || !m.menuFeatures().Has(x.Settings) {
		return nil, false
	}
	switch op := operation.(type) {
	case x.ReadSettings:
		return func(ctx context.Context) x.ClientResult {
			call, cancel := context.WithTimeout(ctx, menuTimeout)
			defer cancel()
			settings, err := backend.Settings(call)
			return x.ClientResult{Settings: &settings, Err: err}
		}, true
	case x.WriteSettings:
		return func(ctx context.Context) x.ClientResult {
			call, cancel := context.WithTimeout(ctx, menuTimeout)
			defer cancel()
			settings, err := backend.ConfigureSettings(call, op.Change)
			return x.ClientResult{Settings: &settings, Err: err}
		}, true
	}
	return nil, false
}

func configMenu(x.Context, x.Invocation) x.Outcome {
	return x.Outcome{x.Client{Op: x.ReadSettings{}, Done: func(_ x.Context, result x.ClientResult) x.Outcome {
		if result.Err != nil {
			return clientNote(result, "")
		}
		if result.Settings == nil {
			return showNote("Configuration unavailable", x.Warning)
		}
		return x.Outcome{x.Local{Op: openConfigOp}}
	}}}
}

func (m *Model) openConfig(r ownerRoute, budget *int) tea.Cmd {
	if !m.foreground(r) {
		return nil
	}
	scope := configDefault
	if r.owner.Valid() {
		scope = configThread
	}
	d := &dialog{route: r, token: m.token(), budget: budget, placement: x.Sheet, title: "Configuration",
		config: &configPanel{scope: scope, settings: m.settings, levels: make(map[sdk.Model][]string)}}
	return tea.Batch(m.pushDialog(d), m.loadConfigCatalog(d))
}

func (m *Model) loadConfigCatalog(d *dialog) tea.Cmd {
	backend, ok := m.backend.(modelCatalogSearchBackend)
	if !ok || d.config == nil || m.focusedDialog() != d {
		return nil
	}
	thread := sdk.ID("")
	if d.config.scope == configThread {
		thread = d.route.owner
	}
	return m.startWork(d.route, func(ctx context.Context) (any, error) {
		call, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		return backend.SearchModelCatalog(call, thread, "")
	}, func(value any, err error, _ *int) tea.Cmd {
		if err != nil || m.focusedDialog() != d || d.config == nil {
			return nil
		}
		catalog, ok := value.(sdk.ModelCatalog)
		if !ok {
			return nil
		}
		for _, row := range catalog.Models {
			d.config.levels[row.Model] = slices.Clone(row.ThinkingLevels)
		}
		return nil
	}, d.budget)
}

func (m *Model) configCurrent(d *dialog) (sdk.Model, string, bool) {
	p := d.config
	if p.scope == configDefault {
		return p.settings.Model, p.settings.ThinkingLevel, p.settings.Reasoning == sdk.ReasoningExpanded
	}
	t := m.threads[d.route.owner]
	if t == nil {
		return sdk.Model{}, "", false
	}
	thread := m.footerThread(t)
	if thread.Model == (sdk.Model{}) {
		thread.Model = p.settings.Model
	}
	if thread.Thinking == "" {
		thread.Thinking = p.settings.ThinkingLevel
	}
	return thread.Model, thread.Thinking, t.reasoningDefault
}

func (m *Model) configLevels(d *dialog, model sdk.Model, current string) []string {
	if available := d.config.levels[model]; len(available) != 0 {
		return available
	}
	if current != "" {
		return []string{current}
	}
	return nil
}

func cycleValue(values []string, current string, delta int) string {
	if len(values) == 0 {
		return ""
	}
	i := slices.Index(values, current)
	if i < 0 {
		if delta < 0 {
			return values[len(values)-1]
		}
		return values[0]
	}
	return values[(i+delta+len(values))%len(values)]
}

func compatibleThinking(available []string, current string) string {
	if slices.Contains(available, current) {
		return current
	}
	for _, preferred := range []string{"medium", "high", "low", "off", "minimal", "xhigh", "max"} {
		if slices.Contains(available, preferred) {
			return preferred
		}
	}
	if len(available) != 0 {
		return available[0]
	}
	return current
}

func (m *Model) configInput(d *dialog, msg tea.Msg, chord string) tea.Cmd {
	p := d.config
	if p == nil {
		return nil
	}
	if !m.dialogCanInput(d) && chord != "esc" {
		return nil
	}
	if p.pickingModel {
		return m.configModelInput(d, msg, chord)
	}
	if chord == "esc" {
		return m.closeDialog(d, nil, false)
	}
	if p.pending {
		return nil
	}
	switch chord {
	case "up":
		p.selected = configSetting((int(p.selected) + 2) % 3)
	case "down":
		p.selected = configSetting((int(p.selected) + 1) % 3)
	case "tab":
		if d.route.owner.Valid() {
			if p.scope == configThread {
				p.scope = configDefault
			} else {
				p.scope = configThread
			}
			return m.loadConfigCatalog(d)
		}
	case "left", "right", "enter":
		delta := 1
		if chord == "left" {
			delta = -1
		}
		if p.selected == configModelSetting {
			return m.openConfigModelPicker(d)
		}
		return m.changeConfigValue(d, delta)
	case "/":
		return m.openConfigModelPicker(d)
	case "u":
		return m.undoConfig(d)
	}
	return nil
}

func (m *Model) changeConfigValue(d *dialog, delta int) tea.Cmd {
	model, thinking, reasoning := m.configCurrent(d)
	switch d.config.selected {
	case configThinking:
		next := cycleValue(m.configLevels(d, model, thinking), thinking, delta)
		if next == "" || next == thinking {
			return nil
		}
		return m.applyConfig(d, configThinking, model, next, reasoning)
	case configReasoning:
		return m.applyConfig(d, configReasoning, model, thinking, !reasoning)
	}
	return nil
}

func (m *Model) openConfigModelPicker(d *dialog) tea.Cmd {
	if !m.menuFeatures().Has(x.NativeCatalogSearch) {
		return m.notifyRoute(d.route, "Model search unavailable on this host", x.Warning)
	}
	p := d.config
	p.pickingModel = true
	d.query, d.choices, d.filtered, d.selected = "", nil, nil, 0
	s := &modelSearchState{models: make(map[string]x.CatalogModel)}
	if p.scope == configThread {
		s.thread = d.route.owner
	} else {
		settings := p.settings
		s.settings = &settings
	}
	d.modelSearch = s
	d.token = m.token()
	return m.searchModels(d, 0)
}

func (m *Model) closeConfigModelPicker(d *dialog) {
	if d.modelSearch != nil {
		d.modelSearch.stop()
	}
	d.config.pickingModel = false
	d.modelSearch = nil
	d.query, d.choices, d.filtered, d.selected = "", nil, nil, 0
	d.token = m.token()
}

func (m *Model) configModelInput(d *dialog, msg tea.Msg, chord string) tea.Cmd {
	if chord == "esc" {
		m.closeConfigModelPicker(d)
		return nil
	}
	if d.config.pending {
		return nil
	}
	switch chord {
	case "up":
		d.move(-1)
		return nil
	case "down":
		d.move(1)
		return nil
	case "enter", "ctrl+d":
		choice, ok := d.choice()
		if !ok || d.modelSearch == nil {
			return nil
		}
		picked, ok := d.modelSearch.models[choice.ID]
		if !ok || picked.Unavailable != "" {
			if ok && picked.Unavailable != "" {
				return m.notifyRoute(d.route, picked.Unavailable, x.Warning)
			}
			return nil
		}
		d.config.levels[picked.Model] = slices.Clone(picked.ThinkingLevels)
		_, currentThinking, reasoning := m.configCurrent(d)
		thinking := compatibleThinking(picked.ThinkingLevels, currentThinking)
		if chord == "ctrl+d" {
			d.config.scope = configDefault
			currentThinking = d.config.settings.ThinkingLevel
			thinking = compatibleThinking(picked.ThinkingLevels, currentThinking)
		}
		m.closeConfigModelPicker(d)
		return m.applyConfig(d, configModelSetting, picked.Model, thinking, reasoning)
	case "backspace", "ctrl+h":
		runes := []rune(d.query)
		if len(runes) != 0 {
			return m.changeDialogQuery(d, string(runes[:len(runes)-1]))
		}
		return nil
	}
	if k, ok := msg.(tea.KeyPressMsg); ok && k.Text != "" {
		return m.changeDialogQuery(d, d.query+oneLine(k.Text))
	}
	if paste, ok := msg.(tea.PasteMsg); ok {
		return m.changeDialogQuery(d, d.query+oneLine(paste.Content))
	}
	return nil
}

func (m *Model) configUndoState(d *dialog, kind configSetting) configUndo {
	undo := configUndo{scope: d.config.scope, kind: kind, settings: d.config.settings}
	if t := m.threads[d.route.owner]; t != nil {
		thread := m.footerThread(t)
		undo.model, undo.thinking = thread.Model, thread.Thinking
		undo.reasoningDefault, undo.detailsSet, undo.expanded = t.reasoningDefault, t.detailsSet, t.expanded
		undo.liveOverride, undo.liveExpanded = t.liveOverride, t.liveExpanded
	}
	return undo
}

func (m *Model) applyConfig(d *dialog, kind configSetting, model sdk.Model, thinking string, reasoning bool) tea.Cmd {
	if d.config.scope == configDefault {
		change := sdk.SettingsChange{}
		switch kind {
		case configModelSetting:
			change.Model, change.ThinkingLevel = &model, thinking
		case configThinking:
			change.ThinkingLevel = thinking
		case configReasoning:
			if reasoning {
				change.Reasoning = sdk.ReasoningExpanded
			} else {
				change.Reasoning = sdk.ReasoningFolded
			}
		}
		return m.writeConfigPanel(d, change, m.configUndoState(d, kind), false)
	}

	t := m.threads[d.route.owner]
	if t == nil || !d.route.owner.Valid() {
		return nil
	}
	undo := m.configUndoState(d, kind)
	if kind == configReasoning {
		t.reasoningDefault, t.detailsSet, t.expanded = reasoning, false, false
		t.liveOverride = false
		t.expandEpoch++
		d.config.undo = &undo
		return nil
	}
	previousModel, previousThinking := m.footerThread(t).Model, m.footerThread(t).Thinking
	if kind == configModelSetting {
		t.info.Model = model
	}
	t.info.Thinking = thinking
	t.revision++
	m.emit(d.route.owner, x.Event{Kind: x.StatusChanged})
	d.config.pending = true
	d.config.undo = &undo
	return m.startWork(d.route, func(ctx context.Context) (any, error) {
		call, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		if backend, ok := m.backend.(catalogConfigureBackend); ok && m.menuFeatures().Has(x.NativeCatalog) {
			var ref *sdk.Model
			if kind == configModelSetting {
				ref = &model
			}
			return nil, backend.ConfigureCatalogThread(call, d.route.owner, ref, thinking)
		}
		change := sdk.Configure{ThinkingLevel: sdk.Set(thinking)}
		if kind == configModelSetting {
			change.Model = sdk.Set(model)
		}
		return nil, m.backend.ConfigureThread(call, d.route.owner, change)
	}, func(_ any, err error, _ *int) tea.Cmd {
		if err != nil {
			t.info.Model, t.info.Thinking = previousModel, previousThinking
			t.revision++
			if m.focusedDialog() == d && d.config != nil {
				d.config.pending, d.config.undo = false, nil
			}
			m.emit(d.route.owner, x.Event{Kind: x.StatusChanged})
			return m.notifyRoute(d.route, err.Error(), x.Danger)
		}
		if m.focusedDialog() == d && d.config != nil {
			d.config.pending = false
		}
		return nil
	}, d.budget)
}

func changedSettings(before sdk.SettingsSnapshot, change sdk.SettingsChange) sdk.SettingsSnapshot {
	after := before
	if change.Model != nil {
		after.Model = *change.Model
	}
	if change.ThinkingLevel != "" {
		after.ThinkingLevel = change.ThinkingLevel
	}
	if change.Reasoning != "" {
		after.Reasoning = change.Reasoning
	}
	return after
}

func (m *Model) writeConfigPanel(d *dialog, change sdk.SettingsChange, undo configUndo, undoing bool) tea.Cmd {
	backend, ok := m.backend.(settingsBackend)
	if !ok {
		return m.notifyRoute(d.route, "Configuration unavailable", x.Warning)
	}
	previous := d.config.settings
	optimistic := changedSettings(previous, change)
	d.config.pending = true
	if undoing {
		d.config.undo = nil
	} else {
		d.config.undo = &undo
	}
	m.applySettings(optimistic)
	return m.startWork(d.route, func(ctx context.Context) (any, error) {
		call, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		return backend.ConfigureSettings(call, change)
	}, func(value any, err error, _ *int) tea.Cmd {
		if err != nil {
			m.applySettings(previous)
			if m.focusedDialog() == d && d.config != nil {
				d.config.pending, d.config.undo = false, nil
			}
			return m.notifyRoute(d.route, err.Error(), x.Danger)
		}
		settings, ok := value.(sdk.SettingsSnapshot)
		if !ok {
			m.applySettings(previous)
			if m.focusedDialog() == d && d.config != nil {
				d.config.pending, d.config.undo = false, nil
			}
			return m.notifyRoute(d.route, "Invalid configuration response", x.Danger)
		}
		m.applySettings(settings)
		if m.focusedDialog() == d && d.config != nil {
			d.config.pending = false
		}
		return nil
	}, d.budget)
}

func (m *Model) configMouse(d *dialog, msg tea.Msg, row int) tea.Cmd {
	click, ok := msg.(tea.MouseClickMsg)
	if !ok || click.Mouse().Button != tea.MouseLeft || row < 0 || row >= len(m.frame.dialogChoices) {
		return nil
	}
	target := m.frame.dialogChoices[row]
	if target < 0 {
		return nil
	}
	if d.config.pickingModel {
		d.selected = target
		return m.configModelInput(d, tea.KeyPressMsg{}, "enter")
	}
	setting := configSetting(target)
	if setting == d.config.selected && setting == configModelSetting {
		return m.openConfigModelPicker(d)
	}
	d.config.selected = setting
	return nil
}

func (m *Model) undoConfig(d *dialog) tea.Cmd {
	undo := d.config.undo
	if undo == nil {
		return nil
	}
	d.config.scope = undo.scope
	if undo.scope == configDefault {
		model := undo.settings.Model
		change := sdk.SettingsChange{Model: &model, ThinkingLevel: undo.settings.ThinkingLevel, Reasoning: undo.settings.Reasoning}
		return m.writeConfigPanel(d, change, *undo, true)
	}
	t := m.threads[d.route.owner]
	if t == nil {
		return nil
	}
	if undo.kind == configReasoning {
		t.reasoningDefault, t.detailsSet, t.expanded = undo.reasoningDefault, undo.detailsSet, undo.expanded
		t.liveOverride, t.liveExpanded = undo.liveOverride, undo.liveExpanded
		t.expandEpoch++
		d.config.undo = nil
		return nil
	}
	currentModel, currentThinking := m.footerThread(t).Model, m.footerThread(t).Thinking
	t.info.Model, t.info.Thinking = undo.model, undo.thinking
	t.revision++
	d.config.pending, d.config.undo = true, nil
	m.emit(d.route.owner, x.Event{Kind: x.StatusChanged})
	return m.startWork(d.route, func(ctx context.Context) (any, error) {
		call, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		if backend, ok := m.backend.(catalogConfigureBackend); ok && m.menuFeatures().Has(x.NativeCatalog) {
			model := undo.model
			return nil, backend.ConfigureCatalogThread(call, d.route.owner, &model, undo.thinking)
		}
		return nil, m.backend.ConfigureThread(call, d.route.owner, sdk.Configure{Model: sdk.Set(undo.model), ThinkingLevel: sdk.Set(undo.thinking)})
	}, func(_ any, err error, _ *int) tea.Cmd {
		if err != nil {
			t.info.Model, t.info.Thinking = currentModel, currentThinking
			t.revision++
			if m.focusedDialog() == d && d.config != nil {
				d.config.pending = false
			}
			m.emit(d.route.owner, x.Event{Kind: x.StatusChanged})
			return m.notifyRoute(d.route, err.Error(), x.Danger)
		}
		if m.focusedDialog() == d && d.config != nil {
			d.config.pending = false
		}
		return nil
	}, d.budget)
}
