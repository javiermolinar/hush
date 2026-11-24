package tui

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/javiermolinar/hush/sdk"
	"github.com/javiermolinar/hush/tui/extension"
)

// Options are copied during New. Hooks that do I/O must return promptly on
// cancellation and must not access shell state.
type Options struct {
	ThreadID        sdk.ID
	NewThread       sdk.NewThread
	Extensions      []extension.Registration
	Keymap          map[extension.ActionID]extension.Binding
	InterceptedKeys []string
	// SegmentOrder overrides segment order by extension ID + ":" + key.
	SegmentOrder map[string]int
	// ProjectThread supplies optional metadata. It is pure and bounded; the SDK snapshot is read-only.
	ProjectThread func(sdk.Snapshot) ThreadMetadata
	Services      MenuServices
	// RestorableEntry reports a known file snapshot; it does not restore files.
	RestorableEntry func(sdk.ID, sdk.ID) bool
	ReducedMotion   bool
	CustomModels    bool
	// ProviderLogin enables /login for a host that can authenticate locally.
	ProviderLogin *ProviderLoginOptions
	// WorkspaceTrust enables the workspace-level tool permission dialog.
	WorkspaceTrust *WorkspaceTrustOptions
	// Intro enables a local startup block in the initial thread.
	Intro *IntroOptions
	// ClientEnvironment describes this invocation, only for user-entered input.
	ClientEnvironment sdk.ClientEnvironment

	// Set WorkspaceLocal only when the client and backend share the workspace.
	Fills          FillMode
	Images         ImageMode // auto uses known Unicode-placeholder terminals; none keeps text only
	WorkspaceLocal bool
	RepositoryURL  string // optional repository root for #PR links
	OpenURL        func(context.Context, string) error
	OpenFile       func(context.Context, string, int) error
	EditPrompt     func(context.Context, string) (string, error)
	CopyText       func(context.Context, string) error
}

// Model is the one loop-owned shell. Use Run for automatic cleanup, or call
// Close after a program using ProgramOptions has stopped (never concurrently).
type Model struct {
	ctx                       context.Context
	cancel                    context.CancelFunc
	backend                   Backend
	options                   Options
	host                      registry
	initialized               int
	started, closed, quitting bool
	threads                   map[sdk.ID]*threadState
	active, previous          sdk.ID
	navigation, serial        uint64
	feed                      feedLease
	reportFeed                feedLease
	directoryFeed             feedLease
	gitDiffFeed               gitDiffPoll
	workspaceTrust            workspaceTrustProbe
	directoryIDs              []sdk.ID
	directoryEnabled          bool
	tabs                      []sdk.ID
	tabFeeds                  map[sdk.ID]feedLease
	focused, disambiguated    bool
	taken                     interceptedKeys
	connection                extension.Connection
	pending                   map[uint64]pendingWork
	overlays                  []*dialog
	sheet                     *dialog
	editor                    promptEditor
	width, height             int
	theme                     extension.Theme
	palette                   palette
	hover                     string
	copied                    copyLabel
	editorTemp                string
	notice                    noticeState
	leader                    string
	escapeAt, stopAt          time.Time
	clock, clockPending       uint64
	phase                     int
	now                       time.Time
	frame                     *frameMemo
	selection                 selection
	slashIndex                int
	files                     filePicker
	fileScope                 fileScopeLease
	nativeFiles               bool
	nativeSkills              bool
	nativeContext             bool
	skills                    skillsLease
	images                    imageState
	intro                     *introState
	introOnly                 bool
	menuFeaturesFromBackend   extension.Features
	slashHidden               bool
}

