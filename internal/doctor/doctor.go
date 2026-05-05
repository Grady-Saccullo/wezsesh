// Package doctor implements the `wezsesh doctor` health-check surface
// (§8.17). Run iterates through a fixed list of small per-check functions
// and aggregates the results into a Report.
//
// The doctor is read-leaning but not strictly read-only. Two checks
// touch the disk:
//
//   - The *.dir.writable family (snapshot/state/data) writes a sentinel
//     via safefs.AtomicWriteFile and removes it. Required to assert
//     that a directory is actually writable, not merely present.
//   - snapshot.pin.consistency calls state.Open via the stateOpener
//     seam. state.Open transparently migrates v1 state.json to the
//     current schema, which writes <dir>/state.json.v<N>.bak and
//     re-persists state.json. Migration is idempotent and one-shot;
//     subsequent doctor runs are pure reads.
//
// Every other check stats, reads, execs a probe binary (gpg/age
// --version), or inspects process env. Disk writes outside the two
// flows above MUST flow through safefs.AtomicWriteFile per §16.5.1.
//
// Test seam: the wezcli client and the state/snapshots openers are
// abstracted behind small package-private interfaces (see seams.go-style
// vars at the bottom of this file) so unit tests can drive every check
// without touching wezterm or the network.
package doctor

import (
	"context"

	"github.com/Grady-Saccullo/wezsesh/internal/config"
)

// Env is the input to Run. DataDir and TrustDir are first-class fields
// here — the caller (cmd/wezsesh) resolves them from the §11.4 chain
// and hands them to the doctor; doctor never touches config.Config for
// either path.
type Env struct {
	BinaryPath    string
	PluginVersion string
	SnapshotDir   string
	StateDir      string
	RuntimeDir    string
	DataDir       string
	TrustDir      string
	Cfg           *config.Config
}

// Report is the aggregated result of Run.
//
// Critical is true iff at least one check returned StatusFail.
// Warnings is true iff at least one check returned StatusWarn (and
// is informational only — it does not imply Critical).
//
// Per §8.20 (`wezsesh doctor [--format json]   → exit 0 on all-OK,
// !=0 otherwise`) the CLI exits non-zero when either Critical or
// Warnings is true.
type Report struct {
	Checks   []Check
	Critical bool
	Warnings bool
}

// Check is one row of the doctor table.
type Check struct {
	ID      string
	Status  Status
	Message string
	Details map[string]any
}

// Status is the per-check verdict. The four values match §8.17 exactly.
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
	StatusSkip Status = "skip"
)

// Run executes every check in §8.17.1 and returns a Report. The order
// of checks below mirrors §8.17.1 top-to-bottom so visual diffing the
// JSON output against the spec is trivial.
func Run(ctx context.Context, env Env) Report {
	checks := []Check{
		checkBinaryVersion(env),
		checkBinaryPath(env),
		checkBinaryFSNetwork(env),
		checkPluginVersion(env),
		checkVersionCompatible(env),
		checkWeztermVersion(ctx, env),
		checkWeztermLuaVersion(ctx, env),
		checkWeztermCLIList(ctx, env),
		checkWeztermCLIListClients(ctx, env),
		checkWeztermCLITTYName(ctx, env),
		checkWeztermPaneEnv(ctx, env),
		checkSnapshotDirExists(env),
		checkSnapshotDirWritable(ctx, env),
		checkSnapshotDirFSNetwork(env),
		checkSnapshotDirMatchesResurrect(env),
		checkSnapshotCount(ctx, env),
		checkSnapshotNameValidation(ctx, env),
		checkSnapshotArgvAllowlistCoverage(ctx, env),
		checkSnapshotEncryptionDetected(ctx, env),
		checkSnapshotPinConsistency(ctx, env),
		checkStateDirExists(env),
		checkStateDirWritable(ctx, env),
		checkStateDirFSNetwork(env),
		checkDataDirExists(env),
		checkDataDirWritable(ctx, env),
		checkDataDirFSNetwork(env),
		checkTrustDirExists(env),
		checkTrustCount(env),
		checkTrustOrphans(env),
		checkRuntimeDirExists(env),
		checkRuntimeDirFSNetwork(env),
		checkRuntimeDirPermissions(env),
		checkRuntimeDirSunPathBudget(env),
		checkRuntimeDirReqOrphans(env),
		checkHomeConsistency(),
		checkLinuxKernelVersion(),
		checkNerdfontDetected(),
		checkPathpickerZoxide(),
		checkPathpickerFD(),
		checkEncryptionAgentResponsive(ctx),
		checkLogRecentErrors(env),
		checkConfigExcludeRegexValidity(env),
		checkUnderMultiplexer(),
	}

	r := Report{Checks: checks}
	for _, c := range checks {
		switch c.Status {
		case StatusFail:
			r.Critical = true
		case StatusWarn:
			r.Warnings = true
		}
	}
	return r
}

// --- Test seams. All seam variables are package-private and default to
// the production implementations. Tests reassign them in t.Cleanup-
// scoped helpers in doctor_test.go.

// envGet is the source of process env vars consulted by the
// `WEZSESH_UNDER_MULTIPLEXER`, `nerdfont.detected`, `home.consistency`,
// and `wezterm.pane.env` checks. Tests inject a controlled map via
// applySeams in doctor_test.go.
var envGet = defaultEnvGet

// homeDirFn returns the user's home directory; replaced in tests for
// the `home.consistency` check.
var homeDirFn = defaultHomeDir

// wezcliFactory builds the wezcli client used by the wezterm.* checks.
// Tests inject a fake by reassigning this variable.
var wezcliFactory = defaultWezcliFactory

// stateOpener loads live pins from the state package. Tests override
// this so the live-pin check can be exercised without a real
// state.Open call.
var stateOpener = defaultStateOpener

// snapshotsOpener provides snapshot listing + per-name presence checks.
// Tests override this for the snapshot.* checks.
var snapshotsOpener = defaultSnapshotsOpener

// gpgVersionProbe runs `gpg --version` (or equivalent) and returns the
// elapsed wall time. Tests inject a fake to drive the slow-path warn.
var gpgVersionProbe = defaultGPGVersionProbe

// ageVersionProbe is the age sibling of gpgVersionProbe.
var ageVersionProbe = defaultAgeVersionProbe

// lookPathFn is the resolver behind pathpicker.zoxide / pathpicker.fd.
// Tests inject a fake when they need to assert binary-absence handling.
var lookPathFn = defaultLookPath
