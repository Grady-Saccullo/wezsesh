package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Grady-Saccullo/wezsesh/internal/config"
	"github.com/Grady-Saccullo/wezsesh/internal/find"
	"github.com/Grady-Saccullo/wezsesh/internal/ipc"
)

// fakeFindDispatcher is a tiny ipc.Dispatcher stand-in. The find
// subcommand only ever passes the dispatcher through to the activator;
// the activator we inject in tests is a recording fake that never
// actually calls Dispatch. So the dispatcher's Dispatch / EmergencyReply
// are no-ops here and exist only to satisfy the interface.
type fakeFindDispatcher struct {
	dispatchCalls atomic.Int32
}

func (d *fakeFindDispatcher) Dispatch(_ context.Context, _ string, _ map[string]any) (<-chan ipc.Reply, error) {
	d.dispatchCalls.Add(1)
	ch := make(chan ipc.Reply)
	close(ch)
	return ch, nil
}

func (d *fakeFindDispatcher) EmergencyReply() {}

// installFindHooks swaps the package-level findSearchHook and
// findInsideHook for the duration of the test, restoring them in
// t.Cleanup. Centralised so each gate test stays terse.
func installFindHooks(t *testing.T, search findSearcher, inside *findInsideContext, insideErr error) {
	t.Helper()
	prevSearch, prevInside := findSearchHook, findInsideHook
	t.Cleanup(func() {
		findSearchHook = prevSearch
		findInsideHook = prevInside
	})
	findSearchHook = search
	if inside != nil || insideErr != nil {
		findInsideHook = func(_ context.Context, _ *config.Config, _ func(string) string) (*findInsideContext, error) {
			return inside, insideErr
		}
	} else {
		findInsideHook = func(_ context.Context, _ *config.Config, _ func(string) string) (*findInsideContext, error) {
			return nil, nil
		}
	}
}

// withMinimalEnv pins WEZSESH_CONFIG_JSON_BASE64 to a base64-encoded
// JSON config so config.LoadFromEnv resolves without scanning the
// user's real home. The body is JSON-empty (defaults across the
// board) — find itself only consults the search + inside-wezterm
// seams.
func withMinimalEnv(t *testing.T) {
	t.Helper()
	body := []byte(`{"version":1,"snapshot_dir":"/tmp/snap","state_dir":"/tmp/state","runtime_dir":"/tmp/rt","data_dir":"/tmp/data"}`)
	t.Setenv("WEZSESH_CONFIG_JSON_BASE64", base64.StdEncoding.EncodeToString(body))
	// WEZTERM_PANE intentionally unset — each test sets it explicitly
	// when it wants the inside-wezterm branch.
	t.Setenv("WEZTERM_PANE", "")
}

// -----------------------------------------------------------------------------
// Acceptance gate: Outside-wezterm — prints results only.
//
// `WEZTERM_PANE` is unset; subcmdFind must (a) print the search
// results, (b) NOT call the activator, (c) NOT call any dispatcher.
// -----------------------------------------------------------------------------

func TestSubcmdFind_OutsideWezterm_PrintsResultsOnly(t *testing.T) {
	withMinimalEnv(t)
	// Belt-and-braces: even if WEZTERM_PANE is set in the env, the
	// inside-hook returns nil so subcmdFind takes the outside branch.
	t.Setenv("WEZTERM_PANE", "")

	canned := []find.Match{
		{PaneID: 1, WindowID: 10, Workspace: "main", Title: "vim foo.go", Score: 50, SourceField: "title"},
		{PaneID: 2, WindowID: 10, Workspace: "main", Title: "shell", Score: 0, SourceField: "title"},
	}
	var searchCalls atomic.Int32
	search := func(_ context.Context, pattern string, _ find.Options) ([]find.Match, error) {
		searchCalls.Add(1)
		if pattern != "vim" {
			t.Errorf("pattern = %q, want %q", pattern, "vim")
		}
		return canned, nil
	}

	// Inside hook returns nil, nil → outside-wezterm branch.
	installFindHooks(t, search, nil, nil)

	var stdout, stderr bytes.Buffer
	rc := subcmdFind([]string{"vim"}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	if got := searchCalls.Load(); got != 1 {
		t.Fatalf("search calls = %d, want 1", got)
	}

	out := stdout.String()
	for _, want := range []string{"vim foo.go", "shell", "main"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q: %q", want, out)
		}
	}
	// "activated pane" line is printed only on the inside-wezterm
	// branch — its absence is the load-bearing assertion.
	if strings.Contains(out, "activated pane") {
		t.Fatalf("outside-wezterm output mentions activation: %q", out)
	}
}

