# PRD Research Findings

Research outcomes for the four deferred items in `PRD_V1.md` §8.2, plus a bonus pass on the wezterm Lua/CLI surface that uncovered architectural issues in the v1 PRD.

Sources are cited inline. **Boldface "→" lines** are the resulting decisions to fold into the PRD.

---

## 0. Architectural corrections (highest priority)

These are not "deferred items" — they're errors in PRD_V1's architecture that surfaced during the bonus Lua/CLI pass.

### 0.1 `wezterm cli set-user-var` does not exist

PRD_V1 §6.2/§6.3/§6.4 assume the Go binary calls `wezterm cli set-user-var wezsesh_op '<json>'` to push events into the Lua context. **This subcommand does not exist.** Verified locally (`wezterm 577474d`: `error: unrecognized subcommand 'set-user-var'`) and against open feature request [wezterm#7307](https://github.com/wezterm/wezterm/issues/7307).

**Available `wezterm cli` subcommands**: `list`, `list-clients`, `proxy`, `tlscreds`, `move-pane-to-new-tab`, `split-pane`, `spawn`, `send-text`, `get-text`, `activate-pane-direction`, `get-pane-direction`, `kill-pane`, `activate-pane`, `adjust-pane-size`, `activate-tab`, `set-tab-title`, `set-window-title`, `rename-workspace`, `zoom-pane`.

**The actual mechanism** for setting a user-var from a subprocess is OSC 1337:

```
printf '\033]1337;SetUserVar=%s=%s\007' KEY $(printf '%s' VALUE | base64)
```

The binary writes this directly to its own stdout (its stdout IS a wezterm pane, since the binary was spawned by wezterm). Notes:
- Value MUST be base64-encoded.
- Must use unwrapped base64. Use `base64 -w0` on Linux; on macOS strip trailing newline.
- No documented max length, but OSC sequences over a few KB get sketchy. Our payloads are small JSON objects — fine.
- Reference: [Passing Data from a pane to Lua](https://wezterm.org/recipes/passing-data.html).

**→ PRD update**: replace every `wezterm cli set-user-var` reference with the OSC 1337 mechanism. Add a small Go helper (`internal/userv ar/`) that wraps it.

### 0.2 `user-var-changed` is broadcast across windows (not scoped)

PRD_V1 §6.5 says "Each plugin listener checks `target_window_id` against `window:window_id()` and silently no-op on mismatch." This is correct. The reason it's needed: per [wezterm#3524](https://github.com/wezterm/wezterm/issues/3524), the `user-var-changed` event fires in **every window of the parent mux process** when any pane sets a user-var, not just the window containing the pane. Our `target_window_id` filter is the right fix.

**→ PRD update**: cite issue #3524 in §6.5 so future maintainers know why the filter is structural, not paranoia.

### 0.3 No `mux.kill_workspace` — delete-live requires a sweep

There is no `wezterm.mux.kill_workspace` or equivalent. Removing a live workspace requires enumerating `mux.all_windows()`, filtering by `:get_workspace()`, and closing each window. PRD_V1 currently doesn't claim to delete *live* workspaces (delete is snapshot-only) but if we ever want that capability we need to spec the sweep.

**→ PRD update**: add a footnote in §6 clarifying that `delete` removes the snapshot file only; killing the live workspace is a separate operation we may add later (and would require `mux.all_windows()` enumeration).

### 0.4 `wezterm cli rename-workspace` is native — skip the Lua roundtrip

`wezterm cli rename-workspace NEW [--workspace OLD] [--pane-id N]` exists. The rename op can be done entirely from the Go side without Lua dispatch (and the snapshot file rename is also direct disk I/O). This simplifies the rename path: only the *snapshot-file* + state-file updates need Lua coordination, and even those can be pure Go.

**→ PRD update**: rename can be a Go-only verb. Lua dispatch is only required for ops that touch live mux state in a way the CLI doesn't expose.

### 0.5 `mux.set_active_workspace` does NOT create

`set_active_workspace(name)` raises if the workspace doesn't exist. The create-or-switch primitive is `act.SwitchToWorkspace { name, spawn = { cwd = path } }` (note: that's an `act` keyassignment, not a `mux` function). For our IPC handler doing a "switch + restore" verb, we use `act.SwitchToWorkspace` via `window:perform_action(...)`.

**→ PRD update**: §6.2 step 7 ("Plugin performs the action") should reference `act.SwitchToWorkspace` for switch verbs that may need to create.

---

## 1. Deferred §C4 — Resurrect schema-drift policy

**Investigation**: cloned `MLFlexer/resurrect.wezterm` (the canonical repo, confirmed via README's `wezterm.plugin.require` URL).

### Findings

- **No version field exists, never has.** `grep -rn "version\|schema" plugin/` returns nothing. `file_io.load_json` (`plugin/resurrect/file_io.lua:99`) just `wezterm.json_parse`s and returns the table. No validation.
- **Schema has silently mutated.** Documented changes from git log of `plugin/resurrect/pane_tree.lua`:
  - `process` field changed from `string` (e.g. `"/bin/bash"`) to a `local_process_info` object `{name, argv, cwd, executable}` (commit `3d79f175`, 2024-08-25).
  - `text` field changed from `string[]` to `string`.
  - `alt_screen_active` flag added; `process` is only populated when this is true and the domain is local.
  - `domain` field added (commit `a842c86`).
  - `is_active` / `is_zoomed` added incrementally.
  - `window_state.size` added later.
- **No migration code anywhere.** No deprecation policy, no `if old_field then` shims. Real users have files spanning these eras on disk *right now*.
- **README example is stale** — still references `"process":"/bin/bash"` string form.

### Verified field names (for Go structs)

Top-level workspace JSON (`workspace_state.lua:50-61`):
```
workspace        string
window_states    []WindowState
```

WindowState (`window_state.lua:8-25`):
```
title             string
tabs              []TabState
size              { rows, cols, pixel_width, pixel_height, dpi }   // all int
```

TabState (`tab_state.lua:71-77`):
```
title       string
is_active   bool
is_zoomed   bool
pane_tree   PaneTree (recursive)
```

PaneTree (`pane_tree.lua:11`, populated lines 78-112):
```
left, top, height, width   int           // cell coords
cwd                        string         // "" if unavailable
domain                     string?        // unset for non-spawnable domains
process                    LocalProcInfo? // only when alt_screen_active=true && domain=="local"
text                       string?        // only when alt_screen_active=false && domain=="local"
alt_screen_active          bool?          // local domain only
is_active, is_zoomed       bool
bottom, right              PaneTree?      // recursive binary split tree
```

LocalProcInfo:
```
name        string
argv        []string
cwd         string
executable  string
```

### Filename encoding (verified, multi-OS)

`state_manager.lua:11-21` — `get_file_path` does `file_name:gsub("/", "+")` (Unix). Layout:

```
<save_state_dir>/{workspace,tab,window}/<encoded-name>.json
<save_state_dir>/current_state               # two-line text file: name, type
```

PRD_V1's claim is correct: `/` → `+` on Unix. wezsesh mirrors this on reverse-mapping.

### Restore error behavior

`grep -n "pcall\|error" plugin/resurrect/{workspace,window,tab}_state.lua` returns **zero matches**. The restore path has no error handling at all:

- `workspace_state.lua:9-46` `restore_workspace`: bare `for` over `window_states`. If `wezterm.mux.spawn_window` raises (line 40), the entire restore aborts, remaining windows are not attempted, the `restore_workspace.finished` event never fires.
- `window_state.lua:45-84`: same — `window:spawn_tab` raising aborts the rest of the tabs. Also, line 82 `active_tab:activate()` will nil-deref crash if no tab had `is_active=true`.
- `tab_state.lua:96-120`: `pane:split` raising aborts the tab; `make_splits` raising mid-fold leaves a partially-restored tab.

The only error catching is on encryption I/O (`file_io.lua` pcalls encrypt/decrypt) and a single coarse pcall around the GUI-startup restore path. There is no per-pane retry, no log-and-continue, no partial-restore mode. The `resurrect.error` event exists but is only emitted from a few places, not from spawn failures.

### Decision

**→ Tolerant parser, no version-aware logic.** Specifically:

1. Don't try version-aware branching — there's no version to detect.
2. Treat every field as optional/pointer in Go. Use a custom unmarshaler for `process` that handles both `string` and `LocalProcInfo` shapes (or accept it via `json.RawMessage` and parse lazily).
3. Recurse `pane_tree.bottom`/`right` defensively.
4. Show "—" in the preview pane when expected metadata is missing. Don't crash.
5. Log and skip individual files that fail JSON parse; the picker keeps rendering.
6. Mirror resurrect's own laxity. Don't be stricter than the source of truth.

**This resolves PRDQ §C4 — move out of deferred.**

---

## 2. Deferred §C7 — Restore-partial-failure behavior

Already mostly answered above. Resurrect itself has zero error handling on restore — first spawn failure aborts the whole workspace with an unhandled Lua error, and `restore_workspace.finished` never fires.

**Implications for wezsesh**:

- Wrapping `resurrect.workspace_state.restore_workspace` in `pcall` is mandatory. Without it, our Lua handler crashes uncleanly.
- Even with pcall, on first failure we get an aborted restore with N-of-M tabs created. There's no rollback path that resurrect provides; we'd have to walk the partial mux state ourselves.
- Listening to `resurrect.workspace_state.restore_workspace.finished` is **not reliable** as a success signal — that event only fires on the success path. Use the `pcall` return value.

### Decision

**→ For v0.1: wrap restore in pcall; on failure, surface `RESURRECT_PARTIAL` via `wezsesh_op_result` with the Lua error message; do not attempt rollback.** Document explicitly: "Partial restores leave the workspace in whatever state resurrect produced; we don't roll back. Re-running restore is safe (idempotent at the workspace level — you'll get duplicate tabs)." This matches user expectation set by tmux session managers, none of which do transactional restore either.

**This resolves PRDQ §C7 — move out of deferred.**

---

## 3. Deferred §D2 — `on_restore` hook trust model

Prior art surveyed: direnv, VSCode Workspace Trust, sesh, tmuxp, tmuxinator, smug, tmux-sessionizer.

### Pattern matrix

| Tool | Storage | Trust model | Auto-runs? |
|---|---|---|---|
| **direnv** | `.envrc` in project | Hash-based allow-list at `~/.local/share/direnv/allow/`; hash = sha256(path + content); fail-closed on unknown/changed | After `direnv allow` |
| **VSCode** | `tasks.json` in workspace | Workspace Trust modal on first open; tasks disabled in Restricted Mode | Only after trust + run |
| **sesh** | `~/.config/sesh/sesh.toml` | None — config dir implicitly trusted | Yes, on session create |
| **tmuxp** | `~/.tmuxp/*.yaml` or project `.tmuxp.yaml` | None | Yes, on `tmuxp load` |
| **tmuxinator** | `~/.config/tmuxinator/*.yml` | None | Yes |
| **smug** | `~/.config/smug/*.yml` or project `.smug.yml` | None | Yes |
| **tmux-sessionizer** | env var + user-authored shell script | None — author == user | Yes |

The tmux ecosystem is uniformly "config dir is trust boundary, no prompts." This works for them because configs don't typically travel as opaque sidecars — users author them.

**wezsesh's threat model is closer to direnv/VSCode**: sidecars sit in a state directory written by tooling, are easy to clobber/sync/copy, and aren't obviously author-authored.

### Decision: direnv-style hash-based trust, fail-closed

**→ Concrete spec**:

**Sidecar location**: `<snapshot_dir>/workspace/<encoded-name>.wezsesh.json` (already in PRD_V1 §6.6).

**Sidecar fields**:
```json
{
  "version": 1,
  "tags": ["api"],
  "pinned": false,
  "on_create": null,
  "on_restore": "docker compose up -d && nvim ."
}
```

**Trust store**: `$XDG_DATA_HOME/wezsesh/allow/<sha256>` (fall back to `~/.local/share/wezsesh/allow/`). Filename = `sha256(<absolute_sidecar_path> || "\n" || <on_restore_bytes>)`. File contents = the absolute sidecar path (for `wezsesh trust list`).

**Default flow (fail-closed)**:
1. User triggers restore.
2. Workspace restores normally.
3. Hook check: trust file missing or hash mismatch → hook is **skipped silently**.
4. Logged via `wezterm.log_warn` and stderr to the binary's pane:
   ```
   wezsesh: on_restore for "<name>" is not trusted on this machine.
     command: <the command>
     approve: wezsesh trust <name>
     reject : wezsesh trust --revoke <name>
   ```
5. After `wezsesh trust <name>`, subsequent restores run the hook silently until the command changes.

**Optional interactive prompt** (`hooks.prompt_on_untrusted = true`):
```
wezsesh: workspace "<name>" has an on_restore hook that is not trusted.

  command: <the command>
  source : <sidecar path>

Trust and run this command? [y/N/show diff]:
```
Default `N`. `show diff` prints previously-trusted command (if any) vs current; re-prompts.

**CLI**:
- `wezsesh trust <name>` — approve current sidecar's command.
- `wezsesh trust --revoke <name>`
- `wezsesh trust --list`
- `wezsesh trust --prune` (orphans)
- `wezsesh trust --show <name>` — print without running.

**Global escape hatches**:
- `wezsesh.hooks.run_hooks = false` in Lua config.
- `WEZSESH_NO_HOOKS=1` env var (beats config).

**This resolves PRDQ §D2 — move to v0.1 scope.** The implementation cost is moderate (sha256, trust dir I/O, one CLI subcommand) but the security cost of NOT having it is high (we'd be telling users "store shell commands in JSON files, we'll auto-run them").

---

## 4. Deferred §D3 — Cross-workspace process search

### Wezterm CLI capabilities

`wezterm cli list --format json` per-pane fields (verified live):

```
window_id, tab_id, pane_id          int
workspace                            string
size                                 { rows, cols, pixel_width, pixel_height, dpi }
title                                string   ← dynamic, derived from foreground process or OSC 0/1/2
cwd                                  string   ← file:// URL
cursor_x, cursor_y                   int
cursor_shape, cursor_visibility      enum
left_col, top_row                    int
tab_title                            string   ← user-set tab title
window_title                         string   ← user-set window title
is_active                            bool     ← per-tab, NOT global focus
is_zoomed                            bool
tty_name                             string|null  ← e.g. "/dev/ttys005"
```

**Gap**: there is **no `foreground_process_name` field** in CLI JSON. It exists on the Lua-side `PaneInformation` but is not serialized. The `title` field is a decent proxy (often shows `nvim`, `pgcli`, `claude` etc.) but is user-/program-overridable.

**Fallback**: `tty_name` lets us shell out to `ps -t <tty> -o stat=,comm=,args=` and pick the row with `+` in STAT (foreground process group marker). This is precise but slow at O(panes) ps invocations.

### Cross-pane visibility

`wezterm cli` connects to one mux server via `$WEZTERM_UNIX_SOCKET`. A subprocess in any pane sees **all windows, all tabs, all workspaces** of that GUI's mux. Confirmed live: `cli list` from one pane returned panes from `default`, `~/.dotfiles`, and `~/code/lucra-labs/lucra` — all visible from a single invocation.

Caveats: detached headless `wezterm-mux-server` and remote `wezterm.connect_*` SSH/TLS domains use different sockets. For v0.1 single-GUI use, `$WEZTERM_UNIX_SOCKET` is sufficient.

### Foreground process accuracy

From wezterm's mux/localpane source: on Unix, uses `tcgetpgrp(fd)` on the pty master, then resolves via `proc_pidpath` (macOS) or `/proc/<pid>/exe` (Linux). This returns the actual tty foreground process group leader — `nvim`, `pgcli`, `node`, not the parent shell — when it works. Documented quirks ([#1898](https://github.com/wezterm/wezterm/issues/1898), [#3995](https://github.com/wezterm/wezterm/issues/3995)) cover edge cases (background jobs, unusual shells like nu) where it returns the shell instead. None of this is exposed via `cli list`.

### Decision

**→ Ship `wezsesh find` as a top-level subcommand in v0.1.** Rationale: process search is a different mental task from workspace switching (you don't know *which* workspace), so a top-level entry point matches user intent. Also script-friendly.

**Spec**:
```
wezsesh find [PATTERN] [--deep] [--json] [--cwd] [--workspace WS]
```

**Match strategy (default)**: case-insensitive substring against `title`, `tab_title`, `window_title`, and the path component of `cwd`. With `--deep`: also run `ps -t <tty_name> -o stat=,comm=,args=` per pane and match against the foreground-pgrp row. `--deep` is opt-in (slow).

**Result row** (TUI + plain stdout):
```
WORKSPACE        TAB              PANE  TITLE                  CWD                    PROC
~/code/wezsesh   "edit"           35    nvim                   ~/code/wezsesh         nvim
default          (untitled)       3     ⠐ Claude Code          ~/.dotfiles            claude
```

**Interactive mode** (no PATTERN, or `Ctrl-/` in main TUI): renders rows in a fuzzy picker. Enter → `wezterm cli activate-pane --pane-id N` then `activate-tab --tab-id T`. `Ctrl-w` switches workspace without focusing pane. `Ctrl-y` copies `pane_id` to clipboard.

**Out of scope**: remote-domain panes (SSH/TLS muxes), headless mux-server discovery, scrollback grep (`get-text`-based search). Track for later.

**This resolves PRDQ §D3 — move to v0.1 scope.**

---

## 5. Bonus: verified Lua API surface

For PRD §6.2/§6.3/§7.1 accuracy. All of these are confirmed-existent at the cited URLs.

- `wezterm.mux.rename_workspace(old, new)` — exists since 20230408. Error semantics undocumented; wrap in pcall.
- `wezterm.mux.get_active_workspace()` — exists.
- `wezterm.mux.set_active_workspace(name)` — exists; **raises if workspace doesn't exist**.
- `wezterm.mux.get_workspace_names()` — exists.
- `wezterm.mux.spawn_window{ workspace, args, cwd, domain, set_environment_variables, width, height, position }` — returns `tab, pane, window`.
- `wezterm.mux.all_windows()` — returns `[]MuxWindow`. Used with `:get_workspace()` for per-workspace enumeration.
- `act.SwitchToWorkspace { name = string?, spawn = SpawnCommand? }` — **creates if missing**. Use this for create-or-switch semantics.
- `MuxWindow:get_workspace()` / `:set_workspace(name)` — exist; useful for moving windows between workspaces.
- `window:window_id()` — returns int, stable for window's lifetime.
- `act.PromptInputLine { description, prompt?, initial_value?, action }` — `line` is nil on Esc/Ctrl-C. Callable from `user-var-changed` via `window:perform_action(...)`.
- `wezterm.GLOBAL` — only persistent Lua-side state across config reloads.

**Resurrect API** (`require` returns a table with five submodules):
- `state_manager.save_state(state, opt_name?)` — sync.
- `state_manager.load_state(id, "workspace"|"window"|"tab")` — sync.
- `state_manager.change_state_save_dir(path)` — setter.
- `state_manager.save_state_dir` — module-level field; **readable but not documented as stable API**. Safer: set explicitly via `change_state_save_dir` and remember the path on the Go side.
- `state_manager.periodic_save{ interval_seconds, save_workspaces, save_windows, save_tabs }`.
- `workspace_state.get_workspace_state()` / `restore_workspace(state, opts)` — sync.
- `window_state.restore_window(window, state, opts)` — sync.
- `tab_state.restore_tab(tab, state, opts)` — sync.
- Subscribable events: `resurrect.error`, `resurrect.{state_manager.save_state,load_state,delete_state,periodic_save}.{start,finished}`, `resurrect.{workspace_state.restore_workspace,window_state.restore_window,tab_state.restore_tab}.{start,finished}`, `resurrect.fuzzy_loader.fuzzy_load.{start,finished}`, `resurrect.file_io.{encrypt,decrypt,write_state,sanitize_json}.{start,finished}`.

**Plugin loading** (`wezterm.plugin.require`):
- HTTPS git URL or `file://`. Clones to `<runtime_dir>/plugins/NAME` on first call. **Does not auto-update.**
- Repo MUST have `plugin/init.lua` at root. Not configurable.
- **Lua only** — does not build, install, or place binaries. PRD's manual-binary-install decision is correct.

---

## Summary of changes for PRD_V1.md

### Architectural (must fix — current spec is wrong)
- §6.1, §6.2, §6.3, §6.4: replace `wezterm cli set-user-var` with OSC 1337 `SetUserVar` mechanism. Add Go helper module `internal/uservar/`.
- §6.5: cite issue #3524 for the multi-window broadcast filter rationale.
- §6.6: replace the snapshot schema description with verified field names. Note filename encoding (`/` → `+`). Note that `process` field has both string and object shapes in the wild and parser must handle both.
- §6.10: add `internal/uservar/` and `internal/trust/` directories.

### Move from deferred to v0.1
- `on_restore` hook with direnv-style trust model (full spec in §3 above).
- `wezsesh find` cross-workspace process search subcommand (full spec in §4 above).
- Resurrect schema: tolerant parser policy (no longer needs investigation; investigated).
- Restore-partial-failure: pcall + surface `RESURRECT_PARTIAL` (no longer needs investigation).

### Slimmer deferred section
After this round, the only items still genuinely deferred are:
- Remote-domain pane discovery for `find` (SSH/TLS muxes).
- Scrollback grep via `get-text`.
- `name_truncate = "left" | "none"` (just config knobs).
- Mouse / SSH / multi-host (PRD non-goals).
- Cloud sync (PRD non-goals).
