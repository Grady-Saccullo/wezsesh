package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Grady-Saccullo/wezsesh/internal/config"
	"github.com/Grady-Saccullo/wezsesh/internal/nameval"
	"github.com/Grady-Saccullo/wezsesh/internal/safefs"
	"github.com/Grady-Saccullo/wezsesh/internal/snapshots"
	"github.com/Grady-Saccullo/wezsesh/internal/trust"
	"github.com/Grady-Saccullo/wezsesh/internal/wezcli"
)

// trustExitMissing is the §13.5.2 TRUST_REBIND_MISSING exit code. The
// rebind path is the only place that uses a non-zero exit beyond the
// generic exitDoctorOrSubcmd bucket: a missing source approval is a
// distinct, scriptable error mode (the user must run `wezsesh trust
// <new>` manually). Picked as 4 to avoid colliding with the generic 2
// (subcmd error) or 3 (keygen).
const trustExitMissing = 4

// trustProjectSidecarBasename is the §10.3 project sidecar filename.
// `wezsesh trust <name>` resolves to `<workspace_cwd>/.wezsesh.json`;
// `--path <picked>` resolves the same way; `--sidecar <abs>` skips this
// suffix and uses the path verbatim.
const trustProjectSidecarBasename = ".wezsesh.json"

// trustAuthorshipGuidance is the §13.5 / spec rule "authorship guidance
// is published, not enforced" text rendered by `wezsesh trust --show`.
// Treat on_create like npm postinstall; stick to commands safe in a CI
// runner; avoid side effects outside the project directory; document
// on_create in the project README.
const trustAuthorshipGuidance = `Authorship guidance:
  - Treat on_create / on_restore like npm postinstall scripts.
  - Stick to commands safe in a CI runner: 'npm install', 'make',
    'bin/setup'.
  - Avoid side effects outside the project directory.
  - Document on_create in the project's README.
`

// hookKinds is the canonical iteration order over per-hook trust
// entries. Matches the §10.2 / §10.3 sidecar field order so `--list`
// rendering and conformance fixtures stay deterministic.
var hookKinds = []string{"on_create", "on_restore"}

// trustListEntry is the on-the-wire shape for `wezsesh trust --list`.
// Field set is a stable contract scripts may depend on.
//
//	{
//	  "hash": "<64-hex sha256>",
//	  "path": "<absolute sidecar path>"
//	}
type trustListEntry struct {
	Hash string `json:"hash"`
	Path string `json:"path"`
}

// trustFlags carries the parsed CLI flags. Mode is set by parseTrustFlags
// to the chosen flag-arm (approve / revoke / list / prune / show /
// rebind). Exactly one mode is selected; mode-conflicts are rejected
// during parse.
type trustFlags struct {
	mode      string // approve | revoke | list | prune | show | rebind
	name      string // workspace name (modes: approve, revoke, show)
	path      string // --path picked dir (mode: approve)
	sidecar   string // --sidecar absolute path (mode: approve)
	rebindOld string // --rebind <old> <new> (mode: rebind)
	rebindNew string // --rebind <old> <new> (mode: rebind)
}

// trustDeps is the seam subcmdTrust reaches through. Production wires
// it from a real config + wezcli; tests inject hand-rolled fakes so the
// CLI surface is exercised without standing up a real wezterm or a real
// `internal/trust.Store`.
type trustDeps struct {
	// store is the trust backend. Tests pass a real *trust.Store rooted
	// in a tempdir; production wires it from cfg.TrustDir.
	store *trust.Store
	// readSidecar reads the project sidecar at the given absolute path.
	// Returned bytes are the raw file contents — used both to extract
	// the on_create / on_restore command (for hash construction) AND
	// to render the --show body. The same bytes MUST be reused by any
	// downstream trust check / hook exec; this is the read-once-
	// exec-from-memory invariant from CLAUDE.md.
	readSidecar func(absPath string) ([]byte, error)
	// resolveWorkspaceCwd returns the absolute cwd for the workspace
	// named `name` (typically the workspace's first pane's cwd, decoded
	// from the file:// URL in `wezterm cli list`). Returns ("", false)
	// when the workspace does not exist; the CLI surfaces a clear error
	// telling the user to use --path.
	resolveWorkspaceCwd func(ctx context.Context, name string) (string, bool, error)
}

