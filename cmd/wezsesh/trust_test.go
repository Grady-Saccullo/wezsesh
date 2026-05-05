package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Grady-Saccullo/wezsesh/internal/trust"
)

// makeTrustDeps builds a trustDeps wired to a real *trust.Store backed
// by a tempdir plus an in-memory readSidecar / resolveWorkspaceCwd. Tests
// call this so the §13.5 trust-store invariants (length-prefixed hash,
// symlink-Lstat, atomic write) are exercised end-to-end without needing
// the wezterm CLI or a real project sidecar on disk.
func makeTrustDeps(t *testing.T, sidecars map[string][]byte, panes map[string]string) (trustDeps, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := trust.Open(context.Background(), filepath.Join(dir, "allow"), nil)
	if err != nil {
		t.Fatalf("trust.Open: %v", err)
	}
	deps := trustDeps{
		store: store,
		readSidecar: func(absPath string) ([]byte, error) {
			b, ok := sidecars[absPath]
			if !ok {
				return nil, os.ErrNotExist
			}
			return b, nil
		},
		resolveWorkspaceCwd: func(_ context.Context, name string) (string, bool, error) {
			cwd, ok := panes[name]
			return cwd, ok, nil
		},
	}
	return deps, dir
}

// makeSidecarBytes builds a JSON sidecar body with the given hooks. A
// nil pointer means "field absent"; the empty string means "field
// present and empty".
func makeSidecarBytes(t *testing.T, onCreate, onRestore *string) []byte {
	t.Helper()
	type body struct {
		Version   int     `json:"version"`
		OnCreate  *string `json:"on_create,omitempty"`
		OnRestore *string `json:"on_restore,omitempty"`
	}
	b, err := json.Marshal(body{Version: 1, OnCreate: onCreate, OnRestore: onRestore})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

// TestParseTrustFlags covers happy + sad paths for each mode.
func TestParseTrustFlags(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantMode string
		wantErr  bool
	}{
		{"approve by name", []string{"my-workspace"}, "approve", false},
		{"approve by --path", []string{"--path", "/abs/proj"}, "approve", false},
		{"approve by --sidecar", []string{"--sidecar", "/abs/proj/.wezsesh.json"}, "approve", false},
		{"approve no args", []string{}, "", true},
		{"approve name + path conflict", []string{"--path", "/abs", "name"}, "", true},
		{"approve path + sidecar conflict", []string{"--path", "/abs", "--sidecar", "/abs/x"}, "", true},

		{"list happy", []string{"--list"}, "list", false},
		{"list with positional", []string{"--list", "name"}, "", true},
		{"list with --path", []string{"--list", "--path", "/x"}, "", true},

		{"prune happy", []string{"--prune"}, "prune", false},
		{"prune with positional", []string{"--prune", "name"}, "", true},

		{"revoke happy", []string{"--revoke", "my-workspace"}, "revoke", false},
		{"revoke no name", []string{"--revoke"}, "", true},
		{"revoke two names", []string{"--revoke", "a", "b"}, "", true},

		{"show by name", []string{"--show", "my-workspace"}, "show", false},
		{"show by --path", []string{"--show", "--path", "/abs/proj"}, "show", false},
		{"show by --sidecar", []string{"--show", "--sidecar", "/abs/proj/.wezsesh.json"}, "show", false},
		{"show no args", []string{"--show"}, "", true},

		{"rebind happy", []string{"--rebind", "/old/.wezsesh.json", "/new/.wezsesh.json"}, "rebind", false},
		{"rebind one arg", []string{"--rebind", "/old"}, "", true},
		{"rebind three args", []string{"--rebind", "/a", "/b", "/c"}, "", true},
		// --rebind --path mutual exclusion: the rebind args are positional, --path is for approve mode only.
		{"rebind with --path rejected", []string{"--rebind", "--path", "/foo", "/old", "/new"}, "", true},
		{"rebind with --sidecar rejected", []string{"--rebind", "--sidecar", "/foo", "/old", "/new"}, "", true},

		{"two modes set", []string{"--list", "--prune"}, "", true},
		{"all three modes", []string{"--list", "--prune", "--revoke", "x"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parseTrustFlags(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (mode=%s)", f.mode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if f.mode != tc.wantMode {
				t.Errorf("mode: want %q, got %q", tc.wantMode, f.mode)
			}
		})
	}
}

