package tui

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const burstSample = 32
const burstPreview = 4
const burstPageSize = 24

// Records retain immutable native entries. The projection indexes only those
// that can draw a row. Appending a tool result extends its burst in place;
// otherwise an offscreen, collapsed burst could make View walk thousands of
// invisible entries to find the preceding text.
type activityProjection struct {
	burst   *toolBurst
	pending map[string]callInfo
}

type callInfo struct {
	args, intent string
	at           uint64
}

type toolChild struct {
	entry   *record
	message int
	call    callInfo
}

const (
	failedChild = iota
	editedChild
	ordinaryChild
)

type toolBurst struct {
	owner                  *record
	slot                   int // message on owner at which the first tool result was committed
	children               []toolChild
	order                  [3][]int // stable child indexes: failures, edits, then other results
	files                  map[string]struct{}
	reads, commands, other int
	start, end             uint64
}

type burstView struct {
	epoch    uint64
	expanded bool
	page     int
	detail   int // one-based result index, or zero when none is open
}

func (t *threadState) projectionAdd(ids []sdk.ID, appendOnly bool) {
	if appendOnly {
		for _, id := range ids {
			r := t.entries[id]
			if t.projectRecord(&t.projected, r) {
				t.shown = append(t.shown, id)
			}
			if r.visible || r.readableThought {
				t.detailShown = append(t.detailShown, id)
			}
		}
		return
	}
	// A backfill can insert a call before its result, or prose between two
	// sets of results. Replay only the affected user turns, never on View.
	lo, hi := t.index(ids[0]), t.index(ids[len(ids)-1])+1
	for lo > 0 && !t.entries[t.ids[lo]].startsTurn() {
		lo--
	}
	for hi < len(t.ids) && !t.entries[t.ids[hi]].startsTurn() {
		hi++
	}
	from, to := indexEntry(t.shown, t.ids[lo]), len(t.shown)
	detailFrom, detailTo := indexEntry(t.detailShown, t.ids[lo]), len(t.detailShown)
	if hi < len(t.ids) {
		to = indexEntry(t.shown, t.ids[hi])
		detailTo = indexEntry(t.detailShown, t.ids[hi])
	}
	p := activityProjection{}
	var visible, details []sdk.ID
	for _, id := range t.ids[lo:hi] {
		r := t.entries[id]
		if t.projectRecord(&p, r) {
			visible = append(visible, id)
		}
		if r.visible || r.readableThought {
			details = append(details, id)
		}
	}
	if hi == len(t.ids) {
		t.projected = p
	}
	t.shown = slices.Replace(t.shown, from, to, visible...)
	t.detailShown = slices.Replace(t.detailShown, detailFrom, detailTo, details...)
	if r := t.entries[t.anchor.id]; !t.following && r != nil && r.burstAnchor != "" && !r.visible && (!t.reasoningVisible() || !r.readableThought) {
		t.anchor = rowAnchor{id: r.burstAnchor}
	}
}

func (r *record) startsTurn() bool {
	return len(r.entry.Record.Messages) > 0 && r.entry.Record.Messages[0].Role == "user"
}

func (t *threadState) projectRecord(p *activityProjection, r *record) bool {
	r.bursts, r.burstAnchor, r.afterBurst, r.cache = nil, "", 0, blockCache{}
	r.voice = voiceAgent
	r.readableThought = false
	e := r.entry.Record
	visible := t.customEntries[r.entry.Kind()] || r.entry.Truncated || len(e.Messages) == 0 && e.Kind != "model"
	for i, msg := range e.Messages {
		if !r.hasImages && slices.ContainsFunc(msg.Blocks, isImageBlock) {
			r.hasImages, t.hasImages = true, true
		}
		switch msg.Role {
		case "user":
			r.voice = voiceUser
			*p = activityProjection{}
		case "toolResult":
			// Only a committed result contributes to counts. A call may have
			// been pending, interrupted, or still running when this was drawn.
			call := p.pending[msg.CallID]
			delete(p.pending, msg.CallID)
			if p.burst == nil {
				b := &toolBurst{owner: r, slot: i}
				if r.bursts == nil {
					r.bursts = make(map[int]*toolBurst)
				}
				r.bursts[i] = b
				p.burst = b
				visible = true
			}
			r.burstAnchor = p.burst.owner.entry.Record.ID
			p.burst.add(r, i, msg, call)
			continue
		}
		intent := ""
		for _, b := range msg.Blocks {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				visible = true
				if msg.Role == "assistant" {
					intent = progressIntent(b.Text)
					if p.burst != nil {
						r.afterBurst |= 1 << i
						p.burst = nil
					}
				}
			case "thinking":
				r.readableThought = r.readableThought || strings.TrimSpace(b.Text) != ""
			case "omitted":
				if b.Kind == "image" {
					visible = true
				}
			case "toolCall":
				if p.pending == nil {
					p.pending = make(map[string]callInfo)
				}
				if len(p.pending) < 1024 {
					p.pending[b.ID] = callInfo{args: string(b.Arguments), intent: intent, at: msg.TimestampMS}
				}
			}
		}
		visible = visible || msg.Error != ""
	}
	r.visible = visible
	return visible
}

