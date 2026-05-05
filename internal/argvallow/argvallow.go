// Package argvallow owns the program-basename allowlist that gates
// resurrect's argv-restore RCE surface (§8.13, §9.11, §9.12).
//
// Source of truth: internal/argvallow/default.txt (//go:embed).
// The same file is codegen'd into plugin/wezsesh/default_allowlist.lua
// via `go run ./internal/argvallow/codegen` (CI freshness gate per §17.4
// "Argv default list sync"). The Go embedded set and the Lua return
// table MUST be byte-equal under codegen — both Go and Lua sides
// consult the same baseline.
//
// User additions extend the list (basename($SHELL) plus
// resurrect_argv_allowlist user-config); they cannot REMOVE default
// entries. Per-snapshot opt-out is intentionally NOT offered (§8.13).
package argvallow

import (
	_ "embed"
	"path/filepath"
	"strings"
)

//go:embed default.txt
var defaultListRaw string

// Default returns the v0.1 baseline list (program basenames) in the
// declaration order recorded in default.txt. Source of truth:
// internal/argvallow/default.txt; the same file is codegen'd into
// plugin/wezsesh/default_allowlist.lua.
func Default() []string {
	return parseList(defaultListRaw)
}

// Auditor scans snapshots and reports argv basenames that would be
// skipped by the active policy. The active policy is:
//
//	default + basename($SHELL) + userAdditions
//
// User additions cannot remove default entries.
type Auditor struct {
	allowed map[string]struct{}
	// list preserves insertion order for deterministic enumeration
	// (Default first, then shell, then userAdditions). Useful for
	// audit reporting and CLI surfaces.
	list []string
}

// NewAuditor builds the active policy from the embedded default list,
// the basename of shell (caller-resolved $SHELL; pass "" to skip), and
// any user additions. Duplicates are deduplicated; the original
// declaration order is preserved.
func NewAuditor(shell string, userAdditions []string) *Auditor {
	a := &Auditor{allowed: make(map[string]struct{}, 64)}
	for _, name := range parseList(defaultListRaw) {
		a.add(name)
	}
	if shell != "" {
		a.add(filepath.Base(shell))
	}
	for _, name := range userAdditions {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		a.add(filepath.Base(name))
	}
	return a
}

// Allows reports whether basename is in the active allowlist. The
// lookup is O(1) — backed by a map[string]struct{}. basename should
// already be the basename (caller's responsibility); Allows does NOT
// re-base, so passing a full path will fail-CLOSED. Empty input
// returns false.
func (a *Auditor) Allows(basename string) bool {
	if a == nil || basename == "" {
		return false
	}
	_, ok := a.allowed[basename]
	return ok
}

// List returns a copy of the active allowlist in insertion order
// (default → shell → user). The returned slice is independent of the
// Auditor's internal state.
func (a *Auditor) List() []string {
	if a == nil {
		return nil
	}
	out := make([]string, len(a.list))
	copy(out, a.list)
	return out
}

func (a *Auditor) add(name string) {
	if name == "" {
		return
	}
	if _, exists := a.allowed[name]; exists {
		return
	}
	a.allowed[name] = struct{}{}
	a.list = append(a.list, name)
}

// parseList splits the embedded default.txt into entries. Blank lines
// and lines starting with '#' are skipped to keep the source tolerant
// of editor-added trailing newlines and future commenting; the codegen
// tool emits no comments and no blank lines, so any leniency here is
// invisible to the Lua side.
func parseList(raw string) []string {
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}
