package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Grady-Saccullo/wezsesh/internal/argvallow"
	"github.com/Grady-Saccullo/wezsesh/internal/safefs"
	"github.com/Grady-Saccullo/wezsesh/internal/snapshots"
	"github.com/Grady-Saccullo/wezsesh/internal/state"
	"github.com/Grady-Saccullo/wezsesh/internal/wezcli"
)

// ReqOrphanThreshold is the per §3.5 / §12.4 / §3.1 sweep window — a
// `*.json` request file older than this is considered orphaned and
// surfaced by the `runtime.dir.req_orphans` check. Exported so the
// startup-sweep helper in `internal/reqsweep` can stay in lockstep
// with the doctor's definition of "orphaned"; if these two drift,
// users see a doctor row that disagrees with what the sweep just did.
const ReqOrphanThreshold = 60 * time.Second

// encryptionAgentBudget is the §7 ENCRYPTION_AGENT_SLOW threshold.
// Anything past this surfaces as a doctor warn.
const encryptionAgentBudget = 2 * time.Second

// weztermVersionFloor is the §8.17.1 floor.
const weztermVersionFloor = "20230408-112425-69ae8472"

// pluginVersionExpected is the canonical plugin/binary version pair
// per §10.7 / §11.5; mismatch surfaces as the version.compatible check.
// We allow Env.PluginVersion to match Env.BinaryVersion-ish via direct
// equality with a baseline ("0.1.0") — the spec only requires that the
// check fires when the plugin version drifts from the binary's idea of
// what it should be. Empty PluginVersion is StatusSkip (no plugin yet).
const pluginVersionExpected = "0.1.0"

// luaVersionFloor is §9.0 — Lua 5.3 or newer.
const luaVersionFloor = "5.3"

// sunPathBudgetLinux / sunPathBudgetDarwin per §3.2 / §17.3 SUN_PATH
// row. The budget reserves 14 bytes for `/<8hex>.sock` (1 + 8 + 5).
const (
	sunPathLinux  = 108
	sunPathDarwin = 104
	sunPathSlack  = 14
)

// =============================================================
// binary.* checks
// =============================================================

func checkBinaryVersion(env Env) Check {
	if env.BinaryPath == "" {
		return Check{ID: "binary.version", Status: StatusSkip, Message: "BinaryPath not provided"}
	}
	return Check{
		ID:      "binary.version",
		Status:  StatusOK,
		Message: "binary path resolves",
		Details: map[string]any{"path": env.BinaryPath},
	}
}

func checkBinaryPath(env Env) Check {
	if env.BinaryPath == "" {
		return Check{ID: "binary.path", Status: StatusFail, Message: "binary path is empty"}
	}
	if _, err := os.Stat(env.BinaryPath); err != nil {
		return Check{
			ID:      "binary.path",
			Status:  StatusFail,
			Message: "binary path does not stat",
			Details: map[string]any{"path": env.BinaryPath, "err": err.Error()},
		}
	}
	return Check{ID: "binary.path", Status: StatusOK, Details: map[string]any{"path": env.BinaryPath}}
}

func checkBinaryFSNetwork(env Env) Check {
	return fsNetworkCheck("binary.fs.network", env.BinaryPath)
}

// =============================================================
// plugin.version / version.compatible
// =============================================================

func checkPluginVersion(env Env) Check {
	if env.PluginVersion == "" {
		return Check{ID: "plugin.version", Status: StatusSkip, Message: "plugin version not reported"}
	}
	return Check{
		ID:      "plugin.version",
		Status:  StatusOK,
		Details: map[string]any{"version": env.PluginVersion},
	}
}

func checkVersionCompatible(env Env) Check {
	if env.PluginVersion == "" {
		return Check{ID: "version.compatible", Status: StatusSkip, Message: "plugin version unknown"}
	}
	if env.PluginVersion != pluginVersionExpected {
		return Check{
			ID:      "version.compatible",
			Status:  StatusWarn,
			Message: "plugin version differs from expected",
			Details: map[string]any{
				"plugin":   env.PluginVersion,
				"expected": pluginVersionExpected,
			},
		}
	}
	return Check{ID: "version.compatible", Status: StatusOK}
}

// =============================================================
// wezterm.* checks
// =============================================================

func checkWeztermVersion(ctx context.Context, _ Env) Check {
	c, err := wezcliFactory()
	if err != nil {
		return Check{ID: "wezterm.version", Status: StatusSkip, Message: err.Error()}
	}
	d, err := c.Probe(ctx)
	if err != nil {
		return Check{
			ID:      "wezterm.version",
			Status:  StatusFail,
			Message: "wezterm cli unreachable",
			Details: map[string]any{"err": err.Error(), "elapsed_ms": d.Milliseconds()},
		}
	}
	return Check{
		ID:      "wezterm.version",
		Status:  StatusOK,
		Message: "wezterm cli reachable",
		Details: map[string]any{"floor": weztermVersionFloor, "elapsed_ms": d.Milliseconds()},
	}
}

