# PRD Research Findings — Round 11 (v2.1-edits-revalidation / File-Provider-cloud-sync / resurrect-API-runtime-validation / encryption-hang-surface audit)

Goal of this round, as round 10's recommendation: re-validate v2.1's recently-introduced edits (`safefs.IsNetworkFS`, `F_SETLK` polling, XDG read timeouts) against the platforms they actually target, plus the four open lenses round 9 surfaced (encryption + flock interaction, resurrect API runtime validation that round 10 had to skip due to repo access, `SwitchToWorkspace` activity coupling, bubbletea v2 resize semantics) and the `SUN_PATH` ceiling lens.

**Two new BLOCKERs found** plus seven HIGH-severity findings and three MEDIUMs. The audit-convergence pattern continues to weaken: round 10 had three BLOCKERs (down from six in rounds 8/9), round 11 has two — but both BLOCKERs are against v2.1's own recently-added defenses, fitting the round-10 thesis that "the right termination condition is not zero findings overall but zero findings AGAINST our own recent edits."

The headline findings: **(1)** v2.1's `safefs.IsNetworkFS` cannot detect modern macOS cloud-sync — iCloud Drive, Dropbox 2024+, and Google Drive Desktop 2024+ all migrated from osxfuse to Apple's File Provider framework, and File Provider–backed paths report `f_fstypename = "apfs"` indistinguishable from local APFS. The v2.1 helper has zero coverage for the dominant macOS cloud-sync risk. **(2)** Resurrect's `default_on_pane_restore` callback signature was wrong in PRD v1.7–v2.1: PRD said `(pane, pane_tree)`; verified actual signature is `(pane_tree)` only with pane accessed as `pane_tree.pane`. The §6.18 RCE defense is built on this callback — a two-argument hook would crash on first restore, fail-open, and silently bypass the defense.

---

## 1. `safefs.IsNetworkFS` against modern macOS cloud-sync — TWO BLOCKER, THREE HIGH, TWO MEDIUM

### Findings

#### BLOCKER A — File Provider cloud-sync is invisible to `Statfs`

