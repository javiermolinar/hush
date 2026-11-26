package tui

import tea "charm.land/bubbletea/v2"

// A panel owns full content-width rows ending at the live rule. Placement
// (Sheet or Overlay) still controls dialog lifetime, not frame geometry.
type menuFrame struct {
	rows    []string
	targets []int // -1 for chrome, otherwise a row or action in the owner's input handler
	cursor  *tea.Cursor
}

func (m *Model) menuFrame(d *dialog, available int) menuFrame {
	if available <= 0 {
		return menuFrame{}
	}
	w, n := m.pane().width, min(available, max(1, m.height*7/10))
	var frame menuFrame
	switch {
	case d.planWorkspace != nil:
		frame.rows, frame.targets = m.planWorkspaceRows(d, w, n)
	case d.planHistory != nil:
		frame.rows, frame.targets = m.planHistoryRows(d, w, n)
	case d.planReview != nil:
		frame.rows, frame.targets = m.planReviewRows(d, w, n)
	case d.plan != nil:
		frame.rows, frame.targets = m.planRows(d, w, n)
	case d.git != nil:
		frame.rows, frame.targets = m.gitReviewRows(d, w, n)
	case d.workTrust != nil:
		frame.rows, frame.targets = m.workspaceTrustRows(d.workTrust, w, n)
	case d.config != nil:
		frame.rows, frame.targets, frame.cursor = m.configDockRows(d, w, n)
	case d.palette:
		frame.rows, frame.targets = m.paletteRows(d, w, n)
		frame.cursor = m.paletteCursor(d, w, len(frame.rows))
	case d.themePicker != nil:
		frame.rows, frame.targets = m.themeRows(d, w, n)
	case d.output != nil:
		frame.rows, frame.targets = m.outputRows(d, w, n)
	default:
		frame.rows, frame.targets = m.dialogRows(d, w, n)
	}
	return frame
}
