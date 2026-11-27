package tui

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const menusID = "hush/menus"
const newThreadID x.ActionID = menusID + ".new"
const renameID x.ActionID = menusID + ".name"
const modelPickerID x.ActionID = menusID + ".model"
const filePickID x.ActionID = menusID + ".file"
const pageChoice = "more"
const menuTimeout = 3 * time.Second

// MenuServices supplies optional host operations not defined by native v1.
// Nil callbacks keep their menus unavailable. Callbacks run off the UI loop;
// they must enforce all workspace, authorization and thread-scope rules.
type MenuServices struct {
	Models  func(context.Context) ([]x.CatalogModel, error)
	Files   func(context.Context, x.ListFiles) ([]x.File, string, error)
	Rename  func(context.Context, x.SetTitle) error
	Archive func(context.Context, x.SetArchived) error
	Modes   func(context.Context, x.ListModes) ([]x.Choice, error)
	SetMode func(context.Context, x.SetMode) error
}

type helloBackend interface {
	Hello(context.Context) (sdk.Hello, error)
}
type modelCatalogBackend interface {
	helloBackend
	ModelCatalog(context.Context, sdk.ID) (sdk.ModelCatalog, error)
	ConfigureCatalogThread(context.Context, sdk.ID, *sdk.Model, string) error
}

type modelCatalogSearchBackend interface {
	SearchModelCatalog(context.Context, sdk.ID, string) (sdk.ModelCatalog, error)
}

type modelCatalogDraftBackend interface {
	NewCatalogThread(context.Context, string, sdk.Model, string) (sdk.Thread, error)
}

type menuCapabilities struct {
	token            uint64
	features         x.Features
	directory        bool
	gitWatch         bool
	files            bool
	fileSearch       bool
	scopeWatch       bool
	skills           bool
	skillDraft       bool
	context          bool
	chatPlanActivity bool
}

// A cold backend can miss the startup probe, and a fresh session has no feed
// whose connection would probe again. menuProbeState keeps at most one probe
// in flight: a failure retries with capped backoff, a successful backend or
// intro report retries at once, and both stop once a probe succeeds. A feed
// that (re)connects verifies capabilities again, as before.
type menuProbeState struct {
	latest   uint64 // newest probe; replies to older ones are ignored
	pending  bool   // the newest probe has not replied
	known    bool   // a probe succeeded since the last feed (re)connection
	failures int    // consecutive failures, which select the next delay
	retry    uint64 // scheduled retry; 0 when none
}

type menuProbeFailed uint64
type menuProbeRetry uint64

const menuRetryBase, menuRetryMax = 250 * time.Millisecond, 4 * time.Second

// menuRetryDelay doubles from menuRetryBase after each consecutive failure,
// up to menuRetryMax.
func menuRetryDelay(failures int) time.Duration {
	return min(menuRetryMax, menuRetryBase<<min(failures, 4))
}

// retryMenuProbe probes unless capabilities are known or a probe is pending.
func (m *Model) retryMenuProbe() tea.Cmd {
	if m.probe.known || m.probe.pending {
		return nil
	}
	return m.menuProbe()
}

func (m *Model) receiveMenuProbeFailure(token uint64) tea.Cmd {
	if token != m.probe.latest || !m.probe.pending {
		return nil
	}
	m.probe.pending = false
	if m.probe.known {
		return nil
	}
	delay := menuRetryDelay(m.probe.failures)
	m.probe.failures++
	retry := m.token()
	m.probe.retry = retry
	ctx := m.ctx
	return func() tea.Msg {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return menuProbeRetry(retry)
		}
	}
}

