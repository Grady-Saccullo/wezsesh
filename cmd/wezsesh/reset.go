package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/Grady-Saccullo/wezsesh/internal/config"
	"github.com/Grady-Saccullo/wezsesh/internal/logger"
	"github.com/Grady-Saccullo/wezsesh/internal/safefs"
)

// resetSidecarSuffix mirrors snapshots.sidecarSuffix; duplicated as an
// unexported package-private constant because the snapshots package keeps
// the suffix unexported and reset must not import private identifiers.
const resetSidecarSuffix = ".wezsesh.json"

// resetReqSubdir / resetReqJSONSuffix / resetReplySockSuffix mirror the
// runtime-dir layout established by ipcdispatcher (`<runtime>/req/<8hex>.json`)
// and ipcsock (`<runtime>/<8hex>.sock`). Duplicated here because both
// packages keep the constants unexported.
const (
	resetReqSubdir       = "req"
	resetReqJSONSuffix   = ".json"
	resetReplySockSuffix = ".sock"
)

// resetWorkspaceSubdir is the resurrect-owned snapshot subdirectory.
// Mirrors `<snapshot_dir>/workspace/` from §13.11 / §10.1.
const resetWorkspaceSubdir = "workspace"

// resetDeprecationToast is the one-line deprecation banner emitted on
// stderr when the binary is invoked as `wezsesh nuke`. §8.20 row 8
// (changelog §0.1 row 8). Wording byte-matches the design's §13.4 prose
// ("nuke renamed to reset; this alias removed in v0.2") so a future
// `grep` against the design doc lands on this callsite.
const resetDeprecationToast = "wezsesh: 'nuke' renamed to 'reset'; this alias removed in v0.2\n"

// resetTTYConfirmPrompt is the §13.4 / §17.3 confirmation prompt rendered
// before destructive `--include-snapshots` runs. Kept as a const so tests
// can assert the exact bytes without scraping I/O.
const resetTTYConfirmPrompt = "Type 'yes' to proceed: "

// resetFlags carries the parsed CLI flags. dryRun and yes are mutually
// observed: --dry-run wins (preview only); --yes alone runs the basic
// reset; --yes --include-snapshots invokes the double-stage gate.
type resetFlags struct {
	dryRun           bool
	yes              bool
	includeSnapshots bool
	yesIReallyMean   bool // --yes-i-really-mean-it (non-TTY confirmation bypass)
}

// resetCategory groups paths by what they represent. Used for the dry-run
// preview header and to keep symlink defenses scoped (a symlink at the
// state dir top is Refuse; a symlink at <state>/state.json is SkipWarn).
type resetCategory string

const (
	resetCatStateFile  resetCategory = "state"
	resetCatTrustFile  resetCategory = "trust"
	resetCatLogFile    resetCategory = "log"
	resetCatSockFile   resetCategory = "sock"
	resetCatReqFile    resetCategory = "req"
	resetCatSidecar    resetCategory = "sidecar"
	resetCatSnapshot   resetCategory = "snapshot"
	resetCatStateDir   resetCategory = "state-dir"
	resetCatTrustDir   resetCategory = "trust-dir"
	resetCatDataDir    resetCategory = "data-dir"
	resetCatRuntimeDir resetCategory = "runtime-dir"
	resetCatReqDir     resetCategory = "req-dir"
)

// resetEntry pairs a path with the category that produced it. Entries are
// applied in slice order so directory removals can run after their
// contents are unlinked.
type resetEntry struct {
	path     string
	category resetCategory
}

// resetPlan is the materialised list of paths a reset run will touch.
// topLevelDirs are the §13.4 SymlinkRefuse anchors verified up front; if
// any is a symlink, the entire run aborts.
type resetPlan struct {
	topLevelDirs []string
	files        []resetEntry
	dirs         []resetEntry
}

