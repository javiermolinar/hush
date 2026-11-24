package tui

import (
	"context"
	"image"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

type ImageMode string

const (
	ImageAuto   ImageMode = "auto"
	ImageKitty  ImageMode = "kitty"
	ImageBlocks ImageMode = "blocks"
	ImageNone   ImageMode = "none"
)

func autoImages() ImageMode {
	// A graphics query cannot prove Unicode placement. Do not send APCs
	// through a multiplexer that hasn't promised to forward them.
	if os.Getenv("TMUX") == "" && os.Getenv("ZELLIJ") == "" && os.Getenv("STY") == "" {
		if term := strings.ToLower(os.Getenv("TERM_PROGRAM")); term == "ghostty" || term == "rex" || kittyTerminal() {
			return ImageKitty
		}
	}
	return ImageBlocks
}

func kittyTerminal() bool {
	return strings.EqualFold(os.Getenv("TERM_PROGRAM"), "kitty") || os.Getenv("KITTY_WINDOW_ID") != "" || os.Getenv("TERM") == "xterm-kitty"
}

type imageBackend interface {
	RequireCapability(context.Context, string, uint64) error
	GetImage(context.Context, sdk.ImageRef) (sdk.Image, error)
}

const previewCacheSize = 16

type imageState struct {
	mode     ImageMode
	capable  bool
	output   *imageOutput
	screen   imageScreen
	revision uint64
	assets   map[sdk.ImageRef]*imagePreview
	clock    uint64
	scanning bool
	busy     bool
	placing  int
	nextID   int
}

type imageDemand struct {
	ref   sdk.ImageRef
	width int
}

type imagePlacement struct {
	screen     imageScreen
	cols, rows int
	id         int
}

type imagePreview struct {
	pixels *image.NRGBA
	used   uint64
	placed imagePlacement
	blocks pixelLayout
}

type pixelLayout struct {
	cols, rows  int
	backgrounds bool
	lines       []string
}

func (p *imagePreview) blockRows(cols, rows int, backgrounds bool) []string {
	if memo := p.blocks; memo.cols != cols || memo.rows != rows || memo.backgrounds != backgrounds {
		p.blocks = pixelLayout{cols, rows, backgrounds, halfBlockPreview(p.pixels, cols, rows, backgrounds)}
	}
	return p.blocks.lines
}

type imageCapability bool

type imageScan struct{}

type imageFetched struct {
	ref    sdk.ImageRef
	pixels *image.NRGBA
}

type imagePlaced struct {
	ref       sdk.ImageRef
	placement imagePlacement
	ok        bool
}

func (m *Model) probeImages() tea.Cmd {
	backend, ok := m.backend.(imageBackend)
	if !ok || m.images.mode == ImageNone {
		return nil
	}
	life := m.ctx
	return func() tea.Msg {
		return imageCapability(backend.RequireCapability(life, "images", sdk.ImagesVersion) == nil)
	}
}

func (m *Model) canInlineImages() bool {
	return m.images.mode == ImageKitty && m.images.output != nil
}

func isImageBlock(b sdk.Block) bool { return b.Type == "omitted" && b.Kind == "image" }

func (m *Model) recordImageRows(ctx x.RenderContext, t *threadState, r *record, budget int) []displayRow {
	var rows []displayRow
	appendImages := func(entry sdk.ID, slot int, blocks []sdk.Block, prefix string, expand bool) {
		for i, b := range blocks {
			if len(rows) >= budget {
				break
			}
			if isImageBlock(b) {
				ref := sdk.ImageRef{ThreadID: t.info.ID, EntryID: entry, MessageIndex: slot, BlockIndex: i}
				image := m.imageRows(ref, ctx.Width, prefix, expand)
				rows = append(rows, image[:min(len(image), budget-len(rows))]...)
			}
		}
	}
	for i, msg := range r.entry.Record.Messages {
		if len(rows) >= budget {
			break
		}
		if msg.Role != "toolResult" {
			appendImages(r.entry.Record.ID, i, msg.Blocks, "", ctx.Expanded)
		} else if burst := r.bursts[i]; burst != nil {
			if view := burst.view(t); view.expanded && view.detail > 0 {
				child := burst.children[view.detail-1]
				entry := child.entry.entry.Record
				appendImages(entry.ID, child.message, entry.Messages[child.message].Blocks, "     ", true)
			}
		}
	}
	return rows
}

func (m *Model) imageRows(ref sdk.ImageRef, w int, prefix string, expanded bool) []displayRow {
	summary := displayRow{text: prefix + paint(x.Muted, "▣ image")}
	kitty := m.canInlineImages()
	if !m.images.capable || m.images.mode == ImageNone || !kitty && !expanded {
		return []displayRow{summary}
	}
	need := &imageDemand{ref: ref, width: w - len(prefix)}
	summary.image = need
	asset := m.images.assets[ref]
	if asset == nil {
		return []displayRow{summary}
	}
	if asset.pixels == nil {
		return []displayRow{{text: prefix + paint(x.Muted, "▣ image unavailable")}}
	}
	cols, rows := previewCells(asset.pixels, need.width)
	if cols <= 0 || rows <= 0 {
		return []displayRow{summary}
	}
	var lines []string
	if kitty {
		p := asset.placed
		if p.id == 0 || !p.screen.up || p.screen != m.images.screen || p.cols != cols || p.rows != rows {
			return []displayRow{summary}
		}
		lines = kittyPlaceholders(p.id, cols, rows)
	} else {
		// A resize often still fits the same capped pixel grid.
		lines = asset.blockRows(cols, rows, m.options.Fills != FillOff)
	}
	out := make([]displayRow, len(lines))
	for i, line := range lines {
		out[i] = displayRow{text: prefix + line, image: need}
	}
	return out
}

func (m *Model) scheduleImageScan() tea.Cmd {
	s := &m.images
	if s.scanning || s.busy || !s.capable || !m.threads[m.active].hasImages || m.quitting {
		return nil
	}
	s.scanning = true
	// Read the frame memo on the next Update, not from the command. View
	// will have applied any scrolling, expansion or width change by then.
	return func() tea.Msg { return imageScan{} }
}

func (m *Model) scanImages() tea.Cmd {
	s := &m.images
	s.scanning = false
	if s.busy || !m.currentFrame() || len(m.frame.imageWants) == 0 {
		return nil
	}
	s.clock++
	for _, wanted := range m.frame.imageWants {
		if a := s.assets[wanted.ref]; a != nil {
			a.used = s.clock
		}
	}
	for _, wanted := range m.frame.imageWants {
		ref := wanted.ref
		a := s.assets[ref]
		if a == nil {
			backend := m.backend.(imageBackend) // negotiated by probeImages
			s.busy = true
			life := m.ctx
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(life, 10*time.Second)
				defer cancel()
				var pixels *image.NRGBA
				if image, err := backend.GetImage(ctx, ref); err == nil {
					pixels, _ = decodePreview(image.Data)
				}
				return imageFetched{ref, pixels}
			}
		}
		if !m.canInlineImages() || !s.screen.up || a.pixels == nil {
			continue
		}
		cols, rows := previewCells(a.pixels, wanted.width)
		if cols == 0 || rows == 0 {
			continue
		}
		old := a.placed
		if old.screen == s.screen && old.cols == cols && old.rows == rows && old.id != 0 {
			continue
		}
		if s.nextID == 0 {
			s.nextID = 256 + rand.IntN(0xefffff-256)
		}
		s.nextID = s.nextID%0xffffff + 1
		p := imagePlacement{s.screen, cols, rows, s.nextID}
		s.busy, s.placing = true, p.id
		out, life, pixels := s.output, m.ctx, a.pixels
		return func() tea.Msg {
			return imagePlaced{ref, p, out.uploadImage(life, p, old, kittyUpload(pixels, p))}
		}
	}
	return nil
}

