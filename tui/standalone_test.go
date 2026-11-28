package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func startStandaloneFixture(t *testing.T, root, cwd string, args ...string) (context.Context, *sdk.Client) {
	t.Helper()
	bundle := os.Getenv("HUSH_TEST_BACKEND")
	if bundle == "" {
		t.Skip("set HUSH_TEST_BACKEND to the built backend bundle")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	grantStandaloneFixture(t, root, cwd)
	cmd := exec.CommandContext(ctx, "node", append([]string{bundle}, args...)...)
	cmd.Dir, cmd.Env = cwd, standaloneFixtureEnv(root)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Signal(os.Interrupt); _ = cmd.Wait(); cancel() })
	scan := bufio.NewScanner(stdout)
	if !scan.Scan() || !strings.Contains(scan.Text(), `"ready":true`) {
		t.Fatalf("standalone backend not ready: %q %v", scan.Text(), scan.Err())
	}
	client := sdk.NewClient(filepath.Join(root, ".hush", "runtime", "hush.sock"), sdk.ClientOptions{
		Capabilities:         standaloneFixtureCapabilities(),
		RequiredCapabilities: map[string]uint64{"hushProfile": 1},
	})
	t.Cleanup(client.Close)
	hello, err := client.Hello(ctx)
	if err != nil || hello.Capabilities["piEvents"] != 0 {
		t.Fatalf("expected native host without Pi events: %+v %v", hello, err)
	}
	return ctx, client
}

func standaloneFixtureEnv(root string) []string {
	// Do not inherit provider credentials into a loopback fixture.
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "PI_CODING_AGENT_DIR=" + filepath.Join(root, "ignored-pi"), "NODE_NO_WARNINGS=1"}
}

func grantStandaloneFixture(t *testing.T, root, cwd string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", os.Getenv("HUSH_TEST_BACKEND"), "trust", "grant", "--scope=work", "--cwd="+cwd)
	cmd.Dir, cmd.Env = cwd, standaloneFixtureEnv(root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture workspace grant: %v: %s", err, out)
	}
}

func standaloneFixtureCapabilities() map[string][]uint64 {
	return map[string][]uint64{
		"intro": {sdk.IntroActionsVersion, sdk.IntroVersion}, "modelCatalog": {sdk.ModelCatalogDraftVersion, sdk.ModelCatalogSearchVersion, sdk.ModelCatalogVersion},
		"threadDirectory": {sdk.ThreadManagementVersion, sdk.ThreadDirectoryVersion}, "threadWorkspace": {sdk.ThreadWorkspaceSearchVersion, sdk.ThreadWorkspaceVersion},
		"nativeStop": {sdk.NativeStopVersion}, "clientEnvironment": {sdk.ClientEnvironmentVersion},
	}
}

func standaloneHome(t *testing.T) (root, cwd string) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "hush-tui-native-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	cwd = filepath.Join(root, "project")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	return root, cwd
}

