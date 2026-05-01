# PRD Research Findings — Round 3 (validation pass)

Goal of this round: validate every architectural assumption in `PRD_V1.md` (post-v1.2) end-to-end, before code lands. Five parallel research spikes; results below. Several findings are **BLOCKERs** that mandate small but specific PRD changes.

---

## 1. Reply path — ship `wezsesh reply` subcommand, drop `nc -U`

### Findings

**`wezterm.background_child_process`** confirmed in source (`lua-api-crates/spawn-funcs/src/lib.rs`):
- Signature: `wezterm.background_child_process(args: Vec<String>)`. **No cwd, no env, no stdin override** (stdin hardcoded to `Stdio::null()`).
- Fire-and-forget via smol async pool; does not block GUI thread.
- Exit status not surfaced to Lua. Spawn-time errors *may* surface; post-exec errors are silent.

**`nc -U` portability is broken in real environments**:
- macOS / Arch / NixOS / modern Ubuntu/Debian: works (openbsd-nc).
- **Alpine (busybox `nc`): no `-U` support at all** — common in containers.
- **netcat-traditional (some legacy Debian): no `-U`** — still on some server installs.
- **Fedora/RHEL: ships `ncat` (nmap)** — `-U` exists, slightly different semantics.

**Lua's `%q` is NOT shell-safe**. Verified live: produces Lua-literal escaping, but `sh -c` will still expand `$variables` and `` `backticks` `` inside double quotes. The shell-quoting Lua code path in PRD_V1 is a real injection vector.

**`SUN_PATH` budget tight**: darwin macOS = 104; Linux = 108. Measured on this machine: `$TMPDIR/wezsesh-<user>/<26-char-ULID>.sock` = **98/104**, six bytes of slack. Long usernames blow past it.

**Latency**: `sh -c "printf ... | nc -U ..."` measured at ~4ms warm / ~8ms cold (3 forks: sh, printf, nc). Single Go binary fork: ~1-2ms.

### Decisions

**→ Ship `wezsesh reply <sock> <b64json>` subcommand.** Reasons (priority order):
1. Eliminates shell-injection / quoting surface entirely — argv passed direct to execve.
2. Eliminates Alpine-busybox / netcat-traditional portability hole.
3. Lower, more predictable latency (one fork vs three).
4. ~30 LOC implementation; reuses existing binary.
5. Fewer lines that can fail than fsnotify / send_text alternatives.

**→ Drop `nc -U` from PRD §6.4.** No fallback to nc; the helper is the path.

**→ Reply directory selection**:
- Linux: `$XDG_RUNTIME_DIR/wezsesh/` (typically `/run/user/<uid>/wezsesh/`).
- darwin: **`/tmp/wezsesh-<uid>/`** (NOT `$TMPDIR` — too deep for SUN_PATH).
- File mode: parent dir 0700; explicit `os.Chmod(sock, 0o600)` after Listen.

**→ Sock filename**: `<8-hex-chars>.sock` (sufficient for our concurrency; saves SUN_PATH budget). Generated from first 8 hex of the request_id ULID.

**→ Lua side reply emission**:
```lua
wezterm.background_child_process({
    wezsesh_bin,    -- absolute path resolved at plugin load
    "reply",
    reply_sock,     -- from request payload, validated to be under our runtime dir
    b64,            -- base64 of canonical-JSON response
})
```

**→ Go side reply subcommand** (~30 LOC):
```go
func runReply(args []string) error {
    payload, err := base64.StdEncoding.DecodeString(args[1])
    if err != nil { return err }
    c, err := net.DialTimeout("unix", args[0], 1*time.Second)
    if err != nil { return nil }  // parent timed out — silent
    defer c.Close()
    c.SetWriteDeadline(time.Now().Add(1 * time.Second))
    _, err = c.Write(payload)
    return err
}
```

**→ Crash cleanup**: at startup, scan `<reply_dir>/*.sock`, remove any with mtime > 60s.

---

## 2. Cryptographic primitives — vendor pure_lua_SHA, write own canonical-JSON

### Findings

**SHA-256 / HMAC**:
- `Egor-Skriptunoff/pure_lua_SHA` is the right vendor: single file `sha2.lua`, MIT, built-in `sha2.hmac(...)`, RFC-tested, 0.2-0.3 ms per <1KB payload on Lua 5.4.
- Pin specific commit; vendor at `plugin/wezsesh/vendor/sha2.lua` with SHA-256 of vendored file in `SOURCES.lock`.

**Canonical JSON — surprise discovery**: per Lua-API verification (research §3 below), `wezterm.json_encode` **does sort keys alphabetically** because wezterm's `Cargo.toml` declares `serde_json = "1.0"` without the `preserve_order` feature, so it serializes to a `BTreeMap`-equivalent ordering.