// menuProbe always starts a probe, superseding any pending one or scheduled
// retry. Update paths use retryMenuProbe to keep one in flight.
func (m *Model) menuProbe() tea.Cmd {
	backend, ok := m.backend.(helloBackend)
	_, titles := m.backend.(renameBackend)
	_, directory := m.backend.(directoryWatchBackend)
	_, worktrees := m.backend.(worktreeBackend)
	_, archive := m.backend.(archiveBackend)
	_, schedules := m.backend.(scheduleBackend)
	_, checkpoints := m.backend.(checkpointBackend)
	_, gitDiff := m.backend.(gitDiffBackend)
	_, gitWatch := m.backend.(gitDiffWatchBackend)
	_, files := m.backend.(fileBrowseBackend)
	_, fileSearch := m.backend.(fileSearchBackend)
	_, scopeWatch := m.backend.(fileScopeWatchBackend)
	_, skills := m.backend.(workspaceSkillsBackend)
	_, skillDraft := m.backend.(workspaceSkillDraftBackend)
	_, plans := m.backend.(planBackend)
	_, planWorkspace := m.backend.(planWorkspaceBackend)
	_, planCatalog := m.backend.(planCatalogBackend)
	_, planReview := m.backend.(planReviewBackend)
	_, workers := m.backend.(workerActivityBackend)
	_, subagents := m.backend.(subagentBackend)
	_, planDraft := m.backend.(planDraftBackend)
	_, planChat := m.backend.(planChatBackend)
	_, chatPlanActivity := m.backend.(chatPlanActivityBackend)
	_, planWorktree := m.backend.(planWorktreeBackend)
	_, loadedContext := m.backend.(loadedContextBackend)
	_, native := m.backend.(modelCatalogBackend)
	_, search := m.backend.(modelCatalogSearchBackend)
	_, draft := m.backend.(modelCatalogDraftBackend)
	_, settings := m.backend.(settingsBackend)
	native = native && m.options.Services.Models == nil
	if !ok || !native && !titles && !directory && !worktrees && !archive &&
		!schedules && !checkpoints && !gitDiff && !files && !skills && !loadedContext && !settings && !plans && !workers && !subagents {
		return nil
	}
	token := m.token()
	m.probe.latest, m.probe.pending, m.probe.retry = token, true, 0
	ctx := m.ctx
	return func() tea.Msg {
		call, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		hello, err := backend.Hello(call)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return menuProbeFailed(token)
		}
		var f x.Features
		catalogVersion := hello.Capabilities["modelCatalog"]
		if native && (catalogVersion == sdk.ModelCatalogVersion || catalogVersion == sdk.ModelCatalogSearchVersion || catalogVersion == sdk.ModelCatalogDraftVersion) {
			f |= x.Catalog | x.NativeCatalog
			if search && catalogVersion >= sdk.ModelCatalogSearchVersion {
				f |= x.NativeCatalogSearch
				if draft && catalogVersion == sdk.ModelCatalogDraftVersion {
					f |= x.NativeCatalogDraft
				}
			}
		}
		if plans && hello.Capabilities["plan"] == sdk.PlanVersion {
			f |= x.Plans
			if planDraft && hello.Capabilities["planDraft"] == sdk.PlanDraftVersion {
				f |= x.PlanDraft
			}
			if planWorkspace && hello.Capabilities["planWorkspace"] == sdk.PlanWorkspaceVersion {
				f |= x.PlanWorkspace
				if planChat && hello.Capabilities["planChat"] == sdk.PlanChatVersion {
					f |= x.PlanChat
					if planWorktree && hello.Capabilities["planWorktree"] == sdk.PlanWorktreeVersion {
						f |= x.PlanWorktree
					}
				}
				if planCatalog && hello.Capabilities["planCatalog"] == sdk.PlanCatalogVersion {
					f |= x.PlanCatalog
				}
				if planReview && hello.Capabilities["planReview"] == sdk.PlanReviewVersion {
					f |= x.PlanReview
				}
			}
		}
		if workers && hello.Capabilities["delegation"] == sdk.DelegationVersion && hello.Capabilities["workerActivity"] == sdk.WorkerActivityVersion {
			f |= x.WorkerActivity
		}
		if subagents && hello.Capabilities["subagents"] == sdk.SubagentOverviewVersion {
			f |= x.Subagents
		}
		if settings && hello.Capabilities["settings"] == sdk.SettingsVersion {
			f |= x.Settings
		}
		if titles && hello.Capabilities["intro"] == sdk.IntroActionsVersion {
			f |= x.Titles
		}
		managed := hello.Capabilities["threadDirectory"] == sdk.ThreadManagementVersion
		if managed && worktrees {
			f |= x.Worktrees
		}
		if managed && archive {
			f |= x.Archive
		}
		if schedules && hello.Capabilities["schedules"] == sdk.SchedulesVersion {
			f |= x.Schedules
		}
		if checkpoints && hello.Capabilities["checkpoints"] == sdk.CheckpointsVersion {
			f |= x.Checkpoints
		}
		if gitDiff && hello.Capabilities["gitDiff"] == sdk.GitDiffVersion {
			f |= x.GitDiff
		}
		fileVersion, skillVersion := hello.Capabilities["workspaceFiles"], hello.Capabilities["workspaceSkills"]
		contextVersion := hello.Capabilities["loadedContext"]
		searchable := fileVersion == sdk.WorkspaceFileSearchVersion || fileVersion == sdk.WorkspaceFileProjectPathsVersion
		return menuCapabilities{token: token, features: f, directory: managed && directory,
			chatPlanActivity: f.Has(x.PlanChat) && chatPlanActivity && hello.Capabilities["planChatActivity"] == sdk.PlanChatActivityVersion,
			gitWatch:         f.Has(x.GitDiff) && gitWatch && hello.Capabilities["gitDiffWatch"] == sdk.GitDiffWatchVersion,
			scopeWatch:       scopeWatch && hello.Capabilities["workspaceScopeWatch"] == sdk.WorkspaceScopeWatchVersion,
			files:            files && (fileVersion == sdk.WorkspaceFilesVersion || searchable),
			fileSearch:       files && fileSearch && searchable,
			skills:           skills && (skillVersion == sdk.WorkspaceSkillsVersion || skillVersion == sdk.WorkspaceSkillDraftVersion),
			skillDraft:       skillDraft && skillVersion == sdk.WorkspaceSkillDraftVersion,
			context:          loadedContext && (contextVersion == sdk.LoadedContextAgentsVersion || contextVersion == sdk.LoadedContextVersion)}
	}
}

func (m *Model) menuFeatures() x.Features {
	s := m.options.Services
	f := m.menuFeaturesFromBackend
	if s.Models != nil {
		f |= x.Catalog
	}
	if m.fileAvailable("") {
		f |= x.Files
	}
	if _, ok := m.skillScope(m.active); ok {
		f |= x.Skills
	}
	if !m.planAvailable() && !m.draftPlanAvailable() {
		f &^= x.Plans
	}
	if m.contextAvailable() {
		f |= x.LoadedContext
	}
	if s.Rename != nil {
		f |= x.Titles
	}
	if s.Archive != nil {
		f |= x.Archive
	}
	if s.Modes != nil && s.SetMode != nil {
		f |= x.Mode
	}
	return f
}

