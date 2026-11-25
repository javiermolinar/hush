package backendstart

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Keep disposable artifacts beside, not inside, the store. In particular a
// cache must not change the fresh-directory or format-1 cutover fences.
func compileCache(agent string) string {
	if os.Getenv("NODE_DISABLE_COMPILE_CACHE") == "1" || !makePrivateCacheDir(agent) {
		return ""
	}
	path := filepath.Join(agent, "backend-cache")
	if !makePrivateCacheDir(path) {
		return ""
	}
	return path
}

func makePrivateCacheDir(path string) bool {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return false
	}
	st, err := os.Lstat(path)
	return err == nil && st.Mode() == os.ModeDir|0o700 && ownedDirectory(st)
}

func ownedDirectory(st os.FileInfo) bool {
	owner, ok := st.Sys().(*syscall.Stat_t)
	return st.IsDir() && st.Mode().Perm()&0o022 == 0 && ok && owner.Uid == uint32(os.Getuid())
}

type cacheCleanup func(ready bool) error

func managedInvocation(bundle, node, version string, t target) (entry, cache string, cleanup cacheCleanup, err error) {
	if root := compileCache(t.agent); root != "" {
		if entry, cache, cleanup, err := cachedMaterialization(root, bundle, node, version); err == nil {
			return entry, cache, cleanup, nil
		}
		// A contender can still try the SQLite writer lock without a compile cache.
	}
	entry, release, err := materialize(bundle)
	if err != nil {
		return "", "", nil, err
	}
	return entry, "", func(bool) error { return release() }, nil
}

// Node keys compiled modules by path and verifies their source. Isolate each
// release/Node generation, but always extract fresh bytes into its launch slot.
func cachedMaterialization(root, bundle, node, version string) (entry, cache string, cleanup cacheCleanup, err error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", "", nil, err
	}
	keepLock := false
	defer func() {
		if !keepLock {
			_ = unix.Close(fd)
		}
	}()
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return "", "", nil, err
	}
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%s\x00", node, version, bundle)
	if bundle == embeddedBundle {
		_, _ = hash.Write(releaseBackend)
	}
	generation := fmt.Sprintf("gen-%x", hash.Sum(nil))
	cache = filepath.Join(root, generation)
	if !makePrivateCacheDir(cache) {
		return "", "", nil, errors.New("unsafe cache generation")
	}
	entry = bundle
	removeSlot := func() error { return nil }
	if bundle == embeddedBundle {
		slot := filepath.Join(cache, "launch")
		removeSlot = func() error { return os.RemoveAll(slot) }
		if err := removeSlot(); err != nil {
			return "", "", nil, err
		}
		if err := os.Mkdir(slot, 0o700); err != nil {
			return "", "", nil, err
		}
		entry, removeSlot, err = extractEmbedded(slot, removeSlot)
		if err != nil {
			return "", "", nil, err
		}
	}
	keepLock = true
	return entry, cache, func(ready bool) error {
		err := removeSlot()
		if ready {
			// A ready backend owns the managed writer lock and cannot race
			// another managed process still starting with an older cache.
			now := time.Now()
			_ = os.Chtimes(cache, now, now)
			pruneCompileCache(root, generation)
		}
		return errors.Join(err, unix.Close(fd))
	}, nil
}

var cacheGeneration = regexp.MustCompile(`^gen-[0-9a-f]{64}$`)
var legacyCache = regexp.MustCompile(`^(launch-[0-9a-f]{64}|v[0-9]+\.[0-9]+\.[0-9]+-[a-z0-9_]+-[0-9a-f]+-[0-9a-f]+)$`)

// Keep this generation and the two most recently used prior generations.
// Prune only recognized, owned directories. Bound deletion per successful
// launch so a large old cache cannot dominate normal restart time.
func pruneCompileCache(root, current string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	var generations, obsolete []os.FileInfo
	for _, entry := range entries {
		name := entry.Name()
		generation := cacheGeneration.MatchString(name)
		if name == current || !generation && !legacyCache.MatchString(name) {
			continue
		}
		info, err := entry.Info() // lstat: never follow a cache symlink
		if err != nil || !ownedDirectory(info) {
			continue
		}
		if generation {
			generations = append(generations, info)
		} else {
			obsolete = append(obsolete, info)
		}
	}
	slices.SortFunc(generations, func(a, b os.FileInfo) int { return b.ModTime().Compare(a.ModTime()) })
	obsolete = append(obsolete, generations[min(2, len(generations)):]...)
	for _, info := range obsolete[:min(8, len(obsolete))] {
		_ = os.RemoveAll(filepath.Join(root, info.Name()))
	}
}

func managedRuntimeEnv(cache string) []string {
	env := RuntimeEnv()
	out := env[:0]
	for _, variable := range env {
		if !strings.HasPrefix(variable, "NODE_COMPILE_CACHE=") {
			out = append(out, variable)
		}
	}
	if cache != "" {
		out = append(out, "NODE_COMPILE_CACHE="+cache)
	}
	return out
}
