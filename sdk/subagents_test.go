package sdk

import (
	"bufio"
	"context"
	"errors"

	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSubagentOverviewReplacementAfterReconnect(t *testing.T) {
	ctx, path := runContext(t)
	seen, done := make(chan struct{}), make(chan struct{})
	defer close(done)
	cursor := strings.Repeat("a", 64)
	workers := []Subagent{{ID: "8", Title: "Check input", CWD: "/repo", Status: "waiting", ReadOnly: true, Usage: Usage{TotalTokens: 80}}}
	var peers atomic.Int32
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		n := peers.Add(1)
		hello, err := readReq(r)
		if err != nil || hello.Method != "hello" {
			t.Errorf("hello: %+v, %v", hello, err)
			return
		}
		reply(c, hello, Hello{Version: 1, Epoch: "fixture", Capabilities: map[string]uint64{subagentsCapability: SubagentOverviewVersion}})
		q, err := readReq(r)
		if err != nil || q.Method != subagentOverview || q.Params["threadId"] != "7" || len(q.Params) != 1 {
			t.Errorf("expected thread-scoped baseline: %+v, %v", q, err)
			return
		}
		if n == 1 {
			reply(c, q, map[string]any{"threadId": "7", "cursor": cursor, "workers": workers, "usage": Usage{TotalTokens: 110}})
			q, err = readReq(r)
			if err != nil || q.Method != subagentOverview || q.Params["after"] != cursor {
				t.Errorf("expected a conditional wait: %+v, %v", q, err)
			}
			select {
			case <-seen:
			case <-ctx.Done():
			}
			return
		}
		row := workers[0]
		row.Status, row.Text = "ok", "Found the missing default."
		reply(c, q, map[string]any{"threadId": "7", "cursor": strings.Repeat("b", 64), "workers": []Subagent{row}, "usage": Usage{TotalTokens: 110}})
		_, _ = readReq(r)
		select {
		case <-done:
		case <-ctx.Done():
		}
	})
	c := NewClient(path, ClientOptions{Capabilities: map[string][]uint64{subagentsCapability: {SubagentOverviewVersion}}})
	defer c.Close()
	watch := c.WatchSubagents(ctx, "7")
	if v := <-watch; v.Err != nil || len(v.Workers) != 1 || v.Workers[0] != workers[0] || v.Usage.TotalTokens != 110 {
		t.Fatalf("first baseline: %+v", v)
	}
	close(seen)
	for v := range watch {
		if v.Err != nil {
			continue
		}
		if len(v.Workers) != 1 || v.Workers[0].Status != "ok" || v.Workers[0].Text == "" || v.Usage.TotalTokens != 110 {
			t.Fatalf("replacement baseline: %+v", v)
		}
		return
	}
	t.Fatal("no overview after reconnect")
}

func TestSubagentOverviewRetriesSlowBaseline(t *testing.T) {
	ctx, path := runContext(t)
	var peers atomic.Int32
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		n := peers.Add(1)
		hello, err := readReq(r)
		if err != nil {
			return
		}
		reply(c, hello, Hello{Version: 1, Epoch: "fixture", Capabilities: map[string]uint64{subagentsCapability: SubagentOverviewVersion}})
		q, err := readReq(r)
		if err != nil {
			return
		}
		if q.Method != subagentOverview || q.Params["threadId"] != "7" || len(q.Params) != 1 {
			t.Errorf("expected fresh baseline: %+v", q)
			return
		}
		if n != 1 {
			reply(c, q, map[string]any{"threadId": "7", "cursor": strings.Repeat("c", 64), "workers": []Subagent{}, "usage": Usage{TotalTokens: 10}})
		}
		_, _ = readReq(r) // first peer never replies to the baseline
	})
	c := NewClient(path, ClientOptions{Capabilities: map[string][]uint64{subagentsCapability: {SubagentOverviewVersion}}})
	defer c.Close()
	watch := c.watchSubagents(ctx, "7", 250*time.Millisecond)
	if v := <-watch; !errors.Is(v.Err, context.DeadlineExceeded) {
		t.Fatalf("expected timed-out baseline, got %+v", v)
	}
	if v, ok := <-watch; !ok || v.Err != nil || v.Usage.TotalTokens != 10 {
		t.Fatalf("no replacement baseline: %+v", v)
	}
}
