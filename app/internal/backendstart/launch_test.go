package backendstart

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestColdStartRequirements(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	ctx := context.Background()
	if _, _, err := findNode(ctx); err == nil || !strings.Contains(err.Error(), "on PATH") {
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
		path, gotVersion, err := findNode(ctx)
		if (err == nil) != tt.ok || tt.ok && (path != node || gotVersion != tt.version) {
			t.Fatalf("Node %s = %q %q, %v, want usable=%t", tt.version, path, gotVersion, err, tt.ok)
		}
	}
}

func TestManagedCompileCacheIsPrivate(t *testing.T) {
	t.Setenv("NODE_COMPILE_CACHE", "/do-not-use-inherited-cache")
	t.Setenv("NODE_DISABLE_COMPILE_CACHE", "")
	agent := filepath.Join(t.TempDir(), ".hush")
	cache := compileCache(agent)
	if cache != filepath.Join(agent, "backend-cache") {
		t.Fatalf("cache = %q", cache)
	}
	for _, path := range []string{agent, cache} {
		st, err := os.Lstat(path)
		if err != nil || st.Mode() != os.ModeDir|0o700 {
			t.Fatalf("not a private directory: %s (%v, %v)", path, st, err)
		}
	}
	if err := os.Remove(cache); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, cache); err != nil {
		t.Fatal(err)
	}
	if got := compileCache(agent); got != "" {
		t.Fatalf("accepted symlink cache %q", got)
	}
	if env := strings.Join(managedRuntimeEnv(""), "\n"); strings.Contains(env, "NODE_COMPILE_CACHE=") {
		t.Fatal("unsafe default cache must not fall back to the inherited path")
	}
	t.Setenv("NODE_DISABLE_COMPILE_CACHE", "1")
	unused := filepath.Join(outside, "disabled")
	if compileCache(unused) != "" {
		t.Fatal("cached when disabled")
	}
	if _, err := os.Lstat(unused); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache disable created an artifact: %v", err)
	}
}

func TestStableExtractionIsExclusiveAndFresh(t *testing.T) {
	root := t.TempDir()
	start := func(version string) (string, string, cacheCleanup, error) {
		return cachedMaterialization(root, embeddedBundle, "/path/to/node", version)
	}
	entry, cache, cleanup, err := start("v22.19.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, release, err := start("v22.20.0"); err == nil {
		_ = release(false)
		t.Error("concurrent launcher took an occupied extraction slot")
	}
	if err := cleanup(true); err != nil {
		t.Fatal(err)
	}
	// Model a stale extraction left by SIGKILL. Never reuse its bytes.
	if err := os.Mkdir(filepath.Dir(entry), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("stale"), 0o400); err != nil {
		t.Fatal(err)
	}
	again, sameCache, done, err := start("v22.19.0")
	if err != nil {
		t.Fatal(err)
	}
	if again != entry || cache != sameCache {
		t.Fatalf("entry path changed: %q, %q", entry, again)
	}
	got, err := os.ReadFile(again)
	if err != nil || string(got) != string(releaseBackend) {
		t.Fatal("did not write fresh embedded bytes", err)
	}
	if err := done(true); err != nil {
		t.Fatal(err)
	}
	newEntry, newCache, done, err := start("v22.20.0")
	if err != nil {
		t.Fatal(err)
	}
	defer done(true)
	if newEntry == entry || newCache == cache {
		t.Fatal("different Node versions shared a generation")
	}
}

func TestPruneCompileCache(t *testing.T) {
	root := t.TempDir()
	current := "gen-" + strings.Repeat("0", 64)
	makeDir := func(name string, date time.Time) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, date, date); err != nil {
			t.Fatal(err)
		}
	}
	old := "gen-" + strings.Repeat("1", 64)
	prior := "gen-" + strings.Repeat("2", 64)
	recent := "gen-" + strings.Repeat("3", 64)
	legacy := "v22.19.0-arm64-2b4477fa-501"
	staleSlot := "launch-" + strings.Repeat("1", 64)
	link := "gen-" + strings.Repeat("f", 64)
	for i, name := range []string{current, old, prior, recent, legacy, staleSlot, "not-a-cache"} {
		makeDir(name, time.Unix(int64(i), 0))
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, link)); err != nil {
		t.Fatal(err)
	}
	pruneCompileCache(root, current)
	for _, name := range []string{current, prior, recent, "not-a-cache", link} {
		if _, err := os.Lstat(filepath.Join(root, name)); err != nil {
			t.Errorf("deleted %s: %v", name, err)
		}
	}
	for _, name := range []string{old, legacy, staleSlot} {
		if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("left %s: %v", name, err)
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
		{"{\"error\":\"wasm_pack\"}\n", true, false},
		{"{\"error\":\"models_configuration\"}\n", true, false},
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
