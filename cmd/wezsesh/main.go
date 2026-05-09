// Package main is the wezsesh binary entry point. The startup sequence
// follows §8.20.1 step by step; subcommand routing follows §8.20. Per-
// subcommand bodies (doctor / list / find / trust / reset / keygen /
// reply / version) live alongside this file (T-801..T-808).
//
// Hard invariants encoded here (CLAUDE.md):
//   - WEZTERM_PANE + WEZSESH_HMAC_KEY validation precede every other
//     piece of TUI setup; we cannot dispatch without them.
//   - safefs.Enforce(SymlinkRefuse) on every wezsesh-managed dir
//     (snapshot, state, runtime). A symlinked top-level dir is a hard
//     refuse-class condition.
//   - Hook env scrub (§13.5.1) drops the three sensitive vars
//     (WEZSESH_HMAC_KEY / WEZSESH_PROTO_VERSION /
//     WEZSESH_CONFIG_JSON_BASE64) and preserves WEZSESH_LOG so user log
//     preferences survive into the hook child. Implemented in
//     scrubHookEnv.
//   - Top-level `defer recover()` writes UNEXPECTED_EXIT on the panic
//     path then os.Exit(2). Out-of-recover crashes (SIGSEGV / SIGKILL /
//     OOM-killer) fall through to IPC_TIMEOUT — acceptable per §13.1.
//   - The save flow (§13.4) is exposed via the runSave helper so the
//     §17.3 Phase A/B/C E2E gates can be exercised without a live TUI.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Grady-Saccullo/wezsesh/internal/config"
	whmac "github.com/Grady-Saccullo/wezsesh/internal/hmac"
	"github.com/Grady-Saccullo/wezsesh/internal/ipc"
	"github.com/Grady-Saccullo/wezsesh/internal/ipcdispatcher"
	"github.com/Grady-Saccullo/wezsesh/internal/ipcsock"
	"github.com/Grady-Saccullo/wezsesh/internal/logger"
	"github.com/Grady-Saccullo/wezsesh/internal/reqsweep"
	"github.com/Grady-Saccullo/wezsesh/internal/safefs"
	"github.com/Grady-Saccullo/wezsesh/internal/snapshots"
	"github.com/Grady-Saccullo/wezsesh/internal/state"
	"github.com/Grady-Saccullo/wezsesh/internal/trust"
	"github.com/Grady-Saccullo/wezsesh/internal/tui"
	"github.com/Grady-Saccullo/wezsesh/internal/uservar"
	"github.com/Grady-Saccullo/wezsesh/internal/wezcli"
)

// Exit codes — §13.14 / §17.3.
const (
	exitOK             = 0
	exitDoctorOrSubcmd = 2 // doctor / list / find / trust / reset / reply
	exitKeygen         = 3 // keygen — Lua falls through §5.2 step 2
	exitUnexpected     = 2 // TUI panic path (§13.1)
	exitInitFailed     = 2 // IPC_INIT_FAILED bucket (§13.9, §6.x)
)

// hexKey64 is the §5.2 / §11.3 64-lowercase-hex shape WEZSESH_HMAC_KEY
// must satisfy. Centralised so the validate_hmac_key gate and the
// keygen subcommand share the same regex.
var hexKey64 = regexp.MustCompile(`^[a-f0-9]{64}$`)

// errMissingPaneID surfaces when WEZTERM_PANE is unset and no
// --pane-id override is provided on the TUI path.
var errMissingPaneID = errors.New("WEZTERM_PANE not set; pass --pane-id <int> when running outside wezterm")

// errBadHMACKey is the validate-hmac-key gate on the TUI path.
var errBadHMACKey = errors.New("WEZSESH_HMAC_KEY is not 64 lowercase hex chars")

// errMissingRuntimeDir surfaces when WEZSESH_RUNTIME_DIR is unset on
// the TUI path. The plugin sets it explicitly at spawn so the binary
// can reach the IPC socket dir BEFORE the bootstrap-verb roundtrip
// lands the rest of the config.
var errMissingRuntimeDir = errors.New("WEZSESH_RUNTIME_DIR not set; the TUI requires the plugin to set it at spawn")

// bootstrapFetcher is the seam tests use to bypass the live
// IPC-bootstrap roundtrip. Production wires `defaultBootstrapFetch`,
// which calls `ipc.AwaitTerminal` against the real dispatcher; tests
// substitute a fake that returns a synthesised *config.Config.
type bootstrapFetcher func(ctx context.Context, disp ipc.Dispatcher) (*config.Config, error)

// defaultBootstrapFetch is the production bootstrap fetch. Calls the
// `bootstrap` IPC verb, validates the reply, and decodes the data
// map into a *config.Config via config.LoadFromBootstrapData.
func defaultBootstrapFetch(ctx context.Context, disp ipc.Dispatcher) (*config.Config, error) {
	reply, err := ipc.AwaitTerminal(ctx, disp, "bootstrap", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("bootstrap: %w", err)
	}
	if !reply.OK {
		if reply.Error != nil {
			return nil, fmt.Errorf("bootstrap: %s: %s", reply.Error.Code, reply.Error.Message)
		}
		return nil, errors.New("bootstrap: ok=false with no error object")
	}
	cfg, err := config.LoadFromBootstrapData(ctx, reply.Data)
	if err != nil {
		return nil, fmt.Errorf("bootstrap decode: %w", err)
	}
	return cfg, nil
}

// hookEnvScrub is the §13.5.1 set of env var names dropped before
// invoking any user hook. Post-bootstrap-cutover the only sensitive
// var is WEZSESH_HMAC_KEY; WEZSESH_RUNTIME_DIR is a path (not a
// secret) and WEZSESH_PLUGIN_VERSION is plain metadata. WEZSESH_LOG /
// WEZSESH_NO_HOOKS / WEZSESH_NERDFONT survive intentionally (§17.3
// row "Hook env: WEZSESH_LOG survives").
var hookEnvScrub = []string{
	"WEZSESH_HMAC_KEY",
}

