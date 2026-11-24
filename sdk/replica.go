package sdk

import (
	"encoding/json"
	"fmt"
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
	if typ != "patch" || !okBase || !okNext || base != r.snapshot.Rev || next <= base || !okOps || schemaCheck("patch", m) != nil {
		return r.invalid()
	}
	tree, err := applyOps(r.tree, ops)
	if err != nil {
		return r.invalid()
	}
	state, err := decodeState(tree, r.snapshot.State.Thread.ID)
	if err != nil {
		return r.invalid()
	}
	r.tree, r.snapshot.State, r.snapshot.Rev = tree, state, next
	return true, nil
}

func copyTree(v any) any {
	switch v := v.(type) {
	case map[string]any:
		res := make(map[string]any, len(v))
		for k, x := range v {
			res[k] = copyTree(x)
		}
		return res
	case []any:
		res := make([]any, len(v))
		for i, x := range v {
			res[i] = copyTree(x)
		}
		return res
	default:
		return v
	}
}

func applyOps(before map[string]any, ops []any) (map[string]any, error) {
	root := copyTree(before)
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
		set = func(v any) any { obj[key] = v; return obj }
		remove = func() any { delete(obj, key); return obj }
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
				arr[n] = v
				return arr
			}
			return append(arr, v)
		}
		if exists {
			remove = func() any { return slices.Delete(arr, n, n+1) }
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
			return set(append(s, utf16.Encode([]rune(suffix))...)), nil
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
		from := min(index, uint64(len(arr)))
		to := from + min(n, uint64(len(arr))-from)
		return set(slices.Replace(arr, int(from), int(to), insert...)), nil
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
