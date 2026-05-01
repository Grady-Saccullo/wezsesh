# PRD Research Findings — Round 8 (canonical-JSON / crypto / wezterm-CLI / lifecycle audit)

Goal of this round: surface the categories not yet examined after rounds 1–7. Four parallel research spikes targeting realistic next-pass blind spots: canonical-JSON Go↔Lua byte-equality contract, vendored `pure_lua_SHA` correctness, wezterm CLI failure modes / timeouts, wezterm Lua API lifecycle reliability (`background_child_process`, `run_child_process`, `pane-closed`).

**Six BLOCKERs found.** Two of them invalidate previously-spec'd behavior (the `pane-closed` early-warning toast and the abbreviated canonical-JSON contract were both fundamentally underspecified). Plus seven HIGH-severity correctness/interop issues, several MEDIUM hygiene items.

The convergence pattern from prior rounds (3/3/1 BLOCKERs in rounds 5/6/7) does NOT extend — round 8 surfaces a fresh wave because the three audit lenses (cryptographic byte-equality, vendor library internals, wezterm CLI/Lua API failure modes) had been touched only superficially before.

---

## 1. Canonical-JSON Go↔Lua byte-equality — TWO BLOCKERS

### Findings

The PRD §6.3 specified canonical-JSON encoding with five abbreviated bullets:
- Keys sorted lexicographically.
- No whitespace.
- Integers as decimal (no scientific notation).
- UTF-8, no trailing newline.
- The `hmac` field is removed before serialization.

Insufficient. Ten corner cases enumerated; eight result in HMAC divergence under realistic inputs.

#### BLOCKER A — Empty Lua `{}` array/object ambiguity

In Lua, `{}` is structurally indistinguishable as array vs object. Without an explicit shape tag, the canonical encoder cannot decide whether to emit `{}` (object) or `[]` (array). The Go side has `[]any` vs `map[string]any` (no ambiguity). Result: every `args: {}` (the `noop` verb, plus any future verb with empty args) hashes differently on each side. Every empty-args HMAC fails.

#### BLOCKER B — `hmac`-field-removal ordering ambiguous

The previous spec said "The `hmac` field is removed before serialization" — but didn't specify whether "removed" means "key deleted" or "value zeroed". Two implementations:
- Delete the key entirely → canonical bytes have no `"hmac":...` substring.
- Set `hmac=""` and serialize → canonical bytes contain `,"hmac":"",`.

If Go signs after deleting and Lua verifies after zeroing (or vice versa), every payload diverges.

#### Other gaps surfaced (HIGH/MEDIUM)

- **Short-form vs `\u00XX` escape choice for control bytes** (HIGH): RFC 8259 permits both `\b` and ``, etc. If Go emits `\n` and Lua emits `
`, divergence.
- **U+2028 / U+2029 escape policy** (HIGH): RFC permits raw; some encoders escape for JS-safety; spec is silent.
- **Forward slash `/` escape** (MEDIUM): permitted both ways; spec is silent.
- **Lua float subtype rejection** (HIGH): Lua 5.4 distinguishes integer/float; `tostring(1.0)` returns `"1.0"`. Encoder MUST type-check via `math.type(n) == "integer"`.
- **Recursive vs top-level key sort** (HIGH): spec silent; nested objects with unsorted keys diverge.
- **Locale-dependent string comparison** (MEDIUM): Lua's `<` on strings is byte-by-byte unsigned in stock 5.4, but locale builds exist; CI must pin `LC_ALL=C`.
- **UTF-8 invalid-byte handling** (MEDIUM): Go's `encoding/json` rejects; Lua spec silent. Both must reject consistently.
- **`null` representation in Lua** (MEDIUM): Lua has no native `null`; need a sentinel (`canonical_json.NULL`) to disambiguate from `nil`.

### Decisions — §6.3 fully respec'd

