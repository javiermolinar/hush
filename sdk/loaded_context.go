package sdk

import (
	"context"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	LoadedContextVersion    = 1
	loadedContextCapability = "loadedContext"
	loadedContextGet        = "ext.loadedContext.get"
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
	out, err := waitOnCapability[struct {
		Sources []InstructionSource `json:"sources"`
	}](ctx, c, loadedContextGet,
		map[string]ID{"threadId": thread}, loadedContextCapability, LoadedContextVersion)
	if err != nil {
		return nil, err
	}
	if out.Sources == nil || len(out.Sources) > 65 {
		return nil, ErrProtocol
	}
	seen := make(map[string]bool, len(out.Sources))
	for i, source := range out.Sources {
		if !path.IsAbs(source.Path) || path.Clean(source.Path) != source.Path || len(source.Path) > 4096 ||
			!utf8.ValidString(source.Path) || strings.IndexFunc(source.Path, unicode.IsControl) >= 0 || seen[source.Path] {
			return nil, ErrProtocol
		}
		switch source.Kind {
		case "personal":
			if i != 0 || path.Base(source.Path) != "APPEND_SYSTEM.md" {
				return nil, ErrProtocol
			}
		case "workspace":
			if path.Base(source.Path) != "AGENTS.md" {
				return nil, ErrProtocol
			}
		default:
			return nil, ErrProtocol
		}
		seen[source.Path] = true
	}
	return out.Sources, nil
}
