package sdk

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"unicode/utf16"
)

type Replica struct {
	tree     map[string]any
	snapshot Snapshot
	stale    bool
}

func (r *Replica) Current() (Snapshot, bool) { return r.snapshot, r.tree != nil && !r.stale }

// Install atomically replaces any old feed. Install and Apply are for a single owner,
// such as a connection reader, rather than concurrent callers.
func (r *Replica) Install(raw json.RawMessage, epoch string, thread ID) error {
	v, err := parseJSON(raw)
	if err != nil {
		return err
	}
	return r.install(v, epoch, thread)
}

func (r *Replica) install(v any, epoch string, thread ID) error {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 4 {
		return ErrProtocol
	}
	sid, _ := m["subscriptionId"].(string)
	gotEpoch, _ := m["epoch"].(string)
	rev, revOK := asUint(m["rev"])
	if !token(sid) || !token(epoch) || gotEpoch != epoch || !revOK || rev != 0 || sid == r.snapshot.SubscriptionID {
		return ErrProtocol
	}
	tree, ok := m["state"].(map[string]any)
	if !ok {
		return ErrProtocol
	}
	state, err := decodeState(tree, thread)
	if err != nil {
		return err
	}
	r.tree, r.stale = tree, false
	r.snapshot = Snapshot{SubscriptionID: sid, Epoch: epoch, State: state}
	return nil
}

// Apply accepts a patch or state_end. Other subscription IDs are ignored. Matching-feed
// errors are atomic and return ErrStale; only a fresh Install can recover the replica.
func (r *Replica) Apply(raw json.RawMessage) (changed bool, err error) {
	v, err := parseJSON(raw)
	if err != nil {
		r.stale = true
		return false, ErrStale
	}
	return r.apply(v)
}

func (r *Replica) invalid() (bool, error) {
	r.stale = true
	return false, ErrStale
}

func (r *Replica) apply(v any) (bool, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return r.invalid()
	}
	sid, _ := m["subscriptionId"].(string)
	if !token(sid) {
		return r.invalid()
	}
	if r.tree == nil || sid != r.snapshot.SubscriptionID {
		return false, nil
	}
	if r.stale || m["epoch"] != r.snapshot.Epoch {
		return r.invalid()
	}
	typ, _ := m["type"].(string)
	if typ == "state_end" {
		return r.invalid()
	}
	base, okBase := asUint(m["baseRev"])
	next, okNext := asUint(m["nextRev"])
	ops, okOps := m["ops"].([]any)
	if typ != "patch" || !okBase || !okNext || base != r.snapshot.Rev || next <= base || !okOps || len(m) != 6 {
		return r.invalid()
	}
	// These envelope checks and the exact single append tuple cover the
	// patch schema on the token path, without trying every oneOf branch.
	index, token := streamAppendIndex(ops)
	if !token && schemaCheck("patch", m) != nil {
		return r.invalid()
	}
	tree, err := applyOps(r.tree, ops)
	if err != nil {
		return r.invalid()
	}
	state := r.snapshot.State
	if token && state.Streaming != nil {
		if stream := decodeStreamAppend(tree, index, state.Streaming); stream != nil {
			state.Streaming = stream
			r.tree, r.snapshot.State, r.snapshot.Rev = tree, state, next
			return true, nil
		}
	}
	state, err = decodePatchedState(r.tree, tree, ops, state)
	if err != nil {
		state, err = decodeState(tree, r.snapshot.State.Thread.ID)
		if err != nil {
			return r.invalid()
		}
	}
	r.tree, r.snapshot.State, r.snapshot.Rev = tree, state, next
	return true, nil
}

// decodePatchedState runs only after applyOps succeeds. Untouched fields retain
// both their wire values and their already-validated typed values. Anything
// outside this decomposition is left to the full decoder.
func decodePatchedState(before, tree map[string]any, ops []any, prev State) (State, error) {
	if !keys(tree, "thread", "agent", "usage", "window", "tools", "queue", "compactions") {
		return State{}, ErrProtocol
	}
	touched := make(map[string]struct{})
	for _, raw := range ops {
		op := raw.([]any)
		code := op[0].(string)
		if code == "r" {
			return State{}, ErrProtocol
		}
		path := op[1].([]any)
		key := path[0].(string) // a successful edit of the root map requires a string
		if key == "window" && !windowMembershipOp(code, path) {
			return State{}, ErrProtocol
		}
		touched[key] = struct{}{}
	}

	next := prev
	for key := range touched {
		var err error
		switch key {
		case "thread":
			err = decodeProperty(tree, key, &next.Thread)
		case "agent":
			err = decodeProperty(tree, key, &next.Agent)
		case "usage":
			err = decodeProperty(tree, key, &next.Usage)
		case "window":
			next.Window, err = decodeWindow(before["window"], tree[key], prev.Window)
		case "tools":
			err = decodeProperty(tree, key, &next.Tools)
		case "queue":
			err = decodeProperty(tree, key, &next.Queue)
		case "compactions":
			err = decodeProperty(tree, key, &next.Compactions)
		case "compactionFailure":
			err = decodeProperty(tree, key, &next.CompactionFailure)
		case "run":
			err = decodeProperty(tree, key, &next.Run)
		case "failure":
			err = decodeProperty(tree, key, &next.Failure)
		case "streaming":
			err = decodeProperty(tree, key, &next.Streaming)
		default:
			return State{}, ErrProtocol
		}
		if err != nil {
			return State{}, err
		}
	}
	if next.Thread.ID != prev.Thread.ID {
		return State{}, ErrProtocol
	}
	return next, nil
}