// runtimeEnv bundles every dependency main.go's helpers consume.
// Tests build a stub runtimeEnv directly; production main() builds it
// from the live env via tuiSetup.
type runtimeEnv struct {
	cfg     *config.Config
	log     *logger.Logger
	repo    *snapshots.Repo
	store   *state.Store
	trust   *trust.Store
	wezcli  *wezcli.Client
	disp    ipc.Dispatcher
	dispWG  func()
	cleanup func()
	// paneID resolves from $WEZTERM_PANE or --pane-id. Captured so the
	// TUI can render in the correct window context.
	paneID int
	// windowID is the wezterm window the paneID lives inside, resolved
	// via wcli.List during tuiSetup. The dispatcher stamps this on every
	// §3.3 payload as `target_window_id` (NOT the pane id). The plugin's
	// §9.3.1 step (g) window-match check rejects requests whose
	// target_window_id does not match the window the plugin sees as
	// active, so a stale or pane-id-shaped value causes every dispatch
	// to time out against a real wezterm.
	windowID int
	// initialData is the §8.20.1 sub-step (8) picker payload assembled
	// before dispatcher init so the canonical step order (8) → (9) → (10)
	// holds. Stored on the env so runTUI hands it straight to tui.New.
	initialData tui.Data
}

// runTUIPanicHook is a test seam for the §13.1 panic-recover branch.
// When non-nil, runTUI invokes it inside the recover-protected region,
// before any setup work, so tests can drive the real recover body
// instead of inlining a copy. Production keeps the hook nil.
var runTUIPanicHook func()

func main() {
	// Mint a per-process ULID for use as the binary_session_id on every
	// log record (CLAUDE.md "Tracing & correlation"). Done before any
	// subcommand dispatch so even keygen / doctor / tail records carry
	// the id. Re-uses the dispatcher's existing ULID encoder rather
	// than pulling in github.com/oklog/ulid for one value (§3.3 ULID
	// shape is the same).
	binarySessionID, _, err := ipcdispatcher.NewULID()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wezsesh: ulid: %v\n", err)
		os.Exit(exitInitFailed)
	}
	// Propagate to child processes (e.g. wezsesh reply spawned by the
	// plugin) so they can stamp the same id into their own records.
	// Setenv on the process, not Setenv on a per-cmd Env: the binary's
	// own subcommand handlers and any goroutines they spawn share this
	// process env, and downstream exec.Cmd defaults to os.Environ().
	if err := os.Setenv("WEZSESH_BINARY_SESSION_ID", binarySessionID); err != nil {
		fmt.Fprintf(os.Stderr, "wezsesh: setenv: %v\n", err)
		os.Exit(exitInitFailed)
	}

	args := os.Args[1:]
	code := run(args, os.Stdout, os.Stderr, binarySessionID)
	os.Exit(code)
}

// run is the testable entry point: pure on os.Args, returns the
// requested exit code. main() wraps it with os.Exit.
//
// binarySessionID is the per-process ULID minted in main(); production
// callers pass the value, tests synthesise their own. It is threaded
// through to logger.New and ipcdispatcher.Deps so every Go-side log
// record and every outgoing request envelope carries it.
//
// The top-level recover for the TUI path is installed inside runTUI;
// non-TUI subcommands have their own §13.14 panic handlers (each
// subcommand body installs its own when it lands).
func run(args []string, stdout, stderr io.Writer, binarySessionID string) int {
	flags, sub, rest, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh: %v\n", err)
		return exitDoctorOrSubcmd
	}

	if flags.version {
		fmt.Fprintf(stdout, "wezsesh %s\n", version)
		return exitOK
	}

	switch sub {
	case "":
		return runTUI(flags, stdout, stderr, binarySessionID)
	case "keygen":
		return subcmdKeygen(rest, stdout, stderr)
	case "reply":
		return subcmdReply(rest, stdout, stderr)
	case "doctor":
		return subcmdDoctor(rest, stdout, stderr)
	case "list":
		return subcmdList(rest, stdout, stderr)
	case "find":
		return subcmdFind(rest, stdout, stderr)
	case "trust":
		return subcmdTrust(rest, stdout, stderr)
	case "reset":
		return subcmdReset("reset", rest, stdout, stderr)
	case "nuke":
		// §8.20 / §0.1 row 8: deprecated alias for reset; the toast is
		// emitted from subcmdReset when invokedAs == "nuke".
		return subcmdReset("nuke", rest, stdout, stderr)
	case "tail":
		return subcmdTail(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "wezsesh: unknown subcommand %q\n", sub)
		return exitDoctorOrSubcmd
	}
}

// parsedFlags holds the top-level flags shared across subcommands.
type parsedFlags struct {
	version bool
	paneID  int // 0 when unset; --pane-id override.
}

