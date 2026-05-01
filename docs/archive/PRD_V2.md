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

### 5.7 Bulk operations

`Tab`/`Space` toggles a mark on the current row. Marked rows render with `✓` in the marker column. `c` clears all marks.

When `d` is pressed:
- If any rows are marked → bulk-delete confirm modal listing marked names.
- Else → single-row delete confirm modal.

Other bulk ops are out of scope for v0.1; tag/pin remain single-row.

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
    - **Retransmit MUST be implemented as `tea.After(2 * time.Second, retransmitMsg{})` returning a `tea.Cmd`, NOT as a raw goroutine or `time.AfterFunc`.** When the first reply arrives before 2s, `Update` ignores any subsequent `retransmitMsg` (idempotent guard: `if model.replyReceived { return model, nil }`). With raw goroutines the timer fires regardless and leaks the goroutine until firing — fails `goleak.VerifyNone(t)` in tests.

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
7. **Verifier** (Lua side): parse the received payload; remove the `hmac` field from the parsed structure; canonical-serialize the rest; compute HMAC; compare against the received `hmac` value via the constant-time helper `ct_eq` (§6.14 Layer 2).

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
  - Lua: every table value passed to the encoder MUST carry an explicit shape tag. Pick one mechanism and use it consistently throughout `plugin/wezsesh/`:
    - **Option A** — sentinel field: `t.__array = true` marks an array; the encoder strips this field before emitting.
    - **Option B** — wrapper functions: `canonical_json.array{...}` and `canonical_json.object{...}` set distinct metatables; the encoder dispatches on metatable.
  - **Empty objects MUST emit `{}`. Empty arrays MUST emit `[]`.** Without the explicit tag, an empty Lua table `{}` is indistinguishable; this is the source of every empty-`args`-payload divergence.
  - For the IPC payload protocol specifically: `args` is ALWAYS an object (even for `noop` where it's `{}`). The Lua encoder constructs request-side `args` as a tagged-object so empty `args` serializes as `{}` not `[]`.
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

The plugin treats unknown verbs as `noop` and logs a warning.

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
    "code": "SNAPSHOT_CHANGED" | "SNAPSHOT_MISSING" | "RENAME_COLLISION"
          | "ILLEGAL_NAME" | "MUX_UNREACHABLE" | "RESURRECT_PARTIAL"
          | "SNAPSHOT_LOAD_FAILED" | "HMAC_MISMATCH" | "FOREIGN_PANE"
          | "STALE_PAYLOAD" | "REPLAY" | "IPC_TIMEOUT" | "UNKNOWN_VERB"
          | "UNEXPECTED_EXIT" | "PANE_CLOSED_RACE"
          | "UNKNOWN",
    "message": "human-readable",
    "details": { /* optional */ }
} }
```

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
   - On Linux: `<reply_dir>` = `$XDG_RUNTIME_DIR/wezsesh/` (typically `/run/user/<uid>/wezsesh/`).
   - On darwin: `<reply_dir>` = `/tmp/wezsesh-<uid>/` (NOT `$TMPDIR` — too deep for `SUN_PATH` 104-byte limit).
   - Filename = first 8 hex chars of the request_id ULID + `.sock` (saves SUN_PATH bytes; collision-safe for our concurrency).
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
- `process` has both string-shape (pre-2024-08) and object-shape (current) in the wild — use a custom unmarshaler that handles both, or `json.RawMessage` and parse lazily.
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

Two TUIs may run simultaneously. We do **not** lock. Each TUI:

1. On open, reads each snapshot file and computes its SHA-256.
2. Stores `(name → hash)` in memory.
3. On `save` (with `overwrite: true`), the binary sends `expected_hash` in the payload. Lua handler reads the current file, compares hashes. If mismatch → returns `SNAPSHOT_CHANGED` error. The binary surfaces a modal: `Snapshot 'foo' has changed since you opened wezsesh. Overwrite anyway? [y/N]`. User confirms → re-send with `overwrite: true` and a refreshed expected_hash from the disk read just performed.
4. `delete` ignores hash (deleting a "newer" snapshot is still a delete). Multi-row delete passes through.
5. `rename` follows the same pattern as save when the target file would be overwritten.

This protects P2 (cleaner) from clobbering a snapshot a parallel session just refreshed.

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

**On selection**: `wezterm cli activate-pane --pane-id N` then `activate-tab --tab-id T`. Workspace switch happens implicitly (activating a pane in a different workspace switches to it).

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
| Unknown verb | Reply with `UNKNOWN_VERB` + `log_warn` |
| **Listener invoked with `wezterm.GLOBAL.wezsesh_session_key == nil`** (keygen failed at plugin load; see §7.1) | Silent `log_warn("wezsesh: ignoring op, HMAC key unavailable")` + drop. Never proceed to dispatch. |
| **Binary unexpected exit / panic** | Two-layer detection. **(1) Go-side panic-recover** — `cmd/wezsesh/main.go` installs a top-level `defer func() { if r := recover(); r != nil { writePanicReply(...); os.Exit(2) } }()` before `tea.Run`. The recovery handler writes a sentinel reply over the OSC channel (or to any open reply socket) with `status: "completed"`, `ok: false`, `error.code: "UNEXPECTED_EXIT"`. The Lua side does NOT need any pane-closed listener; the binary itself signals its own death before exiting. Out-of-recover crashes (SIGSEGV in cgo, SIGKILL, OOM-killer) cannot trigger this path — fall through to (2). **(2) IPC_TIMEOUT fallback** — after 5s with no reply, the TUI's parent surfaces the standard timeout error. The earlier PRD claim of a `wezterm.on('pane-closed', ...)` listener is **dropped**: source-code reading (wezterm-gui/src/termwindow/mod.rs match arms; wezterm.org/config/lua/window-events/) confirms `pane-closed` is NOT a Lua-surfaced event in wezterm — `MuxNotification::PaneRemoved` exists internally but is unhandled in the Lua emit path. The early-warning toast is therefore non-implementable as previously specced. |
| **Binary not on PATH at first run** (version handshake `run_child_process` returns `ok=false`) | Distinct toast: `wezsesh binary not found on PATH. Install: go install github.com/Grady-Saccullo/wezsesh/cmd/wezsesh@latest`. Keybinding stubs out. **Do not conflate with version mismatch** (which is `ok=true` + version regex fails `compatible()`). |
| **`apply_to_config` body raises a Lua error** (vendored module syntax error, bad option type, unexpected wezterm API return) | The `apply_to_config` body is wrapped in `pcall`; on catch, log error, return a no-op stub config (no keybindings, no listeners registered), toast at `gui-startup`. Never break the user's whole wezterm config. |
| **User `on_before_op` / `on_after_op` Lua hook raises** | `pcall`-wrapped at the dispatch boundary; `log_warn` with the error; continue to dispatch/reply. The user's buggy hook does not block ops. |
| **FS timeout / hang on snapshot dir read** (NFS, slow cloud-sync mount) | All Go FS ops in `internal/snapshots/` wrapped in `context.WithTimeout(5 * time.Second)`. On timeout: return partial list + non-fatal toast `wezsesh: snapshot dir read slow; some snapshots may be missing this session`. Don't block TUI startup indefinitely. |

**Never raise a Lua error in event handlers OR in `apply_to_config`'s top-level body.** Uncaught errors in `wezterm.on` handlers wedge the wezterm event loop; uncaught errors in `apply_to_config` abort the user's entire wezterm config eval. Both must be `pcall`-wrapped at the boundary.

#### Residual risks (documented honestly)

1. **Env-var leak via `/proc/<pid>/environ`**: any process running as the same UID can read the HMAC key. Standard Unix env-var caveat; we accept it.
2. **In-pane attacker**: a process running with stdout pointing at the wezsesh TUI pane could emit signed-looking traffic if it can read `WEZSESH_HMAC_KEY`. The wezsesh pane runs only the Go binary — no shell, no `cat`, no `npm`. If we ever add a "shell out from TUI" feature, we must rotate the key or scrub the env first.
3. **Wezterm OSC parser vulnerabilities**: out of our trust boundary. If wezterm's parser is broken we have bigger problems.
4. **Clock skew between plugin and binary**: same host, fine. Documented as an assumption.
5. **Vendored pure-Lua SHA**: supply chain. Pin commit, check in repo, audit.
6. **Reuse after wezterm restart**: pane IDs reset on wezterm restart. Each launch records its own `spawned_pane_id`, so this is fine.

### 6.15 Reload durability — `wezterm.GLOBAL` + single-retry resend

Wezterm reloads the user's config when `wezterm.lua` is saved. Per [wezterm.on docs](https://wezterm.org/config/lua/wezterm/on.html): "the Lua state is built from scratch when the configuration is reloaded… reloading the configuration will clear any existing event handlers." Handlers are cleared and re-registered. **`wezterm.GLOBAL`** survives the reload (verified: it's a `lazy_static!` over `Arc<Mutex<BTreeMap>>` in `share-data/src/lib.rs`; accepts nil/bool/integer/number/string/array tables/object tables — JSON-shaped only).

**Reload-time event delivery**: per source-code reading of `wezterm-gui/src/termwindow/mod.rs:2564-2608` (`emit_user_var_event`) and `config/src/lib.rs:175-220` (`LuaPipe`/`update_to_latest`), `user-var-changed` events are NOT dropped during config reload. Each dispatch holds an `Rc` to the Lua VM that was current when the dispatch began; the new VM is queued in `LUA_PIPE` and adopted on the next dispatch. Old handlers run to completion against the old VM. A previous version of this PRD claimed events were dropped during reload — not supported by source. The single-retry-at-2s in §6.2 is defensive against unrelated transport hiccups (truncated OSC writes, signal-interrupted syscalls), not reload drops.

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

1. Lua dispatches `act.SwitchToWorkspace { name }` via `window:perform_action(...)` and **immediately replies `{"status": "started"}`** through the reply socket (matches the unified status taxonomy in §6.3). This closes the first reply within milliseconds; the TUI dismisses without waiting for the actual switch to complete.
2. **Go binary polls** `wezterm cli list --format json` via `StartSwitchPoller(ctx, name, 50*time.Millisecond)` where `ctx` is `context.WithTimeout(parent, 5*time.Second)`. The poller exits on `<-ctx.Done()` at any iteration boundary. Caller cancels `ctx` on any terminal reply OR on `tea.Run()` return (via `defer cancel()`), so the poller does NOT leak when the workspace appears at 60ms. Without context cancellation the poller would tick 50ms intervals until the 5s ceiling. Goroutine has top-level `defer recover()` (CLI JSON unmarshal could panic on malformed output).
3. Once the workspace appears, the binary either: (a) for a pure `switch` op, exits cleanly (workspace already activated server-side), or (b) for a `switch+restore` (existing-saved snapshot the user wants to load), emits a follow-up `restore` op via a second OSC sequence.
4. If 5s elapses with no workspace visible, surface `MUX_UNREACHABLE` in the TUI and exit.

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
| Bytes: no `\0` (NUL), no `\n`, no `\r`, no other C0 control chars (0x01–0x1F except `\t`) | NUL injection at `os.Create` boundaries, trust-store hash collision, picker render corruption. **Wezterm's OSC parser silently swallows NULs (verified `vtparse/src/transitions.rs:250`); we MUST validate client-side, not rely on wezterm.** |
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

### 6.18 Snapshot argv trust (RCE defense)

§6.11 hook trust covers the wezsesh sidecar's `on_create` / `on_restore` shell commands. **A separate, orthogonal vector exists in resurrect snapshots themselves**: `pane_tree.process.argv`. When restoring a pane with `alt_screen_active = true`, resurrect calls `pane:send_text(wezterm.shell_join_args(pane_tree.process.argv) .. "\r\n")` (verified at `plugin/resurrect/tab_state.lua:127`). This injects the saved command into the shell's stdin — equivalent to the user typing it. There is no validation, no allowlist, no trust check anywhere in resurrect's path.

**Threat surfaces**:
- Dotfiles sync of resurrect's snapshot directory.
- Same-UID trap: a less-privileged process under the user's UID (npm postinstall, downloaded binary, MCP server) writes a malicious snapshot to `~/.local/share/wezterm/state/resurrect/workspace/`.
- Shared / multi-user machine where snapshot dir permissions are loose.
- Snapshot received from another user (intentional sharing, ad-hoc copy).

§6.11 hash-based trust is the right model for `on_*` hooks (rare, user-authored, one-time approval). It is the **wrong** model for `process.argv` (every pane of every restore — per-pane prompts are an unusable UX).

**Adopted mitigation: tmux-resurrect-style allowlist** via a custom `on_pane_restore` callback wezsesh installs over resurrect's default. Pattern follows tmux-resurrect's `@resurrect-processes` config.

**Default allowlist** (basename of `argv[0]`):
```
$SHELL (resolved at install time), sh, bash, zsh, fish, dash, ksh,
nvim, vim, vi, emacs, nano, helix, hx,
less, more, man, info,
git, jj, lazygit, tig,
python, python3, ipython, node, ruby, irb, lua,
htop, btop, top, k9s, lazydocker
```

**Behavior**:
1. wezsesh's `on_pane_restore` callback is passed `pane`, `pane_tree`. It checks `basename(pane_tree.process.argv[1])` against the allowlist.
2. **Match**: also validate every argv element AND `cwd` for control characters (see "control-char defense" below). If any element is dirty, fall through to the No-match path with a different reason in the log. Otherwise call through to resurrect's `default_on_pane_restore`.
3. **No match**: try the cwd-only fallback. If `cwd` passes the control-char check, `pane:send_text("cd " .. wezterm.shell_quote_arg(cwd) .. "\r\n")`. If `cwd` fails the check (or is empty/missing), `pane:send_text("\r\n")` only — the user lands at the shell's default cwd. Log WARN in both cases: `wezsesh: skipped argv restore for "<argv[0]>"; cwd <clean|dirty>`.
4. **Empty argv / no `process` field**: no-op (resurrect's default also no-ops).

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

**`wezsesh doctor` check**: scan all snapshots for `pane_tree.process.argv[0]` values not in the active allowlist; report a count + list of unique non-matching `argv[0]` per snapshot. Helps the user audit what would currently be skipped.

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
- **`cli activate-pane --pane-id N`** / **`cli activate-tab --tab-id T`**: no client-side existence check; pane/tab may be closed between listing and activation. On exit 1, `wezsesh find` re-runs the listing once and retries; if the second attempt also fails, surfaces `PANE_CLOSED_RACE` (new error code, additive to §6.3 — see below). Cross-window/cross-workspace activation is server-side implicit (workspace switch happens automatically per §6.13).
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
                                                     -- for SUN_PATH 104-byte limit)
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
        mark = "Tab", mark_alt = " ", clear_marks = "c",
        help = "?", filter = "/", quit = "q",
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

1. **Pre-flight existence check, fork-free.** Before any `run_child_process` call, validate the binary path via `os.execute`-free Lua filesystem checks: `local f = io.open(binary_abs_path, "r"); if f then f:close(); ok = true end`. If the file doesn't exist, skip the spawn entirely and route to the `"missing"` toast path. This collapses the "binary not on PATH" failure to a no-fork no-hang outcome, and removes one of the three hang triggers.
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
- `bin_v == "missing"` → toast `wezsesh binary not found on PATH. Install: go install github.com/Grady-Saccullo/wezsesh/cmd/wezsesh@latest` (8s); keybinding stubs out. **Distinct from version mismatch** — the user's first-run experience must not say "wrong version" when nothing is installed.
- `bin_v == "unparseable"` → toast `wezsesh binary returned unrecognized version string`; keybinding stubs out.
- `compatible() == false` → toast `wezsesh binary v<X> doesn't match plugin v<Y>. Run 'wezsesh doctor' for details.`; keybinding stubs out.
- `compatible() == true` → proceed with full setup.

