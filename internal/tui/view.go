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
	left := m.renderPickerPanel()
	if m.previewShown {
		preview := m.renderPreviewPanel()
		sb.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, left, " ", preview))
	} else {
		sb.WriteString(left)
	}

	// Footer.
	sb.WriteString("\n")
	sb.WriteString(m.renderFooter())

	// Modal overlay (rendered after the footer so it sits at the bottom
	// of the visible frame; v0.1's modal is a single-line input and
	// fits in the footer row).
	if m.activeModal() != modalNone {
		sb.WriteString("\n")
		sb.WriteString(m.renderModal())
	}

	return tea.NewView(sb.String())
}

// renderHeader is the top status bar: program name, active workspace
// name (emphasised), and a muted total count.
func (m *Model) renderHeader() string {
	active := nameval.SanitizeForDisplay(m.data.ActiveWorkspace)
	if active == "" {
		active = "—"
	}
	prefix := m.styles.header.Render("wezsesh · active=")
	name := m.styles.headerActive.Render(active)
	count := m.styles.headerCount.Render(
		fmt.Sprintf(" · %d workspaces", len(m.rows)))
	return prefix + name + count
}

// renderPickerPanel wraps the picker rows in a rounded panel border with
// a "Workspaces" title.
func (m *Model) renderPickerPanel() string {
	body := m.renderPicker()
	width := m.pickerWidth()
	// Reserve 2 cells for the border. Padding is part of the style.
	inner := width - 2
	if inner < 10 {
		inner = 10
	}
	style := m.styles.picker.Width(inner)
	title := m.styles.pickerTitle.Render("Workspaces")
	return panelWithTitle(style, title, body)
}

// renderPreviewPanel wraps the preview content in a panel titled with
// the cursor row's name.
func (m *Model) renderPreviewPanel() string {
	body := m.renderPreview()
	width := m.previewBudget()
	inner := width - 2
	if inner < 10 {
		inner = 10
	}
	style := m.styles.preview.Width(inner)

	title := "preview"
	if row, ok := m.rowAt(m.cursor); ok && row.Name != "" {
		title = nameval.SanitizeForDisplay(row.Name)
	}
	titleStyled := m.styles.previewTitle.Render(title)
	return panelWithTitle(style, titleStyled, body)
}

// panelWithTitle renders `body` inside a bordered `style` panel and
// stitches `title` into the top border. lipgloss exposes BorderTop in
// v2 but no direct "embed text in border" helper, so we render the
// border first then overlay the title at column 2.
//
// The title is rendered above the body inside the bordered region for
// simplicity; the lipgloss-native top-border-text approach lands when
// the V1 Phase 13 bubbles.list / lipgloss.Border integration is in.
func panelWithTitle(style lipgloss.Style, title, body string) string {
	combined := title + "\n" + body
	return style.Render(combined)
}

