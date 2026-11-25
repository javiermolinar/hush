package sdk

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
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
	if v, err := c.WorkspaceFileScope(ctx, WorkspaceFileTarget{ThreadID: "7"}); err != nil || !v.Workspace || v.Home {
		t.Fatalf("scope=%+v; %v", v, err)
	}
	if v, err := c.ListWorkspaceFiles(ctx, WorkspaceFileTarget{ThreadID: "7"}, "workspace", "nested space", 4, ""); err != nil || len(v.Entries) != 1 || v.Entries[0].Name != "file with space.md" {
		t.Fatalf("files=%+v; %v", v, err)
	}
	if v, err := c.ListWorkspaceSkills(ctx, "7"); err != nil || len(v) != 1 || v[0].Name != "review" {
		t.Fatalf("skills=%+v; %v", v, err)
	}
	if _, err := c.LoadedContext(ctx, "7"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("malformed context accepted: %v", err)
	}
}

func TestLoadedContextSourceVersions(t *testing.T) {
	appendFile := InstructionSource{Kind: "personal", Path: "/home/.hush/APPEND_SYSTEM.md"}
	hush := InstructionSource{Kind: "personal", Path: "/home/.hush/AGENTS.md"}
	shared := InstructionSource{Kind: "personal", Path: "/home/.agents/AGENTS.md"}
	workspace := InstructionSource{Kind: "workspace", Path: "/work/AGENTS.md"}
	hidden := InstructionSource{Kind: "workspace", Path: "/work/.agents/AGENTS.md"}
	for _, tc := range []struct {
		name    string
		version uint64
		sources []InstructionSource
		valid   bool
	}{
		{"v1 fallback", LoadedContextVersion, []InstructionSource{appendFile, workspace}, true},
		{"v1 cannot report personal AGENTS", LoadedContextVersion, []InstructionSource{hush, workspace}, false},
		{"v2 all locations", LoadedContextAgentsVersion, []InstructionSource{appendFile, hush, shared, workspace, hidden}, true},
		{"v2 no addendum", LoadedContextAgentsVersion, []InstructionSource{hush, shared, workspace}, true},
		{"v2 personal after workspace", LoadedContextAgentsVersion, []InstructionSource{workspace, shared}, false},
		{"v2 late addendum", LoadedContextAgentsVersion, []InstructionSource{hush, appendFile, workspace}, false},
		{"v2 duplicate path", LoadedContextAgentsVersion, []InstructionSource{workspace, hidden, hidden}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, socket := runContext(t)
			listener(t, socket, func(c net.Conn, r *bufio.Reader) {
				q, err := readReq(r)
				if err != nil || q.Method != "hello" {
					return
				}
				reply(c, q, Hello{Version: 1, Epoch: "loaded-context-fixture", Capabilities: map[string]uint64{loadedContextCapability: tc.version}})
				q, err = readReq(r)
				if err != nil || q.Method != loadedContextGet || q.Params["threadId"] != "7" {
					t.Errorf("unexpected loaded context request: %+v (%v)", q, err)
					return
				}
				reply(c, q, map[string]any{"sources": tc.sources})
				_, _ = r.ReadByte()
			})
			c := NewClient(socket, ClientOptions{Capabilities: map[string][]uint64{loadedContextCapability: {LoadedContextAgentsVersion, LoadedContextVersion}}})
			defer c.Close()
			got, err := c.LoadedContext(ctx, "7")
			if tc.valid {
				if err != nil || !slices.Equal(got, tc.sources) {
					t.Fatalf("context=%+v; %v", got, err)
				}
			} else if !errors.Is(err, ErrProtocol) {
				t.Fatalf("malformed context accepted: %+v; %v", got, err)
			}
		})
	}
}