In all error cases:
- `wezterm.log_error(...)` (always; visible in debug overlay).
- One-shot `window:toast_notification("wezsesh", message, nil, 8000)` registered via `wezterm.on('gui-startup', ...)`.
- **The plugin does not raise a Lua error**; that would break the user's whole wezterm config.

#### `apply_to_config` body wrapped in `pcall`

The entire body of `apply_to_config` (binary detection, key generation, option validation, vendored module loading, listener registration, keybinding registration) runs inside a top-level `pcall`. On catch:
1. `wezterm.log_error("wezsesh: setup failed: " .. tostring(err))`.
2. Register a `gui-startup` toast: `wezsesh setup failed; check wezterm log`.
3. Return a no-op stub config — no keybindings, no listeners registered, no GLOBAL state written.

This guards against vendored Lua module syntax errors, unexpected `wezterm.run_child_process` return shapes, bad user options (e.g., `keybinding = "string instead of table"`), and any other Lua error during setup. The user's wezterm config still loads.

#### GLOBAL schema versioning across plugin updates

`apply_to_config` writes `wezterm.GLOBAL.wezsesh_plugin_version = M.VERSION` on every load. Before reading any other `wezsesh_*` GLOBAL key, it compares the stored version to `M.VERSION`. **On mismatch (including downgrade), wipe all `wezsesh_*` GLOBAL keys and re-initialize.** Any in-flight TUI session from the old version is in an undefined state post-update; better to drop it cleanly than to read stale-shape entries with new code. This handles the `wezterm.plugin.update_all()` flow without state-migration logic.