var levels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

func (m *Model) menuJob(r ownerRoute, operation x.Operation) (func(context.Context) x.ClientResult, bool) {
	if job, ok := m.settingsJob(operation); ok {
		return job, true
	}
	services := m.options.Services
	features := m.menuFeatures()
	target := func(id sdk.ID) sdk.ID {
		if id == "" {
			return r.owner
		}
		return id
	}
	switch op := operation.(type) {
	case x.ManageCheckpoint:
		if backend, ok := m.backend.(checkpointBackend); ok && features.Has(x.Checkpoints) {
			return checkpointJob(backend, target(op.ThreadID), op), true
		}
	case x.ManageSchedule:
		if backend, ok := m.backend.(scheduleBackend); ok && features.Has(x.Schedules) {
			return scheduleJob(backend, target(op.ThreadID), op), true
		}
	case x.ListSkills:
		if lookup := m.skillLookup(target(op.ThreadID)); lookup != nil {
			return func(ctx context.Context) x.ClientResult {
				call, cancel := context.WithTimeout(ctx, menuTimeout+time.Second)
				defer cancel()
				v, err := lookup(call)
				return x.ClientResult{WorkspaceSkills: v, Err: err}
			}, true
		}
		if backend, ok := m.backend.(introBackend); ok && features.Has(x.Skills) {
			scope := m.introScope(target(op.ThreadID))
			return func(ctx context.Context) x.ClientResult {
				call, cancel := context.WithTimeout(ctx, menuTimeout)
				defer cancel()
				report, err := backend.Intro(call, scope)
				return x.ClientResult{Skills: report.Skills, MoreSkills: report.MoreSkills, Err: err}
			}, true
		}
	case x.ConfigureCatalog:
		if backend, ok := m.backend.(modelCatalogBackend); ok && features.Has(x.NativeCatalog) {
			return func(ctx context.Context) x.ClientResult {
				call, cancel := context.WithTimeout(ctx, menuTimeout+3*time.Second)
				defer cancel()
				var ref *sdk.Model
				if op.Model != (sdk.Model{}) {
					ref = &op.Model
				}
				return x.ClientResult{Err: backend.ConfigureCatalogThread(call, target(op.ThreadID), ref, op.ThinkingLevel)}
			}, true
		}
	case x.ListModels:
		hook := services.Models
		if backend, ok := m.backend.(modelCatalogBackend); hook == nil && ok && features.Has(x.NativeCatalog) {
			return func(ctx context.Context) x.ClientResult {
				call, cancel := context.WithTimeout(ctx, menuTimeout)
				defer cancel()
				var catalog sdk.ModelCatalog
				var err error
				if op.ThreadID == "" {
					search, supported := m.backend.(modelCatalogSearchBackend)
					if !supported || !features.Has(x.NativeCatalogDraft) {
						return x.ClientResult{Err: sdk.ErrProtocol}
					}
					catalog, err = search.SearchModelCatalog(call, "", op.Query)
				} else {
					catalog, err = backend.ModelCatalog(call, target(op.ThreadID))
				}
				if err != nil {
					return x.ClientResult{Err: err}
				}
				return nativeModels(catalog, op)
			}, true
		}
		if hook != nil {
			return func(ctx context.Context) x.ClientResult {
				data, err := hook(ctx)
				if err != nil {
					return x.ClientResult{Err: err}
				}
				data = append([]x.CatalogModel(nil), data[:min(maxChoices, len(data))]...)
				slices.SortStableFunc(data, func(a, b x.CatalogModel) int {
					if a.Model.Provider != b.Model.Provider {
						return cmp.Compare(a.Model.Provider, b.Model.Provider)
					}
					return cmp.Compare(a.Model.ModelID, b.Model.ModelID)
				})
				result := x.ClientResult{}
				start, _ := strconv.Atoi(op.Cursor)
				start = max(0, start)
				seen := map[sdk.Model]bool{}
				n := 0
				for _, item := range data {
					if item.Model.Provider == "" || item.Model.ModelID == "" || seen[item.Model] || !fuzzyMatch(item.Model.Provider+"/"+item.Model.ModelID+" "+item.Label, op.Query) {
						continue
					}
					seen[item.Model] = true
					if n < start {
						n++
						continue
					}
					if len(result.Models) >= pageLimit(op.Limit) {
						result.Cursor = strconv.Itoa(n)
						break
					}
					item.ThinkingLevels = append([]string(nil), item.ThinkingLevels[:min(8, len(item.ThinkingLevels))]...)
					result.Models = append(result.Models, item)
					n++
				}
				return result
			}, true
		}
	case x.ListFiles:
		if services.Files == nil && m.nativeFiles {
			backend := m.backend.(fileBrowseBackend)
			owner, limit := target(op.ThreadID), pageLimit(op.Limit)
			return func(ctx context.Context) x.ClientResult {
				return browseFiles(ctx, backend, sdk.WorkspaceFileTarget{ThreadID: owner}, op.Query, op.Cursor, limit)
			}, true
		}
		if services.Files != nil {
			op.ThreadID, op.Limit = target(op.ThreadID), pageLimit(op.Limit)
			return func(ctx context.Context) x.ClientResult {
				if !op.ThreadID.Valid() {
					return x.ClientResult{Err: sdk.ErrProtocol}
				}
				v, cursor, err := services.Files(ctx, op)
				return x.ClientResult{Files: append([]x.File(nil), v[:min(len(v), op.Limit)]...), Cursor: cursor, Err: err}
			}, true
		}
	case x.SetTitle:
		if services.Rename != nil {
			op.ThreadID = target(op.ThreadID)
			return func(ctx context.Context) x.ClientResult {
				if !op.ThreadID.Valid() {
					return x.ClientResult{Err: sdk.ErrProtocol}
				}
				return x.ClientResult{Err: services.Rename(ctx, op), ThreadID: op.ThreadID, Title: op.Title}
			}, true
		}
		if backend, ok := m.backend.(renameBackend); ok && features.Has(x.Titles) {
			op.ThreadID = target(op.ThreadID)
			return func(ctx context.Context) x.ClientResult {
				title, err := backend.RenameThread(ctx, op.ThreadID, op.Title)
				return x.ClientResult{Err: err, ThreadID: op.ThreadID, Title: title}
			}, true
		}
	case x.SetArchived:
		archive := services.Archive
		if backend, ok := m.backend.(archiveBackend); archive == nil && ok && features.Has(x.Archive) {
			archive = func(ctx context.Context, op x.SetArchived) error {
				return backend.SetThreadArchived(ctx, op.ThreadID, op.Archived)
			}
		}
		if archive != nil {
			op.ThreadID = target(op.ThreadID)
			return func(ctx context.Context) x.ClientResult {
				if !op.ThreadID.Valid() {
					return x.ClientResult{Err: sdk.ErrProtocol}
				}
				return x.ClientResult{Err: archive(ctx, op), ThreadID: op.ThreadID}
			}, true
		}
	case x.NewWorktree:
		if backend, ok := m.backend.(worktreeBackend); ok && features.Has(x.Worktrees) {
			if op.Workspace == "" && m.options.WorkspaceLocal {
				op.Workspace = m.options.NewThread.CWD
			}
			return func(ctx context.Context) x.ClientResult {
				row, err := backend.CreateThread(ctx, sdk.ThreadCreation{CWD: op.Workspace, Title: op.Title, Worktree: true})
				return x.ClientResult{Err: err, ThreadID: row.ID}
			}, true
		}
	case x.ListModes:
		if features.Has(x.Mode) {
			op.ThreadID = target(op.ThreadID)
			return func(ctx context.Context) x.ClientResult {
				if !op.ThreadID.Valid() {
					return x.ClientResult{Err: sdk.ErrProtocol}
				}
				modes, err := services.Modes(ctx, op)
				return x.ClientResult{Modes: append([]x.Choice(nil), modes[:min(len(modes), maxChoices)]...), Err: err}
			}, true
		}
	case x.SetMode:
		if features.Has(x.Mode) {
			op.ThreadID = target(op.ThreadID)
			return func(ctx context.Context) x.ClientResult {
				if !op.ThreadID.Valid() {
					return x.ClientResult{Err: sdk.ErrProtocol}
				}
				return x.ClientResult{Err: services.SetMode(ctx, op), ThreadID: op.ThreadID}
			}, true
		}
	}
	return nil, false
}