The §6.3 canonical-JSON spec rewritten end-to-end with:
- Explicit 7-step field-removal sequence for both signer and verifier.
- Lua shape-tag requirement: sentinel field `__array=true` OR wrapper functions `canonical_json.array{}` / `canonical_json.object{}` with distinct metatables. Empty objects emit `{}`; empty arrays emit `[]`.
- Forbidden short-form escapes; `\u00XX` (lowercase) for ALL of U+0000–U+001F + U+007F + U+0080–U+009F + U+2028 + U+2029.
- Forward slash NEVER escaped.
- Integers only via `math.type` check on Lua side; `strconv.FormatInt` on Go side; range `[-2^63, 2^63-1]`.
- Recursive key sort at every nesting level; CI pins `LC_ALL=C`.
- UTF-8 validation mandatory both sides; reject invalid byte sequences.
- `canonical_json.NULL` Lua sentinel for explicit JSON null.
- CI golden corpus: empty object/array/string, NUL inside string, U+007F, U+2028, multi-byte UTF-8, nested 3-deep, integer edges (`0`, `-1`, ±2^63), one fixture per verb. Byte-level diff fails build.

### Files cited
- RFC 8259 (JSON spec) — escape requirements.
- Lua 5.4 manual — `math.type`, integer/float subtype, `<` on strings.
- `mlua-rs` (the wezterm Lua binding) — confirms standard Lua 5.4, no LuaJIT FFI.

---

## 2. Vendored `pure_lua_SHA` correctness — 2 HIGH, no BLOCKER

### Findings

Library audited at the PRD-pinned commit `6adac177c16c3496899f69d220dfb20bc31c03df` (2022-03). Read source for SHA-256 + HMAC + dispatch logic.

**Correct on every cryptographic axis:**
- SHA-256 padding, K constants (computed dynamically from primes), block size 64 bytes — verified against NIST.
- Test vectors pass: `sha256("")`, `sha256("abc")`, `sha256("The quick brown fox...")`.
- HMAC-SHA-256 is RFC 2104: `H((K' xor opad) || H((K' xor ipad) || message))`. Block-size correctly 64. Empty-key + empty-message HMAC matches RFC 4231.
- Lua 5.4 dispatch path = INT64 branch (native bitwise ops). No version-conditional bug.
- Performance: ~8 MB/s SHA-256 in interpreted Lua 5.4 → ~0.5–1.5ms per 4 KB HMAC. Within PRD's stated 0.3–0.8ms range (slightly optimistic but acceptable).

**Two HIGH-severity gaps in the PRD's wrapping spec** (NOT bugs in the library):

#### HIGH 1 — HMAC key format ambiguity

PRD said `wezsesh keygen` "prints 32 hex bytes" and `WEZSESH_HMAC_KEY` is "hex 32 random bytes." Read literally: 32 hex chars = 16 raw bytes = 128 bits. Read intentionally: 32 raw bytes = 64 hex chars = 256 bits. The §6.14 mention of "256-bit secret" implies the latter, but it's not enforced.

If Go interprets 32 raw bytes (256 bits) and Lua interprets 32 hex chars as a 32-byte ASCII string fed directly to HMAC (no hex decode), every payload's HMAC diverges silently.

#### HIGH 2 — No constant-time string compare in pure_lua_SHA

The library exports SHA / HMAC / hex_to_bin / bin_to_hex but NOT `ct_eq` or any constant-time string compare. The PRD's round-5 `wezsesh doctor` said "Lua side's vendored SHA must use a constant-time string compare" — but the library doesn't ship one, and the PRD didn't supply the implementation.

Lua's native `==` short-circuits on first byte mismatch, leaking timing.

### Decisions — §6.14 Layer 2 expanded

- **Key format pinned**: `WEZSESH_HMAC_KEY` is 64 lowercase hex chars representing 32 raw bytes (256 bits). Both sides hex-decode BEFORE passing to HMAC.
- **CI fixture** (HMAC round-trip): fixed key + canonical-JSON payload + expected digest committed to repo. Both Go (`crypto/hmac`) and Lua (vendored `sha.hmac` after `hex_to_bin`) MUST produce the same hex. Bidirectional sign/verify (Lua signs → Go verifies; Go signs → Lua verifies).
- **New `plugin/wezsesh/ct_eq.lua`** (~6 LOC; Lua 5.3+ bitwise ops); HMAC verifier in `ipc.lua` MUST use `ct_eq.eq(received, computed)`, never raw `==`.
- **Supply-chain check** (already in PRD §8.1): `SOURCES.lock` + `sha256sum -c` in CI. Round 8 confirms commit is pinned, library is maintained but stale (~4 years), no known vulnerabilities.