// New validates the registry before creating a model. Extension initialization
// runs from Init; Run returns any startup error after the program exits.
func New(ctx context.Context, backend Backend, options Options) (*Model, error) {
	if ctx == nil || backend == nil {
		return nil, errors.New("tui: context and backend are required")
	}
	if options.ThreadID != "" && !options.ThreadID.Valid() {
		return nil, sdk.ErrProtocol
	}
	switch options.Fills {
	case "", FillAuto:
		options.Fills = FillOn
		if os.Getenv("TERM_PROGRAM") == "rex" {
			options.Fills = FillOff
		}
	case FillOn, FillOff:
	default:
		return nil, errors.New("tui: invalid fill mode")
	}
	switch options.Images {
	case "", ImageAuto:
		options.Images = autoImages()
	case ImageKitty, ImageBlocks, ImageNone:
	default:
		return nil, errors.New("tui: invalid image mode")
	}
	if login := options.ProviderLogin; login != nil {
		copy := *login
		copy.Providers = slices.Clone(login.Providers[:min(len(login.Providers), maxChoices)])
		options.ProviderLogin = &copy
	}
	if options.WorkspaceTrust != nil {
		copy := *options.WorkspaceTrust
		options.WorkspaceTrust = &copy
	}
	reg, err := makeRegistry(options)
	if err != nil {
		return nil, err
	}
	if options.NewThread.Model != nil {
		model := *options.NewThread.Model
		options.NewThread.Model = &model
	}
	options.Extensions, options.Keymap, options.InterceptedKeys = nil, nil, nil
	life, cancel := context.WithCancel(ctx)
	m := &Model{
		ctx: life, cancel: cancel, backend: backend, options: options, host: reg,
		threads: make(map[sdk.ID]*threadState), pending: make(map[uint64]pendingWork),
		active: options.ThreadID, connection: extension.Connecting,
		focused: true, tabFeeds: make(map[sdk.ID]feedLease),
		editor: newPrompt(), width: 80, height: 24, frame: &frameMemo{},
		images: imageState{mode: options.Images},
	}
	m.editor.area.SetVirtualCursor(false)
	m.ensureThread("")
	t := m.ensureThread(m.active)
	if options.Intro != nil {
		m.intro = newIntro(*options.Intro, t, options)
		t.intro, t.following, t.anchor = m.intro, false, rowAnchor{id: introEntry}
		m.options.Intro = nil // the startup options are already copied
	}
	m.openedTab(m.active)
	return m, nil
}

func (m *Model) token() uint64 { m.serial++; return m.serial }

func (m *Model) Init() tea.Cmd {
	if m.started || m.closed {
		return nil
	}
	m.started = true
	var initial []extension.Outcome
	owner := m.active
	for _, r := range m.host.registrations {
		var out extension.Outcome
		if r.Init != nil {
			var err error
			out, err = r.Init(m.contextFor(owner))
			if err != nil {
				m.host.startupErr = err
				m.Close()
				return tea.Quit
			}
		}
		initial = append(initial, out)
		m.initialized++
	}
	feed := m.beginFeed()
	if m.intro != nil {
		feed = startBeforePaint(m.ctx, feed)
	}
	commands := []tea.Cmd{feed, m.editor.area.Focus(), terminalQueries(), terminalKeymap(m.ctx),
		m.menuProbe(), m.beginIntro(), m.probeImages(), waitImageScreen(m.ctx, m.images.output), waitImagePaint(m.ctx, m.images.output)}
	if m.intro == nil || m.intro.options.Watch != nil {
		commands = append(commands, m.beginBackendReport())
	}
	m.emit(owner, extension.Event{Kind: extension.ConnectionChanged})
	budget := maxEffects
	for i, out := range initial {
		commands = append(commands, m.admit(m.route(owner, m.host.registrations[i].ID), out, &budget))
	}
	return tea.Batch(append(commands, m.syncClock(), m.syncWorkspaceTrust())...)
}

