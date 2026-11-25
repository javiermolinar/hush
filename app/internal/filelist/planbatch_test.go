package filelist

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPlanBatchRejectsChangedReadBeforePublishing(t *testing.T) {
	r := planFileFixture(t)
	paths := []string{"left.txt", "right.txt", "reference.txt"}
	for _, path := range paths {
		putPlanFile(t, filepath.Join(r.Root, path), "before\n")
		r.Batch = append(r.Batch, planBatchTarget{Path: path})
	}
	r.Path = ""
	snapshot := planBatch(r)
	if snapshot.Error != "" || len(snapshot.Batch) != len(paths) {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	after := "updated\n"
	for i := range r.Batch {
		r.Batch[i].Before = snapshot.Batch[i].File
		if i < 2 {
			r.Batch[i].Text = &after
		}
	}
	check := func(path, expected string) {
		t.Helper()
		if data, err := os.ReadFile(filepath.Join(r.Root, path)); err != nil || string(data) != expected {
			t.Fatalf("%s = %q, %v; want %q", path, data, err, expected)
		}
	}
	putPlanFile(t, filepath.Join(r.Root, paths[2]), "external change\n")
	if got := planBatch(r); got.Error != "changed" {
		t.Fatalf("stale read member: %+v", got)
	}
	check(paths[0], "before\n")
	check(paths[1], "before\n")
	check(paths[2], "external change\n")
	read := r
	read.Batch = []planBatchTarget{{Path: paths[2]}}
	r.Batch[2].Before = planBatch(read).Batch[0].File
	if got := planBatch(r); got.Error != "" || len(got.Batch) != 3 {
		t.Fatalf("valid publish: %+v", got)
	}
	check(paths[0], after)
	check(paths[1], after)
	check(paths[2], "external change\n")
	if files, err := os.ReadDir(r.Root); err != nil || len(files) != 3 {
		t.Fatalf("temporary file leaked: %v, %v", files, err)
	}
	if got := planBatch(r); got.Error != "changed" {
		t.Fatalf("replayed write: %+v", got)
	}
}

func TestPlanBatchRetainsDisplacedContentAfterConflict(t *testing.T) {
	r := planFileFixture(t)
	r.Stage = "0123456789abcdef0123456789abcdef"
	paths := []string{"first.txt", "second.txt", "third.txt"}
	for _, path := range paths {
		putPlanFile(t, filepath.Join(r.Root, path), "before\n")
		r.Batch = append(r.Batch, planBatchTarget{Path: path})
	}
	read := planBatch(r)
	if read.Error != "" || len(read.Batch) != 3 {
		t.Fatalf("snapshot: %+v", read)
	}
	for i := range r.Batch {
		text := "proposed\n"
		r.Batch[i].Before, r.Batch[i].Text = read.Batch[i].File, &text
	}
	count := 0
	got := planBatchUsing(r, func(from int, source string, to int, destination string) error {
		count++
		if count == 2 {
			putPlanFile(t, filepath.Join(r.Root, paths[1]), "external edit\n")
		}
		return exchangePlanFile(from, source, to, destination)
	})
	if count != 2 || got.Error != "partial" || got.Batch != nil {
		t.Fatalf("conflict was verified: %+v (%d swaps)", got, count)
	}
	stage := filepath.Join(r.Root, ".hush-plan-"+r.Stage)
	if st, err := os.Stat(stage); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("recovery directory: %v, %v", st, err)
	}
	for path, want := range map[string]string{
		filepath.Join(stage, "2"): "external edit\n", filepath.Join(stage, "1"): "before\n",
		filepath.Join(r.Root, paths[2]): "before\n",
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q (%v), want %q", path, got, err, want)
		}
	}
}