// sidecarHooks is the read-once parsed view of a project sidecar. Raw
// holds the bytes verbatim for `--show`; OnCreate / OnRestore are nil
// when the sidecar does not carry that hook. A single per-hook trust
// file is written per non-nil entry — §13.5 hashes each hook
// independently because the executor calls `ComputeHash(path,
// hook_bytes)` per hook at exec time.
type sidecarHooks struct {
	Raw       []byte
	OnCreate  *string
	OnRestore *string
}

// hookCommand returns the present hook bytes (or nil if absent) for the
// given §10.2 field name. Centralised so the iteration order over
// hookKinds and the per-mode action paths agree.
func (h sidecarHooks) hookCommand(kind string) ([]byte, bool) {
	switch kind {
	case "on_create":
		if h.OnCreate == nil {
			return nil, false
		}
		return []byte(*h.OnCreate), true
	case "on_restore":
		if h.OnRestore == nil {
			return nil, false
		}
		return []byte(*h.OnRestore), true
	default:
		return nil, false
	}
}

// hasAny returns true iff at least one hook is present.
func (h sidecarHooks) hasAny() bool {
	return h.OnCreate != nil || h.OnRestore != nil
}

// subcmdTrust implements `wezsesh trust ...` (§8.20).
//
// §13.14: top-level recover mirrors keygen / list / find / reply. rc on
// panic is exitDoctorOrSubcmd (2). The trust path holds no reply socket.
func subcmdTrust(rest []string, stdout, stderr io.Writer) (rc int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "wezsesh trust: panic: %v\n", r)
			rc = exitDoctorOrSubcmd
		}
	}()

	flags, err := parseTrustFlags(rest)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh trust: %v\n", err)
		return exitDoctorOrSubcmd
	}

	ctx := context.Background()

	cfg, err := config.LoadFromEnv(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh trust: config: %v\n", err)
		return exitDoctorOrSubcmd
	}

	deps, err := buildTrustDeps(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh trust: %v\n", err)
		return exitDoctorOrSubcmd
	}

	return runTrust(ctx, deps, flags, stdout, stderr)
}

