package sdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type wireReq struct {
	ID, Method string
	Params     map[string]any
}

func readReq(r *bufio.Reader) (wireReq, error) {
	var q wireReq
	line, err := readRecord(r)
	if err != nil {
		return q, err
	}
	return q, json.Unmarshal(line, &q)
}

func reply(c net.Conn, q wireReq, result any) {
	frame := map[string]any{"type": "response", "id": q.ID, "method": q.Method, "ok": true, "result": result}
	if e, ok := result.(*ServerError); ok {
		delete(frame, "result")
		frame["ok"], frame["error"] = false, e
	}
	_ = json.NewEncoder(c).Encode(frame)
}

func listener(t *testing.T, name string, handle func(net.Conn, *bufio.Reader)) {
	t.Helper()
	l, err := net.Listen("unix", name)
	if err != nil {
		t.Fatal(err)
	}
	var writers sync.WaitGroup
	ended := make(chan struct{})
	var peers sync.Map
	go func() {
		defer close(ended)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			peers.Store(c, struct{}{})
			writers.Go(func() {
				defer peers.Delete(c)
				defer c.Close()
				handle(c, bufio.NewReader(c))
			})
		}
	}()
	t.Cleanup(func() {
		l.Close()
		<-ended
		peers.Range(func(c, _ any) bool { c.(net.Conn).Close(); return true })
		writers.Wait()
	})
}