// resetTTYProbe returns true when stdin is a character device (i.e. a
// terminal). Centralised so tests can swap it out for a deterministic
// stub. Production keeps it pointed at os.Stdin.
//
// The implementation is the canonical "ModeCharDevice on Stat" check —
// works on linux/darwin without dragging in golang.org/x/term.
var resetTTYProbe = func() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// resetConfirmReader is the io.Reader the TTY confirmation prompt reads
// from. Defaults to os.Stdin; tests inject a strings.Reader.
var resetConfirmReader io.Reader = os.Stdin

// subcmdReset implements `wezsesh reset` (§8.20). invokedAs is "reset"
// or "nuke"; on "nuke" we emit the §0.1-row-8 deprecation toast first
// then fall through to the same code path. §13.14: top-level recover
// matches the keygen / list / trust pattern.
func subcmdReset(invokedAs string, rest []string, stdout, stderr io.Writer) (rc int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "wezsesh reset: panic: %v\n", r)
			rc = exitDoctorOrSubcmd
		}
	}()

	if invokedAs == "nuke" {
		fmt.Fprint(stderr, resetDeprecationToast)
	}

	flags, err := parseResetFlags(rest)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh reset: %v\n", err)
		return exitDoctorOrSubcmd
	}

	ctx := context.Background()
	cfg, err := config.LoadFromEnv(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh reset: config: %v\n", err)
		return exitDoctorOrSubcmd
	}

	// §13.4 symlink defense — top-level managed dirs are Refuse-class.
	// We Enforce on every dir we will touch BEFORE building the plan so a
	// symlink at any anchor aborts the run before any disk mutation.
	if err := enforceResetTopLevels(cfg, nil); err != nil {
		fmt.Fprintf(stderr, "wezsesh reset: %v\n", err)
		return exitDoctorOrSubcmd
	}

	plan, err := buildResetPlan(cfg, flags.includeSnapshots, nil)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh reset: %v\n", err)
		return exitDoctorOrSubcmd
	}

	// Preview mode: no flags, OR --dry-run. Emit the plan to stdout and
	// return. §8.20: "wezsesh reset → preview only (NO writes)".
	if !flags.yes || flags.dryRun {
		writeResetPreview(stdout, plan, flags)
		return exitOK
	}

	// --yes path. --include-snapshots requires the double-stage gate.
	if flags.includeSnapshots {
		if err := confirmResetSnapshots(plan, flags, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "wezsesh reset: %v\n", err)
			return exitDoctorOrSubcmd
		}
	}

	if err := applyResetPlan(plan, nil, stderr); err != nil {
		fmt.Fprintf(stderr, "wezsesh reset: %v\n", err)
		return exitDoctorOrSubcmd
	}
	return exitOK
}

