package tui

import (
	"context"
	"errors"
	"image/color"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func introFixture(t testing.TB, opts Options) (*Model, *time.Time) {
	t.Helper()
	if opts.Intro == nil {
		opts.Intro = &IntroOptions{}
	}
	opts.ThreadID = "1"
	opts.NewThread.CWD = "/work/hush"
	m := modelFor(t, opts)
	at := time.Date(2026, 11, 23, 13, 24, 0, 0, time.UTC)
	m.intro.motion.clock.now = func() time.Time { return at }
	rng := rand.New(rand.NewSource(7))
	m.intro.motion.wave = newIntroWave(rng.Float64)
	m.intro.motion.wave.height = m.intro.motion.wave.final
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 25})
	return m, &at
}

func introFixtureReport(at time.Time) IntroReport {
	return IntroReport{
		StartedAt: at.Add(-3*time.Hour - 12*time.Minute),
		Sessions:  &IntroSessions{6, 2, 2},
		Skills:    []string{"review", "research", "compact", "commit", "test", "explain", "lint", "browse", "diagram"},
		Context:   &IntroContext{RenderedBytes: 33600, Window: 400000},
		Attention: []IntroAttention{
			{Title: "thread #7", Status: x.Stopped, Reason: "model error"},
			{Title: "migrate-db", Status: x.Parked, Until: time.Date(2026, 11, 24, 2, 0, 0, 0, time.Local)},
		},
	}
}

func applyIntroFixtureTheme(m *Model) {
	m.Update(uv.UnknownOscEvent("\x1b]4;4;rgb:4141/7373/fafa\x1b\\"))
	m.Update(uv.UnknownOscEvent("\x1b]4;8;rgb:7373/7474/7878\x07"))
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	m.Update(tea.BackgroundColorMsg{Color: color.White})
	m.Update(tea.ForegroundColorMsg{Color: color.NRGBA{R: 27, G: 28, B: 35, A: 255}})
}

func TestIntroGoldens(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	watch := func(ctx context.Context) <-chan IntroUpdate { return nil }
	m, clock := introFixture(t, Options{Intro: &IntroOptions{Watch: watch}, Fills: FillOn})
	applyIntroFixtureTheme(m)
	a := &m.intro.motion
	zero := *clock
	a.start(zero, true, false)
	m.syncIntroClock()
	v := assertFrame(t, m)
	if strings.Count(v, introSkeleton) < 3 || m.View().Cursor == nil {
		t.Fatal("first frame lacks skeletons or input focus")
	}
	golden(t, "intro-000", m)

	for step := 1; step <= 54; step++ {
		*clock = zero.Add(time.Duration(step) * time.Second / 30)
		if step == 6 {
			s := m.intro.owner
			s.info.Model, s.info.Thinking, s.observed = sdk.Model{ModelID: "gpt-6.2-sol"}, "max", true
			m.introFeed(s, true, false)
			m.Update(introReportMsg{update: IntroUpdate{Report: introFixtureReport(*clock)}})
			if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, "6 open · 2 running · 2 quiet") {
				t.Fatal("data is gated behind the animation")
			}
		}
		if a.tick != 0 {
			m.Update(introTick{a.tick, *clock})
		}
		switch step {
		case 15:
			golden(t, "intro-500", m)
			if !strings.Contains(m.frame.rows[0], "\x1b[38;2;") {
				t.Fatal("truecolor banner did not use the terminal palette")
			}
		case 30:
			golden(t, "intro-1000", m)
		case 54:
			golden(t, "intro-1800", m)
			if !strings.Contains(m.frame.rows[0], "\x1b[38;2;65;115;250m") {
				t.Fatal("intro did not use the terminal's reported accent")
			}
		default:
			m.View()
		}
	}
	if a.playing || a.tick != 0 || a.stop != nil || m.syncIntroClock() != nil || a.wave.height != a.wave.final {
		t.Fatal("completed animation did not settle or disarm")
	}
	other := newIntroMotion()
	if a.wave.height != other.wave.height {
		t.Fatal("animated and static startup have different final waves")
	}
	fast := m.View()
	m.introOnly = false
	if full := m.View(); full.Content != fast.Content || *full.Cursor != *fast.Cursor {
		t.Fatal("intro-only redraw differs from a fresh full frame")
	}
}

