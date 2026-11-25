package sdk

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ThreadDirectoryVersion       = 1 // legacy titles and recency
	ThreadManagementVersion      = 2 // status, checkout ownership, and archive visibility
	ThreadWorkspaceVersion       = 1 // server-side workspace scope on a v2 directory
	ThreadWorkspaceSearchVersion = 2 // named workspaces and directory-wide search
	threadDirectoryList          = "ext.threadDirectory.list"
	threadDirectoryGet           = "ext.threadDirectory.get"
	threadDirectoryNew           = "ext.threadDirectory.new"
	threadDirectoryArchive       = "ext.threadDirectory.archive"
)

var directoryCursor = regexp.MustCompile(`^[A-Za-z0-9_-]{1,512}$`)

// ThreadDirectoryRow is the optional index of an ownerless thread's display
// metadata. Unknown names and dates are absent, never inferred from its ID.
type ThreadDirectoryRow struct {
	ID            ID              `json:"id"`
	ParentID      ID              `json:"parentId,omitempty"`
	Title         string          `json:"title,omitempty"`
	LastMessageMS uint64          `json:"lastMessageMs,omitempty"`
	Status        string          `json:"status,omitempty"`
	Archived      bool            `json:"archived"`
	Checkout      *ThreadCheckout `json:"checkout,omitempty"`
}

// ThreadCheckout describes the host's recorded allocation, not a guarantee
// of a clean checkout or permission to restore files.
type ThreadCheckout struct {
	Kind   string `json:"kind"` // shared or managed
	CWD    string `json:"cwd,omitempty"`
	Branch string `json:"branch,omitempty"`
}

type ThreadDirectoryPage struct {
	Threads    []ThreadDirectoryRow `json:"threads"`
	NextCursor *string              `json:"nextCursor"`
	// Indexing means the server returned a partial first page. Start a
	// fresh listing later; there is no valid cursor until indexing finishes.
	Indexing bool `json:"indexing"`
	// WorkspaceMatches, when requested, is aligned one-to-one with Threads.
	WorkspaceMatches []string `json:"workspaceMatches,omitempty"`
	// Workspaces is aligned with Threads when threadWorkspace v2 is negotiated.
	// A nil element means the workspace is unknown.
	Workspaces []*ThreadWorkspace `json:"workspaces,omitempty"`
}

// ThreadWorkspace labels a group of threads. ID identifies the backend's
// canonical workspace; Name alone is not unique.
type ThreadWorkspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ThreadDirectoryOptions struct {
	Limit           int    `json:"limit"`
	Cursor          string `json:"cursor,omitempty"`
	IncludeArchived bool   `json:"includeArchived,omitempty"` // requires v2
	Workspace       string `json:"workspace,omitempty"`       // requires threadWorkspace; host-local launch directory
	BrowseAll       bool   `json:"browseAll,omitempty"`       // with Workspace, classify all rows instead of filtering
	Query           string `json:"query,omitempty"`           // requires threadWorkspace v2; search workspace, title, and chat
}

// ThreadDirectory reads a page from newest last message to oldest. The client
// must offer threadDirectory version 1 or 2. Cursors are backend-epoch local.
func (c *Client) ThreadDirectory(ctx context.Context, limit int, cursor string) (ThreadDirectoryPage, error) {
	return c.ListThreadDirectory(ctx, ThreadDirectoryOptions{Limit: limit, Cursor: cursor})
}

