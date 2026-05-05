package tui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// modal.go is the seat for any future overlay modals (rename, tag-edit,
// confirm-delete, etc.). T-701 ships the picker frame plus the inline
// quit-confirm; the dedicated modals land with the verbs in T-702+.
//
// The file exists in T-701 to anchor the bubbletea-tui-engineer's
// invariant: modals are NEVER used for the quit-mid-op flow (§13.8) —
// that path is the inline status line in view.go.
//
// When modals are added the discipline is:
//   - Modals are an overlay layer, NOT a separate program. The model
//     keeps a `modal` field of an interface type and Update routes keys
//     to the modal first when present.
//   - Filter mode and modals are mutually exclusive: opening a modal
//     leaves filter mode (calling refreshFilter() with an empty buf).
//   - Modal text rendered from disk-sourced strings MUST pass through
//     nameval.SanitizeForDisplay (the §15.4 invariant).
//   - Modals MUST NOT spawn goroutines that outlive tea.Run; any
//     follow-up dispatch goes through the same startDispatch pipeline.

// modalKind tags the overlay variants T-702+ will land. The current
// surface ships only the inline quit-confirm bar (rendered by view.go
// without claiming this enum), so the values here are the planned
// menu — they unblock the type-switch the modal dispatcher will use
// once the rename / tag-edit / confirm-delete flows land.
type modalKind int

const (
	modalNone modalKind = iota
	modalRename
	modalTagEdit
	modalConfirmDelete
	modalNewWorkspace
)

// String makes modalKind play nicely with fmt and any future logging.
func (m modalKind) String() string {
	switch m {
	case modalRename:
		return "rename"
	case modalTagEdit:
		return "tag_edit"
	case modalConfirmDelete:
		return "confirm_delete"
	case modalNewWorkspace:
		return "new_workspace"
	default:
		return "none"
	}
}

// activeModal returns the model's active modal kind. T-702 will route
// keys here; the v0.1 picker always returns modalNone.
func (m *Model) activeModal() modalKind { return m.modal }

// openNewWorkspaceModal lazy-allocates a textinput.Model and switches
// the picker into the new-workspace modal. Filter mode is dismissed
// per the modal-discipline rule (filter and modals are mutually
// exclusive).
func (m *Model) openNewWorkspaceModal() {
	if m.mode == modeFilter {
		m.mode = modeNav
		m.filterBuf = ""
		m.refreshFilter()
	}
	if m.textInput == nil {
		ti := textinput.New()
		m.textInput = &ti
	}
	m.textInput.Reset()
	m.textInput.Placeholder = "workspace name"
	m.textInput.Prompt = "› "
	m.textInput.CharLimit = 64
	m.modal = modalNewWorkspace
	m.status = ""
}

// closeModal clears the active modal and (if a textinput is open)
// blurs it. Safe to call when no modal is open.
func (m *Model) closeModal() {
	m.modal = modalNone
	if m.textInput != nil {
		m.textInput.Blur()
	}
}

// handleNewWorkspaceKey routes a key press while the new-workspace
// modal is open. Esc cancels; Enter submits (must be non-empty);
// every other key is forwarded to the textinput's Update.
func (m *Model) handleNewWorkspaceKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.closeModal()
		return m, nil
	case "enter":
		name := strings.TrimSpace(m.textInput.Value())
		if name == "" {
			// No-op: keep the modal open until the user types or
			// cancels.
			return m, nil
		}
		m.closeModal()
		// New-workspace via the picker takes the rename trick: the
		// switch verb's "neither live nor saved" branch renames the
		// active workspace into `name`, keeping the current window in
		// place. Empty cwd → no `cd` is sent into the pane. The CLI
		// `wezsesh new` path still dispatches the explicit-spawn
		// `new` verb when callers want a fresh window.
		return m, m.startDispatch("switch", name, map[string]any{
			"name": name,
			"cwd":  "",
		})
	}
	if m.textInput == nil {
		return m, nil
	}
	ti, cmd := m.textInput.Update(msg)
	m.textInput = &ti
	return m, cmd
}
