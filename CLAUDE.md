# CLAUDE.md

Project: **wezsesh** — wezterm session manager TUI. Go bubbletea binary + Lua
plugin, sitting between `smart_workspace_switcher.wezterm` and
`resurrect.wezterm`. Single-user host threat model.

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
| Validating "does the platform already do this?" before touching `wezterm.mux.*` / `wezterm.action.*` / MuxWindow / Pane / GUI window / resurrect APIs | `wezterm-platform-research` (read-only; gates impl) |

When work spans more than one surface, prefer to split it into multiple tasks
each owned by a single agent. The wire-protocol path is intentionally split:
Go side is `wire-protocol-guardian`'s, Lua side is too — same agent, both
sides — because byte-equality requires one mind on both encoders.

`wezterm-platform-research` is a read-only research agent — it never writes
code. It exists because we have a recurring failure mode where implementation
agents see a user complaint, design clever Lua/Go code, and ship it without
checking whether wezterm already handles the case natively. The
`platform-path-first` invariant below makes consulting it a hard
prerequisite for any change that touches the wezterm/resurrect Lua API.

## Plugin layers

Where things live inside `plugin/`. Each layer is enforced by a custom
lualint rule that lands fail-closed in CI; stay inside the layer
boundaries and the rules become invisible.

| Layer | Owns | Enforced by |
|---|---|---|
| `plugin/init.lua` | `apply_to_config` entry point, the `package.loaded["wezsesh.*"]` cache-bust loop, the GLOBAL schema-version stamp, the event-subscription set | `lua-package-loaded-bust-loop`, `lua-resurrect-error-register-presence` |
| `plugin/wezsesh/runtime/` | accessors for the shared global surface — `log` (wezterm.log_warn / log_error), `resurrect_ref` (the user-supplied resurrect plugin), `globals` (typed scalar `wezterm.GLOBAL.wezsesh_*` accessors), `state` (per-pane buckets) | `lua-resurrect-ref-only`, `lua-globals-only` |
| `plugin/wezsesh/verbs/` | per-verb modules (`save`, `load`, `switch`, `new`, `noop`); each exports `{args_shape, dispatch}` and is registered in `verbs/init.lua` | `lua-verb-contract` |
| `plugin/wezsesh/crypto/` | project-owned crypto wrappers (`b64`, `hmac`, `ct_eq`); may only `require("wezsesh.vendor.*")`, never business modules | `lua-crypto-vendor-only` |
| `plugin/wezsesh/vendor/` | third-party crypto (`sha2.lua`); pinned in `SOURCES.lock` | `sha256sum -c plugin/wezsesh/vendor/SOURCES.lock` |
| `plugin/wezsesh/{canonical_json,ipc,manager,result,on_pane_restore,resurrect_error}.lua` | wire protocol, IPC, resurrect interop, reply emitter | wire-protocol byte-equality gate (`LC_ALL=C go test ./internal/canonicaljson/...`) |
| `plugin/spec_helpers.lua` | shared scaffolding for `*_spec.lua` (deepcopy, JSON codec, GLOBAL proxy, minimal-wezterm preload) | self-test: `lua plugin/spec_helpers.lua` |

## Load-bearing invariants

These do not change. Implementation agents already encode them; this list is
the user-facing checklist if you're ever working on a task without an agent.

- **Wire protocol byte-equality.** Go and Lua canonical-JSON encoders must
  produce identical bytes for every shape in the canonical-JSON corpus. HMAC
  field-removal sequence intact; no `hmac=""` set-then-encode.
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
  Enforced by `lua-g-wezterm`, `lua-debug-ban`, `lua-dofile-ban`,
  `lua-wezterm-global-nested-table` (sandbox), plus `lua-globals-only`
  and `lua-resurrect-ref-only` (state ownership — every
  `wezterm.GLOBAL.wezsesh_*` access lives in `runtime/globals.lua` or
  `runtime/state.lua`; every `_G.resurrect` / `_G.wezsesh_resurrect`
  access lives in `runtime/resurrect_ref.lua`).
- **TUI discipline.** No `tea.After` (does not exist). Retransmit is `tea.Tick`.
  OSC writes go through `internal/uservar.Writer` (writes to `/dev/tty`, NOT
  `os.Stdout`). Every render of a disk-sourced string passes through
  `nameval.SanitizeForDisplay`.
- **Trust hash.** Length-prefixed (`uint32_be(len) || bytes || ...`). Any `\n`
  separator is a CVE.
- **Default fail-CLOSED.** Project + snapshot sidecars; trust missing → no exec.
  Hook crash → `pane:send_text("\r\n")` only; NEVER `default_on_pane_restore`.
- **OSC ≤ 256 B.** Forward path uses a sidecar request file. The OSC carries a
  pointer only; oversized OSCs re-open the renderer race and are rejected by
  `uservar.WriteOSC`.
- **Concrete `Dispatcher`** lives only in `internal/ipcdispatcher/`. CI lint
  catches direct `ipcsock.StartListener` callsites elsewhere.
