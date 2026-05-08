package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Grady-Saccullo/wezsesh/internal/snapshots"
	"github.com/Grady-Saccullo/wezsesh/internal/wezcli"
)

// makeListDeps builds a listDeps wired to in-memory fakes. Tests
// configure the three sources independently so the §13.11 union is
// exercised without touching disk or wezterm.
func makeListDeps(panes []wezcli.Pane, panesErr error,
	entries []snapshots.Entry, entriesErr error,
	livePins []string,
) listDeps {
	savedSet := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		savedSet[e.Name] = struct{}{}
	}
	return listDeps{
		livePanes: func(context.Context) ([]wezcli.Pane, error) {
			return panes, panesErr
		},
		repoEntries: func(context.Context) ([]snapshots.Entry, error) {
			return entries, entriesErr
		},
		repoHas: func(_ context.Context, name string) (bool, error) {
			_, ok := savedSet[name]
			return ok, nil
		},
		livePins: func() []string { return livePins },
	}
}

// TestComputeListRows_UnionSemantics covers the §13.11 union arms:
// live-only, saved (unpinned), saved+sidecar-pinned, live+saved, plus
// a workspace listed in three sources at once.
func TestComputeListRows_UnionSemantics(t *testing.T) {
	mtime := time.Unix(1700_000_000, 0)
	deps := makeListDeps(
		[]wezcli.Pane{
			// Live-only.
			{PaneID: 1, WindowID: 1, Workspace: "alpha"},
			// Live + saved.
			{PaneID: 2, WindowID: 1, Workspace: "beta"},
			// Empty workspace label — must be ignored.
			{PaneID: 3, WindowID: 1, Workspace: ""},
		},
		nil,
		[]snapshots.Entry{
			// Saved + sidecar-pinned.
			{Name: "beta", Mtime: mtime, SidecarOK: true,
				Sidecar: snapshots.Sidecar{Version: 1, Pinned: true}},
			// Saved, unpinned.
			{Name: "gamma", Mtime: mtime, SidecarOK: false},
			// Saved + sidecar present + pinned (no live counterpart).
			{Name: "delta", Mtime: mtime, SidecarOK: true,
				Sidecar: snapshots.Sidecar{Version: 1, Pinned: true}},
		},
		nil,
		// LivePins includes "alpha" (live-only pin, valid) and
		// "delta" (sanity-prune target: it's saved, so the live-pin
		// arm drops it — its pinned status comes from the sidecar).
		[]string{"alpha", "delta"},
	)
	rows, err := computeListRows(context.Background(), deps, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("computeListRows: %v", err)
	}
	want := []listRow{
		{Name: "alpha", Live: true, Saved: false, Pinned: true},
		{Name: "beta", Live: true, Saved: true, Pinned: true},
		{Name: "delta", Live: false, Saved: true, Pinned: true},
		{Name: "gamma", Live: false, Saved: true, Pinned: false},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %v, want %v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row[%d] = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

// TestComputeListRows_LivePinSanityPrune asserts the §13.11 rule:
// a live_pins entry whose name IS in repo.List is dropped from the
// live-pin arm. This is the targeted gate: same name in live_pins +
// sidecar-without-pin → row.Pinned must be FALSE (no leak from the
// stale live-pins arm).
func TestComputeListRows_LivePinSanityPrune(t *testing.T) {
	deps := makeListDeps(
		[]wezcli.Pane{},
		nil,
		[]snapshots.Entry{
			// Saved without sidecar pin.
			{Name: "stale", SidecarOK: true,
				Sidecar: snapshots.Sidecar{Version: 1, Pinned: false}},
		},
		nil,
		[]string{"stale"}, // a stale live-pin entry that should be pruned.
	)
	rows, err := computeListRows(context.Background(), deps, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("computeListRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want 1 row", rows)
	}
	if rows[0].Name != "stale" || rows[0].Saved != true || rows[0].Pinned != false || rows[0].Live != false {
		t.Fatalf("row = %+v, want {stale saved-only}", rows[0])
	}
}

// TestComputeListRows_LivePanesError gracefully degrades to an empty
// live-set; saved + saved-pinned rows still render. Stderr carries a
// warning.
func TestComputeListRows_LivePanesError(t *testing.T) {
	deps := makeListDeps(
		nil, errors.New("wezterm not running"),
		[]snapshots.Entry{
			{Name: "saved", SidecarOK: true,
				Sidecar: snapshots.Sidecar{Version: 1, Pinned: true}},
		},
		nil,
		nil,
	)
	var stderr bytes.Buffer
	rows, err := computeListRows(context.Background(), deps, &stderr)
	if err != nil {
		t.Fatalf("computeListRows: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "saved" || !rows[0].Saved || rows[0].Live {
		t.Fatalf("rows = %v, want single saved-only row", rows)
	}
	if !strings.Contains(stderr.String(), "wezcli list failed") {
		t.Fatalf("stderr = %q, want \"wezcli list failed\" warning", stderr.String())
	}
}

// TestComputeListRows_RepoEntriesError surfaces the snapshot-list error
// to the caller (no graceful degradation — the saved arm is the union's
// authoritative source).
func TestComputeListRows_RepoEntriesError(t *testing.T) {
	deps := makeListDeps(
		nil, nil,
		nil, errors.New("snapshot dir missing"),
		nil,
	)
	_, err := computeListRows(context.Background(), deps, &bytes.Buffer{})
	if err == nil {
		t.Fatalf("computeListRows: want error, got nil")
	}
}

// goldenTextOutput is the exact bytes we expect on stdout for the
// fixture scenario in TestSubcmdList_TextGolden. Kept inline (vs
// testdata/) to keep the diff allowlist tight.
const goldenTextOutput = `LIVE SAVED PIN NAME
*    -     *   alpha
*    *     *   beta
-    *     *   delta
-    *     -   gamma
`

// goldenJSONOutput mirrors the same fixture in JSON. json.Marshal's
// byte output is stable across Go versions for this struct shape.
const goldenJSONOutput = `{"workspaces":[{"name":"alpha","live":true,"saved":false,"pinned":true},{"name":"beta","live":true,"saved":true,"pinned":true},{"name":"delta","live":false,"saved":true,"pinned":true},{"name":"gamma","live":false,"saved":true,"pinned":false}]}
`

// TestSubcmdList_TextGolden drives renderListRows through the full
// fixture and asserts byte-for-byte match against goldenTextOutput.
func TestSubcmdList_TextGolden(t *testing.T) {
	rows := []listRow{
		{Name: "alpha", Live: true, Pinned: true},
		{Name: "beta", Live: true, Saved: true, Pinned: true},
		{Name: "delta", Saved: true, Pinned: true},
		{Name: "gamma", Saved: true},
	}
	var buf bytes.Buffer
	if err := renderListRows(&buf, listFormatText, rows); err != nil {
		t.Fatalf("renderListRows text: %v", err)
	}
	if buf.String() != goldenTextOutput {
		t.Fatalf("text golden mismatch.\n got: %q\nwant: %q", buf.String(), goldenTextOutput)
	}
}

// TestSubcmdList_JSONGolden drives the JSON path against the fixture.
// Asserts byte equality so external parsers can rely on field order +
// no-pretty-print.
func TestSubcmdList_JSONGolden(t *testing.T) {
	rows := []listRow{
		{Name: "alpha", Live: true, Pinned: true},
		{Name: "beta", Live: true, Saved: true, Pinned: true},
		{Name: "delta", Saved: true, Pinned: true},
		{Name: "gamma", Saved: true},
	}
	var buf bytes.Buffer
	if err := renderListRows(&buf, listFormatJSON, rows); err != nil {
		t.Fatalf("renderListRows json: %v", err)
	}
	if buf.String() != goldenJSONOutput {
		t.Fatalf("json golden mismatch.\n got: %q\nwant: %q", buf.String(), goldenJSONOutput)
	}
}

// TestRenderListRows_EmptyText: zero rows → header line only.
func TestRenderListRows_EmptyText(t *testing.T) {
	var buf bytes.Buffer
	if err := renderListRows(&buf, listFormatText, nil); err != nil {
		t.Fatalf("renderListRows: %v", err)
	}
	const want = "LIVE SAVED PIN NAME\n"
	if buf.String() != want {
		t.Fatalf("got %q, want %q", buf.String(), want)
	}
}

// TestRenderListRows_EmptyJSON: zero rows → empty workspaces array, not
// `null`. Downstream JSON consumers depend on .workspaces being a
// (possibly empty) array.
func TestRenderListRows_EmptyJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := renderListRows(&buf, listFormatJSON, nil); err != nil {
		t.Fatalf("renderListRows: %v", err)
	}
	const want = `{"workspaces":[]}` + "\n"
	if buf.String() != want {
		t.Fatalf("got %q, want %q", buf.String(), want)
	}
}

// TestRenderListRows_TextSanitizes: a snapshot name carrying an ANSI
// CSI introducer must NOT reach the writer verbatim — the §15.4
// SanitizeForDisplay layer replaces 0x1B with U+FFFD.
func TestRenderListRows_TextSanitizes(t *testing.T) {
	rows := []listRow{
		{Name: "evil\x1b[2J", Saved: true},
	}
	var buf bytes.Buffer
	if err := renderListRows(&buf, listFormatText, rows); err != nil {
		t.Fatalf("renderListRows: %v", err)
	}
	if strings.ContainsRune(buf.String(), 0x1B) {
		t.Fatalf("text output leaked 0x1B: %q", buf.String())
	}
}

// TestParseListFlags covers the four cases: default, explicit text,
// explicit json, unknown format, and unexpected positional args.
func TestParseListFlags(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{"default", nil, listFormatText, false},
		{"explicit text", []string{"--format", "text"}, listFormatText, false},
		{"explicit json", []string{"--format", "json"}, listFormatJSON, false},
		{"equals form", []string{"--format=json"}, listFormatJSON, false},
		{"unknown format", []string{"--format", "xml"}, "", true},
		{"trailing arg", []string{"--format", "text", "extra"}, "", true},
		{"only positional", []string{"oops"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseListFlags(tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSubcmdList_UnknownFormat: routing through subcmdList surfaces the
// flag-parse rejection on stderr with rc=2 and an empty stdout (no
// config load attempted on the rejection path).
func TestSubcmdList_UnknownFormat(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := subcmdList([]string{"--format", "xml"}, &stdout, &stderr)
	if rc != exitDoctorOrSubcmd {
		t.Fatalf("rc = %d, want %d", rc, exitDoctorOrSubcmd)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "unknown --format") {
		t.Fatalf("stderr = %q, want \"unknown --format\" mention", stderr.String())
	}
}

// TestRun_ListRoute_UnknownFormat exercises the routing layer in run()
// for `wezsesh list --format xml`, asserting rc=2 and no stdout. The
// happy-path live-flow is covered by the unit tests above (subcmdList's
// production wiring needs a config + repo + state; a pure routing test
// only needs to confirm the subcommand is dispatched).
func TestRun_ListRoute_UnknownFormat(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := run([]string{"list", "--format", "xml"}, &stdout, &stderr, testBinarySessionID)
	if rc != exitDoctorOrSubcmd {
		t.Fatalf("rc = %d, want %d", rc, exitDoctorOrSubcmd)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}
