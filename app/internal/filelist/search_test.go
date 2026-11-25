package filelist

import (
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

func TestSearchNestedCheckouts(t *testing.T) {
	r := searchFixture(t, "tempo.fork/.git/config", "tempo.fork/modules/generator/generator.go",
		"another/generator.go", "generator.go.txt", "test-generator.go", "generator-a.go",
		".hidden/generator.go", "tempo.fork/node_modules/generator.go", "vendor/generator.go", "~/generator.go",
		"literal folder/générateur.go")
	var cache searchCache
	check := func(query string, expected ...searchMatch) {
		t.Helper()
		r.Query = query
		got, err := cache.search(r)
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
	got, err := cache.search(r)
	if err != nil || got.Partial || !reflect.DeepEqual(got.Matches, []searchMatch{{"found/secret", false}}) {
		t.Fatalf("search returned %+v, %v", got, err)
	}
	r.Inode += "0"
	if _, err := cache.search(r); err == nil {
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
	got, err := cache.search(r)
	if err != nil || got.Partial || !reflect.DeepEqual(got.Matches, []searchMatch{{path, false}}) {
		t.Fatalf("GitHub search: %+v, %v", got, err)
	}
	r.IncludeGitHub = false
	if got, err := cache.search(r); err != nil || len(got.Matches) != 0 {
		t.Fatalf("legacy search reused the GitHub index: %+v, %v", got, err)
	}
}

func TestWarmSearchVerifiesNamesAndRefreshesIndex(t *testing.T) {
	r := searchFixture(t, "src/old.yaml", "folder/old.yaml")
	var cache searchCache
	check := func(query string, expected ...searchMatch) {
		t.Helper()
		r.Query = query
		got, err := cache.search(r)
		if err != nil || !slices.Equal(got.Matches, expected) {
			t.Fatalf("search(%q): %+v, %v; want %v", query, got, err, expected)
		}
	}
	check("old.yaml", searchMatch{"folder/old.yaml", false}, searchMatch{"src/old.yaml", false})
	expires := cache.expires
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
	if !cache.expires.Equal(expires) {
		t.Fatal("changing the query rebuilt the index")
	}
	r.SearchKey = "new grant"
	check("old", searchMatch{"renamed/old.yaml", false})
	if err := os.WriteFile(filepath.Join(r.Root, "src/new.yaml"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	check("new")
	cache.expires = time.Time{}
	check("new", searchMatch{"src/new.yaml", false})
}

func TestSearchReturnsPartialAtEntryBudget(t *testing.T) {
	r := searchFixture(t, "one", "two")
	root, err := openRoot(r.Root, r.Device, r.Inode)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(root)
	s := searchScan{deadline: time.Now().Add(5 * time.Second), seen: maxSearchEntries - 1}
	if _, err := s.visit(root, searchDir{}); err != nil || !s.partial || s.seen != maxSearchEntries {
		t.Fatalf("search didn't stop at entry budget: seen=%d partial=%t err=%v", s.seen, s.partial, err)
	}
}
