package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Grady-Saccullo/wezsesh/internal/safefs"
)

// noRepo is the repoHas callback used by tests that expect every name to
// be live-only (no snapshot exists for any name).
func noRepo(string) bool { return false }

// makeRepo returns a callback that reports true for any name in the
// supplied set; used to drive the "snapshot exists → drop from live_pins"
// migration branch.
func makeRepo(names ...string) func(string) bool {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return func(name string) bool {
		_, ok := set[name]
		return ok
	}
}

// readJSONFile is a test helper: read the persisted state.json and
// unmarshal it. Bypasses Open's symlink/ migration policy because the
// tests are inspecting raw post-write contents.
func readJSONFile(t *testing.T, path string) State {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return st
}

// TestOpen_FreshNoFile — missing state.json yields an empty Store with
// no .bak file, no error.
func TestOpen_FreshNoFile(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, nil, noRepo)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := s.LivePins(); got != nil {
		t.Errorf("LivePins on fresh: want nil, got %v", got)
	}
	if got := s.SwitchCount("anything"); got != 0 {
		t.Errorf("SwitchCount on fresh: want 0, got %d", got)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("unexpected file in fresh state dir: %s", e.Name())
	}
}

// TestOpen_V1Existing — v=1 file already in the new shape loads as-is.
func TestOpen_V1Existing(t *testing.T) {
	dir := t.TempDir()
	in := State{
		Version: 1,
		Usage: map[string]Usage{
			"alpha": {LastSwitched: 1700000000, SwitchCount: 3},
		},
		LivePins: []string{"alpha", "beta"},
	}
	raw, _ := json.Marshal(in)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(context.Background(), dir, nil, noRepo)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !s.IsLivePinned("alpha") || !s.IsLivePinned("beta") {
		t.Errorf("missing pins after load: %v", s.LivePins())
	}
	if got := s.SwitchCount("alpha"); got != 3 {
		t.Errorf("SwitchCount(alpha): want 3, got %d", got)
	}
	if got := s.LastSwitched("alpha"); got != 1700000000 {
		t.Errorf("LastSwitched(alpha): want 1700000000, got %d", got)
	}

	if _, err := os.Stat(filepath.Join(dir, "state.json.v1.bak")); !os.IsNotExist(err) {
		t.Errorf("expected no .bak for v=1; stat err=%v", err)
	}
}

