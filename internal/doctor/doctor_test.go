package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Grady-Saccullo/wezsesh/internal/config"
	"github.com/Grady-Saccullo/wezsesh/internal/safefs"
	"github.com/Grady-Saccullo/wezsesh/internal/snapshots"
	"github.com/Grady-Saccullo/wezsesh/internal/wezcli"
)

// =============================================================
// Test fakes
// =============================================================

type fakeWezcli struct {
	probeErr     error
	probeLatency time.Duration
	listPanes    []wezcli.Pane
	listErr      error
	listClients  []wezcli.ClientInfo
	clientsErr   error
}

func (f *fakeWezcli) Probe(ctx context.Context) (time.Duration, error) {
	if f.probeLatency == 0 {
		f.probeLatency = 5 * time.Millisecond
	}
	return f.probeLatency, f.probeErr
}

func (f *fakeWezcli) List(ctx context.Context) ([]wezcli.Pane, error) {
	return f.listPanes, f.listErr
}

func (f *fakeWezcli) ListClients(ctx context.Context) ([]wezcli.ClientInfo, error) {
	return f.listClients, f.clientsErr
}

type fakeRepo struct {
	entries  []snapshots.Entry
	listErr  error
	hasNames map[string]bool
	hasErr   error
	sniff    map[string]snapshots.Encryption
	sniffErr map[string]error
}

func (r *fakeRepo) List(ctx context.Context) ([]snapshots.Entry, error) {
	return r.entries, r.listErr
}

func (r *fakeRepo) Has(ctx context.Context, name string) (bool, error) {
	if r.hasErr != nil {
		return false, r.hasErr
	}
	return r.hasNames[name], nil
}

func (r *fakeRepo) Sniff(ctx context.Context, name string) (snapshots.Encryption, error) {
	if e, ok := r.sniffErr[name]; ok && e != nil {
		return snapshots.EncryptionUnknown, e
	}
	return r.sniff[name], nil
}

// withSeams reassigns each named seam and returns a restore func that
// reverts all of them on call.
type seams struct {
	envMap          map[string]string
	homeFn          func() (string, error)
	wezcliFn        func() (wezcliClient, error)
	stateFn         func(context.Context, string) ([]string, error)
	snapshotsFn     func(string) (snapshotsRepo, error)
	gpgFn           func(context.Context) (time.Duration, bool)
	ageFn           func(context.Context) (time.Duration, bool)
	lookPathFn      func(string) (string, error)
}

func applySeams(t *testing.T, s seams) {
	t.Helper()
	seamMu.Lock()
	defer seamMu.Unlock()
	prevEnv := envGet
	prevHome := homeDirFn
	prevW := wezcliFactory
	prevS := stateOpener
	prevR := snapshotsOpener
	prevG := gpgVersionProbe
	prevA := ageVersionProbe
	prevL := lookPathFn

	if s.envMap != nil {
		m := s.envMap
		envGet = func(k string) string {
			if v, ok := m[k]; ok {
				return v
			}
			return ""
		}
	}
	if s.homeFn != nil {
		homeDirFn = s.homeFn
	}
	if s.wezcliFn != nil {
		wezcliFactory = s.wezcliFn
	}
	if s.stateFn != nil {
		stateOpener = s.stateFn
	}
	if s.snapshotsFn != nil {
		snapshotsOpener = s.snapshotsFn
	}
	if s.gpgFn != nil {
		gpgVersionProbe = s.gpgFn
	}
	if s.ageFn != nil {
		ageVersionProbe = s.ageFn
	}
	if s.lookPathFn != nil {
		lookPathFn = s.lookPathFn
	}
	t.Cleanup(func() {
		seamMu.Lock()
		defer seamMu.Unlock()
		envGet = prevEnv
		homeDirFn = prevHome
		wezcliFactory = prevW
		stateOpener = prevS
		snapshotsOpener = prevR
		gpgVersionProbe = prevG
		ageVersionProbe = prevA
		lookPathFn = prevL
	})
}

func findCheck(t *testing.T, checks []Check, id string) Check {
	t.Helper()
	for _, c := range checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("check %q not in report", id)
	return Check{}
}

// =============================================================
// Acceptance gate tests (named per task brief)
// =============================================================

func TestDoctor_PinConsistency_Warn(t *testing.T) {
	// Positive overlap → warn.
	ctx := context.Background()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	snapDir := filepath.Join(dir, "snap")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	applySeams(t, seams{
		envMap: map[string]string{},
		stateFn: func(ctx context.Context, dir string) ([]string, error) {
			return []string{"alpha", "beta"}, nil
		},
		snapshotsFn: func(dir string) (snapshotsRepo, error) {
			return &fakeRepo{hasNames: map[string]bool{"alpha": true}}, nil
		},
	})
	r := Run(ctx, Env{StateDir: stateDir, SnapshotDir: snapDir, BinaryPath: os.Args[0]})
	c := findCheck(t, r.Checks, "snapshot.pin.consistency")
	if c.Status != StatusWarn {
		t.Fatalf("expected warn, got %s (%+v)", c.Status, c.Details)
	}
	overlap, _ := c.Details["overlap"].([]string)
	if len(overlap) != 1 || overlap[0] != "alpha" {
		t.Fatalf("unexpected overlap: %v", overlap)
	}
}

