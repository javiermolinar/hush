package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const ModelCatalogVersion = 1
const ModelCatalogSearchVersion = 2
const ModelCatalogDraftVersion = 3
const ModelCatalogLimit = 64
const modelCatalogQueryMaxBytes = 128
const modelCatalogCapability = "modelCatalog"
const modelCatalogList = "ext.modelCatalog.list"
const modelCatalogNew = "ext.modelCatalog.new"

type ModelCatalog struct {
	Current *Model            `json:"current"`
	Default Model             `json:"default"`
	Models  []ModelCatalogRow `json:"models"`
}

type ModelCatalogRow struct {
	Model
	Label          string   `json:"label"`
	Available      bool     `json:"available"`
	Reason         *string  `json:"reason"`
	ThinkingLevels []string `json:"thinkingLevels"`
}

func validModelCatalogVersion(version uint64) bool {
	return version == ModelCatalogVersion || version == ModelCatalogSearchVersion || version == ModelCatalogDraftVersion
}

func (c *Client) modelCatalogSession(ctx context.Context) (*session, error) {
	s, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	if !validModelCatalogVersion(s.hello.Capabilities[modelCatalogCapability]) {
		return nil, &ServerError{Code: "unsupported_capability", Capability: modelCatalogCapability, Message: "Native model catalog was not negotiated"}
	}
	return s, nil
}

func (c *Client) ModelCatalog(ctx context.Context, thread ID) (ModelCatalog, error) {
	if !thread.Valid() {
		return ModelCatalog{}, ErrProtocol
	}
	s, err := c.modelCatalogSession(ctx)
	if err != nil {
		return ModelCatalog{}, err
	}
	return readModelCatalog(ctx, s, thread, "")
}

// SearchModelCatalog applies a case-insensitive search before the host's row
// limit. It requires modelCatalog:2 or newer. An empty query requests the
// default page. An empty thread requests the draft catalog (modelCatalog:3).
// Each search uses a separate session so cancellation doesn't block other calls.
func (c *Client) SearchModelCatalog(ctx context.Context, thread ID, query string) (ModelCatalog, error) {
	if thread != "" && !thread.Valid() || len(query) > modelCatalogQueryMaxBytes || !cleanCatalogText(query) {
		return ModelCatalog{}, ErrProtocol
	}
	life, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.life, cancel)
	defer func() { stop(); cancel() }()
	s, err := c.dial(life, life, c.offered, nil)
	if err != nil {
		return ModelCatalog{}, err
	}
	defer s.fail(ErrClosed)
	version := s.hello.Capabilities[modelCatalogCapability]
	if version != ModelCatalogDraftVersion && (thread == "" || version != ModelCatalogSearchVersion) {
		return ModelCatalog{}, &ServerError{Code: "unsupported_capability", Capability: modelCatalogCapability, Message: "Native model catalog search for this scope was not negotiated"}
	}
	return readModelCatalog(life, s, thread, strings.TrimSpace(query))
}

func readModelCatalog(ctx context.Context, s *session, thread ID, query string) (ModelCatalog, error) {
	var catalog ModelCatalog
	params := struct {
		ThreadID ID     `json:"threadId,omitempty"`
		Query    string `json:"query,omitempty"`
	}{thread, query}
	_, err := s.call(ctx, modelCatalogList, params, func(raw any) error {
		data, err := json.Marshal(raw)
		if err != nil {
			return ErrProtocol
		}
		catalog, err = decodeModelCatalog(data, query != "")
		if thread == "" && catalog.Current != nil {
			return ErrProtocol
		}
		return err
	})
	if err != nil {
		return ModelCatalog{}, err
	}
	return catalog, nil
}

var catalogThinking = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// NewCatalogThread validates and stores a model and level together when
// creating a thread. It requires modelCatalog:3 and does not submit input.
func (c *Client) NewCatalogThread(ctx context.Context, cwd string, model Model, level string) (Thread, error) {
	if !utf8.ValidString(cwd) || utf8.RuneCountInString(cwd) > 4096 || !validCatalogModel(model) || !slices.Contains(catalogThinking, level) {
		return Thread{}, ErrProtocol
	}
	s, err := c.modelCatalogSession(ctx)
	if err != nil {
		return Thread{}, err
	}
	if s.hello.Capabilities[modelCatalogCapability] != ModelCatalogDraftVersion {
		return Thread{}, &ServerError{Code: "unsupported_capability", Capability: modelCatalogCapability, Message: "Thread creation with a model selection was not negotiated"}
	}
	params := struct {
		CWD           string `json:"cwd,omitempty"`
		Model         Model  `json:"model"`
		ThinkingLevel string `json:"thinkingLevel"`
	}{cwd, model, level}
	raw, err := s.call(ctx, modelCatalogNew, params, nil)
	if err != nil {
		return Thread{}, err
	}
	thread, err := decode[Thread](raw)
	if err != nil || !thread.ID.Valid() {
		return Thread{}, &TransportError{Err: ErrProtocol, Indeterminate: true}
	}
	return thread, nil
}

