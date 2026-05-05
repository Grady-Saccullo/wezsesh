package state

import (
	"encoding/json"
	"fmt"
)

// rawState is the on-disk JSON shape used during migration. It carries
// BOTH the legacy `pins` key (v1 prior to the live_pins rename) and the
// current `live_pins` key so a single Unmarshal pass can decide which
// branch the file needs. Once migrate returns, callers operate on the
// in-memory State type whose only pin field is LivePins.
type rawState struct {
	Version  int              `json:"version"`
	Usage    map[string]Usage `json:"usage"`
	LivePins []string         `json:"live_pins"`
	Pins     []string         `json:"pins"`
}

// migrate parses raw state.json bytes and returns a normalised State, the
// version that needs to be backed up (0 means no backup; non-zero means
// the caller MUST write `state.json.v<N>.bak` containing the original
// raw bytes before persisting the fresh state), and a `migrated` flag
// reporting whether the on-disk shape differs from a re-encode of the
// returned State (legacy `pins[]` rename, pruned live_pins, etc.).
//
// repoHas is the snapshot-existence callback the state package cannot
// inspect on its own (avoids a cycle with internal/snapshots). Any name
// in live_pins for which repoHas(name) returns true is silently dropped:
// the sidecar wins for saved workspaces (§13.11).
//
// Migration matrix:
//
//	empty input        → fresh v=1 State, backup=0, migrated=false
//	version > 1        → fresh v=1 State, backup=version, migrated=true
//	version == 1, pins → rename pins → live_pins, prune via repoHas,
//	                     backup=0, migrated=true
//	version == 1       → use as-is, prune live_pins via repoHas;
//	                     migrated=true iff prune dropped any entry or the
//	                     on-disk live_pins was JSON null
func migrate(raw []byte, repoHas func(string) bool) (*State, int, bool, error) {
	if len(raw) == 0 {
		return newState(), 0, false, nil
	}
	var rs rawState
	if err := json.Unmarshal(raw, &rs); err != nil {
		return nil, 0, false, fmt.Errorf("state: parse: %w", err)
	}
	if rs.Version > currentVersion {
		return newState(), rs.Version, true, nil
	}
	hadLegacyPins := len(rs.Pins) > 0
	pins := rs.LivePins
	if len(pins) == 0 && hadLegacyPins {
		pins = rs.Pins
	}
	pruned := pruneLivePins(pins, repoHas)
	usage := rs.Usage
	if usage == nil {
		usage = make(map[string]Usage)
	}
	migrated := hadLegacyPins || len(pruned) != len(pins)
	return &State{
		Version:  currentVersion,
		Usage:    usage,
		LivePins: pruned,
	}, 0, migrated, nil
}

// pruneLivePins drops names with a corresponding snapshot. The sidecar
// is authoritative for saved workspaces (§13.11); a stale live_pins
// entry would otherwise double-count. Order is preserved; duplicates
// are de-duped. Always returns a non-nil slice so the encoded JSON is
// `[]` rather than `null` (§10.4).
func pruneLivePins(pins []string, repoHas func(string) bool) []string {
	out := make([]string, 0, len(pins))
	if len(pins) == 0 {
		return out
	}
	seen := make(map[string]struct{}, len(pins))
	for _, name := range pins {
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		if repoHas != nil && repoHas(name) {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func newState() *State {
	return &State{
		Version:  currentVersion,
		Usage:    make(map[string]Usage),
		LivePins: []string{},
	}
}