// TestRunTrust_Approve_AndIsApproved is the LOAD-BEARING acceptance gate
// for §17.3 row "Project sidecar trust enforcement". An untrusted sidecar
// reports IsApproved=false; running `wezsesh trust <name>` writes the
// trust file; subsequent IsApproved=true.
func TestRunTrust_Approve_AndIsApproved(t *testing.T) {
	cmd := "npm install"
	body := makeSidecarBytes(t, &cmd, nil)
	picked := "/users/alice/proj"
	sidecarAbs := filepath.Join(picked, trustProjectSidecarBasename)
	deps, _ := makeTrustDeps(t,
		map[string][]byte{sidecarAbs: body},
		map[string]string{"my-proj": picked},
	)

	// Pre-state: per-hook IsApproved must be false (fail-closed).
	if deps.store.IsApproved(context.Background(), sidecarAbs, []byte(cmd)) {
		t.Fatal("pre-approval IsApproved=true; want false (fail-closed default)")
	}

	// Run `wezsesh trust my-proj`.
	flags := trustFlags{mode: "approve", name: "my-proj"}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("runTrust(approve)=%d, want %d (stderr=%s)", rc, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "approved "+sidecarAbs) {
		t.Errorf("stdout missing approval line: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "(on_create)") {
		t.Errorf("stdout missing per-hook annotation: %s", stdout.String())
	}

	// Post-state: per-hook IsApproved must now be true under the bytes
	// the executor will use at exec time.
	if !deps.store.IsApproved(context.Background(), sidecarAbs, []byte(cmd)) {
		t.Errorf("post-approval IsApproved=false; want true")
	}
}

// TestRunTrust_Approve_BothHooks_WritesTwoEntries is the CRITICAL
// fix gate: a sidecar carrying BOTH on_create AND on_restore must
// produce TWO independent trust entries (one per hook), each
// IsApproved-checkable with the bytes the executor will use at
// exec time. §13.5.1 hashes per-hook; the legacy "blob the bytes
// together with NUL" scheme produced a hash no executor could ever
// match.
func TestRunTrust_Approve_BothHooks_WritesTwoEntries(t *testing.T) {
	create := "npm install"
	restore := "npm test"
	body := makeSidecarBytes(t, &create, &restore)
	picked := "/users/alice/proj"
	sidecarAbs := filepath.Join(picked, trustProjectSidecarBasename)
	deps, _ := makeTrustDeps(t, map[string][]byte{sidecarAbs: body}, nil)

	flags := trustFlags{mode: "approve", sidecar: sidecarAbs}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("runTrust(approve both)=%d, want %d: %s", rc, exitOK, stderr.String())
	}

	// Per-hook IsApproved checks must BOTH return true under the
	// command bytes the hook executor will use at exec time.
	if !deps.store.IsApproved(context.Background(), sidecarAbs, []byte(create)) {
		t.Error("on_create hook not approved under per-hook bytes")
	}
	if !deps.store.IsApproved(context.Background(), sidecarAbs, []byte(restore)) {
		t.Error("on_restore hook not approved under per-hook bytes")
	}

	// `--list` must surface BOTH entries.
	entries, err := deps.store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 listable entries (one per hook); got %d", len(entries))
	}
	stdoutStr := stdout.String()
	if !strings.Contains(stdoutStr, "(on_create)") || !strings.Contains(stdoutStr, "(on_restore)") {
		t.Errorf("stdout should call out both hooks: %s", stdoutStr)
	}
}

// TestRunTrust_Approve_ByPath covers the --path arm of the approve mode.
func TestRunTrust_Approve_ByPath(t *testing.T) {
	cmd := "make setup"
	body := makeSidecarBytes(t, nil, &cmd) // on_restore only
	picked := "/srv/proj"
	sidecarAbs := filepath.Join(picked, trustProjectSidecarBasename)
	deps, _ := makeTrustDeps(t, map[string][]byte{sidecarAbs: body}, nil)

	flags := trustFlags{mode: "approve", path: picked}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("runTrust(approve --path)=%d, want %d: %s", rc, exitOK, stderr.String())
	}
	if !deps.store.IsApproved(context.Background(), sidecarAbs, []byte(cmd)) {
		t.Errorf("IsApproved=false after --path approval (on_restore)")
	}
}

// TestRunTrust_Approve_BySidecar covers the --sidecar arm.
func TestRunTrust_Approve_BySidecar(t *testing.T) {
	cmd := "bin/setup"
	body := makeSidecarBytes(t, &cmd, nil)
	sidecarAbs := "/some/non-root/place/foo.wezsesh.json"
	deps, _ := makeTrustDeps(t, map[string][]byte{sidecarAbs: body}, nil)

	flags := trustFlags{mode: "approve", sidecar: sidecarAbs}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("runTrust(approve --sidecar)=%d, want %d: %s", rc, exitOK, stderr.String())
	}
}

