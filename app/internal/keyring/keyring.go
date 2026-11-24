// Package keyring implements the private credential pipe between Hush's Go
// launcher and its Node backend. It has no ordinary CLI or socket surface.
package keyring

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

const HelperArg = "--hush-internal-keyring"
const HelperEnv = "HUSH_KEYRING_HELPER"

const maxCredential = 64 << 10
const servicePrefix = "io.github.javiermolinar.hush.oauth."
const account = "openai-codex"

var errUnavailable = errors.New("system keyring unavailable")

type request struct {
	Operation string          `json:"operation"`
	Scope     string          `json:"scope"`
	Record    json.RawMessage `json:"record,omitempty"`
}

// Run reads exactly one bounded request from a private pipe. It writes a
// record only on successful read; empty output means no item. All failure
// detail stays off stdout and stderr. The caller maps the exit status.
func Run(in io.Reader, out io.Writer) int {
	const limit = maxCredential + 1024
	data, err := io.ReadAll(io.LimitReader(in, limit+1))
	if err != nil || len(data) == 0 || len(data) > limit {
		return 2
	}
	defer clear(data)
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var req request
	if dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF || !validScope(req.Scope) {
		return 2
	}
	defer clear(req.Record)
	switch req.Operation {
	case "get", "delete":
		if len(req.Record) != 0 {
			return 2
		}
	case "set":
		if !validRecord(req.Record) {
			return 2
		}
	default:
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3500*time.Millisecond)
	defer cancel()
	result, err := platformKeyring(ctx, req)
	if err != nil {
		return 3
	}
	defer clear(result)
	if len(result) != 0 {
		if req.Operation != "get" || !validRecord(result) {
			return 2
		}
		if _, err := out.Write(result); err != nil {
			return 2
		}
	}
	return 0
}

func validScope(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validRecord(data []byte) bool {
	return len(data) > 2 && len(data) <= maxCredential && data[0] == '{' && json.Valid(data)
}