// parseResetFlags accepts --dry-run, --yes, --include-snapshots, and
// --yes-i-really-mean-it. Any positional arg (other than the empty
// trailing remnant) is rejected.
func parseResetFlags(rest []string) (resetFlags, error) {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var p resetFlags
	fs.BoolVar(&p.dryRun, "dry-run", false, "preview-only; no writes")
	fs.BoolVar(&p.yes, "yes", false, "perform deletions (without --yes the run is preview-only)")
	fs.BoolVar(&p.includeSnapshots, "include-snapshots", false, "also remove resurrect snapshot files in <snapshot_dir>/workspace/")
	fs.BoolVar(&p.yesIReallyMean, "yes-i-really-mean-it", false, "bypass TTY confirmation when --include-snapshots is set on a non-TTY stdin")
	if err := fs.Parse(rest); err != nil {
		return p, err
	}
	if fs.NArg() != 0 {
		return p, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	// --include-snapshots without --yes is a valid PREVIEW request. The
	// §17.3 gate ("only on --yes does it remove resurrect files") is
	// enforced in subcmdReset's preview-vs-apply branch; flag parse is
	// permissive about every flag combination.
	return p, nil
}

// enforceResetTopLevels applies SymlinkRefuse to every top-level managed
// dir referenced by cfg. A symlink at any anchor aborts the run with a
// hard error — §13.4 invariant. log may be nil (the reset path is
// deliberately logger-less so a corrupted log file does not block reset).
func enforceResetTopLevels(cfg *config.Config, log *logger.Logger) error {
	dirs := topLevelResetDirs(cfg)
	for _, d := range dirs {
		if d == "" {
			continue
		}
		ok, err := safefs.Enforce(d, safefs.SymlinkRefuse, log)
		if err != nil {
			return fmt.Errorf("symlink defense %s: %w", d, err)
		}
		if !ok {
			return fmt.Errorf("symlink defense %s: refused", d)
		}
	}
	return nil
}

// topLevelResetDirs lists every wezsesh-managed TOP-LEVEL directory the
// reset run might touch. §13.4 ("Symlink defense") enumerates exactly
// four Refuse-class anchors: snapshot, state, data, runtime. Subordinate
// paths (sidecar files, sock files, request files, trust files) are
// file-level SymlinkSkipWarn, applied at unlink time inside applyResetPlan.
// Order does not matter; Enforce is applied to each independently. A path
// that doesn't exist is treated as non-symlink by Enforce (ok=true) so a
// fresh install reset still works.
func topLevelResetDirs(cfg *config.Config) []string {
	out := []string{}
	if cfg.StateDir != "" {
		out = append(out, cfg.StateDir)
	}
	if cfg.RuntimeDir != "" {
		out = append(out, cfg.RuntimeDir)
	}
	if cfg.TrustDir != "" {
		out = append(out, cfg.TrustDir)
	}
	if cfg.DataDir != "" {
		// §13.4 lists data_dir as one of the four Refuse-class anchors:
		// the trust-store lookup descends through `<data>/allow/`, so a
		// symlinked data_dir would silently let us traverse into a
		// stranger's tree even though TrustDir itself is real.
		out = append(out, cfg.DataDir)
	}
	if cfg.SnapshotDir != "" {
		// §13.4 always Enforces snapshot-dir top-level even when
		// --include-snapshots is OFF: the sidecar removal walk descends
		// into <snapshot>/workspace, so a symlinked snapshot dir would
		// otherwise let us follow into a stranger's tree.
		out = append(out, cfg.SnapshotDir)
	}
	return out
}

// buildResetPlan walks the configured directories and groups every path
// the reset run will visit. Symlink-at-file granularity is NOT applied
// here — that runs at unlink time so the dry-run preview shows the path
// even if it would later be skipped (the user wants to see it for
// auditing).
func buildResetPlan(cfg *config.Config, includeSnapshots bool, _ *logger.Logger) (resetPlan, error) {
	plan := resetPlan{topLevelDirs: topLevelResetDirs(cfg)}

	// Log files first so they are unlinked before the state-dir removal
	// (which would otherwise be non-empty). Two patterns: <state>/wezsesh.log
	// and <state>/wezsesh.log.<n>.
	if cfg.StateDir != "" {
		logs, err := listResetLogFiles(cfg.StateDir)
		if err != nil {
			return resetPlan{}, fmt.Errorf("list logs: %w", err)
		}
		for _, p := range logs {
			plan.files = append(plan.files, resetEntry{path: p, category: resetCatLogFile})
		}
		// state.json + state.json.bak (the v=2-bak migration backup).
		stateFiles, err := listResetStateFiles(cfg.StateDir)
		if err != nil {
			return resetPlan{}, fmt.Errorf("list state files: %w", err)
		}
		for _, p := range stateFiles {
			plan.files = append(plan.files, resetEntry{path: p, category: resetCatStateFile})
		}
	}

	if cfg.TrustDir != "" {
		trustFiles, err := listResetTrustFiles(cfg.TrustDir)
		if err != nil {
			return resetPlan{}, fmt.Errorf("list trust files: %w", err)
		}
		for _, p := range trustFiles {
			plan.files = append(plan.files, resetEntry{path: p, category: resetCatTrustFile})
		}
	}

	if cfg.RuntimeDir != "" {
		socks, err := listResetSockFiles(cfg.RuntimeDir)
		if err != nil {
			return resetPlan{}, fmt.Errorf("list sock files: %w", err)
		}
		for _, p := range socks {
			plan.files = append(plan.files, resetEntry{path: p, category: resetCatSockFile})
		}
		reqs, err := listResetReqFiles(cfg.RuntimeDir)
		if err != nil {
			return resetPlan{}, fmt.Errorf("list req files: %w", err)
		}
		for _, p := range reqs {
			plan.files = append(plan.files, resetEntry{path: p, category: resetCatReqFile})
		}
	}

	if cfg.SnapshotDir != "" {
		sidecars, err := listResetSidecars(cfg.SnapshotDir)
		if err != nil {
			return resetPlan{}, fmt.Errorf("list sidecars: %w", err)
		}
		for _, p := range sidecars {
			plan.files = append(plan.files, resetEntry{path: p, category: resetCatSidecar})
		}
		if includeSnapshots {
			snaps, err := listResetSnapshots(cfg.SnapshotDir)
			if err != nil {
				return resetPlan{}, fmt.Errorf("list snapshots: %w", err)
			}
			for _, p := range snaps {
				plan.files = append(plan.files, resetEntry{path: p, category: resetCatSnapshot})
			}
		}
	}

	// Directories — best-effort post-clean. Order: deepest first so the
	// parent removal sees an empty dir on the happy path.
	if cfg.RuntimeDir != "" {
		plan.dirs = append(plan.dirs, resetEntry{
			path: filepath.Join(cfg.RuntimeDir, resetReqSubdir), category: resetCatReqDir,
		})
		plan.dirs = append(plan.dirs, resetEntry{
			path: cfg.RuntimeDir, category: resetCatRuntimeDir,
		})
	}
	if cfg.TrustDir != "" {
		plan.dirs = append(plan.dirs, resetEntry{
			path: cfg.TrustDir, category: resetCatTrustDir,
		})
	}
	// §13.4 "trust store (all contents + `allow/` dir + `wezsesh/` parent
	// if empty)" — DataDir IS the `wezsesh/` parent of `<data>/allow/`,
	// and must rmdir AFTER TrustDir so the trust-dir removal can leave
	// DataDir empty. resetRmdir's ENOTEMPTY tolerance handles the
	// not-empty case naturally (a stranger's file in <data>/ keeps the
	// dir).
	if cfg.DataDir != "" {
		plan.dirs = append(plan.dirs, resetEntry{
			path: cfg.DataDir, category: resetCatDataDir,
		})
	}
	if cfg.StateDir != "" {
		plan.dirs = append(plan.dirs, resetEntry{
			path: cfg.StateDir, category: resetCatStateDir,
		})
	}
	return plan, nil
}

// listResetLogFiles returns wezsesh.log + every rotated wezsesh.log.<n>.
// Missing dir or no matches → empty slice.
func listResetLogFiles(stateDir string) ([]string, error) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		nm := e.Name()
		if nm == "wezsesh.log" || strings.HasPrefix(nm, "wezsesh.log.") {
			out = append(out, filepath.Join(stateDir, nm))
		}
	}
	sort.Strings(out)
	return out, nil
}

