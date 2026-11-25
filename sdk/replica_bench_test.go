package sdk

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Each Durable partial commit (about ten a second per streaming thread)
// reaches the client as one patch that appends a token to the live answer.
var replicaCases = []struct{ entries, toolBytes int }{{10, 2 << 10}, {10, 16 << 10}, {100, 2 << 10}, {100, 16 << 10}}

const (
	replicaTarget      = 100 * time.Microsecond // per patch, sdk-replica
	replicaTargetBytes = 64 << 10
)

// replicaWindow is a subscribe result whose window cycles through a user
// prompt (200 B), an assistant answer (800 B) with a tool call, and a tool
// result of toolBytes, while a run streams an answer.
func replicaWindow(entries, toolBytes int) json.RawMessage {
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	prompt := strings.Repeat("Please review the cached handler. ", 7)[:200]
	answer := strings.Repeat("The handler reads the cached value before the request. ", 15)[:800]
	result := strings.Repeat("ok  github.com/javiermolinar/hush/sdk  0.7s\n", toolBytes/44+1)[:toolBytes]
	window := make([]any, entries)
	for i := range window {
		var message map[string]any
		switch i % 3 {
		case 0:
			message = map[string]any{"role": "user", "timestampMs": 1795435568245, "blocks": []any{text(prompt)}}
		case 1:
			message = map[string]any{"role": "assistant", "timestampMs": 1795435568245, "stopReason": "toolUse", "blocks": []any{text(answer),
				map[string]any{"type": "toolCall", "id": fmt.Sprint("call-", i), "name": "bash", "arguments": map[string]any{"command": "go test ./..."}}}}
		default:
			message = map[string]any{"role": "toolResult", "timestampMs": 1795435568245, "callId": fmt.Sprint("call-", i-1), "name": "bash",
				"isError": false, "blocks": []any{text(result)}}
		}
		window[i] = map[string]any{"id": fmt.Sprint(i + 1), "threadId": "1", "kind": "model", "messages": []any{message}}
	}
	state := map[string]any{
		"thread": map[string]any{"id": "1"}, "agent": map[string]any{},
		"usage":  map[string]any{"inputTokens": 0, "outputTokens": 0, "cacheReadTokens": 0, "cacheWriteTokens": 0, "totalTokens": 0, "costUsd": 0},
		"window": map[string]any{"beforeEntryId": nil, "lastEntryId": fmt.Sprint(entries), "entries": window},
		"tools":  []any{}, "queue": []any{}, "compactions": []any{},
		"run":       map[string]any{"taskId": "900", "submissionIds": []any{"901"}},
		"streaming": map[string]any{"attempt": 1, "blocks": []any{text("")}},
	}
	raw, err := json.Marshal(map[string]any{"subscriptionId": "bench-sub", "epoch": "bench-epoch", "rev": 0, "state": state})
	if err != nil {
		panic(err)
	}
	return raw
}

// tokenPatch appends one token to the streaming answer at rev.
func tokenPatch(rev uint64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"type":"patch","epoch":"bench-epoch","subscriptionId":"bench-sub","baseRev":%d,"nextRev":%d,"ops":[["a",["streaming","blocks",0,"text"]," token"]]}`, rev, rev+1))
}

func installWindow(tb testing.TB, raw json.RawMessage) *Replica {
	tb.Helper()
	var r Replica
	if err := r.Install(raw, "bench-epoch", "1"); err != nil {
		tb.Fatal(err)
	}
	return &r
}

func applyToken(tb testing.TB, r *Replica, patch json.RawMessage) {
	if changed, err := r.Apply(patch); !changed || err != nil {
		tb.Fatalf("patch not applied: %v", err)
	}
}

// BenchmarkReplicaApply reports ns/op, B/op and allocs/op for one-token
// streaming appends. The answer restarts every 512 patches so that results
// don't depend on the iteration count.
func BenchmarkReplicaApply(b *testing.B) {
	for _, c := range replicaCases {
		b.Run(fmt.Sprintf("window=%d/tool=%dKB", c.entries, c.toolBytes>>10), func(b *testing.B) {
			raw := replicaWindow(c.entries, c.toolBytes)
			patches := make([]json.RawMessage, 512)
			for i := range patches {
				patches[i] = tokenPatch(uint64(i))
			}
			r := installWindow(b, raw)
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				if i%len(patches) == 0 && i > 0 {
					b.StopTimer()
					r = installWindow(b, raw)
					b.StartTimer()
				}
				applyToken(b, r, patches[i%len(patches)])
			}
		})
	}
}

// TestReplicaApplyBudget runs the benchmark's cases in ordinary test runs so
// hosted CI reports them. The target is sdk-replica's; until it lands, a miss
// is reported rather than failed.
func TestReplicaApplyBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("timing is not measured in short/race runs")
	}
	for _, c := range replicaCases {
		t.Run(fmt.Sprintf("window=%d/tool=%dKB", c.entries, c.toolBytes>>10), func(t *testing.T) {
			const warmup, samples = 20, 200
			r := installWindow(t, replicaWindow(c.entries, c.toolBytes))
			patches := make([]json.RawMessage, warmup+samples)
			for i := range patches {
				patches[i] = tokenPatch(uint64(i))
			}
			for _, p := range patches[:warmup] {
				applyToken(t, r, p)
			}
			ds := make([]time.Duration, 0, samples)
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for _, p := range patches[warmup:] {
				start := time.Now()
				applyToken(t, r, p)
				ds = append(ds, time.Since(start))
			}
			runtime.ReadMemStats(&after)
			slices.Sort(ds)
			p99 := ds[len(ds)*99/100-1]
			bytes := (after.TotalAlloc - before.TotalAlloc) / samples
			allocs := (after.Mallocs - before.Mallocs) / samples
			t.Logf("p50 %s, p99 %s, %d B/op, %d allocs/op", ds[len(ds)/2-1], p99, bytes, allocs)
			if p99 > replicaTarget || bytes > replicaTargetBytes {
				reportPerfMiss(t, "sdk-replica", fmt.Sprintf("p99 %s, %d B/op; target %s, %d B/op", p99, bytes, replicaTarget, replicaTargetBytes))
			}
		})
	}
}

// reportPerfMiss records a missed target. A known miss names the plan step
// that fixes it and is reported until that step lands. Otherwise, misses
// fail on a quiet native host, the default, and are reported where
// HUSH_PERF_MODE=report (hosted CI).
func reportPerfMiss(t *testing.T, knownMiss, detail string) {
	t.Helper()
	switch {
	case knownMiss != "":
		t.Logf("performance target missed (known, until %s): %s", knownMiss, detail)
	case os.Getenv("HUSH_PERF_MODE") == "report":
		t.Logf("performance target missed (report-only): %s", detail)
	default:
		t.Fatalf("performance target missed: %s", detail)
	}
}