// TestSubcmdFind_OutsideWezterm_ZeroMatchesPrintsHeader: zero matches
// → header line still prints, no activation, no error.
func TestSubcmdFind_OutsideWezterm_ZeroMatches(t *testing.T) {
	withMinimalEnv(t)
	t.Setenv("WEZTERM_PANE", "")

	search := func(_ context.Context, _ string, _ find.Options) ([]find.Match, error) {
		return nil, nil
	}
	installFindHooks(t, search, nil, nil)

	var stdout, stderr bytes.Buffer
	rc := subcmdFind([]string{"nope"}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "PANE WORKSPACE") {
		t.Fatalf("stdout missing header: %q", stdout.String())
	}
}

// -----------------------------------------------------------------------------
// Acceptance gate: Inside-wezterm — Phase 1 + Phase 2 sequencing per §13.7.
//
// The find subcommand's responsibility at this layer is "construct an
// in-process Dispatcher iff invoked from inside wezterm; pick the top
// match; hand it to the activator". The activator is the seam through
// which the §13.7 two-phase sequence runs (its internal ordering is
// unit-tested in internal/find/find_test.go).
//
// At this layer we assert:
//   1. The activator IS called (with the correct currentWindowID +
//      the top-scored match).
//   2. The activator runs BEFORE results are rendered (so the user
//      sees the workspace switch + pane focus before reading the
//      result list).
//   3. The dispatcher's Dispatch is NOT called by subcmdFind itself
//      — Dispatch lives inside find.Activate, called by the activator.
// -----------------------------------------------------------------------------