// renderPicker renders the workspace list. The cursor row is prefixed
// with "›"; non-cursor rows get a space.
func (m *Model) renderPicker() string {
	visible := m.visibleRows()
	if len(visible) == 0 {
		return m.styles.rowMeta.Render("(no workspaces)")
	}
	var sb strings.Builder
	width := m.pickerWidth() - 4 // reserve panel border + padding
	if width < 10 {
		width = 10
	}
	for vi, ri := range visible {
		row := m.rows[ri]
		isCursor := vi == m.cursor
		caret := "  "
		if isCursor {
			caret = "› "
		}
		line := caret + m.renderRow(row, width-runewidth.StringWidth(caret), isCursor)
		if isCursor {
			// Pad the line to the panel width so the background
			// highlight covers the row edge-to-edge. lipgloss
			// styles measure cell-aware width.
			line = padToWidth(line, width)
			line = m.styles.rowCursor.Render(line)
		}
		sb.WriteString(line)
		if vi+1 < len(visible) {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// renderRow lays out one picker row: marker + name + age + tags. The
// name is middle-truncated; markers are colour-styled per role.
// `isCursor` toggles bold / focus styling on the parts that should
// emphasise when the cursor is on this row.
func (m *Model) renderRow(r WorkspaceRow, budget int, isCursor bool) string {
	if budget < 8 {
		budget = 8
	}
	var sb strings.Builder
	// Markers — coloured per role.
	sb.WriteString(m.renderMarker(r))
	sb.WriteByte(' ')

	// Name (middle-truncated to budget − markers − reserve for age + tags).
	const reserve = 18
	nameBudget := budget - reserve
	if nameBudget < 4 {
		nameBudget = 4
	}
	name := nameval.TruncateMiddle(r.Name, nameBudget)
	if isCursor {
		sb.WriteString(m.styles.rowNameCursor.Render(name))
	} else {
		sb.WriteString(m.styles.rowName.Render(name))
	}

	// Age and tags rendered as muted metadata.
	var meta strings.Builder
	if !r.Mtime.IsZero() {
		meta.WriteString(" · ")
		meta.WriteString(humanAge(m.now().Sub(r.Mtime)))
	}
	if len(r.Tags) > 0 {
		meta.WriteString(" #")
		meta.WriteString(strings.Join(r.Tags, " #"))
	}
	if meta.Len() > 0 {
		if isCursor {
			sb.WriteString(m.styles.rowMetaCursor.Render(meta.String()))
		} else {
			sb.WriteString(m.styles.rowMeta.Render(meta.String()))
		}
	}
	return sb.String()
}

// renderMarker returns a two-cell marker for the row. Layout:
//
//	cell 1 — focus glyph: ▶ (active), or blank.
//	cell 2 — origin glyph: ● (live), · (saved), * (external), or blank.
//
// The two-cell layout means the user always sees `state` as a colour-
// coded dot even when the focus arrow is on the row, so "this workspace
// is also live" is no longer hidden behind the active marker. The
// previous single-cell scheme had Active take priority over Live, which
// made every live workspace except the active one look like the only
// live one — the symptom Grady reported as "live workspaces aren't
// correctly showing."
//
// Pinned rows replace the focus column with the pinned glyph (typically
// the multi-character `[pinned]` token); the origin column still
// renders so a pinned-and-live row reads as `[pinned] ●`.
func (m *Model) renderMarker(r WorkspaceRow) string {
	var focus, origin string

	switch {
	case r.Pinned && m.cfg.Markers.Pinned != "":
		focus = m.styles.markerPinned.Render(m.cfg.Markers.Pinned)
	case r.Active && m.cfg.Markers.Active != "":
		focus = m.styles.markerActive.Render(m.cfg.Markers.Active)
	default:
		focus = " "
	}

	switch {
	case r.Source == SourceLive && m.cfg.Markers.Live != "":
		origin = m.styles.markerLive.Render(m.cfg.Markers.Live)
	case r.Source == SourceSaved:
		origin = m.styles.markerSaved.Render("·")
	case r.Source == SourceExternal:
		glyph := m.cfg.Markers.External
		if glyph == "" {
			glyph = "*"
		}
		origin = m.styles.markerUnsaved.Render(glyph)
	default:
		origin = " "
	}

	return focus + origin
}

// renderFooter is the bottom hint line. Three variants: filter mode,
// quit-confirm, default. The inline quit-confirm is the §13.8 contract
// (NOT a modal).
func (m *Model) renderFooter() string {
	if m.confirmQuit {
		return m.styles.statusError.Render(
			nameval.SanitizeForDisplay(m.status))
	}
	if m.mode == modeFilter {
		buf := nameval.SanitizeForDisplay(m.filterBuf)
		left := m.styles.footerKey.Render("/") +
			m.styles.footerLabel.Render(buf+"_")
		hints := m.renderKeyHint("esc", "cancel") + "  " +
			m.renderKeyHint("enter", "switch") + "  " +
			m.renderKeyHint("ctrl-u", "clear")
		return left + "   " + hints
	}
	keys := m.cfg.Keys
	hint := m.renderKeyHint(keys.Switch, "switch") + "  " +
		m.renderKeyHint(keys.Load, "load") + "  " +
		m.renderKeyHint(keys.Save, "save") + "  " +
		m.renderKeyHint(keys.New, "new") + "  " +
		m.renderKeyHint(keys.Filter, "filter") + "  " +
		m.renderKeyHint(keys.Quit, "quit")
	if m.status != "" {
		statusStyle := m.styles.status
		if strings.Contains(m.status, "IPC_TIMEOUT") ||
			strings.Contains(m.status, "failed") {
			statusStyle = m.styles.statusError
		}
		statusLine := statusStyle.Render(
			nameval.SanitizeForDisplay(m.status))
		return statusLine + "  " + hint
	}
	return hint
}

// renderKeyHint formats a single "[<key>=<label>]"-style hint where the
// key glyph is in the accent colour and the label is muted.
func (m *Model) renderKeyHint(key, label string) string {
	if key == "" {
		return ""
	}
	return m.styles.footerSep.Render("[") +
		m.styles.footerKey.Render(key) +
		m.styles.footerSep.Render("=") +
		m.styles.footerLabel.Render(label) +
		m.styles.footerSep.Render("]")
}

// renderModal is the overlay block for the active modal (currently
// only modalNewWorkspace). The modal sits below the footer and shows
// a one-line text input plus a hint row.
func (m *Model) renderModal() string {
	if m.activeModal() != modalNewWorkspace || m.textInput == nil {
		return ""
	}
	width := m.pickerWidth()
	if m.previewShown {
		// Prefer a centred modal over the full row when both panes
		// are visible.
		width = m.pickerWidth() + m.previewBudget() + 1
	}
	inner := width - 4
	if inner < 24 {
		inner = 24
	}
	title := m.styles.modalTitle.Render("New workspace")
	body := m.textInput.View()
	hint := m.styles.modalHint.Render("enter=create  esc=cancel")
	stack := lipgloss.JoinVertical(lipgloss.Left, title, body, hint)
	return m.styles.modalBox.Width(inner).Render(stack)
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

// padToWidth right-pads `s` with spaces so its visible cell-width
// equals `w`. Cell-aware via runewidth so wide CJK glyphs are accounted
// for. If `s` already exceeds `w`, the original string is returned.
func padToWidth(s string, w int) string {
	cur := runewidth.StringWidth(s)
	if cur >= w {
		return s
	}
	return s + strings.Repeat(" ", w-cur)
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