func TestWorkspaceFileSearchNegotiationAndValidation(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		version uint64
		path    string
		valid   bool
	}{
		{"v1 does not search", 1, "", false},
		{"relative path", 2, "tempo.fork/modules/generator/generator.go", true},
		{"dot path", 2, "repo/../generator.go", false},
		{"home path", 2, "~/generator.go", false},
		{"github paths need v3", 2, ".github/workflows/generator.go", false},
		{"v3 github workflow", 3, ".github/workflows/generator.go", true},
		{"v3 nested github", 3, "repo/.github/generator.go", false},
		{"v3 hidden file", 3, ".github/.generator.go", false},
		{"v3 github must be a directory", 3, ".github", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, socket := runContext(t)
			listener(t, socket, func(c net.Conn, r *bufio.Reader) {
				q, err := readReq(r)
				if err != nil || q.Method != "hello" {
					return
				}
				reply(c, q, Hello{Version: 1, Epoch: "file-search-fixture", Capabilities: map[string]uint64{workspaceFilesCapability: scenario.version}})
				q, err = readReq(r)
				if err != nil {
					return
				}
				if scenario.version < 2 || q.Method != workspaceFileSearchMethod || q.Params["threadId"] != "7" || q.Params["query"] != "generator.go" {
					t.Errorf("unexpected file search: %+v", q)
				}
				reply(c, q, WorkspaceFileSearchResult{Matches: []WorkspaceFileMatch{{Path: scenario.path}}, Partial: true})
				_, _ = r.ReadByte()
			})
			c := NewClient(socket, ClientOptions{Capabilities: map[string][]uint64{workspaceFilesCapability: {3, 2, 1}}})
			defer c.Close()
			got, err := c.SearchWorkspaceFiles(ctx, WorkspaceFileTarget{ThreadID: "7"}, "generator.go", 10)
			switch {
			case scenario.valid:
				if err != nil || !got.Partial || len(got.Matches) != 1 || got.Matches[0].Path != scenario.path {
					t.Fatalf("search=%+v; %v", got, err)
				}
			case scenario.version == 1:
				if !IsCode(err, "unsupported_capability") {
					t.Fatalf("search on v1: %v", err)
				}
			default:
				if !errors.Is(err, ErrProtocol) {
					t.Fatalf("accepted malformed path: %+v, %v", got, err)
				}
			}
		})
	}
}

// A prospective thread requires workspaceFiles:2 or later.
// A v1 host never receives the request.
func TestWorkspaceFileProspectiveTarget(t *testing.T) {
	const cwd = "/work space/repo"
	for _, version := range []uint64{WorkspaceFilesVersion, WorkspaceFileSearchVersion, WorkspaceFileProjectPathsVersion} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			ctx, socket := runContext(t)
			var calls atomic.Int32
			listener(t, socket, func(c net.Conn, r *bufio.Reader) {
				q, err := readReq(r)
				if err != nil || q.Method != "hello" {
					return
				}
				reply(c, q, Hello{Version: 1, Epoch: "prospective-fixture", Capabilities: map[string]uint64{workspaceFilesCapability: version}})
				q, err = readReq(r)
				if err != nil {
					return
				}
				calls.Add(1)
				if _, thread := q.Params["threadId"]; thread || q.Params["cwd"] != cwd {
					t.Errorf("prospective request carried the wrong target: %+v", q)
				}
				switch q.Method {
				case workspaceFilesPrefix + "scope":
					reply(c, q, map[string]bool{"workspace": true, "home": false})
				case workspaceFilesPrefix + "list":
					dir := q.Params["directory"]
					file := WorkspaceFileEntry{Name: "README.md"}
					if version == WorkspaceFileProjectPathsVersion && dir == "" {
						file = WorkspaceFileEntry{Name: ".github", Directory: true}
					} else if version == WorkspaceFileProjectPathsVersion && dir == ".github/workflows" {
						file = WorkspaceFileEntry{Name: "workflow.yaml"}
					} else if dir != "" {
						t.Errorf("unexpected directory on v%d: %q", version, dir)
					}
					reply(c, q, map[string]any{"entries": []WorkspaceFileEntry{file}})
				case workspaceFileSearchMethod:
					reply(c, q, WorkspaceFileSearchResult{Matches: []WorkspaceFileMatch{{Path: "docs/README.md"}}})
				default:
					t.Errorf("unexpected method %q", q.Method)
				}
				_, _ = r.ReadByte()
			})
			c := NewClient(socket, ClientOptions{Capabilities: map[string][]uint64{workspaceFilesCapability: {3, 2, 1}}})
			defer c.Close()
			target := WorkspaceFileTarget{CWD: cwd}
			scope, scopeErr := c.WorkspaceFileScope(ctx, target)
			page, listErr := c.ListWorkspaceFiles(ctx, target, "workspace", "", 10, "")
			found, searchErr := c.SearchWorkspaceFiles(ctx, target, "readme", 10)
			if version == WorkspaceFilesVersion {
				for _, err := range []error{scopeErr, listErr, searchErr} {
					if !IsCode(err, "unsupported_capability") {
						t.Fatalf("v1 accepted a prospective target: %v", err)
					}
				}
				if calls.Load() != 0 {
					t.Fatal("a v1 host received a prospective request")
				}
				return
			}
			if scopeErr != nil || listErr != nil || searchErr != nil || !scope.Workspace || len(page.Entries) != 1 || len(found.Matches) != 1 {
				t.Fatalf("scope=%+v %v; page=%+v %v; search=%+v %v", scope, scopeErr, page, listErr, found, searchErr)
			}
			workflows, err := c.ListWorkspaceFiles(ctx, target, "workspace", ".github/workflows", 10, "")
			if version == WorkspaceFileProjectPathsVersion {
				if page.Entries[0] != (WorkspaceFileEntry{Name: ".github", Directory: true}) || err != nil ||
					len(workflows.Entries) != 1 || workflows.Entries[0].Name != "workflow.yaml" {
					t.Fatalf("github listing %+v: %v; root %+v", workflows, err, page)
				}
			} else if !IsCode(err, "unsupported_capability") {
				t.Fatalf("v%d accepted a hidden directory: %v", version, err)
			}
		})
	}
}

