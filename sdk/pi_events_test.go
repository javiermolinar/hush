package sdk

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
)

const streamOne = "9c199a48-e042-49ca-b5c7-bc8a2a690901"
const streamTwo = "9c199a48-e042-49ca-b5c7-bc8a2a690902"

func piReady(thread ID, epoch string) PiReady {
	return PiReady{ThreadID: thread, Epoch: epoch, Header: PiHeader{
		Type: "session", Version: 3, ID: epoch, Timestamp: "2026-11-22T21:00:00.000Z", CWD: "/fixture",
	}, Services: []string{}}
}

func piGreeting(c net.Conn, r *bufio.Reader, installed bool, t *testing.T) bool {
	q, err := readReq(r)
	if err != nil || q.Method != "hello" {
		return false
	}
	if q.Params["capabilities"].(map[string]any)["piEvents"].([]any)[0] != float64(1) {
		t.Error("client did not offer the pinned Pi event schema")
		return false
	}
	caps := map[string]uint64{}
	if installed {
		caps["piEvents"] = 1
	}
	reply(c, q, Hello{Version: 1, Epoch: "core_epoch", Capabilities: caps})
	return true
}

func event(c net.Conn, thread, epoch string, seq int, typ string) {
	_ = json.NewEncoder(c).Encode(map[string]any{"type": "pi_event", "threadId": thread, "epoch": epoch, "seq": seq,
		"event": map[string]any{"type": typ}})
}

