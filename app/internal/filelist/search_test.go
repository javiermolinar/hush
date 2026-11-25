package filelist

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	return request{Root: root, Device: fmt.Sprint(st.Dev), Inode: fmt.Sprint(st.Ino), Query: "generator.go", Limit: maxSearchResults}
}

func TestSearchNestedCheckouts(t *testing.T) {
	r := searchFixture(t, "tempo.fork/.git/config", "tempo.fork/modules/generator/generator.go",
		"another/generator.go", "generator.go.txt", "test-generator.go", "generator-a.go",
		".hidden/generator.go", "tempo.fork/node_modules/generator.go", "vendor/generator.go", "~/generator.go",
		"literal folder/générateur.go")
	check := func(query string, expected ...searchMatch) {
		t.Helper()
		r.Query = query
		got, err := search(r)
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
	got, err := search(r)
	if err != nil || got.Partial || !reflect.DeepEqual(got.Matches, []searchMatch{{"found/secret", false}}) {
		t.Fatalf("search returned %+v, %v", got, err)
	}
	r.Inode += "0"
	if _, err := search(r); err == nil {
		t.Fatal("search ran with a mismatched root")
	}
}

func TestSearchReturnsPartialAtEntryBudget(t *testing.T) {
	r := searchFixture(t, "one", "two")
	root, err := openRoot(r.Root, r.Device, r.Inode)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(root)
	s := searchScan{query: "missing", limit: 1, deadline: time.Now().Add(5 * time.Second), seen: maxSearchEntries - 1}
	if err := s.visit(root, searchDir{}); err != nil || !s.partial || s.seen != maxSearchEntries {
		t.Fatalf("search didn't stop at entry budget: seen=%d partial=%t err=%v", s.seen, s.partial, err)
	}
}
