package tui

const (
	narrowPaneWidth = 40
	shortPaneHeight = 12
	minPaneWidth    = 12
	minPaneHeight   = 3
)

// Use one geometry for editor reflow, cached transcript blocks and placement.
// Two cells per side in a normal pane; one per side in a narrow one. The
// right edge can contain a scroll rail without changing the content width.
type paneLayout struct {
	edge, width, editorWidth, editorRows int
	narrow, short, usable                bool
}

func (m *Model) pane() paneLayout {
	p := paneLayout{
		edge: 2, editorRows: max(1, m.height/4),
		narrow: m.width < narrowPaneWidth, short: m.height < shortPaneHeight,
		usable: m.width >= minPaneWidth && m.height >= minPaneHeight,
	}
	label := 8 // reserve the widest prompt label, even when idle
	if p.narrow {
		p.edge, label = 1, 2
	}
	p.width = max(1, m.width-2*p.edge)
	p.editorWidth = max(1, p.width-label)
	return p
}

func (m *Model) canScrollTranscript() bool {
	return m.pane().usable && (!m.currentFrame() || m.frame.transcriptHeight > 0)
}