func TestIntroFirstInputCancelsLiveTimer(t *testing.T) {
	for _, c := range []struct {
		name string
		msg  tea.Msg
		want string
	}{{"text", tea.KeyPressMsg{Code: '界', Text: "界"}, "界"},
		{"paste", tea.PasteMsg{Content: "two words"}, "two words"},
		{"slash", tea.KeyPressMsg{Code: '/', Text: "/"}, "/"},
		{"leader", tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl}, ""}} {
		t.Run(c.name, func(t *testing.T) {
			m, clock := introFixture(t, Options{})
			a := &m.intro.motion
			a.start(*clock, true, false)
			armed := make(chan struct{})
			a.clock.wait = func(ctx context.Context, d time.Duration) bool {
				close(armed)
				<-ctx.Done()
				return false
			}
			cmd := m.syncIntroClock()
			oldTick := a.tick
			done := make(chan tea.Msg, 1)
			go func() { done <- cmd() }()
			<-armed
			m.Update(c.msg)
			if m.editor.value() != c.want || c.want != "" && !strings.Contains(ansi.Strip(assertFrame(t, m)), "main › "+c.want) {
				t.Fatalf("keystroke failed to reach prompt: %q", m.editor.value())
			}
			if c.name == "leader" && m.leader == "" {
				t.Fatal("first non-printable key was swallowed")
			}
			select {
			case v := <-done:
				if v != nil {
					t.Fatal("cancelled timer delivered a frame")
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled timer is still waiting")
			}
			m.Update(introTick{oldTick, *clock})
			if a.playing || a.pending || a.tick != 0 || a.wave.height != a.wave.final || m.syncIntroClock() != nil {
				t.Fatal("stale tick restarted the animation")
			}
		})
	}
}

type introTestBackend struct {
	Backend
	watch func(context.Context) <-chan sdk.StateUpdate
	list  func(context.Context) (sdk.ThreadPage, error)
}

func (b introTestBackend) Watch(ctx context.Context, _ sdk.ID, _ sdk.Window) <-chan sdk.StateUpdate {
	return b.watch(ctx)
}
func (b introTestBackend) ListThreads(ctx context.Context, _ int, _ string) (sdk.ThreadPage, error) {
	return b.list(ctx)
}
func (b introTestBackend) NewThread(context.Context, sdk.NewThread) (sdk.Thread, error) {
	return sdk.Thread{ID: "2"}, nil
}
func (b introTestBackend) Submit(context.Context, sdk.Input) (sdk.Receipt, error) {
	return sdk.Receipt{SubmissionID: "3"}, nil
}

func TestIntroStartsAttachBeforePaintWithoutAwaiting(t *testing.T) {
	m, _ := introFixture(t, Options{})
	watch, list := make(chan struct{}), make(chan struct{})
	m.backend = introTestBackend{watch: func(ctx context.Context) <-chan sdk.StateUpdate {
		close(watch)
		<-ctx.Done()
		return nil
	}, list: func(ctx context.Context) (sdk.ThreadPage, error) {
		close(list)
		<-ctx.Done()
		return sdk.ThreadPage{}, ctx.Err()
	}}
	m.Init() // do not execute any returned commands before the first view
	for _, ch := range []chan struct{}{watch, list} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("attach was not kicked off independently of the first view")
		}
	}
	if v := m.View(); v.Cursor == nil || !strings.Contains(v.Content, introSkeleton) {
		t.Fatal("first synchronous frame needs a focused input and pending report")
	}
	m.Update(tea.KeyPressMsg{Code: 'h', Text: "h"})
	if !strings.Contains(ansi.Strip(assertFrame(t, m)), "main › h") {
		t.Fatal("pending backend blocked input")
	}
}

func TestIntroMotionPolicy(t *testing.T) {
	for _, c := range []struct {
		name, term, noAnim string
		tty, reduced       bool
		height             int
	}{{"env", "xterm-256color", "1", true, false, 30},
		{"dumb", "dumb", "", true, false, 30},
		{"not-a-tty", "xterm-256color", "", false, false, 30},
		{"reduced-motion", "xterm-256color", "", true, true, 30},
		{"short", "xterm-256color", "", true, false, 23}} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("TERM", c.term)
			t.Setenv("HUSH_NO_ANIM", c.noAnim)
			m, _ := introFixture(t, Options{ReducedMotion: c.reduced, Intro: &IntroOptions{Watch: func(context.Context) <-chan IntroUpdate { return nil }}})
			m.intro.tty = c.tty
			m.Update(tea.WindowSizeMsg{Width: 120, Height: c.height})
			m.beginIntro()
			if a := &m.intro.motion; a.tick != 0 || a.playing || a.wave.height != a.wave.final {
				t.Fatal("disabled motion did not render the final state directly")
			}
			if !strings.Contains(ansi.Strip(assertFrame(t, m)), "██      ██") {
				t.Fatal("disabled motion hid the wordmark")
			}
		})
	}
}

