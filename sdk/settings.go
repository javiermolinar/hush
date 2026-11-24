package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
)

const SettingsVersion = 1
const settingsCapability = "settings"
const settingsGetMethod = "ext.settings.get"
const settingsSetMethod = "ext.settings.set"

type ReasoningMode string

const (
	ReasoningFolded   ReasoningMode = "folded"
	ReasoningExpanded ReasoningMode = "expanded"
)

type SettingsSnapshot struct {
	Model         Model         `json:"model"`
	ThinkingLevel string        `json:"thinkingLevel"`
	Reasoning     ReasoningMode `json:"reasoning"`
}

// SettingsChange replaces only the fields that are set. Model defaults apply
// to newly created threads; Reasoning controls transcript presentation.
type SettingsChange struct {
	Model         *Model
	ThinkingLevel string
	Reasoning     ReasoningMode
}

func validReasoningMode(v ReasoningMode) bool {
	return v == ReasoningFolded || v == ReasoningExpanded
}

func (c *Client) settingsSession(ctx context.Context) (*session, error) {
	s, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	if s.hello.Capabilities[settingsCapability] != SettingsVersion {
		return nil, &ServerError{Code: "unsupported_capability", Capability: settingsCapability, Message: "Settings were not negotiated"}
	}
	return s, nil
}

func (c *Client) Settings(ctx context.Context) (SettingsSnapshot, error) {
	s, err := c.settingsSession(ctx)
	if err != nil {
		return SettingsSnapshot{}, err
	}
	return readSettings(ctx, s, settingsGetMethod, struct{}{})
}

func (c *Client) ConfigureSettings(ctx context.Context, change SettingsChange) (SettingsSnapshot, error) {
	if change.Model == nil && change.ThinkingLevel == "" && change.Reasoning == "" ||
		change.Model != nil && !validCatalogModel(*change.Model) ||
		change.ThinkingLevel != "" && !slices.Contains(catalogThinking, change.ThinkingLevel) ||
		change.Reasoning != "" && !validReasoningMode(change.Reasoning) {
		return SettingsSnapshot{}, ErrProtocol
	}
	s, err := c.settingsSession(ctx)
	if err != nil {
		return SettingsSnapshot{}, err
	}
	params := struct {
		Model         *Model        `json:"model,omitempty"`
		ThinkingLevel string        `json:"thinkingLevel,omitempty"`
		Reasoning     ReasoningMode `json:"reasoning,omitempty"`
	}{change.Model, change.ThinkingLevel, change.Reasoning}
	return readSettings(ctx, s, settingsSetMethod, params)
}

func readSettings(ctx context.Context, s *session, method string, params any) (SettingsSnapshot, error) {
	var out SettingsSnapshot
	_, err := s.call(ctx, method, params, func(raw any) error {
		data, err := json.Marshal(raw)
		if err != nil {
			return ErrProtocol
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&out); err != nil {
			return ErrProtocol
		}
		if !validCatalogModel(out.Model) || !slices.Contains(catalogThinking, out.ThinkingLevel) || !validReasoningMode(out.Reasoning) {
			return ErrProtocol
		}
		return nil
	})
	if err != nil {
		return SettingsSnapshot{}, err
	}
	return out, nil
}