// applyMenuResult runs on the loop and merges host acknowledgements without
// letting command goroutines read the shell. Unknown thread metadata stays empty.
func (m *Model) applyMenuResult(op x.Operation, result *x.ClientResult) {
	if result.Err != nil {
		return
	}
	switch v := op.(type) {
	case x.ListThreads:
		out := result.Threads[:0]
		matches, workspaces := result.WorkspaceMatches[:0], result.Workspaces[:0]
		for i, row := range result.Threads {
			if t := m.threads[row.ID]; t != nil {
				managed := result.Directory && row.Status != ""
				if t.info.Archived && (!managed || m.options.Services.Archive != nil) {
					continue
				}
				info := t.info
				info.Forked, info.ParentID = row.Forked, row.ParentID
				if info.Title == "" || managed && m.options.ProjectThread == nil && m.options.Services.Rename == nil {
					info.Title = row.Title
				}
				if managed && m.options.ProjectThread == nil {
					info.Branch = row.Branch
					if !t.activeRun || row.Status == x.Running || row.Status == x.Parked {
						info.Status = row.Status
					}
				}
				if info.Workspace == "" {
					info.Workspace = row.Workspace
				}
				info.LastMessageAt = row.LastMessageAt
				info.Preview = menuPreview(t.lastAnswer, true)
				row = info
			}
			out = append(out, row)
			if i < len(result.WorkspaceMatches) {
				matches = append(matches, result.WorkspaceMatches[i])
			}
			if i < len(result.Workspaces) {
				workspaces = append(workspaces, result.Workspaces[i])
			}
		}
		result.Threads, result.WorkspaceMatches, result.Workspaces = out, matches, workspaces
	case x.SetTitle:
		if t := m.threads[result.ThreadID]; t != nil && (result.ThreadID != m.active || t.presentation == nil || m.options.Services.Rename != nil) {
			// The active v2 watch is authoritative; a late command reply must
			// not overwrite a title it already observed another client change.
			t.info.Title = limitText(result.Title, 384)
			t.revision++
			m.emit(result.ThreadID, x.Event{Kind: x.StatusChanged})
		}
	case x.SetArchived:
		if t := m.threads[result.ThreadID]; t != nil && (t.directory == nil || m.options.Services.Archive != nil) {
			t.info.Archived = v.Archived
			t.revision++
			m.emit(result.ThreadID, x.Event{Kind: x.StatusChanged})
		}
	case x.ReadSettings, x.WriteSettings:
		if result.Settings != nil {
			m.applySettings(*result.Settings)
		}
	case x.SetMode:
		if t := m.threads[result.ThreadID]; t != nil {
			t.info.Mode = limitText(v.Mode, 64)
			t.revision++
			m.emit(result.ThreadID, x.Event{Kind: x.StatusChanged})
		}
	}
}