func streamAppendIndex(ops []any) (uint64, bool) {
	if len(ops) != 1 {
		return 0, false
	}
	op, ok := ops[0].([]any)
	if !ok || len(op) != 3 || op[0] != "a" {
		return 0, false
	}
	path, ok := op[1].([]any)
	if !ok || len(path) != 4 || path[0] != "streaming" || path[1] != "blocks" || path[3] != "text" {
		return 0, false
	}
	if _, ok := op[2].(string); !ok {
		return 0, false
	}
	return asUint(path[2])
}

// A token append changes only one string in a previously validated stream.
// Validate that block, then reuse the wire string and the other typed blocks
// instead of round-tripping all of the accumulated text through JSON.
func decodeStreamAppend(tree map[string]any, i uint64, prev *Streaming) *Streaming {
	if i >= uint64(len(prev.Blocks)) {
		return nil
	}
	var schema string
	switch prev.Blocks[i].Type {
	case "text":
		schema = "text"
	case "thinking":
		schema = "thinkingBlock"
	default:
		return nil
	}
	// applyOps has already checked every container on this path.
	block := tree["streaming"].(map[string]any)["blocks"].([]any)[i].(map[string]any)
	if schemaCheck(schema, block) != nil {
		return nil
	}
	text, ok := block["text"].(string)
	if !ok {
		return nil
	}
	next := *prev
	next.Blocks = slices.Clone(prev.Blocks)
	next.Blocks[i].Text = text
	return &next
}

func decodeProperty[T any](obj map[string]any, key string, dst *T) error {
	v, exists := obj[key]
	var value T
	if exists {
		if err := schemaCheck(stateProperty+key, v); err != nil {
			return err
		}
		var err error
		value, err = decode[T](v)
		if err != nil {
			return err
		}
	}
	// Absent optional properties have the zero value, as in a fresh decode.
	*dst = value
	return nil
}

func windowMembershipOp(code string, path []any) bool {
	if len(path) < 2 {
		return false
	}
	switch path[1] {
	case "entries":
		return len(path) == 2 && code == "p" || len(path) <= 3 && oneOf(code, "s", "d")
	case "beforeEntryId", "lastEntryId":
		return len(path) == 2 && oneOf(code, "s", "d")
	}
	return false
}

func decodeWindow(before, after any, prev TranscriptWindow) (TranscriptWindow, error) {
	var w TranscriptWindow
	obj, ok := after.(map[string]any)
	if !ok || len(obj) != 3 || !keys(obj, "beforeEntryId", "lastEntryId", "entries") {
		return w, ErrProtocol
	}
	entries, ok := obj["entries"].([]any)
	if !ok || len(entries) > 100 { // state.window.entries maxItems
		return w, ErrProtocol
	}
	old, ok := before.(map[string]any)
	if !ok {
		return w, ErrProtocol
	}
	oldEntries, ok := old["entries"].([]any)
	if !ok || len(oldEntries) != len(prev.Entries) {
		return w, ErrProtocol
	}
	if err := decodeWindowID(obj, "beforeEntryId", &w.BeforeEntryID); err != nil {
		return w, err
	}
	if err := decodeWindowID(obj, "lastEntryId", &w.LastEntryID); err != nil {
		return w, err
	}

	// The preceding window is the bounded cache. Only reuse a typed entry
	// when its entire wire value matches; an ID alone proves nothing about
	// replacements. DeepEqual short-circuits on shared, copy-on-write maps.
	cached := make(map[ID]int, len(oldEntries))
	for i, entry := range prev.Entries {
		cached[entry.ID] = i
	}
	w.Entries = make([]Entry, len(entries))
	for i, raw := range entries {
		m, _ := raw.(map[string]any)
		id, _ := m["id"].(string)
		if j, ok := cached[ID(id)]; ok && reflect.DeepEqual(oldEntries[j], raw) {
			w.Entries[i] = prev.Entries[j]
			continue
		}
		if err := schemaCheck("entry", raw); err != nil {
			return w, err
		}
		entry, err := decode[Entry](raw)
		if err != nil {
			return w, err
		}
		w.Entries[i] = entry
	}
	if !validWindow(w) {
		return w, ErrProtocol
	}
	return w, nil
}