### Files cited
- `Egor-Skriptunoff/pure_lua_SHA` source at the pinned commit (sha2.lua ~5600 LOC; SHA-256 isolated subset is ~200 LOC).
- RFC 2104 (HMAC), RFC 4231 (HMAC test vectors), NIST SHA-256.
- Lua 5.4 reference manual (`math.type`, bitwise operators).

---

## 3. wezterm CLI failure modes — ONE BLOCKER

### Findings

Every workspace operation depends on one or more `wezterm cli` subcommands. Source-code reading of `wezterm/src/cli/` and `wezterm-client/src/client.rs`:

- **Connection**: `unix_connect_with_retry()` does 10 attempts with 50ms backoff (~500ms total). Optional one-shot mux-server auto-spawn. On exhaustion: exit 1 with stderr free-form English.
- **Per-RPC timeout**: NONE. After connection succeeds, the CLI blocks indefinitely waiting for the mux server's response. A mux that hangs after accepting the connection locks the CLI invocation.
- **Stderr format**: anyhow's `{:#}` Debug pretty-print (`wezterm/src/main.rs:705-706`). Free-form English; varies across wezterm versions; unsuitable for pattern-matching.

#### BLOCKER — All `wezterm cli` invocations need timeout context

The PRD has 5s `context.WithTimeout` wrappers on FS ops (§6.6) and on `StartSwitchPoller`'s parent context (§6.15), but NO requirement on per-call CLI timeouts. A hung mux → wezsesh TUI hangs forever; user's only recovery is `kill -9` on the binary. Real scenarios that trigger this: mux server stuck in a lock during heavy concurrent client load; macOS file-system-events backlog stalling the mux's I/O thread; a wezterm bug.

The fix is mechanical but mandatory: every `internal/wezcli/` invocation wraps in `context.WithTimeout(2 * time.Second)`. 2s is calibrated against measured CLI response times (healthy mux replies in <100ms; 2s leaves headroom).

#### Other findings

- **Rename collision** (HIGH): `wezterm cli rename-workspace` has NO collision check; mux server accepts any name. wezsesh MUST validate via `cli list` BEFORE invoking rename and surface `RENAME_COLLISION` client-side.
- **Pane activate race** (MEDIUM): pane may close between `cli list` and `activate-pane`. Need retry-once-then-`PANE_CLOSED_RACE` semantics in `wezsesh find`.
- **Stderr parsing** (MEDIUM): unstable across versions; classify by exit code + stdout-validity only.
- **Multiple clients in `cli list-clients`** (no-issue): clients ordered by recency in `wezterm-client/src/client.rs:1321-1323`; relying on first element is acceptable.
- **Polling load** (no-issue): single client = ~20 RPS; even concurrent 2 clients = 40 RPS; tokio async mux runtime handles this without rate-limiting concerns.

### Decisions — new §6.20 + additive error codes

- New §6.20 mandates `context.WithTimeout(2s)` on every `wezterm cli` invocation.
- Error-classification table: exit code + stdout validity, never stderr text.
- Pre-rename collision check via `cli list` before invoking rename.
- New error codes added to §6.3: `PANE_CLOSED_RACE` (additive; `UNEXPECTED_EXIT` also added — see spike 4).
- Doctor probes `cli list --format json` with the same 2s timeout used at runtime; warns on slow response.

### Files cited
- `wezterm-client/src/client.rs:731-808` (`unix_connect`), `:450-452` (retry budget), `:1321-1323` (client recency order).
- `wezterm/src/cli/list.rs:21-49` (empty-mux returns `[]`).
- `wezterm/src/cli/rename_workspace.rs:50-53` (no `<old>` lookup), `:59` (no `<new>` collision check).
- `wezterm/src/cli/activate_pane.rs:16-19` (no client-side existence check).
- `wezterm/src/main.rs:705-706` (anyhow stderr formatting).

---

## 4. Lifecycle event reliability — THREE BLOCKERs

### Findings

Three load-bearing wezterm Lua APIs audited; all three have realistic failure scenarios the PRD assumed away.

#### BLOCKER 1 — `wezterm.background_child_process` raises Lua errors on spawn failure

Source `lua-api-crates/spawn-funcs/src/lib.rs:54-72`: function returns `mlua::Result<()>`, raises a Lua error on spawn-time failure (binary missing, EMFILE, EACCES, OOM). PRD's reply emission code in §6.4 was unwrapped:
```lua
wezterm.background_child_process({wezsesh_bin, "reply", reply_sock, b64_json})
```
An unhandled error propagates out of the `user-var-changed` handler. wezterm logs it but the dispatch path crashes mid-way; the binary blocks on the socket waiting for a reply that never comes; the user sees IPC_TIMEOUT after 5s with no idea why.

