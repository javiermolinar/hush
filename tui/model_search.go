package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const modelSearchOp x.LocalOp = "model.search"
const modelSearchDelay = 120 * time.Millisecond

type modelSearchState struct {
	thread   sdk.ID
	settings *sdk.SettingsSnapshot
	models   map[string]x.CatalogModel
	cancel   context.CancelFunc
	failed   bool
}

func (s *modelSearchState) stop() {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

type modelSearchResult struct {
	token   uint64
	catalog sdk.ModelCatalog
	err     error
}

func (m *Model) openModelSearch(r ownerRoute, thread sdk.ID, query string, budget *int) tea.Cmd {
	features := m.menuFeatures()
	if !m.foreground(r) || !features.Has(x.NativeCatalogSearch) {
		return nil
	}
	if !thread.Valid() {
		if thread != "" || r.owner != "" || !features.Has(x.NativeCatalogDraft) {
			return nil
		}
		if m.threads[r.owner].sending {
			return m.notifyRoute(r, "Wait for the pending request", x.Warning)
		}
	}
	s := &modelSearchState{thread: thread}
	d := &dialog{route: r, budget: budget, placement: x.Sheet, title: "Select model",
		query: limitText(oneLine(limitText(query, selectQueryMaxBytes)), selectQueryMaxBytes), modelSearch: s}
	d.done = func(_ x.Context, value any, accepted bool) x.Outcome {
		key, _ := value.(string)
		if !accepted {
			return nil
		}
		return modelPickerSelection(thread, s.models, key, true)
	}
	return tea.Batch(m.pushDialog(d), m.searchModels(d, 0))
}

func (m *Model) searchModels(d *dialog, delay time.Duration) tea.Cmd {
	backend, ok := m.backend.(modelCatalogSearchBackend)
	if !ok || m.focusedDialog() != d || !m.foreground(d.route) {
		return nil
	}
	s := d.modelSearch
	s.stop()
	// Results and click targets for the old query are no longer selectable.
	d.choices, d.filtered, d.selected = nil, nil, 0
	s.models, s.failed = nil, false
	d.token = m.token()
	token, thread, query := d.token, s.thread, d.query
	ctx, cancel := context.WithCancel(m.ctx)
	s.cancel = cancel
	return func() tea.Msg {
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return nil
			case <-timer.C:
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		call, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		catalog, err := backend.SearchModelCatalog(call, thread, query)
		return modelSearchResult{token, catalog, err}
	}
}

func (m *Model) receiveModelSearch(v modelSearchResult) {
	d := m.focusedDialog()
	if d == nil || d.modelSearch == nil || d.token != v.token || !m.foreground(d.route) {
		return
	}
	d.modelSearch.stop()
	d.token = m.token() // require a fresh frame for mouse hit-testing
	if v.err != nil {
		d.modelSearch.failed = true
		return
	}
	if d.modelSearch.settings != nil {
		model := d.modelSearch.settings.Model
		v.catalog.Current = &model
	} else if d.modelSearch.thread == "" {
		if model := m.threads[d.route.owner].info.Model; model != (sdk.Model{}) {
			v.catalog.Current = &model // locally staged, not a saved thread setting
		}
	}
	rows := nativeModels(v.catalog, x.ListModels{Limit: sdk.ModelCatalogLimit})
	d.choices, d.modelSearch.models = modelPickerChoices(rows.Models)
	d.filter() // results are already matched against the backend's catalog
}

// Model display names need not match catalog references. Keep the original
// choice text for filtering and put the reference and availability in detail.
func (d *dialog) modelMenuItem(c x.Choice, focused bool) (menuItem, string) {
	item := menuItem{title: c.Label, focus: focused, disabled: c.Disabled}
	if d.modelSearch == nil {
		return item, c.Description
	}
	model, ok := d.modelSearch.models[c.ID]
	if !ok {
		return item, c.Description
	}
	ref := modelRef(model.Model)
	if model.Label != "" {
		item.title = model.Label
	} else {
		item.title = ref
	}
	var details []string
	if ref != item.title {
		details = append(details, ref)
	}
	if model.Unavailable != "" {
		item.icon, item.state, item.tone = "⏸", "Unavailable", x.Warning
		details = append(details, model.Unavailable)
	} else if model.Current {
		item.icon, item.state, item.tone = "○", "Current", x.Muted
	}
	if len(model.ThinkingLevels) != 0 {
		details = append(details, "Thinking: "+strings.Join(model.ThinkingLevels, ", "))
	}
	return item, strings.Join(details, "\n")
}