func (c *Client) ListThreadDirectory(ctx context.Context, opts ThreadDirectoryOptions) (ThreadDirectoryPage, error) {
	limit, cursor := opts.Limit, opts.Cursor
	if limit < 1 || limit > 100 || cursor != "" && !directoryCursor.MatchString(cursor) ||
		opts.BrowseAll && opts.Workspace == "" || !utf8.ValidString(opts.Workspace) ||
		strings.ContainsRune(opts.Workspace, 0) || utf8.RuneCountInString(opts.Workspace) > 4096 ||
		opts.Query != "" && (strings.TrimSpace(opts.Query) == "" || !utf8.ValidString(opts.Query) || utf8.RuneCountInString(opts.Query) > 128 ||
			strings.ContainsFunc(opts.Query, func(r rune) bool { return r < 32 || r == 127 })) {
		return ThreadDirectoryPage{}, ErrProtocol
	}
	page, workspace, err := c.threadDirectoryPage(ctx, opts)
	if err != nil {
		return ThreadDirectoryPage{}, err
	}
	if len(page.Threads) > limit || page.NextCursor != nil && (page.Indexing || len(page.Threads) != limit || *page.NextCursor == cursor) ||
		workspace == ThreadWorkspaceSearchVersion && (page.Workspaces == nil || len(page.Workspaces) != len(page.Threads)) {
		return ThreadDirectoryPage{}, ErrProtocol
	}
	if opts.Workspace != "" {
		if page.WorkspaceMatches == nil || len(page.WorkspaceMatches) != len(page.Threads) {
			return ThreadDirectoryPage{}, ErrProtocol
		}
		for i, match := range page.WorkspaceMatches {
			if !oneOf(match, "inside", "outside", "unknown") || !opts.BrowseAll && match != "inside" ||
				workspace == ThreadWorkspaceSearchVersion && (page.Workspaces[i] == nil) != (match == "unknown") {
				return ThreadDirectoryPage{}, ErrProtocol
			}
		}
	} else if page.WorkspaceMatches != nil {
		return ThreadDirectoryPage{}, ErrProtocol
	}
	seen := make(map[ID]bool, len(page.Threads))
	for i, row := range page.Threads {
		if !validDirectoryRow(row) || seen[row.ID] || (!opts.IncludeArchived && row.Archived) {
			return ThreadDirectoryPage{}, ErrProtocol
		}
		if i > 0 {
			prev := page.Threads[i-1]
			if prev.LastMessageMS < row.LastMessageMS || prev.LastMessageMS == row.LastMessageMS && cmpID(prev.ID, row.ID) <= 0 {
				return ThreadDirectoryPage{}, ErrProtocol
			}
		}
		seen[row.ID] = true
	}
	return page, nil
}

func (c *Client) threadDirectoryPage(ctx context.Context, opts ThreadDirectoryOptions) (ThreadDirectoryPage, uint64, error) {
	if opts.Query != "" {
		// A picker cancels searches while typing, so use a separate connection.
		page, err := waitOnCapability[ThreadDirectoryPage](ctx, c, threadDirectoryList, opts, "threadWorkspace", ThreadWorkspaceSearchVersion)
		return page, ThreadWorkspaceSearchVersion, err
	}
	s, err := c.connect(ctx)
	if err != nil {
		return ThreadDirectoryPage{}, 0, err
	}
	version, workspace := s.hello.Capabilities["threadDirectory"], s.hello.Capabilities["threadWorkspace"]
	if opts.Workspace != "" && (version != ThreadManagementVersion || workspace != ThreadWorkspaceVersion && workspace != ThreadWorkspaceSearchVersion) {
		return ThreadDirectoryPage{}, 0, &ServerError{Code: "unsupported_capability", Message: "Workspace scope was not negotiated", Capability: "threadWorkspace"}
	}
	if version != ThreadManagementVersion && (version != ThreadDirectoryVersion || opts.IncludeArchived) {
		return ThreadDirectoryPage{}, 0, directoryUnsupported()
	}
	raw, err := s.call(ctx, threadDirectoryList, opts, nil)
	if err != nil {
		return ThreadDirectoryPage{}, 0, err
	}
	page, err := decode[ThreadDirectoryPage](raw)
	return page, workspace, err
}

func directoryUnsupported() *ServerError {
	return &ServerError{Code: "unsupported_capability", Capability: "threadDirectory", Message: "Required thread directory version was not negotiated"}
}

func validDirectoryRow(row ThreadDirectoryRow) bool {
	return row.ID.Valid() && (row.ParentID == "" || row.ParentID.Valid() && row.ParentID != row.ID)
}

type ThreadCreation struct {
	CWD      string `json:"cwd,omitempty"`
	Title    string `json:"title,omitempty"`
	Worktree bool   `json:"worktree,omitempty"`
}

// CreateThread can allocate a managed checkout. Requires threadDirectory v2.
// Like native thread.new, it does not retry across an ambiguous transport loss.
func (c *Client) CreateThread(ctx context.Context, opts ThreadCreation) (ThreadDirectoryRow, error) {
	if !utf8.ValidString(opts.CWD) || strings.ContainsRune(opts.CWD, 0) || utf8.RuneCountInString(opts.CWD) > 4096 ||
		!utf8.ValidString(opts.Title) || utf8.RuneCountInString(opts.Title) > 256 || opts.Title != "" && strings.TrimSpace(opts.Title) == "" {
		return ThreadDirectoryRow{}, ErrProtocol
	}
	result, err := c.directoryAction(ctx, threadDirectoryNew, opts)
	if err != nil {
		return ThreadDirectoryRow{}, err
	}
	row, err := decode[ThreadDirectoryRow](result)
	if err != nil || !validDirectoryRow(row) || row.ParentID != "" || row.Archived || row.Checkout == nil ||
		(row.Checkout.Kind == "managed") != opts.Worktree {
		return ThreadDirectoryRow{}, ErrProtocol
	}
	return row, nil
}