But `json_encode` is still **not** safe for HMAC because:
- Number formatting may include trailing `.0` for floats.
- HTML escaping is automatic (`&`, `<`, `>` → `<` etc.).
- Different control-char escape sequences vs. Go's `encoding/json`.

We still write our own canonical-JSON serializer on both sides. The `json_encode` sort-by-default is a useful fallback for parsing-side determinism but not for signing-side byte-identicality.

### Decisions

**→ Adopted canonical JSON spec**:
1. UTF-8, no BOM, no whitespace, no trailing newline.
2. Object keys sorted by byte-wise lexicographic order of UTF-8 encoding.
3. Numbers: integers only (`-?[0-9]+`). Floats are an error.
4. Strings: escape only `\"`, `\\`, `\b`, `\f`, `\n`, `\r`, `\t`, `U+0000..U+001F` as `\u00xx`. Raw UTF-8 elsewhere.
5. `true`/`false`/`null` literals; arrays `[v,v,...]`; objects `{"k":v,...}`.

**→ Lua serializer**: ~60 LOC, reject non-string keys / floats / NaN / Inf at serialize time. Use `table.concat` for performance.

**→ Go serializer**: ~80 LOC, type-switch on `any`, reject `float32`/`float64`/non-integer `json.Number`.

**→ HMAC wrapper**: ~30 LOC pure-Lua wrapping `pure_lua_SHA`. Standard ipad/opad construction. Constant-time compare in 5 LOC.

**→ Test plan**:
- RFC 4231 HMAC-SHA-256 vectors (all 7 cases).
- 20 fixture canonical-JSON payloads exercising sort, nested objects, Unicode keys, control chars.
- Both Go and Lua produce byte-identical output → `sha256(bytes)` equal in CI.
- **CI parity gate**: any divergence blocks merge.

---

## 3. Resurrect + Lua API verification — mostly correct, three corrections, several discoveries

### Confirmations (verified by reading source, not just docs)

| Claim | Status | Evidence |
|---|---|---|
| `resurrect.file_io.write_state.{start,finished}` events with `(file_path, event_type)` | ✓ | `file_io.lua:73,94` |
| `wezterm.background_child_process` exists, fire-and-forget, no env/cwd | ✓ | `spawn-funcs/src/lib.rs` |
| `wezterm.GLOBAL` survives config reload | ✓ | `share-data/src/lib.rs` (`lazy_static!`) |
| `wezterm.time.call_after(seconds: f64, fn)` | ✓ | `time-funcs/src/lib.rs:151-173` |
| `pane:pane_id()` immediately available after `mux.spawn_window` | ✓ | `mux/src/lib.rs:259-263` — pane_id allocated synchronously before tuple is built |
| `mux.spawn_window` returns `(tab, pane, window)` | ✓ | same |
| `act.SwitchToWorkspace { name, spawn }` creates if missing | ✓ | `wezterm-gui/src/termwindow/mod.rs:3031` |
| `user-var-changed` signature `function(window, pane, name, value)` | ✓ | `wezterm-gui/src/termwindow/mod.rs:1932` |

### Corrections

1. **Base64 is already decoded by wezterm before Lua sees it.** `wezterm-escape-parser/src/osc.rs:1267-1278`: the OSC 1337 SetUserVar parser base64-decodes before dispatching the alert. Lua handler receives plain UTF-8 strings. **Do not base64-decode in Lua.** PRD §6.3 needs to clarify: binary base64-encodes for wire format; Lua receives decoded plaintext.

2. **`SwitchToWorkspace.spawn` is IGNORED when switching to an existing workspace.** `wezterm-gui/src/termwindow/mod.rs:3031`: the `spawn` block runs only when `mux.iter_windows_in_workspace(&name).is_empty()`. If we want to spawn into an existing workspace, use `wezterm.mux.spawn_window { workspace = name, ... }` directly. Affects our switch verb implementation when an existing workspace gets reactivated with a CWD hint.

3. **Resurrect APIs do NOT throw on errors.** `save_state` and friends swallow write/encryption errors and emit `resurrect.error` events instead. **Subscribe to `resurrect.error`** for save-failure observability — `pcall`-wrapping is necessary but not sufficient.

### Newly discovered (worth using)

- **`pane:get_user_vars()`** exists (`mux/src/pane.rs:195`). Returns the full HashMap. Useful as a fallback reply path or for diagnostic polling.
- **`resurrect.error(message)` event** — fires on save/load/decrypt/encrypt errors and from several other internal failure paths.
- **Many other resurrect events** (`load_state.{start,finished}`, `delete_state.{start,finished}`, `restore_workspace.{start,finished}`, etc.) — useful instrumentation hooks.
- **`wezterm.serde.*`** namespace — yaml/toml encode/decode, json_encode_pretty.
- **Plugin clone path encoding**: `${DATA_DIR}/plugins/<sZs-encoded-URL>/`. Ugly but predictable; don't hard-code, use `wezterm.plugin.list()` to find.

