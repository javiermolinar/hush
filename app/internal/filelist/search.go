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
	maxRankTime      = 200 * time.Millisecond
	searchCacheTTL   = 5 * time.Second
)

type searchMatch struct {
	Path      string `json:"path"`
	Directory bool   `json:"directory"`
}

type searchResult struct {
	Matches []searchMatch `json:"matches"`
	Partial bool          `json:"partial"`
}

type searchName struct {
	name, folded string
	directory    bool
}

type searchDir struct {
	path    string
	depth   int
	entries []searchName
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

type searchIndex struct {
	dirs    []searchDir
	partial bool
}

type searchKey struct {
	root, device, inode, scope string
	includeGitHub              bool
}

// Cache at most one bounded index, with no open descriptors or grants. A peer
// can change its query without walking the tree again. A different grant, root,
// or visibility policy cannot reuse its names.
type searchCache struct {
	key     searchKey
	expires time.Time
	index   searchIndex
}

func (c *searchCache) search(r request) (searchResult, error) {
	root, err := openRoot(r.Root, r.Device, r.Inode)
	if err != nil {
		return searchResult{}, err
	}
	defer unix.Close(root)
	key := searchKey{r.Root, r.Device, r.Inode, r.SearchKey, r.IncludeGitHub}
	if c.key == key && time.Now().Before(c.expires) {
		return c.index.match(root, r.Query, r.Limit), nil
	}
	*c = searchCache{}
	index, err := scanSearchIndex(root, r.IncludeGitHub)
	if err != nil {
		return searchResult{}, err
	}
	if r.SearchKey != "" {
		*c = searchCache{key, time.Now().Add(searchCacheTTL), index}
	}
	return index.match(root, r.Query, r.Limit), nil
}

type searchScan struct {
	searchIndex
	includeGitHub bool
	deadline      time.Time
	seen          int
}

func (s *searchScan) exhausted() bool {
	if s.seen >= maxSearchEntries || time.Now().After(s.deadline) {
		s.partial = true
		return true
	}
	return false
}

// Walk breadth-first so shallow paths are scanned before deep ones when the
// budget runs out. Every directory is opened relative to the verified root.
func scanSearchIndex(root int, includeGitHub bool) (searchIndex, error) {
	s := searchScan{searchIndex: searchIndex{dirs: []searchDir{{}}}, includeGitHub: includeGitHub, deadline: time.Now().Add(maxSearchTime)}
	for i := 0; i < len(s.dirs) && !s.exhausted(); i++ {
		entries, err := s.visit(root, s.dirs[i])
		s.dirs[i].entries = entries
		if err != nil {
			if i == 0 {
				return searchIndex{}, err
			}
			s.partial = true
		}
	}
	return s.searchIndex, nil
}

func openSearchDir(root int, path string) (int, error) {
	first, rest, _ := strings.Cut(path, "/")
	if first == "" {
		first = "."
	}
	fd, err := unix.Openat(root, first, openDir, 0)
	if err != nil {
		return -1, err
	}
	return descend(fd, rest)
}

func (s *searchScan) visit(root int, d searchDir) ([]searchName, error) {
	fd, err := openSearchDir(root, d.path)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), "workspace search")
	defer dir.Close()
	prefix := d.path
	if prefix != "" {
		prefix += "/"
	}
	var names []searchName
	for !s.exhausted() {
		// Use entry types instead of statting every file. Verify matches again
		// by descriptor just before returning them, even on a cold search.
		batch, err := dir.ReadDir(min(128, maxSearchEntries-s.seen))
		s.seen += len(batch)
		for _, item := range batch {
			name, isDir := item.Name(), item.IsDir()
			github := s.includeGitHub && d.depth == 0 && name == githubDir && isDir
			if !github && !allowed(name) || d.depth == 0 && name == "~" || !isDir && !item.Type().IsRegular() {
				continue
			}
			if len(prefix)+len(name) > maxSearchPath {
				s.partial = true
				continue
			}
			if isDir && !skipSearchDir(name) {
				if d.depth+1 >= maxSearchDepth || len(s.dirs) >= maxSearchDirs {
					s.partial = true
				} else {
					s.dirs = append(s.dirs, searchDir{path: prefix + name, depth: d.depth + 1})
				}
			}
			names = append(names, searchName{name, strings.ToLower(name), isDir})
		}
		if err == io.EOF {
			return names, nil
		}
		if err != nil {
			return names, err
		}
	}
	return names, nil
}

func skipSearchDir(name string) bool {
	switch name {
	case "node_modules", "vendor", "dist", "target":
		return true
	}
	return false
}

type searchRanking struct {
	query    string
	words    []string
	limit    int
	deadline time.Time
	best     []rankedMatch
	partial  bool
}

func (index searchIndex) match(root int, query string, limit int) searchResult {
	query = strings.ToLower(strings.TrimSpace(query))
	r := searchRanking{query: query, words: strings.Fields(query), limit: limit, deadline: time.Now().Add(maxRankTime), partial: index.partial}
	for _, dir := range index.dirs {
		if !r.matchDir(root, dir) {
			r.partial = true
			break
		}
	}
	result := searchResult{Matches: make([]searchMatch, 0, len(r.best)), Partial: r.partial}
	for _, hit := range r.best {
		result.Matches = append(result.Matches, hit.searchMatch)
	}
	return result
}

func (r *searchRanking) matchDir(root int, d searchDir) bool {
	if len(d.entries) == 0 {
		return true
	}
	prefix := d.path
	if prefix != "" {
		prefix += "/"
	}
	// A quoted query may qualify a basename with parent path terms, e.g.
	// @"tempo.fork generator.go". Still try the whole query as a filename.
	qualifier := ""
	if len(r.words) > 1 {
		parent := strings.ToLower(d.path)
		if !slices.ContainsFunc(r.words[:len(r.words)-1], func(word string) bool { return !strings.Contains(parent, word) }) {
			qualifier = r.words[len(r.words)-1]
		}
	}
	fd := -1
	for i, item := range d.entries {
		if i%128 == 0 && time.Now().After(r.deadline) {
			return false
		}
		rank := nameRank(item.folded, r.query)
		if rank < 0 && qualifier != "" {
			rank = nameRank(item.folded, qualifier)
		}
		if rank < 0 {
			continue
		}
		if len(r.best) == r.limit {
			last := r.best[len(r.best)-1]
			if rank > last.rank || rank == last.rank && d.depth > last.depth {
				continue
			}
		}
		match := rankedMatch{searchMatch{prefix + item.name, item.directory}, rank, d.depth}
		at, duplicate := slices.BinarySearchFunc(r.best, match, compareMatches)
		if duplicate || at >= r.limit {
			continue
		}
		if fd < 0 {
			var err error
			fd, err = openSearchDir(root, d.path)
			if err != nil {
				r.partial = true
				return true
			}
			defer unix.Close(fd)
		}
		var st unix.Stat_t
		if unix.Fstatat(fd, item.name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
			continue
		}
		kind := st.Mode & unix.S_IFMT
		if item.directory && kind != unix.S_IFDIR || !item.directory && (kind != unix.S_IFREG || st.Nlink != 1) {
			continue
		}
		if len(r.best) == r.limit {
			r.best = r.best[:r.limit-1]
		}
		r.best = slices.Insert(r.best, at, match)
	}
	return true
}