// parseArgs splits the argv vector into top-level flags + subcommand +
// remaining args. The flag.FlagSet is local so tests can call run()
// repeatedly without the global flag.CommandLine state leaking between
// invocations.
func parseArgs(args []string) (parsedFlags, string, []string, error) {
	fs := flag.NewFlagSet("wezsesh", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var p parsedFlags
	fs.BoolVar(&p.version, "version", false, "print version and exit")
	fs.IntVar(&p.paneID, "pane-id", 0, "override $WEZTERM_PANE (test/CI)")
	if err := fs.Parse(args); err != nil {
		return p, "", nil, err
	}
	tail := fs.Args()
	if len(tail) == 0 {
		return p, "", nil, nil
	}
	return p, tail[0], tail[1:], nil
}

// runTUI executes the §8.20.1 step-4 startup sequence and hands control
// to bubbletea. The top-level defer recover() is registered first so it
// catches every panic in the setup tail.
func runTUI(flags parsedFlags, _, stderr io.Writer, binarySessionID string) (rc int) {
	rc = exitOK
	// §8.20.1 step 5 / §13.1 panic path: top-level recover. The
	// dispatcher reference is captured into a closure so a panic in
	// tuiSetup itself (env still nil) degrades to log+exit, while a
	// panic anywhere downstream fans out the UNEXPECTED_EXIT sentinel
	// to every outstanding reply socket via env.disp.EmergencyReply().
	var disp ipc.Dispatcher
	defer func() {
		if r := recover(); r != nil {
			if disp != nil {
				disp.EmergencyReply()
			}
			fmt.Fprintf(stderr, "wezsesh: panic: %v\n", r)
			rc = exitUnexpected
		}
	}()

	if hook := runTUIPanicHook; hook != nil {
		hook()
	}

	env, err := tuiSetup(flags, os.Getenv, binarySessionID, defaultBootstrapFetch)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh: %v\n", err)
		// SUN_PATH overflow + every other init failure share the
		// IPC_INIT_FAILED bucket per §13.9 / §6.
		return exitInitFailed
	}
	defer env.cleanup()
	disp = env.disp

	model := tui.New(buildTUIConfig(env.cfg), env.initialData, env.disp, tui.WithLogger(env.log))
	prog := tea.NewProgram(model)

	// §8.7 / §12.4 / §14.2: SIGINT / SIGTERM / SIGHUP must drive the
	// same cleanup path as a normal Ctrl-C exit, so reply sockets and
	// <runtime_dir>/req/ entries are not left as orphans for the §12.4
	// startup-sweep to reap. The handler forwards a tea.Quit so
	// program.Run unwinds cleanly; defer env.cleanup() still fires.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)
	sigDone := make(chan struct{})
	go func() {
		defer close(sigDone)
		// Top-level defer recover() — CLAUDE.md TUI-discipline
		// invariant + §14.2 hygiene. The body is small but the
		// recover keeps any future addition (e.g., cleanup-side
		// EmergencyReply fan-out) from killing the binary if it
		// ever panics.
		defer func() {
			if r := recover(); r != nil && env.log != nil {
				env.log.Error("signal goroutine panic", "err", fmt.Sprint(r))
			}
		}()
		sig, ok := <-sigCh
		if !ok {
			return
		}
		env.log.Warn("signal received; quitting", "signal", sig.String())
		prog.Quit()
	}()

	finalModel, err := prog.Run()
	if err != nil {
		env.log.Error("tea.Run failed", "err", err.Error())
		signal.Stop(sigCh)
		close(sigCh)
		<-sigDone
		return exitUnexpected
	}
	// Detach the signal goroutine: signal.Stop above prevents new
	// notifications, closing the channel unblocks the receive on the
	// goroutine side.
	signal.Stop(sigCh)
	close(sigCh)
	<-sigDone
	// §8.20.1 step 12: drain any deferred-phase replies. dispWG is the
	// dispatcher's WaitGroup; cleanup runs after we've drained.
	if env.dispWG != nil {
		env.dispWG()
	}
	// The TUI is one-shot: the spawning tab serves no purpose once the
	// picker has closed. When the user runs wezterm with the default
	// `exit_behavior = "Hold"`, the tab would otherwise linger as a
	// "Process completed" placeholder. The model stamps closeOwnPane
	// on every clean tea.Quit (verb auto-exit, manual q, mid-op y);
	// the panic-recovery defer above leaves it false so the user can
	// read the failure message. Fire-and-forget — wezterm's CLI is
	// asynchronous from our perspective; the SIGHUP that follows the
	// pane close arrives after env.cleanup() in deferred order.
	if cm, ok := finalModel.(interface{ CloseOwnPaneOnExit() bool }); ok && cm.CloseOwnPaneOnExit() && env.wezcli != nil && env.paneID > 0 {
		killCtx, killCancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := env.wezcli.KillPane(killCtx, env.paneID); err != nil {
			env.log.Warn("kill-pane after exit failed", "err", err.Error())
		}
		killCancel()
	}
	return rc
}