func (b *toolBurst) add(r *record, i int, msg sdk.Message, call callInfo) {
	index := len(b.children)
	b.children = append(b.children, toolChild{r, i, call})
	group := ordinaryChild
	failed := deref(msg.IsError)
	switch msg.Name {
	case "bash":
		b.commands++
	case "read":
		b.reads++
	case "edit", "write":
		if !failed {
			group = editedChild
			var args struct{ Path string }
			if json.Unmarshal([]byte(call.args), &args) == nil && args.Path != "" {
				if b.files == nil {
					b.files = make(map[string]struct{})
				}
				b.files[filepath.Clean(args.Path)] = struct{}{}
			}
		}
	default:
		b.other++
	}
	if failed {
		group = failedChild
	}
	b.order[group] = append(b.order[group], index)
	at := call.at
	if at == 0 || at > msg.TimestampMS {
		at = msg.TimestampMS
	}
	if at > 0 && (b.start == 0 || at < b.start) {
		b.start = at
	}
	b.end = max(b.end, msg.TimestampMS)
	b.owner.cache = blockCache{}
}

func (b *toolBurst) id() string { return string(b.owner.entry.Record.ID) + "/" + strconv.Itoa(b.slot) }

func (b *toolBurst) view(t *threadState) burstView {
	state, ok := b.owner.burstViews[b.slot]
	if ok && state.epoch == t.expandEpoch {
		return state
	}
	return burstView{epoch: t.expandEpoch, expanded: t.expanded}
}

func (t *threadState) toggleBurst(target string) bool {
	parts := strings.Split(target, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return false
	}
	r := t.entries[sdk.ID(parts[0])]
	if r == nil {
		return false
	}
	i, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	b := r.bursts[i]
	if b == nil {
		return false
	}
	state := b.view(t)
	switch {
	case len(parts) == 2:
		state.expanded = !state.expanded
		state.page, state.detail = 0, 0
	case parts[2] == "more" && state.expanded:
		_, end := b.rangeFor(state.page)
		if end < len(b.children) {
			state.page++
			state.detail = 0
		}
	case parts[2] == "first" && state.expanded:
		state.page, state.detail = 0, 0
	case state.expanded:
		child, err := strconv.Atoi(parts[2])
		if err != nil || child < 0 || child >= len(b.children) {
			return false
		}
		if state.detail == child+1 {
			state.detail = 0
		} else {
			state.detail = child + 1
		}
	default:
		return false
	}
	if r.burstViews == nil {
		r.burstViews = make(map[int]burstView)
	}
	r.burstViews[i] = state
	r.cache = blockCache{}
	return true
}

func (b *toolBurst) rangeFor(page int) (int, int) {
	if page == 0 {
		return 0, min(burstPreview, len(b.children))
	}
	start := min((page-1)*burstPageSize, max(0, len(b.children)-1))
	return start, min(start+burstPageSize, len(b.children))
}

func (b *toolBurst) childIndex(rank int) int {
	for _, group := range b.order {
		if rank < len(group) {
			return group[rank]
		}
		rank -= len(group)
	}
	panic("tool burst rank out of bounds")
}