func TestStandaloneTUISubmit(t *testing.T) {
	root, cwd := standaloneHome(t)
	ctx, client := startStandaloneFixture(t, root, cwd, "--faux")
	thread, err := client.NewThread(ctx, sdk.NewThread{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(ctx, client, Options{ThreadID: thread.ID, ReducedMotion: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	first := m.beginFeed()().(feedMsg)
	if !first.ok || first.update.Err != nil {
		t.Fatalf("first feed: %+v", first)
	}
	m.receiveFeed(first)
	m.editor.set("native TUI")
	result := m.sendPrompt(sdk.Steer)().(workMsg)
	if result.err != nil {
		t.Fatal(result.err)
	}
	m.finishWork(result)
	for state := m.threads[m.active]; state.lastAnswer != "Faux: native TUI" || threadBusy(state); {
		msg := awaitFeed(m.feed, first.source)().(feedMsg)
		if !msg.ok || msg.update.Err != nil {
			t.Fatalf("native feed lost: %+v", msg)
		}
		m.receiveFeed(msg)
	}
	if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, "Faux: native TUI") {
		t.Fatal("native answer was not rendered")
	}
	if entries, err := os.ReadDir(cwd); err != nil || len(entries) != 0 {
		t.Fatalf("native fixture touched the project: %v %v", entries, err)
	}
}

func TestStandaloneTUIPrompt(t *testing.T) {
	root, cwd := standaloneHome(t)
	cwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	makeDir := func(path string) {
		t.Helper()
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	type providerRequest struct {
		Messages []struct {
			Role    string
			Content json.RawMessage
		}
		Tools []struct {
			Function struct{ Name string }
		}
	}
	seen := make(chan providerRequest, 4)
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req providerRequest
		if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
			return
		}
		select {
		case seen <- req:
		default:
			http.Error(w, "too many fixture requests", http.StatusBadRequest)
			return
		}
		answer := fmt.Sprintf("scripted TUI reply %d", requests.Add(1))
		w.Header().Set("Content-Type", "text/event-stream")
		const chunk = `data: {"id":"fixture","object":"chat.completion.chunk","created":1,"model":"prompt-fixture","choices":[{"index":0,"delta":%s,"finish_reason":%s}]}` + "\n\n"
		fmt.Fprintf(w, chunk, fmt.Sprintf(`{"content":%q}`, answer), "null")
		fmt.Fprintf(w, chunk, "{}", `"stop"`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(provider.Close)

	home, pi, project := filepath.Join(root, ".hush"), filepath.Join(root, "ignored-pi"), filepath.Join(cwd, ".hush")
	makeDir(home)
	makeDir(pi)
	makeDir(project)
	write(filepath.Join(home, "models.json"), fmt.Sprintf(`{"providers":{"fixture":{"baseUrl":%q,"api":"openai-completions","models":[{"id":"prompt-fixture","name":"Prompt fixture","reasoning":false,"contextWindow":20000,"maxTokens":500}]}}}`, provider.URL+"/v1"))
	write(filepath.Join(home, "auth.json"), `{"fixture":{"type":"api_key","key":"fixture-not-a-secret"}}`)
	write(filepath.Join(pi, "SYSTEM.md"), "tui-pi-system-sentinel")
	write(filepath.Join(pi, "APPEND_SYSTEM.md"), "tui-pi-append-sentinel")
	write(filepath.Join(project, "SYSTEM.md"), "tui-project-system-sentinel")
	write(filepath.Join(project, "APPEND_SYSTEM.md"), "tui-project-append-sentinel")
	write(filepath.Join(root, "AGENTS.md"), "tui-outside-workspace-sentinel")
	agents := filepath.Join(cwd, "AGENTS.md")
	write(agents, "tui-agents-original\n")
	personalAgents := filepath.Join(home, "AGENTS.md")
	write(personalAgents, "tui-personal-agents-original\n")
	appendPath := filepath.Join(home, "APPEND_SYSTEM.md")
	const appendMarker = "tui-personal-append-sentinel"
	write(appendPath, appendMarker+"\n")
	bundled, err := os.ReadFile("../backend/src/prompts/system.md")
	if err != nil {
		t.Fatal(err)
	}

	ctx, client := startStandaloneFixture(t, root, cwd, "--model", "fixture/prompt-fixture")
	thread, err := client.NewThread(ctx, sdk.NewThread{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(ctx, client, Options{ThreadID: thread.ID, ReducedMotion: true,
		ClientEnvironment: sdk.ClientEnvironment{OS: "macos", Shell: "fish"}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	first := m.beginFeed()().(feedMsg)
	if !first.ok || first.update.Err != nil {
		t.Fatalf("first feed: %+v", first)
	}
	m.receiveFeed(first)
	m.editor.area.Focus()
	for i, local := range []string{"tui-agents-original", "tui-agents-edited"} {
		personal := "tui-personal-agents-original"
		if i > 0 {
			if err := os.Remove(appendPath); err != nil {
				t.Fatal(err)
			}
			personal = "tui-personal-agents-edited"
			write(personalAgents, personal+"\n")
			write(agents, local+"\n")
		}
		userText := fmt.Sprintf("TUI prompt fixture turn %d\n", i+1)
		m.editor.set(userText)
		m.Update(tea.PasteMsg{Content: pasteFixture()})
		userText += pasteFixture()
		if !strings.Contains(ansi.Strip(assertFrame(t, m)), pasteLabel29) {
			t.Fatal("large paste did not fold in the prompt")
		}
		result := m.sendPrompt(sdk.Steer)().(workMsg)
		if result.err != nil {
			t.Fatal(result.err)
		}
		m.finishWork(result)
		// The scripted response settles the TUI turn; the captured request,
		// not the response, is evidence about prompt assembly.
		answer := fmt.Sprintf("scripted TUI reply %d", i+1)
		for state := m.threads[m.active]; state.lastAnswer != answer || threadBusy(state); {
			msg := awaitFeed(m.feed, first.source)().(feedMsg)
			if !msg.ok || msg.update.Err != nil {
				t.Fatalf("native feed lost: %+v", msg)
			}
			m.receiveFeed(msg)
		}
		if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, answer) {
			t.Fatalf("answer not rendered: %s", answer)
		}
		var req providerRequest
		select {
		case req = <-seen:
		case <-ctx.Done():
			t.Fatal("loopback provider request missing")
		}
		var system []string
		var lastUser json.RawMessage
		for _, message := range req.Messages {
			if message.Role == "user" {
				lastUser = message.Content
			}
			if message.Role != "system" && message.Role != "developer" {
				if strings.Contains(string(message.Content), "You are Hush") {
					t.Fatal("default leaked into chat messages")
				}
				continue
			}
			var text string
			if err := json.Unmarshal(message.Content, &text); err != nil {
				t.Fatal(err)
			}
			system = append(system, text)
		}
		var submitted string
		if err := json.Unmarshal(lastUser, &submitted); err != nil {
			var parts []struct{ Text string }
			if err := json.Unmarshal(lastUser, &parts); err != nil || len(parts) != 1 {
				t.Fatalf("unexpected user message: %s", lastUser)
			}
			submitted = parts[0].Text
		}
		if submitted != userText {
			t.Fatalf("provider received %q, want %q", submitted, userText)
		}
		prompt := strings.Join(system, "\n\n")
		expected := []string{strings.TrimSpace(string(bundled)), fmt.Sprintf("Working directory: %q", cwd), "## Personal instructions"}
		ignored := []string{"tui-pi-system-sentinel", "tui-pi-append-sentinel", "tui-project-system-sentinel", "tui-project-append-sentinel", "tui-outside-workspace-sentinel"}
		if i == 0 {
			expected = append(expected, appendMarker)
		} else {
			ignored = append(ignored, appendMarker, "tui-personal-agents-original", "tui-agents-original")
		}
		expected = append(expected, personal, "## Workspace context", local, "## Environment", "Client OS: macOS", "Client shell (invoking process): fish")
		after := 0
		for _, text := range expected {
			at := strings.Index(prompt[after:], text)
			if at < 0 || strings.Count(prompt, text) != 1 {
				t.Fatalf("missing, duplicated or out of order: %q\n%s", text, prompt)
			}
			after += at + len(text)
		}
		for _, message := range req.Messages {
			for _, text := range ignored {
				if strings.Contains(string(message.Content), text) {
					t.Fatalf("unexpected prompt text: %q", text)
				}
			}
		}
		var declared []string
		for _, tool := range req.Tools {
			name := tool.Function.Name
			declared = append(declared, name)
			if strings.Contains(prompt, "\n- "+name+": ") {
				t.Fatalf("duplicated tool instructions for %s", name)
			}
		}
		slices.Sort(declared)
		if !slices.Equal(declared, []string{"bash", "edit", "plan_read", "plan_save", "read", "subagent", "thread_start", "write"}) {
			t.Fatalf("unexpected tool declarations: %v", declared)
		}
	}
	if n := requests.Load(); n != 2 {
		t.Fatalf("expected one provider request per turn, got %d", n)
	}
}

func TestStandaloneTUIModelPicker(t *testing.T) {
	root, cwd := standaloneHome(t)
	seen := make(chan string, 4)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Model string }
		if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
			return
		}
		select {
		case seen <- req.Model:
		default:
			http.Error(w, "unexpected extra generation", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		const chunk = `data: {"id":"fixture","object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":%s,"finish_reason":%s}]}` + "\n\n"
		fmt.Fprintf(w, chunk, req.Model, fmt.Sprintf(`{"content":%q}`, "reply from "+req.Model), "null")
		fmt.Fprintf(w, chunk, req.Model, "{}", `"stop"`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(provider.Close)
	home := filepath.Join(root, ".hush")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureIDs := []string{"alpha", "beta"}
	for i := range 70 {
		fixtureIDs = append(fixtureIDs, fmt.Sprintf("zz-%02d", i))
	}
	const firstModel = "zz-69"
	var models []string
	for _, id := range fixtureIDs {
		models = append(models, fmt.Sprintf(`{"id":%q,"name":%q,"reasoning":%t,"contextWindow":20000,"maxTokens":500}`, id, "Fixture "+id, id == firstModel))
	}
	modelsJSON := fmt.Sprintf(`{"providers":{"fixture":{"baseUrl":%q,"api":"openai-completions","models":[%s]}}}`, provider.URL+"/v1", strings.Join(models, ","))
	if err := os.WriteFile(filepath.Join(home, "models.json"), []byte(modelsJSON), 0600); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(home, "auth.json")
	const fixtureAuth = `{"fixture":{"type":"api_key","key":"local-fixture-key"}}`
	if err := os.WriteFile(auth, []byte(fixtureAuth), 0600); err != nil {
		t.Fatal(err)
	}
	var id sdk.ID
	var epoch string
	for _, phase := range []string{"pick", "restart"} {
		if !t.Run(phase, func(t *testing.T) {
			ctx, client := startStandaloneFixture(t, root, cwd, "--model", "fixture/alpha")
			m, err := New(ctx, client, Options{ThreadID: id, NewThread: sdk.NewThread{CWD: cwd}, ReducedMotion: true, Intro: &IntroOptions{}})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			native := m.backend.(*sdk.Client)
			hello, err := native.Hello(ctx)
			if err != nil || hello.Capabilities["modelCatalog"] != sdk.ModelCatalogDraftVersion || hello.Capabilities["piEvents"] != 0 || hello.Epoch == epoch {
				t.Fatalf("native catalog negotiation/epoch: %+v, %v", hello, err)
			}
			epoch = hello.Epoch
			m.Update(m.menuProbe()())
			m.Update(tea.WindowSizeMsg{Width: 46, Height: 20})
			if phase == "pick" {
				// Start without --thread, just like a new interactive invocation.
				report := m.beginBackendReport()().(backendReportMsg)
				if !report.ok || report.update.Err != nil || deref(report.update.Report.Model).ModelID != "alpha" {
					t.Fatalf("expected the default model in the startup report: %+v", report.update)
				}
				m.receiveBackendReport(report)
				before, err := client.ListThreads(ctx, 100, "")
				if err != nil {
					t.Fatal(err)
				}
				const draft = "first prompt from a fresh TUI"
				m.editor.set(draft)
				open := press(m, 'l', tea.ModCtrl)
				if open == nil || m.sheet == nil {
					t.Fatalf("Ctrl+L did not open the draft picker: %s", m.notice.text)
				}
				m.Update(open())
				if len(m.sheet.choices) != sdk.ModelCatalogLimit || slices.ContainsFunc(m.sheet.choices, func(c x.Choice) bool {
					return strings.HasSuffix(c.Label, "fixture/"+firstModel)
				}) {
					t.Fatal("fixture did not crowd out the model in the default page")
				}
				press(m, tea.KeyEscape, 0)
				if m.active != "" || m.editor.value() != draft || m.threads[""].info.Model != (sdk.Model{}) {
					t.Fatal("canceling model selection changed the draft")
				}
				m.Update(press(m, 'l', tea.ModCtrl)())
				_, search := m.Update(tea.PasteMsg{Content: strings.ToUpper(firstModel)})
				m.Update(search())
				if len(m.sheet.choices) != 1 || m.sheet.choices[0].Label != "fixture/"+firstModel {
					t.Fatal("draft catalog search did not find the model beyond the first page")
				}
				assertFrame(t, m)
				press(m, tea.KeyEnter, 0)
				if m.sheet == nil || m.sheet.title != "Thinking level" {
					t.Fatal("draft model picker did not offer thinking levels")
				}
				for _, level := range m.sheet.choices {
					if level.ID == "high" {
						break
					}
					press(m, tea.KeyDown, 0)
				}
				press(m, tea.KeyEnter, 0)
				if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, firstModel+" high") || m.editor.value() != draft || m.active != "" ||
					!strings.Contains(m.intro.labels[introModel].text, firstModel) {
					t.Fatalf("selection not shown on the untouched draft:\n%s", body)
				}
				unchanged := func() {
					t.Helper()
					after, err := client.ListThreads(ctx, 100, "")
					if err != nil || !reflect.DeepEqual(before, after) || len(seen) != 0 {
						t.Fatalf("unexpected thread or provider request: %+v, %v", after, err)
					}
				}
				unchanged()
				if err := os.WriteFile(auth, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
				rejected := m.sendPrompt(sdk.Steer)().(workMsg)
				if !sdk.IsCode(rejected.err, "invalid_request") {
					t.Fatalf("expected credentials to be rechecked at creation: %v", rejected.err)
				}
				m.finishWork(rejected)
				if m.active != "" || m.editor.value() != draft || m.threads[""].info.Model.ModelID != firstModel {
					t.Fatal("rejected creation lost the draft or staged model")
				}
				unchanged()
				if err := os.WriteFile(auth, []byte(fixtureAuth), 0600); err != nil {
					t.Fatal(err)
				}
				msg := m.sendPrompt(sdk.Steer)().(workMsg)
				if msg.err != nil {
					t.Fatal(msg.err)
				}
				m.finishWork(msg)
				id = m.active
				if !id.Valid() || m.threads[""].info.Model != (sdk.Model{}) {
					t.Fatal("first prompt did not consume the model selection and attach the new thread")
				}
			}
			first := m.beginFeed()().(feedMsg)
			if !first.ok || first.update.Err != nil {
				t.Fatalf("first feed: %+v", first)
			}
			m.receiveFeed(first)
			await := func() {
				t.Helper()
				msg := awaitFeed(m.feed, first.source)().(feedMsg)
				if !msg.ok || msg.update.Err != nil {
					t.Fatalf("native feed lost: %+v", msg)
				}
				m.receiveFeed(msg)
			}
			run := func(cmd tea.Cmd) {
				t.Helper()
				if cmd == nil {
					t.Fatal("expected async picker work")
				}
				msg, ok := cmd().(workMsg)
				if !ok {
					t.Fatal("expected picker work result")
				}
				m.finishWork(msg)
			}
			open := func() {
				t.Helper()
				cmd := m.runAction(m.host.byID[modelPickerID], "")
				if cmd == nil {
					t.Fatal("expected async model search")
				}
				m.Update(cmd())
				if m.sheet == nil || len(m.sheet.choices) < 2 {
					t.Fatalf("no native picker: %s", m.notice.text)
				}
				assertFrame(t, m)
			}
			answered := func(want string, previous sdk.ID) {
				t.Helper()
				state := m.threads[id]
				for state.info.LastEntryID == previous || state.lastAnswer != "reply from "+want || threadBusy(state) {
					await()
				}
				select {
				case got := <-seen:
					if got != want {
						t.Fatalf("provider request model = %q, want %q", got, want)
					}
				case <-ctx.Done():
					t.Fatal("no provider request")
				}
			}
			ask := func(want, turn string) {
				t.Helper()
				previous := m.threads[id].info.LastEntryID
				m.editor.set(turn)
				msg := m.sendPrompt(sdk.Steer)().(workMsg)
				if msg.err != nil {
					t.Fatal(msg.err)
				}
				m.finishWork(msg)
				answered(want, previous)
			}
			if phase == "pick" {
				answered(firstModel, "")
				if info := m.threads[id].info; info.Model.ModelID != firstModel || info.Thinking != "high" {
					t.Fatalf("new thread did not store the draft selection: %+v", info)
				}
				for _, want := range []string{"alpha", "beta"} {
					open()
					at := -1
					for i, choice := range m.sheet.choices {
						if strings.HasSuffix(choice.Label, "fixture/"+want) {
							at = i
							break
						}
					}
					if at < 0 {
						t.Fatalf("usable model missing: %s", want)
					}
					for range at {
						press(m, tea.KeyDown, 0)
					}
					press(m, tea.KeyEnter, 0)
					if m.sheet == nil || len(m.sheet.choices) != 1 || m.sheet.choices[0].ID != "off" {
						t.Fatal("non-reasoning model must only offer off")
					}
					assertFrame(t, m)
					run(m.acceptChoice(m.sheet))
					if m.notice.text != "Model updated" {
						t.Fatalf("configure did not succeed: %s", m.notice.text)
					}
					ask(want, "pick "+want)
				}
				reconnected := sdk.NewClient(filepath.Join(root, ".hush", "runtime", "hush.sock"), sdk.ClientOptions{
					Capabilities: map[string][]uint64{"modelCatalog": {sdk.ModelCatalogVersion}},
				})
				defer reconnected.Close()
				catalog, err := reconnected.ModelCatalog(ctx, id)
				if err != nil || catalog.Current == nil || catalog.Current.ModelID != "beta" || len(catalog.Models) != sdk.ModelCatalogLimit {
					t.Fatalf("selection after reconnect: %+v, %v", catalog.Current, err)
				}
				if err := reconnected.ConfigureCatalogThread(ctx, id, nil, "high"); !sdk.IsCode(err, "invalid_request") {
					t.Fatalf("native host accepted unsupported thinking: %v", err)
				}
				run(m.runAction(m.host.byID[menusID+".thinking"], ""))
				if m.sheet == nil || len(m.sheet.choices) != 1 || m.sheet.choices[0].ID != "off" {
					t.Fatal("native thinking action offered unsupported levels")
				}
				run(m.acceptChoice(m.sheet))
				if m.notice.text != "Thinking level updated" {
					t.Fatalf("native thinking action: %s", m.notice.text)
				}
				late := sdk.Model{Provider: "fixture", ModelID: "zz-69"}
				lateThread, err := client.NewThread(ctx, sdk.NewThread{CWD: cwd, Model: &late})
				if err != nil {
					t.Fatal(err)
				}
				catalog, err = reconnected.ModelCatalog(ctx, lateThread.ID)
				if err != nil || len(catalog.Models) != sdk.ModelCatalogLimit || catalog.Models[0].Model != late || !catalog.Models[0].Available ||
					catalog.Models[1].Model.ModelID != "alpha" {
					t.Fatalf("cap did not retain and prioritize current/default: %v", err)
				}
				missing := sdk.Model{Provider: "fixture", ModelID: "removed"}
				thread, err := client.NewThread(ctx, sdk.NewThread{CWD: cwd, Model: &missing})
				if err != nil {
					t.Fatal(err)
				}
				catalog, err = reconnected.ModelCatalog(ctx, thread.ID)
				if err != nil || catalog.Current == nil || *catalog.Current != missing || len(catalog.Models) < 2 ||
					catalog.Models[0].Model != missing || catalog.Models[0].Available || deref(catalog.Models[0].Reason) != "missing_model" {
					t.Fatalf("catalog lost stored missing model: %+v, %v", catalog.Current, err)
				}
				return
			}
			if m.threads[id].info.Model.ModelID != "beta" {
				t.Fatal("saved selection lost on restart")
			}
			ask("beta", "after restart")
			if err := os.WriteFile(auth, []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}
			open()
			if !strings.Contains(m.sheet.choices[0].Label, "Current (unavailable) · fixture/beta") {
				t.Fatal("unavailable selection lost from picker")
			}
			press(m, tea.KeyEnter, 0)
			if !strings.Contains(m.notice.text, "Configure provider credentials") || len(m.pending) != 0 {
				t.Fatalf("unavailable model was not actionable without mutation: %s", m.notice.text)
			}
			if _, err := client.Submit(ctx, sdk.Input{ThreadID: id, RequestID: "unavailable", Text: "must not admit"}); !sdk.IsCode(err, "invalid_request") {
				t.Fatalf("unexpected unavailable input result: %v", err)
			}
			if _, err := client.LookupSubmission(ctx, id, "unavailable"); !sdk.IsCode(err, "not_found") {
				t.Fatalf("unavailable input admitted: %v", err)
			}
			select {
			case model := <-seen:
				t.Fatalf("unexpected provider request for %q", model)
			default:
			}
		}) {
			return
		}
	}
}

type workflowHistoryBackend struct {
	Backend
	history func(context.Context, sdk.ID, sdk.HistoryRange, int) (sdk.HistoryPage, error)
}

func (b workflowHistoryBackend) History(ctx context.Context, id sdk.ID, range_ sdk.HistoryRange, limit int) (sdk.HistoryPage, error) {
	return b.history(ctx, id, range_, limit)
}

func workflowEntries(lo, hi int) []sdk.Entry {
	var entries []sdk.Entry
	for i := lo; i <= hi; i++ {
		entries = append(entries, sdk.Entry{ThreadID: "1", ID: sdk.ID(fmt.Sprint(i)), Kind: "test"})
	}
	return entries
}

func workflowFeed(m *Model, lower sdk.ID, lo, hi int) feedMsg {
	return feedMsg{token: m.feed.token, owner: m.active, ok: true, update: sdk.StateUpdate{Snapshot: sdk.Snapshot{State: sdk.State{
		Thread: sdk.Thread{ID: m.active}, Window: sdk.TranscriptWindow{
			BeforeEntryID: idPtr(lower), Entries: workflowEntries(lo, hi), LastEntryID: idPtr(sdk.ID(fmt.Sprint(hi))),
		},
	}}}}
}

func TestWorkflowHistoryMidGapRetry(t *testing.T) {
	calls := 0
	bounds := [][2]sdk.ID{{"39", "260"}, {"139", "260"}, {"139", "260"}, {"239", "260"}, {"260", "261"}}
	backend := workflowHistoryBackend{history: func(_ context.Context, id sdk.ID, r sdk.HistoryRange, n int) (sdk.HistoryPage, error) {
		t.Helper()
		if calls >= len(bounds) || id != "1" || n != 100 || !reflect.DeepEqual(r, sdk.After(&bounds[calls][0], &bounds[calls][1])) {
			t.Fatalf("unexpected bounded history request %d: %+v", calls, r)
		}
		calls++
		page := sdk.HistoryPage{ThreadID: id}
		switch calls {
		case 1:
			page.Entries, page.NextCursor = workflowEntries(40, 139), ptr("page1")
		case 2:
			return sdk.HistoryPage{}, &sdk.TransportError{Err: io.ErrUnexpectedEOF, Indeterminate: true}
		case 3:
			page.Entries, page.NextCursor = workflowEntries(140, 239), ptr("page2")
		case 4:
			page.Entries = workflowEntries(240, 260)
		case 5:
			page.Entries = workflowEntries(261, 261)
		}
		return page, nil
	}}
	m, err := New(t.Context(), backend, Options{ThreadID: "1", ReducedMotion: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	_ = m.beginFeed()
	m.receiveFeed(workflowFeed(m, "", 20, 39))
	state := m.threads[m.active]
	old := state.entries["39"]
	m.receiveFeed(feedMsg{token: m.feed.token, owner: m.active, ok: true, update: sdk.StateUpdate{Err: io.EOF}})
	batch := m.receiveFeed(workflowFeed(m, "229", 230, 260))().(tea.BatchMsg)
	overlap := state.entries["235"]
	next := m.receiveHistory(batch[1]().(historyMsg))
	retry := m.receiveHistory(next().(historyMsg))
	if retry == nil || state.through != "139" || state.entries["140"] != nil || state.gapUpper != "260" || state.pageError {
		t.Fatalf("failed page advanced or closed the gap: %s through %s", state.through, state.gapUpper)
	}
	// The next watch observation cannot move the in-flight upper fence.
	m.receiveFeed(workflowFeed(m, "230", 231, 261))
	for cmd := retry; cmd != nil; {
		cmd = m.receiveHistory(cmd().(historyMsg))
	}
	want := workflowEntries(20, 261)
	if calls != len(bounds) || len(state.ids) != len(want) || state.through != "261" || state.gapUpper != "" ||
		state.entries["39"] != old || state.entries["235"] != overlap {
		t.Fatalf("incomplete or replaced scrollback: %d calls, %d entries, %s through %s", calls, len(state.ids), state.through, state.gapUpper)
	}
	for i, id := range state.ids {
		if id != want[i].ID {
			t.Fatalf("gap, duplicate or reorder at index %d: %q != %q", i, id, want[i].ID)
		}
	}
}

func TestWorkflowHistoryRefusesUnprovedProgress(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []sdk.Entry
		err  error
	}{
		{"omitted overlap", append(workflowEntries(3, 3), workflowEntries(5, 5)...), nil},
		{"omitted predecessor", workflowEntries(4, 5), nil},
		{"terminal short of fence", workflowEntries(3, 4), nil},
		{"permanent", nil, &sdk.ServerError{Code: "invalid_cursor"}},
		{"malformed", nil, &sdk.TransportError{Err: sdk.ErrProtocol}},
		{"changed store", nil, &sdk.TransportError{Err: sdk.ErrStoreChanged}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := New(t.Context(), workflowHistoryBackend{}, Options{ThreadID: "1"})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			_ = m.beginFeed()
			m.receiveFeed(workflowFeed(m, "", 1, 2))
			m.receiveFeed(workflowFeed(m, "3", 4, 5))
			state := m.threads[m.active]
			old := state.entries["4"]
			m.receiveHistory(historyMsg{lease: m.feed.token, owner: m.active, lower: "2", upper: "5", forward: true,
				page: sdk.HistoryPage{ThreadID: m.active, Entries: tc.rows}, err: tc.err})
			if state.gapRetries != 0 || state.through != "2" || state.gapUpper != "5" || state.entries["4"] != old || !state.pageError ||
				state.entries["3"] != nil || !strings.Contains(m.notice.text, "History incomplete") {
				t.Fatalf("accepted unproven progress or tried to retry: through %s, gap %s, retries %d, %s", state.through, state.gapUpper, state.gapRetries, m.notice.text)
			}
		})
	}
}

func TestWorkflowHistoryRetryBudgetAndLease(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%t", canceled), func(t *testing.T) {
			calls := 0
			backend := workflowHistoryBackend{history: func(_ context.Context, _ sdk.ID, _ sdk.HistoryRange, _ int) (sdk.HistoryPage, error) {
				calls++
				return sdk.HistoryPage{}, &sdk.TransportError{Err: io.EOF}
			}}
			m, err := New(t.Context(), backend, Options{ThreadID: "1"})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			_ = m.beginFeed()
			m.receiveFeed(workflowFeed(m, "", 1, 2))
			batch := m.receiveFeed(workflowFeed(m, "3", 4, 5))().(tea.BatchMsg)
			next := m.receiveHistory(batch[1]().(historyMsg))
			state := m.threads[m.active]
			if next == nil {
				t.Fatal("transient failure did not schedule retry")
			}
			if canceled {
				m.stopFeed()
				if next() != nil || calls != 1 {
					t.Fatal("released lease ran a queued retry")
				}
				return
			}
			for range maxHistoryRetries {
				next = m.receiveHistory(next().(historyMsg))
			}
			if calls != 1+maxHistoryRetries || state.through != "2" || state.gapUpper != "5" || !state.pageError || m.fetchGap(state) != nil {
				t.Fatalf("retry budget did not leave a latched gap: %d calls, %s through %s", calls, state.through, state.gapUpper)
			}
		})
	}
}

func TestWorkflowStopRestoresOnlyConfirmedInputAndImage(t *testing.T) {
	m, err := New(t.Context(), workflowHistoryBackend{}, Options{ThreadID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.editor.set("keep draft")
	inputs := []localInput{
		{id: "2", text: "placed", images: []draftImage{{label: "placed", image: sdk.Image{Data: []byte{1}}}}},
		{id: "3", images: []draftImage{{label: "withdrawn", image: sdk.Image{Data: []byte{2}}}}},
		{id: "4", text: "unknown", images: []draftImage{{label: "unknown", image: sdk.Image{Data: []byte{3}}}}},
	}
	m.finishStop(m.route(m.active, coreID), inputs, false, stopResult{StopResult: sdk.StopResult{Stopped: true, Candidates: []sdk.StopCandidate{
		{SubmissionID: "2", Outcome: sdk.NotWithdrawn}, {SubmissionID: "3", Outcome: sdk.Withdrawn}, {SubmissionID: "4", Outcome: sdk.StopUnknown},
	}}}, nil)
	images := m.threads[m.active].attachments
	if m.editor.value() != "keep draft" || len(images) != 1 || images[0].label != "withdrawn" ||
		!strings.Contains(m.notice.text, "unverified inputs not restored: #4") {
		t.Fatalf("stop mixed uncertain/placed input into the draft: %q, %+v, %q", m.editor.value(), images, m.notice.text)
	}
}

func workflowWork(t *testing.T, m *Model, cmd tea.Cmd) workMsg {
	t.Helper()
	if cmd == nil {
		t.Fatal("workflow did not schedule work")
	}
	msg, ok := cmd().(workMsg)
	if !ok || msg.err != nil {
		t.Fatalf("workflow command failed: %+v", msg)
	}
	m.finishWork(msg)
	return msg
}

func workflowWatch(t *testing.T, m *Model) func(func(feedMsg) bool) {
	t.Helper()
	first, ok := m.beginFeed()().(feedMsg)
	if !ok || !first.ok || first.update.Err != nil {
		t.Fatalf("initial workflow feed: %+v", first)
	}
	m.receiveFeed(first)
	return func(want func(feedMsg) bool) {
		t.Helper()
		for msg := first; !want(msg); {
			var ok bool
			msg, ok = awaitFeed(m.feed, first.source)().(feedMsg)
			if !ok || !msg.ok || msg.update.Err != nil {
				t.Fatalf("workflow feed lost: %+v (%v)", msg, m.ctx.Err())
			}
			m.receiveFeed(msg)
		}
	}
}

func workflowWait(ctx context.Context, t *testing.T, want func() bool) {
	t.Helper()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for !want() {
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatalf("workflow condition not reached: %v", ctx.Err())
		}
	}
}

type workflowStopBackend struct {
	*sdk.Client
	started chan struct{}
	proceed chan struct{}
}

func (b workflowStopBackend) Stop(ctx context.Context, id sdk.ID, ids []sdk.ID) (sdk.StopResult, error) {
	close(b.started)
	select {
	case <-ctx.Done():
		return sdk.StopResult{}, ctx.Err()
	case <-b.proceed:
		return b.Client.Stop(ctx, id, ids)
	}
}

func TestStandaloneTUIStopRace(t *testing.T) {
	root, cwd := standaloneHome(t)
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(cwd, "fixture.txt"), "fixture tool result\n")
	first, second, release := make(chan struct{}), make(chan string, 1), make(chan struct{})
	var calls atomic.Int32
	life, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || r.URL.Path != "/v1/chat/completions" {
			return
		}
		switch calls.Add(1) {
		case 1:
			close(first)
			select {
			case <-release:
			case <-life.Done():
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			const chunk = `data: {"id":"fixture","object":"chat.completion.chunk","created":1,"model":"stop-fixture","choices":[{"index":0,"delta":%s,"finish_reason":%s}]}` + "\n\n"
			fmt.Fprintf(w, chunk, `{"role":"assistant","tool_calls":[{"index":0,"id":"read-before-steer","type":"function","function":{"name":"read","arguments":"{\"path\":\"fixture.txt\"}"}}]}`, "null")
			fmt.Fprintf(w, chunk, "{}", `"tool_calls"`)
			fmt.Fprint(w, "data: [DONE]\n\n")
		case 2:
			second <- string(body)
			select {
			case <-r.Context().Done():
			case <-life.Done():
			}
		default:
			http.Error(w, "unexpected automatic replay", http.StatusBadRequest)
		}
	}))
	t.Cleanup(func() { cancel(); provider.Close() })
	home := filepath.Join(root, ".hush")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(home, "models.json"), fmt.Sprintf(`{"providers":{"fixture":{"baseUrl":%q,"api":"openai-completions","models":[{"id":"stop-fixture","name":"Stop fixture","reasoning":false,"contextWindow":20000,"maxTokens":500}]}}}`, provider.URL+"/v1"))
	write(filepath.Join(home, "auth.json"), `{"fixture":{"type":"api_key","key":"local-fixture-key"}}`)
	ctx, client := startStandaloneFixture(t, root, cwd, "--model", "fixture/stop-fixture")
	peer := sdk.NewClient(filepath.Join(home, "runtime", "hush.sock"), sdk.ClientOptions{RequiredCapabilities: map[string]uint64{"hushProfile": 1}})
	defer peer.Close()
	thread, err := peer.NewThread(ctx, sdk.NewThread{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	b := workflowStopBackend{client, make(chan struct{}), make(chan struct{})}
	m, err := New(ctx, b, Options{ThreadID: thread.ID, ReducedMotion: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	wait := workflowWatch(t, m)
	if _, err := peer.Submit(ctx, sdk.Input{ThreadID: thread.ID, RequestID: "begin", Text: "start run"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first:
	case <-ctx.Done():
		t.Fatal("first run never reached the provider")
	}
	state := m.threads[m.active]
	wait(func(feedMsg) bool { return state.activeRun })
	m.editor.set("steer wins delivery")
	steer := workflowWork(t, m, m.sendPrompt(sdk.Steer)).result.(sendResult).receipt.SubmissionID
	m.editor.set("follow-up is withdrawn")
	follow := workflowWork(t, m, m.sendPrompt(sdk.FollowUp)).result.(sendResult).receipt.SubmissionID
	wait(func(feedMsg) bool { return len(state.queue) == 2 })
	if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "steer queued · steer wins delivery") ||
		!strings.Contains(frame, "follow-up queued · follow-up is withdrawn") {
		t.Fatalf("waiting inputs disappeared from the frame:\n%s", frame)
	}
	for _, id := range []sdk.ID{steer, follow} {
		if row, err := peer.GetSubmission(ctx, thread.ID, id); err != nil || row.Status != "queued" || row.EntryID != nil {
			t.Fatalf("fixture requires an actual queued receipt: %+v %v", row, err)
		}
	}
	m.editor.set("keep existing draft")
	press(m, tea.KeyEscape, 0)
	if state.aborting || m.stopAt.IsZero() {
		t.Fatal("first Esc should only arm stop")
	}
	cmd := press(m, tea.KeyEscape, 0)
	if cmd == nil || !state.aborting {
		t.Fatal("second Esc did not schedule stop")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case <-b.started:
	case <-ctx.Done():
		t.Fatal("stop command did not enter fixture gate")
	}
	close(release)
	select {
	case body := <-second:
		if !strings.Contains(body, "steer wins delivery") || strings.Contains(body, "follow-up is withdrawn") {
			t.Fatalf("expected steer (not follow-up) to be delivered before stop: %s", body)
		}
	case <-ctx.Done():
		t.Fatal("tool did not reach steer delivery")
	}
	placed, err := peer.GetSubmission(ctx, thread.ID, steer)
	if err != nil || placed.EntryID == nil || placed.Status != "placed" {
		t.Fatalf("steer won the race but lacks durable placement: %+v %v", placed, err)
	}
	// Deliberately do not consume the updated view. The stop must use current
	// Durable evidence, not the stale queue (which still lists both inputs).
	close(b.proceed)
	select {
	case msg := <-done:
		w, ok := msg.(workMsg)
		if !ok || w.err != nil {
			t.Fatalf("stop result: %+v", msg)
		}
		m.finishWork(w)
	case <-ctx.Done():
		t.Fatal("stop did not return")
	}
	withdrawn, err := peer.GetSubmission(ctx, thread.ID, follow)
	if err != nil || withdrawn.Status != "unanswered" || withdrawn.Reason != "aborted" || withdrawn.EntryID != nil {
		t.Fatalf("follow-up lacks withdrawal evidence: %+v %v", withdrawn, err)
	}
	settled, err := peer.GetSubmission(ctx, thread.ID, steer)
	if err != nil || deref(settled.EntryID) != *placed.EntryID || m.editor.value() != "follow-up is withdrawn\nkeep existing draft" ||
		!strings.Contains(m.notice.text, "restored 1 withdrawn") || !strings.HasPrefix(m.notice.text, "Stopped;") || calls.Load() != 2 {
		t.Fatalf("wrong restoration/replay: placed %+v (%v), draft %q, notice %q, provider calls %d", settled, err, m.editor.value(), m.notice.text, calls.Load())
	}
	wait(func(feedMsg) bool { return len(state.queue) == 0 && state.entries[*placed.EntryID] != nil })
	if frame := ansi.Strip(assertFrame(t, m)); strings.Contains(frame, "steer queued") ||
		strings.Contains(frame, "follow-up queued") || strings.Count(frame, "steer wins delivery") != 1 {
		t.Fatalf("pending previews did not yield to placement and withdrawal:\n%s", frame)
	}
}

func TestStandaloneTUIWorkspaceReturnAndFork(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for the two-repository fixture")
	}
	root, inside := standaloneHome(t)
	outside, sub := filepath.Join(root, "outside"), filepath.Join(inside, "sub")
	for _, path := range []string{outside, sub} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	paths := map[string]string{inside: "root marker must not win", sub: "inside checkout marker", outside: "outside checkout marker"}
	for dir := range paths {
		if err := os.WriteFile(filepath.Join(dir, "workspace-marker.txt"), []byte("committed fixture\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{inside, outside} {
		git(dir, "init", "--quiet")
		git(dir, "add", ".")
		git(dir, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--quiet", "-m", "committed fixture")
	}
	for dir, text := range paths {
		if err := os.WriteFile(filepath.Join(dir, "workspace-marker.txt"), []byte(text+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	oldStatusA, oldStatusB := git(inside, "status", "--porcelain"), git(outside, "status", "--porcelain")
	ctx, client := startStandaloneFixture(t, root, inside, "--faux")
	grantStandaloneFixture(t, root, sub)
	grantStandaloneFixture(t, root, outside)
	create := func(cwd string) sdk.ID {
		t.Helper()
		thread, err := client.NewThread(ctx, sdk.NewThread{CWD: cwd})
		if err != nil {
			t.Fatal(err)
		}
		return thread.ID
	}
	insideID, rootID, outsideID := create(sub), create(inside), create(outside)
	var first sdk.ThreadDirectoryPage
	workspace := sdk.ThreadDirectoryOptions{Limit: 1, Workspace: inside}
	workflowWait(ctx, t, func() bool {
		var err error
		first, err = client.ListThreadDirectory(ctx, workspace)
		if err != nil {
			t.Fatal(err)
		}
		return !first.Indexing && len(first.Threads) == 1 && first.Threads[0].ID == rootID && first.NextCursor != nil
	})
	all := workspace
	all.BrowseAll = true
	workflowWait(ctx, t, func() bool {
		page, err := client.ListThreadDirectory(ctx, all)
		if err != nil {
			t.Fatal(err)
		}
		return !page.Indexing && len(page.Threads) == 1 && page.Threads[0].ID == outsideID && reflect.DeepEqual(page.WorkspaceMatches, []string{"outside"})
	})
	workspace.Cursor = *first.NextCursor
	workspace.Workspace = sub // same Git common directory, not a different scope
	page, err := client.ListThreadDirectory(ctx, workspace)
	if err != nil || len(page.Threads) != 1 || page.Threads[0].ID != insideID || !reflect.DeepEqual(page.WorkspaceMatches, []string{"inside"}) {
		t.Fatalf("outside thread stole a scoped pagination slot: %+v, %v", page, err)
	}
	if len(page.Workspaces) != 1 || len(first.Workspaces) != 1 || page.Workspaces[0] == nil || first.Workspaces[0] == nil ||
		*page.Workspaces[0] != *first.Workspaces[0] || page.Workspaces[0].Name != filepath.Base(inside) {
		t.Fatalf("Git root and subfolder did not share a workspace name and identity: %+v vs %+v", page.Workspaces, first.Workspaces)
	}
	workspace.Workspace = outside
	if _, err := client.ListThreadDirectory(ctx, workspace); !sdk.IsCode(err, "invalid_cursor") {
		t.Fatalf("changed workspace reused a scope cursor: %v", err)
	}
	all.Cursor = *first.NextCursor
	if _, err := client.ListThreadDirectory(ctx, all); !sdk.IsCode(err, "invalid_cursor") {
		t.Fatalf("browse-all reused a scoped cursor: %v", err)
	}

	open := func(c *sdk.Client, id sdk.ID, local bool) *Model {
		t.Helper()
		m, err := New(ctx, c, Options{ThreadID: id, NewThread: sdk.NewThread{CWD: inside}, WorkspaceLocal: local, ReducedMotion: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(m.Close)
		return m
	}
	choice := func(m *Model, id string) (x.Choice, bool) {
		t.Helper()
		if m.sheet == nil {
			t.Fatalf("workflow picker did not open: %s", m.notice.text)
		}
		for i, j := range m.sheet.filtered {
			if item := m.sheet.choices[j]; item.ID == id {
				m.sheet.selected = i
				return item, true
			}
		}
		return x.Choice{}, false
	}
	pick := func(m *Model, id string) tea.Cmd {
		t.Helper()
		if _, ok := choice(m, id); !ok {
			t.Fatalf("workflow choice %q missing", id)
		}
		return m.acceptChoice(m.sheet)
	}
	ask := func(m *Model, marker string) {
		t.Helper()
		wait := workflowWatch(t, m)
		m.editor.set("!workspace-read")
		workflowWork(t, m, m.sendPrompt(sdk.Steer))
		state := m.threads[m.active]
		wait(func(feedMsg) bool { return strings.Contains(state.lastAnswer, marker) && !threadBusy(state) })
		if !strings.Contains(ansi.Strip(assertFrame(t, m)), marker) {
			t.Fatalf("tool did not render marker from its checked-out cwd: %s", state.lastAnswer)
		}
	}
	m := open(client, outsideID, true)
	ask(m, paths[outside])
	threadPickerStep(t, m, m.runAction(m.host.byID[switcherID], ""))
	if _, ok := choice(m, string(outsideID)); ok || !strings.Contains(m.sheet.title, "launch workspace") {
		t.Fatal("active outside thread changed the default switcher scope")
	}
	query := paths[outside] // committed assistant text, not the title or workspace name
	threadPickerStep(t, m, m.changeDialogQuery(m.sheet, query))
	if _, ok := choice(m, string(outsideID)); ok {
		t.Fatal("scoped search returned a conversation in another workspace")
	}
	threadPickerStep(t, m, pick(m, "all-workspaces"))
	if _, ok := choice(m, string(outsideID)); !ok || m.sheet.query != query ||
		m.sheet.threadPicker.rows[m.sheet.filtered[m.sheet.selected]].workspaceName != "outside" {
		t.Fatal("explicit browse did not retain the query and find the outside workspace by chat text")
	}
	press(m, tea.KeyUp, 0)
	workflowWork(t, m, threadPickerStep(t, m, press(m, tea.KeyEnter, 0)))
	if !m.active.Valid() || m.active == outsideID || m.sheet != nil {
		t.Fatal("new thread here did not open a fresh thread in the selected workspace")
	}
	ask(m, paths[outside]) // tool reads must use the selected workspace, not the launch directory
	threadPickerStep(t, m, m.runAction(m.host.byID[switchAllID], ""))
	_ = pick(m, string(insideID))
	if m.active != insideID {
		t.Fatal("picker did not switch projects")
	}
	ask(m, paths[sub])
	m.Close()
	client.Close()

	socket := filepath.Join(root, ".hush", "runtime", "hush.sock")
	back := sdk.NewClient(socket, sdk.ClientOptions{Capabilities: standaloneFixtureCapabilities(), RequiredCapabilities: map[string]uint64{"hushProfile": 1}})
	defer back.Close()
	m = open(back, "", true)
	threadPickerStep(t, m, m.runAction(m.host.byID[switcherID], ""))
	_ = pick(m, string(insideID))
	workflowWatch(t, m)
	if m.active != insideID || !strings.Contains(ansi.Strip(assertFrame(t, m)), paths[sub]) {
		t.Fatal("reopened TUI did not resume its committed transcript")
	}
	state := m.threads[insideID]
	at := state.ids[0]
	workflowWork(t, m, m.runAction(m.host.byID[menusID+".fork"], ""))
	if !strings.Contains(m.sheet.title, "files unchanged") {
		t.Fatal("fork picker does not distinguish transcript from file restore")
	}
	workflowWork(t, m, pick(m, string(at)))
	if m.active == insideID || !m.active.Valid() {
		t.Fatal("fork did not switch to a distinct thread")
	}
	fork, err := back.History(ctx, m.active, sdk.Before(nil), 100)
	if err != nil || len(fork.Entries) != 1 || fork.Entries[0].ID != at || fork.Entries[0].ThreadID != insideID {
		t.Fatalf("not an inclusive fork of the selected committed entry: %+v (%v)", fork, err)
	}
	for dir, text := range paths {
		if bytes, err := os.ReadFile(filepath.Join(dir, "workspace-marker.txt")); err != nil || string(bytes) != text+"\n" {
			t.Fatalf("transcript lifecycle touched checkout files: %q %v", bytes, err)
		}
	}
	if git(inside, "status", "--porcelain") != oldStatusA || git(outside, "status", "--porcelain") != oldStatusB {
		t.Fatal("checkout status changed across switch, reopen or fork")
	}

	legacyCaps := standaloneFixtureCapabilities()
	delete(legacyCaps, "threadWorkspace")
	legacy := sdk.NewClient(socket, sdk.ClientOptions{Capabilities: legacyCaps})
	defer legacy.Close()
	old := open(legacy, "", true)
	threadPickerStep(t, old, old.runAction(old.host.byID[switcherID], ""))
	if _, ok := choice(old, string(outsideID)); ok || !strings.Contains(old.sheet.title, "scope unavailable") {
		t.Fatal("older directory was silently substituted for launch workspace scope")
	}
	threadPickerStep(t, old, pick(old, "all-workspaces"))
	if _, ok := choice(old, string(outsideID)); !ok || old.sheet.threadPicker.searchable ||
		old.sheet.threadPicker.rows[old.sheet.filtered[old.sheet.selected]].workspaceName != "Workspace unknown" {
		t.Fatal("legacy explicitly browsed workspace is not visibly unknown")
	}
	v1Caps := standaloneFixtureCapabilities()
	v1Caps["threadWorkspace"] = []uint64{sdk.ThreadWorkspaceVersion}
	v1 := sdk.NewClient(socket, sdk.ClientOptions{Capabilities: v1Caps})
	defer v1.Close()
	v1Opts := sdk.ThreadDirectoryOptions{Limit: 10, Workspace: outside}
	if got, err := v1.ListThreadDirectory(ctx, v1Opts); err != nil || !reflect.DeepEqual(got.WorkspaceMatches, []string{"inside", "inside"}) || got.Workspaces != nil {
		t.Fatalf("v1 directory workspace shape changed: %+v (%v)", got, err)
	}
	v1Opts.Query = "outside"
	if _, err := v1.ListThreadDirectory(ctx, v1Opts); !sdk.IsCode(err, "unsupported_capability") {
		t.Fatalf("v1 unexpectedly allowed a search: %v", err)
	}
	remote := open(back, "", false)
	remote.Update(tea.WindowSizeMsg{Width: 100, Height: 26})
	threadPickerStep(t, remote, remote.runAction(remote.host.byID[switcherID], ""))
	threadPickerStep(t, remote, remote.changeDialogQuery(remote.sheet, "OUTSIDE"))
	if _, ok := choice(remote, string(outsideID)); !ok || !strings.Contains(remote.sheet.title, "all workspaces") ||
		remote.sheet.threadPicker.rows[remote.sheet.filtered[remote.sheet.selected]].workspaceName != "outside" {
		t.Fatal("remote attach without local opt-in should search named workspaces without a launch path")
	}
	threadTurnStep(t, remote, remote.syncThreadTurn())
	if frame := ansi.Strip(assertFrame(t, remote)); !strings.Contains(frame, "Prompt: !workspace-read") ||
		!strings.Contains(frame, "Reply:  "+paths[outside]) || remote.active != "" || remote.threads[outsideID] != nil {
		t.Fatalf("picker did not read the committed turn without opening the thread:\n%s", frame)
	}
}
