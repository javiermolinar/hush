package filelist

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func searchFixture(t *testing.T, names ...string) request {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("contents are not a search input"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var st unix.Stat_t
	if err := unix.Stat(root, &st); err != nil {
		t.Fatal(err)
	}
	return request{Root: root, Device: fmt.Sprint(st.Dev), Inode: fmt.Sprint(st.Ino), Query: "generator.go", SearchKey: "fixture", Limit: maxSearchResults}
}

func cachedSearch(t *testing.T, cache *searchCache, r request) (searchResult, error) {
	t.Helper()
	return cache.search(t.Context(), t.Context(), r)
}

func TestSearchNestedCheckouts(t *testing.T) {
	r := searchFixture(t, "tempo.fork/.git/config", "tempo.fork/modules/generator/generator.go",
		"another/generator.go", "generator.go.txt", "test-generator.go", "generator-a.go",
		".hidden/generator.go", "tempo.fork/node_modules/generator.go", "vendor/generator.go", "~/generator.go",
		"literal folder/générateur.go")
	var cache searchCache
	check := func(query string, expected ...searchMatch) {
		t.Helper()
		r.Query = query
		got, err := cachedSearch(t, &cache, r)
		if err != nil || got.Partial || !reflect.DeepEqual(got.Matches, expected) {
			t.Fatalf("search(%q): %+v, %v; want %v", query, got, err, expected)
		}
	}
	check("GENERATOR.GO", searchMatch{"another/generator.go", false}, searchMatch{"tempo.fork/modules/generator/generator.go", false},
		searchMatch{"generator.go.txt", false}, searchMatch{"test-generator.go", false}, searchMatch{"generator-a.go", false})
	check("tempo.fork generator.go", searchMatch{"tempo.fork/modules/generator/generator.go", false})
	check("GÉNÉRATEUR", searchMatch{"literal folder/générateur.go", false})
	check("tempo.fork", searchMatch{"tempo.fork", true})
	r.Limit = 1
	check("generator.go", searchMatch{"another/generator.go", false})
}

func TestSearchChecksEntryTypesAndRoot(t *testing.T) {
	r := searchFixture(t, "found/secret", "bad:name/secret")
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(r.Root, "symlink dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(r.Root, "linked secret")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(r.Root, "hardlinked secret")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(r.Root, "secret fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	r.Query = "secret"
	var cache searchCache
	got, err := cachedSearch(t, &cache, r)
	if err != nil || got.Partial || !reflect.DeepEqual(got.Matches, []searchMatch{{"found/secret", false}}) {
		t.Fatalf("search returned %+v, %v", got, err)
	}
	r.Inode += "0"
	if _, err := cachedSearch(t, &cache, r); err == nil {
		t.Fatal("search ran with a mismatched root")
	}
}

func TestSearchIncludesOnlyTopLevelGitHubDirectory(t *testing.T) {
	const workflow = "delete-old-tempo-releases.yaml"
	const path = ".github/workflows/" + workflow
	r := searchFixture(t, path, ".github/.hidden/"+workflow, ".git/"+workflow, "nested/.github/"+workflow)
	r.Query = workflow
	r.IncludeGitHub = true
	var cache searchCache
	got, err := cachedSearch(t, &cache, r)
	if err != nil || got.Partial || !reflect.DeepEqual(got.Matches, []searchMatch{{path, false}}) {
		t.Fatalf("GitHub search: %+v, %v", got, err)
	}
	r.IncludeGitHub = false
	if got, err := cachedSearch(t, &cache, r); err != nil || len(got.Matches) != 0 {
		t.Fatalf("legacy search reused the GitHub index: %+v, %v", got, err)
	}
}

func TestWarmSearchVerifiesNamesAndRefreshesIndex(t *testing.T) {
	r := searchFixture(t, "src/old.yaml", "folder/old.yaml")
	var cache searchCache
	check := func(query string, expected ...searchMatch) {
		t.Helper()
		r.Query = query
		got, err := cachedSearch(t, &cache, r)
		if err != nil || !slices.Equal(got.Matches, expected) {
			t.Fatalf("search(%q): %+v, %v; want %v", query, got, err, expected)
		}
	}
	check("old.yaml", searchMatch{"folder/old.yaml", false}, searchMatch{"src/old.yaml", false})
	first := cache.build
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "old.yaml"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(r.Root, "src/old.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(outside, "old.yaml"), filepath.Join(r.Root, "src/old.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(r.Root, "folder"), filepath.Join(r.Root, "renamed")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(r.Root, "folder")); err != nil {
		t.Fatal(err)
	}
	check("old") // neither the symlinked parent nor the hard link can escape revalidation
	if cache.build != first {
		t.Fatal("changing the query rebuilt the index")
	}
	r.SearchKey = "new grant"
	check("old", searchMatch{"renamed/old.yaml", false})
	if err := os.WriteFile(filepath.Join(r.Root, "src/new.yaml"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	check("new")
	cache.build.expires = time.Time{}
	check("new", searchMatch{"src/new.yaml", false})
}

func TestSearchReturnsPartialAtEntryBudget(t *testing.T) {
	r := searchFixture(t, "one", "two")
	root, err := openRoot(r.Root, r.Device, r.Inode)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(root)
	s := searchScan{ctx: t.Context(), deadline: time.Now().Add(5 * time.Second), seen: maxSearchEntries - 1}
	if _, err := s.visit(root, searchDir{}); err != nil || !s.partial || s.seen != maxSearchEntries {
		t.Fatalf("search didn't stop at entry budget: seen=%d partial=%t err=%v", s.seen, s.partial, err)
	}
}

func TestCancelledQueryLeavesSharedBuildForNextQuery(t *testing.T) {
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	key := searchKey{scope: "fixture"}
	b := &searchBuild{key: key, done: make(chan struct{}), cancel: func() { t.Error("same-key build was cancelled") }}
	cache := searchCache{build: b}
	stop()
	if _, err := cache.names(ctx, t.Context(), -1, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter: %v", err)
	}
	b.index = searchIndex{dirs: []searchDir{{path: "shared"}}}
	b.expires = time.Now().Add(searchCacheTTL)
	close(b.done)
	index, err := cache.names(t.Context(), t.Context(), -1, key)
	if err != nil || cache.build != b || len(index.dirs) != 1 || index.dirs[0].path != "shared" {
		t.Fatalf("next query did not reuse the shared build: %+v, %v", index, err)
	}
	if _, err := scanSearchIndex(ctx, -1, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scanner: %v", err)
	}
	if _, err := index.match(ctx, -1, "shared", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ranking: %v", err)
	}
}

func TestTenCharacterQueryUsesOneBuildOn100kFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("creates 100,000 fixture files")
	}
	const query = "qxdlpychmw"
	r := searchFixture(t, query+".txt")
	for batch := range 100 {
		path := filepath.Join(r.Root, fmt.Sprintf("batch-%02d", batch))
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		for n := range 1000 {
			if batch == 99 && n == 999 { // the root-level match is the 100,000th file
				break
			}
			if err := os.WriteFile(filepath.Join(path, fmt.Sprintf("bulk-%03d", n)), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	var warm, fresh searchCache
	var first *searchBuild
	for n := 1; n <= len(query); n++ {
		r.Query = query[:n]
		got, err := cachedSearch(t, &warm, r)
		if err != nil || !slices.Equal(got.Matches, []searchMatch{{query + ".txt", false}}) {
			t.Fatalf("search(%q): %+v, %v", r.Query, got, err)
		}
		if first == nil {
			first = warm.build
		}
		if warm.build != first {
			t.Fatalf("search(%q) walked the tree again", r.Query)
		}
		want, err := cachedSearch(t, &fresh, r)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("search(%q): reused %+v; fresh index %+v, %v", r.Query, got, want, err)
		}
	}
}
