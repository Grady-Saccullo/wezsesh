# CLAUDE.md

Project: **wezsesh** — wezterm session manager TUI. Go bubbletea binary + Lua
plugin, sitting between `smart_workspace_switcher.wezterm` and
`resurrect.wezterm`. Single-user host threat model.

## Build state

Pre-code. Iteration backlog lives in [`PROJECT.md`](PROJECT.md). Advance the
build with `/next-task` (see `.claude/skills/next-task/SKILL.md`); do NOT
hand-roll tasks unless the user explicitly asks. The skill keeps state in
`PROJECT.md` so any fresh session can resume.

## Spec sources

- `docs/design.md` — normative technical spec (contracts, APIs, schemas, state
  machines). § refs in `PROJECT.md` and tasks point here. § numbers may drift;
  prefer matching by heading text.
- `docs/prd.md` — rationale + UX. Cite as `(P §x.y)`.
- `docs/issues/{1,2,3}.md` — spike findings that drove the most non-obvious
  parts of the design. Read these whenever a task names "(spike-#N)":
  - **#1** — IPC integration spike. mlua sandbox constraints, `wezterm.GLOBAL`
    value-shape rule, `apply_to_config` cache-bust, PTY-multiplexer caveat,
    HMAC fixture correction.
  - **#2** — `resurrect.error` capture. Why `pcall(state_manager.save_state)`
    is empirically broken; the dual-path detection scheme (pcall + capture).
  - **#3** — Sidecar forward path. Why the OSC carries only a ≤256 B pointer
    and the canonical-JSON request lives on disk; the renderer-OSC interleave
    race that motivated it.
- The 8 specialist agents under `.claude/agents/` own per-area invariants — see
  the routing table below.

## Agent routing table

| Surface | Owner agent |
|---|---|
| `internal/safefs/`, `internal/state/`, file locking, atomic writes | `safefs-engineer` |
| `internal/canonicaljson/`, `internal/hmac/`, `internal/ipc*/`, `internal/uservar/`, `plugin/wezsesh/{canonical_json,hmac,ct_eq,ipc,result}.lua` | `wire-protocol-guardian` |
| `internal/wezcli/`, `internal/find/` | `wezterm-interop-engineer` |
| `internal/snapshots/`, `internal/argvallow/`, `plugin/wezsesh/{on_pane_restore,resurrect_error,default_allowlist}.lua` | `resurrect-interop-engineer` |
| `internal/trust/`, hook execution, `wezsesh trust` CLI | `trust-and-hooks-engineer` |
| `internal/tui/`, `internal/pathpicker/`, modal flows, key bindings | `bubbletea-tui-engineer` |
| `plugin/wezsesh/*.lua`, `plugin/init.lua` | `lua-plugin-engineer` |
| Read-only post-implementation audit | `design-conformance-reviewer` |

When work spans more than one surface, prefer to split it into multiple tasks
each owned by a single agent. The wire-protocol path is intentionally split:
Go side is `wire-protocol-guardian`'s, Lua side is too — same agent, both
sides — because byte-equality requires one mind on both encoders.

## Load-bearing invariants

These do not change. Implementation agents already encode them; this list is
the user-facing checklist if you're ever working on a task without an agent.

- **Wire protocol byte-equality.** Go and Lua canonical-JSON encoders must
  produce identical bytes for every shape in the §17.1 corpus. HMAC field-removal
  sequence intact (§4.3); no `hmac=""` set-then-encode.
- **Filesystem.** Every disk write under wezsesh-managed dirs (state, data,
  snapshot, runtime) goes through `safefs.AtomicWriteFile`. Every path-touch
  goes through `safefs.Enforce(...)`. `unix.F_OFD_SETLK` only inside
  `internal/safefs/lock_linux.go`. Locks held briefly around verify-hash, NEVER
  across IPC.
- **Wezterm CLI.** All `wezterm cli` invocations live inside `internal/wezcli/`.
  No direct `exec.Command("wezterm", ...)` elsewhere.
- **Lua handler synchrony.** `user-var-changed` steps (a)–(h) contain ZERO
  async wezterm calls. `wezterm.background_child_process` permitted in step (i)
  only, always `pcall`-wrapped.
- **mlua sandbox.** No `_G.wezterm` (always `local wezterm = require("wezterm")`).
  No `debug.*`. No `dofile(`. No nested-table values into `wezterm.GLOBAL`.
- **TUI discipline.** No `tea.After` (does not exist). Retransmit is `tea.Tick`.
  OSC writes go through `internal/uservar.Writer` (writes to `/dev/tty`, NOT
  `os.Stdout`). Every render of a disk-sourced string passes through
  `nameval.SanitizeForDisplay`.
- **Trust hash.** Length-prefixed (`uint32_be(len) || bytes || ...`). Any `\n`
  separator is a CVE.
- **Default fail-CLOSED.** Project + snapshot sidecars; trust missing → no exec.
  Hook crash → `pane:send_text("\r\n")` only; NEVER `default_on_pane_restore`.
- **OSC ≤ 256 B.** Forward path uses sidecar request file (spike #3). The OSC
  carries a pointer only; oversized OSCs re-open the renderer race and are
  rejected by `uservar.WriteOSC`.
- **Concrete `Dispatcher`** lives only in `internal/ipcdispatcher/`. CI lint
  catches direct `ipcsock.StartListener` callsites elsewhere.

## Build & test commands

These pin the canonical commands. The CI matrix in §16.4 is the source of
truth; use these for local parity.

```bash
# Verify modules
go mod verify

# Vet + static analysis
go vet ./...
staticcheck ./...
govulncheck ./...

# Tests (locale-sensitive subset must run with LC_ALL=C)
go test -race ./...
LC_ALL=C go test ./internal/canonicaljson/... ./plugin/...

# Vendored crypto integrity
sha256sum -c plugin/wezsesh/vendor/SOURCES.lock

# Custom Lua lints (T-005 onward)
go run ./cmd/lualint plugin/

# Reproducible release build
go build -trimpath -ldflags="-s -w -X main.version=v$(git describe --tags --always)" ./cmd/wezsesh
```

## Conventions

- **VCS is jj-colocated.** `.jj/` + `.git/` side by side. Use `jj` for commits,
  diffs, history (`jj log`, `jj diff`, `jj describe`, `jj commit`). Git tools
  (CI, IDE, `gh pr`) see the colocated `.git/` and work normally. Don't
  invoke `git commit` / `git add` for build work — `/next-task` uses jj's
  auto-snapshot model and a diff-allowlist check instead of explicit staging.
- **One commit per task.** Format: `<type>(<scope>): T-XXX <title>`.
- **Don't push from agents.** Pushes are user-initiated (`jj git push -b main`).
- **Don't edit `docs/design.md` or `docs/prd.md` from build tasks.** Spec gaps
  queue as a `T-DOC-NNN` task in PROJECT.md (auto-handled by `/next-task`'s
  spec-drift logic; see the skill).
- **Comments are rare.** Default to none; add only when the WHY is non-obvious
  (a hidden invariant, a workaround for a specific bug). The codebase already
  cites § headings; redundant `// per §X` comments are noise.