func TestDoctor_PinConsistency_OK_NoOverlap(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	snapDir := filepath.Join(dir, "snap")
	_ = os.MkdirAll(stateDir, 0o700)
	_ = os.MkdirAll(snapDir, 0o700)
	applySeams(t, seams{
		envMap: map[string]string{},
		stateFn: func(ctx context.Context, dir string) ([]string, error) {
			return []string{"alpha"}, nil
		},
		snapshotsFn: func(dir string) (snapshotsRepo, error) {
			return &fakeRepo{hasNames: map[string]bool{"beta": true}}, nil
		},
	})
	r := Run(ctx, Env{StateDir: stateDir, SnapshotDir: snapDir, BinaryPath: os.Args[0]})
	c := findCheck(t, r.Checks, "snapshot.pin.consistency")
	if c.Status != StatusOK {
		t.Fatalf("expected ok, got %s", c.Status)
	}
}

func TestDoctor_ExcludeInvalidRegex_Reported(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{
		Exclude: []string{"^ok$", "[unterminated"},
		ExcludeErrors: []config.ExcludeError{
			{Index: 1, Source: "[unterminated", Reason: "missing closing ]"},
		},
	}
	applySeams(t, seams{envMap: map[string]string{}})
	r := Run(ctx, Env{Cfg: cfg, BinaryPath: os.Args[0]})
	c := findCheck(t, r.Checks, "config.exclude.regex_validity")
	if c.Status != StatusWarn {
		t.Fatalf("expected warn, got %s", c.Status)
	}
	errs, _ := c.Details["errors"].([]map[string]any)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %v", errs)
	}
}

func TestDoctor_ExcludeRegex_OK(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{Exclude: []string{"^ok$"}}
	cfg.ExcludeCompiled = []*regexp.Regexp{regexp.MustCompile("^ok$")}
	applySeams(t, seams{envMap: map[string]string{}})
	r := Run(ctx, Env{Cfg: cfg, BinaryPath: os.Args[0]})
	c := findCheck(t, r.Checks, "config.exclude.regex_validity")
	if c.Status != StatusOK {
		t.Fatalf("expected ok, got %s (details=%v)", c.Status, c.Details)
	}
}