func runContext(t *testing.T) (context.Context, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	t.Cleanup(cancel)
	dir, err := os.MkdirTemp("/tmp", "hsg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return ctx, filepath.Join(dir, "sock")
}

func greeting(c net.Conn, r *bufio.Reader, epoch string) bool {
	q, err := readReq(r)
	if err != nil || q.Method != "hello" {
		return false
	}
	reply(c, q, Hello{Version: 1, Epoch: epoch, Capabilities: map[string]uint64{}})
	return true
}

func TestCustomDialCoversDedicatedConnections(t *testing.T) {
	ctx, path := runContext(t)
	dialErr := errors.New("fixture dial failed")
	var calls atomic.Int32
	c := NewClient(path, ClientOptions{DialUnix: func(ctx context.Context, socket string) (net.Conn, error) {
		calls.Add(1)
		if socket != path || ctx.Err() != nil {
			t.Errorf("unexpected dial target or context: %q %v", socket, ctx.Err())
		}
		return nil, dialErr
	}})
	defer c.Close()

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if msg := <-c.Watch(watchCtx, "1", Window{}); !errors.Is(msg.Err, dialErr) {
		t.Fatalf("state watch bypassed custom dial: %v", msg.Err)
	}
	if msg := <-c.WatchIntro(watchCtx, IntroScope{}); !errors.Is(msg.Err, dialErr) {
		t.Fatalf("intro watch bypassed custom dial: %v", msg.Err)
	}
	cancel()
	_, helloErr := c.Hello(ctx)
	_, catalogErr := c.PiCatalog(ctx)
	for _, err := range []error{helloErr, catalogErr, c.WaitIdle(ctx, "1")} {
		var transport *TransportError
		if !errors.Is(err, dialErr) || !errors.As(err, &transport) || transport.Indeterminate {
			t.Fatalf("error should be a pre-admission dial error: %v", err)
		}
	}
	if calls.Load() < 5 {
		t.Fatalf("only %d custom dial calls", calls.Load())
	}
}

func TestTransportRetriesOnlySameKeyAndSeparatesWaits(t *testing.T) {
	ctx, path := runContext(t)
	var admits, attempts, continues atomic.Int32
	var answerReady atomic.Bool
	var stopWait atomic.Bool
	waitEntered, waitingGone := make(chan struct{}), make(chan struct{})
	var cancelled sync.Once
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		if !greeting(c, r, "scripted") {
			return
		}
		for {
			q, err := readReq(r)
			if err != nil {
				return
			}
			switch q.Method {
			case "submit":
				if q.Params["requestId"] != "retry-key" || q.Params["text"] != "one prompt" || q.Params["threadId"] != "1" {
					t.Error("submission retry changed payload")
					return
				}
				if attempts.Add(1) == 1 {
					admits.Add(1)
					return // durable admission without a wire reply
				}
				reply(c, q, Receipt{"7"})
			case "continue":
				continues.Add(1)
				return
			case "thread.wait":
				answerReady.Store(true)
				reply(c, q, struct{}{})
			case "submission.get":
				placed := ID("8")
				reply(c, q, Submission{ThreadID: "1", SubmissionID: "7", Status: "placed", EntryID: &placed})
			case "submission.wait":
				if !stopWait.Load() {
					cancelled.Do(func() { close(waitEntered) })
					_, _ = io.Copy(io.Discard, r)
					close(waitingGone)
					return
				}
				if !answerReady.Load() {
					t.Error("settlement must be independently enabled in fixture")
					return
				}
				placed, answer := ID("8"), ID("10")
				reply(c, q, Submission{ThreadID: "1", SubmissionID: "7", Status: "done", EntryID: &placed, AnswerEntryID: &answer})
			case "thread.list":
				reply(c, q, ThreadPage{Threads: []Thread{{ID: "1"}}})
			default:
				t.Errorf("unexpected method %s", q.Method)
				return
			}
		}
	})
	c := NewClient(path, ClientOptions{Capabilities: map[string][]uint64{"piEvents": {1}}})
	defer c.Close()
	if err := c.RequireCapability(ctx, "piEvents", 1); !IsCode(err, "unsupported_capability") {
		t.Fatal(err)
	}
	receipt, err := c.Submit(ctx, Input{ThreadID: "1", RequestID: "retry-key", Text: "one prompt"})
	if err != nil || receipt.SubmissionID != "7" || attempts.Load() != 2 || admits.Load() != 1 {
		t.Fatalf("%+v %v attempts=%d admits=%d", receipt, err, attempts.Load(), admits.Load())
	}
	lookup, err := c.LookupSubmission(ctx, "1", "retry-key")
	if err != nil || lookup.Terminal() || lookup.EntryID == nil {
		t.Fatal(lookup, err)
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 1)
	go func() { _, err := c.WaitSubmission(waitCtx, "1", "7"); errs <- err }()
	select {
	case <-waitEntered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := c.WaitIdle(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-waitingGone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stopWait.Store(true)
	done, err := c.WaitSubmission(ctx, "1", "7")
	if err != nil || !done.Terminal() || *done.AnswerEntryID != "10" {
		t.Fatal(done, err)
	}
	_, err = c.Continue(ctx, "1")
	var tx *TransportError
	if !errors.As(err, &tx) || !tx.Indeterminate {
		t.Fatal(err)
	}
	if _, err := c.ListThreads(ctx, 1, ""); err != nil {
		t.Fatal(err)
	}
	if continues.Load() != 1 {
		t.Fatal("unkeyed request repeated")
	}
}

func entry(i int) Entry {
	return Entry{ID: ID(strconv.Itoa(i + 100)), ThreadID: "1", Kind: "fixture", Messages: []Message{}}
}

func stateFor(thread ID, rows ...Entry) State {
	if rows == nil {
		rows = []Entry{}
	}
	var last *ID
	if len(rows) > 0 {
		last = &rows[len(rows)-1].ID
	}
	return State{Thread: Thread{ID: thread}, Window: TranscriptWindow{LastEntryID: last, Entries: rows},
		Tools: []Tool{}, Queue: []QueueItem{}, Compactions: []Compaction{}}
}

func receive(t *testing.T, ctx context.Context, updates <-chan StateUpdate, match func(StateUpdate) bool) StateUpdate {
	t.Helper()
	for {
		select {
		case u, ok := <-updates:
			if !ok {
				t.Fatal("feed closed unexpectedly")
			}
			if match(u) {
				return u
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestWatchResnapshotAndIndependentLargeGap(t *testing.T) {
	ctx, path := runContext(t)
	var epoch atomic.Int32
	epoch.Store(1)
	var subscriptions atomic.Int32
	endFeed := make(chan struct{})
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		current := fmt.Sprintf("boot-%d", epoch.Load())
		if !storeGreeting(t, c, r, current, fixtureStore, nil) {
			return
		}
		for {
			q, err := readReq(r)
			if err != nil {
				return
			}
			switch q.Method {
			case "subscribe":
				thread := ID(q.Params["threadId"].(string))
				sid := fmt.Sprintf("feed-%d", subscriptions.Add(1))
				if thread == "2" {
					reply(c, q, Snapshot{SubscriptionID: sid, Epoch: current, State: stateFor(thread)})
					continue
				}
				if current == "boot-1" {
					reply(c, q, Snapshot{SubscriptionID: sid, Epoch: current, State: stateFor(thread, entry(0))})
					<-endFeed
					_ = json.NewEncoder(c).Encode(map[string]any{
						"type": "state_end", "subscriptionId": sid, "epoch": current, "reason": "overflow",
					})
					return
				}
				reply(c, q, Snapshot{SubscriptionID: sid, Epoch: current, State: stateFor(thread, entry(249))})
				_ = json.NewEncoder(c).Encode(map[string]any{
					"type": "patch", "subscriptionId": sid, "epoch": current, "baseRev": 0, "nextRev": 1,
					"ops": [][]any{{"s", []string{"agent", "cwd"}, "/new"}},
				})
			case "history":
				range_ := q.Params["range"].(map[string]any)
				lim := int(q.Params["limit"].(float64))
				lower, upper := 99, 349
				if range_["kind"] == "cursor" {
					n, _ := strconv.Atoi(strings.TrimPrefix(range_["cursor"].(string), "cursor-"))
					lower = n
				} else {
					if s, ok := range_["afterEntryId"].(string); ok {
						lower, _ = strconv.Atoi(s)
					}
					upper, _ = strconv.Atoi(range_["throughEntryId"].(string))
				}
				page := HistoryPage{ThreadID: "1", Entries: []Entry{}}
				end := min(upper, lower+lim)
				for i := lower + 1; i <= end; i++ {
					page.Entries = append(page.Entries, entry(i-100))
				}
				if end < upper {
					more := fmt.Sprintf("cursor-%d", end)
					page.NextCursor = &more
				}
				reply(c, q, page)
			default:
				t.Errorf("unexpected %s", q.Method)
				return
			}
		}
	})
	defer func() {
		select {
		case <-endFeed:
		default:
			close(endFeed)
		}
	}()
	c := NewClient(path, ClientOptions{})
	defer c.Close()
	stateCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	updates := c.Watch(stateCtx, "1", Window{Min: 1, Max: 2})
	old := receive(t, ctx, updates, func(u StateUpdate) bool { return u.Snapshot.Epoch == "boot-1" }).Snapshot
	tr := NewTranscript("1")
	if err := tr.Sync(ctx, c, old); err != nil {
		t.Fatal(err)
	}
	if tr.Through() != "100" {
		t.Fatal(tr.Through())
	}
	epoch.Store(2)
	close(endFeed)
	receive(t, ctx, updates, func(u StateUpdate) bool { return errors.Is(u.Err, ErrStale) })
	fresh := receive(t, ctx, updates, func(u StateUpdate) bool { return u.Snapshot.Epoch == "boot-2" && u.Snapshot.Rev == 1 }).Snapshot
	if fresh.State.Agent.CWD != "/new" || fresh.SubscriptionID == old.SubscriptionID {
		t.Fatal(fresh)
	}
	if err := tr.MergeWindow(fresh); err != nil {
		t.Fatal(err)
	}
	if tr.Through() != "100" || len(tr.Entries()) != 2 {
		t.Fatal("window advanced history anchor")
	}
	if err := tr.Sync(ctx, c, fresh); err != nil {
		t.Fatal(err)
	}
	if entries := tr.Entries(); len(entries) != 250 || entries[0].ID != "100" || tr.Through() != "349" {
		t.Fatal("missing or duplicate history through", tr.Through(), len(entries))
	}
	cancel()
	next := c.Watch(ctx, "2", Window{})
	if last := receive(t, ctx, next, func(u StateUpdate) bool { return u.Snapshot.State.Thread.ID == "2" }); last.Snapshot.State.Window.LastEntryID != nil {
		t.Fatal("thread history crossed a switch")
	}
}
