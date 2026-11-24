package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

// IntroOptions enables the local startup report. StatePath is an optional
// client-owned timestamp file, separate from the runtime's state. Watch can
// supply live report data from a host with more than the native v1 wire view.
// It is called once off the UI loop. Send immutable replacement snapshots,
// honor cancellation, and handle reconnection in the source. Without Watch,
// use the negotiated SDK report or fall back to thread listing and the active feed.
type IntroOptions struct {
	StatePath string
	Watch     func(context.Context) <-chan IntroUpdate
}

// IntroUpdate is a new host report, or a transient attachment error. An error
// preserves the last report, with the connection marked offline until recovery.
type IntroUpdate struct {
	Report IntroReport
	Err    error
}

// IntroReport contains only observations the host actually has.
type IntroReport struct {
	StartedAt       time.Time // runtime start, if known
	Workspace       string
	Model           *sdk.Model
	Thinking        string
	Sessions        *IntroSessions // nil when aggregate state is unavailable
	SessionsPartial bool
	Attention       []IntroAttention // actionable settled failures and parked sessions
	MoreAttention   int              // additional attention rows not included in the snapshot
	Thread          *sdk.IntroThread // presentation metadata for an explicitly selected thread
	Skills          []string         // loaded skill names in display order
	MoreSkills      int              // additional names not included in the snapshot
	Context         *IntroContext    // nil when unavailable
}

// IntroSessions counts session states over the same host inventory.
type IntroSessions struct{ Open, Running, Quiet int }

// IntroAttention describes an actionable failure or a parked session.
type IntroAttention struct {
	Title    string
	Status   x.ThreadStatus // Failed, Stopped, or Parked
	Reason   string         // legacy failure category or park reason
	Until    time.Time      // known wake deadline, in the host's display time zone
	ThreadID sdk.ID
	Issue    *sdk.IntroRunIssue
}

// IntroContext is input sizing for the active model.
type IntroContext struct {
	Window         uint64
	RenderedBytes  uint64  // rendered system sections, tool definitions, skills and project instructions combined
	ReportedTokens *uint64 // actual context occupancy, not cumulative usage; nil uses the byte estimate
}

// introEntry is a local scrollback anchor, not a valid durable ID. It is
// never inserted in the SDK replica, durable history or fork picker.
const introEntry sdk.ID = "intro"

const (
	introHeading = iota
	introStatus
	introModel
	introProblem
	introSessions
	introAttention
	introAttentionSecond
	introAttentionThird
	introReview
	introSkills
	introContext
	introFieldCount
)

const introSkeleton = "·····"

type introLabel struct {
	text, title, age string
	tone             x.Tone
	actions          runActions
}

type introReportMsg struct {
	update IntroUpdate
	listed int
	more   bool
	native bool
	rich   bool
	next   tea.Cmd
}

type introState struct {
	options IntroOptions
	owner   *threadState
	cancel  context.CancelFunc
	home    string
	workdir string

	profile                  colorprofile.Profile
	noColor, noMotion, tty   bool
	frozen, reportReady      bool
	reportProblem            string
	expectReport, richReport bool
	feedReady, feedError     bool
	listed                   *int
	moreListed               bool
	standaloneSkillCount     *int
	labels                   [introFieldCount]introLabel
	report                   IntroReport
	revision                 uint64

	motion introMotion
	memo   *introMemo
}

func newIntro(opts IntroOptions, owner *threadState, base Options) *introState {
	i := &introState{options: opts, owner: owner, workdir: base.NewThread.CWD,
		noColor: os.Getenv("NO_COLOR") != "", noMotion: base.ReducedMotion || os.Getenv("HUSH_NO_ANIM") == "1" || os.Getenv("TERM") == "dumb",
		expectReport: opts.Watch != nil, memo: &introMemo{}, motion: newIntroMotion()}
	if base.WorkspaceLocal {
		i.home, _ = os.UserHomeDir()
	}
	i.refresh(i.motion.clock.now())
	return i
}

// startBeforePaint launches only an already detached command; neither this
// goroutine nor the returned command can read or change the model. Bubble Tea
// normally executes Init's commands after asking for the first View.
func startBeforePaint(ctx context.Context, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	out := make(chan tea.Msg, 1)
	go func() { out <- cmd() }()
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case v := <-out:
			return v
		}
	}
}

