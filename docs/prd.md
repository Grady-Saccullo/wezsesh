# wezsesh — PRD v1

A workspace/session manager for [WezTerm](https://wezterm.org). Terminal-native TUI plus a thin Lua plugin that brings tmux-sessionizer-style ergonomics to wezterm workspaces.

> v1 of this PRD folds in the decisions from `PRD_questions.md`. Everything in §8 is v0.1 scope unless explicitly deferred.

---

## 1. Problem

WezTerm has no first-class session manager. The current ecosystem covers half the problem each:

- **`smart_workspace_switcher.wezterm`** — fuzzy-picks workspaces from `zoxide`. Switching only. No rename/delete/preview.
- **`resurrect.wezterm`** — persists workspace/window/tab layouts to disk. Snapshots only. No browsing UI beyond a generic `InputSelector`-based fuzzy loader.
- **Custom Lua glue** — chained `act.InputSelector` modals. Modal stack is clunky, no preview, no live update, no live/saved differentiation.

Tmux users have `tmux-sessionizer`, `sesh`, `choose-tree` — all polished, all keyboard-driven. WezTerm has none of that. This is the gap.

## 2. Goals

- One keybinding opens a TUI showing **every** workspace — both live (currently in the mux) and saved (resurrect snapshots on disk) — in one unified view.
- Operations from the TUI: **switch**, **load** (restore snapshot into current workspace), **rename**, **delete**, **save current**, **new from path**, **pin**, **bulk-delete**, **tag**.
- Preview pane visible from day one — no waiting for "v1.0."
- Sub-200ms cold start. Keyboard-only navigation. No mouse required.
- Distributable like any wezterm plugin: `wezterm.plugin.require()` for the Lua side; binary installed via `nix`, `go install`, or GitHub releases (no auto-download — install pattern matches the rest of the ecosystem).
- Plays well with existing plugins. Coexists with `smart_workspace_switcher` and uses `resurrect` as the storage layer.

## 3. Non-Goals (v1)

- Mouse interaction.
- Multi-host / SSH-aware workspace management. wezsesh requires a local mux; multiplexer / SSH-domain workspaces are explicitly unsupported because `pane:send_text` and Unix-socket return channels both require local-pane semantics.
- **Windows is not supported and not planned.** wezsesh targets Unix only: darwin-arm64, darwin-amd64, linux-amd64, linux-arm64. The architecture (Unix sockets for IPC reply, `tty_name`-based foreground-process probe in `wezsesh find --deep`, XDG paths) is Unix-shaped throughout. Wezterm itself runs on Windows, but adding Windows support would mean a parallel IPC + paths + `find` strategy and a sustained dogfooding effort we don't intend to take on.
- Replacing `resurrect` as the persistence layer. We read/write its snapshot files; we don't reinvent them.
- Auto-downloading the binary on first run. Install matches normal wezterm plugin practice.
- Cloud sync of snapshots. Use `git` if you need that.

## 4. Personas & Scenarios

**P1 — Daily switcher.** Hits the TUI 20–50× a day to jump between active projects. Cares about: fuzzy filter, single-keystroke select, instant feel, MRU sort.

**P2 — Spring cleaner.** Once a week deletes stale snapshots and renames sloppy ones. Cares about: bulk visibility, multi-select + bulk delete, mtime sort, confirm-on-delete, overwrite warnings.

**P3 — Restorer.** After reboot, brings back yesterday's setup. Cares about: which workspaces are saved-but-not-loaded, restore landing in the right window, predictable behavior if a snapshot is partial.

**P4 — Project starter.** Starts a new workspace from a directory, optionally with a per-project launch hook (start dev server, open editor). Cares about: fast directory picker, automation on restore.

## 5. UX

### 5.1 Main view

```
┌─ wezsesh ──────────────────────────────────────────────────────┬─ preview ──────────────┐
│ filter: ▌                                                      │ ~/code/foo             │
├────────────────────────────────────────────────────────────────┤  Tab 1: nvim            │
│ ▶ ~/.dotfiles             live    3 tabs    2m ago    [pinned] │   └ pane: ~/code/foo   │
│ ● ~/code/foo              live    5 tabs    1h ago    #api     │  Tab 2: pgcli           │
│   ~/code/bar              saved   3d ago              #db      │   └ pane: ~/code/foo   │
│   default                 saved   1w ago                       │                        │
│   ~/code/scratch          live  (unsaved)              --      │ Last used: 1h ago      │
│ ✓ ~/code/old              saved   42d ago             #archive │ Tags: api              │
├────────────────────────────────────────────────────────────────┴────────────────────────┤
│ s switch  l load  r rename  d delete  S save  n new  p pin  t tag  Tab mark  ? help     │
└─────────────────────────────────────────────────────────────────────────────────────────┘
```

Markers (left column):
- `▶` active workspace in the current window
- `●` live workspace (in mux, not focused)
- (blank) saved-only
- `✓` marked for bulk op
- `[pinned]` annotation appended to row when pinned
- `(unsaved)` annotation appended to row when a live workspace has never been saved (always rendered, regardless of sort)

Pinned workspaces always sort to the top of their group (live or saved), regardless of `sort` mode.

### 5.2 Modes

The TUI has two input modes. The bottom hint line indicates the active mode.

**Nav mode (default)** — single-key bindings active.
**Filter mode** — entered via `/`. Letter keys (including `j`/`k`) are literal characters appended to the filter buffer. Only the keys explicitly listed below operate the picker while in filter mode.

| Key (filter mode)   | Action                                            |
|---------------------|---------------------------------------------------|
| `↑` / `↓`           | Navigate filtered list                            |
| `Ctrl-P` / `Ctrl-N` | Navigate filtered list                            |
| `Enter`             | Trigger `default_action` on highlighted entry     |
| `Esc`               | Exit filter mode (filter buffer preserved until `/` cleared with `Ctrl-U`) |
| `Ctrl-U`            | Clear filter buffer                               |

In nav mode, `j`/`k` move; in filter mode, they insert. Decision rationale: a modal split is easier to reason about and matches how vim users expect a `/` prompt to behave.

### 5.3 Keybindings (nav mode)

| Key       | Action                                                              |
|-----------|---------------------------------------------------------------------|
| `j` / `↓` | Next entry                                                          |
| `k` / `↑` | Previous entry                                                      |
| `gg`/`G`  | Top / bottom                                                        |
| `/`       | Enter filter mode                                                   |
| `Enter`   | `default_action` on highlighted entry (default: switch + restore)   |
| `s`       | Switch to                                                           |
| `l`       | Load snapshot into **current** workspace (with confirm — see §5.7)  |
| `r`       | Rename — text input modal                                           |
| `d`       | Delete — y/N confirm; if rows are marked, deletes all marked        |
| `S`       | Save current workspace as a snapshot                                |
| `n`       | New workspace from path — opens path picker (see §5.6)              |
| `p`       | Toggle pin on highlighted entry                                     |
| `t`       | Edit tags on highlighted entry — text input, comma-separated        |
| `Tab` / `Space` | Toggle mark on highlighted entry (for bulk ops)               |
| `c`       | Clear all marks                                                     |
| `?`       | Toggle help overlay                                                 |
| `q` / `Esc` | Quit (when a write op is in flight: inline `quit anyway? [y/N]` — see §6.16) |

All bindings remappable via `opts.keys.*`; set any to `false` to disable.

### 5.4 Sort & filter

- **Sort modes**, configurable via `opts.sort`:
  - `live_first` (default) — pinned first, then live (active at top), then saved by mtime newest-first.
  - `recent` — pinned first, then ordered by **last switched-to** (from the usage state file, see §6.8). Falls back to mtime for entries with no usage record.
  - `mtime` — pure mtime, no live grouping.
  - `alphabetical` — name order.
- **Fuzzy filter** — subsequence match against display name *and tags*. Match characters are highlighted via the `match_highlight` color.
- Pinned workspaces always sort first within their group regardless of sort mode.

### 5.5 Modals

- **Rename** — text input pre-filled with current name. `Enter` confirms; `Esc` cancels. If the target name already has a snapshot, second confirm: `Overwrite [y/N]`.
- **Delete (single)** — `Delete '~/.dotfiles' [y/N]`. Default `N`.
- **Delete (bulk)** — `Delete N marked workspaces? [y/N]`. Default `N`. Lists names if N ≤ 5; otherwise summarizes.
- **Save** — `Save current workspace as [name]` text input, pre-filled with the current workspace name. If overwriting and the existing snapshot's hash differs from what was loaded into the picker (see §6.7), the modal becomes `Snapshot changed since open. Overwrite anyway? [y/N]`.
- **Load (clobber confirm)** — `l` on a saved entry while the current workspace is non-empty: `Replace current workspace 'foo' with 'bar'? [y/N]`. Skipped when `default_action_load_no_prompt = true`.
- **Tag** — text input listing current tags comma-separated; `Enter` saves to sidecar (§6.6).
- **New** — see §5.6.

### 5.6 New-workspace flow

`n` invokes a sub-picker fed by `new_workspace_command`. Default detection (uses `exec.LookPath`):

1. If `zoxide` is on `PATH`: `zoxide query -l`.
2. Else if `fd` is on `PATH`: `fd -t d --max-depth 4 . ~`.
3. Else: surface `NO_PATH_PROVIDER` error with config hint.

**Exec model**: the resolved command is executed via `exec.CommandContext(ctx, shell, "-c", cmd)` using §6.11's shell-resolution logic (`$SHELL` → `/bin/sh` fallback). Stdin closed; `WEZSESH_*` env vars scrubbed (same as hooks); `SysProcAttr.Setpgid = true` so kill-on-timeout reaches children.

**Timeout**: hard 15 seconds via `context.WithTimeout`. On expiry: `syscall.Kill(-pgid, SIGKILL)`; surface `PATH_PICKER_TIMEOUT`. Not configurable in v0.1 — no legitimate path-listing exceeds this. Distinct from the 10-min hook timeout (§6.11) because the user is blocked staring at an empty picker.

**Output caps**: stdout limited to 1 MiB total (`io.LimitedReader`). Per-line scanner buffer 512 KiB. Lines beyond 10 000 silently dropped with a non-fatal status-bar note. Per-line validation: strip trailing `\r`; skip blank lines; reject non-UTF-8 (`utf8.ValidString`); reject lines containing `\x00`. Empty output (zero lines) is NOT an error — show empty-state nudge `No directories found. Is zoxide populated? (zoxide add <dir> to seed it.)`.

**Path resolution per line**: tilde-expand (`~/` → `os.UserHomeDir()`), then `filepath.Clean`, then validate: must be `filepath.IsAbs`, must `os.Stat` to an existing directory (`stat.IsDir()`). Symlinked directories ARE accepted (don't reject). Invalid lines surface as picker-level errors but don't abort the list.

**Distinct error codes**:
- `NO_PATH_PROVIDER` — neither `zoxide` nor `fd` on PATH and no user override. Toast points to install instructions.
- `PATH_PICKER_TIMEOUT` — 15s ceiling exceeded.
- `PATH_PICKER_CMD_FAILED` — tool found but exited non-zero. Surface stderr (first 256 bytes) so the user knows whether it's a corrupted DB, permissions, etc.

Selecting a path:
- Names the new workspace after the path (`~`-collapsed, e.g. `~/code/foo`).
- Spawns it via `mux.spawn_window { workspace = name, cwd = absolute_path }`.
- Reads the **project sidecar** `<picked_path>/.wezsesh.json` (NOT the snapshot sidecar). Runs `on_create` per §6.11 trust check; trust hash binds the project sidecar's absolute path + command bytes.

**Sidecar terminology** (clarified v1.8 to remove an existing PRD ambiguity): wezsesh has TWO distinct sidecar locations:
- **Project sidecar** at `<picked_path>/.wezsesh.json` — lives in the user's project directory. Carries `on_create`. Travels with the project (committed to git, etc.). Read by the `n` flow at workspace creation time.
- **Snapshot sidecar** at `<snapshot_dir>/workspace/<encoded-name>.wezsesh.json` — lives next to the resurrect snapshot file. Carries `on_restore`, `tags`, `pinned`. Travels with the snapshot (e.g., when synced via dotfiles). Read at restore time.

Both use the same JSON schema (§6.6); only the location and trigger differ. Trust files (§6.11) are computed independently per location.

**Project sidecar trust enforcement** (clarified v2.1). The `n` flow's `on_create` hook is treated **identically** to the snapshot sidecar's `on_restore` for trust purposes:

1. After `mux.spawn_window` returns a workspace, the binary reads `<picked_path>/.wezsesh.json`.
2. `command_bytes = sidecar.on_create` (in-memory; not re-read).
3. Trust hash computed per §6.11: `sha256(uint32_be(len(absolute_sidecar_path)) || absolute_sidecar_path bytes || uint32_be(len(command_bytes)) || command_bytes)`. The absolute path is `<picked_path>/.wezsesh.json` — fully qualified.
4. Trust file lookup at `$XDG_DATA_HOME/wezsesh/allow/<hash>`.
5. Hash match → run `on_create` per §6.11 hook execution environment. Hash miss → fail-closed (silent skip + log_warn) by default; interactive prompt if `wezsesh.hooks.prompt_on_untrusted = true`.

**Threat model for project sidecars**: a user clones an untrusted git repo (or extracts a tarball) that committed `.wezsesh.json` with malicious `on_create`. Picking that directory through the `n` flow — without explicit trust approval — would execute attacker-controlled shell commands. The fail-closed default closes this path. **The trust check IS mandatory and runs identically to snapshot-sidecar restore**; a future code path that bypasses the trust check (for example, a hypothetical `--no-trust-check` flag) would be a CVE.

**`wezsesh trust <name>` resolution for project sidecars** (clarified v2.1):
- The `<name>` argument is the **workspace name** (typically the `~`-collapsed picked path, e.g., `~/code/my-project`).
- The binary resolves the project sidecar path as `<workspace_cwd>/.wezsesh.json` where `workspace_cwd` comes from `wezterm cli list --format json` (the workspace's first pane's cwd).
- If the workspace doesn't exist (sidecar trust requested *before* picking the directory), `wezsesh trust --path <picked_path>` accepts a path argument as fallback.
- If the project sidecar lives at a non-root path within the project, `wezsesh trust --sidecar <absolute_path>` accepts the sidecar path directly.

**First-time-use UX for project sidecars** (added v2.1): the silent fail-closed default (§6.11) is the right safety posture but produces a footgun for the `n` flow — the user picks a directory expecting their dev server to start, sees no error, and only later discovers the hook was skipped. Two improvements:

1. **A toast on every silent skip** (not just a log line): `wezsesh: on_create not trusted for "~/code/foo". Run 'wezsesh trust ~/code/foo' to approve.` — visible in wezterm's toast UI for 6 seconds.
2. **Project sidecars suggest enabling interactive prompt**: README + `wezsesh doctor` recommend `hooks.prompt_on_untrusted = true` for users who frequently use the `n` flow with new directories. The default stays `false` to match `direnv`'s behavior; the recommendation is opt-in.

**Authorship guidance for project sidecar publishers** (added v2.1): `on_create` runs as the user with full filesystem access and access to the user's environment (minus `WEZSESH_*` per §6.11). Project authors committing `.wezsesh.json` to a repo should:
- Treat `on_create` as if it were `npm postinstall` — published code that users may run without inspection.
- Stick to commands that would be safe to execute in a CI runner or fresh sandbox: `npm install`, `make`, `bin/setup`, etc.
- Avoid commands with side effects outside the project directory (modifying `~/.bashrc`, deleting sibling directories, exfiltrating env vars).
- Document `on_create` in the project's README so users know what they're approving.

This guidance is published in the wezsesh README and surfaced by `wezsesh trust --show <name>` before approval. It does NOT enforce anything — the user's approval authorizes arbitrary code execution.

**Cross-machine trust** (clarified v2.1): trust hashes are absolute-path-bound. A project cloned to `~/code/foo` on one machine has hash `H1`; cloned to `~/work/foo` on another has hash `H2`. Both must be approved independently. This is correct behavior (an attacker who can write to `/srv/work/foo` should not gain trust from an unrelated `~/code/foo` approval) but undocumented. README and `wezsesh trust --help` now state: `Trust is path-bound; cloning the same project at a different absolute path requires re-approving on each machine.`

### 5.7 Bulk operations

`Tab`/`Space` toggles a mark on the current row. Marked rows render with `✓` in the marker column. `c` clears all marks.

When `d` is pressed:
- If any rows are marked → bulk-delete confirm modal listing marked names.
- Else → single-row delete confirm modal.

Other bulk ops are out of scope for v0.1; tag/pin remain single-row.

**Bulk-delete OSC batching** (added v2.0): the IPC layer caps `delete` at 5 names per OSC payload to keep each request safely under the 4 KB canonical-JSON ceiling (§9 risks). Names can carry TAB / U+007F / C1 controls that escape to 6 bytes each per §6.3; a 200-byte name can expand to 1200 canonical bytes, so 5 worst-case names + envelope ≈ 6 KB pre-base64 — already at the ceiling. If the user marks N > 5, the Go binary serializes the deletion as ⌈N/5⌉ sequential OSCs sharing a single in-flight `bulk_id` (a request-time ULID separate from the per-OSC `id`); each OSC awaits its own reply on the shared accept-loop socket. The TUI shows one combined progress indicator (`Deleting N workspaces...`) and a single final summary toast (`Deleted X of N; Y errors`). Per-batch failures (e.g., `SNAPSHOT_MISSING` for one name) are aggregated into the summary; the bulk op is best-effort, not transactional.

### 5.8 Preview pane

Right column, ~40% of screen width (configurable via `opts.preview.width`). Shows for the highlighted entry:

- Workspace display name + canonical path (if path-named).
- Per tab: title + per-pane CWD + foreground process (from snapshot JSON or live `wezterm cli list`).
- Last used (from usage state file).
- Tags.
- For live workspaces: any divergence note ("modified since last save").

Toggleable via `opts.preview.enabled = true|false` (default `true`). Hide column with `P` key.

### 5.9 Empty / error states

| Case | Message |
|---|---|
| No saved or live workspaces; first-run | `No workspaces yet. Press 'n' to create one from a directory, or 'S' to save the current workspace.` |
| Wezterm CLI unreachable | `wezterm CLI not reachable. Are you running this from inside a wezterm pane?` |
| Snapshot dir missing | `Resurrect snapshot dir not found at <path>. Configure resurrect first or override via opts.snapshot_dir.` |
| Binary version doesn't match plugin | `wezsesh binary v<X> doesn't match plugin v<Y>. Run wezsesh doctor for details.` |
| Resurrect schema unknown | `Snapshot '<name>' has unfamiliar schema; running in degraded mode (no preview metadata).` (see §10) |

### 5.10 Help overlay

`?` toggles a centered overlay containing:

- All keybindings, grouped by category (Navigation / Operations / Modes / Bulk), reflecting the user's configured overrides.
- Binary version + path.
- Plugin version.
- Snapshot dir path.
- State dir path.
- Link to README.

## 6. Architecture

### 6.1 Components

```
┌─ wezterm Lua plugin (wezsesh) ────┐               ┌─ wezsesh binary (Go) ─────────┐
│ apply_to_config(config, opts)     │               │  Inputs:                      │
│   • pcall-wraps full body         │   ◀──────     │   • $WEZSESH_SNAPSHOT_DIR     │
│   • generates HMAC key (cached)   │   spawn       │   • $WEZSESH_STATE_DIR        │
│   • registers keybinding          │               │   • $WEZSESH_RUNTIME_DIR      │
│   • registers user-var listener   │   ──────▶     │   • $WEZTERM_PANE             │
│   • registers pane-closed listener│               │   • $WEZSESH_HMAC_KEY         │
│   • installs on_pane_restore      │               │   • `wezterm cli list --json` │
│     (argv allowlist, §6.18)       │               │                               │
│                                   │   ◀─────      │  Renders bubbletea TUI        │
│ on user-var-changed(wezsesh_op):  │ OSC 1337      │  On action:                   │
│   pane-ID + HMAC + replay verify  │ user-var      │   open Unix socket listener   │
│   dispatch op                     │   ──────▶     │   write OSC 1337 SetUserVar   │
│   spawn `wezsesh reply` to write  │               │     via /dev/tty + sync.Mutex │
│     reply over Unix socket        │   ◀──────     │   read reply from socket      │
│                                   │  Unix sock    │     (split-reply for restore: │
└───────────────────────────────────┘  reply        │      "started" → "completed") │
                                                    └───────────────────────────────┘
```

### 6.2 Data flow

1. User presses `Leader+Shift+W`.
2. Plugin's keybinding spawns the binary in a new tab (or overlay), passing snapshot dir, state dir, and parent pane id as env vars.
3. Binary reads snapshot files (`<snapshot_dir>/workspace/*.json`) + sidecars (`*.wezsesh.json`) + state file (usage + pins) + queries `wezterm cli list --format json`. Records each snapshot's content hash on load.
4. Binary renders the TUI. User picks an action.
5. Before emitting any OSC, the binary creates a Unix-domain socket at `<reply_dir>/<8-hex>.sock` (`<reply_dir>` = `$XDG_RUNTIME_DIR/wezsesh/` on Linux, `/tmp/wezsesh-<uid>/` on darwin; `<8-hex>` = first 8 hex chars of the request ULID; full path stays under `SUN_PATH` 104 bytes) and starts a goroutine listening for the response. The socket path is included in the request payload as `reply_sock`. See §6.4.
6. Binary writes an OSC 1337 `SetUserVar` sequence: the value is base64'd JSON with `v`, `id` (ULID), `ts`, `op`, `args`, `reply_sock`, `hmac`, and `target_window_id`. The bytes are written to a separately-opened `/dev/tty` fd (NOT bubbletea's stdout) under a `sync.Mutex` — bubbletea's renderer holds its own mutex on `os.Stdout` that user code can't share, so OSC and frame writes must use different fds (see §6.4).
7. Wezterm fires `user-var-changed` in **every** window of the parent mux process — wezterm behavior per [#3524](https://github.com/wezterm/wezterm/issues/3524). Each plugin listener:
   - rejects events whose `pane:pane_id()` doesn't match the spawned binary's recorded pane (security layer 1);
   - HMAC-verifies the payload against `WEZSESH_HMAC_KEY` (security layer 2);
   - rejects payloads outside a 30s `ts` window or whose `id` was already seen (replay guard);
   - rejects payloads whose `target_window_id` doesn't match the current `window:window_id()`.
   Surviving events advance to dispatch.
8. Plugin performs the action (using `wezterm.mux.*`, `act.SwitchToWorkspace` via `window:perform_action`, or direct CLI shortcuts like `wezterm cli rename-workspace`).
9. Plugin writes a JSON response back via `wezterm.background_child_process({wezsesh_bin, "reply", reply_sock, b64_json})`. See §6.4 — the binary's `reply` subcommand connects to the socket and writes the payload. The response carries a `status` field:
   - `"completed"` — terminal. The op finished synchronously. TUI dismisses or refreshes.
   - `"started"` — non-terminal. Used only by long-running verbs (`load`, `switch+spawn` triggering a restore). The op was accepted; the TUI dismisses immediately so the user can watch resurrect spawn tabs in the wezterm window itself. A `"completed"` or `"partial"` follow-up is expected.
   - `"partial"` — terminal. The op completed with errors (e.g., `RESURRECT_PARTIAL`). Surfaces as a toast.

   Restore-class verbs emit two replies: `"started"` immediately (before calling `resurrect.workspace_state.restore_workspace`), then `"completed"`/`"partial"` when restore returns. Non-restore verbs emit one `"completed"` reply.
10. Retry/ceiling, per-status:
    - **First reply** (any status): single retransmit at 2s if absent; **5s hard ceiling** for the first reply, after which `IPC_TIMEOUT` surfaces. The replay guard (§6.14) suppresses duplicate dispatches.
    - **Follow-up reply** (only after a `"started"`): **30s additional ceiling** for `"completed"`/`"partial"`. If absent, the TUI shows a status-bar toast `restore confirmation timed out — workspace may still be loading` (degrades gracefully — the actual restore is not affected by our timeout).
    - These timings are calibrated against measured restore latency: a 24-pane workspace with a heavy zsh init can take 8–15s; rare worst cases (large monorepo + many panes) can reach 30s. The split-reply pattern makes the user experience independent of restore wall-clock.
    - **Retransmit MUST be implemented as `tea.Tick(2 * time.Second, func(t time.Time) tea.Msg { return retransmitMsg{} })` returning a `tea.Cmd`, NOT as a raw goroutine or `time.AfterFunc`.** `tea.Tick` is bubbletea's one-shot timer Cmd (fires once per Cmd invocation, not periodically). The often-cited `tea.After` does NOT exist in any released bubbletea version — verified against v1.3.5 and v2.0.6 source (`commands.go` in both versions exports `Tick`, `Every`, `Batch`, `Sequence`, but not `After`). An earlier PRD revision had this name wrong. When the first reply arrives before 2s, `Update` ignores any subsequent `retransmitMsg` via the idempotent guard `if model.replyReceived { return model, nil }`. Bubbletea's `Tick` Cmd executes as a goroutine bounded by `program.ctx`; on `tea.Run` return, the context cancels and the goroutine exits cleanly. Raw `time.AfterFunc` would fire regardless and leak the goroutine until firing — fails `goleak.VerifyNone(t)` in tests.

### 6.3 IPC: payload protocol

#### Request

The binary base64-encodes the canonical JSON of the request and emits it as an OSC 1337 `SetUserVar` sequence (see §6.4 for the bytes). All fields are required.

```jsonc
{
  "v": 1,                              // protocol version
  "id": "01JABCDEFGHIJKLMNPQRSTUVWXY", // ULID; replay-guard key
  "ts": 1745875200,                    // unix seconds; freshness check (±30s)
  "target_window_id": 42,              // §6.5; rejects mismatched windows
  "reply_sock": "/run/user/1000/wezsesh/01JAB....sock",  // unix socket path
  "op": "switch",                      // verb
  "args": { "name": "~/.dotfiles" },   // verb-specific
  "hmac": "9f86d081884c7d659a2feaa..." // hex HMAC-SHA-256 of canonical_json(payload-minus-hmac)
}
```

**Canonical JSON for HMAC** — the encoder on the Go side (`internal/canonicaljson/`) and the encoder on the Lua side (`plugin/wezsesh/canonical_json.lua`) MUST produce byte-identical output for identical inputs. Any divergence is at minimum an HMAC-failure DOS for affected payloads, and at worst a confused-deputy bug (one side validates a payload, the other dispatches a different verb). The rules below are exhaustive — implementations MUST follow them verbatim and MUST NOT add escapes, whitespace, or transformations not listed here.

**Field-removal ordering for HMAC** (load-bearing; both sides MUST follow the same sequence):

1. Construct the in-memory payload structure with all fields **except** `hmac`.
2. Serialize that structure to canonical JSON per the rules below.
3. Compute `HMAC-SHA-256(canonical_json_bytes)` using the shared key.
4. Hex-encode the digest (lowercase).
5. Set the `hmac` field on the payload structure to the hex digest.
6. Re-serialize the now-complete payload (including `hmac`) for wire emission. The wire form is base64'd; the HMAC was computed over the pre-`hmac`-insert canonical bytes.
7. **Verifier** (Lua side): parse the received payload; apply verb-aware tagging per `docs/design.md` §4.2 (so empty `{}` vs `[]` containers re-acquire canonical shape); remove the `hmac` field from the tagged structure; canonical-serialize what remains; compute HMAC; compare against the received `hmac` value via the constant-time helper `ct_eq` (§6.14 Layer 2). Tag-before-remove is load-bearing: `ROOT_PAYLOAD_SHAPE` (§4.2) declares `hmac` as a required key, so tagging a payload from which `hmac` had already been dropped would raise `CANONICAL_SHAPE_MISMATCH`. See `docs/design.md` §4.3 for the normative sequence.

The forbidden alternative is "set `hmac=""` then serialize then compute": that produces a different byte sequence (`,"hmac":"",` vs. no `hmac` key at all). Both sides MUST remove the key entirely before the HMAC-input serialization.

**Encoding rules** (Go and Lua MUST agree):

- **Object keys**: sorted by **unsigned UTF-8 byte order** (locale-independent), recursively at every nesting level. Go: `sort.Strings`. Lua: `table.sort(keys, function(a, b) return a < b end)` — Lua's string `<` is byte-by-byte unsigned comparison in stock Lua 5.4 (no locale dependence in mlua-rs's bundled Lua). CI MUST run with `LC_ALL=C` to remove environmental drift.
- **Whitespace**: none. No spaces between keys/values, no trailing newline.
- **Numbers**: integers only, range `[-2^63, 2^63-1]` (int64), decimal ASCII digits with optional leading `-`, no leading zeros except for `0` itself. **Reject** floats, NaN, ±Inf, scientific notation, and any Lua value of float subtype. Lua 5.4 distinguishes integer/float; the Lua encoder MUST type-check via `math.type(n) == "integer"` and reject otherwise — `tostring(1.0)` returns `"1.0"` in Lua 5.4 which would corrupt the canonical form. Go: `strconv.FormatInt(n, 10)`.
- **Strings**: UTF-8. Both sides MUST validate UTF-8 and reject invalid byte sequences (Go's `encoding/json` does this; Lua side MUST run a UTF-8 validator before serializing). Escapes:
  - `\\` for U+005C (reverse solidus). Always.
  - `\"` for U+0022 (quotation mark). Always.
  - `\u00XX` (lowercase hex) for ALL of: U+0000–U+001F (C0 controls), U+007F (DEL), U+0080–U+009F (C1 controls when present as valid UTF-8).
  - ` ` and ` ` for U+2028 (LINE SEPARATOR) and U+2029 (PARAGRAPH SEPARATOR) — JavaScript-safe; eliminates a corner case where vendored Lua VMs differ on whether these are "control" or "printable."
  - **Short-form escapes (`\b`, `\f`, `\n`, `\r`, `\t`) are FORBIDDEN.** All control chars use `\u00XX`. Avoids the divergence where Go's encoder might emit `\n` while a hand-rolled Lua encoder emits the literal byte (or vice versa).
  - **Forward slash (`/`, U+002F) is NEVER escaped.** Both sides emit it raw.
  - All other code points ≥ U+0020 (excluding the special cases above) are emitted as their raw UTF-8 bytes.
- **Booleans**: `true`, `false`. Lowercase, no whitespace.
- **Null**: emitted as `null`. Lua side uses a sentinel (`canonical_json.NULL`) to disambiguate from `nil` (which means "absent key" in Lua); the encoder emits `null` only when the value is the sentinel, never for `nil`.
- **Arrays vs objects** (BLOCKER — Lua `{}` is structurally ambiguous):
  - Go: `[]any` → array `[...]`; `map[string]any` → object `{...}`. No ambiguity.
  - **Lua: PIN to wrapper-function metatables** (the alternative sentinel-field approach is rejected because user code that legitimately uses an `__array` field for its own purposes would silently corrupt the encoding). The Lua encoder exports two constructors:
    ```lua
    -- canonical_json.lua
    local M = {}
    M.array_mt  = { __wezsesh_canonical = "array" }
    M.object_mt = { __wezsesh_canonical = "object" }
    function M.array(t)  return setmetatable(t or {}, M.array_mt)  end
    function M.object(t) return setmetatable(t or {}, M.object_mt) end
    M.NULL = setmetatable({}, { __wezsesh_canonical = "null" })  -- json null sentinel
    ```
    The encoder dispatches on metatable: `getmetatable(t).__wezsesh_canonical == "array"` → emit `[...]`; `"object"` → emit `{...}`; `"null"` → emit `null`. **Untagged tables are an encoder error** (logged + return nil); never silently guess. Tables parsed from `wezterm.json_parse` are re-tagged by a helper `M.from_parsed(t)` that walks the structure, tagging each table by inspecting its key shape (all integer keys 1..N → array; all string keys → object; mixed → error).
  - **Empty objects MUST emit `{}`. Empty arrays MUST emit `[]`.** Without the explicit tag, an empty Lua table `{}` is indistinguishable; this is the source of every empty-`args`-payload divergence.
  - For the IPC payload protocol specifically: `args` is ALWAYS a tagged object (even for `noop` where it's `canonical_json.object{}`); `args.names` for `delete` is a tagged array (`canonical_json.array{...}`); `args.tags` for `tag` is a tagged array.
- **Recursion**: all rules apply recursively (sort keys at every level, escape strings at every level, etc.).

**CI gate (golden tests, expanded from §8.1)**: the Go and Lua canonical-JSON encoders run side-by-side on a fixed corpus. Vectors MUST include: empty object `{}`, empty array `[]`, empty string `""`, NUL inside string, U+007F, U+2028, multi-byte UTF-8 (`café`, `日本語`), nested objects 3-deep, arrays of mixed types, integer edge cases (`0`, `-1`, `-9223372036854775808`, `9223372036854775807`), boolean values, explicit `null`, the full request-payload corpus (one fixture per verb in §6.3 with realistic and edge-case args). Any byte-level divergence fails the build.

**Verb-specific `args`**:

```jsonc
// switch — go to a workspace; if saved-not-live, also restore
"op": "switch",  "args": { "name": "~/.dotfiles" }

// load — restore snapshot into current workspace
"op": "load",    "args": { "name": "~/.dotfiles" }

// rename — rename workspace and/or its snapshot file
"op": "rename",  "args": { "name": "old", "to": "new", "overwrite": false }

// delete — remove one or more snapshots (does not affect live workspaces)
// Per-OSC cap: 5 names. If the user marks more than 5, the binary issues
// multiple OSCs sequentially and aggregates replies before showing a final
// status to the user. This keeps each OSC payload comfortably under the 4 KB
// canonical-JSON ceiling even with worst-case escape expansion (200-byte
// names with TAB / U+007F / C1 controls all escaping to 6 bytes each).
"op": "delete",  "args": { "names": ["~/.dotfiles", "default"] }

// save — snapshot current workspace
"op": "save",    "args": { "name": "~/.dotfiles", "overwrite": false,
                            "expected_hash": "sha256:..." }

// new — create new workspace from path
"op": "new",     "args": { "name": "~/code/foo", "cwd": "/Users/me/code/foo" }

// tag — write tags to sidecar
"op": "tag",     "args": { "name": "~/code/foo", "tags": ["api", "backend"] }

// pin — toggle pin in state file
"op": "pin",     "args": { "name": "~/code/foo", "pinned": true }

// noop — TUI cancelled
"op": "noop",    "args": {}
```

Unknown verbs are silent-dropped at `ipc.lua` step (e) with `wezterm.log_warn("ipc: no shape registered for op=…")` and **no wire reply** — the §4.2 verb-keyed shape lookup runs before HMAC verify, so an unknown `op` short-circuits before `ops.dispatch` is reached. The binary observes `IPC_TIMEOUT` after the 5 s first-reply ceiling and surfaces a generic timeout toast; operators diagnose unknown-verb conditions via the wezterm log. See `docs/design.md` §13.13 (and §0.1 row 35) for the full rationale.

#### Response

Returned via Unix socket, JSON, no envelope.

```jsonc
// success — terminal
{ "id": "01JAB...", "status": "completed", "ok": true, "data": { /* verb-specific */ } }

// success — non-terminal (long-running ops); follow-up reply expected
{ "id": "01JAB...", "status": "started", "ok": true }

// success — completed with non-fatal errors (e.g., partial restore)
{ "id": "01JAB...", "status": "partial", "ok": true, "data": { /* verb-specific */ },
  "warnings": [{ "code": "RESURRECT_PARTIAL", "message": "..." }] }

// failure — terminal
{ "id": "01JAB...", "status": "completed", "ok": false, "error": {
    "code": "SNAPSHOT_CHANGED" | "SNAPSHOT_MISSING" | "SNAPSHOT_LOCKED"
          | "RENAME_COLLISION" | "ILLEGAL_NAME" | "MUX_UNREACHABLE"
          | "RESURRECT_PARTIAL" | "SNAPSHOT_LOAD_FAILED" | "HMAC_MISMATCH"
          | "FOREIGN_PANE" | "STALE_PAYLOAD" | "REPLAY" | "IPC_TIMEOUT"
          | "UNKNOWN_VERB" | "UNEXPECTED_EXIT" | "PANE_CLOSED_RACE"
          | "XDG_PATH_TIMEOUT" | "IPC_INIT_FAILED"
          | "ENCRYPTION_AGENT_SLOW" | "UNKNOWN",
    "message": "human-readable",
    "details": { /* optional */ }
} }
```

Note: `UNKNOWN_VERB` is wire-silent in design v3 (see `docs/design.md` §0.1 row 35 / §13.13); it is listed in the union above for catalog completeness and to keep operator-facing log lines / fuzz mutation classes self-naming, but it is never emitted on the wire — an unknown `op` short-circuits at `ipc.lua` step (e) with no reply, and the binary observes `IPC_TIMEOUT`.

The `status` field disambiguates terminal from non-terminal replies. The TUI's reply listener accepts connections in a loop on the same socket until a terminal status (`"completed"` or `"partial"`) arrives or the per-status ceiling is hit (§6.2 step 10).

For `save` with a stale `expected_hash`, the binary surfaces the snapshot-changed modal; user confirms; binary re-emits with `overwrite: true` and refreshed `expected_hash`.

`SNAPSHOT_MISSING` covers the rename/load/delete-of-vanished-snapshot race: TUI A opens a rename modal for `foo` while TUI B deletes `foo`. A's `rename` lands on a missing file; the binary surfaces `Snapshot 'foo' was deleted by another session` and re-lists.

**Lua handler also surfaces errors via `wezterm.log_error` + status-line toast** in addition to writing to the reply socket — covers cases where the socket write itself fails.

**Note on base64**: the binary base64-encodes the canonical JSON for the OSC wire format. Wezterm's OSC parser base64-decodes before dispatching `user-var-changed` (verified at `wezterm-escape-parser/src/osc.rs:1267-1278`). Lua handlers receive plain UTF-8 — **do NOT base64-decode in Lua**. The Lua side does need to *encode* base64 for the reply via `wezsesh reply` — see §6.4.

### 6.4 IPC mechanism: OSC 1337 out, Unix socket back

#### Forward path: binary → Lua via OSC 1337

There is no `wezterm cli set-user-var` subcommand (verified against the CLI subcommand list and [open feature request #7307](https://github.com/wezterm/wezterm/issues/7307)). The supported way to set a user-var from a subprocess is to emit an OSC 1337 `SetUserVar` sequence to its own stdout:

```
ESC ] 1337 ; SetUserVar=wezsesh_op= <base64-of-canonical-JSON> BEL
```

Equivalent shell form: `printf '\033]1337;SetUserVar=wezsesh_op=%s\007' $(printf '%s' '<json>' | base64 -w0)`.

**Stdout buffering**: Go's `os.Stdout` is unbuffered (no `bufio` wrapper). Under `wezterm cli spawn`, stdout is a PTY. A single `os.Stdout.Write` produces one `write(2)` syscall and is dispatched to wezterm's terminal parser inline. **No `Sync()` call is required.**

**Bubbletea coexistence**: bubbletea v1.3.5 runs every `tea.Cmd` returned from `Update` as its own goroutine (`tea.go:355-367` in handleCommands), with no serialization between them. Cmd bodies that write to `os.Stdout` race directly with the renderer's ticker goroutine, which flushes frame data to `os.Stdout` under its own internal `r.mtx` (`standard_renderer.go:161-291`) that user code cannot acquire. Byte-level interleaving is real, not theoretical.

The fix: open `/dev/tty` separately (not `os.Stdout`) and protect all OSC writes with our own `sync.Mutex`. Bubbletea keeps its own fd for frames; we keep ours for OSCs; the kernel serializes the two write streams independently.

**PIPE_BUF atomicity caveat (clarified v2.2)**: the kernel's "serialization" guarantee is per-`write(2)`-syscall up to `PIPE_BUF` bytes (4096 on Linux, 512 on darwin per POSIX minimum — though darwin's actual TTY-driver buffer is larger; treat 4 KB as the conservative ceiling on both). Writes LARGER than `PIPE_BUF` are NOT atomic — Go's `Write` may issue multiple `write(2)` syscalls under the hood, and a concurrent `write(2)` from the OSC fd can interleave at the boundary between two of bubbletea's calls. Worst case: bubbletea is mid-emission of an 8 KB full-redraw frame on terminal resize, and our OSC write slips in between bubbletea's two syscalls. The wezterm parser sees `[first half of frame] + [full OSC] + [rest of frame]` — both are well-formed escape sequences, the parser handles them in order, but the visual frame may flash glitchy mid-render. Risk is **cosmetic, not security or correctness**: the OSC payload is `\x1b]1337;...\x07` which doesn't contain control sequences that affect frame state. The 4 KB OSC ceiling (§6.3) ensures our writes stay atomic per-syscall; we rely on bubbletea's frame writes being either small (atomic) or splittable (cosmetic glitch acceptable).

```go
// internal/uservar/writer.go (sketch)
type Writer struct {
    mu  sync.Mutex
    tty *os.File              // os.OpenFile("/dev/tty", os.O_WRONLY, 0)
}
func (w *Writer) WriteOSC(payload []byte) error {
    w.mu.Lock(); defer w.mu.Unlock()
    _, err := fmt.Fprintf(w.tty, "\x1b]1337;SetUserVar=wezsesh_op=%s\x07", payload)
    return err
}
```

Routing rules:
- **Pre-`tea.Run()` / post-return**: safe to call `Writer.WriteOSC` directly.
- **During render**: call `Writer.WriteOSC` from inside a `tea.Cmd` body. The Cmd runs as a goroutine, but the mutex + separate fd makes the write order safe regardless of when the renderer flushes.
- **`tea.Sequence(emitOSC, awaitReply)`** orders Cmd execution but does NOT order their effects against the renderer — that's the mutex's job.

`program.Send(msg)` from the reply-socket goroutine is safe and well-supported (`tea.go:737-742`); use it to inject reply messages into the Update loop.

**Stdin is not a return channel**: bubbletea's stdin parser does not handle OSCs cleanly (PR #467 fixed CSI only). Never use `pane:send_text` to push responses; the bytes will arrive in bubbletea's input parser and surface as garbled `KeyMsg` events.

#### Reverse path: Lua → binary via Unix socket + `wezsesh reply` subcommand

`wezterm cli list --format json` does **not** expose user-vars (verified by reading `wezterm/src/cli/list.rs::CliListResultItem` and live `cli list` output). Polling the CLI for a response is dead.

`pane:inject_output(text)` writes to the **terminal display side** (calls `pane.perform_actions`) — programs running in the pane never read these bytes. Useless as a return channel.

`nc -U` is **not portable**: busybox `nc` (default on Alpine, common in containers) lacks `-U`; netcat-traditional lacks it; Lua's `string.format("%q", ...)` is **not** shell-safe (real injection vector — `$variables` and `` `backticks` `` still expand inside double-quoted shell context). Using `sh -c` from Lua is too dangerous.

**The chosen mechanism**: Unix-domain socket per request, with the **reply written by our own `wezsesh reply` subcommand** (no shell, no nc).

1. Before emitting the OSC, the binary creates `<reply_dir>/<8-hex>.sock`:
   - On Linux: `<reply_dir>` = `$XDG_RUNTIME_DIR/wezsesh/` (typically `/run/user/<uid>/wezsesh/`). Linux `SUN_PATH` ceiling is 108 bytes (verified `linux/un.h`).
   - On darwin: `<reply_dir>` = `/tmp/wezsesh-<uid>/` (NOT `$TMPDIR` — too deep for `SUN_PATH` 104-byte limit; verified `sys/un.h` on macOS 14/15 SDK).
   - Filename = first 8 hex chars of the request_id ULID + `.sock` (saves SUN_PATH bytes; collision-safe for our concurrency). Worst-case path component length is 14 bytes (`/<8hex>.sock`).
   - **SUN_PATH validation at config-load (added v2.2; contract clarified + hardened v2.3)**: the Lua-side `apply_to_config` MUST validate `len(expanded_runtime_dir) + 14 ≤ ceiling` (104 on darwin, 108 on Linux) and raise a config error with the exact remediation message. Without this check, a user who sets `opts.runtime_dir` to a long path silently passes config load, then every TUI launch fails at `net.Listen("unix", path)` with `ENAMETOOLONG`. The TUI surfaces a generic `IPC_TIMEOUT` and the root cause is invisible.

     **Contract clarified v2.3**: `opts.runtime_dir` IS the FINAL reply directory (it INCLUDES the trailing `/wezsesh/` component if isolation from other tools is desired). The Go binary does NOT append `/wezsesh/` on top of a user-supplied `opts.runtime_dir`. Default auto-detection (when `opts.runtime_dir == nil`) constructs the path as `$XDG_RUNTIME_DIR/wezsesh/` on Linux or `/tmp/wezsesh-<uid>/` on darwin. The earlier v2.2 ambiguity caused the `+14` budget to silently undercount by 9 bytes if a user-supplied `runtime_dir` lacked `/wezsesh/` and the binary appended it. README + doctor explicitly document this contract.

     **Hardened v2.3 + v2.4 polish**: tilde expansion BEFORE length check; explicit string-type assertion; tightened `target_triple` regex anchor; SUN_PATH error surfaced via toast (not generic pcall fallback); `$XDG_RUNTIME_DIR` fallback for non-systemd Linux. v2.4 polish: `error(msg, 0)` (level=0) suppresses the noisy `file:line:` prefix Lua's default `error()` prepends, so the toast text is just the sentinel + remediation; sentinel substring match in the outer pcall still works (`string.find` is unanchored — verified Lua 5.4 reference manual §6.4.1). Validation example (Lua):
     ```lua
     -- Type check first: surface a clear message via the outer pcall toast path.
     -- error(msg, 0) suppresses Lua's default "file:line:" prefix on the caught
     -- message, keeping the toast text legible. Substring match in the outer
     -- pcall still works (string.find is unanchored).
     if type(opts.runtime_dir) ~= "string" then
         error("WEZSESH_RUNTIME_DIR_TYPE: opts.runtime_dir must be a string path", 0)
     end

     -- Tilde expansion BEFORE length measurement: Go binary will expand `~`
     -- against the user's $HOME at runtime; the Lua check must mirror that
     -- expansion to avoid false-pass on a `~`-prefixed path whose expansion
     -- overflows SUN_PATH. wezterm.home_dir is the documented Lua API
     -- (verified config/src/lib.rs `HOME_DIR = dirs_next::home_dir().expect(...)`,
     -- exposed via config/src/lua.rs as the `home_dir` field on the wezterm
     -- module). It is initialized at wezterm startup and is always a string
     -- once wezterm itself is up — wezterm panics at boot if the lookup fails.
     -- The `or os.getenv("HOME") or ""` chain is forward-compat hygiene
     -- against a future wezterm release that surfaces nil; harmless redundancy
     -- today.
     local expanded = opts.runtime_dir
     if expanded:sub(1, 2) == "~/" then
         expanded = (wezterm.home_dir or os.getenv("HOME") or "") .. expanded:sub(2)
     end

     -- Anchor the platform check on the FULL "-apple-darwin" tail to avoid
     -- substring false-positives (a hypothetical "aarch64-apple-linux" triple
     -- — defense-in-depth against future wezterm changes; cheap to be exact).
     -- `%-` is Lua 5.4's literal-hyphen escape (verified Lua reference manual
     -- §6.4.1); plain `-` outside a character class is also literal in Lua
     -- patterns, but `%-` is the idiomatic explicit form. Verified to match
     -- both `aarch64-apple-darwin` (Apple Silicon) and `x86_64-apple-darwin`
     -- (Intel); wezterm's `target_triple` is the Rust stable target triple
     -- (env!("TARGET")) with no version suffix on shipped releases.
     local ceiling = (wezterm.target_triple:match("%-apple%-darwin") and 104) or 108
     local needed = #expanded + 14   -- "/<8hex>.sock"
     if needed > ceiling then
         -- Sentinel-prefixed error so apply_to_config's outer pcall (§7.1)
         -- detects this specific class and surfaces the FULL message via a
         -- 10-second toast (NOT the generic "wezsesh setup failed" toast).
         -- Level=0 (the trailing 0) suppresses the file:line prefix; string.find
         -- in the outer catch still matches the sentinel.
         error(string.format(
             "WEZSESH_SUN_PATH_OVERFLOW: runtime_dir too long for AF_UNIX SUN_PATH " ..
             "(needed=%d, ceiling=%d, path=%q). Shorten the path or use the default.",
             needed, ceiling, expanded), 0)
     end
     ```

     The Go binary independently re-validates at runtime as defense-in-depth (returns `IPC_INIT_FAILED` with the same diagnostic before even attempting `net.Listen`). The Go binary also pre-flights the parent dir via `safefs.VerifyDir(filepath.Dir(reply_dir))` to reject symlink ancestors (closes the path-traversal gap that bare `O_NOFOLLOW` doesn't cover for socket paths) AND fails fast on `$XDG_RUNTIME_DIR` ownership/permission mismatches (per XDG spec, must be user-owned mode 0700; doctor reports owner UID, mode, FS type).

     **`$XDG_RUNTIME_DIR` fallback (added v2.3)**: on Linux, when `$XDG_RUNTIME_DIR` is unset (Alpine, void, gentoo-without-systemd, raw SSH sessions without logind, container-based desktops without `pam_systemd`), the Go binary falls back to `/tmp/wezsesh-<uid>/` (matching the darwin pattern). Documented in README install guide; doctor reports the resolved reply dir.
   - **Accept loop**: accepts connections sequentially on the same socket until a terminal status (`"completed"` or `"partial"`) arrives or the per-status ceiling is hit. Each connection reads a single JSON payload (capped at 1 MiB), then closes. This shape supports the split-reply pattern (§6.2 step 9) for restore-class ops without needing a second socket.
   - Parent dir mode 0700; sock file `os.Chmod(path, 0o600)` after Listen.
   - **`net.Listen` MUST be called synchronously inside bubbletea's `Update`, NOT inside the returned `tea.Cmd`.** Otherwise the OSC may fire before the listener is up. The `internal/ipcsock/` helper exposes `StartListener(reply_sock) (chan Response, cleanup func(), error)` to enforce this.
   - **`cleanup()` contract**: closes the listener fd via `listener.Close()` (which causes the accept loop to return `net.ErrClosed` and exit cleanly) AND `os.Remove(sockPath)`. Wrapped in `sync.Once` so double-calls (signal handler + `defer`) are idempotent. Without `Close()`, the listener goroutine remains blocked on `Accept()` after `tea.Run()` returns, holding the fd indefinitely. **`defer cleanup()` MUST be placed in `main.go` immediately after `StartListener` returns**; the same `cleanup` reference is passed to `InstallSignalHandler` (§6.4 signal section). The listener goroutine itself has a top-level `defer recover()` logging via the structured logger — a panic in JSON unmarshal (e.g., truncated payload) must degrade to an error log, not a process crash.
   - 2-second per-connection read deadline. Overall accept deadline: **5s** for the first reply (covers transport hiccups); **30s additional** after a `"started"` reply (covers worst-case restore latency on heavy-shell setups). Surface `IPC_TIMEOUT` on overrun of the first; surface a non-fatal toast on overrun of the follow-up (the actual op continues regardless of our timeout).
2. The OSC payload includes `reply_sock: "<absolute-path>"`.
3. Lua handler dispatches the op, then emits the reply via `background_child_process`. **The call MUST be wrapped in `pcall`**: per `lua-api-crates/spawn-funcs/src/lib.rs:54-72`, the function returns `mlua::Result<()>` and raises a Lua error on spawn failure (binary missing, EMFILE, EACCES, OOM). An unwrapped error propagates out of the `user-var-changed` handler; wezterm logs the error but the dispatch path crashes mid-way, leaving the binary blocked on the socket waiting for a reply that never comes. Pattern:
   ```lua
   local ok, err = pcall(wezterm.background_child_process, {
       wezsesh_bin,    -- absolute path resolved at plugin load (§7.1); cached in
                       -- wezterm.GLOBAL.wezsesh_bin_path. Re-validated against
                       -- presence of the file before EVERY spawn (binary may have
                       -- been removed mid-session by `nix-collect-garbage`,
                       -- `brew uninstall`, or a build that overwrote the path).
       "reply",
       reply_sock,     -- from request payload
       b64,            -- base64 of the canonical-JSON response
   })
   if not ok then
       wezterm.log_error("wezsesh: failed to emit reply: " .. tostring(err))
       -- The TUI will hit IPC_TIMEOUT (5s); from its perspective this is
       -- indistinguishable from the binary panicking after dispatch. The
       -- Lua-side op DID complete (state was mutated), so the user sees
       -- "operation timed out" while the operation actually succeeded.
       -- Surface a toast so the user knows to refresh the picker.
       window:toast_notification("wezsesh",
           "reply emission failed; operation may have succeeded — refresh picker",
           nil, 4000)
   end
   ```
   Background-spawned children are NOT auto-reaped by wezterm's smol runtime; the `wezsesh reply` subcommand exits within ~1ms after writing to the socket, and zombies are reaped by the kernel when the wezterm process eventually exits. No accumulation in normal use.
4. The `wezsesh reply` subcommand connects to the socket and writes the decoded payload (~30 LOC):
   ```go
   func runReply(args []string) error {
       payload, _ := base64.StdEncoding.DecodeString(args[1])
       c, err := net.DialTimeout("unix", args[0], 1*time.Second)
       if err != nil { return nil }  // parent timed out — silent
       defer c.Close()
       c.SetWriteDeadline(time.Now().Add(1 * time.Second))
       _, err = c.Write(payload)
       return err
   }
   ```
5. Binary's main process reads, decodes, surfaces the result via `program.Send(replyMsg{...})` from a dedicated goroutine.

**Crash cleanup**: at startup, scan `<reply_dir>/*.sock`, remove any with mtime > 60s. Plus `defer os.Remove(sock)` on every normal exit path AND a signal handler that runs the same cleanup before exiting:

```go
// internal/ipcsock/cleanup.go
func InstallSignalHandler(cleanup func()) {
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
    go func() {
        <-sigCh
        cleanup()
        os.Exit(130) // 128 + SIGINT
    }()
}
```

`defer` does NOT run on `os.Exit` or on receipt of a signal that terminates the process (SIGHUP fires when the user closes the wezterm window). The signal handler is the only mechanism that guarantees socket-file cleanup in those cases. Stale `.sock` files left by a missed cleanup are recovered by the next launch's startup sweep, but the explicit handler keeps `<reply_dir>` clean for `wezsesh doctor` and reduces noise.

**Latency**: ~1-2ms per round-trip (single binary fork). PRD goal of <5ms is achievable warm; ~3-5ms cold (binary not in page cache).

#### Spawn invocation

`wezterm cli spawn` does **not** have a `--env` flag (verified). Pass env vars by wrapping in argv. Note: we do NOT pass `WEZSESH_PANE_ID` — the pane ID isn't known at spawn time (`mux.spawn_window` returns it AFTER spawn). The binary instead reads `WEZTERM_PANE` (which wezterm injects automatically into spawned panes) and resolves it via `wezterm cli list --format json`. The Lua side stores the spawned pane's ID in `wezterm.GLOBAL` after `mux.spawn_window` returns.

```
# spawn_mode = "tab" (default): new tab in current window
wezterm cli spawn --cwd <project-cwd> -- \
  env WEZSESH_HMAC_KEY=<hex> \
      WEZSESH_PROTO_VERSION=1 \
      WEZSESH_SNAPSHOT_DIR=<path> \
      WEZSESH_STATE_DIR=<path> \
      WEZSESH_RUNTIME_DIR=<reply-sock-dir> \
      WEZSESH_PLUGIN_VERSION=<from-init-lua> \
      wezsesh

# spawn_mode = "window": new wezterm window — add --new-window
```

In practice the Lua plugin uses `wezterm.mux.spawn_window { ... }` (for `spawn_mode = "window"`) or `current_window:spawn_tab { ... }` (for `spawn_mode = "tab"`) — the CLI form above is shown for clarity about env-vector wrapping.

The binary's pane closes automatically on clean exit because wezterm's default `exit_behavior = "Close"` (since 2022). For users who set `exit_behavior = "Hold"` globally, the top-level `force_close = true` config knob (§7.1) emits an explicit close OSC sentinel before exit.

A small Go helper (`internal/uservar/`) wraps the OSC framing + base64 + bubbletea-Cmd boundary so call sites don't have to think about it.

### 6.5 Active-workspace and target-window detection

`wezterm cli list --format json` returns panes with `workspace`, `window_id`, `is_active`. The binary:
- Reads `$WEZTERM_PANE` to find its own pane.
- Resolves to its `window_id` via the listing.
- Embeds that as `target_window_id` in every op payload.

Lua listeners compare against `window:window_id()` and silently no-op on mismatch. This filter is structurally required, not paranoid: per [wezterm#3524](https://github.com/wezterm/wezterm/issues/3524), the `user-var-changed` event fires in **every window** of the parent mux process, not only the window containing the source pane. Without the filter, every op would execute N times for N open wezterm windows.

#### Note on `is_active`

`is_active` in `cli list` is per-tab (the active pane within its tab), **not** "the globally focused pane." For the globally focused pane, use `wezterm cli list-clients --format json` and read `focused_pane_id`.

### 6.6 Snapshot files and sidecar

#### Snapshot file (owned by `resurrect.wezterm`)

Lives at `<snapshot_dir>/workspace/<encoded-name>.json`. The encoding is `name:gsub("/", "+")` (see `resurrect/utils.lua` and `resurrect/state_manager.lua`). wezsesh mirrors this transform on encode and reverse-decode.

**Verified schema** (from MLFlexer/resurrect.wezterm at the time of this PRD; resurrect has no version field and has silently mutated the schema in past releases — we parse tolerantly):

```
WorkspaceState:
  workspace        string
  window_states    []WindowState

WindowState:
  title            string
  tabs             []TabState
  size             { rows, cols, pixel_width, pixel_height, dpi }   // ints

TabState:
  title            string
  is_active        bool
  is_zoomed        bool
  pane_tree        PaneTree (recursive)

PaneTree:
  left, top, height, width   int            // cell coords
  cwd                        string         // "" if unavailable
  domain                     string?        // unset for non-spawnable domains
  process                    LocalProcInfo? // ONLY when alt_screen_active=true && domain=="local"
  text                       string?        // ONLY when alt_screen_active=false && domain=="local"
                                              // (note: was string[] in older snapshots)
  alt_screen_active          bool?          // local domain only
  is_active, is_zoomed       bool
  bottom, right              PaneTree?      // recursive binary split tree

LocalProcInfo:
  name              string
  argv              []string
  cwd               string
  executable        string
```

**Parser policy**: tolerant. Specifically:
- Every field is optional/pointer in our Go structs.
- `process` is currently always an object-shape `LocalProcInfo` in resurrect's main branch (verified [resurrect pane_tree.lua](https://raw.githubusercontent.com/MLFlexer/resurrect.wezterm/main/plugin/resurrect/pane_tree.lua) at audit time). Pre-2024-08 snapshots may have a bare-string `process` field; the Go parser MUST still handle the string-shape via custom unmarshaler or `json.RawMessage` lazy-parse — old saved snapshots in users' state dirs do not migrate. Resurrect's README schema lags the implementation; treat the source as authoritative.
- Recurse `pane_tree.bottom`/`right` defensively.
- Render "—" in the preview pane when expected metadata is missing.
- Log and skip individual files that fail JSON parse; never crash the picker.
- **Per-file size cap: 10 MiB.** Snapshot files larger than this are skipped with a warning; doctor flags them. Caps the OOM blast radius of a corrupted/maliciously-large file (resurrect itself imposes no cap). Realistic snapshots are <100 KiB; 10 MiB is generous.
- **Per-file parse depth cap: 100.** Defends against pathological JSON nesting. Implemented via `json.Decoder` with a wrapping reader that counts `{` / `[` minus `}` / `]`.

This matches resurrect's own laxity (it just calls `wezterm.json_parse` and uses the table without validation).

#### Foreground process limitation

`pane_tree.process` is only present when `alt_screen_active=true` and `domain=="local"`. For shell panes (the common case where you're sitting at a prompt), `process` is null and we have only `cwd` plus the saved scrollback `text`. The preview pane should display whichever of `process.name`, `text` (last line), or `cwd` is informative.

#### Sidecar file (owned by wezsesh)

`<snapshot_dir>/workspace/<encoded-name>.wezsesh.json`. Schema:

```jsonc
{
  "version": 1,
  "tags": ["api", "backend"],
  "pinned": false,                // also mirrored to state file; sidecar is source of truth for portability
  "on_create": null,              // optional shell command run after `n` creates the workspace
  "on_restore": null,             // optional shell command run after restore (gated by trust — see §6.11)
  "notes": null                   // free-form user notes (future)
}
```

Sidecars are optional. Missing sidecar → defaults (no tags, not pinned, no hooks). Sidecars travel with snapshots if a user manually copies them, which is desirable for tags/notes but security-relevant for hooks (hence §6.11).

#### Encryption — graceful degrade

Resurrect supports opt-in encryption via age / rage / gpg, configured by `state_manager.set_encryption({ enable = true, ... })`. **Default is off.** When enabled, the on-disk file is **whole-file ciphertext** (binary age v1 or OpenPGP, no JSON wrapper). The `.json` extension is preserved but the contents are opaque.

wezsesh detects encryption by **magic-byte sniffing** the first 32 bytes of each snapshot file:
- `age` / `rage`: starts with ASCII `age-encryption.org/v1\n`.
- `gpg`: starts with an OpenPGP packet tag (high bit set).
- Plaintext JSON: starts with `{` `[` or whitespace.

When a snapshot is detected as encrypted:
- Picker still lists the workspace (filename → name decoding works).
- Preview pane shows `(encrypted snapshot — preview unavailable)`.
- Tab count / CWD / process columns omit gracefully.
- `wezsesh doctor` warns when any snapshot has non-JSON magic bytes, including pointing at the user's resurrect encryption config.

Decryption-aware preview is deferred (see §8.2): would require a Lua shim calling `resurrect.state_manager.load_state` and writing decrypted summaries to a runtime cache.

**What still works on encrypted snapshots** (operations that don't need the cleartext):

| Operation | Works on encrypted? | Mechanism |
|---|---|---|
| Switch (live workspace) | ✓ | Workspace already in mux; resurrect file isn't read. |
| Load / restore | ✓ | Lua hands the path to `resurrect.state_manager.load_state`, which decrypts via the user's configured age/rage/gpg keys. |
| Save (overwrite) | ✓ | The `expected_hash` compare runs over the raw ciphertext bytes; resurrect re-writes a fresh ciphertext. We never look inside. |
| Rename | ✓ | Pure filesystem op (`os.Rename` of `<old>.json` → `<new>.json` and the matching `.wezsesh.json` sidecar). |
| Delete | ✓ | Pure filesystem op. |
| Tag / pin | ✓ | Sidecar file is plaintext and separate from the encrypted snapshot. |
| Preview | degraded | `(encrypted snapshot — preview unavailable)`. |

The hash check on save (§6.7) treats encrypted and plaintext snapshots identically — SHA-256 over the on-disk bytes either way.

### 6.7 Concurrent invocations & snapshot hashes

Two TUIs may run simultaneously. The hash check + write sequence MUST be serialized via file locking; without it, two TUIs can both pass each other's hash check and the second write silently clobbers the first. Each TUI:

1. On open, reads each snapshot file and computes its SHA-256.
2. Stores `(name → hash)` in memory.
3. On `save` (with `overwrite: true`), the binary sends `expected_hash` in the payload. **Lua handler acquires a file-level exclusive lock on the snapshot file BEFORE reading for hash comparison**, holds it through the resurrect write, releases after (see "Save serialization via flock" below). If the lock is held by another TUI's handler, returns `SNAPSHOT_LOCKED` (new error code) and the TUI re-prompts the user with retry option. With the lock acquired: read current file, compare to `expected_hash`. If mismatch → release lock, return `SNAPSHOT_CHANGED` error. The binary surfaces a modal: `Snapshot 'foo' has changed since you opened wezsesh. Overwrite anyway? [y/N]`. User confirms → re-send with `overwrite: true` and a refreshed expected_hash from the disk read just performed.
4. `delete` ignores hash (deleting a "newer" snapshot is still a delete). Multi-row delete passes through (each entry locks individually).
5. `rename` follows the same pattern as save when the target file would be overwritten.

This protects P2 (cleaner) from clobbering a snapshot a parallel session just refreshed AND closes the read-then-write TOCTOU between hash-check (T0) and resurrect's actual write (T2) where another TUI could otherwise slip in.

#### Save serialization via flock (added v2.0; lock pattern revised v2.1)

The Lua-side `save` handler uses POSIX advisory locks via `fcntl(F_SETLK, F_WRLCK)` (non-blocking) polled with backoff. **The earlier v2.0 spec used `F_SETLKW` (blocking) plus a "SIGALRM-equivalent watchdog" — that pattern is unimplementable on darwin: `fcntl` syscalls auto-restart on `SA_RESTART` signals (FreeBSD/Darwin behavior; verified [`man 2 fcntl`](https://man.freebsd.org/cgi/man.cgi?query=fcntl&sektion=2)), so a watchdog signal does not interrupt the blocked syscall.** Polling `F_SETLK` is the only portable pattern with a deadline. Lua does not expose `fcntl` natively, so the lock is acquired and released by the Go binary:

1. Binary's `save` handler invokes (locally, before emitting OSC) `safefs.AcquireExclusive(ctx, snapshotPath)` where `ctx = context.WithTimeout(parent, 5*time.Second)`. This call:
   - Calls `safefs.VerifyDir(filepath.Dir(snapshotPath))` first (rejects symlink in the parent path; closes the path-traversal gap that bare `O_NOFOLLOW` doesn't cover, since `O_NOFOLLOW` only blocks symlinks in the *final* path component — a symlink at any earlier component is silently followed; verified [LWN on symlink TOCTOU](https://lwn.net/Articles/899543/)).
   - Opens the file with `O_RDWR|O_NOFOLLOW` via `unix.Openat(dirfd, basename, ...)` from the verified dirfd, NOT a path-based `os.OpenFile`.
   - Calls `unix.FcntlFlock(fd, unix.F_SETLK, &unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0})` in a poll loop with backoff (10ms → 100ms exponential, capped). On `EAGAIN`/`EWOULDBLOCK` the lock is held by another process; sleep and retry until `<-ctx.Done()`. On any other errno, surface as I/O error.
   - Logs a structured `WARN` at 1s and 3s of contended waiting (per-host operational visibility — POSIX advisory locks have NO defined fairness across Linux/macOS/FreeBSD; verified [Pendleton 2010 fairness test](http://bryanpendleton.blogspot.com/2010/07/unix-file-locking-does-not-implement.html); a starved waiter would otherwise hit the deadline silently).
   - Returns the file descriptor + a `release()` closure.
   - On `ctx.Done()`: returns `ErrLockTimeout` → caller maps to `SNAPSHOT_LOCKED`.
2. With the lock held, the binary reads the current file, computes hash, compares to `expected_hash`. If mismatch → release lock → return `SNAPSHOT_CHANGED`.
3. If hash matches → emit `save` OSC to Lua. Lua calls `resurrect.state_manager.save_state(...)`. The lock is held throughout (the binary's process holds the POSIX lock; resurrect's call writes via wezterm's process — so the wezterm process is NOT the lock-holder. **The serialization is across `wezsesh` binaries, not across resurrect calls.** Sound under the assumption that resurrect's `save_state` is only invoked through wezsesh's IPC path; if a user calls `resurrect.state_manager.save_state` directly outside wezsesh, the lock is bypassed. Document as v0.1 limitation; future hardening: have wezsesh always be the writer instead of dispatching through Lua.)
4. After resurrect's write completes (signaled by `file_io.write_state.finished`), the binary releases the lock.

**Cross-binary semantics**: POSIX advisory locks are per-process; a second wezsesh binary attempting `F_SETLK` on the same path while the first holds it gets `EAGAIN`/`EWOULDBLOCK`. The 5s polling deadline bounds wait time. POSIX advisory locks have **no defined fairness guarantee** on Linux, macOS, or FreeBSD — a starved waiter is possible, hence the WARN-at-1s/3s observability.

**Critical clarification (added v2.2)**: POSIX advisory locks are ADVISORY — they are observed only by code that explicitly calls `fcntl(F_GETLK)` / `fcntl(F_SETLK)`. Plain `open(2)` and `write(2)` ignore them. This means:

- The lock DOES serialize across two `wezsesh` binaries (both call `fcntl(F_SETLK)`).
- The lock does NOT block resurrect's writer. Resurrect (running inside wezterm's process) calls `io.open(path, "w+")` which calls libc `fopen(3)` → kernel `open(2)`. None of those consult advisory locks. So while wezsesh holds the lock, resurrect can still rewrite the file at any time — and DOES, when it actually performs the save we requested.

The serialization works anyway because of the message ordering: wezsesh acquires lock → emits OSC → resurrect writes (lock-bypass-but-this-is-the-write-we-asked-for) → emits `file_io.write_state.finished` → wezsesh releases lock. A second `wezsesh` cannot enter its own hash-check until the first releases. The lock is a "binary-instance gate", not a "physical write barrier". This is sound under the assumption that **resurrect's `save_state` is invoked ONLY through wezsesh's IPC path during normal operation**. If a user invokes `resurrect.state_manager.save_state(...)` directly from their wezterm config (e.g., via a separate keybinding), the lock is bypassed entirely. Document as v0.1 limitation; future hardening: have wezsesh always be the actual file writer (Go-side), invoking resurrect only for state-object construction.

**Network / cloud-sync filesystem detection** (added v2.1; substantially revised v2.2): before opening the snapshot dir, the binary calls `safefs.IsNetworkFS(snapshotPath)` which combines two layers of detection. Layer 1 is `unix.Statfs`-based (catches mounted network filesystems). Layer 2 is path-prefix–based (catches modern File Provider–backed cloud-sync folders that statfs cannot see).

**Layer 1 — `unix.Statfs` matching**:
- Linux: `Statfs_t.Type` (int64) compared against `NFS_SUPER_MAGIC` (0x6969), `CIFS_MAGIC_NUMBER` (0xff534d42), `FUSE_SUPER_MAGIC` (0x65735546), `AUTOFS_SUPER_MAGIC` (0x0187), `SMB2_MAGIC_NUMBER` (0xfe534d42), and the SSHFS-via-FUSE generic magic. Constants live in `golang.org/x/sys/unix`.
- darwin: `Statfs_t.Fstypename` (C-string array) compared against `"nfs"`, `"smbfs"`, `"webdav"`, `"osxfuse"`, `"fuse"` (some FUSE implementations report this generic string), `"afpfs"`, and `"autofs"`. **The earlier v2.1 list omitted `"fuse"` — corrected v2.2.** Comparison is exact-match against the trimmed `Fstypename`.

**Layer 2 — path-prefix heuristic for File Provider extensions on darwin** (added v2.2; the only practical detection mechanism for modern macOS cloud sync). Modern macOS (13+) iCloud Drive, Dropbox (2024+ client), and Google Drive Desktop (2024+ client) all migrated from osxfuse mounts to Apple's File Provider framework. **They are invisible to `Statfs`** — File Provider–backed paths report `f_fstypename = "apfs"`, indistinguishable from local APFS. The Layer-1 check returns "not network" for the most common cloud-sync paths on macOS. Without Layer 2, the helper has zero coverage for what is, in practice, the dominant cloud-sync risk on darwin.

Layer 2 matches the resolved absolute path against the prefix list below. **Resolution pipeline (revised v2.3)**:

1. Tilde-expand `~` against `os.UserHomeDir()` → produce intermediate path. Doctor warns if `$HOME` differs from `os/user.Current().HomeDir` (catches `sudo` / `doas` divergence).
2. Wrap `filepath.EvalSymlinks` in a goroutine under `context.WithTimeout(500ms)`. On overrun (symlink loop, hung NFS ancestor, dataless cloud-only ancestor that triggers daemon-mediated download), return the unresolved path AND emit a non-fatal WARN: `"snapshot path symlink resolution timed out — Layer 2 cloud-sync detection may be incomplete."` The unresolved path is treated as non-network (conservative — better a missed warning than a startup hang). On `os.Readlink` permission errors (`EACCES`/`EPERM` on ancestor symlinks owned by another user, mode 0700 dirs), the goroutine returns the partially-resolved path; same WARN. Without this wrapper, the helper designed to warn about hangs becomes one — exactly the inversion v2.2 closed for `Statfs` (added v2.3 closing the symmetric gap for the symlink path).
3. `filepath.Clean` on the result (normalizes redundant slashes from trailing-slash `$HOME` like `/Users/grady/`).
4. NFC-normalize the resolved path AND the tilde-expanded prefix anchors at the SAME step using `golang.org/x/text/unicode/norm.NFC.String`. APFS preserves on-disk filename bytes as-given (verified [Apple APFS reference](https://developer.apple.com/library/archive/documentation/FileManagement/Conceptual/APFS_Guide/FAQ/FAQ.html); APFS does NOT VFS-normalize the way HFS+ did). If `$HOME` is `/Users/café` with `é` stored as NFD on-disk and the prefix anchor is constructed with NFC `é`, comparison would fail without consistent normalization.
5. Use `strings.EqualFold` (case-insensitive) for darwin prefix matching; case-sensitive on Linux. APFS is case-insensitive but case-preserving by default; a user-created `~/dropbox` (lowercase) resolves to the same inode as `~/Dropbox` but Go's `filepath.Match` is case-sensitive without `EqualFold`.

**Prefix list (extended v2.3)**:
- `~/Library/Mobile Documents/` (iCloud Drive)
- `~/Library/CloudStorage/iCloud~*` (iCloud Drive in macOS 13+ standardized layout)
- `~/Library/CloudStorage/Dropbox*` (Dropbox File Provider)
- `~/Library/CloudStorage/GoogleDrive*` (Google Drive File Provider)
- `~/Library/CloudStorage/OneDrive*` (OneDrive File Provider)
- `~/Library/CloudStorage/Box*` **(added v2.3 — Box for Mac uses File Provider since 2024)**
- `~/Library/CloudStorage/Nextcloud*` **(added v2.3 — community NextCloud builds with File Provider; verified [open issue #1337](https://github.com/nextcloud/desktop/issues/1337))**
- `~/Library/CloudStorage/Proton*` **(added v2.3 — ProtonDrive experimental File Provider on macOS 14+)**
- `~/Library/CloudStorage/Seafile*` **(added v2.3 — SeaDrive File Provider; community feature request live)**
- `~/iCloud Drive` (legacy symlinked alias). v2.3: when matched, ALSO `EvalSymlinks` and check the resolved target — handles users who renamed the alias via Terminal (Finder blocks rename but `mv` works; verified [Apple Discussions thread 8392536](https://discussions.apple.com/thread/8392536)). The alias resolves under `~/Library/Mobile Documents/` regardless of its display name.
- `~/Dropbox` (legacy direct-mount layout, pre-File-Provider)
- `~/Google Drive` and `/Volumes/GoogleDrive*` (legacy macFUSE Google Drive — vestigial on Apple Silicon; produces benign false-positive for unrelated `/Volumes/GoogleDriveBackup`-named external mounts; documented).
- `~/Desktop` and `~/Documents` *only* if iCloud Drive's "Desktop & Documents" sync is enabled (detect via the presence of `~/Library/Mobile Documents/com~apple~CloudDocs/Desktop` or `Documents` symlink; if found, the `~/Desktop` and `~/Documents` paths under `$HOME` are also iCloud-backed). v2.3 caveat: this assumes Apple's symlink implementation; if Apple switches to bind-mounts in a future macOS release, detection breaks. Documented.

**Statfs-itself hang surface** (added v2.2). `Statfs(path)` syscall can block in the kernel for tens of seconds on a hung NFS server (verified [Red Hat NFS hang KB](https://access.redhat.com/solutions/28211)) — meaning the helper designed to warn about hang-prone filesystems can itself hang. Mitigation: run `Statfs` in a goroutine under `context.WithTimeout(2 * time.Second)`; on overrun, return `(fsType: "unknown", isNetwork: true, err: nil)` (treat as network because we couldn't prove it's local). New error code `XDG_PATH_TIMEOUT` from §6.19 covers the caller; the IsNetworkFS helper itself fails-warn rather than fails-closed.

**Behavior on detect**:

- **NFS**: `fcntl` locks require `lockd` running and have known kernel-level hang/loop bugs (verified [Linux-NFS list, infinite F_SETLKW loops](https://linux-nfs.vger.kernel.narkive.com/f0ZYr5dG/nfs-infinite-loop-in-fcntl-f-setlkw)). The lock pattern still uses `F_SETLK` (non-blocking) so we cannot deadlock, but cross-host serialization may silently fail. The binary surfaces a one-time WARN at TUI open: `wezsesh: snapshot dir on NFS — concurrent writes from multiple hosts may not serialize. Recommend single-host access only.` Operation is NOT refused — many users will run a single host with NFS-mounted home and benefit from local serialization.
- **FUSE-T (Apple Silicon kext-less FUSE replacement)**: implements FUSE via NFS loopback. `Fstypename` returns `"nfs"`. This is technically a false-positive (the underlying data isn't a remote NFS share) but the cautionary WARN is still appropriate — FUSE-T inherits NFS-style locking quirks. No special handling needed; the NFS branch covers it.
- **autofs**: returns `AUTOFS_SUPER_MAGIC` on Linux / `"autofs"` on darwin **before** the underlying mount is triggered. `statfs(2)` does not trigger automount on Linux (verified [Linux kernel autofs docs](https://docs.kernel.org/filesystems/autofs.html)). The helper treats autofs as "network — err on side of caution" without attempting to resolve the underlying mount. Rationale: probing the underlying mount requires `stat()` which DOES trigger automount and can hang.
- **iCloud Drive / Dropbox / Google Drive / OneDrive (File Provider on macOS 13+)**: detected by Layer 2 path-prefix match. File Provider extensions perform on-demand materialization through `fileproviderd` (Apple) or vendor daemons (Dropbox / Google) which write through the standard APFS path BUT do not coordinate with held POSIX locks (verified [eclecticlight on APFS+iCloud sync](https://eclecticlight.co/2023/11/15/backup-errors-icloud-drive-and-the-limits-of-apfs/), [Apple TidBITS on File Provider migration](https://tidbits.com/2023/03/10/apples-file-provider-forces-mac-cloud-storage-changes/)). The binary surfaces WARN: `wezsesh: snapshot dir under <provider> (File Provider–backed) — sync rewrites bypass file locks. Concurrent saves may silently lose data. Recommend a non-synced path (e.g., ~/.local/share/wezterm/state/resurrect/).` Operation is NOT refused.
- **Legacy osxfuse iCloud / Dropbox / Google Drive (Intel Macs / pre-2024 clients)**: caught by Layer 1 `osxfuse` match. Same WARN.
- **SMB / SSHFS / WebDAV / AFP**: same NFS-style WARN.
- `wezsesh doctor` reports the resolved FS type AND the matched cloud-sync prefix (if any) per path. The doctor output explicitly distinguishes "Statfs network type" vs "path-prefix cloud-sync detection" so users understand which layer triggered the warning.

**Known false-negative classes** (documented honestly; expanded v2.3 with the round-12 audit's findings). The WARN is best-effort; the actual data-loss defense is the recommendation that snapshot dirs AND ALL THEIR CONTENTS live under `~/.local/share/wezterm/state/resurrect/` (the resurrect default; explicitly NOT in any cloud-sync directory). Layer 2 has known gaps in:

1. **Custom mount paths**: Dropbox, OneDrive, Google Drive allow advanced users to configure non-default sync locations. Once the user moves the sync root outside `~/Library/CloudStorage/`, Layer 2 cannot find it.
2. **Third-party providers not on the list**: pCloud, Sync.com, MEGA, Yandex Disk use legacy macFUSE or proprietary kexts and SHOULD trip Layer 1 (`osxfuse`/`fuse` match). If a future provider ships a File Provider extension under a non-listed path, Layer 2 misses it. v2.3 extended the list with Box, NextCloud, Proton, Seafile to cover the most-requested gaps.
3. **Symlinks INSIDE the snapshot dir** (added v2.3 — round-12 audit). Layer 2 checks the snapshot dir's own resolved path. If the snapshot dir is local (e.g., `~/.local/share/wezterm/state/resurrect/`) but contains a symlink `workspace/shared → ~/iCloud Drive/team-snapshots/`, every snapshot file accessed via that subdir is cloud-synced. Layer 2 cannot detect this without recursively scanning every file's resolved path, which would itself fork-and-hang on a stalled File Provider daemon. Recommendation strengthened: ALL contents of the snapshot dir must be local-only — no symlinks pointing into cloud-synced paths.
4. **TOCTOU between check and use** (added v2.3). Layer 2 evaluates at TUI open. If the user enables/disables iCloud Drive's "Desktop & Documents" sync, renames the iCloud alias, or moves the snapshot dir between TUI open and a save, the WARN is stale. Reload the TUI to re-evaluate.
5. **`pluginkit` discovery deferred to v0.2+** (added v2.3). Dynamic enumeration of File Provider extensions via `pluginkit -m -p com.apple.fileprovider-nonui` would close most of the gaps above. But `pluginkit` itself forks a process that queries `fileproviderd`; if that daemon is stalled, `pluginkit` hangs. Out of scope for v0.1 — when implemented, MUST be wrapped in `context.WithTimeout(2s)` with a fallback to the static list, AND should run only at doctor time (not TUI launch). Document.

**Crash recovery**: `fcntl` locks release automatically when the holding process exits (unlike file-based lock files that need GC). A crashed wezsesh leaves no lock state to clean up.

**Hold time**: lock is held for the duration of the resurrect write (~milliseconds for a typical snapshot, up to ~100ms for very large snapshots). Acceptable; comparable to `git`'s index lock.

**New error code** `SNAPSHOT_LOCKED` is added to §6.3, additive to the existing list.

#### Sidecar serialization via flock (added v2.0)

The same `safefs.AcquireExclusive` pattern is used for sidecar updates (`tag`, `pin`, on-disk-only field changes). Without serialization, two TUIs both reading sidecar V1, modifying their local copy, and writing back lose one of the updates (last-writer-wins on the entire sidecar object). With the lock: each tag/pin op holds the sidecar's lock for read-modify-write, serializing the operations across binaries on the same machine.

Lua-side tag/pin calls go through the binary's IPC handler (per §6.19 "sidecar writes are Go-only"); the binary acquires the lock, reads, modifies, writes via `safefs.AtomicWriteFile`, releases. Hold time is sub-millisecond.

#### Resurrect periodic_save race

Resurrect's `state_manager.periodic_save` rewrites snapshot files in the background using `io.open(path, "w+")` — a non-atomic truncate-and-rewrite. A reader concurrent with a write will see a half-written or empty file. Mitigation is layered:

1. **Lua-side gate**. Plugin subscribes to:
   ```lua
   wezterm.on('resurrect.file_io.write_state.start', function(path) wezterm.GLOBAL.wezsesh_writing[path] = true end)
   wezterm.on('resurrect.file_io.write_state.finished', function(path) wezterm.GLOBAL.wezsesh_writing[path] = nil end)
   ```
   When the binary requests "open TUI," plugin first stalls (small `wezterm.time.call_after` retry, max 500ms) if any wezsesh-relevant snapshot is mid-write. The `.finished` event fires on **both success and failure paths** of `file_io.write_state` (`file_io.lua:94` is unconditional, after the inner pcall captures the error and emits `resurrect.error`), so the gate clears correctly even if the write fails.

2. **Go-side defensive parsing**. Treat any JSON parse error during snapshot read as transient: retry 3× with 25ms backoff. On continued failure, log a warning and skip that snapshot — never abort the TUI open. Covers the path where wezsesh runs without the Lua gate (e.g., `wezsesh list` from a shell).

### 6.8 State file (usage + pins)

Path: `$XDG_STATE_HOME/wezsesh/state.json` (fall back to `~/.local/state/wezsesh/state.json`).

```jsonc
{
  "version": 1,
  "usage": {
    "~/.dotfiles": { "last_switched": 1714435200, "switch_count": 142 },
    "~/code/foo":  { "last_switched": 1714438800, "switch_count": 87 }
  },
  "pins": ["~/.dotfiles"]
}
```

Updated by the binary on `switch`, `pin`, etc. Reads are cheap (single small file). **Atomic-write via temp + rename** within the same dir (so the rename is atomic on Unix). No locking — concurrent writes are last-writer-wins. For `usage` (counters), this is benign: a lost increment of `switch_count` doesn't break correctness. For `pins` (set), concurrent toggles can lose updates; the impact is "I pinned X but my pin didn't stick" once in a blue moon. Acceptable trade-off vs. introducing a lock file. If the impact ever surfaces in practice, switch to `flock(2)` on the state file path.

### 6.9 Logging

- **Binary**: writes to `$XDG_STATE_HOME/wezsesh/wezsesh.log` (rotated at 1MB, keep last 3).
- **Plugin**: uses `wezterm.log_info` / `log_warn` / `log_error`. Visible in wezterm's debug overlay.
- **Log level resolution**: `min(opts.log_level, env.WEZSESH_LOG)`. The lower (more verbose) level wins so `WEZSESH_LOG=debug` always works for triage even if the user's config says `info`. Levels: `error`, `warn`, `info`, `debug`, `trace`.

### 6.10 Plugin layout

```
wezsesh/
├── plugin/
│   ├── init.lua              ← entry, exposes apply_to_config; M.VERSION constant
│   └── wezsesh/
│       ├── manager.lua       ← spawn binary, register keybinding, generate HMAC key
│       ├── ipc.lua           ← user-var-changed handler: pane filter + HMAC verify + replay guard + dispatch
│       ├── ops.lua           ← rename/delete/load/switch/save/new/tag/pin
│       ├── result.lua        ← write reply via Unix socket; toast helper
│       ├── state.lua         ← wezterm.GLOBAL wrapper (in-flight requests, write gate)
│       ├── canonical_json.lua ← canonical-JSON serializer (matches Go side; rules in §6.3)
│       ├── hmac.lua          ← HMAC-SHA-256 wrapper (RFC 2104; ~30 LOC); hex_to_bin's the key first
│       ├── ct_eq.lua         ← constant-time string compare for HMAC verify (§6.14)
│       ├── b64.lua           ← base64 encode (for reply payloads; ~20 LOC)
│       └── vendor/
│           ├── sha2.lua      ← Egor-Skriptunoff/pure_lua_SHA, MIT, pinned commit
│           └── SOURCES.lock  ← upstream commit + sha256 of vendored file
├── cmd/
│   └── wezsesh/
│       ├── main.go           ← bubbletea TUI entry; subcommand router
│       ├── reply.go          ← `wezsesh reply <sock> <b64json>` — Lua-side reply path
│       ├── doctor.go         ← `wezsesh doctor`
│       ├── list.go           ← `wezsesh list --format json`
│       ├── find.go           ← `wezsesh find`
│       ├── trust.go          ← `wezsesh trust`
│       └── version.go
├── internal/
│   ├── snapshots/            ← snapshot + sidecar I/O, hashing, tolerant parser, magic-byte sniff
│   ├── state/                ← XDG state file (usage + pins), atomic writes
│   ├── wezcli/               ← wrapper over wezterm cli invocations
│   ├── uservar/              ← OSC 1337 emit (writes to /dev/tty, mutex-protected) + bubbletea Cmd wrapper
│   ├── ipcsock/              ← per-request Unix-socket listener; `StartListener()` synchronous helper
│   ├── canonicaljson/        ← Go canonical-JSON serializer (byte-identical to Lua side)
│   ├── hmac/                 ← HMAC-SHA-256 sign + canonical-JSON marshal helper
│   ├── trust/                ← hook trust store (~/.local/share/wezsesh/allow/<sha256>)
│   ├── safefs/               ← O_NOFOLLOW + openat(2) atomic-write helper (§6.19);
│   │                            mandatory for state.json, sidecars, trust files
│   ├── argvallow/            ← snapshot argv allowlist for the on_pane_restore callback (§6.18)
│   ├── find/                 ← cross-workspace pane search (§6.13)
│   ├── pathpicker/           ← `n` flow, runs new_workspace_command
│   ├── nameval/              ← workspace name validation rules (§6.17); single source of truth
│   │                            for ILLEGAL_NAME triggers
│   ├── doctor/               ← health checks
│   └── tui/                  ← bubbletea models (list, modals, preview, help); `huh` for dialogs
├── flake.nix                 ← dev shell + builds wezsesh binary
├── go.mod
├── README.md
└── LICENSE                   ← MIT
```

### 6.11 Hook trust model

`on_create` and `on_restore` are arbitrary shell commands stored in a sidecar file that may travel between machines (copied, dotfiles repo, sync). Auto-executing them is the same threat model as direnv's `.envrc` or VSCode's `tasks.json`. We adopt direnv's hash-based fail-closed approach.

**Trust store**: `$XDG_DATA_HOME/wezsesh/allow/<sha256>` (fall back to `~/.local/share/wezsesh/allow/`). File contents = the absolute sidecar path (for `wezsesh trust --list`).

**Hash construction (length-prefixed, NOT separator-delimited)**:
```
sha256( uint32_be(len(absolute_sidecar_path))
     || absolute_sidecar_path bytes
     || uint32_be(len(command_bytes))
     || command_bytes )
```

The hash binds **path + content**: copying a sidecar to another machine fails closed (path differs); editing the command on the same machine fails closed (content differs).

**Why length-prefixed, not `\n`-delimited**: workspace names are user-supplied and become sidecar paths. A name containing a literal `\n` (e.g., `foo\nrm -rf ~`) under a `path || "\n" || cmd` scheme allows hash collision — `(path="dir/foo\nrm -rf ~", cmd="")` produces the same bytes as `(path="dir/foo", cmd="rm -rf ~")`. With user-controlled path components the collision is forgeable. Length-prefixing eliminates the ambiguity: no path can carry its own length prefix as suffix bytes, and a zero-length command is distinct from any non-empty command. `uint32_be` rather than varint keeps the encoding fixed-width and trivial to implement in both Go and (vendored) Lua.

**Default flow (fail-closed, non-interactive)**:
1. User triggers restore (or `n` create).
2. Workspace creates/restores normally.
3. Hook check runs after the workspace operation succeeds.
4. If the trust file exists and the hash matches → run the command silently.
5. If trust is missing or the hash doesn't match → **skip the hook silently**. Log to `wezterm.log_warn` and write one line to the binary's stderr:
   ```
   wezsesh: on_restore for "<name>" is not trusted on this machine.
     command: <the command>
     approve: wezsesh trust <name>
     reject : wezsesh trust --revoke <name>
   ```

**Optional interactive prompt** (`hooks.prompt_on_untrusted = true`): on untrusted hook, show a TUI confirm prior to running:
```
wezsesh: workspace "<name>" has an on_restore hook that is not trusted.

  command: <the command>
  source : <sidecar path>

Trust and run this command? [y/N/show diff]:
```
Default `N`. `show diff` prints the previously-trusted command (if any) vs. the current command, then re-prompts.

**Global escape hatches**:
- `wezsesh.hooks.run_hooks = false` in Lua config — disables hooks entirely.
- `WEZSESH_NO_HOOKS=1` env var — beats config (useful for CI / shared machines).

**Hook execution environment** (concrete Go semantics):

1. **Read-once-exec-from-memory** (TOCTOU defense). The sidecar is read exactly once. The `command_bytes` value captured at that read is used for both hash computation AND `exec.Command` invocation. The binary MUST NOT re-read the sidecar between trust check and exec — between `os.ReadFile(sidecarPath)` (T0) and `cmd.Run()` (T3), an attacker could swap the file. Idiomatic pattern:
   ```go
   data, _ := os.ReadFile(sidecarPath)
   sidecar, _ := parseSidecar(data)            // command in memory
   if !trustOK(sidecar.Path, sidecar.OnRestore) { return }
   cmd := exec.Command(shell, "-c", sidecar.OnRestore)  // same in-memory bytes
   ```

2. **Shell resolution**. Look up `$SHELL`; if empty, missing, or `exec.LookPath` fails, fall back to `/bin/sh` and log WARN. Don't pre-process or re-quote the command — pass as a single argv element to `-c`. Fish/nushell users own the compatibility of their hooks with their `$SHELL`.

3. **Working directory**. `Cmd.Dir = primaryCwd`. For `on_create`, primaryCwd is the picked path (always exists). For `on_restore`, primaryCwd is the first pane's cwd from snapshot — may be stale. `os.Stat(primaryCwd)` first; if `IsNotExist`, fall back to `os.UserHomeDir()` and log WARN.

4. **Stdin closed**. `Cmd.Stdin = nil` (Go maps to `/dev/null`). Default inheritance would block forever on hooks that read stdin.

5. **Stdout/stderr**. Inherit `os.Stderr` from the wezsesh binary. Hook output appears in the launching pane after the binary exits.

6. **Process group**. `Cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}`. Ensures the hook + any children form a single process group rooted at the hook PID. On timeout (below), kill the group via `syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)`. Without this, grandchildren survive a `npm run watch`-style hook indefinitely.

7. **Timeout**. `context.WithTimeout(10 * time.Minute)` wrapping `cmd.Run`. On expiry: SIGTERM the process group, wait 5s, SIGKILL. Log `wezsesh: hook timed out after 10m`. Configurable via `wezsesh.hooks.timeout_seconds` (min 1, default 600).

8. **Environment scrubbing**. Construct `Cmd.Env` from `os.Environ()` minus any vars matching `WEZSESH_*` prefix. Prevents accidental HMAC-key leakage to a hook that runs `printenv` or to a subprocess that logs its env. Defense-in-depth — the hook is user-trusted, but session secrets shouldn't propagate.

9. **Symlink defense on the trust file** (separate from the §6.19 filesystem-safety section because it's specific to the trust-check path). Use `os.Lstat` (NOT `os.Stat`) on the trust file path; if `ModeSymlink`, treat as untrusted and log WARN `wezsesh: trust file at <path> is a symlink; ignoring`. On binary startup, `os.Lstat` the trust directory (`$XDG_DATA_HOME/wezsesh/allow/`); if symlink or non-directory, abort with a hard error rather than silently following.

10. Hook is run **after** the workspace operation completes. A failing hook does not roll back the create/restore. The reply socket has already been written by then; hook failures are surfaced via stderr + `resurrect.error`-style log lines, not via the IPC reply.

### 6.12 Restore behavior

`resurrect.wezterm`'s restore path has zero error handling — `restore_workspace`, `restore_window`, `restore_tab` all use bare `for` loops and the first spawn failure raises an unhandled Lua error. The `restore_workspace.finished` event only fires on the success path, so it cannot be used as a completion signal.

**wezsesh approach**:
- Lua-side handler wraps `resurrect.workspace_state.restore_workspace(...)` in `pcall`. Without this, partial restores crash the listener uncleanly. Note: resurrect's `restore_workspace.finished` event reaches its emission point only when **no Lua error escapes** the bare-`for` loops in `workspace_state.lua:19,43` and `window_state.lua:56` — there is no error path in resurrect's restore code at all (verified). pcall on our side is the only mechanism that turns a partial-restore error into a structured `RESURRECT_PARTIAL` reply.
- On `pcall` failure, emit a reply via the Unix socket (per §6.4) with `status: "partial"` and `error.code = "RESURRECT_PARTIAL"`; raw Lua error message in `details`.
- We do **not** attempt rollback. The mux is left in whatever state resurrect produced (typically: N-of-M tabs spawned).
- Documented behavior for users: "Partial restores are best-effort; we don't roll back. Re-running restore is safe but may produce duplicate tabs."

This matches user expectations set by tmux session managers, none of which do transactional restore.

**Caveat: resurrect doesn't `error()` on save/load failures.** Verified in source: `state_manager.save_state` and `file_io.write_state` swallow errors and emit `resurrect.error` events instead. `pcall`-wrapping is **necessary but not sufficient** for failure observability.

The plugin subscribes to `resurrect.error`:

```lua
wezterm.on('resurrect.error', function(message)
    wezterm.log_warn('resurrect error: ' .. tostring(message))
    -- Correlate with in-flight requests if possible (best-effort)
    local now = os.time()
    for id, req in pairs(wezterm.GLOBAL.wezsesh_requests or {}) do
        if (now - req.started_at) < 5 then
            -- Likely the request that triggered this; surface via reply socket
            -- (best effort; the actual ops handler will also surface through pcall)
        end
    end
end)
```

Snapshot-load failures (file deleted between picker open and switch, parse error, decryption rejected) get the dedicated `SNAPSHOT_LOAD_FAILED` error code.

### 6.13 Cross-workspace process search (`wezsesh find`)

A top-level subcommand for finding panes/processes across all workspaces. Different mental model from the workspace picker (you don't know *which* workspace), so it lives outside the main TUI.

**Data source**: `wezterm cli list --format json` from the binary's pane. A subprocess in any pane sees all windows, all tabs, all workspaces of the local mux via `$WEZTERM_UNIX_SOCKET`. Augmented with `wezterm cli list-clients --format json` to mark the globally focused pane.

**Available fields per pane** (verified against `wezterm/src/cli/list.rs::CliListResultItem`): `window_id`, `tab_id`, `pane_id`, `workspace`, `size`, `title`, `cwd`, `cursor_x/y`, `tab_title`, `window_title`, `is_active` (per-tab, NOT global), `is_zoomed`, `tty_name` (`Option<String>` — non-empty on Unix when pty reports a name; `null` on Windows or when unreported). Note: there is **no** `foreground_process_name` in CLI JSON — it's only available via Lua's `PaneInformation`.

**`cwd` parsing gotcha**: the field is a `file://` URL string built via `url::Url::from_directory_path` (`mux/src/localpane.rs:1057`), but is the empty string `""` (NOT `null`) when the working dir is unknown. The Go-side parser must guard for `""` before URL-decoding — naive `url.Parse("")` returns a non-nil URL with empty Path, which silently breaks downstream substring/cwd-match logic.

**Match strategy**:
- **Default**: case-insensitive substring against `title`, `tab_title`, `window_title`, and the path component of `cwd`. `title` is the pane's dynamic title (often `nvim`, `pgcli`, `claude`) — a decent process proxy but user-overridable.
- **`--deep`**: additionally shell out to `ps -t <tty_name> -o stat=,comm=,args=` per pane and match against the foreground-process-group row (STAT contains `+`). Slow (O(panes) ps invocations), opt-in.

**On selection** (corrected v2.3 — round-12 audit overturned a v1.x assumption that had never been verified against wezterm source):

- **Same workspace** (selected pane's `workspace` == current focused client's active workspace): `wezterm cli activate-pane --pane-id N`. The server-side `SetFocusedPane` handler at [`wezterm-mux-server-impl/src/sessionhandler.rs`](https://raw.githubusercontent.com/wezterm/wezterm/main/wezterm-mux-server-impl/src/sessionhandler.rs) calls `window.save_and_then_set_active(tab_idx)` + `tab.set_active_pane(&pane)` + `mux.notify(MuxNotification::PaneFocused(pane_id))`. This correctly raises the tab and pane within the user's current workspace and window.

- **Different workspace** (selected pane's `workspace` != current active workspace): the binary MUST use a two-phase sequence:
  1. **Phase 1 — workspace switch**: emit a `switch` op via the standard OSC + reply-socket path (§6.4). Lua dispatches `act.SwitchToWorkspace { name = selected.workspace }` against `window:active_pane()`. The `started` reply returns within milliseconds; the Go binary then polls per §6.15 (`StartSwitchPoller` with 5s ceiling) until the workspace transition is confirmed via the cross-reference of `cli list-clients` (focused_pane_id) → `cli list` (that pane's `workspace` AND `window_id`).
  2. **Phase 2 — pane activation**: once the polling predicate fires, the binary issues `wezterm cli activate-pane --pane-id N`. The user is now on the target workspace AND the target pane is active.

**Why the two-phase sequence is mandatory**: PRD revisions before v2.3 carried forward a claim from v1.x that "`cli activate-pane` cross-workspace activation is server-side implicit (workspace switch happens automatically)." Round-12 audit definitively refuted this by reading the server-side `SetFocusedPane` PDU handler. The handler does NOT call `mux.set_active_workspace_for_client(...)`. If the user invokes `cli activate-pane` with a pane in a different workspace, the server marks that pane as active within ITS window/workspace, but the user's active workspace is unchanged — the user remains on their original workspace and the activated pane is invisible. Without the explicit Phase 1 switch, `wezsesh find` selecting a cross-workspace pane appears to do nothing from the user's perspective. CI assertion: a `find` selecting a cross-workspace pane MUST result in BOTH (a) the user being on the target workspace AND (b) the target pane being active.

**Two-phase find race conditions** (clarified v2.4 — round-13 audit):

- **Client pinning across Phase 1 ticks (HIGH, v2.4)**: §6.5's multi-GUI-client disambiguation rule ("pick the client with most-recent `last_input`, tie-break on `client_id`") is applied **once at Phase 1 start** to capture a `target_client_id`. Every subsequent polling tick re-evaluates the predicate against THAT pinned client only — NOT against whatever client is "most recent" at the tick. Without pinning, a second connected GUI client (mosh, SSH-mux, another local wezterm) becoming "most recent" mid-poll would flip the predicate to the wrong client, causing the poller to either (a) succeed against the wrong client's workspace or (b) time out spuriously. The pinned `target_client_id` is the same client that triggered the `wezsesh find` invocation; its workspace is the only one whose transition matters. Pinning has no UX downside: if the originating client disconnects mid-poll, the predicate fails closed (poller times out at 5s with `MUX_UNREACHABLE`), which is the correct behavior.
- **Window-id scoping in Phase 1 predicate (HIGH, v2.4)**: §6.15 step 4's predicate "workspace `target` is now the active workspace of the focused client in `target_window_id`" is reaffirmed: the `cli list` lookup of the pinned client's `focused_pane_id` MUST verify that the resolved pane lives in `target_window_id`. If the user closes the wezterm window mid-Phase-1 and the pinned client's focused_pane_id rolls over to a different window, the predicate must fail (not succeed against the wrong window). The corresponding error is `MUX_UNREACHABLE` if the predicate never fires within 5s.
- **Pane-closed race between Phase 1 and Phase 2**: between Phase 1 success and Phase 2 invocation, the target pane could close (Ctrl-D, window close). Phase 2 already inherits the existing `PANE_CLOSED_RACE` handling (§6.20): on `cli activate-pane` exit 1, the binary re-lists once and retries; second failure surfaces the toast. The two-phase delay widens this race window slightly (~50–5000ms vs. ~0ms in same-workspace selection); the existing single-retry covers it adequately.
- **Cumulative wall-clock budget (MEDIUM, v2.4)**: worst case 5s (Phase 1 ceiling) + 2s (`cli activate-pane` per §6.20 timeout) = 7s end-to-end. Documented; the TUI MUST render a Phase-1 progress indicator (one-line status `Switching workspace...` → `Activating pane...`) so the user sees movement during the 5s polling window. CI assertion: progress indicator updates at the Phase-1→Phase-2 transition. Exponential-backoff polling (50ms→500ms) deferred to v0.2 if user feedback reports concurrent-TUI contention; current cadence (50ms × 2 CLI invocations = ~30ms/tick × 100 ticks = ~60% CPU per polling slice on one core) is acceptable for v0.1 single-TUI use.

**Edge case — multi-window**: even within the same workspace, if the selected pane is in a different wezterm GUI window than the user is focused in, `cli activate-pane` raises the tab and pane within their parent window's mux state but does NOT raise the GUI window in OS z-order (no documented `MuxNotification` triggers OS-level window-raise). The user may need to alt-tab to the other window. Documented; out of scope to fix without deeper wezterm integration.

**Future**: `Ctrl-/` from the main TUI invokes find inline. Out of scope for v0.1: remote-domain panes (SSH/TLS muxes), headless mux-server discovery, scrollback grep via `get-text`.

### 6.14 Security model — pane binding + HMAC + replay guard

OSC 1337 escape sequences can be emitted from **any** pane by **any** process running there. A user `cat`ing a malicious file, an npm postinstall script, a `curl` of a server response with embedded OSC bytes — all can set `wezsesh_op` to arbitrary JSON and trigger our Lua handler. iTerm2 had CVE-2019-9535 for this exact threat class.

We mitigate with three layered defenses.

#### Layer 1: pane-ID binding

At TUI spawn, the plugin records the binary's pane ID. `pane:pane_id()` is **immediately available** after `wezterm.mux.spawn_window` returns (verified in source — pane_id is allocated synchronously before the tuple is built):

```lua
local _, pane, _ = wezterm.mux.spawn_window { args = { 'env', ..., 'wezsesh' } }
local pid_key = tostring(pane:pane_id())   -- MUST be string, see note below
wezterm.GLOBAL.wezsesh_state[pid_key] = {
    hmac_key = key,
    target_window_id = window:window_id(),
    seen_ids = {},
    spawned_at = os.time(),
}
```

**Why `tostring(pane_id)` is required**: `wezterm.GLOBAL` is backed by an `Arc<Mutex<BTreeMap<String, Value>>>` (verified in `lua-api-crates/share-data/src/lib.rs`). Object-shaped GLOBAL nodes only accept string keys; assigning to a numeric index raises `Err("can only index objects using string values")` at runtime. Mixed-key tables are also hard-rejected. **Every pane-id key in this PRD is the string form (`tostring(...)`)**, even when the source value (`pane:pane_id()`) is an int. The Lua-side helper `state.lua` exposes `set_state(pane_id, ...)` / `get_state(pane_id)` wrappers that perform the coercion in one place.

GLOBAL state is keyed by stringified `spawned_pane_id` (NOT a global single-object) so multiple TUIs in different windows are independent. A separate `wezterm.GLOBAL.wezsesh_requests[request_id]` map tracks in-flight requests by ULID (already a string) for reload durability (§6.15).

The `user-var-changed` listener uses `tostring(pane:pane_id())` to look up the matching session. Pane IDs are wezterm-internal, immutable for the pane's lifetime, and not forgeable from outside — there is no escape sequence or API to write an OSC and have it appear sourced from a different pane.

A periodic TTL sweep (in `window-config-reloaded` and after each dispatch) prunes `wezsesh_state[pid_key]` entries with `os.time() - spawned_at > 60` to bound memory across crashed binaries.

#### Layer 2: HMAC-SHA-256 payload signing

Wezterm's Lua API has **zero crypto primitives** (no `wezterm.hash`, no random, no base64, no HMAC). We:

1. Generate a 256-bit secret **once at plugin load** via `wezterm.run_child_process({wezsesh_bin, 'keygen'})`. The `keygen` subcommand reads 32 raw bytes from Go's `crypto/rand` (which uses `getrandom(2)` / `/dev/urandom` under the hood) and prints them as **64 lowercase hex characters** (32 bytes × 2 hex chars each = 256 bits of entropy). This removes the openssl dependency entirely — the binary is already required, so the entropy source is uniform across machines and we control it. The key is cached in `wezterm.GLOBAL.wezsesh_session_key` and reused for every TUI spawn within the wezterm session, so the keygen fork-exec runs once, not per-keypress. Fallback chain at plugin load: `wezsesh keygen` → `/dev/urandom` via Lua `io.open` → **hard fail** (toast `wezsesh: HMAC key generation failed; not available this session`, keybinding stubs out, `log_error`). Never proceed with a weak/empty key.

   The blast-radius cost of session-stable vs. per-spawn keys is theoretical (an attacker that can read `/proc/<pid>/environ` is same-UID and already wins). The latency cost of regenerating per spawn is real (~10ms fork-exec on the keypress hot path), so we cache.
2. Pass it to the binary via env var `WEZSESH_HMAC_KEY`.
3. Vendor a pure-Lua SHA-256 (~200 LOC, MIT — e.g., [pure_lua_SHA](https://github.com/Egor-Skriptunoff/pure_lua_SHA)) plus a 30-line HMAC wrapper (standard ipad/opad). Performance: ~0.3-0.8ms per payload, negligible vs. the operations they trigger.
4. Vendor a 20-line pure-Lua base64 decoder.
5. Pin the vendored SHA implementation to a specific commit and audit it (~200 LOC is reviewable).

The `hmac` field in every payload is computed over **canonical JSON** per the rules in §6.3. Both Go and Lua sides MUST agree byte-for-byte; CI golden tests gate this.

**Key format and conversion** (load-bearing — silent mismatch fails every payload):
- `WEZSESH_HMAC_KEY` is **always 64 hex characters** (32 raw bytes, 256 bits).
- Both sides hex-decode to 32 raw bytes BEFORE feeding to HMAC. Passing the hex string directly to `hmac()` is wrong: `HMAC(hex_str)` and `HMAC(decoded_bytes)` produce different digests.
- Go: `key, _ := hex.DecodeString(os.Getenv("WEZSESH_HMAC_KEY")); h := hmac.New(sha256.New, key)`.
- Lua: `local key = sha.hex_to_bin(os.getenv("WEZSESH_HMAC_KEY")); local mac = sha.hmac(sha.sha256, key, payload)`.
- 32 bytes < SHA-256 block size (64 bytes); HMAC's "key longer than block" rehash path is not exercised. CI MUST run a fixture confirming both sides emit the same digest for the test vector below.

**Constant-time HMAC comparison** (Lua side):

`pure_lua_SHA` does NOT export a constant-time string compare. Lua's native `==` short-circuits on first byte mismatch and leaks timing information. Vendor a 6-line helper at `plugin/wezsesh/ct_eq.lua`:

```lua
-- Constant-time byte-string equality. Runtime is independent of where bytes
-- differ. Required by the HMAC verification step. Lua 5.3+ bitwise ops.
local M = {}
function M.eq(a, b)
    if #a ~= #b then return false end
    local d = 0
    for i = 1, #a do d = d | (a:byte(i) ~ b:byte(i)) end
    return d == 0
end
return M
```

The HMAC verifier in `plugin/wezsesh/ipc.lua` MUST use `ct_eq.eq(received_hmac, computed_hmac)`, never raw `==`. The practical timing-attack risk is negligible (an attacker who can measure Lua-VM timing can also read `WEZSESH_HMAC_KEY` from `/proc/<pid>/environ` directly), but constant-time compare is standard cryptographic hygiene and the cost is one loop over 64 hex chars per verification.

**Required CI fixture** (HMAC round-trip):

```
key_hex     = "a0b1c2d3e4f5a0b1c2d3e4f5a0b1c2d3e4f5a0b1c2d3e4f5a0b1c2d3e4f5a0b1"
canonical   = '{"args":{},"id":"01JABCDEFGHIJKLMNPQRSTUVWXY","op":"noop","reply_sock":"/tmp/x.sock","target_window_id":1,"ts":1700000000,"v":1}'
expected_hmac = (computed by RFC-4231-compliant HMAC-SHA-256, fixed string)
```

Both Go (`crypto/hmac`) and Lua (`pure_lua_SHA.hmac` + hex-decoded key) MUST produce the exact same hex digest for this input. CI fails the build on mismatch. This catches: key-format divergence, canonical-JSON divergence, and any future SHA library swap.

#### Layer 3: replay & freshness

Payloads include `ts` (Unix seconds) and `id` (ULID). The listener rejects:
- Payloads where `|now - ts| > 30` seconds (`STALE_PAYLOAD`).
- Payloads whose `id` is already in `wezterm.GLOBAL.wezsesh_state[tostring(pid)].seen_ids` (`REPLAY`). The `seen_ids` table uses ULIDs as string keys with `true` values (set pattern); GLOBAL preserves this round-trip across config reload.

The replay guard also collapses the wezterm broadcast bug ([#3524](https://github.com/wezterm/wezterm/issues/3524)) — the same payload arrives in N windows; only the first matching `target_window_id` window dispatches.

#### Env vars set at spawn

| Var | Value | Lifetime |
|---|---|---|
| `WEZSESH_HMAC_KEY` | hex 32 random bytes | generated once at plugin load (`wezsesh keygen`); cached in `wezterm.GLOBAL.wezsesh_session_key`; same value passed to every spawn this wezterm session |
| `WEZSESH_PROTO_VERSION` | `"1"` | per-launch |
| `WEZSESH_SNAPSHOT_DIR` | path | per-launch |
| `WEZSESH_STATE_DIR` | path | per-launch |
| `WEZSESH_RUNTIME_DIR` | path for reply sockets | per-launch |
| `WEZSESH_PLUGIN_VERSION` | semver string from `M.VERSION` | per-launch |

The binary's own pane ID is read from `WEZTERM_PANE` (injected automatically by wezterm into spawned panes), not passed explicitly — pre-spawn env can't include it because `mux.spawn_window` only returns the pane ID after spawn.

#### Failure mode

| Trigger | Action |
|---|---|
| Foreign pane | Silent drop + `wezterm.log_warn` (foreign-pane events are noisy in normal use; toast would spam) |
| HMAC mismatch | `wezterm.log_error` + `window:toast_notification("wezsesh", "Rejected unsigned operation", nil, 4000)` |
| Stale `ts` | `wezterm.log_warn` + reply with `STALE_PAYLOAD` |
| Replayed `id` | Silent drop (deduplication, not attack) |
| Unknown verb | Silent drop at `ipc.lua` step (e); `log_warn("ipc: no shape registered for op=…")`; no wire reply (binary hits `IPC_TIMEOUT`). See `docs/design.md` §13.13 / §0.1 row 35. |
| **Listener invoked with `wezterm.GLOBAL.wezsesh_session_key == nil`** (keygen failed at plugin load; see §7.1) | Silent `log_warn("wezsesh: ignoring op, HMAC key unavailable")` + drop. Never proceed to dispatch. |
| **Binary unexpected exit / panic** | Two-layer detection. **(1) Go-side panic-recover** — `cmd/wezsesh/main.go` installs a top-level `defer func() { if r := recover(); r != nil { writePanicReply(...); os.Exit(2) } }()` before `tea.Run`. The recovery handler writes a sentinel reply over the OSC channel (or to any open reply socket) with `status: "completed"`, `ok: false`, `error.code: "UNEXPECTED_EXIT"`. The Lua side does NOT need any pane-closed listener; the binary itself signals its own death before exiting. Out-of-recover crashes (SIGSEGV in cgo, SIGKILL, OOM-killer) cannot trigger this path — fall through to (2). **(2) IPC_TIMEOUT fallback** — after 5s with no reply, the TUI's parent surfaces the standard timeout error. The earlier PRD claim of a `wezterm.on('pane-closed', ...)` listener is **dropped**: source-code reading (wezterm-gui/src/termwindow/mod.rs match arms; wezterm.org/config/lua/window-events/) confirms `pane-closed` is NOT a Lua-surfaced event in wezterm — `MuxNotification::PaneRemoved` exists internally but is unhandled in the Lua emit path. The early-warning toast is therefore non-implementable as previously specced. |
| **Binary not on PATH at first run** (version handshake `run_child_process` returns `ok=false`) | Distinct toast: `wezsesh binary not found on PATH. Install: go install github.com/Grady-Saccullo/wezsesh/cmd/wezsesh@latest`. Keybinding stubs out. **Do not conflate with version mismatch** (which is `ok=true` + version regex fails `compatible()`). |
| **`apply_to_config` body raises a Lua error** (vendored module syntax error, bad option type, unexpected wezterm API return) | The `apply_to_config` body is wrapped in `pcall`; on catch, log error, return a no-op stub config (no keybindings, no listeners registered), toast at `gui-startup`. Never break the user's whole wezterm config. |
| **User `on_before_op` / `on_after_op` Lua hook raises** | `pcall`-wrapped at the dispatch boundary; `log_warn` with the error; continue to dispatch/reply. The user's buggy hook does not block ops. |
| **FS timeout / hang on snapshot dir read** (NFS, slow cloud-sync mount) | All Go FS ops in `internal/snapshots/` wrapped in `context.WithTimeout(5 * time.Second)`. On timeout: return partial list + non-fatal toast `wezsesh: snapshot dir read slow; some snapshots may be missing this session`. Don't block TUI startup indefinitely. |

**Never raise a Lua error in event handlers OR in `apply_to_config`'s top-level body.** Uncaught errors in `wezterm.on` handlers wedge the wezterm event loop; uncaught errors in `apply_to_config` abort the user's entire wezterm config eval. Both must be `pcall`-wrapped at the boundary.

#### Mandatory `user-var-changed` handler structure (added v2.0)

The Layer 1 / Layer 2 / Layer 3 defenses above describe the *what*. This subsection pins down the *order of operations* — getting it wrong invalidates the defenses. Every step that can raise a Lua error MUST be `pcall`-wrapped or preceded by a type/shape check that makes the raise impossible. The structure below is normative; deviations require updating this section.

```lua
wezterm.on('user-var-changed', function(window, pane, name, value)
    if name ~= 'wezsesh_op' then return end

    -- (a) Pane-ID check FIRST. Drops 99% of foreign-OSC traffic at near-zero cost.
    --     Without this ordering, an attacker firing 10000 OSCs/sec from a
    --     foreign pane would force base64-decode + JSON-parse + canonical-JSON
    --     + HMAC-compute on every event, DoSing the wezterm GUI thread.
    --     Pane ID is wezterm-context-supplied (not from the payload), so this
    --     check is independent of any payload parsing.
    local pane_id_key = tostring(pane:pane_id())
    local session = (wezterm.GLOBAL.wezsesh_state or {})[pane_id_key]
    if not session then return end

    -- (b) HMAC key availability check. Per §6.14 failure-mode matrix:
    --     keygen failure leaves wezsesh_session_key nil; we drop silently.
    local hmac_key = wezterm.GLOBAL.wezsesh_session_key
    if not hmac_key then
        wezterm.log_warn('wezsesh: ignoring op, HMAC key unavailable')
        return
    end

    -- (c) JSON parse with pcall. wezterm.json_parse RAISES a Lua error on
    --     malformed input (verified `lua-api-crates/json/src/lib.rs`); an
    --     uncaught raise here wedges the event loop.
    local ok, payload = pcall(wezterm.json_parse, value)
    if not ok or type(payload) ~= 'table' then
        wezterm.log_warn('wezsesh: malformed JSON in OSC payload')
        return
    end

    -- (d) Field-shape validator. Type + length checks for every required
    --     field. Runs BEFORE HMAC verify so that downstream code (canonical
    --     encode, ct_eq.eq) never sees nil/wrong-type values that would
    --     raise (e.g., `#nil` if hmac is missing). This is a *cheap* gate;
    --     it does not validate semantics, only structural well-formedness.
    if not (
        type(payload.v) == 'number' and payload.v == 1
        and type(payload.id) == 'string' and #payload.id == 26
        and type(payload.ts) == 'number'
        and type(payload.op) == 'string' and #payload.op > 0 and #payload.op <= 32
        and type(payload.args) == 'table'
        and type(payload.reply_sock) == 'string' and #payload.reply_sock > 0 and #payload.reply_sock <= 104
        and type(payload.target_window_id) == 'number'
        and type(payload.hmac) == 'string' and #payload.hmac == 64
    ) then
        wezterm.log_warn('wezsesh: payload field-shape validation failed')
        return
    end

    -- (e) Canonical-JSON re-serialize for HMAC verify. Tag-before-remove
    --     is load-bearing per `docs/design.md` §4.3 step 7:
    --     `ROOT_PAYLOAD_SHAPE` (§4.2) declares `hmac` as a required key, so
    --     tagging a payload from which `hmac` had already been dropped
    --     would raise `CANONICAL_SHAPE_MISMATCH`. Order: tag full payload
    --     (with `hmac` still present) → copy_without('hmac') → encode the
    --     copy. Both tag and encode can raise (tag on shape mismatch;
    --     encode on float subtype values per math.type, invalid UTF-8,
    --     untagged tables, or deep nesting); both are pcall-wrapped.
    local ok_tag = pcall(canonical_json.tag_in_place, payload,
                         canonical_json.ROOT_PAYLOAD_SHAPE,
                         canonical_json.verb_args_shape[payload.op])
    if not ok_tag then
        wezterm.log_error('wezsesh: canonical-JSON tag failed')
        return
    end
    local payload_minus_hmac = canonical_json.copy_without(payload, 'hmac')
    local ok2, canonical_bytes = pcall(canonical_json.encode, payload_minus_hmac)
    if not ok2 then
        wezterm.log_error('wezsesh: canonical-JSON encode failed: ' .. tostring(canonical_bytes))
        return
    end

    -- (f) HMAC verify with constant-time compare. ct_eq.eq is nil-safe by
    --     contract (the field-shape gate above guarantees both args are
    --     64-char hex strings).
    local computed = hmac.compute(canonical_bytes, hmac_key)  -- hex_to_bin's the key
    if not ct_eq.eq(payload.hmac, computed) then
        wezterm.log_error('wezsesh: HMAC mismatch')
        window:toast_notification('wezsesh', 'Rejected unsigned operation', nil, 4000)
        return
    end

    -- (g) Freshness, replay, target-window checks. All cheap; safe AFTER HMAC
    --     because by this point the payload is authenticated.
    if math.abs(os.time() - payload.ts) > 30 then
        result.reply_error(payload, 'STALE_PAYLOAD')
        return
    end
    if (session.seen_ids or {})[payload.id] then return end
    if payload.target_window_id ~= window:window_id() then return end

    -- (h) Replay-guard write-back + TTL prune. wezterm.GLOBAL is
    --     `Arc<Mutex<BTreeMap>>`; subtable mutation requires explicit
    --     `state.set_state` write-back to persist across reload.
    session.seen_ids = session.seen_ids or {}
    session.seen_ids[payload.id] = { ts = os.time() }
    state.prune_seen_ids(session, 60)  -- drop entries with ts < now - 60
    state.set_state(pane_id_key, session)

    -- (i) Dispatch with pcall. Any error in ops.* surfaces as a structured
    --     reply, not an event-handler crash.
    local ok3, err = pcall(ops.dispatch, payload, window, pane)
    if not ok3 then
        wezterm.log_error('wezsesh: dispatch failed: ' .. tostring(err))
        result.reply_error(payload, 'UNKNOWN', tostring(err))
    end
end)
```

**Why this exact order**:
- (a) cuts DoS amplification from foreign-pane OSC spam by 100–1000× (drop before parse).
- (c)/(e)/(i) every operation that can raise a Lua error is `pcall`-wrapped — required by the "never raise" rule.
- (d) before (e) — field-shape validation makes downstream operations nil-safe and type-safe; without it, `#nil` errors and type mismatches reach (e) and (f).
- (e)/(f) before (g) — freshness/replay should not be checked on unauthenticated payloads (an attacker who controls `ts` could force `STALE_PAYLOAD` to spam logs; we want HMAC to gate that).
- (g)/(h) before (i) — only well-formed, fresh, authenticated, deduplicated, window-scoped payloads reach dispatch.

**Concurrency assumption — handler steps (a)–(h) MUST be synchronous Lua bytecode** (added v2.1). `wezterm.GLOBAL` is `Arc<Mutex<BTreeMap<String, Value>>>` with **per-key** mutex granularity (verified `lua-api-crates/share-data/src/lib.rs`). Lua-side reads via `__index` create a deserialized snapshot Lua table — they are NOT shared references back into the Rust BTreeMap. Sub-table mutations are local until written back via `__newindex`. This means the seen_ids read-modify-write is NOT atomic across two `__index`/`__newindex` operations; if two handlers concurrently read the same `wezsesh_state[pid]` snapshot and both write back, **the second writer wins and the first writer's seen_ids entry is lost** — a silent replay-guard regression.

The race is closed only because **wezterm's Lua VM runs handlers cooperatively**: handlers are async tasks scheduled via `promise::spawn::spawn` (cited `wezterm-gui/src/termwindow/mod.rs` `emit_user_var_event`; round-9 cited line 2564–2608, round-10 re-validation found the function at ~line 2080 — line drift across versions; the function and behavior are unchanged), but they share a single-threaded Lua context that only yields at `.await` points. Within one handler invocation, all synchronous Lua code runs to completion before any other handler can interleave.

**Therefore: steps (a) through (h) MUST contain zero `.await` points.** Specifically NO calls to:
- `wezterm.run_child_process` (verified async, `lua-api-crates/spawn-funcs/src/lib.rs:30-50`).
- `wezterm.sleep_ms`.
- Any other wezterm API that mlua exposes via `add_async_function`.

`wezterm.background_child_process` is *fire-and-forget* (returns immediately without yielding the caller's future); calling it from step (i) is fine, AFTER the seen_ids write-back at (h). Calling it from (a)–(h) would open the race.

`wezterm.json_parse`, `wezterm.log_warn`, `wezterm.log_error`, `os.time`, `math.abs`, `pcall`, all pure Lua: synchronous, no yields. The handler as specified above is correct.

**CI lint** (added v2.1): a unit test parses `plugin/wezsesh/ipc.lua`, walks the `wezterm.on('user-var-changed', ...)` callback's AST, and fails if any call site between (a) and (h) names a known-async wezterm function. The list is enumerated in `internal/lualint/async_funcs.go` with citations to `lua-api-crates/spawn-funcs/`, `lua-api-crates/mux/` etc. for each.

**Fallback if the assumption is ever violated**: even with the seen_ids race opening, the 30s freshness window + per-payload HMAC keep replay attacks bounded — the practical attack surface becomes "an attacker who can replay a captured payload within 30s," which is mitigated externally (HMAC binds the payload, freshness binds the window). Document `seen_ids` as defense-in-depth, not the primary replay defense. The primary defense is HMAC + 30s freshness; `seen_ids` collapses duplicate dispatches from wezterm's broadcast bug (#3524).

**Fuzz-testing requirement** (§8.1): the CI fuzz test fires random/mutated bytes at the `user-var-changed` handler and asserts (i) no Lua error escapes the handler boundary, (ii) no `ops.dispatch` invocation occurs for non-authenticated payloads, (iii) the wezterm process remains responsive after 10000 invalid payloads.

#### Residual risks (documented honestly)

1. **Env-var leak via `/proc/<pid>/environ`**: any process running as the same UID can read the HMAC key. Standard Unix env-var caveat; we accept it.
2. **In-pane attacker**: a process running with stdout pointing at the wezsesh TUI pane could emit signed-looking traffic if it can read `WEZSESH_HMAC_KEY`. The wezsesh pane runs only the Go binary — no shell, no `cat`, no `npm`. If we ever add a "shell out from TUI" feature, we must rotate the key or scrub the env first.
3. **Wezterm OSC parser vulnerabilities**: out of our trust boundary. If wezterm's parser is broken we have bigger problems.
4. **Clock skew between plugin and binary**: same host, fine. Documented as an assumption.
5. **Vendored pure-Lua SHA**: supply chain. Pin commit, check in repo, audit.
6. **Reuse after wezterm restart**: pane IDs reset on wezterm restart. Each launch records its own `spawned_pane_id`, so this is fine.

### 6.15 Reload durability — `wezterm.GLOBAL` + single-retry resend

Wezterm reloads the user's config when `wezterm.lua` is saved. Per [wezterm.on docs](https://wezterm.org/config/lua/wezterm/on.html): "the Lua state is built from scratch when the configuration is reloaded… reloading the configuration will clear any existing event handlers." Handlers are cleared and re-registered. **`wezterm.GLOBAL`** survives the reload (verified: it's a `lazy_static!` over `Arc<Mutex<BTreeMap>>` in `share-data/src/lib.rs`; accepts nil/bool/integer/number/string/array tables/object tables — JSON-shaped only).

**Reload-time event delivery**: per source-code reading of `wezterm-gui/src/termwindow/mod.rs` (`emit_user_var_event`, ~line 2080 in current main; v9 PRD cited 2564–2608, but the function moved across wezterm versions — line citations to wezterm should be treated as approximate-and-drifty, function names as authoritative) and `config/src/lib.rs:175-220` (`LuaPipe`/`update_to_latest`), `user-var-changed` events are NOT dropped during config reload. Each dispatch holds an `Rc` to the Lua VM that was current when the dispatch began; the new VM is queued in `LUA_PIPE` and adopted on the next dispatch. Old handlers run to completion against the old VM. A previous version of this PRD claimed events were dropped during reload — not supported by source. The single-retry-at-2s in §6.2 is defensive against unrelated transport hiccups (truncated OSC writes, signal-interrupted syscalls), not reload drops.

#### State maps

```
wezterm.GLOBAL.wezsesh_state[tostring(spawned_pane_id)]   -- key MUST be string;
                                                           -- GLOBAL Object nodes reject
                                                           -- integer keys (verified in
                                                           -- share-data/src/lib.rs)
                                                           -- value: { hmac_key,
                                                           --          target_window_id,
                                                           --          seen_ids,
                                                           --          spawned_at }
wezterm.GLOBAL.wezsesh_requests[request_id]               -- ULID is already a string
                                                           -- value: { spawned_pane_id,
                                                           --          started_at }
wezterm.GLOBAL.wezsesh_writing[file_path]                 -- string path, bool value
wezterm.GLOBAL.wezsesh_session_key                        -- hex string (HMAC key)
```

#### Rules

1. **All in-flight state lives in `wezterm.GLOBAL`.** Module-local Lua tables get blown away on reload.
2. **Handlers are idempotent**: re-registering on reload is harmless because old handlers are GC'd.
3. **Single-retry resend**: if no reply within 2s, the binary retransmits the OSC once. Hard 5s ceiling for the **first** reply (`"completed"`, `"started"`, or error), after which `IPC_TIMEOUT` surfaces in the TUI. After a `"started"` reply, an additional 30s ceiling applies for `"completed"`/`"partial"` follow-ups (covers worst-case restore latency; see §6.2 step 10, §6.4 reply listener). Defense against transport-layer hiccups (truncated OSC writes, signal-interrupted syscalls), not reload drops. The replay guard (§6.14 — `seen_ids` keyed by ULID) ensures the duplicate doesn't double-dispatch when both copies arrive.
4. **TTL sweep**: in `wezterm.on('window-config-reloaded', ...)` and at the end of each dispatch, prune (a) `wezsesh_state[pid_key]` and `wezsesh_requests[id]` entries older than 60 seconds, and (b) **`seen_ids` entries inside each `wezsesh_state[pid_key]`** older than 60 seconds (sub-pruning by storing each ULID as a `{ ts = os.time() }` value rather than `true`, then dropping entries whose `ts < now - 60`). Without (b), a long-lived TUI session accumulates ULIDs forever — a 30s freshness window means at most ~1 entry per second, but ULIDs are 26 bytes plus table overhead and a 24-hour session leaks ~2 MB per pid_key. Bounding to a 60s rolling window keeps the set under 100 entries per session.

#### Switching workspaces — Go-side polling (primary, no event hook)

A previous PRD revision proposed subscribing to `smart_workspace_switcher.workspace_switcher.{chosen,created,selected}` as the primary post-`SwitchToWorkspace` completion signal. Source-code reading of the plugin (`MLFlexer/smart_workspace_switcher.wezterm`, recent main) showed the events are emitted **only from inside the plugin's own picker callbacks** (`pub.switch_workspace`, `pub.switch_to_prev_workspace`). They do NOT fire from programmatic `act.SwitchToWorkspace` calls — and wezsesh always invokes the action programmatically. **The primary path was architecturally wrong; dropped.**

Wezterm has no built-in `workspace-switched` event either. The only general-purpose post-switch detection mechanism is observing mux state.

Adopted strategy (single path):

1. **Pre-switch state capture.** Before emitting the switch OSC, the binary records the current active workspace by calling `wezterm cli list-clients --format json` and extracting the focused client's `workspace` field, plus its `target_window_id` (already known from §6.5) AND its `client_id` (used for client-pinning per §6.5 / §6.13 v2.4). This pre-state is stored locally; it is NOT sent over IPC. Captured fields:
   ```go
   preSwitch := SwitchPreState{
       ActiveWorkspace: focusedClient.Workspace,
       TargetWindowID:  targetWindowID,
       TargetClientID:  focusedClient.ClientID,  // pinned for the duration
                                                 //   of polling (v2.4)
   }
   ```
   **Client pinning rationale (added v2.4)**: `TargetClientID` MUST be captured at Phase 1 start and held constant for every subsequent polling tick. Re-evaluating §6.5's "most-recent `last_input`" rule per tick is incorrect: a second connected GUI client (mosh, SSH-mux, another local wezterm session) becoming "most recent" mid-poll would flip the predicate to track THAT client's workspace, which is unrelated to the user's `wezsesh find` invocation. Pinning to the originating `client_id` ensures the predicate observes the right transition. If the pinned client disconnects mid-poll (the `client_id` no longer appears in `cli list-clients`), the predicate fails closed — `MUX_UNREACHABLE` at the 5s ceiling, which surfaces correctly.
2. Lua dispatches `act.SwitchToWorkspace { name }` via `window:perform_action(...)` and **immediately replies `{"status": "started"}`** through the reply socket (matches the unified status taxonomy in §6.3). This closes the first reply within milliseconds; the TUI dismisses without waiting for the actual switch to complete.
3. **Go binary polls** via `StartSwitchPoller(ctx, target, preSwitch, 50*time.Millisecond)` where `ctx` is `context.WithTimeout(parent, 5*time.Second)`. The poller exits on `<-ctx.Done()` at any iteration boundary. Caller cancels `ctx` on any terminal reply OR on `tea.Run()` return (via `defer cancel()`), so the poller does NOT leak when the workspace appears at 60ms. Without context cancellation the poller would tick 50ms intervals until the 5s ceiling. Goroutine has top-level `defer recover()` (CLI JSON unmarshal could panic on malformed output).

   **Two-step query (clarified v2.3 — round-12 audit; client-pinned v2.4 — round-13 audit)**: `wezterm cli list --format json` does NOT expose a top-level "currently active workspace" field. Verified against [`wezterm/src/cli/list.rs::CliListResultItem`](https://raw.githubusercontent.com/wezterm/wezterm/main/wezterm/src/cli/list.rs): each pane entry has a per-pane `workspace` field and a per-tab `is_active` bool, but no global active-workspace marker. To evaluate the poller's predicate, each tick MUST issue BOTH:
   - `wezterm cli list-clients --format json` → look up the entry whose `client_id == preSwitch.TargetClientID` (the client pinned at Phase 1 start; see step 1 v2.4 client-pinning rationale). Read its `focused_pane_id`. If the pinned client is absent from the list (disconnected), the predicate fails for this tick — the poller continues until either the client returns or the 5s ceiling fires (whichever first).
   - `wezterm cli list --format json` → look up that `pane_id`. Read its `workspace` field AND its `window_id` field. The success predicate (step 4) requires BOTH the workspace match AND `window_id == preSwitch.TargetWindowID` — the latter guard prevents a false-positive when the pinned client's focused pane has rolled over to a different window mid-poll (e.g., the user closed the originating wezterm window).

   Cadence remains 50ms but each tick now issues two CLI invocations (~30ms total fork-exec on a healthy mux). Acceptable; documented in §6.20 polling load.

4. **Poller success predicate** (defends against false-positive when the user is already in the target workspace):
   ```
   succeeded = (workspace `target` is now the active workspace of the focused client
                in `target_window_id` — derived from cli list-clients →
                cli list cross-reference per step 3)
            AND ((target != preSwitch.ActiveWorkspace) OR isRestoreFlow)
   ```
   The second clause handles two distinct cases:
   - **Pure `switch` to an already-active workspace**: predicate (b) is false (target == pre-active); poller short-circuits to "no-op success" without waiting. The binary exits cleanly. Avoids spuriously polling for 5s when the action was already a no-op.
   - **`switch+restore` to an already-active workspace** (the user explicitly chose load-into-current): `isRestoreFlow=true` is set by the binary based on the verb that triggered the poll. Predicate becomes "workspace appears in target window" (which is trivially true), and the binary proceeds to emit the follow-up `restore` op even though the workspace was already active.
   The window scope check (workspace appears in target_window_id specifically, not any window) prevents cross-window false-positives where the workspace exists in window B and we're in window A.
5. Once the workspace appears AND the predicate fires, the binary either: (a) for a pure `switch` op, exits cleanly (workspace already activated server-side), or (b) for a `switch+restore` (existing-saved snapshot the user wants to load), emits a follow-up `restore` op via a second OSC sequence.
6. If 5s elapses with no positive predicate, surface `MUX_UNREACHABLE` in the TUI and exit.

Polling on the Go side avoids blocking the Lua event loop with `wezterm.run_child_process` (which suspends the config-eval coroutine, awkward here) and removes any third-party plugin dependency.

#### `SwitchToWorkspace.spawn` semantics

Critical: `act.SwitchToWorkspace { name, spawn }` only runs the `spawn` block when the workspace doesn't yet exist (`mux.iter_windows_in_workspace(&name).is_empty()`). If we're switching to an existing workspace and want to spawn into it (e.g., for the new-from-CWD flow), we must use `wezterm.mux.spawn_window { workspace = name, cwd = ... }` directly.

#### Pane argument for `perform_action`

When dispatching `window:perform_action(act.SwitchToWorkspace{...}, ?)`, the `pane` argument should be `window:active_pane()` — NOT the source (wezsesh) pane from `user-var-changed`, which lives in a possibly different window and may behave unexpectedly.

### 6.16 Quitting the TUI mid-operation

Once the binary dispatches an op via OSC and is waiting on the reply socket, the user may press `q` or `Esc`. Lua-side ops cannot be cancelled (no rollback for restore, file-rename is non-reversible at the FS level, etc.). Three patterns considered:

| Pattern | How comparable TUIs behave | Verdict for wezsesh |
|---|---|---|
| **A. Eager quit** (instant) | fzf, sesh, gum, atuin — but all are read-only selection tools | Wrong here; our ops mutate state |
| **B. Block quit** (wait for ack) | None | Hostile UX if socket never replies |
| **C. Inline-prompt quit** | lazygit (for self-update only) — exactly the case where silent completion creates inconsistent state | **Adopted** |

**Behavior**: the TUI tracks an `op_in_flight bool` flag set on dispatch and cleared on reply or timeout.

- **`q` / `Esc` while idle** (no in-flight op): immediate quit. `defer os.Remove(sock)` cleans any sock files.
- **`q` / `Esc` while op_in_flight**: render a one-line inline status (NOT a modal — follows lazygit's status-bar pattern) — `op in progress, quit anyway? [y/N]`. `y` → quit immediately, reply dropped, Lua-side op continues to completion silently. `n` or any other key → dismiss the prompt, stay in TUI, wait for the reply.
- After quit during in-flight op: the orphaned reply socket is reaped by the next launch's startup sweep (§6.4 crash cleanup, mtime > 60s). Lua-side op completes, writes to socket, gets `ECONNREFUSED` or write failure — handled silently in `wezsesh reply`.

Most ops are sub-100ms, so the prompt is rare in practice. With the split-reply pattern (§6.2 step 9), the TUI dismisses on `"started"` for restore-class ops, so the prompt fires only during the brief first-reply window — not for the full restore duration. Update §5.3 keybindings accordingly:

> `q` / `Esc` — Quit (or `op in progress, quit anyway? [y/N]` if a write op is in flight)

### 6.17 Workspace name validation (`ILLEGAL_NAME`)

Workspace names enter the system from user input (rename modal, save modal, `n` path picker), from disk (snapshot filenames), and from wezterm mux state. They become filesystem paths via resurrect's encoding `name:gsub("/", "+")` (`plugin/resurrect/utils.lua`). The PRD's `ILLEGAL_NAME` error code is the explicit reject path.

**Validation rules** (applied client-side in the Go binary before any IPC, FS, or mux call; surfaces `ILLEGAL_NAME` with the specific reason in `details`):

| Rule | Reason |
|---|---|
| Length: 1 ≤ `len(name_utf8_bytes)` ≤ 200 | Filesystems cap path components at 255 bytes; `.wezsesh.json` extension consumes 13; resurrect's `+` encoding can grow length by `n` bytes for `n` slashes. 200 leaves safe headroom. |
| Bytes: no `\0` (NUL), no `\n`, no `\r`, no `\t` (TAB), no other C0 control chars (0x01–0x1F) | NUL injection at `os.Create` boundaries, trust-store hash collision, picker render corruption, payload-size blowup (each C0 byte becomes 6 canonical bytes per §6.3 `\u00XX`). Earlier PRD revision exempted `\t` from the C0 rule; that exemption is REMOVED in v2.0 because TAB inside a workspace name has no legitimate use and pathological inputs (a user paste of indentation-laden text) would inflate canonical payloads. **Wezterm's OSC parser silently swallows NULs (verified `vtparse/src/transitions.rs:250`); we MUST validate client-side, not rely on wezterm.** |
| No U+007F (DEL); no U+0080–U+009F (C1 controls) when present as valid UTF-8 | Same payload-size blowup concern as C0. Rendering corruption (DEL behaves erratically across terminals; C1 maps to legacy ECMA-48 controls in some 8-bit modes). Each escapes to 6 canonical bytes per §6.3. |
| No U+2028 (LINE SEPARATOR) or U+2029 (PARAGRAPH SEPARATOR) | These are 3-byte UTF-8 sequences that escape to 6 canonical bytes per §6.3 (canonical encoder forces escape for JS-safety even when raw is valid JSON). 200-byte cap of LS/PS = ~67 chars × 6 bytes = 402 canonical bytes per name; a `delete` with worst-case names would otherwise blow past the 4 KB ceiling. Forbidding them at the validator costs nothing (no legitimate workspace name uses Unicode line separators) and bounds canonical expansion. |
| No leading/trailing whitespace; no all-whitespace names | UX hygiene; trims the rare hand-typed name with stray space. |
| Not exactly `"."` or `".."` | Filesystem-special. |
| No `\` (backslash) | Reserved for future Windows port; keeps name space portable. |
| Names containing literal `+` are accepted but flagged: warn the user via toast `"Names containing '+' may collide with names containing '/' due to resurrect's filename encoding. Recommended to avoid."` (don't reject — many legitimate paths contain `+`, e.g., `c++` projects.) | Resurrect's `name:gsub("/", "+")` is one-way: both `a/b` and `a+b` encode to `a+b.json`. Round-trip decode of `a+b` yields `a/b`. We inherit this and surface it. |
| Unicode NFC-normalize names before comparing or storing | Cross-platform portability. macOS APFS NFD-normalizes filenames at the VFS layer; Linux ext4 does not. A workspace `~/code/café` saved on macOS and listed on Linux must match. Apply `golang.org/x/text/unicode/norm.NFC.String(name)` at every ingestion point (rename, save, `n`, picker filter, sidecar key). |

The trust-store hash (§6.11) uses length-prefixed concatenation specifically because workspace names are user-controlled and become sidecar paths; a name with embedded `\n` would otherwise allow trust forgery. The validation above complements that defense by rejecting most pathological names at the boundary.

**Doctor check**: `wezsesh doctor` scans the snapshot dir for filenames whose decoded names fail validation; reports each as a warning so users can rename or delete.

**Render-time sanitization** (added v1.8). The validation rules above gate names *entering* the system. But existing on-disk snapshot filenames can already contain malicious bytes — a snapshot file placed by a same-UID attacker, or shared via dotfiles. When such a name is rendered into the picker, an unfiltered lipgloss render of (e.g.) `\x1b[2J` would clear the user's terminal; `\x1b[H\x1b[K` would corrupt the wezsesh TUI's cursor position. lipgloss does NOT sanitize input.

Rule: **every string read from disk that flows into a render call (lipgloss, fmt to stderr, log lines, toast text) is first sanitized**. Helper `internal/nameval.SanitizeForDisplay(s) string` replaces every byte in 0x00–0x1F (except `\t`) and 0x7F with the Unicode replacement character `U+FFFD` (or a visible placeholder like `␀`). C1 controls (0x80–0x9F as raw single bytes — invalid UTF-8 — should already be rejected by `utf8.ValidString` upstream, but the sanitizer catches valid UTF-8 representations of `U+0080`–`U+009F` too).

Apply at: picker row render, preview pane render, modal labels, toast messages, log lines that include user-controlled strings, doctor output. The validation rules at §6.17 reject these on *write*; the sanitizer renders safely on *read*.

**Tag-string validation** (added v2.0). Tag strings are stored in the sidecar's `tags` array (§6.6) and travel through the `tag` IPC verb. They were previously unvalidated, opening the same control-byte payload-blowup concern as workspace names plus a render-corruption surface. Rules:

| Rule | Reason |
|---|---|
| Tag count: 1 ≤ N ≤ 10 per workspace | Bounds the `tag` IPC payload size; 10 × 50 + envelope still well under 4 KB. |
| Tag bytes: 1 ≤ `len(tag_utf8_bytes)` ≤ 50 | Tags are UI labels, not free text. 50 bytes covers `production-v2-1-4-rc1` and similar. |
| Tag content: same byte rules as workspace names — no NUL, no C0 (incl. TAB), no DEL, no C1, no LS/PS, no leading/trailing whitespace | Identical reasoning: payload-size, render corruption. |
| Tag NFC-normalize before comparing or storing | Same cross-platform portability rationale as names. |

Validation runs client-side in the Go binary at the `tag` verb's request-construction boundary. The Lua-side `tag` handler trusts the binary and writes the array verbatim to the sidecar. Failed validation surfaces `ILLEGAL_NAME` with `details.field = "tags[i]"` indicating the offending entry.

### 6.18 Snapshot argv trust (RCE defense)

§6.11 hook trust covers the wezsesh sidecar's `on_create` / `on_restore` shell commands. **A separate, orthogonal vector exists in resurrect snapshots themselves**: `pane_tree.process.argv`. When restoring a pane with `alt_screen_active = true`, resurrect calls `pane:send_text(wezterm.shell_join_args(pane_tree.process.argv) .. "\r\n")` (verified at `plugin/resurrect/tab_state.lua:127`). This injects the saved command into the shell's stdin — equivalent to the user typing it. There is no validation, no allowlist, no trust check anywhere in resurrect's path.

**Threat surfaces**:
- Dotfiles sync of resurrect's snapshot directory.
- Same-UID trap: a less-privileged process under the user's UID (npm postinstall, downloaded binary, MCP server) writes a malicious snapshot to `~/.local/share/wezterm/state/resurrect/workspace/`.
- Shared / multi-user machine where snapshot dir permissions are loose.
- Snapshot received from another user (intentional sharing, ad-hoc copy).

§6.11 hash-based trust is the right model for `on_*` hooks (rare, user-authored, one-time approval). It is the **wrong** model for `process.argv` (every pane of every restore — per-pane prompts are an unusable UX).

**Adopted mitigation: tmux-resurrect-style allowlist** via a custom `on_pane_restore` callback wezsesh installs over resurrect's default. Pattern follows tmux-resurrect's `@resurrect-processes` config.

**Default allowlist** (basename of the program; see indexing note below):
```
$SHELL (resolved at install time), sh, bash, zsh, fish, dash, ksh,
nvim, vim, vi, emacs, nano, helix, hx,
less, more, man, info,
git, jj, lazygit, tig,
python, python3, ipython, node, ruby, irb, lua,
htop, btop, top, k9s, lazydocker
```

**Callback signature** (corrected v2.2; verified against [resurrect tab_state.lua](https://raw.githubusercontent.com/MLFlexer/resurrect.wezterm/main/plugin/resurrect/tab_state.lua)). Resurrect's `on_pane_restore` takes a SINGLE argument:

```lua
function pub.default_on_pane_restore(pane_tree)
  local pane = pane_tree.pane
  -- ...
end
```

The pane is accessed as `pane_tree.pane`, NOT received as a separate parameter. wezsesh's custom hook MUST match this signature; an `on_pane_restore(pane, pane_tree)` two-argument hook will be invoked with one argument and crash on first restore. Earlier PRD revisions (v1.7–v2.1) incorrectly described the signature as `(pane, pane_tree)` — corrected v2.2. The argv-allowlist defense fails-open if the callback errors, so this signature mismatch is a security regression risk.

**Argv indexing note** (clarified v2.2). Lua arrays are 1-based and resurrect's `pane_tree.process.argv` follows that convention: **`argv[1]` is the program name** (e.g., `"/bin/bash"` or `"bash"`), `argv[2..]` are the program's arguments. This is opposite to C-style argv where `argv[0]` is the program. Earlier PRD prose interchangeably said "argv[0]" and "argv[1]"; v2.2 normalizes to `argv[1]` everywhere, matching what the Lua code actually executes.

**Behavior**:
1. wezsesh's `on_pane_restore(pane_tree)` callback derives `pane = pane_tree.pane` and checks `basename(pane_tree.process.argv[1])` against the allowlist.
2. **Match**: also validate every argv element AND `cwd` for control characters (see "control-char defense" below). If any element is dirty, fall through to the No-match path with a different reason in the log. Otherwise call through to resurrect's `default_on_pane_restore(pane_tree)` (passing the same single argument).
3. **No match**: try the cwd-only fallback. If `cwd` passes the control-char check, `pane:send_text("cd " .. wezterm.shell_quote_arg(cwd) .. "\r\n")`. If `cwd` fails the check (or is empty/missing), `pane:send_text("\r\n")` only — the user lands at the shell's default cwd. Log WARN in both cases: `wezsesh: skipped argv restore for "<argv[1]>"; cwd <clean|dirty>`.
4. **Empty argv / no `process` field**: no-op (resurrect's default also no-ops).
5. **Hook crashes**: fail-CLOSED. If `on_pane_restore` raises a Lua error, restore-into-shell is preferable to silently invoking resurrect's default (which would re-introduce the RCE surface). Wezsesh's hook body is `pcall`-wrapped at the outer boundary; on caught error, log WARN, send `pane:send_text("\r\n")` only, and proceed. CI assertion: a hook that raises must NOT result in `pane:send_text(shell_join_args(argv))` being executed.

**Critical: control-char defense for `pane:send_text`** (added v1.8). `wezterm.shell_quote_arg` delegates to Rust's `shlex::try_quote` (`config/src/lua.rs:414-422` in wezterm; `shlex` v1.3.0). `shlex` errors **only** on NUL bytes — it accepts `\n` (LF) and `\r` (CR) inside the quoted string, including them as literal bytes. `pane:send_text` writes raw bytes directly to the pane's PTY (`lua-api-crates/mux/src/pane.rs:118-124` — bare `pane.writer().write_all(text.as_bytes())`), so embedded `\n`/`\r` are seen by the shell as line terminators. Result: a malicious `cwd = "/tmp/foo\nrm -rf ~"` under naive `shell_quote_arg(cwd)` becomes:

```
cd '/tmp/foo
rm -rf ~'
```

The shell parses three commands: `cd '/tmp/foo` (incomplete), then `rm -rf ~` (executed), then `/tmp'` (syntax error). **Command injection.** The same hazard applies inside any allowed `argv` element.

Mitigation (Lua-side, in the `on_pane_restore` callback before any `send_text`):

```lua
-- Reject any byte in 0x00-0x1F or 0x7F. This blocks NUL, \n, \r, and all
-- C0 controls + DEL. Printable Unicode (including paths with spaces, $, `,
-- and other shell metacharacters) is allowed; shell_quote_arg handles those.
local function bytes_clean(s)
  return type(s) == "string" and #s > 0 and not s:find("[%z\1-\31\127]")
end
```

Apply `bytes_clean` to: every element of `pane_tree.process.argv` (when matching the allowlist), and `pane_tree.cwd` (in both match and no-match branches). On any failure, downgrade per the rules above.

**`wezsesh doctor` checks** (extended): list snapshots whose `pane_tree.cwd` or any `pane_tree.process.argv[*]` element contains control characters; these would trigger the dirty-input downgrade at restore time. Combined with the existing argv-allowlist audit, the user gets a single command to audit every snapshot's restore-time risk surface.

**User extension** via the Lua config:
```lua
wezsesh.apply_to_config(config, {
    resurrect_argv_allowlist = { "make", "cargo", "go", "yarn", "pnpm" },
    -- additions to default; user cannot disable the default list (which is conservative)
})
```

**Per-snapshot opt-out** is NOT offered. The threat model includes "I'm restoring a snapshot I don't fully trust"; if the user wants permissive restore for one workspace, they extend the global allowlist. A per-snapshot trust bit complicates the model without meaningful benefit.

**`wezsesh doctor` check**: scan all snapshots for `pane_tree.process.argv[1]` values not in the active allowlist; report a count + list of unique non-matching `argv[1]` per snapshot. Helps the user audit what would currently be skipped.

This defense is independent of and stacks with §6.11 (hook trust) — a snapshot can have a malicious argv AND a malicious sidecar `on_restore`; both must be defended.

### 6.19 Filesystem safety (symlink + TOCTOU defense)

The wezsesh binary writes to several user-owned paths: `state.json`, `*.wezsesh.json` sidecars, `<sha256>` trust files, reply socket (`<8-hex>.sock`), and the rotated log. A same-UID attacker (npm postinstall, MCP server, downloaded binary) cannot read root-owned secrets but CAN manipulate paths under the user's home and `/tmp` to redirect wezsesh's writes — the classic symlink-hijack pattern.

**Required helper: `internal/safefs/`**:

```go
// AtomicWriteFile writes data to <parentDir>/<filename> atomically and safely.
// Uses openat(2) with O_NOFOLLOW on the parent dir AND the temp file; uses
// renameat(2) for atomic replacement. parentDir must already exist and must
// not be a symlink. All Go file writes in internal/snapshots/, internal/state/,
// and internal/trust/ MUST go through this helper. Direct os.WriteFile or
// os.OpenFile in those packages is a lint error.
func AtomicWriteFile(parentDir, filename string, data []byte, perm fs.FileMode) error

// VerifyDir opens parentDir with O_DIRECTORY|O_NOFOLLOW and returns the fd
// plus a stat. Errors if the path is a symlink or not a directory. Caller
// closes the returned fd.
func VerifyDir(parentDir string) (fd int, info fs.FileInfo, err error)

// SafeOpenForRead opens a file for read with O_NOFOLLOW. Errors with ELOOP
// if the path's final component is a symlink.
func SafeOpenForRead(path string) (*os.File, error)

// SafeRemove removes a file after `os.Lstat`-checking it is not a symlink.
// Used by `wezsesh nuke --yes`. Returns ErrIsSymlink if the path is a
// symlink; the caller logs and skips. Used in preference to `os.Remove`
// for any path under user control.
func SafeRemove(path string) error

// SafeRemoveAll recursively removes a directory tree, but at every step
// rejects symlinks via `Lstat` before descending or unlinking. Refuses to
// follow a symlinked directory (which would otherwise cause `os.RemoveAll`
// to delete the symlink target's contents). Used by `wezsesh nuke --yes`
// for the state dir, trust store, reply sock dir, log dir.
func SafeRemoveAll(path string) error

// AcquireExclusive opens path with O_RDWR|O_NOFOLLOW (via openat from a
// VerifyDir'd parent dirfd, since O_NOFOLLOW only protects the final path
// component) and acquires a POSIX advisory lock by polling fcntl F_SETLK
// (NON-blocking) with backoff until ctx is Done. F_SETLKW is NOT used: it
// blocks indefinitely and on darwin auto-restarts on SA_RESTART signals
// (verified man 2 fcntl), making any "watchdog signal" pattern unimplementable.
// Logs WARN at 1s and 3s of contended waiting (POSIX advisory locks have no
// fairness guarantee). Returns the fd and a release closure. Used by §6.7
// save / sidecar write serialization. On ctx.Done(): returns ErrLockTimeout.
//
// CRITICAL fd-hygiene contract (added v2.2; build-tag + O_CLOEXEC enforcement
// added v2.3): POSIX advisory-lock semantics release ALL locks the process
// holds on a file when the process closes ANY fd referring to that file
// (verified fcntl(2) Linux + macOS man pages; "If a process closes any file
// descriptor referring to a file, then all of the process's locks on that file
// are released, regardless of the file descriptor(s) on which the locks were
// obtained"). This means: while AcquireExclusive's fd is live, NO other code
// in the same process may open() the same path/inode. Doing so and then
// closing that second fd silently drops the lock without notification.
// Enforcement:
//
//   1. The release closure is the ONLY way to drop the lock (callers must
//      not close the returned fd directly).
//   2. Lint rule: while internal/safefs/lock_*.go's fd is live, callers
//      MUST NOT call os.Open / unix.Openat / os.OpenFile on the same path.
//      Read operations during the lock hold use the locked fd (pread / Read+Seek).
//
//   3. **Build-tag split (added v2.3)**: On Linux, AcquireExclusive uses
//      F_OFD_SETLK (Open File Description locks; Linux ≥ 3.15) — these bind
//      to the fd, not the process, and survive close-of-other-fd. The
//      F_OFD_SETLK constant is defined ONLY in
//      `golang.org/x/sys/unix/zerrors_linux.go`; it is NOT defined in
//      `zerrors_darwin_*.go` (verified). Code that references
//      `unix.F_OFD_SETLK` directly will FAIL TO COMPILE under GOOS=darwin
//      regardless of `runtime.GOOS == "linux"` runtime branching — the
//      compiler resolves all constants at compile time.
//
//      Mandatory implementation pattern:
//          // internal/safefs/lock_linux.go
//          //go:build linux
//          package safefs
//          // Uses unix.F_OFD_SETLK directly.
//
//          // internal/safefs/lock_other.go
//          //go:build !linux
//          package safefs
//          // Falls back to unix.F_SETLK with the multi-fd discipline above
//          // as the only defense.
//
//      CI lint rule: any reference to `unix.F_OFD_SETLK` outside
//      `internal/safefs/lock_linux.go` is a build error. Any
//      `runtime.GOOS == "linux"` branch that conditionally chooses
//      F_OFD_SETLK vs F_SETLK in a single .go file is forbidden — split
//      into build-tagged files instead.
//
//      **Non-Linux platform coverage (clarified v2.4)**: the `//go:build !linux`
//      tag matches darwin (amd64/arm64), FreeBSD, OpenBSD, and NetBSD.
//      Verified against `golang.org/x/sys/unix/zerrors_*.go`:
//      - `unix.F_SETLK` is defined on darwin, FreeBSD, OpenBSD, NetBSD —
//        the F_SETLK fallback compiles and runs cleanly on each.
//      - `unix.F_OFD_SETLK` is defined ONLY in `zerrors_linux.go`. No BSD
//        variant has it. The lock_linux.go split is sufficient.
//      v0.1 supported targets are darwin (Apple Silicon + Intel) and Linux
//      (amd64 + arm64); FreeBSD/OpenBSD/NetBSD are NOT a v0.1 commitment but
//      will compile under the same build-tag plan if a future port targets
//      them. CI builds run on linux-amd64, linux-arm64, darwin-amd64,
//      darwin-arm64; FreeBSD/OpenBSD/NetBSD are best-effort.
//
//   4. **`O_CLOEXEC` mandatory (added v2.3; defense-in-depth rationale
//      clarified v2.4)**: Every `unix.Openat` call in `internal/safefs/`
//      MUST include `unix.O_CLOEXEC` in the flag set. Required flag set:
//          unix.O_RDWR | unix.O_NOFOLLOW | unix.O_CLOEXEC
//
//      **Why this is not redundant with Go's os/exec default** (clarified
//      v2.4 after round-13 verification): Go's `os/exec.Cmd.Start()` runtime
//      DOES mark all inherited fds close-on-exec via the "mark all, unmark
//      intentional" strategy in `src/syscall/exec_unix.go` since 1.10+ —
//      verified by reading the Go runtime source. So for the os/exec.Start
//      path specifically, our O_CLOEXEC mandate is superseded by Go's
//      default. The mandate remains as defense-in-depth for THREE reasons:
//        (a) Direct syscall escape paths: any future code using
//            `syscall.ForkExec` or raw `fork(2)` directly would NOT get
//            Go's runtime CloseOnExec marking. The O_CLOEXEC flag is
//            applied at open time and travels with the fd.
//        (b) Window between Openat and exec: if a goroutine spawns a
//            subprocess immediately after we open a lock fd, the brief
//            window before the runtime marks fds is theoretically a race.
//            O_CLOEXEC closes that window unconditionally.
//        (c) Refactor resilience: enforcing the flag at every open call
//            site catches accidental regressions if a future maintainer
//            uses a non-os/exec spawn path.
//
//      Without O_CLOEXEC, the lock fd is inherited by `os/exec` children
//      (`wezsesh reply` from Lua, `wezsesh keygen`). For OFD locks, the
//      lock is owned by the open file description shared across forks;
//      a child that explicitly calls `fcntl(F_OFD_UNLCK)` on the inherited
//      fd releases the OFD lock from the parent's perspective. Even
//      without explicit unlock, fd-table inheritance is a hygiene leak.
//      Verified `man fcntl(2)` Linux Open file description locks section.
//
//      Constant availability across platforms (verified v2.4): `unix.O_CLOEXEC`
//      is defined uniformly in `zerrors_*.go` for linux, darwin, freebsd,
//      openbsd, netbsd — different numeric values per OS (0x80000 Linux,
//      0x1000000 darwin, 0x100000 FreeBSD, 0x10000 OpenBSD, 0x400000
//      NetBSD) but identical semantics. No platform gap.
//
//      CI test: AcquireExclusive a path, fork-exec a subprocess via
//      os/exec, child inspects /proc/self/fd/ (Linux) or runs lsof -p
//      (darwin) and asserts the lock fd is NOT in its fd table.
//
//   5. **Linux 3.15+ kernel baseline (added v2.3)**: F_OFD_SETLK requires
//      Linux 3.15 (April 2014). All supported distros (RHEL 8+, Ubuntu 20.04+,
//      Alpine 3.x, Arch, Fedora) ship with 5.x+. On older kernels, fcntl(2)
//      returns EINVAL for unrecognized cmd codes. The spec does NOT implement
//      runtime fallback — pre-3.15 is unreasonable to support. Doctor probes
//      `os.ReadFile("/proc/sys/kernel/osrelease")` and emits a clear error
//      if older. Optional EINVAL→F_SETLK fallback deferred to v0.2.
//
//   6. CI test (multi-fd close footgun): opens a path, takes the lock,
//      opens-and-closes a second fd to the same path, attempts a second
//      AcquireExclusive from a subprocess — must succeed on darwin (lock
//      dropped, demonstrating the footgun) and FAIL on Linux (OFD lock
//      survived).
func AcquireExclusive(ctx context.Context, path string) (fd int, release func(), err error)

// IsNetworkFS returns the filesystem type and whether it is a known
// non-local filesystem (or cloud-sync–backed local filesystem) where
// POSIX advisory locks may behave incorrectly. Two-layer detection
// (revised v2.2; resolution pipeline + prefix list extended v2.3):
//
//   Layer 1 — unix.Statfs:
//     Linux: Statfs_t.Type compared against NFS_SUPER_MAGIC (0x6969),
//       CIFS_MAGIC_NUMBER (0xff534d42), FUSE_SUPER_MAGIC (0x65735546),
//       AUTOFS_SUPER_MAGIC (0x0187), SMB2_MAGIC_NUMBER (0xfe534d42).
//     darwin: Statfs_t.Fstypename matched against
//       "nfs", "smbfs", "webdav", "osxfuse", "fuse", "afpfs", "autofs".
//
//   Layer 2 — path-prefix heuristic (darwin only; modern macOS cloud-sync
//   uses Apple's File Provider framework, which is INVISIBLE to Statfs —
//   Fstypename returns "apfs" indistinguishable from local APFS).
//   Resolution pipeline (revised v2.3):
//     1. Tilde-expand against os.UserHomeDir().
//     2. filepath.EvalSymlinks under context.WithTimeout(500ms) in a
//        goroutine. On overrun (symlink loop, hung NFS ancestor, dataless
//        cloud-only ancestor) OR Readlink permission error (EACCES on
//        ancestor symlink), fall back to unresolved/partial path + WARN.
//        Without this wrapper, EvalSymlinks itself becomes a startup-hang
//        surface — exactly the inversion v2.2 closed for Statfs but
//        reintroduced for symlinks (closed v2.3). Verified Go has no
//        portable loop limit for symlink resolution
//        (golang/go#73572 magic-link recursion).
//     3. filepath.Clean to normalize redundant slashes.
//     4. NFC-normalize via golang.org/x/text/unicode/norm.NFC.String;
//        same form applied to BOTH the resolved path AND the tilde-expanded
//        prefix anchors. APFS preserves on-disk filename bytes (no VFS
//        normalization the way HFS+ did); inconsistent normalization
//        between $HOME (NFD on-disk) and prefix anchors (NFC literal)
//        would silently fail comparison.
//     5. Case-insensitive prefix match on darwin via strings.EqualFold
//        (APFS is case-insensitive but case-preserving by default; a
//        user-created ~/dropbox lowercase resolves to the same inode as
//        ~/Dropbox but Go's filepath.Match without EqualFold misses it).
//        Case-sensitive on Linux.
//   Prefix list (extended v2.3; coverage scope clarified v2.4):
//     ~/Library/Mobile Documents/   (iCloud Drive — verified)
//     ~/Library/CloudStorage/iCloud~*    (verified — Apple-managed)
//     ~/Library/CloudStorage/Dropbox*    (verified — Dropbox 2024+ FP)
//     ~/Library/CloudStorage/GoogleDrive* (verified — GDD 2024+ FP)
//     ~/Library/CloudStorage/OneDrive*   (verified — Microsoft FP)
//     ~/Library/CloudStorage/Box*        (added v2.3 — Box for Mac
//                                         FP; actual subdir often
//                                         `Box-<UserName>`; the `Box*`
//                                         glob covers the family)
//     ~/Library/CloudStorage/Nextcloud*  (added v2.3 — community FP
//                                         variants ONLY; the standard
//                                         NextCloud Desktop client
//                                         syncs to ~/Nextcloud/, not
//                                         ~/Library/CloudStorage/. The
//                                         standard-client path is
//                                         documented as an out-of-list
//                                         false-negative — see below.)
//     ~/Library/CloudStorage/Proton*     (added v2.3 — best-effort.
//                                         Proton's macOS File Provider
//                                         was experimental as of 2025;
//                                         actual subdir naming has not
//                                         been verified against a
//                                         shipped client. If Proton's
//                                         client uses a different prefix,
//                                         this anchor is harmless dead
//                                         code that does NOT produce
//                                         false-positives. Verify on
//                                         first user report.)
//     ~/Library/CloudStorage/Seafile*    (added v2.3 — best-effort.
//                                         SeaDrive on Apple Silicon
//                                         migrated toward File Provider
//                                         in 2024; Intel SeaDrive uses
//                                         FUSE which Layer 1 catches
//                                         via `osxfuse`. Same harmless-
//                                         dead-code property as Proton
//                                         on platforms where the prefix
//                                         is wrong.)
//     ~/iCloud Drive (resolve target via EvalSymlinks; handles
//                     user-renamed alias via Terminal — Finder blocks
//                     rename but `mv` works, the symlink target still
//                     bottoms out at Mobile Documents)
//     ~/Dropbox       (legacy non-FP layout)
//     ~/Nextcloud     (added v2.4 — covers the *standard* NextCloud
//                     Desktop client, which is the dominant install
//                     and uses ~/Nextcloud/ NOT ~/Library/CloudStorage.
//                     The Library/CloudStorage/Nextcloud* anchor above
//                     covers community FP variants only. False-positive
//                     risk: any user-created ~/Nextcloud/ that is NOT
//                     a NextCloud sync root produces a benign cloud-
//                     sync WARN at startup; mitigated by Layer 1
//                     statfs check unmasking the absence of FUSE.)
//     ~/Google Drive   /Volumes/GoogleDrive*  (vestigial on Apple Silicon)
//     ~/Desktop, ~/Documents (only when iCloud "Desktop & Documents" is on)
//
//     **False-positive risk on Library/CloudStorage globs**: the
//     `Box*`/`Nextcloud*`/`Proton*`/`Seafile*` anchors are simple
//     `strings.HasPrefix` matches after NFC normalization. A user-
//     created `~/Library/CloudStorage/Boxes-archive/` would match
//     `Box*`, but `~/Library/CloudStorage/` is OS-managed by File
//     Provider — Finder hides user-created subdirs and the FP daemon
//     manages the namespace. Manual mkdir into this directory is
//     possible but rare. Layer 1 (`unix.Statfs`) verification of the
//     resolved path mitigates: if the matched prefix resolves to a
//     non-FP path, the Layer 1 fstype check confirms `apfs` and
//     downgrades the warning. Documented MEDIUM nit, not a bug.
//
// Internal implementation runs Statfs under context.WithTimeout(2s) in a
// goroutine — Statfs itself can block on a hung NFS server. EvalSymlinks
// runs under context.WithTimeout(500ms) for the same reason (added v2.3).
// On Statfs timeout, returns ("unknown", "statfs-timeout", true, nil) —
// treats unprovable paths as network to err on the side of cautionary
// WARN.
//
// Used by §6.7 startup WARN and §7.3 doctor. Documented false-negative
// classes (extended v2.3): (1) cloud-sync paths NOT on the Layer 2 prefix
// list (custom mount paths, niche providers); (2) symlinks INSIDE the
// snapshot dir pointing to cloud-synced data (Layer 2 only checks the dir
// itself, not its contents); (3) TOCTOU between TUI open and snapshot use
// (state can change). Recommendation: snapshot dir AND ALL contents must
// be local-only.
func IsNetworkFS(path string) (fsType string, fsLayer string, isNetwork bool, err error)
```

`AtomicWriteFile` internally:
1. `VerifyDir(parentDir)` to get `dirfd`. Errors if dir is a symlink.
2. Generate temp name `"." + filename + "." + randomHex(8) + ".tmp"`.
3. `unix.Openat(dirfd, tmpName, O_WRONLY|O_CREAT|O_EXCL|O_NOFOLLOW, perm)`.
4. Write, `Sync()`.
5. `unix.Renameat(dirfd, tmpName, dirfd, filename)`.
6. Close fds; defer unlink of tmpName on any pre-rename error.

**Per-write-site requirements**:

| Path | Open flags / pattern |
|---|---|
| `state.json` (state dir) | `safefs.AtomicWriteFile`. |
| `*.wezsesh.json` sidecars (snapshot dir) | `safefs.AtomicWriteFile`. **Sidecar writes are Go-only** — Lua's `io.open` has no `O_NOFOLLOW` equivalent. Tag/pin updates from Lua dispatch through the IPC layer to the binary, which performs the write. |
| `<sha256>` trust files | `safefs.AtomicWriteFile`. Plus: on first use, `os.Lstat` the trust *directory*; if symlink or non-directory, hard error. |
| Reply socket | `MkdirAll(replyDir, 0700)` → `os.Lstat(replyDir)` (error if symlink) → `os.Chmod(replyDir, 0700)` (in case it pre-existed loose). Set `unix.Umask(0077)` *before* `net.Listen("unix", path)` so the sock file is born `0600`; restore previous umask after. The earlier-spec'd `os.Chmod(sock, 0o600)` is a backstop, but umask handles it natively. |
| Log file | `os.Lstat` before each rotation open; if `ModeSymlink`, refuse to rotate and log to stderr instead. The log directory itself is `$XDG_STATE_HOME/wezsesh/` — XDG dirs are not normally attacker-writable, but check costs nothing. |
| Snapshot reads (resurrect-owned files) | `safefs.SafeOpenForRead` defends against an attacker swapping a snapshot file for a symlink to `/etc/passwd` etc. (read-only access, but still wastes our file-read budget on attacker-chosen content). |

**On `O_NOFOLLOW` availability**: not exposed as `os.O_NOFOLLOW` in Go stdlib. Use `syscall.O_NOFOLLOW` (defined on both Linux and darwin, identical behavior — `open()` returns `ELOOP` if final component is symlink) passed through `os.OpenFile`'s flag parameter. `openat(2)` is via `golang.org/x/sys/unix.Openat`.

**`O_NOFOLLOW` only protects the FINAL path component** (clarified v2.1). A symlink at any earlier path component is silently followed. Defense: every site that opens a path under user-controllable directories MUST first `safefs.VerifyDir` the parent (which rejects parent symlinks via `Lstat`), then `unix.Openat(dirfd, basename, O_NOFOLLOW, ...)` from that dirfd — never `os.OpenFile(path, O_NOFOLLOW, ...)` directly. The `AtomicWriteFile` flow already does this; `AcquireExclusive` (added v2.0, hardened v2.1) now does this too. (Verified [LWN on symbolic-link TOCTOU](https://lwn.net/Articles/899543/) and [SEI CERT POS35-C](https://wiki.sei.cmu.edu/confluence/display/c/POS35-C.+Avoid+race+conditions+while+checking+for+the+existence+of+a+symbolic+link).)

**XDG read timeouts** (added v2.1): `internal/state/`, `internal/trust/`, and `internal/snapshots/` — every read of `$XDG_STATE_HOME/wezsesh/`, `$XDG_DATA_HOME/wezsesh/`, and `$XDG_CONFIG_HOME/wezsesh/` MUST be wrapped in `context.WithTimeout(2 * time.Second)`. These paths default to `~/.local/state/`, `~/.local/share/`, `~/.config/` — all under `$HOME`. If `$HOME` is on a network mount (autofs, NFS, SMB, SSHFS), reads can block in the kernel for tens of seconds awaiting an unresponsive server. Without the wrapper, a single state.json read or trust file lookup blocks the binary indefinitely. On timeout: surface `XDG_PATH_TIMEOUT` (additive to §6.3) with remediation hint `XDG path on network mount? Move to local disk or override via opts.state_dir / opts.snapshot_dir.` This complements the existing 5s timeout on `internal/snapshots/` (§6.6) — the timeout floor for state/trust is tighter (2s) because those reads are far smaller than the snapshot dir scan and should complete in milliseconds locally.

**Trust file race is benign**: the trust file is content-addressed by hash. An attacker swapping the contents between `wezsesh trust` writing and the next restore reading gains nothing — the hash is recomputed from the in-memory `(path, command)` at restore time and compared against the filename. The on-disk contents are only used by `wezsesh trust --list` for display; mismatch is cosmetic, not a privilege escalation.

### 6.20 Wezterm CLI failure modes and timeouts

Every workspace operation depends on one or more `wezterm cli` subcommand invocations from the Go binary: `cli list --format json`, `cli list-clients --format json`, `cli rename-workspace`, `cli activate-pane`, `cli activate-tab`, `cli spawn`. The CLI client (verified `wezterm-client/src/client.rs:731-808`, `unix.rs:49-53`) has a bounded connection-attempt budget (10 retries × 50ms backoff ≈ 500ms total, with optional one-shot mux-server auto-spawn) but **no internal RPC timeout once connected**. A mux server that hangs after accepting a connection blocks the CLI invocation indefinitely.

**MANDATORY**: every `internal/wezcli/` invocation MUST be wrapped in `context.WithTimeout(2 * time.Second)`. The `os/exec.CommandContext` returns `context.DeadlineExceeded` on overrun; `internal/wezcli/` translates this to the `MUX_UNREACHABLE` error code (or to a poll-iteration retry inside `StartSwitchPoller`, which already inherits a 5s parent context). The 2s ceiling is calibrated against measured CLI response times: a healthy mux replies in <100ms; 2s leaves ample headroom for slow systems and concurrent load.

Without this wrapper, a single hung mux blocks the wezsesh TUI forever — no recovery short of `kill -9` on the binary. With it, every CLI failure path collapses to a fast, recoverable error.

**Error classification** (Go-side `internal/wezcli/`):

| Exit code | Stdout | Classification |
|---|---|---|
| 0 | valid JSON matching expected schema | success |
| 0 | empty `[]` array | success — empty result (workspace hasn't appeared yet during polling, no clients connected, etc.) |
| 0 | empty string OR invalid JSON | **abort** — wezterm CLI version mismatch or corrupt mux. Surface `MUX_UNREACHABLE` with a hint about wezterm version. |
| 1 | (any) | transient — surface as `MUX_UNREACHABLE`, eligible for retry by the poller |
| timeout | (any) | transient — same as exit 1 |

**Stderr is NOT parsed.** wezterm CLI errors are formatted as anyhow's `{:#}` Debug pretty-print (`wezterm/src/main.rs:705-706`); the format is free-form English and varies across wezterm versions. We classify by exit code + stdout-validity only.

**Per-subcommand contracts** (failure-mode supplement; the happy paths are already specified in §6.5, §6.13, §6.15):

- **`cli list --format json`**: connection retries 10×50ms then exits 1 if mux-server auto-spawn also fails. Empty mux returns `[]` (success). Polling layer (§6.15) retries until 5s parent context expires.
- **`cli rename-workspace <old> <new>`** (verified `wezterm/src/cli/rename_workspace.rs`): exits 1 with stderr `"unable to resolve current workspace"` when `<old>` is absent. Mux server has no collision check for `<new>`. **wezsesh MUST validate before sending**: the binary calls `cli list` first, computes the set of live workspace names, and rejects rename ops where `<new>` already exists with a `RENAME_COLLISION` error. Same-name renames (`<old> == <new>`) are short-circuited as no-ops.
- **`cli activate-pane --pane-id N`** / **`cli activate-tab --tab-id T`**: no client-side existence check; pane/tab may be closed between listing and activation. On exit 1, `wezsesh find` re-runs the listing once and retries; if the second attempt also fails, surfaces `PANE_CLOSED_RACE` (new error code, additive to §6.3 — see below). **Cross-workspace activation is NOT server-side implicit (corrected v2.3).** The server-side `SetFocusedPane` PDU handler at [`wezterm-mux-server-impl/src/sessionhandler.rs`](https://raw.githubusercontent.com/wezterm/wezterm/main/wezterm-mux-server-impl/src/sessionhandler.rs) calls `window.save_and_then_set_active(tab_idx)` and `tab.set_active_pane(&pane)`, then emits `MuxNotification::PaneFocused`. It does NOT call `mux.set_active_workspace_for_client(...)`. So if the target pane is in workspace B and the user's focused client is on workspace A, the server marks the pane as active within workspace B's window/tab, but the user remains on workspace A and the pane is invisible. Earlier PRD revisions (v1.x–v2.2) carried the wrong assumption forward without source verification. v2.3 corrects: `wezsesh find` (§6.13) MUST run a two-phase sequence — workspace switch (via `act.SwitchToWorkspace` + §6.15 polling) FOLLOWED by `cli activate-pane`. Cross-window activation within the same workspace works correctly (server raises the tab/pane within the target window) but does NOT raise the GUI window in OS z-order — that is a separate residual UX limitation.
- **`cli list-clients --format json`**: zero clients returns `[]` (success). Multiple clients: pick the one with smallest `idle_time`. Per `wezterm-client/src/client.rs:1321-1323`, clients are returned in a stable order by recency; relying on the first element is acceptable.
- **`cli spawn`**: `--cwd` is not validated client-side; mux server creates the pane with the requested cwd or defaults. Argv is not validated; bad binaries surface as shell errors inside the spawned pane. Exit code 0 means the pane was created, NOT that the program inside it succeeded.

**New error code** (additive to §6.3): `PANE_CLOSED_RACE` — surfaces when `cli activate-pane` / `activate-tab` fails twice in succession on a `wezsesh find` selection. UI behavior: status-bar toast `wezsesh: target pane closed; refresh and retry`.

**Polling load** (§6.15 supplement): single client = 100 invocations × ~15ms fork-exec = ~1.5s of CPU spread over 5s wall-clock. Two concurrent wezsesh TUIs polling = 2× that. Async tokio mux runtime handles this without rate-limiting concerns. Optimization (exponential backoff 50ms→500ms) is deferred; current cadence is acceptable for v0.1.

**Doctor checks** (extends §7.3):
- Probe `wezterm cli list --format json` with the same 2s timeout used at runtime; surface "mux unreachable" or "mux response slow (>500ms)" if degraded.
- Probe `wezterm cli rename-workspace --help` (no-op probe of subcommand availability) to detect wezterm < `20230408-112425-69ae8472`; surface "wezterm too old" with the version number floor.

## 7. API contracts

### 7.1 Lua plugin API

```lua
local wezsesh = wezterm.plugin.require("https://github.com/Grady-Saccullo/wezsesh")

wezsesh.apply_to_config(config, {
    -- ── Plumbing ────────────────────────────────────────────────────────
    binary = "wezsesh",                              -- PATH lookup
    keybinding = { key = "W", mods = "LEADER|SHIFT" },
    spawn_mode = "tab",                              -- "tab" | "window"
    snapshot_dir = nil,                              -- auto-detect from resurrect
    state_dir = nil,                                 -- auto-detect XDG_STATE_HOME
    runtime_dir = nil,                               -- reply sockets; auto-detect $XDG_RUNTIME_DIR on Linux,
                                                     -- /tmp/wezsesh-<uid>/ on darwin (NOT $TMPDIR — too deep
                                                     -- for SUN_PATH 104-byte limit). User overrides MUST
                                                     -- satisfy len(runtime_dir) + 14 ≤ SUN_PATH ceiling
                                                     -- (104 darwin / 108 Linux), validated at config-load
                                                     -- (§6.4 SUN_PATH validation).
    force_close = false,                             -- emit explicit close OSC for users with exit_behavior="Hold"

    -- ── Behavior ────────────────────────────────────────────────────────
    sort = "live_first",                             -- "live_first"|"recent"|"mtime"|"alphabetical"
    default_action = "switch",                       -- "switch"|"load"|"none"
    default_action_load_no_prompt = false,           -- skip clobber confirm on Enter when action is "load"

    confirm_delete = true,
    confirm_overwrite = true,

    exclude = { "^default$" },                       -- Go RE2 regex strings (NOT Lua patterns).
                                                     -- Picker-only: does NOT affect `wezsesh find`,
                                                     -- `wezsesh list`, or CLI bulk operations.
                                                     -- Compiled at binary startup; invalid regex
                                                     -- surfaces in `wezsesh doctor`. Lua patterns
                                                     -- were considered but rejected — silent
                                                     -- mistranslation between Lua's character classes
                                                     -- (`%d`, `%s`, `%w`) and Go's (`\d`, `\s`, `\w`)
                                                     -- is a real correctness hazard, and Lua patterns
                                                     -- lack alternation `|` and quantifier ranges
                                                     -- `{n,m}` that users will reach for.

    -- ── New-workspace flow ──────────────────────────────────────────────
    new_workspace_command = nil,                     -- nil → auto (zoxide → fd); string → user override

    -- ── Preview ─────────────────────────────────────────────────────────
    preview = {
        enabled = true,
        width = 0.4,                                 -- fraction of TUI width
    },

    -- ── Visual ──────────────────────────────────────────────────────────
    markers = {
        active = "▶",
        live = "●",
        marked = "✓",
        unsaved = "(unsaved)",
        pinned = "[pinned]",
    },

    columns = { "marker", "name", "tabs", "age", "tags" },

    name_truncate = "middle",                        -- "middle" only in v0.1; "left"|"none" deferred

    colors = {
        accent = nil,                                -- nil → terminal default
        muted = nil,
        error = nil,
        success = nil,
        focus_bg = nil,
        match_highlight = nil,
        live_marker = nil,
        saved_marker = nil,
    },

    -- ── Hook trust (see §6.11) ──────────────────────────────────────────
    hooks = {
        run_hooks = true,                            -- master switch; env WEZSESH_NO_HOOKS=1 overrides
        prompt_on_untrusted = false,                 -- false → fail-closed silent; true → interactive prompt
    },

    -- ── Logging ─────────────────────────────────────────────────────────
    log_level = "info",                              -- "error"|"warn"|"info"|"debug"|"trace"

    -- ── In-TUI keybindings ──────────────────────────────────────────────
    keys = {
        switch = "s", load = "l", rename = "r", delete = "d",
        save = "S", new = "n", pin = "p", tag = "t",
        mark = "Tab", mark_alt = "Space", clear_marks = "c",
        help = "?", filter = "/", preview = "P", quit = "q",
        up = "k", down = "j", top = "gg", bottom = "G",
    },

    -- ── Hooks ───────────────────────────────────────────────────────────
    on_before_op = nil,        -- function(op_table)
    on_after_op = nil,         -- function(op_table, result_table)
})
```

#### Version handshake

`plugin/init.lua` exposes `M.VERSION = "0.1.0"` (bumped on every tagged release; CI asserts the constant matches the about-to-be-cut tag). At plugin-load, `apply_to_config` runs the handshake exactly once (memoized).

**Hang-resistance contract for `run_child_process`** — `wezterm.run_child_process` (verified `lua-api-crates/spawn-funcs/src/lib.rs:30-50`) is `cmd.output().await` with **no timeout**. It suspends the config-eval coroutine until the child exits. A binary that exists but hangs at startup (broken dyld linkage on darwin from a stale `nix-collect-garbage` clean, partial `brew install`, codesign verification stall, or any Go-stdlib-init hang) **stalls wezterm's GUI paint indefinitely**. wezterm has no config-eval watchdog. Mitigation has two stages:

1. **Pre-flight existence check, fork-free.** Before any `run_child_process` call, validate the binary path via `os.execute`-free Lua filesystem checks: `local f = io.open(binary_abs_path, "r"); if f then f:close(); ok = true end`. If the file doesn't exist, skip the spawn entirely and route to the `"missing"` toast path. This collapses the "binary not on PATH" failure to a no-fork outcome, and removes one of three hang triggers.

   **Caveat — fork-free ≠ hang-free** (clarified v2.1). `io.open` calls libc `fopen(3)` → kernel `open(2)`. On a network-mounted home directory (autofs, NFS, SMB, SSHFS, Apple File Provider for iCloud Drive cloud-only files), `open(2)` itself can block in the kernel for 30–60 seconds awaiting an unresponsive server before returning `ETIMEDOUT` or `ESTALE`. The pre-flight check eliminates fork overhead and eliminates the hang surface in the *binary execution path* (codesign / dyld / Go runtime init), but it does NOT eliminate hangs in the *path resolution* itself. Document honestly: this is a residual hang risk for users with `$HOME` on a network mount, with **no portable mitigation** that doesn't itself fork or block. (Lua has no `pselect`-style timeout for filesystem ops.) Recommend installing wezsesh to a local-disk path (`/usr/local/bin/`, `~/.local/bin/` on tmpfs) for users on network-mounted home dirs. `wezsesh doctor` reports if the binary path resolves to a known network FS (§7.4).
2. **Cache the resolved absolute path.** Once `detect_binary_version` succeeds, cache `wezterm.GLOBAL.wezsesh_bin_path = absolute_path`. Subsequent spawns (keygen, every TUI launch, every reply emission) use the cached path and do NOT re-run version detection. Reduces the per-session hang surface to one event (the initial probe) rather than one per spawn. The path is re-resolved on `window-config-reloaded` (config reload is the user's signal that something changed).

**Residual hang risk**: if the binary exists, is readable, and runs but hangs (e.g., `crypto/rand` blocks on early-boot Linux with insufficient entropy — extremely rare on modern systems with `getrandom(2)`), the coroutine stalls. Document this in `wezsesh doctor`'s output so users can diagnose: a check that probes `wezsesh --version` with `wezterm.time.call_after`-driven timing tries to detect if the version probe took >2s and warns in doctor output. We cannot abort a hung `run_child_process` from inside Lua; the user's only recourse is to kill the wezterm process and remove the broken binary.



```lua
local function detect_binary_version(binary)
    local ok, stdout = wezterm.run_child_process({ binary, "--version" })
    if not ok then return "missing" end                 -- distinct sentinel
    return stdout:match("v?%d+%.%d+%.%d+") or "unparseable"
end

local function compatible(plugin_v, bin_v)
    -- 0.x: pin minor (plugin 0.M.* requires binary 0.M.*)
    -- 1.x+: binary minor >= plugin minor
    local pM, pm = plugin_v:match("v?(%d+)%.(%d+)")
    local bM, bm = bin_v:match("v?(%d+)%.(%d+)")
    if not pM or not bM then return false end
    if pM == "0" or bM == "0" then return pM == bM and pm == bm end
    return pM == bM and tonumber(bm) >= tonumber(pm)
end
```

On result:
- `bin_v == "missing"` → toast `wezsesh binary not found on PATH. Install: go install github.com/Grady-Saccullo/wezsesh/cmd/wezsesh@latest` (10s); keybinding stubs out. **Distinct from version mismatch** — the user's first-run experience must not say "wrong version" when nothing is installed.
- `bin_v == "unparseable"` → toast `wezsesh binary returned unrecognized version string`; keybinding stubs out.
- `compatible() == false` → toast `wezsesh binary v<X> doesn't match plugin v<Y>. Run 'wezsesh doctor' for details.`; keybinding stubs out.
- `compatible() == true` → proceed with full setup.

In all error cases:
- `wezterm.log_error(...)` (always; visible in debug overlay).
- One-shot `window:toast_notification("wezsesh", message, nil, 10000)` registered via `wezterm.on('gui-startup', ...)`.
- **The plugin does not raise a Lua error**; that would break the user's whole wezterm config.

#### `apply_to_config` body wrapped in `pcall`

The entire body of `apply_to_config` (binary detection, key generation, option validation, vendored module loading, listener registration, keybinding registration) runs inside a top-level `pcall`. On catch:
1. `wezterm.log_error("wezsesh: setup failed: " .. tostring(err))`.
2. Register a `gui-startup` toast: `wezsesh setup failed; check wezterm log`.
3. Return a no-op stub config — no keybindings, no listeners registered, no GLOBAL state written.

This guards against vendored Lua module syntax errors, unexpected `wezterm.run_child_process` return shapes, bad user options (e.g., `keybinding = "string instead of table"`), and any other Lua error during setup. The user's wezterm config still loads.

#### GLOBAL schema versioning across plugin updates

`apply_to_config` writes `wezterm.GLOBAL.wezsesh_plugin_version = M.VERSION` on every load. Before reading any other `wezsesh_*` GLOBAL key, it compares the stored version to `M.VERSION`. **On mismatch (including downgrade), wipe all `wezsesh_*` GLOBAL keys and re-initialize.** Any in-flight TUI session from the old version is in an undefined state post-update; better to drop it cleanly than to read stale-shape entries with new code. This handles the `wezterm.plugin.update_all()` flow without state-migration logic.

The Go binary embeds its version via `-ldflags="-X main.version=v$(git describe --tags --always)"` in CI / Nix / Goreleaser. Without that linker injection — including `go install github.com/Grady-Saccullo/wezsesh/cmd/wezsesh@vX.Y.Z`, since `go install` does not auto-set ldflags — `version` falls back to the literal string `dev` (`cmd/wezsesh/version.go`).

`wezterm.plugin.list()` is **constrained**: it returns only `{url, component, plugin_dir}`, no commit/tag. The embedded constant is the only viable mechanism for plugin self-identification.

#### Configuration precedence

1. Defaults baked into the plugin.
2. Values passed to `apply_to_config`.
3. Direct field assignment (`wezsesh.colors = { ... }`).

#### Programmatic API

```lua
wezsesh.open(window, pane)         -- trigger the TUI explicitly
wezsesh.is_running()               -- bool: is a TUI instance currently open in this window
wezsesh.list()                     -- returns workspace data as a Lua table
wezsesh.tags(name)                 -- returns tags for a workspace
wezsesh.pinned(name)               -- bool
```

### 7.2 Binary CLI

```
wezsesh                                          # launch interactive TUI (default)
wezsesh --version
wezsesh --snapshot-dir <path>
wezsesh --state-dir <path>
wezsesh --pane-id <int>                          # override $WEZTERM_PANE
wezsesh list [--format json]                     # non-interactive workspace listing
wezsesh doctor [--format json]                   # health check (see §7.3)
wezsesh find [PATTERN] [--deep] [--json]         # cross-workspace pane search (§6.13)
                [--cwd] [--workspace WS]
wezsesh trust <name>                             # approve current sidecar's on_restore command
wezsesh trust --revoke <name>
wezsesh trust --list                             # show trusted sidecars
wezsesh trust --prune                            # remove orphan trust entries
wezsesh trust --show <name>                      # print command without running
wezsesh keygen                                   # print 32 random hex bytes from crypto/rand;
                                                  #   used by Lua at plugin-load to seed WEZSESH_HMAC_KEY
                                                  #   (replaces the openssl dependency entirely)
wezsesh nuke                                     # default: PREVIEW mode. Lists every file/dir
                                                  #   that would be deleted; performs no I/O writes.
                                                  #   Output ends with: "Run 'wezsesh nuke --yes' to delete."
wezsesh nuke --dry-run                           # verbose preview — full paths, file sizes, symlink
                                                  #   warnings, target rationale per file. Same safety
                                                  #   as default (no deletion); more diagnostic output.
wezsesh nuke --yes                               # ACTUALLY delete. Removes wezsesh-owned files for
                                                  #   a clean uninstall: state dir, trust store, reply
                                                  #   sock dir, log files, *.wezsesh.json sidecars in
                                                  #   <snapshot_dir>/workspace/. Does NOT touch
                                                  #   resurrect snapshots themselves.
                                                  # Symlink defense (added v2.0): every target path is
                                                  #   `os.Lstat`-checked before any unlink/remove; any
                                                  #   ModeSymlink target is logged as a warning and
                                                  #   SKIPPED. The `--snapshot-dir` is independently
                                                  #   `safefs.VerifyDir`-checked at command entry; if
                                                  #   it is a symlink, the entire nuke aborts before
                                                  #   any deletion (cf §6.19).
                                                  # Reversal of v1.7 default: previously a bare `nuke`
                                                  #   deleted; v2.0 makes `--yes` mandatory because the
                                                  #   blast radius was hostile-by-default and a typo
                                                  #   in `--snapshot-dir` could remove unrelated files.
wezsesh reply <sock> <b64json>                   # internal — invoked by Lua for IPC reply (§6.4)
```

### 7.3 `wezsesh doctor`

Prints a status report covering:

- Binary version + path.
- Plugin version (read from `$WEZSESH_PLUGIN_VERSION` env var set by the Lua side, or `unknown`).
- Plugin/binary version compatibility.
- **Wezterm version ≥ `20230408-112425-69ae8472`** (driven by `wezterm cli rename-workspace`, introduced Apr 2023). Parsed from `wezterm --version`. Older wezterm may run but verbs that depend on the CLI rename will surface `MUX_UNREACHABLE`.
- `tty_name` field present in `wezterm cli list --format json` output (used by `wezsesh find --deep`; runtime probe).
- Snapshot dir: existence, readability, writability, file count.
- State dir: existence, readability, writability.
- `resurrect.wezterm` plugin: detected (via env or fallback path).
- `wezterm cli list` reachable.
- `wezterm cli list-clients` reachable + reports `focused_pane_id`.
- `WEZTERM_PANE` set + resolves to a real pane.
- Nerd font detection: `$WEZSESH_NERDFONT` env hint, terminal capability probe.
- Path picker: `zoxide` / `fd` on PATH.
- Trust store: existence, count of approved sidecars, count of orphans.
- `WEZSESH_NO_HOOKS` env var present (yes/no — informational).
- **Network-filesystem checks** (added v2.1): for each of `binary path`, `snapshot dir`, `state dir`, `trust dir`, run `safefs.IsNetworkFS` and report. NFS / SMB / SSHFS / autofs / cloud-sync (osxfuse) detection surfaces a WARN with the FS type (cf §6.7 lock semantics, §7.4 hang risks).
- Recent log tail (last 50 lines).

Exits 0 if all critical checks pass, non-zero otherwise. JSON output via `--format json` for machine consumption (and CI).

### 7.4 Platform hang risks (added v2.1)

The PRD's hang-resistance contracts (§7.1, §6.20, §6.6) bound *known* hang surfaces with `context.WithTimeout`. There are residual hangs that no Go context can cancel — the underlying syscalls block in the kernel waiting on remote servers and ignore POSIX signals. Document honestly so users know what to expect:

| Surface | Trigger | Effect | Mitigation |
|---|---|---|---|
| `io.open(binary_abs_path)` (Lua pre-flight, §7.1) | `$HOME` on autofs / NFS / SMB / SSHFS, server unresponsive | Stalls wezterm's GUI paint until kernel times out (typically 30–60s). | Install wezsesh binary to a local-disk path; `wezsesh doctor` reports binary FS type. |
| First `open(2)` of a cloud-only iCloud Drive file (darwin) | binary or plugin in `~/Library/Mobile Documents/...` with "Optimize Storage" enabled | Triggers daemon-mediated download; multi-second on slow connection. | Disable Optimize Storage on the wezsesh path, OR install to `~/.local/bin/`. |
| `wezterm.run_child_process({wezsesh_bin, "keygen"})` at plugin load | binary itself is on a network mount or cloud-only | wezterm's `cmd.output().await` has no timeout (verified `lua-api-crates/spawn-funcs/src/lib.rs:30-50`); coroutine stalls. | Install to local disk; doctor warns. |
| Plugin-update reload re-runs `apply_to_config` per window | plugin dir on network mount, every wezterm window's `apply_to_config` re-runs after `wezterm.plugin.update_all()` | Cascading hangs across windows. | Plugin dir should be on local disk; documented in README install guide. |
| State / trust file reads with `$HOME` on network mount | every TUI launch reads state.json + trust files | Each read could block in `open(2)`. | 2s `context.WithTimeout` (§6.19 XDG read timeouts) bounds Go-side reads; surfaces `XDG_PATH_TIMEOUT`. Lua-side has no equivalent — pre-flight `io.open` checks are documented as residual hang surface. |
| **Resurrect encryption pipeline during save (added v2.2)** | `resurrect.state_manager.set_encryption({enable=true, type="age"\|"rage"\|"gpg"})` — resurrect's [`encryption.lua`](https://raw.githubusercontent.com/MLFlexer/resurrect.wezterm/main/plugin/resurrect/encryption.lua) shells out via `wezterm.run_child_process({"age", "-e", ...})` (or `gpg`) with NO timeout. If gpg-agent is unresponsive (locked YubiKey, missing pinentry-mac, smartcard daemon stalled, network HSM glitch), the encrypt subprocess blocks indefinitely. | wezterm's config-eval coroutine suspends → entire wezterm GUI freezes until the process is killed externally. The `file_io.write_state.finished` event never fires → wezsesh's binary holds its `AcquireExclusive` lock until 5s ctx expires → returns `SNAPSHOT_LOCKED` even on subsequent retries. | Document in README that users with encryption MUST configure `gpg-agent` with `default-cache-ttl` / `max-cache-ttl` and a working pinentry; users with YubiKey-backed keys MUST verify the smartcard daemon is responsive (`gpg --card-status`) before relying on resurrect encryption. `wezsesh doctor` probes encryption configuration: detects `state_manager.set_encryption` enabled (via the loaded resurrect plugin's introspection) and runs a no-op `<provider> --version` probe with 2s timeout; surfaces `ENCRYPTION_AGENT_SLOW` (additive to §6.3) as a non-fatal hint. The hang itself remains uncatchable from wezsesh — it is upstream resurrect's process. Out-of-scope future hardening: contribute a timeout wrapper to resurrect upstream. |
| **`Statfs` syscall on hung NFS path (added v2.2)** | `safefs.IsNetworkFS` calls `unix.Statfs(path, &st)`; if the path's filesystem is on a hung NFS server, `Statfs` blocks in the kernel awaiting `lockd`/`nfsd` response. The helper designed to warn about hangs becomes one. | TUI launch hangs at the network-FS detection step before any UI renders. | `IsNetworkFS` runs `Statfs` in a goroutine under `context.WithTimeout(2s)`; on overrun, returns `("unknown", "statfs-timeout", true, nil)` and the helper falls through to "treat as network — emit cautionary WARN". The kernel's `Statfs` syscall is NOT canceled by ctx cancellation (Go has no portable way to interrupt a blocked syscall); the goroutine leaks until the kernel returns. Acceptable for v0.1 — one leaked goroutine per launch on a hung NFS path is a UX nuisance, not a correctness problem. |
| **`filepath.EvalSymlinks` on Layer 2 path (added v2.3)** | `safefs.IsNetworkFS` Layer 2 calls `filepath.EvalSymlinks(path)` to resolve the snapshot dir. Go has no portable loop limit (verified [golang/go#73572](https://github.com/golang/go/issues/73572) — magic-link recursion on Linux). On a circular symlink chain, on a chain whose ancestors live on a hung NFS path, or on a chain with a dataless cloud-only File Provider ancestor that triggers daemon-mediated download, `EvalSymlinks` can stall for tens of seconds — making the helper itself a startup-hang surface, exactly the inversion v2.2 closed for `Statfs`. | TUI launch hangs at the symlink-resolution step before any UI renders. | v2.3: wrap `EvalSymlinks` in a goroutine under `context.WithTimeout(500ms)`. On overrun OR on `os.Readlink` `EACCES`/`EPERM` errors (an ancestor symlink owned by another user with mode 0700), fall back to the unresolved or partially-resolved path and emit a non-fatal WARN. Treat the unresolved path as non-network (conservative — better a missed cloud-sync warning than a startup hang). Goroutine leaks until kernel returns; acceptable. |
| **`pluginkit -m -p com.apple.fileprovider-nonui` for File Provider discovery (deferred v0.2+)** | Future enhancement to dynamically enumerate File Provider extensions on darwin (would close most Layer 2 false-negatives). `pluginkit` forks a process that queries `fileproviderd`; if the daemon is stalled, `pluginkit` hangs (verified [eclecticlight on PlugInKit](https://eclecticlight.co/2025/04/16/how-pluginkit-enables-app-extensions/)). | TUI launch hangs at File Provider discovery. | Out of scope for v0.1. When implemented (v0.2+): MUST wrap in `context.WithTimeout(2s)`, fall back to the static prefix list on overrun, AND run only at doctor time (NOT TUI launch). |

There is no portable Lua API to bound a filesystem syscall by a wall-clock deadline. The mitigation strategy is: bound everything we *can* bound (Go-side context timeouts), and document everything we *can't*.

## 8. Implementation plan

### 8.1 v0.1 scope (everything below ships in v0.1)

**Plugin**
- `apply_to_config` honoring the full options table; **entire body wrapped in `pcall`** so setup errors don't break the user's wezterm config.
- `M.VERSION` constant + load-time version-handshake against `wezsesh --version`. Distinct toasts for "binary not found" vs "version mismatch" vs "unparseable" (§7.1).
- GLOBAL schema version stamp: `apply_to_config` writes `wezterm.GLOBAL.wezsesh_plugin_version = M.VERSION` and wipes `wezsesh_*` keys on mismatch — clean migration across `wezterm.plugin.update_all()`.
- Keybinding registration.
- HMAC key generation at plugin load via `wezterm.run_child_process({wezsesh_bin, "keygen"})`; `/dev/urandom` Lua fallback; hard-fail with toast + log_error if both fail (no weak-key launch). Key cached in `wezterm.GLOBAL.wezsesh_session_key` per wezterm session.
- Spawn invocation via env-wrapping (`env K=V ... wezsesh`) since `wezterm cli spawn` has no `--env` flag.
- `user-var-changed` listener with: pane-ID match, HMAC verify, replay/freshness guard, target_window_id filter, **early-return when `wezsesh_session_key == nil`**.
- `pane-closed` listener: when a pane with an in-flight `wezsesh_state[pid_key]` entry closes, surface `UNEXPECTED_EXIT` toast (don't wait for IPC_TIMEOUT).
- Vendored pure-Lua SHA-256 + HMAC + base64 + canonical-JSON modules.
- Custom **`on_pane_restore` callback** wrapping resurrect's default; allowlist-checks `pane_tree.process.argv[0]` per §6.18; degrades to cwd-only `cd <cwd>\r\n` for unknown programs.
- Dispatch for verbs: `switch`, `load`, `rename`, `delete` (single + bulk), `save`, `new`, `tag`, `pin`, `noop`. Restore-class verbs use the split-reply pattern (§6.2 step 9).
- Reply emission via `wezterm.background_child_process({wezsesh_bin, "reply", sock, b64})` — calls our own `wezsesh reply` subcommand, no shell, no nc dependency.
- Structured response for every op (success or error code) carrying `status` field (`completed`/`started`/`partial`).
- Toast helper for surfacing errors.
- `wezterm.GLOBAL.wezsesh_state` + `wezterm.GLOBAL.wezsesh_requests` + `wezterm.GLOBAL.wezsesh_writing` + `wezterm.GLOBAL.wezsesh_session_key` + `wezterm.GLOBAL.wezsesh_plugin_version` for reload durability. All keys are strings (GLOBAL Object nodes reject integer keys).
- Subscribe to `resurrect.file_io.write_state.{start,finished}` for the snapshot-write gate.
- Hooks: `on_before_op`, `on_after_op` — both `pcall`-wrapped at the dispatch boundary so user's buggy hooks don't block ops.
- Programmatic API: `open`, `is_running`, `list`, `tags`, `pinned`.

**Binary — TUI**
- Bubbletea list with: marker column, name (middle-truncated), tabs, age, tags.
- Sort modes: `live_first`, `recent`, `mtime`, `alphabetical`.
- Filter mode (modal, per §5.2) with match highlighting.
- Preview pane (toggleable via `P`, configurable via `opts.preview`).
- Modals: rename, delete (single + bulk), save, save-overwrite, save-changed, load-clobber-confirm, tag, new.
- Bulk select (`Tab`/`Space`/`c`).
- Pin toggle.
- Help overlay with full keybindings + version + paths.
- Active-workspace marker.
- Empty-state nudges including first-run.

**Binary — non-TUI**
- `wezsesh list --format json`.
- `wezsesh doctor` (built **early** to support dogfooding). Checks: wezterm version floor, snapshot dir / state dir / runtime dir validity (with symlink rejection), trust store integrity, snapshot argv allowlist coverage (lists snapshots whose argv[0] is not allowlisted), workspace-name validation scan, plugin/binary version compat.
- `wezsesh find` cross-workspace pane search (default + `--deep`).
- `wezsesh trust` subcommand family for hook trust management.
- `wezsesh keygen` — emits 32 random hex bytes from `crypto/rand`.
- `wezsesh nuke [--dry-run]` — clean uninstall: removes state dir, trust store, reply sock dir, log files, and `*.wezsesh.json` sidecars in `<snapshot_dir>/workspace/`. Leaves resurrect's snapshot files alone.
- `wezsesh --version`.

**Storage / IPC**
- OSC 1337 `SetUserVar` emit helper (`internal/uservar/`) writing to `/dev/tty` (mutex-protected, off bubbletea's stdout) with `tea.Cmd` wrapper.
- Per-request Unix-socket reply listener (`internal/ipcsock/`) with synchronous `StartListener()` API (must be called from Update, not Cmd). Reply dir = `$XDG_RUNTIME_DIR/wezsesh/` (Linux) or `/tmp/wezsesh-<uid>/` (darwin); sock filename = first 8 hex of request_id.
- `wezsesh reply <sock> <b64json>` subcommand (~30 LOC) for the Lua-side reply path. No `nc -U` dependency, no shell injection surface.
- HMAC-SHA-256 sign module on Go side (`internal/hmac/`) using canonical JSON (`internal/canonicaljson/`) byte-identical to Lua's.
- Canonical JSON spec: integers only (reject floats), keys sorted by UTF-8 byte order, no whitespace, minimal escape rules (see §6.14 + research findings v3).
- ULID generation for `request_id`.
- Single OSC retransmit at 2s if no reply; hard 5s ceiling (defensive against transport hiccups; reload-drops do not occur per source-code reading of wezterm — see §6.15).
- Snapshot dir auto-detect from resurrect (`state_manager.save_state_dir`); manual override via `snapshot_dir`.
- Tolerant resurrect-snapshot parser handling both `process: string` and `process: object` shapes.
- Magic-byte sniff: if first 32 bytes don't start with `{`/`[`/whitespace, mark snapshot as encrypted-opaque, render preview as `(encrypted)`.
- Filename encoding `/` ↔ `+` matching resurrect's transform.
- Defensive snapshot read with 3× JSON parse retry + 25ms backoff (resurrect race).
- Sidecar reading/writing (`*.wezsesh.json`) — **all writes via `internal/safefs/AtomicWriteFile`** (O_NOFOLLOW + openat(2) + Renameat). Lua side does NOT write sidecars directly; tag/pin updates dispatch through IPC.
- State file (`state.json`) for usage tracking + pins. Last-writer-wins under concurrent TUIs (acceptable per §6.8). Schema migration policy: on `version` mismatch, back up to `state.json.v<N>.bak` and reinitialize; loss of usage/pins history is acceptable.
- SHA-256 hash check on save/rename for concurrent-safety.
- **Workspace name validation** (`internal/nameval/`) per §6.17 — applied at every ingestion boundary before IPC/FS/mux ops.
- **Snapshot argv allowlist** (`internal/argvallow/`) per §6.18 — drives the Lua `on_pane_restore` callback's allow/deny decision.
- **Filesystem safety helper** (`internal/safefs/`) per §6.19 — `AtomicWriteFile`, `VerifyDir`, `SafeOpenForRead`. Mandatory for `internal/snapshots/`, `internal/state/`, `internal/trust/`. Lint rule: `os.WriteFile`/`os.OpenFile` forbidden in those packages.
- **Hook execution** (Go side, called from Lua via dispatch) per §6.11 expanded contract: read-once-exec-from-memory, `$SHELL` → `/bin/sh` fallback, cwd Stat + `$HOME` fallback, `Stdin = nil`, 10min timeout (configurable), `SysProcAttr.Setpgid` + group kill on timeout, env scrub of `WEZSESH_*` vars, `os.Lstat` on trust file (reject symlinks).
- Hook trust store + fail-closed execution per §6.11.
- `pcall`-wrapped restore + `RESURRECT_PARTIAL` / `SNAPSHOT_LOAD_FAILED` error surfacing per §6.12.
- Subscribe to `resurrect.error` event for save-failure observability.
- Logging to XDG state dir, level resolution per §6.9. Log file open: `os.Lstat` before each rotation; refuse if symlink.
- New-workspace path picker with zoxide → fd fallback.
- Crash-cleanup sweep at startup: remove stale `.sock` files in reply dir with mtime > 60s. Runs in `main()` before `tea.Run()`.
- Signal handler (`internal/ipcsock.InstallSignalHandler`) for SIGINT/SIGTERM/SIGHUP that runs the same cleanup before exiting (Go's `defer` doesn't run on signals).
- **5s `context.WithTimeout`** wrapping all FS ops in `internal/snapshots/`. On timeout: return partial list + non-fatal toast. Defends against NFS/cloud-sync hangs.
- **2s `context.WithTimeout`** wrapping every `wezterm cli` invocation in `internal/wezcli/`. wezterm's CLI has no internal RPC timeout once connected; a hung mux server can otherwise lock the binary indefinitely. On timeout: surface `MUX_UNREACHABLE` (or retry inside `StartSwitchPoller`'s 5s parent context). See §6.20.
- **Top-level panic recovery in `cmd/wezsesh/main.go`**: `defer func() { if r := recover(); r != nil { writePanicReply(...); os.Exit(2) } }()` placed before `tea.Run`. The handler writes a sentinel `UNEXPECTED_EXIT` reply over the OSC channel + any open reply socket so the Lua side learns of the death without waiting 5s for IPC_TIMEOUT. Only catches Go panics; SIGSEGV / SIGKILL / OOM-kill fall through to IPC_TIMEOUT (acceptable). Replaces the previously-spec'd `wezterm.on('pane-closed', ...)` listener which is non-implementable (event does not exist in wezterm; verified against documented window-events list).
- **Binary path cache + pre-flight existence check** at plugin load: `apply_to_config` resolves `wezsesh_bin` to an absolute path, verifies file existence via `io.open` (no fork), then probes `wezterm.run_child_process({wezsesh_bin, "--version"})` (which can hang per `lua-api-crates/spawn-funcs/src/lib.rs:30-50`'s untimed `cmd.output().await`). On success, the absolute path is stored in `wezterm.GLOBAL.wezsesh_bin_path` and reused for every subsequent spawn. Re-resolved on `window-config-reloaded`. See §7.1 hang-resistance contract.
- **Pre-rename collision check** in `internal/wezcli/RenameWorkspace`: before invoking `wezterm cli rename-workspace <old> <new>`, the binary calls `cli list` and verifies no live workspace already uses `<new>`. wezterm's CLI does NOT enforce uniqueness; without the pre-check, the rename either silently merges or surfaces a stderr error we can't reliably parse. Same-name renames (`<old> == <new>`) short-circuit as no-ops.

**Pinned dependencies** (revised v1.8 — charmbracelet v2 stack is now stable; round-3 had pinned v1 because v2 was RC, that's resolved)
- `charm.land/bubbletea/v2 v2.0.6` (released 2025-04-16; module path is `charm.land/bubbletea/v2` — upstream migrated from the old `charmbracelet` org path to `charm.land` at this version).
- `charm.land/bubbles/v2 v2.1.0`.
- `charm.land/lipgloss/v2 v2.0.3`.
- `github.com/charmbracelet/x/ansi v0.11.7` (not migrated upstream; retains `github.com/...` path).
- `charm.land/huh/v2 v2.0.3` (modal forms — rename / save / new dialogs).
- `github.com/sahilm/fuzzy v0.1.1` (last commit 2025-08-02, includes the NUL-byte panic fix).
- `golang.org/x/sys/unix` (for safefs `O_NOFOLLOW`/`Openat`/`Renameat`/`Umask`).
- `golang.org/x/text/unicode/norm` (NFC normalization for §6.17 names).
- `go.uber.org/goleak v1.3+` (test-only; goroutine leak detection).
- Vendored `Egor-Skriptunoff/pure_lua_SHA` at pinned commit `6adac177c16c3496899f69d220dfb20bc31c03df` (Lua-side SHA-256 + HMAC). `plugin/wezsesh/vendor/SOURCES.lock` records the upstream commit + sha256 of the file; CI runs `sha256sum -c` to detect tampering.

**Go runtime**: pin `go 1.26.2` in `go.mod` (current stable as of 2026-04). Includes patches for `crypto/tls` (CVE-2025-68121 session-resumption bypass), `archive/tar`, `html/template`, `crypto/x509`. wezsesh doesn't directly use TLS or zip but stdlib hygiene matters.

**No CVEs in any pinned dep** (audited against GitHub Advisory Database + `pkg.go.dev/vuln` 2026-04-29). One lipgloss usage hazard documented separately: §5.x (terminal-output sanitization).

**Required CI checks for supply-chain integrity**:
- `go mod verify` — go.sum tampering detection.
- `govulncheck ./...` — call-graph-aware vulnerability scan against `vuln.go.dev`.
- `sha256sum -c plugin/wezsesh/vendor/pure_lua_SHA.sha256` — vendored crypto code integrity.
- `go build -trimpath -ldflags="-s -w"` — reproducible builds; strip local file paths from binary, omit DWARF.
- `staticcheck ./...` and `go vet ./...`.

**Testing invariants**
- All integration tests exercising `StartListener`, `StartSwitchPoller`, `InstallSignalHandler`, or `tea.Run` MUST end with `goleak.VerifyNone(t, goleak.IgnoreTopFunction("internal/ipcsock.installSignalHandlerWorker"))` deferred. The signal-handler worker is the only intentional survivor; its top-function name is whitelisted by exact match. Any other goroutine surviving teardown is a test failure.
- Every goroutine spawned in `internal/ipcsock/`, `internal/wezcli/`, `internal/argvallow/`, and `internal/tui/` MUST have a top-level `defer func() { if r := recover(); r != nil { log.Error("goroutine panic", "err", r, "stack", debug.Stack()) } }()`. Mandatory, not advisory — these goroutines process untrusted-shaped input (socket payloads, CLI JSON output, snapshot data) and a panic must degrade to an error log, not a process crash.
- **Golden tests for canonical-JSON byte-equality** between Go (`internal/canonicaljson/`) and Lua (`plugin/wezsesh/canonical_json.lua`) per the rules in §6.3. Vectors MUST include: empty object `{}`, empty array `[]`, empty string `""`, NUL inside string, U+007F (DEL), U+2028 / U+2029, multi-byte UTF-8 (`café`, `日本語`, emoji), nested objects 3-deep, arrays of mixed types, integer edge cases (`0`, `-1`, `-9223372036854775808`, `9223372036854775807`), boolean values, explicit `null` (Lua sentinel), one fixture per verb in §6.3 with realistic and edge-case args. Both encoders run; outputs `diff`'d byte-for-byte; **any divergence fails the build**. CI runs with `LC_ALL=C` to lock locale-dependent comparison out.
- **HMAC round-trip golden test** (canonical fixture, run on every CI build): key = `"a0b1c2d3e4f5a0b1c2d3e4f5a0b1c2d3e4f5a0b1c2d3e4f5a0b1c2d3e4f5a0b1"` (64 hex chars), payload = the canonical-JSON noop fixture, expected_hmac = pre-computed via openssl/RFC-4231 reference tooling and committed to the repo. Both Go (`crypto/hmac`) and Lua (vendored `pure_lua_SHA.hmac` after `hex_to_bin`) MUST emit the exact same hex digest. Catches: key-format divergence (hex string vs raw bytes), canonical-JSON divergence, future SHA library swaps. Lua signs a fixture → Go verifies; Go signs a fixture → Lua verifies (round-trip both directions). The Lua side uses `ct_eq.eq` (constant-time, §6.14) for the equality check, never `==`.
- Race tests under simulated `resurrect.file_io.write_state` mid-write (truncate + rewrite cycle); confirm 3× retry + tolerant parser handles it.
- Multi-window broadcast test (#3524): N=3 windows, 1 pane emits OSC, only the matching `target_window_id` window dispatches.
- **Lua handler fuzz test** (§6.14 mandatory handler structure): fire 10 000 random/mutated bytes at the `user-var-changed` handler. Assert: (a) no Lua error escapes the handler (wezterm log shows no uncaught errors); (b) `ops.dispatch` is invoked zero times when the input is non-authenticated; (c) wezterm GUI remains responsive (frame paint times stay <50ms throughout). Mutation classes: random bytes, valid base64 of garbage, valid base64 of malformed JSON, malformed JSON missing each field one at a time, valid JSON with type-swapped fields (`ts: "string"`, `args: 42`), float-subtype `ts` and `target_window_id`, untagged Lua tables in `args`, oversized strings (1 MiB `id`).
- **Save/sidecar flock serialization test**: spawn two `wezsesh` binaries that both attempt to save the same workspace within 10ms of each other. Assert: (a) one succeeds, the other receives `SNAPSHOT_LOCKED` with retry option; (b) no silent lost-update — both snapshots' content is unique enough that the second writer's data is detectable in the final file iff the second succeeded.
- **Switch-poller false-positive test**: invoke `switch` to the workspace that is already active. Assert: poller short-circuits within one iteration (predicate (b) `target != preSwitch.ActiveWorkspace` is false); binary exits cleanly without 5s timeout. Separately: invoke `switch+restore` (load) to the already-active workspace. Assert: poller bypasses predicate (b) via `isRestoreFlow=true` and proceeds to emit the follow-up restore.
- **`wezsesh nuke` symlink defense test**: pre-place a symlink at `~/.local/state/wezsesh` pointing to a sentinel file. Run `wezsesh nuke --yes`. Assert: the symlink is detected, logged as a warning, and SKIPPED; the sentinel file is untouched. Repeat for the trust dir, log dir, sidecar files. CI must run as a non-root user with write access to the sentinel for this test to be valid.
- **`tea.Tick` retransmit cancellation test**: dispatch an op; the reply arrives at 100ms (well before the 2s retransmit). Assert via `goleak.VerifyNone` that the `tea.Tick` Cmd's goroutine has exited within 100ms of `tea.Run` returning (bubbletea's `program.ctx` cancellation propagates).
- **`F_SETLK` polling fairness/timeout test** (added v2.1): spawn three `wezsesh` binaries that all call `safefs.AcquireExclusive` on the same path within 10ms. One holds the lock for 100ms. Assert: (a) the other two acquire the lock within 5s (no infinite starvation under our backoff schedule), (b) WARN-at-1s and WARN-at-3s log lines fire if the lock is held >1s/3s, (c) on context cancel via `ctx.Done()`, the polling goroutine exits within 100ms (no leaked goroutine).
- **`safefs.IsNetworkFS` detection test** (added v2.1): mount a tmpfs at a sentinel path; assert detection returns `("tmpfs", false, nil)`. On CI runners with NFS access, mount NFS at a sentinel; assert `("nfs", true, nil)`. Skip darwin-specific cloud-sync probe in CI (cannot mount iCloud Drive in CI).
- **Project sidecar trust-enforcement test** (added v2.1): create a project dir with `.wezsesh.json` containing `on_create = "touch /tmp/wezsesh-test-rce"`. Pick the dir through the `n` flow without trusting it. Assert: (a) workspace creates successfully, (b) `/tmp/wezsesh-test-rce` does NOT exist (hook was silently skipped), (c) toast `wezsesh: on_create not trusted ...` was emitted, (d) `wezsesh trust ~/<picked>` succeeds and the next `n` of the same dir runs the hook.
- **Lua handler `.await`-free CI lint** (added v2.1): static-analyzer parses `plugin/wezsesh/ipc.lua` AST; identifies the `wezterm.on('user-var-changed', ...)` callback body; walks all call sites between markers `-- (a)` and `-- (h)`; fails if any callee is in `internal/lualint/async_funcs.go`'s known-async list (which includes `wezterm.run_child_process`, `wezterm.sleep_ms`, and any other mlua `add_async_function`-exposed wezterm API). Run on every PR.

**Distribution**
- Manual install: `go install ./cmd/wezsesh` and `nix run .#wezsesh`.
- `flake.nix` with dev shell + buildable binary.
- README with install matrix and screencast.
- GitHub Actions release workflow: cross-compile darwin-arm64, darwin-amd64, linux-amd64, linux-arm64. Build with `-trimpath -ldflags="-s -w -X main.version=v$(git describe --tags --always)"`. Tag-based semver from v0.1.0.
- Release workflow gated on the supply-chain CI (govulncheck, sha256 vendor verify, go mod verify) — failed gate blocks the artifact upload.
- Plugin's `init.lua` checks binary version at plugin load (memoized) and surfaces mismatches via `log_error` + one-shot toast.

### 8.2 Deferred

After three research passes (see `PRD_research_findings.md`, `PRD_research_findings_v2.md`, and `PRD_research_findings_v3.md`), most prior deferred items are folded into v0.1 scope. What's still genuinely deferred:

**Daemon mode (cold-start optimization)**. Per §10, cold start is bounded at ~250ms on darwin by mmap + adhoc-codesign cost — beyond user-code reach. v0.2: opt-in `wezsesh --daemon` started once at wezterm-launch via `wezterm.on('gui-startup', ...)`. Picker invocations send requests over the existing daemon's Unix socket instead of fork-execing a new binary. Defers the cold-start cost to once-per-wezterm-session. The IPC layer needed for OSC reply already gets us 90% of the way to a daemon model, so this is a moderate addition rather than a redesign. Out of scope for v0.1.

**Encryption-aware preview (Lua-shim cache)**. Resurrect's encryption is opt-in and turns snapshots into opaque ciphertext. v0.1 detects via magic-byte sniff and degrades to "(encrypted) — preview unavailable." Future: hook `resurrect.file_io.write_state.finished` from Lua, call `resurrect.state_manager.load_state` to get the decrypted table, marshal a metadata summary to `$XDG_RUNTIME_DIR/wezsesh/cache/<hash>.json`. Binary reads cache files when primary is encrypted. Skipped in v0.1 because encryption is rarely enabled.

**`wezsesh find` extensions**:
- Remote-domain pane discovery (SSH/TLS muxes via `wezterm.connect_*`). v0.1 only sees the local mux via `$WEZTERM_UNIX_SOCKET`.
- Scrollback grep via `wezterm cli get-text` ("find the pane where I saw error string X"). Useful but expensive; design pass needed.
- `Ctrl-/` in the main TUI to invoke `find` inline.

**Live-workspace deletion**. Wezterm has no `mux.kill_workspace`; doing it correctly requires enumerating `mux.all_windows()` and closing each. v0.1 `delete` is snapshot-only.

**`name_truncate` modes other than `middle`** — config knobs, add when asked.

**`pane:send_text` reverse channel** as a fallback. The Unix socket path is primary; if the socket fails (e.g., user has aggressive sandboxing), we could fall back to `pane:send_text` of a JSON line that the binary reads from stdin in a non-bubbletea phase. Sharp edges with bubbletea's stdin parser; deferred until needed.

**Mouse / SSH / multi-host** — PRD non-goals, kept out.

**Cloud sync** — out of scope forever.

## 9. Risks & open questions

| Risk / Question | Mitigation / Plan |
|---|---|
| Wezterm CLI API drift between versions | Pin minimum supported wezterm `>= 20230408-112425-69ae8472` in README. `wezsesh doctor` validates `wezterm --version` and warns on mismatch. Floor driven by `wezterm cli rename-workspace`; verbs requiring later features (none currently) would bump it. |
| `bubbletea` major-version bumps | Pin in `go.mod`. Cross-compile in CI. |
| Spawning the TUI in a new tab feels disruptive | Default `spawn_mode = "tab"`; `"window"` available. Re-evaluate default after dogfooding. |
| OSC 1337 length / encoding edge cases | Use Go `encoding/base64` unwrapped (no line wrap). **Hard ceiling: 4 KB canonical-JSON request, ~5.4 KB after base64.** CI test fails if any verb's encoded payload exceeds this. Wezterm itself has no byte cap on OSC sequences (`vtparse/src/lib.rs`, MAX_OSC=64 limits *parameter count*, not bytes); the 4 KB ceiling is self-imposed for sanity. |
| Wezterm silently swallows NUL bytes inside OSCs (`vtparse/src/transitions.rs:250`) | Validate names/payloads client-side in the Go binary before emitting OSC; do NOT rely on wezterm to reject. See §6.17. |
| Invalid base64 in OSC value silently drops the entire OSC (`wezterm-escape-parser/src/osc.rs:1276`) | Indistinguishable from a lost OSC; covered by the 2s single-retry resend (§6.2 step 10) and the 5s `IPC_TIMEOUT` ceiling. CI test fails if any payload Go produces fails round-trip Go-encode → Go-decode. |
| HMAC compare timing-attack | Go side: `hmac.Equal` (constant-time). Lua side: vendored pure-Lua SHA must use a constant-time string compare for the HMAC equality check. Practical threat is negligible (same-UID attacker reads the key directly via `/proc/<pid>/environ`); included for cryptographic hygiene. |
| `user-var-changed` is broadcast across all windows | Structurally addressed via `target_window_id` filter + replay guard; cited [#3524](https://github.com/wezterm/wezterm/issues/3524). |
| Bubbletea + OSC race-corruption during render | Open `/dev/tty` separately under `sync.Mutex`; do NOT write OSCs to `os.Stdout`. Bubbletea's renderer holds its own mutex on `os.Stdout` (`standard_renderer.go:161-291`) that user code can't share, and Cmds run as concurrent goroutines (`tea.go:355-367`), so a Cmd writing to `os.Stdout` races directly with frame flushes. Two fds, two mutexes, kernel serializes. |
| `wezterm cli list --format json` does NOT include user-vars | Reply path uses Unix socket per request, not CLI polling. See §6.4. |
| `wezterm cli spawn` has no `--env` flag | Wrap with `env K=V ... wezsesh` in argv. |
| OSC injection from `cat`/`curl` of malicious files | Pane-ID binding + HMAC-SHA-256 + replay guard. See §6.14. |
| No Lua crypto primitives in wezterm | Vendor pure-Lua SHA-256 (~200 LOC, MIT, audited) and base64. See §6.10, §6.14. |
| `wezterm.plugin.list()` does NOT expose commit/tag | Plugin self-knowledge via embedded `M.VERSION` constant. See §7.1 version handshake. |
| Transport-layer OSC delivery hiccups (truncated write, signal-interrupted syscall) | Single OSC retransmit at 2s; hard 5s ceiling; replay guard suppresses duplicates. All in-flight state in `wezterm.GLOBAL` for reload survivability. See §6.15. (Note: a previous PRD revision claimed reload drops events — not supported by source.) |
| Wezterm CLI floor below `20230408-112425-69ae8472` | `wezsesh doctor` parses `wezterm --version` and warns; README states the requirement. Floor driven by `wezterm cli rename-workspace`. |
| Resurrect `write_file` is non-atomic (truncate-and-rewrite) | Subscribe to `resurrect.file_io.write_state.{start,finished}`; gate TUI open via `wezterm.GLOBAL.wezsesh_writing`. Defensive 3× JSON parse retry on Go side. See §6.7. |
| Resurrect encryption makes snapshots opaque | Magic-byte sniff; render `(encrypted)` placeholder; encryption-aware Lua-shim cache deferred to post-v0.1. See §6.6, §8.2. |
| Wezterm cli spawn lacks per-spawn `--hold` / `--exit-behavior` | Default global `exit_behavior = "Close"` handles clean exit. `force_close` config knob covers users with `Hold` set globally. |
| Resurrect's snapshot dir path varies by encryption opts | Read from the loaded resurrect plugin's `state_manager.save_state_dir` (note: not documented as stable API; safer to set explicitly via `change_state_save_dir` and remember on Go side). Fall back to env var; last resort: default macOS path. Doctor checks. |
| Resurrect schema drift on their side | Tolerant parser per §6.6; mismatched `process` shapes handled by custom unmarshaler. |
| Restore-on-switch landing in wrong workspace (timing) | Go-side polling (50ms cadence, 5s ceiling) is the only path. The earlier `smart_workspace_switcher` event subscription was architecturally wrong (events fire only from its own picker UI, not programmatic `act.SwitchToWorkspace`). See §6.15. |
| **Snapshot `process.argv` as RCE vector** (resurrect calls `pane:send_text(shell_join_args(argv))` for alt-screen panes; malicious snapshot via dotfiles sync or same-UID trap executes arbitrary commands) | Wrap resurrect with custom `on_pane_restore` callback enforcing argv[0]-basename allowlist. Unknown programs degrade to `cd <cwd>\r\n`. User-extensible via `resurrect_argv_allowlist`. See §6.18. |
| **Symlink hijack on file writes** (state.json, sidecars, trust files redirected by same-UID attacker creating symlinks) | All writes via `internal/safefs.AtomicWriteFile` using `O_NOFOLLOW` + `openat(2)` + `Renameat`. Sidecar writes are Go-only (Lua's `io.open` lacks O_NOFOLLOW). See §6.19. |
| **TOCTOU between trust check and hook exec** (sidecar swapped between hash check and `exec.Command`) | Read sidecar exactly once; use the in-memory `command_bytes` for both hash and exec. Spec'd in §6.11 hook exec environment. |
| **Hook hangs / spawns orphan children** (`make dev`-style hooks) | 10-minute timeout (configurable), `SysProcAttr.Setpgid` + group SIGTERM-then-SIGKILL on expiry, `Stdin = nil`. See §6.11. |
| **Hook leaks WEZSESH_HMAC_KEY via environment** | `Cmd.Env` constructed from `os.Environ()` minus `WEZSESH_*` prefix. See §6.11. |
| **Trust file replaced with symlink** | `os.Lstat` (not `os.Stat`) on trust file path; reject symlinks. Trust-dir Lstat'd at startup. See §6.11. |
| **Binary panic surfaces as IPC_TIMEOUT** (not "binary crashed") | `pane-closed` listener cross-references `wezsesh_state[pid_key]`; on match emits `UNEXPECTED_EXIT` toast immediately, doesn't wait for the 5s ceiling. See §6.14 failure-mode matrix. |
| **NFS / cloud-sync hang on snapshot dir reads** | All FS ops in `internal/snapshots/` wrapped in `context.WithTimeout(5 * time.Second)`. On timeout: partial list + non-fatal toast. See §6.6. |
| **Plugin-update GLOBAL state-shape drift** (`wezterm.plugin.update_all()` + GLOBAL persists across reload) | `wezterm.GLOBAL.wezsesh_plugin_version` stamped at every `apply_to_config`; mismatch wipes all `wezsesh_*` GLOBAL keys. See §7.1. |
| **`apply_to_config` raises and breaks user's wezterm config** (vendored module syntax error, bad option type) | Entire body `pcall`-wrapped; on catch, log + toast at gui-startup, return no-op stub config. See §7.1. |
| **Binary not on PATH at first run shows "version mismatch" toast** | `detect_binary_version` distinguishes `"missing"` (run_child_process ok=false), `"unparseable"` (no semver), and `compatible()=false` (real mismatch); each has a distinct toast. See §7.1. |
| **Sidecar/sidecar-snap files left behind on uninstall** | `wezsesh nuke [--dry-run]` subcommand removes wezsesh-owned files. See §7.2. |
| **Command injection via `\n`/`\r` in snapshot `cwd` or `argv` element through `pane:send_text`** (Rust `shlex::try_quote` allows literal newlines inside single-quoted strings; pane's PTY sees them as line terminators) | Lua-side `bytes_clean` regex `[%z\1-\31\127]` rejects all C0 controls + DEL + NUL before passing to `wezterm.shell_quote_arg`. Dirty inputs downgrade to no-op send (Option C). See §6.18. |
| **Listener goroutine orphaned when TUI quits before terminal reply** (blocked on `Accept`, holds socket fd indefinitely) | `cleanup()` returned by `StartListener` calls `listener.Close()` AND `os.Remove(sockPath)` under `sync.Once`; `defer cleanup()` immediately after `StartListener`; signal handler invokes the same. Listener checks `net.ErrClosed` for graceful shutdown. See §6.4. |
| **Switch poller leaks until 5s ceiling on early workspace appearance** | `StartSwitchPoller(ctx, ...)` accepts `context.Context`; caller cancels on terminal reply or `tea.Run` return. Poller exits at next iteration boundary. See §6.15. |
| **Retransmit raw goroutine leaks until 2s firing even if reply arrived earlier** | Implemented as `tea.Tick(2*time.Second, fn)` Cmd (NOT `tea.After`, which does not exist in any bubbletea version); `Update` ignores via `model.replyReceived` flag. Bubbletea handles cancellation on program exit via `program.ctx`. See §6.2 step 10. |
| **Goroutine panic on malformed JSON / CLI output crashes the binary** (no diagnostic, dead pane) | Mandatory top-level `defer recover()` in every goroutine in `internal/ipcsock/`, `internal/wezcli/`, `internal/argvallow/`, `internal/tui/`. Panic logged via structured logger; goroutine exits cleanly. See §8.1 testing invariants. |
| **lipgloss renders untrusted control chars from snapshot filenames** (a workspace named `\x1b[2J` would clear the terminal at picker render) | `internal/nameval.SanitizeForDisplay` strips C0/C1 + DEL bytes before passing any disk-sourced string to a render call (lipgloss, fmt to stderr, log lines, toasts). See §6.17 render-time sanitization. |
| **charmbracelet v1 stack pinned in v1.0–1.7 but v2 is now stable** (round-3 avoided v2 RC; that's resolved) | Upgraded to v2 stack: bubbletea v2.0.6, bubbles v2.1.0, lipgloss v2.0.3, huh v2.0.3, x/ansi v0.11.7. Module path changed (`/v2`); fresh import. See §8.1 pinned dependencies. |
| **Path picker (`new_workspace_command`) exec model unspecified; timeout absent; output unbounded** | `exec.CommandContext(ctx, shell, "-c", cmd)` matching §6.11 shell resolution; 15s `context.WithTimeout`; 1 MiB stdout cap + 10 000 line cap; per-line UTF-8 + NUL validation; tilde-expansion + `os.Stat` post-pick. Distinct error codes `NO_PATH_PROVIDER` / `PATH_PICKER_TIMEOUT` / `PATH_PICKER_CMD_FAILED`. See §5.6. |
| **PRD ambiguity: "sidecar" referred to both `<picked_path>/.wezsesh.json` (on_create) and `<snapshot_dir>/workspace/<name>.wezsesh.json` (on_restore)** | Clarified in §5.6: "project sidecar" lives in the project dir; "snapshot sidecar" lives next to the resurrect snapshot. Same JSON schema, different locations and lifecycles. Trust hashes computed independently per location. |
| **Canonical-JSON empty `{}` vs `[]` ambiguity in Lua** (Lua tables are structurally ambiguous; without an explicit shape tag, an empty Lua `{}` could be serialized as `[]` by Lua and `{}` by Go — every empty-args op fails HMAC) | §6.3 Lua encoder uses an explicit shape tag (sentinel field `__array=true` OR distinct metatables via `canonical_json.array{}` / `canonical_json.object{}`). Empty objects emit `{}`, empty arrays emit `[]`. CI golden tests gate. |
| **Canonical-JSON `hmac`-field-removal ordering ambiguous** (one side could serialize with `hmac=""` while the other deletes the key entirely; the byte sequences differ → HMAC mismatch on every payload) | §6.3 specifies the exact 7-step sequence for both signer and verifier. Field is REMOVED entirely (not zeroed) before the HMAC-input serialization. |
| **Canonical-JSON encoding rules underspecified** (short-form vs `\u00XX` escapes, U+007F / U+2028 / U+2029 handling, integer-only enforcement on Lua float subtype, recursive vs top-level key sort) | §6.3 expanded with exhaustive rules: forbid short-form escapes, escape DEL + C1 + LS/PS, reject Lua floats via `math.type`, recursive key sort at every level. CI golden tests with multi-byte UTF-8, nested objects, integer edges. |
| **HMAC key format ambiguity** (PRD said "32 hex bytes" — could mean 32 raw bytes encoded as 64 hex chars, or 16 raw bytes encoded as 32 hex chars; if Go and Lua disagree on which to feed to HMAC, every payload fails) | §6.14 Layer 2 specifies: 32 raw bytes = 256 bits = 64 hex chars. Both sides hex-decode to 32 raw bytes BEFORE passing to HMAC. CI fixture with known key + known canonical payload + known expected digest. |
| **Lua-side HMAC compare not constant-time** (`pure_lua_SHA` does not export one; native `==` short-circuits) | New `plugin/wezsesh/ct_eq.lua` (~6 LOC); HMAC verifier uses `ct_eq.eq`, never raw `==`. Hygiene; threat is theoretical (same-UID attacker reads key from `/proc`). |
| **Every `wezterm cli` invocation can hang indefinitely** (CLI client has bounded connection-attempt budget but NO RPC timeout once connected; a hung mux locks the wezsesh TUI) | New §6.20: every `internal/wezcli/` invocation MUST be wrapped in `context.WithTimeout(2s)`. On overrun: `MUX_UNREACHABLE` (or retry inside the poller's 5s parent context). |
| **`wezterm.background_child_process` raises Lua errors on spawn failure; reply emission was unprotected** (binary missing, EMFILE, EACCES → unhandled error propagates out of the `user-var-changed` handler, dispatch path crashes mid-way) | §6.4 reverse path now wraps in `pcall`. Failure logged via `wezterm.log_error` + user-facing toast `reply emission failed; operation may have succeeded — refresh picker`. TUI still hits IPC_TIMEOUT but the user is informed. |
| **`wezterm.run_child_process` can hang indefinitely** (`cmd.output().await` has no timeout; a binary that exists but stalls on dyld / codesign / blocked stdlib init stalls wezterm's GUI paint with no watchdog) | §7.1 hang-resistance contract: pre-flight `io.open` existence check (no-fork, can't hang); cache resolved absolute path in `wezterm.GLOBAL.wezsesh_bin_path`; doctor probes timing and warns if version probe took >2s. Residual hang risk is documented; mitigation is removing the broken binary. |
| **`wezterm.on('pane-closed', ...)` does NOT exist as a Lua-surfaced event** (the previously-spec'd UNEXPECTED_EXIT early-warning toast is non-implementable; verified against documented window-events list and `wezterm-gui/src/termwindow/mod.rs` match arms — `MuxNotification::PaneRemoved` exists internally but is unhandled in the Lua emit path) | §6.14 failure-mode matrix now uses Go-side panic-recover: `cmd/wezsesh/main.go` installs a top-level `defer recover()` before `tea.Run`; on Go panic, writes `UNEXPECTED_EXIT` reply over OSC + open reply socket so Lua learns of the death immediately. SIGSEGV / SIGKILL / OOM-kill fall through to IPC_TIMEOUT (acceptable rare edge). |
| **Mux server has no rename-collision detection; `cli rename-workspace` exits 1 with free-form English on conflict** | §6.20: wezsesh validates client-side via `cli list` BEFORE invoking rename. If `<new>` is taken, surface `RENAME_COLLISION` without round-tripping wezterm. Same-name renames short-circuit as no-ops. |
| **`cli activate-pane` race when target pane closes between listing and selection** | New `PANE_CLOSED_RACE` error code. `wezsesh find` re-lists once on `cli activate-pane` exit 1 and retries; second failure surfaces the new code with toast `target pane closed; refresh and retry`. |
| **wezterm CLI stderr is unstable across versions** (anyhow's `{:#}` Debug pretty-print) | Error classification by exit code + stdout-validity ONLY (§6.20 table). Never parse stderr text. |
| **`tea.After` does not exist in any released bubbletea version** (PRD revisions through v1.9 mandated `tea.After(2s, retransmitMsg{})` for retransmit; verified absent in both v1.3.5 and v2.0.6 `commands.go`) | §6.2 step 10 / §6.4 / §6.15 / §9 corrected to `tea.Tick(2s, fn)` — bubbletea's actual one-shot timer Cmd. Cancellation on program exit is via `program.ctx`. |
| **DELETE worst-case payload exceeds 4 KB OSC ceiling** (50 marked names × 1200 canonical bytes worst-case escape expansion ≈ 60 KB; even 5 worst-case names approach the ceiling with envelope) | Per-OSC cap of 5 names; bulk-delete batches into ⌈N/5⌉ sequential OSCs sharing a single `bulk_id`. TUI shows one combined progress + final summary toast. Best-effort, not transactional. See §5.7. |
| **Save's `expected_hash` check + resurrect write is not atomic across concurrent TUIs** (two TUIs both pass each other's hash check within milliseconds; second writer silently clobbers the first) | §6.7 mandatory `safefs.AcquireExclusive` (POSIX `fcntl(F_SETLKW)`) on the snapshot file held through hash-check + resurrect's write + release. New error code `SNAPSHOT_LOCKED` for contended waits exceeding 5s. NFS caveat documented. |
| **Sidecar concurrent-write lost-update** (TUI A and B both edit tags on the same workspace; A's tag can be silently lost) | Same `safefs.AcquireExclusive` pattern as save (§6.7). Sidecar tag/pin updates are Go-side already (per §6.19); the binary acquires the lock, reads, modifies, writes via `safefs.AtomicWriteFile`, releases. |
| **Switch-poller false-positive when user is already in target workspace** (predicate "workspace appears in cli list" is true at t=0; poller declares success without waiting for actual switch; for switch+restore this could bypass the restore) | §6.15 pre-switch state capture + augmented predicate: succeed only when (workspace in target window) AND (target != pre-active OR isRestoreFlow). Cross-window false-positives also closed by scoping predicate to target_window_id. |
| **`wezsesh nuke` symlink hijack + dangerous default** (a same-UID attacker pre-places symlinks at ~/.local/state/wezsesh; bare `nuke` deletes through them; v1 default was DELETE) | §7.2 / §8.1: `--yes` mandatory for actual deletion (default is preview). Every target path `os.Lstat`-checked; symlinks logged + skipped. New `safefs.SafeRemove` / `SafeRemoveAll` helpers. `--snapshot-dir` `safefs.VerifyDir`-checked at command entry. |
| **Lua-side `user-var-changed` handler crashes on attacker-controlled payload** (`wezterm.json_parse` raises Lua error on malformed input; `ct_eq.eq(nil, computed)` raises on `#nil`; `background_child_process` raises on spawn failure) | §6.14 mandatory handler structure: every step that can raise is `pcall`-wrapped or preceded by a type/shape gate. Pane-ID check FIRST (drops foreign-pane DoS amplification). Field-shape validator runs BEFORE HMAC verify. CI fuzz test fires 10000 mutated payloads and asserts no Lua error escapes. |
| **Canonical encoder Option A (sentinel field) vs Option B (metatable wrappers) was not pinned in v1.9** (encoder must distinguish empty `{}` from `[]`; without an explicit choice, two implementations might pick differently) | §6.3 PINS to Option B (wrapper functions via metatables): `canonical_json.array{}`, `canonical_json.object{}`, `canonical_json.NULL`. Untagged tables are encoder errors (return nil + log). The `from_parsed(t)` helper re-tags structures from `wezterm.json_parse`. |
| **TAG verb tag-content unvalidated** (tags carried unbounded user input through IPC and into sidecar; canonical-JSON escape expansion of pathological tag content could blow past 4 KB) | §6.17 added tag-string validation rules: 1–10 tags per workspace, each 1–50 UTF-8 bytes, same byte rules as workspace names (no NUL/C0/DEL/C1/LS/PS/leading-trailing-whitespace). Validation runs client-side in the binary at the `tag` verb's request-construction boundary. |
| **§6.17 name validation allowed TAB / DEL / C1 / LS/PS** (each escapes to 6 canonical bytes per §6.3, enabling worst-case payload blowup; no legitimate workspace name uses these) | §6.17 rules tightened: TAB now forbidden (the prior `\t`-exemption was a UX lapse, not a need); DEL, C1 controls, U+2028 / U+2029 LS/PS all forbidden. Bounds canonical expansion to ~3× from multi-byte UTF-8, not 6× from escapes. |
| **bubbletea v2 renderer architecture changed** (v1's `standard_renderer.go` → v2's `cursed_renderer.go` delegating to `ultraviolet.TerminalRenderer`) | The renderer's internal mutex is preserved (verified `cursed_renderer.go:27` `mu sync.Mutex`); the `/dev/tty` + external `sync.Mutex` pattern in §6.4 still holds. CI integration test asserts no frame-OSC byte interleaving under concurrent load. No PRD architecture change required. |
| Restore partial failure | pcall + `RESURRECT_PARTIAL` error surface per §6.12. No rollback. |
| What if a user runs the binary outside a wezterm pane? | Detect via `WEZTERM_PANE`; fail fast with `error: wezsesh must run inside wezterm`. Doctor catches. |
| Two TUIs writing the same snapshot | Snapshot-hash check on save/rename (§6.7). |
| Hook command in sidecar is malicious / clobbered | Hash-based fail-closed trust per §6.11. |
| Auto-installed binary footgun | Rejected — stay with manual install matching wezterm plugin norms. |
| Sidecar schema drift on our side | We own it; bump `version` field and gate parsing accordingly. |
| `wezterm cli rename-workspace` exists; can skip Lua roundtrip | Use the CLI directly for rename verbs that don't touch the snapshot file. Lua handler only needed for ops requiring mux state changes the CLI doesn't expose. |
| **`fcntl(F_SETLKW)` is not interruptible on darwin** (PRD v2.0 spec'd "5s deadline via SIGALRM-equivalent watchdog"; Apple's fcntl auto-restarts on `SA_RESTART` signals, so the watchdog cannot break the syscall — verified [`man 2 fcntl`](https://man.freebsd.org/cgi/man.cgi?query=fcntl&sektion=2) on FreeBSD/Darwin) | §6.7 / §6.19 lock pattern revised v2.1: poll non-blocking `F_SETLK` with 10–100ms exponential backoff under a `context.WithTimeout(parent, 5s)`. WARN-at-1s/3s logging exposes contended waits (POSIX advisory locks have no fairness guarantee — verified [Pendleton 2010 lock-fairness test](http://bryanpendleton.blogspot.com/2010/07/unix-file-locking-does-not-implement.html)). |
| **NFS / cloud-sync filesystems silently break advisory locks** (NFS `fcntl` requires `lockd`, has known kernel hang/loop bugs; iCloud Drive / Dropbox / Google Drive sync agents perform atomic-replace bypassing held locks — verified [Linux-NFS list](https://linux-nfs.vger.kernel.narkive.com/f0ZYr5dG/nfs-infinite-loop-in-fcntl-f-setlkw), [eclecticlight on APFS+iCloud](https://eclecticlight.co/2023/11/15/backup-errors-icloud-drive-and-the-limits-of-apfs/)) | §6.7 / §7.3 (added v2.1): `safefs.IsNetworkFS` runtime detection via `unix.Statfs` + f_type / f_fstypename; one-time WARN at TUI open; doctor reports FS type per path. Operation NOT refused; user makes informed choice. Local-disk usage remains the default and recommended path. |
| **`O_NOFOLLOW` only protects the FINAL path component** (a symlink at any earlier component is silently followed; the v1.7 `safefs` spec used bare `O_NOFOLLOW` on path strings, leaving an exploitable hole — verified [LWN on symlink TOCTOU](https://lwn.net/Articles/899543/)) | §6.19 v2.1: every site that opens a path under user-controllable directories MUST first `safefs.VerifyDir(parentDir)` → `unix.Openat(dirfd, basename, O_NOFOLLOW)`. `AcquireExclusive` updated. Lint rule forbids `os.OpenFile(path, O_NOFOLLOW)` in `internal/safefs/`-using packages. |
| **XDG path reads under `$HOME` on network mounts can hang indefinitely** (state.json + trust files; `open(2)` blocks in kernel awaiting unresponsive autofs/NFS/SMB/SSHFS server, sometimes 30–60s, ignoring Go's context cancellation — kernel-level block, not Go's to abort) | §6.19 v2.1: 2s `context.WithTimeout` wrapping every read in `internal/state/`, `internal/trust/`. New error code `XDG_PATH_TIMEOUT` (additive to §6.3). Caveat: Go context cancels the goroutine but cannot kill an in-flight kernel `open(2)` — the syscall completes after the kernel timeout regardless. Mitigation is bounding *our* wait on the result, not unblocking the kernel. Documented in §7.4. |
| **`io.open` pre-flight in §7.1 is fork-free but NOT hang-free** (v1.8 mitigation removed fork overhead but didn't address that `open(2)` itself can block on network mounts; the v1.8 wording "no-fork no-hang" was wrong) | §7.1 v2.1: clarified that `io.open` eliminates fork overhead and binary-execution-path hang triggers (codesign / dyld / Go runtime init), but does NOT eliminate path-resolution hangs. README install guide recommends local-disk paths for network-mounted home dirs. New §7.4 documents the residual hang surface honestly. |
| **iCloud Drive / Dropbox / Google Drive cloud-only files block first `open(2)`** (darwin-specific; "Optimize Storage" enabled materializes files on-demand via daemon, multi-second on slow connection) | §7.4 (added v2.1): platform hang risks table documents this. `wezsesh doctor` checks if binary or plugin path is in a known cloud-sync directory. README recommends "Keep this Mac" for the wezsesh path or installing to local-disk path. |
| **Plugin-update reload re-runs `apply_to_config` per window — cascading hangs on network mounts** (`wezterm.plugin.update_all()` triggers config reload in every window; each runs the pre-flight `io.open` + `run_child_process` chain) | §7.4 (added v2.1) documents; README install guide states plugin dir should not be on network mount. |
| **Project sidecar (`<picked_path>/.wezsesh.json`) trust enforcement was ambiguous** (PRD §5.6 referred to §6.11 for trust check but didn't explicitly state the check is mandatory and identical to the snapshot-sidecar restore path; ambiguity could allow a future implementer to skip the check, opening RCE-on-pick from a malicious git-cloned repo) | §5.6 v2.1: explicit subsection "Project sidecar trust enforcement" pins the trust hash construction (path-bound to `<picked_path>/.wezsesh.json`), the fail-closed default, and the toast-on-silent-skip UX. `wezsesh trust <name>` resolves the project sidecar path via `cli list`. README + `wezsesh trust --show` surface authorship guidance for project authors. |
| **Project sidecar `n`-flow silent failure UX is footgunny** (default `prompt_on_untrusted = false` means user picks dir, hook silently doesn't run, dev server doesn't start, no obvious error) | §5.6 v2.1: every silent skip surfaces a 6s toast `wezsesh: on_create not trusted for "<name>". Run 'wezsesh trust <name>' to approve.` README + doctor recommend `prompt_on_untrusted = true` for users who frequently use `n` flow with new directories. |
| **wezterm.GLOBAL replay-guard read-modify-write race depends on cooperative scheduling assumption** (Lua handlers run as async tasks; if any step (a)–(h) yields via `.await`, two concurrent handlers can lose-update each other's `seen_ids` entry, weakening replay protection) | §6.14 v2.1: handler steps (a)–(h) MUST be synchronous Lua bytecode with zero `.await` points. CI lint parses `plugin/wezsesh/ipc.lua` AST and fails if (a)–(h) calls any known-async wezterm function. Fallback documented: 30s freshness window + per-payload HMAC are the primary replay defense; `seen_ids` is defense-in-depth + #3524-broadcast-bug deduplication. |
| **wezterm source line citations drift across versions** (round-9 cited `emit_user_var_event` at 2564–2608; round-10 found it at ~2080. Function name unchanged, behavior unchanged. Stale line numbers create false alarms during future audits) | §6.15 v2.1 clarifies: line citations to wezterm should be treated as approximate-and-drifty; function/symbol names are authoritative. Audit guidance: validate by symbol name + signature, not line number. |
| **`safefs.IsNetworkFS` cannot detect modern macOS cloud-sync** (iCloud Drive, Dropbox 2024+, Google Drive Desktop 2024+ migrated from osxfuse to Apple File Provider framework; File Provider–backed paths report `f_fstypename = "apfs"` indistinguishable from local APFS — verified [Apple TidBITS, 2023](https://tidbits.com/2023/03/10/apples-file-provider-forces-mac-cloud-storage-changes/), [eclecticlight, 2023 Sonoma iCloud Drive](https://eclecticlight.co/2023/10/25/macos-sonoma-has-changed-icloud-drive-radically/). v2.1's "match against `osxfuse`" approach gives ZERO coverage for the dominant macOS cloud-sync risk) | §6.7 / §6.19 v2.2: two-layer detection. Layer 1 (`Statfs`) keeps the network-FS list and adds `"fuse"` (was missing in v2.1). Layer 2 path-prefix heuristic added for File Provider extensions: `~/Library/Mobile Documents/`, `~/Library/CloudStorage/{iCloud~*,Dropbox*,GoogleDrive*,OneDrive*}`, `~/iCloud Drive`, `~/Dropbox`, `~/Google Drive`, `/Volumes/GoogleDrive*`, plus iCloud-redirected `~/Desktop`/`~/Documents` when iCloud "Desktop & Documents" is enabled. Documented false-negative class: cloud-sync paths NOT on the prefix list bypass detection — best-effort warning, not a guarantee. |
| **Resurrect's `default_on_pane_restore` callback signature was wrong in PRD v1.7–v2.1** (PRD said `(pane, pane_tree)`; verified actual signature is `(pane_tree)` only with pane accessed as `pane_tree.pane` — [resurrect tab_state.lua](https://raw.githubusercontent.com/MLFlexer/resurrect.wezterm/main/plugin/resurrect/tab_state.lua) line ~95. An `on_pane_restore(pane, pane_tree)` two-argument hook installed per the v2.1 spec would crash on first restore, fail-open, and the §6.18 RCE defense would silently bypass) | §6.18 v2.2: callback corrected to single-argument `on_pane_restore(pane_tree)`; pane derived as `pane_tree.pane`; explicit fail-CLOSED on hook crash (caught by outer `pcall`, send `\r\n` only, never call resurrect's default). CI assertion: hook that raises must NOT result in `pane:send_text(shell_join_args(argv))`. |
| **Argv indexing was inconsistent in PRD prose** (some sections said `argv[0]`, others `argv[1]`; resurrect uses Lua's 1-based convention where `argv[1]` IS the program name. The defense was actually checking the right element but the prose ambiguity could mislead implementers) | §6.18 v2.2: normalized to `argv[1]` everywhere with explicit note that this is the program (NOT the first argument; opposite of C-convention `argv[0]`). |
| **POSIX advisory locks: any close-on-the-same-file releases ALL locks** (verified Linux fcntl_locking(2) + macOS fcntl(2): "If a process closes any file descriptor referring to a file, then all of the process's locks on that file are released, regardless of the file descriptor(s) on which the locks were obtained." A latent footgun: if any code path inside `AcquireExclusive`'s critical section opens-and-closes a second fd to the locked path, the lock silently drops without notification) | §6.19 v2.2: explicit fd-hygiene contract on `AcquireExclusive`. Lint rule forbids re-opening the same path while the lock fd is live; reads during the hold use the locked fd (pread). Linux uses `F_OFD_SETLK` (Open File Description locks; Linux ≥ 3.15) which avoid the footgun by binding to the fd, not the process. darwin/FreeBSD lack OFD locks — discipline-only. CI test demonstrates the difference. |
| **POSIX advisory locks do not block `io.open(w+)` from another process** (advisory locks are observed only by code calling `fcntl(F_GETLK)`/`fcntl(F_SETLK)`; resurrect's writer in wezterm's process uses bare `io.open` and does NOT consult locks. v2.1 prose suggested the lock prevented "concurrent writes" — true only across wezsesh binaries, not against resurrect's own writer) | §6.7 v2.2: explicit clarification — the lock is a "binary-instance gate", not a "physical write barrier". Sound under the assumption that resurrect's `save_state` is invoked ONLY through wezsesh's IPC path; a user calling `resurrect.state_manager.save_state` directly bypasses the lock. Documented as v0.1 limitation; future hardening: have wezsesh be the actual file writer (Go-side). |
| **Resurrect encryption pipeline can hang the wezterm GUI on `save`** (confirmed [resurrect encryption.lua](https://raw.githubusercontent.com/MLFlexer/resurrect.wezterm/main/plugin/resurrect/encryption.lua): shells out via `wezterm.run_child_process({"age", "-e", ...})` or `gpg` with NO timeout. If gpg-agent is unresponsive — locked YubiKey, missing pinentry, smartcard daemon stalled — the encrypt subprocess blocks indefinitely, suspending wezterm's config-eval coroutine and freezing the entire GUI. v2.1's §7.4 hang-surfaces table omitted this) | §7.4 v2.2: encryption pipeline added as a residual hang surface. Doctor probes encryption configuration and runs a non-blocking `<provider> --version` probe with 2s timeout; surfaces `ENCRYPTION_AGENT_SLOW` (additive to §6.3) as a non-fatal hint. README requires users with encryption to verify gpg-agent / pinentry / smartcard daemon health. The hang itself is uncatchable from wezsesh — upstream resurrect's process. Future hardening: contribute a timeout wrapper to resurrect upstream. |
| **`Statfs` itself can hang on a hung NFS server** (the `safefs.IsNetworkFS` helper designed to warn about hang surfaces would itself hang at TUI launch on a hung NFS path before any UI renders — verified [Red Hat NFS hang KB](https://access.redhat.com/solutions/28211)) | §6.19 v2.2: `IsNetworkFS` runs `Statfs` in a goroutine under `context.WithTimeout(2s)`; on overrun, returns `("unknown", "statfs-timeout", true, nil)` — treats unprovable paths as network to err on the side of cautionary WARN. Goroutine leaks until kernel returns (Go has no portable way to interrupt a blocked syscall); acceptable nuisance for v0.1. Documented in §7.4. |
| **`runtime_dir` overrides not validated against AF_UNIX `SUN_PATH` ceiling** (104 darwin / 108 Linux). v2.1 documented the ceiling for the default path but did not require `apply_to_config` to validate user overrides. A user setting `runtime_dir = "~/Library/Application Support/.../"` silently passes config load, then every TUI launch fails at `net.Listen("unix", path)` with `ENAMETOOLONG`; the TUI surfaces a generic `IPC_TIMEOUT` and the root cause is invisible) | §6.4 v2.2: Lua-side `apply_to_config` validates `len(opts.runtime_dir) + 14 ≤ ceiling` and raises a config error with the exact remediation message. Go binary independently re-validates at runtime as defense-in-depth, surfacing new error code `IPC_INIT_FAILED` (additive to §6.3). |
| **PIPE_BUF atomicity caveat for §6.4 "two fds, kernel serializes"** (kernel atomicity is per-`write(2)`-syscall up to PIPE_BUF — 4 KB on Linux, 512 bytes POSIX-min on darwin. Bubbletea full-redraw frames on resize can exceed PIPE_BUF; concurrent OSC writes interleave at the boundary between bubbletea's two underlying syscalls, producing visibly glitchy frames during resize. Risk is cosmetic, not security/correctness — both byte streams remain well-formed escape sequences) | §6.4 v2.2: clarified that the atomicity guarantee is per-syscall up to PIPE_BUF; the 4 KB OSC ceiling (§6.3) keeps our writes atomic; bubbletea frames over PIPE_BUF accept cosmetic interleaving as acceptable cost. Risk classified as MEDIUM/cosmetic. |
| **Resurrect snapshot `process` shape drift** (PRD v2.1 said "both string-shape and object-shape in the wild"; round 11 verified resurrect main branch now consistently writes object-shape only — string-shape only appears in pre-2024-08 saved files in users' state dirs. Tolerant parser still required for legacy snapshots, but the in-the-wild characterization was inaccurate) | §6.6 v2.2: clarified — current resurrect writes object-shape only; tolerant Go parser still handles string-shape for old snapshots that haven't been re-saved. README schema lags implementation; treat resurrect source as authoritative. |
| **`F_OFD_SETLK` constant is undefined on darwin in `golang.org/x/sys/unix`** (verified `zerrors_darwin_*.go` does not declare F_OFD_SETLK; only `zerrors_linux.go` does). v2.2's spec said "use F_OFD_SETLK on Linux when available" without mandating build-tag separation; a naive implementation using `runtime.GOOS == "linux"` runtime branching FAILS TO COMPILE on darwin because the compiler resolves all constants at compile time. | §6.19 v2.3: mandatory build-tag split — `internal/safefs/lock_linux.go` (//go:build linux) holds the F_OFD_SETLK path; `internal/safefs/lock_other.go` (//go:build !linux) holds the F_SETLK fallback with the multi-fd discipline as the only defense. CI lint rule rejects any `unix.F_OFD_SETLK` reference outside `lock_linux.go`. |
| **`AcquireExclusive`'s lock fd is inherited by `os/exec` children** (`unix.Openat` without `O_CLOEXEC` produces an fd that's inherited by every fork-spawned child — `wezsesh reply` from Lua per request, `wezsesh keygen` per session. For OFD locks, the lock is owned by the open file description shared across forks; a child that explicitly calls `fcntl(F_OFD_UNLCK)` on the inherited fd releases the OFD lock from the parent's perspective. For F_SETLK on darwin, fd-table inheritance is a hygiene leak. Verified `man fcntl(2)` Linux Open file description locks section + Go runtime `os/exec_unix.go` for default CLOEXEC behavior — `os.OpenFile` sets FD_CLOEXEC since 1.10+ but `unix.Openat` honors only the flags passed.) | §6.19 v2.3: every `unix.Openat` in `internal/safefs/` MUST include `unix.O_CLOEXEC` in the flag set: `unix.O_RDWR | unix.O_NOFOLLOW | unix.O_CLOEXEC`. Lint rule enforces. CI test: AcquireExclusive a path, fork-exec a subprocess, child inspects /proc/self/fd/ (Linux) or `lsof -p` (darwin) and asserts the lock fd is NOT in its fd table. |
| **`filepath.EvalSymlinks` in `IsNetworkFS` Layer 2 is a startup-hang surface** (Go has no portable loop limit for symlink resolution — verified [golang/go#73572](https://github.com/golang/go/issues/73572) magic-link recursion. On a circular symlink chain, on a chain whose ancestors live on a hung NFS path, or on a chain with a dataless cloud-only File Provider ancestor that triggers daemon-mediated download, `EvalSymlinks` can stall for tens of seconds — exactly the inversion v2.2 closed for `Statfs` but reintroduced for symlinks.) | §6.7 / §6.19 v2.3: wrap `EvalSymlinks` in a goroutine under `context.WithTimeout(500ms)`. On overrun OR on `os.Readlink` `EACCES`/`EPERM` errors (an ancestor symlink owned by another user with mode 0700), fall back to the unresolved/partial path + non-fatal WARN. Treat unresolved as non-network (conservative). Goroutine leaks until kernel returns; acceptable. |
| **Layer 2 prefix list misses third-party File Provider extensions** (Box for Mac since 2024, NextCloud community builds, ProtonDrive experimental, Seafile/SeaDrive — all use `~/Library/CloudStorage/<provider>*` paths NOT on v2.2's prefix list. Plus: custom mount paths configured by advanced users — the heuristic has narrower coverage than v2.2's narrative implied.) | §6.7 / §6.19 v2.3: prefix list extended with `Box*`, `Nextcloud*`, `Proton*`, `Seafile*`. Documented false-negative classes expanded explicitly: (1) custom mount paths, (2) niche providers not on the list, (3) symlinks INSIDE the snapshot dir pointing to cloud-synced data (Layer 2 only checks the dir itself, not its contents — recommendation strengthened to "ALL contents must be local-only"). `pluginkit -m -p com.apple.fileprovider-nonui` for dynamic discovery deferred to v0.2+ with explicit hang-risk note. |
| **NFC vs NFD normalization mismatch between `$HOME` and prefix anchors** (APFS preserves on-disk filename bytes as-given; `os.UserHomeDir()` returns whatever encoding the on-disk dir uses, typically NFD on Finder-created dirs. v2.2 said "apply NFC normalization" without specifying that the prefix anchors must also be NFC-normalized at the same step. If `$HOME` is `/Users/café` with `é` stored as NFD on-disk and the prefix anchor is constructed with NFC `é`, comparison fails silently.) | §6.19 v2.3: explicit pipeline — tilde-expand → `EvalSymlinks` (timeout-wrapped) → `filepath.Clean` → NFC-normalize BOTH the resolved path AND the prefix anchors at the same step via `golang.org/x/text/unicode/norm.NFC.String`. Prefix matching uses `strings.EqualFold` on darwin (APFS is case-insensitive but case-preserving), case-sensitive on Linux. |
| **`opts.runtime_dir` `+14` SUN_PATH budget undercounts by 9 bytes if user-supplied path lacks `/wezsesh/`** (v2.2's check is `len(opts.runtime_dir) + 14 ≤ ceiling`. The +14 is for `/<8hex>.sock`. If `opts.runtime_dir = "/run/user/1000"` and the binary appends `/wezsesh/` (matching the §6.4 default), the actual path is 15+9+14=38 bytes — within budget for short paths but overflows for long-XDG-override or per-user-dir overrides. Lua check passes, runtime fails with `ENAMETOOLONG`.) | §6.4 v2.3: explicit contract — `opts.runtime_dir` IS the FINAL reply directory (INCLUDES `/wezsesh/` if isolation desired). Binary does NOT append `/wezsesh/` on top. Default auto-detection (when nil) constructs the path as `$XDG_RUNTIME_DIR/wezsesh/` (Linux) or `/tmp/wezsesh-<uid>/` (darwin). Documented in README + doctor output. |
| **Tilde expansion timing mismatch in SUN_PATH check** (Lua's `#` measures the literal `"~/foo/bar"` string while Go expands `~` against `$HOME` at runtime. A long `$HOME` — corporate-managed Macs with `/Users/employee.firstname.lastname/` — can blow past the ceiling after expansion, but the Lua check passed earlier.) | §6.4 v2.3: Lua-side `~` expansion BEFORE length measurement, mirroring Go's runtime expansion: `expanded = (wezterm.home_dir or os.getenv("HOME") or "") .. opts.runtime_dir:sub(2)` for `~/`-prefixed paths. |
| **`wezterm.target_triple:find("apple")` is a fragile substring match** (substring-match against `target_triple` could false-positive on hypothetical future triples like `aarch64-apple-linux`. Defense-in-depth.) | §6.4 v2.3: tightened regex to `wezterm.target_triple:match("%-apple%-darwin")` — anchors on the full Apple-darwin tail. |
| **`opts.runtime_dir` type validation missing** (user passing `runtime_dir = nil`/`123`/`{}` causes `#nil` to raise inside `apply_to_config`; outer pcall catches and degrades to a generic "wezsesh setup failed" toast — user never sees the helpful "must be a string path" guidance.) | §6.4 v2.3: explicit `assert(type(opts.runtime_dir) == "string", "WEZSESH_RUNTIME_DIR_TYPE: ...")` BEFORE the length check; sentinel-prefixed error caught by `apply_to_config`'s outer pcall and surfaced via toast. |
| **`$XDG_RUNTIME_DIR` fallback for non-systemd Linux undefined in v2.2** (Alpine, void, gentoo-without-systemd, raw SSH sessions without logind, container-based desktops without `pam_systemd` all leave `$XDG_RUNTIME_DIR` UNSET. v2.2 said "auto-detect $XDG_RUNTIME_DIR on Linux" without specifying a fallback.) | §6.4 v2.3: when `$XDG_RUNTIME_DIR` is unset on Linux, the Go binary falls back to `/tmp/wezsesh-<uid>/` (matching darwin pattern). README + doctor document. |
| **`$XDG_RUNTIME_DIR` ownership/permissions not pre-flight-checked** (per XDG spec, must be user-owned mode 0700 on tmpfs. Sysadmin override could violate; runtime `net.Listen` fails with EACCES, surfaces opaque `IPC_INIT_FAILED`.) | §6.4 / §7.3 v2.3: doctor probes `os.Stat($XDG_RUNTIME_DIR)`, reports owner UID + mode + FS type; warns on misconfiguration. |
| **`SwitchToWorkspace` poller predicate requires two-step CLI cross-reference** (`wezterm cli list --format json` has NO global active-workspace field — verified `wezterm/src/cli/list.rs::CliListResultItem`. Each tick must issue BOTH `cli list-clients` (for `focused_pane_id`) AND `cli list` (lookup that pane's `workspace`). The PRD §6.15 predicate prose was correct in concept but didn't make the two-step requirement explicit.) | §6.15 v2.3: explicit two-step query documented. Per-tick cost ~30ms fork-exec on a healthy mux; doubled from v2.2's single-step assumption. Multi-GUI-client disambiguation: pick client with most-recent `last_input`, tie-break on `client_id`. `MuxNotification::ActiveWorkspaceChanged` exists internally in mux but is NOT Lua-surfaced (verified — not in [documented window-events](https://wezterm.org/config/lua/window-events.html)); polling remains the only Lua-side mechanism. |
| **`cli activate-pane` does NOT switch the user's active workspace** (verified [`wezterm-mux-server-impl/src/sessionhandler.rs::SetFocusedPane`](https://raw.githubusercontent.com/wezterm/wezterm/main/wezterm-mux-server-impl/src/sessionhandler.rs): server-side handler calls `window.save_and_then_set_active(tab_idx)` + `tab.set_active_pane(&pane)` + `mux.notify(MuxNotification::PaneFocused(pane_id))` but does NOT call `mux.set_active_workspace_for_client(...)`. PRD revisions before v2.3 carried forward a wrong claim that "cross-workspace activation is server-side implicit"; never verified against source. `wezsesh find` selecting a cross-workspace pane was a no-op from the user's perspective.) | §6.13 / §6.20 v2.3: corrected. `wezsesh find` cross-workspace selection requires explicit two-phase sequence: Phase 1 — workspace switch (Lua dispatches `act.SwitchToWorkspace`, Go polls per §6.15); Phase 2 — `cli activate-pane`. Same-workspace selection skips Phase 1. CI assertion: a `find` selecting a cross-workspace pane MUST result in BOTH (a) the user being on the target workspace AND (b) the target pane being active. |
| **`sudo`/`doas` clears `$HOME` — Layer 2 path-prefix expansion uses wrong home dir** (uncommon but real in privilege-elevation diagnostic flows; `$HOME` becomes `/var/root` or `/root` instead of the user's actual home, causing prefix matching against `~/Library/...` to fail entirely.) | §6.19 / §7.3 v2.3: doctor reports resolved `$HOME` and warns if it differs from `os/user.Current().HomeDir` (which uses SUID/SGID-aware lookup). Recommendation: invoke wezsesh with `sudo -E` or `HOME=/Users/user wezsesh ...` for privilege elevation. |
| **Linux 3.15+ kernel baseline for F_OFD_SETLK undocumented as hard requirement in v2.2** (every supported distro ships with 5.x+, but pre-3.15 kernels return EINVAL on F_OFD_SETLK and the spec doesn't define behavior.) | §6.19 v2.3: hard requirement documented. Doctor probes `os.ReadFile("/proc/sys/kernel/osrelease")` and emits a clear error if older than 3.15. Optional EINVAL→F_SETLK runtime fallback deferred to v0.2 with clear WARN. |
| **Phase 1 polling re-resolves "most-recent client" per tick instead of pinning to the originating client** (§6.5 disambiguation rule applied per-tick lets a second connected GUI client become "most recent" mid-poll, flipping the predicate to track the wrong client's workspace. v2.3 prose said "pick the client with most-recent `last_input`" without specifying once-vs-per-tick; ambiguity could land as a runtime race that fires only when a second user types in another wezterm/mosh/SSH-mux session connected to the same mux.) | §6.5 / §6.13 / §6.15 v2.4: client pinning is mandatory. `StartSwitchPoller` captures `TargetClientID` at Phase 1 start; every subsequent tick looks up THAT client_id specifically in `cli list-clients --format json`. If the pinned client disconnects mid-poll, the predicate fails closed at the 5s ceiling (`MUX_UNREACHABLE`). CI test: a second client gaining "most-recent" status mid-poll MUST NOT flip the predicate. Spike C. |
| **Phase 1 predicate window-id scoping was implicit in §6.15 step 4 but not echoed in §6.13 cross-workspace prose** (the spec text correctly required `target_window_id` scoping in the success predicate, but the §6.13 Phase 1 description elided it. An implementer reading §6.13 alone could omit the window-id check, allowing a false-positive when the user closes the originating wezterm window mid-poll and the pinned client's focused_pane_id rolls over to a different window.) | §6.13 / §6.15 v2.4: explicit `window_id == TargetWindowID` check added to the Phase 1 step-3 cross-reference description. The predicate fires ONLY when both `workspace == target` AND `window_id == TargetWindowID`. CI test: closing the wezterm window mid-Phase-1 surfaces `MUX_UNREACHABLE`, not a phantom-success activation in another window. Spike C. |
| **`wezsesh find` cumulative wall-clock budget undocumented; UX progress indicator unspecified** (worst-case 5s Phase 1 + 2s Phase 2 = 7s end-to-end. Without a Phase-1 progress indicator, the user sees the TUI close on Enter and then nothing for up to 5 seconds — indistinguishable from "find is broken." A 7-second pause is long enough that users will start mashing keys.) | §6.13 v2.4: TUI MUST render a one-line progress status during Phase 1 (`Switching workspace...` → `Activating pane...`). 7s ceiling documented. Per-tick CPU cost (~30ms × 100 ticks = 60% of one core during the 5s slice) acknowledged; exponential backoff (50ms→500ms) deferred to v0.2 if user feedback reports concurrent-TUI contention. Spike C. |
| **Sentinel-prefixed Lua error included noisy file:line prefix in the user-visible toast** (Lua's `error(msg)` with default level=1 prepends `<file>:<line>: ` to the caught message; the toast text ended up as `init.lua:42: WEZSESH_SUN_PATH_OVERFLOW: runtime_dir too long...` — sentinel match still worked but the user-visible diagnostic was cluttered.) | §6.4 v2.4: use `error(msg, 0)` (level=0) to suppress the prefix. Substring match in the outer pcall still works (`string.find` is unanchored). Verified Lua 5.4 reference manual §6.1 / §6.4.1. Spike E. |
| **NextCloud Layer 2 anchor coverage was misleading** (v2.3's `~/Library/CloudStorage/Nextcloud*` only covers community-built File Provider variants, NOT the standard NextCloud Desktop client which syncs to `~/Nextcloud/`. The dominant NextCloud install was being silently missed.) | §6.7 / §6.19 v2.4: anchor list extended with `~/Nextcloud` (covers the standard client). Existing `~/Library/CloudStorage/Nextcloud*` retained for community FP variants. Provider-coverage scope explicitly documented per anchor (verified vs. best-effort vs. unverified). False-positive risk on user-created `~/Nextcloud/` that is NOT a sync root mitigated by Layer 1 statfs (will see APFS, not osxfuse, and the warning is downgraded). Spike D. |
| **Proton Drive and Seafile/SeaDrive Layer 2 anchors are best-effort and unverified against shipped clients** (v2.3 added these without verifying actual on-disk subdirectory naming. If the prefixes are wrong, they're harmless dead code; the documented false-negative class subsumes them.) | §6.19 v2.4: anchors retained but tagged "best-effort, unverified" inline. Hardening deferred until first user-report or v0.2+ when `pluginkit` discovery (already deferred) lands. Spike D. |
| **`wezterm.toast_notification` message-length and control-char-filtering behavior unverified by spec audit** (the spec surfaces SUN_PATH error messages of ~150 bytes via the toast layer; if wezterm truncates aggressively or doesn't filter control characters, the user-facing diagnostic could be cropped or — in a malicious-path scenario — escape-sequence-inject the wezterm UI. `string.format("%q")` already escapes the path component, providing defense-in-depth, but the toast layer's own behavior is not spec'd.) | §6.4 v2.4: documented as a runtime validation item. The `%q`-escaped path is safe regardless of toast-layer filtering. Truncation behavior on long paths to be measured during integration testing; if aggressive, the spec will switch to a relative-tail abbreviation. Spike E. |
| **`wezterm.home_dir` always-truthy property documented inline** (round-13 Spike B verified the constant is initialized at wezterm boot from `dirs_next::home_dir().expect(...)` — wezterm panics if lookup fails, so the runtime value is always a string. The `or os.getenv("HOME") or ""` fallback chain in §6.4 is redundant but harmless; documented as forward-compat hygiene.) | §6.4 v2.4: documented inline. No change to the fallback chain. Spike B. |
| **FreeBSD/OpenBSD/NetBSD coverage of the //go:build !linux branch was undocumented in v2.3** (the build-tag plan is correct — `unix.F_SETLK` is defined and `unix.F_OFD_SETLK` is absent on all three — but v2.3 didn't state v0.1's supported-target matrix or what happens on those platforms.) | §6.19 v2.4: explicit non-Linux platform coverage statement. v0.1 ships for darwin (Apple Silicon + Intel) and Linux (amd64 + arm64); BSD targets compile under the same build-tag plan but are best-effort, not a v0.1 commitment. CI matrix lists the four supported triples. Spike A. |
| **`O_CLOEXEC` mandate's relationship to Go's os/exec default behavior was unstated** (Go's runtime marks all inherited fds CloseOnExec via the "mark all, unmark intentional" strategy in `src/syscall/exec_unix.go` since 1.10+. For the os/exec.Start path specifically, our O_CLOEXEC mandate is superseded by Go's default. The mandate remains as defense-in-depth, but the rationale was not documented — a future maintainer might remove the flag thinking it's redundant.) | §6.19 v2.4: defense-in-depth rationale documented inline. Three reasons the O_CLOEXEC flag is retained despite Go's default: (a) direct syscall escape paths (`syscall.ForkExec`, raw `fork(2)`) are not protected; (b) the brief window between `Openat` and the runtime's CloseOnExec marking is closed unconditionally by O_CLOEXEC at open time; (c) refactor resilience — enforcing the flag at every open call site catches accidental regressions. Constant availability across linux/darwin/freebsd/openbsd/netbsd verified. Spike A. |

## 10. Success criteria

- I (the author) replace my current `Leader+Shift+W` Lua manager with `wezsesh` and don't miss the old one within a week.
- **Cold start p95 < 350 ms; warm start p95 < 50 ms on darwin-arm64.** Cold start is bounded by darwin's per-page mmap + adhoc-codesign verification of an unsigned 4 MB Go binary (measured at 200-260ms baseline that we cannot beat in user code). Warm cache (steady-state developer use, hourly invocations) is the realistic operating regime. A daemon-mode opt-in (deferred per §8.2) would defer the cold cost to once-per-wezterm-session.
- One external user (friend / coworker) installs it and reports back without needing hand-holding.
- `wezsesh doctor` resolves >80% of installation issues without further interaction.
- Issues filed on GitHub get triaged within a week.

## 11. Out of scope (forever)

- A GUI app. This is a terminal tool.
- Cloud sync of snapshots. Use `git` if you need that.
- Plugin marketplace UX. wezterm's plugin model is git-URL-based and that's fine.
- Replacing `resurrect`. They have years of edge cases handled; we benefit from their stability.
- Auto-downloading the binary. Install matches wezterm plugin convention.

---

**Status**: design v2.4 (round-13 v2.3-edits-revalidation / two-phase-find-race-conditions / build-tag-portability-confirmation / Lua-API-existence-confirmation / sentinel-error-pcall-roundtrip audit complete; **audit phase CLOSED — next phase is code-and-test**).
**Owner**: Grady Saccullo.
**Last updated**: 2026-04-30.
**See also**:
- `PRD_research_findings.md` — round 1 research (drove §6.4 OSC pivot, §6.5, §6.6 schema, §6.11, §6.12, §6.13).
- `PRD_research_findings_v2.md` — round 2 deep research (drove §6.4 socket return-path, §6.7 race mitigation, §6.14 security model, §6.15 reload durability, §7.1 version handshake, encryption degraded mode).
- `PRD_research_findings_v3.md` — round 3 validation (drove `wezsesh reply` subcommand, dropped `WEZSESH_PANE_ID`, GLOBAL keying clarification, `SNAPSHOT_LOAD_FAILED` code, `resurrect.error` subscription, honest cold-start SLO, bubbletea stack pins, daemon-mode deferral).
- `PRD_research_findings_v4.md` — round 4 deep validation (eight spikes: bubbletea Cmd ordering, reload-drop refutation, wezterm version pin, q/Esc patterns, GLOBAL integer-key bug, run_child_process semantics, CLI JSON schema, SwitchToWorkspace + resurrect restore).
- `PRD_research_findings_v5.md` — round 5 architecture + attack-vector audit (four spikes + six analyses: restore-latency vs IPC ceiling, snapshot-name security/correctness, OSC parser edges, smart_workspace_switcher events, seen_ids growth, Lua-vs-RE2 patterns, signal handling, HMAC compare, snapshot file caps, state-file concurrency).
- `PRD_research_findings_v6.md` — round 6 deep RCE / filesystem / hook / lifecycle audit (four spikes: resurrect snapshot argv RCE, filesystem TOCTOU + symlinks, hook execution gaps, plugin lifecycle edges).
- `PRD_research_findings_v7.md` — round 7 secondary-defense / supply-chain / lifecycle audit (four spikes: path picker exec contract, control-char injection through `pane:send_text`, dependency CVE / charmbracelet v1→v2, goroutine lifecycle leaks).
- `PRD_research_findings_v8.md` — round 8 canonical-JSON / crypto / wezterm-CLI / lifecycle audit (four spikes: canonical-JSON Go↔Lua byte-equality, vendored pure_lua_SHA correctness, wezterm CLI failure modes / timeouts, wezterm Lua API lifecycle reliability).
- `PRD_research_findings_v9.md` — round 9 bubbletea-v2 / payload-budget / TOCTOU / Lua-payload-robustness audit (four spikes: bubbletea v2.0.6 migration semantics, OSC payload size worst-case under v8 escape rules, state-mutation TOCTOU + concurrent-TUI hazards, Lua-side untrusted-payload robustness).
- `PRD_research_findings_v10.md` — round 10 fcntl-semantics / API-revalidation / GLOBAL-concurrency / project-sidecar-trust audit (four spikes: POSIX advisory lock semantics on darwin/APFS/NFS/cloud-sync, re-validation of every cited Lua-side API after the v9 `tea.After` fabrication finding, `wezterm.GLOBAL` concurrent mutation race in the replay guard, project sidecar trust enforcement + network-FS hang surfaces).
- `PRD_research_findings_v11.md` — round 11 v2.1-edits-revalidation / File-Provider-cloud-sync / resurrect-API-runtime-validation / encryption-hang-surface audit (four spikes: `safefs.IsNetworkFS` against modern macOS File Provider cloud-sync, `F_SETLK` advisory-lock semantics + multi-fd close footgun + encryption hang, full resurrect API runtime validation including `default_on_pane_restore` callback signature, bubbletea v2 PIPE_BUF interleaving + `SUN_PATH` config-load validation).
- `PRD_research_findings_v12.md` — round 12 v2.2-edits-revalidation / Layer-2-heuristic-robustness / F_OFD_SETLK-portability / runtime_dir-TOCTOU / cli-activate-pane-cross-workspace audit (five spikes: File Provider path-prefix heuristic robustness against EvalSymlinks loops + NFC normalization + third-party providers + symlinks-in-snapshot-dir, F_OFD_SETLK build-tag pattern + O_CLOEXEC + Linux kernel baseline, runtime_dir SUN_PATH +14 budget contract + tilde-expansion + target_triple regex + XDG fallback, SwitchToWorkspace polling cross-reference + multi-client disambiguation, cli activate-pane SetFocusedPane server handler reading — definitively refuted v1.x's "server-side implicit" cross-workspace claim).
- `PRD_research_findings_v13.md` — round 13 v2.3-edits-revalidation / two-phase-find-race-conditions / build-tag-portability-confirmation / Lua-API-existence-confirmation / sentinel-error-pcall-roundtrip audit (five spikes targeting v2.3's added surfaces only: F_OFD_SETLK build-tag + O_CLOEXEC portability across darwin/Linux/FreeBSD, wezterm.home_dir API existence + Lua pattern syntax + target_triple variants, two-phase wezsesh find race conditions including client pinning + window-id scoping + cumulative latency budget, extended Layer 2 prefix coverage + runtime_dir contract trailing-slash byte counts, sentinel-prefixed error pattern survival through pcall/tostring round-trip). **Round 13 found ZERO BLOCKERs and TWO HIGHs against v2.3 edits — both in the v2.3-introduced two-phase find flow (client pinning, window-id scoping). Per the round-12 termination thesis, this confirms that audit-only iteration introduces drift as fast as it closes it; the audit phase is closed regardless of severity. Next phase is code-and-test.**

**v2.4 changes** (round-13 audit):

- **§6.13 / §6.15 / §9 (HIGH, correctness — v2.3 spec drift)**: Phase 1 polling re-resolved "most-recent client" per tick (per §6.5's disambiguation rule applied naively per-tick) instead of pinning the originating client at Phase 1 start. A second connected GUI client (mosh, SSH-mux, another local wezterm) becoming "most recent" mid-poll would flip the predicate to track the wrong client's workspace transition. v2.4 mandates client pinning: `StartSwitchPoller` captures `TargetClientID` at Phase 1 start; every subsequent tick looks up THAT client_id specifically in `cli list-clients`. If the pinned client disconnects mid-poll, the predicate fails closed at the 5s ceiling. CI test: a second client gaining "most-recent" status mid-poll MUST NOT flip the predicate. Spike C.

- **§6.13 / §6.15 / §9 (HIGH, correctness — v2.3 prose gap)**: Phase 1 predicate window-id scoping was implicit in §6.15 step 4 (`workspace appears in target_window_id specifically`) but NOT echoed in §6.13 cross-workspace prose. An implementer reading §6.13 in isolation could omit the `window_id == TargetWindowID` check, allowing a false-positive when the user closes the originating wezterm window mid-poll and the pinned client's focused_pane_id rolls over to another window. v2.4 makes the window-id check explicit at every step of the §6.13 / §6.15 description. Predicate fires ONLY when `workspace == target` AND `window_id == TargetWindowID`. CI test: closing the wezterm window mid-Phase-1 surfaces `MUX_UNREACHABLE`, not a phantom-success activation. Spike C.

- **§6.13 / §9 (MEDIUM, UX)**: `wezsesh find` cumulative wall-clock budget (5s Phase 1 + 2s Phase 2 = 7s worst case) was undocumented and the TUI had no progress indicator during the 5s polling window — user sees the TUI close on Enter and then nothing for up to 5 seconds, indistinguishable from "find is broken." v2.4: TUI MUST render a one-line progress status (`Switching workspace...` → `Activating pane...`) during Phase 1. 7s ceiling documented. Per-tick polling cost (~30ms × 100 ticks = 60% of one core during the 5s slice) acknowledged; exponential backoff (50ms→500ms) deferred to v0.2 if user feedback reports concurrent-TUI contention. Spike C.

- **§6.4 / §9 (MEDIUM, UX)**: sentinel-prefixed Lua errors used the default `error(msg)` form (level=1), which prepends `<file>:<line>: ` to the caught message. The user-facing toast then displayed `init.lua:42: WEZSESH_SUN_PATH_OVERFLOW: ...` — sentinel substring match still worked (`string.find` is unanchored, verified Lua 5.4 reference manual §6.4.1) but the diagnostic was visually cluttered. v2.4: switch all sentinel-prefixed errors to `error(msg, 0)` (level=0) which suppresses the file:line prefix. Spike E.

- **§6.7 / §6.19 / §9 (MEDIUM, doc-clarity)**: Layer 2 NextCloud anchor coverage was misleading — v2.3's `~/Library/CloudStorage/Nextcloud*` only matches community-built File Provider variants. The standard NextCloud Desktop client (the dominant install) syncs to `~/Nextcloud/`, completely outside `~/Library/CloudStorage/`. v2.4: anchor list extended with `~/Nextcloud` (covers the standard client). Provider-coverage scope explicitly tagged per anchor (verified vs. best-effort vs. unverified). False-positive risk on user-created `~/Nextcloud/` mitigated by Layer 1 statfs check (sees APFS, not osxfuse, downgrades the warning). Spike D.

- **§6.19 / §9 (MEDIUM, doc-clarity)**: Proton Drive and Seafile/SeaDrive Layer 2 anchors were added in v2.3 without verifying actual on-disk subdirectory naming against shipped clients. v2.4: anchors retained but tagged "best-effort, unverified" inline. If the prefixes don't match real paths, they're harmless dead code (no false-positives, just no benefit); the documented false-negative class subsumes them. First-user-report or `pluginkit`-discovery (deferred to v0.2+) will resolve. Spike D.

- **§6.19 / §9 (MEDIUM, defense-in-depth rationale)**: the v2.3 spec mandated `unix.O_CLOEXEC` on every `unix.Openat` in `internal/safefs/` without documenting WHY this is needed when Go's `os/exec.Cmd.Start()` runtime already marks inherited fds CloseOnExec via the "mark all, unmark intentional" strategy in `src/syscall/exec_unix.go` (since Go 1.10+; verified in round 13). A future maintainer reading the spec might remove the flag thinking it's redundant. v2.4: defense-in-depth rationale documented inline — three reasons the flag is retained: (a) direct syscall escape paths (`syscall.ForkExec`, raw `fork(2)`) are not protected by the runtime; (b) the brief window between `Openat` and the runtime's CloseOnExec marking is closed unconditionally by O_CLOEXEC at open time; (c) refactor resilience — enforcing at every open call site catches accidental regressions. Spike A.

- **§6.19 / §9 (MEDIUM, doc-clarity)**: FreeBSD/OpenBSD/NetBSD coverage of the `//go:build !linux` branch was undocumented in v2.3. The build-tag plan IS correct — `unix.F_SETLK` is defined and `unix.F_OFD_SETLK` is absent on all three BSD variants (verified `golang.org/x/sys/unix/zerrors_*.go`). But v2.3 didn't state v0.1's supported-target matrix or what happens on those platforms. v2.4: explicit statement — v0.1 ships for darwin (Apple Silicon + Intel) and Linux (amd64 + arm64); BSD targets compile under the same build-tag plan but are best-effort, not a v0.1 commitment. CI matrix lists the four supported triples. Spike A.

- **§6.4 / §9 (nit, doc-clarity)**: `wezterm.home_dir` was confirmed via round-13 source reading to be initialized at wezterm boot from `dirs_next::home_dir().expect(...)` and exposed through `config/src/lua.rs` as the `home_dir` field on the `wezterm` module. It panics at boot if lookup fails, so the runtime value is always a string. The `or os.getenv("HOME") or ""` fallback chain in §6.4's example is redundant but harmless; v2.4 documents inline as forward-compat hygiene. No code change. Spike B.

- **§6.4 / §9 (nit, runtime-validation)**: `wezterm.toast_notification` message-length and control-character-filtering behavior was not verifiable from spec audit (wezterm docs cover the API surface but not internal limits). The spec surfaces SUN_PATH error messages of ~150 bytes; if wezterm truncates aggressively or doesn't filter control characters, the user-facing diagnostic could be cropped — though `string.format("%q")` already escapes the path component, providing defense-in-depth against escape-sequence injection. v2.4: documented as a runtime-validation item to measure during integration testing. If aggressive truncation is observed, switch to a relative-tail abbreviation. Spike E.

- **§9 risks**: 11 new rows covering all v2.4 v2.3-edit-drift items.

**v2.3 changes** (round-12 audit):

- **§6.19 / §9 (BLOCKER, portability)**: `unix.F_OFD_SETLK` is defined ONLY in `golang.org/x/sys/unix/zerrors_linux.go`, NOT in `zerrors_darwin_*.go` (verified). v2.2's spec said "use F_OFD_SETLK on Linux when available" without mandating the build-tag split — naive `runtime.GOOS == "linux"` branching FAILS TO COMPILE on darwin (compiler resolves all constants at compile time). v2.3 mandates `internal/safefs/lock_linux.go` (//go:build linux) + `internal/safefs/lock_other.go` (//go:build !linux) split. CI lint rule rejects any `unix.F_OFD_SETLK` reference outside `lock_linux.go`. Spike 2.

- **§6.19 / §9 (BLOCKER, security)**: `AcquireExclusive`'s lock fd lacked `O_CLOEXEC`. The fd is inherited by every `os/exec` child (`wezsesh reply` from Lua per request, `wezsesh keygen` per session). For OFD locks, the lock is owned by the open file description shared across forks — a child that calls `fcntl(F_OFD_UNLCK)` on the inherited fd releases the OFD lock from the parent's perspective. For F_SETLK on darwin, fd-table inheritance is a hygiene leak. Verified `man fcntl(2)` Linux Open file description locks section. v2.3: every `unix.Openat` in `internal/safefs/` MUST include `unix.O_CLOEXEC` in the flag set. CI test verifies child process doesn't inherit the lock fd. Spike 2.

- **§6.7 / §6.19 / §7.4 / §9 (BLOCKER, reliability)**: `filepath.EvalSymlinks` in Layer 2 is a startup-hang surface — Go has no portable loop limit (verified [golang/go#73572](https://github.com/golang/go/issues/73572) magic-link recursion). On a circular symlink chain, on a hung NFS ancestor, or on a dataless cloud-only File Provider ancestor that triggers daemon-mediated download, `EvalSymlinks` stalls for tens of seconds — exactly the inversion v2.2 closed for `Statfs` but reintroduced for symlinks. v2.3: wrap in goroutine under `context.WithTimeout(500ms)`; on overrun OR on `Readlink` permission errors (`EACCES` ancestor), fall back to unresolved/partial path + non-fatal WARN. Treat unresolved as non-network (conservative). Spike 1.

- **§6.7 / §6.19 / §9 (BLOCKER, correctness)**: Layer 2 prefix list missed third-party File Provider extensions (Box for Mac since 2024, NextCloud community builds, ProtonDrive experimental, Seafile/SeaDrive). Plus: custom mount paths and symlinks INSIDE the snapshot dir pointing to cloud-synced data are silent false-negatives. v2.3: extended prefix list with `Box*`, `Nextcloud*`, `Proton*`, `Seafile*`. Documented false-negative classes explicitly (custom paths, symlinks-in-dir, TOCTOU between check and use). Recommendation strengthened to "ALL contents of the snapshot dir must be local-only." `pluginkit -m -p com.apple.fileprovider-nonui` for dynamic discovery deferred to v0.2+ with explicit hang-risk note (would itself fork and can hang on stalled `fileproviderd`). Spike 1.

- **§6.4 / §9 (BLOCKER, correctness)**: `+14` SUN_PATH budget undercounts by 9 bytes if user-supplied `runtime_dir` lacks `/wezsesh/` and the binary appends it (matching the §6.4 default pattern). v2.3 explicitly defines the contract: `opts.runtime_dir` IS the FINAL reply directory (INCLUDES `/wezsesh/` if isolation desired); the binary does NOT append `/wezsesh/`. Default auto-detection (when nil) constructs the path. Documented in README + doctor output. Spike 3.

- **§6.13 / §6.20 / §9 (BLOCKER, correctness)**: `cli activate-pane` does NOT switch the user's active workspace. Verified [`wezterm-mux-server-impl/src/sessionhandler.rs::SetFocusedPane`](https://raw.githubusercontent.com/wezterm/wezterm/main/wezterm-mux-server-impl/src/sessionhandler.rs): server-side handler calls `window.save_and_then_set_active(tab_idx)` + `tab.set_active_pane(&pane)` + `mux.notify(MuxNotification::PaneFocused(pane_id))` but does NOT call `mux.set_active_workspace_for_client(...)`. PRD revisions before v2.3 carried forward a wrong claim that "cross-workspace activation is server-side implicit"; never verified against source until round 12. `wezsesh find` selecting a cross-workspace pane was a no-op from the user's perspective. v2.3: corrected. `wezsesh find` cross-workspace selection requires two-phase sequence: Phase 1 — workspace switch (Lua dispatches `act.SwitchToWorkspace`, Go polls per §6.15); Phase 2 — `cli activate-pane`. Same-workspace selection skips Phase 1. CI assertion enforces. Spike 5.

- **§6.19 (HIGH, correctness)**: `EvalSymlinks` `EACCES`/`EPERM` permission errors on ancestor symlinks not handled in v2.2. v2.3: on Readlink error, use unresolved/partial path + WARN. Spike 1.

- **§6.19 (HIGH, correctness)**: NFC vs NFD normalization mismatch — APFS preserves on-disk filename bytes, Finder-created dirs typically use NFD, prefix anchors constructed with NFC. v2.3 mandates the same normalization on BOTH the resolved path AND the tilde-expanded prefix anchors at the same step via `golang.org/x/text/unicode/norm.NFC.String`. Spike 1.

- **§6.19 (HIGH, correctness)**: User-renamed `~/iCloud Drive` alias bypasses detection (Finder blocks rename but `mv` works). v2.3: when matched, also `EvalSymlinks` to the resolved target — alias bottoms out at Mobile Documents regardless of display name. Spike 1.

- **§6.19 / §7.3 (HIGH, correctness)**: `sudo`/`doas` clears `$HOME`, breaking Layer 2 expansion. v2.3: doctor reports resolved `$HOME` and warns if it differs from `os/user.Current().HomeDir`. README documents `sudo -E` recommendation. Spike 1.

- **§6.7 / §6.19 (HIGH, false-positive)**: `/Volumes/GoogleDrive*` prefix is vestigial on Apple Silicon (Google Drive Desktop 2024+ uses File Provider exclusively); produces benign false-positives for unrelated `/Volumes/GoogleDriveBackup`-named external mounts. v2.3 documents. Spike 1.

- **§6.4 (HIGH, UX)**: tilde expansion timing mismatch — Lua's `#` measures literal `~/...` while Go expands `~` against `$HOME` at runtime; expanded form is longer. v2.3: Lua-side `~` expansion BEFORE length check via `wezterm.home_dir` or `os.getenv("HOME")`. Spike 3.

- **§6.4 (HIGH, correctness)**: `wezterm.target_triple:find("apple")` is a fragile substring match. v2.3: tightened regex to `target_triple:match("%-apple%-darwin")` — anchors on the full Apple-darwin tail. Spike 3.

- **§6.4 (HIGH, UX)**: `opts.runtime_dir` type validation missing — user passing `nil`/`123`/`{}` raises inside `apply_to_config`; outer pcall degrades to a generic toast. v2.3: explicit `assert(type(opts.runtime_dir) == "string", "WEZSESH_RUNTIME_DIR_TYPE: ...")` BEFORE the length check; sentinel-prefixed error caught by outer pcall and surfaced via 10s toast. Spike 3.

- **§6.4 (HIGH, correctness)**: `$XDG_RUNTIME_DIR` fallback for non-systemd Linux undefined in v2.2. v2.3: when unset, fall back to `/tmp/wezsesh-<uid>/` (matching darwin pattern). README + doctor document. Spike 3.

- **§6.4 / §7.3 (HIGH, UX)**: `$XDG_RUNTIME_DIR` ownership/permissions not pre-flight-checked. v2.3: doctor reports owner UID, mode, FS type; warns on misconfiguration. Spike 3.

- **§6.15 (HIGH, clarity)**: `cli list --format json` has no top-level active-workspace field; poller predicate requires two-step CLI cross-reference (`cli list-clients` for `focused_pane_id` → `cli list` for that pane's workspace). v2.2 prose was correct in concept but didn't make the two-step explicit. v2.3 documents. Multi-GUI-client disambiguation: pick client with most-recent `last_input`, tie-break on `client_id`. Spike 4.

- **§6.19 (MEDIUM, hygiene)**: case sensitivity — APFS is case-insensitive but case-preserving by default; Go's `filepath.Match` is case-sensitive. v2.3: `strings.EqualFold` on darwin, case-sensitive on Linux. Spike 1.

- **§6.19 (MEDIUM, hygiene)**: trailing slash in `$HOME` produces double-slash paths. v2.3: mandatory `filepath.Clean` after expansion. Spike 1.

- **§6.7 / §6.19 (MEDIUM, TOCTOU)**: Layer 2 evaluated at TUI open; iCloud sync state can change between check and use. Documented. Spike 1.

- **§6.19 (MEDIUM, assumption)**: Desktop & Documents detection assumes Apple symlink implementation; if Apple switches to bind-mounts, breaks. Documented. Spike 1.

- **§6.19 (MEDIUM, baseline)**: Linux 3.15+ kernel baseline for F_OFD_SETLK undocumented as hard requirement in v2.2. v2.3: hard requirement documented; doctor probes `/proc/sys/kernel/osrelease`. Optional EINVAL→F_SETLK fallback deferred to v0.2. Spike 2.

- **§6.4 (MEDIUM, UX)**: outer pcall in `apply_to_config` hides the clear SUN_PATH error message. v2.3: SUN_PATH errors use a sentinel-prefixed string (`WEZSESH_SUN_PATH_OVERFLOW: ...`) that the outer catch detects and surfaces via 10s toast. Spike 3.

- **§6.4 (MEDIUM, race)**: filesystem remount race between Lua check and Go binary; mitigated by Go re-validation. Document. Spike 3.

- **§6.4 (MEDIUM, TOCTOU)**: symlink in `runtime_dir` not pre-flight-checked. v2.3: `safefs.VerifyDir(parent_of_runtime_dir)` before `net.Listen`. Spike 3.

- **§6.5 (MEDIUM, clarity)**: multi-GUI-client disambiguation rule — pick most-recent `last_input`, tie-break on `client_id`. Spike 4.

- **§9 risks**: 16 new rows covering all v2.3 BLOCKERs and the path-prefix robustness / lock fd hygiene / runtime_dir contract / cross-workspace activation hardening.

**v2.2 changes** (round-11 audit):

- **§6.7 / §6.19 / §9 (BLOCKER, correctness)**: `safefs.IsNetworkFS` cannot detect modern macOS cloud-sync. iCloud Drive, Dropbox 2024+, Google Drive Desktop 2024+ all migrated from osxfuse to Apple's File Provider framework — File Provider–backed paths report `f_fstypename = "apfs"` indistinguishable from local APFS (verified [TidBITS 2023 File Provider migration](https://tidbits.com/2023/03/10/apples-file-provider-forces-mac-cloud-storage-changes/), [eclecticlight 2023 Sonoma iCloud Drive](https://eclecticlight.co/2023/10/25/macos-sonoma-has-changed-icloud-drive-radically/)). v2.1's `osxfuse`-only matching gives ZERO coverage for the dominant macOS cloud-sync risk. v2.2 adds two-layer detection: Layer 1 (`Statfs`) keeps the network-FS list and adds `"fuse"` (was missing in v2.1). Layer 2 path-prefix heuristic added for File Provider extensions: `~/Library/Mobile Documents/`, `~/Library/CloudStorage/{iCloud~*,Dropbox*,GoogleDrive*,OneDrive*}`, `~/iCloud Drive`, `~/Dropbox`, `~/Google Drive`, `/Volumes/GoogleDrive*`, plus iCloud-redirected `~/Desktop`/`~/Documents`. Documented false-negative class: cloud-sync paths NOT on the prefix list bypass detection — best-effort warning, not a guarantee.
- **§6.18 / §9 (BLOCKER, security)**: resurrect's `default_on_pane_restore` callback signature was wrong in PRD v1.7–v2.1. PRD said `(pane, pane_tree)`; verified actual signature is `(pane_tree)` only with pane accessed as `pane_tree.pane` ([resurrect tab_state.lua](https://raw.githubusercontent.com/MLFlexer/resurrect.wezterm/main/plugin/resurrect/tab_state.lua) line ~95). An `on_pane_restore(pane, pane_tree)` two-argument hook installed per the v2.1 spec would crash on first restore, fail-open, and the §6.18 RCE defense would silently bypass. v2.2 corrects the signature, requires explicit fail-CLOSED on hook crash (caught by outer `pcall`, send `\r\n` only, never call resurrect's default), and adds a CI assertion that a hook that raises must NOT result in `pane:send_text(shell_join_args(argv))`.
- **§6.18 (HIGH, hygiene)**: argv indexing was inconsistent in PRD prose. Some sections said `argv[0]`, others `argv[1]`. Lua arrays are 1-based and resurrect uses Lua convention: `argv[1]` IS the program name, opposite of C-convention `argv[0]`. The defense was actually checking the right element but the prose ambiguity could mislead implementers. v2.2 normalizes to `argv[1]` everywhere with explicit note that this is the program (NOT the first argument).
- **§6.19 / §9 (HIGH, correctness)**: POSIX advisory-lock multi-fd close footgun. Verified Linux `fcntl_locking(2)` and macOS `fcntl(2)`: "If a process closes any file descriptor referring to a file, then all of the process's locks on that file are released, regardless of the file descriptor(s) on which the locks were obtained." A latent footgun: any code path inside `AcquireExclusive`'s critical section that opens-and-closes a second fd to the locked path silently drops the lock without notification. v2.2 adds explicit fd-hygiene contract on `AcquireExclusive`. Lint rule forbids re-opening the same path while the lock fd is live; reads during the hold use the locked fd (pread). Linux uses `F_OFD_SETLK` (Open File Description locks; Linux ≥ 3.15) which avoid the footgun by binding to the fd, not the process. darwin/FreeBSD lack OFD locks — discipline-only on those platforms; CI test demonstrates the difference.
- **§7.4 / §9 (HIGH, reliability)**: resurrect encryption pipeline can hang the wezterm GUI on `save`. Verified [resurrect encryption.lua](https://raw.githubusercontent.com/MLFlexer/resurrect.wezterm/main/plugin/resurrect/encryption.lua) shells out via `wezterm.run_child_process({"age", "-e", ...})` or `gpg` with NO timeout. If gpg-agent is unresponsive (locked YubiKey, missing pinentry, smartcard daemon stalled), the encrypt subprocess blocks indefinitely, suspending wezterm's config-eval coroutine and freezing the entire GUI. v2.1's §7.4 hang-surfaces table omitted this. v2.2 adds the encryption pipeline as a residual hang surface; doctor probes encryption configuration and runs a non-blocking `<provider> --version` probe with 2s timeout; surfaces `ENCRYPTION_AGENT_SLOW` (additive to §6.3) as a non-fatal hint. README requires users with encryption to verify gpg-agent / pinentry / smartcard daemon health. The hang itself is uncatchable from wezsesh — upstream resurrect's process. Future hardening: contribute a timeout wrapper to resurrect upstream.
- **§6.4 / §9 (HIGH, UX)**: `runtime_dir` overrides not validated against AF_UNIX `SUN_PATH` ceiling (104 darwin / 108 Linux). v2.1 documented the ceiling for the default path but did not require `apply_to_config` to validate user overrides. v2.2 adds Lua-side validation in `apply_to_config`: `len(opts.runtime_dir) + 14 ≤ ceiling`; raises a config error with exact remediation message. Go binary independently re-validates at runtime as defense-in-depth, surfacing new error code `IPC_INIT_FAILED` (additive to §6.3).
- **§6.19 / §9 (HIGH, reliability)**: `Statfs` itself can hang on a hung NFS server. The `safefs.IsNetworkFS` helper designed to warn about hang surfaces would itself hang at TUI launch on a hung NFS path before any UI renders ([Red Hat NFS hang KB](https://access.redhat.com/solutions/28211)). v2.2: `IsNetworkFS` runs `Statfs` in a goroutine under `context.WithTimeout(2s)`; on overrun, returns `("unknown", "statfs-timeout", true, nil)` — treats unprovable paths as network to err on the side of cautionary WARN. Goroutine leaks until kernel returns (Go has no portable way to interrupt a blocked syscall); acceptable nuisance for v0.1. Documented in §7.4.
- **§6.7 / §9 (MEDIUM, clarity)**: POSIX advisory locks do not block `io.open(w+)` from another process. Advisory locks are observed only by code calling `fcntl(F_GETLK)`/`fcntl(F_SETLK)`; resurrect's writer in wezterm's process uses bare `io.open` and does NOT consult locks. v2.1 prose suggested the lock prevented "concurrent writes" — true only across wezsesh binaries, not against resurrect's own writer. v2.2 explicit clarification: the lock is a "binary-instance gate", not a "physical write barrier". Sound under the assumption that resurrect's `save_state` is invoked ONLY through wezsesh's IPC path; a user calling `resurrect.state_manager.save_state` directly bypasses the lock. Documented as v0.1 limitation.
- **§6.4 / §9 (MEDIUM, cosmetic)**: PIPE_BUF atomicity caveat for "two fds, kernel serializes". Kernel atomicity is per-`write(2)` syscall up to PIPE_BUF (4 KB on Linux). Bubbletea full-redraw frames on resize can exceed PIPE_BUF; concurrent OSC writes interleave at the boundary between bubbletea's two underlying syscalls, producing visibly glitchy frames during resize. Risk is cosmetic — both byte streams remain well-formed escape sequences. v2.2 clarifies the boundary; the 4 KB OSC ceiling (§6.3) keeps our writes atomic.
- **§6.6 (MEDIUM, accuracy)**: resurrect snapshot `process` shape drift characterization corrected. PRD v2.1 said "both string-shape and object-shape in the wild"; round 11 verified resurrect main branch now consistently writes object-shape only — string-shape only appears in pre-2024-08 saved files in users' state dirs. Tolerant Go parser still required for legacy snapshots, but the in-the-wild characterization was inaccurate. README schema lags implementation; treat resurrect source as authoritative.
- **§6.3 (additive)**: new error codes `IPC_INIT_FAILED` and `ENCRYPTION_AGENT_SLOW`.
- **§9 risks**: 10 new rows covering all v2.2 BLOCKERs and the FS-detection / lock-hygiene / encryption-hang / config-validation hardening.

**v2.1 changes** (round-10 audit):

- **§6.7 / §6.19 / §9 (BLOCKER, correctness)**: v2.0's `safefs.AcquireExclusive` spec used `fcntl(F_SETLKW)` (blocking) plus a "SIGALRM-equivalent watchdog" — that pattern is unimplementable on darwin: `fcntl` syscalls auto-restart on `SA_RESTART` signals (FreeBSD/Darwin behavior; verified [`man 2 fcntl`](https://man.freebsd.org/cgi/man.cgi?query=fcntl&sektion=2)), so a watchdog signal does NOT interrupt the blocked syscall. Replaced with non-blocking `F_SETLK` polled with 10ms→100ms exponential backoff under `context.WithTimeout(parent, 5s)`. WARN-at-1s/3s logging exposes contended waits (POSIX advisory locks have no fairness guarantee — verified Pendleton 2010). Signature changed: `AcquireExclusive(ctx context.Context, path string)`.
- **§6.7 / §7.3 / §9 (HIGH, correctness)**: `safefs.IsNetworkFS` runtime detection added. NFS / SMB / SSHFS / autofs / cloud-sync (osxfuse — iCloud Drive / Dropbox / Google Drive) detected via `unix.Statfs` + f_type / f_fstypename. One-time WARN at TUI open per detected condition: NFS notes potential cross-host serialization failure; cloud-sync notes lock-bypass via atomic-replace by sync agents. Operation NOT refused; user makes informed choice. `wezsesh doctor` reports FS type per path.
- **§6.19 / §9 (HIGH, correctness)**: `O_NOFOLLOW` only protects the FINAL path component — a symlink at any earlier path component is silently followed (verified [LWN on symlink TOCTOU](https://lwn.net/Articles/899543/)). Every site that opens a path under user-controllable directories MUST first `safefs.VerifyDir(parentDir)` → `unix.Openat(dirfd, basename, O_NOFOLLOW)`. `AcquireExclusive` updated to use this pattern. Lint rule forbids `os.OpenFile(path, O_NOFOLLOW)` in `internal/safefs/`-using packages.
- **§6.19 / §9 (BLOCKER, reliability)**: every read in `internal/state/` and `internal/trust/` MUST be wrapped in `context.WithTimeout(2 * time.Second)`. Defaults to `~/.local/state/` and `~/.local/share/` — under `$HOME`. If `$HOME` is on autofs / NFS / SMB / SSHFS, `open(2)` blocks for 30–60s in the kernel awaiting an unresponsive server; without the wrapper, every doctor check or state.json read hangs the binary indefinitely. New error code `XDG_PATH_TIMEOUT` (additive to §6.3).
- **§7.1 (HIGH, reliability)**: v1.8's "fork-free = no-hang" claim corrected. The pre-flight `io.open` check eliminates fork overhead and binary-execution-path hang triggers (codesign / dyld / Go runtime init), but does NOT eliminate path-resolution hangs — `open(2)` itself blocks on network mounts. Documented as residual hang risk; README install guide recommends local-disk paths for users with network-mounted home directories.
- **§7.4 (NEW, HIGH, reliability)**: platform hang risks table documents residual surfaces no Go context can cancel: `io.open` on network mount, iCloud Drive cloud-only files (Optimize Storage), `run_child_process` for keygen on network-mounted binary, plugin-update reload triggering cascading hangs across windows. The mitigation strategy: bound everything we *can* (Go-side context timeouts), document everything we *can't*.
- **§5.6 (HIGH, security)**: project sidecar (`<picked_path>/.wezsesh.json`) trust enforcement was ambiguous in v2.0 — §5.6 referred to §6.11 for trust check but didn't explicitly state the check is mandatory and identical to the snapshot-sidecar restore path. Ambiguity could allow a future implementer to skip the check, opening RCE-on-pick from a malicious git-cloned repo. Explicit subsection "Project sidecar trust enforcement" added: trust hash bound to `<picked_path>/.wezsesh.json` absolute path; fail-closed default; toast-on-silent-skip UX (`wezsesh: on_create not trusted for "<name>". Run 'wezsesh trust <name>' to approve.`); `wezsesh trust <name>` resolves project sidecar via `cli list`. README + `wezsesh trust --show` surface authorship guidance.
- **§5.6 (MEDIUM, UX)**: project sidecar `n`-flow silent failure UX hardened. Default `prompt_on_untrusted = false` is preserved (matches `direnv`'s opt-in `direnv allow` UX) but every silent skip now surfaces a 6-second toast with the `wezsesh trust` command to copy-paste. README + doctor recommend `prompt_on_untrusted = true` for users who frequently use the `n` flow with new directories.
- **§5.6 / §6.11 (MEDIUM, docs)**: project-sidecar authorship guidance added (treat `on_create` as a postinstall script users will execute without inspection; stick to CI-safe commands; document in project README) and cross-machine trust note added (path-bound; cloning to a different absolute path requires re-approval per machine).
- **§6.14 (HIGH, correctness)**: `wezterm.GLOBAL` concurrent mutation race in the replay-guard write-back was reachable in principle. Lua handlers are scheduled as async tasks via `promise::spawn::spawn`; if any step (a)–(h) yields via `.await`, two concurrent handlers can lose-update each other's `seen_ids` entry, weakening replay protection. Verified `lua-api-crates/share-data/src/lib.rs` — GLOBAL reads via `__index` create deserialized snapshots; sub-table mutations are local until written back; the read-modify-write is NOT atomic across two `__index`/`__newindex` operations. Closed by an explicit assumption: handler steps (a)–(h) MUST be synchronous Lua bytecode with zero `.await` points. CI lint parses `plugin/wezsesh/ipc.lua` AST and fails if (a)–(h) calls any known-async wezterm function. Fallback documented: 30s freshness window + per-payload HMAC are the primary replay defense; `seen_ids` is defense-in-depth + #3524-broadcast-bug deduplication.
- **§6.15 (MEDIUM, hygiene)**: wezterm source line citations drift across versions. Round-9 cited `emit_user_var_event` at 2564–2608; round-10 found it at ~2080. Function name and behavior unchanged. Audit guidance added: validate by symbol name + signature, not line number — line citations to wezterm source are approximate-and-drifty.
- **§6.3 (additive)**: new error code `XDG_PATH_TIMEOUT`.
- **§6.10 / §6.19 (additive)**: `internal/safefs/` extended with `IsNetworkFS` helper.
- **§9 risks**: 12 new rows covering all v2.1 BLOCKERs and the FS / API / concurrency / project-sidecar hardening.

**v2.0 changes** (round-9 audit):

- **§6.2 / §6.4 / §6.15 / §9 (BLOCKER, correctness)**: `tea.After` was referenced through PRD revisions v1.6–v1.9 but does NOT exist in any released bubbletea version (verified against v1.3.5 and v2.0.6 `commands.go` source). Replaced with `tea.Tick(2*time.Second, fn)` — bubbletea's actual one-shot timer Cmd. Cancellation guarantee is via `program.ctx`; CI test asserts the goroutine exits within 100ms of `tea.Run` return.
- **§5.7 / §6.3 / §9 (BLOCKER, correctness)**: DELETE worst-case payload (50 names × 1200-byte canonical-escape expansion) blew past the 4 KB OSC ceiling by 5×. Per-OSC cap of 5 names; bulk-delete batches into ⌈N/5⌉ sequential OSCs sharing a single `bulk_id`. TUI shows one combined progress + final summary; best-effort, not transactional.
- **§6.7 / §6.19 / §9 (BLOCKER, correctness — silent data loss)**: `save`'s hash-check and `resurrect.state_manager.save_state` write were NOT atomic across concurrent TUIs; both could pass each other's hash check within milliseconds and the second writer silently clobbered the first. New `safefs.AcquireExclusive` (POSIX `fcntl(F_SETLKW)`) holds a file-level exclusive lock through the entire hash-check + write sequence. New error code `SNAPSHOT_LOCKED` for contended waits >5s. Same pattern applied to sidecar tag/pin/notes writes (§6.6 lost-update race). NFS caveat documented.
- **§6.14 (BLOCKER, security)**: mandatory `user-var-changed` handler structure pinned. Pane-ID check FIRST (drops foreign-pane DoS amplification by 100–1000×); `wezterm.json_parse` MUST be `pcall`-wrapped (raises Lua error on malformed input — verified `lua-api-crates/json/src/lib.rs`); field-shape validator MUST run BEFORE HMAC verify (downstream code assumes `hmac` is a 64-char hex string; `ct_eq.eq` raises on `#nil`); canonical-JSON encode MUST be `pcall`-wrapped; dispatch MUST be `pcall`-wrapped. CI fuzz test fires 10 000 mutated payloads and asserts no Lua error escapes the handler boundary.
- **§6.3 (BLOCKER, correctness)**: canonical encoder PINNED to **Option B (wrapper functions via metatables)** — `canonical_json.array{}`, `canonical_json.object{}`, `canonical_json.NULL`. Sentinel-field approach (Option A) rejected because user code that legitimately uses an `__array` field would silently corrupt encoding. Untagged tables are encoder errors. The `from_parsed(t)` helper re-tags structures from `wezterm.json_parse`.
- **§6.15 (HIGH, correctness)**: switch-poller false-positive when user is already in target workspace was unspecified. Pre-switch state capture (active workspace + target window ID) added; predicate now requires (workspace in target window) AND (target != pre-active OR isRestoreFlow). Pure switch to already-active workspace short-circuits in one iteration; switch+restore bypasses the equality check via `isRestoreFlow=true`.
- **§7.2 / §8.1 (HIGH, security)**: `wezsesh nuke` blast-radius hardened. `--yes` MANDATORY for actual deletion (default is preview; v1 default was DELETE — hostile-by-default). Every target path `os.Lstat`-checked before unlink; symlinks logged + skipped (`safefs.SafeRemove` / `SafeRemoveAll`). `--snapshot-dir` `safefs.VerifyDir`-checked at command entry; symlink target aborts the whole run.
- **§6.17 (HIGH, correctness + security)**: name validation tightened to forbid TAB (the prior `\t`-exemption inflated payload sizes via canonical escapes), DEL, C1 controls, U+2028 (LINE SEPARATOR), U+2029 (PARAGRAPH SEPARATOR). Bounds canonical expansion to ~3× from multi-byte UTF-8 instead of 6× from C0/DEL/C1/LS escapes.
- **§6.17 (HIGH, correctness)**: tag-string validation added — 1–10 tags per workspace, each 1–50 UTF-8 bytes, same byte rules as workspace names. Was previously unvalidated; tag content could carry payload-blowing escape-heavy bytes.
- **§6.4 / §9 (MEDIUM)**: bubbletea v2 renderer architecture noted (v1's `standard_renderer.go` → v2's `cursed_renderer.go` delegating to `ultraviolet.TerminalRenderer`). Internal mutex is preserved (`cursed_renderer.go:27`); the `/dev/tty` + external `sync.Mutex` pattern in §6.4 still holds. No architecture change required; CI integration test asserts no frame-OSC byte interleaving.
- **§6.3 (additive)**: new error code `SNAPSHOT_LOCKED` for save/sidecar contended-waits.
- **§6.10 / §6.19 (additive)**: `internal/safefs/` extended with `SafeRemove`, `SafeRemoveAll`, `AcquireExclusive` helpers.
- **§9 risks**: 12 new rows covering all v2.0 BLOCKERs and the payload-budget / TOCTOU / Lua-payload hardening.

**v1.9 changes** (round-8 audit):

- **§6.3 (BLOCKER, security/correctness)**: canonical-JSON encoding rules expanded from 5 abbreviated bullets to an exhaustive specification. Two BLOCKERs closed:
  - Lua `{}` array/object ambiguity — Lua encoder REQUIRES an explicit shape tag (sentinel field or wrapper-function metatable). Empty objects emit `{}`, empty arrays emit `[]`. Without this, every empty-`args` payload fails HMAC.
  - `hmac`-field-removal ordering — both signer and verifier follow the exact 7-step sequence; the field is REMOVED entirely (not zeroed) before the HMAC-input serialization.
  Also: forbid short-form escapes (`\b\f\n\r\t`); use `\u00XX` for all C0 + DEL + C1; escape U+2028 / U+2029 for JS-safety; reject Lua floats via `math.type(n) == "integer"`; recursive key sort at every nesting level. CI golden tests with multi-byte UTF-8, nested objects, integer edge cases. CI runs with `LC_ALL=C`.
- **§6.14 Layer 2 (HIGH, security)**: HMAC key format pinned — `WEZSESH_HMAC_KEY` is 64 hex chars representing 32 raw bytes (256 bits); both sides hex-decode BEFORE passing to HMAC. Required CI round-trip fixture (known key + canonical payload + expected digest) gates Lua/Go interop. New `plugin/wezsesh/ct_eq.lua` for constant-time HMAC compare (pure_lua_SHA does not export one; native `==` short-circuits).
- **§6.20 (BLOCKER, new section)**: every `wezterm cli` invocation in `internal/wezcli/` MUST be wrapped in `context.WithTimeout(2s)`. wezterm's CLI client has bounded connection retries but NO internal RPC timeout once connected (verified `wezterm-client/src/client.rs:731-808`); without the wrapper, a hung mux locks the wezsesh TUI indefinitely. Plus: error-classification table (exit code + stdout validity, never parse stderr); pre-rename collision check via `cli list` (mux server doesn't enforce uniqueness); `PANE_CLOSED_RACE` retry semantics for `cli activate-pane` failures during `wezsesh find`.
- **§6.4 (BLOCKER, correctness)**: `wezterm.background_child_process` raises Lua errors on spawn failure (verified `lua-api-crates/spawn-funcs/src/lib.rs:54-72`). Reply emission MUST be `pcall`-wrapped; on failure, log via `wezterm.log_error` and surface a toast `reply emission failed; operation may have succeeded — refresh picker`. Without this, an unhandled error propagates out of the `user-var-changed` handler and crashes the dispatch path mid-way.
- **§6.14 failure-mode matrix (BLOCKER, correctness)**: `wezterm.on('pane-closed', ...)` does NOT exist as a Lua-surfaced event in wezterm (verified against documented window-events list and `wezterm-gui/src/termwindow/mod.rs` match arms; `MuxNotification::PaneRemoved` is internal-only). The previously-spec'd UNEXPECTED_EXIT early-warning toast is non-implementable as written. Replaced with a Go-side top-level `defer recover()` in `cmd/wezsesh/main.go` that writes a sentinel `UNEXPECTED_EXIT` reply over OSC + open reply socket on Go panic. SIGSEGV / SIGKILL / OOM-kill fall through to IPC_TIMEOUT (rare-edge; acceptable).
- **§7.1 (BLOCKER, reliability)**: `wezterm.run_child_process` can hang indefinitely (verified `lua-api-crates/spawn-funcs/src/lib.rs:30-50` — `cmd.output().await` has no timeout; wezterm has no config-eval watchdog). Mitigation: pre-flight `io.open` existence check (fork-free), cache resolved absolute path in `wezterm.GLOBAL.wezsesh_bin_path`, doctor probes version-detect timing and warns if >2s. Residual hang risk documented (binary exists, runs, but stalls — only fix is removal).
- **§6.3 / §3 (additive)**: new error codes `UNEXPECTED_EXIT` and `PANE_CLOSED_RACE`.
- **§6.10 (additive)**: plugin layout adds `ct_eq.lua` for constant-time HMAC compare.
- **§8.1 (additive)**: testing invariants expanded — explicit canonical-JSON corpus, HMAC round-trip golden, pre-flight binary check + path cache, `internal/wezcli/` 2s timeout requirement, top-level Go panic-recover in `main.go`, pre-rename collision check.
- **§9 risks**: 12 new rows covering all v1.9 BLOCKERs and the canonical-JSON / CLI / lifecycle hardening.

**v1.8 changes** (round-7 audit):
- **§6.18 (BLOCKER, security)**: `pane:send_text` cwd/argv control-char defense. `wezterm.shell_quote_arg` (Rust `shlex`) accepts `\n`/`\r` literally inside quoted strings; the PTY sees them as line terminators → command injection. Lua-side `bytes_clean` regex `[%z\1-\31\127]` validates `cwd` and every `argv` element before any `send_text`; dirty inputs downgrade to no-op send. Doctor flags snapshots with control-char `cwd`/argv.
- **§6.4 (correctness)**: `cleanup()` contract spec'd — `listener.Close()` + `os.Remove(sockPath)` under `sync.Once`; `defer cleanup()` immediately after `StartListener`; same passed to `InstallSignalHandler`. Listener exits on `net.ErrClosed`. Without this, listener goroutine + socket fd leaked when TUI quit before terminal reply.
- **§6.2 / §6.4 (correctness)**: retransmit MUST be `tea.After(2s, retransmitMsg{})` returning `tea.Cmd`. Raw goroutines leak until firing.
- **§6.15 (correctness)**: `StartSwitchPoller(ctx, ...)` accepts `context.Context`; caller cancels on terminal reply or `tea.Run` return. Poller exits at iteration boundary; no leak on early workspace appearance.
- **§5.6 (clarity + correctness)**: full path-picker contract — `exec.CommandContext(ctx, shell, "-c", cmd)`, 15s timeout, 1 MiB / 10 000 line caps, UTF-8 + NUL validation, tilde-expansion, `os.Stat` post-pick, distinct error codes. **Disambiguated "sidecar"** to "project sidecar" (`<picked_path>/.wezsesh.json` for `on_create`) vs "snapshot sidecar" (`<snapshot_dir>/workspace/<name>.wezsesh.json` for `on_restore`). Same schema, different locations, independent trust files.
- **§6.17 (defense-in-depth)**: render-time sanitization — `internal/nameval.SanitizeForDisplay` strips C0/C1 + DEL from any disk-sourced string before lipgloss/fmt/log/toast. lipgloss does not sanitize; a snapshot-named `\x1b[2J` would clear the terminal.
- **§8.1 (supply-chain)**: charmbracelet stack upgraded v1 → v2 (bubbletea v2.0.6, bubbles v2.1.0, lipgloss v2.0.3, huh v2.0.3, x/ansi v0.11.7). Round-3 had pinned v1 because v2 was RC; that's resolved as of 2026-04. Module path changed to `/v2`. Pinned `go 1.26.2`. Pinned `pure_lua_SHA` commit `6adac177` with `SOURCES.lock` + CI sha256 verify.
- **§8.1 (CI)**: required supply-chain checks — `go mod verify`, `govulncheck ./...`, `sha256sum -c` for vendored Lua, `-trimpath` reproducible build, staticcheck + go vet.
- **§8.1 (testing)**: mandatory `goleak.VerifyNone(t)` in tests with whitelist for `installSignalHandlerWorker`. Mandatory `defer recover()` in every spawned goroutine in `internal/ipcsock/`, `internal/wezcli/`, `internal/argvallow/`, `internal/tui/`. Panic in goroutine must degrade to logged error, not crash. Golden tests for canonical-JSON Go ↔ Lua byte-equality. HMAC compatibility round-trip tests. Resurrect race tests. Multi-window broadcast (#3524) test.
- **§9 risks**: 11 new rows covering all v1.8 BLOCKERs + secondary defenses + the lifecycle/supply-chain hardening.

**v1.7 changes** (round-6 audit):
- **§6.18 (new, BLOCKER, security)**: snapshot argv allowlist (`internal/argvallow/`). Resurrect's `tab_state.lua:127` does `pane:send_text(shell_join_args(argv) .. "\r\n")` for alt-screen panes — unconditional RCE if attacker can place a snapshot file (dotfiles sync, same-UID trap). Mitigation: wezsesh wraps `resurrect.workspace_state.restore_workspace` with a custom `on_pane_restore` callback that allowlist-checks `argv[0]` basename. Default allowlist: shells, editors, common tools. Unknown → degrade to `cd <cwd>\r\n`. User-extensible via `resurrect_argv_allowlist` config.
- **§6.19 (new, BLOCKER, security)**: filesystem safety — `internal/safefs/` package with `AtomicWriteFile` / `VerifyDir` / `SafeOpenForRead` using `O_NOFOLLOW` + `openat(2)` + `Renameat` via `golang.org/x/sys/unix`. Mandatory for state.json, sidecars, trust files. Sidecar writes are Go-only — Lua's `io.open` has no `O_NOFOLLOW`. Reply socket creation uses `unix.Umask(0077)` before `net.Listen` so sock is born 0600.
- **§6.11 (BLOCKER, security)**: hook execution path expanded with eight concrete contracts: read-once-exec-from-memory (TOCTOU), `$SHELL` → `/bin/sh` fallback, cwd Stat + `$HOME` fallback, `Stdin = nil`, 10min timeout (configurable; SIGTERM then SIGKILL), `SysProcAttr.Setpgid` + process-group kill, `WEZSESH_*` env scrubbing, `os.Lstat` on trust file + trust dir for symlink defense.
- **§6.14**: failure-mode matrix expanded with seven new rows (listener with nil HMAC key, binary unexpected exit, binary not on PATH, `apply_to_config` raises, user hook raises, FS timeout, distinct binary-not-found vs version-mismatch toasts). `apply_to_config` body now `pcall`-wrapped at the top level.
- **§6.17**: workspace name validation already covers most ILLEGAL_NAME triggers; cross-references with §6.18 (separate threat) and §6.19 (overlapping FS defense).
- **§7.1**: GLOBAL schema versioning — `apply_to_config` writes `wezterm.GLOBAL.wezsesh_plugin_version` and wipes all `wezsesh_*` keys on mismatch. Distinct toast for "binary missing" vs "unparseable" vs `compatible()=false`.
- **§7.2**: added `wezsesh nuke [--dry-run]` subcommand for clean uninstall.
- **§8.1**: storage/IPC list expanded with safefs requirements, signal handler spec, FS context deadline, env scrubbing on hooks.
- **§9 risks**: 13 new rows covering all v1.7 BLOCKERs and the lifecycle gaps. The risk table now serves as the audit trail of every exploitable surface considered.

**v1.6 changes** (round-5 audit):
- **§6.11 (BLOCKER, security)**: trust-store hash construction switched from `path || "\n" || cmd` to length-prefixed `uint32_be(len(path)) || path || uint32_be(len(cmd)) || cmd`. The `\n`-delimited form was forgeable: workspace names are user-supplied and become sidecar paths; a name containing `\n` allowed hash collision and trust forgery for arbitrary commands.
- **§6.2 / §6.3 / §6.4 / §6.15 (BLOCKER, correctness)**: adopted split-reply pattern. Reply payloads carry a `status` field (`"completed"`, `"started"`, `"partial"`); restore-class verbs (`load`, `switch+spawn`) reply `"started"` immediately and emit a follow-up `"completed"`/`"partial"` when restore finishes. First-reply ceiling stays at 5s; follow-up ceiling is 30s additional. Resurrect's serial-spawn restore takes 8–15s on heavy-shell setups, well past the previous 5s cap.
- **§6.15 (BLOCKER, architecture)**: dropped `smart_workspace_switcher` event-subscription "primary path" — events fire only from the plugin's own picker UI, never from programmatic `act.SwitchToWorkspace`. Go-side polling at 50ms cadence with 5s ceiling is the only path; no third-party plugin coupling.
- §6.4: spec'd `signal.Notify` handler for socket cleanup (`internal/ipcsock.InstallSignalHandler`); Go's `defer` doesn't run on SIGHUP/SIGTERM.
- §6.6: per-file snapshot size cap 10 MiB; JSON parse depth cap 100.
- §6.8: documented state-file last-writer-wins under concurrent TUIs (benign for usage counters, mildly visible for pins).
- §6.15: `seen_ids` 60s sub-pruning by storing `{ ts = os.time() }` per ULID; bounds set to ~100 entries.
- §6.17 (new): workspace name validation rules and `ILLEGAL_NAME` triggers (length 1–200 bytes, no NUL/newline/C0 controls, no `.`/`..`, no `\`, NFC normalization, `+` UI warning for resurrect's encoding collision).
- §7.1: `exclude` accepts Go RE2 regex strings, not Lua patterns (silent miscompilation risk).
- §9: added rows for wezterm OSC NUL silent-swallow (must validate client-side; `vtparse/src/transitions.rs:250`), invalid-base64 silent drop (`osc.rs:1276`), HMAC constant-time compare hygiene, OSC byte-cap clarification (wezterm has none; 4 KB is self-imposed).

**v1.5 changes** (round-4 validation):
- **§6.4 (BLOCKER)**: corrected bubbletea Cmd-coexistence story — Cmds run concurrently (`tea.go:355-367`), so `/dev/tty` + `sync.Mutex` is **required**, not defensive. Removed the wrong "render loop serializes Cmds" claim.
- **§6.14 / §6.15 (BLOCKER)**: every `wezterm.GLOBAL.wezsesh_state[...]` key must be `tostring(pane_id)` — integer keys raise `Err("can only index objects using string values")` per `share-data/src/lib.rs`. Updated example code, state-maps box, and replay-guard reference.
- §6.2 / §6.15: simplified retransmit from 250ms × 20 → single retry at 2s. Reload-drop justification refuted by reading `wezterm-gui/src/termwindow/mod.rs:2564-2608` and `config/src/lib.rs:175-220`; events are NOT dropped during reload.
- §6.3: added `SNAPSHOT_MISSING` error code for the rename/load/delete-of-vanished-snapshot race.
- §6.4: dropped `--new-window` from spawn example to match `spawn_mode = "tab"` default. Clarified that Lua plugin uses `mux.spawn_window` / `Window:spawn_tab` natively.
- §6.4 / §7.1: `force_close` is top-level, not nested under `wezsesh.spawn.`.
- §6.6: documented which ops work on encrypted snapshots (rename/delete/save/load all work; only preview degrades).
- §6.7: noted `file_io.write_state.finished` fires on both success and failure paths — write-gate clears correctly regardless.
- §6.12: clarified `restore_workspace.finished` reaches emission only when no Lua error escapes (resurrect has no error path); pcall on our side is the only mechanism for `RESURRECT_PARTIAL`.
- §6.13: noted `cwd` field is empty string `""`, not null, when unavailable; Go-side parser must guard before URL-decoding.
- §6.14 / §7.2: added `wezsesh keygen` subcommand using Go `crypto/rand`; removes openssl dependency. Key cached in `wezterm.GLOBAL.wezsesh_session_key` per wezterm session, not per-spawn.
- §6.16 (new): defined q/Esc mid-op behavior — inline prompt for write ops in flight, eager quit otherwise (lazygit-style).
- §7.1: clarified `runtime_dir` darwin path; clarified `exclude` is picker-only and translated to Go regex at plugin load.
- §7.3: `wezsesh doctor` now checks wezterm `>= 20230408-112425-69ae8472`, `tty_name` field availability, `cli list-clients` reachability.
- §9: pinned OSC payload ceiling at 4 KB canonical-JSON; added wezterm version pin row; rewrote bubbletea + reload-drop rows.