// Close cancels local work and tears down initialized extensions. It does not
// close the caller's SDK client or abort durable work.
func (m *Model) Close() {
	if m.editorTemp != "" {
		_ = os.RemoveAll(m.editorTemp)
		m.editorTemp = ""
	}
	if m.closed {
		return
	}
	m.closed = true
	m.cancel()
	if m.intro != nil {
		m.intro.motion.stopTick()
	}
	m.stopFeed()
	m.stopBackendReport()
	m.stopGitDiff()
	m.stopFileScope()
	m.stopSkills()
	for id, work := range m.pending {
		work.cancel()
		delete(m.pending, id)
	}
	m.overlays, m.sheet = nil, nil
	for i := m.initialized - 1; i >= 0; i-- {
		if close := m.host.registrations[i].Shutdown; close != nil {
			close(m.contextFor(m.active))
		}
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.closed {
		return m, nil
	}
	m.introOnly = false
	if handled, cmd := m.terminalMessage(msg); handled {
		if v, ok := msg.(tea.ColorProfileMsg); ok && m.intro != nil {
			m.intro.profile = v.Profile
		}
		return m, cmd
	}
	var cmd tea.Cmd
	var scanImages bool
	switch v := msg.(type) {
	case imageCapability:
		m.images.capable = bool(v)
		m.images.revision++
		scanImages = true
	case imageScan:
		return m, m.scanImages()
	case imageScreen:
		if v.epoch > m.images.screen.epoch {
			m.images.screen = v
			m.images.revision++
			scanImages = true
		}
		cmd = waitImageScreen(m.ctx, m.images.output)
	case imagePainted:
		if out := m.images.output; out != nil {
			cmd = tea.Batch(out.redrawImage(m.ctx, v.placement), waitImagePaint(m.ctx, out))
		}
	case imageFetched:
		cmd = m.cacheImage(v)
		scanImages = true
	case imagePlaced:
		m.images.busy, m.images.placing = false, 0
		if a := m.images.assets[v.ref]; a != nil && v.ok && v.placement.screen == m.images.screen {
			a.placed = v.placement
			m.images.revision++
		}
		scanImages = v.ok || v.placement.screen != m.images.screen
	case uv.KittyGraphicsEvent:
		cmd = m.imageError(v)
		scanImages = true
	case introTick:
		return m, m.tickIntro(v)
	case introStamp:
		if m.intro != nil {
			cmd = m.receiveIntroStamp(v)
		}
	case introReportMsg:
		cmd = m.receiveIntro(v)
	case backendReportMsg:
		cmd = m.receiveBackendReport(v)
	case tea.WindowSizeMsg:
		scanImages = true
		m.width, m.height = max(0, v.Width), max(0, v.Height)
		m.selection = selection{}
		cmd = tea.Batch(m.resizeEditors(), m.resizeCustom())
	case feedMsg:
		cmd = m.receiveFeed(v)
		scanImages = true
	case tabFeed:
		cmd = m.receiveTab(v)
	case menuCapabilities:
		m.menuFeaturesFromBackend = v.features
		m.directoryEnabled, m.nativeFiles, m.nativeSkills, m.nativeContext = v.directory, v.files, v.skills, v.context
		cmd = tea.Batch(m.syncDirectory(), m.syncGitDiff())
	case skillsMsg:
		m.receiveSkills(v)
	case fileScopeMsg:
		cmd = m.receiveFileScope(v)
	case renewFileScope:
		if uint64(v) == m.fileScope.generation {
			cmd = m.fileScopeCommand()
		}
	case nativeFilePage:
		cmd = m.receiveNativeFilePage(v)
	case gitDiffTick:
		if uint64(v) == m.gitDiffFeed.tick {
			cmd = m.scanGitDiff()
		}
	case gitDiffSummaryMsg:
		cmd = m.receiveGitSummary(v)
	case gitDiffPatchMsg:
		cmd = m.receiveGitPatch(v)
	case workspaceTrustMsg:
		cmd = m.receiveWorkspaceTrust(v)
	case directoryFeedMsg:
		cmd = m.receiveDirectory(v)
	case tea.FocusMsg:
		m.focused = true
	case tea.BlurMsg:
		m.focused = false
	case tea.KeyboardEnhancementsMsg:
		m.disambiguated = true
	case interceptedKeys:
		m.taken = v
	case historyMsg:
		cmd = m.receiveHistory(v)
		scanImages = true
	case workMsg:
		cmd = m.finishWork(v)
		scanImages = true
	case admissionExpired:
		if t := m.threads[v.owner]; t != nil && t.life == v.life {
			t.forgetAdmission(v.submission)
			if t.awaiting == v.submission {
				t.awaiting = ""
			}
		}
	case queueResolved:
		if t := m.threads[v.owner]; t != nil && t.life == v.life {
			t.forgetAdmission(v.submission)
		}
	case dialogMsg:
		cmd = m.receiveDialog(v)
	case modelSearchResult:
		m.receiveModelSearch(v)
		scanImages = true
	case openFinished:
		if v.err != nil {
			cmd = m.notifyRoute(v.route, v.err.Error(), extension.Danger)
		}
	case promptEditFinished:
		cmd = m.finishPromptEdit(v)
	case loginFinished:
		cmd = m.finishLogin(v)
	case copyLabelExpired:
		if m.copied.token == v.token {
			m.copied = copyLabel{}
		}
	case noticeExpired:
		scanImages = true
		if m.notice.token == v.token {
			m.notice = noticeState{}
		}
	case effectTick:
		if v.token == m.clock {
			m.clockPending = 0
			m.now = v.at
			if !m.options.ReducedMotion {
				m.phase = (m.phase + 1) % 4
			}
		}
	case tea.KeyPressMsg:
		scanImages = true
		m.snapIntroKey()
		m.selection = selection{}
		if len(m.overlays) != 0 || m.sheet != nil {
			cmd = m.dialogInput(v)
		} else {
			cmd = tea.Batch(m.promptKey(v), m.refreshFilePicker())
		}
	case tea.MouseClickMsg, tea.MouseReleaseMsg, tea.MouseMotionMsg, tea.MouseWheelMsg:
		cmd = m.mouse(msg)
		_, motion := msg.(tea.MouseMotionMsg)
		scanImages = !motion
	case tea.PasteMsg:
		m.snapIntroKey()
		if len(m.overlays) != 0 || m.sheet != nil {
			cmd = m.dialogInput(v)
		} else {
			cmd = tea.Batch(m.pastePrompt(v), m.refreshFilePicker())
		}
		scanImages = true
	default:
		// Editor blink messages are local; custom commands are always wrapped.
		if d := m.focusedDialog(); d != nil {
			if d.input != nil {
				cmd = d.input.update(msg, d.multiline)
			}
		} else {
			cmd = m.updatePrompt(msg)
		}
	}
	if scanImages {
		cmd = tea.Batch(cmd, m.scheduleImageScan())
	}
	return m, tea.Batch(cmd, m.syncClock(), m.syncIntroClock(), m.syncWorkspaceTrust(), m.syncFileScope(), m.syncSkills())
}

func (m *Model) ensureThread(id sdk.ID) *threadState {
	if t := m.threads[id]; t != nil {
		return t
	}
	t := &threadState{life: m.token(), info: extension.Thread{ID: id}, entries: make(map[sdk.ID]*record),
		status: make(map[string]extension.SetStatus), following: true, customEntries: m.host.customEntries}
	m.threads[id] = t
	return t
}

func (m *Model) switchThread(id sdk.ID) tea.Cmd {
	if id == m.active {
		return nil
	}
	previous := m.active
	m.threads[previous].draft = m.editor.value()
	m.editor.resetHistory()
	m.active, m.previous = id, previous
	m.openedTab(id)
	m.navigation++
	t := m.ensureThread(id)
	m.editor.set(t.draft)
	m.leader, m.escapeAt, m.stopAt, m.slashHidden, m.slashIndex = "", time.Time{}, time.Time{}, false, 0
	m.notice, m.selection, m.hover = noticeState{}, selection{}, ""
	// Old dialogs cannot stay focused on a different transcript. Dismissals
	// retain their old owner (and cannot navigate via a stale continuation).
	old := m.overlays
	if m.sheet != nil {
		old = append(old, m.sheet)
	}
	m.overlays, m.sheet = nil, nil
	m.emit(id, extension.Event{Kind: extension.ThreadSwitched, PreviousThreadID: previous})
	cmds := []tea.Cmd{m.beginFeed(), m.beginBackendReport(), m.editor.area.Focus(), m.syncTabFeeds(), m.syncDirectory(), m.syncGitDiff()}
	budget := maxEffects
	for _, d := range old {
		cmds = append(cmds, m.replyDialog(d, nil, false, &budget))
	}
	return tea.Batch(cmds...)
}

func (m *Model) contextFor(id sdk.ID) extension.Context {
	c := extension.Context{ActiveThreadID: m.active, PreviousThreadID: m.previous, Connection: m.connection, Thread: extension.Thread{ID: id}}
	if id != m.active {
		c.Connection = extension.Disconnected
	}
	c.Features = m.menuFeatures()
	if m.options.CustomModels {
		c.Features |= extension.CustomModels
	}
	if t := m.threads[id]; t != nil {
		c.Thread, c.Revision = t.info, t.revision
	}
	return c
}

func (m *Model) emit(owner sdk.ID, event extension.Event) {
	if m.closed {
		return
	}
	for _, sub := range m.host.events[event.Kind] {
		copy := event
		if copy.Kind == extension.EntryObserved {
			copy.Entry = detachEntry(event.Entry.Record)
			copy.Entry.Truncated = copy.Entry.Truncated || event.Entry.Truncated
		}
		if copy.Kind == extension.StreamChanged && copy.Stream.Present {
			copy.Stream = detachStream(&event.Stream.Value)
			copy.Stream.Truncated = copy.Stream.Truncated || event.Stream.Truncated
		}
		sub(m.contextFor(owner), copy)
	}
}

type noticeState struct {
	token uint64
	text  string
	tone  extension.Tone
}
type noticeExpired struct{ token uint64 }

func (m *Model) notify(owner sdk.ID, text string, tone extension.Tone) tea.Cmd {
	if owner != m.active || text == "" {
		return nil
	}
	id := m.token()
	m.notice = noticeState{id, limitText(text, 1024), tone}
	duration := 5 * time.Second
	switch tone {
	case extension.Warning:
		duration = 8 * time.Second
	case extension.Danger:
		duration = 30 * time.Second
	}
	return tea.Tick(duration, func(time.Time) tea.Msg { return noticeExpired{id} })
}

type effectTick struct {
	token uint64
	at    time.Time
}

func (m *Model) syncClock() tea.Cmd {
	t := m.threads[m.active]
	phase, _, _ := t.liveStatus()
	moving := phase != liveIdle && phase != liveFailed && phase != liveResponding && phase != liveWorking
	timed := moving && phase != liveSending && phase != liveStarting
	animated := !m.options.ReducedMotion && (moving || m.connection == extension.Connecting && m.active != "" && (t.intro == nil || t.intro.frozen))
	visible := m.width >= 40 && m.height >= 12 && (animated || timed)
	if !visible {
		if m.clockPending != 0 {
			m.clock++
			m.clockPending = 0
		}
		m.phase = 0
		return nil
	}
	if m.clockPending != 0 {
		return nil
	}
	m.clock++
	id := m.clock
	m.clockPending = id
	interval := 50 * time.Millisecond
	if m.options.ReducedMotion {
		interval = time.Second
	}
	return tea.Tick(interval, func(at time.Time) tea.Msg { return effectTick{id, at} })
}

const maxEffects = 48
const maxPending = 64

// Navigation gates presentation/focus, life gates the owner itself. Inactive
// owners still receive their non-presentation work replies.
type ownerRoute struct {
	owner            sdk.ID
	life, navigation uint64
	extension        string
}

func (m *Model) route(id sdk.ID, ext string) ownerRoute {
	return ownerRoute{id, m.threads[id].life, m.navigation, ext}
}
func (m *Model) live(r ownerRoute) bool {
	t := m.threads[r.owner]
	return !m.closed && t != nil && t.life == r.life
}
func (m *Model) foreground(r ownerRoute) bool {
	return m.live(r) && m.active == r.owner && m.navigation == r.navigation
}

type pendingWork struct {
	route  ownerRoute
	cancel context.CancelFunc
	budget *int
	done   func(any, error, *int) tea.Cmd // never captured by the command
}
type workMsg struct {
	id     uint64
	result any
	err    error
}

func (m *Model) startWork(r ownerRoute, run func(context.Context) (any, error), done func(any, error, *int) tea.Cmd, budget *int) tea.Cmd {
	if !m.live(r) || run == nil {
		return nil
	}
	if len(m.pending) >= maxPending {
		if done != nil {
			return done(nil, extension.ErrUnavailable, budget)
		}
		return m.notifyRoute(r, "Too many pending requests", extension.Warning)
	}
	id := m.token()
	ctx, cancel := context.WithCancel(m.ctx)
	m.pending[id] = pendingWork{r, cancel, budget, done}
	return func() tea.Msg {
		if ctx.Err() != nil {
			return nil
		}
		result, err := run(ctx)
		return workMsg{id, result, err}
	}
}

func (m *Model) finishWork(msg workMsg) tea.Cmd {
	w, ok := m.pending[msg.id]
	if !ok {
		return nil
	}
	delete(m.pending, msg.id)
	w.cancel()
	if !m.live(w.route) {
		return nil
	}
	if w.done != nil {
		return w.done(msg.result, msg.err, w.budget)
	}
	if msg.err != nil {
		return m.notifyRoute(w.route, msg.err.Error(), extension.Danger)
	}
	return nil
}

func (m *Model) notifyRoute(r ownerRoute, text string, tone extension.Tone) tea.Cmd {
	if !m.foreground(r) {
		return nil
	}
	return m.notify(r.owner, text, tone)
}

func (m *Model) eligible(a registeredAction, owner sdk.ID) bool {
	return m.eligibleFor(a, owner, "")
}

func (m *Model) eligibleFor(a registeredAction, owner sdk.ID, target string) bool {
	ctx := m.contextFor(owner)
	if m.closed || !ctx.Features.Has(a.action.Requires) || a.action.Available != nil && !a.action.Available(ctx) {
		return false
	}
	if a.action.ID == pasteImageID {
		return m.images.capable
	}
	if a.action.ID == retryFailureID || a.action.ID == dismissFailureID {
		recovery := m.recoveryFor(owner, target)
		return recovery.task.Valid() && (a.action.ID == dismissFailureID || recovery.retry)
	}
	return true
}

func (m *Model) invoke(r ownerRoute, ref extension.ActionRef, budget *int) tea.Cmd {
	if *budget <= 0 || !m.live(r) {
		return nil
	}
	if ref.ID == stageSkillID {
		if m.foreground(r) {
			if name, token, ok := strings.Cut(ref.Target, "\n"); ok {
				m.stageSkill(stagedSkill{name, token})
			}
		}
		return nil
	}
	i, ok := m.host.byID[ref.ID]
	if !ok || !m.eligibleFor(m.host.actions[i], r.owner, ref.Target) {
		return nil
	}
	*budget = *budget - 1
	a := m.host.actions[i]
	r.extension = a.owner
	return m.admit(r, a.action.Run(m.contextFor(r.owner), extension.Invocation{Args: limitText(ref.Args, maxPromptBytes), Target: limitText(ref.Target, 1024)}), budget)
}

func valueOf[T any](x T) T {
	v := reflect.ValueOf(x)
	if v.IsValid() && v.Kind() == reflect.Pointer {
		if v.IsNil() {
			var zero T
			return zero
		}
		if y, ok := v.Elem().Interface().(T); ok {
			return y
		}
	}
	return x
}

func (m *Model) admit(r ownerRoute, out extension.Outcome, budget *int) tea.Cmd {
	var commands []tea.Cmd
	for _, e := range out {
		if !m.live(r) {
			break
		}
		if *budget <= 0 {
			commands = append(commands, m.notifyRoute(r, "Extension effect limit reached", extension.Warning))
			break
		}
		*budget = *budget - 1
		var cmd tea.Cmd
		switch e := valueOf(e).(type) {
		case extension.Notice:
			cmd = m.notifyRoute(r, e.Text, e.Tone)
		case extension.SetStatus:
			if !ident.MatchString(e.Key) {
				break
			}
			statuses := m.threads[r.owner].status
			key := r.extension + ":" + e.Key
			if e.Text == "" {
				delete(statuses, key)
			} else if _, exists := statuses[key]; exists || len(statuses) < 64 {
				e.Text = limitText(e.Text, 1024)
				statuses[key] = e
			}
		case extension.Dispatch:
			cmd = m.invoke(r, e.Action, budget)
		case extension.Local:
			cmd = m.localEffect(r, e, budget)
		case extension.Client:
			cmd = m.clientEffect(r, e, budget)
		case extension.Work:
			var done func(any, error, *int) tea.Cmd
			if e.Done != nil {
				done = func(v any, err error, left *int) tea.Cmd {
					return m.admit(r, e.Done(m.contextFor(r.owner), v, err), left)
				}
			}
			if e.Run != nil {
				cmd = m.startWork(r, e.Run, done, budget)
			}
		case extension.SelectDialog, extension.ConfirmDialog, extension.ReviewDialog, extension.InputDialog, extension.CustomDialog:
			cmd = m.openDialog(r, e, budget)
		}
		commands = append(commands, cmd)
	}
	return tea.Batch(commands...)
}

func (m *Model) clientEffect(r ownerRoute, e extension.Client, budget *int) tea.Cmd {
	finish := func(result extension.ClientResult, left *int) tea.Cmd {
		if e.Done != nil {
			return m.admit(r, e.Done(m.contextFor(r.owner), result), left)
		}
		if result.Err != nil {
			return m.notifyRoute(r, result.Err.Error(), extension.Danger)
		}
		return nil
	}
	op := valueOf(e.Op)
	if v, ok := op.(extension.ConfigureCatalog); ok && r.owner == "" && v.ThreadID == "" && m.menuFeatures().Has(extension.NativeCatalogDraft) {
		t := m.threads[r.owner]
		if !m.foreground(r) || t.sending || v.Model == (sdk.Model{}) || !slices.Contains(levels, v.ThinkingLevel) {
			return finish(extension.ClientResult{Err: extension.ErrUnavailable}, budget)
		}
		t.info.Model, t.info.Thinking = v.Model, v.ThinkingLevel
		t.revision++
		if i := t.intro; i != nil && !i.frozen {
			i.refresh(i.motion.clock.now())
		}
		m.emit(r.owner, extension.Event{Kind: extension.StatusChanged})
		return finish(extension.ClientResult{}, budget)
	}
	if sw, ok := op.(extension.Switch); ok {
		if !m.foreground(r) || !sw.ThreadID.Valid() {
			return finish(extension.ClientResult{Err: extension.ErrUnavailable}, budget)
		}
		cmd := m.switchThread(sw.ThreadID)
		if !m.live(r) {
			return cmd
		}
		return tea.Batch(cmd, finish(extension.ClientResult{ThreadID: sw.ThreadID}, budget))
	}
	var id sdk.ID
	threadScoped := true
	switch v := op.(type) {
	case extension.Submit:
		id = v.ThreadID
	case extension.Steer:
		id = v.ThreadID
	case extension.Fork:
		id = v.ThreadID
	case extension.Abort:
		id = v.ThreadID
	case extension.Configure:
		id = v.ThreadID
	case extension.Compact:
		id = v.ThreadID
	case extension.Reset:
		id = v.ThreadID
	case extension.History:
		id = v.ThreadID
	default:
		threadScoped = false
	}
	if threadScoped {
		if id == "" {
			id = r.owner
		}
		if !id.Valid() {
			return finish(extension.ClientResult{Err: sdk.ErrProtocol}, budget)
		}
	}
	return m.startWork(r, m.clientJob(r, op), func(v any, err error, left *int) tea.Cmd {
		result, _ := v.(extension.ClientResult)
		if err != nil {
			result.Err = err
		}
		m.applyMenuResult(op, &result)
		return finish(result, left)
	}, budget)
}

func (m *Model) localEffect(r ownerRoute, e extension.Local, budget *int) tea.Cmd {
	if !m.live(r) {
		return nil
	}
	t := m.threads[r.owner]
	switch e.Op {
	case openLoadedContextOp:
		return m.openLoadedContext(r, budget)
	case openGitDiffOp:
		return m.openGitDiff(r)
	case openWorkspaceTrustOp:
		return m.openWorkspaceTrust(r)
	case openUsageOp:
		return m.openUsage(r, t, budget)
	case retryFailureOp, dismissFailureOp:
		return m.recoverRun(r, e, budget)
	case providerLoginOp:
		return m.startLogin(r, e.Text, budget)
	case modelSearchOp:
		return m.openModelSearch(r, e.ThreadID, e.Text, budget)
	case pasteImageOp:
		if m.foreground(r) {
			return m.pasteClipboardImage()
		}
	case extension.OpenPalette:
		if m.foreground(r) {
			return m.openPalette(r, budget)
		}
	case extension.CloseTab:
		if m.foreground(r) {
			id := e.ThreadID
			if id == "" {
				id = r.owner
			}
			if id.Valid() && m.threads[id] != nil {
				return m.closeThread(id)
			}
		}
	case stageSkillOp:
		if m.foreground(r) {
			if name, token, pinned := strings.Cut(e.Text, "\n"); pinned {
				m.stageSkill(stagedSkill{name, token})
			}
		}
	case extension.CompleteFile:
		if m.foreground(r) {
			return m.replaceFile(e.Text)
		}
	case extension.ToggleDetails:
		t.expanded = !t.expanded
		t.expandEpoch++
		t.liveOverride = false
	case extension.ToggleActivity:
		if target, ok := strings.CutPrefix(e.Text, outputTargetPrefix); ok {
			return m.openToolOutput(r, target)
		}
		if t.toggleBurst(e.Text) {
			break
		}
		if e.Text == "live" {
			value := t.expanded
			if t.liveOverride {
				value = t.liveExpanded
			}
			t.liveOverride, t.liveExpanded = true, !value
		} else if row := t.entries[sdk.ID(e.Text)]; row != nil {
			value := t.expanded
			if row.override && row.expandEpoch == t.expandEpoch {
				value = row.expanded
			}
			row.expandEpoch, row.override, row.expanded = t.expandEpoch, true, !value
		}
	case extension.ReplacePrompt:
		if m.foreground(r) {
			m.editor.set(e.Text)
			m.slashHidden = false
		} else if r.owner != m.active {
			t.draft = cleanPrompt(e.Text)
		}
	case extension.EditPrompt:
		if !m.foreground(r) {
			return nil
		}
		hook, text, rev := m.options.EditPrompt, m.editor.value(), m.editor.revision
		if hook == nil {
			return m.editPrompt(r, text, rev, budget)
		}
		return m.startWork(r, func(ctx context.Context) (any, error) { return hook(ctx, text) }, func(v any, err error, _ *int) tea.Cmd {
			if err != nil {
				return m.notifyRoute(r, err.Error(), extension.Danger)
			}
			if m.foreground(r) && m.editor.revision == rev {
				if edited, ok := v.(string); ok {
					m.editor.set(edited)
					m.slashHidden = false
				}
			} else {
				return m.notifyRoute(r, "Prompt changed; external edit not applied", extension.Warning)
			}
			return nil
		}, budget)
	case extension.CopyText:
		return m.copyText(r, e.Text, budget)
	case extension.CopyLastAnswer:
		if t.lastAnswer == "" {
			return m.notifyRoute(r, "No answer to copy", extension.Warning)
		}
		return m.copyText(r, t.lastAnswer, budget)
	case extension.CopyLastCode:
		return m.copyLastCode(r, budget)
	case extension.RecallQueued:
		return m.notifyRoute(r, "Cannot recall admitted input without stopping the run", extension.Warning)
	case extension.Quit:
		if m.foreground(r) {
			m.quitting = true
			return tea.Quit
		}
	}
	return nil
}

func (m *Model) copyText(r ownerRoute, text string, budget *int) tea.Cmd {
	hook := m.options.CopyText
	if !m.foreground(r) || text == "" {
		return nil
	}
	text = limitText(text, 128<<10)
	if hook == nil {
		return tea.Batch(tea.SetClipboard(text), m.notifyRoute(r, "Copied", extension.Good))
	}
	return m.startWork(r, func(ctx context.Context) (any, error) { return nil, hook(ctx, text) }, func(_ any, err error, _ *int) tea.Cmd {
		if err != nil {
			return m.notifyRoute(r, err.Error(), extension.Danger)
		}
		return m.notifyRoute(r, "Copied", extension.Good)
	}, budget)
}

func (m *Model) closeThread(id sdk.ID) tea.Cmd {
	for token, w := range m.pending {
		if w.route.owner == id {
			w.cancel()
			delete(m.pending, token)
		}
	}
	m.freezeIntro(m.threads[id])
	m.threads[id].life = m.token() // owner teardown invalidates dialog replies too
	m.tabsWithout(id)
	if id != m.active {
		delete(m.threads, id)
		return tea.Batch(m.syncTabFeeds(), m.syncDirectory())
	}
	var next sdk.ID
	if len(m.tabs) > 0 {
		next = m.tabs[len(m.tabs)-1]
	}
	cmd := m.switchThread(next)
	delete(m.threads, id)
	return cmd
}

func submissionKey() string { return "hush-tui-" + rand.Text() }