// listResetStateFiles returns state.json + .v<N>.bak migration backups.
func listResetStateFiles(stateDir string) ([]string, error) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		nm := e.Name()
		if nm == "state.json" || strings.HasPrefix(nm, "state.json.") {
			out = append(out, filepath.Join(stateDir, nm))
		}
	}
	sort.Strings(out)
	return out, nil
}

// listResetTrustFiles returns every regular file in trustDir. The trust
// store body is `<sha256-hex>` named files (§10.5); we don't hard-match
// the basename pattern because a leftover from a partial write or a
// future rename should still be cleaned up.
func listResetTrustFiles(trustDir string) ([]string, error) {
	entries, err := os.ReadDir(trustDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out = append(out, filepath.Join(trustDir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

// listResetSockFiles returns *.sock files at the top of runtimeDir.
func listResetSockFiles(runtimeDir string) ([]string, error) {
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		nm := e.Name()
		if !strings.HasSuffix(nm, resetReplySockSuffix) {
			continue
		}
		out = append(out, filepath.Join(runtimeDir, nm))
	}
	sort.Strings(out)
	return out, nil
}

// listResetReqFiles returns *.json files under <runtimeDir>/req/.
func listResetReqFiles(runtimeDir string) ([]string, error) {
	dir := filepath.Join(runtimeDir, resetReqSubdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		nm := e.Name()
		if !strings.HasSuffix(nm, resetReqJSONSuffix) {
			continue
		}
		out = append(out, filepath.Join(dir, nm))
	}
	sort.Strings(out)
	return out, nil
}

// listResetSidecars returns *.wezsesh.json under <snapshotDir>/workspace/.
// Resurrect snapshot files (*.json) are NOT included here — those are
// gated on --include-snapshots and listed by listResetSnapshots.
func listResetSidecars(snapshotDir string) ([]string, error) {
	dir := filepath.Join(snapshotDir, resetWorkspaceSubdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		nm := e.Name()
		if !strings.HasSuffix(nm, resetSidecarSuffix) {
			continue
		}
		out = append(out, filepath.Join(dir, nm))
	}
	sort.Strings(out)
	return out, nil
}

// listResetSnapshots returns *.json files under <snapshotDir>/workspace/
// that are NOT sidecars. These are the resurrect-owned snapshot files —
// only deleted when --include-snapshots is set.
func listResetSnapshots(snapshotDir string) ([]string, error) {
	dir := filepath.Join(snapshotDir, resetWorkspaceSubdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		nm := e.Name()
		if !strings.HasSuffix(nm, ".json") {
			continue
		}
		// Skip sidecars (those have the .wezsesh.json suffix which also
		// ends in .json).
		if strings.HasSuffix(nm, resetSidecarSuffix) {
			continue
		}
		out = append(out, filepath.Join(dir, nm))
	}
	sort.Strings(out)
	return out, nil
}

// writeResetPreview emits the planned removals to stdout. Output shape is
// designed for scripts: every actionable path is on its own line with a
// stable category prefix.
func writeResetPreview(w io.Writer, plan resetPlan, flags resetFlags) {
	if flags.dryRun {
		fmt.Fprintln(w, "wezsesh reset --dry-run: previewing planned removals")
	} else {
		fmt.Fprintln(w, "wezsesh reset: preview-only (pass --yes to perform deletions)")
	}
	if !flags.includeSnapshots {
		fmt.Fprintln(w, "(resurrect snapshots NOT included; pass --include-snapshots to remove them)")
	}
	for _, e := range plan.files {
		fmt.Fprintf(w, "  remove %s %s\n", e.category, e.path)
	}
	for _, e := range plan.dirs {
		fmt.Fprintf(w, "  rmdir  %s %s\n", e.category, e.path)
	}
	if len(plan.files) == 0 && len(plan.dirs) == 0 {
		fmt.Fprintln(w, "  (nothing to remove)")
	}
}

// confirmResetSnapshots is the §13.4 double-stage gate for
// `--yes --include-snapshots`. Behaviour:
//
//   - Stdin is a TTY: print the absolute paths of each snapshot file plus
//     the prompt; require the user to type "yes" verbatim.
//   - Stdin is NOT a TTY: require --yes-i-really-mean-it to bypass.
//
// Returns nil on success, an error on refusal. The error string is the
// caller-surfaced message; "" wraps to no extra text.
func confirmResetSnapshots(plan resetPlan, flags resetFlags, stdout, stderr io.Writer) error {
	// Print the snapshot paths so the user can audit before answering.
	fmt.Fprintln(stderr, "wezsesh reset --include-snapshots will REMOVE these resurrect-owned files:")
	any := false
	for _, e := range plan.files {
		if e.category != resetCatSnapshot {
			continue
		}
		fmt.Fprintf(stderr, "  %s\n", e.path)
		any = true
	}
	if !any {
		// No snapshot files to remove — nothing to confirm.
		return nil
	}
	if !resetTTYProbe() {
		if flags.yesIReallyMean {
			return nil
		}
		return errors.New("--include-snapshots requires a TTY for confirmation; pass --yes-i-really-mean-it to bypass")
	}
	fmt.Fprint(stderr, resetTTYConfirmPrompt)
	scanner := bufio.NewScanner(resetConfirmReader)
	if !scanner.Scan() {
		return errors.New("--include-snapshots: no confirmation input received; aborting")
	}
	if strings.TrimSpace(scanner.Text()) != "yes" {
		return errors.New("--include-snapshots: confirmation declined; aborting")
	}
	return nil
}

// applyResetPlan executes the plan. Per-file symlink defense is
// SymlinkSkipWarn: a symlink in the plan is logged and left in place so
// one bad entry does not abort the batch. Top-level Refuse already ran
// in subcmdReset before the plan was built. log may be nil (the reset
// path is intentionally logger-less to avoid bootstrapping a logger
// whose log file is itself slated for removal); when nil, we surface
// SkipWarn lines on stderr instead.
func applyResetPlan(plan resetPlan, log *logger.Logger, stderr io.Writer) error {
	for _, e := range plan.files {
		if err := resetUnlinkFile(e.path, log, stderr); err != nil {
			return err
		}
	}
	// Best-effort dir removal. We deliberately do NOT use SafeRemoveTree
	// here because the per-file pass above already ran with SkipWarn
	// granularity; SafeRemoveTree would re-invoke that walk and double-
	// log. os.Remove succeeds only on an empty dir, leaves a non-empty
	// dir in place, and the os.ErrNotExist branch is benign (dir was
	// never created).
	for _, e := range plan.dirs {
		if err := resetRmdir(e.path, log, stderr); err != nil {
			return err
		}
	}
	return nil
}

// resetUnlinkFile removes a single file with file-scope symlink defense
// (SkipWarn). Returns nil on missing-file (already gone). Other unlink
// errors propagate.
func resetUnlinkFile(path string, log *logger.Logger, stderr io.Writer) error {
	ok, err := safefs.Enforce(path, safefs.SymlinkSkipWarn, log)
	if err != nil {
		return fmt.Errorf("enforce %s: %w", path, err)
	}
	if !ok {
		// SymlinkSkipWarn: Enforce already log_warned via the logger
		// (when non-nil). Surface on stderr too so a logger-less reset
		// run still prints the SkipWarn.
		if log == nil {
			fmt.Fprintf(stderr, "wezsesh reset: skipping symlink %s\n", path)
		}
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// resetRmdir removes an empty directory. Symlink-at-dir is treated as
// Refuse-class — but the top-level Enforce in subcmdReset already
// enforced that. Here we re-Enforce defensively (a TOCTOU window between
// the top-level check and the remove) and SkipWarn on the file-level
// dir-symlink path. A non-empty dir is left in place silently — the
// per-file walk leaves dirs containing skipped symlinks intact, and the
// rmdir call returning ENOTEMPTY is the canonical signal to leave the
// dir alone.
func resetRmdir(path string, log *logger.Logger, stderr io.Writer) error {
	ok, err := safefs.Enforce(path, safefs.SymlinkSkipWarn, log)
	if err != nil {
		return fmt.Errorf("enforce %s: %w", path, err)
	}
	if !ok {
		if log == nil {
			fmt.Fprintf(stderr, "wezsesh reset: skipping symlink dir %s\n", path)
		}
		return nil
	}
	err = os.Remove(path)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrNotExist):
		return nil
	case isNotEmpty(err):
		return nil
	default:
		return fmt.Errorf("rmdir %s: %w", path, err)
	}
}

// isNotEmpty checks for a "directory not empty" errno across linux/darwin.
// The Go runtime wraps the syscall error as an *os.PathError whose Err is
// the platform errno; errors.Is matches the Errno against syscall.ENOTEMPTY
// uniformly.
func isNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY)
}