func (m *Model) burstContent(ctx x.RenderContext, t *threadState, b *toolBurst) x.Content {
	state := b.view(t)
	ctx.Expanded = state.expanded
	arrow, hint := "▸ ", "expand activity"
	if state.expanded {
		arrow, hint = "▾ ", "collapse activity"
	}
	stamp := ""
	if b.start > 0 {
		stamp = time.UnixMilli(int64(b.start)).UTC().Format("15:04")
	}
	room := ctx.Width
	if stamp != "" && stringWidth(stamp)+14 <= room {
		room -= stringWidth(stamp) + 2
	}
	line := b.summary(arrow, room)
	if b.start > 0 && b.end > b.start {
		elapsed := " · " + elapsedLabel(entryElapsed(b.start, b.end))
		if activityWidth(line)+stringWidth(elapsed) <= room {
			line = append(line, x.Span{Text: elapsed, Tone: x.Muted})
		}
	}
	if len(m.host.activity) > 0 {
		n := min(len(b.children), burstSample)
		items := make([]x.ActivityItem, n)
		for i, child := range b.children[:n] {
			msg := child.entry.entry.Record.Messages[child.message]
			items[i] = x.ActivityItem{Kind: x.ActivityTool, Label: msg.Name, EntryID: child.entry.entry.Record.ID, CallID: msg.CallID}
		}
		for _, s := range m.host.activity {
			if note, ok := s.Summarize(ctx, x.Activity{Items: items, Omitted: len(b.children) - n}); ok && len(note) > 0 {
				line = append(line, x.Span{Text: " · ", Tone: x.Muted})
				for _, span := range note {
					span.Tone = x.Muted
					line = append(line, span)
				}
				break
			}
		}
	}
	id := b.id()
	line = activityTarget(line, id, hint)
	out := x.Content{activityRow(line, stamp, ctx.Width)}
	if !state.expanded {
		return out
	}
	start, end := b.rangeFor(state.page)
	cardWidth := max(1, ctx.Width-2)
	for rank := start; rank < end; rank++ {
		i := b.childIndex(rank)
		child := b.children[i]
		msg := child.entry.entry.Record.Messages[child.message]
		result := x.Tool{CallID: msg.CallID, Name: msg.Name, ArgumentsJSON: child.call.args, Status: "done", ResultEntryID: child.entry.entry.Record.ID, Failed: deref(msg.IsError)}
		mark, tone := "✓ ", x.Good
		if result.Failed {
			mark, tone = "✗ ", x.Danger
		} else if rank < len(b.order[failedChild])+len(b.order[editedChild]) {
			mark, tone = "✎ ", x.Warning
		}
		label := toolActionLabel(result)
		meta := ""
		if child.call.at > 0 && msg.TimestampMS > child.call.at {
			meta = elapsedLabel(entryElapsed(child.call.at, msg.TimestampMS))
		}
		if result.Failed {
			meta = toolFailureLabel(msg)
		}
		line := x.Line{{Text: "  " + mark, Tone: tone}, {Text: label, Tone: x.Muted}}
		out = append(out, activityRow(activityTarget(line, id+"/"+strconv.Itoa(i), "show tool result"), meta, cardWidth))
		if state.detail == i+1 {
			raw := toolResultText(msg)
			result.ResultText = limitText(raw, toolPreviewBytes)
			detail := ctx
			detail.Expanded = true
			for _, block := range m.toolContent(detail, result) {
				if block.Format == x.Text {
					if block.Lines == nil {
						block.Text = "     " + strings.ReplaceAll(block.Text, "\n", "\n     ")
					} else {
						lines := make([]x.Line, len(block.Lines))
						for j, line := range block.Lines {
							lines[j] = append(x.Line{{Text: "     "}}, line...)
						}
						block.Lines = lines
					}
				}
				out = append(out, block)
			}
			lines := 1 + strings.Count(strings.TrimSuffix(raw, "\n"), "\n")
			if raw != "" && (lines > toolPreviewLines || len(raw) > toolPreviewBytes || child.entry.entry.Truncated) {
				target := outputTargetPrefix + string(result.ResultEntryID) + "/" + strconv.Itoa(child.message)
				label := "     … View full output"
				if lines > toolPreviewLines {
					label = fmt.Sprintf("     … %d more lines · view output", lines-toolPreviewLines)
				}
				out = append(out, activityOpenRow(label, x.Muted, target, "open retained tool output", cardWidth))
			}
		}
	}
	if n := len(b.children) - end; n > 0 {
		out = append(out, activityOpenRow(fmt.Sprintf("  … %d more", n), x.Muted, id+"/more", "show more results", cardWidth))
	} else if state.page > 1 {
		out = append(out, activityOpenRow("  ‹ first results", x.Muted, id+"/first", "show first results", cardWidth))
	}
	return out
}

func (b *toolBurst) summary(arrow string, width int) x.Line {
	const (
		reads = iota
		commands
		edits
		other
		failures
	)
	var stats [5]x.Span
	if b.reads > 0 {
		stats[reads] = x.Span{Text: fmt.Sprintf("read %d", b.reads), Tone: x.Muted}
	}
	if b.commands > 0 {
		stats[commands] = x.Span{Text: fmt.Sprintf("ran %d", b.commands), Tone: x.Muted}
	}
	if n := len(b.files); n > 0 {
		stats[edits] = x.Span{Text: "edited " + nounCount(n, "file"), Tone: x.Warning}
	} else if n := len(b.order[editedChild]); n > 0 {
		stats[edits] = x.Span{Text: nounCount(n, "edit"), Tone: x.Warning}
	}
	if b.other > 0 {
		stats[other] = x.Span{Text: fmt.Sprintf("%d other", b.other), Tone: x.Muted}
	}
	if n := len(b.order[failedChild]); n > 0 {
		stats[failures] = x.Span{Text: fmt.Sprintf("%d failed", n), Tone: x.Danger}
	}
	// Keep failures and file changes at narrow widths, without reordering the
	// summary. Choose complete segments before drawing or reserving duration.
	gap := x.Span{Text: " · ", Tone: x.Muted}
	space := width - stringWidth(arrow) + stringWidth(gap.Text)
	for _, i := range [...]int{failures, edits, reads, commands, other} {
		if stats[i].Text == "" {
			continue
		}
		if n := stringWidth(stats[i].Text) + stringWidth(gap.Text); n <= space {
			space -= n
		} else {
			stats[i].Text = ""
		}
	}
	row := x.Line{{Text: arrow, Tone: x.Muted}}
	for _, span := range stats {
		if span.Text != "" {
			if len(row) > 1 {
				row = append(row, gap)
			}
			row = append(row, span)
		}
	}
	return row
}