// TestRunTrust_Approve_NoCommand — a sidecar with neither hook is a
// no-op (nothing to approve). The CLI must reject so the user knows.
func TestRunTrust_Approve_NoCommand(t *testing.T) {
	body := makeSidecarBytes(t, nil, nil)
	sidecarAbs := "/abs/proj/.wezsesh.json"
	deps, _ := makeTrustDeps(t, map[string][]byte{sidecarAbs: body}, nil)

	flags := trustFlags{mode: "approve", sidecar: sidecarAbs}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc == exitOK {
		t.Fatalf("runTrust(approve no-command)=ok, want failure")
	}
	if !strings.Contains(stderr.String(), "no on_create or on_restore") {
		t.Errorf("stderr missing 'no on_create' message: %s", stderr.String())
	}
}

// TestRunTrust_Approve_MissingSidecar — a missing sidecar fails closed.
func TestRunTrust_Approve_MissingSidecar(t *testing.T) {
	deps, _ := makeTrustDeps(t, map[string][]byte{}, nil)
	flags := trustFlags{mode: "approve", sidecar: "/no/such/file"}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc == exitOK {
		t.Fatalf("runTrust(approve missing)=ok, want failure")
	}
}

// TestRunTrust_Approve_NameWithoutWorkspace — name resolution fails when
// the workspace isn't live. User must use --path / --sidecar instead.
func TestRunTrust_Approve_NameWithoutWorkspace(t *testing.T) {
	deps, _ := makeTrustDeps(t, nil, map[string]string{}) // no panes
	flags := trustFlags{mode: "approve", name: "ghost"}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc == exitOK {
		t.Fatalf("runTrust(approve no-workspace)=ok, want failure")
	}
	if !strings.Contains(stderr.String(), "ghost") {
		t.Errorf("stderr missing workspace name: %s", stderr.String())
	}
}

// TestRunTrust_Revoke_Happy — Revoke removes an existing approval.
func TestRunTrust_Revoke_Happy(t *testing.T) {
	cmd := "make"
	body := makeSidecarBytes(t, &cmd, nil)
	sidecarAbs := "/abs/proj/.wezsesh.json"
	deps, _ := makeTrustDeps(t,
		map[string][]byte{sidecarAbs: body},
		map[string]string{"proj": "/abs/proj"},
	)
	// Pre-approve under the per-hook bytes the executor uses.
	if err := deps.store.Approve(context.Background(), sidecarAbs, []byte(cmd)); err != nil {
		t.Fatalf("pre-Approve: %v", err)
	}
	if !deps.store.IsApproved(context.Background(), sidecarAbs, []byte(cmd)) {
		t.Fatal("pre-condition: IsApproved=false")
	}

	flags := trustFlags{mode: "revoke", name: "proj"}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("runTrust(revoke)=%d, want %d: %s", rc, exitOK, stderr.String())
	}
	if deps.store.IsApproved(context.Background(), sidecarAbs, []byte(cmd)) {
		t.Errorf("post-revoke: IsApproved=true")
	}
}

// TestRunTrust_Revoke_BothHooks — revoking a both-hooks sidecar removes
// BOTH per-hook trust entries.
func TestRunTrust_Revoke_BothHooks(t *testing.T) {
	create := "npm install"
	restore := "npm test"
	body := makeSidecarBytes(t, &create, &restore)
	sidecarAbs := "/abs/proj/.wezsesh.json"
	deps, _ := makeTrustDeps(t, map[string][]byte{sidecarAbs: body}, nil)

	if err := deps.store.Approve(context.Background(), sidecarAbs, []byte(create)); err != nil {
		t.Fatalf("Approve on_create: %v", err)
	}
	if err := deps.store.Approve(context.Background(), sidecarAbs, []byte(restore)); err != nil {
		t.Fatalf("Approve on_restore: %v", err)
	}

	flags := trustFlags{mode: "revoke", sidecar: sidecarAbs}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("runTrust(revoke both)=%d, want %d: %s", rc, exitOK, stderr.String())
	}
	if deps.store.IsApproved(context.Background(), sidecarAbs, []byte(create)) {
		t.Error("post-revoke: on_create still approved")
	}
	if deps.store.IsApproved(context.Background(), sidecarAbs, []byte(restore)) {
		t.Error("post-revoke: on_restore still approved")
	}
}

// TestRunTrust_Revoke_Idempotent — revoking a non-existent approval is
// not an error (mirrors trust.Revoke's contract).
func TestRunTrust_Revoke_Idempotent(t *testing.T) {
	cmd := "make"
	body := makeSidecarBytes(t, &cmd, nil)
	sidecarAbs := "/abs/proj/.wezsesh.json"
	deps, _ := makeTrustDeps(t,
		map[string][]byte{sidecarAbs: body},
		map[string]string{"proj": "/abs/proj"},
	)

	flags := trustFlags{mode: "revoke", name: "proj"}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("runTrust(revoke missing)=%d, want %d: %s", rc, exitOK, stderr.String())
	}
}

