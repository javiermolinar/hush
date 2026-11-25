package filelist

import (
	"cmp"
	"io"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	maxSearchResults = 32
	maxSearchEntries = 100_000
	maxSearchDirs    = 8_192
	maxSearchDepth   = 32
	maxSearchPath    = 2048
	maxSearchTime    = 1500 * time.Millisecond
)

type searchMatch struct {
	Path      string `json:"path"`
	Directory bool   `json:"directory"`
}

type searchResult struct {
	Matches []searchMatch `json:"matches"`
	Partial bool          `json:"partial"`
}

type searchDir struct {
	path  string
	depth int
}

type rankedMatch struct {
	searchMatch
	rank, depth int
}

func allowedQuery(q string) bool {
	return len(q) <= 255 && utf8.ValidString(q) && strings.TrimSpace(q) != "" &&
		!strings.ContainsAny(q, "/\\\"@:") && strings.IndexFunc(q, unicode.IsControl) < 0
}

func nameRank(name, query string) int {
	switch {
	case name == query:
		return 0
	case strings.HasPrefix(name, query):
		return 1
	case strings.Contains(name, query):
		return 2
	}
	for _, r := range query {
		at := strings.IndexRune(name, r)
		if at < 0 {
			return -1
		}
		name = name[at+utf8.RuneLen(r):]
	}
	return 3
}

func compareMatches(a, b rankedMatch) int {
	return cmp.Or(cmp.Compare(a.rank, b.rank), cmp.Compare(a.depth, b.depth), strings.Compare(a.Path, b.Path))
}

type searchScan struct {
	query    string
	words    []string
	limit    int
	deadline time.Time
	seen     int
	partial  bool
	dirs     []searchDir
	best     []rankedMatch
}

func (s *searchScan) exhausted() bool {
	if s.seen >= maxSearchEntries || time.Now().After(s.deadline) {
		s.partial = true
		return true
	}
	return false
}

// Search breadth-first to reach neighboring checkouts before exploring deeper
// paths. Keep the verified root descriptor and open every component with
// O_NOFOLLOW, including when revisiting parent paths.
func search(r request) (searchResult, error) {
	root, err := openRoot(r.Root, r.Device, r.Inode)
	if err != nil {
		return searchResult{}, err
	}
	defer unix.Close(root)
	query := strings.ToLower(strings.TrimSpace(r.Query))
	s := searchScan{query: query, words: strings.Fields(query), limit: r.Limit, deadline: time.Now().Add(maxSearchTime), dirs: []searchDir{{}}}
	for i := 0; i < len(s.dirs) && !s.exhausted(); i++ {
		d := s.dirs[i]
		if err := s.visit(root, d); err != nil {
			if d.depth == 0 {
				return searchResult{}, err
			}
			s.partial = true
		}
	}
	result := searchResult{Matches: make([]searchMatch, 0, len(s.best)), Partial: s.partial}
	for _, hit := range s.best {
		result.Matches = append(result.Matches, hit.searchMatch)
	}
	return result, nil
}

func (s *searchScan) visit(root int, d searchDir) error {
	first, rest, _ := strings.Cut(d.path, "/")
	if first == "" {
		first = "."
	}
	fd, err := unix.Openat(root, first, openDir, 0)
	if err != nil {
		return err
	}
	fd, err = descend(fd, rest)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), "workspace search")
	defer dir.Close()
	prefix := d.path
	if prefix != "" {
		prefix += "/"
	}
	// A quoted query may qualify a basename with parent path terms, e.g.
	// @"tempo.fork generator.go". Still try the whole query as a filename.
	qualifier := ""
	if len(s.words) > 1 {
		parent := strings.ToLower(d.path)
		if !slices.ContainsFunc(s.words[:len(s.words)-1], func(word string) bool { return !strings.Contains(parent, word) }) {
			qualifier = s.words[len(s.words)-1]
		}
	}
	for !s.exhausted() {
		// Use directory entry types to avoid a stat per file. Matching entries
		// are verified by descriptor below; child directories are opened with
		// no-follow, never via the path returned to the caller.
		batch, err := dir.ReadDir(min(128, maxSearchEntries-s.seen))
		s.seen += len(batch)
		for _, item := range batch {
			name := item.Name()
			if !allowed(name) || d.depth == 0 && name == "~" || !item.IsDir() && !item.Type().IsRegular() {
				continue
			}
			if len(prefix)+len(name) > maxSearchPath {
				s.partial = true
				continue
			}
			if item.IsDir() && !skipSearchDir(name) {
				if d.depth+1 >= maxSearchDepth || len(s.dirs) >= maxSearchDirs {
					s.partial = true
				} else {
					s.dirs = append(s.dirs, searchDir{prefix + name, d.depth + 1})
				}
			}
			folded := strings.ToLower(name)
			rank := nameRank(folded, s.query)
			if rank < 0 && qualifier != "" {
				rank = nameRank(folded, qualifier)
			}
			if rank >= 0 {
				s.add(fd, name, rankedMatch{searchMatch{prefix + name, item.IsDir()}, rank, d.depth})
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func skipSearchDir(name string) bool {
	switch name {
	case "node_modules", "vendor", "dist", "target":
		return true
	}
	return false
}

func (s *searchScan) add(fd int, name string, match rankedMatch) {
	at, duplicate := slices.BinarySearchFunc(s.best, match, compareMatches)
	if duplicate || at >= s.limit {
		return
	}
	var st unix.Stat_t
	if unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return
	}
	kind := st.Mode & unix.S_IFMT
	if match.Directory && kind != unix.S_IFDIR || !match.Directory && (kind != unix.S_IFREG || st.Nlink != 1) {
		return
	}
	if len(s.best) == s.limit {
		s.best = s.best[:s.limit-1]
	}
	s.best = slices.Insert(s.best, at, match)
}