// parseTrustFlags accepts the §8.20 surface and selects exactly one
// mode. Mode selection is precedence-based — flags that select a mode
// (`--list`, `--prune`, `--revoke`, `--show`, `--rebind`) win over the
// default (`approve`). Conflicting flag combinations are rejected here
// rather than at runTrust time so the user gets one consistent error
// shape.
func parseTrustFlags(rest []string) (trustFlags, error) {
	fs := flag.NewFlagSet("trust", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	listMode := fs.Bool("list", false, "list trust entries")
	pruneMode := fs.Bool("prune", false, "remove trust entries whose recorded path no longer exists")
	revokeMode := fs.Bool("revoke", false, "revoke approval for the named workspace")
	showMode := fs.Bool("show", false, "display the project sidecar contents and authorship guidance")
	rebindMode := fs.Bool("rebind", false, "transfer approval from <old-abs> to <new-abs> (§13.5.2)")

	pickedPath := fs.String("path", "", "absolute picked-path (project root) whose <root>/.wezsesh.json to approve")
	sidecarPath := fs.String("sidecar", "", "absolute path to a project sidecar file to approve directly")

	if err := fs.Parse(rest); err != nil {
		return trustFlags{}, err
	}

	tail := fs.Args()
	p := trustFlags{
		path:    *pickedPath,
		sidecar: *sidecarPath,
	}

	// Mode-selection guard: at most one of the mode flags may be set.
	modeFlags := 0
	for _, b := range []bool{*listMode, *pruneMode, *revokeMode, *showMode, *rebindMode} {
		if b {
			modeFlags++
		}
	}
	if modeFlags > 1 {
		return trustFlags{}, errors.New("at most one of --list, --prune, --revoke, --show, --rebind may be set")
	}

	switch {
	case *listMode:
		p.mode = "list"
		if len(tail) != 0 {
			return trustFlags{}, fmt.Errorf("--list takes no positional args; got %d", len(tail))
		}
		if p.path != "" || p.sidecar != "" {
			return trustFlags{}, errors.New("--list does not accept --path or --sidecar")
		}
	case *pruneMode:
		p.mode = "prune"
		if len(tail) != 0 {
			return trustFlags{}, fmt.Errorf("--prune takes no positional args; got %d", len(tail))
		}
		if p.path != "" || p.sidecar != "" {
			return trustFlags{}, errors.New("--prune does not accept --path or --sidecar")
		}
	case *revokeMode:
		p.mode = "revoke"
		if len(tail) != 1 {
			return trustFlags{}, fmt.Errorf("--revoke requires exactly one workspace name; got %d", len(tail))
		}
		p.name = tail[0]
	case *showMode:
		p.mode = "show"
		// --show accepts: a positional name OR --path OR --sidecar.
		// Picking exactly one is required.
		hasName := len(tail) == 1
		hasPath := p.path != ""
		hasSidecar := p.sidecar != ""
		count := 0
		for _, b := range []bool{hasName, hasPath, hasSidecar} {
			if b {
				count++
			}
		}
		if count != 1 {
			return trustFlags{}, errors.New("--show requires exactly one of: <name>, --path <picked>, --sidecar <abs>")
		}
		if hasName {
			p.name = tail[0]
		}
	case *rebindMode:
		p.mode = "rebind"
		if len(tail) != 2 {
			return trustFlags{}, fmt.Errorf("--rebind requires exactly two positional args (<old-abs> <new-abs>); got %d", len(tail))
		}
		if p.path != "" || p.sidecar != "" {
			return trustFlags{}, errors.New("--rebind does not accept --path or --sidecar")
		}
		p.rebindOld = tail[0]
		p.rebindNew = tail[1]
	default:
		// Default mode: approve. Accepts a positional name OR --path OR
		// --sidecar. Pick exactly one.
		p.mode = "approve"
		hasName := len(tail) == 1
		hasPath := p.path != ""
		hasSidecar := p.sidecar != ""
		count := 0
		for _, b := range []bool{hasName, hasPath, hasSidecar} {
			if b {
				count++
			}
		}
		if count == 0 {
			return trustFlags{}, errors.New("approve mode requires one of: <name>, --path <picked>, --sidecar <abs>")
		}
		if count > 1 {
			return trustFlags{}, errors.New("approve mode accepts exactly one of: <name>, --path <picked>, --sidecar <abs>")
		}
		if hasName {
			p.name = tail[0]
		}
	}

	return p, nil
}

// buildTrustDeps wires production data sources from a loaded config.
// The wezcli is only constructed when the workspace-name resolution path
// is reachable (approve / revoke / show with a positional name); a
// missing wezterm binary at runtime surfaces as a clear error from the
// resolver itself, NOT as a hard build-deps error here.
func buildTrustDeps(ctx context.Context, cfg *config.Config) (trustDeps, error) {
	store, err := trust.Open(ctx, cfg.TrustDir, nil)
	if err != nil {
		return trustDeps{}, fmt.Errorf("trust: %w", err)
	}

	// readSidecar uses safefs.SafeOpenForRead to honour the fail-closed
	// posture: a symlinked sidecar is treated as missing, matching the
	// hook trust check's own read-time symlink defense (§13.5). ELOOP
	// (wrapped as safefs.ErrIsSymlink) is also surfaced as ErrNotExist
	// so callers see a single "missing-or-symlink" shape — the hook
	// executor's own SafeOpenForRead does the same merge.
	readSidecar := func(absPath string) ([]byte, error) {
		f, err := safefs.SafeOpenForRead(absPath)
		if err != nil {
			if errors.Is(err, safefs.ErrIsSymlink) {
				// Treat as missing — fail-closed, parity with hook
				// executor's read path.
				return nil, fmt.Errorf("symlinked sidecar treated as missing: %w", err)
			}
			return nil, err
		}
		defer f.Close()
		// Cap reads at MaxFileSize (10 MiB) — same cap snapshots use for
		// the snapshot sidecar; project sidecars are not a separate
		// threat class. A LimitReader caps at MaxFileSize+1 so we can
		// detect oversize via the +1-byte trailer rather than silently
		// truncating.
		buf, err := io.ReadAll(io.LimitReader(f, snapshots.MaxFileSize+1))
		if err != nil {
			return nil, err
		}
		if int64(len(buf)) > snapshots.MaxFileSize {
			return nil, fmt.Errorf("sidecar exceeds %d-byte size cap", int64(snapshots.MaxFileSize))
		}
		return buf, nil
	}

	// resolveWorkspaceCwd defers wezcli construction to first call so a
	// `wezsesh trust --list` invocation still works on a host without
	// wezterm installed. The lookup walks `wezterm cli list --format
	// json` for the workspace's first pane, decodes its CWD URL, and
	// returns the absolute filesystem path.
	resolveWorkspaceCwd := func(ctx context.Context, name string) (string, bool, error) {
		// §15.1: every workspace-name ingestion site validates first.
		// A name with a literal NUL / C0 byte / leading whitespace would
		// otherwise round-trip into the wezcli match below and silently
		// fail to resolve.
		if err := nameval.ValidateWorkspaceName(name); err != nil {
			return "", false, fmt.Errorf("validate workspace name: %w", err)
		}
		wcli, err := wezcli.NewClient(nil)
		if err != nil {
			return "", false, fmt.Errorf("wezcli: %w", err)
		}
		panes, err := wcli.List(ctx)
		if err != nil {
			return "", false, fmt.Errorf("wezcli list: %w", err)
		}
		nfcName := nameval.NormalizeNFC(name)
		// First pane in the list whose workspace matches; wezcli's
		// output is pane-id ordered so this is deterministic.
		for _, p := range panes {
			if nameval.NormalizeNFC(p.Workspace) != nfcName {
				continue
			}
			cwd, ok := p.CWDPath()
			if ok {
				return cwd, true, nil
			}
		}
		return "", false, nil
	}

	return trustDeps{
		store:               store,
		readSidecar:         readSidecar,
		resolveWorkspaceCwd: resolveWorkspaceCwd,
	}, nil
}

// runTrust dispatches on the parsed mode. Each branch returns an exit
// code; rebind has a distinct trustExitMissing path (§13.5.2).
func runTrust(ctx context.Context, deps trustDeps, flags trustFlags, stdout, stderr io.Writer) int {
	switch flags.mode {
	case "list":
		return runTrustList(ctx, deps, stdout, stderr)
	case "prune":
		return runTrustPrune(ctx, deps, stdout, stderr)
	case "approve":
		return runTrustApprove(ctx, deps, flags, stdout, stderr)
	case "revoke":
		return runTrustRevoke(ctx, deps, flags, stdout, stderr)
	case "show":
		return runTrustShow(ctx, deps, flags, stdout, stderr)
	case "rebind":
		return runTrustRebind(ctx, deps, flags, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "wezsesh trust: internal error: unknown mode %q\n", flags.mode)
		return exitDoctorOrSubcmd
	}
}

// runTrustList prints all trust entries as one-row-per-entry text. JSON
// rendering is intentionally NOT exposed via --format on the trust list
// surface (§8.20 lists no --format flag for trust); scripts that want
// JSON can post-process the text output. The text shape is tab-stable.
func runTrustList(ctx context.Context, deps trustDeps, stdout, stderr io.Writer) int {
	entries, err := deps.store.List(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh trust: list: %v\n", err)
		return exitDoctorOrSubcmd
	}
	rows := make([]trustListEntry, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, trustListEntry{Hash: e.Hash, Path: e.Path})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Hash < rows[j].Hash })

	var b strings.Builder
	b.WriteString("HASH                                                             PATH\n")
	for _, r := range rows {
		b.WriteString(r.Hash)
		b.WriteByte(' ')
		b.WriteString(nameval.SanitizeForDisplay(r.Path))
		b.WriteByte('\n')
	}
	if _, err := io.WriteString(stdout, b.String()); err != nil {
		fmt.Fprintf(stderr, "wezsesh trust: list write: %v\n", err)
		return exitDoctorOrSubcmd
	}
	return exitOK
}