func piCmd(t *testing.T, text string) PiCommand {
	t.Helper()
	cmd, err := ParsePiCommand([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestPiReadyOrdersEventReplyAndExplicitRebind(t *testing.T) {
	ctx, path := runContext(t)
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		if !piGreeting(c, r, true, t) {
			return
		}
		q, err := readReq(r)
		if err != nil || q.Method != piOpen {
			t.Error("missing open")
			return
		}
		reply(c, q, piReady("1", streamOne))
		q, err = readReq(r)
		if err != nil || q.Method != piCommand || q.Params["epoch"] != streamOne {
			t.Error("wrong initial fence")
			return
		}
		event(c, "1", streamOne, 1, "agent_start")
		id := "same"
		reply(c, q, map[string]any{"response": PiResponse{Type: "response", Command: "new_session", ID: &id, Success: true,
			Data: json.RawMessage(`{"cancelled":false}`)}, "ready": piReady("2", streamTwo)})
		q, err = readReq(r)
		if err != nil || q.Params["epoch"] != streamTwo {
			t.Error("request after switch raced the ready fence")
			return
		}
		event(c, "2", streamTwo, 1, "agent_start")
		reply(c, q, map[string]any{"response": PiResponse{Type: "response", Command: "get_state", ID: &id, Success: true}})
	})
	client := NewClient(path, ClientOptions{})
	defer client.Close()
	p, got, err := client.OpenPi(ctx, PiOptions{CWD: "/fixture"})
	if err != nil || got.ThreadID != "1" || got.Epoch != streamOne || got.Seq != 0 {
		t.Fatalf("fence=%+v / %v", got, err)
	}
	defer p.Close()
	if err := p.Send(ctx, piCmd(t, `{"type":"new_session","id":"same"}`)); err != nil {
		t.Fatal(err)
	}
	if err := p.Send(ctx, piCmd(t, `{"type":"get_state","id":"same"}`)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		row, err := p.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		switch i {
		case 0:
			if row.Event == nil || row.Event.ThreadID != "1" {
				t.Fatal(row)
			}
		case 1:
			if row.Response == nil || row.Response.Command != "new_session" || row.Ready == nil || row.Ready.ThreadID != "2" {
				t.Fatal(row)
			}
		case 2:
			if row.Event == nil || row.Event.Epoch != streamTwo || row.Event.Seq != 1 {
				t.Fatal(row)
			}
		case 3:
			if row.Response == nil || row.Response.Command != "get_state" || *row.Response.ID != "same" {
				t.Fatal(row)
			}
		}
	}
}

func TestPiNoReplayOnGapOverflowRestartAndIndeterminateReply(t *testing.T) {
	for _, trouble := range []string{"gap", "duplicate", "epoch", "wrong-thread", "overflow", "socket", "uncorrelated", "missing-switch-ready"} {
		t.Run(trouble, func(t *testing.T) {
			ctx, path := runContext(t)
			var connections, commands atomic.Int32
			listener(t, path, func(c net.Conn, r *bufio.Reader) {
				connections.Add(1)
				if !piGreeting(c, r, true, t) {
					return
				}
				q, err := readReq(r)
				if err != nil || q.Method != piOpen {
					return
				}
				reply(c, q, piReady("1", streamOne))
				q, err = readReq(r)
				if err != nil || q.Method != piCommand {
					return
				}
				commands.Add(1)
				event(c, "1", streamOne, 1, "agent_start")
				switch trouble {
				case "gap":
					event(c, "1", streamOne, 3, "turn_start")
				case "duplicate":
					event(c, "1", streamOne, 1, "turn_start")
				case "epoch":
					event(c, "1", streamTwo, 2, "turn_start")
				case "wrong-thread":
					event(c, "2", streamOne, 2, "turn_start")
				case "overflow":
					_ = json.NewEncoder(c).Encode(map[string]any{"type": "pi_end", "threadId": "1", "epoch": streamOne, "reason": "overflow"})
				case "uncorrelated":
					bad := "wrong"
					reply(c, q, map[string]any{"response": PiResponse{Type: "response", Command: "prompt", ID: &bad, Success: true}})
				case "missing-switch-ready":
					id := "correct"
					reply(c, q, map[string]any{"response": PiResponse{Type: "response", Command: "clone", ID: &id, Success: true}})
				}
			})
			client := NewClient(path, ClientOptions{})
			defer client.Close()
			p, _, err := client.OpenPi(ctx, PiOptions{CWD: "/fixture"})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			cmd := `{"type":"prompt","id":"correct","message":"must not replay"}`
			if trouble == "missing-switch-ready" {
				cmd = `{"type":"clone","id":"correct"}`
			}
			err = p.Send(ctx, piCmd(t, cmd))
			if trouble != "missing-switch-ready" && err != nil {
				t.Fatal(err)
			}
			if trouble == "missing-switch-ready" && err == nil {
				t.Fatal("successful switch without a new ready fence")
			}
			first, err := p.Next(ctx)
			if err != nil || first.Event == nil || first.Event.Seq != 1 {
				t.Fatalf("verified prefix: %+v / %v", first, err)
			}
			_, err = p.Next(ctx)
			var lost *TransportError
			if !errors.As(err, &lost) || !lost.Indeterminate {
				t.Fatalf("loss: %v", err)
			}
			if trouble == "overflow" && !strings.Contains(err.Error(), "overflow") {
				t.Fatal(err)
			}
			if err := p.Send(ctx, piCmd(t, `{"type":"abort"}`)); err == nil {
				t.Fatal("lost stream reused")
			}
			if connections.Load() != 1 || commands.Load() != 1 {
				t.Fatal("implicitly reconnected or replayed")
			}
		})
	}
}

func TestPiAbsentCapabilityNeverOpens(t *testing.T) {
	ctx, path := runContext(t)
	observed := make(chan error, 1)
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		if piGreeting(c, r, false, t) {
			_, err := readReq(r)
			observed <- err
		}
	})
	client := NewClient(path, ClientOptions{})
	defer client.Close()
	_, _, err := client.OpenPi(ctx, PiOptions{CWD: "/fixture"})
	if !IsCode(err, "unsupported_capability") {
		t.Fatal(err)
	}
	select {
	case err := <-observed:
		if !errors.Is(err, io.EOF) {
			t.Fatal("server got input without negotiation", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestPiLFOnlyAndInvalidCorrelationFields(t *testing.T) {
	r := NewPiLineReader(strings.NewReader("{\"type\":\"steer\",\"id\":\"\",\"message\":\"a\u2028b\u2029c\"}\r\n{\"type\":\"get_state\"}"))
	line, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	cmd := piCmd(t, string(line))
	if cmd.ID == nil || *cmd.ID != "" {
		t.Fatal("empty id was silently omitted")
	}
	if _, err := r.Read(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("unterminated record: %v", err)
	}
	for _, record := range []string{`{"type":"get_state","id":null}`, `{"type":"prompt","id":"a","id":"b"}`, `{"type":"prompt","message":"\ud800"}`} {
		if _, err := ParsePiCommand([]byte(record)); err == nil {
			t.Fatalf("accepted invalid record %s", record)
		}
	}
}
