package sdk

import (
	"context"
	"errors"
	"reflect"
	"slices"
)

type HistoryRange struct {
	kind   string
	lower  *ID
	upper  *ID
	cursor string
}

// After returns the exclusive/inclusive committed range (lower, upper].
func After(lower, upper *ID) HistoryRange {
	return HistoryRange{kind: "after", lower: lower, upper: upper}
}
func Before(upper *ID) HistoryRange         { return HistoryRange{kind: "before", upper: upper} }
func PageCursor(cursor string) HistoryRange { return HistoryRange{kind: "cursor", cursor: cursor} }

type HistoryPage struct {
	ThreadID   ID      `json:"threadId"`
	Entries    []Entry `json:"entries"`
	NextCursor *string `json:"nextCursor"`
}

func (c *Client) History(ctx context.Context, thread ID, query HistoryRange, limit int) (HistoryPage, error) {
	if !thread.Valid() || query.lower != nil && !query.lower.Valid() || query.upper != nil && !query.upper.Valid() {
		return HistoryPage{}, ErrProtocol
	}
	r := map[string]any{"kind": query.kind}
	switch query.kind {
	case "after":
		r["afterEntryId"], r["throughEntryId"] = query.lower, query.upper
	case "before":
		r["beforeEntryId"] = query.upper
	case "cursor":
		r["cursor"] = query.cursor
	default:
		return HistoryPage{}, ErrProtocol
	}
	page, err := invoke[HistoryPage](ctx, c, "history", struct {
		threadParam
		Range map[string]any `json:"range"`
		Limit int            `json:"limit"`
	}{threadParam{thread}, r, limit})
	if err != nil {
		return HistoryPage{}, err
	}
	if page.ThreadID != thread || !ordered(page.Entries) || len(page.Entries) > limit ||
		(page.NextCursor != nil && (len(page.Entries) == 0 || *page.NextCursor == query.cursor)) {
		return HistoryPage{}, ErrProtocol
	}
	for _, entry := range page.Entries {
		if query.kind == "before" && query.upper != nil && cmpID(entry.ID, *query.upper) >= 0 ||
			query.kind == "after" && (query.upper == nil || cmpID(entry.ID, *query.upper) > 0 ||
				query.lower != nil && cmpID(entry.ID, *query.lower) <= 0) {
			return HistoryPage{}, ErrProtocol
		}
	}
	return page, nil
}

// Transcript merges immutable entries by viewed thread and stable entry ID. Use a
// separate instance for each thread and data store; it is single-owner, like Replica.
type Transcript struct {
	thread  ID
	entries map[ID]Entry
	through ID
}

func NewTranscript(thread ID) *Transcript {
	return &Transcript{thread: thread, entries: make(map[ID]Entry)}
}

// Through is the highest contiguous forward anchor. A window can be newer without
// advancing it. An empty ID means no anchor.
func (t *Transcript) Through() ID { return t.through }

func (t *Transcript) Entries() []Entry {
	out := make([]Entry, 0, len(t.entries))
	for _, entry := range t.entries {
		out = append(out, entry)
	}
	slices.SortFunc(out, func(a, b Entry) int { return cmpID(a.ID, b.ID) })
	return out
}

// MergeWindow retains visible entries but deliberately does not advance the anchor.
func (t *Transcript) MergeWindow(s Snapshot) error {
	if s.State.Thread.ID != t.thread || !ordered(s.State.Window.Entries) {
		return ErrProtocol
	}
	return t.merge(s.State.Window.Entries)
}

func (t *Transcript) merge(rows []Entry) error {
	pending := make(map[ID]Entry, len(rows))
	for _, row := range rows {
		old, ok := t.entries[row.ID]
		if !ok {
			old, ok = pending[row.ID]
		}
		if ok && !reflect.DeepEqual(old, row) {
			return ErrProtocol
		}
		pending[row.ID] = row
	}
	for id, row := range pending {
		t.entries[id] = row
	}
	return nil
}