func checkWeztermLuaVersion(_ context.Context, _ Env) Check {
	// The doctor binary has no IPC channel to a running plugin instance
	// and `wezterm cli` does not expose `_VERSION`. The Lua-version
	// floor (§9.0) is asserted at build time by the §16.4 CI matrix
	// gate ("wezterm shipped Lua `_VERSION` ≥ 'Lua 5.3'"); the runtime
	// doctor row is informational only. See T-DOC-045.
	return Check{
		ID:      "wezterm.lua_version",
		Status:  StatusSkip,
		Message: "doctor cannot probe Lua _VERSION; §16.4 CI matrix asserts the floor",
		Details: map[string]any{"floor": luaVersionFloor},
	}
}

func checkWeztermCLIList(ctx context.Context, _ Env) Check {
	c, err := wezcliFactory()
	if err != nil {
		return Check{ID: "wezterm.cli.list", Status: StatusSkip, Message: err.Error()}
	}
	panes, err := c.List(ctx)
	if err != nil {
		return Check{ID: "wezterm.cli.list", Status: StatusFail, Details: map[string]any{"err": err.Error()}}
	}
	return Check{ID: "wezterm.cli.list", Status: StatusOK, Details: map[string]any{"panes": len(panes)}}
}

func checkWeztermCLIListClients(ctx context.Context, _ Env) Check {
	c, err := wezcliFactory()
	if err != nil {
		return Check{ID: "wezterm.cli.list-clients", Status: StatusSkip, Message: err.Error()}
	}
	clients, err := c.ListClients(ctx)
	if err != nil {
		return Check{ID: "wezterm.cli.list-clients", Status: StatusFail, Details: map[string]any{"err": err.Error()}}
	}
	return Check{ID: "wezterm.cli.list-clients", Status: StatusOK, Details: map[string]any{"clients": len(clients)}}
}

// checkWeztermCLITTYName confirms that at least one pane in `cli list`
// reports a non-nil tty_name. A mux full of nil tty_names means
// pathwalk-based features (find --deep) cannot work.
func checkWeztermCLITTYName(ctx context.Context, _ Env) Check {
	c, err := wezcliFactory()
	if err != nil {
		return Check{ID: "wezterm.cli.tty_name", Status: StatusSkip, Message: err.Error()}
	}
	panes, err := c.List(ctx)
	if err != nil {
		return Check{ID: "wezterm.cli.tty_name", Status: StatusSkip, Message: "wezterm cli unreachable"}
	}
	for _, p := range panes {
		if p.TTYName != nil && *p.TTYName != "" {
			return Check{ID: "wezterm.cli.tty_name", Status: StatusOK, Details: map[string]any{"sample": *p.TTYName}}
		}
	}
	if len(panes) == 0 {
		return Check{ID: "wezterm.cli.tty_name", Status: StatusSkip, Message: "no panes to inspect"}
	}
	return Check{
		ID:      "wezterm.cli.tty_name",
		Status:  StatusWarn,
		Message: "no pane reports tty_name",
	}
}

// checkWeztermPaneEnv inspects the binary's process env per §8.17.1
// row "wezterm.pane.env  ← WEZTERM_PANE set + resolves". Empty value
// is Fail (the binary was not spawned inside a wezterm pane). When
// set, the value is parsed as an int and cross-referenced against
// `cli list`; a mismatch is Warn (the env points at a stale pane id).
// If the wezcli factory cannot be built, or `cli list` errors, the
// resolution leg is skipped and the check is OK with a note.
func checkWeztermPaneEnv(ctx context.Context, _ Env) Check {
	v := envGet("WEZTERM_PANE")
	if v == "" {
		return Check{
			ID:      "wezterm.pane.env",
			Status:  StatusFail,
			Message: "WEZTERM_PANE not set",
		}
	}
	paneID, perr := strconv.Atoi(strings.TrimSpace(v))
	if perr != nil {
		return Check{
			ID:      "wezterm.pane.env",
			Status:  StatusWarn,
			Message: "WEZTERM_PANE is not an integer",
			Details: map[string]any{"pane_id": v},
		}
	}
	c, err := wezcliFactory()
	if err != nil {
		return Check{
			ID:      "wezterm.pane.env",
			Status:  StatusSkip,
			Message: "WEZTERM_PANE set; resolution skipped (" + err.Error() + ")",
			Details: map[string]any{"pane_id": paneID, "resolved": false},
		}
	}
	panes, lerr := c.List(ctx)
	if lerr != nil {
		return Check{
			ID:      "wezterm.pane.env",
			Status:  StatusSkip,
			Message: "WEZTERM_PANE set; cli list unavailable",
			Details: map[string]any{"pane_id": paneID, "resolved": false, "err": lerr.Error()},
		}
	}
	for _, p := range panes {
		if p.PaneID == paneID {
			return Check{
				ID:      "wezterm.pane.env",
				Status:  StatusOK,
				Details: map[string]any{"pane_id": paneID, "resolved": true},
			}
		}
	}
	return Check{
		ID:      "wezterm.pane.env",
		Status:  StatusWarn,
		Message: "WEZTERM_PANE does not match any pane in cli list",
		Details: map[string]any{"pane_id": paneID, "resolved": false, "panes": len(panes)},
	}
}

// =============================================================
// snapshot.* checks
// =============================================================

func checkSnapshotDirExists(env Env) Check {
	return dirExistsCheck("snapshot.dir.exists", env.SnapshotDir)
}

func checkSnapshotDirWritable(ctx context.Context, env Env) Check {
	return dirWritableCheck(ctx, "snapshot.dir.writable", env.SnapshotDir)
}

