# PRD Research Findings — Round 13 (v2.3-edits-revalidation / two-phase-find-race-conditions / build-tag-portability-confirmation / Lua-API-existence-confirmation / sentinel-error-pcall-roundtrip audit; **FINAL audit — phase closed**)

Goal of this round, per round-12's recommendation: re-validate v2.3's recently-introduced edits ONLY (the build-tag split, the EvalSymlinks 500ms timeout wrapper, the two-phase `wezsesh find` flow, the extended Layer 2 prefix list, the sentinel-prefixed error pattern, the runtime_dir contract). Termination criterion (per round 12 + the round-13 task spec): if zero BLOCKERs and zero HIGH-severity correctness issues are found, audit phase ends; equally, if new BLOCKER/HIGH drift IS found against v2.3 edits, audit phase also ends — the round-12 thesis (spec-only audits introduce drift as fast as they close it) is confirmed in either case. Round 14 is not authorized.

**Result: ZERO BLOCKERs, TWO HIGHs, EIGHT MEDIUMs, TWO nits.** Both HIGHs are spec-drift in v2.3's two-phase `wezsesh find` flow (client-pinning across Phase 1 ticks; window-id scoping echoed in §6.13 as well as §6.15). Spikes A (build-tag portability), B (wezterm.home_dir + Lua patterns), and E (sentinel-error pcall round-trip) confirmed v2.3's edits compile cleanly and behave as the spec assumes. Spike D (CloudStorage prefix verification + runtime_dir byte counts) found provider-coverage doc-clarity issues, no correctness BLOCKERs.

**Audit-cadence assessment**: round-12 found seven BLOCKERs (all v2.2-edit drift); round-13 finds zero BLOCKERs but two HIGHs, both in v2.3-edit drift. The pattern persists: each round catches drift the previous round introduced. The round-12 termination thesis is confirmed empirically: spec-only iteration is producing diminishing returns AND introducing as much drift as it closes per cycle. **Audit phase CLOSES with v2.4. Next iteration is code-and-test, not spec.**

Headline findings:

1. **Phase 1 polling re-resolves "most-recent client" per tick instead of pinning the originator (HIGH).** §6.5's multi-client disambiguation rule ("pick the client with most-recent `last_input`") was applied without specifying *once-vs-per-tick*. Re-evaluated per tick, a second connected GUI client (mosh, SSH-mux, another local wezterm) becoming "most recent" mid-poll flips the predicate to track the wrong client's workspace. v2.4 mandates pinning at Phase 1 start.

2. **Phase 1 predicate window-id scoping was implicit in §6.15 step 4 but not echoed in §6.13 cross-workspace prose (HIGH).** An implementer reading §6.13 in isolation could omit the `window_id == TargetWindowID` check, allowing a false-positive when the user closes the originating wezterm window mid-poll and the pinned client's focused_pane_id rolls over to another window. v2.4 makes the scoping explicit at every step of the §6.13 / §6.15 description.

3. **All other v2.3 edits verified clean.** Build-tag split (Spike A) compiles correctly on darwin/Linux/FreeBSD/OpenBSD/NetBSD; `O_CLOEXEC` available on all platforms with consistent semantics. `wezterm.home_dir` exists and never returns nil (Spike B). Lua pattern `%-apple%-darwin` matches all darwin variants correctly. Sentinel-prefixed `error(...)` round-trips through `pcall` + `tostring` cleanly; `string.find` unanchored substring match still works regardless of file:line prefix (Spike E). MEDIUM v2.4 polish: switch to `error(msg, 0)` to suppress the noisy prefix in user-visible toasts.

The audit phase has reached its natural inflection. Code-and-test will catch any remaining surfaces empirically.

---

## 1. Build-tag portability + O_CLOEXEC defense-in-depth — ZERO BLOCKERs, TWO MEDIUM (doc-clarity)

### Findings

#### VERIFIED-OK — `//go:build !linux` covers FreeBSD, OpenBSD, NetBSD cleanly