// Sync fills the independent forward gap through the snapshot's last immutable
// entry. A stale page cursor is restarted by its last explicit anchor. An invalid
// old anchor instead rebuilds through the same upper fence. A failed scan leaves
// the old anchor untouched.
func (t *Transcript) Sync(ctx context.Context, c *Client, s Snapshot) error {
	if s.State.Thread.ID != t.thread {
		return ErrProtocol
	}
	window := s.State.Window
	upper, lower := window.LastEntryID, t.through
	if !ordered(window.Entries) || upper != nil && !upper.Valid() {
		return ErrProtocol
	}
	if upper == nil {
		if len(window.Entries) != 0 {
			return ErrProtocol
		}
		if lower == "" {
			return nil
		}
		_, err := c.History(ctx, t.thread, After(&lower, &lower), 1)
		if IsCode(err, "invalid_cursor") {
			t.entries, t.through = make(map[ID]Entry), ""
			return nil
		}
		return err
	}
	if lower == *upper {
		return t.MergeWindow(s)
	}
	// A same-feed older observation is not a reason to delete retained history.
	if lower != "" && cmpID(lower, *upper) > 0 {
		rangeResult, err := c.History(ctx, t.thread, After(&lower, &lower), 1)
		if err == nil && len(rangeResult.Entries) == 0 {
			return t.MergeWindow(s)
		}
		if !IsCode(err, "invalid_cursor") {
			return errors.Join(ErrProtocol, err)
		}
	}
	var anchor *ID
	if lower != "" {
		anchor = &lower
	}
	rows, err := gather(ctx, c, t.thread, After(anchor, upper))
	if IsCode(err, "invalid_cursor") && anchor != nil {
		rows, err = gather(ctx, c, t.thread, After(nil, upper))
		if err == nil {
			staged := NewTranscript(t.thread)
			if err := staged.merge(rows); err != nil {
				return err
			}
			if err := staged.MergeWindow(s); err != nil {
				return err
			}
			t.entries, t.through = staged.entries, *upper
			return nil
		}
	}
	if err != nil {
		return err
	}
	if err := t.merge(append(rows, window.Entries...)); err != nil {
		return err
	}
	t.through = *upper
	return nil
}

func gather(ctx context.Context, c *Client, thread ID, explicit HistoryRange) ([]Entry, error) {
	query := explicit
	rows := make([]Entry, 0)
	for {
		page, err := c.History(ctx, thread, query, 100)
		if IsCode(err, "invalid_cursor") && query.kind == "cursor" {
			query = explicit
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range page.Entries {
			if explicit.lower != nil && cmpID(e.ID, *explicit.lower) <= 0 ||
				explicit.upper == nil || cmpID(e.ID, *explicit.upper) > 0 {
				return nil, ErrProtocol
			}
		}
		rows = append(rows, page.Entries...)
		if len(page.Entries) > 0 {
			last := page.Entries[len(page.Entries)-1].ID
			explicit.lower = &last
		}
		if page.NextCursor == nil {
			if explicit.lower == nil || explicit.upper == nil || *explicit.lower != *explicit.upper {
				return nil, ErrProtocol
			}
			return rows, nil
		}
		query = PageCursor(*page.NextCursor)
	}
}

// Older fetches up to limit entries before the oldest retained entry, or the
// newest page if no entries have been retained. It does not advance Through.
func (t *Transcript) Older(ctx context.Context, c *Client, limit int) ([]Entry, error) {
	var oldest ID
	for id := range t.entries {
		if oldest == "" || cmpID(id, oldest) < 0 {
			oldest = id
		}
	}
	var anchor *ID
	if oldest != "" {
		anchor = &oldest
	}
	page, err := c.History(ctx, t.thread, Before(anchor), limit)
	if err != nil {
		return nil, err
	}
	if err := t.merge(page.Entries); err != nil {
		return nil, err
	}
	return page.Entries, nil
}