func TestIntroWarmState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client", "intro")
	m, clock := introFixture(t, Options{Intro: &IntroOptions{StatePath: path}})
	a := &m.intro.motion
	start := *clock
	recordIntroStamp(path, start.Add(-29*time.Minute))()
	a.start(start, true, true)
	cmd := m.receiveIntroStamp(readIntroStamp(path))
	if a.playing || a.pending || a.wave.height != a.wave.final || m.syncIntroClock() != nil {
		t.Fatal("warm start tried to animate")
	}
	if cmd == nil {
		t.Fatal("warm intro did not renew its timestamp")
	}
	cmd()
	if stamp := readIntroStamp(path); !stamp.ok || !stamp.last.Equal(start) {
		t.Fatalf("read stamp %+v, want %v", stamp, start)
	}
	*clock = start.Add(30 * time.Minute)
	a.start(*clock, true, true)
	m.receiveIntroStamp(readIntroStamp(path))
	if !a.playing || a.pending || m.syncIntroClock() == nil {
		t.Fatal("cold start did not animate")
	}
}

func TestIntroNoColorAndResize(t *testing.T) {
	t.Setenv("NO_COLOR", "on")
	m, clock := introFixture(t, Options{Fills: FillOn})
	applyIntroFixtureTheme(m)
	a := &m.intro.motion
	a.start(*clock, true, false)
	a.snap()
	m.Update(introReportMsg{update: IntroUpdate{Report: introFixtureReport(*clock)}})
	for _, w := range []int{40, 49, 50, 69, 70, 120} {
		m.Update(tea.WindowSizeMsg{Width: w, Height: 25})
		v := assertFrame(t, m)
		for _, row := range m.frame.rows[:m.frame.intro.count] {
			if strings.Contains(row, "\x1b[3") || strings.Contains(row, "\x1b[9") {
				t.Fatalf("NO_COLOR was ignored at width %d: %q", w, row)
			}
		}
		first := ansi.Strip(m.frame.rows[0])
		if w < 50 && !strings.HasPrefix(first, "  hush") || w >= 50 && !strings.HasPrefix(first, "  ██") {
			t.Fatalf("bad wordmark at %d: %q", w, first)
		}
		baseline := ansi.Strip(m.frame.rows[4])
		wave := strings.TrimSpace(ansi.Cut(baseline, len(margin)+wordColumns, w))
		if w >= 50 && ((wave != "") != (w >= 70)) {
			t.Fatalf("wrong wave visibility at %d:\n%s", w, ansi.Strip(v))
		}
	}
	a.start(*clock, true, false)
	m.syncIntroClock()
	timer := a.tick
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 25})
	if a.tick != timer || !a.playing {
		t.Fatal("resize restarted the animation")
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 23})
	if a.tick != 0 || a.playing {
		t.Fatal("short resize left animation running")
	}
}