func TestDoctor_RuntimeReqOrphans_Warn(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	reqDir := filepath.Join(dir, "req")
	if err := os.MkdirAll(reqDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(reqDir, "stale.json")
	if err := os.WriteFile(old, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	applySeams(t, seams{envMap: map[string]string{}})
	r := Run(ctx, Env{RuntimeDir: dir, BinaryPath: os.Args[0]})
	c := findCheck(t, r.Checks, "runtime.dir.req_orphans")
	if c.Status != StatusWarn {
		t.Fatalf("expected warn, got %s (details=%v)", c.Status, c.Details)
	}
}

func TestDoctor_RuntimeReqOrphans_OK(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	reqDir := filepath.Join(dir, "req")
	_ = os.MkdirAll(reqDir, 0o700)
	// Fresh file: not an orphan.
	fresh := filepath.Join(reqDir, "fresh.json")
	if err := os.WriteFile(fresh, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	applySeams(t, seams{envMap: map[string]string{}})
	r := Run(ctx, Env{RuntimeDir: dir, BinaryPath: os.Args[0]})
	c := findCheck(t, r.Checks, "runtime.dir.req_orphans")
	if c.Status != StatusOK {
		t.Fatalf("expected ok, got %s", c.Status)
	}
}

func TestDoctor_UnderMultiplexer_Detected(t *testing.T) {
	for _, key := range underMultiplexerEnvKeys {
		t.Run(key, func(t *testing.T) {
			ctx := context.Background()
			applySeams(t, seams{envMap: map[string]string{key: "1"}})
			r := Run(ctx, Env{BinaryPath: os.Args[0]})
			c := findCheck(t, r.Checks, "WEZSESH_UNDER_MULTIPLEXER")
			if c.Status != StatusFail {
				t.Fatalf("expected fail with %s set, got %s", key, c.Status)
			}
			if !r.Critical {
				t.Fatalf("expected report.Critical=true with multiplexer detected")
			}
		})
	}
}

func TestDoctor_UnderMultiplexer_OK_Native(t *testing.T) {
	ctx := context.Background()
	applySeams(t, seams{envMap: map[string]string{"WEZTERM_PANE": "42"}})
	r := Run(ctx, Env{BinaryPath: os.Args[0]})
	c := findCheck(t, r.Checks, "WEZSESH_UNDER_MULTIPLEXER")
	if c.Status != StatusOK {
		t.Fatalf("expected ok in native env, got %s", c.Status)
	}
}

// =============================================================
// Per-check positive + negative coverage
// =============================================================

func TestBinary_VersionPath(t *testing.T) {
	ctx := context.Background()
	t.Run("OK", func(t *testing.T) {
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "binary.path").Status != StatusOK {
			t.Fatalf("expected ok")
		}
		if findCheck(t, r.Checks, "binary.version").Status != StatusOK {
			t.Fatalf("expected ok")
		}
	})
	t.Run("Missing", func(t *testing.T) {
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{BinaryPath: "/nonexistent/wezsesh-doctor-test"})
		if findCheck(t, r.Checks, "binary.path").Status != StatusFail {
			t.Fatalf("expected fail")
		}
	})
}

func TestBinary_FSNetwork(t *testing.T) {
	ctx := context.Background()
	applySeams(t, seams{envMap: map[string]string{}})
	t.Run("LocalOK", func(t *testing.T) {
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		c := findCheck(t, r.Checks, "binary.fs.network")
		// On the dev machine this is local; could also be skip if path empty.
		if c.Status != StatusOK && c.Status != StatusWarn {
			t.Fatalf("unexpected status %s", c.Status)
		}
	})
	t.Run("EmptySkip", func(t *testing.T) {
		r := Run(ctx, Env{})
		c := findCheck(t, r.Checks, "binary.fs.network")
		if c.Status != StatusSkip {
			t.Fatalf("expected skip with empty path, got %s", c.Status)
		}
	})
}

func TestPlugin_Version(t *testing.T) {
	ctx := context.Background()
	applySeams(t, seams{envMap: map[string]string{}})
	t.Run("OK", func(t *testing.T) {
		r := Run(ctx, Env{PluginVersion: pluginVersionExpected, BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "plugin.version").Status != StatusOK {
			t.Fatal("expected ok")
		}
		if findCheck(t, r.Checks, "version.compatible").Status != StatusOK {
			t.Fatal("expected ok")
		}
	})
	t.Run("MissingSkip", func(t *testing.T) {
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "plugin.version").Status != StatusSkip {
			t.Fatal("expected skip")
		}
		if findCheck(t, r.Checks, "version.compatible").Status != StatusSkip {
			t.Fatal("expected skip")
		}
	})
	t.Run("DriftWarn", func(t *testing.T) {
		r := Run(ctx, Env{PluginVersion: "9.9.9", BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "version.compatible").Status != StatusWarn {
			t.Fatal("expected warn")
		}
	})
}

func TestWezterm_Checks(t *testing.T) {
	ctx := context.Background()
	t.Run("OK", func(t *testing.T) {
		tty := "/dev/ttys001"
		applySeams(t, seams{
			envMap: map[string]string{"WEZTERM_PANE": "1"},
			wezcliFn: func() (wezcliClient, error) {
				return &fakeWezcli{
					listPanes:   []wezcli.Pane{{PaneID: 1, TTYName: &tty}},
					listClients: []wezcli.ClientInfo{{ClientID: "c0"}},
				}, nil
			},
		})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		for _, id := range []string{
			"wezterm.version",
			"wezterm.cli.list", "wezterm.cli.list-clients",
			"wezterm.cli.tty_name", "wezterm.pane.env",
		} {
			c := findCheck(t, r.Checks, id)
			if c.Status != StatusOK {
				t.Fatalf("%s expected ok, got %s (%v)", id, c.Status, c.Details)
			}
		}
		// wezterm.lua_version is informational-only (see T-DOC-045);
		// the binary cannot probe Lua _VERSION today, so the row is
		// always Skip with the §16.4 CI gate as the load-bearing
		// assertion.
		c := findCheck(t, r.Checks, "wezterm.lua_version")
		if c.Status != StatusSkip {
			t.Fatalf("wezterm.lua_version expected skip, got %s", c.Status)
		}
	})
	t.Run("CliUnreachable", func(t *testing.T) {
		applySeams(t, seams{
			envMap: map[string]string{},
			wezcliFn: func() (wezcliClient, error) {
				return &fakeWezcli{probeErr: errors.New("mux unreachable")}, nil
			},
		})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "wezterm.version").Status != StatusFail {
			t.Fatal("expected fail")
		}
		if findCheck(t, r.Checks, "wezterm.lua_version").Status != StatusSkip {
			t.Fatal("expected skip")
		}
		if findCheck(t, r.Checks, "wezterm.pane.env").Status != StatusFail {
			t.Fatal("expected fail (WEZTERM_PANE absent)")
		}
	})
	t.Run("WezcliFactoryFail", func(t *testing.T) {
		applySeams(t, seams{
			envMap: map[string]string{},
			wezcliFn: func() (wezcliClient, error) {
				return nil, wezcli.ErrWeztermNotFound
			},
		})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "wezterm.version").Status != StatusSkip {
			t.Fatal("expected skip")
		}
	})
	t.Run("ListErrFail", func(t *testing.T) {
		applySeams(t, seams{
			envMap: map[string]string{"WEZTERM_PANE": "1"},
			wezcliFn: func() (wezcliClient, error) {
				return &fakeWezcli{listErr: errors.New("nope")}, nil
			},
		})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "wezterm.cli.list").Status != StatusFail {
			t.Fatal("expected fail")
		}
	})
	t.Run("TTYNameMissingWarn", func(t *testing.T) {
		applySeams(t, seams{
			envMap: map[string]string{"WEZTERM_PANE": "1"},
			wezcliFn: func() (wezcliClient, error) {
				return &fakeWezcli{listPanes: []wezcli.Pane{{PaneID: 1, TTYName: nil}}}, nil
			},
		})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "wezterm.cli.tty_name").Status != StatusWarn {
			t.Fatal("expected warn")
		}
	})
	t.Run("ListClientsErrFail", func(t *testing.T) {
		applySeams(t, seams{
			envMap: map[string]string{"WEZTERM_PANE": "1"},
			wezcliFn: func() (wezcliClient, error) {
				return &fakeWezcli{clientsErr: errors.New("oops")}, nil
			},
		})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "wezterm.cli.list-clients").Status != StatusFail {
			t.Fatal("expected fail")
		}
	})
}