// tuiSetup realises §8.20.1 step 4 substeps (1)–(11). Returned
// runtimeEnv wraps every constructed dependency plus a cleanup closure
// the caller defers.
//
// binarySessionID is threaded into logger.New so every log record
// carries it and into the dispatcher's Deps so every outgoing request
// envelope carries it.
//
// fetchBootstrap is the IPC-side config-fetch seam: production wires
// `defaultBootstrapFetch`; tests substitute a fake to bypass the live
// IPC roundtrip. Must not be nil — callers always provide a fetcher.
//
// Phase order (post-bootstrap rework):
//
//	(1)  WEZTERM_PANE + WEZSESH_HMAC_KEY + WEZSESH_RUNTIME_DIR
//	(2)  Resolve windowID via wezcli.List (no cfg needed)
//	(3)  uservar Writer + HMAC signer
//	(4)  Bootstrap-time logger (writes to autodetect StateDir at
//	     ResolveLevel("info", $WEZSESH_LOG)). Used by the dispatcher
//	     for its own warn/error path during the bootstrap window.
//	(5)  Construct the dispatcher with the real windowID
//	(6)  fetchBootstrap → *config.Config
//	(7)  Re-create logger with cfg.StateDir + ResolveLevel(cfg.LogLevel,
//	     $WEZSESH_LOG); the bootstrap-time logger is closed.
//	(8)  Sweeps
//	(9)  symlink-refuse all dirs + mkdir-enforce data/allow
//	(10) snapshots / state / trust open
//	(11) buildTUIData
func tuiSetup(flags parsedFlags, getEnv func(string) string, binarySessionID string, fetchBootstrap bootstrapFetcher) (*runtimeEnv, error) {
	if fetchBootstrap == nil {
		return nil, errors.New("tuiSetup: fetchBootstrap is nil")
	}

	// Sub-step (1): WEZTERM_PANE + HMAC key + runtime_dir validation.
	paneID, err := resolvePaneID(flags, getEnv)
	if err != nil {
		return nil, err
	}
	hexKey := strings.TrimSpace(getEnv("WEZSESH_HMAC_KEY"))
	if !hexKey64.MatchString(hexKey) {
		return nil, errBadHMACKey
	}
	runtimeDir := getEnv("WEZSESH_RUNTIME_DIR")
	if runtimeDir == "" {
		return nil, errMissingRuntimeDir
	}
	ctx, cancelCtx := context.WithCancel(context.Background())

	// Sub-step (2): signer + uservar Writer. wezcli + windowID
	// resolution is deferred to sub-step (10) so the symlink-refuse
	// gate in sub-step (8) runs BEFORE the wezterm CLI subprocess —
	// otherwise an attacker-planted symlink at a managed dir would be
	// observed only after a successful wezcli call, which is
	// unfriendly to test environments without wezterm available.
	signer, err := whmac.NewSigner(hexKey)
	if err != nil {
		cancelCtx()
		return nil, fmt.Errorf("hmac: %w", err)
	}
	uvw, err := uservar.New()
	if err != nil {
		cancelCtx()
		return nil, fmt.Errorf("uservar: %w", err)
	}

	// Sub-step (4): bootstrap-time logger. Writes to the autodetected
	// StateDir at level=ResolveLevel("info", $WEZSESH_LOG). Used by
	// the dispatcher during the bootstrap window; replaced after
	// bootstrap with a logger pointing at cfg.StateDir.
	autoCfg, err := config.AutoDetect()
	if err != nil {
		_ = uvw.Close()
		cancelCtx()
		return nil, fmt.Errorf("autodetect: %w", err)
	}
	bootLevel := logger.ResolveLevel("info", getEnv("WEZSESH_LOG"))
	bootLog, err := logger.New(autoCfg.StateDir, bootLevel, binarySessionID)
	if err != nil {
		_ = uvw.Close()
		cancelCtx()
		return nil, fmt.Errorf("bootstrap logger: %w", err)
	}

	// Sub-step (5): dispatcher. TargetWindowID starts at the §3.3
	// "any window" sentinel (-1); the plugin accepts -1 for the
	// bootstrap dispatch. SetTargetWindowID is called in sub-step
	// (10) once wcli.List resolves the real id.
	disp, dispCleanup, err := ipcdispatcher.New(ipcdispatcher.Deps{
		Writer:          uvw,
		Signer:          signer,
		RuntimeDir:      runtimeDir,
		TargetWindowID:  -1,
		Logger:          bootLog,
		BinarySessionID: binarySessionID,
	})
	if err != nil {
		_ = bootLog.Close()
		_ = uvw.Close()
		cancelCtx()
		return nil, fmt.Errorf("ipc init: %w", err)
	}

	// Sub-step (6): bootstrap fetch. The bootstrap dispatch has its
	// own deadline so a slow / wedged plugin doesn't leave the binary
	// hung indefinitely; aligns with the existing 30 s reply budget.
	bootCtx, bootCancel := context.WithTimeout(ctx, 10*time.Second)
	cfg, err := fetchBootstrap(bootCtx, disp)
	bootCancel()
	if err != nil {
		if dispCleanup != nil {
			dispCleanup()
		}
		_ = bootLog.Close()
		_ = uvw.Close()
		cancelCtx()
		return nil, fmt.Errorf("bootstrap: %w", err)
	}
	if cfg.RuntimeDir != runtimeDir {
		// Defensive: the plugin sets WEZSESH_RUNTIME_DIR and
		// opts.runtime_dir from the same source, so they must agree.
		// A mismatch would mean the dispatcher's req/ + reply socket
		// dir live somewhere different from where the rest of the
		// binary looks. Fail loudly rather than silently splitting.
		if dispCleanup != nil {
			dispCleanup()
		}
		_ = bootLog.Close()
		_ = uvw.Close()
		cancelCtx()
		return nil, fmt.Errorf("bootstrap: WEZSESH_RUNTIME_DIR=%q disagrees with bootstrap cfg.RuntimeDir=%q", runtimeDir, cfg.RuntimeDir)
	}

	// Sub-step (7): real logger. Closes the bootstrap-time logger.
	level := logger.ResolveLevel(cfg.LogLevel, getEnv("WEZSESH_LOG"))
	log, err := logger.New(cfg.StateDir, level, binarySessionID)
	if err != nil {
		if dispCleanup != nil {
			dispCleanup()
		}
		_ = bootLog.Close()
		_ = uvw.Close()
		cancelCtx()
		return nil, fmt.Errorf("logger: %w", err)
	}
	_ = bootLog.Close()

	// Sub-step (8): sweeps + plugin.log rotation.
	if err := ipcsock.SweepStale(cfg.RuntimeDir, log); err != nil {
		log.Warn("ipcsock sweep failed", "err", err.Error())
	}
	if err := reqsweep.SweepStale(filepath.Join(cfg.RuntimeDir, "req"), log); err != nil {
		log.Warn("reqsweep failed", "err", err.Error())
	}
	if err := safefs.RotateSingleDeep(cfg.StateDir, "plugin.log", 1<<20); err != nil {
		log.Warn("plugin.log rotate failed", "err", err.Error())
	}
	// Log level no longer flows through a sidecar — the Lua plugin
	// resolves it locally at apply_to_config via the same algorithm
	// (`logger.ResolveLevel(opts.log_level, $WEZSESH_LOG)`) so the
	// two sides agree without disk coordination. Both see the same
	// parent env at wezterm-launch time.

	// Sub-step (9): symlink-refuse all top-level dirs (runtime/req
	// already enforced by ipcdispatcher.New). data/allow gets the
	// MkdirEnforce treatment.
	for _, d := range []string{cfg.SnapshotDir, cfg.StateDir, cfg.RuntimeDir, cfg.DataDir} {
		if d == "" {
			continue
		}
		if ok, err := safefs.Enforce(d, safefs.SymlinkRefuse, log); err != nil {
			_ = log.Close()
			if dispCleanup != nil {
				dispCleanup()
			}
			_ = uvw.Close()
			cancelCtx()
			return nil, fmt.Errorf("safefs enforce %s: %w", d, err)
		} else if !ok {
			_ = log.Close()
			if dispCleanup != nil {
				dispCleanup()
			}
			_ = uvw.Close()
			cancelCtx()
			return nil, fmt.Errorf("safefs enforce %s: refusing symlink", d)
		}
	}
	if cfg.DataDir != "" {
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			_ = log.Close()
			if dispCleanup != nil {
				dispCleanup()
			}
			_ = uvw.Close()
			cancelCtx()
			return nil, fmt.Errorf("mkdir %s: %w", cfg.DataDir, err)
		}
		if err := safefs.MkdirEnforce(cfg.DataDir, "allow", 0o700); err != nil {
			_ = log.Close()
			if dispCleanup != nil {
				dispCleanup()
			}
			_ = uvw.Close()
			cancelCtx()
			return nil, fmt.Errorf("safefs enforce %s/allow: %w", cfg.DataDir, err)
		}
	}

	// Sub-step (10): wezcli + List + windowID resolution. Deferred
	// from earlier (see sub-step (2) comment) so the symlink-refuse
	// gate in sub-step (9) runs first. Once windowID is resolved,
	// swap it onto the dispatcher so subsequent dispatches stamp
	// the real wezterm window id (the bootstrap dispatch already
	// completed using the -1 sentinel).
	wcli, err := wezcli.NewClient(log)
	if err != nil {
		_ = log.Close()
		if dispCleanup != nil {
			dispCleanup()
		}
		_ = uvw.Close()
		cancelCtx()
		return nil, fmt.Errorf("wezcli: %w", err)
	}
	panes, err := wcli.List(ctx)
	if err != nil {
		_ = log.Close()
		if dispCleanup != nil {
			dispCleanup()
		}
		_ = uvw.Close()
		cancelCtx()
		return nil, fmt.Errorf("wezcli list: %w", err)
	}
	windowID, err := resolveWindowID(panes, paneID)
	if err != nil {
		_ = log.Close()
		if dispCleanup != nil {
			dispCleanup()
		}
		_ = uvw.Close()
		cancelCtx()
		return nil, fmt.Errorf("resolve window id: %w", err)
	}
	if d, ok := disp.(*ipcdispatcher.Dispatcher); ok {
		d.SetTargetWindowID(windowID)
	}

	// Sub-step (11): snapshots / state / trust.
	repo, err := snapshots.NewRepo(cfg.SnapshotDir, log)
	if err != nil {
		_ = log.Close()
		if dispCleanup != nil {
			dispCleanup()
		}
		_ = uvw.Close()
		cancelCtx()
		return nil, fmt.Errorf("snapshots: %w", err)
	}
	repoHas := func(name string) bool {
		ok, _ := repo.Has(ctx, name)
		return ok
	}
	store, err := state.Open(ctx, cfg.StateDir, log, repoHas)
	if err != nil {
		_ = log.Close()
		if dispCleanup != nil {
			dispCleanup()
		}
		_ = uvw.Close()
		cancelCtx()
		return nil, fmt.Errorf("state: %w", err)
	}
	trustStore, err := trust.Open(ctx, cfg.TrustDir, log)
	if err != nil {
		_ = log.Close()
		if dispCleanup != nil {
			dispCleanup()
		}
		_ = uvw.Close()
		cancelCtx()
		return nil, fmt.Errorf("trust: %w", err)
	}

	// Sub-step (12): initial TUI Data.
	initialData := buildTUIData(ctx, store, repo, panes, paneID, log)
	// DirProviders flow via cfg from the bootstrap reply; the TUI's
	// Init() fires a tea.Cmd that runs dirproviders.RunAll on these
	// configs and merges the resulting rows into the picker. Empty /
	// nil disables the path.
	initialData.DirProviders = cfg.DirProviders

	wgFn := func() {}
	if d, ok := disp.(*ipcdispatcher.Dispatcher); ok {
		wgFn = d.Wait
	}

	cleanup := func() {
		if dispCleanup != nil {
			dispCleanup()
		}
		_ = uvw.Close()
		_ = log.Close()
		cancelCtx()
	}
	_ = trustStore // T-803 wires the trust store into the TUI Config.

	return &runtimeEnv{
		cfg:         cfg,
		log:         log,
		repo:        repo,
		store:       store,
		trust:       trustStore,
		wezcli:      wcli,
		disp:        disp,
		dispWG:      wgFn,
		cleanup:     cleanup,
		paneID:      paneID,
		windowID:    windowID,
		initialData: initialData,
	}, nil
}