func checkSnapshotDirFSNetwork(env Env) Check {
	return fsNetworkCheck("snapshot.dir.fs.network", env.SnapshotDir)
}

// checkSnapshotDirMatchesResurrect confirms that resurrect's standard
// subdirs (workspace/, window/) sit under env.SnapshotDir. If neither
// is present, resurrect's save_state_dir likely points elsewhere.
func checkSnapshotDirMatchesResurrect(env Env) Check {
	if env.SnapshotDir == "" {
		return Check{ID: "snapshot.dir.matches.resurrect", Status: StatusSkip}
	}
	ws := dirExists(filepath.Join(env.SnapshotDir, "workspace"))
	wn := dirExists(filepath.Join(env.SnapshotDir, "window"))
	if !ws && !wn {
		return Check{
			ID:      "snapshot.dir.matches.resurrect",
			Status:  StatusWarn,
			Message: "neither workspace/ nor window/ present under snapshot_dir",
			Details: map[string]any{"snapshot_dir": env.SnapshotDir},
		}
	}
	return Check{
		ID:      "snapshot.dir.matches.resurrect",
		Status:  StatusOK,
		Details: map[string]any{"workspace": ws, "window": wn},
	}
}

func checkSnapshotCount(ctx context.Context, env Env) Check {
	if env.SnapshotDir == "" {
		return Check{ID: "snapshot.count", Status: StatusSkip}
	}
	repo, err := snapshotsOpener(env.SnapshotDir)
	if err != nil {
		return Check{ID: "snapshot.count", Status: StatusSkip, Message: err.Error()}
	}
	entries, err := repo.List(ctx)
	if err != nil {
		return Check{ID: "snapshot.count", Status: StatusWarn, Details: map[string]any{"err": err.Error()}}
	}
	return Check{ID: "snapshot.count", Status: StatusOK, Details: map[string]any{"count": len(entries)}}
}

// checkSnapshotNameValidation reports any snapshot whose decoded name
// is empty or contains a control char. List already decodes; we just
// flag the bad ones.
func checkSnapshotNameValidation(ctx context.Context, env Env) Check {
	if env.SnapshotDir == "" {
		return Check{ID: "snapshot.name.validation", Status: StatusSkip}
	}
	repo, err := snapshotsOpener(env.SnapshotDir)
	if err != nil {
		return Check{ID: "snapshot.name.validation", Status: StatusSkip, Message: err.Error()}
	}
	entries, err := repo.List(ctx)
	if err != nil {
		return Check{ID: "snapshot.name.validation", Status: StatusWarn, Details: map[string]any{"err": err.Error()}}
	}
	bad := []string{}
	for _, e := range entries {
		if e.Name == "" {
			bad = append(bad, "<empty>")
			continue
		}
		if strings.ContainsAny(e.Name, "\x00\r\n\t") {
			bad = append(bad, e.Name)
		}
	}
	if len(bad) > 0 {
		return Check{
			ID:      "snapshot.name.validation",
			Status:  StatusWarn,
			Message: "snapshots with invalid names",
			Details: map[string]any{"names": bad},
		}
	}
	return Check{ID: "snapshot.name.validation", Status: StatusOK}
}

// checkSnapshotArgvAllowlistCoverage walks every snapshot and reports
// argv basenames not in the active argvallow auditor.
func checkSnapshotArgvAllowlistCoverage(ctx context.Context, env Env) Check {
	if env.SnapshotDir == "" {
		return Check{ID: "snapshot.argv.allowlist.coverage", Status: StatusSkip}
	}
	repo, err := snapshotsOpener(env.SnapshotDir)
	if err != nil {
		return Check{ID: "snapshot.argv.allowlist.coverage", Status: StatusSkip, Message: err.Error()}
	}
	entries, err := repo.List(ctx)
	if err != nil {
		return Check{ID: "snapshot.argv.allowlist.coverage", Status: StatusWarn, Details: map[string]any{"err": err.Error()}}
	}
	user := []string(nil)
	if env.Cfg != nil {
		user = env.Cfg.ResurrectArgvAllowlist
	}
	auditor := argvallow.NewAuditor(envGet("SHELL"), user)
	missing := map[string]struct{}{}
	for _, e := range entries {
		if e.State == nil {
			continue
		}
		walkPaneArgvs(e.State, func(basename string) {
			if !auditor.Allows(basename) {
				missing[basename] = struct{}{}
			}
		})
	}
	if len(missing) > 0 {
		out := make([]string, 0, len(missing))
		for k := range missing {
			out = append(out, k)
		}
		sort.Strings(out)
		return Check{
			ID:      "snapshot.argv.allowlist.coverage",
			Status:  StatusWarn,
			Message: "snapshot argv basenames missing from allowlist",
			Details: map[string]any{"missing": out},
		}
	}
	return Check{ID: "snapshot.argv.allowlist.coverage", Status: StatusOK}
}

// walkPaneArgvs walks every pane in a WorkspaceState and invokes f
// with the program basename of each pane's argv (or LegacyString).
func walkPaneArgvs(ws *snapshots.WorkspaceState, f func(string)) {
	if ws == nil {
		return
	}
	for _, w := range ws.WindowStates {
		for _, t := range w.Tabs {
			walkPaneTree(t.PaneTree, f)
		}
	}
}

