package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/Grady-Saccullo/wezsesh/internal/nameval"
	"github.com/Grady-Saccullo/wezsesh/internal/snapshots"
)

// renderPreview is the right-pane content for the cursor's row. Shows
// display name, mtime, tags, per-tab/per-pane summary, and the "live
// vs saved divergence" hint when both exist for the same name. All
// disk-sourced strings (including snapshot pane titles, CWDs, process
// names) flow through SanitizeForDisplay before composition — the §15.4
// invariant binds here as much as anywhere else in the renderer.
func (m *Model) renderPreview() string {
	row, ok := m.rowAt(m.cursor)
	if !ok {
		return previewBox("(no selection)", m.previewBudget())
	}

	var sb strings.Builder
	// Header.
	sb.WriteString(nameval.SanitizeForDisplay(row.Name))
	sb.WriteString("\n")

	// Path / status.
	if row.Snapshot != nil && row.Snapshot.Path != "" {
		sb.WriteString("path: ")
		sb.WriteString(nameval.SanitizeForDisplay(row.Snapshot.Path))
		sb.WriteString("\n")
	}
	statusLine := []string{}
	switch row.Source {
	case SourceLive:
		statusLine = append(statusLine, "live")
	case SourceSaved:
		statusLine = append(statusLine, "saved")
	case SourceExternal:
		statusLine = append(statusLine, "external")
	}
	if row.Active {
		statusLine = append(statusLine, "active")
	}
	if row.Pinned {
		statusLine = append(statusLine, "pinned")
	}
	if len(statusLine) > 0 {
		sb.WriteString("status: ")
		sb.WriteString(strings.Join(statusLine, ", "))
		sb.WriteString("\n")
	}

	// External rows: surface the provider-supplied CWD as the path.
	// Selecting one renames the active workspace into row.Name and
	// `cd`s the active pane into this directory; showing the path here
	// is the user's only chance to confirm before pressing Enter. Git
	// status / branch / etc. are v1 future work.
	if row.Source == SourceExternal && row.CWD != "" {
		sb.WriteString("path: ")
		sb.WriteString(nameval.SanitizeForDisplay(row.CWD))
		sb.WriteString("\n")
		sb.WriteString("(switching renames the active workspace and cd's the pane)\n")
	}

	// Mtime.
	if !row.Mtime.IsZero() {
		sb.WriteString("mtime: ")
		sb.WriteString(row.Mtime.Format(time.RFC3339))
		sb.WriteString("\n")
	}

	// Tags.
	if len(row.Tags) > 0 {
		sb.WriteString("tags: ")
		// Already sanitised in sanitiseRows.
		sb.WriteString(strings.Join(row.Tags, ", "))
		sb.WriteString("\n")
	}

	// Per-tab / per-pane summary from the snapshot if we have one.
	if row.Snapshot != nil && row.Snapshot.State != nil {
		sb.WriteString("\n")
		sb.WriteString(renderSnapshotState(row.Snapshot.State))
	}

	// Divergence hint: a live row that also has a snapshot file means
	// the in-memory state may have drifted from the on-disk snapshot
	// since the last save.
	if row.Source == SourceLive && row.Snapshot != nil {
		sb.WriteString("\nnote: live state may diverge from snapshot\n")
	}

	return previewBox(sb.String(), m.previewBudget())
}

// renderSnapshotState walks the WorkspaceState tree and produces a
// compact tab/pane listing. All string fields are sanitised — pane CWDs
// and process names come from disk and could carry control bytes.
func renderSnapshotState(s *snapshots.WorkspaceState) string {
	if s == nil {
		return ""
	}
	var sb strings.Builder
	for wi, w := range s.WindowStates {
		if w.Title != nil && *w.Title != "" {
			sb.WriteString(fmt.Sprintf("window %d: %s\n", wi+1,
				nameval.SanitizeForDisplay(*w.Title)))
		} else {
			sb.WriteString(fmt.Sprintf("window %d:\n", wi+1))
		}
		for ti, t := range w.Tabs {
			label := ""
			if t.Title != nil {
				label = " " + nameval.SanitizeForDisplay(*t.Title)
			}
			sb.WriteString(fmt.Sprintf("  tab %d:%s\n", ti+1, label))
			if t.PaneTree != nil {
				renderPane(&sb, t.PaneTree, "    ")
			}
		}
	}
	return sb.String()
}

func renderPane(sb *strings.Builder, p *snapshots.PaneTree, indent string) {
	if p == nil {
		return
	}
	cwd := ""
	if p.CWD != nil {
		cwd = nameval.SanitizeForDisplay(*p.CWD)
	}
	proc := ""
	if p.Process != nil && p.Process.Name != nil {
		proc = nameval.SanitizeForDisplay(*p.Process.Name)
	} else if p.Process != nil && p.Process.LegacyString != nil {
		proc = nameval.SanitizeForDisplay(*p.Process.LegacyString)
	}
	if cwd != "" || proc != "" {
		sb.WriteString(indent)
		sb.WriteString("pane")
		if cwd != "" {
			sb.WriteString(" cwd=")
			sb.WriteString(cwd)
		}
		if proc != "" {
			sb.WriteString(" proc=")
			sb.WriteString(proc)
		}
		sb.WriteString("\n")
	}
	if p.Bottom != nil {
		renderPane(sb, p.Bottom, indent+"  ")
	}
	if p.Right != nil {
		renderPane(sb, p.Right, indent+"  ")
	}
}

// previewBox is a thin column wrap so the preview content sits visually
// distinct from the picker. lipgloss could handle this with NewStyle().
// Border(...), but the v0.1 picker keeps the renderer dependency-light.
func previewBox(s string, width int) string {
	if width < 10 {
		width = 10
	}
	lines := strings.Split(s, "\n")
	var sb strings.Builder
	for i, ln := range lines {
		// Truncate per-line by terminal cell width (NOT byte length),
		// otherwise CJK / wide-rune pane titles overrun the budget.
		// nameval.TruncateMiddle is itself cell-aware; we gate the call
		// on runewidth.StringWidth so single-byte ASCII paths skip the
		// allocation.
		if runewidth.StringWidth(ln) > width {
			ln = nameval.TruncateMiddle(ln, width)
		}
		sb.WriteString(ln)
		if i+1 < len(lines) {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// previewBudget is the column count the preview pane should consume.
func (m *Model) previewBudget() int {
	w := m.width
	if w <= 0 {
		w = 80
	}
	return w - m.pickerWidth() - 1
}