// TestRunTrust_Revoke_MissingSidecar — sidecar gone; CLI tells the user
// to run --prune (cannot recompute hash without the command bytes).
func TestRunTrust_Revoke_MissingSidecar(t *testing.T) {
	deps, _ := makeTrustDeps(t, map[string][]byte{}, nil)
	flags := trustFlags{mode: "revoke", sidecar: "/abs/missing/.wezsesh.json"}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc == exitOK {
		t.Fatalf("runTrust(revoke missing sidecar)=ok, want failure")
	}
	if !strings.Contains(stderr.String(), "--prune") {
		t.Errorf("stderr should hint at --prune: %s", stderr.String())
	}
}

// TestRunTrust_List_Empty — list on a fresh store prints just the header.
func TestRunTrust_List_Empty(t *testing.T) {
	deps, _ := makeTrustDeps(t, nil, nil)
	flags := trustFlags{mode: "list"}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("runTrust(list empty)=%d, want %d: %s", rc, exitOK, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "HASH") {
		t.Errorf("stdout missing header: %s", stdout.String())
	}
}

// TestRunTrust_List_Populated — entries appear in the listing.
func TestRunTrust_List_Populated(t *testing.T) {
	cmd := "make"
	body := makeSidecarBytes(t, &cmd, nil)
	sidecarAbs := "/abs/proj/.wezsesh.json"
	deps, _ := makeTrustDeps(t, map[string][]byte{sidecarAbs: body}, nil)

	if err := deps.store.Approve(context.Background(), sidecarAbs, []byte(cmd)); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	flags := trustFlags{mode: "list"}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("runTrust(list populated)=%d, want %d: %s", rc, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), sidecarAbs) {
		t.Errorf("stdout missing sidecar path: %s", stdout.String())
	}
}

// TestRunTrust_Prune_RemovesDangling — Prune removes entries whose
// recorded path no longer exists. We approve a real path on disk, then
// rm the file, then run prune.
func TestRunTrust_Prune_RemovesDangling(t *testing.T) {
	deps, _ := makeTrustDeps(t, nil, nil)
	tmp := t.TempDir()
	realPath := filepath.Join(tmp, "real.wezsesh.json")
	if err := os.WriteFile(realPath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cmdBytes := []byte("make")
	if err := deps.store.Approve(context.Background(), realPath, cmdBytes); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := os.Remove(realPath); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	flags := trustFlags{mode: "prune"}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("runTrust(prune)=%d, want %d: %s", rc, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "pruned 1") {
		t.Errorf("stdout should report 'pruned 1': %s", stdout.String())
	}
}

// TestRunTrust_Prune_NoOp — empty store prunes nothing.
func TestRunTrust_Prune_NoOp(t *testing.T) {
	deps, _ := makeTrustDeps(t, nil, nil)
	flags := trustFlags{mode: "prune"}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("runTrust(prune empty)=%d, want %d: %s", rc, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "pruned 0") {
		t.Errorf("stdout should report 'pruned 0': %s", stdout.String())
	}
}

// TestRunTrust_Show_Happy — show prints sidecar bytes + authorship guidance.
func TestRunTrust_Show_Happy(t *testing.T) {
	cmd := "make"
	body := makeSidecarBytes(t, &cmd, nil)
	sidecarAbs := "/abs/proj/.wezsesh.json"
	deps, _ := makeTrustDeps(t,
		map[string][]byte{sidecarAbs: body},
		map[string]string{"proj": "/abs/proj"},
	)

	flags := trustFlags{mode: "show", name: "proj"}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("runTrust(show)=%d, want %d: %s", rc, exitOK, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "sidecar: "+sidecarAbs) {
		t.Errorf("stdout missing sidecar header: %s", out)
	}
	if !strings.Contains(out, "Authorship guidance") {
		t.Errorf("stdout missing authorship guidance: %s", out)
	}
	if !strings.Contains(out, `"on_create"`) {
		t.Errorf("stdout missing sidecar bytes: %s", out)
	}
	if !strings.Contains(out, "on_create [") {
		t.Errorf("stdout should display per-hook trust hash: %s", out)
	}
}

// TestRunTrust_Show_BothHooks — §17.3 / per-hook coverage: a sidecar
// carrying BOTH hooks must surface both per-hook trust hashes in
// `--show` output. The CRITICAL fix made --show the only surface that
// renders both hooks at once; this test pins that behaviour.
func TestRunTrust_Show_BothHooks(t *testing.T) {
	create := "npm install"
	restore := "npm test"
	body := makeSidecarBytes(t, &create, &restore)
	sidecarAbs := "/abs/proj/.wezsesh.json"
	deps, _ := makeTrustDeps(t, map[string][]byte{sidecarAbs: body}, nil)

	flags := trustFlags{mode: "show", sidecar: sidecarAbs}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("runTrust(show both)=%d, want %d: %s", rc, exitOK, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "on_create [") {
		t.Errorf("show: missing on_create hash header: %s", out)
	}
	if !strings.Contains(out, "on_restore [") {
		t.Errorf("show: missing on_restore hash header: %s", out)
	}
	createHash := trust.ComputeHash(sidecarAbs, []byte(create))
	restoreHash := trust.ComputeHash(sidecarAbs, []byte(restore))
	if !strings.Contains(out, createHash) {
		t.Errorf("show: missing on_create hash %s in output: %s", createHash, out)
	}
	if !strings.Contains(out, restoreHash) {
		t.Errorf("show: missing on_restore hash %s in output: %s", restoreHash, out)
	}
	if createHash == restoreHash {
		t.Fatal("two distinct hooks must produce two distinct hashes (length-prefix invariant)")
	}
}

// TestRunTrust_Show_SanitisesAnsi — disk-sourced bytes must not be
// rendered verbatim: ANSI/control-char repaint can smuggle approved-
// looking content past a terminal-only review (§15.4). The raw 0x1B
// (ESC) byte must be replaced before reaching stdout.
func TestRunTrust_Show_SanitisesAnsi(t *testing.T) {
	// Build a sidecar whose on_create command embeds a literal ESC.
	// The JSON encoder produces "" in the body so the byte on
	// disk is 0x5C 0x75... — already escape-sequenced. Inject the raw
	// byte at the file level to exercise SanitizeForDisplay.
	body := []byte("{\"version\":1,\"on_create\":\"\x1b[31mDANGER\x1b[0m\"}")
	sidecarAbs := "/abs/proj/.wezsesh.json"
	deps, _ := makeTrustDeps(t, map[string][]byte{sidecarAbs: body}, nil)

	flags := trustFlags{mode: "show", sidecar: sidecarAbs}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("runTrust(show)=%d: %s", rc, stderr.String())
	}
	if strings.ContainsRune(stdout.String(), 0x1B) {
		t.Fatal("show output contains literal ESC (0x1B); SanitizeForDisplay should have stripped it")
	}
}

