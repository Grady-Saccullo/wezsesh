# PRD Research Findings — Round 2 (deep architecture pass)

Consolidates results from six parallel research spikes on Tier-1 unknowns. Several findings invalidate parts of `PRD_V1.md` and require architectural pivots.

Format mirrors v1: source citations inline; **boldface "→" lines** are decisions to fold into the PRD.

---

## 1. IPC mechanics — OSC 1337 out, Unix socket back

### Findings

**Stdout buffering**: Go's `os.Stdout` is unbuffered (no `bufio` wrapper); under `wezterm cli spawn`, the binary's stdout is connected to a PTY. A single `os.Stdout.Write` produces one `write(2)` syscall. **No `Sync()` needed.** Empirically confirmed: OSC 1337 sequences are absorbed and dispatched.

**Bubbletea + OSC**: writing OSC bytes from a goroutine while bubbletea's render loop is also writing to stdout will race-corrupt at byte boundaries. The supported way to emit OSCs during a bubbletea program is via a `tea.Cmd` (the render loop serializes Cmds). **Before `tea.Run()`** and **after it returns**: safe direct write. Bubbletea's stdin parser does not handle OSCs cleanly (PR #467 fixed CSI only) — never use stdin as a return channel.

**Lua → Go return channel** (the critical unknown):
- **`wezterm cli list --format json` does NOT include user-vars.** Verified by reading wezterm source (`wezterm/src/cli/list.rs::CliListResultItem`) and live `cli list` output. Issue [wezterm#7307](https://github.com/wezterm/wezterm/issues/7307) requests this; not landed. **Polling for response via CLI is dead.**
- **`pane:inject_output(text)`** feeds bytes into the **terminal display side** via `pane.perform_actions(actions)` — programs running in the pane never read these bytes. **Useless as a return channel.**
- **`pane:send_text(text)`** writes to the PTY master = the program's stdin. This *is* a real reverse channel, but bytes get parsed by bubbletea's stdin reader (sharp edge). **Avoid as primary path.**
- **Filesystem rendezvous + fsnotify**: works, ~5-20ms latency, slightly higher complexity.
- **Unix-domain socket**: works, sub-5ms latency, sidesteps bubbletea-stdin entirely. Lua side can write via `wezterm.background_child_process({"sh","-c","printf %s | nc -U sock"})`.

**Spawn semantics**:
- **`wezterm cli spawn` has NO `--env` flag.** Verified locally. Workaround: wrap in argv with `env K=V`.
- **No `--hold` / `--exit-behavior` per-spawn flag.** Issue [wezterm#6643](https://github.com/wezterm/wezterm/issues/6643) open. Default `exit_behavior = "Close"` (since 2022) handles clean exit — pane auto-closes.
- `--cwd`, `--workspace`, `--new-window`, `--window-id`, `--pane-id`, `--domain-name` exist.

**Local panes only**: `pane:send_text` and `inject_output` "work for local panes but not for multiplexer panes." Our socket approach also requires shared filesystem with the wezterm process. **v0.1 explicitly local-only.**

### Decisions

**→ Primary IPC**: OSC 1337 from binary → Lua. Unix-domain socket from Lua → binary.

- Binary creates `$XDG_RUNTIME_DIR/wezsesh/<request_id>.sock` (fallback `/tmp/wezsesh-<request_id>.sock` on macOS).
- Binary passes the path as a `reply_sock` field in the OSC payload.
- Binary's Go-side listener spawns *before* the OSC is emitted (avoid race).
- Lua handler dispatches the op, then writes JSON response via `wezterm.background_child_process({"sh","-c","printf %s | nc -U sock"})`.
- Timeout: 2s on the Go side; surface "ipc timeout" as an error.

**→ Spawn invocation**: `wezterm cli spawn --new-window -- env WEZSESH_X=... WEZSESH_Y=... wezsesh` (env wrapping replaces missing `--env`).

**→ Bubbletea OSC writes**: route via `tea.Cmd`, never raw goroutine writes during render. Pre/post-Run direct writes are fine.

**→ Multiplexer scope**: PRD §3 already says "no SSH/multi-host" — keep, and add explicit "wezsesh requires local mux; multiplexer/SSH workspaces are unsupported."

---

## 2. OSC 1337 injection attack mitigation

### Findings

The threat is real and mainstream: terminal escape injection via `cat`-able files / npm postinstalls / `curl` output. iTerm2 had CVE-2019-9535 for the same class. Pane-attribution from the Lua side is reliable: the `pane` argument to `user-var-changed` is the originating pane (verified at [wezterm#3524](https://github.com/wezterm/wezterm/issues/3524)).

**Wezterm has zero Lua crypto primitives**: no `wezterm.hash`, no `hmac`, no `sha256`, no `encode_base64`, no random. Verified by sweeping the entire `wezterm.*` Lua API index.

**Spawn API**: `wezterm.mux.spawn_window` returns `tab, pane, mux_window`. We capture `pane:pane_id()` at spawn time. Pane IDs are monotonic, immutable for the pane's lifetime, not reused while wezterm is running.

**No wezterm-level filtering** of user-var OSCs by source pane — there's no config knob like "only honor OSC user-vars from this pane."

### Decisions — defense in depth

**→ Layer 1: pane-ID binding.** At TUI spawn, plugin records the spawned binary's `pane_id`. The `user-var-changed` listener rejects events whose `pane:pane_id()` doesn't match. This blocks the dominant attack (malicious data in another pane).

**→ Layer 2: HMAC-SHA-256 payload signing.** At spawn, plugin generates a 256-bit random secret; passes it via env var. Every payload from the binary carries an `hmac` field. Listener verifies; rejects mismatches.

- Secret generation: `wezterm.run_child_process({'openssl','rand','-hex','32'})`; fallback `/dev/urandom` via Lua `io.open`.
- Vendored pure-Lua SHA-256 (~200 LOC, MIT — e.g., [pure_lua_SHA](https://github.com/Egor-Skriptunoff/pure_lua_SHA)) and base64 (~20 LOC).
- Performance: ~0.3-0.8ms per payload — negligible vs. the ops they trigger.

**→ Layer 3: replay/freshness guard.** Payload includes `ts` (Unix timestamp) and `id` (ULID). Plugin rejects payloads outside a 30s window or with already-seen `id`. The wezterm broadcast bug (events fire in every window) means the same payload arrives N times; replay guard collapses to one dispatch.

**→ Wire format** (JSON, base64'd into OSC value):

```json
{
  "v": 1,
  "id": "01JAB...ULID",
  "ts": 1745875200,
  "op": "switch",
  "args": { "name": "~/code/foo" },
  "reply_sock": "/tmp/wezsesh-<id>.sock",
  "hmac": "9f86d081..."
}
```

`hmac` = `lowerhex(HMAC_SHA256(key=secret_bytes, msg=canonical_json(payload_minus_hmac)))`. Canonical: keys sorted lex, no whitespace, integers as decimal.

**→ Env vars at spawn**:
- `WEZSESH_HMAC_KEY` — hex 32 bytes, regenerated per spawn
- `WEZSESH_PANE_ID` — decimal pane_id of spawned pane
- `WEZSESH_PROTO_VERSION` — `"1"`

**→ Failure mode**: silent drop + `wezterm.log_error` for HMAC mismatch and foreign-pane events. Visible toast only on HMAC mismatch (foreign-pane drops would spam toasts in normal use). Never raise a Lua error — uncaught errors in event handlers can wedge the wezterm event loop.

**→ Residual risks documented**:
- Env-var leak via `/proc/<pid>/environ` (any process running as same UID can read). Standard Unix caveat; accept.
- In-pane attacker (something running in the wezsesh pane). Mitigated by us *not* exec-ing a shell or arbitrary command in the TUI pane — the binary runs alone and no shell is spawned. If we ever add "shell out from TUI," we must rotate the key or scrub env first.
- Wezterm broadcast (#3524): mitigated by replay guard.
- Pure-Lua SHA-256 supply chain: pin commit, vendor in tree, audit (~200 LOC is reviewable).

---

## 3. Resurrect encryption — opaque, gracefully degrade

### Findings

Resurrect supports three external CLI encryption backends: `age` (default), `rage`, `gpg`. Verified in `plugin/resurrect/encryption.lua` and `plugin/resurrect/file_io.lua`.

**On-disk format when encryption is enabled**: **whole-file ciphertext**. The file at `<save_state_dir>/workspace/<encoded-name>.json` becomes raw binary `age` v1 (no `-a` armor) or binary OpenPGP (no `--armor`). The `.json` extension is misleading — content is opaque ciphertext.

**No JSON wrapper**, no metadata field, no plaintext envelope. wezsesh cannot extract tab counts, pane CWDs, or process info without invoking the user's decrypt key.

**Configuration**: opt-in via `resurrect.state_manager.set_encryption({ enable = true, method = "age", private_key = ..., public_key = ... })`. **Default is `enable = false`.** No env var, no config file — purely a Lua call.

**Detection** (no machine-readable flag on disk): magic-byte sniff. `age`/`rage` files start with `age-encryption.org/v1\n` ASCII. `gpg` files start with an OpenPGP packet tag (high bit set). Plaintext JSON starts with `{` `[` or whitespace. Reading first 32 bytes and checking is reliable.

**Lua-shim option** (deferred): `state_manager.load_state(name, type)` transparently decrypts and returns a Lua table; we could marshal via `wezterm.json_encode` to a temp file. Cost: spawn age/gpg per workspace per render — slow + risk of gpg-agent prompt.

### Decisions

**→ v0.1 behavior**: graceful degrade.
- Picker still lists workspaces (filename → name decoding works for encrypted files; just opaque content).
- Preview pane shows `(encrypted snapshot — preview unavailable)` when first-byte sniff says encrypted.
- Tab count / CWD / process columns omit gracefully.
- `wezsesh doctor` warns when any `.json` files in the snapshot dir have non-JSON magic bytes.

**→ Document as known limitation**, not as a deferred feature. Encryption is opt-in; default-off; most users won't have it. A footnote in §6.6 covers it.

**→ Future (deferred)**: optional Lua-shim cache. Plugin hooks `resurrect.file_io.write_state.finished`, calls `state_manager.load_state` to get the decrypted table, marshals to `$XDG_RUNTIME_DIR/wezsesh/cache/<hash>.json`. Binary reads cache files when primary is encrypted.

---

## 4. Plugin↔binary version handshake

### Findings

**Hard constraint**: `wezterm.plugin.list()` returns only `{url, component, plugin_dir}`. No commit hash, no tag, no git metadata. Verified at [list.md](https://wezterm.org/config/lua/wezterm.plugin/list.html). The plugin **cannot** derive its own version from wezterm metadata.

`wezterm.run_child_process({"git", "-C", plugin_dir, "describe", "--tags"})` works but adds a `git`-on-PATH dependency and runs on every config reload. Not worth it.

**Go binary version**: `runtime/debug.ReadBuildInfo()` populates `Main.Version` only for `go install module@vX.Y.Z` builds. `go build` and Nix and release tarballs all need `-ldflags="-X main.version=v..."` injection.

**Toast notifications**: `window:toast_notification(title, message, url, duration_ms)` — **window method, not top-level**. Must be wired through `gui-startup` event, not callable at plugin-load time.

### Decisions

**→ Plugin self-knowledge**: embed `M.VERSION = "0.1.0"` in `plugin/init.lua`. Bump on every tagged release. CI asserts the constant matches the about-to-be-cut tag.

**→ Binary self-knowledge**: layered.
1. `var version = "dev"` at package scope.
2. CI / release / Nix builds with `-ldflags="-X main.version=v$(git describe --tags --always)"`.
3. At runtime, fall back to `debug.ReadBuildInfo().Main.Version` for `go install module@vX.Y.Z` users.

**→ Compatibility**:
- `0.x`: pin minor (plugin `0.MINOR.x` requires binary `0.MINOR.x`, any patch).
- `1.x+`: "binary minor ≥ plugin minor" so newer binary serves older plugin.

**→ Check timing**: once at plugin load, memoized. `wezterm.run_child_process({"wezsesh", "--version"})`. ~10ms; cheap enough for every config reload.

**→ Mismatch UX**:
- Always `wezterm.log_error(...)`.
- One-shot `window:toast_notification(...)` registered through `wezterm.on('gui-startup', ...)`.
- **Never raise a Lua error** — it would break the user's whole wezterm config. Instead: keybindings stub out to a "version mismatch" toast.

**→ Code shape** (paste into `plugin/init.lua`):

```lua
local M = {}
M.VERSION = "0.1.0"

local function parse(v) return v:match("v?(%d+)%.(%d+)%.(%d+)") end

local function compatible(plugin_v, bin_v)
  local pM, pm = parse(plugin_v); local bM, bm = parse(bin_v)
  if not pM or not bM then return false end
  if pM == "0" or bM == "0" then return pM == bM and pm == bm end
  return pM == bM and tonumber(bm) >= tonumber(pm)
end
```

(See research-findings v2 transcript for full handshake code.)

---

## 5. Pane lifecycle, resurrect race, plugin reload

### Findings

**TUI exit**: default `exit_behavior = "Close"` (since 2022) auto-closes the pane on clean exit. **No work needed for the happy path.** Programmatic close (error path): `window:perform_action(wezterm.action.CloseCurrentTab{ confirm = false }, pane)`.

**Resurrect periodic_save race is real**: `plugin/resurrect/file_io.lua` uses `io.open(path, "w+")` (truncate + rewrite, **not atomic**). A reader concurrent with a write can see a half-written file. `state_manager.periodic_save` iterates without cross-file locking.

Resurrect emits these events (verified):
- `resurrect.file_io.write_state.{start,finished}(file_path, event_type)`
- `resurrect.state_manager.periodic_save.{start,finished}` (whole cycle — too coarse)
- `resurrect.error` on failure

**Plugin reload semantics** (verified in wezterm docs):
- `wezterm.on` handlers are **cleared and re-registered** on reload. Lua state is rebuilt from scratch.
- `wezterm.GLOBAL` survives reload. Accepts `string | number | table | boolean`.
- Pane / mux state survives reload (only Lua VM is rebuilt). Env vars on child processes are kernel-level, untouched.
- `window-config-reloaded` event fires when reload completes.
- **`user-var-changed` events arriving during the reload window are dropped, not queued.** Empirical / source-derived; not officially documented.

**`wezterm.time.call_after(0.05, ...)` is NOT canonical.** The `smart_workspace_switcher.wezterm` reference implementation uses event-driven sequencing: `smart_workspace_switcher.workspace_switcher.{chosen,created,selected}`. No `call_after` needed. The 0.05 delay was a workaround for SwitchToWorkspace lacking a completion callback — not a guaranteed lower bound on any system.

### Decisions

**→ TUI exit**: rely on default `exit_behavior = "Close"`. Document one assumption: users with `exit_behavior = "Hold"` set globally will see the lingering message; ship a config knob `wezsesh.spawn.force_close = true` that emits an explicit-close OSC sentinel before exit.

**→ Resurrect race mitigation** (layered):
1. **Lua-side gate**: subscribe to `resurrect.file_io.write_state.start` / `.finished`. Maintain `wezterm.GLOBAL.wezsesh_writing = { [path] = true }`. When binary requests "open TUI," plugin first stalls (small `call_after` retry, max 500ms) if any wezsesh-relevant snapshot is mid-write.
2. **Go-side defensive parsing**: treat any JSON parse error during snapshot read as transient; retry up to 3× with 25ms backoff; on continued failure log a warning and skip that snapshot (do not abort the TUI).

**→ Reload durability**:
1. **Always store request state in `wezterm.GLOBAL.wezsesh_requests`** keyed by `request_id` (ULID). Never use module-local Lua tables for in-flight ops.
2. **Heartbeat / resend protocol**: binary retransmits its OSC every 250ms until either an ack or 5s timeout. Plugin clears `wezterm.GLOBAL.wezsesh_requests[id]` on completion to bound memory.
3. **Optional polish**: in `window-config-reloaded`, scan `wezterm.GLOBAL.wezsesh_requests` for in-flight ids and proactively re-emit acks.

**→ Replace `call_after(0.05, ...)`** with event-driven sequencing:
- Use `smart_workspace_switcher.workspace_switcher.created/.selected` events when available.
- Otherwise, observe new workspace via `wezterm cli list` polling (10ms cadence, 1s ceiling).
- Keep `call_after` only as a last-resort fallback with **250ms** (5× safety margin), not 50ms.

---

## 6. Platform scope — Unix only

**Decision: Unix only, not planned to add other platforms.** wezsesh targets darwin-arm64, darwin-amd64, linux-amd64, linux-arm64. The architecture is Unix-shaped throughout: Unix sockets for IPC reply, `tty_name`-based foreground-process probe in `wezsesh find --deep`, XDG paths for state/logs/trust. Adding non-Unix platforms would require a parallel IPC + paths + `find` strategy and sustained dogfooding effort we don't intend to take on.

---

## Summary of changes for PRD_V1.md

### Architectural pivots (must rewrite)

- **§6.4 IPC mechanism**: replace polling with Unix-socket return channel; document env-wrapping for spawn (no `--env` on `wezterm cli spawn`); document bubbletea OSC routing rules.
- **§6.3 IPC payload schema**: add `v`, `ts`, `id` (ULID), `reply_sock`, `hmac` fields; define canonical JSON for HMAC.
- **NEW §6.14 Security model**: pane-ID binding + HMAC-SHA-256 + replay/freshness guard. Vendored pure-Lua SHA-256 + base64. Env vars at spawn (`WEZSESH_HMAC_KEY`, `WEZSESH_PANE_ID`, `WEZSESH_PROTO_VERSION`).
- **NEW §6.15 Reload durability**: `wezterm.GLOBAL.wezsesh_requests` + heartbeat/resend protocol.

### Updates

- **§3 Non-Goals**: add explicit Unix-only / no-Windows + multiplexer/SSH lines.
- **§6.6 Snapshot files**: encryption magic-byte sniff + degraded-mode rendering.
- **§6.7 Concurrent invocations**: subscribe to `resurrect.file_io.write_state` events; defensive parsing on JSON errors.
- **§6.10 Plugin layout**: add `plugin/wezsesh/sha2.lua`, `plugin/wezsesh/b64.lua`, `plugin/wezsesh/state.lua` (GLOBAL wrapper).
- **§7.1**: add `M.VERSION` constant pattern; document plugin-load version check; add `force_close` config knob.
- **§9 Risks**: replace stale entries (e.g., the `call_after(0.05)` line — replace with event-driven recommendation).
- **§8.2 Deferred**: add encryption-aware Lua-shim cache; add `pane:send_text` reverse-channel as reserved fallback.

### Status
Bump to "v1.2 (post-deep-research)".
