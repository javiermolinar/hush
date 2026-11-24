package sdk

import (
	"bufio"
	"errors"
	"net"
	"reflect"
	"testing"
)

func TestSettings(t *testing.T) {
	ctx, path := runContext(t)
	initial := SettingsSnapshot{Model: Model{Provider: "fixture", ModelID: "one"}, ThinkingLevel: "high", Reasoning: ReasoningFolded}
	changed := SettingsSnapshot{Model: Model{Provider: "fixture", ModelID: "two"}, ThinkingLevel: "low", Reasoning: ReasoningExpanded}
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		q, err := readReq(r)
		if err != nil || q.Method != "hello" {
			return
		}
		reply(c, q, Hello{Version: 1, Epoch: "settings-fixture", Capabilities: map[string]uint64{settingsCapability: SettingsVersion}})
		q, err = readReq(r)
		if err != nil || q.Method != settingsGetMethod || len(q.Params) != 0 {
			t.Errorf("settings get = %+v, %v", q, err)
			return
		}
		reply(c, q, initial)
		q, err = readReq(r)
		if err != nil || q.Method != settingsSetMethod || q.Params["reasoning"] != "expanded" || q.Params["thinkingLevel"] != "low" {
			t.Errorf("settings set = %+v, %v", q, err)
			return
		}
		reply(c, q, changed)
	})
	client := NewClient(path, ClientOptions{Capabilities: map[string][]uint64{settingsCapability: {SettingsVersion}}})
	defer client.Close()
	got, err := client.Settings(ctx)
	if err != nil || !reflect.DeepEqual(got, initial) {
		t.Fatalf("settings = %+v, %v", got, err)
	}
	model := changed.Model
	got, err = client.ConfigureSettings(ctx, SettingsChange{Model: &model, ThinkingLevel: "low", Reasoning: ReasoningExpanded})
	if err != nil || !reflect.DeepEqual(got, changed) {
		t.Fatalf("changed settings = %+v, %v", got, err)
	}
	for _, change := range []SettingsChange{{}, {ThinkingLevel: "infinite"}, {Reasoning: "live"}} {
		if _, err := client.ConfigureSettings(ctx, change); !errors.Is(err, ErrProtocol) {
			t.Fatalf("invalid change %+v = %v", change, err)
		}
	}
}
