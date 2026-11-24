package sdk

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestWorkspaceMetadataNegotiationAndValidation(t *testing.T) {
	ctx, socket := runContext(t)
	caps := map[string][]uint64{workspaceFilesCapability: {1}, workspaceSkillsCapability: {1}, loadedContextCapability: {1}}
	listener(t, socket, func(c net.Conn, r *bufio.Reader) {
		q, err := readReq(r)
		if err != nil || q.Method != "hello" {
			return
		}
		reply(c, q, Hello{Version: 1, Epoch: "metadata-fixture", Capabilities: map[string]uint64{
			workspaceFilesCapability: 1, workspaceSkillsCapability: 1, loadedContextCapability: 1,
		}})
		q, err = readReq(r)
		if err != nil {
			return
		}
		if q.Params["threadId"] != "7" {
			t.Errorf("metadata request escaped thread scope: %+v", q)
		}
		switch q.Method {
		case workspaceFilesPrefix + "scope":
			reply(c, q, map[string]bool{"workspace": true, "home": false})
		case workspaceFilesPrefix + "list":
			if q.Params["directory"] != "nested space" || q.Params["root"] != "workspace" {
				t.Errorf("bad file request: %+v", q)
			}
			reply(c, q, map[string]any{"entries": []WorkspaceFileEntry{{Name: "file with space.md"}}})
		case workspaceSkillsList:
			reply(c, q, map[string]any{"skills": []WorkspaceSkill{{Name: "review", Token: strings.Repeat("A", 80), Source: "workspace", Description: "A fixture skill"}}})
		case loadedContextGet:
			// The SDK refuses data that is not metadata, even from a negotiated peer.
			reply(c, q, map[string]any{"sources": []any{map[string]string{"path": "/remote/AGENTS.md", "kind": "workspace", "text": "content"}}})
		default:
			t.Errorf("unexpected method %q", q.Method)
		}
		_, _ = r.ReadByte()
	})
	c := NewClient(socket, ClientOptions{Capabilities: caps})
	defer c.Close()
	if v, err := c.WorkspaceFileScope(ctx, "7"); err != nil || !v.Workspace || v.Home {
		t.Fatalf("scope=%+v; %v", v, err)
	}
	if v, err := c.ListWorkspaceFiles(ctx, "7", "workspace", "nested space", 4, ""); err != nil || len(v.Entries) != 1 || v.Entries[0].Name != "file with space.md" {
		t.Fatalf("files=%+v; %v", v, err)
	}
	if v, err := c.ListWorkspaceSkills(ctx, "7"); err != nil || len(v) != 1 || v[0].Name != "review" {
		t.Fatalf("skills=%+v; %v", v, err)
	}
	if _, err := c.LoadedContext(ctx, "7"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("malformed context accepted: %v", err)
	}
}

func TestStagedSkillSubmitDoesNotDowngrade(t *testing.T) {
	for _, capable := range []bool{false, true} {
		t.Run(map[bool]string{false: "older host", true: "supported"}[capable], func(t *testing.T) {
			ctx, socket := runContext(t)
			pin := strings.Repeat("A", 80)
			listener(t, socket, func(c net.Conn, r *bufio.Reader) {
				q, err := readReq(r)
				if err != nil {
					return
				}
				caps := map[string]uint64{clientEnvironmentCapability: 1}
				if capable {
					caps[workspaceSkillsCapability] = 1
				}
				reply(c, q, Hello{Version: 1, Epoch: "skill-fixture", Capabilities: caps})
				q, err = readReq(r)
				if err != nil {
					return
				}
				if !capable || q.Method != workspaceSkillsSubmit || q.Params["token"] != pin || q.Params["environment"] == nil {
					t.Errorf("unpinned or changed skill request: %+v", q)
				}
				input, _ := q.Params["input"].(map[string]any)
				if input["text"] != "/skill:review instructions" || input["token"] != nil {
					t.Errorf("wrong core input: %+v", input)
				}
				reply(c, q, Receipt{SubmissionID: "3"})
				_, _ = r.ReadByte()
			})
			c := NewClient(socket, ClientOptions{Capabilities: map[string][]uint64{workspaceSkillsCapability: {1}, clientEnvironmentCapability: {1}}})
			defer c.Close()
			got, err := c.Submit(ctx, Input{ThreadID: "7", RequestID: "key", Text: "/skill:review instructions", SkillToken: pin, Environment: ClientEnvironment{OS: "linux"}})
			if capable && (err != nil || got.SubmissionID != "3") || !capable && !IsCode(err, "unsupported_capability") {
				t.Fatalf("receipt=%+v; %v", got, err)
			}
		})
	}
}