// TestWezterm_PaneEnv_Resolves covers the §8.17.1 row "wezterm.pane.env
// ← WEZTERM_PANE set + resolves" (M1 from the T-702 conformance
// review). The base "OK with matching pane" case is already covered by
// TestWezterm_Checks/OK; this exercises the resolution-failure paths.
func TestWezterm_PaneEnv_Resolves(t *testing.T) {
	ctx := context.Background()

	t.Run("MismatchWarn", func(t *testing.T) {
		applySeams(t, seams{
			envMap: map[string]string{"WEZTERM_PANE": "99"},
			wezcliFn: func() (wezcliClient, error) {
				return &fakeWezcli{listPanes: []wezcli.Pane{{PaneID: 1}}}, nil
			},
		})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		c := findCheck(t, r.Checks, "wezterm.pane.env")
		if c.Status != StatusWarn {
			t.Fatalf("expected warn on stale pane id, got %s (%v)", c.Status, c.Details)
		}
	})

	t.Run("NonIntWarn", func(t *testing.T) {
		applySeams(t, seams{
			envMap: map[string]string{"WEZTERM_PANE": "not-an-int"},
			wezcliFn: func() (wezcliClient, error) {
				return &fakeWezcli{listPanes: []wezcli.Pane{{PaneID: 1}}}, nil
			},
		})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		c := findCheck(t, r.Checks, "wezterm.pane.env")
		if c.Status != StatusWarn {
			t.Fatalf("expected warn on non-int pane id, got %s", c.Status)
		}
	})

	t.Run("FactoryFailSkip", func(t *testing.T) {
		applySeams(t, seams{
			envMap: map[string]string{"WEZTERM_PANE": "1"},
			wezcliFn: func() (wezcliClient, error) {
				return nil, wezcli.ErrWeztermNotFound
			},
		})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		c := findCheck(t, r.Checks, "wezterm.pane.env")
		if c.Status != StatusSkip {
			t.Fatalf("expected skip with resolution skipped, got %s", c.Status)
		}
		if got, _ := c.Details["resolved"].(bool); got {
			t.Fatalf("expected resolved=false when factory fails, got details=%v", c.Details)
		}
	})

	t.Run("ListErrSkip", func(t *testing.T) {
		applySeams(t, seams{
			envMap: map[string]string{"WEZTERM_PANE": "1"},
			wezcliFn: func() (wezcliClient, error) {
				return &fakeWezcli{listErr: errors.New("transient")}, nil
			},
		})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		c := findCheck(t, r.Checks, "wezterm.pane.env")
		if c.Status != StatusSkip {
			t.Fatalf("expected skip when cli list errs, got %s", c.Status)
		}
	})
}

func TestSnapshot_Dir_Checks(t *testing.T) {
	ctx := context.Background()
	t.Run("OK", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.MkdirAll(filepath.Join(dir, "workspace"), 0o700)
		_ = os.MkdirAll(filepath.Join(dir, "window"), 0o700)
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{SnapshotDir: dir, BinaryPath: os.Args[0]})
		for _, id := range []string{
			"snapshot.dir.exists", "snapshot.dir.writable", "snapshot.dir.matches.resurrect",
		} {
			c := findCheck(t, r.Checks, id)
			if c.Status != StatusOK {
				t.Fatalf("%s expected ok, got %s (%v)", id, c.Status, c.Details)
			}
		}
	})
	t.Run("MissingDirFail", func(t *testing.T) {
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{SnapshotDir: "/nonexistent/wezsesh-doctor-test", BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "snapshot.dir.exists").Status != StatusFail {
			t.Fatal("expected fail")
		}
		// Writable falls through to skip when dir absent.
		if findCheck(t, r.Checks, "snapshot.dir.writable").Status != StatusSkip {
			t.Fatal("expected skip")
		}
	})
	t.Run("ResurrectMatchWarn", func(t *testing.T) {
		dir := t.TempDir() // empty — no workspace/, no window/
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{SnapshotDir: dir, BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "snapshot.dir.matches.resurrect").Status != StatusWarn {
			t.Fatal("expected warn")
		}
	})
	t.Run("WritableSentinelGone", func(t *testing.T) {
		dir := t.TempDir()
		applySeams(t, seams{envMap: map[string]string{}})
		_ = Run(ctx, Env{SnapshotDir: dir, BinaryPath: os.Args[0]})
		// Sentinel file should be gone after probe.
		if _, err := os.Stat(filepath.Join(dir, ".wezsesh-doctor-write-probe")); !os.IsNotExist(err) {
			t.Fatalf("sentinel left behind: %v", err)
		}
	})
}

func TestSnapshot_FSNetwork(t *testing.T) {
	ctx := context.Background()
	applySeams(t, seams{envMap: map[string]string{}})
	t.Run("LocalOK", func(t *testing.T) {
		dir := t.TempDir()
		r := Run(ctx, Env{SnapshotDir: dir, BinaryPath: os.Args[0]})
		c := findCheck(t, r.Checks, "snapshot.dir.fs.network")
		if c.Status != StatusOK && c.Status != StatusWarn {
			t.Fatalf("unexpected %s", c.Status)
		}
	})
	t.Run("EmptySkip", func(t *testing.T) {
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "snapshot.dir.fs.network").Status != StatusSkip {
			t.Fatal("expected skip")
		}
	})
}