func (m *Model) beginIntro() tea.Cmd {
	i := m.intro
	if i == nil {
		return nil
	}
	i.motion.start(i.motion.clock.now(), i.tty && !i.noMotion && m.height >= 24, i.options.StatePath != "")
	watch := i.options.Watch
	_, sdkReport := m.backend.(introBackend)
	i.expectReport = watch != nil || sdkReport
	i.refresh(i.motion.clock.now())
	var report tea.Cmd
	if watch != nil {
		ctx, cancel := context.WithCancel(m.ctx)
		i.cancel = cancel
		report = startBeforePaint(ctx, func() tea.Msg { return awaitIntroReport(ctx, watch(ctx))() })
	} else if sdkReport {
		report = startBeforePaint(m.ctx, m.beginBackendReport())
	} else {
		report = m.beginBasicIntro(i)
	}
	var cooldown tea.Cmd
	if path := i.options.StatePath; path != "" {
		// Always record a shown intro, even if its motion is disabled. Once
		// the stat returns, painting does not depend on the timestamp write.
		cooldown = func() tea.Msg { return readIntroStamp(path) }
	}
	return tea.Batch(report, cooldown, m.syncIntroClock())
}

func (m *Model) beginBasicIntro(i *introState) tea.Cmd {
	if i.cancel != nil {
		i.cancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	i.cancel = cancel
	out := make(chan introReportMsg)
	go listIntroThreads(ctx, m.backend, out)
	return awaitIntroList(ctx, out)
}

func awaitIntroReport(ctx context.Context, src <-chan IntroUpdate) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case v, ok := <-src:
			if !ok {
				return nil
			}
			return introReportMsg{update: v, next: awaitIntroReport(ctx, src)}
		}
	}
}

func awaitIntroList(ctx context.Context, src <-chan introReportMsg) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case v, ok := <-src:
			if !ok {
				return nil
			}
			if v.update.Err != nil {
				v.next = awaitIntroList(ctx, src)
			}
			return v
		}
	}
}

func listIntroThreads(ctx context.Context, backend Backend, out chan<- introReportMsg) {
	defer close(out)
	for delay := 250 * time.Millisecond; ctx.Err() == nil; delay = min(4*time.Second, 2*delay) {
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		page, err := backend.ListThreads(call, 100, "")
		cancel()
		msg := introReportMsg{update: IntroUpdate{Err: err}, native: true, listed: len(page.Threads), more: page.NextCursor != nil}
		select {
		case <-ctx.Done():
			return
		case out <- msg:
		}
		if err == nil || !waitIntro(ctx, delay) {
			return
		}
	}
}

func (m *Model) receiveIntro(v introReportMsg) tea.Cmd {
	i := m.intro
	if i == nil || i.frozen {
		return nil
	}
	i.reportProblem = ""
	if err := v.update.Err; err != nil {
		var transport *sdk.TransportError
		if errors.As(err, &transport) {
			err = transport.Err
		}
		i.reportProblem = oneLine(limitText(err.Error(), 512))
	}
	if v.native {
		i.expectReport, i.richReport = false, false
		i.report = IntroReport{}
		if v.update.Err == nil {
			m.menuFeaturesFromBackend &^= x.Skills
		}
	} else if v.rich && v.update.Err == nil {
		i.richReport = true
		if len(v.update.Report.Skills) != 0 || v.update.Report.MoreSkills > 0 {
			m.menuFeaturesFromBackend |= x.Skills
		}
	}
	if v.update.Err == nil {
		i.reportReady = true
		if v.native {
			i.listed, i.moreListed = ptr(v.listed), v.more
		} else {
			i.report = v.update.Report
			if !v.rich {
				i.owner.footer = metadataFromReport(i.report)
				m.applyPresentation(i.owner, i.report.Thread)
			}
		}
	}
	i.refresh(i.motion.clock.now())
	return v.next
}

func (m *Model) introFeed(t *threadState, ready, offline bool) {
	i := t.intro
	if i == nil || i.frozen {
		return
	}
	i.feedReady, i.feedError = ready, offline
	i.refresh(i.motion.clock.now())
}

