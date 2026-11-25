package sdk

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func fixtures(t *testing.T, name string) []map[string]any {
	t.Helper()
	file, err := os.Open(filepath.Join("..", "protocol", "v1", "fixtures", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	s := bufio.NewScanner(file)
	var out []map[string]any
	for s.Scan() {
		f, err := parseJSON(s.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, f.(map[string]any))
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFrozenSchemaCopy(t *testing.T) {
	for _, schema := range []struct {
		path string
		copy []byte
	}{
		{"../protocol/v1/core.schema.json", coreSchema},
		{"../protocol/v1/thread-directory.schema.json", threadDirectorySchema},
	} {
		source, err := os.ReadFile(schema.path)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, source); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bytes.TrimSpace(schema.copy), buf.Bytes()) {
			t.Fatalf("run go generate after updating %s", schema.path)
		}
	}
}

func TestGoldenReplica(t *testing.T) {
	var r, full Replica
	var epoch string
	var target ID
	var effects []string
	var ops []string
	for _, f := range fixtures(t, "replication") {
		switch f["type"] {
		case "request":
			if f["method"] == "subscribe" {
				target = ID(f["params"].(map[string]any)["threadId"].(string))
			}
		case "response":
			if f["ok"] != true {
				continue
			}
			out := f["result"].(map[string]any)
			switch f["method"] {
			case "hello":
				epoch = out["epoch"].(string)
			case "subscribe":
				if err := r.install(out, epoch, target); err != nil {
					t.Fatal(err)
				}
				if err := full.install(out, epoch, target); err != nil {
					t.Fatal(err)
				}
			}
		case "patch", "state_end":
			changed, err := compareReplicaPatch(t, &r, &full, f)
			switch {
			case err != nil:
				if !errors.Is(err, ErrStale) {
					t.Fatal(err)
				}
				effects = append(effects, "stale")
			case changed:
				effects = append(effects, "applied")
				for _, op := range f["ops"].([]any) {
					ops = append(ops, op.([]any)[0].(string))
				}
				if r.snapshot.SubscriptionID == "state-a" && r.snapshot.Rev == 3 {
					s := r.snapshot.State
					if *s.Tools[1].Output != "BC!" || s.Streaming.Blocks[0].Text != "start more" ||
						s.Tools[0].CallID != "tool-b" {
						t.Fatalf("wrong trim, append or order: %+v", s.Tools)
					}
				}
			default:
				effects = append(effects, "ignored")
			}
		}
	}
	want := []string{"applied", "applied", "applied", "applied", "stale", "ignored", "applied", "stale", "stale", "stale", "stale", "stale"}
	if !slices.Equal(effects, want) {
		t.Fatalf("effects %v, want %v", effects, want)
	}
	slices.Sort(ops)
	if !slices.Equal(slices.Compact(ops), []string{"a", "d", "m", "p", "r", "s", "t"}) {
		t.Fatal("not all ops applied", ops)
	}
}

func TestPatchIsAtomicAndKeepsOwnedShape(t *testing.T) {
	for _, ops := range []string{
		`[["s",["agent","cwd"],"/not-published"],["s",["usage","totalTokens"],-1]]`,
		`[["d",["window","lastEntryId"]]]`,
		`[["s",["agent","__proto__"],{}]]`,
		`[["s",["tools",9],{}]]`,
		`[["s",["thread","id"],"63"]]`,
		`[["s",["agent","cwd"],"A😀BC"],["t",["agent","cwd"],2]]`,
		`[["m",["tools"],[0,0]]]`,
	} {
		var r Replica
		for _, f := range fixtures(t, "replication") {
			if f["method"] == "subscribe" && f["ok"] == true {
				got := f["result"].(map[string]any)
				thread := ID(got["state"].(map[string]any)["thread"].(map[string]any)["id"].(string))
				if err := r.install(got, got["epoch"].(string), thread); err != nil {
					t.Fatal(err)
				}
				break
			}
		}
		s, _ := r.Current()
		msg := json.RawMessage(`{"type":"patch","epoch":"` + s.Epoch + `","subscriptionId":"` + s.SubscriptionID +
			`","baseRev":0,"nextRev":1,"ops":` + ops + `}`)
		if changed, err := r.Apply(msg); changed || !errors.Is(err, ErrStale) {
			t.Fatalf("%s: %v %v", msg, changed, err)
		}
		if after, ok := r.Current(); ok || after.Rev != 0 || after.State.Agent.CWD != s.State.Agent.CWD {
			t.Fatal("partially applied a batch")
		}
	}
}

// applyFullReplica is the eager validation/decoding reference. Operation
// semantics are shared; copy-on-write itself is checked by retaining old trees.
func applyFullReplica(r *Replica, frame map[string]any) (bool, error) {
	sid, _ := frame["subscriptionId"].(string)
	if !token(sid) {
		return r.invalid()
	}
	if r.tree == nil || sid != r.snapshot.SubscriptionID {
		return false, nil
	}
	base, baseOK := asUint(frame["baseRev"])
	next, nextOK := asUint(frame["nextRev"])
	ops, opsOK := frame["ops"].([]any)
	if r.stale || frame["epoch"] != r.snapshot.Epoch || frame["type"] != "patch" ||
		!baseOK || !nextOK || base != r.snapshot.Rev || next <= base || !opsOK || schemaCheck("patch", frame) != nil {
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

func replicaJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func compareReplicaPatch(t *testing.T, inc, full *Replica, frame map[string]any) (bool, error) {
	t.Helper()
	oldTree, oldSnapshot := inc.tree, inc.snapshot
	treeJSON, snapshotJSON, frameJSON := replicaJSON(t, oldTree), replicaJSON(t, oldSnapshot), replicaJSON(t, frame)
	changed, err := inc.apply(frame)
	if !bytes.Equal(treeJSON, replicaJSON(t, oldTree)) || !bytes.Equal(snapshotJSON, replicaJSON(t, oldSnapshot)) ||
		!bytes.Equal(frameJSON, replicaJSON(t, frame)) {
		t.Fatalf("mutated prior data applying %s", frameJSON)
	}
	if !changed && (!bytes.Equal(treeJSON, replicaJSON(t, inc.tree)) || !bytes.Equal(snapshotJSON, replicaJSON(t, inc.snapshot))) {
		t.Fatalf("published a partial update applying %s", frameJSON)
	}
	wantChanged, wantErr := applyFullReplica(full, frame)
	if changed != wantChanged || err != wantErr || inc.stale != full.stale ||
		!reflect.DeepEqual(inc.snapshot, full.snapshot) || !reflect.DeepEqual(inc.tree, full.tree) {
		t.Fatalf("incremental/full disagreement applying %s: (%v, %v) vs (%v, %v)", frameJSON, changed, err, wantChanged, wantErr)
	}
	return changed, err
}

func replicaPatchFrame(t *testing.T, epoch string, base, next uint64, ops string) map[string]any {
	t.Helper()
	raw := fmt.Sprintf(`{"type":"patch","epoch":%q,"subscriptionId":"bench-sub","baseRev":%d,"nextRev":%d,"ops":%s}`, epoch, base, next, ops)
	v, err := parseJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[string]any)
}

func TestReplicaIncrementalDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(547))
	raw := replicaWindow(4, 512)
	valid := []string{
		`[["a",["streaming","blocks",0,"text"],"😀 token"]]`,
		`[["t",["streaming","blocks",0,"text"],100000]]`,
		`[["s",["streaming","blocks",0,"text"],"A😀BC"],["t",["streaming","blocks",0,"text"],2],["a",["streaming","blocks",0,"text"],"!"],["t",["streaming","blocks",0,"text"],1]]`,
		`[["s",["tools"],[{"callId":"a","name":"read","status":"running","output":"abc"}]],["p",["tools"],1,0,[{"callId":"b","name":"read","status":"done"}]],["m",["tools"],[1,0]],["d",["tools",1,"output"]]]`,
		`[["s",["queue",0],{"submissionId":"7","mode":"steer","text":"later"}]]`,
		`[["s",["queue"],[]]]`,
		`[["s",["agent","cwd"],"/sdk"],["s",["usage","totalTokens"],15],["s",["thread","owner"],{"threadId":"2","taskId":"3"}]]`,
		`[["s",["run"],{"taskId":"8","submissionIds":[]}],["s",["failure"],{"taskId":"9","reason":"no_model"}]]`,
		`[["d",["run"]],["d",["failure"]]]`,
		`[["s",["compactions"],[{"taskId":"10","reason":"manual","blocking":true,"attempt":1}]],["s",["compactionFailure"],{"taskId":"10","reason":"manual","detail":"failed"}]]`,
		`[["d",["compactionFailure"]]]`,
		`[["s",["future"],true],["d",["future"]]]`, // unexpected path, final state valid
	}
	invalid := []string{
		`[["s",["agent","cwd"],"/not-published"],["d",["tools"]]]`,
		`[["s",["window","lastEntryId"],"3"]]`,
		`[["s",["window","beforeEntryId"],"1"]]`,
		`[["s",["window","beforeEntryId"],"9007199254740992"]]`,
		`[["s",["window","entries",0],{"id":"1","threadId":"9007199254740992","kind":"model","messages":[]}]]`,
		`[["s",["window","entries",0],{"id":"1","threadId":"1","kind":"model","messages":[{}]}]]`,
		`[["s",["streaming","blocks"],[{"type":"toolCall","id":"tool","name":"test","arguments":{"x":"😀"}}]],["t",["streaming","blocks",0,"arguments","x"],1]]`,
		`[["s",["agent","cwd"],"/not-published"],["s",["queue",99],{}]]`,
		`[["s",["thread","id"],"2"]]`,
		`[["s",["compactionFailure"],{"taskId":"10","reason":"manual"}]]`,
		`[["s",["streaming","attempt"],0]]`,
		`[["s",["tools"],{}]]`,
		`[["bad",["agent"]]]`,
	}
	for scenario := 0; scenario < len(invalid)+3; scenario++ {
		inc, full := installWindow(t, raw), installWindow(t, raw)
		firstSnapshot := inc.snapshot
		firstJSON := replicaJSON(t, firstSnapshot)
		for range 12 {
			var ops string
			i := rng.Intn(4)
			switch rng.Intn(len(valid) + 2) {
			case len(valid):
				// Replace an entry with the same ID but new content via splice.
				ops = fmt.Sprintf(`[["p",["window","entries"],%d,1,[{"id":"%d","threadId":"1","kind":"kind-%d","messages":[]}]]]`, i, i+1, rng.Intn(100))
			case len(valid) + 1:
				// Nested history edits are legal if the full decode accepts them.
				ops = fmt.Sprintf(`[["s",["window","entries",%d,"kind"],"kind-%d"]]`, i, rng.Intn(100))
			default:
				ops = valid[rng.Intn(len(valid))]
			}
			base := inc.snapshot.Rev
			frame := replicaPatchFrame(t, "bench-epoch", base, base+1+uint64(rng.Intn(3)), ops)
			if changed, err := compareReplicaPatch(t, inc, full, frame); !changed || err != nil {
				t.Fatalf("valid patch rejected: %s (%v)", ops, err)
			}
		}
		epoch, base := "bench-epoch", inc.snapshot.Rev
		next, ops := base+1, valid[0]
		switch scenario {
		case len(invalid):
			epoch = "wrong-epoch"
		case len(invalid) + 1:
			next = base
		case len(invalid) + 2:
			base--
		default:
			ops = invalid[scenario]
		}
		if changed, err := compareReplicaPatch(t, inc, full, replicaPatchFrame(t, epoch, base, next, ops)); changed || err != ErrStale {
			t.Fatalf("patch should stale the replica: %s (%v)", ops, err)
		}
		base = inc.snapshot.Rev
		if changed, err := compareReplicaPatch(t, inc, full, replicaPatchFrame(t, "bench-epoch", base, base+1, valid[0])); changed || err != ErrStale {
			t.Fatal("patch recovered a stale replica without install")
		}
		if !bytes.Equal(firstJSON, replicaJSON(t, firstSnapshot)) {
			t.Fatal("first snapshot mutated across later revisions")
		}
	}
}

func TestReplicaPatchEnvelopeAndTupleValidation(t *testing.T) {
	raw := replicaWindow(3, 512)
	for _, ops := range []string{
		`[]`,
		`[["s",[],true]]`,
		`[["s",["agent","__proto__"],true],["d",["agent","__proto__"]]]`,
		`[["s",["tools",-1],{}]]`,
		`[["s",["agent","cwd"]]]`,
		`[["d",["agent","cwd"],null]]`,
		`[["a",["streaming","blocks",0,"text"],true]]`,
		`[["t",["streaming","blocks",0,"text"],1.5]]`,
		`[["p",["tools"],0,0,{}]]`,
		`[["m",["tools"],[0,0]]]`,
		`[["r",{}],["s",["thread"],{"id":"1"}]]`,
	} {
		t.Run(ops, func(t *testing.T) {
			inc, full := installWindow(t, raw), installWindow(t, raw)
			if changed, err := compareReplicaPatch(t, inc, full, replicaPatchFrame(t, "bench-epoch", 0, 1, ops)); changed || err != ErrStale {
				t.Fatal("accepted invalid tuple")
			}
		})
	}
	inc, full := installWindow(t, raw), installWindow(t, raw)
	frame := replicaPatchFrame(t, "bench-epoch", 0, 1, `[["d",["agent","cwd"]]]`)
	frame["extra"] = nil
	if changed, err := compareReplicaPatch(t, inc, full, frame); changed || err != ErrStale {
		t.Fatal("accepted unknown patch field")
	}
}

func TestReplicaReusesWindowEntries(t *testing.T) {
	raw := replicaWindow(3, 512)
	inc, full := installWindow(t, raw), installWindow(t, raw)
	start := inc.snapshot.State
	wireEntries := func() []any { return inc.tree["window"].(map[string]any)["entries"].([]any) }
	startWireEntries := wireEntries()
	apply := func(ops string) {
		t.Helper()
		base := inc.snapshot.Rev
		frame := replicaPatchFrame(t, "bench-epoch", base, base+1, ops)
		if changed, err := compareReplicaPatch(t, inc, full, frame); !changed || err != nil {
			t.Fatalf("valid patch rejected: %s (%v)", ops, err)
		}
	}
	apply(`[["a",["streaming","blocks",0,"text"]," token"]]`)
	if &wireEntries()[0] != &startWireEntries[0] || &inc.snapshot.State.Window.Entries[0] != &start.Window.Entries[0] ||
		inc.snapshot.State.Streaming == start.Streaming {
		t.Fatal("did not reuse the untouched window or replace the modified stream")
	}
	apply(`[["d",["window","entries",0]],["s",["window","beforeEntryId"],"1"],["s",["window","entries",2],{"id":"4","threadId":"1","kind":"model","messages":[]}],["s",["window","lastEntryId"],"4"]]`)
	slid := inc.snapshot.State.Window
	if &slid.Entries[0].Messages[0] != &start.Window.Entries[1].Messages[0] || &slid.Entries[1].Messages[0] != &start.Window.Entries[2].Messages[0] {
		t.Fatal("did not reuse typed entries across a membership change")
	}
	apply(`[["p",["window","entries"],2,1,[{"id":"4","threadId":"1","kind":"replacement","messages":[]}]]]`)
	if inc.snapshot.State.Window.Entries[2].Kind != "replacement" {
		t.Fatal("reused an entry based only on its ID")
	}
	apply(`[["p",["window","entries"],0,3,[]],["s",["window","beforeEntryId"],null],["s",["window","lastEntryId"],null]]`)
	apply(`[["s",["streaming","blocks",1],{"type":"thinking","text":"𐐀"}],["s",["streaming","blocks",2],{"type":"toolCall","id":"call","name":"read","arguments":{"path":"foo"}}]]`)
	apply(`[["a",["streaming","blocks",1,"text"],"😀 thought\n\t"]]`)
	apply(`[["a",["streaming","blocks",2,"arguments","path"],"/bar"]]`)

	raw = replicaWindow(100, 512)
	inc, full = installWindow(t, raw), installWindow(t, raw)
	frame := replicaPatchFrame(t, "bench-epoch", 0, 1, `[["p",["window","entries"],100,0,[{"id":"101","threadId":"1","kind":"model","messages":[]}]],["s",["window","lastEntryId"],"101"]]`)
	if changed, err := compareReplicaPatch(t, inc, full, frame); changed || err != ErrStale {
		t.Fatal("accepted an oversized window")
	}
}

func TestApplyOpsDoesNotWriteSplitUTF16BackingSlice(t *testing.T) {
	units := splitUTF16{0xde00, 'B', 'C'}
	before := map[string]any{"text": units[:1]}
	ops := []any{[]any{"a", []any{"text"}, "x"}}
	tree, err := applyOps(before, ops)
	if err != nil || !reflect.DeepEqual(tree["text"], splitUTF16{0xde00, 'x'}) {
		t.Fatalf("split append failed: %v, %v", tree, err)
	}
	if !slices.Equal(units, splitUTF16{0xde00, 'B', 'C'}) {
		t.Fatal("split append mutated the old backing array")
	}
}

func TestStrictJSON(t *testing.T) {
	for _, raw := range []string{
		`{"a": 1, "\u0061": 2}`, `{"a":"\ud800"}`, `{"a":"\udfff"}`,
		`{"n":1e999}`, "{\"a\":\"\xff\"}", "\ufeff{}", "{} {}",
	} {
		if _, err := parseJSON([]byte(raw)); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if _, err := parseJSON([]byte(`{"a":"\ud83d\ude00","b":"\\ud800"}`)); err != nil {
		t.Fatal(err)
	}
}