// resolvePaneID picks --pane-id when set; otherwise falls back to
// $WEZTERM_PANE. Returns errMissingPaneID when both are absent. The
// resulting pane id is mapped to a window id by resolveWindowID
// before the dispatcher is constructed.
func resolvePaneID(flags parsedFlags, getEnv func(string) string) (int, error) {
	if flags.paneID > 0 {
		return flags.paneID, nil
	}
	if v := getEnv("WEZTERM_PANE"); v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, fmt.Errorf("WEZTERM_PANE %q: %w", v, err)
		}
		if n <= 0 {
			return 0, fmt.Errorf("WEZTERM_PANE %q: must be positive", v)
		}
		return n, nil
	}
	return 0, errMissingPaneID
}

// resolveWindowID maps the active paneID to the wezterm window id by
// scanning the wezcli.List output. Spec §3.3 + plugin §9.3.1 step (g):
// `target_window_id` carries the WINDOW id, never the pane id; passing
// the pane id makes every dispatch fail the plugin's window-match
// check and surface as IPC_TIMEOUT to the TUI. Pulled out as a free
// function so the lookup stays unit-testable independently of wcli.
func resolveWindowID(panes []wezcli.Pane, paneID int) (int, error) {
	for _, p := range panes {
		if p.PaneID == paneID {
			return p.WindowID, nil
		}
	}
	return 0, fmt.Errorf("pane %d not found in wezterm cli list", paneID)
}