func TestSnapshot_Count_Name_Argv_Encryption(t *testing.T) {
	ctx := context.Background()
	t.Run("OK", func(t *testing.T) {
		dir := t.TempDir()
		applySeams(t, seams{
			envMap: map[string]string{},
			snapshotsFn: func(string) (snapshotsRepo, error) {
				return &fakeRepo{
					entries: []snapshots.Entry{
						{Name: "alpha", State: simpleState("bash")},
					},
					sniff: map[string]snapshots.Encryption{"alpha": snapshots.EncryptionPlaintext},
				}, nil
			},
		})
		r := Run(ctx, Env{SnapshotDir: dir, BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "snapshot.count").Status != StatusOK {
			t.Fatal("count expected ok")
		}
		if findCheck(t, r.Checks, "snapshot.name.validation").Status != StatusOK {
			t.Fatal("name expected ok")
		}
		if findCheck(t, r.Checks, "snapshot.argv.allowlist.coverage").Status != StatusOK {
			t.Fatal("argv expected ok")
		}
		if findCheck(t, r.Checks, "snapshot.encryption.detected").Status != StatusOK {
			t.Fatal("encryption expected ok")
		}
	})
	t.Run("BadName", func(t *testing.T) {
		dir := t.TempDir()
		applySeams(t, seams{
			envMap: map[string]string{},
			snapshotsFn: func(string) (snapshotsRepo, error) {
				return &fakeRepo{entries: []snapshots.Entry{{Name: "bad\nworkspace"}}}, nil
			},
		})
		r := Run(ctx, Env{SnapshotDir: dir, BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "snapshot.name.validation").Status != StatusWarn {
			t.Fatal("expected warn")
		}
	})
	t.Run("ArgvNotAllowed", func(t *testing.T) {
		dir := t.TempDir()
		applySeams(t, seams{
			envMap: map[string]string{},
			snapshotsFn: func(string) (snapshotsRepo, error) {
				return &fakeRepo{entries: []snapshots.Entry{
					{Name: "x", State: simpleState("/usr/bin/totally-bogus-binary-doctor")},
				}}, nil
			},
		})
		r := Run(ctx, Env{SnapshotDir: dir, BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "snapshot.argv.allowlist.coverage").Status != StatusWarn {
			t.Fatal("expected warn")
		}
	})
	t.Run("EncryptionUnknown", func(t *testing.T) {
		dir := t.TempDir()
		applySeams(t, seams{
			envMap: map[string]string{},
			snapshotsFn: func(string) (snapshotsRepo, error) {
				return &fakeRepo{
					entries: []snapshots.Entry{{Name: "x"}},
					sniff:   map[string]snapshots.Encryption{"x": snapshots.EncryptionUnknown},
				}, nil
			},
		})
		r := Run(ctx, Env{SnapshotDir: dir, BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "snapshot.encryption.detected").Status != StatusWarn {
			t.Fatal("expected warn")
		}
	})
	t.Run("EmptyDirSkip", func(t *testing.T) {
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		for _, id := range []string{
			"snapshot.count", "snapshot.name.validation",
			"snapshot.argv.allowlist.coverage", "snapshot.encryption.detected",
		} {
			if findCheck(t, r.Checks, id).Status != StatusSkip {
				t.Fatalf("%s expected skip", id)
			}
		}
	})
}

func simpleState(prog string) *snapshots.WorkspaceState {
	name := prog
	return &snapshots.WorkspaceState{
		WindowStates: []snapshots.WindowState{{
			Tabs: []snapshots.TabState{{
				PaneTree: &snapshots.PaneTree{
					Process: &snapshots.LocalProcInfo{
						Name: &name,
						Argv: []string{prog},
					},
				},
			}},
		}},
	}
}

func TestSnapshot_PinConsistency_StateOpenerSkip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	applySeams(t, seams{
		envMap: map[string]string{},
		stateFn: func(ctx context.Context, dir string) ([]string, error) {
			return nil, errors.New("state open failed")
		},
	})
	r := Run(ctx, Env{StateDir: dir, SnapshotDir: dir, BinaryPath: os.Args[0]})
	c := findCheck(t, r.Checks, "snapshot.pin.consistency")
	if c.Status != StatusSkip {
		t.Fatalf("expected skip, got %s", c.Status)
	}
}

func TestState_Data_Dirs(t *testing.T) {
	ctx := context.Background()
	t.Run("OK", func(t *testing.T) {
		stateDir := t.TempDir()
		dataDir := t.TempDir()
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{StateDir: stateDir, DataDir: dataDir, BinaryPath: os.Args[0]})
		for _, id := range []string{
			"state.dir.exists", "state.dir.writable",
			"data.dir.exists", "data.dir.writable",
		} {
			if findCheck(t, r.Checks, id).Status != StatusOK {
				t.Fatalf("%s expected ok", id)
			}
		}
	})
	t.Run("MissingFail", func(t *testing.T) {
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{
			StateDir:   "/nonexistent/wezsesh-doctor-state",
			DataDir:    "/nonexistent/wezsesh-doctor-data",
			BinaryPath: os.Args[0],
		})
		if findCheck(t, r.Checks, "state.dir.exists").Status != StatusFail {
			t.Fatal("expected fail")
		}
		if findCheck(t, r.Checks, "data.dir.exists").Status != StatusFail {
			t.Fatal("expected fail")
		}
	})
	t.Run("EmptySkip", func(t *testing.T) {
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		for _, id := range []string{
			"state.dir.exists", "data.dir.exists",
			"state.dir.fs.network", "data.dir.fs.network",
		} {
			if findCheck(t, r.Checks, id).Status != StatusSkip {
				t.Fatalf("%s expected skip", id)
			}
		}
	})
}

func TestTrust_Dir(t *testing.T) {
	ctx := context.Background()
	t.Run("OK", func(t *testing.T) {
		dir := t.TempDir()
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{TrustDir: dir, BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "trust.dir.exists").Status != StatusOK {
			t.Fatal("expected ok")
		}
		if findCheck(t, r.Checks, "trust.count").Status != StatusOK {
			t.Fatal("expected ok")
		}
		if findCheck(t, r.Checks, "trust.orphans").Status != StatusOK {
			t.Fatal("expected ok")
		}
	})
	t.Run("SymlinkFail", func(t *testing.T) {
		base := t.TempDir()
		target := filepath.Join(base, "real")
		_ = os.MkdirAll(target, 0o700)
		link := filepath.Join(base, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink unsupported on this fs: %v", err)
		}
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{TrustDir: link, BinaryPath: os.Args[0]})
		c := findCheck(t, r.Checks, "trust.dir.exists")
		if c.Status != StatusFail {
			t.Fatalf("expected fail on symlink, got %s", c.Status)
		}
	})
	t.Run("MissingWarn", func(t *testing.T) {
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{TrustDir: "/nonexistent/wezsesh-doctor-trust", BinaryPath: os.Args[0]})
		c := findCheck(t, r.Checks, "trust.dir.exists")
		if c.Status != StatusWarn {
			t.Fatalf("expected warn, got %s", c.Status)
		}
	})
	t.Run("OrphanWarn", func(t *testing.T) {
		dir := t.TempDir()
		// Write a fake trust file pointing at a non-existent path.
		hash := strings.Repeat("a", 64)
		body := []byte(`{"path":"/nonexistent/wezsesh-doctor-orphan-target"}`)
		if err := os.WriteFile(filepath.Join(dir, hash), body, 0o600); err != nil {
			t.Fatal(err)
		}
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{TrustDir: dir, BinaryPath: os.Args[0]})
		c := findCheck(t, r.Checks, "trust.orphans")
		if c.Status != StatusWarn {
			t.Fatalf("expected warn, got %s (details=%v)", c.Status, c.Details)
		}
	})
	t.Run("EmptySkip", func(t *testing.T) {
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		for _, id := range []string{"trust.dir.exists", "trust.count", "trust.orphans"} {
			if findCheck(t, r.Checks, id).Status != StatusSkip {
				t.Fatalf("%s expected skip", id)
			}
		}
	})
}