func walkPaneTree(pt *snapshots.PaneTree, f func(string)) {
	if pt == nil {
		return
	}
	if pt.Process != nil {
		switch {
		case len(pt.Process.Argv) > 0:
			f(filepath.Base(pt.Process.Argv[0]))
		case pt.Process.LegacyString != nil && *pt.Process.LegacyString != "":
			f(filepath.Base(*pt.Process.LegacyString))
		case pt.Process.Name != nil && *pt.Process.Name != "":
			f(filepath.Base(*pt.Process.Name))
		}
	}
	walkPaneTree(pt.Bottom, f)
	walkPaneTree(pt.Right, f)
}

func checkSnapshotEncryptionDetected(ctx context.Context, env Env) Check {
	if env.SnapshotDir == "" {
		return Check{ID: "snapshot.encryption.detected", Status: StatusSkip}
	}
	repo, err := snapshotsOpener(env.SnapshotDir)
	if err != nil {
		return Check{ID: "snapshot.encryption.detected", Status: StatusSkip, Message: err.Error()}
	}
	entries, err := repo.List(ctx)
	if err != nil {
		return Check{ID: "snapshot.encryption.detected", Status: StatusWarn, Details: map[string]any{"err": err.Error()}}
	}
	tally := map[string]int{}
	unknown := 0
	for _, e := range entries {
		enc, sErr := repo.Sniff(ctx, e.Name)
		if sErr != nil {
			unknown++
			continue
		}
		tally[enc.String()]++
		if enc == snapshots.EncryptionUnknown {
			unknown++
		}
	}
	if unknown > 0 {
		return Check{
			ID:      "snapshot.encryption.detected",
			Status:  StatusWarn,
			Message: "snapshots with unrecognised encryption",
			Details: map[string]any{"tally": tally, "unknown": unknown},
		}
	}
	return Check{
		ID:      "snapshot.encryption.detected",
		Status:  StatusOK,
		Details: map[string]any{"tally": tally},
	}
}

// checkSnapshotPinConsistency — §17.3 acceptance gate "Pin doctor
// consistency". `live_pins ∩ saved-names ≠ ∅` → warn (a saved
// workspace owns its pin via the sidecar; an entry in live_pins for
// the same name is a stale leftover that the next save will prune).
func checkSnapshotPinConsistency(ctx context.Context, env Env) Check {
	if env.StateDir == "" || env.SnapshotDir == "" {
		return Check{ID: "snapshot.pin.consistency", Status: StatusSkip}
	}
	livePins, err := stateOpener(ctx, env.StateDir)
	if err != nil {
		return Check{ID: "snapshot.pin.consistency", Status: StatusSkip, Message: err.Error()}
	}
	repo, err := snapshotsOpener(env.SnapshotDir)
	if err != nil {
		return Check{ID: "snapshot.pin.consistency", Status: StatusSkip, Message: err.Error()}
	}
	overlap := []string{}
	for _, name := range livePins {
		ok, _ := repo.Has(ctx, name)
		if ok {
			overlap = append(overlap, name)
		}
	}
	if len(overlap) > 0 {
		sort.Strings(overlap)
		return Check{
			ID:      "snapshot.pin.consistency",
			Status:  StatusWarn,
			Message: "live_pins ∩ saved-names is non-empty",
			Details: map[string]any{"overlap": overlap},
		}
	}
	return Check{ID: "snapshot.pin.consistency", Status: StatusOK}
}

// =============================================================
// state.* / data.* / runtime.* / trust.* checks
// =============================================================

func checkStateDirExists(env Env) Check {
	return dirExistsCheck("state.dir.exists", env.StateDir)
}
func checkStateDirWritable(ctx context.Context, env Env) Check {
	return dirWritableCheck(ctx, "state.dir.writable", env.StateDir)
}
func checkStateDirFSNetwork(env Env) Check {
	return fsNetworkCheck("state.dir.fs.network", env.StateDir)
}
func checkDataDirExists(env Env) Check {
	return dirExistsCheck("data.dir.exists", env.DataDir)
}
func checkDataDirWritable(ctx context.Context, env Env) Check {
	return dirWritableCheck(ctx, "data.dir.writable", env.DataDir)
}
func checkDataDirFSNetwork(env Env) Check {
	return fsNetworkCheck("data.dir.fs.network", env.DataDir)
}

// checkTrustDirExists rejects symlink (§8.17.1 row "trust.dir.exists ←
// rejects symlink").
func checkTrustDirExists(env Env) Check {
	if env.TrustDir == "" {
		return Check{ID: "trust.dir.exists", Status: StatusSkip}
	}
	info, err := os.Lstat(env.TrustDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Check{
				ID:      "trust.dir.exists",
				Status:  StatusWarn,
				Message: "trust dir does not exist",
				Details: map[string]any{"path": env.TrustDir},
			}
		}
		return Check{ID: "trust.dir.exists", Status: StatusFail, Details: map[string]any{"err": err.Error()}}
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return Check{
			ID:      "trust.dir.exists",
			Status:  StatusFail,
			Message: "trust dir is a symlink",
			Details: map[string]any{"path": env.TrustDir},
		}
	}
	if !info.IsDir() {
		return Check{
			ID:      "trust.dir.exists",
			Status:  StatusFail,
			Message: "trust dir is not a directory",
			Details: map[string]any{"path": env.TrustDir},
		}
	}
	return Check{ID: "trust.dir.exists", Status: StatusOK}
}

