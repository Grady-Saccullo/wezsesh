# PRD Research Findings — Round 4 (post-v1 deep validation)

Goal of this round: validate the load-bearing claims that survived rounds 1–3 against current source — specifically the ones that were marked "verified" or "empirically observed" but never re-checked, and the design assumptions that depend on third-party library internals (bubbletea, wezterm event dispatch, GLOBAL serialization, resurrect).

Six parallel research spikes. **Two of them flipped existing decisions** — bubbletea Cmd serialization (it doesn't) and reload-time event drops (they don't) — and **one identified a load-bearing PRD bug** (integer keys on `wezterm.GLOBAL` raise a runtime error).

---

## 1. Bubbletea Cmd execution semantics — `/dev/tty` + mutex is REQUIRED

### Findings

Source: `github.com/charmbracelet/bubbletea` v1.3.5.

**`tea.Sequence(a, b)`** does what you'd expect at the Cmd level: a single goroutine iterates the commands, calls `a()`, sends the message, then calls `b()` (`tea.go:485-510`). But `p.Send(msg)` is non-blocking — `a`'s message is enqueued, not necessarily processed by `Update`, before `b` starts. Sequence orders Cmd *execution*, not Cmd-effect-vs-Update-handling.

**Individual `tea.Cmd`s run as concurrent goroutines.** `handleCommands` (`tea.go:334-373`) reads from the `cmds` channel and for each one launches `go func() { msg := cmd(); p.Send(msg) }()` (`tea.go:355-367`). There is **no serialization** between in-flight Cmds.

**A Cmd writing to `os.Stdout` races directly with the renderer.** The `standardRenderer` flushes frame data via `flush()` (`standard_renderer.go:161-291`) from a ticker goroutine, holding `r.mtx` (line 162). A Cmd closure writing `os.Stdout.Write(...)` runs in a separate goroutine with no reference to `r.mtx`. Byte-level interleaving at the kernel `write(2)` boundary is real, not theoretical.

**`program.Send(msg)` from external goroutines is safe** (`tea.go:737-742`). It writes to `p.msgs` channel; drops silently if program has exited. Correct for the reply-socket goroutine.

### Decision — corrects PRD §6.4 narrative

The PRD V1 §6.4 said "(the render loop serializes Cmds)" as the justification for routing OSC writes through `tea.Cmd`. **This is wrong.** The implementation plan in §8.1 (writing OSC to `/dev/tty` under `sync.Mutex`) is **necessary**, not defensive.

- Open `/dev/tty` separately (not `os.Stdout`), wrap writes in `sync.Mutex`. Bubbletea keeps its own fd for frames; we keep ours for OSCs; the kernel serializes the two write streams.
- Cmds that need to emit OSCs call our mutex-protected `Writer.WriteOSC`; the goroutine + mutex + separate fd makes the write order safe regardless of when the renderer flushes.
- `tea.Sequence(emitOSC, awaitReply)` correctly orders Cmd execution but does NOT order their effects against the renderer.

### Files cited
- `tea.go:334-373` (handleCommands), `:355-367` (concurrent Cmd dispatch), `:485-510` (Sequence), `:737-742` (Send)
- `standard_renderer.go:147-158` (ticker goroutine), `:161-291` (flush), `:162` (mtx)

---

## 2. Wezterm `user-var-changed` reload-window drop claim — REFUTED

### Findings

Source: github.com/wezterm/wezterm, recent main.

The PRD V1 §6.15 stated:
> `user-var-changed` events arriving during the reload window are dropped, not queued — empirically observed; not officially documented but consistent with wezterm's per-frame dispatch model.

Reading the source contradicts this:

**`emit_user_var_event`** (`wezterm-gui/src/termwindow/mod.rs:2564-2608`) responds to `MuxNotification::Alert(SetUserVar)` via `dispatch_notif` and calls `promise::spawn::spawn(config::with_lua_config_on_main_thread(...)).detach()`. There is no event queue. Each invocation captures a snapshot of the current Lua VM at task start.