func menusRegistration(options Options) x.Registration {
	da := func(id, title, direct, leader, slash string, requires x.Features, run func(x.Context, x.Invocation) x.Outcome) x.Action {
		a := x.Action{ID: x.ActionID(menusID + "." + id), Title: title, Binding: x.Binding{Direct: direct, Leader: leader}, Run: run, Requires: requires}
		if slash != "" {
			a.Slash = &x.Slash{Name: slash}
		}
		return a
	}
	r := x.Registration{ID: menusID, APIVersion: x.APIVersion, Actions: []x.Action{
		da("switch", "Switch threads", "ctrl+t", "s", "threads", 0, func(x.Context, x.Invocation) x.Outcome {
			return x.Outcome{x.Local{Op: openThreadPickerOp}}
		}),
		da("switch-all", "Browse all workspaces", "", "shift+s", "threads-all", 0, func(x.Context, x.Invocation) x.Outcome {
			return x.Outcome{x.Local{Op: openThreadPickerOp, Text: "all"}}
		}),
		da("new", "New thread", "ctrl+n", "n", "new", 0, func(_ x.Context, _ x.Invocation) x.Outcome {
			// Use the launch directory, not the active thread's workspace.
			return x.Outcome{x.Client{Op: x.NewThread{}, Done: switchResult}}
		}),
		da("schedule", "Scheduled occurrences", "", "i s", "schedule", x.Schedules, scheduleMenu),
		da("checkpoint", "Record checkpoint", "", "i c", "checkpoint", x.Checkpoints, checkpointMenu),
		da("rewind", "Preview rewind", "", "i r", "rewind", x.Checkpoints, rewindMenu),
		da("worktree", "New thread in a worktree", "", "g", "worktree", x.Worktrees, func(c x.Context, v x.Invocation) x.Outcome {
			return x.Outcome{x.Client{Op: x.NewWorktree{Workspace: c.Thread.Workspace, Title: strings.TrimSpace(v.Args)}, Done: switchResult}}
		}),
		da("fork", "Fork history", "ctrl+f", "f", "fork", 0, func(c x.Context, v x.Invocation) x.Outcome {
			id := actionThread(c, v)
			if !id.Valid() {
				return showNote("No thread to fork", x.Warning)
			}
			return forkMenu(id, "")
		}),
		da("plan", "Open plan", "", "i n", "plan", x.Plans, func(_ x.Context, _ x.Invocation) x.Outcome {
			return x.Outcome{x.Local{Op: openPlanOp}}
		}),
		da("plan-run", "Open current plan run", "", "i k", "", x.Plans, func(_ x.Context, v x.Invocation) x.Outcome {
			return x.Outcome{x.Local{Op: openPlanRunOp, Text: v.Target}}
		}),
		da("loaded-context", "Show loaded context", "", "i a", "", x.LoadedContext, func(_ x.Context, _ x.Invocation) x.Outcome {
			return x.Outcome{x.Local{Op: openLoadedContextOp}}
		}),
		da("skills", "List loaded skills", "", "l", "skills", x.Skills, func(_ x.Context, _ x.Invocation) x.Outcome {
			return x.Outcome{x.Client{Op: x.ListSkills{}, Done: showSkills}}
		}),
		da("config", "Configuration", "", "i o", "config", x.Settings, configMenu),
		da("model", "Select model", "ctrl+l", "m", "model", x.Catalog, func(c x.Context, v x.Invocation) x.Outcome {
			id := actionThread(c, v)
			if !id.Valid() && !(id == "" && c.Features.Has(x.NativeCatalogDraft)) {
				return showNote("Use /new to open a thread before selecting a model", x.Warning)
			}
			if c.Features.Has(x.NativeCatalogSearch) {
				return x.Outcome{x.Local{Op: modelSearchOp, ThreadID: id, Text: v.Args}}
			}
			return modelMenu(id, "")
		}),
		da("thinking", "Select thinking level", "shift+tab", "t", "thinking", 0, func(c x.Context, _ x.Invocation) x.Outcome {
			if c.Thread.ID == "" {
				if c.Features.Has(x.NativeCatalogDraft) {
					return showNote("Use /model to choose the model and thinking level for the first prompt", x.Warning)
				}
				return showNote("Open a thread before setting thinking level", x.Warning)
			}
			if c.Features.Has(x.NativeCatalog) {
				return nativeThinkingMenu(c.Thread.ID)
			}
			if !c.Features.Has(x.Catalog) {
				return showNote("Thinking levels unavailable on this host", x.Warning)
			}
			return thinkingMenu(c.Thread.ID, levels, sdk.Model{}, x.Sheet)
		}),
		da("name", "Rename thread", "ctrl+r", "r", "name", x.Titles, renameMenu),
		da("retry", "Retry failed run", "", "b", "retry", 0, runRecovery(retryFailureOp)),
		da("dismiss", "Dismiss run failure", "", "x", "dismiss", 0, runRecovery(dismissFailureOp)),
		da("mode", "Permission mode", "", "u", "mode", x.Mode, modeMenu),
		da("archive", "Archive thread", "", "v", "archive", x.Archive, archiveMenu),
		da("compact", "Compact the context", "", "j", "compact", 0, func(c x.Context, _ x.Invocation) x.Outcome {
			return confirmNative(c, "Compact context?", "The backend will summarize the current context.", x.Compact{}, "Compaction requested")
		}),
		da("reset", "Reset the context", "", "z", "reset", 0, func(c x.Context, _ x.Invocation) x.Outcome {
			return confirmNative(c, "Reset context?", "Start fresh without deleting transcript history.", x.Reset{}, "Context reset")
		}),
		da("file", "Complete @ file", "", "@", "", x.Files, func(_ x.Context, v x.Invocation) x.Outcome {
			return x.Outcome{x.Local{Op: x.CompleteFile, Text: v.Target}}
		}),
	}}
	if login := options.ProviderLogin; login != nil && login.Command != nil && len(login.Providers) != 0 {
		r.Actions = append(r.Actions, da("login", "Sign in to a provider", "", "i l", "login", 0, providerLoginMenu(login.Providers)))
	}
	if options.OpenThreadPane != nil {
		a := da("new-pane", "Open a new Hush pane", "", "i e", "new-pane", 0, func(x.Context, x.Invocation) x.Outcome {
			return x.Outcome{x.Local{Op: openThreadPaneOp}}
		})
		a.Available = func(c x.Context) bool { return c.Thread.ID.Valid() }
		a.Description = "Open an independent thread in this directory on the same backend"
		r.Actions = append(r.Actions, a)
	}
	// Reserve /pane even when the terminal can't offer the action.
	a := da("pane-command", "Request a command pane", "", "i y", "pane", 0, func(x.Context, x.Invocation) x.Outcome {
		return x.Outcome{x.Local{Op: stagePaneCommandOp}}
	})
	a.Available = func(x.Context) bool { return options.paneCommandsAvailable() }
	r.Actions = append(r.Actions, a)
	return r
}

