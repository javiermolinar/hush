package tui

import (
	"context"
	"errors"
	"image/color"
	"os"
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

// These are production-model frames with deterministic terminal colors and
// inputs, not proof of a particular terminal emulator or an installed PTY run.
func TestReleaseFrames(t *testing.T) {
	for _, theme := range []struct {
		name   string
		bg, fg color.NRGBA
	}{
		{"latte", color.NRGBA{R: 239, G: 241, B: 245, A: 255}, color.NRGBA{R: 76, G: 79, B: 105, A: 255}},
		{"mocha", color.NRGBA{R: 30, G: 30, B: 46, A: 255}, color.NRGBA{R: 205, G: 214, B: 244, A: 255}},
	} {
		t.Run(theme.name, func(t *testing.T) {
			var clipboard string
			m, s := demo(t, Options{Fills: FillOn, ReducedMotion: true,
				CopyText:      func(_ context.Context, text string) error { clipboard = text; return nil },
				Services:      MenuServices{Rename: func(context.Context, x.SetTitle) error { return nil }},
				ProviderLogin: loginFixture(),
			})
			m.Update(tea.WindowSizeMsg{Width: 100, Height: 22})
			m.Update(tea.BackgroundColorMsg{Color: theme.bg})
			m.Update(tea.ForegroundColorMsg{Color: theme.fg})
			m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
			shot := func(name string) string {
				t.Helper()
				frame := assertFrame(t, m)
				if dir := os.Getenv("HUSH_RELEASE_FRAMES"); dir != "" {
					path := filepath.Join(dir, theme.name+"-"+name+".ansi")
					if err := os.WriteFile(path, []byte(frame+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return ansi.Strip(frame)
			}
			m.editor.set("/login")
			press(m, tea.KeyEnter, 0)
			if frame := shot("provider-login"); !strings.Contains(frame, "Fixture provider") || !strings.Contains(frame, "Device provider") {
				t.Fatal("the provider picker did not show both sign-in choices")
			}
			press(m, tea.KeyEscape, 0)

			m.editor.set("Keep this draft")
			s.queue = []sdk.QueueItem{{SubmissionID: "7", Mode: string(sdk.FollowUp), Text: "Queued fixture"}}
			s.info.Status = x.Running
			if frame := shot("alt-up-before"); !strings.Contains(frame, "follow-up queued · Queued fixture") {
				t.Fatal("Alt+Up fixture has no queued preview")
			}
			press(m, tea.KeyUp, tea.ModAlt)
			shot("alt-up-after")
			// Capture the actual action, not a mocked success. Until the host
			// supports dequeue, the warning is an open release-review item.
			t.Logf("Alt+Up: notice=%q editor=%q", m.notice.text, m.editor.value())
			s.queue = nil
			s.info.Status = x.Idle
			m.notice = noticeState{}

			assertFrame(t, m)
			for y, row := range m.frame.rows[:m.frame.transcriptHeight] {
				plain := ansi.Strip(row)
				if offset := strings.Index(plain, "What changed"); offset >= 0 {
					col := ansi.StringWidth(plain[:offset])
					m.Update(tea.MouseClickMsg(tea.Mouse{X: col, Y: y, Button: tea.MouseLeft}))
					m.Update(tea.MouseMotionMsg(tea.Mouse{X: col + 3, Y: y, Button: tea.MouseLeft}))
					shot("selection")
					_, cmd := m.Update(tea.MouseReleaseMsg(tea.Mouse{X: col + 3, Y: y, Button: tea.MouseLeft}))
					workflowWork(t, m, cmd)
					break
				}
			}
			if frame := shot("copied"); clipboard != "What" || !strings.Contains(frame, "Copied") {
				t.Fatalf("clipboard=%q, frame=%s", clipboard, frame)
			}
			m.notice = noticeState{}

			m.runAction(m.host.byID[paletteID], "")
			d := m.focusedDialog()
			d.query = "rename"
			d.filter()
			m.acceptChoice(d)
			if len(m.overlays) != 2 {
				t.Fatal("no nested rename")
			}
			shot("nested-escape-before")
			press(m, tea.KeyEscape, 0)
			if len(m.overlays) != 1 || !m.overlays[0].palette || !m.stopAt.IsZero() {
				t.Fatal("Esc did not return to the palette only")
			}
			shot("nested-escape-after")
			press(m, tea.KeyEscape, 0)
			s.observed = true
			m.Update(feedMsg{owner: "1", ok: true, update: sdk.StateUpdate{Err: errors.New("backend connection lost")}})
			if frame := shot("reconnecting"); !strings.Contains(frame, "reconnecting") || !strings.Contains(frame, "Keep this draft") {
				t.Fatal("reconnect row erased the prompt")
			}

			blue := "1e/66/f5"
			if theme.name == "mocha" {
				blue = "89/b4/fa"
			}
			m.Update(uv.UnknownOscEvent("\x1b]4;4;rgb:" + blue + "\x07"))
			m.setConnection(x.Connected)
			m.notice = noticeState{}
			m.Update(tea.WindowSizeMsg{Width: 108, Height: 23})
			s.following = true
			s.info.Title, s.info.Branch, s.info.Model.ModelID = "tab-click bug", "main", "fixture-model"
			s.info.Workspace, s.info.Thinking, s.info.Status = "/work/hush", "max", x.Running
			s.info.Usage = sdk.Usage{TotalTokens: 16_000_000, CostUSD: 5.48}
			s.info.ContextUse = x.ContextUse{Known: true, Tokens: 26_000, Window: 100_000}
			m.ensureThread("2").info.Title = "skills loading"
			m.threads["2"].info.Status = x.Done
			m.tabs = []sdk.ID{"2", "1"}
			m.now = time.Unix(200, 0)
			s.merge([]sdk.Entry{entry(3, "assistant", "Use `hush stop` for an attach-only shutdown."),
				toolCallEntry(4, "tests", "bash", `{"command":"go test ./..."}`, m.now.Add(-time.Minute)),
				toolResultEntry(5, "tests", "bash", "Command exited with code 1", true, m.now),
				toolResultEntry(6, "read", "read", "fixture content", false, m.now),
				entry(7, "assistant", "The targeted reload test passes. See backend/src/index.ts:260.")})
			s.toggleBurst("5/0")
			s.tools = []x.Tool{{CallID: "full", Name: "bash", Live: true}}
			s.activity.intent = "Running full tests"
			s.activity.tools = map[string]time.Time{"full": m.now.Add(-61 * time.Second)}
			body := shot("thread-chrome")
			if !strings.Contains(body, "│ What changed") || !strings.Contains(body, "Running full tests · 1m 01s") ||
				!strings.Contains(body, "$5.48") || !strings.Contains(body, "exit 1") {
				t.Fatalf("incomplete chrome capture:\n%s", body)
			}
			if !strings.HasPrefix(m.frame.rows[m.height-1], m.palette.softInk) || m.palette.activeTab == "" ||
				!strings.Contains(m.frame.rows[m.height-1], m.palette.activeTab) {
				t.Fatal("tab band or active tab did not retain its separate surface")
			}

			thought := entry(8, "assistant", "")
			thought.Messages[0].Blocks = []sdk.Block{{Type: "thinking", Text: "Check the settings flow before changing the defaults."}}
			s.merge([]sdk.Entry{thought})
			m.applySettings(sdk.SettingsSnapshot{Model: configModel().Model, ThinkingLevel: "high", Reasoning: sdk.ReasoningExpanded})
			if body := shot("reasoning-expanded"); !strings.Contains(body, "Check the settings flow") {
				t.Fatal("expanded reasoning was not captured")
			}
			fixture := &configFixture{settings: configModel()}
			m.backend = fixture
			m.Update(m.menuProbe()())
			m.Update(m.loadSettings()())
			m.finishWork(m.runAction(m.host.byID[x.ActionID(menusID+".config")], "")().(workMsg))
			configDialog := m.focusedDialog()
			currentModel, _, _ := m.configCurrent(configDialog)
			configDialog.config.levels[currentModel] = []string{"off", "low", "high", "max"}
			if body := shot("config"); !strings.Contains(body, "settings") || !strings.Contains(body, "reasoning") || !strings.Contains(body, "folded") {
				t.Fatal("configuration sheet was not captured")
			}
			press(m, tea.KeyEscape, 0)

			enableGitDiffFixture(m, s)
			s.info.Branch = s.gitDiff.summary.Branch
			m.runAction(m.host.byID[usageID], "")
			if body := shot("usage-details"); !strings.Contains(body, "Current context: 26000 of 100000 tokens") {
				t.Fatal("usage details did not show current context")
			}
			press(m, tea.KeyEscape, 0)
			m.openGitDiff(m.route(m.active, coreID))
			if body := shot("git-diff-scope"); !strings.Contains(body, "/work/project/src  ⎇ feature/review") {
				t.Fatal("Git review did not show its workspace and branch")
			}
			press(m, tea.KeyEscape, 0)
			m.backend = deniedGitDiffFixture{}
			s.gitDiff = gitDiffSnapshot(sdk.GitDiffSummary{}, gitTrustDenied)
			workspaceTrustStep(t, m, m.openGitDiff(m.route(m.active, coreID)))
			if body := shot("git-diff-denied"); !strings.Contains(body, "Workspace trust required for Git review") {
				t.Fatal("Git denial did not explain why review is unavailable")
			}
			press(m, tea.KeyEscape, 0)
			m.options.WorkspaceTrust = workspaceTrustFixture("/work/project/src")
			openWorkspaceTrustFixture(t, m)
			if body := shot("workspace-trust-confirm"); !strings.Contains(body, "Trust this workspace?") || !strings.Contains(body, "Tools, file browsing, and Git (whole checkout)") {
				t.Fatal("no local workspace trust confirmation")
			}
			workspaceTrustStep(t, m, press(m, tea.KeyEscape, 0))
			m.options.WorkspaceTrust = &WorkspaceTrustOptions{}
			m.openWorkspaceTrust(m.route(m.active, coreID))
			if body := shot("workspace-trust-attached"); !strings.Contains(body, "hush trust grant --cwd PATH") {
				t.Fatal("no backend-host instructions")
			}
			press(m, tea.KeyEscape, 0)
			m.options.WorkspaceTrust = nil
			openBrowseScope(t, m, fileScopeFixture{capable: true, scope: sdk.WorkspaceFileScope{Workspace: true},
				search: func(context.Context, sdk.WorkspaceFileTarget, string, int) (sdk.WorkspaceFileSearchResult, error) {
					return sdk.WorkspaceFileSearchResult{Partial: true, Matches: []sdk.WorkspaceFileMatch{
						{Path: "other/pkg/generator.go"}, {Path: "tempo.fork/modules/generator/generator.go"},
					}}, nil
				}})
			m.editor.set("@generator.go")
			cmd := m.refreshFilePicker()
			if body := shot("file-search-pending"); !strings.Contains(body, "Searching workspace") {
				t.Fatal("file search did not show its pending state")
			}
			m.Update(cmd())
			press(m, tea.KeyDown, 0)
			if body := shot("file-search-results"); !strings.Contains(body, "› tempo.fork/modules/generator/generator.go") || !strings.Contains(body, "partial results") {
				t.Fatal("file search did not show the selected path and scan state")
			}

			m.editor.set("")
			openGroupedThreadPicker(t, m)
			if body := shot("thread-switcher"); !strings.Contains(body, "Deployment tools") ||
				!strings.Contains(body, "needs attention") || !strings.Contains(body, "stopped") ||
				!strings.Contains(body, "+ New thread here") || !strings.Contains(body, "ctrl+x n new here") {
				t.Fatal("the thread picker did not show groups, statuses, and workspace creation")
			}
			press(m, tea.KeyDown, 0)
			press(m, tea.KeyDown, 0)
			threadPickerStep(t, m, press(m, tea.KeyEnter, 0))
			if body := shot("thread-directory-chooser"); !strings.Contains(body, "New thread · Deployment tools") ||
				!strings.Contains(body, deployCWD) || !strings.Contains(body, deployCheckout) {
				t.Fatal("the directory chooser did not show both checkout paths")
			}
		})
	}
}
