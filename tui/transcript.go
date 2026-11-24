package tui

import (
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	"github.com/javiermolinar/hush/tui/extension"
)

const recordBytes = 128 << 10
const recordRows = 512

type threadState struct {
	intro                                                    *introState
	footer                                                   footerMetadata
	presentation                                             *sdk.IntroThread
	directory                                                *sdk.ThreadDirectoryRow
	gitDiff                                                  gitDiffState
	life, revision                                           uint64
	info                                                     extension.Thread
	ids, shown, detailShown                                  []sdk.ID
	projected                                                activityProjection
	customEntries                                            map[string]bool
	entries                                                  map[sdk.ID]*record
	through, gapUpper, gapBefore, windowBefore               sdk.ID
	gapRetries                                               uint
	awaiting                                                 sdk.ID
	runInputs                                                []sdk.ID
	atStart, loadingOlder, loadingGap, pageError, olderError bool
	following, expanded                                      bool
	anchor                                                   rowAnchor
	stream                                                   extension.Stream
	tools                                                    []extension.Tool
	queue                                                    []sdk.QueueItem
	sendingInput                                             sdk.QueueItem
	admittedInputs                                           []sdk.QueueItem
	streamRevision                                           uint64
	liveCache                                                blockCache
	liveDocs                                                 map[int]*markdownStream
	status                                                   map[string]extension.SetStatus
	lastAnswer                                               string
	lastAnswerID                                             sdk.ID
	draft                                                    string
	stagedSkill                                              stagedSkill
	history                                                  []string
	aborting, sending                                        bool
	activeRun, observed                                      bool
	hasImages, attaching                                     bool
	attachments                                              []draftImage
	stopInputs                                               []localInput
	stopEvicted                                              bool
	activity                                                 activityState
	expandEpoch                                              uint64
	liveOverride, liveExpanded                               bool
}

type rowAnchor struct {
	id  sdk.ID
	row int
}
type voice uint8

const (
	voiceAgent voice = iota
	voiceUser
)

type record struct {
	entry                                        extension.Entry
	cache                                        blockCache
	visible, readableThought, override, expanded bool
	voice                                        voice
	bursts                                       map[int]*toolBurst
	burstViews                                   map[int]burstView
	burstAnchor                                  sdk.ID
	afterBurst                                   uint64 // message slots with prose after a tool burst
	expandEpoch                                  uint64
	thought                                      time.Duration
	hasImages                                    bool
}
type renderKey struct {
	owner               sdk.ID
	entry               sdk.ID
	block               int
	width               int
	theme               uint64
	expanded            bool
	workspace           string
	revision, viewEpoch uint64
	context, navigation uint64
	images              uint64

	speakerBreak    bool
	first, forkHint bool
}
type blockCache struct {
	key  renderKey
	rows []displayRow
}
type spanHit struct {
	first, last int
	action      extension.ActionRef
	hint        string
}
type displayRow struct {
	text                string
	hits                []spanHit
	user, card, tabBand bool
	image               *imageDemand
}

func limitText(s string, size int) string {
	if len(s) <= size {
		return s
	}
	if size <= 0 {
		return ""
	}
	for size > 0 && !utf8.RuneStart(s[size]) {
		size--
	}
	return strings.Clone(s[:size])
}

func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t':
			return ' '
		case r == 0x7f:
			return '␡'
		case r < 32:
			return '\u2400' + r
		case unicode.IsControl(r):
			return '�'
		default:
			return r
		}
	}, s)
}
func oneLine(s string) string { return strings.ReplaceAll(safeText(s), "\n", " ") }

func takeText(s string, left *int, truncated *bool) string {
	if len(s) > *left {
		*truncated = true
		s = limitText(s, *left)
	}
	*left -= len(s)
	return s
}

func detachBlocks(values []sdk.Block, left *int, truncated *bool) []sdk.Block {
	count := min(len(values), 128)
	if count < len(values) {
		*truncated = true
	}
	out := make([]sdk.Block, 0, count)
	for _, b := range values[:count] {
		if *left <= 0 {
			*truncated = true
			break
		}
		b.Text = takeText(b.Text, left, truncated)
		b.ID = limitText(b.ID, 512)
		b.Name = limitText(b.Name, 512)
		b.Kind = limitText(b.Kind, 512)
		if len(b.Arguments) > *left {
			*truncated = true
			b.Arguments = nil
		} else {
			*left -= len(b.Arguments)
			b.Arguments = slices.Clone(b.Arguments)
		}
		out = append(out, b)
	}
	return out
}

