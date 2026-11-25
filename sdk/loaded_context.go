package sdk

import (
	"context"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	LoadedContextVersion       = 1
	LoadedContextAgentsVersion = 2
	loadedContextCapability    = "loadedContext"
	loadedContextGet           = "ext.loadedContext.get"
)

// InstructionSource is metadata for the next resolved prompt, not retained
// prompt bytes. Path is on the backend host, even on an explicit socket.
type InstructionSource struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

func (c *Client) LoadedContext(ctx context.Context, thread ID) ([]InstructionSource, error) {
	if !thread.Valid() {
		return nil, ErrProtocol
	}
	out, version, err := waitOnVersionedCapability[struct {
		Sources []InstructionSource `json:"sources"`
	}](ctx, c, loadedContextGet,
		map[string]ID{"threadId": thread}, loadedContextCapability, LoadedContextAgentsVersion, LoadedContextVersion)
	if err != nil {
		return nil, err
	}
	// Up to 64 checkout ancestors, with a second file in .agents/ on v2.
	maxWorkspace, maxPersonalAgents := 64, 0
	if version == LoadedContextAgentsVersion {
		maxWorkspace, maxPersonalAgents = 128, 2
	}
	if out.Sources == nil || len(out.Sources) > maxWorkspace+maxPersonalAgents+1 {
		return nil, ErrProtocol
	}
	seen := make(map[string]bool, len(out.Sources))
	workspace, personalAgents := 0, 0
	for i, source := range out.Sources {
		if !path.IsAbs(source.Path) || path.Clean(source.Path) != source.Path || len(source.Path) > 4096 ||
			!utf8.ValidString(source.Path) || strings.IndexFunc(source.Path, unicode.IsControl) >= 0 || seen[source.Path] {
			return nil, ErrProtocol
		}
		name := path.Base(source.Path)
		switch source.Kind {
		case "personal":
			if i == 0 && name == "APPEND_SYSTEM.md" {
				break
			}
			if name != "AGENTS.md" || workspace != 0 || personalAgents >= maxPersonalAgents {
				return nil, ErrProtocol
			}
			personalAgents++
		case "workspace":
			workspace++
			if name != "AGENTS.md" || workspace > maxWorkspace {
				return nil, ErrProtocol
			}
		default:
			return nil, ErrProtocol
		}
		seen[source.Path] = true
	}
	return out.Sources, nil
}
