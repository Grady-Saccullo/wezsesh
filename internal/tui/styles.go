package tui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// styles is the cached lipgloss style set the renderer uses across all
// frames. It is built once per Model and re-keyed if Config.Colors
// changes (the v0.1 picker has no live re-theming, so this is a one-
// shot allocation at construction).
//
// Color resolution: every field falls back to a sensible default when
// the corresponding *string in Config.Colors is nil. Defaults track
// the V1 design (`docs/tui_v1.md`): rounded borders in muted grey,
// accent in bright cyan, success in green, error in red.
type styles struct {
	// Panels.
	picker        lipgloss.Style // bordered + titled "Workspaces" panel
	preview       lipgloss.Style // bordered + titled preview panel
	footer        lipgloss.Style // bottom hint line

	// Panel titles (rendered inside the top border).
	pickerTitle   lipgloss.Style
	previewTitle  lipgloss.Style

	// Header / status text.
	header        lipgloss.Style // top "wezsesh · active=…" bar
	headerActive  lipgloss.Style // emphasised active workspace name
	headerCount   lipgloss.Style // muted "N workspaces" suffix

	// Row rendering.
	row           lipgloss.Style // unselected row
	rowCursor     lipgloss.Style // cursor / selected row
	rowName       lipgloss.Style
	rowNameCursor lipgloss.Style
	rowMeta       lipgloss.Style // age / tags / etc.
	rowMetaCursor lipgloss.Style

	// Markers.
	markerActive  lipgloss.Style
	markerLive    lipgloss.Style
	markerSaved   lipgloss.Style
	markerPinned  lipgloss.Style
	markerUnsaved lipgloss.Style

	// Footer hints.
	footerKey     lipgloss.Style // bound key name (bold accent)
	footerLabel   lipgloss.Style // verb / hint label (muted)
	footerSep     lipgloss.Style // between hints

	// Status / errors.
	status        lipgloss.Style // status text (success or muted)
	statusError   lipgloss.Style // error / IPC_TIMEOUT etc.

	// Modal overlay.
	modalBox      lipgloss.Style
	modalTitle    lipgloss.Style
	modalHint     lipgloss.Style
}

// defaultPalette is the colour set used when Config.Colors leaves a
// slot at its zero (*string == nil). Hex values picked to read on both
// dark and light terminals; the numbers are bright enough to register
// against a dark background but stay readable on solarized-light too.
type palette struct {
	border         color.Color
	accent         color.Color
	muted          color.Color
	error          color.Color
	success        color.Color
	focusBG        color.Color
	focusFG        color.Color
	matchHighlight color.Color
	liveMarker     color.Color
	savedMarker    color.Color
	pinnedMarker   color.Color
	activeMarker   color.Color
}

func defaultPalette() palette {
	return palette{
		border:         lipgloss.Color("#5C6370"),
		accent:         lipgloss.Color("#56B6C2"), // cyan
		muted:          lipgloss.Color("#7F848E"),
		error:          lipgloss.Color("#E06C75"),
		success:        lipgloss.Color("#98C379"),
		focusBG:        lipgloss.Color("#3E4452"),
		focusFG:        lipgloss.Color("#FFFFFF"),
		matchHighlight: lipgloss.Color("#E5C07B"), // yellow
		liveMarker:     lipgloss.Color("#98C379"), // green
		savedMarker:    lipgloss.Color("#7F848E"), // muted
		pinnedMarker:   lipgloss.Color("#E5C07B"), // yellow
		activeMarker:   lipgloss.Color("#56B6C2"), // cyan
	}
}

// resolveColor returns the override when non-nil, else the default.
// The override is taken literally — lipgloss.Color accepts ANSI names,
// 0–255 indices, and hex strings.
func resolveColor(override *string, dflt color.Color) color.Color {
	if override == nil || *override == "" {
		return dflt
	}
	return lipgloss.Color(*override)
}

// buildStyles assembles the style set from the Config's Colors slot.
// Every field always returns a usable lipgloss.Style; missing colours
// fall back to defaultPalette.
func buildStyles(c Colors) styles {
	p := defaultPalette()
	border := resolveColor(c.Muted, p.border)
	accent := resolveColor(c.Accent, p.accent)
	muted := resolveColor(c.Muted, p.muted)
	errClr := resolveColor(c.Error, p.error)
	success := resolveColor(c.Success, p.success)
	focusBG := resolveColor(c.FocusBG, p.focusBG)
	live := resolveColor(c.LiveMarker, p.liveMarker)
	saved := resolveColor(c.SavedMarker, p.savedMarker)

	rounded := lipgloss.RoundedBorder()

	return styles{
		picker: lipgloss.NewStyle().
			Border(rounded).
			BorderForeground(border).
			Padding(0, 1),
		preview: lipgloss.NewStyle().
			Border(rounded).
			BorderForeground(border).
			Padding(0, 1),
		footer: lipgloss.NewStyle().
			Foreground(muted),

		pickerTitle: lipgloss.NewStyle().
			Foreground(accent).
			Bold(true),
		previewTitle: lipgloss.NewStyle().
			Foreground(accent).
			Bold(true),

		header: lipgloss.NewStyle().
			Foreground(muted),
		headerActive: lipgloss.NewStyle().
			Foreground(accent).
			Bold(true),
		headerCount: lipgloss.NewStyle().
			Foreground(muted),

		row: lipgloss.NewStyle(),
		rowCursor: lipgloss.NewStyle().
			Background(focusBG).
			Foreground(p.focusFG),
		rowName: lipgloss.NewStyle(),
		rowNameCursor: lipgloss.NewStyle().
			Background(focusBG).
			Foreground(p.focusFG).
			Bold(true),
		rowMeta: lipgloss.NewStyle().
			Foreground(muted),
		rowMetaCursor: lipgloss.NewStyle().
			Background(focusBG).
			Foreground(muted),

		markerActive:  lipgloss.NewStyle().Foreground(p.activeMarker),
		markerLive:    lipgloss.NewStyle().Foreground(live),
		markerSaved:   lipgloss.NewStyle().Foreground(saved),
		markerPinned:  lipgloss.NewStyle().Foreground(p.pinnedMarker),
		markerUnsaved: lipgloss.NewStyle().Foreground(muted),

		footerKey: lipgloss.NewStyle().
			Foreground(accent).
			Bold(true),
		footerLabel: lipgloss.NewStyle().
			Foreground(muted),
		footerSep: lipgloss.NewStyle().
			Foreground(muted),

		status: lipgloss.NewStyle().
			Foreground(success),
		statusError: lipgloss.NewStyle().
			Foreground(errClr).
			Bold(true),

		modalBox: lipgloss.NewStyle().
			Border(rounded).
			BorderForeground(accent).
			Padding(0, 1),
		modalTitle: lipgloss.NewStyle().
			Foreground(accent).
			Bold(true),
		modalHint: lipgloss.NewStyle().
			Foreground(muted),
	}
}
