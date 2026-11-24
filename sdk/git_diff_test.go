package sdk

import (
	"bufio"
	"errors"
	"net"
	"testing"
)

func TestGitDiffNegotiationAndScope(t *testing.T) {
	const revision = "129cb6d4-9d9b-4757-a711-b7b1fd839a5f"
	for _, mode := range []string{"negotiated", "unnegotiated", "wrong patch"} {
		t.Run(mode, func(t *testing.T) {
			ctx, path := runContext(t)
			listener(t, path, func(c net.Conn, r *bufio.Reader) {
				q, err := readReq(r)
				if err != nil || q.Method != "hello" {
					return
				}
				caps := map[string]uint64{}
				if mode != "unnegotiated" {
					caps[gitDiffCapability] = GitDiffVersion
				}
				reply(c, q, Hello{Version: 1, Epoch: "git-diff-fixture", Capabilities: caps})
				q, err = readReq(r)
				if mode == "unnegotiated" {
					if err == nil {
						t.Errorf("unnegotiated Git operation sent: %+v", q)
					}
					return
				}
				if err != nil || q.Params["threadId"] != "1" || len(q.Params) > 3 {
					t.Errorf("unexpected Git request: %+v, %v", q, err)
					return
				}
				switch q.Method {
				case gitDiffPrefix + "summary":
					if q.Params["range"] != GitDiffHead {
						t.Errorf("unexpected range %+v", q.Params)
					}
					branch := ""
					if mode == "negotiated" {
						branch = "feature/review"
					}
					reply(c, q, GitDiffSummary{Revision: revision, Range: GitDiffHead, Commit: "1111111111111111111111111111111111111111",
						Branch: branch, Files: []GitDiffFile{{Path: "src/main.go", Status: "M", Text: true}}})
				case gitDiffPrefix + "file":
					if q.Params["revision"] != revision || q.Params["file"] != float64(0) {
						t.Errorf("unexpected file selector %+v", q.Params)
					}
					patch := GitDiffPatch{Revision: revision, File: 0, Lines: []GitDiffLine{{Kind: "add", NewLine: 10, Text: "updated"}}}
					if mode == "wrong patch" {
						patch.File++
					}
					reply(c, q, patch)
				default:
					t.Errorf("unexpected operation %s", q.Method)
				}
			})
			client := NewClient(path, ClientOptions{Capabilities: map[string][]uint64{gitDiffCapability: {GitDiffVersion}}})
			defer client.Close()
			summary, err := client.GitDiffSummary(ctx, "1", GitDiffHead)
			if mode == "unnegotiated" {
				var server *ServerError
				if !errors.As(err, &server) || server.Code != "unsupported_capability" {
					t.Fatalf("unnegotiated summary: %v", err)
				}
				return
			}
			if err != nil || len(summary.Files) != 1 || summary.Files[0].Path != "src/main.go" {
				t.Fatalf("summary: %+v %v", summary, err)
			}
			if mode == "negotiated" && summary.Branch != "feature/review" || mode == "wrong patch" && summary.Branch != "" {
				t.Fatalf("summary branch: %q", summary.Branch)
			}
			patch, err := client.GitDiffFile(ctx, "1", summary.Revision, 0)
			if mode == "wrong patch" {
				if !errors.Is(err, ErrProtocol) {
					t.Fatalf("accepted patch for another file: %+v %v", patch, err)
				}
			} else if err != nil || len(patch.Lines) != 1 || patch.Lines[0].NewLine != 10 {
				t.Fatalf("patch: %+v %v", patch, err)
			}
		})
	}
}
