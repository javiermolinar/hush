package sdk

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestCodeReviewNegotiationAndReceipts(t *testing.T) {
	handle := ReviewHandle{strings.Repeat("a", 32), "Reader"}
	selection := ReviewSelection{Model{Provider: "fixture", ModelID: "model"}, "high"}
	launch := ReviewLaunch{ThreadID: "7", RequestID: "exact", Scope: ReviewScope{Tracked: []string{"main.go"}},
		Assignments: []ReviewAssignment{{"Reader", "Inspect the dirty file"}}, Selection: &selection}
	for _, mode := range []string{"negotiated", "old host", "mismatched result"} {
		t.Run(mode, func(t *testing.T) {
			ctx, path := runContext(t)
			receipt := ReviewReceipt{ThreadID: launch.ThreadID, RequestID: launch.RequestID,
				SnapshotID: strings.Repeat("b", 64), Base: strings.Repeat("c", 40), Mode: "sequential",
				Selection: selection, Workers: []ReviewHandle{handle}}
			listener(t, path, func(c net.Conn, r *bufio.Reader) {
				q, err := readReq(r)
				if err != nil || q.Method != "hello" {
					return
				}
				caps := map[string]uint64{}
				if mode != "old host" {
					caps[delegationCapability] = DelegationVersion
				}
				reply(c, q, Hello{Version: 1, Epoch: "review-fixture", Capabilities: caps})
				for {
					q, err = readReq(r)
					if err != nil {
						return
					}
					if mode == "old host" || q.Params["threadId"] != "7" {
						t.Errorf("unexpected request: %+v", q)
						return
					}
					switch q.Method {
					case reviewStart:
						p, _ := q.Params["scope"].(map[string]any)
						if other, ok := p["untracked"].([]any); !ok || len(other) != 0 || q.Params["requestId"] != launch.RequestID {
							t.Errorf("incorrect native launch: %+v", q.Params)
						}
						reply(c, q, receipt)
					case reviewGet:
						worker := ReviewWorker{ReviewHandle: handle, State: "succeeded", Report: "Advisory", NoticeID: "15"}
						if mode == "mismatched result" {
							worker.Handle = strings.Repeat("e", 32)
						}
						reply(c, q, ReviewView{Receipt: receipt, Target: "stale", Workers: []ReviewWorker{worker}})
					case reviewStop:
						if q.Params["handle"] != handle.Handle {
							t.Errorf("incorrect stop scope: %+v", q.Params)
						}
						reply(c, q, map[string]any{"worker": ReviewWorker{ReviewHandle: handle, State: "stopping"}})
					default:
						t.Errorf("unexpected operation %s", q.Method)
					}
				}
			})
			c := NewClient(path, ClientOptions{Capabilities: map[string][]uint64{delegationCapability: {DelegationVersion}}})
			defer c.Close()
			got, err := c.StartCodeReview(ctx, launch)
			if mode == "old host" {
				var unavailable *ServerError
				if !errors.As(err, &unavailable) || unavailable.Code != "unsupported_capability" {
					t.Fatalf("old host: %v", err)
				}
				return
			}
			if err != nil || len(got.Workers) != 1 || got.Workers[0] != handle || got.Selection != selection {
				t.Fatalf("start: %+v, %v", got, err)
			}
			view, err := c.CodeReview(ctx, launch.ThreadID, launch.RequestID)
			if mode == "mismatched result" {
				if !errors.Is(err, ErrProtocol) {
					t.Fatalf("wrong handle: %v", err)
				}
				return
			}
			if err != nil || view.Target != "stale" || view.Workers[0].NoticeID != "15" {
				t.Fatalf("get: %+v, %v", view, err)
			}
			stop, err := c.StopReviewWorker(ctx, launch.ThreadID, handle.Handle)
			if err != nil || stop.State != "stopping" || stop.NoticeID != "" {
				t.Fatalf("stop: %+v, %v", stop, err)
			}
		})
	}
}