### Decisions

**→ Subscribe to `resurrect.error`** in our plugin — pipe to `wezterm.log_warn` and to our op-result error channel when the error correlates with an in-flight request.

**→ Update PRD §6.3** to clarify: binary base64-encodes for OSC wire format; wezterm decodes before Lua handler.

**→ Update PRD §6.13 / switch verb** to handle the `SwitchToWorkspace.spawn` ignored-on-existing-workspace case.

---

## 4. End-to-end POC dry-run — 4 BLOCKERs identified, all fixable

### BLOCKER 1: `WEZSESH_PANE_ID` is impossible to set pre-spawn

PRD §6.14 lists `WEZSESH_PANE_ID` as a per-launch env var, but `wezterm.mux.spawn_window` returns the pane only **after** spawn. Env vector is fixed by then.

**→ Fix**: drop `WEZSESH_PANE_ID` from the env-var spec. The binary reads `WEZTERM_PANE` (which wezterm injects automatically into spawned panes) and trusts it. Lua side stores the pane_id in GLOBAL keyed by spawned_pane_id.

### BLOCKER 2: Listener-before-OSC ordering depends on bubbletea Cmd semantics

`net.Listen` for the reply socket must run synchronously inside `Update`, not inside the returned `tea.Cmd`. If a developer puts the Listen inside the Cmd, the OSC may fire first and Lua's reply hits a closed socket.

**→ Fix**: spec a helper `internal/ipcsock/StartListener(reply_sock) (chan Response, cleanup func(), error)` that performs `Listen` synchronously. Document loudly: "Call this from `Update`, NOT from the returned `tea.Cmd`."

### BLOCKER 3: GLOBAL state keying not specified

PRD §6.14 conflates `wezsesh_state` (single object) with `wezsesh_requests` (keyed). For multi-launch (two windows opening wezsesh simultaneously), `wezsesh_state` must be keyed.

**→ Fix**: clarify in PRD:
- `wezterm.GLOBAL.wezsesh_state[spawned_pane_id]` = `{hmac_key, target_window_id, seen_ids = {}, spawned_at}`. Keyed by **`spawned_pane_id`**.
- `wezterm.GLOBAL.wezsesh_requests[request_id]` = `{spawned_pane_id, started_at}`. Keyed by **`request_id`** (ULID).
- Two separate maps, two different keys, both in `wezterm.GLOBAL`.

### BLOCKER 4: Missing error code for snapshot load failure

PRD §6.3 enumerates `RESURRECT_PARTIAL` for restore failures, but if `state_manager.load_state` fails (file deleted between picker open and Enter, encryption prompt rejected, parse error), there's no specific code.

**→ Fix**: add `SNAPSHOT_LOAD_FAILED` to the error code enum in §6.3.

### Other ship-risk fixes

- **Use `window:active_pane()` not source pane** for `window:perform_action(SwitchToWorkspace, ?)`. The source pane is the wezsesh pane, possibly in a different window from `window`. Internally `SwitchToWorkspace` needs the pane for keytable resolution; passing a different-window pane may behave unexpectedly.
- **Polling for new workspace creation should happen Go-side**, not Lua-side. Lua's `wezterm.run_child_process` blocks the event loop; doing the polling Lua-side is awkward. Instead: Lua dispatches `SwitchToWorkspace`, replies "switch initiated"; Go binary polls `wezterm cli list --format json` for the new workspace presence (10ms cadence, 1s ceiling), then sends a follow-up `restore` op. Adds a roundtrip but keeps Lua handlers fast.
- **TTL sweep for orphaned GLOBAL state**: `window-config-reloaded` handler (or a scheduled `call_after(30, ...)`) prunes `wezsesh_state` entries with `os.time() - spawned_at > 60`. Bounds memory.

---

## 5. Bubbletea performance — warm path comfortable, cold path NOT achievable on darwin

### Findings (live measurements on this darwin-arm64 / M1 / Go 1.25.7)

| Scenario | p50 | p95 |
|---|---|---|
| Minimal Go binary (warm cache) | 2.6 ms | 2.95 ms |
| Minimal bubbletea Run+Quit (warm) | 2.9 ms | 3.6 ms |
| Bubbletea + 100 parallel JSON snapshot reads (warm) | 4.8 ms | 5.2 ms |
| **Same binary copied to fresh path (defeats page cache)** | **217 ms** | **258 ms** |
| Stripped 3.3 MB binary, fresh path | 202 ms | 342 ms |

