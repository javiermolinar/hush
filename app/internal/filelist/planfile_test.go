package filelist

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func planFileFixture(t *testing.T) planFileRequest {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Stat(root, &st); err != nil {
		t.Fatal(err)
	}
	return planFileRequest{Root: root, Device: fmt.Sprint(st.Dev), Inode: fmt.Sprint(st.Ino), Path: "values.txt"}
}

func putPlanFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestPlanFilePublishesOnlyVerifiedReplacement(t *testing.T) {
	r := planFileFixture(t)
	path := filepath.Join(r.Root, r.Path)
	putPlanFile(t, path, "theme=latte\n")
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	before := planFile(r)
	if before.File == nil || before.Text == nil || *before.Text != "theme=latte\n" {
		t.Fatalf("preflight: %+v", before)
	}
	check := func(want string) {
		t.Helper()
		if b, err := os.ReadFile(path); err != nil || string(b) != want {
			t.Fatalf("file = %q, %v; want %q", b, err, want)
		}
	}
	r.Apply = &replacement{Before: *before.File, OldText: "theme=latte", NewText: "theme=dark", Contains: "theme=mocha"}
	if got := planFile(r); got.Error != "check" {
		t.Fatalf("bad check: %+v", got)
	}
	check("theme=latte\n")
	r.Apply.NewText = "theme=mocha"
	if got := planFile(r); got.File == nil || got.Error != "" || got.File.Hash == before.File.Hash {
		t.Fatalf("valid edit: %+v", got)
	}
	check("theme=mocha\n")
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("permissions not preserved: %v, %v", st, err)
	}
	if got := planFile(r); got.Error != "changed" {
		t.Fatalf("replayed write: %+v", got)
	}
	files, err := os.ReadDir(r.Root)
	if err != nil || len(files) != 1 {
		t.Fatalf("temporary file leaked: %v, %v", files, err)
	}
}

func TestPlanFileRejectsAliasesAndChangedTargets(t *testing.T) {
	r := planFileFixture(t)
	outside := filepath.Join(t.TempDir(), "values.txt")
	putPlanFile(t, outside, "theme=latte\n")
	path := filepath.Join(r.Root, r.Path)
	putPlanFile(t, path, "theme=latte\n")
	before := planFile(r)
	if before.File == nil {
		t.Fatalf("preflight: %+v", before)
	}
	r.Apply = &replacement{Before: *before.File, OldText: "theme=latte", NewText: "theme=mocha", Contains: "theme=mocha"}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(r.Root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(r.Root, "hard.txt")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"link/values.txt", "hard.txt", "../values.txt", ".git/config", outside} {
		alias := r
		alias.Path = p
		if got := planFile(alias); got.Error != "scope" {
			t.Fatalf("allowed %s: %+v", p, got)
		}
	}
	wrong := r
	wrong.Inode = "0"
	if got := planFile(wrong); got.Error != "scope" {
		t.Fatalf("allowed replaced workspace: %+v", got)
	}
	putPlanFile(t, path, "theme=user-owned\n")
	if got := planFile(r); got.Error != "changed" {
		t.Fatalf("allowed stale edit: %+v", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if got := planFile(r); got.Error != "scope" {
		t.Fatalf("followed a symlink: %+v", got)
	}
	if b, err := os.ReadFile(outside); string(b) != "theme=latte\n" || err != nil {
		t.Fatalf("outside file changed: %q, %v", b, err)
	}
}