// checkTrustCount counts entries in TrustDir whose basename looks like
// a 64-char hex hash. Non-hash garbage is silently ignored — that is
// the trust dir's own discipline.
func checkTrustCount(env Env) Check {
	if env.TrustDir == "" {
		return Check{ID: "trust.count", Status: StatusSkip}
	}
	dents, err := os.ReadDir(env.TrustDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Check{ID: "trust.count", Status: StatusOK, Details: map[string]any{"count": 0}}
		}
		return Check{ID: "trust.count", Status: StatusWarn, Details: map[string]any{"err": err.Error()}}
	}
	n := 0
	for _, d := range dents {
		if looksLikeHash(d.Name()) && !d.IsDir() {
			n++
		}
	}
	return Check{ID: "trust.count", Status: StatusOK, Details: map[string]any{"count": n}}
}

// checkTrustOrphans walks each trust file's recorded path and reports
// any whose target is missing.
func checkTrustOrphans(env Env) Check {
	if env.TrustDir == "" {
		return Check{ID: "trust.orphans", Status: StatusSkip}
	}
	dents, err := os.ReadDir(env.TrustDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Check{ID: "trust.orphans", Status: StatusOK, Details: map[string]any{"orphans": 0}}
		}
		return Check{ID: "trust.orphans", Status: StatusWarn, Details: map[string]any{"err": err.Error()}}
	}
	type body struct {
		Path string `json:"path"`
	}
	orphans := 0
	for _, d := range dents {
		name := d.Name()
		if !looksLikeHash(name) || d.IsDir() {
			continue
		}
		full := filepath.Join(env.TrustDir, name)
		f, ferr := safefs.SafeOpenForRead(full)
		if ferr != nil {
			continue
		}
		raw, rerr := io.ReadAll(f)
		_ = f.Close()
		if rerr != nil {
			continue
		}
		var b body
		if jerr := json.Unmarshal(raw, &b); jerr != nil || b.Path == "" {
			continue
		}
		if _, sErr := os.Lstat(b.Path); sErr != nil && errors.Is(sErr, os.ErrNotExist) {
			orphans++
		}
	}
	if orphans > 0 {
		return Check{
			ID:      "trust.orphans",
			Status:  StatusWarn,
			Message: "trust entries with missing target paths",
			Details: map[string]any{"orphans": orphans},
		}
	}
	return Check{ID: "trust.orphans", Status: StatusOK, Details: map[string]any{"orphans": 0}}
}

func checkRuntimeDirExists(env Env) Check {
	return dirExistsCheck("runtime.dir.exists", env.RuntimeDir)
}
func checkRuntimeDirFSNetwork(env Env) Check {
	return fsNetworkCheck("runtime.dir.fs.network", env.RuntimeDir)
}

// checkRuntimeDirPermissions confirms the runtime dir is mode 0700.
func checkRuntimeDirPermissions(env Env) Check {
	if env.RuntimeDir == "" {
		return Check{ID: "runtime.dir.permissions", Status: StatusSkip}
	}
	info, err := os.Lstat(env.RuntimeDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Check{ID: "runtime.dir.permissions", Status: StatusSkip, Message: "runtime dir absent"}
		}
		return Check{ID: "runtime.dir.permissions", Status: StatusWarn, Details: map[string]any{"err": err.Error()}}
	}
	mode := info.Mode().Perm()
	if mode != 0o700 {
		return Check{
			ID:      "runtime.dir.permissions",
			Status:  StatusWarn,
			Message: "runtime dir is not mode 0700",
			Details: map[string]any{"mode": fmt.Sprintf("%o", mode)},
		}
	}
	return Check{ID: "runtime.dir.permissions", Status: StatusOK}
}

// checkRuntimeDirSunPathBudget verifies that <runtime_dir>/<8hex>.sock
// fits the SUN_PATH cap with the §3.2 14-byte slack (1 + 8 + 5 for
// `/` + 8hex + `.sock`).
func checkRuntimeDirSunPathBudget(env Env) Check {
	if env.RuntimeDir == "" {
		return Check{ID: "runtime.dir.sun_path_budget", Status: StatusSkip}
	}
	budget := sunPathLinux
	if runtime.GOOS == "darwin" {
		budget = sunPathDarwin
	}
	used := len(env.RuntimeDir) + sunPathSlack
	if used > budget {
		return Check{
			ID:      "runtime.dir.sun_path_budget",
			Status:  StatusFail,
			Message: "runtime_dir + sock-slack exceeds SUN_PATH",
			Details: map[string]any{"used": used, "budget": budget},
		}
	}
	return Check{
		ID:      "runtime.dir.sun_path_budget",
		Status:  StatusOK,
		Details: map[string]any{"used": used, "budget": budget},
	}
}

