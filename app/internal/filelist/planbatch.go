package filelist

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	maxPlanSetFiles  = 8
	maxPlanSetWrites = 4
)

type planBatchTarget struct {
	Path   string
	Before *fileVersion
	Text   *string // nil declares a read-only member, not a deletion
}
type planBatchFile struct {
	target       planBatchTarget
	parent, name string
	dir          int
	version      fileVersion
	meta         unix.Stat_t
	slot         string
	prepared     unix.Stat_t
}

func sameInode(a, b unix.Stat_t) bool { return a.Dev == b.Dev && a.Ino == b.Ino && a.Ino != 0 }

// Remove only entries with an identity we created or verified. Never do a
// recursive cleanup, and never unlink an unverified displaced file.
func removePlanSlot(dir int, name string, expected unix.Stat_t, flags int) bool {
	var current unix.Stat_t
	return unix.Fstatat(dir, name, &current, unix.AT_SYMLINK_NOFOLLOW) == nil && sameInode(current, expected) && unix.Unlinkat(dir, name, flags) == nil
}

func planBatch(r planFileRequest) planFileResult { return planBatchUsing(r, exchangePlanFile) }

// Validate the exact file exchanged as well as the installed proposal. The
// OS offers no compare-and-swap by file version; retain displaced content if
// any comparison fails instead of overwriting it with a second rename.
func planBatchUsing(r planFileRequest, exchange func(int, string, int, string) error) (result planFileResult) {
	failure := func(code string) planFileResult { return planFileResult{Error: code} }
	if !planRootPath(r.Root) || len(r.Batch) == 0 || len(r.Batch) > maxPlanSetFiles {
		return failure("scope")
	}
	writes := 0
	if r.Apply != nil {
		if len(r.Batch) != 1 || r.Batch[0].Text != nil || r.Batch[0].Before != nil {
			return failure("scope")
		}
		r.Batch[0].Before = &r.Apply.Before
		writes = 1
	}
	paths := make(map[string]bool, len(r.Batch))
	for _, target := range r.Batch {
		if !planFilePath(target.Path) || paths[target.Path] {
			return failure("scope")
		}
		paths[target.Path] = true
		if target.Text != nil {
			if !plainFileText(*target.Text, maxPlanFile) {
				return failure("replacement")
			}
			writes++
		}
	}
	if writes > maxPlanSetWrites {
		return failure("scope")
	}
	for _, target := range r.Batch {
		if (target.Before != nil) != (writes > 0) {
			return failure("scope")
		}
	}
	root, err := openRoot(r.Root, r.Device, r.Inode)
	if err != nil {
		return failure("scope")
	}
	defer unix.Close(root)
	lock := unix.LOCK_SH
	if writes > 0 {
		lock = unix.LOCK_EX
	}
	if unix.Flock(root, lock|unix.LOCK_NB) != nil {
		return failure("busy")
	}
	var files []planBatchFile
	defer func() {
		for _, f := range files {
			_ = unix.Close(f.dir)
		}
	}()
	seen := make(map[string]bool, len(r.Batch))
	results := make([]planFileResult, 0, len(r.Batch))
	for _, target := range r.Batch {
		parent, name := filepath.Split(target.Path)
		dir, err := unix.Openat(root, ".", openDir, 0)
		if err == nil {
			dir, err = descend(dir, strings.TrimSuffix(parent, "/"))
		}
		if err != nil {
			return failure("scope")
		}
		files = append(files, planBatchFile{target: target, parent: parent, name: name, dir: dir})
		f := &files[len(files)-1]
		text, st, err := readPlanFile(dir, name)
		if err != nil || !samePlanDirectory(r, dir, parent) {
			return failure("scope")
		}
		f.version, f.meta = planFileVersion(text, st), st
		if seen[f.version.Inode] {
			return failure("scope")
		}
		seen[f.version.Inode] = true
		if writes == 0 {
			results = append(results, planFileResult{Path: target.Path, File: &f.version, Text: &text})
			continue
		}
		if f.version != *target.Before {
			return failure("changed")
		}
		if a := r.Apply; a != nil {
			match := strings.Index(text, a.OldText)
			if !plainFileText(a.OldText, 8192) || a.OldText == "" || !plainFileText(a.NewText, 8192) ||
				!plainFileText(a.Contains, 640) || strings.TrimSpace(a.Contains) == "" || match < 0 || match != strings.LastIndex(text, a.OldText) {
				return failure("replacement")
			}
			next := strings.Replace(text, a.OldText, a.NewText, 1)
			if !plainFileText(next, maxPlanFile) || strings.Contains(text, a.Contains) || !strings.Contains(next, a.Contains) {
				return failure("check")
			}
			f.target.Text = &next
		} else if target.Text != nil && *target.Text == text {
			return failure("replacement")
		}
	}
	if writes == 0 {
		return planFileResult{Batch: results}
	}

	// Stage at the pinned root, not next to each destination. An exchange
	// across filesystems fails closed. The manifest maps slots back to paths.
	id := r.Stage
	if id == "" && r.Version == 0 {
		var bits [16]byte
		_, _ = rand.Read(bits[:])
		id = hex.EncodeToString(bits[:])
	}
	if decoded, err := hex.DecodeString(id); err != nil || len(decoded) != 16 || id != strings.ToLower(id) {
		return failure("protocol")
	}
	stageName := ".hush-plan-" + id
	if unix.Mkdirat(root, stageName, 0o700) != nil {
		return failure("publish")
	}
	stage, err := unix.Openat(root, stageName, openDir, 0)
	if err != nil {
		return failure("cleanup")
	}
	defer unix.Close(stage)
	var stageStat unix.Stat_t
	if unix.Fstat(stage, &stageStat) != nil || stageStat.Uid != uint32(unix.Geteuid()) || stageStat.Mode&0o777 != 0o700 {
		return failure("cleanup")
	}
	stageAtRoot := func() bool {
		var current unix.Stat_t
		return unix.Fstatat(root, stageName, &current, unix.AT_SYMLINK_NOFOLLOW) == nil &&
			sameInode(current, stageStat) && current.Mode&0o777 == 0o700
	}
	owned := make(map[string]unix.Stat_t, writes+1)
	published := false
	defer func() {
		// After a partial publication leave the entire stage in place. An
		// interruption can likewise leave it; the Run identifies the directory.
		if published && result.Error != "" {
			return
		}
		if !stageAtRoot() {
			if published {
				result = failure("partial")
			} else {
				result = failure("cleanup")
			}
			return
		}
		clean := true
		for name, identity := range owned {
			if !removePlanSlot(stage, name, identity, 0) {
				clean = false
			}
		}
		if !clean || !removePlanSlot(root, stageName, stageStat, unix.AT_REMOVEDIR) {
			if published {
				result = failure("partial")
			} else {
				result = failure("cleanup")
			}
		}
	}()
	manifest := make(map[string]string, writes)
	for i := range files {
		f := &files[i]
		if f.target.Text == nil {
			continue
		}
		f.slot = strconv.Itoa(i + 1)
		manifest[f.slot] = f.target.Path
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return failure("publish")
	}
	identity, err := writePlanStage(stage, "files.json", string(data)+"\n", nil)
	if identity.Ino != 0 {
		owned["files.json"] = identity
	}
	if err != nil {
		return failure("publish")
	}
	for i := range files {
		f := &files[i]
		if f.slot == "" {
			continue
		}
		f.prepared, err = writePlanStage(stage, f.slot, *f.target.Text, &f.meta)
		if f.prepared.Ino != 0 {
			owned[f.slot] = f.prepared
		}
		if err != nil {
			return failure("publish")
		}
	}
	if unix.Fsync(stage) != nil || unix.Fsync(root) != nil {
		return failure("publish")
	}
	matches := func(f planBatchFile, dir int, name string) bool {
		text, meta, err := readPlanFile(dir, name)
		return err == nil && meta.Mode == f.meta.Mode && meta.Gid == f.meta.Gid && planFileVersion(text, meta) == f.version
	}
	atPath := func(f planBatchFile) bool { return samePlanDirectory(r, f.dir, f.parent) }
	for _, f := range files {
		if !atPath(f) || !matches(f, f.dir, f.name) {
			return failure("changed")
		}
	}
	publishFailure := func(code string) planFileResult {
		if published {
			return failure("partial")
		}
		return failure(code)
	}
	for _, f := range files {
		if f.slot == "" {
			continue
		}
		if !atPath(f) || !matches(f, f.dir, f.name) {
			return publishFailure("changed")
		}
		stagedText, staged, err := readPlanFile(stage, f.slot)
		if err != nil || stagedText != *f.target.Text || !sameInode(staged, f.prepared) || !stageAtRoot() {
			return publishFailure("cleanup")
		}
		if err := exchange(stage, f.slot, f.dir, f.name); err != nil {
			if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EINVAL) {
				return publishFailure("exchange")
			}
			return publishFailure("publish")
		}
		published = true
		delete(owned, f.slot) // the slot no longer holds our proposal
		if unix.Fsync(stage) != nil || unix.Fsync(f.dir) != nil || !atPath(f) || !matches(f, stage, f.slot) {
			return failure("partial")
		}
	}
	for _, f := range files {
		if f.slot == "" {
			if !atPath(f) || !matches(f, f.dir, f.name) {
				return failure("partial")
			}
			results = append(results, planFileResult{Path: f.target.Path, File: &f.version})
			continue
		}
		text, st, err := readPlanFile(f.dir, f.name)
		if err != nil || text != *f.target.Text || !sameInode(st, f.prepared) || !atPath(f) || !matches(f, stage, f.slot) {
			return failure("partial")
		}
		owned[f.slot] = f.meta // verified displaced file, not our prepared inode
		version := planFileVersion(text, st)
		results = append(results, planFileResult{Path: f.target.Path, File: &version})
	}
	return planFileResult{Batch: results}
}
