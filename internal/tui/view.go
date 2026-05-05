package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"

	"github.com/Grady-Saccullo/wezsesh/internal/nameval"
)

// View renders the picker. The function is intentionally allocation-aware
// — string concatenation inside hot loops would dominate the per-frame
// budget on a 1000-row picker. lipgloss is reserved for the final wrap;
// row content is built with a strings.Builder.
func (m *Model) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}

	var sb strings.Builder

	// Header.
	sb.WriteString(m.renderHeader())
	sb.WriteString("\n")

	// Body: split between picker and (optional) preview pane.
	left := m.renderPicker()
	if m.previewShown {
		preview := m.renderPreview()
		// JoinHorizontal handles cell-aware width; the picker takes
		// (1 - PreviewWidth) of the terminal width, preview takes the
		// remainder. lipgloss is the only consumer of styled output;
		// the disk-sourced strings have already been sanitised in
		// sanitiseRows so we can hand them straight in.
		sb.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, left, " ", preview))
	} else {
		sb.WriteString(left)
	}

	// Footer.
	sb.WriteString("\n")
	sb.WriteString(m.renderFooter())

	return tea.NewView(sb.String())
}

// renderHeader is the one-line picker title + active workspace marker.
func (m *Model) renderHeader() string {
	active := nameval.SanitizeForDisplay(m.data.ActiveWorkspace)
	if active == "" {
		active = "—"
	}
	return fmt.Sprintf("wezsesh · active=%s · %d workspaces", active, len(m.rows))
}

// renderPicker renders the workspace list. The cursor row is prefixed
// with "›"; non-cursor rows get a space.
func (m *Model) renderPicker() string {
	visible := m.visibleRows()
	if len(visible) == 0 {
		return "(no workspaces)"
	}
	var sb strings.Builder
	width := m.pickerWidth()
	for vi, ri := range visible {
		row := m.rows[ri]
		caret := "  "
		if vi == m.cursor {
			caret = "› "
		}
		sb.WriteString(caret)
		sb.WriteString(m.renderRow(row, width-runewidth.StringWidth(caret)))
		if vi+1 < len(visible) {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// renderRow lays out one picker row. Markers + name + age + tags. The
// name is middle-truncated to the available width budget; markers are
// fixed-width single cells. Disk-sourced strings have already been
// sanitised at construction; this function does no sanitisation.
func (m *Model) renderRow(r WorkspaceRow, budget int) string {
	if budget < 8 {
		budget = 8
	}
	var sb strings.Builder
	// Markers. Each marker contributes its own configured width so the
	// renderer remains stable across configurations.
	if r.Pinned && m.cfg.Markers.Pinned != "" {
		sb.WriteString(m.cfg.Markers.Pinned)
	} else if r.Active && m.cfg.Markers.Active != "" {
		sb.WriteString(m.cfg.Markers.Active)
	} else if r.Live && m.cfg.Markers.Live != "" {
		sb.WriteString(m.cfg.Markers.Live)
	} else if !r.Saved && m.cfg.Markers.Unsaved != "" {
		sb.WriteString(m.cfg.Markers.Unsaved)
	} else {
		sb.WriteByte(' ')
	}
	sb.WriteByte(' ')

	// Name (middle-truncated to budget − markers − age − tags reserve).
	// reserve covers " · 99d" age (≤8 cells) plus a short trailing tag
	// string (~10 cells); see §15.5 row layout. Tags are not clamped
	// here in v0.1, so a row carrying many long tags can overrun the
	// budget — the picker still renders cleanly because the terminal
	// wraps; tag-clamping is a v0.2 candidate.
	const reserve = 18
	nameBudget := budget - reserve
	if nameBudget < 4 {
		nameBudget = 4
	}
	sb.WriteString(nameval.TruncateMiddle(r.Name, nameBudget))

	// Age. m.now() is the test seam; production routes through time.Now
	// and preview/picker share the clock so snapshot-render tests can
	// pin a fixed instant.
	if !r.Mtime.IsZero() {
		sb.WriteString(" · ")
		sb.WriteString(humanAge(m.now().Sub(r.Mtime)))
	}
	// Tags.
	if len(r.Tags) > 0 {
		sb.WriteString(" #")
		sb.WriteString(strings.Join(r.Tags, " #"))
	}
	return sb.String()
}

// renderFooter is the bottom hint line. Three variants: filter mode,
// quit-confirm, default. The inline quit-confirm is the §13.8 contract
// (NOT a modal).
func (m *Model) renderFooter() string {
	if m.confirmQuit {
		return nameval.SanitizeForDisplay(m.status)
	}
	if m.mode == modeFilter {
		// "/<buf>_" makes the filter visible without a separate widget.
		return fmt.Sprintf("/%s_   [esc=cancel  enter=switch  ctrl-u=clear]",
			nameval.SanitizeForDisplay(m.filterBuf))
	}
	keys := m.cfg.Keys
	hint := fmt.Sprintf("[%s=switch  %s=load  %s=filter  %s=quit]",
		keys.Switch, keys.Load, keys.Filter, keys.Quit)
	if m.status != "" {
		return nameval.SanitizeForDisplay(m.status) + "  " + hint
	}
	return hint
}

// pickerWidth is the column budget the picker should consume. Honours
// the preview pane: when shown, the picker takes (1 - PreviewWidth) of
// total. width=0 (pre-WindowSizeMsg) ⇒ 80 as a fallback so initial Init
// renders something useful.
func (m *Model) pickerWidth() int {
	w := m.width
	if w <= 0 {
		w = 80
	}
	if !m.previewShown {
		return w
	}
	pw := int(float64(w) * (1.0 - m.cfg.PreviewWidth))
	if pw < 20 {
		pw = 20
	}
	return pw
}

// humanAge renders a duration in compact form (e.g. "5m", "3h", "2d").
// Negative durations (clock skew) round up to "0s".
func humanAge(d time.Duration) string {
	if d < 0 {
		return "0s"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