// buildTUIConfig narrows config.Config (§10.7 wide) down to the
// tui.Config slice the picker actually uses.
func buildTUIConfig(c *config.Config) tui.Config {
	cols := make([]tui.Column, 0, len(c.Columns))
	for _, col := range c.Columns {
		cols = append(cols, tui.Column(col))
	}
	return tui.Config{
		Sort:                      tui.SortMode(c.Sort),
		DefaultAction:             tui.Action(c.DefaultAction),
		DefaultActionLoadNoPrompt: c.DefaultActionLoadNoPrompt,
		PreviewEnabled:            c.Preview.Enabled,
		PreviewWidth:              c.Preview.Width,
		Markers: tui.Markers{
			Active:  c.Markers.Active,
			Live:    c.Markers.Live,
			Marked:  c.Markers.Marked,
			Unsaved: c.Markers.Unsaved,
			Pinned:  c.Markers.Pinned,
			// External marker is not yet a config knob; the TUI defaults
			// to a sensible glyph when this is empty.
			External: "",
		},
		Columns:          cols,
		NameTruncate:     c.NameTruncate,
		Keys:             tui.KeyMap{},
		ConfirmDelete:    c.ConfirmDelete,
		ConfirmOverwrite: c.ConfirmOverwrite,
	}
}

// buildTUIData assembles the §8.20.1 sub-step (8) initial picker
// payload. The reconciliation contract (§8.16) is to surface EVERY
// reachable workspace as one row, drawn from three sources unioned by
// name:
//
//   - repo.List — every snapshot file under <snapshot_dir>/workspace/.
//     Pinned-or-not, the row carries Saved=true, Mtime, Snapshot, and
//     (when the sidecar parses) Tags + Pinned.
//   - wcli.List workspace names — distinct workspace strings from the
//     live wezterm mux. Marks the row Live=true; merges into existing
//     snapshot rows when the names collide.
//   - state.LivePins — workspaces pinned before any save. Carried in as
//     Live=true + Pinned=true (the §13.11 disjoint-domain invariant
//     guarantees no overlap with sidecar-pinned saved rows; if a stale
//     entry collides, the existing row's Pinned is OR'd in without
//     duplicating).
//
// Active workspace marker resolves via the wcli.List output: the pane
// matching the binary's own paneID names the workspace the user is
// currently looking at.
func buildTUIData(ctx context.Context, store *state.Store, repo *snapshots.Repo, panes []wezcli.Pane, paneID int, log *logger.Logger) tui.Data {
	d := tui.Data{
		State:          store,
		ActiveWindowID: paneID,
	}

	// Resolve the active workspace from panes[paneID].Workspace. Empty
	// when the pane is not in the slice (test fixtures, pre-attach
	// states); the TUI then renders no Active marker until reconcile.
	activeWorkspace := ""
	for _, p := range panes {
		if p.PaneID == paneID {
			activeWorkspace = p.Workspace
			break
		}
	}
	d.ActiveWorkspace = activeWorkspace

	seen := make(map[string]int) // name → index into d.Workspaces

	// (1) Snapshot rows — repo.List is the spine of the picker. A
	// repo.List error is logged but not fatal (resurrect may not have
	// populated the dir yet); per-entry parse errors travel on the
	// Entry and are surfaced by the TUI's render layer.
	if repo != nil {
		entries, err := repo.List(ctx)
		if err != nil {
			if log != nil {
				log.Warn("buildTUIData repo.List failed", "err", err.Error())
			}
		} else {
			for _, e := range entries {
				snap := e
				row := tui.WorkspaceRow{
					Name:     e.Name,
					Source:   tui.SourceSaved,
					Mtime:    e.Mtime,
					Snapshot: &snap,
				}
				if e.SidecarOK {
					row.Pinned = e.Sidecar.Pinned
					row.Tags = append([]string(nil), e.Sidecar.Tags...)
				}
				seen[e.Name] = len(d.Workspaces)
				d.Workspaces = append(d.Workspaces, row)
			}
		}
	}

	// (2) Live workspaces from the wcli.List output — distinct
	// workspace names. Merges into an existing snapshot row when the
	// name already lives in `seen`.
	liveSeen := make(map[string]struct{})
	for _, p := range panes {
		name := p.Workspace
		if name == "" {
			continue
		}
		if _, dup := liveSeen[name]; dup {
			continue
		}
		liveSeen[name] = struct{}{}
		if idx, ok := seen[name]; ok {
			d.Workspaces[idx].Source = tui.SourceLive
			continue
		}
		seen[name] = len(d.Workspaces)
		d.Workspaces = append(d.Workspaces, tui.WorkspaceRow{
			Name:   name,
			Source: tui.SourceLive,
		})
	}

	// (3) Live pins — usually disjoint from saved per §13.11; still
	// merged defensively. A pin without a snapshot AND without a live
	// row gets a synthetic Live+Pinned row so the picker still surfaces
	// it.
	if store != nil {
		for _, name := range store.LivePins() {
			if idx, ok := seen[name]; ok {
				d.Workspaces[idx].Pinned = true
				continue
			}
			seen[name] = len(d.Workspaces)
			d.Workspaces = append(d.Workspaces, tui.WorkspaceRow{
				Name:   name,
				Source: tui.SourceLive,
				Pinned: true,
			})
		}
	}

	// (4) Active marker — set the matching row's Active=true. Skipped
	// when activeWorkspace did not appear in any of the three sources
	// (e.g., binary attached before plugin populated the mux state); the
	// TUI's later reconciliation loop will fill it in.
	if activeWorkspace != "" {
		if idx, ok := seen[activeWorkspace]; ok {
			d.Workspaces[idx].Active = true
		}
	}

	return d
}

