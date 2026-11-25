package tui

import (
	"context"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

type introBackend interface {
	Intro(context.Context, sdk.IntroScope) (sdk.IntroReport, error)
	WatchIntro(context.Context, sdk.IntroScope) <-chan sdk.IntroUpdate
}

func (m *Model) introScope(thread sdk.ID) sdk.IntroScope {
	scope := sdk.IntroScope{ThreadID: thread}
	if thread == "" && m.options.WorkspaceLocal && filepath.IsAbs(m.options.NewThread.CWD) {
		scope.CWD = m.options.NewThread.CWD
	}
	return scope
}

// The active thread has one report lease. Freezing its intro block does not
// freeze the footer. Navigation cancels the old lease before opening another.
type backendReportMsg struct {
	token  uint64
	owner  sdk.ID
	source <-chan sdk.IntroUpdate
	update IntroUpdate
	ok     bool
}

func (m *Model) stopBackendReport() {
	if m.reportFeed.cancel != nil {
		m.reportFeed.cancel()
	}
	m.reportFeed = feedLease{}
}

func (m *Model) beginBackendReport() tea.Cmd {
	m.stopBackendReport()
	source, ok := m.backend.(introBackend)
	if !ok {
		return nil
	}
	scope := m.introScope(m.active)
	ctx, cancel := context.WithCancel(m.ctx)
	f := feedLease{m.token(), m.active, ctx, cancel}
	m.reportFeed = f
	return func() tea.Msg {
		if ctx.Err() != nil {
			return nil
		}
		return awaitBackendReport(f, source.WatchIntro(ctx, scope))()
	}
}

func awaitBackendReport(f feedLease, src <-chan sdk.IntroUpdate) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-f.ctx.Done():
			return nil
		case v, ok := <-src:
			out := IntroUpdate{Err: v.Err}
			if ok && v.Err == nil {
				out.Report = projectIntro(v.Report)
			}
			return backendReportMsg{f.token, f.owner, src, out, ok}
		}
	}
}

func (m *Model) receiveBackendReport(v backendReportMsg) tea.Cmd {
	if v.token == 0 || v.token != m.reportFeed.token || v.owner != m.active {
		return nil
	}
	t := m.threads[v.owner]
	if !v.ok || sdk.IsCode(v.update.Err, "unsupported_capability") || sdk.IsCode(v.update.Err, "not_found") {
		m.stopBackendReport()
		if t.intro == nil || t.intro.options.Watch == nil {
			t.footer = footerMetadata{}
			m.applyPresentation(t, nil)
			m.menuFeaturesFromBackend &^= x.Skills | x.Titles
		}
		if i := t.intro; i != nil && !i.frozen && i.options.Watch == nil {
			return m.beginBasicIntro(i)
		}
		return nil
	}
	var probe tea.Cmd
	if v.update.Err == nil {
		probe = m.retryMenuProbe() // the initial short probe may have missed a cold start
		t.footer = metadataFromReport(v.update.Report)
		m.applyPresentation(t, v.update.Report.Thread)
		if len(v.update.Report.Skills) != 0 || v.update.Report.MoreSkills > 0 {
			m.menuFeaturesFromBackend |= x.Skills
		}
	}
	if i := t.intro; i != nil && !i.frozen && i.options.Watch == nil {
		m.receiveIntro(introReportMsg{update: v.update, rich: true})
	}
	return tea.Batch(awaitBackendReport(m.reportFeed, v.source), probe)
}

func (m *Model) applyPresentation(t *threadState, presentation *sdk.IntroThread) {
	prior := t.info
	t.presentation = presentation
	if t.directory == nil && presentation != nil && presentation.ID == t.info.ID && presentation.Title != "" &&
		(m.options.ProjectThread == nil && m.options.Services.Rename == nil || t.info.Title == "") {
		t.info.Title = limitText(oneLine(presentation.Title), 384)
	}
	if t.directory == nil && !threadBusy(t) && t.activity.failure.TaskID != "" && t.activity.compactionFailure.TaskID == "" {
		if t.dismissedFailure(t.activity.failure.TaskID) {
			t.info.Status = x.Idle
		} else {
			t.info.Status = x.Failed
		}
	}
	if prior != t.info {
		t.revision++
		m.emit(t.info.ID, x.Event{Kind: x.StatusChanged})
	}
}

func projectIntro(v sdk.IntroReport) IntroReport {
	r := IntroReport{
		Workspace: v.CWD, Model: v.Model, Thinking: v.Thinking, Thread: v.Thread,
		Sessions:        &IntroSessions{Open: v.Sessions.Open, Running: v.Sessions.Running, Quiet: v.Sessions.Quiet},
		SessionsPartial: v.Sessions.Partial, Skills: v.Skills, MoreSkills: v.MoreSkills, MoreAttention: v.MoreAttention,
	}
	if v.StartedAtMS > 0 {
		r.StartedAt = time.UnixMilli(v.StartedAtMS)
	}
	for _, a := range v.Attention {
		item := IntroAttention{Title: a.Title, Status: x.ThreadStatus(a.Status), Reason: a.Reason, ThreadID: a.ThreadID, Issue: a.Issue}
		if a.UntilMS != nil {
			item.Until = time.UnixMilli(*a.UntilMS).Local()
			item.Until.Zone() // resolve the system time zone off the UI loop
		}
		r.Attention = append(r.Attention, item)
	}
	if v.Context != nil {
		r.Context = &IntroContext{Window: v.Context.Window, RenderedBytes: v.Context.RenderedBytes, ReportedTokens: v.Context.ReportedTokens}
	}
	return r
}