**Reload mechanism** (`config/src/lib.rs:175-220`): `ConfigInner::reload()` succeeds → sends new VM through `LUA_PIPE` (unbounded async channel) to the main thread. Main thread calls `lc.update_to_latest()` at the start of every `with_lua_config_on_main_thread` call to drain the pipe and replace `self.lua` with the latest.

**The "valley" doesn't exist**:
- Any in-flight `emit_user_var_event` task that already called `get_lua()` holds an `Rc` clone of the **old** VM. It runs to completion against the old VM (old handlers); the old VM is not dropped until the last `Rc` releases.
- New events after `update_to_latest` consumed the new VM run against the **new** VM (new handlers).
- Brief window between `LUA_PIPE.sender.try_send()` and the next `update_to_latest` call: events run against the **old** VM (benign — old handlers still exist).

**There is no point** at which the handler registry is empty.

**`emit_window_event`** (`mod.rs:2705-2737`) DOES have a one-deep queue (`EventState::InProgressWithQueued`) for `window-config-reloaded` and `update-status`. **`emit_user_var_event` does NOT use this** — bare `.detach()`, no dedup, no queue. Multiple in-flight events can run concurrently.

**Issue #3524** is about cross-window broadcast (already fixed structurally via `target_window_id`), not about reload drops.

### Decision — simplifies PRD §6.2 step 10, §6.15, §9 risks

