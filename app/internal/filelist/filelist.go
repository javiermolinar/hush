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
	githubDir  = ".github"
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
	Root          string `json:"root"`
	Device        string `json:"device"`
	Inode         string `json:"inode"`
	Directory     string `json:"directory"`
	Query         string `json:"query,omitempty"`
	SearchKey     string `json:"searchKey,omitempty"` // opaque cache scope from the authorized host
	IncludeGitHub bool   `json:"includeGitHub,omitempty"`
	Offset        int    `json:"offset"`
	Limit         int    `json:"limit"`
}

type response struct {
	Page   *page         `json:"page,omitempty"`
	Search *searchResult `json:"search,omitempty"`
	Error  string        `json:"error,omitempty"`
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

func openRoot(root, device, inode string) (int, error) {
	fd, err := unix.Open("/", openDir, 0)
	if err != nil {
		return -1, err
	}
	fd, err = descend(fd, strings.TrimPrefix(root, "/"))
	if err != nil {
		return -1, err
	}
	var actual unix.Stat_t
	if err := unix.Fstat(fd, &actual); err != nil || device != fmt.Sprint(actual.Dev) || inode != fmt.Sprint(actual.Ino) {
		unix.Close(fd)
		return -1, errors.New("workspace root changed")
	}
	return fd, nil
}

func read(r request) (page, error) {
	fd, err := openRoot(r.Root, r.Device, r.Inode)
	if err != nil {
		return page{}, err
	}
	fd, err = descend(fd, r.Directory)
	if err != nil {
		return page{}, err
	}
	dir := os.NewFile(uintptr(fd), "workspace directory")
	defer dir.Close()

	// Bound total work, including hidden/invalid entries. List immediate
	// names only; a page never reads contents or descends.
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
			github := r.IncludeGitHub && r.Directory == "" && name == githubDir
			if !github && !allowed(name) {
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
				if !github && st.Nlink == 1 {
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
	offset := min(r.Offset, len(entries))
	end := min(offset+r.Limit, len(entries))
	return page{entries[offset:end], fmt.Sprintf("%x", hash.Sum(nil)), end < len(entries)}, nil
}

func (r request) valid() bool {
	if !filepath.IsAbs(r.Root) || r.Root != filepath.Clean(r.Root) || len(r.Root) > 4096 || strings.ContainsRune(r.Root, 0) ||
		len(r.Directory) > 2048 || strings.Count(r.Directory, "/") >= 32 || r.Offset < 0 || r.Offset > maxEntries ||
		r.Limit < 1 || r.Limit > 100 {
		return false
	}
	if r.Query != "" {
		return r.Directory == "" && r.Offset == 0 && r.Limit <= maxSearchResults && len(r.SearchKey) <= 64 && allowedQuery(r.Query)
	}
	if r.SearchKey != "" {
		return false
	}
	if r.Directory != "" {
		github := r.IncludeGitHub
		for name := range strings.SplitSeq(r.Directory, "/") {
			if !(github && name == githubDir) && !allowed(name) {
				return false
			}
			github = false // only the first component may be .github
		}
	}
	return true
}

// Run serves one private stream from the backend. It caches names, not trusted
// state: each call reopens the exact root and the backend rechecks grants.
func Run(in io.Reader, out io.Writer) int {
	lines, replies := bufio.NewScanner(in), json.NewEncoder(out)
	var cache searchCache
	for lines.Scan() {
		dec := json.NewDecoder(bytes.NewReader(lines.Bytes()))
		dec.DisallowUnknownFields()
		var r request
		if dec.Decode(&r) != nil || dec.Decode(&struct{}{}) != io.EOF || !r.valid() {
			return 2
		}
		var reply response
		var err error
		if r.Query != "" {
			var result searchResult
			result, err = cache.search(r)
			reply.Search = &result
		} else {
			var result page
			result, err = read(r)
			reply.Page = &result
		}
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
