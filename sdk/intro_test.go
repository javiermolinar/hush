package sdk

import (
	"bufio"
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
)

func TestIntroWatchRebindsAfterTransportLossAndCancels(t *testing.T) {
	ctx, path := runContext(t)
	life, cancel := context.WithCancel(ctx)
	defer cancel()
	offer := ClientOptions{Capabilities: map[string][]uint64{"intro": {IntroVersion}}}
	var peers atomic.Int32
	waiting, released, drop := make(chan struct{}), make(chan struct{}), make(chan struct{})
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		q, err := readReq(r)
		if err != nil || q.Method != "hello" {
			return
		}
		peer := peers.Add(1)
		epoch := "after-reconnect"
		if peer == 1 {
			epoch = "before-reconnect"
		}
		reply(c, q, Hello{Version: 1, Epoch: epoch, Capabilities: map[string]uint64{"intro": 1, storeIdentityCap: 1}})
		q, err = readReq(r)
		if err != nil || q.Method != storeIdentityGet {
			t.Errorf("store proof request: %+v, %v", q, err)
			return
		}
		reply(c, q, map[string]string{"storeId": "50585983-c167-459d-9c4b-492d3da95284", "generation": "50585983-c167-459d-9c4b-492d3da95285"})
		q, err = readReq(r)
		if err != nil || q.Method != introGet || q.Params["threadId"] != "2" || len(q.Params) != 1 {
			t.Errorf("scoped first request (no old revision): %+v, %v", q, err)
			return
		}
		rev := uint64(4)
		if peer > 1 {
			rev = 1 // the watermark is scoped to this backend epoch
		}
		reply(c, q, IntroReport{Revision: rev, StartedAtMS: 1, CWD: "/fixture", Sessions: IntroSessions{Open: 1, Quiet: 1},
			Skills: []string{"review"}, Attention: []IntroAttention{}})
		q, err = readReq(r)
		if err != nil || q.Method != introGet || q.Params["after"] != float64(rev) {
			t.Errorf("watch request: %+v, %v", q, err)
			return
		}
		if peer == 1 {
			select {
			case <-drop:
			case <-ctx.Done():
			}
			return
		}
		if peer != 2 {
			t.Error("unexpected extra intro peer")
			return
		}
		close(waiting)
		if _, err := readReq(r); err == nil {
			t.Error("cancellation did not close the passive watch connection")
		}
		close(released)
	})
	client := NewClient(path, offer)
	defer client.Close()
	stream := client.WatchIntro(life, IntroScope{ThreadID: "2"})
	next := func() IntroUpdate {
		t.Helper()
		select {
		case v, ok := <-stream:
			if !ok {
				t.Fatal("intro stream closed")
			}
			return v
		case <-life.Done():
			t.Fatal(life.Err())
			return IntroUpdate{}
		}
	}
	if got := next(); got.Err != nil || got.Report.Revision != 4 {
		t.Fatalf("initial report: %+v", got)
	}
	close(drop)
	for {
		v := next()
		if v.Err == nil {
			if v.Report.Revision != 1 || len(v.Report.Skills) != 1 {
				t.Fatalf("new epoch report: %+v", v)
			}
			break
		}
	}
	select {
	case <-waiting:
	case <-life.Done():
		t.Fatal("passive report did not start")
	}
	cancel()
	select {
	case <-released:
	case <-ctx.Done():
		t.Fatal("passive report did not release the connection")
	}
}

func TestIntroRejectsUnnegotiatedAndMalformedReports(t *testing.T) {
	for _, tc := range []string{"unnegotiated", "malformed"} {
		t.Run(tc, func(t *testing.T) {
			ctx, path := runContext(t)
			listener(t, path, func(c net.Conn, r *bufio.Reader) {
				q, err := readReq(r)
				if err != nil || q.Method != "hello" {
					return
				}
				caps := map[string]uint64{}
				if tc == "malformed" {
					caps["intro"] = 1
				}
				reply(c, q, Hello{Version: 1, Epoch: "intro", Capabilities: caps})
				q, err = readReq(r)
				if err == nil {
					if tc == "unnegotiated" {
						t.Error("SDK called an unnegotiated method")
					}
					reply(c, q, map[string]any{"skills": []string{"not a report"}})
				}
			})
			c := NewClient(path, ClientOptions{Capabilities: map[string][]uint64{"intro": {1}}})
			defer c.Close()
			_, err := c.Intro(ctx, IntroScope{})
			if tc == "unnegotiated" && !IsCode(err, "unsupported_capability") || tc == "malformed" && !errors.Is(err, ErrProtocol) {
				t.Fatalf("unexpected %s response: %v", tc, err)
			}
		})
	}
}
