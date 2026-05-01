# PRD Research Findings — Round 5 (architecture / process / attack-vector audit)

Goal of this round: surface the architectural missteps, process gaps, and attack vectors that survived rounds 1–4 by not being scrutinized. Four parallel research spikes plus six non-research-driven analyses. **Three BLOCKERs** identified — one a real security bug, one an architectural mistake, one a correctness gap that would surface as `IPC_TIMEOUT` in normal use.

---

## 1. Reply-socket timeout vs. real-world restore latency — 5s ceiling is empirically wrong

### Findings

Source: `MLFlexer/resurrect.wezterm` (recent main).

- `workspace_state.lua:19-43` iterates windows in a plain `for` loop; calls `window_state.restore_window` per window.
- `window_state.lua:56` iterates tabs in another plain `for` loop.
- `tab_state.lua:27,43` calls `pane:split()` synchronously per pane via a recursive `pane_tree.fold`.
- **Zero `wezterm.time.call_after`, `coroutine.yield`, or background dispatch in the entire restore path.** Pane spawning is strictly serial.

Each pane spawn is a `fork-exec($SHELL)` plus full profile load. Empirical numbers:
- Plain bash: ~50ms/pane.
- zsh + oh-my-zsh + plugins: 200–500ms/pane.

For a typical 24-pane workspace (3 windows × 4 tabs × 2 panes) with a moderate zsh setup: **8–15s** wall-clock for `restore_workspace` to return.

The PRD's hard 5s IPC ceiling (§6.4 listener + §6.2 step 10) is **routinely exceeded** by legitimate restores. After 5s, the binary surfaces `IPC_TIMEOUT` and exits — but resurrect continues running, leaving the user with: tabs spawning in the wezterm window, the wezsesh TUI showing a generic timeout error, and no clean correlation between the two.

The PRD's switch-leg flow (§6.15) already used a similar shape — Lua replies "switch initiated" immediately and the Go binary polls. That pattern was already partially present; the design just wasn't extended to the restore leg.

### Decision (BLOCKER) — split-reply pattern across §6.2, §6.3, §6.4, §6.15

Adopt a two-message reply protocol. Response `status` field disambiguates:
- `"completed"` (terminal) — op finished synchronously; TUI dismisses or refreshes.
- `"started"` (non-terminal) — op accepted; TUI dismisses immediately so the user watches resurrect spawn tabs in the wezterm window itself. Follow-up reply expected.
- `"partial"` (terminal) — op completed with non-fatal warnings (e.g., `RESURRECT_PARTIAL`).

Verb-specific use:
- `rename`, `delete`, `save`, `tag`, `pin`, `new` — single `"completed"` reply. 5s ceiling.
- `load`, `switch`, `switch+spawn`, anything triggering a restore — `"started"` immediately, then `"completed"`/`"partial"` when the op finishes. **5s for the first reply; additional 30s for the follow-up.**

The reply socket listener becomes an **accept loop** (was: single accept) — same socket, sequential connections, breaks on terminal status or ceiling. No second socket needed.

If the follow-up never arrives (>30s), surface a non-fatal toast (`restore confirmation timed out — workspace may still be loading`); the actual op continues regardless of our timeout.

### Files cited
- `plugin/resurrect/workspace_state.lua:19,43` (no error/yield)
- `plugin/resurrect/window_state.lua:56` (no error/yield)
- `plugin/resurrect/tab_state.lua:27,43` (synchronous pane:split)

---

## 2. Snapshot name handling — security bug + multiple correctness gaps

### Findings

#### Trust-store hash collision (CRITICAL — security bug)

PRD V1 §6.11 specified:
```
sha256(<absolute_sidecar_path> || "\n" || <command_bytes>)
```

This is **forgeable**. Workspace names are user-supplied and become sidecar paths. A workspace named `foo\nrm -rf ~` produces a sidecar at a path containing a literal newline byte. Under the `\n`-delimited construction, two different `(path, command)` pairs collide:
- `(path = "dir/foo\nrm -rf ~", cmd = "")`
- `(path = "dir/foo", cmd = "rm -rf ~")`

These hash to the same trust-store filename. A user trusting an empty hook on the malicious-named workspace silently grants trust to the second pair's command.