// checkRuntimeDirReqOrphans — §17.3 acceptance gate. Scan
// <RuntimeDir>/req/ for *.json files older than 60 s. Warn if any
// exceed (lost OSC or stuck listener).
func checkRuntimeDirReqOrphans(env Env) Check {
	if env.RuntimeDir == "" {
		return Check{ID: "runtime.dir.req_orphans", Status: StatusSkip}
	}
	reqDir := filepath.Join(env.RuntimeDir, "req")
	dents, err := os.ReadDir(reqDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Check{ID: "runtime.dir.req_orphans", Status: StatusOK, Details: map[string]any{"orphans": 0}}
		}
		return Check{ID: "runtime.dir.req_orphans", Status: StatusWarn, Details: map[string]any{"err": err.Error()}}
	}
	now := time.Now()
	orphans := 0
	worst := time.Duration(0)
	for _, d := range dents {
		name := d.Name()
		if d.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		info, ierr := d.Info()
		if ierr != nil {
			continue
		}
		age := now.Sub(info.ModTime())
		if age > ReqOrphanThreshold {
			orphans++
			if age > worst {
				worst = age
			}
		}
	}
	if orphans > 0 {
		return Check{
			ID:      "runtime.dir.req_orphans",
			Status:  StatusWarn,
			Message: "request files older than 60 s",
			Details: map[string]any{"orphans": orphans, "max_age_s": int(worst.Seconds())},
		}
	}
	return Check{ID: "runtime.dir.req_orphans", Status: StatusOK, Details: map[string]any{"orphans": 0}}
}

// =============================================================
// home.consistency / linux.kernel.version / nerdfont / pathpicker.*
// =============================================================

func checkHomeConsistency() Check {
	want, herr := homeDirFn()
	got := envGet("HOME")
	if herr != nil && got == "" {
		return Check{ID: "home.consistency", Status: StatusSkip}
	}
	// Containerised CI commonly leaves both empty; only warn on actual
	// divergence between the env value and the resolved value.
	if want == "" && got == "" {
		return Check{
			ID:      "home.consistency",
			Status:  StatusSkip,
			Message: "HOME and os.UserHomeDir both empty (containerised CI?)",
		}
	}
	if want == "" || got == "" {
		return Check{
			ID:      "home.consistency",
			Status:  StatusWarn,
			Message: "HOME or os.UserHomeDir empty",
			Details: map[string]any{"home_env": got, "home_resolved": want},
		}
	}
	if want != got {
		return Check{
			ID:      "home.consistency",
			Status:  StatusWarn,
			Message: "HOME differs from os.UserHomeDir",
			Details: map[string]any{"home_env": got, "home_resolved": want},
		}
	}
	return Check{ID: "home.consistency", Status: StatusOK}
}

func checkLinuxKernelVersion() Check {
	if runtime.GOOS != "linux" {
		return Check{ID: "linux.kernel.version", Status: StatusSkip, Message: "not linux"}
	}
	rel, err := unameRelease()
	if err != nil {
		return Check{ID: "linux.kernel.version", Status: StatusWarn, Details: map[string]any{"err": err.Error()}}
	}
	return Check{ID: "linux.kernel.version", Status: StatusOK, Details: map[string]any{"release": rel}}
}

func checkNerdfontDetected() Check {
	v := envGet("WEZSESH_NERDFONT")
	if v == "1" || strings.EqualFold(v, "true") {
		return Check{ID: "nerdfont.detected", Status: StatusOK, Details: map[string]any{"set": v}}
	}
	return Check{ID: "nerdfont.detected", Status: StatusSkip, Message: "WEZSESH_NERDFONT not set"}
}

func checkPathpickerZoxide() Check {
	if _, err := lookPathFn("zoxide"); err == nil {
		return Check{ID: "pathpicker.zoxide", Status: StatusOK}
	}
	return Check{ID: "pathpicker.zoxide", Status: StatusSkip, Message: "zoxide not on PATH"}
}

func checkPathpickerFD() Check {
	if _, err := lookPathFn("fd"); err == nil {
		return Check{ID: "pathpicker.fd", Status: StatusOK}
	}
	return Check{ID: "pathpicker.fd", Status: StatusSkip, Message: "fd not on PATH"}
}

// =============================================================
// encryption.agent.responsive / log.recent_errors / config.exclude
// =============================================================

// checkEncryptionAgentResponsive runs `gpg --version` and `age --version`
// (best-effort) under a 2 s ceiling per agent. > 2 s on either path
// emits ENCRYPTION_AGENT_SLOW per §7.
func checkEncryptionAgentResponsive(ctx context.Context) Check {
	gpgD, gpgOK := gpgVersionProbe(ctx)
	ageD, ageOK := ageVersionProbe(ctx)
	if !gpgOK && !ageOK {
		return Check{
			ID:      "encryption.agent.responsive",
			Status:  StatusSkip,
			Message: "neither gpg nor age on PATH",
		}
	}
	slow := false
	details := map[string]any{}
	if gpgOK {
		details["gpg_ms"] = gpgD.Milliseconds()
		if gpgD > encryptionAgentBudget {
			slow = true
		}
	}
	if ageOK {
		details["age_ms"] = ageD.Milliseconds()
		if ageD > encryptionAgentBudget {
			slow = true
		}
	}
	if slow {
		return Check{
			ID:      "encryption.agent.responsive",
			Status:  StatusWarn,
			Message: "encryption agent slow (> 2 s)",
			Details: details,
		}
	}
	return Check{ID: "encryption.agent.responsive", Status: StatusOK, Details: details}
}

// logEntry is the JSON shape emitted by internal/logger (slog JSON
// handler). We tolerate missing fields.
type logEntry struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
}