func (m *Model) cacheImage(msg imageFetched) tea.Cmd {
	s := &m.images
	s.busy = false
	if s.assets == nil {
		s.assets = make(map[sdk.ImageRef]*imagePreview, previewCacheSize)
	}
	var cleanup tea.Cmd
	if len(s.assets) >= previewCacheSize {
		var key sdk.ImageRef
		var victim *imagePreview
		for k, a := range s.assets {
			if victim == nil || a.used < victim.used {
				key, victim = k, a
			}
		}
		delete(s.assets, key)
		if p := victim.placed; p.id != 0 && s.output != nil {
			out, life := s.output, m.ctx
			cleanup = func() tea.Msg { out.deleteImage(life, p); return nil }
		}
	}
	s.assets[msg.ref] = &imagePreview{pixels: msg.pixels, used: s.clock}
	s.revision++
	return cleanup
}

func (m *Model) imageError(msg uv.KittyGraphicsEvent) tea.Cmd {
	if !m.canInlineImages() || msg.Options.ID == 0 || string(msg.Payload) == "OK" {
		return nil
	}
	s := &m.images
	ours := s.placing == msg.Options.ID
	for _, a := range s.assets {
		ours = ours || a.placed.id == msg.Options.ID && a.placed.screen == s.screen
	}
	if !ours {
		return nil
	}
	s.mode, s.revision = ImageBlocks, s.revision+1
	return m.notify(m.active, "Terminal rejected an inline image; use details for a pixel preview", x.Warning)
}

func (m *Model) attachmentRow(t *threadState, width int) displayRow {
	var labels []string
	for _, item := range t.attachments {
		labels = append(labels, "▣ "+oneLine(item.label))
	}
	if t.attaching {
		labels = append(labels, "◌ attaching image…")
	}
	return displayRow{text: m.control(ansi.Truncate(strings.Join(labels, "  "), width, "…"))}
}