// TestOpen_V1MigratesPinsToLivePins — Acceptance gate "Schema migration
// state.json v=1 → live_pins". v=1 with old `pins` key → migrated to
// `live_pins`; entries with corresponding snapshot are dropped (sidecar
// is authoritative for saved workspaces, §13.11). The migrated shape
// MUST be persisted to disk: the legacy `pins` key is gone, `live_pins`
// is present.
func TestOpen_V1MigratesPinsToLivePins(t *testing.T) {
	dir := t.TempDir()
	legacy := map[string]any{
		"version": 1,
		"usage": map[string]any{
			"keep": map[string]any{"last_switched": 1700000001, "switch_count": 5},
		},
		"pins": []string{"keep", "saved-workspace", "another-live"},
	}
	raw, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(context.Background(), dir, nil, makeRepo("saved-workspace"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	pins := s.LivePins()
	if !contains(pins, "keep") {
		t.Errorf("expected 'keep' in live_pins; got %v", pins)
	}
	if !contains(pins, "another-live") {
		t.Errorf("expected 'another-live' in live_pins; got %v", pins)
	}
	if contains(pins, "saved-workspace") {
		t.Errorf("expected 'saved-workspace' to be dropped (sidecar wins); got %v", pins)
	}
	if got := s.SwitchCount("keep"); got != 5 {
		t.Errorf("usage was not preserved across migration: SwitchCount(keep)=%d", got)
	}

	// The migration must be persisted to disk, not just held in memory:
	// the obsolete `pins` key must be gone and `live_pins` must be
	// present (§10.4).
	persistedRaw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("re-read state.json: %v", err)
	}
	if bytes.Contains(persistedRaw, []byte(`"pins":`)) {
		t.Errorf("migrated state.json still contains legacy `pins` key: %s", persistedRaw)
	}
	if !bytes.Contains(persistedRaw, []byte(`"live_pins":`)) {
		t.Errorf("migrated state.json missing `live_pins` key: %s", persistedRaw)
	}
	for _, want := range []string{`"keep"`, `"another-live"`} {
		if !bytes.Contains(persistedRaw, []byte(want)) {
			t.Errorf("migrated state.json missing %s: %s", want, persistedRaw)
		}
	}
}

// TestOpen_V1MigrationKeepsAllWhenNoSnapshots verifies the no-snapshot
// branch of the live_pins prune.
func TestOpen_V1MigrationKeepsAllWhenNoSnapshots(t *testing.T) {
	dir := t.TempDir()
	legacy := map[string]any{
		"version": 1,
		"pins":    []string{"a", "b", "c"},
	}
	raw, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(context.Background(), dir, nil, noRepo)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	pins := s.LivePins()
	for _, want := range []string{"a", "b", "c"} {
		if !contains(pins, want) {
			t.Errorf("missing %q in live_pins: %v", want, pins)
		}
	}
}

// TestOpen_V2BackupAndReinit — Acceptance gate "Schema migration
// state.json v>1". v=2 file → backed up to .v2.bak + reinitialised; no
// error. The .bak content equals the original raw bytes.
func TestOpen_V2BackupAndReinit(t *testing.T) {
	dir := t.TempDir()
	future := map[string]any{
		"version":   2,
		"future":    "garbage",
		"live_pins": []string{"x"},
	}
	raw, _ := json.Marshal(future)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(context.Background(), dir, nil, noRepo)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	bakPath := filepath.Join(dir, "state.json.v2.bak")
	bak, err := os.ReadFile(bakPath)
	if err != nil {
		t.Fatalf("read .v2.bak: %v", err)
	}
	if string(bak) != string(raw) {
		t.Errorf(".v2.bak content mismatch:\n  want: %s\n   got: %s", raw, bak)
	}

	// Mode 0600 (§10.4): the .bak file path also flows through
	// safefs.AtomicWriteFile and MUST honour the mode argument.
	bakFi, err := os.Stat(bakPath)
	if err != nil {
		t.Fatalf("stat .v2.bak: %v", err)
	}
	if perm := bakFi.Mode().Perm(); perm != 0o600 {
		t.Errorf(".v2.bak mode: want 0600, got %#o", perm)
	}

	if pins := s.LivePins(); pins != nil {
		t.Errorf("post-reinit LivePins: want nil, got %v", pins)
	}

	persisted := readJSONFile(t, filepath.Join(dir, "state.json"))
	if persisted.Version != 1 {
		t.Errorf("post-reinit version: want 1, got %d", persisted.Version)
	}
}

// TestOpen_RejectsSymlinkedDir — Open enforces SymlinkRefuse on the state
// directory itself. A symlinked stateDir aborts.
func TestOpen_RejectsSymlinkedDir(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	sym := filepath.Join(tmp, "sym")
	if err := os.Symlink(real, sym); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), sym, nil, noRepo)
	if !errors.Is(err, safefs.ErrIsSymlink) {
		t.Errorf("Open(symlinked dir): want ErrIsSymlink, got %v", err)
	}
}

// TestOpen_RejectsSymlinkedStateFile — state.json itself being a symlink
// must be rejected by SafeOpenForRead's O_NOFOLLOW.
func TestOpen_RejectsSymlinkedStateFile(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tmp, "target.json")
	if err := os.WriteFile(target, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "state.json")); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), dir, nil, noRepo)
	if !errors.Is(err, safefs.ErrIsSymlink) {
		t.Errorf("Open(symlinked state.json): want ErrIsSymlink, got %v", err)
	}
}

// TestRecordSwitch_PersistsAndIncrements — RecordSwitch increments,
// updates last_switched, and persists to disk.
func TestRecordSwitch_PersistsAndIncrements(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, nil, noRepo)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSwitch(context.Background(), "ws"); err != nil {
		t.Fatalf("RecordSwitch: %v", err)
	}
	if err := s.RecordSwitch(context.Background(), "ws"); err != nil {
		t.Fatalf("RecordSwitch 2: %v", err)
	}
	if got := s.SwitchCount("ws"); got != 2 {
		t.Errorf("SwitchCount: want 2, got %d", got)
	}
	if s.LastSwitched("ws") == 0 {
		t.Errorf("LastSwitched not updated")
	}

	persisted := readJSONFile(t, filepath.Join(dir, "state.json"))
	if persisted.Usage["ws"].SwitchCount != 2 {
		t.Errorf("on-disk SwitchCount: want 2, got %d", persisted.Usage["ws"].SwitchCount)
	}
	if persisted.Usage["ws"].LastSwitched == 0 {
		t.Errorf("on-disk LastSwitched not persisted")
	}

	// Mode 0600 (§10.4): safefs.AtomicWriteFile must honour the mode
	// argument on the post-rename file.
	fi, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("stat state.json: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("state.json mode: want 0600, got %#o", perm)
	}
}