// scrubHookEnv is the §13.5.1 / §17.3 row "Hook env: WEZSESH_LOG
// survives" implementation: filter os.Environ()-like strings dropping
// only the three sensitive keys. Pure on its inputs so the test can
// drive it without setenv side effects.
func scrubHookEnv(parent []string) []string {
	drops := make(map[string]struct{}, len(hookEnvScrub))
	for _, k := range hookEnvScrub {
		drops[k] = struct{}{}
	}
	out := make([]string, 0, len(parent))
	for _, kv := range parent {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			out = append(out, kv)
			continue
		}
		if _, drop := drops[kv[:eq]]; drop {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// ──────────────────────────────────────────────────────────────────────
// Save flow (§13.4) — Phase A/B/C exposed as runSave so the §17.3
// gates ("Save Phase C re-hash", "Save in-process serialisation",
// "Save with stale hash", "Save first-write", "Save flock
// serialisation", "Pin sync on save") have a handle without the live
// TUI. T-501 lands the TUI keypress wiring; this helper is the
// single binary-side implementation that keypress will invoke.
// ──────────────────────────────────────────────────────────────────────

// SaveResult is the §13.4 success surface returned to the TUI: the
// re-hashed sha256 (Phase C) plus the workspace name. Error path is
// surfaced via the err return; SaveError carries the §6 error code.
type SaveResult struct {
	Name string
	Hash string
}

// SaveError mirrors the §6 universal error envelope subset relevant
// to the save flow. Code is one of:
//   - SNAPSHOT_LOCKED  (lock acquire timeout in Phase A or C)
//   - SNAPSHOT_CHANGED (Phase A hash mismatch)
//   - SNAPSHOT_MISSING (overwrite path, file gone)
//   - SAVE_FAILED      (Phase B Lua-side failure)
//   - IPC_TIMEOUT      (Phase B no reply within ipcCtx)
type SaveError struct {
	Code    string
	Message string
}

func (e *SaveError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// saveDeps is the runSave dependency surface. Tests build a fake
// dispatcher (any ipc.Dispatcher works) plus a fake Repo and Store
// (both via the live structs — they are filesystem-backed and pure on
// their disks).
type saveDeps struct {
	disp        ipc.Dispatcher
	repo        *snapshots.Repo
	store       *state.Store
	log         *logger.Logger // sync-flushed Warn/Error sink (§17.3)
	nameLock    func(name string) *sync.Mutex
	now         func() time.Time
	lockTimeout time.Duration // §14.1 Phase A budget (5 s prod)
	rehashLock  time.Duration // §14.1 Phase C budget (2 s prod)
	ipcTimeout  time.Duration // §14.1 IPC roundtrip budget (5 s prod)
}

// runSave performs the §13.4 Phase A/B/C save flow. expectedHash is
// the empty string for the first-save path; otherwise the prefixed
// "sha256:..." form the TUI captured at picker time.
//
// Returned SaveResult.Hash is the Phase-C re-hash of the file as
// written by Lua. On failure the error is a *SaveError carrying the
// §6 code; callers translate to the TUI status line / reply.
func runSave(ctx context.Context, deps saveDeps, name, expectedHash string, overwrite bool) (*SaveResult, error) {
	if name == "" {
		return nil, &SaveError{Code: "ILLEGAL_NAME", Message: "empty name"}
	}
	// In-process per-name serialisation — §13.4 / §17.3 row "Save
	// in-process serialisation".
	mu := deps.nameLock(name)
	mu.Lock()
	defer mu.Unlock()

	lockBudget := deps.lockTimeout
	if lockBudget <= 0 {
		lockBudget = 5 * time.Second
	}
	rehashBudget := deps.rehashLock
	if rehashBudget <= 0 {
		rehashBudget = 2 * time.Second
	}
	ipcBudget := deps.ipcTimeout
	if ipcBudget <= 0 {
		ipcBudget = 5 * time.Second
	}

	snapshotPath := filepath.Join(deps.repo.SnapshotDir(), "workspace", snapshots.EncodeName(name)+".json")

	// PHASE A — verify hash under brief lock.
	lockCtx, cancelLock := context.WithTimeout(ctx, lockBudget)
	if expectedHash == "" {
		// First-save path: AcquireExclusiveOrCreate. Concurrent
		// first-saves serialise here — the second caller waits on the
		// lock, then sees the file exists and proceeds (overwrite=false
		// rejection is the dispatcher's responsibility; we just pass
		// expected_hash through as null per §3.3).
		_, release, err := safefs.AcquireExclusiveOrCreate(lockCtx,
			filepath.Join(deps.repo.SnapshotDir(), "workspace"),
			snapshots.EncodeName(name)+".json", 0o600)
		cancelLock()
		if err != nil {
			if errors.Is(err, safefs.ErrLockTimeout) {
				return nil, &SaveError{Code: "SNAPSHOT_LOCKED"}
			}
			return nil, &SaveError{Code: "SAVE_FAILED", Message: err.Error()}
		}
		release()
	} else {
		fd, release, err := safefs.AcquireExclusive(lockCtx, snapshotPath)
		cancelLock()
		if err != nil {
			if errors.Is(err, safefs.ErrLockTimeout) {
				return nil, &SaveError{Code: "SNAPSHOT_LOCKED"}
			}
			if errors.Is(err, os.ErrNotExist) {
				return nil, &SaveError{Code: "SNAPSHOT_MISSING"}
			}
			return nil, &SaveError{Code: "SAVE_FAILED", Message: err.Error()}
		}
		readCtx, cancelRead := context.WithTimeout(ctx, lockBudget)
		body, err := fd.ReadAll(readCtx)
		cancelRead()
		release()
		if err != nil {
			return nil, &SaveError{Code: "SAVE_FAILED", Message: err.Error()}
		}
		gotHash := "sha256:" + hex.EncodeToString(sha256Sum(body))
		if gotHash != expectedHash {
			return nil, &SaveError{Code: "SNAPSHOT_CHANGED",
				Message: fmt.Sprintf("hash %s != expected %s", gotHash, expectedHash)}
		}
	}

	// PHASE B — emit save dispatch (no lock held).
	ipcCtx, cancelIPC := context.WithTimeout(ctx, ipcBudget)
	defer cancelIPC()
	args := map[string]any{
		"name":      name,
		"overwrite": overwrite,
	}
	// expected_hash: null for first-save, otherwise the canonical hash.
	if expectedHash == "" {
		args["expected_hash"] = nil
	} else {
		args["expected_hash"] = expectedHash
	}
	ch, err := deps.disp.Dispatch(ipcCtx, "save", args)
	if err != nil {
		return nil, &SaveError{Code: "SAVE_FAILED", Message: err.Error()}
	}
	var terminal *ipc.Reply
	var lastReplyID string // captured from any inbound reply for the timeout-log id slot
	for terminal == nil {
		select {
		case <-ipcCtx.Done():
			deps.log.Error("ipc save timeout",
				"id", lastReplyID, "verb", "save", "name", name,
				"reason", "ctx_done")
			return nil, &SaveError{Code: "IPC_TIMEOUT"}
		case reply, ok := <-ch:
			if !ok {
				deps.log.Error("ipc save timeout",
					"id", lastReplyID, "verb", "save", "name", name,
					"reason", "channel_closed")
				return nil, &SaveError{Code: "IPC_TIMEOUT"}
			}
			if reply.ID != "" {
				lastReplyID = reply.ID
			}
			if reply.Status == "completed" || reply.Status == "partial" {
				rcopy := reply
				terminal = &rcopy
			}
		}
	}
	if !terminal.OK {
		code := "SAVE_FAILED"
		msg := ""
		if terminal.Error != nil {
			if terminal.Error.Code != "" {
				code = terminal.Error.Code
			}
			msg = terminal.Error.Message
		}
		return nil, &SaveError{Code: code, Message: msg}
	}

	// PHASE C — re-hash under brief second lock.
	rehashCtx, cancelRehash := context.WithTimeout(ctx, rehashBudget)
	fd2, release2, err := safefs.AcquireExclusive(rehashCtx, snapshotPath)
	if err != nil {
		cancelRehash()
		if errors.Is(err, safefs.ErrLockTimeout) {
			return nil, &SaveError{Code: "SNAPSHOT_LOCKED"}
		}
		return nil, &SaveError{Code: "SAVE_FAILED", Message: err.Error()}
	}
	body2, err := fd2.ReadAll(rehashCtx)
	cancelRehash()
	release2()
	if err != nil {
		return nil, &SaveError{Code: "SAVE_FAILED", Message: err.Error()}
	}
	newHash := "sha256:" + hex.EncodeToString(sha256Sum(body2))

	// state.RecordSwitch is fire-and-forget on the success branch.
	// §13.4 calls it before the pin-migration block; the order ensures
	// usage telemetry is updated even if the migration short-circuits.
	if deps.store != nil {
		_ = deps.store.RecordSwitch(ctx, name)
	}

	// Pin migration (§13.11). Runs after Phase C so a save failure
	// leaves the live pin intact. SetLivePin(false) is GATED on a
	// successful WriteSidecar — §13.4 lists the three steps as a
	// happy-path sequence with no error handling, but unconditionally
	// dropping the live pin when the sidecar write failed would lose
	// the pin entirely (the sidecar isn't there to carry it forward).
	// Gating preserves the invariant "pin always has exactly one home".
	if deps.store != nil && deps.store.IsLivePinned(name) {
		side, _, _ := deps.repo.ReadSidecar(ctx, name)
		side.Version = snapshots.SidecarSchemaVersion
		side.Pinned = true
		if err := deps.repo.WriteSidecar(ctx, name, side); err == nil {
			_ = deps.store.SetLivePin(ctx, name, false)
		}
	}

	return &SaveResult{Name: name, Hash: newHash}, nil
}

// sha256Sum returns the raw 32-byte digest of buf. Pulled out so the
// runSave hot path (Phase A read + Phase C rehash) shares a single
// implementation, and the test harness can compare its expected hash
// against it without re-hashing inline.
func sha256Sum(buf []byte) []byte {
	h := sha256.Sum256(buf)
	return h[:]
}

// ──────────────────────────────────────────────────────────────────────
// Subcommand routing skeletons (T-801..T-808).
// ──────────────────────────────────────────────────────────────────────
//
// Each subcommand body lives in its own file (doctor.go, list.go, …).
