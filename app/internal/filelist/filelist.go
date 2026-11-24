// Package filelist is the backend's bounded, descriptor-relative file picker.
package filelist

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	HelperArg  = "--hush-internal-file-list"
	HelperEnv  = "HUSH_FILE_LIST_HELPER"
	maxEntries = 4096
)

type entry struct {
	Name      string `json:"name"`
	Directory bool   `json:"directory"`
}

type page struct {
	Entries []entry `json:"entries"`
	Stamp   string  `json:"stamp"`
	More    bool    `json:"more"`
}

type request struct {
	Root      string `json:"root"`
	Device    string `json:"device"`
	Inode     string `json:"inode"`
	Directory string `json:"directory"`
	Offset    int    `json:"offset"`
	Limit     int    `json:"limit"`
}

type response struct {
	Page  *page  `json:"page,omitempty"`
	Error string `json:"error,omitempty"`
}

var errLimit = errors.New("directory listing limit")

func allowed(name string) bool {
	return name != "" && len(name) <= 255 && utf8.ValidString(name) && !strings.HasPrefix(name, ".") &&
		!strings.ContainsAny(name, "/\\\"@:") && strings.IndexFunc(name, unicode.IsControl) == -1
}

const openDir = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK

// Each component is opened relative to the preceding descriptor. In particular
// neither an intermediate symlink swap nor a renamed workspace can redirect an
// already-authorized root descriptor outside that root.
func descend(fd int, p string) (int, error) {
	if p == "" {
		return fd, nil
	}
	for component := range strings.SplitSeq(p, "/") {
		next, err := unix.Openat(fd, component, openDir, 0)
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

func read(root, device, inode, relative string, offset, limit int) (page, error) {
	fd, err := unix.Open("/", openDir, 0)
	if err != nil {
		return page{}, err
	}
	fd, err = descend(fd, strings.TrimPrefix(root, "/"))
	if err != nil {
		return page{}, err
	}
	var actual unix.Stat_t
	if err := unix.Fstat(fd, &actual); err != nil || device != fmt.Sprint(actual.Dev) || inode != fmt.Sprint(actual.Ino) {
		unix.Close(fd)
		return page{}, errors.New("workspace root changed")
	}
	fd, err = descend(fd, relative)
	if err != nil {
		return page{}, err
	}
	dir := os.NewFile(uintptr(fd), "workspace directory")
	defer dir.Close()

	// Bound total work, including hidden/invalid entries. Recurse only on
	// a later explicit directory selection; list names, never contents.
	entries := make([]entry, 0)
	seen := 0
	for {
		batch, err := dir.Readdirnames(64)
		if err != nil && err != io.EOF {
			return page{}, err
		}
		seen += len(batch)
		if seen > maxEntries {
			return page{}, errLimit
		}
		for _, name := range batch {
			if !allowed(name) {
				continue
			}
			var st unix.Stat_t
			if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				continue
			}
			switch st.Mode & unix.S_IFMT {
			case unix.S_IFDIR:
				entries = append(entries, entry{name, true})
			case unix.S_IFREG:
				if st.Nlink == 1 {
					entries = append(entries, entry{name, false})
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	slices.SortFunc(entries, func(a, b entry) int {
		if a.Directory != b.Directory {
			if a.Directory {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	var identity unix.Stat_t
	if err := unix.Fstat(fd, &identity); err != nil {
		return page{}, err
	}
	hash := sha256.New()
	// Bind later pages to the directory inode and the observed listing.
	_ = json.NewEncoder(hash).Encode([]any{identity.Dev, identity.Ino, entries})
	offset = min(offset, len(entries))
	end := min(offset+limit, len(entries))
	return page{entries[offset:end], fmt.Sprintf("%x", hash.Sum(nil)), end < len(entries)}, nil
}

func (r request) valid() bool {
	if !filepath.IsAbs(r.Root) || r.Root != filepath.Clean(r.Root) || len(r.Root) > 4096 || strings.ContainsRune(r.Root, 0) ||
		len(r.Directory) > 2048 || strings.Count(r.Directory, "/") >= 32 || r.Offset < 0 || r.Offset > maxEntries ||
		r.Limit < 1 || r.Limit > 100 {
		return false
	}
	if r.Directory != "" {
		for name := range strings.SplitSeq(r.Directory, "/") {
			if !allowed(name) {
				return false
			}
		}
	}
	return true
}

// Run serves one private stream from the backend. It caches no trusted state:
// every call reopens the exact root by descriptor; the backend rechecks grants.
func Run(in io.Reader, out io.Writer) int {
	lines, replies := bufio.NewScanner(in), json.NewEncoder(out)
	for lines.Scan() {
		dec := json.NewDecoder(bytes.NewReader(lines.Bytes()))
		dec.DisallowUnknownFields()
		var r request
		if dec.Decode(&r) != nil || dec.Decode(&struct{}{}) != io.EOF || !r.valid() {
			return 2
		}
		result, err := read(r.Root, r.Device, r.Inode, r.Directory, r.Offset, r.Limit)
		reply := response{Page: &result}
		if errors.Is(err, errLimit) {
			reply = response{Error: "limit"}
		} else if err != nil {
			reply = response{Error: "not_found"}
		}
		if replies.Encode(reply) != nil {
			return 3
		}
	}
	if lines.Err() != nil {
		return 2
	}
	return 0
}
