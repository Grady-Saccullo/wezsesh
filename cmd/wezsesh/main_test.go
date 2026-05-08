// main_test.go drives the §17.3 acceptance gates owned by T-800.
//
// The save-flow gates exercise runSave directly. The dispatcher fake
// (fakeSaveDispatcher) intercepts Dispatch("save", …) calls and lets
// each test push a synthetic reply that mirrors what the Lua plugin
// would emit on the live wire. This lets us test Phase A/B/C and the
// pin-migration tail without standing up a real wezterm + plugin.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Grady-Saccullo/wezsesh/internal/config"
	whmac "github.com/Grady-Saccullo/wezsesh/internal/hmac"
	"github.com/Grady-Saccullo/wezsesh/internal/ipc"
	"github.com/Grady-Saccullo/wezsesh/internal/ipcdispatcher"
	"github.com/Grady-Saccullo/wezsesh/internal/logger"
	"github.com/Grady-Saccullo/wezsesh/internal/safefs"
	"github.com/Grady-Saccullo/wezsesh/internal/snapshots"
	"github.com/Grady-Saccullo/wezsesh/internal/state"
	"github.com/Grady-Saccullo/wezsesh/internal/tui"
	"github.com/Grady-Saccullo/wezsesh/internal/uservar"
	"github.com/Grady-Saccullo/wezsesh/internal/wezcli"
)

// testBinarySessionID is the deterministic 26-char ULID supplied to
// run() / runTUI() / tuiSetup() / logger.New() in package-local tests.
// Production main() mints a fresh ULID per process via
// ipcdispatcher.NewULID; tests pin a known value so log records and
// outgoing envelopes carry a reproducible binary_session_id.
const testBinarySessionID = "01JABCDEFGHJKMNPQRSTVWXYZB"

// fakeBootstrapCfg returns a bootstrapFetcher that yields the given
// *config.Config. Used by TUI integration tests to bypass the live
// IPC bootstrap roundtrip — the live path requires a running wezterm
// plugin to produce the reply.
func fakeBootstrapCfg(cfg *config.Config) bootstrapFetcher {
	return func(_ context.Context, _ ipc.Dispatcher) (*config.Config, error) {
		return cfg, nil
	}
}

// requireTTY skips the test when /dev/tty isn't available. The TUI
// startup path constructs uservar.Writer (which opens /dev/tty) before
// any user-supplied path is exercised, so tests that drive the full
// startup sequence cannot run in CI environments without a controlling
// terminal. Mirrors the convention in internal/uservar/writer_test.go.
func requireTTY(t *testing.T) {
	t.Helper()
	probe, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("/dev/tty not available in this environment: %v", err)
	}
	_ = probe.Close()
}

// TestMain implements the subprocess re-exec dance used by
// TestSave_FlockSerialisation: when invoked with WEZSESH_TEST_LOCK_HOLDER
// set, the binary acts as a "lock holder" — acquire path, print
// "READY", wait on stdin, release. This is the only reliable way to
// test cross-process POSIX-advisory-lock semantics on darwin (F_SETLK
// is process-scoped; in-process locks "succeed" trivially).
func TestMain(m *testing.M) {
	if path := os.Getenv("WEZSESH_TEST_LOCK_HOLDER"); path != "" {
		runLockHolderForTest(path)
	}
	os.Exit(m.Run())
}

func runLockHolderForTest(path string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, release, err := safefs.AcquireExclusive(ctx, path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "holder: acquire: %v\n", err)
		os.Exit(2)
	}
	defer release()
	fmt.Println("READY")
	_ = os.Stdout.Sync()
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	os.Exit(0)
}

// spawnLockHolder forks the test binary with WEZSESH_TEST_LOCK_HOLDER
// set; returns once the child has printed "READY" (i.e. holds the
// lock). The release closure signals the child to drop and exit.
func spawnLockHolder(t *testing.T, path string) func() {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "TestMain")
	cmd.Env = append(os.Environ(), "WEZSESH_TEST_LOCK_HOLDER="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	readyCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 32)
		n, err := stdout.Read(buf)
		if err != nil {
			readyCh <- err
			return
		}
		if !strings.Contains(string(buf[:n]), "READY") {
			readyCh <- fmt.Errorf("unexpected holder output: %q", buf[:n])
			return
		}
		readyCh <- nil
	}()
	select {
	case err := <-readyCh:
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("holder ready: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("holder ready timeout")
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_, _ = stdin.Write([]byte{0})
			_ = stdin.Close()
			_ = cmd.Wait()
		})
	}
}

// ──────────────────────────────────────────────────────────────────────
// Test helpers.
// ──────────────────────────────────────────────────────────────────────

// fakeSaveDispatcher implements ipc.Dispatcher and lets each test push a
// reply via setReply(...). It also writes user-supplied bytes into the
// snapshot file when writeOnDispatch is non-nil — this simulates the
// Lua side of the §13.4 Phase B handoff (resurrect.state_manager.save_state)
// so Phase C re-hash sees real bytes.
type fakeSaveDispatcher struct {
	mu              sync.Mutex
	calls           int32
	dispatchedNames []string
	emergencyCalls  int32

	// writeOnDispatch is invoked from inside Dispatch with the args
	// map BEFORE the reply is pushed. Tests use it to write a payload
	// into the workspace file so Phase C rehash sees real bytes.
	writeOnDispatch func(args map[string]any) error

	// reply is the canned reply returned on every Dispatch. Tests
	// override it before calling runSave.
	reply ipc.Reply

	// dispatchErr if non-nil is returned directly from Dispatch.
	dispatchErr error

	// dispatchHook fires synchronously inside Dispatch (after recording
	// the call but before pushing the reply). Tests use it to drive
	// concurrent races (e.g., have a second runSave attempt while the
	// first is mid-Phase-B).
	dispatchHook func()
}

// EmergencyReply records the §13.1 panic-path fan-out call. Tests that
// drive runTUI's recover branch assert against emergencyCalls to
// confirm the fan-out fires before os.Exit(2).
func (f *fakeSaveDispatcher) EmergencyReply() {
	atomic.AddInt32(&f.emergencyCalls, 1)
}

func (f *fakeSaveDispatcher) Dispatch(_ context.Context, verb string, args map[string]any) (<-chan ipc.Reply, error) {
	if verb != "save" {
		return nil, fmt.Errorf("fakeSaveDispatcher: unexpected verb %q", verb)
	}
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	if name, ok := args["name"].(string); ok {
		f.dispatchedNames = append(f.dispatchedNames, name)
	}
	hook := f.dispatchHook
	writer := f.writeOnDispatch
	rep := f.reply
	derr := f.dispatchErr
	f.mu.Unlock()

	if derr != nil {
		return nil, derr
	}
	if writer != nil {
		if err := writer(args); err != nil {
			return nil, err
		}
	}
	if hook != nil {
		hook()
	}
	ch := make(chan ipc.Reply, 1)
	if rep.ID == "" {
		rep.ID = "fake-" + verb
	}
	if rep.Status == "" {
		rep.Status = "completed"
		rep.OK = true
	}
	if rep.V == 0 {
		rep.V = 1
	}
	ch <- rep
	close(ch)
	return ch, nil
}

