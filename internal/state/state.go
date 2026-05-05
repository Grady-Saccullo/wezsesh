// Package state backs $XDG_STATE_HOME/wezsesh/state.json (§8.11, §10.4).
// state.json holds usage stats (last_switched / switch_count per
// workspace) and pins for LIVE-ONLY workspaces — pins for SAVED
// workspaces live in the snapshot sidecar (§13.11), single source of
// truth.
//
// All disk writes go through safefs.AtomicWriteFile. Concurrent TUIs
// race on last-writer-wins (§10.4); the sync.Mutex on Store guards only
// the in-memory copy from concurrent goroutines in the same process.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Grady-Saccullo/wezsesh/internal/logger"
	"github.com/Grady-Saccullo/wezsesh/internal/safefs"
)

const (
	stateFilename  = "state.json"
	stateFileMode  = 0o600
	currentVersion = 1
)

// State is the on-disk JSON shape (§10.4). Marshalled with encoding/json
// (no MarshalIndent) so output is deterministic and diff-friendly under
// last-writer-wins concurrency.
type State struct {
	Version  int              `json:"version"`
	Usage    map[string]Usage `json:"usage"`
	LivePins []string         `json:"live_pins"`
}

// Usage is the per-workspace usage tuple recorded by RecordSwitch.
type Usage struct {
	LastSwitched int64 `json:"last_switched"`
	SwitchCount  int   `json:"switch_count"`
}

// Store is the binary-side handle to state.json. Construct via Open.
// Methods are safe for concurrent use within a process.
type Store struct {
	dir string
	log *logger.Logger

	mu    sync.Mutex
	state *State
}

// Open verifies stateDir is not a symlink, reads any existing state.json,
// runs migration (back up v>1 files; rename pins → live_pins on v=1;
// prune live_pins entries that have a corresponding snapshot), and
// returns a Store ready for reads/writes.
//
// Signature widening from §8.11 (Open(ctx) → Open(ctx, stateDir, log,
// repoHas)) — see Accepted findings in the task report.
//
// repoHas is the snapshot-existence callback used during live_pins
// sanity-pruning. The state package cannot import internal/snapshots
// (cycle risk; the package does not exist yet at this stage of the
// build), so the dependency is inverted via a callback. May be nil — in
// which case no pruning is performed.
func Open(ctx context.Context, stateDir string, log *logger.Logger, repoHas func(name string) bool) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if stateDir == "" {
		return nil, fmt.Errorf("state: empty stateDir")
	}

	ok, err := safefs.Enforce(stateDir, safefs.SymlinkRefuse, log)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, safefs.ErrIsSymlink
	}

	statePath := filepath.Join(stateDir, stateFilename)
	raw, readErr := readStateFile(statePath)
	if readErr != nil {
		return nil, readErr
	}

	st, backupVersion, migrated, err := migrate(raw, repoHas)
	if err != nil {
		return nil, err
	}

	if backupVersion > 0 {
		bak := fmt.Sprintf("%s.v%d.bak", stateFilename, backupVersion)
		if err := safefs.AtomicWriteFile(ctx, stateDir, bak, raw, stateFileMode); err != nil {
			return nil, fmt.Errorf("state: write backup: %w", err)
		}
	}
	// Persist whenever the on-disk shape differs from the migrated
	// state (legacy `pins[]` rename, pruned live_pins, v>1 reinit).
	// The fresh-no-file path leaves raw nil so migrated=false here.
	if migrated {
		if err := writeState(ctx, stateDir, st); err != nil {
			return nil, err
		}
	}

	return &Store{
		dir:   stateDir,
		log:   log,
		state: st,
	}, nil
}

// readStateFile returns the raw bytes of state.json, or nil if the file
// is absent. Symlinked state.json is rejected via SafeOpenForRead (which
// uses O_NOFOLLOW); the caller's Open contract surfaces ErrIsSymlink.
func readStateFile(path string) ([]byte, error) {
	f, err := safefs.SafeOpenForRead(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("state: open %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("state: read %s: %w", path, err)
	}
	return data, nil
}

// RecordSwitch increments switch_count and updates last_switched for
// name, then persists. ctx is honoured before the disk write.
func (s *Store) RecordSwitch(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("state: RecordSwitch: empty name")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state.Usage == nil {
		s.state.Usage = make(map[string]Usage)
	}
	u := s.state.Usage[name]
	u.SwitchCount++
	u.LastSwitched = time.Now().Unix()
	s.state.Usage[name] = u

	return writeState(ctx, s.dir, s.state)
}

// SetLivePin adds or removes name from live_pins. Used only when no
// snapshot exists for name; once the workspace is saved the pin
// migrates to the sidecar (§13.11). ctx is mandatory (§0.1 row 17).
func (s *Store) SetLivePin(ctx context.Context, name string, pinned bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("state: SetLivePin: empty name")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state.LivePins == nil {
		s.state.LivePins = []string{}
	}
	idx := indexOf(s.state.LivePins, name)
	switch {
	case pinned && idx < 0:
		s.state.LivePins = append(s.state.LivePins, name)
	case !pinned && idx >= 0:
		s.state.LivePins = append(s.state.LivePins[:idx], s.state.LivePins[idx+1:]...)
		if s.state.LivePins == nil {
			s.state.LivePins = []string{}
		}
	default:
		return nil
	}

	return writeState(ctx, s.dir, s.state)
}

// IsLivePinned returns true iff name is in live_pins. No disk I/O.
func (s *Store) IsLivePinned(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return indexOf(s.state.LivePins, name) >= 0
}

// LastSwitched returns the unix-seconds timestamp of the last switch to
// name, or 0 if name has never been switched to.
func (s *Store) LastSwitched(name string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Usage[name].LastSwitched
}

// SwitchCount returns the cumulative switch count for name (0 if never
// recorded).
func (s *Store) SwitchCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Usage[name].SwitchCount
}

// LivePins returns a sorted copy of the live_pins set so callers can
// freely mutate without racing the in-memory state.
func (s *Store) LivePins() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.state.LivePins) == 0 {
		return nil
	}
	out := make([]string, len(s.state.LivePins))
	copy(out, s.state.LivePins)
	sort.Strings(out)
	return out
}

// writeState marshals and atomically writes state.json. Caller is
// responsible for any required locking on st. Forces LivePins to a
// non-nil slice so the on-disk shape is `"live_pins":[]` rather than
// `"live_pins":null` (§10.4).
func writeState(ctx context.Context, dir string, st *State) error {
	if st.LivePins == nil {
		st.LivePins = []string{}
	}
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("state: marshal: %w", err)
	}
	return safefs.AtomicWriteFile(ctx, dir, stateFilename, data, stateFileMode)
}

func indexOf(xs []string, target string) int {
	for i, x := range xs {
		if x == target {
			return i
		}
	}
	return -1
}
