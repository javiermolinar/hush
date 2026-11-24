package tui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
)

func TestImageFromDropToModelAndFork(t *testing.T) {
	bundle := os.Getenv("HUSH_TEST_BACKEND")
	if bundle == "" {
		t.Skip("set HUSH_TEST_BACKEND to the built backend bundle")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	requests := make(chan []byte, 8)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, sdk.MaxRecord))
		select {
		case requests <- body:
		case <-ctx.Done():
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"image-fixture","object":"chat.completion.chunk","created":1,"model":"vision","choices":[{"index":0,"delta":{"content":"Image received."},"finish_reason":null}]}`+"\n\n"+
			`data: {"id":"image-fixture","object":"chat.completion.chunk","created":1,"model":"vision","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	dir, err := os.MkdirTemp("/tmp", "hush-img-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	isolated := filepath.Join(dir, "standalone.mjs")
	code, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(isolated, code, 0600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(dir, ".hush")
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"auth.json": `{"fixture":{"type":"api_key","key":"local-test-only"}}`,
		"models.json": `{"providers":{"fixture":{"baseUrl":"` + provider.URL + `/v1","api":"openai-completions","models":[` +
			`{"id":"vision","name":"Vision fixture","input":["text","image"],"inputLimits":{"images":{"resize":{"maxWidth":96,"maxHeight":96,"maxBytes":50000}}},"contextWindow":20000,"maxTokens":500},` +
			`{"id":"text","name":"Text fixture","input":["text"],"contextWindow":20000,"maxTokens":500}]}}}`,
	} {
		if err := os.WriteFile(filepath.Join(profile, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	socket := filepath.Join(profile, "runtime", "hush.sock")
	boot := func() func() {
		t.Helper()
		proc := exec.CommandContext(ctx, "node", isolated, "--profile", "hush", "--model", "fixture/vision")
		proc.Dir = dir
		proc.Env = append(os.Environ(), "HOME="+dir, "PI_CODING_AGENT_DIR="+dir, "NODE_NO_WARNINGS=1")
		proc.Stderr = os.Stderr
		stdout, err := proc.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := proc.Start(); err != nil {
			t.Fatal(err)
		}
		scan := bufio.NewScanner(stdout)
		if !scan.Scan() || !strings.Contains(scan.Text(), `"ready":true`) {
			_ = proc.Process.Kill()
			_ = proc.Wait()
			t.Fatalf("image backend startup: %s %v", scan.Text(), scan.Err())
		}
		return func() { _ = proc.Process.Signal(os.Interrupt); _ = proc.Wait() }
	}
	stop := boot()
	defer func() { stop() }()
	client := sdk.NewClient(socket, sdk.ClientOptions{Capabilities: map[string][]uint64{"images": {sdk.ImagesVersion}}})
	defer client.Close()
	thread, err := client.NewThread(ctx, sdk.NewThread{CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(ctx, client, Options{ThreadID: thread.ID, Images: ImageBlocks, Fills: FillOn, ReducedMotion: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.Update(m.probeImages()())
	first := m.beginFeed()().(feedMsg)
	m.receiveFeed(first)
	path := filepath.Join(t.TempDir(), "client screenshot.png") // not in the backend workspace
	png := pngImage(t, 1000, 240)
	if err := os.WriteFile(path, png, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := m.pastePrompt(tea.PasteMsg{Content: strings.ReplaceAll(path, " ", "\\ ")})
	if cmd == nil {
		t.Fatal("escaped drop not recognized")
	}
	m.Update(cmd().(workMsg))
	if !strings.Contains(ansi.Strip(assertFrame(t, m)), "▣ client screenshot.png") {
		t.Fatal("dropped image chip not rendered")
	}
	// Keep this an image-only submission to prove no synthetic path/caption is needed.
	result := m.sendPrompt(sdk.RejectBusy)().(workMsg)
	if result.err != nil {
		t.Fatal(result.err)
	}
	m.finishWork(result)
	admitted := result.result.(sendResult).receipt.SubmissionID
	settled, err := client.WaitSubmission(ctx, thread.ID, admitted)
	if err != nil || settled.Status != "done" || settled.EntryID == nil {
		t.Fatalf("image submission did not settle: %+v %v", settled, err)
	}
	var body []byte
	select {
	case body = <-requests:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var payload struct {
		Messages []struct {
			Role    string
			Content json.RawMessage
		}
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	var dataURL string
	for _, msg := range payload.Messages {
		if msg.Role == "user" {
			var parts []struct {
				Type     string
				ImageURL struct{ URL string } `json:"image_url"`
			}
			if err := json.Unmarshal(msg.Content, &parts); err != nil {
				t.Fatal(err)
			}
			for _, p := range parts {
				if p.Type == "image_url" {
					dataURL = p.ImageURL.URL
				}
			}
		}
	}
	_, encoded, found := strings.Cut(dataURL, ";base64,")
	fitted, err := base64.StdEncoding.DecodeString(encoded)
	cfg, _, cfgErr := imageConfig(fitted)
	if !found || err != nil || cfgErr != nil || cfg.Width != 96 || cfg.Height != 23 || bytes.Equal(fitted, png) || bytes.Contains(body, []byte(path)) {
		t.Fatalf("provider did not receive fitted image-only input: dimensions %dx%d; %v %v", cfg.Width, cfg.Height, err, cfgErr)
	}
	for !m.threads[m.active].hasImages {
		msg, ok := awaitFeed(m.feed, first.source)().(feedMsg)
		if !ok || !msg.ok || msg.update.Err != nil {
			t.Fatalf("missing committed image from replica: %v", ctx.Err())
		}
		m.receiveFeed(msg)
	}
	if frame := assertFrame(t, m); !strings.Contains(frame, "▣ image") || m.scanImages() != nil {
		t.Fatal("replicated state should show collapsed image summary, not request bytes")
	}
	press(m, 'o', tea.ModCtrl)
	assertFrame(t, m)
	fetch := m.scanImages()
	if fetch == nil {
		t.Fatal("expanded image did not request its ref")
	}
	m.Update(fetch().(imageFetched))
	if frame := assertFrame(t, m); !strings.Contains(frame, "▀") {
		t.Fatal("missing pixel preview")
	}
	at := *settled.EntryID
	ref := sdk.ImageRef{ThreadID: thread.ID, EntryID: at}
	retrieved, err := client.GetImage(ctx, ref)
	if err != nil || !bytes.Equal(retrieved.Data, fitted) {
		t.Fatalf("fitted blob did not match committed model content: %v", err)
	}
	wrong, err := client.NewThread(ctx, sdk.NewThread{CWD: dir, Model: &sdk.Model{Provider: "fixture", ModelID: "text"}})
	if err != nil {
		t.Fatal(err)
	}
	ref.ThreadID = wrong.ID
	if _, err := client.GetImage(ctx, ref); !imageWireCode(err, "not_found") {
		t.Fatalf("cross-thread image scope: %v", err)
	}
	if _, err := client.Submit(ctx, sdk.Input{ThreadID: wrong.ID, RequestID: "text-only", Images: []sdk.Image{retrieved}}); !imageWireCode(err, "invalid_request") {
		t.Fatalf("text-only model admitted attachment: %v", err)
	}
	if _, err := client.LookupSubmission(ctx, wrong.ID, "text-only"); !imageWireCode(err, "not_found") {
		t.Fatalf("rejected attachment received a durable receipt: %v", err)
	}
	bad := sdk.Input{ThreadID: thread.ID, RequestID: "bad-image",
		Images: []sdk.Image{{MIMEType: "image/png", Data: []byte("not a png")}}}
	if _, err := client.Submit(ctx, bad); !imageWireCode(err, "invalid_request") {
		t.Fatalf("malformed image admitted: %v", err)
	}
	if _, err := client.LookupSubmission(ctx, thread.ID, bad.RequestID); !imageWireCode(err, "not_found") {
		t.Fatalf("malformed image has a receipt: %v", err)
	}
	duplicate := sdk.Input{ThreadID: thread.ID, RequestID: "replay-image", Text: "follow-up caption", Images: []sdk.Image{retrieved}}
	saved, err := client.Submit(ctx, duplicate)
	if err != nil {
		t.Fatal(err)
	}
	if done, err := client.WaitSubmission(ctx, thread.ID, saved.SubmissionID); err != nil || done.Status != "done" {
		t.Fatalf("captioned image did not settle: %+v %v", done, err)
	}
	fork, err := client.Fork(ctx, thread.ID, at)
	if err != nil {
		t.Fatal(err)
	}
	ref.ThreadID = fork.ID
	stop()
	stop = func() {}
	stop = boot()
	retrieved, err = client.GetImage(ctx, ref)
	if err != nil || !bytes.Equal(retrieved.Data, fitted) {
		t.Fatalf("fork ancestry did not resolve the same fitted bytes after restart: %v", err)
	}
	if again, err := client.Submit(ctx, duplicate); err != nil || again != saved {
		t.Fatalf("same-key image retry after restart: %+v %v, want %+v", again, err, saved)
	}
	if err := client.WaitIdle(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 { // no second provider call after the receipt replayed
		t.Fatalf("unexpected provider calls: retained %d, want 1", len(requests))
	}
}

func imageWireCode(err error, code string) bool {
	var wire *sdk.ServerError
	return errors.As(err, &wire) && wire.Code == code
}