#### `ILLEGAL_NAME` undefined

PRD §6.3 listed `ILLEGAL_NAME` as an error code, but no triggers were spec'd. NUL bytes, newlines, control chars, empty names, names longer than the FS limit, `.`/`..`, and `\` (Windows path separator, reserved for future port) all need explicit rejection.

#### `+` encoding collision in resurrect

Resurrect's filename encoding `name:gsub("/", "+")` is one-way. Names `a/b` and `a+b` both encode to `a+b.json`. Reverse-decode of `a+b` corrupts to `a/b`. Inherited bug; needs documentation + warning, not a hard reject (`c++` projects are common).

#### Unicode normalization

darwin APFS NFD-normalizes filenames at the VFS layer; Linux ext4 doesn't. A workspace `~/code/café` saved on macOS and listed on Linux has different bytes; the state file's `usage` map (PRD §6.8) won't match.

#### Length limit

No limit specified. ext4/APFS: 255 bytes/component. After `.wezsesh.json` (13 bytes) + worst-case slash-encoding growth, 200 bytes is the practical safe maximum.

### Decisions (BLOCKER for the trust-hash; rest are gaps)

**§6.11 hash construction**: replace separator with length-prefixed encoding:
```
sha256( uint32_be(len(path)) || path_bytes
     || uint32_be(len(cmd))  || cmd_bytes )
```
Fixed-width prefixes; no path can carry its own length-prefix as suffix; zero-length cmd is distinct from any non-empty cmd.

