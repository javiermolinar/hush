package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
)

func TestStreamFromBackend(t *testing.T) {
	bundle := os.Getenv("HUSH_TEST_BACKEND")
	if bundle == "" {
		t.Skip("set HUSH_TEST_BACKEND to the built backend bundle")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	firstToken := make(chan struct{})
	release := make(chan struct{})
	sse := func(w http.ResponseWriter, delta string, finish any) {
		payload, _ := json.Marshal(map[string]any{"id": "stream-fixture", "object": "chat.completion.chunk", "created": 1, "model": "fixture",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": delta}, "finish_reason": finish}}})
		_, _ = io.WriteString(w, "data: "+string(payload)+"\n\n")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("content-type", "text/event-stream")
		w.(http.Flusher).Flush()
		select {
		case <-firstToken:
		case <-r.Context().Done():
			return
		case <-ctx.Done():
			return
		}
		sse(w, "A live answer", nil)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		case <-ctx.Done():
			return
		}
		sse(w, " from the backend.", nil)
		sse(w, "", "stop")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	data, err := os.MkdirTemp("/tmp", "hush-stream-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(data)
	for name, body := range map[string]string{
		"settings.json": `{"defaultProvider":"fixture","defaultModel":"fixture","retry":{"enabled":false},"compaction":{"enabled":false}}`,
		"auth.json":     `{"fixture":{"type":"api_key","key":"local-test-only"}}`,
		"models.json":   `{"providers":{"fixture":{"baseUrl":"` + server.URL + `/v1","api":"openai-completions","models":[{"id":"fixture","name":"Fixture","reasoning":false,"contextWindow":20000,"maxTokens":500}]}}}`,
	} {
		if err := os.WriteFile(filepath.Join(data, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	proc := exec.CommandContext(ctx, "node", bundle, "--profile", "pi", "--data-dir", data, "--model", "fixture/fixture")
	proc.Dir = data
	proc.Env = append(os.Environ(), "HOME="+data, "PI_CODING_AGENT_DIR="+data, "NODE_NO_WARNINGS=1")
	proc.Stderr = os.Stderr
	stdout, err := proc.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = proc.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = proc.Wait() }()
	scan := bufio.NewScanner(stdout)
	if !scan.Scan() || !strings.Contains(scan.Text(), `"ready":true`) {
		t.Fatalf("startup: %s %v", scan.Text(), scan.Err())
	}
	client := sdk.NewClient(filepath.Join(data, "hush.sock"), sdk.ClientOptions{})
	defer client.Close()
	thread, err := client.NewThread(ctx, sdk.NewThread{CWD: data})
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(ctx, client, Options{ThreadID: thread.ID, ReducedMotion: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	feed := m.beginFeed()().(feedMsg)
	m.receiveFeed(feed)
	state := m.threads[m.active]
	m.editor.set("Observe a live response")
	result := m.sendPrompt(sdk.RejectBusy)().(workMsg)
	if result.err != nil {
		t.Fatal(result.err)
	}
	m.finishWork(result)
	wait := func(f func() bool) {
		t.Helper()
		for !f() {
			msg, ok := awaitFeed(m.feed, feed.source)().(feedMsg)
			if !ok || !msg.ok || msg.update.Err != nil {
				t.Fatalf("missing streamed state: %v", ctx.Err())
			}
			m.receiveFeed(msg)
		}
	}
	wait(func() bool {
		return state.activeRun && strings.Contains(ansi.Strip(m.View().Content), "Waiting for model")
	})
	if state.activity.thinking {
		t.Fatal("waiting for first content was labeled as thinking")
	}
	close(firstToken)
	wait(func() bool {
		return state.stream.Present && strings.Contains(ansi.Strip(m.View().Content), "A live answer")
	})
	if strings.Contains(ansi.Strip(m.View().Content), "Waiting for model") {
		t.Fatal("the wait row outlived the first text")
	}
	if state.lastAnswer != "" {
		t.Fatal("partial stream was treated as a committed answer")
	}
	close(release)
	wait(func() bool { return !state.stream.Present && state.lastAnswer == "A live answer from the backend." })
}