// SetThreadArchived changes directory visibility only. Neither Git files nor
// transcript entries are removed. Requires threadDirectory v2.
func (c *Client) SetThreadArchived(ctx context.Context, thread ID, archived bool) error {
	if !thread.Valid() {
		return ErrProtocol
	}
	_, err := c.directoryAction(ctx, threadDirectoryArchive, struct {
		ThreadID ID   `json:"threadId"`
		Archived bool `json:"archived"`
	}{thread, archived})
	return err
}

func (c *Client) directoryAction(ctx context.Context, method string, params any) (any, error) {
	s, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	if s.hello.Capabilities["threadDirectory"] != ThreadManagementVersion {
		return nil, directoryUnsupported()
	}
	return s.call(ctx, method, params, nil)
}

type ThreadDirectoryReport struct {
	Revision uint64               `json:"revision"`
	Threads  []ThreadDirectoryRow `json:"threads"`
}

type ThreadDirectoryUpdate struct {
	Report ThreadDirectoryReport
	Err    error
}

type directoryLookup struct {
	ThreadIDs []ID    `json:"threadIds"`
	After     *uint64 `json:"after,omitempty"`
}

func (q directoryLookup) valid() bool {
	if len(q.ThreadIDs) == 0 || len(q.ThreadIDs) > 24 {
		return false
	}
	seen := make(map[ID]bool, len(q.ThreadIDs))
	for _, id := range q.ThreadIDs {
		if !id.Valid() || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func directoryReport(ctx context.Context, s *session, q directoryLookup) (ThreadDirectoryReport, error) {
	if s.hello.Capabilities["threadDirectory"] != ThreadManagementVersion {
		return ThreadDirectoryReport{}, directoryUnsupported()
	}
	raw, err := s.call(ctx, threadDirectoryGet, q, nil)
	if err != nil {
		return ThreadDirectoryReport{}, err
	}
	v, err := decode[ThreadDirectoryReport](raw)
	if err != nil || q.After != nil && v.Revision <= *q.After || len(v.Threads) > len(q.ThreadIDs) {
		return ThreadDirectoryReport{}, ErrProtocol
	}
	seen := make(map[ID]bool, len(v.Threads))
	for _, row := range v.Threads {
		if !validDirectoryRow(row) || !slices.Contains(q.ThreadIDs, row.ID) || seen[row.ID] {
			return ThreadDirectoryReport{}, ErrProtocol
		}
		seen[row.ID] = true
	}
	return v, nil
}

// WatchThreadDirectory observes metadata for up to 24 open tabs on one
// dedicated connection. Unknown or not-yet-indexed IDs are omitted. It
// resnapshots after transport loss; passive revisions never cross a reconnect.
func (c *Client) WatchThreadDirectory(ctx context.Context, ids []ID) <-chan ThreadDirectoryUpdate {
	out := make(chan ThreadDirectoryUpdate, 1)
	q := directoryLookup{ThreadIDs: slices.Clone(ids)}
	go func() {
		defer close(out)
		if !q.valid() {
			out <- ThreadDirectoryUpdate{Err: ErrProtocol}
			return
		}
		life, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(c.life, cancel)
		defer func() { stop(); cancel() }()
		publish := func(v ThreadDirectoryUpdate) {
			select {
			case <-out:
			default:
			}
			out <- v
		}
		for delay := 250 * time.Millisecond; life.Err() == nil; delay = min(delay*2, 4*time.Second) {
			ready, err := c.watchDirectory(life, q, publish)
			if life.Err() != nil {
				return
			}
			publish(ThreadDirectoryUpdate{Err: err})
			if !retryableTransport(err) {
				return
			}
			if ready {
				delay = 250 * time.Millisecond
			}
			if !pause(life, c.life, delay) {
				return
			}
		}
	}()
	return out
}

func (c *Client) watchDirectory(life context.Context, q directoryLookup, publish func(ThreadDirectoryUpdate)) (bool, error) {
	ctx, cancel := context.WithTimeout(life, 10*time.Second)
	defer cancel()
	s, err := c.dial(ctx, life, c.offered, nil)
	if err != nil {
		return false, err
	}
	defer s.fail(ErrClosed)
	v, err := directoryReport(ctx, s, q)
	cancel()
	for err == nil {
		publish(ThreadDirectoryUpdate{Report: v})
		rev := v.Revision
		q.After = &rev
		v, err = directoryReport(life, s, q)
	}
	return q.After != nil, err
}