The Go binary embeds its version via `-ldflags="-X main.version=v$(git describe --tags --always)"` in CI / Nix / Goreleaser, with `runtime/debug.ReadBuildInfo()` as a fallback for `go install module@vX.Y.Z` users.

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
wezsesh nuke [--dry-run]                         # remove all wezsesh-owned files for a clean uninstall:
                                                  #   state dir, trust store, reply sock dir, log files,
                                                  #   *.wezsesh.json sidecars in <snapshot_dir>/workspace/.
                                                  #   Does NOT touch resurrect snapshots themselves.
                                                  #   --dry-run lists targets without deleting.
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
- Recent log tail (last 50 lines).

Exits 0 if all critical checks pass, non-zero otherwise. JSON output via `--format json` for machine consumption (and CI).

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
- `github.com/charmbracelet/bubbletea/v2 v2.0.6` (released 2025-04-16; module path changed from `bubbletea` → `bubbletea/v2`).
- `github.com/charmbracelet/bubbles/v2 v2.1.0`.
- `github.com/charmbracelet/lipgloss/v2 v2.0.3`.
- `github.com/charmbracelet/x/ansi v0.11.7`.
- `github.com/charmbracelet/huh/v2 v2.0.3` (modal forms — rename / save / new dialogs).
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
| **Retransmit raw goroutine leaks until 2s firing even if reply arrived earlier** | Implemented as `tea.After(2*time.Second, retransmitMsg{})` Cmd; `Update` ignores via `model.replyReceived` flag. Bubbletea handles cancellation on program exit. See §6.2 step 10. |
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
| Restore partial failure | pcall + `RESURRECT_PARTIAL` error surface per §6.12. No rollback. |
| What if a user runs the binary outside a wezterm pane? | Detect via `WEZTERM_PANE`; fail fast with `error: wezsesh must run inside wezterm`. Doctor catches. |
| Two TUIs writing the same snapshot | Snapshot-hash check on save/rename (§6.7). |
| Hook command in sidecar is malicious / clobbered | Hash-based fail-closed trust per §6.11. |
| Auto-installed binary footgun | Rejected — stay with manual install matching wezterm plugin norms. |
| Sidecar schema drift on our side | We own it; bump `version` field and gate parsing accordingly. |
| `wezterm cli rename-workspace` exists; can skip Lua roundtrip | Use the CLI directly for rename verbs that don't touch the snapshot file. Lua handler only needed for ops requiring mux state changes the CLI doesn't expose. |

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

**Status**: design v1.9 (round-8 canonical-JSON / crypto / wezterm-CLI / lifecycle audit complete).
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