// runTrustPrune removes trust entries whose recorded path no longer
// exists on disk. The store's Prune is idempotent and best-effort; we
// surface the removal count for human inspection.
func runTrustPrune(ctx context.Context, deps trustDeps, stdout, stderr io.Writer) int {
	removed, err := deps.store.Prune(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh trust: prune: %v\n", err)
		return exitDoctorOrSubcmd
	}
	fmt.Fprintf(stdout, "pruned %d entr%s\n", removed, plural(removed, "y", "ies"))
	return exitOK
}

// runTrustApprove resolves the sidecar path (one of name / --path /
// --sidecar), reads the sidecar exactly once, and writes ONE trust file
// per present hook (§13.5: the executor hashes per-hook; a sidecar
// carrying both on_create and on_restore therefore produces two
// independent trust entries).
//
// Read-once invariant: the bytes used to compute the hash here are the
// SAME bytes the hook trust check consumes at exec time (it re-reads
// the sidecar then, but produces identical hash inputs because the
// hook command bytes depend only on the sidecar's parsed JSON).
func runTrustApprove(ctx context.Context, deps trustDeps, flags trustFlags, stdout, stderr io.Writer) int {
	absSidecar, err := resolveTrustSidecarPath(ctx, deps, flags.name, flags.path, flags.sidecar)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh trust: %v\n", err)
		return exitDoctorOrSubcmd
	}
	hooks, err := readSidecarHooks(deps.readSidecar, absSidecar)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh trust: read sidecar %s: %v\n", absSidecar, err)
		return exitDoctorOrSubcmd
	}
	if !hooks.hasAny() {
		fmt.Fprintf(stderr, "wezsesh trust: sidecar %s carries no on_create or on_restore command; nothing to approve\n", absSidecar)
		return exitDoctorOrSubcmd
	}
	for _, kind := range hookKinds {
		cmd, ok := hooks.hookCommand(kind)
		if !ok {
			continue
		}
		if err := deps.store.Approve(ctx, absSidecar, cmd); err != nil {
			fmt.Fprintf(stderr, "wezsesh trust: approve %s: %v\n", kind, err)
			return exitDoctorOrSubcmd
		}
		fmt.Fprintf(stdout, "approved %s (%s)\n", absSidecar, kind)
	}
	return exitOK
}

