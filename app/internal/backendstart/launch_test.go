package backendstart

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestColdStartRequirements(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	ctx := context.Background()
	if _, err := findNode(ctx); err == nil || !strings.Contains(err.Error(), "on PATH") {
		t.Fatalf("missing Node = %v", err)
	}
	node := filepath.Join(dir, "node")
	for _, tt := range []struct {
		version string
		ok      bool
	}{
		{"v22.18.9", false}, {"v22.19.0", true}, {"v26.0.0", true}, {"unexpected", false},
	} {
		if err := os.WriteFile(node, []byte("#!/bin/sh\necho "+tt.version+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		path, err := findNode(ctx)
		if (err == nil) != tt.ok || tt.ok && path != node {
			t.Fatalf("Node %s = %q, %v, want usable=%t", tt.version, path, err, tt.ok)
		}
	}
}

func TestStartupRecord(t *testing.T) {
	for _, tt := range []struct {
		record  string
		wantBad bool
		locked  bool
	}{
		{"{\"ready\":true,\"socket\":\"/local/hush.sock\"}\n", false, false},
		{"{\"error\":\"data_locked\"}\n", true, true},
		{"{\"error\":\"startup_failed\"}\n", true, false},
		{"{\"ready\":true,\"socket\":\"/wrong\"}\n", true, false},
		{"{\"ready\":true,\"socket\":\"/local/hush.sock\"}", true, false},
		{strings.Repeat("x", 5000) + "\n", true, false},
	} {
		err := readStartup(strings.NewReader(tt.record), "/local/hush.sock")
		if (err != nil) != tt.wantBad || errors.Is(err, errContended) != tt.locked {
			t.Fatalf("startup error = %v, want err=%t lock=%t", err, tt.wantBad, tt.locked)
		}
	}
}