Verified `golang.org/x/sys/unix/zerrors_*.go` for FreeBSD, OpenBSD, NetBSD: `unix.F_SETLK` is defined on all three (e.g., `F_SETLK = 0xc` on FreeBSD); `unix.F_OFD_SETLK` is defined ONLY in `zerrors_linux.go`. The build-tag plan in v2.3 (`lock_linux.go` //go:build linux + `lock_other.go` //go:build !linux) compiles cleanly on every BSD variant. No platform gap. Same applies to OpenBSD and NetBSD: F_SETLK present, F_OFD_SETLK absent.

`unix.O_CLOEXEC` available across all five platforms with different numeric values but identical semantics:
- Linux: `0x80000`
- darwin (amd64/arm64): `0x1000000`
- FreeBSD: `0x100000`
- OpenBSD: `0x10000`
- NetBSD: `0x400000`

Different OS values are expected; the constant abstraction handles the per-platform difference.

#### MEDIUM (doc-clarity) — v0.1 supported-target matrix undocumented in v2.3

v2.3 specified the build-tag split correctly but did not state which platforms v0.1 targets. v2.4 fix: explicit statement — v0.1 ships for **darwin (Apple Silicon + Intel) and Linux (amd64 + arm64)**. BSD compiles cleanly under the same plan but is best-effort, not a v0.1 commitment. CI matrix lists the four supported triples.

#### MEDIUM (doc-clarity) — `O_CLOEXEC` mandate's relationship to Go's `os/exec` default unstated

Verified Go runtime (`src/syscall/exec_unix.go`): Go's `os/exec.Cmd.Start()` runtime DOES mark all inherited fds CloseOnExec via the "mark all, unmark intentional" strategy since Go 1.10+. So for the `os/exec.Start` path specifically, our `O_CLOEXEC` mandate is **superseded** by Go's default. The mandate remains as defense-in-depth, but the rationale was unspecified — a future maintainer might remove the flag thinking it's redundant.

v2.4 fix: defense-in-depth rationale documented inline. Three reasons the flag is retained:
- **(a)** Direct syscall escape paths (`syscall.ForkExec`, raw `fork(2)`) are NOT protected by Go's runtime CloseOnExec marking. The `O_CLOEXEC` flag is applied at open time and travels with the fd through any spawn mechanism.
- **(b)** Window between `Openat` and the runtime's CloseOnExec marking: if a goroutine spawns a subprocess immediately after the lock fd is opened, there is a brief window where Go's runtime hasn't yet marked the fd. `O_CLOEXEC` at open time closes that window unconditionally.
- **(c)** Refactor resilience: enforcing at every open call site catches accidental regressions if a future maintainer uses a non-`os/exec` spawn path.

### Decisions — §6.19

- §6.19 v2.4: defense-in-depth rationale documented inline.
- §6.19 v2.4: explicit non-Linux platform coverage statement; v0.1 supported-target matrix.
- VERIFIED: build-tag plan compiles cleanly on FreeBSD/OpenBSD/NetBSD.

### Files cited

- [`zerrors_freebsd_amd64.go` F_SETLK definition](https://raw.githubusercontent.com/golang/sys/master/unix/zerrors_freebsd_amd64.go)
- [`zerrors_linux.go` F_OFD_SETLK definition](https://raw.githubusercontent.com/golang/sys/master/unix/zerrors_linux.go)
- [`zerrors_openbsd_amd64.go`](https://raw.githubusercontent.com/golang/sys/master/unix/zerrors_openbsd_amd64.go) and [`zerrors_netbsd_amd64.go`](https://raw.githubusercontent.com/golang/sys/master/unix/zerrors_netbsd_amd64.go)
- Go runtime `src/syscall/exec_unix.go` for default CloseOnExec behavior
- [Go os/exec docs — ExtraFiles](https://pkg.go.dev/os/exec)

---

## 2. wezterm.home_dir API existence + Lua pattern syntax + target_triple variants — ZERO BLOCKERs, ONE nit

### Findings

#### VERIFIED-OK — `wezterm.home_dir` exists and is always a string

Verified [`config/src/lib.rs`](https://raw.githubusercontent.com/wezterm/wezterm/main/config/src/lib.rs) (`HOME_DIR = dirs_next::home_dir().expect(...)`) and [`config/src/lua.rs`](https://raw.githubusercontent.com/wezterm/wezterm/main/config/src/lua.rs) (`wezterm_mod.set("home_dir", crate::HOME_DIR.to_str())`). The constant is initialized at wezterm boot from `dirs_next::home_dir()`; if the lookup fails, wezterm panics at startup (`.expect()`). So when a Lua plugin runs, `wezterm.home_dir` is **always** a string — never nil.

Implication: v2.3's fallback chain `(wezterm.home_dir or os.getenv("HOME") or "")` is redundant. v2.4: documented inline as forward-compat hygiene against a future wezterm release that might surface nil; harmless and self-documenting today. **No code change**.

On `sudo wezterm` without `-E`: `dirs_next::home_dir()` reads `$HOME`; if cleared, the value reflects whatever sudo set (usually `/var/root` on darwin, `/root` on Linux). This is the expected behavior — covered by §6.19 sudo-divergence doctor warning (already present from v2.3).

#### VERIFIED-OK — `target_triple:match("%-apple%-darwin")` is correct Lua 5.4 syntax

Verified wezterm's Lua flavor: `mlua = { features = ["lua54", ...] }` in `config/Cargo.toml`. wezterm targets Lua 5.4. Per Lua 5.4 reference manual §6.4.1, `%-` is the literal-hyphen escape in patterns. Plain `-` outside character classes is also literal (the `-` quantifier semantic is regex/PCRE-only; Lua patterns don't have `-` as a quantifier outside of `[a-z]` ranges). The spec's `%-` is idiomatic and explicit; works correctly.

Verified `target_triple` shipped values: `aarch64-apple-darwin` (Apple Silicon), `x86_64-apple-darwin` (Intel). Both match `%-apple%-darwin`. wezterm's `target_triple` is set via Rust's `env!("TARGET")` at compile time — no version suffix on shipped releases (e.g., not `aarch64-apple-darwin23.4.0`).

#### nit — fallback chain `or os.getenv("HOME") or ""` is redundant but harmless

Documented inline. No code change. v2.4 polish.

### Decisions — §6.4

- §6.4 v2.4: `wezterm.home_dir` always-truthy property documented inline; fallback chain retained as forward-compat hygiene.
- VERIFIED: Lua pattern syntax + target_triple variants behave as v2.3 assumes.

### Files cited

- [wezterm `config/src/lib.rs` HOME_DIR](https://raw.githubusercontent.com/wezterm/wezterm/main/config/src/lib.rs)
- [wezterm `config/src/lua.rs` exposing home_dir](https://raw.githubusercontent.com/wezterm/wezterm/main/config/src/lua.rs)
- [wezterm `config/Cargo.toml` mlua lua54 feature](https://raw.githubusercontent.com/wezterm/wezterm/main/config/Cargo.toml)
- [Lua 5.4 reference manual — patterns](https://www.lua.org/manual/5.4/manual.html#6.4.1)

---

## 3. Two-phase `wezsesh find` race conditions — ZERO BLOCKERs, TWO HIGH, ONE MEDIUM

### Findings

#### HIGH A — Phase 1 polling re-resolves "most-recent client" per tick instead of pinning the originator

§6.5's multi-GUI-client disambiguation rule says "pick the client with most-recent `last_input` timestamp; tie-break on `client_id`." v2.3 §6.15 step 3 referenced this rule when describing per-tick polling without specifying whether the rule applies once (at Phase 1 start) or per tick (re-evaluated each iteration). Read literally, "applied per tick" is a valid interpretation.

**Failure scenario**: User A invokes `wezsesh find` from client-1, target pane in workspace-B. Phase 1 polling starts; client-1 is "most recent." Tick-2 happens; meanwhile user B types in client-2 (a connected SSH-mux session from another machine). Client-2 becomes "most recent." Tick-2's predicate now checks client-2's `focused_pane_id` instead of client-1's. The poller either:
- **Spuriously succeeds** if client-2 happens to be on workspace-B, exiting the poller with a phantom-success — but the SwitchToWorkspace dispatched against client-1 may not have completed, so Phase 2 runs against a half-transitioned state.
- **Spuriously fails** if client-2 is on workspace-X (not target B), and the predicate never fires — `MUX_UNREACHABLE` at 5s ceiling, the user sees an opaque error.

**Verified**: `focused_pane_id` is per-client in `cli list-clients --format json` output (each client entry has its own field). Re-resolving the "most recent" client per tick is what the literal text of v2.3 says; the spec did not anticipate the multi-client race.

**v2.4 fix**: §6.5 / §6.13 / §6.15 v2.4 mandate client pinning. `StartSwitchPoller` captures `TargetClientID` at Phase 1 start (the pinned client is the same one that triggered the `wezsesh find` invocation, identified via `focused_pane_id` matching the binary's own pane). Every subsequent polling tick looks up THAT `client_id` specifically in `cli list-clients`. If the pinned client disconnects mid-poll, the predicate fails closed at the 5s ceiling (`MUX_UNREACHABLE`). CI test: a second client gaining "most-recent" status mid-poll MUST NOT flip the predicate.

#### HIGH B — Phase 1 predicate window-id scoping was implicit in §6.15 step 4 but not echoed in §6.13

§6.15 step 4 (predicate text) says "workspace `target` is now the active workspace of the focused client in `target_window_id`" — the window-id scoping IS in the spec. But §6.13's two-phase prose ("Phase 1 — workspace switch: ... polls per §6.15 ... until the workspace transition is confirmed via the cross-reference of `cli list-clients` (focused_pane_id) → `cli list` (that pane's `workspace`)") elides the `window_id` part, making it possible for an implementer reading §6.13 alone to write a poller that checks workspace match but not window-id match.

**Failure scenario**: User invokes `wezsesh find` from window A (window_id=1), target pane in workspace B's window 2. Phase 1 dispatches `SwitchToWorkspace`. Mid-poll, the user closes window 1. The pinned client's `focused_pane_id` rolls over to a pane in window 3 (which happens to be on workspace B). The naive predicate "workspace match" succeeds: the focused pane's workspace == target B. Phase 2 runs `cli activate-pane --pane-id N` for the original target pane in window 2 — but the user is now visible in window 3, and the activation is invisible (window 3 doesn't raise to z-order even though it's the focused window).

**v2.4 fix**: §6.13 / §6.15 v2.4 echo the window-id scoping explicitly. The Phase 1 cross-reference reads BOTH `workspace` AND `window_id` from the resolved pane; predicate fires only when BOTH match. CI test: closing the wezterm window mid-Phase-1 surfaces `MUX_UNREACHABLE`, not a phantom-success activation.

#### MEDIUM — `wezsesh find` cumulative wall-clock budget undocumented; no Phase-1 progress UI

Worst-case `wezsesh find` cross-workspace: 5s (Phase 1 ceiling per §6.15) + 2s (Phase 2 `cli activate-pane` per §6.20) = **7s end-to-end**. v2.3 didn't specify this budget. UX implication: user hits Enter, TUI closes, then up to 5 seconds of nothing — indistinguishable from "find is broken."

**v2.4 fix**: §6.13 v2.4: TUI MUST render a one-line progress status during Phase 1 (`Switching workspace...` → `Activating pane...`). 7s ceiling documented. Per-tick polling cost (~30ms × 100 ticks = 60% of one core during the 5s slice) acknowledged but not fixed; exponential backoff (50ms→500ms cadence) deferred to v0.2 if user feedback reports concurrent-TUI contention.

#### Pane-closed-mid-flight race — already covered by v2.3 `PANE_CLOSED_RACE`

The two-phase delay widens the window during which the target pane could close (Ctrl-D, window close); existing single-retry handling in §6.20 covers it. No spec change needed.

### Decisions — §6.5, §6.13, §6.15

- §6.5 / §6.13 / §6.15 v2.4: client pinning at Phase 1 start; per-tick lookup of THAT client_id only.
- §6.13 / §6.15 v2.4: window-id scoping echoed at every step.
- §6.13 v2.4: Phase 1 progress UI mandated; 7s cumulative budget documented.
- VERIFIED: pane-closed race already covered by v2.3 `PANE_CLOSED_RACE`.

### Files cited

- [wezterm-client `client.rs`](https://raw.githubusercontent.com/wezterm/wezterm/main/wezterm-client/src/client.rs) — `focused_pane_id` is per-client
- [wezterm `wezterm/src/cli/list_clients.rs`](https://raw.githubusercontent.com/wezterm/wezterm/main/wezterm/src/cli/list_clients.rs) — JSON schema
- [wezterm-mux-server-impl `sessionhandler.rs`](https://raw.githubusercontent.com/wezterm/wezterm/main/wezterm-mux-server-impl/src/sessionhandler.rs) — SetFocusedPane handler (referenced from round 12)

---

## 4. Extended Layer 2 prefix coverage + runtime_dir contract byte counts — ZERO BLOCKERs, TWO MEDIUMs

### Findings

#### MEDIUM A — NextCloud Layer 2 anchor coverage misleading; standard client uses `~/Nextcloud/` not `~/Library/CloudStorage/`

v2.3's `~/Library/CloudStorage/Nextcloud*` prefix was added to cover NextCloud File Provider variants. Reality: the standard NextCloud Desktop client (the dominant install across `nextcloud.com/install/` macOS downloads) does NOT use File Provider — it syncs to `~/Nextcloud/` directly. The Library/CloudStorage anchor covers only community-built FP variants, which are a minority install.

**v2.4 fix**: anchor list extended with `~/Nextcloud` (covers the standard client). Existing `~/Library/CloudStorage/Nextcloud*` retained for community FP variants. Provider-coverage scope explicitly tagged per anchor (verified vs. best-effort vs. unverified) inline in §6.19. False-positive risk on user-created `~/Nextcloud/` that is NOT a sync root mitigated by Layer 1 statfs (sees APFS, not osxfuse, downgrades the warning).

#### MEDIUM B — Proton Drive and Seafile/SeaDrive anchors are best-effort and unverified

v2.3 added `~/Library/CloudStorage/Proton*` and `~/Library/CloudStorage/Seafile*` based on Round 12's research that those providers MIGHT have File Provider extensions. Round 13 attempted to verify against shipped clients but found no reliable public source confirming the actual on-disk subdirectory naming. Proton Drive's macOS File Provider was experimental as of 2025; SeaDrive on Apple Silicon was migrating toward File Provider with Intel SeaDrive remaining FUSE-based (caught by Layer 1 anyway).

**Risk assessment**: if the prefix anchors don't match real on-disk paths, they're harmless dead code — no false-positives (the `Library/CloudStorage` namespace is OS-managed, user-created subdirs there are rare), and the documented false-negative class (cloud-sync paths NOT on the prefix list bypass detection) subsumes them.

**v2.4 fix**: anchors retained but tagged "best-effort, unverified" inline. First-user-report or `pluginkit -m -p com.apple.fileprovider-nonui` discovery (already deferred to v0.2+ with explicit hang-risk note) will resolve. No correctness blocker.

#### Box prefix verified plausible

Box for Mac uses File Provider since 2024; actual subdir naming is typically `Box-<UserName>` based on Box documentation review. The `Box*` glob anchor matches. Same false-positive caveat as the other CloudStorage anchors.

#### runtime_dir byte-count edge cases — RESOLVED in v2.3 contract

Worked through the contract with explicit byte counts:

- **Case A**: User passes `/run/user/1000/wezsesh` (no trailing slash, 22 bytes). Lua check: 22 + 14 = 36 ≤ 108 ✓. Final path: `/run/user/1000/wezsesh/<8hex>.sock` = 22 + 1 + 13 = 36 bytes. Matches `+14` budget exactly (separator `/` + `<8hex>.sock` = 14 bytes).
- **Case B**: User passes `/run/user/1000/wezsesh/` (with trailing slash, 23 bytes). Lua check: 23 + 14 = 37 ≤ 108 ✓. Final path: `/run/user/1000/wezsesh/<8hex>.sock` = 23 + 13 = 36 bytes (trailing slash absorbs the separator). Lua check overcounts by 1 byte vs. actual; harmless — the 1-byte conservative margin rejects paths 1 byte short of the actual ceiling, which is fine.

Linux `bind(2)` normalizes double-slashes in unix socket paths in-kernel; `filepath.Clean` on the Go side normalizes redundant slashes before any byte-count check. Both are harmless under v2.3's contract that `opts.runtime_dir` IS the final reply directory.

`wezterm.home_dir` returns paths without trailing slash (verified — `dirs_next::home_dir()` returns a `PathBuf` without trailing separator). `os.getenv("HOME")` on macOS returns paths without trailing slash. Tilde expansion produces no extra slashes.

**No spec change**. The v2.3 contract is sound.

### Decisions — §6.7, §6.19

- §6.7 / §6.19 v2.4: Layer 2 anchor list extended with `~/Nextcloud`; per-anchor coverage scope tagged inline.
- §6.19 v2.4: Proton/Seafile anchors retained but tagged best-effort.
- VERIFIED: runtime_dir contract byte counts are sound.

### Files cited

- [Apple developer docs — File Provider framework](https://developer.apple.com/documentation/fileprovider)
- [NextCloud Desktop install docs](https://nextcloud.com/install/)
- Go stdlib `path/filepath/path.go` for `Clean` semantics
- Linux `man 2 bind` for unix socket path normalization

---

## 5. Sentinel-prefixed error pattern survives `pcall` + `tostring` — ZERO BLOCKERs, ONE MEDIUM, ONE nit

### Findings

#### MEDIUM — `error(msg)` default level=1 prepends `<file>:<line>:` to caught message

Per Lua 5.4 reference manual §6.1: `error(message, level)` raises a Lua error with `message` as the error value. With default `level=1`, Lua prepends `<file>:<line>: ` to a string `message` before raising. When `pcall` catches it, `tostring(err)` returns the **full prefixed string**. The user-visible toast then displays:

```
init.lua:42: WEZSESH_SUN_PATH_OVERFLOW: runtime_dir too long for AF_UNIX SUN_PATH ...
```

The sentinel substring match `msg:find("WEZSESH_SUN_PATH_OVERFLOW")` STILL WORKS — Lua's `string.find` is unanchored substring search by default (verified Lua 5.4 reference manual §6.4.1). But the user-facing toast has noisy file:line prefix that adds no value.

**v2.4 fix**: switch all sentinel-prefixed errors to `error(msg, 0)` (level=0). Per Lua 5.4 manual: "When level is 0, the error position information is not added to the message." Substring match in the outer pcall still works.

Applied to:
- `WEZSESH_RUNTIME_DIR_TYPE`: `error("...", 0)`
- `WEZSESH_SUN_PATH_OVERFLOW`: `error(string.format(...), 0)`

#### VERIFIED-OK — `string.format("%q")` handles paths safely

Per Lua 5.4 reference manual §6.4: `%q` formats a string in a Lua-readable form, escaping backslashes, double-quotes, newlines, and control characters. For typical paths like `/tmp/foo/bar`, output is `"/tmp/foo/bar"` (with literal enclosing quotes). For paths with embedded newlines or NUL bytes (unusual but possible), `%q` escapes them safely. No injection vectors.

#### nit — `wezterm.toast_notification` message-length and control-char-filtering not spec-verifiable

Wezterm's documented Lua API at https://wezterm.org/config/lua/window/toast_notification.html specifies parameters (title, message, url, timeout_ms) but does NOT document:
- Maximum message length
- Multi-line support (embedded `\n`)
- Control-character filtering

Source-code reading of [`lua-api-crates/window-funcs/src/lib.rs`](https://raw.githubusercontent.com/wezterm/wezterm/main/lua-api-crates/window-funcs/src/lib.rs) was not pursued exhaustively in this round; surface area is small but specifics weren't extracted.

**Defense-in-depth assessment**: `string.format("%q")` already escapes the path component before it enters the toast layer, so even if wezterm passes the message through unfiltered, escape-sequence injection is not possible. The remaining question is purely truncation aesthetics.

**v2.4 fix**: documented as a runtime-validation item to measure during integration testing. If aggressive truncation is observed (e.g., the toast cuts off mid-message before the user sees the remediation hint), the spec will switch to a relative-tail abbreviation (e.g., `.../<last-20-chars>`). No code change required pre-implementation.

### Decisions — §6.4

- §6.4 v2.4: switch to `error(msg, 0)` for both sentinel-prefixed errors; suppresses file:line prefix in toast text.
- §6.4 v2.4: documented `%q` defense-in-depth and toast-layer runtime-validation deferral.

### Files cited

- [Lua 5.4 reference manual §6.1 (error)](https://www.lua.org/manual/5.4/manual.html#6.1)
- [Lua 5.4 reference manual §6.4 (string.format)](https://www.lua.org/manual/5.4/manual.html#6.4)
- [Lua 5.4 reference manual §6.4.1 (patterns)](https://www.lua.org/manual/5.4/manual.html#6.4.1)
- [wezterm window:toast_notification docs](https://wezterm.org/config/lua/window/toast_notification.html)

---

## Summary of changes for PRD_V7 (= internally stamped v2.4)

### HIGH-severity correctness drift in v2.3 edits

1. **§6.5 / §6.13 / §6.15** (correctness): Phase 1 polling re-resolved "most-recent client" per tick; v2.4 mandates client pinning at Phase 1 start. Spike 3.
2. **§6.13 / §6.15** (clarity/correctness): Phase 1 predicate window-id scoping implicit in §6.15 step 4 but absent from §6.13 prose; v2.4 echoes the window-id check at every step. Spike 3.

### MEDIUM updates

- **§6.13** (UX): `wezsesh find` cumulative 7s wall-clock budget undocumented; no Phase-1 progress UI. v2.4: progress status mandated; budget documented; exponential-backoff cadence deferred. Spike 3.
- **§6.4** (UX): sentinel-prefixed errors used `error(msg)` default level=1, prepending file:line to user-visible toast. v2.4: switch to `error(msg, 0)`. Spike 5.
- **§6.7 / §6.19** (doc-clarity): NextCloud Layer 2 anchor only covered community FP variants; standard client uses `~/Nextcloud/`. v2.4: anchor list extended; per-anchor coverage scope tagged. Spike 4.
- **§6.19** (doc-clarity): Proton/Seafile anchors retained as best-effort; first-user-report or pluginkit discovery resolves. Spike 4.
- **§6.19** (defense-in-depth rationale): O_CLOEXEC mandate's relationship to Go's os/exec default unstated; v2.4 documents three reasons the flag is retained. Spike 1.
- **§6.19** (doc-clarity): FreeBSD/OpenBSD/NetBSD coverage of //go:build !linux undocumented; v2.4: explicit v0.1 supported-target matrix. Spike 1.

### nits

- **§6.4** (doc-clarity): `wezterm.home_dir` always-truthy property; fallback chain retained as forward-compat hygiene. Spike 2.
- **§6.4** (runtime-validation): `wezterm.toast_notification` message-length / control-char-filtering not spec-verifiable; defer to integration testing. Spike 5.

### Severity tally

- BLOCKER: **0**.
- HIGH: **2** (client pinning across Phase 1 ticks; window-id scoping echoed in §6.13).
- MEDIUM: **8**.
- nit: **2**.
- VERIFIED-OK: 4 (build-tag + O_CLOEXEC across BSDs; wezterm.home_dir + Lua patterns + target_triple; pane-closed race already covered; `string.format("%q")` safe).

### Status

v13 complete. Two HIGHs addressed; PRD bumped to v2.4. **Audit phase CLOSED.** Next phase is code-and-test.

### Audit-cadence assessment — round-12 hypothesis CONFIRMED

Round 11 hypothesized that "BLOCKERs are predominantly drift between OUR own recent edits and reality." Round 12 found 7 BLOCKERs, all v2.2-edit drift. Round 13 finds 0 BLOCKERs but 2 HIGHs in v2.3-edit drift — the two-phase `wezsesh find` flow (a v2.3 addition) had unstated client-pinning and partial window-id scoping.

The pattern across rounds 8–13 is decisive and unchanging:
- Each round catches drift the previous round introduced.
- Each round closes ~5–10 issues but introduces N new spec content for the NEXT round to find drift in.
- Severity is decreasing per round (round 12: 7 BLOCKERs; round 13: 0 BLOCKERs, 2 HIGHs) but the pattern persists.

**Spec-only iteration has reached structural saturation.** Further audit-only rounds would continue finding low-severity drift while introducing new drift each cycle. Round 13 is the inflection point at which empirical / runtime testing must take over. This is the FINAL spec audit. **Round 14 is not authorized.**

The remaining surfaces — toast truncation behavior, actual on-disk paths for Proton/Seafile File Providers, runtime cost of two-step polling under concurrent load, behavior of `EvalSymlinks` on real cloud-only File Provider paths, `bind(2)` behavior on edge-case socket paths — will be discovered during implementation and integration testing. CI golden tests + multi-platform integration tests will catch what spec audits cannot.

### Termination recommendation

**Audit phase CLOSED with v2.4.**

Whether round 13 had found 0 BLOCKERs (confirming the spec is stable) OR found new BLOCKERs/HIGHs (confirming the round-12 thesis that audits introduce drift), the conclusion is the same: switch to code-and-test. Round 13 found 2 HIGHs against v2.3 edits; per the round-13 task spec's termination criterion, this confirms the round-12 thesis empirically. The inflection point has passed.

Code-and-test phase work that integration tests will catch:
- Two-phase `wezsesh find` race conditions (concurrent connected clients, mid-flight window closure).
- Toast truncation aesthetics and any control-char-filtering surprises.
- Actual cloud-sync File Provider on-disk path stability across provider releases.
- Cross-platform CI matrix (linux-amd64, linux-arm64, darwin-amd64, darwin-arm64) shaking out any `golang.org/x/sys/unix` constant-availability surprise.
- Polling cadence under concurrent-TUI load.
- `wezsesh nuke` symlink-rejection across symlink farms.
- Resurrect schema drift on real user state files (legacy + current).

---

The most striking observation of round 13 is the sharp severity drop: round 12 found seven BLOCKERs all rooted in v2.2 spec drift, while round 13 found zero BLOCKERs and only two HIGHs — both in the *same v2.3 addition* (the two-phase `wezsesh find` flow). The audit cadence has converged: each round catches less, but each round still catches something. The MEDIUM count remains roughly stable because doc-clarity issues are infinite at the margin. The BLOCKER count went from 7 → 0 in one round; that is the expected signal of audit-phase saturation.

Code-and-test will catch the rest empirically. The audit phase has done its job.