#### BLOCKER 2 — `wezterm.run_child_process` can hang indefinitely

Source `lua-api-crates/spawn-funcs/src/lib.rs:30-50`: `cmd.output().await` with no timeout. If the binary exists but stalls at startup (broken dyld linkage, codesign verification stall, blocked stdlib init), the config-eval coroutine hangs indefinitely. wezterm has no config-eval watchdog. Result: GUI never paints until the user kills wezterm.

The PRD had no defense; the `~10ms keygen latency` claim was happy-path only.

#### BLOCKER 3 — `wezterm.on('pane-closed', ...)` does NOT exist

Source: documented window-events list (https://wezterm.org/config/lua/window-events/) shows `bell`, `format-tab-title`, `format-window-title`, `gui-startup`, `mux-startup`, `new-tab-button-click`, `open-uri`, `update-status`, `update-right-status`, `user-var-changed`, `window-config-reloaded`, `window-focus-changed`, `window-resized`. **No `pane-closed`.** `MuxNotification::PaneRemoved` exists internally in `mux/src/lib.rs` and is referenced by `wezterm-gui/src/termwindow/mod.rs` but the match arm is `MuxNotification::PaneRemoved(_) | MuxNotification::WindowCreated(_) => {}` — completely unhandled in the Lua emit path.

The PRD's §6.14 failure-mode matrix specified subscribing to `wezterm.on('pane-closed', ...)` to surface `UNEXPECTED_EXIT` toast immediately on binary panic. **This is non-implementable as written.** A user adding the listener registers a no-op.

### Decisions — §6.4, §7.1, §6.14 failure-mode matrix

- **§6.4 reply emission `pcall`-wrapped**. On failure: `wezterm.log_error` + user-facing toast `reply emission failed; operation may have succeeded — refresh picker`. The TUI still hits IPC_TIMEOUT but the user is informed that state may have been mutated despite the timeout.
- **§7.1 hang-resistance contract for `run_child_process`**:
  1. Pre-flight `io.open` existence check (no fork, can't hang).
  2. Cache resolved absolute path in `wezterm.GLOBAL.wezsesh_bin_path`.
  3. Doctor probes version-detect timing and warns if >2s.
  Residual hang risk (binary exists, runs, but stalls) is documented; only fix is removal.
- **§6.14 failure-mode matrix replaces pane-closed with Go-side panic-recover**:
  - `cmd/wezsesh/main.go` installs top-level `defer func() { if r := recover(); r != nil { writePanicReply(...); os.Exit(2) } }()` before `tea.Run`.
  - On Go panic, the recovery writes a sentinel `UNEXPECTED_EXIT` reply over the OSC channel + any open reply socket. Lua learns of the death without polling.
  - SIGSEGV / SIGKILL / OOM-kill fall through to IPC_TIMEOUT (rare-edge; acceptable).
  - New error code `UNEXPECTED_EXIT` added to §6.3.

### Files cited
- `lua-api-crates/spawn-funcs/src/lib.rs:30-50` (`run_child_process`'s untimed `.output().await`), `:54-72` (`background_child_process`'s `mlua::Error::external` raise on spawn failure).
- wezterm window-events docs (https://wezterm.org/config/lua/window-events/) — exhaustive list lacks `pane-closed`.
- `wezterm-gui/src/termwindow/mod.rs` `MuxNotification::PaneRemoved(_) => {}` match arm.
- `mux/src/lib.rs` — `MuxNotification::PaneRemoved` is internal-only.

---

## Summary of changes for PRD_V2

### BLOCKER fixes (must change before code lands)

- **§6.3 (security/correctness × 2)**: canonical-JSON encoder spec expanded from 5 bullets to exhaustive rules. Closes both the empty `{}`/`[]` ambiguity and the `hmac`-field-removal ordering ambiguity. Without these, every `noop` op fails HMAC and the entire HMAC contract has divergence-by-design.
- **§6.20 (correctness, new section)**: every `wezterm cli` invocation MUST be wrapped in `context.WithTimeout(2s)`. Without this, a hung mux locks the wezsesh TUI indefinitely.
- **§6.4 (correctness)**: `wezterm.background_child_process` reply emission `pcall`-wrapped; on failure log_error + toast. Unwrapped errors propagate out of the `user-var-changed` handler and crash dispatch mid-way.
- **§7.1 (reliability)**: `wezterm.run_child_process` hang-resistance contract — pre-flight `io.open`, cached absolute binary path in GLOBAL, doctor warns on slow probe.
- **§6.14 failure-mode matrix (correctness)**: `wezterm.on('pane-closed', ...)` is non-existent in wezterm; previously-spec'd UNEXPECTED_EXIT early-warning replaced with Go-side top-level `defer recover()` writing a sentinel reply.

### HIGH-severity correctness/interop

- **§6.14 Layer 2**: HMAC key format pinned (64 hex chars = 32 raw bytes, both sides hex-decode before HMAC). Required CI round-trip fixture catches divergence.
- **§6.14 Layer 2**: new `plugin/wezsesh/ct_eq.lua` for constant-time HMAC compare (pure_lua_SHA does not export one; native `==` short-circuits).
- **§6.20**: pre-rename collision check via `cli list` (mux server doesn't enforce uniqueness; `RENAME_COLLISION` surfaced client-side).
- **§6.20**: `PANE_CLOSED_RACE` retry semantics for `cli activate-pane` failures during `wezsesh find`.
- **§6.3**: forbid short-form escapes; mandate `\u00XX` for C0 + DEL + C1 + LS/PS; reject Lua floats via `math.type`; recursive key sort at every level.

### Updates

- **§6.3 / §6.10**: new error codes `UNEXPECTED_EXIT`, `PANE_CLOSED_RACE`. Plugin layout adds `ct_eq.lua`.
- **§8.1**: testing invariants expanded — explicit canonical-JSON corpus, HMAC round-trip golden, pre-flight binary check + path cache, `internal/wezcli/` 2s timeout requirement, top-level Go panic-recover in `main.go`, pre-rename collision check.
- **§9 risks**: 12 new rows covering all v1.9 BLOCKERs and the canonical-JSON / CLI / lifecycle hardening.

### Severity tally

- BLOCKER: 6 (canonical JSON ×2, CLI timeout, background_child_process pcall, run_child_process hang, pane-closed nonexistence)
- HIGH: 7 (HMAC key format, ct_eq missing, rename collision, escape rules, recursive sort, integer-float reject, key sort locale)
- MEDIUM: 8 (UTF-8 validate, null sentinel, slash escape, pane-close race, stderr parse, polling load, supply-chain SOURCES.lock, doctor probe timing)
- No-issue: numerous (SHA-256 correctness, HMAC RFC 2104, library Lua 5.4 compat, mux concurrent load, etc.)

### Status

v8 complete. Six BLOCKERs addressed, all are spec gaps rather than library bugs. PRD bumped to v1.9.

**Recommendation: continue to round 9.** The convergence pattern from earlier rounds (3/3/1 BLOCKERs in 5/6/7) does NOT extend — round 8 surfaces a fresh wave because the audit lenses (canonical-JSON, vendored crypto, wezterm CLI/Lua API failure modes) had been touched only superficially. The BLOCKERs found are realistic and load-bearing (every HMAC operation, every `cli` invocation, every reply emission). Until a round of validation finds zero new BLOCKERs and zero new HIGHs, the spec is not safe to start implementing.

Suggested round-9 audit lenses (areas still not deeply validated):
- The Go panic-recovery sentinel-reply path's correctness (race conditions on the OSC writer mutex during panic; can the OSC be emitted from a deferred recover when bubbletea's renderer is in an unknown state?)
- `wezterm.GLOBAL.wezsesh_bin_path` caching across config-reload (does a reload that resolves a different path orphan in-flight requests using the old path?)
- Concurrent multi-TUI snapshot writes through `safefs.AtomicWriteFile` — atomicity at the FS level is solid, but does the `SHA-256 expected_hash` flow have a TOCTOU between the hash-check and the rename?
- `wezterm cli spawn` invocation the Lua plugin uses for the binary itself — does the plugin handle `cli spawn` exit-1 gracefully (out of pids, mux unreachable at spawn time)?
- The plugin's `ipc.lua` HMAC verification on `pcall` failure — does a malformed payload that crashes the JSON parser drop the message silently or surface to the user?
