package sdk

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
		{"../protocol/v1/pi-1.6/pi-events.schema.json", piSchema},
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
	r := Replica{}
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
			}
		case "patch", "state_end":
			changed, err := r.apply(f)
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
