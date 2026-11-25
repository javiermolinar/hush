package sdk

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGitDiffWatchResnapshotsAndCancelsOnReconnect(t *testing.T) {
	ctx, path := runContext(t)
	life, cancel := context.WithCancel(ctx)
	defer cancel()
	first, changed, reboot := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	waiting, released := make(chan struct{}), make(chan struct{})
	var peers atomic.Int32
	negotiated := map[string]uint64{gitDiffCapability: 1, gitDiffWatchCapability: 1, storeIdentityCap: 1}
	listener(t, path, func(conn net.Conn, r *bufio.Reader) {
		n := peers.Add(1)
		q, err := readReq(r)
		if err != nil || q.Method != "hello" {
			t.Errorf("hello: %+v %v", q, err)
			return
		}
		epoch := "first-boot"
		if n > 1 {
			epoch = "next-boot"
		}
		reply(conn, q, Hello{Version: 1, Epoch: epoch, Capabilities: negotiated})
		q, err = readReq(r)
		if err != nil || q.Method != storeIdentityGet {
			t.Errorf("store proof: %+v %v", q, err)
			return
		}
		reply(conn, q, map[string]string{"storeId": "3fd57e76-e28a-46a8-ba8e-50053c3348b1", "generation": "3fd57e76-e28a-46a8-ba8e-50053c3348b2"})
		read := func(after string) bool {
			q, err = readReq(r)
			if err != nil || q.Method != gitDiffWaitMethod || q.Params["threadId"] != "4" ||
				after == "" && len(q.Params) != 1 || after != "" && q.Params["after"] != after {
				t.Errorf("Git watch after %q: %+v %v", after, q, err)
				return false
			}
			return true
		}
		if !read("") {
			return
		}
		summary := GitDiffSummary{Revision: "3fd57e76-e28a-46a8-ba8e-50053c3348b3", Range: GitDiffHead,
			Commit: strings.Repeat("1", 40), Files: []GitDiffFile{}}
		if n == 1 {
			reply(conn, q, map[string]any{"revision": first, "error": map[string]string{"code": "not_found", "message": "No such record"}})
			if !read(first) {
				return
			}
			reply(conn, q, map[string]any{"revision": changed, "summary": summary})
			_ = read(changed) // close with an after-bound request pending
			return
		}
		if n != 2 {
			t.Errorf("unexpected extra peer %d", n)
			return
		}
		reply(conn, q, map[string]any{"revision": reboot, "summary": summary})
		if read(reboot) {
			close(waiting)
			if _, err := readReq(r); err == nil {
				t.Error("cancel did not close the watch session")
			}
			close(released)
		}
	})
	c := NewClient(path, ClientOptions{Capabilities: map[string][]uint64{gitDiffCapability: {1}, gitDiffWatchCapability: {1}}})
	defer c.Close()
	out := c.WatchGitDiff(life, "4")
	next := func() GitDiffUpdate {
		t.Helper()
		select {
		case v, ok := <-out:
			if !ok {
				t.Fatal("Git watch closed")
			}
			return v
		case <-ctx.Done():
			t.Fatal("Git watch timed out")
			return GitDiffUpdate{}
		}
	}
	// The channel coalesces snapshots. The wire fixture proves that the
	// first observed error still advanced the subscription's cursor.
	for {
		v := next()
		if v.Revision == reboot {
			if v.Err != nil || v.Summary.Files == nil {
				t.Fatalf("reboot result: %+v", v)
			}
			break
		}
		if v.Revision == first && !IsCode(v.Err, "not_found") {
			t.Fatalf("unexpected watched error: %+v", v)
		}
	}
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("replacement wait didn't start")
	}
	cancel()
	select {
	case <-released:
	case <-ctx.Done():
		t.Fatal("peer did not see cancellation")
	}
}

func TestWorkspaceScopeWatchChecksNegotiationAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		base   uint64
		result map[string]any
	}{
		{"no prospective scope in v1", 1, nil},
		{"scope without explicit home field", 2, map[string]any{"revision": strings.Repeat("1", 64), "workspace": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, path := runContext(t)
			listener(t, path, func(conn net.Conn, r *bufio.Reader) {
				q, err := readReq(r)
				if err != nil || q.Method != "hello" {
					t.Errorf("hello: %+v %v", q, err)
					return
				}
				reply(conn, q, Hello{Version: 1, Epoch: "scope", Capabilities: map[string]uint64{workspaceFilesCapability: tc.base, workspaceScopeWatchCapability: 1}})
				q, err = readReq(r)
				if tc.result == nil {
					if err == nil {
						t.Errorf("ineligible watch reached server: %+v", q)
					}
					return
				}
				if err != nil || q.Method != workspaceScopeWaitMethod || q.Params["cwd"] != "/fixture" || len(q.Params) != 1 {
					t.Errorf("scope watch: %+v %v", q, err)
					return
				}
				reply(conn, q, tc.result)
			})
			c := NewClient(path, ClientOptions{Capabilities: map[string][]uint64{workspaceFilesCapability: {1, 2}, workspaceScopeWatchCapability: {1}}})
			defer c.Close()
			out := c.WatchWorkspaceFileScope(ctx, WorkspaceFileTarget{CWD: "/fixture"})
			select {
			case v := <-out:
				if tc.result == nil && !IsCode(v.Err, "unsupported_capability") || tc.result != nil && !errors.Is(v.Err, ErrProtocol) {
					t.Fatalf("watch accepted incompatible peer: %+v", v)
				}
			case <-ctx.Done():
				t.Fatal("watch didn't fail")
			}
		})
	}
}