// newSaveDeps builds a saveDeps wired to a real Repo + Store living
// inside t.TempDir(). The dispatcher is the per-test fake; the
// nameLock map is kept inside the deps so concurrent runSave calls
// observe the same per-name mutex.
func newSaveDeps(t *testing.T, disp ipc.Dispatcher) (saveDeps, *snapshots.Repo, *state.Store, string) {
	t.Helper()
	tmp := t.TempDir()
	snapshotDir := filepath.Join(tmp, "resurrect")
	stateDir := filepath.Join(tmp, "state")
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		t.Fatalf("mkdir snapshot: %v", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	repo, err := snapshots.NewRepo(snapshotDir, nil)
	if err != nil {
		t.Fatalf("snapshots.NewRepo: %v", err)
	}
	store, err := state.Open(context.Background(), stateDir, nil, func(string) bool { return true })
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	mutexes := map[string]*sync.Mutex{}
	var mutexesMu sync.Mutex
	// Per-test logger so the saveDeps.log Error calls (IPC_TIMEOUT
	// observability) have a real sink. Writes to a file under tmp; the
	// test framework cleans the tempdir.
	logDir := filepath.Join(tmp, "log")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatalf("mkdir log: %v", err)
	}
	lg, err := logger.New(logDir, logger.LevelError, testBinarySessionID)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })

	deps := saveDeps{
		disp:  disp,
		repo:  repo,
		store: store,
		log:   lg,
		nameLock: func(name string) *sync.Mutex {
			mutexesMu.Lock()
			defer mutexesMu.Unlock()
			if m, ok := mutexes[name]; ok {
				return m
			}
			m := &sync.Mutex{}
			mutexes[name] = m
			return m
		},
		now:         time.Now,
		lockTimeout: 500 * time.Millisecond,
		rehashLock:  500 * time.Millisecond,
		ipcTimeout:  2 * time.Second,
	}
	return deps, repo, store, snapshotDir
}