func decodeWindowID(obj map[string]any, key string, dst **ID) error {
	v := obj[key]
	if err := schemaCheck(stateProperty+"window/properties/"+key, v); err != nil {
		return err
	}
	id, err := decode[*ID](v)
	*dst = id
	return err
}

func applyOps(before map[string]any, ops []any) (map[string]any, error) {
	// edit copies only the containers it writes, including when they came
	// from an earlier op's argument. Published trees are never edited in place.
	var root any = before
	for index, raw := range ops {
		op, ok := raw.([]any)
		if !ok || len(op) < 2 {
			return nil, ErrProtocol
		}
		code, ok := op[0].(string)
		if !ok {
			return nil, ErrProtocol
		}
		if code == "r" {
			if index != 0 || len(op) != 2 {
				return nil, ErrProtocol
			}
			if _, ok := op[1].(map[string]any); !ok {
				return nil, ErrProtocol
			}
			root = op[1]
			continue
		}
		path, ok := op[1].([]any)
		if !ok || len(path) < 1 || len(path) > 32 {
			return nil, ErrProtocol
		}
		var err error
		root, err = edit(root, path, code, op[2:])
		if err != nil {
			return nil, err
		}
	}
	return root.(map[string]any), nil
}

func edit(parent any, path []any, code string, args []any) (any, error) {
	var target any
	var exists bool
	var set func(any) any
	var remove func() any
	if key, ok := path[0].(string); ok {
		obj, ok := parent.(map[string]any)
		if !ok || oneOf(key, "__proto__", "constructor", "prototype") {
			return nil, ErrProtocol
		}
		target, exists = obj[key]
		set = func(v any) any {
			out := maps.Clone(obj)
			out[key] = v
			return out
		}
		remove = func() any {
			out := maps.Clone(obj)
			delete(out, key)
			return out
		}
	} else {
		at, ok := asUint(path[0])
		arr, isArray := parent.([]any)
		if !ok || !isArray || at > uint64(len(arr)) {
			return nil, ErrProtocol
		}
		n := int(at)
		exists = n < len(arr)
		if exists {
			target = arr[n]
		}
		set = func(v any) any {
			if exists {
				out := slices.Clone(arr)
				out[n] = v
				return out
			}
			return append(arr[:n:n], v)
		}
		if exists {
			remove = func() any { return slices.Delete(slices.Clone(arr), n, n+1) }
		}
	}

	if len(path) > 1 {
		if !exists {
			return nil, ErrProtocol
		}
		next, err := edit(target, path[1:], code, args)
		if err != nil {
			return nil, err
		}
		return set(next), nil
	}
	if code == "s" && len(args) == 1 {
		return set(args[0]), nil
	}
	if code == "d" && len(args) == 0 && remove != nil {
		return remove(), nil
	}
	if !exists {
		return nil, ErrProtocol
	}
	switch code {
	case "a":
		if len(args) != 1 {
			break
		}
		suffix, ok := args[0].(string)
		if !ok {
			break
		}
		switch s := target.(type) {
		case string:
			return set(s + suffix), nil
		case splitUTF16:
			return set(append(s[:len(s):len(s)], utf16.Encode([]rune(suffix))...)), nil
		}
	case "t":
		if len(args) != 1 {
			break
		}
		count, ok := asUint(args[0])
		if !ok {
			break
		}
		var units []uint16
		switch s := target.(type) {
		case string:
			units = utf16.Encode([]rune(s))
		case splitUTF16:
			units = s
		default:
			return nil, ErrProtocol
		}
		kept := units[min(count, uint64(len(units))):]
		if len(kept) > 0 && kept[0] >= 0xdc00 && kept[0] <= 0xdfff {
			return set(splitUTF16(kept)), nil
		}
		return set(string(utf16.Decode(kept))), nil
	case "p":
		arr, ok := target.([]any)
		if !ok || len(args) != 3 {
			break
		}
		index, indexOK := asUint(args[0])
		n, nOK := asUint(args[1])
		insert, insertOK := args[2].([]any)
		if !indexOK || !nOK || !insertOK {
			break
		}
		from := int(min(index, uint64(len(arr))))
		to := from + int(min(n, uint64(len(arr)-from)))
		out := make([]any, from+len(insert)+len(arr)-to)
		copy(out, arr[:from])
		copy(out[from:], insert)
		copy(out[from+len(insert):], arr[to:])
		return set(out), nil
	case "m":
		arr, ok := target.([]any)
		if !ok || len(args) != 1 {
			break
		}
		order, ok := args[0].([]any)
		if !ok || len(order) != len(arr) {
			break
		}
		out := make([]any, len(arr))
		seen := make([]bool, len(arr))
		for i, p := range order {
			index, ok := asUint(p)
			if !ok || index >= uint64(len(arr)) || seen[index] {
				return nil, ErrProtocol
			}
			seen[index], out[i] = true, arr[index]
		}
		return set(out), nil
	}
	return nil, ErrProtocol
}

type splitUTF16 []uint16

func (splitUTF16) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("trim splits a surrogate pair")
}