// TestRunTrust_Show_MissingSidecar — show fails when the sidecar is gone.
func TestRunTrust_Show_MissingSidecar(t *testing.T) {
	deps, _ := makeTrustDeps(t, nil, nil)
	flags := trustFlags{mode: "show", sidecar: "/no/such/sidecar.json"}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc == exitOK {
		t.Fatalf("runTrust(show missing)=ok, want failure")
	}
}

// TestRunTrust_Rebind_Happy — §17.3 row "Trust rebind happy path".
// Approve old sidecar, write identical new sidecar at a new path, run
// --rebind. Old approval gone; new approval present.
func TestRunTrust_Rebind_Happy(t *testing.T) {
	cmd := "make"
	body := makeSidecarBytes(t, &cmd, nil)
	oldAbs := "/old/proj/.wezsesh.json"
	newAbs := "/new/proj/.wezsesh.json"
	deps, _ := makeTrustDeps(t,
		map[string][]byte{
			oldAbs: body,
			newAbs: body, // identical bytes — rebind should succeed
		},
		nil,
	)
	if err := deps.store.Approve(context.Background(), oldAbs, []byte(cmd)); err != nil {
		t.Fatalf("pre-Approve: %v", err)
	}

	flags := trustFlags{mode: "rebind", rebindOld: oldAbs, rebindNew: newAbs}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("runTrust(rebind happy)=%d, want %d: %s", rc, exitOK, stderr.String())
	}

	// Old approval gone; new approval present.
	if deps.store.IsApproved(context.Background(), oldAbs, []byte(cmd)) {
		t.Errorf("post-rebind: old approval still present")
	}
	if !deps.store.IsApproved(context.Background(), newAbs, []byte(cmd)) {
		t.Errorf("post-rebind: new approval missing")
	}
}