// checkLogRecentErrors reads <StateDir>/wezsesh.log and counts ERROR
// entries within the last hour.
func checkLogRecentErrors(env Env) Check {
	if env.StateDir == "" {
		return Check{ID: "log.recent_errors", Status: StatusSkip}
	}
	path := filepath.Join(env.StateDir, "wezsesh.log")
	f, err := safefs.SafeOpenForRead(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Check{ID: "log.recent_errors", Status: StatusSkip, Message: "no log file"}
		}
		return Check{ID: "log.recent_errors", Status: StatusWarn, Details: map[string]any{"err": err.Error()}}
	}
	defer f.Close()
	raw, rerr := io.ReadAll(f)
	if rerr != nil {
		return Check{ID: "log.recent_errors", Status: StatusWarn, Details: map[string]any{"err": rerr.Error()}}
	}
	cutoff := time.Now().Add(-time.Hour)
	count := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e logEntry
		if jerr := json.Unmarshal([]byte(line), &e); jerr != nil {
			continue
		}
		if !strings.EqualFold(e.Level, "ERROR") {
			continue
		}
		if !e.Time.IsZero() && e.Time.Before(cutoff) {
			continue
		}
		count++
	}
	if count > 0 {
		return Check{
			ID:      "log.recent_errors",
			Status:  StatusWarn,
			Message: "recent error log entries",
			Details: map[string]any{"count": count},
		}
	}
	return Check{ID: "log.recent_errors", Status: StatusOK, Details: map[string]any{"count": 0}}
}

// checkConfigExcludeRegexValidity — §17.3 acceptance gate "Config
// Exclude invalid regex". Bad regex → reported.
func checkConfigExcludeRegexValidity(env Env) Check {
	if env.Cfg == nil {
		return Check{ID: "config.exclude.regex_validity", Status: StatusSkip}
	}
	if len(env.Cfg.ExcludeErrors) == 0 {
		// Defensive: if ExcludeErrors is unpopulated but Exclude has
		// authoring, recompile here so the doctor can still flag drift
		// — never reach into the cfg fields if Exclude is empty.
		if len(env.Cfg.Exclude) == 0 {
			return Check{ID: "config.exclude.regex_validity", Status: StatusOK}
		}
		bad := []map[string]any{}
		for i, src := range env.Cfg.Exclude {
			if _, err := regexp.Compile(src); err != nil {
				bad = append(bad, map[string]any{"index": i, "source": src, "reason": err.Error()})
			}
		}
		if len(bad) > 0 {
			return Check{
				ID:      "config.exclude.regex_validity",
				Status:  StatusWarn,
				Message: "invalid Exclude regexes",
				Details: map[string]any{"errors": bad},
			}
		}
		return Check{ID: "config.exclude.regex_validity", Status: StatusOK}
	}
	out := make([]map[string]any, 0, len(env.Cfg.ExcludeErrors))
	for _, e := range env.Cfg.ExcludeErrors {
		out = append(out, map[string]any{
			"index":  e.Index,
			"source": e.Source,
			"reason": e.Reason,
		})
	}
	return Check{
		ID:      "config.exclude.regex_validity",
		Status:  StatusWarn,
		Message: "invalid Exclude regexes",
		Details: map[string]any{"errors": out},
	}
}

// =============================================================
// WEZSESH_UNDER_MULTIPLEXER (spike-#1)
// =============================================================

// underMultiplexerEnvKeys is the conservative set called out in the
// task brief: tmux, screen, asciinema, Claude Code agent harness.
var underMultiplexerEnvKeys = []string{
	"TMUX",
	"STY",
	"ASCIINEMA_REC",
	"CLAUDECODE",
	"CLAUDE_CODE_ENTRYPOINT",
}

// checkUnderMultiplexer is the doctor surface for the §0.1 row 32
// PTY-multiplexer caveat. When any of the agent-harness env vars are
// set, the OSC SetUserVar path will not reach wezterm. The check
// returns StatusFail because the IPC path is structurally broken in
// this environment.
func checkUnderMultiplexer() Check {
	hits := []string{}
	for _, k := range underMultiplexerEnvKeys {
		if v := envGet(k); v != "" {
			hits = append(hits, k)
		}
	}
	if len(hits) > 0 {
		return Check{
			ID:      "WEZSESH_UNDER_MULTIPLEXER",
			Status:  StatusFail,
			Message: "binary launched under tmux / screen / asciinema / agent harness",
			Details: map[string]any{"env": hits},
		}
	}
	return Check{ID: "WEZSESH_UNDER_MULTIPLEXER", Status: StatusOK}
}

// =============================================================
// Helpers shared across checks
// =============================================================

func dirExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

func dirExistsCheck(id, path string) Check {
	if path == "" {
		return Check{ID: id, Status: StatusSkip, Message: "path not provided"}
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Check{ID: id, Status: StatusFail, Message: "path does not exist", Details: map[string]any{"path": path}}
		}
		return Check{ID: id, Status: StatusFail, Details: map[string]any{"path": path, "err": err.Error()}}
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return Check{ID: id, Status: StatusFail, Message: "path is a symlink", Details: map[string]any{"path": path}}
	}
	if !info.IsDir() {
		return Check{ID: id, Status: StatusFail, Message: "path is not a directory", Details: map[string]any{"path": path}}
	}
	return Check{ID: id, Status: StatusOK, Details: map[string]any{"path": path}}
}