func nounCount(n int, noun string) string {
	if n != 1 {
		noun += "s"
	}
	return strconv.Itoa(n) + " " + noun
}

func toolFailureLabel(msg sdk.Message) string {
	if msg.Name == "bash" {
		for i := len(msg.Blocks) - 1; i >= 0; i-- {
			b := msg.Blocks[i]
			if b.Type != "text" || strings.TrimSpace(b.Text) == "" {
				continue
			}
			s := strings.TrimSpace(b.Text)
			last := s[strings.LastIndexByte(s, '\n')+1:]
			if value, ok := strings.CutPrefix(last, "Command exited with code "); ok {
				if n, err := strconv.Atoi(value); err == nil && n > 0 && n <= 255 {
					return "exit " + strconv.Itoa(n)
				}
			}
			break
		}
	}
	return "failed"
}

func toolResultText(msg sdk.Message) string {
	var out []string
	for _, b := range msg.Blocks {
		if b.Type == "text" {
			out = append(out, b.Text)
		}
	}
	return limitText(strings.Join(out, "\n"), recordBytes)
}

func toolActionLabel(tool x.Tool) string {
	var args struct{ Command, Path string }
	if json.Unmarshal([]byte(tool.ArgumentsJSON), &args) == nil {
		if tool.Name == "bash" && args.Command != "" {
			return args.Command
		}
		if args.Path != "" {
			return tool.Name + " " + args.Path
		}
	}
	if tool.Name != "" {
		return tool.Name
	}
	return "tool result"
}

func moreActivityTarget(row displayRow) string {
	for _, hit := range row.hits {
		if hit.action.ID == expandRowID && (strings.HasSuffix(hit.action.Target, "/more") || strings.HasSuffix(hit.action.Target, "/first") ||
			strings.HasPrefix(hit.action.Target, outputTargetPrefix)) {
			return hit.action.Target
		}
	}
	return ""
}

func activityTarget(line x.Line, id, hint string) x.Line {
	for i, span := range line {
		if span.Action.ID == "" {
			span.Action = x.ActionRef{ID: expandRowID, Target: id}
			span.Hint = hint
			line[i] = span
		}
	}
	return line
}

func activityWidth(line x.Line) int {
	w := 0
	for _, span := range line {
		w += stringWidth(span.Text)
	}
	return w
}

// Keep the open hint beside its target. Don't reserve columns by clipping the
// label: optional hints disappear whole when the remaining cells are too few.
func activityOpenRow(label string, tone x.Tone, target, hover string, width int) x.Block {
	line := activityTarget(x.Line{{Text: label, Tone: tone}}, target, hover)
	const hint = " · enter open"
	if stringWidth(label)+stringWidth(hint) <= width {
		line = append(line, x.Span{Text: hint})
	}
	return activityRow(line, "", width)
}

// Activity headers and children are one screen row. Reserve the supported
// metadata at the right; ellipsize chrome without wrapping it like prose.
func activityRow(line x.Line, right string, width int) x.Block {
	right = oneLine(right)
	rw := stringWidth(right)
	if rw+14 > width {
		right = ""
		rw = 0
	}
	room := width
	if rw > 0 {
		room -= rw + 2
	}
	var out x.Line
	used := 0
	for _, span := range line[:min(32, len(line))] {
		if used >= room || span.Text == " · " && room-used < 5 {
			break
		}
		text := oneLine(limitText(span.Text, 4096))
		span.Text = ansi.Truncate(text, room-used, "…")
		out = append(out, span)
		used += stringWidth(span.Text)
		if span.Text != text {
			break
		}
	}
	if rw > 0 {
		out = append(out, x.Span{Text: strings.Repeat(" ", width-rw-used) + right, Tone: x.Muted})
	}
	return x.Block{Lines: []x.Line{out}}
}

func entryElapsed(start, end uint64) time.Duration {
	return time.UnixMilli(int64(end)).Sub(time.UnixMilli(int64(start)))
}

func elapsedLabel(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	s := int(d / time.Second)
	if s < 60 {
		return strconv.Itoa(s) + "s"
	}
	return fmt.Sprintf("%dm %02ds", s/60, s%60)
}
