package tui

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

// coldStartBackend misses its first capability probes, as a backend that is
// still starting can, then negotiates prospective file browsing and the
// native model catalog. It never creates threads.
type coldStartBackend struct {
	inertBackend
	hellos, created *atomic.Int32
	misses          int32
}

func (b coldStartBackend) Hello(context.Context) (sdk.Hello, error) {
	if b.hellos.Add(1) <= b.misses {
		return sdk.Hello{}, &sdk.TransportError{Err: io.EOF}
	}
	return sdk.Hello{Capabilities: map[string]uint64{
		"workspaceFiles": sdk.WorkspaceFileProjectPathsVersion, "modelCatalog": sdk.ModelCatalogDraftVersion}}, nil
}

func (b coldStartBackend) NewThread(context.Context, sdk.NewThread) (sdk.Thread, error) {
	b.created.Add(1)
	return sdk.Thread{}, x.ErrUnavailable
}

func (coldStartBackend) WorkspaceFileScope(_ context.Context, target sdk.WorkspaceFileTarget) (sdk.WorkspaceFileScope, error) {
	return sdk.WorkspaceFileScope{Workspace: target == sdk.WorkspaceFileTarget{CWD: "/workspace"}}, nil
}

func (coldStartBackend) ListWorkspaceFiles(context.Context, sdk.WorkspaceFileTarget, string, string, int, string) (sdk.WorkspaceFilePage, error) {
	return sdk.WorkspaceFilePage{Entries: []sdk.WorkspaceFileEntry{{Name: "README.md"}}}, nil
}

func (coldStartBackend) SearchWorkspaceFiles(context.Context, sdk.WorkspaceFileTarget, string, int) (sdk.WorkspaceFileSearchResult, error) {
	return sdk.WorkspaceFileSearchResult{}, nil
}

func (coldStartBackend) ModelCatalog(context.Context, sdk.ID) (sdk.ModelCatalog, error) {
	return fullModelMenuFixture(), nil
}

func (coldStartBackend) SearchModelCatalog(_ context.Context, thread sdk.ID, _ string) (sdk.ModelCatalog, error) {
	if thread != "" {
		return sdk.ModelCatalog{}, sdk.ErrProtocol
	}
	return fullModelMenuFixture(), nil
}

func (coldStartBackend) ConfigureCatalogThread(context.Context, sdk.ID, *sdk.Model, string) error {
	return sdk.ErrProtocol
}

func (coldStartBackend) NewCatalogThread(context.Context, string, sdk.Model, string) (sdk.Thread, error) {
	return sdk.Thread{}, x.ErrUnavailable
}

func (coldStartBackend) Intro(context.Context, sdk.IntroScope) (sdk.IntroReport, error) {
	return sdk.IntroReport{}, sdk.ErrProtocol
}

func (coldStartBackend) WatchIntro(context.Context, sdk.IntroScope) <-chan sdk.IntroUpdate {
	return nil // tests deliver report observations directly
}

func freshColdStart(t *testing.T, misses int32) (*Model, coldStartBackend) {
	t.Helper()
	backend := coldStartBackend{hellos: new(atomic.Int32), created: new(atomic.Int32), misses: misses}
	m := modelFor(t, Options{ReducedMotion: true, WorkspaceLocal: true})
	m.backend = backend
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	return m, backend
}

// reply runs a command tree concurrently, as Bubble Tea would, and returns its
// first message of type T. Waits that never resolve end with the model.
func reply[T tea.Msg](t *testing.T, cmd tea.Cmd) T {
	t.Helper()
	out := make(chan tea.Msg, 64)
	var run func(tea.Cmd)
	run = func(c tea.Cmd) {
		if c == nil {
			return
		}
		go func() {
			msg := c()
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, c := range batch {
					run(c)
				}
				return
			}
			select {
			case out <- msg:
			default:
			}
		}()
	}
	run(cmd)
	timeout := time.After(5 * time.Second)
	for {
		select {
		case msg := <-out:
			if v, ok := msg.(T); ok {
				return v
			}
		case <-timeout:
			var zero T
			t.Fatalf("no %T reply", zero)
			return zero
		}
	}
}

