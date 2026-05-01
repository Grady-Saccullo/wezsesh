# PRD Research Findings — Round 9 (bubbletea-v2 / payload-budget / TOCTOU / Lua-payload-robustness audit)

Goal of this round: validate four lenses that prior rounds touched only superficially — bubbletea v2 migration semantics (round 7 spec'd the upgrade but never read v2 source), OSC payload size under round-8's expanded canonical escape rules, state-mutation TOCTOUs across concurrent TUIs, and the Lua-side `user-var-changed` handler's robustness against attacker-controlled payloads.

**Six BLOCKERs found** — equal to round 8. The audit-convergence pattern from rounds 5/6/7 (3/3/1 BLOCKERs) is decisively NOT extending; rounds 8 and 9 each surface a fresh wave because the audit lenses keep finding new surfaces. Plus eight HIGH-severity findings, four MEDIUM hygiene/clarity issues.

The headline finding: **`tea.After` was referenced through every PRD revision from v1.6 to v1.9 as the load-bearing retransmit Cmd primitive — and it has never existed in any released bubbletea version.** This was a fabrication that survived multiple rounds of "verified against source" claims because the prior audits read `handleCommands` and `Sequence` semantics but never checked whether `After` was actually exported. Cited correctly: `tea.Tick`.

---

## 1. Bubbletea v2.0.6 migration semantics — ONE BLOCKER

### Findings

Round 7 pinned the v1→v2 upgrade ("module path changed from `bubbletea` → `bubbletea/v2`") but did NOT read v2 source. Round 4's load-bearing claims (Cmd concurrency, renderer mutex, Sequence ordering, Send safety) were all measured against v1.3.5. This spike re-validated each claim against v2.0.6.

**Five claims confirmed unchanged**:
- Cmd execution as concurrent goroutines (v2 `tea.go` `handleCommands` retains the `go func() { p.Send(cmd()) }()` pattern verbatim, with the same comment about shutdown latency).
- Renderer mutex protection (v2's `cursed_renderer.go:27` declares `mu sync.Mutex`; same pattern, different impl).
- `tea.Sequence` orders Cmd execution but not Cmd-effect-vs-renderer (v2 `execSequenceMsg` is a sequential for-loop over Cmds, sending each result through `p.Send` — no renderer synchronization).
- `Program.Send` safety (v2 retains identical `select` over `p.ctx.Done()` / `p.msgs <- msg` — non-blocking, silently drops on exit).
- User Cmds cannot access renderer mutex (`mu` is private, unexported).

**One claim invalidated — BLOCKER**:

#### `tea.After` does not exist

PRD §6.2 step 10, §6.4, §6.15, and §9 all referenced:
```go
tea.After(2 * time.Second, retransmitMsg{})
```
as the canonical retransmit primitive. **Verified absent in both `commands.go` of v1.3.5 and v2.0.6.** Both versions export `Tick`, `Every`, `Batch`, `Sequence` — no `After`.

This was a fabrication that propagated through PRD revisions from v1.6 onward. Multiple "verified against source" claims (rounds 4, 7, 8) read adjacent functions but never checked the existence of `After`.

### Decisions — §6.2 step 10, §6.4 retransmit pattern, §6.15

- Replace every reference to `tea.After(d, msg)` with `tea.Tick(d, func(t time.Time) tea.Msg { return msg })`.
- `tea.Tick` is bubbletea's actual one-shot timer Cmd (fires once per Cmd invocation; not periodic).
- Cancellation guarantee: bubbletea's `program.ctx` cancels in-flight Cmd goroutines on `tea.Run` return; the goroutine exits via the `select` in `Tick`'s implementation.
- New CI test: `goleak.VerifyNone` after `tea.Run` returns; the Tick Cmd's goroutine MUST exit within 100ms of context cancellation.

#### MEDIUM finding — v2 renderer architecture changed

v1's `standard_renderer.go` → v2's `cursed_renderer.go` delegating to `ultraviolet.TerminalRenderer` (a third-party ncurses-style library). Internal mutex is preserved; the `/dev/tty` + external `sync.Mutex` pattern in §6.4 still holds. CI integration test asserts no frame-OSC byte interleaving under concurrent load.

### Files cited
- `charmbracelet/bubbletea/v1.3.5/tea.go` (handleCommands, Send, Sequence) and `commands.go` (Tick, Every, Batch, Sequence — no After).
- `charmbracelet/bubbletea/v2.0.6/tea.go` (same handlers retained), `commands.go` (same exports), `cursed_renderer.go:27` (mutex preserved), `cursed_renderer.go:257-260, 579-581` (flush/render under mu).

---

## 2. OSC payload size budget under v8 escape rules — ONE BLOCKER, ONE HIGH

### Findings

Round 5 set the OSC ceiling at 4 KB canonical-JSON / 5.4 KB base64. Round 8 expanded canonical-JSON encoding to forbid short-form escapes (`\b`, `\n`, etc.) and require `\u00XX` for ALL of: U+0000–U+001F (C0), U+007F (DEL), U+0080–U+009F (C1), U+2028 / U+2029 (LS/PS). Worst-case bytes-per-control-char went from 2 (`\n`) to 6 (`
`).

**Per-verb worst-case canonical-JSON sizes** (with v8 escape rules + 200-byte name cap from §6.17 + envelope overhead ~252 bytes):

| Verb | Worst case (canonical) | Base64+OSC | Fits 4 KB? |
|---|---|---|---|
| `switch` / `load` | ~638 B | ~882 B | ✓ |
| `rename` | ~1,063 B | ~1,449 B | ✓ |
| `save` | ~743 B | ~1,023 B | ✓ |
| `new` | ~1,044 B | ~1,423 B | ✓ |
| `pin` | ~650 B | ~898 B | ✓ |
| `noop` | ~227 B | ~303 B | ✓ |
| **`delete` (50 names worst-case)** | **~20,438 B** | **~27,282 B** | **✗ 5× over** |
| **`tag` (10 escape-heavy tags)** | **~4,319 B** | **~5,758 B** | **✗ 1.05× over** |

#### BLOCKER — `delete` worst-case 5× over the ceiling

The `delete` verb takes an array of names. §5.7 documents bulk-delete via Tab/Space marking but specifies no per-OSC cap. A 200-byte name composed entirely of 3-byte LS/PS chars escapes to ~402 canonical bytes; 50 such names + envelope = ~20 KB. Even less-pathological control-byte content (TAB or DEL, which are 1 byte but still escape to 6) hits the same magnitude.

#### HIGH — `tag` verb tags content unvalidated

§6.6 sidecar schema lists `tags: ["api", "backend"]` but §6.17 validates only workspace names, not tag strings. An attacker (or a careless user paste) could emit tags with escape-heavy bytes; 10 tags × 67 LS chars × 6 bytes ≈ 4 KB just for tags content.

#### MEDIUM — §6.17 allowed TAB / DEL / C1 / LS/PS

§6.17 explicitly forbade C0 controls EXCEPT `\t`, but didn't forbid DEL (U+007F), C1 (U+0080–U+009F), or LS/PS (U+2028/U+2029). Each escapes to 6 canonical bytes per §6.3. No legitimate workspace name uses these; allowing them was a UX-vs-budget tradeoff that round 5 didn't make explicit.

### Decisions — §5.7, §6.3, §6.17

- **§5.7 / §6.3**: per-OSC cap of 5 names for `delete`. Bulk-delete batches as ⌈N/5⌉ sequential OSCs sharing a `bulk_id`. TUI shows one combined progress + final summary; best-effort, not transactional.
- **§6.17 name validation tightened**: TAB now forbidden (the prior `\t`-exemption was a UX lapse, not a need); DEL, C1 controls, U+2028 / U+2029 LS/PS all forbidden. Bounds canonical expansion to ~3× from multi-byte UTF-8, not 6× from escape encoding.
- **§6.17 tag validation added**: 1–10 tags per workspace, each 1–50 UTF-8 bytes, same byte rules as workspace names. Validation runs client-side in the binary at the `tag` verb's request-construction boundary.

### Files cited
- PRD §6.3 (canonical encoding rules), §6.17 (name validation), §9 (4 KB ceiling), §5.7 (bulk delete UX).
- `vtparse/src/lib.rs` (wezterm has no actual OSC byte cap; `MAX_OSC=64` is parameter count not bytes — confirmed by round 5).

---

## 3. State-mutation TOCTOU + concurrent-TUI hazards — ONE BLOCKER, THREE HIGH

### Spike 3a: Switch-poller false-positive (HIGH)

**Bug**: §6.15's poller predicate is "workspace appears in `cli list`". When the user invokes switch-to-X while ALREADY in workspace X, the predicate is true on iteration 0 (t=0) before any switch action occurs. For pure switch this is benign (correct outcome); for `switch+restore` (load-into-current), the false-positive could bypass the restore.

Plus a cross-window variant: workspace exists in window B; user in window A invokes switch; predicate sees the workspace in any window and declares success.

**Decision** — §6.15 pre-switch capture + augmented predicate:
- Before emitting the switch OSC, binary captures `(activeWorkspace, targetWindowID)` via `cli list-clients`.
- Poller predicate becomes: `(workspace appears in target_window_id) AND (target != pre-active OR isRestoreFlow)`.
- Pure switch to already-active workspace: short-circuits in one iteration (no 5s wait).
- `switch+restore` to already-active: bypasses equality check via `isRestoreFlow=true`; proceeds to emit follow-up restore.

### Spike 3b: `save`'s `expected_hash` TOCTOU — BLOCKER (silent data loss)

**Bug**: §6.7 specified that the Lua handler reads the snapshot, hashes, compares to `expected_hash`, then calls `resurrect.state_manager.save_state`. Between the hash check (T0) and resurrect's actual write (T2):
1. TUI A reads → hash matches → calls save_state.
2. TUI C, in the gap, reads → hash matches (still A's pre-write content) → calls save_state with C's content.
3. C's write finishes first; A's write completes second and silently clobbers C's data.

`safefs.AtomicWriteFile` makes individual writes atomic but does NOT serialize the read-then-write sequence across binaries. Both TUIs pass each other's hash check; the second writer wins; the first loses silently.

**Decision** — §6.7 mandatory `safefs.AcquireExclusive` (POSIX `fcntl(F_SETLKW)`):
- Lua handler acquires file-level exclusive lock on snapshot path BEFORE reading for hash comparison.
- Lock held through resurrect's write.
- Released after `file_io.write_state.finished`.
- 5s deadline; on contention, returns `SNAPSHOT_LOCKED` (new error code, additive to §6.3).
- Cross-binary semantics: POSIX advisory locks are per-process; second wezsesh's `F_SETLK` gets `EAGAIN`. Crash-safe (fcntl locks release automatically on process exit).
- NFS caveat documented (advisory locks are weakly defined on NFS; document and recommend external serialization).

### Spike 3c: Sidecar concurrent-write lost-update — HIGH

**Bug**: Sidecar writes for tags/pin/notes are read-modify-write. Two TUIs both read sidecar V1 with tags `["api"]`, both add a different tag, both write back. Last writer wins; the other's tag is silently lost.

`safefs.AtomicWriteFile` makes the write atomic but doesn't cover read+modify+write across binaries.

**Decision** — same `safefs.AcquireExclusive` pattern as save (§6.7 sidecar serialization subsection). Sidecar tag/pin/notes writes hold an exclusive lock for read-modify-write. Sub-millisecond hold time; users unaffected.

### Spike 3d: `wezsesh nuke` blast-radius — HIGH

**Bug**: §7.2 spec'd `nuke [--dry-run]` with default = DELETE (hostile by default). Plus: a same-UID attacker pre-placing symlinks at `~/.local/state/wezsesh` → `/home/user/important-data` would cause `nuke`'s `os.RemoveAll` to follow the link and delete user data.

**Decision** — §7.2 / §8.1:
- `--yes` MANDATORY for actual deletion. Default is preview (lists targets, no I/O writes).
- `--dry-run` is verbose preview (full paths, sizes, symlink warnings).
- Every target path `os.Lstat`-checked before unlink; symlinks logged and SKIPPED.
- `--snapshot-dir` `safefs.VerifyDir`-checked at command entry; symlink target aborts the entire run.
- New `safefs.SafeRemove` and `safefs.SafeRemoveAll` helpers; lint rule forbids `os.Remove` / `os.RemoveAll` in `cmd/wezsesh/nuke.go`.

### Files cited
- PRD §6.7 (hash check flow), §6.15 (switch poller), §6.19 (safefs), §7.2 (nuke spec).
- POSIX `fcntl(2)` advisory lock semantics; `flock(2)` cross-platform notes.

---

## 4. Lua-side untrusted-payload robustness — FOUR BLOCKERS, THREE HIGH

### Findings

The `wezterm.on('user-var-changed', ...)` handler receives attacker-controllable bytes (any process in any pane can emit OSC 1337 SetUserVar). The handler must navigate base64-decode → JSON parse → field validation → canonical-JSON re-serialize → HMAC verify → freshness/replay/window checks → dispatch. Each step can fail; some failures could:
- Raise a Lua error that wedges the event loop (PRD §6.14: "Never raise a Lua error in event handlers").
- Consume excessive CPU (DoS the wezterm GUI thread).
- Reach dispatch with malformed args.

**Four BLOCKERs**:

#### BLOCKER A — Pane-ID check placement

PRD §6.14 names pane-ID as "Layer 1" (first defense), but the handler structure was unspecified — implementations could naturally do JSON-parse first. An attacker firing 10000 OSCs/sec from a foreign pane would force base64-decode + JSON-parse + canonical-JSON encode + HMAC compute on every event before the pane-ID mismatch drops the message. DoS amplification by 100–1000×.

#### BLOCKER B — `wezterm.json_parse` raises on malformed JSON

Source: `lua-api-crates/json/src/lib.rs`. `wezterm.json_parse` returns `mlua::Result<...>`; on `serde_json::Error`, it raises a Lua error via `mlua::Error::external(...)`. NOT a return-tuple. An unwrapped raise propagates out of the `user-var-changed` handler; per PRD §6.14, "Uncaught errors in `wezterm.on` handlers wedge the wezterm event loop."

Trivially exploitable: send `{v:1,"id":"01JAB"}` (unquoted key) — handler crashes.

#### BLOCKER C — `ct_eq.eq` nil propagation

The constant-time compare from round 8:
```lua
function M.eq(a, b)
    if #a ~= #b then return false end
    ...
end
```
If `payload.hmac` is missing or wrong-type, calling `ct_eq.eq(payload.hmac, computed)` raises "attempt to get length of nil". Round 8 added the function but didn't specify the field-shape gate that must precede it.

#### BLOCKER D — `background_child_process` reply emission unwrapped

(Already partially addressed in round 8 §6.4, but spike 4 confirms a second occurrence: any other site that calls `background_child_process` outside the reply path needs the same `pcall` wrap. The `apply_to_config` keygen path is wrapped via the broader `apply_to_config` `pcall`; the IPC-reply path is wrapped per round 8; no new sites identified, but the rule is now codified as part of the §6.14 handler structure.)

#### HIGH findings

- Empty `{}`/`[]` shape ambiguity: round 8 said "pick Option A or B and use consistently"; PRD V2.md said "Pick one mechanism and use it consistently" but didn't pick. Implementation could choose either; CI test could be written for whichever is chosen, but the spec must pin one.
- Field-shape validator missing before HMAC: type/length checks must run before HMAC verify so downstream code is nil-safe and type-safe.
- Canonical-JSON encoder must `pcall`-wrap its caller: the encoder can raise on float subtype, deep nesting, invalid UTF-8, or untagged tables.

#### MEDIUM findings

- `seen_ids` TTL pruning placement uncodified: §6.15 said "at the end of each dispatch" but the actual pruning loop wasn't shown.
- GLOBAL write-back semantics: `wezterm.GLOBAL` is `Arc<Mutex<BTreeMap>>`; modifying a sub-table in-place doesn't persist across reads — must use a setter `state.set_state(pane_id_key, session)` after mutation.

### Decisions — §6.14 mandatory handler structure (added v2.0)

§6.14 now contains a normative "Mandatory `user-var-changed` handler structure" subsection with the exact code for steps (a)–(i):

(a) Pane-ID check FIRST (drop foreign panes before any payload processing).
(b) HMAC key availability check (`wezsesh_session_key == nil` → drop).
(c) JSON parse with `pcall`.
(d) Field-shape validator (type + length checks for v, id, ts, op, args, reply_sock, target_window_id, hmac).
(e) Canonical-JSON re-serialize with `pcall` (uses metatable-tagged `canonical_json.array` / `object` per §6.3 PIN).
(f) HMAC verify via `ct_eq.eq` (now nil-safe by virtue of the field-shape gate).
(g) Freshness, replay, target-window checks.
(h) Replay-guard write-back + TTL prune (60s rolling window) via `state.set_state`.
(i) Dispatch with `pcall`.

§6.3 PINS the canonical encoder to **Option B (wrapper-function metatables)**: `canonical_json.array{}`, `canonical_json.object{}`, `canonical_json.NULL`. Sentinel-field approach (Option A) rejected because user code that legitimately uses an `__array` field would silently corrupt encoding. Untagged tables are encoder errors.

CI fuzz test (§8.1): fire 10000 random/mutated payloads at the handler; assert (a) no Lua error escapes the handler boundary, (b) no `ops.dispatch` invocation for non-authenticated payloads, (c) wezterm GUI remains responsive.

### Files cited
- `wezterm/lua-api-crates/json/src/lib.rs` (`json_parse` raises Lua error path).
- `wezterm/lua-api-crates/spawn-funcs/src/lib.rs:54-72` (`background_child_process` raises on spawn failure).
- `wezterm/lua-api-crates/share-data/src/lib.rs` (GLOBAL `Arc<Mutex<BTreeMap>>` semantics — sub-table mutations don't persist without write-back).

---

## Summary of changes for PRD_V3 (= internally stamped v2.0)

### BLOCKER fixes (must change before code lands)

- **§6.2 / §6.4 / §6.15 / §9** (correctness): `tea.After` doesn't exist in any released bubbletea; replaced with `tea.Tick(d, fn)`. Spike 1.
- **§5.7 / §6.3** (correctness): DELETE worst-case payload exceeded 4 KB ceiling by 5×; per-OSC cap of 5 names + bulk-delete batching in Go. Spike 2.
- **§6.7 / §6.19** (correctness — silent data loss): save's hash-check + resurrect write was not atomic across TUIs; mandatory `safefs.AcquireExclusive` (POSIX `fcntl(F_SETLKW)`); new `SNAPSHOT_LOCKED` error code. Same pattern for sidecar writes. Spike 3.
- **§6.14** (security): mandatory `user-var-changed` handler structure pinned. Pane-ID check FIRST; `wezterm.json_parse` `pcall`-wrapped; field-shape validator before HMAC verify; canonical-JSON encode `pcall`-wrapped; dispatch `pcall`-wrapped. CI fuzz test added. Spike 4 (multiple BLOCKERs collapsed into one structural fix).
- **§6.3** (correctness): canonical encoder PINNED to Option B (wrapper-function metatables). Sentinel-field approach rejected. Spike 4.
- (Implicit BLOCKER closed: with the field-shape gate in (d), `ct_eq.eq` becomes nil-safe; with `pcall` around `wezterm.json_parse`, malformed-JSON DoS is closed.)

### HIGH-severity correctness/security

- **§6.15**: switch-poller false-positive when user is already in target workspace. Pre-switch state capture + augmented predicate. Spike 3a.
- **§6.7 sidecar serialization**: lost-update on concurrent tag/pin writes. `safefs.AcquireExclusive` for sidecars. Spike 3c.
- **§7.2 / §8.1**: `wezsesh nuke` symlink hijack + dangerous default. `--yes` mandatory; `os.Lstat` defense via `safefs.SafeRemove` / `SafeRemoveAll`. Spike 3d.
- **§6.17**: tag-string validation added (1–10 tags, 1–50 UTF-8 bytes each, same byte rules as names). Spike 2.
- **§6.17**: name validation tightened — TAB / DEL / C1 / LS/PS now forbidden. Spike 2.
- **§6.14 (d)**: field-shape validator MUST run before HMAC verify. Spike 4.
- **§6.14 (e)**: canonical-JSON encoder MUST be `pcall`-wrapped. Spike 4.

### MEDIUM updates

- **§6.4 / §9**: bubbletea v2 renderer architecture changed to ultraviolet; mutex protection equivalent; `/dev/tty` pattern still holds. Spike 1.
- **§6.14 (h)**: `seen_ids` TTL pruning loop pinned to "end of each dispatch"; explicit `state.prune_seen_ids(session, 60)` call.
- **§6.14 (h)**: GLOBAL write-back via `state.set_state` after every sub-table mutation.

### Severity tally

- BLOCKER: 6 (tea.After fabrication, DELETE payload-budget overflow, save TOCTOU, Lua handler structure including JSON-parse/ct_eq/pane-ID-ordering, canonical encoder pin)
- HIGH: 8 (switch poller false-positive, sidecar lost-update, nuke blast-radius, tag content validation, name validation tightening, field-shape gate, canonical encoder pcall, bubbletea v2 renderer)
- MEDIUM: 4 (renderer architecture, seen_ids pruning placement, GLOBAL write-back, locale-pinned sort already covered v8)
- No-issue: numerous (Cmd concurrency, Send safety, Sequence semantics, etc.)

### Status

v9 complete. Six new BLOCKERs addressed; PRD bumped to v2.0. The audit-convergence pattern from rounds 5/6/7 (3/3/1) decisively does NOT extend — rounds 8 and 9 each found 6 BLOCKERs because the audit lenses keep finding new under-specified surfaces. **Continue iterating; do NOT terminate.**

The most striking finding of this round is that `tea.After` was fabricated and propagated through four PRD revisions despite multiple "verified against source" rounds. This suggests that re-validating already-cited claims (not just looking for new surfaces) has continued value — round 10 should include a "go back and verify every previously-cited bubbletea / wezterm / resurrect API still exists" sweep.

Suggested round-10 audit lenses:

- **Re-validate every cited API** in §6.4, §6.5, §6.13, §6.14, §6.15. The `tea.After` fabrication has uncomfortable implications — what other "verified" Lua / Rust APIs might similarly not exist? Worth a sweep with grep + GitHub source.
- **`safefs.AcquireExclusive` cross-binary semantics on darwin** (POSIX advisory locks have macOS-specific quirks; verify `fcntl(F_SETLKW)` works as expected on APFS, especially with iCloud-Drive-synced snapshot dirs).
- **`SwitchToWorkspace` activity coupling** (§6.15 says `mux.set_active_workspace` runs unconditionally before the spawn-when-empty check — verify this is still true in current wezterm; also verify the implicit "workspace switch" semantics of `cli activate-pane`).
- **Encryption + flock interaction**: encrypted snapshots are opaque ciphertext; do POSIX advisory locks behave correctly when the file is concurrently being decrypted/re-encrypted by resurrect's age/rage/gpg path?
- **Lua coroutine semantics on `pcall` boundary inside `apply_to_config`** — the round-8 `io.open` pre-flight check assumed Lua's filesystem ops are blocking but bounded; what about a network-mounted `~` (autofs, cloud-sync) where `io.open` itself can hang?
- **`wezterm.GLOBAL` mutation semantics under concurrent dispatch** — multiple `user-var-changed` events can fire concurrently in the same VM; does `seen_ids` write-back race? Is the `Arc<Mutex<BTreeMap>>` lock acquired per-key or per-tree?