iCloud Drive on macOS 13+ migrated from a separate FUSE/osxfuse mount to Apple's **File Provider framework** ([eclecticlight 2023, Sonoma iCloud Drive radically changed](https://eclecticlight.co/2023/10/25/macos-sonoma-has-changed-icloud-drive-radically/); [TidBITS 2023, Apple's File Provider forces Mac cloud storage changes](https://tidbits.com/2023/03/10/apples-file-provider-forces-mac-cloud-storage-changes/)). File Provider extensions are NOT mounted as separate filesystems. They are user-space backends to standard APFS paths. `Statfs(path).Fstypename` returns `"apfs"` regardless of whether the path's contents are local or File-Provider–materialized.

Dropbox migrated to File Provider in their 2024+ macOS client ([Dropbox Help: macOS File Provider support](https://help.dropbox.com/installs/dropbox-for-macos-support)). Google Drive Desktop migrated similarly. The pre-2024 osxfuse-based clients are still in use, but the helper's `osxfuse` match catches only those.

**Impact**: v2.1's `safefs.IsNetworkFS(snapshotPath)` returns `(fsType: "apfs", isNetwork: false)` for ANY iCloud Drive / Dropbox / Google Drive Desktop folder on a modern macOS install. The helper, intended to warn users that POSIX advisory locks don't serialize against cloud-sync atomic-replace writes, gives ZERO coverage for the dominant cloud-sync risk on darwin. A user puts their snapshot dir under `~/iCloud Drive/wezterm/` expecting wezsesh to warn about lock-bypass; it doesn't.

#### BLOCKER B — `Statfs`-as-hang-surface

A second, subtler issue: `Statfs(path)` itself can block in the kernel on a hung NFS server ([Red Hat NFS hang KB](https://access.redhat.com/solutions/28211); NFS lockd / nfsd unresponsive). The helper designed to warn about hang-prone filesystems would itself hang at TUI launch on a hung NFS path BEFORE any UI renders. v2.1's `IsNetworkFS` is called eagerly at startup and on every doctor invocation. Without timeout-wrapping, the helper inverts its intent: it becomes the hang it was designed to warn about.

**Severity**: BLOCKER because v2.1 introduced the helper specifically to bound hang surfaces, and the helper is itself unbounded.

#### HIGH — FUSE-T reports as NFS

FUSE-T, the Apple-Silicon kext-less FUSE replacement, implements FUSE via NFS loopback ([fuse-t.org](https://www.fuse-t.org/), [macos-fuse-t/fuse-t](https://github.com/macos-fuse-t/fuse-t)). `Fstypename` returns `"nfs"`. The helper currently classifies this as a true NFS mount and emits the cross-host serialization WARN — technically a false-positive (the data isn't remote-NFS-shared) but acceptable: FUSE-T inherits NFS-style locking quirks, and the WARN is appropriate. Document the false-positive class.

#### HIGH — autofs returns autofs magic, not underlying NFS

On Linux, `Statfs` on an autofs path returns `AUTOFS_SUPER_MAGIC` (0x0187) BEFORE the underlying mount is triggered ([Linux kernel autofs docs](https://docs.kernel.org/filesystems/autofs.html); `statfs(2)` does not trigger automount, only `stat(2)` does). v2.1's helper recognizes autofs and emits the WARN — correct behavior, but worth documenting that probing the underlying mount type would require `stat()` which can hang. The pattern is: treat autofs as "warn" without trying to drill down.

#### HIGH — `f_fstypename` enumeration was missing `"fuse"`

v2.1's darwin match list was `nfs`, `smbfs`, `webdav`, `osxfuse`, `afpfs`, `autofs`. Some FUSE implementations on macOS report `"fuse"` (generic) instead of `"osxfuse"` (kext-specific). v2.1 missed this; v2.2 adds it.

#### MEDIUM — Bind-mounted network FS

On Linux, bind-mounting a network filesystem under a local mount does NOT hide the underlying network-fs type from `Statfs` (`Statfs` returns the underlying filesystem's `f_type`, not the bind-mount's). No false-negative class here. Document for completeness.

#### MEDIUM — Documented false-negative class

A snapshot dir at a cloud-sync path NOT on Layer 2's prefix list (e.g., custom symlink to a third-party File Provider extension we don't recognize, or a non-default location) silently bypasses detection. v2.2 documents this honestly: the WARN is best-effort. The actual data-loss defense is the recommendation that snapshot dirs live under `~/.local/share/wezterm/state/resurrect/` (resurrect default) — explicitly NOT in any cloud-sync directory.

### Decisions — §6.7, §6.19

- §6.7 / §6.19 v2.2: two-layer detection. Layer 1 (`Statfs`) keeps the network-FS list and adds `"fuse"`. Layer 2 path-prefix heuristic added for File Provider extensions.
- §6.19 v2.2: `IsNetworkFS` runs `Statfs` in a goroutine under `context.WithTimeout(2s)`; on overrun, returns `("unknown", "statfs-timeout", true, nil)` — treats unprovable paths as network.
- `IsNetworkFS` signature changed: now returns `(fsType string, fsLayer string, isNetwork bool, err error)` — `fsLayer` records which detection layer triggered (statfs / cloudpath / unknown).
- Doctor reports both `Statfs` type AND matched cloud-sync prefix per path; user sees which layer triggered the warning.
- Documented false-negative class explicit.

### Files cited

- [eclecticlight 2023 Sonoma iCloud Drive](https://eclecticlight.co/2023/10/25/macos-sonoma-has-changed-icloud-drive-radically/)
- [TidBITS 2023 File Provider migration](https://tidbits.com/2023/03/10/apples-file-provider-forces-mac-cloud-storage-changes/)
- [Dropbox Help: macOS File Provider support](https://help.dropbox.com/installs/dropbox-for-macos-support)
- [Apple Developer: File Provider framework](https://developer.apple.com/documentation/fileprovider)
- [fuse-t.org](https://www.fuse-t.org/), [macos-fuse-t/fuse-t](https://github.com/macos-fuse-t/fuse-t)
- [Linux kernel autofs docs](https://docs.kernel.org/filesystems/autofs.html)
- [Red Hat NFS hang KB](https://access.redhat.com/solutions/28211)
- [statfs(2) Linux man page](https://man7.org/linux/man-pages/man2/statfs.2.html)
- [`golang.org/x/sys/unix`](https://pkg.go.dev/golang.org/x/sys/unix) — Statfs constants

---

## 2. POSIX advisory-lock semantics in cross-process / multi-fd / encryption — ONE HIGH, ONE HIGH, ONE MEDIUM

### Findings

#### HIGH — Multi-fd close releases ALL locks the process holds on a file

POSIX semantics — verified Linux [`fcntl_locking(2)`](https://man7.org/linux/man-pages/man2/fcntl_locking.2.html) and [macOS `fcntl(2)`](https://man.freebsd.org/cgi/man.cgi?query=fcntl&sektion=2):

> "If a process closes any file descriptor referring to a file, then all of the process's locks on that file are released, regardless of the file descriptor(s) on which the locks were obtained."

A latent footgun in `safefs.AcquireExclusive`'s critical section: if any code path opens-and-closes a second fd to the locked path (e.g., a JSON parser library, a logging helper that writes to the same file, a defensive re-read), the lock silently drops without notification. v2.1's spec doesn't document this discipline.

**Linux mitigation**: `F_OFD_SETLK` (Open File Description locks; Linux ≥ 3.15) bind to the open file description (the fd entry), not the process. They survive close-of-other-fd. **darwin/FreeBSD lack OFD locks** — verified absent in current macOS SDK. On those platforms the multi-fd discipline is the only defense.

v2.2 adds an explicit fd-hygiene contract on `AcquireExclusive`:
1. The `release` closure is the ONLY way to drop the lock (callers must not close the returned fd directly).
2. Lint rule: while internal/safefs/lock_*.go's fd is live, callers MUST NOT call `os.Open` / `unix.Openat` / `os.OpenFile` on the same path. Read operations during the hold use the locked fd (pread / Read+Seek).
3. Linux uses `F_OFD_SETLK` when available; darwin/FreeBSD use `F_SETLK` with discipline.
4. CI test demonstrates the difference: opens path, takes lock, opens-and-closes a second fd to the same path, attempts a second `AcquireExclusive` from a subprocess — must succeed on darwin (lock dropped) and fail on Linux (OFD lock survived).

#### MEDIUM — Lock framing in §6.7 implies the lock blocks resurrect's writer

POSIX advisory locks are observed only by code that calls `fcntl(F_GETLK)`/`fcntl(F_SETLK)`. Plain `open(2)` and `write(2)` ignore them. Resurrect's writer (running inside wezterm's process) calls `io.open(path, "w+")` → libc `fopen(3)` → kernel `open(2)`. None of those consult advisory locks.

The v2.1 prose in §6.7 said the lock "serializes save and resurrect's write" — true only across multiple wezsesh BINARIES, NOT against resurrect's own writer. The serialization actually works through MESSAGE ORDERING, not the lock: wezsesh acquires lock → emits OSC → resurrect writes (lock-bypass-but-this-is-the-write-we-asked-for) → emits `file_io.write_state.finished` → wezsesh releases lock. A second `wezsesh` cannot enter its own hash-check-and-OSC until the first releases. The lock is a "binary-instance gate", not a "physical write barrier".

This is sound under the assumption that resurrect's `save_state` is invoked ONLY through wezsesh's IPC path. If a user calls `resurrect.state_manager.save_state(...)` directly from their wezterm config (e.g., a separate keybinding), the lock is bypassed. v2.2 clarifies the framing and documents as v0.1 limitation.

#### LOW — F_WRLCK on a 0-byte file with `Len=0`

POSIX semantics: `Flock_t{Start: 0, Len: 0}` means "from byte 0 to end-of-file (Len=0 = unbounded, future bytes included)". Verified macOS `fcntl(2)` and Linux `fcntl_locking(2)`. On a 0-byte file, the lock covers all future appends. `io.open(path, "w+")` truncating to 0 bytes does not affect the lock's coverage. No change required.

### HIGH — Encryption pipeline hang surface

Verified [resurrect encryption.lua](https://raw.githubusercontent.com/MLFlexer/resurrect.wezterm/main/plugin/resurrect/encryption.lua): the encrypt step shells out via `wezterm.run_child_process({"age", "-e", ...})` (or `gpg`) with NO timeout. If gpg-agent is unresponsive (locked YubiKey, missing pinentry, smartcard daemon stalled, network HSM glitch), the encrypt subprocess blocks indefinitely. wezterm's `run_child_process` is `cmd.output().await` — verified prior rounds, no timeout. The config-eval coroutine suspends → entire wezterm GUI freezes until the process is killed externally.

Worse: while wezterm's GUI is frozen, wezsesh's binary is still holding its `AcquireExclusive` lock waiting for the `file_io.write_state.finished` event that never fires. The lock times out after 5s and returns `SNAPSHOT_LOCKED` even on subsequent retries — but the GUI itself remains frozen.

v2.1's §7.4 hang-surfaces table omitted this. v2.2 adds it as a residual hang surface; doctor probes encryption configuration and runs a non-blocking `<provider> --version` probe with 2s timeout; surfaces `ENCRYPTION_AGENT_SLOW` (additive to §6.3) as a non-fatal hint. README requires users with encryption to verify gpg-agent / pinentry / smartcard daemon health. The hang itself is uncatchable from wezsesh — upstream resurrect's process. Future hardening: contribute a timeout wrapper to resurrect upstream.

### Decisions — §6.7, §6.19, §7.4

- §6.19 v2.2: explicit fd-hygiene contract on `AcquireExclusive`; lint rule; OFD locks on Linux; CI test.
- §6.7 v2.2: framing clarification — lock is a "binary-instance gate", not a "physical write barrier".
- §7.4 v2.2: encryption pipeline added as residual hang surface; doctor probe; `ENCRYPTION_AGENT_SLOW` error code.

### Files cited

- [Linux fcntl_locking(2)](https://man7.org/linux/man-pages/man2/fcntl_locking.2.html)
- [macOS / FreeBSD fcntl(2)](https://man.freebsd.org/cgi/man.cgi?query=fcntl&sektion=2)
- [resurrect encryption.lua](https://raw.githubusercontent.com/MLFlexer/resurrect.wezterm/main/plugin/resurrect/encryption.lua)
- `lua-api-crates/spawn-funcs/src/lib.rs:30-50` (`run_child_process` no-timeout, prior round verified)

---

## 3. Resurrect API runtime validation (round 10's deferred spike) — ONE BLOCKER, ONE HIGH, ONE MEDIUM

### Findings

Round 10 attempted this spike but failed at GitHub access. Round 11 successfully fetched MLFlexer/resurrect.wezterm sources at https://raw.githubusercontent.com/MLFlexer/resurrect.wezterm/main/.

#### BLOCKER — `default_on_pane_restore` callback signature mismatch

Verified [resurrect tab_state.lua](https://raw.githubusercontent.com/MLFlexer/resurrect.wezterm/main/plugin/resurrect/tab_state.lua) line ~95:

```lua
function pub.default_on_pane_restore(pane_tree)
  local pane = pane_tree.pane
  -- ...
end
```

And the call site (tab_state.lua line ~115 in `make_splits`):
```lua
if opts.on_pane_restore then
  opts.on_pane_restore(pane_tree)  -- single argument, NOT (pane, pane_tree)
end
```

PRD v1.7–v2.1 said the callback was `(pane, pane_tree)`. This is wrong. Resurrect passes a SINGLE argument. The pane is accessed as `pane_tree.pane`.

**Impact**: a wezsesh hook installed per the v2.1 spec as `function on_pane_restore(pane, pane_tree)` would receive `pane_tree` as `pane` (first param) and `nil` as `pane_tree` (second param). The very first attempt to access `pane_tree.process.argv[1]` would error on `nil` index. The hook would crash before reaching the allowlist check. The crash would propagate through resurrect's call (no `pcall` wrapper around `opts.on_pane_restore` in resurrect's source) and abort the restore — but ONLY for that pane. Other panes' restores would proceed. Whether the crash blocks resurrect's `default_on_pane_restore` from running depends on resurrect's caller — verified that resurrect's own call site does NOT pcall-wrap user hooks, so the error propagates up and the default does NOT run. This is fail-CLOSED in the lucky case (no `send_text` of malicious argv) but the user sees a crashed restore with no explanation.

If a future maintainer adds a `pcall` wrapper around the hook call (a reasonable hygiene change), the failure becomes fail-OPEN: hook crashes → `pcall` catches → resurrect proceeds to call `default_on_pane_restore` which executes `pane:send_text(shell_join_args(argv))` — the exact RCE the §6.18 defense was supposed to block.

**v2.2 fix**:
- Callback signature corrected to single-argument `on_pane_restore(pane_tree)`; pane derived as `pane_tree.pane`.
- Explicit fail-CLOSED on hook crash: hook body wrapped in our own outer `pcall`; on caught error, log WARN, send `pane:send_text("\r\n")` only, NEVER call resurrect's default.
- CI assertion: a hook that raises must NOT result in `pane:send_text(shell_join_args(argv))`.

#### HIGH — argv indexing was inconsistent in PRD prose

Lua arrays are 1-based. Resurrect's `pane_tree.process.argv` follows that convention: `argv[1]` is the program name (e.g., `"/bin/bash"` or `"bash"`), `argv[2..]` are the program's arguments. This is opposite to C-style argv where `argv[0]` is the program.

PRD prose used `argv[0]` and `argv[1]` interchangeably across sections. The defense was actually checking the right element (`argv[1]`) but the prose ambiguity could mislead implementers. v2.2 normalizes to `argv[1]` everywhere with explicit note about the indexing convention.

#### Verified — all other resurrect APIs

- `resurrect.file_io.write_state.start` and `.finished`: both fire unconditionally; payload is `(file_path, event_type)`. `.finished` fires on both success and failure paths. Matches PRD §6.7 claim.
- `resurrect.error`: single unified event for all error paths (encryption, write, decryption); payload is a formatted string.
- `resurrect.workspace_state.restore_workspace.start` and `.finished`: both exist; no payload. `.finished` fires after all windows processed (partial-success-tolerant).
- `state_manager.save_state(state, opt_name)`: actual signature has an optional second param; PRD's single-arg usage is compatible.
- `state_manager.save_state_dir` and `change_state_save_dir`: both public; default path set per-OS via utils branching.
- Encryption path goes through SAME `file_io.write_state.{start,finished}` events (plus inner `.encrypt.{start,finished}`); the snapshot-write gate (`wezsesh_writing[path]`) clears correctly regardless of encryption.
- `resurrect.file_io.encrypt.start` / `.finished` events exist (PRD didn't reference them; out of scope).

#### MEDIUM — `process` schema drift characterization stale

PRD v2.1 said `process` "has both string-shape (pre-2024-08) and object-shape (current) in the wild." Verified resurrect's main branch now writes object-shape only; string-shape only appears in pre-2024-08 saved files in users' state dirs. Tolerant Go parser still required for legacy snapshots, but the in-the-wild characterization was inaccurate. README schema lags implementation; treat resurrect source as authoritative. v2.2 corrects.

### Decisions — §6.18, §6.6

- §6.18 v2.2: callback signature corrected; explicit fail-CLOSED on hook crash; CI assertion.
- §6.18 v2.2: argv indexing normalized to `argv[1]` everywhere.
- §6.6 v2.2: schema drift characterization updated; README-vs-source authority note.

### Files cited

- [resurrect tab_state.lua](https://raw.githubusercontent.com/MLFlexer/resurrect.wezterm/main/plugin/resurrect/tab_state.lua)
- [resurrect file_io.lua](https://raw.githubusercontent.com/MLFlexer/resurrect.wezterm/main/plugin/resurrect/file_io.lua)
- [resurrect state_manager.lua](https://raw.githubusercontent.com/MLFlexer/resurrect.wezterm/main/plugin/resurrect/state_manager.lua)
- [resurrect workspace_state.lua](https://raw.githubusercontent.com/MLFlexer/resurrect.wezterm/main/plugin/resurrect/workspace_state.lua)
- [resurrect pane_tree.lua](https://raw.githubusercontent.com/MLFlexer/resurrect.wezterm/main/plugin/resurrect/pane_tree.lua)
- [resurrect encryption.lua](https://raw.githubusercontent.com/MLFlexer/resurrect.wezterm/main/plugin/resurrect/encryption.lua)

---

## 4. SwitchToWorkspace + bubbletea v2 + SUN_PATH — ONE HIGH, ONE MEDIUM

### Findings

#### HIGH — `runtime_dir` not validated against `SUN_PATH` ceiling at config-load

v2.1 documented the `SUN_PATH` ceiling (104 darwin, 108 Linux) for the default `/tmp/wezsesh-<uid>/` path but did not require `apply_to_config` to validate user overrides. A user setting `runtime_dir = "~/Library/Application Support/.../"` silently passes config load, then every TUI launch fails at `net.Listen("unix", path)` with `ENAMETOOLONG`. The TUI surfaces a generic `IPC_TIMEOUT` and the root cause is invisible to the user.

**v2.2 fix**: Lua-side `apply_to_config` validates `len(opts.runtime_dir) + 14 ≤ ceiling` (14 = `/<8hex>.sock`); raises a config error with exact remediation message at config-load time. Go binary independently re-validates at runtime as defense-in-depth, surfacing new error code `IPC_INIT_FAILED` (additive to §6.3).

#### MEDIUM — PIPE_BUF atomicity caveat for "two fds, kernel serializes"

The §6.4 fix for bubbletea ↔ OSC byte interleaving relies on opening `/dev/tty` separately and writing under our own mutex. The PRD claimed "two fds, two mutexes, kernel serializes" — true for atomic-PIPE_BUF writes only.

Kernel atomicity is per-`write(2)` syscall up to PIPE_BUF (4096 on Linux, 512 POSIX-min on darwin — though darwin's actual TTY-driver buffer is larger; treat 4 KB as the conservative ceiling). Writes LARGER than PIPE_BUF are NOT atomic — Go's `Write` may issue multiple `write(2)` syscalls under the hood, and a concurrent `write(2)` from the OSC fd can interleave at the boundary between two of bubbletea's calls.

Worst case: bubbletea is mid-emission of an 8 KB full-redraw frame on terminal resize, and our OSC write slips in between bubbletea's two syscalls. The wezterm parser sees `[first half of frame] + [full OSC] + [rest of frame]` — both are well-formed escape sequences, the parser handles them in order, but the visual frame may flash glitchy mid-render.

**Risk classification**: cosmetic, not security or correctness. The OSC payload is `\x1b]1337;...\x07` which doesn't contain control sequences that affect frame state. The 4 KB OSC ceiling (§6.3) ensures our writes stay atomic per-syscall; we rely on bubbletea's frame writes being either small (atomic) or splittable (cosmetic glitch acceptable).

v2.2 clarifies the boundary in §6.4. No structural change.

#### Verified — bubbletea v2 SIGWINCH handling

bubbletea v2.0.6 handles SIGWINCH via `handleResize()` which captures the signal and emits a `WindowSizeMsg` to the event loop (NOT via direct write from the signal handler). The renderer (`cursed_renderer.go` line 27) preserves its internal `mu sync.Mutex` from v1. SIGWINCH does NOT cause a concurrent `write(2)` from a signal handler. The §6.4 mutex pattern remains sound.

#### Verified — `tea.Tick` exists; `tea.After` does not

Round 10's finding (renamed `tea.After` to `tea.Tick`) is correctly applied. v2.0.6 `commands.go` exports `Tick`, `Every`, `Batch`, `Sequence`. No `After`. Cancellation guarantee via `program.ctx`.

#### Drop — Spike D's claimed concurrent-TUI seen_ids race

Spike D claimed a BLOCKER: two concurrent wezsesh TUIs reading-modifying-writing `wezterm.GLOBAL.wezsesh_state[pane_id]` could lose updates to `seen_ids`. **This is wrong.** Re-checking the schema (§6.15): `wezsesh_state` is keyed by `tostring(spawned_pane_id)`. Each TUI session has its own pane id. Two concurrent TUIs in different panes operate on DIFFERENT GLOBAL keys; no shared key, no race. Round 10's actual race finding (multiple concurrent handler invocations within ONE process for ONE pane_id, e.g., #3524 broadcast-bug duplicates) was already closed by the cooperative-scheduling guarantee (handler steps (a)–(h) MUST be `.await`-free). Drop Spike D's 3b finding.

#### Speculative non-findings — Spike D's 1.1 / 1.2

Spike D flagged "SwitchToWorkspace execution order unverified" and "cli activate-pane cross-workspace semantics unverified" as HIGH/MEDIUM. The agent did not verify these against wezterm source — they're calls for further audit, not findings. Round 10 and earlier rounds already verified `SwitchToWorkspace.spawn` semantics (the spawn-when-empty check); the activity-coupling concern is speculative without source evidence. Defer to runtime-integration testing in v0.1 dev. No PRD edit.

### Decisions — §6.4

- §6.4 v2.2: `SUN_PATH` config-load validation in `apply_to_config`; `IPC_INIT_FAILED` error code added.
- §6.4 v2.2: PIPE_BUF atomicity caveat clarified.

### Files cited

- bubbletea v2.0.6 `cursed_renderer.go`, `commands.go` (verified previous rounds)
- macOS SDK `sys/un.h` (SUN_PATH = 104)
- Linux `linux/un.h` (SUN_PATH = 108)
- POSIX `pipe(7)` man page (PIPE_BUF semantics)

---

## Summary of changes for PRD_V5 (= internally stamped v2.2)

### BLOCKER fixes (must change before code lands)

- **§6.7 / §6.19** (correctness): `safefs.IsNetworkFS` cannot detect modern macOS cloud-sync (iCloud Drive, Dropbox 2024+, Google Drive 2024+ all use Apple File Provider, invisible to `Statfs`). v2.2 adds Layer 2 path-prefix heuristic for File Provider extensions; documented false-negative class. Spike 1.
- **§6.18** (security): resurrect's `default_on_pane_restore` callback signature was wrong in PRD v1.7–v2.1 (`(pane, pane_tree)` → actually `(pane_tree)` only). The §6.18 RCE defense would silently bypass on hook crash if a future resurrect maintainer adds `pcall` to the hook call. v2.2 corrects signature, mandates fail-CLOSED on hook crash, adds CI assertion. Spike 3.

### HIGH-severity correctness/security/reliability

- **§6.19**: POSIX advisory-lock multi-fd close footgun documented; `F_OFD_SETLK` on Linux; lint rule on darwin/FreeBSD; CI demonstrating the difference. Spike 2.
- **§6.18**: argv indexing normalized to `argv[1]` everywhere (Lua 1-based, opposite of C). Spike 3.
- **§7.4**: resurrect encryption pipeline hang surface added; doctor probe; `ENCRYPTION_AGENT_SLOW` code. Spike 2.
- **§6.4**: `runtime_dir` SUN_PATH validation at config-load time; `IPC_INIT_FAILED` code. Spike 4.
- **§6.19**: `Statfs` itself can hang on hung NFS path; helper now wraps in goroutine + 2s context timeout. Spike 1.
- **§6.7 / §6.19**: `f_fstypename` enumeration was missing `"fuse"`. Spike 1.
- **§6.7 / §6.19**: FUSE-T reports as NFS — false-positive class documented. Spike 1.

### MEDIUM updates

- **§6.7**: lock framing clarified — the lock is a "binary-instance gate", not a "physical write barrier"; sound only when resurrect is invoked through wezsesh's IPC path. Spike 2.
- **§6.4**: PIPE_BUF atomicity caveat clarified for "two fds, kernel serializes". Risk is cosmetic. Spike 4.
- **§6.6**: `process` schema drift characterization corrected — current resurrect writes object-shape only; pre-2024-08 string-shape only in legacy saved files. Spike 3.
- **§6.7 / §6.19**: autofs `Statfs` returns autofs magic before mount triggered — documented; treat as warn-by-default. Spike 1.

### Severity tally

- BLOCKER: 2 (File Provider cloud-sync invisibility, `default_on_pane_restore` callback signature wrong).
- HIGH: 7 (multi-fd close footgun, argv indexing prose, encryption hang surface, runtime_dir config-load validation, Statfs-as-hang-surface, missing `"fuse"` enumeration, FUSE-T false-positive).
- MEDIUM: 4 (lock framing clarification, PIPE_BUF atomicity caveat, schema drift characterization, autofs `Statfs` semantics).
- No-issue: numerous resurrect API surfaces re-confirmed (file_io events, restore_workspace events, save_state signature, state_manager paths, encryption events).

### Status

v11 complete. Two BLOCKERs addressed; PRD bumped to v2.2.

The audit-convergence pattern continues to hold the round-10 hypothesis: BLOCKERs found are predominantly against our OWN recent edits. Round 11's two BLOCKERs are both against v2.1 additions:

1. v2.1 introduced `safefs.IsNetworkFS` to warn about cloud-sync lock-bypass; round 11 found the helper has zero coverage on modern macOS for the dominant cloud-sync surface.
2. v2.1 (and earlier) inherited a wrong callback signature for `default_on_pane_restore`; round 11 finally read the actual resurrect source and caught it.

Both are corrections to OUR earlier specs, not newly-discovered library bugs. This is the convergence signature: each round catches drift between spec and reality, but the spec is increasingly self-consistent.

**Recommend ONE more round** before terminating, focused exclusively on what v2.2's edits introduced and on three lenses still not deeply audited:

- **Path-prefix heuristic for File Provider** introduced this round: how robust against `filepath.EvalSymlinks` quirks, NFC normalization, user-renamed iCloud Drive aliases, multiple iCloud accounts, `~/Library/CloudStorage/iCloud~*` schema variants?
- **`F_OFD_SETLK` Linux-only path** introduced this round: kernel version detection, fallback semantics, interaction with `pidfd` namespace tricks.
- **`SwitchToWorkspace` activity coupling** lens: round 11 deferred this to runtime testing because Spike D didn't verify against wezterm source. Worth one more focused source-read pass before the audit phase closes.
- **`cli activate-pane` cross-workspace semantics** lens: same — speculative concern from Spike D, never resolved.
- **`runtime_dir` permissions / TOCTOU** at config-load: the new validation runs at `apply_to_config`, but is the validated path the same one used at TUI launch? `apply_to_config` runs once at wezterm start; if the user's `$HOME` changes (rare but possible across `wezterm.plugin.update_all()`), or if `runtime_dir` includes `$HOME` and the home directory is remounted, the validated path could differ from the runtime path.

If round 12 finds zero BLOCKERs and zero HIGH-severity correctness issues, the audit phase is done. If round 12 again finds new edits-vs-reality drift in v2.2's additions, that suggests a structural issue with the audit cadence — at some point we have to ship code, run integration tests, and discover the remaining surfaces empirically.

---

The most striking finding of this round is that the §6.18 RCE defense — central to the PRD's threat model since round 6 — was built on a callback signature that doesn't exist in resurrect. The defense actually works in the current resurrect (resurrect doesn't `pcall`-wrap the hook, so a crashed hook fails-CLOSED), but only by accident: any future hygiene improvement upstream would silently invert our defense to fail-OPEN. v2.2's outer-`pcall` + explicit-no-default-call-on-crash makes the defense robust to this drift.

The pattern across rounds 8–11 is clear: each round finds smaller and more specific bugs, but the bugs are increasingly load-bearing (a one-character argv indexing question, a one-argument-vs-two-argument signature question, a string match `"osxfuse"` vs `"apfs"`). These are the kinds of bugs that normally only surface in production. The audit is shifting from "find architectural mistakes" to "find load-bearing details that drift". Worth continuing while the cost is low (one more round) but the value of additional rounds beyond that is diminishing.
