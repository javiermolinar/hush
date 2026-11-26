package sdk

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWorkerActivityRebaselinesAfterConnectionLoss(t *testing.T) {
	ctx, path := runContext(t)
	rows := []ReviewWorkerActivity{{ReviewHandle: ReviewHandle{strings.Repeat("a", 32), "Reader"}, State: "running", Activity: "Read pinned api.go"}}
	first := strings.Repeat("a", 64)
	receivedFirst, receivedReset, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(done)
	var peers atomic.Int32
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		peer := peers.Add(1)
		hello, err := readReq(r)
		if err != nil || hello.Method != "hello" {
			t.Errorf("hello: %+v, %v", hello, err)
			return
		}
		reply(c, hello, Hello{Version: 1, Epoch: "fixture", Capabilities: map[string]uint64{delegationCapability: DelegationVersion, workerActivityCapability: WorkerActivityVersion}})
		q, err := readReq(r)
		if err != nil || q.Method != workerActivityWait || q.Params["threadId"] != "7" || len(q.Params) != 1 {
			t.Errorf("baseline requested with invalid scope or a resume cursor: %+v, %v", q, err)
			return
		}
		if peer == 1 {
			reply(c, q, map[string]any{"threadId": "7", "cursor": first, "workers": rows})
			q, err = readReq(r)
			if err != nil || q.Method != workerActivityWait || q.Params["after"] != first {
				t.Errorf("missing conditional wait: %+v, %v", q, err)
			}
			select {
			case <-receivedFirst:
			case <-done:
			}
			return // lost transport with an outstanding conditional request
		}
		complete := []ReviewWorkerActivity{{ReviewHandle: rows[0].ReviewHandle, State: "succeeded"}}
		reply(c, q, map[string]any{"threadId": "7", "cursor": strings.Repeat("b", 64), "workers": complete})
		q, err = readReq(r)
		if err == nil {
			select {
			case <-receivedReset:
			case <-done:
				return
			}
			// Duplicate handles must not replace the good baseline.
			reply(c, q, map[string]any{"threadId": "7", "cursor": strings.Repeat("c", 64), "workers": append(complete, complete[0])})
		}
	})
	c := NewClient(path, ClientOptions{Capabilities: map[string][]uint64{delegationCapability: {DelegationVersion}, workerActivityCapability: {WorkerActivityVersion}}})
	defer c.Close()
	watch := c.WatchReviewActivity(ctx, "7")
	if update := <-watch; update.Err != nil || len(update.Workers) != 1 || update.Workers[0] != rows[0] {
		t.Fatalf("first baseline: %+v", update)
	}
	close(receivedFirst)
	var retained bool
	for update := range watch {
		if errors.Is(update.Err, ErrProtocol) {
			if !retained {
				t.Fatal("did not observe a coherent rebaseline before the rejected duplicate")
			}
			return
		}
		if update.Err == nil {
			retained = len(update.Workers) == 1 && update.Workers[0].Handle == rows[0].Handle && update.Workers[0].State == "succeeded"
			if !retained {
				t.Fatalf("wrong rebaseline: %+v", update)
			}
			close(receivedReset)
		}
	}
	t.Fatal("watch never rejected duplicate handles")
}

func TestReviewEvidenceAndActivityDoNotFallBackOnOldHost(t *testing.T) {
	ctx, path := runContext(t)
	var ops atomic.Int32
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		hello, err := readReq(r)
		if err != nil || hello.Method != "hello" {
			return
		}
		reply(c, hello, Hello{Version: 1, Epoch: "legacy", Capabilities: map[string]uint64{delegationCapability: DelegationVersion}})
		if q, err := readReq(r); err == nil {
			ops.Add(1)
			t.Errorf("unsupported operation reached host: %v", q)
		}
	})
	c := NewClient(path, ClientOptions{Capabilities: map[string][]uint64{delegationCapability: {DelegationVersion},
		workerActivityCapability: {WorkerActivityVersion}, reviewEvidenceCapability: {ReviewEvidenceVersion}}})
	defer c.Close()
	q := ReviewLaunch{ThreadID: "7", RequestID: "check", Assignments: []ReviewAssignment{{"Reader", "Review"}}, Scope: ReviewScope{Tracked: []string{"api.go"}},
		Assessment: &ReviewAssessment{Checks: []ReviewCheck{{"tests", "Read the test excerpt"}}}}
	if _, err := c.StartCodeReview(ctx, q); !IsCode(err, "unsupported_capability") {
		t.Fatal(err)
	}
	if _, err := c.InspectReviewWorker(ctx, "7", strings.Repeat("a", 32), true); !IsCode(err, "unsupported_capability") {
		t.Fatal(err)
	}
	if update := <-c.WatchReviewActivity(ctx, "7"); !IsCode(update.Err, "unsupported_capability") {
		t.Fatal(update.Err)
	}
	if ops.Load() != 0 {
		t.Fatal("optional requests were not fenced by hello")
	}
}
