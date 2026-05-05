package tui

import (
	tea "charm.land/bubbletea/v2"
)

// keyAction is the categorical decision the key handler emits. Update
// reads the action and decides which dispatch / state transition to
// fire. Centralising the lookup means filter mode (which suppresses
// most actions) only needs one early return.
type keyAction int

const (
	actNone keyAction = iota
	actUp
	actDown
	actTop
	actBottom
	actSwitch
	actLoad
	actFilterEnter // entering filter mode (`/`)
	actFilterExit  // leaving filter mode (Esc)
	actFilterClear // Ctrl-U
	actFilterDel   // backspace inside filter mode
	actHelp
	actPreview
	actQuit
	actMark
	actClearMarks
	actPin
)

// matchNavKey resolves a tea.KeyPressMsg against the configured KeyMap
// in nav mode. Empty bindings disable the action. Returns actNone if
// nothing matches. Nav mode never produces a literal rune — `j`/`k`
// etc. are bindings, not text — so the rune-bearing variant lives in
// matchFilterKey to avoid leaking a stray rune into nav-mode callers
// (a footgun for the modal handlers landing in T-702).
func matchNavKey(km KeyMap, msg tea.KeyPressMsg) keyAction {
	str := msg.String()

	switch str {
	case "esc":
		return actQuit
	case "enter":
		return actSwitch
	case "up", "ctrl+p":
		return actUp
	case "down", "ctrl+n":
		return actDown
	}
	switch str {
	case km.Up:
		return actUp
	case km.Down:
		return actDown
	case km.Top:
		return actTop
	case km.Bottom:
		return actBottom
	case km.Switch:
		return actSwitch
	case km.Load:
		return actLoad
	case km.Filter:
		return actFilterEnter
	case km.Help:
		return actHelp
	case km.Preview:
		return actPreview
	case km.Quit:
		return actQuit
	case km.Mark, km.MarkAlt:
		return actMark
	case km.ClearMarks:
		return actClearMarks
	case km.Pin:
		return actPin
	}
	return actNone
}

// matchFilterKey resolves a tea.KeyPressMsg in filter mode. In modeFilter,
// letter keys are literal characters and the picker MUST NOT receive nav
// actions for j/k/g/G/etc. Only the explicitly-allowed "operate the
// picker while filtering" keys (↑/↓/Ctrl-P/Ctrl-N, Enter, Esc, Ctrl-U,
// backspace) map to actions; everything else returns (actNone, rune)
// where rune is the literal character to append to the filter buffer
// (or 0 if no printable rune was carried).
func matchFilterKey(msg tea.KeyPressMsg) (keyAction, rune) {
	str := msg.String()

	switch str {
	case "esc":
		return actFilterExit, 0
	case "enter":
		return actSwitch, 0
	case "up", "ctrl+p":
		return actUp, 0
	case "down", "ctrl+n":
		return actDown, 0
	case "ctrl+u":
		return actFilterClear, 0
	case "backspace", "ctrl+h":
		return actFilterDel, 0
	}
	// Literal characters: use Text so shifted/printable runes pass
	// through verbatim. The filter buffer is rune-stored (we trim by
	// rune in update.go's actFilterDel branch).
	if r := keyToRune(msg); r != 0 {
		return actNone, r
	}
	return actNone, 0
}

// keyToRune extracts the printable rune from a key press, if any. Used
// only in filter mode where we want literal characters.
func keyToRune(msg tea.KeyPressMsg) rune {
	if msg.Text != "" {
		// Text is the raw printable; take the first rune. Multi-rune
		// Text (composed sequences) are appended as-is by the caller
		// via msg.Text rather than this single-rune helper.
		for _, r := range msg.Text {
			return r
		}
	}
	return 0
}