func TestRuntime_Dir_Permissions_SunPath(t *testing.T) {
	ctx := context.Background()
	t.Run("OK", func(t *testing.T) {
		// Use a short path so SUN_PATH budget passes on darwin where
		// $TMPDIR alone is ~50 B and t.TempDir() pushes us over the
		// 104 B budget.
		dir, err := os.MkdirTemp("/tmp", "wzd-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		_ = os.Chmod(dir, 0o700)
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{RuntimeDir: dir, BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "runtime.dir.exists").Status != StatusOK {
			t.Fatal("expected ok")
		}
		if findCheck(t, r.Checks, "runtime.dir.permissions").Status != StatusOK {
			t.Fatalf("expected ok")
		}
		if findCheck(t, r.Checks, "runtime.dir.sun_path_budget").Status != StatusOK {
			t.Fatalf("expected ok, got %s", findCheck(t, r.Checks, "runtime.dir.sun_path_budget").Status)
		}
	})
	t.Run("PermsWarn", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.Chmod(dir, 0o755)
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{RuntimeDir: dir, BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "runtime.dir.permissions").Status != StatusWarn {
			t.Fatalf("expected warn")
		}
	})
	t.Run("SunPathOverflowFail", func(t *testing.T) {
		// Build an over-budget path string. We don't actually need
		// the dir to exist for the budget check; the dir-existence
		// check failing in parallel is fine.
		long := "/" + strings.Repeat("x", 200)
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{RuntimeDir: long, BinaryPath: os.Args[0]})
		c := findCheck(t, r.Checks, "runtime.dir.sun_path_budget")
		if c.Status != StatusFail {
			t.Fatalf("expected fail, got %s", c.Status)
		}
	})
	t.Run("EmptySkip", func(t *testing.T) {
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "runtime.dir.permissions").Status != StatusSkip {
			t.Fatal("expected skip")
		}
		if findCheck(t, r.Checks, "runtime.dir.sun_path_budget").Status != StatusSkip {
			t.Fatal("expected skip")
		}
	})
}

