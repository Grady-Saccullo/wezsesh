package tui

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
	default:
		return "none"
	}
}

// activeModal returns the model's active modal kind. T-702 will route
// keys here; the v0.1 picker always returns modalNone.
func (m *Model) activeModal() modalKind { return m.modal }