func TestFreshSessionRetriesMissedCapabilityProbe(t *testing.T) {
	m, backend := freshColdStart(t, 1)
	_, retry := m.Update(m.menuProbe()()) // Init's probe misses the cold backend
	scheduled := m.probe.retry
	if retry == nil || scheduled == 0 || m.probe.pending || m.fileAvailable("") ||
		m.menuFeatures().Has(x.NativeCatalogDraft) || m.fileScope.generation != 0 {
		t.Fatal("a missed probe must schedule one retry and enable nothing")
	}
	_, probe := m.Update(reply[menuProbeRetry](t, retry)) // after the first backoff delay
	if probe == nil || !m.probe.pending {
		t.Fatal("the retry did not probe again")
	}
	m.Update(reply[menuCapabilities](t, probe))
	if backend.hellos.Load() != 2 || !m.probe.known || m.probe.pending || m.probe.retry != 0 {
		t.Fatalf("second probe not applied: hellos=%d %+v", backend.hellos.Load(), m.probe)
	}

	// File browsing: the prospective launch-directory scope, with no thread.
	if m.fileScope.target != (sdk.WorkspaceFileTarget{CWD: "/workspace"}) {
		t.Fatalf("no prospective scope request: %+v", m.fileScope)
	}
	m.Update(m.fileScopeCommand()().(fileScopeMsg))
	if !m.fileAvailable("") || !strings.Contains(ansi.Strip(assertFrame(t, m)), "@ files") {
		t.Fatal("file browsing stayed off after the retried probe")
	}
	m.editor.set("look @")
	m.receiveNativeFilePage(m.refreshFilePicker()().(nativeFilePage))
	if _, choices, ok := m.fileSuggestions(); !ok || len(choices) != 1 || choices[0].Path != "README.md" {
		t.Fatalf("browse choices: %v, %t", choices, ok)
	}
	m.editor.set("")

	// The native catalog: the first prompt's model can be picked before a thread exists.
	open := m.runAction(m.host.byID[modelPickerID], "")
	if open == nil || m.sheet == nil || m.sheet.title != "Select model" {
		t.Fatalf("model picker unavailable: %q", m.notice.text)
	}
	m.Update(reply[modelSearchResult](t, open))
	if len(m.sheet.choices) != sdk.ModelCatalogLimit {
		t.Fatalf("catalog rows: %d", len(m.sheet.choices))
	}
	press(m, tea.KeyEscape, 0)
	if m.active != "" || backend.created.Load() != 0 {
		t.Fatal("enabling features created a thread")
	}

	// Once capabilities are known, nothing probes again.
	latest := m.probe.latest
	m.Update(menuProbeRetry(scheduled))
	m.Update(menuProbeFailed(latest))
	m.Update(introReportMsg{update: IntroUpdate{Report: IntroReport{}}})
	m.beginBackendReport()
	lease := m.reportFeed
	m.Update(backendReportMsg{token: lease.token, owner: lease.owner, ok: true, update: IntroUpdate{Report: IntroReport{Skills: []string{"review"}}}})
	if m.retryMenuProbe() != nil || m.probe.latest != latest || m.probe.pending || m.probe.retry != 0 || backend.hellos.Load() != 2 {
		t.Fatalf("extra probe after capabilities were known: hellos=%d %+v", backend.hellos.Load(), m.probe)
	}
}

func TestMissedProbeRetriesOnceWhenTheBackendReports(t *testing.T) {
	m, backend := freshColdStart(t, 2)
	m.Update(m.menuProbe()())
	scheduled := m.probe.retry
	m.beginBackendReport()
	lease := m.reportFeed
	report := backendReportMsg{token: lease.token, owner: lease.owner, ok: true, update: IntroUpdate{Report: IntroReport{}}}

	// A successful report proves the backend is reachable: probe now, and
	// let the probe supersede the scheduled retry.
	_, now := m.Update(report)
	latest := m.probe.latest
	if now == nil || !m.probe.pending || m.probe.retry != 0 {
		t.Fatalf("the report did not probe at once: %+v", m.probe)
	}
	m.Update(report)
	m.Update(menuProbeRetry(scheduled))
	m.Update(introReportMsg{update: IntroUpdate{Report: IntroReport{}}})
	if m.probe.latest != latest {
		t.Fatal("a second probe started while one was in flight")
	}

	// This probe misses too, so the backoff grows; the next one succeeds.
	_, retry := m.Update(reply[menuProbeFailed](t, now))
	if retry == nil || m.probe.failures != 2 || m.probe.pending {
		t.Fatalf("missed retry did not back off: %+v", m.probe)
	}
	_, probe := m.Update(report)
	m.Update(reply[menuCapabilities](t, probe))
	known := m.probe.latest
	if backend.hellos.Load() != 3 || !m.probe.known || !m.menuFeatures().Has(x.NativeCatalogDraft) {
		t.Fatalf("capabilities not applied: hellos=%d %+v", backend.hellos.Load(), m.probe)
	}
	m.Update(report)
	if m.probe.latest != known || m.probe.pending || backend.hellos.Load() != 3 {
		t.Fatalf("a report probed again after capabilities were known: %+v", m.probe)
	}
}

func TestMenuRetryBackoffIsCapped(t *testing.T) {
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second,
		4 * time.Second, 4 * time.Second, 4 * time.Second}
	for failures, delay := range want {
		if got := menuRetryDelay(failures); got != delay {
			t.Fatalf("after %d failures: %v; want %v", failures, got, delay)
		}
	}
	if got := menuRetryDelay(1 << 20); got != menuRetryMax {
		t.Fatalf("uncapped delay %v", got)
	}
	// Each missed probe counts toward the next delay; success resets it.
	m, _ := freshColdStart(t, 3)
	for i := range 3 {
		m.Update(m.menuProbe()())
		if m.probe.failures != i+1 || m.probe.retry == 0 {
			t.Fatalf("failure %d: %+v", i+1, m.probe)
		}
	}
	m.Update(m.menuProbe()())
	if m.probe.failures != 0 || !m.probe.known {
		t.Fatalf("success did not reset the backoff: %+v", m.probe)
	}
}