// ConfigureCatalogThread calls native thread.configure only on the same
// session that negotiated modelCatalog, never on a bare fallback connection.
// Nil model means to change only the thinking level.
func (c *Client) ConfigureCatalogThread(ctx context.Context, thread ID, model *Model, level string) error {
	if !thread.Valid() || !slices.Contains(catalogThinking, level) || model != nil && !validCatalogModel(*model) {
		return ErrProtocol
	}
	s, err := c.modelCatalogSession(ctx)
	if err != nil {
		return err
	}
	change := map[string]any{"thinkingLevel": level}
	if model != nil {
		change["model"] = *model
	}
	raw, err := s.call(ctx, "thread.configure", map[string]any{"threadId": thread, "change": change}, nil)
	if err != nil {
		return err
	}
	_, err = decode[struct{}](raw)
	return err
}

func cleanCatalogText(v string) bool {
	return utf8.ValidString(v) && !strings.ContainsFunc(v, func(r rune) bool {
		return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
	})
}

func catalogText(v string) bool {
	return len(v) <= 1024 && cleanCatalogText(v) && utf8.RuneCountInString(v) <= 256 && strings.TrimSpace(v) != ""
}

func validCatalogModel(v Model) bool { return catalogText(v.Provider) && catalogText(v.ModelID) }

func decodeModelCatalog(raw []byte, filtered bool) (ModelCatalog, error) {
	// Raw nullable fields distinguish null from an omitted member. The strict
	// decoder also rejects fields outside the public projection at any depth.
	var wire struct {
		Current json.RawMessage
		Default Model
		Models  []struct {
			Model
			Label          string
			Available      *bool
			Reason         json.RawMessage
			ThinkingLevels []string
		}
	}
	strict := func(data []byte, dst any) error {
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		return d.Decode(dst)
	}
	if strict(raw, &wire) != nil || len(wire.Current) == 0 || !validCatalogModel(wire.Default) || wire.Models == nil || len(wire.Models) > ModelCatalogLimit {
		return ModelCatalog{}, ErrProtocol
	}
	out := ModelCatalog{Default: wire.Default, Models: make([]ModelCatalogRow, 0, len(wire.Models))}
	if strict(wire.Current, &out.Current) != nil || out.Current != nil && !validCatalogModel(*out.Current) {
		return ModelCatalog{}, ErrProtocol
	}
	seen := make(map[Model]bool, len(wire.Models))
	for _, r := range wire.Models {
		var reason *string
		if !validCatalogModel(r.Model) || !catalogText(r.Label) || r.Available == nil || strict(r.Reason, &reason) != nil ||
			r.ThinkingLevels == nil || len(r.ThinkingLevels) > len(catalogThinking) || seen[r.Model] {
			return ModelCatalog{}, ErrProtocol
		}
		if *r.Available {
			if reason != nil || len(r.ThinkingLevels) == 0 {
				return ModelCatalog{}, ErrProtocol
			}
		} else if reason == nil || !(*reason == "missing_model" && len(r.ThinkingLevels) == 0 ||
			(*reason == "credentials_required" || *reason == "availability_unknown") && len(r.ThinkingLevels) > 0) {
			return ModelCatalog{}, ErrProtocol
		}
		for i, level := range r.ThinkingLevels {
			if !slices.Contains(catalogThinking, level) || slices.Contains(r.ThinkingLevels[:i], level) {
				return ModelCatalog{}, ErrProtocol
			}
		}
		seen[r.Model] = true
		out.Models = append(out.Models, ModelCatalogRow{r.Model, r.Label, *r.Available, reason, r.ThinkingLevels})
	}
	if !filtered && (!seen[out.Default] || out.Current != nil && !seen[*out.Current]) {
		return ModelCatalog{}, ErrProtocol
	}
	return out, nil
}
