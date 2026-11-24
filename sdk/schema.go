package sdk

import (
	"bytes"
	_ "embed"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:generate go run ./internal/copy-schema
//go:embed internal/core.schema.json
var coreSchema []byte

//go:embed internal/pi-events.schema.json
var piSchema []byte

//go:embed internal/intro.schema.json
var introSchema []byte

//go:embed internal/thread-directory.schema.json
var threadDirectorySchema []byte

//go:embed internal/images.schema.json
var imagesSchema []byte

var imagesWire = loadSchema(imagesSchema, "urn:hush:protocol:images:1", "successEnvelope")
var threadDirectoryWire = loadSchema(threadDirectorySchema, "urn:hush:protocol:thread-directory:1", "successEnvelope", "successEnvelopeV2", "successWorkspace")
var intros = loadSchema(introSchema, "urn:hush:protocol:intro:1", "successEnvelope", "successEnvelopeV2")
var wireSchema = loadSchema(coreSchema, "urn:hush:protocol:core:1", "request", "success", "failureReply", "state", "stateEnd", "patch")
var orderedSchema = loadSchema(piSchema, "urn:hush:protocol:pi-events:1", "successEnvelope", "open", "command", "ready", "frame", "end", "callResult", "catalogResult")

func loadSchema(data []byte, url string, keys ...string) map[string]*jsonschema.Schema {
	c := jsonschema.NewCompiler()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		panic(err)
	}
	if err := c.AddResource(url, doc); err != nil {
		panic(err)
	}
	result := make(map[string]*jsonschema.Schema)
	for _, key := range keys {
		result[key], err = c.Compile(url + "#/$defs/" + key)
		if err != nil {
			panic(err)
		}
	}
	return result
}

func schemaCheck(name string, value any) error {
	if err := wireSchema[name].Validate(value); err != nil {
		return fmt.Errorf("%w: schema %s", ErrProtocol, name)
	}
	return nil
}

func piSchemaCheck(name string, value any) error {
	if err := orderedSchema[name].Validate(value); err != nil {
		return fmt.Errorf("%w: piEvents schema %s", ErrProtocol, name)
	}
	return nil
}

func decodeState(tree map[string]any, expected ID) (State, error) {
	if err := schemaCheck("state", tree); err != nil {
		return State{}, err
	}
	s, err := decode[State](tree)
	if err != nil {
		return State{}, err
	}
	w := s.Window
	if s.Thread.ID != expected || !ordered(w.Entries) ||
		(len(w.Entries) == 0 && (w.BeforeEntryID != nil || w.LastEntryID != nil)) ||
		(len(w.Entries) > 0 && (w.LastEntryID == nil || *w.LastEntryID != w.Entries[len(w.Entries)-1].ID ||
			w.BeforeEntryID != nil && cmpID(*w.BeforeEntryID, w.Entries[0].ID) >= 0)) {
		return State{}, ErrProtocol
	}
	return s, nil
}

func ordered(entries []Entry) bool {
	for i, entry := range entries {
		if !entry.ID.Valid() || i > 0 && cmpID(entries[i-1].ID, entry.ID) >= 0 {
			return false
		}
	}
	return true
}