func TestIntroOfflineRecoversAndFreezesOnSubmit(t *testing.T) {
	watch := func(context.Context) <-chan IntroUpdate { return nil }
	m, clock := introFixture(t, Options{Intro: &IntroOptions{Watch: watch}})
	m.backend = introTestBackend{}
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.ANSI256})
	m.Update(introReportMsg{update: IntroUpdate{Err: errors.New("offline")}})
	if body := assertFrame(t, m); !strings.Contains(body, paint(x.Warning, "○ runtime offline · retrying")) || !strings.Contains(ansi.Strip(body), "runtime     offline") {
		t.Fatalf("missing runtime warning:\n%s", body)
	}
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if m.editor.value() != "x" || m.View().Cursor == nil {
		t.Fatal("offline runtime disabled editor")
	}
	report := introFixtureReport(*clock)
	report.Attention = append(report.Attention, IntroAttention{Title: "review", Status: x.Parked},
		IntroAttention{Title: "lint", Status: x.Stopped, Reason: "turn limit"}, IntroAttention{Title: "bench", Status: x.Stopped, Reason: "faulted"})
	m.Update(introReportMsg{update: IntroUpdate{Report: report}})
	body := ansi.Strip(assertFrame(t, m))
	for _, want := range []string{"attached · up 3h 12m", "run status  ✗ thread #7   no answer · model run failed", "migrate-db   sleeping until 02:00",
		"review   parked", "/threads to review · +2 more", "skills      9 loaded", "8.4k of 400k (estimate)"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "lint") || strings.Contains(body, "retrying") {
		t.Fatal("overflow rows or the old runtime status were retained")
	}
	var cancelled bool
	m.intro.cancel = func() { cancelled = true }
	cmd := m.sendPrompt(sdk.Steer)
	if cmd == nil || !m.intro.frozen || !cancelled || m.intro.motion.tick != 0 {
		t.Fatal("first submission failed to freeze the report and detach its source")
	}
	m.finishWork(cmd().(workMsg))
	m.Update(introReportMsg{update: IntroUpdate{Report: IntroReport{Skills: []string{"later"}}}})
	thread := m.threads[m.active]
	thread.merge([]sdk.Entry{entry(100, "user", "x"), entry(101, "assistant", "new reply")})
	press(m, tea.KeyHome, tea.ModCtrl)
	body = ansi.Strip(assertFrame(t, m))
	if m.frame.top.id != introEntry || !strings.Contains(body, "6 open") || !strings.Contains(body, "new reply") || strings.Contains(body, "later") {
		t.Fatal("frozen intro is not an immutable scrollback block")
	}
	if len(thread.ids) != 2 || thread.entries[introEntry] != nil {
		t.Fatal("intro contaminated the native history index")
	}
}

func TestIntroReviewOpensSwitcher(t *testing.T) {
	m, clock := introFixture(t, Options{Intro: &IntroOptions{Watch: func(context.Context) <-chan IntroUpdate { return nil }}})
	m.backend = introTestBackend{list: func(context.Context) (sdk.ThreadPage, error) {
		return sdk.ThreadPage{Threads: []sdk.Thread{{ID: "7"}}}, nil
	}}
	m.connection = x.Connected
	a := &m.intro.motion
	a.start(*clock, true, false)
	m.syncIntroClock()
	report := introFixtureReport(*clock)
	m.Update(introReportMsg{update: IntroUpdate{Report: report}})
	const review = "/threads to review"
	var click tea.Mouse
	for _, width := range []int{40, 120} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 25})
		assertFrame(t, m)
		*clock = clock.Add(introPeriod)
		m.Update(introTick{a.tick, *clock})
		if !m.introOnly {
			t.Fatal("expected the animation's partial redraw")
		}
		body := ansi.Strip(assertFrame(t, m))
		click.X = -1
		for y, line := range strings.Split(body, "\n") {
			if idx := strings.Index(line, review); idx >= 0 {
				click = tea.Mouse{X: stringWidth(line[:idx]), Y: y, Button: tea.MouseLeft}
				break
			}
		}
		x, y := click.X, click.Y
		if x < 0 || m.hoverAt(x, y) != "open thread list" || m.hoverAt(x-1, y) != "" || m.hoverAt(x+stringWidth(review), y) != "" {
			t.Fatalf("review hit region does not match the visible action at width %d:\n%s", width, body)
		}
	}
	m.Update(tea.PasteMsg{Content: "keep this draft"})
	m.View()
	m.Update(tea.MouseClickMsg(click))
	m.View()
	_, cmd := m.Update(tea.MouseReleaseMsg(click))
	if cmd == nil {
		t.Fatal("review click did not request the thread list")
	}
	m.Update(cmd())
	if m.sheet == nil || m.active != "1" || m.editor.value() != "keep this draft" {
		t.Fatal("review click should only open the switcher and leave the draft intact")
	}
	press(m, tea.KeyDown, 0)
	press(m, tea.KeyEnter, 0)
	if m.active != "7" || m.threads["1"].draft != "keep this draft" {
		t.Fatal("selecting the failed thread did not switch to it or preserve the original draft")
	}
}