// TestRunTrust_Rebind_BothHooks — rebind must transfer BOTH per-hook
// approvals when the sidecar carries both. Pre-approve both at the old
// path; rebind; assert both flipped to the new path.
func TestRunTrust_Rebind_BothHooks(t *testing.T) {
	create := "npm install"
	restore := "npm test"
	body := makeSidecarBytes(t, &create, &restore)
	oldAbs := "/old/proj/.wezsesh.json"
	newAbs := "/new/proj/.wezsesh.json"
	deps, _ := makeTrustDeps(t,
		map[string][]byte{oldAbs: body, newAbs: body},
		nil,
	)
	if err := deps.store.Approve(context.Background(), oldAbs, []byte(create)); err != nil {
		t.Fatalf("pre-Approve on_create: %v", err)
	}
	if err := deps.store.Approve(context.Background(), oldAbs, []byte(restore)); err != nil {
		t.Fatalf("pre-Approve on_restore: %v", err)
	}

	flags := trustFlags{mode: "rebind", rebindOld: oldAbs, rebindNew: newAbs}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("runTrust(rebind both)=%d, want %d: %s", rc, exitOK, stderr.String())
	}

	for _, kind := range []struct {
		name  string
		bytes []byte
	}{{"on_create", []byte(create)}, {"on_restore", []byte(restore)}} {
		if deps.store.IsApproved(context.Background(), oldAbs, kind.bytes) {
			t.Errorf("post-rebind: old %s still approved", kind.name)
		}
		if !deps.store.IsApproved(context.Background(), newAbs, kind.bytes) {
			t.Errorf("post-rebind: new %s missing", kind.name)
		}
	}
}

// TestRunTrust_Rebind_DivergedCommand — §17.3 row "Trust rebind diverged
// command". New sidecar's command_bytes differ from old; rebind must
// REFUSE so a silent uplift of approval scope cannot happen.
func TestRunTrust_Rebind_DivergedCommand(t *testing.T) {
	oldCmd := "make"
	newCmd := "make && echo 'i am evil'"
	oldBody := makeSidecarBytes(t, &oldCmd, nil)
	newBody := makeSidecarBytes(t, &newCmd, nil)
	oldAbs := "/old/proj/.wezsesh.json"
	newAbs := "/new/proj/.wezsesh.json"
	deps, _ := makeTrustDeps(t,
		map[string][]byte{oldAbs: oldBody, newAbs: newBody},
		nil,
	)
	if err := deps.store.Approve(context.Background(), oldAbs, []byte(oldCmd)); err != nil {
		t.Fatalf("pre-Approve: %v", err)
	}

	flags := trustFlags{mode: "rebind", rebindOld: oldAbs, rebindNew: newAbs}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc == exitOK {
		t.Fatalf("runTrust(rebind diverged)=ok, want failure (silent uplift refused)")
	}
	if !strings.Contains(stderr.String(), "refused") {
		t.Errorf("stderr missing 'refused' phrasing: %s", stderr.String())
	}
	// Old approval MUST remain intact.
	if !deps.store.IsApproved(context.Background(), oldAbs, []byte(oldCmd)) {
		t.Errorf("old approval lost after refused rebind")
	}
	// New approval MUST NOT appear.
	if deps.store.IsApproved(context.Background(), newAbs, []byte(newCmd)) {
		t.Errorf("diverged-rebind silently uplifted approval to new path")
	}
}