// runTrustRevoke resolves the sidecar path the same way approve does
// but takes the inverse action. Revoke is idempotent: a missing trust
// file is NOT an error, so a script that loops `revoke` can succeed on
// every iteration. Each present hook gets its own Revoke call (one
// trust file per hook — see runTrustApprove).
func runTrustRevoke(ctx context.Context, deps trustDeps, flags trustFlags, stdout, stderr io.Writer) int {
	absSidecar, err := resolveTrustSidecarPath(ctx, deps, flags.name, flags.path, flags.sidecar)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh trust: %v\n", err)
		return exitDoctorOrSubcmd
	}
	hooks, err := readSidecarHooks(deps.readSidecar, absSidecar)
	if err != nil {
		// Sidecar gone or unreadable: we cannot recompute the hash, so
		// we cannot target the exact trust entries. Tell the user to run
		// `wezsesh trust --prune` to GC dangling entries — that path
		// uses the recorded `path` field and will reap entries whose
		// sidecar no longer exists on disk.
		fmt.Fprintf(stderr, "wezsesh trust: read sidecar %s: %v\n", absSidecar, err)
		fmt.Fprintf(stderr, "wezsesh trust: cannot revoke without command bytes; run 'wezsesh trust --prune' to GC dangling entries\n")
		return exitDoctorOrSubcmd
	}
	if !hooks.hasAny() {
		fmt.Fprintf(stderr, "wezsesh trust: sidecar %s carries no on_create or on_restore command; nothing to revoke\n", absSidecar)
		return exitDoctorOrSubcmd
	}
	for _, kind := range hookKinds {
		cmd, ok := hooks.hookCommand(kind)
		if !ok {
			continue
		}
		if err := deps.store.Revoke(ctx, absSidecar, cmd); err != nil {
			fmt.Fprintf(stderr, "wezsesh trust: revoke %s: %v\n", kind, err)
			return exitDoctorOrSubcmd
		}
		fmt.Fprintf(stdout, "revoked %s (%s)\n", absSidecar, kind)
	}
	return exitOK
}