- **Platform path first.** Before introducing custom logic against
  `wezterm.mux.*`, `wezterm.action.*`, MuxWindow / MuxTab / Pane methods,
  GUI window methods, workspace identity, pane visibility, scrollback
  restoration, or anything resurrect.wezterm exposes, route a prompt
  through the `wezterm-platform-research` agent first. Its verdict gates
  the implementation. If the verdict is `platform-handles-this`, the
  implementation MUST use the platform path even if a local custom fix
  is shorter — wezterm hides non-active-workspace MuxWindows
  automatically, and we keep relearning the cost of fighting that. The
  bypass mode is "verdict says `custom-justified` with cited evidence."
  Same rule applies when delegating to `lua-plugin-engineer`,
  `resurrect-interop-engineer`, or `wezterm-interop-engineer`: they
  consult the research agent before designing, not after shipping.

## Build & test commands

These pin the canonical commands. The CI matrix is the source of truth; use
these for local parity.

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

# Custom Lua lints
go run ./cmd/lualint plugin/

# Reproducible release build
go build -trimpath -ldflags="-s -w -X main.version=v$(git describe --tags --always)" ./cmd/wezsesh
```

## Adding to plugin/

Concrete recipes for the four most common plugin extensions. Each recipe
is one file's worth of work; the lualint rules and the wire-protocol
gate catch the things that have to stay aligned across the layers.

### Adding a verb

1. Drop a file in `plugin/wezsesh/verbs/` exposing
   `{args_shape, dispatch}`:
   ```lua
   local M = {}
   M.args_shape = { foo = "string", bar = "integer" }
   function M.dispatch(args, window, pane)
       -- ...
       return result.completed{ data = {} }
   end
   return M
   ```
2. Register it in `verbs/init.lua` next to the existing entries.
3. The `lua-verb-contract` rule asserts the export shape; the
   wire-protocol byte-equality gate
   (`LC_ALL=C go test ./internal/canonicaljson/...`) asserts the
   canonical encoder honours the new shape with the same byte-output
   on Go and Lua.

A future contributor should be able to add a sixth verb without
grepping outside `plugin/wezsesh/verbs/`.

### Adding a shared global

1. Add a typed accessor pair to `plugin/wezsesh/runtime/globals.lua`:
   ```lua
   function M.set_my_thing(v) wezterm.GLOBAL.wezsesh_my_thing = v end
   function M.my_thing()      return wezterm.GLOBAL.wezsesh_my_thing end
   ```
2. Call those accessors from elsewhere in the plugin — never touch
   `wezterm.GLOBAL.wezsesh_*` directly.

The `lua-globals-only` rule keeps every `wezterm.GLOBAL.wezsesh_*`
touch inside `runtime/globals.lua` or `runtime/state.lua`. If you
need a per-pane bucket (a table indexed by pane id, with JSON-encoded
values to dodge the no-nested-tables-in-GLOBAL invariant), follow
`runtime/state.lua`'s pattern; the rule covers `state.lua` as the
second authorised accessor.

### Reaching into the resurrect plugin

Call `runtime/resurrect_ref.get()`. It returns the user-supplied
resurrect module if `apply_to_config` was passed `opts.resurrect`,
falling back to `_G.resurrect` (the legacy global the resurrect
plugin used to install).

The `lua-resurrect-ref-only` rule blocks `rawget(_G, "resurrect")`
and `rawget(_G, "wezsesh_resurrect")` everywhere except
`runtime/resurrect_ref.lua`. Don't add a `_set_deps{ resurrect = ... }`
hatch in your module — the central accessor already exists.

### Adding a crypto primitive

1. Vendor the upstream Lua source under `plugin/wezsesh/vendor/`,
   add a sha256 entry to `SOURCES.lock`, and make sure
   `sha256sum -c plugin/wezsesh/vendor/SOURCES.lock` passes.
2. Wrap the vendored module in `plugin/wezsesh/crypto/`:
   ```lua
   local sha2 = require("wezsesh.vendor.sha2")
   local M = {}
   function M.do_thing(...) ... end
   return M
   ```

The `lua-crypto-vendor-only` rule blocks `crypto/*.lua` from
importing business modules — `result`, `ipc`, `runtime/*`, etc. If
your wrapper needs to look at config or state, restructure: the
business module should call into `crypto`, not the other way around.

## Conventions

- **VCS is jj-colocated; use `jj` for ALL VCS operations.** `.jj/` + `.git/`
  sit side by side, but every commit, diff, log, status, branch, rebase, or
  history operation goes through `jj` — never `git`. Canonical commands:
  `jj st`, `jj log`, `jj diff`, `jj describe`, `jj commit`, `jj new`,
  `jj rebase`, `jj bookmark`, `jj git push`. The only acceptable `git` usage
  is read-only inspection by external tools you don't control (CI, IDE,
  `gh pr` for GitHub interactions) — when *you* take an action, reach for
  `jj`. If a workflow seems to require a `git` command, find the `jj`
  equivalent first.
- **Don't push from agents.** Pushes are user-initiated (`jj git push -b main`).
- **Comments are rare.** Default to none; add only when the WHY is non-obvious
  (a hidden invariant, a workaround for a specific bug). The codebase already
  cites § headings; redundant `// per §X` comments are noise.
