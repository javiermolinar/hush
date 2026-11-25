package filelist

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const PlanFileArg = "--hush-internal-plan-file"

const maxPlanFile = 32 * 1024

var planPath = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)*$`)

type fileVersion struct {
	Hash  string `json:"hash"`
	Inode string `json:"inode"`
}

type replacement struct {
	Before                     fileVersion
	OldText, NewText, Contains string
}

const planFileProtocol = 2

type planFileRequest struct {
	Version                   int
	Stage                     string
	Root, Device, Inode, Path string
	Apply                     *replacement
	Batch                     []planBatchTarget
}

type planFileResult struct {
	Error   string           `json:"error,omitempty"`
	File    *fileVersion     `json:"file,omitempty"`
	Text    *string          `json:"text,omitempty"`
	Path    string           `json:"path,omitempty"`
	Version int              `json:"version,omitempty"`
	Batch   []planFileResult `json:"batch,omitempty"`
}

// RunPlanFile accepts one bounded request over the host's private helper pipes.
// There is no direct model access. Every open and publish is relative to an
// inode-checked directory descriptor; no component can follow a symlink.
func RunPlanFile(in io.Reader, out io.Writer) int {
	d := json.NewDecoder(io.LimitReader(in, 1024*1024))
	d.DisallowUnknownFields()
	var req planFileRequest
	if d.Decode(&req) != nil || d.Decode(new(any)) != io.EOF {
		return 2
	}
	result := planFileResult{Error: "protocol"}
	if req.Version == planFileProtocol && req.Root == "" && req.Path == "" && req.Apply == nil && req.Batch == nil && req.Stage == "" {
		result.Error = "" // capability probe; performs no filesystem operation
	} else if req.Version == planFileProtocol || req.Version == 0 && req.Batch == nil {
		if req.Batch == nil {
			result = planFile(req)
		} else if req.Path == "" && req.Apply == nil {
			result = planBatch(req)
		}
	}
	result.Version = planFileProtocol
	if err := json.NewEncoder(out).Encode(result); err != nil {
		return 2
	}
	return 0
}

func plainFileText(s string, limit int) bool {
	return len(s) <= limit && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// Single-file callers use the same publication and recovery path.
func planFile(r planFileRequest) planFileResult {
	r.Batch = []planBatchTarget{{Path: r.Path}}
	result := planBatch(r)
	if result.Error == "" && len(result.Batch) == 1 {
		return result.Batch[0]
	}
	return result
}

func planRootPath(root string) bool {
	return filepath.IsAbs(root) && filepath.Clean(root) == root && plainFileText(root, 4096)
}

func planFilePath(path string) bool {
	return len(path) <= 160 && strings.Count(path, "/") < 16 && planPath.MatchString(path)
}

func writePlanStage(dir int, name, text string, original *unix.Stat_t) (stat unix.Stat_t, err error) {
	fd, err := unix.Openat(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return stat, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() { err = errors.Join(err, file.Close()) }()
	// Record the identity even if writing or syncing the contents fails.
	if err = unix.Fstat(fd, &stat); err != nil {
		return stat, err
	}
	if _, err = io.WriteString(file, text); err != nil {
		return stat, err
	}
	if original != nil {
		if stat.Gid != original.Gid {
			if err = unix.Fchown(fd, -1, int(original.Gid)); err != nil {
				return stat, err
			}
		}
		if err = unix.Fchmod(fd, uint32(original.Mode)&0o777); err != nil {
			return stat, err
		}
	}
	return stat, file.Sync()
}

func planFileVersion(text string, st unix.Stat_t) fileVersion {
	return fileVersion{Hash: fmt.Sprintf("%x", sha256.Sum256([]byte(text))), Inode: fmt.Sprintf("%d:%d", st.Dev, st.Ino)}
}

func samePlanDirectory(r planFileRequest, dir int, parent string) bool {
	root, err := openRoot(r.Root, r.Device, r.Inode)
	if err != nil {
		return false
	}
	current, err := descend(root, strings.TrimSuffix(parent, "/"))
	if err != nil {
		return false
	}
	defer unix.Close(current)
	var a, b unix.Stat_t
	return unix.Fstat(dir, &a) == nil && unix.Fstat(current, &b) == nil && a.Dev == b.Dev && a.Ino == b.Ino
}

func readPlanFile(dir int, name string) (string, unix.Stat_t, error) {
	var st, listed, after unix.Stat_t
	unsafe := errors.New("not a bounded regular text file")
	if unix.Fstatat(dir, name, &listed, unix.AT_SYMLINK_NOFOLLOW) != nil || !safePlanFile(listed) {
		return "", st, unsafe
	}
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", st, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	if unix.Fstat(fd, &st) != nil || !safePlanFile(st) || st.Dev != listed.Dev || st.Ino != listed.Ino {
		return "", st, unsafe
	}
	// Do not drop user attributes when replacing the inode. Darwin may
	// attach its own nonportable provenance attribute to every created file.
	var attrs [256]byte
	if n, err := unix.Flistxattr(fd, attrs[:]); err != nil || n < 0 || n > len(attrs) ||
		n != 0 && (runtime.GOOS != "darwin" || string(attrs[:n]) != "com.apple.provenance\x00") {
		return "", st, unsafe
	}
	data, err := io.ReadAll(io.LimitReader(file, maxPlanFile+1))
	if err != nil || !plainFileText(string(data), maxPlanFile) || unix.Fstat(fd, &after) != nil ||
		!safePlanFile(after) || after.Size != int64(len(data)) || after.Mtim != st.Mtim || after.Ctim != st.Ctim {
		return "", st, unsafe
	}
	return string(data), st, nil
}

func safePlanFile(st unix.Stat_t) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFREG && st.Nlink == 1 && st.Size <= maxPlanFile &&
		int(st.Uid) == os.Geteuid() && st.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) == 0
}