// runTrustShow prints the sidecar contents (sanitised for terminal
// rendering) plus the §13.5 authorship-guidance text. Render goes
// through SanitizeForDisplay because untrusted sidecar bytes can carry
// ANSI / control-char repaints that smuggle approved-looking content
// past a terminal-only review (§15.4 — every render of a disk-sourced
// string must be sanitised). The user can still get the literal bytes
// via `cat /path/to/.wezsesh.json` if they need the unsanitised view.
func runTrustShow(ctx context.Context, deps trustDeps, flags trustFlags, stdout, stderr io.Writer) int {
	absSidecar, err := resolveTrustSidecarPath(ctx, deps, flags.name, flags.path, flags.sidecar)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh trust: %v\n", err)
		return exitDoctorOrSubcmd
	}
	raw, err := deps.readSidecar(absSidecar)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh trust: read sidecar %s: %v\n", absSidecar, err)
		return exitDoctorOrSubcmd
	}
	fmt.Fprintf(stdout, "sidecar: %s\n\n", absSidecar)
	// Sanitise raw bytes against ANSI/control-char repaint smuggling.
	if _, err := io.WriteString(stdout, nameval.SanitizeForDisplay(string(raw))); err != nil {
		fmt.Fprintf(stderr, "wezsesh trust: write sidecar: %v\n", err)
		return exitDoctorOrSubcmd
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		_, _ = io.WriteString(stdout, "\n")
	}
	_, _ = io.WriteString(stdout, "\n")
	// Render each present hook with its computed trust hash so the user
	// can cross-reference `wezsesh trust --list` output. Failures to
	// parse the sidecar JSON degrade gracefully to "no hooks rendered"
	// rather than blocking the show — the raw bytes were already
	// printed above for the user to inspect.
	if hooks, err := parseSidecarHooks(raw); err == nil && hooks.hasAny() {
		_, _ = io.WriteString(stdout, "Hooks (per-hook trust hashes — §13.5):\n")
		for _, kind := range hookKinds {
			cmd, ok := hooks.hookCommand(kind)
			if !ok {
				continue
			}
			fmt.Fprintf(stdout, "  %s [%s]: %s\n", kind, trust.ComputeHash(absSidecar, cmd), nameval.SanitizeForDisplay(string(cmd)))
		}
		_, _ = io.WriteString(stdout, "\n")
	}
	_, _ = io.WriteString(stdout, trustAuthorshipGuidance)
	return exitOK
}