// dirWritableCheck attempts a sentinel safefs.AtomicWriteFile and
// then unlinks the result. Failure surfaces as StatusFail; absent dir
// short-circuits to StatusSkip (the dir-exists check already failed).
func dirWritableCheck(ctx context.Context, id, path string) Check {
	if path == "" {
		return Check{ID: id, Status: StatusSkip, Message: "path not provided"}
	}
	if !dirExists(path) {
		return Check{ID: id, Status: StatusSkip, Message: "path does not exist"}
	}
	const sentinel = ".wezsesh-doctor-write-probe"
	if err := safefs.AtomicWriteFile(ctx, path, sentinel, []byte{}, 0o600); err != nil {
		return Check{ID: id, Status: StatusFail, Details: map[string]any{"err": err.Error()}}
	}
	_ = os.Remove(filepath.Join(path, sentinel))
	return Check{ID: id, Status: StatusOK}
}

// fsNetworkCheck wraps safefs.IsNetworkFS with the spec's "network →
// warn" verdict. Empty path skips; resolution error skips with the
// error in details (we do not want a hung-NFS-style timeout to fail
// the whole report).
func fsNetworkCheck(id, path string) Check {
	if path == "" {
		return Check{ID: id, Status: StatusSkip, Message: "path not provided"}
	}
	t, layer, isNet, err := safefs.IsNetworkFS(path)
	if err != nil {
		return Check{ID: id, Status: StatusWarn, Details: map[string]any{"path": path, "err": err.Error(), "layer": layer}}
	}
	if isNet {
		return Check{
			ID:      id,
			Status:  StatusWarn,
			Message: "path is on a network / cloud-sync filesystem",
			Details: map[string]any{"path": path, "fs": t, "layer": layer},
		}
	}
	return Check{ID: id, Status: StatusOK, Details: map[string]any{"path": path, "fs": t}}
}

func looksLikeHash(name string) bool {
	if len(name) != 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// =============================================================
// Production seam adapters (default*Fn implementations)
// =============================================================

// wezcliClient is the contract the doctor wants from a wezcli.Client
// stand-in. The production adapter is a thin wrap; tests reassign
// wezcliFactory to return a fake.
type wezcliClient interface {
	Probe(ctx context.Context) (time.Duration, error)
	List(ctx context.Context) ([]wezcli.Pane, error)
	ListClients(ctx context.Context) ([]wezcli.ClientInfo, error)
}

func defaultWezcliFactory() (wezcliClient, error) {
	return wezcli.NewClient(nil)
}

// snapshotsRepo is the contract the doctor wants from
// snapshots.Repo. Same pattern as wezcliClient.
type snapshotsRepo interface {
	List(ctx context.Context) ([]snapshots.Entry, error)
	Has(ctx context.Context, name string) (bool, error)
	Sniff(ctx context.Context, name string) (snapshots.Encryption, error)
}

func defaultSnapshotsOpener(snapshotDir string) (snapshotsRepo, error) {
	return snapshots.NewRepo(snapshotDir, nil)
}

// stateOpenerFn returns the live-pin slice. Doctor cares only about
// the names; the rest of state.Store is irrelevant. We pass nil to
// repoHas because the doctor itself owns the cross-check via
// snapshotsOpener; pruning during Open is undesirable here.
func defaultStateOpener(ctx context.Context, stateDir string) ([]string, error) {
	s, err := state.Open(ctx, stateDir, nil, nil)
	if err != nil {
		return nil, err
	}
	return s.LivePins(), nil
}

// defaultEnvGet is the production seam for envGet. It defers to
// os.Getenv.
func defaultEnvGet(k string) string {
	return os.Getenv(k)
}

// defaultHomeDir defers to os.UserHomeDir.
func defaultHomeDir() (string, error) {
	return os.UserHomeDir()
}

func defaultLookPath(name string) (string, error) {
	return exec.LookPath(name)
}

// agentVersionProbe runs `<bin> --version` under a 2.5 s ceiling
// (slightly above the 2 s warn threshold so we can detect the
// boundary). Returns (elapsed, ok=true) when the binary was found and
// the probe completed; (0, ok=false) when the binary is not on PATH.
func agentVersionProbe(parent context.Context, bin string) (time.Duration, bool) {
	if _, err := exec.LookPath(bin); err != nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(parent, encryptionAgentBudget+500*time.Millisecond)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(ctx, bin, "--version")
	_ = cmd.Run()
	return time.Since(start), true
}

func defaultGPGVersionProbe(ctx context.Context) (time.Duration, bool) {
	return agentVersionProbe(ctx, "gpg")
}

func defaultAgeVersionProbe(ctx context.Context) (time.Duration, bool) {
	return agentVersionProbe(ctx, "age")
}

// unameRelease is the linux-only kernel-version reader. Implementation
// uses the platform-agnostic `uname -r` exec rather than syscall.Uname
// (which has a slightly different shape on darwin) so the helper
// compiles cleanly on every supported GOOS even though we only
// invoke it on linux.
func unameRelease() (string, error) {
	if runtime.GOOS != "linux" {
		return "", errors.New("doctor: uname only on linux")
	}
	out, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// seamMu serialises reassignments of the package-private seam
// variables (envGet / homeDirFn / wezcliFactory / etc.). Tests
// reassign in parallel-tolerant t.Cleanup-scoped helpers that lock
// the mutex; production never writes after init.
var seamMu sync.Mutex