func TestIntroNativeModelFailureNamesAttachedThread(t *testing.T) {
	m, _ := introFixture(t, Options{ReducedMotion: true})
	s := m.threads[m.active]
	s.info.Status = x.Failed
	s.activity.failure.Reason = "no_model"
	m.introFeed(s, true, false)
	body := ansi.Strip(assertFrame(t, m))
	if !strings.Contains(body, "thread 1   no answer · no model available") || !strings.Contains(body, "/threads to review") {
		t.Fatalf("native fallback did not identify the thread or show the next step:\n%s", body)
	}
}

func TestIntroNativeRetryStopsAfterList(t *testing.T) {
	m, _ := introFixture(t, Options{ReducedMotion: true})
	attempts := 0
	m.backend = introTestBackend{list: func(context.Context) (sdk.ThreadPage, error) {
		attempts++
		if attempts == 1 {
			return sdk.ThreadPage{}, errors.New("offline")
		}
		return sdk.ThreadPage{Threads: []sdk.Thread{{ID: "1"}}, NextCursor: ptr("page")}, nil
	}}
	first := m.beginIntro()()
	_, retry := m.Update(first)
	if retry == nil || !strings.Contains(ansi.Strip(assertFrame(t, m)), "runtime offline · retrying") {
		t.Fatal("native listing did not expose attachment failure and retry")
	}
	msg, ok := retry().(introReportMsg)
	if !ok || msg.next != nil || msg.update.Err != nil || attempts != 2 {
		t.Fatalf("native retry did not stop after success: %+v, calls = %d", msg, attempts)
	}
	_, idle := m.Update(msg)
	body := ansi.Strip(assertFrame(t, m))
	if idle != nil || !strings.Contains(body, "1+ listed") || strings.Contains(body, "quiet") || strings.Contains(body, "retrying") {
		t.Fatalf("incorrect native summary or idle clock:\n%s", body)
	}
}

func TestIntroLateReportFadesWithoutDelay(t *testing.T) {
	m, clock := introFixture(t, Options{Intro: &IntroOptions{Watch: func(context.Context) <-chan IntroUpdate { return nil }}})
	a := &m.intro.motion
	a.start(*clock, true, false)
	*clock = clock.Add(introDuration)
	a.advance(*clock)
	m.View()
	m.Update(introReportMsg{update: IntroUpdate{Report: introFixtureReport(*clock)}})
	if a.playing || a.tick == 0 || a.fadeProgress(introSkills) != 0 || !strings.Contains(assertFrame(t, m), "9 loaded") {
		t.Fatal("late data did not appear immediately and fade without replaying the banner")
	}
	*clock = clock.Add(introFade)
	m.Update(introTick{a.tick, *clock})
	if a.tick != 0 || a.fading() || m.syncIntroClock() != nil {
		t.Fatal("late report fade kept its clock armed")
	}
	report := introFixtureReport(*clock)
	report.Context.ReportedTokens = ptr(uint64(10_000))
	m.Update(introReportMsg{update: IntroUpdate{Report: report}})
	body := ansi.Strip(assertFrame(t, m))
	if !strings.Contains(body, "10k of 400k") || strings.Contains(body, "(estimate)") {
		t.Fatal("reported context did not replace the estimate")
	}
}

func TestIntroTransfersToFirstCreatedThread(t *testing.T) {
	m := modelFor(t, Options{Intro: &IntroOptions{}, ReducedMotion: true})
	m.backend = introTestBackend{}
	m.Update(tea.WindowSizeMsg{Width: 90, Height: 40})
	unbound := m.threads[""]
	assertAtTop := func(phase string) string {
		t.Helper()
		body := ansi.Strip(assertFrame(t, m))
		f := m.frame.intro
		if f.count == 0 || f.at != 0 || f.first != 0 {
			t.Fatalf("%s: intro should start at row 0, got row %d (%d visible rows, starting at %d)", phase, f.at, f.count, f.first)
		}
		return body
	}
	assertAtTop("startup")
	m.Update(tea.PasteMsg{Content: "first message"})
	assertAtTop("typing")
	cmd := press(m, tea.KeyEnter, 0)
	assertAtTop("submitting")
	m.Update(cmd().(workMsg))
	thread := m.threads["2"]
	if m.active != "2" || unbound.intro != nil || thread.intro != m.intro || !m.intro.frozen {
		t.Fatal("first created thread did not inherit the frozen report")
	}

	user := entry(1, "user", "first message")
	reply := entry(2, "assistant", "first reply")
	user.ThreadID, reply.ThreadID = m.active, m.active
	thread.merge([]sdk.Entry{user, reply})
	assertAtTop("first turn")
	if row := ansi.Strip(m.frame.rows[m.frame.intro.count]); !strings.Contains(row, "first message") {
		t.Fatalf("first message should directly follow the intro, got %q", row)
	}

	long := entry(3, "assistant", strings.Repeat("more conversation\n\n", m.height)+"latest line")
	long.ThreadID = m.active
	thread.merge([]sdk.Entry{long})
	if body := assertFrame(t, m); m.frame.intro.count != 0 || !strings.Contains(body, "latest line") {
		t.Fatal("the intro should scroll away when tail-following fills the viewport")
	}
	press(m, tea.KeyHome, tea.ModCtrl)
	if body := assertAtTop("scroll back"); strings.Contains(body, introSkeleton) {
		t.Fatal("frozen report kept pending placeholders")
	}
}