// runTrustRebind implements §13.5.2: read newPath/.wezsesh.json (or
// fail if absent / untrusted-shape), read oldPath/.wezsesh.json (must
// exist and have identical command bytes for every present hook), call
// store.Rebind once per present hook. On `TRUST_REBIND_MISSING` we exit
// trustExitMissing (4) so wrappers can distinguish "user must approve
// manually" from generic CLI errors.
//
// The rebind args are absolute SIDECAR paths, NOT picked-path roots —
// the spec phrases them as `<old-abs> <new-abs>` and §13.5.2 reads
// `oldPath/.wezsesh.json` (the path is the file). We accept both for
// usability: if the user passes a directory, append the sidecar
// basename; otherwise use the path verbatim.
//
// Both sides MUST carry the same set of present hooks AND each present
// hook's bytes must be byte-equal. A divergence on any hook refuses
// the entire rebind (silent uplift would be the threat).
func runTrustRebind(ctx context.Context, deps trustDeps, flags trustFlags, stdout, stderr io.Writer) int {
	oldPath := normaliseRebindArg(flags.rebindOld)
	newPath := normaliseRebindArg(flags.rebindNew)

	if !filepath.IsAbs(oldPath) {
		fmt.Fprintf(stderr, "wezsesh trust: --rebind old path %q must be absolute\n", oldPath)
		return exitDoctorOrSubcmd
	}
	if !filepath.IsAbs(newPath) {
		fmt.Fprintf(stderr, "wezsesh trust: --rebind new path %q must be absolute\n", newPath)
		return exitDoctorOrSubcmd
	}

	newHooks, err := readSidecarHooks(deps.readSidecar, newPath)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh trust: --rebind read new %s: %v\n", newPath, err)
		return exitDoctorOrSubcmd
	}
	if !newHooks.hasAny() {
		fmt.Fprintf(stderr, "wezsesh trust: --rebind new sidecar %s carries no on_create/on_restore command; nothing to rebind\n", newPath)
		return exitDoctorOrSubcmd
	}
	oldHooks, err := readSidecarHooks(deps.readSidecar, oldPath)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh trust: --rebind read old %s: %v\n", oldPath, err)
		return exitDoctorOrSubcmd
	}

	// Per-hook divergence check. Either side carrying a hook the other
	// lacks is "diverged"; differing bytes for the same hook is
	// "diverged". §13.5.2 phrasing: identical command_bytes → silent
	// uplift refusal applies to ANY mismatch.
	for _, kind := range hookKinds {
		oldCmd, oldOK := oldHooks.hookCommand(kind)
		newCmd, newOK := newHooks.hookCommand(kind)
		if oldOK != newOK {
			fmt.Fprintf(stderr, "wezsesh trust: --rebind refused: hook %s present on one side only (%s vs %s); run 'wezsesh trust --sidecar %s' to approve the new shape manually\n",
				kind, oldPath, newPath, newPath)
			return exitDoctorOrSubcmd
		}
		if oldOK && !bytesEqual(oldCmd, newCmd) {
			fmt.Fprintf(stderr, "wezsesh trust: --rebind refused: hook %s command bytes differ between %s and %s; run 'wezsesh trust --sidecar %s' to approve the new shape manually\n",
				kind, oldPath, newPath, newPath)
			return exitDoctorOrSubcmd
		}
	}

	// All hooks byte-equal. Rebind each present hook in turn. The trust
	// store's Rebind returns ErrTrustRebindMissing for any hook whose
	// source approval doesn't exist; we surface trustExitMissing on the
	// FIRST such miss so the caller learns the rebind is incomplete and
	// can re-approve manually.
	for _, kind := range hookKinds {
		cmd, ok := newHooks.hookCommand(kind)
		if !ok {
			continue
		}
		if err := deps.store.Rebind(ctx, oldPath, newPath, cmd); err != nil {
			if errors.Is(err, trust.ErrTrustRebindMissing) {
				fmt.Fprintf(stderr, "wezsesh trust: --rebind: source approval not found for %s hook %s; run 'wezsesh trust --sidecar %s' to approve the new path manually (TRUST_REBIND_MISSING)\n", oldPath, kind, newPath)
				return trustExitMissing
			}
			fmt.Fprintf(stderr, "wezsesh trust: --rebind %s: %v\n", kind, err)
			return exitDoctorOrSubcmd
		}
		fmt.Fprintf(stdout, "rebound %s -> %s (%s)\n", oldPath, newPath, kind)
	}
	return exitOK
}