func TestHome_Consistency(t *testing.T) {
	ctx := context.Background()
	t.Run("OK", func(t *testing.T) {
		applySeams(t, seams{
			envMap: map[string]string{"HOME": "/tmp/x"},
			homeFn: func() (string, error) { return "/tmp/x", nil },
		})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "home.consistency").Status != StatusOK {
			t.Fatal("expected ok")
		}
	})
	t.Run("DriftWarn", func(t *testing.T) {
		applySeams(t, seams{
			envMap: map[string]string{"HOME": "/tmp/x"},
			homeFn: func() (string, error) { return "/tmp/y", nil },
		})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "home.consistency").Status != StatusWarn {
			t.Fatal("expected warn")
		}
	})
	t.Run("BothEmptySkip", func(t *testing.T) {
		applySeams(t, seams{
			envMap: map[string]string{},
			homeFn: func() (string, error) { return "", nil },
		})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "home.consistency").Status != StatusSkip {
			t.Fatal("expected skip when both env and resolved are empty")
		}
	})
}

func TestLinux_Kernel_Version(t *testing.T) {
	ctx := context.Background()
	applySeams(t, seams{envMap: map[string]string{}})
	r := Run(ctx, Env{BinaryPath: os.Args[0]})
	c := findCheck(t, r.Checks, "linux.kernel.version")
	if runtime.GOOS == "linux" {
		// Could be ok or warn depending on uname availability.
		if c.Status != StatusOK && c.Status != StatusWarn {
			t.Fatalf("unexpected status on linux: %s", c.Status)
		}
	} else {
		if c.Status != StatusSkip {
			t.Fatalf("expected skip off-linux, got %s", c.Status)
		}
	}
}

func TestNerdfont_Detected(t *testing.T) {
	ctx := context.Background()
	t.Run("On", func(t *testing.T) {
		applySeams(t, seams{envMap: map[string]string{"WEZSESH_NERDFONT": "1"}})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "nerdfont.detected").Status != StatusOK {
			t.Fatal("expected ok")
		}
	})
	t.Run("Off", func(t *testing.T) {
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "nerdfont.detected").Status != StatusSkip {
			t.Fatal("expected skip")
		}
	})
}

func TestPathpicker_Tools(t *testing.T) {
	ctx := context.Background()
	t.Run("Both", func(t *testing.T) {
		applySeams(t, seams{
			envMap:     map[string]string{},
			lookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "pathpicker.zoxide").Status != StatusOK {
			t.Fatal("expected ok")
		}
		if findCheck(t, r.Checks, "pathpicker.fd").Status != StatusOK {
			t.Fatal("expected ok")
		}
	})
	t.Run("Neither", func(t *testing.T) {
		applySeams(t, seams{
			envMap:     map[string]string{},
			lookPathFn: func(name string) (string, error) { return "", errors.New("not found") },
		})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		if findCheck(t, r.Checks, "pathpicker.zoxide").Status != StatusSkip {
			t.Fatal("expected skip")
		}
		if findCheck(t, r.Checks, "pathpicker.fd").Status != StatusSkip {
			t.Fatal("expected skip")
		}
	})
}

func TestEncryption_Agent_Responsive(t *testing.T) {
	ctx := context.Background()
	t.Run("Fast", func(t *testing.T) {
		applySeams(t, seams{
			envMap: map[string]string{},
			gpgFn:  func(context.Context) (time.Duration, bool) { return 50 * time.Millisecond, true },
			ageFn:  func(context.Context) (time.Duration, bool) { return 0, false },
		})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		c := findCheck(t, r.Checks, "encryption.agent.responsive")
		if c.Status != StatusOK {
			t.Fatalf("expected ok, got %s", c.Status)
		}
	})
	t.Run("Slow", func(t *testing.T) {
		applySeams(t, seams{
			envMap: map[string]string{},
			gpgFn:  func(context.Context) (time.Duration, bool) { return 3 * time.Second, true },
			ageFn:  func(context.Context) (time.Duration, bool) { return 0, false },
		})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		c := findCheck(t, r.Checks, "encryption.agent.responsive")
		if c.Status != StatusWarn {
			t.Fatalf("expected warn, got %s", c.Status)
		}
	})
	t.Run("NeitherSkip", func(t *testing.T) {
		applySeams(t, seams{
			envMap: map[string]string{},
			gpgFn:  func(context.Context) (time.Duration, bool) { return 0, false },
			ageFn:  func(context.Context) (time.Duration, bool) { return 0, false },
		})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		c := findCheck(t, r.Checks, "encryption.agent.responsive")
		if c.Status != StatusSkip {
			t.Fatalf("expected skip, got %s", c.Status)
		}
	})
}

func TestLog_Recent_Errors(t *testing.T) {
	ctx := context.Background()
	t.Run("WithErrorWarn", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "wezsesh.log")
		recent := time.Now().UTC().Format(time.RFC3339Nano)
		body := fmt.Sprintf(`{"time":%q,"level":"INFO","msg":"hi"}`+"\n"+
			`{"time":%q,"level":"ERROR","msg":"boom"}`+"\n", recent, recent)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{StateDir: dir, BinaryPath: os.Args[0]})
		c := findCheck(t, r.Checks, "log.recent_errors")
		if c.Status != StatusWarn {
			t.Fatalf("expected warn, got %s (details=%v)", c.Status, c.Details)
		}
	})
	t.Run("NoErrorsOK", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "wezsesh.log")
		recent := time.Now().UTC().Format(time.RFC3339Nano)
		body := fmt.Sprintf(`{"time":%q,"level":"INFO","msg":"hi"}`+"\n", recent)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{StateDir: dir, BinaryPath: os.Args[0]})
		c := findCheck(t, r.Checks, "log.recent_errors")
		if c.Status != StatusOK {
			t.Fatalf("expected ok, got %s", c.Status)
		}
	})
	t.Run("AbsentSkip", func(t *testing.T) {
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{StateDir: t.TempDir(), BinaryPath: os.Args[0]})
		c := findCheck(t, r.Checks, "log.recent_errors")
		if c.Status != StatusSkip {
			t.Fatalf("expected skip, got %s", c.Status)
		}
	})
}