func (m *Model) freezeIntro(t *threadState) {
	i := t.intro
	if i == nil || i.frozen {
		return
	}
	i.refresh(i.motion.clock.now())
	i.frozen = true
	i.revision++ // the snapshot remains visible, but recovery controls are retired
	for k, label := range i.labels {
		if label.text == introSkeleton {
			i.labels[k] = introLabel{} // unavailable, not permanently loading
			i.revision++
		}
	}
	i.motion.snap()
	if i.cancel != nil {
		i.cancel()
		i.cancel = nil
	}
	i.owner, i.report = nil, IntroReport{}
	// The next user entry belongs at the tail, even when the report opened
	// above an existing transcript.
	t.following = true
}

func (i *introState) refresh(at time.Time) {
	t := i.owner.info
	r := i.report
	var next [introFieldCount]introLabel
	put := func(key int, s string, tone x.Tone) { next[key] = introLabel{text: s, tone: tone} }
	pending := func(key int) { put(key, introSkeleton, x.Muted) }
	branch := t.Branch
	if branch == "" {
		branch = "main"
	}
	cwd := t.Workspace
	if cwd == "" {
		cwd = r.Workspace
	}
	if cwd == "" {
		cwd = i.workdir
	}
	if i.home != "" && cwd == i.home {
		cwd = "~"
	} else if i.home != "" {
		if rel, ok := strings.CutPrefix(cwd, i.home+string(filepath.Separator)); ok {
			cwd = "~/" + rel
		}
	}
	put(introHeading, "⎇ "+oneLine(branch)+"  "+oneLine(limitText(cwd, 512)), x.Plain)
	switch {
	case i.feedError || i.reportProblem != "" && !i.feedReady:
		put(introStatus, "○ runtime offline · retrying", x.Warning)
		put(introProblem, i.reportProblem, x.Warning)
	case i.feedReady || i.reportReady:
		status := "● attached"
		if up := at.Sub(r.StartedAt); !r.StartedAt.IsZero() && up >= 0 {
			status += " · up " + introUptime(up)
		}
		put(introStatus, status, x.Accent)
	default:
		pending(introStatus)
	}
	model, thinking := t.Model, t.Thinking
	draftModel := t.ID == "" && model != (sdk.Model{})
	if r.Model != nil && !i.owner.observed && !draftModel {
		model, thinking = *r.Model, r.Thinking
	}
	if model.ModelID != "" {
		put(introModel, "◇ "+oneLine(model.ModelID)+"  "+oneLine(thinking), x.Plain)
	} else if t.ID != "" && !i.owner.observed || i.expectReport && !i.reportReady {
		pending(introModel)
	}
	if s := r.Sessions; s != nil {
		if r.SessionsPartial {
			put(introSessions, fmt.Sprintf("%d+ open · %d running · %d quiet (sample)", s.Open, s.Running, s.Quiet), x.Plain)
		} else {
			put(introSessions, fmt.Sprintf("%d open · %d running · %d quiet", s.Open, s.Running, s.Quiet), x.Plain)
		}
	} else if i.listed != nil {
		count := strconv.Itoa(*i.listed)
		if i.moreListed {
			count += "+"
		}
		put(introSessions, count+" listed", x.Plain)
	} else if !i.reportReady {
		pending(introSessions)
	}
	extra := r.MoreAttention
	if (i.options.Watch != nil || i.richReport) && i.reportReady {
		n := 0
		for _, item := range r.Attention {
			label := introAttentionLabel(item, at)
			if label.text == "" {
				continue
			}
			if n < 3 {
				if i.reportProblem != "" || i.feedError {
					label.actions = runActions{}
				}
				next[introAttention+n] = label
				n++
			} else {
				extra++
			}
		}
	} else if t.Status == x.Failed || t.Status == x.Parked {
		f := i.owner.activity.failure
		next[introAttention] = introAttentionLabel(IntroAttention{Title: threadTitle(t), Status: t.Status, Reason: f.Reason}, at)
	}
	if next[introAttention].text != "" {
		review := "/threads to review"
		if extra > 0 {
			review += " · +" + strconv.Itoa(extra) + " more"
		}
		put(introReview, review, x.Accent)
	}
	if i.standaloneSkillCount != nil {
		put(introSkills, strconv.Itoa(*i.standaloneSkillCount)+" loaded", x.Plain)
	} else if r.Skills != nil || r.MoreSkills > 0 {
		put(introSkills, strconv.Itoa(len(r.Skills)+r.MoreSkills)+" loaded", x.Plain)
	} else if i.expectReport && !i.reportReady {
		pending(introSkills)
	}
	if c := r.Context; c != nil && c.Window > 0 && (!draftModel || deref(r.Model) == model) {
		usage := contextFromReport(c)
		put(introContext, introContextLabel(usage.Tokens, usage.Window, usage.Estimated), x.Plain)
	} else if c := t.ContextUse; c.Known && c.Window > 0 {
		put(introContext, introContextLabel(c.Tokens, c.Window, true), x.Plain)
	} else if i.expectReport && !i.reportReady {
		pending(introContext)
	}
	for k, v := range next {
		old := i.labels[k]
		if old == v {
			continue
		}
		i.revision++
		if v.text != "" && v.text != introSkeleton && (old.text == "" || old.text == introSkeleton) {
			i.motion.fade(k, at)
		}
	}
	i.labels = next
}