// resolveTrustSidecarPath turns one of (name, picked-path, sidecar) into
// an absolute project-sidecar path. Resolution order is:
//
//   - --sidecar wins if non-empty (already absolute by contract).
//   - --path appends `/.wezsesh.json` to the picked dir.
//   - <name> looks up the workspace's first-pane cwd via wezcli.
//
// Exactly-one-source is enforced by parseTrustFlags; this helper does
// not re-validate.
func resolveTrustSidecarPath(ctx context.Context, deps trustDeps, name, pickedPath, sidecarPath string) (string, error) {
	switch {
	case sidecarPath != "":
		if !filepath.IsAbs(sidecarPath) {
			return "", fmt.Errorf("--sidecar %q must be absolute", sidecarPath)
		}
		return sidecarPath, nil
	case pickedPath != "":
		if !filepath.IsAbs(pickedPath) {
			return "", fmt.Errorf("--path %q must be absolute", pickedPath)
		}
		return filepath.Join(pickedPath, trustProjectSidecarBasename), nil
	case name != "":
		if deps.resolveWorkspaceCwd == nil {
			return "", errors.New("workspace lookup unavailable (no wezcli wired)")
		}
		cwd, ok, err := deps.resolveWorkspaceCwd(ctx, name)
		if err != nil {
			return "", fmt.Errorf("resolve workspace %q: %w", name, err)
		}
		if !ok || cwd == "" {
			return "", fmt.Errorf("workspace %q has no live pane (or its cwd is unreported); use --path <picked> or --sidecar <abs> instead", name)
		}
		return filepath.Join(cwd, trustProjectSidecarBasename), nil
	default:
		return "", errors.New("no name / --path / --sidecar provided")
	}
}

// readSidecarHooks reads the sidecar at absPath and returns one entry
// per present hook (`on_create`, `on_restore`). Empty raw → error.
//
// Per §13.5 the executor computes ComputeHash(path, command_bytes) PER
// HOOK; therefore the CLI must mirror that when approving / revoking /
// rebinding. A sidecar carrying both hooks produces TWO trust files at
// approve time and TWO removals at revoke time.
func readSidecarHooks(read func(string) ([]byte, error), absPath string) (sidecarHooks, error) {
	raw, err := read(absPath)
	if err != nil {
		return sidecarHooks{}, err
	}
	if len(raw) == 0 {
		return sidecarHooks{}, errors.New("empty sidecar")
	}
	return parseSidecarHooks(raw)
}

// parseSidecarHooks parses the sidecar bytes into a sidecarHooks. Pure
// on its input so the show path can call it after readSidecar without
// re-reading.
func parseSidecarHooks(raw []byte) (sidecarHooks, error) {
	var s snapshots.Sidecar
	if err := json.Unmarshal(raw, &s); err != nil {
		return sidecarHooks{Raw: raw}, fmt.Errorf("parse: %w", err)
	}
	return sidecarHooks{Raw: raw, OnCreate: s.OnCreate, OnRestore: s.OnRestore}, nil
}

// normaliseRebindArg accepts either an absolute sidecar path or a
// directory; in the latter case, append the sidecar basename so the
// caller sees a consistent shape downstream. Empty input passes through
// (the callsite validates absoluteness afterwards).
func normaliseRebindArg(arg string) string {
	if arg == "" {
		return arg
	}
	if strings.HasSuffix(arg, "/"+trustProjectSidecarBasename) || filepath.Base(arg) == trustProjectSidecarBasename {
		return arg
	}
	// If it doesn't end in `.wezsesh.json`, treat it as a directory and
	// append the sidecar basename.
	return filepath.Join(arg, trustProjectSidecarBasename)
}

// bytesEqual is a constant-time-aware byte-equality check. Length check
// short-circuits the common "obviously different" case; the inner
// compare is a straight byte loop (timing-side-channel concerns don't
// apply to user-supplied trust input on a single-user host, but the
// shape mirrors trust.VerifyRebindEligible for consistency).
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// plural returns single when n == 1, else many. Used for human-readable
// "0 entries" / "1 entry" / "2 entries" CLI output without depending on
// a third-party pluralisation library.
func plural(n int, single, many string) string {
	if n == 1 {
		return single
	}
	return many
}