func showSkills(_ x.Context, result x.ClientResult) x.Outcome {
	if result.Err != nil {
		return clientNote(result, "")
	}
	if result.WorkspaceSkills != nil {
		return stageSkillChoice(result.WorkspaceSkills)
	}
	if len(result.Skills) == 0 {
		return showNote("No skills are loaded for this workspace", x.Muted)
	}
	choices := make([]x.Choice, 0, min(maxChoices, len(result.Skills)))
	for _, name := range result.Skills[:min(maxChoices, len(result.Skills))] {
		choices = append(choices, x.Choice{ID: name, Label: name, Action: "Insert skill command for"})
	}
	title := "Loaded skills"
	if n := len(result.Skills) - len(choices) + result.MoreSkills; n > 0 {
		title += fmt.Sprintf(" · +%d not listed", n)
	}
	return x.Outcome{x.SelectDialog{Title: title, Placement: x.Sheet, Choices: choices, Done: func(_ x.Context, r x.Reply[string]) x.Outcome {
		if r.Accepted {
			return x.Outcome{x.Local{Op: x.ReplacePrompt, Text: "/skill:" + r.Value + " "}}
		}
		return nil
	}}}
}

func showNote(s string, tone x.Tone) x.Outcome { return x.Outcome{x.Notice{Text: s, Tone: tone}} }
func clientNote(result x.ClientResult, ok string) x.Outcome {
	if result.Err != nil {
		return showNote(result.Err.Error(), x.Danger)
	}
	if ok == "" {
		return nil
	}
	return showNote(ok, x.Good)
}
func switchResult(_ x.Context, result x.ClientResult) x.Outcome {
	if result.Err != nil {
		return clientNote(result, "")
	}
	return x.Outcome{x.Client{Op: x.Switch{ThreadID: result.ThreadID}}}
}
func actionThread(c x.Context, v x.Invocation) sdk.ID {
	id := sdk.ID(v.Target)
	if id.Valid() {
		return id
	}
	return c.Thread.ID
}