// TestRunTrust_Rebind_HookSetMismatch — old has only on_create; new has
// both on_create and on_restore. Adding a new hook is silent uplift of
// approval scope; rebind must refuse.
func TestRunTrust_Rebind_HookSetMismatch(t *testing.T) {
	create := "npm install"
	restore := "npm test"
	oldBody := makeSidecarBytes(t, &create, nil)
	newBody := makeSidecarBytes(t, &create, &restore)
	oldAbs := "/old/proj/.wezsesh.json"
	newAbs := "/new/proj/.wezsesh.json"
	deps, _ := makeTrustDeps(t,
		map[string][]byte{oldAbs: oldBody, newAbs: newBody},
		nil,
	)
	if err := deps.store.Approve(context.Background(), oldAbs, []byte(create)); err != nil {
		t.Fatalf("pre-Approve: %v", err)
	}

	flags := trustFlags{mode: "rebind", rebindOld: oldAbs, rebindNew: newAbs}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc == exitOK {
		t.Fatalf("runTrust(rebind hook-set-mismatch)=ok, want failure")
	}
	if !strings.Contains(stderr.String(), "refused") {
		t.Errorf("stderr missing 'refused' phrasing: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "on_restore") {
		t.Errorf("stderr should name the diverged hook: %s", stderr.String())
	}
}

// TestRunTrust_Rebind_MissingSource — old approval not in the trust store
// → exit code TRUST_REBIND_MISSING (= trustExitMissing).
func TestRunTrust_Rebind_MissingSource(t *testing.T) {
	cmd := "make"
	body := makeSidecarBytes(t, &cmd, nil)
	oldAbs := "/old/proj/.wezsesh.json"
	newAbs := "/new/proj/.wezsesh.json"
	deps, _ := makeTrustDeps(t,
		map[string][]byte{oldAbs: body, newAbs: body},
		nil,
	)
	// Note: NO pre-Approve. Source approval is missing.

	flags := trustFlags{mode: "rebind", rebindOld: oldAbs, rebindNew: newAbs}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc != trustExitMissing {
		t.Fatalf("runTrust(rebind missing-source)=%d, want trustExitMissing(%d) (stderr=%s)", rc, trustExitMissing, stderr.String())
	}
	if !strings.Contains(stderr.String(), "TRUST_REBIND_MISSING") {
		t.Errorf("stderr missing TRUST_REBIND_MISSING token: %s", stderr.String())
	}
}

// TestRunTrust_Rebind_RelativePathRejected — both rebind args MUST be
// absolute. A relative path is rejected at parse-time.
func TestRunTrust_Rebind_RelativePathRejected(t *testing.T) {
	deps, _ := makeTrustDeps(t, nil, nil)
	flags := trustFlags{mode: "rebind", rebindOld: "old/.wezsesh.json", rebindNew: "/new/.wezsesh.json"}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc == exitOK {
		t.Fatalf("runTrust(rebind relative-old)=ok, want failure")
	}
}

// TestNormaliseRebindArg — directory args get the sidecar basename
// appended; absolute sidecar paths pass through unchanged.
func TestNormaliseRebindArg(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/abs/proj", "/abs/proj/" + trustProjectSidecarBasename},
		{"/abs/proj/" + trustProjectSidecarBasename, "/abs/proj/" + trustProjectSidecarBasename},
		{"/abs/" + trustProjectSidecarBasename, "/abs/" + trustProjectSidecarBasename},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := normaliseRebindArg(tc.in)
			if got != tc.want {
				t.Errorf("normaliseRebindArg(%q)=%q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestReadSidecarHooks — covers each branch of the on_create /
// on_restore extraction.
func TestReadSidecarHooks(t *testing.T) {
	t.Run("on_create only", func(t *testing.T) {
		cmd := "npm install"
		body := makeSidecarBytes(t, &cmd, nil)
		read := func(string) ([]byte, error) { return body, nil }
		got, err := readSidecarHooks(read, "/x")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.OnCreate == nil || *got.OnCreate != cmd {
			t.Errorf("on_create: got %v, want %q", got.OnCreate, cmd)
		}
		if got.OnRestore != nil {
			t.Errorf("on_restore: want nil, got %q", *got.OnRestore)
		}
		if !got.hasAny() {
			t.Error("hasAny=false; want true")
		}
	})
	t.Run("on_restore only", func(t *testing.T) {
		cmd := "make"
		body := makeSidecarBytes(t, nil, &cmd)
		read := func(string) ([]byte, error) { return body, nil }
		got, err := readSidecarHooks(read, "/x")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.OnCreate != nil {
			t.Errorf("on_create: want nil, got %q", *got.OnCreate)
		}
		if got.OnRestore == nil || *got.OnRestore != cmd {
			t.Errorf("on_restore: got %v, want %q", got.OnRestore, cmd)
		}
	})
	t.Run("both", func(t *testing.T) {
		c1 := "npm install"
		c2 := "npm test"
		body := makeSidecarBytes(t, &c1, &c2)
		read := func(string) ([]byte, error) { return body, nil }
		got, err := readSidecarHooks(read, "/x")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.OnCreate == nil || *got.OnCreate != c1 {
			t.Errorf("on_create: got %v, want %q", got.OnCreate, c1)
		}
		if got.OnRestore == nil || *got.OnRestore != c2 {
			t.Errorf("on_restore: got %v, want %q", got.OnRestore, c2)
		}
		// Per-hook bytes must be exactly the JSON-decoded string,
		// without any cross-hook concat smearing — the §13.5 hash
		// inputs depend on this.
		oc, ok := got.hookCommand("on_create")
		if !ok || string(oc) != c1 {
			t.Errorf("hookCommand(on_create) = %q want %q", oc, c1)
		}
		or, ok := got.hookCommand("on_restore")
		if !ok || string(or) != c2 {
			t.Errorf("hookCommand(on_restore) = %q want %q", or, c2)
		}
	})
	t.Run("neither", func(t *testing.T) {
		body := makeSidecarBytes(t, nil, nil)
		read := func(string) ([]byte, error) { return body, nil }
		got, err := readSidecarHooks(read, "/x")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.hasAny() {
			t.Errorf("hasAny=true; want false")
		}
	})
	t.Run("read error", func(t *testing.T) {
		read := func(string) ([]byte, error) { return nil, errors.New("boom") }
		_, err := readSidecarHooks(read, "/x")
		if err == nil {
			t.Fatal("want error, got nil")
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		read := func(string) ([]byte, error) { return []byte("not-json"), nil }
		_, err := readSidecarHooks(read, "/x")
		if err == nil {
			t.Fatal("want error, got nil")
		}
	})
	t.Run("empty file", func(t *testing.T) {
		read := func(string) ([]byte, error) { return []byte{}, nil }
		_, err := readSidecarHooks(read, "/x")
		if err == nil {
			t.Fatal("want error, got nil")
		}
	})
}

// TestRunTrust_HashLengthPrefixIntact — the load-bearing CVE-prevention
// gate. After approval, IsApproved with a (path, cmd) pair that would
// collide under a `\n` separator scheme MUST report false. This is the
// CLI-level mirror of trust.TestComputeHash_NoNewlineCollision.
func TestRunTrust_HashLengthPrefixIntact(t *testing.T) {
	cmd := "make"
	body := makeSidecarBytes(t, &cmd, nil)
	sidecarAbs := "/abs/proj/.wezsesh.json"
	deps, _ := makeTrustDeps(t, map[string][]byte{sidecarAbs: body}, nil)

	flags := trustFlags{mode: "approve", sidecar: sidecarAbs}
	var stdout, stderr bytes.Buffer
	if rc := runTrust(context.Background(), deps, flags, &stdout, &stderr); rc != exitOK {
		t.Fatalf("approve failed: %s", stderr.String())
	}

	// Forge attempt: a different path that, under a separator-style
	// scheme, would hash-collide with the real one. With length-
	// prefixed hashing this MUST report unapproved.
	forgedPath := sidecarAbs + "\nrm"
	if deps.store.IsApproved(context.Background(), forgedPath, []byte(cmd)) {
		t.Fatal("hash collision under separator-style attack — length-prefix invariant violated")
	}
}

// TestRunTrust_UnknownMode — defensive guard. runTrust with an unknown
// mode prints a sensible error rather than panicking.
func TestRunTrust_UnknownMode(t *testing.T) {
	deps, _ := makeTrustDeps(t, nil, nil)
	flags := trustFlags{mode: "wat"}
	var stdout, stderr bytes.Buffer
	rc := runTrust(context.Background(), deps, flags, &stdout, &stderr)
	if rc == exitOK {
		t.Fatalf("runTrust(unknown)=ok, want failure")
	}
	if !strings.Contains(stderr.String(), "unknown mode") {
		t.Errorf("stderr missing 'unknown mode': %s", stderr.String())
	}
}

// TestRunTrust_ResolveSidecarPath — covers the three resolution branches
// of resolveTrustSidecarPath end-to-end.
func TestRunTrust_ResolveSidecarPath(t *testing.T) {
	deps, _ := makeTrustDeps(t, nil, map[string]string{"proj": "/abs/proj"})
	cases := []struct {
		name, sidecar, picked, want string
		wantErr                     bool
		wsName                      string
	}{
		{name: "by --sidecar", sidecar: "/x/sc.json", want: "/x/sc.json"},
		{name: "by --path", picked: "/abs/proj", want: "/abs/proj/" + trustProjectSidecarBasename},
		{name: "by name", wsName: "proj", want: "/abs/proj/" + trustProjectSidecarBasename},
		{name: "name no workspace", wsName: "ghost", wantErr: true},
		{name: "rel sidecar", sidecar: "rel/x", wantErr: true},
		{name: "rel path", picked: "rel", wantErr: true},
		{name: "nothing", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveTrustSidecarPath(context.Background(), deps, tc.wsName, tc.picked, tc.sidecar)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// TestPlural — pluralisation helper sanity check.
func TestPlural(t *testing.T) {
	if plural(0, "y", "ies") != "ies" {
		t.Errorf("plural(0): %s", plural(0, "y", "ies"))
	}
	if plural(1, "y", "ies") != "y" {
		t.Errorf("plural(1): %s", plural(1, "y", "ies"))
	}
	if plural(2, "y", "ies") != "ies" {
		t.Errorf("plural(2): %s", plural(2, "y", "ies"))
	}
}

// TestBytesEqual — helper sanity check.
func TestBytesEqual(t *testing.T) {
	if !bytesEqual([]byte("abc"), []byte("abc")) {
		t.Error("equal bytes report unequal")
	}
	if bytesEqual([]byte("abc"), []byte("abd")) {
		t.Error("differing bytes report equal")
	}
	if bytesEqual([]byte("abc"), []byte("abcd")) {
		t.Error("different lengths report equal")
	}
	if !bytesEqual(nil, nil) {
		t.Error("nil bytes report unequal")
	}
}