**§6.17 (new) — Workspace name validation**: explicit rules for `ILLEGAL_NAME` triggers (length 1–200 bytes, no NULs, no newlines, no C0 controls, no leading/trailing whitespace, no `.`/`..`, no `\`). Names containing `+` are accepted with a UI warning. NFC normalization at every ingestion point. Doctor scans existing snapshots for name violations.

### Files cited
- PRD §6.11 (was: `\n`-delimited hash)
- PRD §6.3 (was: undefined `ILLEGAL_NAME`)
- `plugin/resurrect/utils.lua` (the gsub encoding)

---

## 3. Wezterm OSC 1337 parser limits — no major risks, two gotchas

### Findings

Source: `vtparse/src/lib.rs`, `vtparse/src/transitions.rs`, `wezterm-escape-parser/src/osc.rs`, `term/src/terminalstate/performer.rs`.

**Buffer size**: no byte cap. `MAX_OSC = 64` (`vtparse/src/lib.rs:312`) limits *parameter count* (semicolon-separated), not bytes. The OSC byte buffer is unbounded (`Vec<u8>` on heap). For `SetUserVar`, the format is `1337;SetUserVar=name=base64` — only 2 semicolons, so MAX_OSC is irrelevant. PRD's 4 KB self-imposed ceiling is conservative-but-correct.

**Terminators**: BEL (`\x07`) and ST (`ESC \\` or single C1 byte `\x9c`) both fire `OscEnd` correctly (`vtparse/src/transitions.rs:344`). All base64 alphabet chars (A-Z, a-z, 0-9, +, /, =) are in the `OscPut` (accumulate) range.

**NUL bytes (`\x00`)**: **silently ignored**, not rejected (`vtparse/src/transitions.rs:250`). The PRD's stated "reject sequences with embedded NULs at parse time" does NOT happen at the wezterm boundary. Validate client-side or NULs survive into the value (corrupt the base64 / fail to decode).

**Invalid base64**: `osc.rs:1321-1328` returns `Err`, propagated up to drop the entire OSC at `osc.rs:1276`. **No `user-var-changed` event fires.** From the binary's point of view, this is indistinguishable from a lost OSC — covered by the 2s single-retry resend.

**Multi-OSC writes**: each BEL fires `OscEnd` independently; back-to-back OSCs parse as separate events. Streaming state machine; partial OSCs survive across `read(2)` boundaries until BEL/ST.

**Rate limiting**: none in wezterm. Heartbeat (single retry) at 2s cadence is fine; even hundreds of OSCs/sec would not trigger throttling.

**CVEs**: no SetUserVar-specific CVEs. iTerm2's CVE-2019-9535 was about file-download names triggering shell exec via OSC 1337 `File=`; wezterm's `File=` path renders images, doesn't shell out. SetUserVar simply calls `user_vars.insert` + `handler.alert` (`performer.rs:830`) — no shell, no fs side-effects.

### Decisions

- **§6.17 NUL rule**: explicitly add "no NUL bytes in names" as `ILLEGAL_NAME` trigger; client-side enforcement is mandatory. Wezterm won't reject for us.
- **§9 risks**: add row noting that invalid base64 silently drops the OSC — covered by retry + ceiling.
- **§9 risks**: keep the OSC ceiling but note wezterm has no actual cap; the 4 KB is self-imposed.

### Files cited
- `vtparse/src/lib.rs:312` (MAX_OSC=64)
- `vtparse/src/transitions.rs:246-267` (osc_string match), `:250` (NUL drop), `:344` (OscEnd at exit)
- `wezterm-escape-parser/src/osc.rs:1276` (base64 error propagation), `:1321-1328` (decode)
- `term/src/terminalstate/performer.rs:830` (user_vars.insert)

---

## 4. `smart_workspace_switcher` event subscription — primary path is dead

### Findings

Source: `MLFlexer/smart_workspace_switcher.wezterm/plugin/init.lua` (recent main).

Events emitted by the plugin (verbatim names):
- `smart_workspace_switcher.workspace_switcher.start` (line ~98)
- `smart_workspace_switcher.workspace_switcher.selected` (~110)
- `smart_workspace_switcher.workspace_switcher.created` (~146)
- `smart_workspace_switcher.workspace_switcher.chosen` (~158)
- `smart_workspace_switcher.workspace_switcher.canceled` (~117)
- `smart_workspace_switcher.workspace_switcher.switched_to_prev` (~195)

**Critical gotcha**: every `wezterm.emit(...)` lives inside `pub.switch_workspace()` and `pub.switch_to_prev_workspace()`. These are `wezterm.action_callback` functions invoked when the user opens the plugin's *own* picker. They are NOT hooks on `act.SwitchToWorkspace`.

When wezsesh dispatches `act.SwitchToWorkspace` programmatically (which is always — wezsesh has its own picker), **none of these events fire.** PRD §6.15's "primary path" was architecturally incorrect.

There is no built-in wezterm `workspace-switched` event either.

### Decision (BLOCKER) — drop primary path, polling becomes the only path

PRD §6.15 rewritten: Lua dispatches `act.SwitchToWorkspace` and immediately replies `{"status": "started"}`. Go binary polls `wezterm cli list --format json` at 50ms cadence with a 5s ceiling, looking for the new workspace name. No third-party event subscription. Removes coupling to smart_workspace_switcher entirely; the two plugins coexist without protocol entanglement.

### Files cited
- `smart_workspace_switcher.wezterm/plugin/init.lua:98,110,117,146,158,195` (event emit sites)
- All emits are inside `pub.switch_workspace` / `pub.switch_to_prev_workspace` — user-picker scope only.

---

## 5. `wezterm.GLOBAL.seen_ids` unbounded growth (non-research, analysis)

### Finding

PRD §6.15 rule 4 swept `wezsesh_state[pid_key]` and `wezsesh_requests[id]` at 60s, but `seen_ids` *inside* a state entry was never pruned. A long-lived TUI session accumulates ULIDs forever.

### Decision

§6.15 rule 4 updated: `seen_ids` entries store `{ ts = os.time() }` (not bare `true`); sub-prune at 60s. Bounds the set to ~100 entries per session (one ULID per second × 60s rolling window).

---

## 6. Lua patterns vs Go RE2 for `exclude` (non-research, analysis)

### Finding

§7.1 said `exclude` accepts Lua patterns translated to Go regex at plugin load. Lua patterns differ enough from PCRE/RE2 that silent miscompilation is realistic:
- Character classes: `%d` ↔ `\d`, `%s` ↔ `\s`, `%w` ↔ `\w`, `%a` (no Go equivalent — must expand to `[A-Za-z]`).
- No `|` alternation in Lua; users will reach for it.
- No `{n,m}` quantifier ranges in Lua.
- Inside `[...]`, Lua uses `%a` for char-class includes; Go uses `[:alpha:]`.

### Decision

Switch `exclude` to accept **Go RE2 regex strings directly**. Compile at binary startup; invalid regex surfaces in `wezsesh doctor`. Documented in §7.1.

---

## 7. Signal handling for socket cleanup (non-research, analysis)

### Finding

§6.4 said "defer `os.Remove(sock)` on every exit path including signal handlers" — vague. Go's `defer` does NOT fire on `os.Exit` or on signal-driven termination (SIGHUP fires when the user closes the wezterm window).

### Decision

§6.4 spec'd the `signal.Notify` pattern: `internal/ipcsock.InstallSignalHandler(cleanup)` registers a goroutine that catches SIGINT, SIGTERM, SIGHUP, runs the cleanup, then `os.Exit(130)`. Stale `.sock` files are also reaped by the next launch's startup sweep — the explicit handler is for hygiene, not correctness.

---

## 8. HMAC constant-time compare (non-research, hygiene)

### Finding

§6.14 didn't specify the comparison. Variable-time string equality leaks key information in theory; practical risk is negligible because the threat model includes a same-UID attacker who can read `/proc/<pid>/environ` directly anyway.

### Decision

§9 added: Go side uses `hmac.Equal`; Lua side's vendored SHA module needs constant-time compare for the HMAC equality check. Hygiene only.

---

## 9. Snapshot file size + parse-depth caps (non-research, defense)

### Finding

§6.6 had no upper bound on snapshot file reads. Resurrect itself imposes none. A 1 GB snapshot file would OOM the binary.

### Decision

§6.6 added: 10 MiB per-file size cap (skip + warn beyond), 100-level JSON nesting cap. Realistic snapshots are <100 KiB.

---

## 10. State file concurrency (non-research, hygiene)

### Finding

§6.8's atomic-rename pattern gives last-writer-wins under concurrent TUI sessions. Lost increments of `usage.switch_count` are benign; lost `pins` toggles are mildly visible but rare.

### Decision

§6.8 documented as last-writer-wins explicitly. No locking; deferred to `flock(2)` if it ever surfaces in practice.

---

## Summary of changes for PRD_V1

### BLOCKER fixes (must change before code lands)

- **§6.11 (security)**: trust-store hash uses length-prefixed concatenation, NOT `\n` delimiter. Workspace names are user-controlled and become sidecar paths; a name with `\n` allows hash collision and trust forgery. (Spike 2.)
- **§6.2 / §6.3 / §6.4 / §6.15 (correctness)**: split-reply pattern. Restore-class verbs reply `"started"` immediately, follow-up `"completed"`/`"partial"` after restore returns. 5s ceiling for first reply, 30s additional for follow-up. (Spike 1.)
- **§6.15 (architecture)**: dropped `smart_workspace_switcher` event-subscription "primary path" — its events fire only from the plugin's own picker UI, never from programmatic `act.SwitchToWorkspace`. Go-side polling is the only path. (Spike 4.)

### Updates

- **§6.17 (new)**: Workspace name validation rules for `ILLEGAL_NAME`. Length 1–200 bytes, no C0/NUL/newline, no `.`/`..`, no `\`, NFC normalization, `+` warning. (Spike 2.)
- **§6.4**: signal handler spec for socket cleanup; Go's `defer` doesn't run on signals. (Analysis 7.)
- **§6.6**: per-file snapshot size cap 10 MiB, parse depth 100. (Analysis 9.)
- **§6.8**: documented last-writer-wins for state file. (Analysis 10.)
- **§6.15**: `seen_ids` 60s sub-pruning to bound memory growth. (Analysis 5.)
- **§7.1**: `exclude` accepts Go RE2 strings, not Lua patterns. (Analysis 6.)
- **§9**: added rows for NUL silent-swallow (must validate client-side), invalid-base64 silent drop, HMAC constant-time hygiene, OSC byte-cap clarification (wezterm has none; 4 KB is self-imposed). (Spike 3, Analysis 8.)

### Status
v5 complete. Three BLOCKERs fixed in PRD_V1.md (now v1.6). All ten findings folded in.