func TestIntroUsesMonochromeBelow256Colors(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	m, _ := introFixture(t, Options{Fills: FillOn})
	applyIntroFixtureTheme(m)
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.ANSI})
	m.View()
	for _, row := range m.frame.rows[:m.frame.intro.count] {
		if strings.Contains(row, "\x1b[3") {
			t.Fatal("low-color terminal did not get a monochrome intro")
		}
	}
}

func TestIntroANSI256DoesNotEmitRGB(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	m, _ := introFixture(t, Options{Fills: FillOn, ReducedMotion: true})
	applyIntroFixtureTheme(m)
	if !strings.Contains(assertFrame(t, m), "\x1b[38;2;") {
		t.Fatal("fixture did not start with truecolor")
	}
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.ANSI256})
	body := assertFrame(t, m)
	if strings.Contains(body, "\x1b[38;2;") || !strings.Contains(m.frame.rows[0], "\x1b[34m") {
		t.Fatal("ANSI256 intro did not fall back to terminal tones")
	}
}

func TestIntroLateForegroundRepaintsBanner(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	m, _ := introFixture(t, Options{Fills: FillOn, ReducedMotion: true})
	applyIntroFixtureTheme(m)
	before := assertFrame(t, m)
	m.Update(tea.ForegroundColorMsg{Color: color.NRGBA{R: 211, G: 132, B: 243, A: 255}})
	after := assertFrame(t, m)
	if before == after || !strings.Contains(m.frame.rows[0], "\x1b[38;2;211;132;243m") {
		t.Fatal("late foreground did not repaint the settled banner")
	}
}

func TestIntroUptimeNeedsKnownStart(t *testing.T) {
	m, now := introFixture(t, Options{ReducedMotion: true})
	report := projectIntro(sdk.IntroReport{})
	m.Update(introReportMsg{update: IntroUpdate{Report: report}})
	if !report.StartedAt.IsZero() || m.intro.labels[introStatus].text != "● attached" {
		t.Fatalf("unknown start time displayed as uptime: %v %q", report.StartedAt, m.intro.labels[introStatus].text)
	}
	report = projectIntro(sdk.IntroReport{StartedAtMS: now.Add(-35 * time.Second).UnixMilli()})
	m.Update(introReportMsg{update: IntroUpdate{Report: report}})
	if !strings.Contains(ansi.Strip(assertFrame(t, m)), "attached · up 35s") {
		t.Fatal("known start time did not display uptime")
	}
}

func TestIntroFastPathAndSlowFrameCutoff(t *testing.T) {
	var paints int
	m, clock := introFixture(t, Options{Extensions: []x.Registration{{ID: "fixture/widget", APIVersion: x.APIVersion,
		Widgets: []x.Widget{{Key: "value", Placement: x.AboveEditor, Render: func(x.RenderContext) x.Content {
			paints++
			return x.Content{{Text: "counted widget"}}
		}}},
	}}})
	a := &m.intro.motion
	a.start(*clock, true, false)
	m.syncIntroClock()
	m.View()
	before := paints
	for n := 0; n < 3; n++ {
		*clock = clock.Add(introPeriod)
		m.Update(introTick{a.tick, *clock})
		*clock = clock.Add(17 * time.Millisecond) // simulate a slow composed frame
		m.View()
	}
	if before == 0 || paints != before {
		t.Fatalf("animation laid out non-intro chrome: %d -> %d render calls", before, paints)
	}
	if a.playing || a.tick != 0 || a.wave.height != a.wave.final {
		t.Fatal("two consecutive slow frames did not stop the animation")
	}
}