func forkMenu(id, before sdk.ID) x.Outcome {
	return x.Outcome{x.Client{Op: x.History{ThreadID: id, BeforeEntryID: before, Limit: 80}, Done: func(_ x.Context, r x.ClientResult) x.Outcome {
		if r.Err != nil {
			return clientNote(r, "")
		}
		var choices []x.Choice
		var oldest sdk.ID
		for i := len(r.Entries) - 1; i >= 0; i-- {
			e := r.Entries[i].Record
			oldest = e.ID
			body := ""
			for _, msg := range e.Messages {
				for _, b := range msg.Blocks {
					if b.Type == "text" && body == "" {
						body = menuPreview(b.Text, msg.Role == "assistant" && !checkpointMarker(e.Kind))
					}
				}
			}
			label := string(e.ID) + " · " + e.Kind
			if body != "" {
				label = string(e.ID) + " · " + body
			}
			choices = append(choices, x.Choice{ID: string(e.ID), Label: label, Description: "transcript fork"})
		}
		if len(choices) == 0 {
			return showNote("No history entries to fork", x.Warning)
		}
		if oldest != "" && r.Cursor != "" {
			choices = append(choices, x.Choice{ID: pageChoice, Label: "Older entries…"})
		}
		return x.Outcome{x.SelectDialog{Title: "Fork transcript at an entry · files unchanged", Placement: x.Sheet, Choices: choices, Done: func(_ x.Context, reply x.Reply[string]) x.Outcome {
			if !reply.Accepted {
				return nil
			}
			if reply.Value == pageChoice {
				return forkMenu(id, oldest)
			}
			return x.Outcome{x.Client{Op: x.Fork{ThreadID: id, AtEntryID: sdk.ID(reply.Value)}, Done: switchResult}}
		}}}
	}}}
}

func nativeModels(catalog sdk.ModelCatalog, op x.ListModels) x.ClientResult {
	out := x.ClientResult{NativeModels: true}
	limit := pageLimit(op.Limit)
	if len(catalog.Models) > maxChoices {
		limit = min(limit, maxChoices-1) // Leave room for the existing More row.
	}
	start, _ := strconv.Atoi(op.Cursor)
	n := 0
	for _, row := range catalog.Models {
		current := catalog.Current != nil && *catalog.Current == row.Model
		if !current && !fuzzyMatch(row.Provider+"/"+row.ModelID+" "+row.Label, op.Query) {
			continue
		}
		if n < start {
			n++
			continue
		}
		if len(out.Models) >= limit {
			out.Cursor = strconv.Itoa(n)
			break
		}
		item := x.CatalogModel{Model: row.Model, Label: row.Label, ThinkingLevels: row.ThinkingLevels, Current: current}
		if !row.Available {
			item.Unavailable = "Model unavailable. Configure provider credentials on the backend host or pick another model."
			switch deref(row.Reason) {
			case "missing_model":
				item.Unavailable = "Model not installed. Restore it on the backend host or pick another model."
			case "availability_unknown":
				item.Unavailable = "Could not check model availability. Reopen /model to retry or pick another model."
			case "credentials_required":
				if row.Provider == "openai-codex" {
					item.Unavailable = "Model unavailable. Run hush auth login on the backend host or pick another model."
				}
			}
		}
		out.Models = append(out.Models, item)
		n++
	}
	return out
}

func nativeThinkingMenu(id sdk.ID) x.Outcome {
	return x.Outcome{x.Client{Op: x.ListModels{ThreadID: id, Limit: sdk.ModelCatalogLimit}, Done: func(_ x.Context, r x.ClientResult) x.Outcome {
		if r.Err != nil {
			return clientNote(r, "")
		}
		for _, row := range r.Models {
			if row.Current {
				if row.Unavailable != "" {
					return showNote(row.Unavailable, x.Warning)
				}
				return modelThinkingMenu(id, row.ThinkingLevels, sdk.Model{}, x.Sheet, true)
			}
		}
		return showNote("No model selected. Choose one with /model first.", x.Warning)
	}}}
}

func modelMenu(id sdk.ID, cursor string) x.Outcome {
	return x.Outcome{x.Client{Op: x.ListModels{ThreadID: id, Limit: 100, Cursor: cursor}, Done: func(_ x.Context, r x.ClientResult) x.Outcome {
		if r.Err != nil {
			return clientNote(r, "")
		}
		choices, models := modelPickerChoices(r.Models)
		if len(choices) == 0 {
			return showNote("No available models", x.Warning)
		}
		if r.Cursor != "" {
			choices = append(choices, x.Choice{ID: pageChoice, Label: "More models…"})
		}
		return x.Outcome{x.SelectDialog{Title: "Select model", Placement: x.Sheet, Choices: choices, Done: func(_ x.Context, reply x.Reply[string]) x.Outcome {
			if !reply.Accepted {
				return nil
			}
			if reply.Value == pageChoice {
				return modelMenu(id, r.Cursor)
			}
			return modelPickerSelection(id, models, reply.Value, r.NativeModels)
		}}}
	}}}
}

func modelPickerChoices(models []x.CatalogModel) ([]x.Choice, map[string]x.CatalogModel) {
	byChoice := make(map[string]x.CatalogModel, len(models))
	choices := make([]x.Choice, 0, len(models))
	for i, model := range models {
		key := strconv.Itoa(i)
		byChoice[key] = model
		ref := model.Model.Provider + "/" + model.Model.ModelID
		label, detail := ref, model.Label
		if model.Current {
			label = "Current · " + ref
		}
		if model.Unavailable != "" {
			if model.Current {
				label = "Current (unavailable) · " + ref
			} else {
				label = "Unavailable · " + ref
			}
			detail = model.Unavailable + " · " + detail
		}
		choices = append(choices, x.Choice{ID: key, Label: limitText(label, 512), Description: limitText(detail, 1024)})
	}
	return choices, byChoice
}