func TestWorkspaceFileTargetValidation(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), "absent.sock"), ClientOptions{})
	defer c.Close()
	for _, target := range []WorkspaceFileTarget{{}, {ThreadID: "7", CWD: "/repo"}, {ThreadID: "x"}, {CWD: "repo"},
		{CWD: "/repo\x00name"}, {CWD: "/repo\u0085name"}, {CWD: "/" + strings.Repeat("a", 4096)}} {
		if _, err := c.WorkspaceFileScope(t.Context(), target); !errors.Is(err, ErrProtocol) {
			t.Fatalf("%+v: %v", target, err) // rejected before dialing
		}
	}
}

func TestWorkspaceSkillDraftNegotiation(t *testing.T) {
	for _, version := range []uint64{WorkspaceSkillsVersion, WorkspaceSkillDraftVersion} {
		t.Run(map[uint64]string{1: "thread only", 2: "draft capable"}[version], func(t *testing.T) {
			ctx, socket := runContext(t)
			listener(t, socket, func(c net.Conn, r *bufio.Reader) {
				q, err := readReq(r)
				if err != nil {
					return
				}
				reply(c, q, Hello{Version: 1, Epoch: "draft-skill", Capabilities: map[string]uint64{workspaceSkillsCapability: version}})
				q, err = readReq(r)
				if err != nil {
					return
				}
				if version == WorkspaceSkillsVersion || q.Method != workspaceSkillsList || len(q.Params) != 1 || q.Params["cwd"] != "/backend/path with spaces" {
					t.Errorf("unexpected draft lookup: %+v", q)
				}
				reply(c, q, map[string]any{"skills": []WorkspaceSkill{{Name: "review", Token: strings.Repeat("B", 80), Source: "workspace", Description: "Review"}}})
			})
			c := NewClient(socket, ClientOptions{Capabilities: map[string][]uint64{workspaceSkillsCapability: {WorkspaceSkillDraftVersion, WorkspaceSkillsVersion}}})
			defer c.Close()
			got, err := c.PreviewWorkspaceSkills(ctx, "/backend/path with spaces")
			if version == WorkspaceSkillsVersion {
				if !IsCode(err, "unsupported_capability") {
					t.Fatalf("legacy host accepted draft preview: %v", err)
				}
			} else if err != nil || len(got) != 1 || got[0].Name != "review" {
				t.Fatalf("preview=%+v; %v", got, err)
			}
		})
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