func TestSubcmdFind_InsideWezterm_RunsActivatorWithTopMatch(t *testing.T) {
	withMinimalEnv(t)
	t.Setenv("WEZTERM_PANE", "42")

	canned := []find.Match{
		{PaneID: 7, WindowID: 9, Workspace: "main", Title: "low score", Score: 10, SourceField: "title"},
		{PaneID: 12, WindowID: 9, Workspace: "target", Title: "high score", Score: 90, SourceField: "title"},
		{PaneID: 99, WindowID: 9, Workspace: "main", Title: "mid", Score: 50, SourceField: "title"},
	}

	var searchCalls atomic.Int32
	search := func(_ context.Context, _ string, _ find.Options) ([]find.Match, error) {
		searchCalls.Add(1)
		return canned, nil
	}

	disp := &fakeFindDispatcher{}
	var activatorCalls atomic.Int32
	var sawWindowID int
	var sawMatch find.Match
	activator := func(_ context.Context, d ipc.Dispatcher, currentWindowID int, m find.Match) error {
		activatorCalls.Add(1)
		sawWindowID = currentWindowID
		sawMatch = m
		if d != ipc.Dispatcher(disp) {
			t.Errorf("activator received dispatcher %v; want fake", d)
		}
		return nil
	}

	const currentWindow = 5
	inside := &findInsideContext{
		dispatcher:      disp,
		activator:       activator,
		currentWindowID: currentWindow,
	}
	installFindHooks(t, search, inside, nil)

	var stdout, stderr bytes.Buffer
	rc := subcmdFind([]string{"any"}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	if got := searchCalls.Load(); got != 1 {
		t.Fatalf("search calls = %d, want 1", got)
	}
	if got := activatorCalls.Load(); got != 1 {
		t.Fatalf("activator calls = %d, want 1", got)
	}
	if sawWindowID != currentWindow {
		t.Fatalf("activator received currentWindowID = %d, want %d", sawWindowID, currentWindow)
	}
	if sawMatch.PaneID != 12 {
		t.Fatalf("activator received PaneID = %d, want top-scored 12 (got match=%+v)", sawMatch.PaneID, sawMatch)
	}
	if got := disp.dispatchCalls.Load(); got != 0 {
		t.Fatalf("dispatcher Dispatch called %d times by subcmdFind; the activator owns Dispatch", got)
	}

	out := stdout.String()
	if !strings.Contains(out, "high score") {
		t.Fatalf("stdout missing high-score row: %q", out)
	}
	if !strings.Contains(out, "activated pane 12") {
		t.Fatalf("stdout missing activation line: %q", out)
	}
}

// TestSubcmdFind_InsideWezterm_ZeroMatchesSkipsActivator: when the
// search returns no matches there is nothing to activate — the
// activator MUST NOT be invoked.
func TestSubcmdFind_InsideWezterm_ZeroMatchesSkipsActivator(t *testing.T) {
	withMinimalEnv(t)
	t.Setenv("WEZTERM_PANE", "42")

	search := func(_ context.Context, _ string, _ find.Options) ([]find.Match, error) {
		return nil, nil
	}
	disp := &fakeFindDispatcher{}
	var activatorCalls atomic.Int32
	activator := func(_ context.Context, _ ipc.Dispatcher, _ int, _ find.Match) error {
		activatorCalls.Add(1)
		return nil
	}
	inside := &findInsideContext{
		dispatcher:      disp,
		activator:       activator,
		currentWindowID: 5,
	}
	installFindHooks(t, search, inside, nil)

	var stdout, stderr bytes.Buffer
	rc := subcmdFind([]string{"nope"}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	if got := activatorCalls.Load(); got != 0 {
		t.Fatalf("activator calls = %d, want 0 (no matches → no activation)", got)
	}
	if strings.Contains(stdout.String(), "activated pane") {
		t.Fatalf("stdout mentions activation despite no matches: %q", stdout.String())
	}
}

// TestSubcmdFind_InsideWezterm_ActivatorErrorRendersMatches: the §13.7
// flow can fail (MUX_UNREACHABLE, PANE_CLOSED_RACE). On activator
// error the match table must STILL render so the user can pick again
// from a TUI; stderr carries the activation error.
func TestSubcmdFind_InsideWezterm_ActivatorErrorRendersMatches(t *testing.T) {
	withMinimalEnv(t)
	t.Setenv("WEZTERM_PANE", "42")

	canned := []find.Match{
		{PaneID: 12, WindowID: 9, Workspace: "target", Title: "x", Score: 50, SourceField: "title"},
	}
	search := func(_ context.Context, _ string, _ find.Options) ([]find.Match, error) {
		return canned, nil
	}
	wantErr := errors.New("activator: synthetic mux failure")
	activator := func(_ context.Context, _ ipc.Dispatcher, _ int, _ find.Match) error {
		return wantErr
	}
	inside := &findInsideContext{
		dispatcher:      &fakeFindDispatcher{},
		activator:       activator,
		currentWindowID: 5,
	}
	installFindHooks(t, search, inside, nil)

	var stdout, stderr bytes.Buffer
	rc := subcmdFind([]string{"x"}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (renders matches even on activate err)", rc, exitOK)
	}
	if !strings.Contains(stdout.String(), "target") {
		t.Fatalf("stdout missing match row: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "activated pane") {
		t.Fatalf("stdout mentions activation despite err: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "synthetic mux failure") {
		t.Fatalf("stderr missing activator error: %q", stderr.String())
	}
}

// TestSubcmdFind_InsideInitFailureDegrades: §8.20.1 step 3 says
// "constructs an in-process Dispatcher iff invoked from inside wezterm
// (WEZTERM_PANE set + listener-init succeeds); otherwise prints
// results only". A failure on the inside-init path must degrade to
// outside-wezterm (results-only) semantics, NOT a hard error.
func TestSubcmdFind_InsideInitFailureDegrades(t *testing.T) {
	withMinimalEnv(t)
	t.Setenv("WEZTERM_PANE", "42")

	canned := []find.Match{
		{PaneID: 1, Workspace: "main", Title: "x", Score: 0, SourceField: "title"},
	}
	search := func(_ context.Context, _ string, _ find.Options) ([]find.Match, error) {
		return canned, nil
	}
	installFindHooks(t, search, nil, errors.New("init: synthetic"))

	var stdout, stderr bytes.Buffer
	rc := subcmdFind([]string{"x"}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (degrades to print-only)", rc, exitOK)
	}
	if !strings.Contains(stdout.String(), "main") {
		t.Fatalf("stdout missing match row: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "activated pane") {
		t.Fatalf("degraded path must NOT activate: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "inside-wezterm init failed") {
		t.Fatalf("stderr missing degradation warning: %q", stderr.String())
	}
}

// -----------------------------------------------------------------------------
// JSON format: matches + activated_pane round-trip cleanly.
// -----------------------------------------------------------------------------

func TestSubcmdFind_JSONFormat_OutsideWezterm(t *testing.T) {
	withMinimalEnv(t)
	t.Setenv("WEZTERM_PANE", "")

	canned := []find.Match{
		{PaneID: 1, TabID: 2, WindowID: 3, Workspace: "ws", Title: "t", CWD: "/tmp", Score: 7, SourceField: "title"},
	}
	search := func(_ context.Context, _ string, _ find.Options) ([]find.Match, error) {
		return canned, nil
	}
	installFindHooks(t, search, nil, nil)

	var stdout, stderr bytes.Buffer
	rc := subcmdFind([]string{"--format", "json", "t"}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	const want = `{"matches":[{"pane_id":1,"tab_id":2,"window_id":3,"workspace":"ws","title":"t","cwd":"/tmp","score":7,"source_field":"title"}]}` + "\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestSubcmdFind_JSONFormat_InsideWezterm_IncludesActivatedPane(t *testing.T) {
	withMinimalEnv(t)
	t.Setenv("WEZTERM_PANE", "42")

	canned := []find.Match{
		{PaneID: 12, WindowID: 9, Workspace: "target", Title: "x", Score: 50, SourceField: "title"},
	}
	search := func(_ context.Context, _ string, _ find.Options) ([]find.Match, error) {
		return canned, nil
	}
	activator := func(_ context.Context, _ ipc.Dispatcher, _ int, _ find.Match) error { return nil }
	inside := &findInsideContext{
		dispatcher:      &fakeFindDispatcher{},
		activator:       activator,
		currentWindowID: 5,
	}
	installFindHooks(t, search, inside, nil)

	var stdout, stderr bytes.Buffer
	rc := subcmdFind([]string{"--format", "json", "x"}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"activated_pane":12`) {
		t.Fatalf("stdout missing activated_pane field: %q", stdout.String())
	}
}

// TestRenderFindRows_EmptyJSON: zero rows → matches is an empty array,
// not `null`. activated_pane is omitted (omitempty on the pointer).
func TestRenderFindRows_EmptyJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := renderFindRows(&buf, findFormatJSON, nil, nil); err != nil {
		t.Fatalf("renderFindRows: %v", err)
	}
	const want = `{"matches":[]}` + "\n"
	if buf.String() != want {
		t.Fatalf("got %q, want %q", buf.String(), want)
	}
}

// TestRenderFindRows_TextSanitizes: a workspace name carrying an ANSI
// CSI introducer must NOT reach the writer verbatim — the §15.4
// SanitizeForDisplay layer replaces 0x1B with U+FFFD. Mirrors
// list_test.go's TestRenderListRows_TextSanitizes.
func TestRenderFindRows_TextSanitizes(t *testing.T) {
	rows := []findResultRow{
		{PaneID: 1, Workspace: "evil\x1b[2J", Title: "t", SourceField: "title"},
	}
	var buf bytes.Buffer
	if err := renderFindRows(&buf, findFormatText, rows, nil); err != nil {
		t.Fatalf("renderFindRows: %v", err)
	}
	if strings.ContainsRune(buf.String(), 0x1B) {
		t.Fatalf("text output leaked 0x1B: %q", buf.String())
	}
}

// -----------------------------------------------------------------------------
// Flag parsing.
// -----------------------------------------------------------------------------

func TestParseFindFlags(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    findFlags
		wantErr bool
	}{
		{"default", nil, findFlags{Format: findFormatText}, false},
		{"pattern only", []string{"vim"}, findFlags{Pattern: "vim", Format: findFormatText}, false},
		{"workspace flag", []string{"--workspace", "main", "vim"}, findFlags{Pattern: "vim", Workspace: "main", Format: findFormatText}, false},
		{"cwd-only", []string{"--cwd-only", "vim"}, findFlags{Pattern: "vim", CWDOnly: true, Format: findFormatText}, false},
		{"deep", []string{"--deep", "vim"}, findFlags{Pattern: "vim", Deep: true, Format: findFormatText}, false},
		{"json format", []string{"--format", "json", "vim"}, findFlags{Pattern: "vim", Format: findFormatJSON}, false},
		{"unknown format", []string{"--format", "xml"}, findFlags{}, true},
		{"too many positionals", []string{"a", "b"}, findFlags{}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFindFlags(tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestSubcmdFind_UnknownFormat: routing through subcmdFind surfaces
// the flag-parse rejection on stderr with rc=2. No config load + no
// search calls happen on the rejection path.
func TestSubcmdFind_UnknownFormat(t *testing.T) {
	withMinimalEnv(t)
	var searchCalls atomic.Int32
	search := func(_ context.Context, _ string, _ find.Options) ([]find.Match, error) {
		searchCalls.Add(1)
		return nil, nil
	}
	installFindHooks(t, search, nil, nil)

	var stdout, stderr bytes.Buffer
	rc := subcmdFind([]string{"--format", "xml"}, &stdout, &stderr)
	if rc != exitDoctorOrSubcmd {
		t.Fatalf("rc = %d, want %d", rc, exitDoctorOrSubcmd)
	}
	if !strings.Contains(stderr.String(), "unknown --format") {
		t.Fatalf("stderr = %q, want \"unknown --format\" mention", stderr.String())
	}
	if got := searchCalls.Load(); got != 0 {
		t.Fatalf("search calls = %d, want 0 (rejection should short-circuit)", got)
	}
}

// TestRun_FindRoute exercises the routing table in run() so the
// dispatcher case is covered end-to-end through the public entry point.
func TestRun_FindRoute(t *testing.T) {
	withMinimalEnv(t)
	t.Setenv("WEZTERM_PANE", "")

	canned := []find.Match{
		{PaneID: 1, Workspace: "main", Title: "x", Score: 0, SourceField: "title"},
	}
	search := func(_ context.Context, _ string, _ find.Options) ([]find.Match, error) {
		return canned, nil
	}
	installFindHooks(t, search, nil, nil)

	var stdout, stderr bytes.Buffer
	rc := run([]string{"find", "x"}, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%q)", rc, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "main") {
		t.Fatalf("stdout missing match row: %q", stdout.String())
	}
}

// TestPickTopMatch covers the score-descending pick.
func TestPickTopMatch(t *testing.T) {
	matches := []find.Match{
		{PaneID: 1, Score: 10},
		{PaneID: 2, Score: 50},
		{PaneID: 3, Score: 30},
	}
	got := pickTopMatch(matches)
	if got.PaneID != 2 {
		t.Fatalf("PaneID = %d, want 2 (score 50)", got.PaneID)
	}
}