func modelPickerSelection(id sdk.ID, models map[string]x.CatalogModel, key string, native bool) x.Outcome {
	v, ok := models[key]
	if !ok {
		return nil
	}
	if v.Unavailable != "" {
		return showNote(v.Unavailable, x.Warning)
	}
	if len(v.ThinkingLevels) > 0 || native {
		return modelThinkingMenu(id, v.ThinkingLevels, v.Model, x.Sheet, native)
	}
	return x.Outcome{x.Client{Op: x.Configure{ThreadID: id, Change: sdk.Configure{Model: sdk.Set(v.Model)}},
		Done: func(_ x.Context, r x.ClientResult) x.Outcome { return clientNote(r, "Model updated") }}}
}

func thinkingMenu(id sdk.ID, available []string, model sdk.Model, place x.Placement) x.Outcome {
	return modelThinkingMenu(id, available, model, place, false)
}

func modelThinkingMenu(id sdk.ID, available []string, model sdk.Model, place x.Placement, native bool) x.Outcome {
	var rows []x.Choice
	for _, v := range available {
		if slices.Contains(levels, v) {
			rows = append(rows, x.Choice{ID: v, Label: v})
		}
	}
	if len(rows) == 0 {
		return showNote("No thinking levels available", x.Warning)
	}
	return x.Outcome{x.SelectDialog{Title: "Thinking level", Placement: place, Choices: rows, Done: func(_ x.Context, reply x.Reply[string]) x.Outcome {
		if !reply.Accepted {
			return nil
		}
		if native {
			note := "Thinking level updated"
			if id == "" {
				note = "Model selected for the first prompt"
			} else if model != (sdk.Model{}) {
				note = "Model updated"
			}
			return x.Outcome{x.Client{Op: x.ConfigureCatalog{ThreadID: id, Model: model, ThinkingLevel: reply.Value}, Done: func(_ x.Context, r x.ClientResult) x.Outcome { return clientNote(r, note) }}}
		}
		change := sdk.Configure{ThinkingLevel: sdk.Set(reply.Value)}
		if model != (sdk.Model{}) {
			change.Model = sdk.Set(model)
		}
		return x.Outcome{x.Client{Op: x.Configure{ThreadID: id, Change: change}, Done: func(_ x.Context, r x.ClientResult) x.Outcome { return clientNote(r, "Thinking level updated") }}}
	}}}
}

func renameMenu(c x.Context, v x.Invocation) x.Outcome {
	id := actionThread(c, v)
	if !id.Valid() {
		return showNote("No thread to rename", x.Warning)
	}
	change := func(title string) x.Outcome {
		return x.Outcome{x.Client{Op: x.SetTitle{ThreadID: id, Title: strings.TrimSpace(title)}, Done: func(_ x.Context, r x.ClientResult) x.Outcome { return clientNote(r, "Thread renamed") }}}
	}
	if strings.TrimSpace(v.Args) != "" {
		return change(v.Args)
	}
	old := ""
	if c.Thread.ID == id {
		old = c.Thread.Title
	}
	return x.Outcome{x.InputDialog{Title: "Rename thread", Initial: old, Placeholder: "Thread title", Done: func(_ x.Context, reply x.Reply[string]) x.Outcome {
		if !reply.Accepted {
			return nil
		}
		return change(reply.Value)
	}}}
}

func modeMenu(c x.Context, _ x.Invocation) x.Outcome {
	if !c.Thread.ID.Valid() {
		return showNote("No thread to configure", x.Warning)
	}
	return x.Outcome{x.Client{Op: x.ListModes{}, Done: func(_ x.Context, r x.ClientResult) x.Outcome {
		if r.Err != nil {
			return clientNote(r, "")
		}
		return x.Outcome{x.SelectDialog{Title: "Permission mode", Placement: x.Sheet, Choices: r.Modes, Done: func(_ x.Context, reply x.Reply[string]) x.Outcome {
			if !reply.Accepted {
				return nil
			}
			return x.Outcome{x.Client{Op: x.SetMode{Mode: reply.Value}, Done: func(_ x.Context, r x.ClientResult) x.Outcome { return clientNote(r, "Permission mode updated") }}}
		}}}
	}}}
}

func archiveMenu(c x.Context, _ x.Invocation) x.Outcome {
	if !c.Thread.ID.Valid() {
		return showNote("No thread to archive", x.Warning)
	}
	if c.Thread.Archived {
		return confirmNative(c, "Unarchive thread?", "Show the thread in the switcher again.", x.SetArchived{Archived: false}, "Thread unarchived")
	}
	return confirmNative(c, "Archive thread?", "Hide the thread without deleting its history or files.", x.SetArchived{Archived: true}, "Thread archived")
}

func confirmNative(c x.Context, title, message string, op x.Operation, ok string) x.Outcome {
	if !c.Thread.ID.Valid() {
		return showNote("No thread selected", x.Warning)
	}
	return x.Outcome{x.ConfirmDialog{Placement: x.Sheet, Title: title, Message: message, Done: func(_ x.Context, reply x.Reply[bool]) x.Outcome {
		if !reply.Accepted || !reply.Value {
			return nil
		}
		return x.Outcome{x.Client{Op: op, Done: func(_ x.Context, r x.ClientResult) x.Outcome { return clientNote(r, ok) }}}
	}}}
}