The dominant cost in the cold case is **darwin's per-page mmap + adhoc-codesign verification of the unsigned Go binary**. Stripping doesn't help materially. We cannot beat this in user code.

### Decisions

**→ Restate the cold-start SLO honestly**: "warm p95 < 50 ms; cold p95 < 350 ms." PRD §10 currently claims sub-200ms p95 cold start, which is **not achievable on darwin** for a 4 MB Go binary that hasn't been launched recently. Update.

**→ Daemon mode is deferred to v0.2.** The architecture (per-request Unix sockets, fire-and-forget reply) lends itself to a long-running `wezsesh --daemon` started once per wezterm session, with picker invocations sent over an existing socket. Defers cold-start cost to once-per-wezterm. **Out of scope for v0.1** but worth flagging the migration path.

### Adopted bubbletea stack

Pin the following (matches the Charm ecosystem standard — used by glow, gum, sesh, bubbles, huh):

| Component | Pin |
|---|---|
| `github.com/charmbracelet/bubbletea` | `v1.3.5` |
| `github.com/charmbracelet/bubbles` | `v0.21.0` |
| `github.com/charmbracelet/lipgloss` | `v1.1.0` |
| `github.com/charmbracelet/x/ansi` | `v0.9.3` |
| `github.com/charmbracelet/huh` | latest stable |
| `github.com/sahilm/fuzzy` | `v0.1.1` |

**Avoid bubbletea v2** (RC, API churn).

### OSC routing pattern

- Open `/dev/tty` separately (NOT `os.Stdout`) for OSC writes; wrap in a `sync.Mutex`-protected writer. This keeps OSC bytes off the bubbletea render stream.
- Emit OSC via a `tea.Cmd` returning `nil`; chain with `awaitReply` via `tea.Sequence(emitOSC, awaitReply)` for correct ordering.
- Reply socket runs in a dedicated goroutine that calls `program.Send(replyMsg{...})`. Reference: `bubbletea/examples/send-msg/main.go`.

### Modal pattern

Use `charmbracelet/huh.Form` for rename / save / new dialogs. Don't roll our own modal framework. Embed `mainModel { picker, dialog *huh.Form, dialogActive bool }`; route Update to the form when dialogActive.

---

## Summary of changes for PRD_V1

### BLOCKER fixes (architectural)

- **§6.4 / §6.14**: drop `WEZSESH_PANE_ID`; binary reads `WEZTERM_PANE` instead.
- **§6.4**: replace `nc -U` reply with `wezsesh reply <sock> <b64json>` subcommand. Update Lua emission code. Note darwin uses `/tmp/wezsesh-<uid>/`, Linux uses `$XDG_RUNTIME_DIR/wezsesh/`. Sock filename = first 8 hex of request_id.
- **§6.4 / IPC sequence**: document that `net.Listen` must run synchronously inside `Update`, not in the returned `tea.Cmd`. Spec the `internal/ipcsock/StartListener` helper.
- **§6.3**: add `SNAPSHOT_LOAD_FAILED` error code. Clarify that wezterm base64-decodes before Lua sees the value.
- **§6.14**: clarify `wezterm.GLOBAL.wezsesh_state[spawned_pane_id] = {...}` (keyed by pane_id) and `wezterm.GLOBAL.wezsesh_requests[request_id] = {...}` (keyed by ULID). Document TTL sweep (60s).

### Ship-risk fixes

- **§6.13 / switch verb**: handle the `SwitchToWorkspace.spawn` ignored-on-existing-workspace case. Use `wezterm.mux.spawn_window { workspace = name }` for re-spawning into existing.
- **§6.15 (workspace switch sequencing)**: do the post-switch polling on the Go side, not Lua. Lua dispatches and immediately replies "switch initiated"; Go observes new workspace via `wezterm cli list` polling then sends follow-up `restore` op.
- **§6.12 / §6.x**: subscribe to `resurrect.error` event for save-failure observability.
- **§6.x dispatcher**: use `window:active_pane()` not source pane when calling `window:perform_action(SwitchToWorkspace, pane)`.

### Updates

- **§6.10 plugin layout**: add `cmd/wezsesh/reply.go`. Add `internal/ipcsock/`. Add Lua `vendor/sha2.lua`, `canonical_json.lua`, `hmac.lua`, `b64.lua`. Pin bubbletea + bubbles + lipgloss + fuzzy + huh versions in `go.mod`.
- **§7.2 binary CLI**: add `wezsesh reply <sock> <b64json>` subcommand.
- **§10 success criteria**: restate cold-start as "warm p95 < 50 ms; cold p95 < 350 ms." Document daemon-mode migration path.
- **§8.2 deferred**: add daemon-mode (v0.2) for cold-start optimization.

### Status
Bump to "v1.3 (validation pass complete)". Architecture is now end-to-end verified.
