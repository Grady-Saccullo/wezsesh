package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Grady-Saccullo/wezsesh/internal/config"
)

// resetTestEnv builds a self-contained config tree under t.TempDir().
// Every dir wezsesh-managed is a subdir of dir so each test starts
// from a clean slate. WEZSESH_*_DIR env overrides point AutoDetect +
// applyEnvOverrides at the scratch tree (the env-blob transport is
// gone post-bootstrap-cutover).
type resetTestEnv struct {
	dir         string
	stateDir    string
	dataDir     string
	trustDir    string
	runtimeDir  string
	reqDir      string
	snapshotDir string
	workspace   string
}

func newResetTestEnv(t *testing.T) *resetTestEnv {
	t.Helper()
	dir := t.TempDir()
	env := &resetTestEnv{
		dir:         dir,
		stateDir:    filepath.Join(dir, "state"),
		dataDir:     filepath.Join(dir, "data"),
		trustDir:    filepath.Join(dir, "data", "allow"),
		runtimeDir:  filepath.Join(dir, "rt"),
		reqDir:      filepath.Join(dir, "rt", "req"),
		snapshotDir: filepath.Join(dir, "snap"),
		workspace:   filepath.Join(dir, "snap", "workspace"),
	}
	for _, d := range []string{env.stateDir, env.trustDir, env.runtimeDir, env.reqDir, env.workspace} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	t.Setenv("WEZSESH_SNAPSHOT_DIR", env.snapshotDir)
	t.Setenv("WEZSESH_STATE_DIR", env.stateDir)
	t.Setenv("WEZSESH_RUNTIME_DIR", env.runtimeDir)
	t.Setenv("WEZSESH_DATA_DIR", env.dataDir)
	// WEZTERM_PANE / HMAC key not consulted by reset; keep them empty.
	t.Setenv("WEZTERM_PANE", "")
	return env
}

// seed populates a representative file in every reset category so the
// gates ("does it remove X", "does it skip X") have something to check.
func (e *resetTestEnv) seed(t *testing.T) {
	t.Helper()
	files := map[string][]byte{
		filepath.Join(e.stateDir, "state.json"):                  []byte(`{"version":1}`),
		filepath.Join(e.stateDir, "wezsesh.log"):                 []byte(`{"msg":"x"}` + "\n"),
		filepath.Join(e.stateDir, "wezsesh.1.log"):               []byte(`{"msg":"old"}` + "\n"),
		filepath.Join(e.stateDir, "plugin.log"):                  []byte(`{"msg":"p"}` + "\n"),
		filepath.Join(e.stateDir, "plugin.1.log"):                []byte(`{"msg":"po"}` + "\n"),
		filepath.Join(e.trustDir, "deadbeef0000000000000000000000000000000000000000000000000000aaaa"): []byte(`{"path":"/tmp/x"}`),
		filepath.Join(e.runtimeDir, "abcd1234.sock"):             []byte{},
		filepath.Join(e.reqDir, "01234567.json"):                 []byte(`{"v":1}`),
		filepath.Join(e.workspace, "ws.wezsesh.json"):            []byte(`{"version":1}`),
		filepath.Join(e.workspace, "ws.json"):                    []byte(`{"resurrect":true}`),
	}
	for p, body := range files {
		if err := os.WriteFile(p, body, 0o600); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
}

// loadResetCfg surfaces the resolved config the tests cross-check against.
func (e *resetTestEnv) loadResetCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.LoadFromEnv(t.Context())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// -----------------------------------------------------------------------------
// Gate: --dry-run previews everything without writes.
// -----------------------------------------------------------------------------

func TestSubcmdReset_DryRun_NoWrites(t *testing.T) {
	env := newResetTestEnv(t)
	env.seed(t)

	var stdout, stderr bytes.Buffer
	rc := subcmdReset("reset", []string{"--dry-run"}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"state.json", "wezsesh.log", "abcd1234.sock", "01234567.json", "ws.wezsesh.json"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run preview missing %q: %s", want, out)
		}
	}
	// Without --include-snapshots, the resurrect snapshot file MUST NOT
	// appear in the preview.
	if strings.Contains(out, "/ws.json") {
		t.Fatalf("dry-run preview leaked snapshot file: %s", out)
	}

	// Files must still exist on disk.
	for _, p := range []string{
		filepath.Join(env.stateDir, "state.json"),
		filepath.Join(env.stateDir, "wezsesh.log"),
		filepath.Join(env.runtimeDir, "abcd1234.sock"),
		filepath.Join(env.reqDir, "01234567.json"),
		filepath.Join(env.workspace, "ws.wezsesh.json"),
		filepath.Join(env.workspace, "ws.json"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("dry-run touched %s: %v", p, err)
		}
	}
}