func detachEntry(e sdk.Entry) extension.Entry {
	out := extension.Entry{Record: e}
	count := min(len(e.Messages), 64)
	out.Truncated = count < len(e.Messages)
	if e.HeadEntryID != nil {
		out.Record.HeadEntryID = ptr(*e.HeadEntryID)
	}
	out.Record.Messages = make([]sdk.Message, 0, count)
	left := recordBytes
	for _, msg := range e.Messages[:count] {
		if left <= 0 {
			out.Truncated = true
			break
		}
		if msg.IsError != nil {
			msg.IsError = ptr(*msg.IsError)
		}
		msg.Error = takeText(msg.Error, &left, &out.Truncated)
		msg.CallID = limitText(msg.CallID, 512)
		msg.Name = limitText(msg.Name, 512)
		msg.StopReason = limitText(msg.StopReason, 512)
		msg.Blocks = detachBlocks(msg.Blocks, &left, &out.Truncated)
		out.Record.Messages = append(out.Record.Messages, msg)
	}
	return out
}

func detachStream(s *sdk.Streaming) extension.Stream {
	if s == nil {
		return extension.Stream{}
	}
	out := extension.Stream{Present: true, Value: *s}
	left := recordBytes
	out.Value.Blocks = detachBlocks(s.Blocks, &left, &out.Truncated)
	if s.Retry != nil {
		retry := *s.Retry
		retry.Detail = takeText(retry.Detail, &left, &out.Truncated)
		out.Value.Retry = &retry
	}
	return out
}