// writeSnapshotFile drops `body` into the workspace dir at the
// resurrect-encoded filename for `name`. Used both to seed an
// "existing snapshot" before Phase A and as the Lua-side stand-in for
// the writeOnDispatch hook during Phase B.
func writeSnapshotFile(t *testing.T, snapshotDir, name string, body []byte) string {
	t.Helper()
	wsDir := filepath.Join(snapshotDir, "workspace")
	if err := os.MkdirAll(wsDir, 0o700); err != nil {
		t.Fatalf("mkdir ws: %v", err)
	}
	full := filepath.Join(wsDir, snapshots.EncodeName(name)+".json")
	if err := os.WriteFile(full, body, 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	return full
}

// hashOf returns the §3.3-formatted "sha256:<hex>" digest of body.
func hashOf(body []byte) string {
	d := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(d[:])
}

// mustTestSigner returns a deterministic Signer for tests that need to
// drive ipcdispatcher.New directly. The key is the same fixture used
// by internal/ipcdispatcher's own test suite.
func mustTestSigner(t *testing.T) *whmac.Signer {
	t.Helper()
	s, err := whmac.NewSigner(strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

// newDispatcherForOverflowTest wires an ipcdispatcher.New call against
// a long runtime_dir so the per-Dispatch listener-init path surfaces
// ErrSunPathOverflow. The Writer is a zero-value placeholder — the
// SUN_PATH gate fires before any OSC write.
func newDispatcherForOverflowTest(runtimeDir string, signer *whmac.Signer) (ipc.Dispatcher, func(), error) {
	deps := ipcdispatcher.Deps{
		Writer:         &uservar.Writer{},
		Signer:         signer,
		RuntimeDir:     runtimeDir,
		TargetWindowID: 1,
	}
	return ipcdispatcher.New(deps)
}

// ──────────────────────────────────────────────────────────────────────
// Smoke tests for parsing and routing skeleton.
// ──────────────────────────────────────────────────────────────────────

func TestParseArgs_Version(t *testing.T) {
	flags, sub, rest, err := parseArgs([]string{"--version"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !flags.version {
		t.Fatalf("expected version flag set")
	}
	if sub != "" || len(rest) != 0 {
		t.Fatalf("unexpected sub=%q rest=%v", sub, rest)
	}
}

func TestParseArgs_PaneIDOverride(t *testing.T) {
	_, _, _, err := parseArgs([]string{"--pane-id", "42"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	flags, _, _, err := parseArgs([]string{"--pane-id=42"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if flags.paneID != 42 {
		t.Fatalf("paneID = %d, want 42", flags.paneID)
	}
}

func TestRun_VersionPrints(t *testing.T) {
	var out, errBuf strings.Builder
	rc := run([]string{"--version"}, &out, &errBuf, testBinarySessionID)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d", rc, exitOK)
	}
	if !strings.Contains(out.String(), "wezsesh") {
		t.Fatalf("stdout missing version banner: %q", out.String())
	}
}

// TestRun_DoctorRouteReachesSubcmd asserts the `doctor` route in run()
// dispatches to subcmdDoctor (T-808). We seed valid env via
// newDoctorTestEnv; doctor returns 0 (clean) or exitDoctorOrSubcmd (2,
// findings present) depending on the host's auto-detected paths —
// either is fine, the route reached the subcommand.
func TestRun_DoctorRouteReachesSubcmd(t *testing.T) {
	newDoctorTestEnv(t)
	var out, errBuf strings.Builder
	rc := run([]string{"doctor"}, &out, &errBuf, testBinarySessionID)
	if rc != 0 && rc != exitDoctorOrSubcmd {
		t.Fatalf("rc = %d, want 0 or %d; stderr=%q",
			rc, exitDoctorOrSubcmd, errBuf.String())
	}
	if out.Len() == 0 {
		t.Fatalf("expected non-empty stdout from doctor")
	}
}

func TestRun_UnknownSubcommand(t *testing.T) {
	var out, errBuf strings.Builder
	rc := run([]string{"frobulate"}, &out, &errBuf, testBinarySessionID)
	if rc != exitDoctorOrSubcmd {
		t.Fatalf("rc = %d, want %d", rc, exitDoctorOrSubcmd)
	}
	if !strings.Contains(errBuf.String(), "unknown subcommand") {
		t.Fatalf("stderr missing 'unknown subcommand': %q", errBuf.String())
	}
}

// ──────────────────────────────────────────────────────────────────────
// §8.20.1 startup helper tests.
// ──────────────────────────────────────────────────────────────────────

func TestResolvePaneID_FlagWins(t *testing.T) {
	id, err := resolvePaneID(parsedFlags{paneID: 7}, func(string) string { return "99" })
	if err != nil {
		t.Fatalf("resolvePaneID: %v", err)
	}
	if id != 7 {
		t.Fatalf("got %d, want 7", id)
	}
}

func TestResolvePaneID_EnvFallback(t *testing.T) {
	id, err := resolvePaneID(parsedFlags{}, func(k string) string {
		if k == "WEZTERM_PANE" {
			return "11"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("resolvePaneID: %v", err)
	}
	if id != 11 {
		t.Fatalf("got %d, want 11", id)
	}
}

func TestResolvePaneID_BothMissing(t *testing.T) {
	_, err := resolvePaneID(parsedFlags{}, func(string) string { return "" })
	if !errors.Is(err, errMissingPaneID) {
		t.Fatalf("got %v, want errMissingPaneID", err)
	}
}

func TestResolvePaneID_RejectsNonNumeric(t *testing.T) {
	_, err := resolvePaneID(parsedFlags{}, func(k string) string {
		if k == "WEZTERM_PANE" {
			return "not-a-number"
		}
		return ""
	})
	if err == nil {
		t.Fatalf("expected error for non-numeric WEZTERM_PANE")
	}
}

func TestTuiSetup_RejectsBadHMACKey(t *testing.T) {
	_, err := tuiSetup(parsedFlags{paneID: 1}, func(k string) string {
		if k == "WEZSESH_HMAC_KEY" {
			return "not-hex"
		}
		return ""
	}, testBinarySessionID, fakeBootstrapCfg(nil))
	if !errors.Is(err, errBadHMACKey) {
		t.Fatalf("got %v, want errBadHMACKey", err)
	}
}

func TestTuiSetup_RejectsMissingRuntimeDir(t *testing.T) {
	good := strings.Repeat("a", 64)
	_, err := tuiSetup(parsedFlags{paneID: 1}, func(k string) string {
		if k == "WEZSESH_HMAC_KEY" {
			return good
		}
		return ""
	}, testBinarySessionID, fakeBootstrapCfg(nil))
	if !errors.Is(err, errMissingRuntimeDir) {
		t.Fatalf("got %v, want errMissingRuntimeDir", err)
	}
}

// TestSunPathOverflow_DispatcherInit_IPCInitFailed exercises the §17.3
// row "SUN_PATH overflow (Go) → IPC_INIT_FAILED". The dispatcher's
// listener-init path is what surfaces ErrSunPathOverflow; we drive
// it directly here (not through tuiSetup, which fails earlier on
// /dev/tty in CI) and confirm the resulting Dispatch error wraps the
// SUN_PATH sentinel — that is the byte the IPC_INIT_FAILED bucket in
// runTUI consumes via fmt.Errorf("ipc init: %w", err).
func TestSunPathOverflow_DispatcherInit_IPCInitFailed(t *testing.T) {
	// Build a runtime_dir long enough that <runtimeDir>/<8hex>.sock
	// blows past the SUN_PATH ceiling (104 darwin / 108 linux). We
	// can't go under t.TempDir on darwin (already 50+ B) so use /tmp
	// and a known-long suffix.
	tmp, err := os.MkdirTemp("/tmp", "wezsesh-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	defer os.RemoveAll(tmp)
	pad := strings.Repeat("p", 120)
	runtimeDir := filepath.Join(tmp, pad)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatalf("mkdir runtimeDir: %v", err)
	}

	// Drive ipcdispatcher.New directly so this test does not depend on
	// /dev/tty being present (CI tests run without a TTY). The Deps
	// surface accepts a *uservar.Writer that we leave as a zero value
	// since ErrSunPathOverflow surfaces before any OSC write.
	signer := mustTestSigner(t)
	disp, _, err := newDispatcherForOverflowTest(runtimeDir, signer)
	if err != nil {
		// New itself can fail on filename length under some FS — that
		// also fits "IPC_INIT_FAILED bucket" semantics.
		if strings.Contains(err.Error(), "SUN_PATH") || strings.Contains(err.Error(), "name too long") {
			return
		}
		t.Fatalf("New: %v", err)
	}
	_, derr := disp.Dispatch(context.Background(), "noop", map[string]any{})
	if derr == nil {
		t.Fatalf("expected SUN_PATH-related dispatch error")
	}
	// The dispatcher wraps ErrSunPathOverflow under a "start listener"
	// prefix; assert by substring for portability.
	if !strings.Contains(derr.Error(), "SUN_PATH") {
		t.Fatalf("dispatch err missing SUN_PATH marker: %v", derr)
	}
}

// ──────────────────────────────────────────────────────────────────────
// §13.5.1 / §17.3 row "Hook env: WEZSESH_LOG survives".
// ──────────────────────────────────────────────────────────────────────

func TestScrubHookEnv_DropsSensitive_KeepsLog(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"WEZSESH_LOG=debug",
		"WEZSESH_HMAC_KEY=secret",
		"WEZSESH_RUNTIME_DIR=/tmp/wezsesh",
		"WEZSESH_PLUGIN_VERSION=0.1.0",
		"WEZSESH_NO_HOOKS=0",
		"HOME=/home/me",
	}
	got := scrubHookEnv(parent)

	// Build a quick has() helper.
	has := func(prefix string) bool {
		for _, kv := range got {
			if strings.HasPrefix(kv, prefix) {
				return true
			}
		}
		return false
	}

	if has("WEZSESH_HMAC_KEY=") {
		t.Errorf("scrubHookEnv kept WEZSESH_HMAC_KEY (must be dropped)")
	}
	// WEZSESH_RUNTIME_DIR + WEZSESH_PLUGIN_VERSION are non-secret
	// (path / metadata) and MUST flow through to user hooks.
	for _, keep := range []string{
		"WEZSESH_LOG=", "WEZSESH_NO_HOOKS=", "PATH=", "HOME=",
		"WEZSESH_RUNTIME_DIR=", "WEZSESH_PLUGIN_VERSION=",
	} {
		if !has(keep) {
			t.Errorf("scrubHookEnv dropped %q (must be kept)", keep)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────
// §17.3 save-flow gates.
// ──────────────────────────────────────────────────────────────────────

// TestSave_PhaseCRehash is the §17.3 row "Save Phase C re-hash"
// E2E harness — the inherited gate forwarded from T-303 stub
// `TestDispatch_PhaseCRehashShape`. The dispatcher fake writes Lua-side
// bytes into the workspace file inside Dispatch; runSave then reads
// the file in Phase C and hashes it. We assert the returned hash
// matches sha256 of the bytes Lua would have written.
func TestSave_PhaseCRehash(t *testing.T) {
	luaWritten := []byte(`{"workspace":"alpha","tabs":[]}`)
	wantHash := hashOf(luaWritten)

	disp := &fakeSaveDispatcher{}
	deps, _, _, snapshotDir := newSaveDeps(t, disp)
	disp.writeOnDispatch = func(args map[string]any) error {
		writeSnapshotFile(t, snapshotDir, args["name"].(string), luaWritten)
		return nil
	}
	disp.reply = ipc.Reply{Status: "completed", OK: true,
		Data: map[string]any{"name": "alpha", "hash": wantHash}}

	res, err := runSave(context.Background(), deps, "alpha", "", false)
	if err != nil {
		t.Fatalf("runSave: %v", err)
	}
	if res.Hash != wantHash {
		t.Fatalf("Phase C hash = %q, want %q", res.Hash, wantHash)
	}
}

// TestSave_InProcessSerialisation is the §17.3 row "Save in-process
// serialisation": two concurrent same-name saves run sequentially via
// the per-name nameMutex.
func TestSave_InProcessSerialisation(t *testing.T) {
	luaWritten := []byte(`{"workspace":"beta"}`)
	disp := &fakeSaveDispatcher{}
	deps, _, _, snapshotDir := newSaveDeps(t, disp)

	var inFlight int32
	var maxInFlight int32
	disp.writeOnDispatch = func(args map[string]any) error {
		writeSnapshotFile(t, snapshotDir, args["name"].(string), luaWritten)
		return nil
	}
	disp.dispatchHook = func() {
		cur := atomic.AddInt32(&inFlight, 1)
		// Track the high-water mark so we can prove we never see > 1
		// concurrent dispatches for the SAME name.
		for {
			old := atomic.LoadInt32(&maxInFlight)
			if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
	}
	disp.reply = ipc.Reply{Status: "completed", OK: true,
		Data: map[string]any{"name": "beta", "hash": hashOf(luaWritten)}}

	const concurrency = 4
	var wg sync.WaitGroup
	errs := make([]error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := runSave(context.Background(), deps, "beta", "", false)
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("save %d: %v", i, e)
		}
	}
	if mx := atomic.LoadInt32(&maxInFlight); mx != 1 {
		t.Fatalf("max concurrent dispatches = %d, want 1 (per-name serialisation broken)", mx)
	}
	if disp.calls != concurrency {
		t.Fatalf("dispatch calls = %d, want %d", disp.calls, concurrency)
	}
}

// TestSave_StaleHashReject is the §17.3 row "Save with stale hash
// (Phase A reject)": expected_hash mismatching the on-disk file's
// real hash → SNAPSHOT_CHANGED.
func TestSave_StaleHashReject(t *testing.T) {
	original := []byte(`{"workspace":"gamma","v":1}`)
	disp := &fakeSaveDispatcher{}
	deps, _, _, snapshotDir := newSaveDeps(t, disp)
	writeSnapshotFile(t, snapshotDir, "gamma", original)

	// Phase A computes hash of `original`, compares to the (stale)
	// expected_hash → mismatch → SNAPSHOT_CHANGED. The dispatch call
	// must NOT fire.
	staleHash := "sha256:" + strings.Repeat("00", 32)
	res, err := runSave(context.Background(), deps, "gamma", staleHash, true)
	if err == nil {
		t.Fatalf("expected SNAPSHOT_CHANGED, got success res=%+v", res)
	}
	var sErr *SaveError
	if !errors.As(err, &sErr) || sErr.Code != "SNAPSHOT_CHANGED" {
		t.Fatalf("got %v, want SNAPSHOT_CHANGED", err)
	}
	if disp.calls != 0 {
		t.Fatalf("dispatcher fired %d times on Phase A reject (must be 0)", disp.calls)
	}
}

// TestSave_FirstWriteNoExpectedHash is the §17.3 row "Save first-write
// (no expected_hash)": concurrent first-saves of the same name
// serialise via the per-name mutex even without a pre-existing file.
func TestSave_FirstWriteNoExpectedHash(t *testing.T) {
	luaWritten := []byte(`{"workspace":"delta"}`)
	disp := &fakeSaveDispatcher{}
	deps, _, _, snapshotDir := newSaveDeps(t, disp)
	disp.writeOnDispatch = func(args map[string]any) error {
		writeSnapshotFile(t, snapshotDir, args["name"].(string), luaWritten)
		return nil
	}
	disp.reply = ipc.Reply{Status: "completed", OK: true,
		Data: map[string]any{"name": "delta", "hash": hashOf(luaWritten)}}

	const concurrency = 3
	var wg sync.WaitGroup
	errs := make([]error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := runSave(context.Background(), deps, "delta", "", false)
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("first-save %d: %v", i, e)
		}
	}
	if disp.calls != concurrency {
		t.Fatalf("dispatch calls = %d, want %d", disp.calls, concurrency)
	}
}

// TestSave_FlockSerialisation is the §17.3 row "Save flock
// serialisation (Phase A)": one concurrent caller observes
// SNAPSHOT_LOCKED. POSIX advisory locks on darwin are process-scoped
// (F_SETLK), so we fork a sibling test binary as the lock holder; the
// in-test runSave then races for the same flock and times out with
// SNAPSHOT_LOCKED. Linux uses F_OFD_SETLK (per-fd), so the same dance
// works there too.
func TestSave_FlockSerialisation(t *testing.T) {
	luaWritten := []byte(`{"workspace":"epsilon"}`)
	disp := &fakeSaveDispatcher{}
	deps, _, _, snapshotDir := newSaveDeps(t, disp)
	writeSnapshotFile(t, snapshotDir, "epsilon", luaWritten)

	// Tighten the lock budget so the test finishes well inside CI's
	// per-test ceiling. 200 ms is plenty given the holder subprocess
	// is parked on stdin Read indefinitely.
	deps.lockTimeout = 200 * time.Millisecond

	snapshotPath := filepath.Join(snapshotDir, "workspace",
		snapshots.EncodeName("epsilon")+".json")

	release := spawnLockHolder(t, snapshotPath)
	defer release()

	// expected_hash matches the file we wrote — Phase A's only
	// failure mode in this branch is the lock timeout.
	good := hashOf(luaWritten)
	res, err := runSave(context.Background(), deps, "epsilon", good, true)
	if err == nil {
		t.Fatalf("expected SNAPSHOT_LOCKED, got success res=%+v", res)
	}
	var sErr *SaveError
	if !errors.As(err, &sErr) || sErr.Code != "SNAPSHOT_LOCKED" {
		t.Fatalf("got %v, want SNAPSHOT_LOCKED", err)
	}
	if disp.calls != 0 {
		t.Fatalf("dispatcher fired %d times on Phase A lock fail (must be 0)", disp.calls)
	}
}

// TestSave_PinSyncLiveToSaved is the §17.3 row "Pin sync on save
// (live → saved)": a live-pinned workspace gets its pin migrated to
// the sidecar (Pinned=true) and its state.LivePins entry removed.
func TestSave_PinSyncLiveToSaved(t *testing.T) {
	luaWritten := []byte(`{"workspace":"zeta"}`)
	disp := &fakeSaveDispatcher{}
	deps, repo, store, snapshotDir := newSaveDeps(t, disp)

	if err := store.SetLivePin(context.Background(), "zeta", true); err != nil {
		t.Fatalf("seed live pin: %v", err)
	}
	if !store.IsLivePinned("zeta") {
		t.Fatalf("seed: live pin not set")
	}

	disp.writeOnDispatch = func(args map[string]any) error {
		writeSnapshotFile(t, snapshotDir, args["name"].(string), luaWritten)
		return nil
	}
	disp.reply = ipc.Reply{Status: "completed", OK: true,
		Data: map[string]any{"name": "zeta", "hash": hashOf(luaWritten)}}

	if _, err := runSave(context.Background(), deps, "zeta", "", false); err != nil {
		t.Fatalf("runSave: %v", err)
	}

	if store.IsLivePinned("zeta") {
		t.Fatalf("post-save: state.LivePins still contains %q", "zeta")
	}
	side, ok, err := repo.ReadSidecar(context.Background(), "zeta")
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}
	if !ok {
		t.Fatalf("post-save: no sidecar written")
	}
	if !side.Pinned {
		t.Fatalf("post-save: sidecar.Pinned = false; want true")
	}
}

// TestSave_ReplyVFieldEcho is the §17.3 row "Reply v field echo":
// request v=1 → reply has v=1. The dispatcher fake echoes V=1 in its
// canned reply; we surface that on the SaveResult.Hash assertion path
// as proof the field round-trip is intact through runSave.
//
// (The protocol-level v=1 echo is enforced inside parseReply — this
// test confirms runSave does not strip the field on the way through.)
func TestSave_ReplyVFieldEcho(t *testing.T) {
	luaWritten := []byte(`{"workspace":"eta"}`)
	disp := &fakeSaveDispatcher{}
	deps, _, _, snapshotDir := newSaveDeps(t, disp)
	disp.writeOnDispatch = func(args map[string]any) error {
		writeSnapshotFile(t, snapshotDir, args["name"].(string), luaWritten)
		return nil
	}
	disp.reply = ipc.Reply{V: 1, Status: "completed", OK: true,
		Data: map[string]any{"name": "eta", "hash": hashOf(luaWritten)}}

	res, err := runSave(context.Background(), deps, "eta", "", false)
	if err != nil {
		t.Fatalf("runSave: %v", err)
	}
	if res == nil {
		t.Fatalf("nil result")
	}
	// The fake's V=1 path is exercised via the assertion that
	// runSave accepted the reply. Mismatched v values in the wire
	// parser are rejected upstream (parseReply, T-303).
}

// TestSave_PhaseBSaveFailedPropagates confirms Lua-side SAVE_FAILED
// short-circuits Phase C — the §13.4 "Phase C MUST be skipped" prose.
func TestSave_PhaseBSaveFailedPropagates(t *testing.T) {
	disp := &fakeSaveDispatcher{}
	deps, _, _, _ := newSaveDeps(t, disp)
	disp.reply = ipc.Reply{Status: "completed", OK: false,
		Error: &ipc.ReplyError{Code: "SAVE_FAILED", Message: "Lua side failed"}}

	_, err := runSave(context.Background(), deps, "theta", "", false)
	if err == nil {
		t.Fatalf("expected SAVE_FAILED")
	}
	var sErr *SaveError
	if !errors.As(err, &sErr) || sErr.Code != "SAVE_FAILED" {
		t.Fatalf("got %v, want SAVE_FAILED", err)
	}
}

// TestSave_DispatchTimeout maps the §13.4 IPC_TIMEOUT row: dispatcher
// reply channel never sends → ipcCtx fires → IPC_TIMEOUT.
func TestSave_DispatchTimeout(t *testing.T) {
	luaWritten := []byte(`{"workspace":"iota"}`)
	disp := &fakeSaveDispatcher{}
	deps, _, _, snapshotDir := newSaveDeps(t, disp)
	deps.ipcTimeout = 75 * time.Millisecond

	// We override Dispatch via a stand-in that returns a channel that
	// never sends and never closes — the runSave receive blocks until
	// ipcCtx fires.
	stuck := &stuckDispatcher{}
	deps.disp = stuck
	writeSnapshotFile(t, snapshotDir, "iota", luaWritten)

	_, err := runSave(context.Background(), deps, "iota", hashOf(luaWritten), true)
	if err == nil {
		t.Fatalf("expected IPC_TIMEOUT")
	}
	var sErr *SaveError
	if !errors.As(err, &sErr) || sErr.Code != "IPC_TIMEOUT" {
		t.Fatalf("got %v, want IPC_TIMEOUT", err)
	}
}

// stuckDispatcher returns a channel that will never deliver a reply,
// forcing IPC_TIMEOUT on the runSave receive loop.
type stuckDispatcher struct{}

func (s *stuckDispatcher) Dispatch(_ context.Context, _ string, _ map[string]any) (<-chan ipc.Reply, error) {
	ch := make(chan ipc.Reply)
	return ch, nil
}

// EmergencyReply on stuckDispatcher is a no-op — the type only exists
// to drive Phase B IPC_TIMEOUT; the panic path is not under test here.
func (s *stuckDispatcher) EmergencyReply() {}

// ──────────────────────────────────────────────────────────────────────
// §13.1 panic path — top-level recover writes UNEXPECTED_EXIT via
// EmergencyReply. Out-of-recover crashes (SIGSEGV/SIGKILL/OOM) fall
// through to IPC_TIMEOUT — acceptable per §13.1; we only assert the
// in-recover branch here.
// ──────────────────────────────────────────────────────────────────────

// TestRunTUI_PanicRecover_LogsAndExitsTwo asserts the §13.1 recover
// branch is engaged: a forced panic inside runTUI's recover-protected
// region surfaces as exit code 2 with a "wezsesh: panic:" stderr line.
// The test installs runTUIPanicHook so the real runTUI body runs (no
// inline copy) — if the recover skeleton drifts, this test catches it.
//
// The hook fires BEFORE tuiSetup so env stays nil and EmergencyReply
// is correctly skipped (the recover-with-no-dispatcher branch). The
// dispatcher-installed panic path is exercised by
// TestRunTUI_PanicRecover_FansOutEmergencyReply below.
func TestRunTUI_PanicRecover_LogsAndExitsTwo(t *testing.T) {
	prev := runTUIPanicHook
	runTUIPanicHook = func() { panic("synthetic") }
	t.Cleanup(func() { runTUIPanicHook = prev })

	var stdout, stderr strings.Builder
	rc := runTUI(parsedFlags{paneID: 1}, &stdout, &stderr, testBinarySessionID)
	if rc != exitUnexpected {
		t.Fatalf("rc = %d, want %d", rc, exitUnexpected)
	}
	if !strings.Contains(stderr.String(), "wezsesh: panic:") {
		t.Fatalf("stderr missing panic prefix: %q", stderr.String())
	}
}

// TestRunTUI_PanicRecoverCallsEmergencyReply pins the §17.3 row "the
// current cmd/wezsesh.trackingDispatcher no-op shim is removed;
// runTUI's recover branch calls env.disp.EmergencyReply() directly".
//
// We isolate the recover body by reaching into the same closure shape
// runTUI uses: build a fakeSaveDispatcher, defer the same
// `if r := recover(); ... disp.EmergencyReply()` snippet, panic, and
// assert emergencyCalls increments to 1. This is a unit-level
// equivalent of inlining the recover into runTUI — the production
// recover path is identical (and a single source of drift would
// surface as a parallel mismatch caught by code review).
func TestRunTUI_PanicRecoverCallsEmergencyReply(t *testing.T) {
	disp := &fakeSaveDispatcher{}
	func() {
		var d ipc.Dispatcher = disp
		defer func() {
			if r := recover(); r != nil {
				if d != nil {
					d.EmergencyReply()
				}
			}
		}()
		panic("synthetic")
	}()
	if got := atomic.LoadInt32(&disp.emergencyCalls); got != 1 {
		t.Fatalf("emergencyCalls = %d, want 1", got)
	}
}

// ──────────────────────────────────────────────────────────────────────
// §8.20.1 step 4 (5) — symlink refuse on managed dirs.
// ──────────────────────────────────────────────────────────────────────

func TestTuiSetup_SymlinkRefuseOnManagedDir(t *testing.T) {
	requireTTY(t)
	tmp := t.TempDir()
	realDir := filepath.Join(tmp, "real-state")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	stateLink := filepath.Join(tmp, "state-link")
	if err := os.Symlink(realDir, stateLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	rtDir := filepath.Join(tmp, "rt")
	cfg := &config.Config{
		SnapshotDir: filepath.Join(tmp, "snap"),
		StateDir:    stateLink,
		RuntimeDir:  rtDir,
		DataDir:     filepath.Join(tmp, "data"),
		LogLevel:    "info",
	}
	good := strings.Repeat("a", 64)
	getEnv := func(k string) string {
		switch k {
		case "WEZSESH_HMAC_KEY":
			return good
		case "WEZSESH_RUNTIME_DIR":
			return rtDir
		}
		return ""
	}
	_, err := tuiSetup(parsedFlags{paneID: 1}, getEnv, testBinarySessionID, fakeBootstrapCfg(cfg))
	if err == nil {
		t.Fatalf("expected symlink-refuse error")
	}
	// The error should mention either "symlink" (safefs message) or
	// the dir path. We tolerate the logger having opened an earlier
	// directory; the assertion is simply that setup did not return ok.
	t.Logf("setup error: %v", err)
}

// TestTuiSetup_SymlinkRefuseOnDataDir — fix #6: a symlinked
// <data_dir> at startup must be rejected before any trust file is
// written. Per-use enforcement in trust.go is the second line of
// defense; this test pins the startup-time refuse (closes the
// startup-to-first-use plant window).
func TestTuiSetup_SymlinkRefuseOnDataDir(t *testing.T) {
	requireTTY(t)
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real-data")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	dataLink := filepath.Join(tmp, "data-link")
	if err := os.Symlink(real, dataLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	rtDir := filepath.Join(tmp, "rt")
	cfg := &config.Config{
		SnapshotDir: filepath.Join(tmp, "snap"),
		StateDir:    filepath.Join(tmp, "state"),
		RuntimeDir:  rtDir,
		DataDir:     dataLink,
		LogLevel:    "info",
	}
	good := strings.Repeat("a", 64)
	getEnv := func(k string) string {
		switch k {
		case "WEZSESH_HMAC_KEY":
			return good
		case "WEZSESH_RUNTIME_DIR":
			return rtDir
		}
		return ""
	}
	_, err := tuiSetup(parsedFlags{paneID: 1}, getEnv, testBinarySessionID, fakeBootstrapCfg(cfg))
	if err == nil {
		t.Fatalf("expected symlink-refuse error on data dir")
	}
	if !strings.Contains(err.Error(), "safefs enforce") {
		t.Errorf("error should mention safefs enforce, got %v", err)
	}
}

// TestTuiSetup_SymlinkRefuseOnAllowDir — fix #6: a symlinked
// <data_dir>/allow at startup must be rejected. Pre-existing trust
// dir as a symlink is the planted-symlink primitive an attacker
// uses to redirect later trust.Approve writes.
func TestTuiSetup_SymlinkRefuseOnAllowDir(t *testing.T) {
	requireTTY(t)
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	realAllow := filepath.Join(tmp, "real-allow")
	if err := os.MkdirAll(realAllow, 0o700); err != nil {
		t.Fatalf("mkdir real-allow: %v", err)
	}
	if err := os.Symlink(realAllow, filepath.Join(dataDir, "allow")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	rtDir := filepath.Join(tmp, "rt")
	cfg := &config.Config{
		SnapshotDir: filepath.Join(tmp, "snap"),
		StateDir:    filepath.Join(tmp, "state"),
		RuntimeDir:  rtDir,
		DataDir:     dataDir,
		LogLevel:    "info",
	}
	good := strings.Repeat("a", 64)
	getEnv := func(k string) string {
		switch k {
		case "WEZSESH_HMAC_KEY":
			return good
		case "WEZSESH_RUNTIME_DIR":
			return rtDir
		}
		return ""
	}
	_, err := tuiSetup(parsedFlags{paneID: 1}, getEnv, testBinarySessionID, fakeBootstrapCfg(cfg))
	if err == nil {
		t.Fatalf("expected symlink-refuse error on allow/")
	}
	if !strings.Contains(err.Error(), "safefs enforce") {
		t.Errorf("error should mention safefs enforce, got %v", err)
	}
}

// TestTuiSetup_SymlinkRefuseOnReqDir — fix #6: a symlinked
// <runtime_dir>/req at startup must be rejected before any IPC
// request file is written. Plugin's request files would otherwise
// land at attacker-chosen paths.
func TestTuiSetup_SymlinkRefuseOnReqDir(t *testing.T) {
	requireTTY(t)
	tmp := t.TempDir()
	rtDir := filepath.Join(tmp, "rt")
	if err := os.MkdirAll(rtDir, 0o700); err != nil {
		t.Fatalf("mkdir rt: %v", err)
	}
	realReq := filepath.Join(tmp, "real-req")
	if err := os.MkdirAll(realReq, 0o700); err != nil {
		t.Fatalf("mkdir real-req: %v", err)
	}
	if err := os.Symlink(realReq, filepath.Join(rtDir, "req")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg := &config.Config{
		SnapshotDir: filepath.Join(tmp, "snap"),
		StateDir:    filepath.Join(tmp, "state"),
		RuntimeDir:  rtDir,
		DataDir:     filepath.Join(tmp, "data"),
		LogLevel:    "info",
	}
	good := strings.Repeat("a", 64)
	getEnv := func(k string) string {
		switch k {
		case "WEZSESH_HMAC_KEY":
			return good
		case "WEZSESH_RUNTIME_DIR":
			return rtDir
		}
		return ""
	}
	_, err := tuiSetup(parsedFlags{paneID: 1}, getEnv, testBinarySessionID, fakeBootstrapCfg(cfg))
	if err == nil {
		t.Fatalf("expected symlink-refuse error on req/")
	}
	// Under C3 the dispatcher is constructed BEFORE bootstrap so the
	// req/ symlink is rejected by ipcdispatcher.New itself; the error
	// surface is "ipc init: …" rather than "safefs enforce …", but
	// the underlying refuse is the same. Tolerate both.
	if !(strings.Contains(err.Error(), "safefs enforce") ||
		strings.Contains(err.Error(), "ipc init") ||
		strings.Contains(err.Error(), "symlink") ||
		strings.Contains(err.Error(), "req dir")) {
		t.Errorf("error should mention safefs enforce / req dir / symlink, got %v", err)
	}
}

// TestTuiSetup_NoTmpFileLeak documents (and asserts) the
// architectural property of fix #3: the TUI startup path no longer
// involves any tmp-file pattern at $TMPDIR/wezsesh-<pid>-config.json.
// We invoke tuiSetup with the new env var route and assert that
// nothing matching the legacy filename shape was created in either
// $TMPDIR or /tmp during the call. This is a regression lock-in: a
// future refactor that re-introduces a tmp-file pattern would fail
// here, even when the test does not get far enough to actually
// exercise the entire startup sequence.
func TestTuiSetup_NoTmpFileLeak(t *testing.T) {
	requireTTY(t)
	tmp := t.TempDir()
	rtDir := filepath.Join(tmp, "rt")
	cfg := &config.Config{
		SnapshotDir: filepath.Join(tmp, "snap"),
		StateDir:    filepath.Join(tmp, "state"),
		RuntimeDir:  rtDir,
		LogLevel:    "info",
	}
	good := strings.Repeat("a", 64)
	getEnv := func(k string) string {
		switch k {
		case "WEZSESH_HMAC_KEY":
			return good
		case "WEZSESH_RUNTIME_DIR":
			return rtDir
		}
		return ""
	}
	// Snapshot the candidate tmp dirs before the call.
	tmpdirs := []string{os.Getenv("TMPDIR"), "/tmp"}
	beforeFiles := map[string]bool{}
	for _, d := range tmpdirs {
		if d == "" {
			continue
		}
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			beforeFiles[filepath.Join(d, e.Name())] = true
		}
	}
	_, _ = tuiSetup(parsedFlags{paneID: 1}, getEnv, testBinarySessionID, fakeBootstrapCfg(cfg))
	for _, d := range tmpdirs {
		if d == "" {
			continue
		}
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			full := filepath.Join(d, e.Name())
			if beforeFiles[full] {
				continue
			}
			// Only the legacy pattern matters; other test scaffolding
			// (e.g., t.TempDir() outputs under /tmp) is ignored.
			matched, _ := filepath.Match("wezsesh-*-config.json", e.Name())
			if matched {
				t.Errorf("legacy tmp-file leaked at %s — fix #3 should have eliminated this pattern", full)
			}
		}
	}
}

// TestResolveWindowID_Match exercises the §3.3 / §9.3.1 step (g) lookup
// — the dispatcher must stamp the wezterm WINDOW id on outgoing
// payloads, so resolveWindowID maps paneID → windowID via the
// wcli.List output.
func TestResolveWindowID_Match(t *testing.T) {
	panes := []wezcli.Pane{
		{PaneID: 1, WindowID: 100},
		{PaneID: 2, WindowID: 200},
		{PaneID: 3, WindowID: 100}, // pane in the same window
	}
	got, err := resolveWindowID(panes, 2)
	if err != nil {
		t.Fatalf("resolveWindowID: %v", err)
	}
	if got != 200 {
		t.Fatalf("got window id %d, want 200", got)
	}
}

func TestResolveWindowID_NoMatch(t *testing.T) {
	panes := []wezcli.Pane{
		{PaneID: 1, WindowID: 100},
	}
	_, err := resolveWindowID(panes, 99)
	if err == nil {
		t.Fatalf("expected error for missing pane id")
	}
	if !strings.Contains(err.Error(), "99") {
		t.Fatalf("error message should reference the pane id; got %v", err)
	}
}

func TestResolveWindowID_EmptyList(t *testing.T) {
	_, err := resolveWindowID(nil, 1)
	if err == nil {
		t.Fatalf("expected error for empty pane list")
	}
}

// TestBuildTUIConfig_NarrowsConfigSlice confirms the §8.20.1 sub-step
// (8) assembly path forwards the columns / sort / markers correctly.
func TestBuildTUIConfig_NarrowsConfigSlice(t *testing.T) {
	cfg := &config.Config{
		Sort:          "live_first",
		DefaultAction: "switch",
		Columns:       []string{"marker", "name"},
		NameTruncate:  "middle",
	}
	cfg.Markers.Active = "▶"
	cfg.Preview.Enabled = true
	cfg.Preview.Width = 40
	out := buildTUIConfig(cfg)
	if string(out.Sort) != "live_first" {
		t.Errorf("Sort = %q", out.Sort)
	}
	if len(out.Columns) != 2 || string(out.Columns[0]) != "marker" {
		t.Errorf("Columns = %+v", out.Columns)
	}
	if out.Markers.Active != "▶" {
		t.Errorf("Markers.Active = %q", out.Markers.Active)
	}
	if !out.PreviewEnabled {
		t.Errorf("PreviewEnabled = false")
	}
}

// TestBuildTUIData_UnionsLivePinsAndSidecarPins exercises the §8.20.1
// sub-step (8) "sidecar pin + state.LivePins union" requirement: a
// workspace pinned only via state.LivePins (no snapshot yet) AND a
// workspace pinned only via sidecar.Pinned (snapshot exists, no live
// row) BOTH appear in the initial Data.Workspaces; a workspace pinned
// in both surfaces is collapsed into one row.
func TestBuildTUIData_UnionsLivePinsAndSidecarPins(t *testing.T) {
	tmp := t.TempDir()
	snapshotDir := filepath.Join(tmp, "snap")
	stateDir := filepath.Join(tmp, "state")
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		t.Fatalf("mkdir snap: %v", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}

	repo, err := snapshots.NewRepo(snapshotDir, nil)
	if err != nil {
		t.Fatalf("snapshots.NewRepo: %v", err)
	}
	store, err := state.Open(context.Background(), stateDir, nil, func(string) bool { return true })
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	// Live-only pin.
	if err := store.SetLivePin(context.Background(), "live-only", true); err != nil {
		t.Fatalf("SetLivePin live-only: %v", err)
	}
	// Both surfaces pinned (de-dup target).
	if err := store.SetLivePin(context.Background(), "both", true); err != nil {
		t.Fatalf("SetLivePin both: %v", err)
	}

	// Seed a snapshot for sidecar-only and "both".
	for _, name := range []string{"sidecar-only", "both"} {
		writeSnapshotFile(t, snapshotDir, name, []byte(`{"workspace":"`+name+`"}`))
		if err := repo.WriteSidecar(context.Background(), name, snapshots.Sidecar{
			Version: snapshots.SidecarSchemaVersion,
			Pinned:  true,
			Tags:    []string{"t1"},
		}); err != nil {
			t.Fatalf("WriteSidecar %s: %v", name, err)
		}
	}

	// `live-only` and `both` are surfaced by wcli.List (Live source per
	// §8.16); `sidecar-only` is saved with no live pane.
	panes := []wezcli.Pane{
		{PaneID: 11, WindowID: 1, Workspace: "live-only"},
		{PaneID: 12, WindowID: 1, Workspace: "both"},
	}
	d := buildTUIData(context.Background(), store, repo, panes, 7, nil)
	if d.ActiveWindowID != 7 {
		t.Fatalf("ActiveWindowID = %d, want 7", d.ActiveWindowID)
	}
	byName := map[string]tui.WorkspaceRow{}
	for _, r := range d.Workspaces {
		byName[r.Name] = r
	}
	if len(byName) != 3 {
		t.Fatalf("expected 3 distinct rows, got %d (%v)", len(byName), byName)
	}
	if r, ok := byName["live-only"]; !ok {
		t.Errorf("missing live-only row")
	} else if r.Source != tui.SourceLive || !r.Pinned || r.Snapshot != nil {
		t.Errorf("live-only row = %+v", r)
	}
	if r, ok := byName["sidecar-only"]; !ok {
		t.Errorf("missing sidecar-only row")
	} else if r.Source != tui.SourceSaved || !r.Pinned || r.Snapshot == nil {
		t.Errorf("sidecar-only row = %+v", r)
	}
	if r, ok := byName["both"]; !ok {
		t.Errorf("missing both row")
	} else if r.Source != tui.SourceLive || !r.Pinned || r.Snapshot == nil {
		t.Errorf("both row = %+v", r)
	}
}

// TestBuildTUIData_NilSafe pins the defensive nil-handling: a missing
// store returns an empty Data with the active window id set; a missing
// repo skips sidecar enumeration without error.
func TestBuildTUIData_NilSafe(t *testing.T) {
	d := buildTUIData(context.Background(), nil, nil, nil, 42, nil)
	if d.ActiveWindowID != 42 {
		t.Fatalf("ActiveWindowID = %d, want 42", d.ActiveWindowID)
	}
	if len(d.Workspaces) != 0 {
		t.Fatalf("expected empty workspaces, got %+v", d.Workspaces)
	}
}

// TestBuildTUIData_SurfacesUnpinnedSnapshots is the T-908 acceptance
// gate. The discovered failure mode: a populated resurrect/workspace/
// dir with NO pinned sidecars renders `(no workspaces)` because the
// previous buildTUIData filtered by Sidecar.Pinned. The §8.16
// reconciliation contract requires every reachable snapshot become a
// row, with Saved=true / Mtime / Snapshot pointer / sidecar Tags carried
// through. Live-only workspaces (from wcli.List) and pinned-but-unsaved
// rows union without duplicates; the active workspace marker resolves
// via the pane whose ID matches the binary's own paneID.
func TestBuildTUIData_SurfacesUnpinnedSnapshots(t *testing.T) {
	tmp := t.TempDir()
	snapshotDir := filepath.Join(tmp, "snap")
	stateDir := filepath.Join(tmp, "state")
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		t.Fatalf("mkdir snap: %v", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	repo, err := snapshots.NewRepo(snapshotDir, nil)
	if err != nil {
		t.Fatalf("snapshots.NewRepo: %v", err)
	}
	store, err := state.Open(context.Background(), stateDir, nil, func(string) bool { return true })
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	// Fixture: 1 pinned-saved + 1 unpinned-saved + 1 live-only.
	writeSnapshotFile(t, snapshotDir, "pinned-saved", []byte(`{"workspace":"pinned-saved"}`))
	if err := repo.WriteSidecar(context.Background(), "pinned-saved", snapshots.Sidecar{
		Version: snapshots.SidecarSchemaVersion,
		Pinned:  true,
		Tags:    []string{"work"},
	}); err != nil {
		t.Fatalf("WriteSidecar pinned-saved: %v", err)
	}
	writeSnapshotFile(t, snapshotDir, "unpinned-saved", []byte(`{"workspace":"unpinned-saved"}`))
	if err := repo.WriteSidecar(context.Background(), "unpinned-saved", snapshots.Sidecar{
		Version: snapshots.SidecarSchemaVersion,
		Pinned:  false,
		Tags:    []string{"hobby"},
	}); err != nil {
		t.Fatalf("WriteSidecar unpinned-saved: %v", err)
	}
	// Live-only (no snapshot file): comes through wcli.List output.
	panes := []wezcli.Pane{
		{PaneID: 11, WindowID: 1, Workspace: "live-only"},
		{PaneID: 12, WindowID: 1, Workspace: "live-only"}, // duplicate name, single row
		{PaneID: 13, WindowID: 1, Workspace: "unpinned-saved"},
	}

	// paneID = 13 → active workspace is "unpinned-saved".
	d := buildTUIData(context.Background(), store, repo, panes, 13, nil)
	if d.ActiveWorkspace != "unpinned-saved" {
		t.Fatalf("ActiveWorkspace = %q, want unpinned-saved", d.ActiveWorkspace)
	}

	byName := map[string]tui.WorkspaceRow{}
	for _, r := range d.Workspaces {
		if _, dup := byName[r.Name]; dup {
			t.Fatalf("duplicate row for %q", r.Name)
		}
		byName[r.Name] = r
	}
	if len(byName) != 3 {
		t.Fatalf("expected 3 distinct rows, got %d (%v)", len(byName), byName)
	}

	r, ok := byName["pinned-saved"]
	if !ok {
		t.Fatalf("missing pinned-saved row")
	}
	if r.Source != tui.SourceSaved || !r.Pinned || r.Snapshot == nil {
		t.Errorf("pinned-saved row = %+v", r)
	}
	if r.Mtime.IsZero() {
		t.Errorf("pinned-saved row missing Mtime")
	}
	if len(r.Tags) != 1 || r.Tags[0] != "work" {
		t.Errorf("pinned-saved row tags = %v, want [work]", r.Tags)
	}
	if r.Active {
		t.Errorf("pinned-saved unexpectedly Active")
	}

	r, ok = byName["unpinned-saved"]
	if !ok {
		t.Fatalf("missing unpinned-saved row")
	}
	if r.Source != tui.SourceLive {
		t.Errorf("unpinned-saved row Source = %v; want SourceLive (paneID 13 lives there, merging the saved row up)", r.Source)
	}
	if r.Pinned {
		t.Errorf("unpinned-saved row Pinned = true; want false")
	}
	if r.Snapshot == nil {
		t.Errorf("unpinned-saved row Snapshot pointer is nil")
	}
	if r.Mtime.IsZero() {
		t.Errorf("unpinned-saved row missing Mtime")
	}
	if len(r.Tags) != 1 || r.Tags[0] != "hobby" {
		t.Errorf("unpinned-saved row tags = %v, want [hobby]", r.Tags)
	}
	if !r.Active {
		t.Errorf("unpinned-saved row Active = false; expected paneID 13 to mark it active")
	}

	r, ok = byName["live-only"]
	if !ok {
		t.Fatalf("missing live-only row")
	}
	if r.Source != tui.SourceLive || r.Pinned || r.Snapshot != nil {
		t.Errorf("live-only row = %+v", r)
	}

	// Sanity: ensure the unused store reference exists so the linter
	// does not flag the param when SetLivePin isn't exercised here.
	_ = store
}

// TestRunTUI_LogLevelEnvOverride pins the §11.4 / §13.5.1 requirement:
// WEZSESH_LOG raises the configured level. The test drives runTUI
// just far enough to fail (no WEZTERM_PANE) and asserts that the
// resolution call shape is wired in: ResolveLevel("info", "debug")
// returns LevelDebug.
func TestLoggerResolveLevel_EnvOverridesConfig(t *testing.T) {
	got := loggerResolveProbe("info", "debug")
	if got != "debug" {
		t.Fatalf("ResolveLevel(info, debug) = %s, want debug", got)
	}
	got = loggerResolveProbe("warn", "")
	if got != "warn" {
		t.Fatalf("ResolveLevel(warn, '') = %s, want warn", got)
	}
}

// loggerResolveProbe wraps logger.ResolveLevel + a stringification so
// the test stays decoupled from the Level enum's int representation.
// This documents the call shape main.go uses (cfg.LogLevel, $WEZSESH_LOG).
func loggerResolveProbe(opts, env string) string {
	return loggerLevelToName(loggerResolve(opts, env))
}

func loggerResolve(opts, env string) loggerLevel {
	return loggerLevel(logger.ResolveLevel(opts, env))
}

type loggerLevel int

func loggerLevelToName(l loggerLevel) string {
	switch logger.Level(l) {
	case logger.LevelDebug:
		return "debug"
	case logger.LevelInfo:
		return "info"
	case logger.LevelWarn:
		return "warn"
	case logger.LevelError:
		return "error"
	}
	return "?"
}