func introAttentionLabel(a IntroAttention, at time.Time) introLabel {
	label := introLabel{title: oneLine(limitText(a.Title, 384)), tone: x.Danger}
	if a.Status == x.Parked {
		label.tone, label.text = x.Warning, "parked"
		if !a.Until.IsZero() {
			label.text = "sleeping until " + a.Until.Format("15:04")
		} else if a.Reason != "" {
			label.text += " · " + oneLine(limitText(a.Reason, 128))
		}
	} else if a.Issue != nil && failedRunCause(a.Issue.Reason) != "" {
		if cause := runFailureDetail(a.Issue.Reason, a.Issue.Detail); cause != "" {
			label.text = cause
		} else {
			label.text = runFailureText(a.Issue.Reason, "")
		}
		if a.Issue.EndedAtMS != nil && *a.Issue.EndedAtMS > 0 {
			if age := at.Sub(time.UnixMilli(*a.Issue.EndedAtMS)); age >= time.Minute {
				label.age = " · " + introUptime(age) + " ago"
			}
		}
		label.actions = issueActions(a.ThreadID, a.Issue)
	} else {
		label.text = runFailureText(a.Reason, "")
	}
	return label
}

func introContextLabel(n, window uint64, estimated bool) string {
	cells := 12
	filled := min(cells, int(0.5+float64(cells)*float64(min(n, window))/float64(window)))
	if n > 0 {
		filled = max(1, filled)
	}
	short := func(n uint64) string {
		s := strings.TrimSuffix(tokenLabel(n), " tok")
		if len(s) > 3 && s[len(s)-3:len(s)-1] == ".0" {
			return s[:len(s)-3] + s[len(s)-1:]
		}
		return s
	}
	s := strings.Repeat("▰", filled) + strings.Repeat("▱", cells-filled) + " " + short(n) + " of " + short(window)
	if estimated {
		s += " (estimate)"
	}
	return s
}

func introUptime(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh %dm", d/time.Hour, d%time.Hour/time.Minute)
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}

type introStamp struct {
	last time.Time
	ok   bool
}

func readIntroStamp(path string) introStamp {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return introStamp{ok: true}
	}
	if err != nil || !info.Mode().IsRegular() {
		return introStamp{}
	}
	return introStamp{info.ModTime(), true}
}

func recordIntroStamp(path string, at time.Time) tea.Cmd {
	return func() tea.Msg {
		dir := filepath.Dir(path)
		if os.MkdirAll(dir, 0o700) != nil {
			return nil // state persistence is best effort, not an attach dependency
		}
		f, err := os.CreateTemp(dir, ".intro-*")
		if err != nil {
			return nil
		}
		name := f.Name()
		defer os.Remove(name)
		if f.Close() == nil && os.Chtimes(name, at, at) == nil {
			_ = os.Rename(name, path)
		}
		return nil
	}
}