// TestSetLivePin_RoundTrip — SetLivePin(true) survives close+reopen,
// SetLivePin(false) removes.
func TestSetLivePin_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, nil, noRepo)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetLivePin(context.Background(), "alpha", true); err != nil {
		t.Fatalf("SetLivePin true: %v", err)
	}
	if !s.IsLivePinned("alpha") {
		t.Errorf("IsLivePinned after set true: want true")
	}

	s2, err := Open(context.Background(), dir, nil, noRepo)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !s2.IsLivePinned("alpha") {
		t.Errorf("IsLivePinned after reopen: want true")
	}

	if err := s2.SetLivePin(context.Background(), "alpha", false); err != nil {
		t.Fatalf("SetLivePin false: %v", err)
	}
	if s2.IsLivePinned("alpha") {
		t.Errorf("IsLivePinned after set false: want false")
	}

	persisted := readJSONFile(t, filepath.Join(dir, "state.json"))
	if len(persisted.LivePins) != 0 {
		t.Errorf("on-disk live_pins after unset: want empty, got %v", persisted.LivePins)
	}

	// §10.4: an empty live_pins set marshals as `[]`, NOT `null`.
	persistedRaw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("re-read state.json: %v", err)
	}
	if !bytes.Contains(persistedRaw, []byte(`"live_pins":[]`)) {
		t.Errorf("on-disk live_pins shape: want `\"live_pins\":[]`, raw=%s", persistedRaw)
	}
	if bytes.Contains(persistedRaw, []byte(`"live_pins":null`)) {
		t.Errorf("on-disk live_pins is null (must be `[]`): %s", persistedRaw)
	}
}

// TestSetLivePin_Idempotent — Re-setting the same value is a no-op (does
// not duplicate the entry).
func TestSetLivePin_Idempotent(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, nil, noRepo)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.SetLivePin(context.Background(), "n", true); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	pins := s.LivePins()
	if len(pins) != 1 || pins[0] != "n" {
		t.Errorf("idempotent SetLivePin produced %v", pins)
	}
}

// TestRecordSwitch_CtxCanceled — ctx cancellation surfaces via ctx.Err.
func TestRecordSwitch_CtxCanceled(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, nil, noRepo)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.RecordSwitch(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Errorf("RecordSwitch(canceled): want context.Canceled, got %v", err)
	}
}

// TestSetLivePin_CtxCanceled — same for SetLivePin.
func TestSetLivePin_CtxCanceled(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, nil, noRepo)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.SetLivePin(ctx, "x", true); !errors.Is(err, context.Canceled) {
		t.Errorf("SetLivePin(canceled): want context.Canceled, got %v", err)
	}
}

// TestLivePins_ReturnsSortedCopy — caller may mutate the returned slice
// without affecting subsequent reads.
func TestLivePins_ReturnsSortedCopy(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir, nil, noRepo)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"c", "a", "b"} {
		if err := s.SetLivePin(context.Background(), n, true); err != nil {
			t.Fatal(err)
		}
	}
	got := s.LivePins()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("LivePins: want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("LivePins[%d]: want %s, got %s", i, want[i], got[i])
		}
	}
	got[0] = "MUTATED"
	again := s.LivePins()
	if again[0] != "a" {
		t.Errorf("LivePins did not return a defensive copy: %v", again)
	}
}

// TestOpen_EmptyStateDir rejects empty stateDir argument.
func TestOpen_EmptyStateDir(t *testing.T) {
	_, err := Open(context.Background(), "", nil, noRepo)
	if err == nil {
		t.Errorf("Open(\"\"): want error, got nil")
	}
}

// TestOpen_CtxCanceled — ctx cancellation at Open time surfaces.
func TestOpen_CtxCanceled(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Open(ctx, dir, nil, noRepo)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Open(canceled): want context.Canceled, got %v", err)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