The 250ms-up-to-5s heartbeat retransmit was justified entirely by the (false) reload-drop claim. Recommend **single retry at 2s + 5s ceiling**:
- Defends against unrelated transport-layer hiccups (truncated OSC writes, signal-interrupted syscalls).
- Cuts maximum retransmits per request from ~20 to 1.
- Eliminates heartbeat-amplification concerns under config-reload thrash.
- Replay guard stays — it serves the broadcast filter (#3524), which is real.

Drop the "events fired during reload are not queued" sentence everywhere it appears.

### Files cited
- `wezterm-gui/src/termwindow/mod.rs:2564-2608` (emit_user_var_event), `:2705-2737` (emit_window_event)
- `config/src/lib.rs:175-220` (LuaPipe, update_to_latest, with_lua_config_on_main_thread)

---

## 3. Wezterm minimum version floor — `20230408-112425-69ae8472`

### Findings

Source: wezterm changelog (https://wezterm.org/changelog.html) + per-feature doc pages.

| Feature used by wezsesh | Min version | Date |
|---|---|---|
| `wezterm.run_child_process` | 20200503-171512 | May 2020 |
| `window:perform_action()` | 20201031-154415 | Oct 2020 |
| `wezterm.log_*`, `window-config-reloaded` event | 20210314-114017 | Mar 2021 |
| `window:toast_notification()` | 20210502-154244 | May 2021 |
| `wezterm.background_child_process` | 20211204-082213 | Dec 2021 |
| `wezterm.action.SwitchToWorkspace` | 20220319-142410 | Mar 2022 |
| `mux.spawn_window`, `gui-startup`, `wezterm.GLOBAL`, `cli list --format json`, `cli list-clients.focused_pane_id`, `exit_behavior = "Close"` default | 20220624-141144 | Jun 2022 |
| `wezterm.time.call_after` | 20220807-113146 | Aug 2022 |
| `user-var-changed` event (OSC 1337 SetUserVar) | 20220903-194523 | Sep 2022 |
| `wezterm.plugin.require` | 20230320-124340 | Mar 2023 |
| `wezterm cli activate-pane`, `activate-tab` | 20230326-111934 | Mar 2023 |
| **`wezterm cli rename-workspace`** | **20230408-112425** | **Apr 2023** |

### Decision

Pin **`wezterm >= 20230408-112425-69ae8472`** in:
- README install matrix.
- `wezsesh doctor` (parses `wezterm --version`, surfaces if older).
- §9 risk row about CLI API drift.

### Three follow-ups requiring runtime verification
1. **`tty_name` field** in `cli list --format json`: claimed introduced in `20230408`. Doctor probes at runtime.
2. **`SwitchToWorkspace { spawn }` only-on-empty semantics**: documented since `20220319`, but exact guard behavior should be exercised against the floor version. (Validated separately in §6 below.)
3. **`mux.iter_windows_in_workspace`**: PRD §6.15 quotes this as the internal guard for SwitchToWorkspace.spawn. It is wezterm's *internal* implementation, not a Lua API call wezsesh makes; the line is descriptive prose.

---

## 4. TUI quit-mid-operation pattern — adopt lazygit-style inline prompt

### Findings

Survey of comparable TUIs:

| Tool | Pattern | Notes |
|---|---|---|
| **fzf** | Eager quit | `actAbort` calls `reader.terminate()`; no prompt, no block. Read-only selection tool. |
| **sesh** | Eager quit (fire-and-forget) | Thin fzf wrapper; no async ack loop. |
| **gum** | Eager quit | `gum confirm` / `gum spin` call `tea.Quit()` immediately on Ctrl-C/Esc. |
| **atuin** | Eager quit | `InputAction::Exit` breaks the `tokio::select!` loop; async DB queries dropped. |
| **tmux-sessionizer** | Synchronous (no TUI) | Bash script; `tmux switch-client` is sync. |
| **lazygit** | **Inline prompt — but ONLY for self-update** | `confirmQuitDuringUpdate()` shows `"An update is in progress. Are you sure you want to quit?"`. All other background ops use eager quit. The prompt exists precisely because partial completion of self-update leaves the binary broken. |
| **lazydocker** | Eager (optional generic confirm) | `taskManager.Close()` cancels tasks. |

**Prior art for "TUI exits but work continues silently"**: known footgun. `nohup`-style background-job semantics surprise users. For TUIs specifically, bubbletea's idiomatic mitigation is passing a context into Cmds for cancellation — which we cannot do (Lua-side ops are not cancellable).

### Decision — adopt lazygit's pattern, scoped to write ops

Pattern A (eager quit) is wrong for wezsesh because, unlike fzf/sesh/gum/atuin, our ops have side effects that persist after exit. Pattern B (block) is hostile if the socket never replies. Pattern C (inline prompt) matches lazygit's reasoning: the one class of op where silent completion creates inconsistent state.

**Concrete behavior** (added as PRD §6.16):
- Track `op_in_flight bool` flag on dispatch / clear on reply or timeout.
- `q` / `Esc` while idle → immediate quit. `defer os.Remove(sock)` cleans up.
- `q` / `Esc` while op_in_flight → render one-line inline status (NOT modal) `op in progress, quit anyway? [y/N]`. `y` → quit; reply dropped; Lua-side op completes silently. `n` → dismiss, stay in TUI.
- After quit-during-in-flight, the orphaned socket is reaped by the next launch's startup sweep (mtime > 60s).

Most ops are sub-100ms; the prompt rarely fires. It surfaces specifically for `load`/`restore` (1–3s).

### Files cited
- lazygit: `pkg/gui/controllers/quit_actions.go` (`quitAux`, `confirmQuitDuringUpdate`)
- fzf: `src/core.go` (`actAbort`, `EvtQuit`)
- gum: `spin/spin.go`
- atuin: `crates/atuin/src/command/client/search/interactive.rs`

---

## 5. `wezterm.GLOBAL` table-shape constraints — load-bearing PRD bug found

### Findings

Source: `lua-api-crates/share-data/src/lib.rs`.

GLOBAL is a `lazy_static!` `Value::Object` over `Arc<Mutex<BTreeMap<String, Value>>>`. The internal `Value` enum has two compound shapes:
- `Value::Object` — `BTreeMap<String, Value>` (string keys ONLY)
- `Value::Array` — `Vec<Value>` (sequential integer indices, 1-based Lua / 0-based internally)

**1. Integer keys on Object nodes raise a hard error.** The `__newindex` metamethod on Object only accepts `LuaValue::String` keys; passing any other type returns `Err("can only index objects using string values")`. The top-level `GLOBALS` is an Object, so `wezterm.GLOBAL.wezsesh_state[42] = {...}` will **fail at runtime**.

**2. Mixed-key tables are hard-rejected.** Table-detection heuristic at `lib.rs:175-215`: if `table.contains_key(1)` is true, every key must be a sequential integer; otherwise every key must be a string. There is no path for a mixed-key table.

**3. Deeply nested string-keyed Object tables work.** Recursive Object values are fine.

**4. The "set" pattern `{ ["01JAB..."] = true }` round-trips correctly.** String keys preserved as `BTreeMap` keys; `true`/`false` converted via `LuaValue::Boolean ↔ Value::Bool`.

**5. In-place mutation via chained `__newindex` works.** `Arc<Mutex<...>>` shares the pointer, so `wezterm.GLOBAL.wezsesh_state["42"].seen_ids["foo"] = true` persists without reassigning the whole table — but ONLY if all keys are strings.

### Decision — corrects PRD §6.14, §6.15

**Every pane-id key in the PRD must be `tostring(pane_id)`**, not the raw int. Updates:
- §6.14 example code: `wezterm.GLOBAL.wezsesh_state[tostring(pane:pane_id())] = {...}`
- §6.15 state-maps box: annotate `wezterm.GLOBAL.wezsesh_state[tostring(spawned_pane_id)]`
- §6.14 replay-guard line: `seen_ids` lookup goes through the stringified pane_id.
- Add a Lua helper `state.lua` that wraps `set_state(pane_id, ...) / get_state(pane_id)` and performs coercion in one place.

`wezsesh_requests[request_id]` is fine — ULIDs are strings.
`wezsesh_writing[file_path]` is fine — paths are strings.
`seen_ids` as `{ [ulid] = true, ... }` is fine — ULIDs are strings.

### Files cited
- `lua-api-crates/share-data/src/lib.rs:175-215` (table-shape detection), `:~390` (Object __newindex string-only)

---

## 6. `wezterm.run_child_process` and `background_child_process` — semantics

### Findings

Source: `lua-api-crates/spawn-funcs/src/lib.rs`.

| Function | Backing | Blocks Lua coroutine | Waits for child | Captures output |
|---|---|---|---|---|
| `run_child_process` | `smol::process::Command::output().await` (line ~42) | **Yes** (awaits) | Yes | Yes (stdout+stderr returned) |
| `background_child_process` | `cmd.spawn()` (lines ~54-72) | No | No | No (stdio inherited; stdin → `Stdio::null()`) |

Both are registered as `create_async_function` (line 8-11). They yield at the `.await` point; the GUI thread continues to process other events while the Lua coroutine is suspended. ~10ms `wezsesh keygen` is **not** a visible GUI stall, but DOES delay completion of `apply_to_config` by that amount.

`background_child_process` exit status is not surfaced to Lua; spawn-time errors *may* surface; post-spawn errors are silent.

### Decision — confirms PRD §6.14 keygen design

Plugin-load keygen via `wezterm.run_child_process({wezsesh_bin, 'keygen'})` is acceptable: ~10ms suspension of the config-eval coroutine is invisible to the user. Caching the result in `wezterm.GLOBAL.wezsesh_session_key` means we pay this once per wezterm session, not per keypress.

`background_child_process` is correct for the Lua → binary reply path (§6.4). Exit-status invisibility is fine because the actual reply arrives over the Unix socket, not via the child's exit code.

### Files cited
- `lua-api-crates/spawn-funcs/src/lib.rs:8-11` (registration), `:~42` (run_child_process .output().await), `:54-72` (background_child_process .spawn())

---

## 7. CLI JSON output schema — confirmed, with one parser gotcha

### Findings

Source: `wezterm/src/cli/list.rs`, `wezterm/src/cli/list_clients.rs`, `mux/src/tab.rs`, `mux/src/localpane.rs`. Recent main.

**`cli list --format json` (`CliListResultItem`)**:
- `tty_name: Option<String>` (line ~109) — present, not empty on darwin (returns `/dev/ttys003`-shape values via `self.pty.lock().tty_name()` on Unix; `None` on Windows; `null` only if pty doesn't report a name). The earlier suspicion that darwin might return empty is unfounded.
- `cwd: String` — actually a `file://` URL string (`url::Url::from_directory_path(path)` in `localpane.rs:1057`). **Important: empty string `""` when working dir is unknown, NOT null.** Parser must guard for `""` before URL-decoding.
- `is_active: bool` — per-tab only (`tab.rs:2156` sets it via `is_pane(pane, &active)` against per-tab `get_active_pane()` from line 1735). PRD claim correct.
- All other PRD-assumed fields present: `window_id`, `tab_id`, `pane_id`, `workspace`, `size`, `title`, `cursor_x`, `cursor_y`, `tab_title`, `window_title`, `is_zoomed`.
- Bonus fields not in PRD (informational): `cursor_shape`, `cursor_visibility`, `left_col`, `top_row`.
- Confirmed absent (PRD §6.13 already notes): `foreground_process_name`. Only available via Lua's `PaneInformation`, not CLI JSON.

**`cli list-clients --format json` (`CliListClientsResultItem`)**:
- `focused_pane_id: Option<PaneId>` — present. Serializes as int or null.
- Full schema: `username`, `hostname`, `pid`, `connection_elapsed` (Duration `{secs, nanos}`), `idle_time` (Duration), `workspace`, `focused_pane_id`, `ssh_auth_sock`.

### Decision — minor PRD update

- **§6.13 / §6.5**: add explicit note that `cwd` is `""` (empty string), not `null`, when unavailable. The Go-side `wezsesh find` and active-workspace detection must guard for `""` before URL-parse.
- **§6.13**: drop any tty_name darwin caveat (none currently in PRD; reaffirmed).
- All other claims unchanged.

### Files cited
- `wezterm/src/cli/list.rs:~109` (CliListResultItem with tty_name + cwd assignment)
- `wezterm/src/cli/list_clients.rs` (CliListClientsResultItem with focused_pane_id)
- `mux/src/tab.rs:1735, 2156` (is_active per-tab semantics)
- `mux/src/localpane.rs:520-531` (tty_name platform behavior), `:1057` (cwd URL build)

---

## 8. `SwitchToWorkspace.spawn` semantics + Resurrect `restore_workspace` error behavior

### Findings — `SwitchToWorkspace { spawn }`

Source: `wezterm-gui/src/termwindow/mod.rs:3021-3055`.

The exact match arm:

```rust
SwitchToWorkspace { name, spawn } => {
    let activity = crate::Activity::new();
    let mux = Mux::get();
    let name = name.as_ref().map(|n| n.to_string())
                  .unwrap_or_else(|| mux.generate_workspace_name());
    let switcher = crate::frontend::WorkspaceSwitcher::new(&name);
    mux.set_active_workspace(&name);                        // line 3029 — UNCONDITIONAL
    if mux.iter_windows_in_workspace(&name).is_empty() {    // line 3031 — exact PRD predicate
        let spawn = spawn.as_ref().map(|s| s.clone()).unwrap_or_default();
        // ... spawns window async
        switcher.do_switch();
        drop(activity);
    } else {
        switcher.do_switch();                               // line 3054 — silent skip
    }
}
```

- The PRD's predicate `mux.iter_windows_in_workspace(&name).is_empty()` is **verbatim correct**.
- `set_active_workspace` runs **before** the emptiness check — the workspace is activated regardless.
- When the workspace already has windows, the spawn block is **silently skipped**. No error, no log.

### Findings — Resurrect `restore_workspace`

Sources: `plugin/resurrect/workspace_state.lua`, `plugin/resurrect/window_state.lua`, `plugin/resurrect/state_manager.lua`, `plugin/resurrect/file_io.lua` (current main).

**`restore_workspace` (workspace_state.lua)**:
- Line 13: emits `restore_workspace.start`
- Line 19: bare `for i, window_state in ipairs(...)` — no pcall
- Line 43: calls `window_state_mod.restore_window(...)` — no pcall around it
- Line 45: emits `restore_workspace.finished`

If a `mux.spawn_window` raises mid-loop, the error propagates unhandled and `.finished` is **never emitted**. The PRD framing ("`.finished` fires only on the success path") is correct in effect — but more precisely: there is no error path; `.finished` is the only exit, reachable only when no error escapes. **`pcall`-wrapping the call from wezsesh is still necessary** for clean partial-restore handling.

**`restore_window` (window_state.lua:45-83)**: same pattern — bare `for` loop over tabs at line 56, no pcall, `.finished` at line 83 only on full success.

**`state_manager.save_state` (state_manager.lua:25-33)**: calls `file_io.write_state(...)` with no pcall, no error handling at this layer. Errors that escape `write_state` would propagate, but per below, `write_state` doesn't escape errors.

**`file_io.write_state` (file_io.lua:73-94)**:
- Line 73: emits `write_state.start`
- Line 78-82: encryption path uses pcall internally; on failure emits `resurrect.error`
- Line 89-91: non-encryption path checks `pub.write_file` ok-flag; on failure emits `resurrect.error` + `wezterm.log_error`
- **Line 94: `.finished` is always emitted regardless of success/failure.**

So `write_state` swallows errors and surfaces them via `resurrect.error` events; the `.start` / `.finished` pair is reliable as a write-window indicator (the resurrect race-mitigation gate in PRD §6.7 is sound).

**`save_state_dir` (state_manager.lua:155, used at :17)**: plain public field, written by `change_state_save_dir(directory)`, read directly throughout. No getter. The PRD's "not documented as stable API" wording is accurate; treat as best-effort with env-var override fallback.

### Decision — confirms PRD §6.7, §6.12, §6.15

All claims confirmed. One small clarification worth folding into PRD §6.12:

> The `restore_workspace.finished` event fires only when no Lua error escapes — not by deliberate success-gating, but because there is no error path in resurrect's restore code. `pcall`-wrapping is therefore necessary; otherwise a partial restore raises an uncaught error that wedges the `user-var-changed` listener.

Also fold into PRD §6.7: `file_io.write_state.finished` fires on both success and failure paths — so the wezsesh-writing gate in `wezterm.GLOBAL.wezsesh_writing` is correctly cleared whether or not the write succeeded.

### Files cited
- `wezterm-gui/src/termwindow/mod.rs:3021-3055` (SwitchToWorkspace match arm)
- `plugin/resurrect/workspace_state.lua:13,19,43,45` (restore_workspace structure)
- `plugin/resurrect/window_state.lua:45-83` (restore_window mirrors restore_workspace)
- `plugin/resurrect/state_manager.lua:25-33` (save_state), `:155` (save_state_dir field), `:17` (read site)
- `plugin/resurrect/file_io.lua:73-94` (write_state error swallowing + .finished always-emit)

---

## Summary of changes for PRD_V1 (so far)

### BLOCKER fixes (must change before code lands)

- **§6.14 / §6.15**: every GLOBAL pane-id key must be `tostring(pane_id)`. Integer keys raise a runtime error per `share-data/src/lib.rs`. (Spike 5.)
- **§6.4 Bubbletea coexistence narrative**: rewrite. Cmds run as concurrent goroutines; `/dev/tty` + `sync.Mutex` is REQUIRED, not defensive. Remove "render loop serializes Cmds" claim. (Spike 1.)

### Architectural simplifications

- **§6.2 step 10 / §6.15 / §8.1 / §9**: simplify retransmit to single retry at 2s + 5s ceiling. Reload-drop claim is unsupported by source. (Spike 2.)

### Updates

- **§7.3 doctor / §9**: pin `wezterm >= 20230408-112425-69ae8472`. (Spike 3.)
- **§5.3 / §6.16 (new)**: lazygit-style inline prompt for q/Esc when a write op is in flight; eager quit otherwise. (Spike 4.)
- **§6.14 keygen**: confirmed safe at plugin-load via `run_child_process`; key cached in GLOBAL. (Spike 6.)
- **§6.13 / §6.5 cwd parsing**: `cwd` is empty string `""`, not null, when unavailable; Go-side parser must guard before URL-decoding. (Spike 7.)
- **§6.12 restore semantics clarification**: `restore_workspace.finished` is reachable only on the no-error path because resurrect's restore has no error path at all (not by deliberate success-gating). pcall is still required. (Spike 8.)
- **§6.7 write-gate clarification**: `file_io.write_state.finished` fires on both success and failure — the wezsesh-writing gate clears correctly regardless. (Spike 8.)

### Status
v4 complete. All eight spikes resolved. Two BLOCKER fixes (spike 1 bubbletea narrative, spike 5 GLOBAL integer keys) and one architectural simplification (spike 2 retransmit) applied to PRD_V1.md. Other six spikes confirmed existing PRD claims with minor wording polish.