// The SDK promises ascending immutable entries. Retain already seen records
// (and their cached rows) even when evicted from the live window.
func (t *threadState) merge(values []sdk.Entry) []*record {
	var added []*record
	var ids []sdk.ID
	for _, e := range values {
		if !e.ID.Valid() || t.entries[e.ID] != nil {
			continue
		}
		r := &record{entry: detachEntry(e)}
		t.entries[e.ID] = r
		added = append(added, r)
		ids = append(ids, e.ID)
		if !checkpointMarker(e.Kind) && lessID(t.lastAnswerID, e.ID) {
			var parts []string
			for _, msg := range r.entry.Record.Messages {
				if msg.Role != "assistant" {
					continue
				}
				for _, b := range msg.Blocks {
					if b.Type == "text" && b.Text != "" {
						parts = append(parts, b.Text)
					}
				}
			}
			if len(parts) > 0 {
				t.lastAnswerID = e.ID
				t.lastAnswer = limitText(strings.Join(parts, "\n"), recordBytes)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if len(t.ids) == 0 || lessID(t.ids[len(t.ids)-1], ids[0]) {
		t.ids = append(t.ids, ids...)
		t.projectionAdd(ids, true)
		return added
	}
	i := t.index(ids[0])
	merged := make([]sdk.ID, i, len(t.ids)+len(ids))
	copy(merged, t.ids[:i])
	j := 0
	for i < len(t.ids) && j < len(ids) {
		if lessID(t.ids[i], ids[j]) {
			merged = append(merged, t.ids[i])
			i++
		} else {
			merged = append(merged, ids[j])
			j++
		}
	}
	t.ids = append(append(merged, t.ids[i:]...), ids[j:]...)
	t.projectionAdd(ids, false)
	return added
}

func (t *threadState) index(id sdk.ID) int      { return indexEntry(t.ids, id) }
func (t *threadState) shownIndex(id sdk.ID) int { return indexEntry(t.visibleIDs(), id) }
func (t *threadState) visibleIDs() []sdk.ID {
	if t.expanded {
		return t.detailShown
	}
	return t.shown
}
func indexEntry(ids []sdk.ID, id sdk.ID) int {
	i, _ := slices.BinarySearchFunc(ids, id, func(a, b sdk.ID) int {
		if lessID(a, b) {
			return -1
		}
		if a == b {
			return 0
		}
		return 1
	})
	return i
}

func (m *Model) renderContext(t *threadState, width, lines int, live bool) extension.RenderContext {
	return extension.RenderContext{Context: m.contextFor(t.info.ID), Width: width, MaxLines: lines, MaxBytes: recordBytes,
		Theme: m.theme, Expanded: t.expanded, Live: live}
}

func (m *Model) blockRows(t *threadState, i, width int) []displayRow {
	if i == -1 {
		return m.introRows(t.intro, width)
	}
	shown := t.visibleIDs()
	live := i == len(shown)
	key := renderKey{owner: t.info.ID, width: width, theme: m.theme.Revision, expanded: t.expanded, workspace: t.info.Workspace}
	ctx := m.renderContext(t, width, recordRows, live)
	if live {
		key.revision = t.streamRevision
		ctx.Expanded = false
		key.speakerBreak = t.projected.burst != nil || i > 0 && t.entries[shown[i-1]].voice == voiceUser
		if t.liveCache.key == key && t.liveCache.rows != nil {
			return t.liveCache.rows
		}
		content := m.liveRecordContent(ctx, t)
		rows := m.layoutContent(ctx, key, content)
		if key.speakerBreak && len(rows) > 0 {
			rows = append([]displayRow{{}}, rows...)
		}
		t.liveCache = blockCache{key, rows}
		return rows
	}
	r := t.entries[shown[i]]
	key.entry = r.entry.Record.ID
	ctx.Expanded = r.details(t)
	key.expanded = ctx.Expanded
	if r.hasImages || t.hasImages && len(r.bursts) > 0 {
		key.images = m.images.revision
	}
	if len(r.bursts) > 0 {
		key.viewEpoch = t.expandEpoch
		key.first = i == t.firstBlock()
	}
	key.speakerBreak = i > 0 && t.entries[shown[i-1]].voice != r.voice
	if len(r.entry.Record.Messages) == 0 {
		key.forkHint = m.canHintFork()
	}
	if m.host.customEntries[r.entry.Kind()] || (len(r.bursts) > 0 && (m.host.customTools || len(m.host.activity) > 0)) {
		key.context = t.revision
		key.navigation = m.navigation
	}
	if r.cache.key == key && r.cache.rows != nil {
		return r.cache.rows
	}
	content, cards := m.recordContent(ctx, t, r)
	rows := m.layoutBlocks(ctx, key, content, cards)
	if r.hasImages || t.hasImages && len(r.bursts) > 0 {
		rows = append(rows, m.recordImageRows(ctx, t, r, ctx.MaxLines-len(rows))...)
	}
	if len(rows) > 0 {
		// A burst divider also serves as the speaker break. Don't stack
		// two blank rows or show a divider before the first record.
		if rows[0].text == "" && key.first {
			rows = rows[1:]
		} else if key.speakerBreak && rows[0].text != "" {
			rows = append([]displayRow{{}}, rows...)
		}
	}
	r.cache = blockCache{key, rows}
	return rows
}

func (r *record) details(t *threadState) bool {
	if r.override && r.expandEpoch == t.expandEpoch {
		return r.expanded
	}
	return t.expanded
}

func (m *Model) recordContent(ctx extension.RenderContext, t *threadState, r *record) (extension.Content, []blockRange) {
	var content extension.Content
	var cards []blockRange
	var handled bool
	if hook, ok := m.host.entries[r.entry.Kind()]; ok {
		content, handled = hook.Render(ctx, detachEntryForRender(r.entry))
	}
	if !handled {
		content, cards = m.activityEntry(ctx, t, r)
	}
	if r.entry.Truncated {
		content = append(content, extension.Block{Text: "… record truncated", Tone: extension.Muted})
	}
	return content, cards
}

func (m *Model) liveRecordContent(ctx extension.RenderContext, t *threadState) extension.Content {
	var content extension.Content
	if t.stream.Present {
		answer := ctx
		answer.Expanded = false
		content = m.blocksContent(answer, "assistant", t.stream.Value.Blocks)
	}
	return content
}

func detachEntryForRender(e extension.Entry) extension.Entry {
	out := detachEntry(e.Record)
	out.CustomKind, out.DataJSON = e.CustomKind, e.DataJSON
	out.Truncated = out.Truncated || e.Truncated
	return out
}

func (m *Model) layoutContent(ctx extension.RenderContext, key renderKey, content extension.Content) []displayRow {
	return m.layoutBlocks(ctx, key, content, nil)
}

func (m *Model) layoutBlocks(ctx extension.RenderContext, key renderKey, content extension.Content, cards []blockRange) []displayRow {
	rows := make([]displayRow, 0, min(8, ctx.MaxLines))
	bytes := ctx.MaxBytes
	for i, b := range content[:min(128, len(content))] {
		if len(rows) >= ctx.MaxLines || bytes <= 0 {
			break
		}
		first := len(rows)
		if b.Lines != nil {
			for _, line := range b.Lines[:min(ctx.MaxLines, len(b.Lines))] {
				rows = append(rows, m.layoutLine(ctx, line, &bytes, ctx.MaxLines-len(rows))...)
				if len(rows) >= ctx.MaxLines || bytes <= 0 {
					break
				}
			}
		} else {
			text := limitText(b.Text, bytes)
			bytes -= len(text)
			key.block = i
			lines := m.renderDocument(key, b.Format, text, limitText(b.Language, 128), ctx.Width, ctx.Thread.Workspace, ctx.Live)
			if ctx.Live && len(lines) > ctx.MaxLines-len(rows) {
				lines = lines[len(lines)-(ctx.MaxLines-len(rows)):]
			}
			for _, line := range lines[:min(ctx.MaxLines-len(rows), len(lines))] {
				rows = append(rows, displayRow{text: paint(b.Tone, line), user: b.Format == extension.UserText})
			}
		}
		if len(cards) > 0 && i >= cards[0].start {
			for j := first; j < len(rows); j++ {
				rows[j].card = true
			}
			if i+1 == cards[0].end {
				cards = cards[1:]
			}
		}
	}
	return rows
}

func (m *Model) layoutLine(ctx extension.RenderContext, line extension.Line, bytes *int, budget int) []displayRow {
	if budget <= 0 || ctx.Width <= 0 {
		return nil
	}
	out := make([]displayRow, 0, 1)
	row := displayRow{}
	x := 0
	flush := func() { out = append(out, row); row = displayRow{}; x = 0 }
	for _, span := range line[:min(64, len(line))] {
		if *bytes <= 0 || len(out) >= budget {
			break
		}
		v := limitText(span.Text, *bytes)
		*bytes -= len(v)
		text := safeText(v)
		i, ok := m.host.byID[span.Action.ID]
		hit := ok && m.eligibleFor(m.host.actions[i], ctx.Thread.ID, span.Action.Target)
		for n, part := range strings.Split(text, "\n") {
			if n != 0 && len(out) < budget {
				flush()
			}
			// Wrap against the remaining width, then full row width. The
			// semantic span's hit follows the exact drawn cell fragment.
			if len(out) >= budget {
				break
			}
			if x == ctx.Width {
				flush()
			}
			for part != "" && len(out) < budget {
				first := ansi.Truncate(part, ctx.Width-x, "")
				if first == "" {
					if x == 0 {
						_, n := utf8.DecodeRuneInString(part)
						part = part[n:]
					} else {
						flush()
					}
					continue
				}
				w := ansi.StringWidth(first)
				if hit && span.Tone == extension.Muted {
					row.text += m.control(first)
				} else {
					row.text += paint(span.Tone, first)
				}
				if hit && w > 0 {
					ref := span.Action
					ref.Args = limitText(ref.Args, maxPromptBytes)
					ref.Target = limitText(ref.Target, 1024)
					row.hits = append(row.hits, spanHit{x, x + w, ref, limitText(span.Hint, 256)})
				}
				x += w
				part = part[len(first):]
				if part != "" {
					flush()
				}
			}
		}
	}
	if len(out) < budget {
		flush()
	}
	return out
}

type transcriptLine struct {
	displayRow
	anchor rowAnchor
}

func (m *Model) tail(t *threadState, w, h int) []transcriptLine {
	out := make([]transcriptLine, 0, h)
	for i := len(t.visibleIDs()); i >= t.firstBlock() && len(out) < h; i-- {
		block := m.blockRows(t, i, w)
		for j := len(block) - 1; j >= 0 && len(out) < h; j-- {
			out = append(out, transcriptLine{block[j], rowAnchor{t.idAt(i), j}})
		}
	}
	slices.Reverse(out)
	return out
}

func (t *threadState) firstBlock() int {
	if t.intro != nil {
		return -1
	}
	return 0
}

func (t *threadState) blockIndex(id sdk.ID) int {
	switch id {
	case introEntry:
		return t.firstBlock()
	case "":
		return len(t.visibleIDs())
	default:
		return t.shownIndex(id)
	}
}

func (t *threadState) idAt(i int) sdk.ID {
	if i == -1 && t.intro != nil {
		return introEntry
	}
	if i >= 0 && i < len(t.visibleIDs()) {
		return t.visibleIDs()[i]
	}
	return ""
}

func (m *Model) transcript(t *threadState, w, h int) []transcriptLine {
	if h <= 0 {
		return nil
	}
	if t.following {
		return m.tail(t, w, h)
	}
	out := make([]transcriptLine, 0, h)
	i := t.blockIndex(t.anchor.id)
	for j := i; j <= len(t.visibleIDs()) && len(out) < h; j++ {
		block := m.blockRows(t, j, w)
		start := 0
		if j == i {
			start = min(t.anchor.row, max(0, len(block)-1))
		}
		for k := start; k < len(block) && len(out) < h; k++ {
			out = append(out, transcriptLine{block[k], rowAnchor{t.idAt(j), k}})
		}
	}
	return out
}

// scrollSpan measures position in display blocks rather than terminal lines.
// Only the two already rendered boundary blocks need a row count.
func (m *Model) scrollSpan(t *threadState, rows []transcriptLine, w int) (from, to, total float64) {
	if len(rows) == 0 {
		return
	}
	first := t.firstBlock()
	count := len(t.visibleIDs())
	total = float64(count - first)
	last := rows[len(rows)-1].anchor
	if last.id == "" || t.stream.Present && t.blockIndex(last.id) == count-1 && len(m.blockRows(t, count, w)) != 0 {
		// Check the live block only at the known tail; the mere presence of
		// a generation (e.g. a tool-only stream) doesn't add transcript rows.
		total++
	}
	position := func(anchor rowAnchor, edge int) float64 {
		i := t.blockIndex(anchor.id)
		p := float64(i - first)
		if offset := anchor.row + edge; offset > 0 {
			p += float64(offset) / float64(max(1, len(m.blockRows(t, i, w))))
		}
		return p
	}
	from, to = position(rows[0].anchor, 0), position(rows[len(rows)-1].anchor, 1)
	if t.observed && !t.atStart && m.feed.token != 0 && len(t.ids) > 0 {
		from, to, total = from+1, to+1, total+1 // an older page is available
	}
	return
}

func (m *Model) scroll(amount int) tea.Cmd {
	t := m.threads[m.active]
	if amount == 0 || m.width < 40 || m.height < 12 || len(t.visibleIDs()) == 0 && t.intro == nil && !t.stream.Present {
		return nil
	}
	w := m.width - 4
	if t.following {
		if amount > 0 {
			return nil
		}
		if !m.currentFrame() {
			return nil
		}
		top := m.frame.top
		if !m.frame.hasTranscript {
			last := max(t.firstBlock(), len(t.visibleIDs())-1)
			top = rowAnchor{t.idAt(last), max(0, len(m.blockRows(t, last, w))-1)}
		}
		t.anchor, t.following = top, false
	}
	i := t.blockIndex(t.anchor.id)
	j := t.anchor.row
	for amount != 0 && i >= t.firstBlock() && i <= len(t.visibleIDs()) {
		rows := m.blockRows(t, i, w)
		j = min(j, max(0, len(rows)-1))
		if amount < 0 {
			if j+amount >= 0 {
				j += amount
				break
			}
			if i == t.firstBlock() {
				j = 0
				break
			}
			amount += j + 1
			i--
			j = max(0, len(m.blockRows(t, i, w))-1)
		} else {
			if j+amount < len(rows) {
				j += amount
				break
			}
			if i == len(t.visibleIDs()) {
				t.following = true
				break
			}
			amount -= len(rows) - j
			i++
			j = 0
		}
	}
	if amount > 0 && !t.following && m.currentFrame() {
		bottom := m.tail(t, w, m.frame.transcriptHeight)
		if len(bottom) != 0 {
			a := bottom[0].anchor
			index := t.blockIndex(a.id)
			if i > index || i == index && j >= a.row {
				t.following = true
			}
		}
	}
	t.anchor = rowAnchor{t.idAt(i), j}
	return m.maybeOlder(true)
}