// -----------------------------------------------------------------------------
// Gate: default `wezsesh reset` (no flags) is preview-only.
// -----------------------------------------------------------------------------

func TestSubcmdReset_NoFlags_PreviewOnly(t *testing.T) {
	env := newResetTestEnv(t)
	env.seed(t)

	var stdout, stderr bytes.Buffer
	rc := subcmdReset("reset", nil, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "preview-only") {
		t.Fatalf("stdout missing preview-only banner: %q", stdout.String())
	}
	// Files must still exist on disk.
	if _, err := os.Stat(filepath.Join(env.stateDir, "state.json")); err != nil {
		t.Errorf("default invocation removed state.json: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Gate: --yes performs deletions (without --include-snapshots).
// -----------------------------------------------------------------------------

func TestSubcmdReset_Yes_RemovesEverythingExceptSnapshots(t *testing.T) {
	env := newResetTestEnv(t)
	env.seed(t)

	var stdout, stderr bytes.Buffer
	rc := subcmdReset("reset", []string{"--yes"}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	// The --yes pass MUST remove these.
	gone := []string{
		filepath.Join(env.stateDir, "state.json"),
		filepath.Join(env.stateDir, "wezsesh.log"),
		filepath.Join(env.stateDir, "wezsesh.1.log"),
		filepath.Join(env.stateDir, "plugin.log"),
		filepath.Join(env.stateDir, "plugin.1.log"),
		filepath.Join(env.trustDir, "deadbeef0000000000000000000000000000000000000000000000000000aaaa"),
		filepath.Join(env.runtimeDir, "abcd1234.sock"),
		filepath.Join(env.reqDir, "01234567.json"),
		filepath.Join(env.workspace, "ws.wezsesh.json"),
	}
	for _, p := range gone {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("path %s still exists (err=%v); --yes should have removed it", p, err)
		}
	}
	// The resurrect snapshot file MUST survive --yes alone.
	if _, err := os.Stat(filepath.Join(env.workspace, "ws.json")); err != nil {
		t.Errorf("resurrect snapshot ws.json removed without --include-snapshots: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Gate: `wezsesh reset --include-snapshots` confirmation gate enforced;
// only on `--yes` does it remove resurrect files.
// -----------------------------------------------------------------------------

// Without --yes (just --include-snapshots), it's still a preview — does
// NOT touch resurrect snapshots.
func TestSubcmdReset_IncludeSnapshots_PreviewWithoutYes(t *testing.T) {
	env := newResetTestEnv(t)
	env.seed(t)

	var stdout, stderr bytes.Buffer
	rc := subcmdReset("reset", []string{"--include-snapshots"}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(env.workspace, "ws.json")); err != nil {
		t.Errorf("ws.json removed during preview-only --include-snapshots: %v", err)
	}
	// Preview output should still mention the snapshot file.
	if !strings.Contains(stdout.String(), "ws.json") {
		t.Fatalf("preview missing ws.json: %s", stdout.String())
	}
}

// With --yes --include-snapshots on a non-TTY without --yes-i-really-
// mean-it, the gate refuses (rc=2, snapshot file remains).
func TestSubcmdReset_IncludeSnapshots_NonTTYRefuses(t *testing.T) {
	env := newResetTestEnv(t)
	env.seed(t)

	prev := resetTTYProbe
	t.Cleanup(func() { resetTTYProbe = prev })
	resetTTYProbe = func() bool { return false }

	var stdout, stderr bytes.Buffer
	rc := subcmdReset("reset", []string{"--yes", "--include-snapshots"}, &stdout, &stderr)
	if rc == exitOK {
		t.Fatalf("rc = %d, want non-zero (stderr=%q)", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "TTY") {
		t.Fatalf("stderr missing TTY hint: %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(env.workspace, "ws.json")); err != nil {
		t.Errorf("ws.json removed despite refused confirmation: %v", err)
	}
}

// With --yes --include-snapshots --yes-i-really-mean-it (non-TTY bypass),
// the snapshot file IS removed.
func TestSubcmdReset_IncludeSnapshots_NonTTYBypass(t *testing.T) {
	env := newResetTestEnv(t)
	env.seed(t)

	prev := resetTTYProbe
	t.Cleanup(func() { resetTTYProbe = prev })
	resetTTYProbe = func() bool { return false }

	var stdout, stderr bytes.Buffer
	rc := subcmdReset("reset",
		[]string{"--yes", "--include-snapshots", "--yes-i-really-mean-it"},
		&stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(env.workspace, "ws.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ws.json still exists after --include-snapshots --yes: %v", err)
	}
}

// With --yes --include-snapshots on a TTY, typing "yes" proceeds.
func TestSubcmdReset_IncludeSnapshots_TTYConfirmYes(t *testing.T) {
	env := newResetTestEnv(t)
	env.seed(t)

	prevTTY := resetTTYProbe
	prevReader := resetConfirmReader
	t.Cleanup(func() {
		resetTTYProbe = prevTTY
		resetConfirmReader = prevReader
	})
	resetTTYProbe = func() bool { return true }
	resetConfirmReader = strings.NewReader("yes\n")

	var stdout, stderr bytes.Buffer
	rc := subcmdReset("reset",
		[]string{"--yes", "--include-snapshots"}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(env.workspace, "ws.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ws.json should be gone: %v", err)
	}
}

// With --yes --include-snapshots on a TTY, typing anything other than
// "yes" refuses.
func TestSubcmdReset_IncludeSnapshots_TTYConfirmNo(t *testing.T) {
	env := newResetTestEnv(t)
	env.seed(t)

	prevTTY := resetTTYProbe
	prevReader := resetConfirmReader
	t.Cleanup(func() {
		resetTTYProbe = prevTTY
		resetConfirmReader = prevReader
	})
	resetTTYProbe = func() bool { return true }
	resetConfirmReader = strings.NewReader("n\n")

	var stdout, stderr bytes.Buffer
	rc := subcmdReset("reset",
		[]string{"--yes", "--include-snapshots"}, &stdout, &stderr)
	if rc == exitOK {
		t.Fatalf("rc = %d, want non-zero (stderr=%q)", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "declined") {
		t.Fatalf("stderr missing 'declined': %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(env.workspace, "ws.json")); err != nil {
		t.Errorf("ws.json should still exist: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Gate: `wezsesh reset` symlink defense — pre-placed symlink at state
// dir → ABORT.
// -----------------------------------------------------------------------------

func TestSubcmdReset_Symlink_StateDirAborts(t *testing.T) {
	env := newResetTestEnv(t)
	env.seed(t)

	// Replace the state dir with a symlink pointing to a sibling. Reset
	// must refuse before any disk mutation.
	target := filepath.Join(env.dir, "state-target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.RemoveAll(env.stateDir); err != nil {
		t.Fatalf("rm stateDir: %v", err)
	}
	if err := os.Symlink(target, env.stateDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Drop a sentinel inside the target so we can prove it survived.
	sentinel := filepath.Join(target, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	var stdout, stderr bytes.Buffer
	rc := subcmdReset("reset", []string{"--yes"}, &stdout, &stderr)
	if rc == exitOK {
		t.Fatalf("rc = %d, want non-zero on symlink at state dir (stderr=%q)", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "symlink") {
		t.Fatalf("stderr missing symlink defense marker: %q", stderr.String())
	}
	// The symlink target must NOT have been touched.
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel was removed (symlink defense breached): %v", err)
	}
	// Other (non-state) seed files must also still be present — the run
	// aborted BEFORE any unlink.
	if _, err := os.Stat(filepath.Join(env.runtimeDir, "abcd1234.sock")); err != nil {
		t.Fatalf("symlink-defense run still removed sock file: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Gate: `wezsesh reset` symlink defense — pre-placed symlink at
// <state>/state.json → SKIP+WARN.
// -----------------------------------------------------------------------------

func TestSubcmdReset_Symlink_StateFileSkipWarns(t *testing.T) {
	env := newResetTestEnv(t)
	env.seed(t)

	// Replace state.json with a symlink. The file-level defense is
	// SkipWarn, so the rest of the reset proceeds; the symlink target
	// is left in place.
	target := filepath.Join(env.dir, "state-victim.json")
	if err := os.WriteFile(target, []byte("victim"), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	stateFile := filepath.Join(env.stateDir, "state.json")
	if err := os.Remove(stateFile); err != nil {
		t.Fatalf("remove stateFile: %v", err)
	}
	if err := os.Symlink(target, stateFile); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	var stdout, stderr bytes.Buffer
	rc := subcmdReset("reset", []string{"--yes"}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	// The symlink target must survive — SkipWarn does NOT follow the
	// link.
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("victim file was removed (SkipWarn breached): %v", err)
	}
	// SkipWarn surfaces on stderr (logger is nil on this path).
	if !strings.Contains(stderr.String(), "skipping symlink") {
		t.Fatalf("stderr missing SkipWarn marker: %q", stderr.String())
	}
	// The symlink itself remains in place (we did not unlink it; doing
	// so would be a regression — Enforce returned ok=false and we
	// short-circuited).
	if _, err := os.Lstat(stateFile); err != nil {
		t.Fatalf("state.json symlink itself was removed: %v", err)
	}
	// But other reset categories DID run: the sock file should be gone.
	if _, err := os.Stat(filepath.Join(env.runtimeDir, "abcd1234.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sock file should have been removed despite SkipWarn on state.json: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Gate: `wezsesh nuke` deprecation alias — invoking `nuke` runs `reset`
// and prints deprecation toast.
// -----------------------------------------------------------------------------

func TestSubcmdReset_NukeAliasPrintsToast(t *testing.T) {
	env := newResetTestEnv(t)
	env.seed(t)

	var stdout, stderr bytes.Buffer
	rc := subcmdReset("nuke", []string{"--dry-run"}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	// §13.4 prose verbatim ("nuke renamed to reset; this alias removed
	// in v0.2") — assert the byte-match so a future spec edit drift is
	// caught.
	if !strings.Contains(stderr.String(), "renamed") {
		t.Fatalf("stderr missing 'renamed' toast: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "v0.2") {
		t.Fatalf("stderr toast missing v0.2 cutoff: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "reset") {
		t.Fatalf("stderr toast does not redirect to 'reset': %q", stderr.String())
	}
	// Reset is the same code path: dry-run still produces the preview
	// header on stdout.
	if !strings.Contains(stdout.String(), "previewing planned removals") {
		t.Fatalf("nuke alias did not run reset preview: %s", stdout.String())
	}
	// And it must NOT have written anything.
	if _, err := os.Stat(filepath.Join(env.stateDir, "state.json")); err != nil {
		t.Errorf("nuke --dry-run touched disk: %v", err)
	}
}

// TestSubcmdReset_NukeAlias_Run also checks the route through main.run
// to confirm the alias dispatches to subcmdReset (not to a stub or to
// the unknown-subcommand path).
func TestSubcmdReset_NukeAlias_Run(t *testing.T) {
	_ = newResetTestEnv(t)
	var stdout, stderr bytes.Buffer
	rc := run([]string{"nuke", "--dry-run"}, &stdout, &stderr, testBinarySessionID)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	if !strings.Contains(stderr.String(), "renamed") {
		t.Fatalf("run('nuke') stderr missing 'renamed' toast: %q", stderr.String())
	}
}

// -----------------------------------------------------------------------------
// Gate: §13.4 symlink defense — pre-placed symlink at data dir → ABORT.
// data_dir is one of the four Refuse-class anchors (snapshot / state /
// data / runtime). A symlinked data_dir would let the trust-store walk
// (which lives at <data>/allow/) descend into a stranger's tree.
// -----------------------------------------------------------------------------

func TestSubcmdReset_Symlink_DataDirAborts(t *testing.T) {
	env := newResetTestEnv(t)
	env.seed(t)

	// Replace the data dir with a symlink. The trust dir lives inside
	// it; we must abort BEFORE traversing the symlink to look up trust
	// files.
	target := filepath.Join(env.dir, "data-target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	// Move the existing data tree out of the way so we can plant the
	// symlink at env.dataDir.
	if err := os.RemoveAll(env.dataDir); err != nil {
		t.Fatalf("rm dataDir: %v", err)
	}
	if err := os.Symlink(target, env.dataDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Drop a sentinel inside the target so we can prove the symlink was
	// not followed.
	sentinel := filepath.Join(target, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	var stdout, stderr bytes.Buffer
	rc := subcmdReset("reset", []string{"--yes"}, &stdout, &stderr)
	if rc == exitOK {
		t.Fatalf("rc = %d, want non-zero on symlink at data dir (stderr=%q)", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "symlink") {
		t.Fatalf("stderr missing symlink defense marker: %q", stderr.String())
	}
	// The symlink target must NOT have been touched.
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel was removed (symlink defense breached on data dir): %v", err)
	}
	// Other (non-data) seed files must still be present — the run
	// aborted BEFORE any unlink. The sock file lives under runtimeDir
	// and must survive even though the abort came from the data-dir
	// Enforce.
	if _, err := os.Stat(filepath.Join(env.runtimeDir, "abcd1234.sock")); err != nil {
		t.Fatalf("symlink-defense run still removed sock file: %v", err)
	}
	// The data symlink itself is left in place.
	if fi, err := os.Lstat(env.dataDir); err != nil {
		t.Fatalf("data dir symlink itself was removed: %v", err)
	} else if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("data dir was no longer a symlink after abort: mode=%s", fi.Mode())
	}
}

// -----------------------------------------------------------------------------
// Gate: §13.4 "trust store … + `wezsesh/` parent if empty" — empty
// data_dir is rmdir'd alongside state_dir on `--yes`. Trust files live
// under <data>/allow/, so once the trust pass removes them, allow/
// rmdirs, leaving <data>/ empty for its own rmdir.
// -----------------------------------------------------------------------------

func TestSubcmdReset_Yes_RmdirsEmptyDataDir(t *testing.T) {
	env := newResetTestEnv(t)
	env.seed(t)

	var stdout, stderr bytes.Buffer
	rc := subcmdReset("reset", []string{"--yes"}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	// trustDir = <data>/allow/ — must rmdir once its file is gone.
	if _, err := os.Stat(env.trustDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("trustDir still exists (err=%v); --yes should rmdir it", err)
	}
	// dataDir must rmdir AFTER trustDir (since trustDir lives inside
	// it). Both should be gone after a clean --yes run.
	if _, err := os.Stat(env.dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dataDir still exists (err=%v); --yes should rmdir it once empty", err)
	}
	// Parallel: stateDir should also be gone — anchors the §13.4
	// "state dir … + dir itself if empty post-cleanup" prose.
	if _, err := os.Stat(env.stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stateDir still exists (err=%v); --yes should rmdir it once empty", err)
	}
}

// TestSubcmdReset_Yes_LeavesDataDirIfNotEmpty asserts the inverse: a
// stranger file in <data>/ keeps the dir intact. resetRmdir's
// ENOTEMPTY tolerance is the canonical signal here.
func TestSubcmdReset_Yes_LeavesDataDirIfNotEmpty(t *testing.T) {
	env := newResetTestEnv(t)
	env.seed(t)

	stranger := filepath.Join(env.dataDir, "stranger.txt")
	if err := os.WriteFile(stranger, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("write stranger: %v", err)
	}

	var stdout, stderr bytes.Buffer
	rc := subcmdReset("reset", []string{"--yes"}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	// The stranger file is not part of the plan, so it must survive
	// AND keep dataDir alive (ENOTEMPTY tolerance).
	if _, err := os.Stat(stranger); err != nil {
		t.Errorf("stranger file removed (out-of-plan): %v", err)
	}
	if _, err := os.Stat(env.dataDir); err != nil {
		t.Errorf("dataDir was removed despite non-empty contents: %v", err)
	}
}

// -----------------------------------------------------------------------------
// LOW (regression): plan.files must be processed BEFORE plan.dirs, else
// the rmdir step would see a non-empty dir, hit ENOTEMPTY tolerance,
// and silently leave the dir behind. This test would fail if the order
// inverted in applyResetPlan.
// -----------------------------------------------------------------------------

func TestApplyResetPlan_FilesProcessedBeforeDirs(t *testing.T) {
	env := newResetTestEnv(t)
	env.seed(t)
	// Pre-place an additional log file inside stateDir so that the
	// state-dir rmdir absolutely depends on the file being removed
	// first. (The base seed already puts wezsesh.log + state.json there;
	// we add one more for belt-and-braces.)
	extra := filepath.Join(env.stateDir, "wezsesh.99.log")
	if err := os.WriteFile(extra, []byte("noise"), 0o600); err != nil {
		t.Fatalf("seed extra log: %v", err)
	}

	var stdout, stderr bytes.Buffer
	rc := subcmdReset("reset", []string{"--yes"}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	// File must be gone.
	if _, err := os.Stat(extra); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("log file still exists: %v", err)
	}
	// AND stateDir must be gone — only possible if the log file (and
	// every other state-dir resident) was unlinked BEFORE the
	// state-dir rmdir. If plan.dirs ran first, rmdir would have seen a
	// non-empty dir, ENOTEMPTY-tolerated, and left it in place.
	if _, err := os.Stat(env.stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stateDir still exists (err=%v); files must precede dirs in applyResetPlan", err)
	}
}

// TestBuildResetPlan_FilesPrecedeDirs is the structural variant: the
// resetPlan returned by buildResetPlan must list plan.files before
// plan.dirs (i.e., applyResetPlan's loop ordering matches the plan
// invariant). This catches a refactor that interleaves files+dirs into
// a single slice without preserving the file-first contract.
func TestBuildResetPlan_FilesPrecedeDirs(t *testing.T) {
	env := newResetTestEnv(t)
	env.seed(t)
	cfg := env.loadResetCfg(t)
	plan, err := buildResetPlan(cfg, true /*includeSnapshots*/, nil)
	if err != nil {
		t.Fatalf("buildResetPlan: %v", err)
	}
	if len(plan.files) == 0 {
		t.Fatalf("plan.files is empty; cannot test ordering")
	}
	if len(plan.dirs) == 0 {
		t.Fatalf("plan.dirs is empty; cannot test ordering")
	}
	// Categories used in plan.files MUST be disjoint from those used in
	// plan.dirs — applyResetPlan iterates files then dirs and assumes
	// no category overlap.
	fileCats := map[resetCategory]struct{}{}
	for _, e := range plan.files {
		fileCats[e.category] = struct{}{}
	}
	for _, e := range plan.dirs {
		if _, overlap := fileCats[e.category]; overlap {
			t.Errorf("category %q appears in both plan.files and plan.dirs", e.category)
		}
	}
}

// -----------------------------------------------------------------------------
// Plan-builder unit tests.
// -----------------------------------------------------------------------------

// TestBuildResetPlan_FiltersByExtensionsAndSuffixes: sock files, req
// files, sidecars, snapshots, log files all land in the right buckets;
// non-matching files are ignored.
func TestBuildResetPlan_FiltersByExtensionsAndSuffixes(t *testing.T) {
	env := newResetTestEnv(t)
	env.seed(t)
	// Add some non-matching files that must NOT appear in the plan.
	noisePaths := []string{
		filepath.Join(env.runtimeDir, "stranger.txt"),
		filepath.Join(env.reqDir, "stranger.txt"),
		filepath.Join(env.workspace, "stranger.txt"),
	}
	for _, p := range noisePaths {
		if err := os.WriteFile(p, []byte("noise"), 0o600); err != nil {
			t.Fatalf("seed noise %s: %v", p, err)
		}
	}

	cfg := env.loadResetCfg(t)
	plan, err := buildResetPlan(cfg, true /*includeSnapshots*/, nil)
	if err != nil {
		t.Fatalf("buildResetPlan: %v", err)
	}
	got := map[string]resetCategory{}
	for _, e := range plan.files {
		got[e.path] = e.category
	}
	want := map[string]resetCategory{
		filepath.Join(env.stateDir, "state.json"):      resetCatStateFile,
		filepath.Join(env.stateDir, "wezsesh.log"):     resetCatLogFile,
		filepath.Join(env.stateDir, "wezsesh.1.log"):   resetCatLogFile,
		filepath.Join(env.stateDir, "plugin.log"):      resetCatLogFile,
		filepath.Join(env.stateDir, "plugin.1.log"):    resetCatLogFile,
		filepath.Join(env.trustDir, "deadbeef0000000000000000000000000000000000000000000000000000aaaa"): resetCatTrustFile,
		filepath.Join(env.runtimeDir, "abcd1234.sock"): resetCatSockFile,
		filepath.Join(env.reqDir, "01234567.json"):     resetCatReqFile,
		filepath.Join(env.workspace, "ws.wezsesh.json"): resetCatSidecar,
		filepath.Join(env.workspace, "ws.json"):         resetCatSnapshot,
	}
	for p, cat := range want {
		if got[p] != cat {
			t.Errorf("plan[%s] = %q, want %q", p, got[p], cat)
		}
	}
	// Noise must NOT appear.
	for _, p := range noisePaths {
		if _, present := got[p]; present {
			t.Errorf("noise leaked into plan: %s", p)
		}
	}
}

// TestBuildResetPlan_IncludeSnapshotsFalseExcludesSnapshots: snapshot
// files appear ONLY when includeSnapshots=true; sidecars always appear.
func TestBuildResetPlan_IncludeSnapshotsFalseExcludesSnapshots(t *testing.T) {
	env := newResetTestEnv(t)
	env.seed(t)
	cfg := env.loadResetCfg(t)
	plan, err := buildResetPlan(cfg, false, nil)
	if err != nil {
		t.Fatalf("buildResetPlan: %v", err)
	}
	for _, e := range plan.files {
		if e.category == resetCatSnapshot {
			t.Errorf("snapshot leaked into plan with includeSnapshots=false: %s", e.path)
		}
	}
	// Sidecar should still be present.
	hasSidecar := false
	for _, e := range plan.files {
		if e.category == resetCatSidecar {
			hasSidecar = true
		}
	}
	if !hasSidecar {
		t.Fatal("sidecar missing from plan")
	}
}

// TestParseResetFlags_RejectsUnexpectedArgs: positional arg → error.
func TestParseResetFlags_RejectsUnexpectedArgs(t *testing.T) {
	if _, err := parseResetFlags([]string{"--yes", "extra"}); err == nil {
		t.Fatalf("expected error on positional arg")
	}
}

// TestParseResetFlags_ValidCombos: --yes --include-snapshots is valid.
func TestParseResetFlags_ValidCombos(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"empty", nil},
		{"dry-run", []string{"--dry-run"}},
		{"yes", []string{"--yes"}},
		{"yes-include", []string{"--yes", "--include-snapshots"}},
		{"yes-include-bypass", []string{"--yes", "--include-snapshots", "--yes-i-really-mean-it"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseResetFlags(tc.args); err != nil {
				t.Fatalf("parseResetFlags(%v): %v", tc.args, err)
			}
		})
	}
}