// TestReport_Warnings exercises the §8.20 contract that
// `wezsesh doctor` exits non-zero on any non-OK status (Critical or
// Warnings). Critical retains its fail-only meaning; Warnings is the
// new field surfaced by Run.
func TestReport_Warnings(t *testing.T) {
	ctx := context.Background()
	t.Run("AllOK", func(t *testing.T) {
		stateDir := t.TempDir()
		dataDir := t.TempDir()
		applySeams(t, seams{
			envMap: map[string]string{},
			homeFn: func() (string, error) { return "", nil },
			lookPathFn: func(string) (string, error) {
				return "", errors.New("not found")
			},
			gpgFn: func(context.Context) (time.Duration, bool) { return 0, false },
			ageFn: func(context.Context) (time.Duration, bool) { return 0, false },
		})
		// All paths empty/clean: every check either OK or Skip.
		r := Run(ctx, Env{
			StateDir:   stateDir,
			DataDir:    dataDir,
			BinaryPath: os.Args[0],
		})
		// We only assert Warnings is false when no Warn-status check
		// fires. (Some statuses on this machine might still warn —
		// e.g. binary.fs.network on a network FS — so we filter.)
		sawWarn := false
		for _, c := range r.Checks {
			if c.Status == StatusWarn {
				sawWarn = true
				break
			}
		}
		if r.Warnings != sawWarn {
			t.Fatalf("Warnings=%v but sawWarn=%v", r.Warnings, sawWarn)
		}
	})

	t.Run("WarnFlipsWarnings", func(t *testing.T) {
		// Force a Warn via the snapshot.dir.matches.resurrect path.
		dir := t.TempDir() // empty — no workspace/, no window/
		applySeams(t, seams{envMap: map[string]string{}})
		r := Run(ctx, Env{SnapshotDir: dir, BinaryPath: os.Args[0]})
		if !r.Warnings {
			t.Fatalf("expected Warnings=true with at least one Warn check")
		}
	})

	t.Run("FailFlipsCritical_NotJustWarnings", func(t *testing.T) {
		// WEZSESH_UNDER_MULTIPLEXER returns Fail when TMUX is set.
		applySeams(t, seams{envMap: map[string]string{"TMUX": "1"}})
		r := Run(ctx, Env{BinaryPath: os.Args[0]})
		if !r.Critical {
			t.Fatalf("expected Critical=true on Fail")
		}
	})
}

// Sanity: every check listed in §8.17.1 has an entry in the Report.
func TestEveryCheckPresent(t *testing.T) {
	ctx := context.Background()
	applySeams(t, seams{envMap: map[string]string{}})
	r := Run(ctx, Env{BinaryPath: os.Args[0]})
	want := []string{
		"binary.version", "binary.path", "binary.fs.network",
		"plugin.version", "version.compatible",
		"wezterm.version", "wezterm.lua_version",
		"wezterm.cli.list", "wezterm.cli.list-clients", "wezterm.cli.tty_name",
		"wezterm.pane.env",
		"snapshot.dir.exists", "snapshot.dir.writable", "snapshot.dir.fs.network",
		"snapshot.dir.matches.resurrect",
		"snapshot.count", "snapshot.name.validation",
		"snapshot.argv.allowlist.coverage", "snapshot.encryption.detected",
		"snapshot.pin.consistency",
		"state.dir.exists", "state.dir.writable", "state.dir.fs.network",
		"data.dir.exists", "data.dir.writable", "data.dir.fs.network",
		"trust.dir.exists", "trust.count", "trust.orphans",
		"runtime.dir.exists", "runtime.dir.fs.network",
		"runtime.dir.permissions", "runtime.dir.sun_path_budget",
		"runtime.dir.req_orphans",
		"home.consistency", "linux.kernel.version", "nerdfont.detected",
		"pathpicker.zoxide", "pathpicker.fd",
		"encryption.agent.responsive", "log.recent_errors",
		"config.exclude.regex_validity",
		"WEZSESH_UNDER_MULTIPLEXER",
	}
	got := map[string]bool{}
	for _, c := range r.Checks {
		got[c.ID] = true
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("missing check %q", id)
		}
	}
	if len(r.Checks) != len(want) {
		t.Errorf("check count drift: got %d, want %d", len(r.Checks), len(want))
	}
}

// Sanity: the safefs symlink helper compiles into the binary so the
// test build picks up the same package as the production build.
var _ = safefs.ErrIsSymlink
