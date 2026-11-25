package backendstart

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestServiceStatusJSONStates(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	status := func() ServiceStatus {
		t.Helper()
		var out bytes.Buffer
		if err := ServiceStatusJSON(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		var s ServiceStatus
		if err := json.Unmarshal(out.Bytes(), &s); err != nil || s.Attention == nil {
			t.Fatalf("decode %q: %+v, %v", out.String(), s, err)
		}
		return s
	}
	if s := status(); s.State != "not_installed" || s.Log != "" || s.Sessions != nil {
		t.Fatalf("fresh profile: %+v", s)
	}
	// An installed service is recorded by its claim; nothing answers the socket.
	agent := filepath.Join(home, ".hush")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agent, serviceClaim), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if installed, err := serviceInstalled(agent); err != nil || !installed {
		t.Skipf("fixture claim not recognized as installed (%v); covered by app/test/service.test.mjs", err)
	}
	if s := status(); s.State != "unavailable" || s.Log != filepath.Join(agent, "supervisor.log") {
		t.Fatalf("installed without a backend: %+v", s)
	}
	if err := os.WriteFile(filepath.Join(agent, serviceHold), []byte("paused\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := status(); s.State != "paused" {
		t.Fatalf("held service: %+v", s)
	}
}
