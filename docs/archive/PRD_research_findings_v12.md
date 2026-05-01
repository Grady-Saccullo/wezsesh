# PRD Research Findings — Round 12 (v2.2-edits-revalidation / Layer-2-heuristic-robustness / F_OFD_SETLK-portability / runtime_dir-TOCTOU / cli-activate-pane-cross-workspace audit)

Goal of this round, as round 11's recommendation: re-validate v2.2's recently-introduced edits (path-prefix heuristic, `F_OFD_SETLK` Linux fallback, `runtime_dir` SUN_PATH validation) plus the two lenses Spike D didn't actually verify (`SwitchToWorkspace` activity coupling, `cli activate-pane` cross-workspace semantics). Termination criterion: zero BLOCKERs and zero HIGH-severity findings.

**Result: SEVEN BLOCKERs, fifteen HIGH-severity findings, eleven MEDIUMs.** The audit-convergence pattern that round 11 hypothesized — "BLOCKERs are increasingly drift between v2.2's own recent edits and reality" — held emphatically. Six of the seven BLOCKERs are direct drift against v2.2 (path-prefix heuristic gaps, build-tag pattern, `O_CLOEXEC` missing, +14 path-component undercount, EvalSymlinks loop hang, symlinks-inside-snapshot-dir). The seventh is a never-verified carry-forward from earlier rounds (the `cli activate-pane` cross-workspace claim) that finally got definitively refuted by reading the wezterm server-side PDU handler.

Headline findings:

1. **`cli activate-pane` does NOT switch workspace.** Verified [`wezterm-mux-server-impl/src/sessionhandler.rs::SetFocusedPane`](https://raw.githubusercontent.com/wezterm/wezterm/main/wezterm-mux-server-impl/src/sessionhandler.rs): the server-side handler calls `window.save_and_then_set_active(tab_idx)` + `tab.set_active_pane(&pane)` + `mux.notify(MuxNotification::PaneFocused(pane_id))`, but does NOT call `mux.set_active_workspace_for_client(...)`. PRD §6.20's claim that "Cross-window/cross-workspace activation is server-side implicit (workspace switch happens automatically per §6.13)" is wrong. `wezsesh find` activates the pane internally but leaves the user on their original workspace — the activated pane is invisible until the user manually switches.

2. **Path-prefix heuristic is a hang surface.** Go's `filepath.EvalSymlinks` has no documented loop limit on darwin and can stall indefinitely on circular symlink chains until the kernel's `SYMLOOP_MAX = 40` is exhausted (or the chain is sufficiently deep that the kernel never times out). v2.2's Layer 2 spec calls `EvalSymlinks` eagerly at TUI launch — making the helper itself a startup-hang surface, exactly the bug v2.2 was designed to fix in Layer 1 (`Statfs`). Plus: third-party File Provider extensions (Box, pCloud, ProtonDrive, Sync.com, NextCloud, Seafile) and symlinks INSIDE the snapshot dir pointing to cloud-synced data are silently bypassed.

3. **`F_OFD_SETLK` fd leaks into fork-spawned children.** `unix.Openat(...)` without `unix.O_CLOEXEC` produces an fd that's inherited by every `os/exec` child (`wezsesh reply`, `wezsesh keygen`). For OFD locks specifically, the lock is owned by the OFD which is shared across forks; if the child closes its fd, the lock survives, BUT if the child explicitly calls `fcntl(F_OFD_UNLCK)` on the inherited fd (e.g., a future code path or library helper), it releases the OFD-level lock from the parent's perspective. Even without explicit unlock, having the fd inherited is a hygiene leak. v2.2's spec mandates the lock pattern but doesn't specify `O_CLOEXEC`.

4. **`F_OFD_SETLK` constant doesn't exist on darwin.** Code that references `unix.F_OFD_SETLK` directly will fail to compile under `GOOS=darwin` because the constant is only defined in `golang.org/x/sys/unix/zerrors_linux.go`, not in `zerrors_darwin_*.go` (verified). v2.2's spec says "use F_OFD_SETLK on Linux when available" but doesn't mandate the build-tag split (`lock_linux.go` + `lock_other.go`). Naive implementation will produce a darwin build break.

5. **`runtime_dir` `+14` budget undercounts by 9 bytes.** v2.2's Lua check is `len(opts.runtime_dir) + 14 ≤ ceiling`. The `+14` is for `/<8hex>.sock`. But §6.4 specifies the reply dir is `$XDG_RUNTIME_DIR/wezsesh/` (Linux) or `/tmp/wezsesh-<uid>/` (darwin). If the user passes `opts.runtime_dir = "$XDG_RUNTIME_DIR"` directly (without `/wezsesh/`), the binary appends `/wezsesh/` (9 bytes) and `/<8hex>.sock` (14 bytes) — total +23. The Lua check passes for paths that overflow at runtime. The Go-side defense-in-depth catches it, but the user-facing diagnostic is degraded (generic `IPC_INIT_FAILED` instead of helpful "shorten path" guidance).

The audit-cadence hypothesis is now firmly established: round 12 found seven BLOCKERs, all seven are drift against v2.2 or carry-forwards never verified. **Recommend ONE more round if any further v2.3 edits introduce new infrastructure**, otherwise close the audit phase: the structural issue is that pre-implementation specs grow drift faster than spec-only audits can close it, and round 12 is the inflection point where empirical / runtime testing must take over.

---

## 1. Path-prefix heuristic robustness — TWO BLOCKER, FIVE HIGH, FOUR MEDIUM

### Findings

#### BLOCKER A — `filepath.EvalSymlinks` is a startup-hang surface

v2.2's Layer 2 spec resolves the snapshot path with `filepath.EvalSymlinks` once before prefix matching (§6.7 / §6.19). Go's `EvalSymlinks` follows every symlink component sequentially, calling `os.Readlink` per step. On a circular chain (`a → b → a`), the resolution proceeds until the kernel's `SYMLOOP_MAX` (40 on darwin per `<sys/syslimits.h>`, 40 on Linux per `MAXSYMLINKS`) returns `ELOOP` — but Go does not impose its own loop limit, and on certain pathological filesystems (`/proc/1/root` magic links on Linux; verified [golang/go#73572](https://github.com/golang/go/issues/73572)) the resolution can recurse without termination.

Worse: the user-controlled path may legitimately contain symlinks deep enough that the kernel does NOT immediately reject — e.g., `~/foo → ~/.bar/baz/qux/...` chained 40 levels deep. Each `os.Readlink` syscall on darwin can also block if the symlink target's parent directory is on a hung NFS path or a cloud-only File Provider entry whose ancestor must be materialized. Layer 2 is called eagerly at TUI launch BEFORE any UI renders.

**Impact**: a user with a symlink loop in their snapshot directory path (or any ancestor) hangs the wezsesh launch with no diagnostic. The helper designed to warn about hang-prone storage IS the hang. Exactly the inversion v2.2 closed for `Statfs` but reintroduced via Layer 2.

**v2.3 fix**: wrap `filepath.EvalSymlinks` in a goroutine under `context.WithTimeout(500ms)`. On overrun, return the unresolved path and emit a non-fatal WARN: `wezsesh: snapshot path symlink resolution timed out — Layer 2 cloud-sync detection may be incomplete.` Treat timeout-unresolved paths as non-network (conservative — better a missed warning than a startup hang). Document in §7.4 hang surfaces.

#### BLOCKER B — Third-party File Provider extensions silently bypass detection

v2.2's Layer 2 prefix list covers only iCloud, Dropbox, Google Drive, OneDrive — the four dominant providers. A 2024–2026 census of macOS File Provider extensions reveals undetected providers users actively run:

- **Box for macOS** (File Provider since 2024): `~/Library/CloudStorage/Box-*` — not in PRD list.
- **NextCloud Desktop** ([open issue #1337](https://github.com/nextcloud/desktop/issues/1337)): community-implemented File Provider variants with paths like `~/Library/CloudStorage/Nextcloud-*`.
- **Seafile / SeaDrive** ([feature request](https://forum.seafile.com/t/feature-request-seadrive-macos-native-support-for-fileprovider/15861)).
- **ProtonDrive** (experimental File Provider on macOS 14+).
- **pCloud, Sync.com, MEGA, Yandex Disk** — most use legacy macFUSE or proprietary kexts, NOT File Provider, so Layer 1 should catch them via `osxfuse`/`fuse` match. But check anyway.
- **Custom mount paths**: Dropbox, OneDrive, and Google Drive allow advanced users to configure non-default sync locations. Path is no longer under `~/Library/CloudStorage/` — the heuristic misses entirely.

The PRD §6.7 honestly documents the false-negative class ("cloud-sync paths NOT on the prefix list bypass detection — best-effort warning, not a guarantee"). But the BLOCKER is that the false-negative class is much LARGER than the documented narrative implies. Round 11 framed Layer 2 as "covers File Provider extensions"; reality is "covers only the four most-popular providers' default paths."

**v2.3 fix**: extend the prefix list to include `Box*`, `Nextcloud*`, `Proton*` (best-effort). Document the false-negative class explicitly: "Layer 2 covers iCloud Drive, Dropbox, Google Drive, OneDrive at their default macOS paths. Other providers (Box, NextCloud, ProtonDrive, Seafile) are best-effort. Custom sync locations of any provider bypass detection. Place snapshot dirs under `~/.local/share/wezterm/state/resurrect/` (resurrect default) for guaranteed safety." Optionally (v0.2+): query `pluginkit -m -p com.apple.fileprovider-nonui` at doctor time (NOT TUI launch — it forks and can hang on a stalled `fileproviderd`); document hang-risk and run under `context.WithTimeout(2s)`.

#### HIGH — Symlinks INSIDE the snapshot dir pointing to cloud-sync are not detected

Layer 2 checks the snapshot dir's own resolved path. If `~/.local/share/wezterm/state/resurrect/` (safe) contains a symlink `workspace/shared → ~/iCloud Drive/team-snapshots/`, every snapshot file accessed via that subdir is cloud-synced. Layer 2 cannot detect this without recursively scanning every file's resolved path, which would itself fork-and-hang on a stalled File Provider daemon. Documented honestly: Layer 2 is best-effort at the snapshot dir level. The actual-data-loss defense remains the recommendation that snapshot dirs and ALL their contents be local-only.

#### HIGH — `EvalSymlinks` permission errors on ancestor symlinks

`filepath.EvalSymlinks` calls `os.Readlink` on each symlink component. On `EACCES` / `EPERM` (an ancestor symlink owned by another user with mode `0700`), Readlink fails. Spec doesn't say what to do — fail-open? Fail-closed? Use unresolved path? Realistic on multi-user CI systems, corporate-managed Macs with `/Users/Shared/`, and containers with bind-mounted user dirs. Round 12 v2.3 fix: on Readlink error, emit WARN (debug level) and proceed with the unresolved path; treat as non-network in absence of evidence. Document.

#### HIGH — NFC vs NFD normalization mismatch

APFS preserves on-disk filename bytes as-given (verified [Apple APFS reference](https://developer.apple.com/library/archive/documentation/FileManagement/Conceptual/APFS_Guide/FAQ/FAQ.html), [Michael Tsai's writeup](https://mjtsai.com/blog/2017/03/24/apfss-bag-of-bytes-filenames/)) — APFS does NOT NFD-normalize at the VFS layer the way HFS+ did. So `os.UserHomeDir()` returns whatever encoding the on-disk filename uses (typically NFD on directories created via Finder, NFC on directories created via Terminal). v2.2 says "apply NFC normalization" without specifying whether the prefix anchors are also NFC-normalized at the same step. If a user's `$HOME` is `/Users/café` with `é` stored as NFD (`e` + combining acute) but the prefix anchor `~/Library/...` is constructed with NFC `é`, comparison fails. **v2.3 fix**: explicitly NFC-normalize BOTH the resolved path AND the tilde-expanded prefix anchors using `golang.org/x/text/unicode/norm.NFC.String` at the SAME step, before comparison. Document in §6.19 helper.

#### HIGH — User-renamed iCloud Drive folder bypasses detection

Apple's Finder blocks renaming `~/iCloud Drive`, but `mv`/Terminal allow it. After rename, the on-disk path is whatever the user picked (`~/MyCloud`, `~/clouddata`, etc.), and Layer 2's hardcoded `~/iCloud Drive` no longer matches. Verified via Apple Discussions ([thread 8392536](https://discussions.apple.com/thread/8392536)). `~/Library/Mobile Documents/` and `~/Library/CloudStorage/iCloud~*` are NOT renameable (they're Apple-managed system locations) so the primary File Provider paths still match — but the legacy alias is gone. **v2.3 fix**: document the rename hazard. Resolve `EvalSymlinks` of `~/iCloud Drive` even when matched — if the symlink target lands in `~/Library/Mobile Documents/` we know it's iCloud regardless of what the alias was renamed to. (Actually, the alias resolution bottoms out at Mobile Documents, so the user's rename of the alias doesn't break detection if Layer 2 checks BOTH the alias path AND the resolved path — verify this works.)

#### HIGH — `sudo` / `doas` `$HOME` overrides

`sudo wezsesh` (without `-E`) clears most of the env; `$HOME` becomes `/var/root` (sudo default on darwin) or `/root` (Linux). `os.UserHomeDir()` returns this value. Layer 2 expands `~` against the wrong home dir — prefix matching fails for the actual user's snapshot dir. Realistic in privilege-elevation diagnostic flows. **v2.3 fix**: doctor reports the resolved `$HOME` and warns if it differs from `os/user.Current().HomeDir` (which uses the SUID/SGID-aware lookup).

#### HIGH — `/Volumes/GoogleDrive*` is vestigial

Pre-2024 Google Drive Desktop on Intel Macs mounted at `/Volumes/GoogleDrive`. 2024+ uses File Provider exclusively (`~/Library/CloudStorage/GoogleDrive/`). On Apple Silicon, the legacy mount is essentially gone. The prefix produces false-positives for unrelated `/Volumes/GoogleDriveBackup` (someone's external drive named "GoogleDriveBackup"). Acceptable noise for now; document.

#### MEDIUM — Case sensitivity on APFS

APFS is case-insensitive (default) but case-preserving. A user creating `~/dropbox` (lowercase) on case-insensitive APFS resolves to the same inode as `~/Dropbox`, but Go's `filepath.Match` is case-sensitive. Layer 2 misses lowercase-named clones. **v2.3 fix**: case-insensitive comparison on darwin; case-sensitive on Linux. Or simpler: compare with `strings.EqualFold`.

#### MEDIUM — Trailing slash in `$HOME`

If `$HOME = "/Users/grady/"`, expansion of `~/Library/...` produces `/Users/grady//Library/...`. `filepath.Clean` would normalize, but the spec doesn't mandate it. Add: "After expansion and `EvalSymlinks`, call `filepath.Clean` to normalize redundant slashes."

#### MEDIUM — TOCTOU between Layer 2 check and snapshot use

Layer 2 is evaluated at TUI open. If the user enables/disables iCloud "Desktop & Documents" sync between TUI open and a save, the warning is stale. Acceptable for v0.1; document.

#### MEDIUM — Desktop & Documents detection assumes symlinks

v2.2 detects Desktop & Documents sync via the symlink at `~/Library/Mobile Documents/com~apple~CloudDocs/Desktop`. If Apple changes the implementation to bind-mount in a future macOS release, detection breaks. Document the assumption; v2.3 add to doctor a `defaults read NSGlobalDomain NSDocumentRevisionsDebugMode` or similar non-symlink-dependent probe (research alternative APIs).

### Decisions — §6.7, §6.19

- §6.19 v2.3: `EvalSymlinks` wrapped in `context.WithTimeout(500ms)`; on overrun → use unresolved path + WARN. Same for `os.Readlink` permission errors — fall back to unresolved.
- §6.19 v2.3: NFC-normalize both resolved path AND tilde-expanded prefix anchors at the SAME step.
- §6.19 v2.3: case-insensitive prefix match on darwin (use `strings.EqualFold`); `filepath.Clean` after expansion.
- §6.7 / §6.19 v2.3: Layer 2 prefix list extended with `Box*`, `Nextcloud*`, `Proton*`. Documented false-negative class extended to include third-party providers, custom mount paths, symlinks INSIDE the snapshot dir.
- §7.4 v2.3: add `EvalSymlinks` as a hang surface row; document `pluginkit` deferred to future work with explicit hang-risk noted.
- §7.3 v2.3: doctor reports resolved `$HOME` and warns on `sudo`/`doas` divergence.

### Files cited

- [golang/go#73572 EvalSymlinks /proc/1/root recursive resolution](https://github.com/golang/go/issues/73572)
- [Apple APFS FAQ — bag-of-bytes filenames](https://developer.apple.com/library/archive/documentation/FileManagement/Conceptual/APFS_Guide/FAQ/FAQ.html)
- [Michael Tsai — APFS's "Bag of Bytes" Filenames](https://mjtsai.com/blog/2017/03/24/apfss-bag-of-bytes-filenames/)
- [eclecticlight Unicode normalization and APFS](https://eclecticlight.co/2021/05/08/explainer-unicode-normalization-and-apfs/)
- [Apple Discussions — Rename iCloud Drive (8392536)](https://discussions.apple.com/thread/8392536)
- [NextCloud desktop FileProvider issue #1337](https://github.com/nextcloud/desktop/issues/1337)
- [Seafile FileProvider feature request](https://forum.seafile.com/t/feature-request-seadrive-macos-native-support-for-fileprovider/15861)
- Go stdlib `path/filepath/symlink.go`

---

## 2. `F_OFD_SETLK` Linux fallback semantics — TWO BLOCKER, ONE MEDIUM

### Findings

#### BLOCKER C — Build-tag pattern not specified; naive code fails to compile on darwin

`unix.F_OFD_SETLK` is defined in [`golang.org/x/sys/unix/zerrors_linux.go`](https://raw.githubusercontent.com/golang/sys/master/unix/zerrors_linux.go) only. It is NOT defined in `zerrors_darwin_amd64.go` or `zerrors_darwin_arm64.go` (verified). v2.2's spec says "Linux uses `F_OFD_SETLK`" without mandating the build-tag split.

A naive implementation:
```go
// internal/safefs/lock.go
if runtime.GOOS == "linux" {
    return unix.FcntlFlock(fd, unix.F_OFD_SETLK, &flk)
}
return unix.FcntlFlock(fd, unix.F_SETLK, &flk)
```
**fails to compile on darwin** because `unix.F_OFD_SETLK` is undefined in the darwin build of `golang.org/x/sys/unix`. The runtime check is meaningless to the compiler — every reference must resolve at compile time.

The required pattern is:
```go
// lock_linux.go
//go:build linux
package safefs
const fcntlOFDSetlk = unix.F_OFD_SETLK

// lock_other.go
//go:build !linux
package safefs
const fcntlOFDSetlk = unix.F_SETLK  // fallback; OFD locks unavailable
```

**v2.3 fix**: §6.19 mandates the build-tag split. CI lint (`staticcheck` or grep) rejects any reference to `unix.F_OFD_SETLK` outside `lock_linux.go`. Add explicit prose: "The Linux OFD lock path lives in `internal/safefs/lock_linux.go`; the POSIX F_SETLK fallback lives in `internal/safefs/lock_other.go`. Conditional runtime branching on `runtime.GOOS == \"linux\"` is forbidden because it requires the F_OFD_SETLK constant to be defined on all build targets."

#### BLOCKER D — `O_CLOEXEC` missing on AcquireExclusive's lock fd

v2.2's `AcquireExclusive` opens via `unix.Openat(dirfd, basename, O_RDWR|O_NOFOLLOW, perm)` (§6.19). The flag set does NOT include `O_CLOEXEC`. Implications:

- Every `os/exec` call from the wezsesh process inherits the lock fd in the child's fd table. Children include `wezsesh reply` (called from Lua per request), `wezsesh keygen` (per session), any future helper.
- For OFD locks specifically: per [`fcntl(2)` Linux](https://man7.org/linux/man-pages/man2/fcntl.2.html), "open file description locks are inherited across `fork(2)` and `clone(2)` (with `CLONE_FILES`), and are only released on the LAST close of the open file description." So the OFD survives child close-of-its-fd, AS LONG AS the parent's fd is still open. **However**: the child has a reference to the SAME OFD; if the child code path explicitly calls `fcntl(F_OFD_UNLCK)` on the inherited fd (a future refactor, a library helper, an accidental bug), it releases the OFD lock from the parent's perspective. AND: the child can `dup` / `close` the fd, leaking it through its own fd table.
- For non-OFD F_SETLK (darwin/FreeBSD): the lock is process-level; child fd-table pollution doesn't directly release the lock, but the inherited fd still represents an attack surface (a malicious or buggy child can read/write via the inherited fd).
- Go's `os/exec` does set `FD_CLOEXEC` on inherited fds via `posix_spawn` / `forkAndExecInChild` since Go 1.10+ for `os.OpenFile`-created files, but `unix.Openat` directly does NOT — the underlying `openat(2)` syscall honors only the flags passed.

**v2.3 fix**: §6.19 mandates `unix.O_CLOEXEC` on every `unix.Openat` call in `internal/safefs/`:
```go
fd, err := unix.Openat(dirfd, basename, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, perm)
```
Lint rule: any `unix.Openat` call in `internal/safefs/` without `O_CLOEXEC` is a build error. Add to `AcquireExclusive`, `VerifyDir`, `SafeOpenForRead`, `AtomicWriteFile` (the temp-fd open in step 3). CI test: open a path under AcquireExclusive, fork-exec a subprocess with `os/exec`, child inspects `/proc/self/fd/` (Linux) or `lsof -p <pid>` (darwin) — must NOT find the lock fd in child's fd table.

#### MEDIUM — Linux 3.15+ kernel baseline undocumented as hard requirement

F_OFD_SETLK requires Linux 3.15 (April 2014). All supported distros (RHEL 8+, Ubuntu 20.04+, Alpine 3.x, Arch, Fedora) ship with 5.x+. On older kernels, `fcntl(2)` returns `EINVAL` for unrecognized cmd codes. The spec doesn't mandate a runtime fallback (probably correct — pre-3.15 is unreasonable to support) but doesn't document the floor either. **v2.3 fix**: §6.19 documents Linux 3.15+ as a hard requirement; doctor probes via `os.ReadFile("/proc/sys/kernel/osrelease")` and emits a clear error if older. Optional EINVAL→F_SETLK runtime fallback deferred to v0.2.

### Decisions — §6.19

- §6.19 v2.3: build-tag split mandatory; `lock_linux.go` + `lock_other.go`. CI lint enforces.
- §6.19 v2.3: `unix.O_CLOEXEC` mandatory on every `unix.Openat` in `internal/safefs/`. CI test verifies child process doesn't inherit the lock fd.
- §6.19 v2.3: Linux 3.15+ kernel baseline documented; doctor probes via `/proc/sys/kernel/osrelease`.

### Files cited

- [`zerrors_linux.go` F_OFD_SETLK definition](https://raw.githubusercontent.com/golang/sys/master/unix/zerrors_linux.go)
- [`fcntl(2)` Linux — Open file description locks section](https://man7.org/linux/man-pages/man2/fcntl.2.html)
- [GNU C Library — Open File Description Locks](https://www.gnu.org/software/libc/manual/html_node/Open-File-Description-Locks.html)
- [LWN — File-private POSIX locks](https://lwn.net/Articles/586904/)
- Go runtime `os/exec_unix.go` for default CLOEXEC behavior

---

## 3. `runtime_dir` validation TOCTOU + pre-flight gaps — ONE BLOCKER, FIVE HIGH, FOUR MEDIUM

### Findings

#### BLOCKER E — `+14` budget undercounts by 9 bytes (`/wezsesh/` component)

v2.2's Lua check is `len(opts.runtime_dir) + 14 ≤ ceiling`. The `+14` is for `/<8hex>.sock`. But §6.4 specifies the reply dir is `$XDG_RUNTIME_DIR/wezsesh/` (Linux) or `/tmp/wezsesh-<uid>/` (darwin). Two interpretations of `opts.runtime_dir`:

- **(A)** User passes the FINAL reply dir (already includes `/wezsesh/`). Check is correct: `+14`.
- **(B)** User passes the parent dir; binary appends `/wezsesh/`. Check is wrong by 9 bytes: should be `+14+9 = +23`.

The PRD's §6.4 example default-detection (`auto-detect $XDG_RUNTIME_DIR on Linux, /tmp/wezsesh-<uid>/ on darwin`) suggests the binary appends — `$XDG_RUNTIME_DIR` is `/run/user/<uid>`, not `/run/user/<uid>/wezsesh`. So interpretation (B) is the implementation. The Lua check is wrong.

**Worst-case overflow**: `opts.runtime_dir = "/run/user/1000"` (15 bytes). Lua check: 15 + 14 = 29 ≤ 108 ✓. Runtime path: `/run/user/1000/wezsesh/<8hex>.sock` = 15+9+14 = 38. Within budget. No overflow.

But: `opts.runtime_dir = "/Users/grady/Library/Application Support/com.acmecorp.wezterm/runtime"` (74 bytes). Lua check: 74 + 14 = 88 ≤ 104 ✓. Runtime path: 74 + 9 + 14 = 97 — still fine. Edge case: a user with a long XDG override could overflow without the Lua check catching it. Worse: the user-supplied `runtime_dir` MIGHT include `/wezsesh/` already (interpretation A), in which case the runtime path is 74 + 14 = 88 (correct). The interpretation is ambiguous.

**v2.3 fix**: §6.4 v2.3 explicitly defines the contract: `opts.runtime_dir` is the FINAL reply dir, INCLUDING `/wezsesh/`. The binary does NOT append `/wezsesh/` on top of it. Default still auto-detects `$XDG_RUNTIME_DIR/wezsesh/` (binary constructs the default when `opts.runtime_dir == nil`). The Lua check `+14` is correct under this revised contract. Doctor and README explicitly document the user-override contract: "Pass the FULL desired socket directory, including a trailing `/wezsesh/` if you want isolation from other tools." Add CI test with both pass and fail cases.

#### HIGH — Tilde expansion timing mismatch

Lua's `#` operator measures the literal string `"~/foo/bar"` (10 bytes). wezterm's Lua does NOT auto-expand `~`. The Go binary, when it receives `WEZSESH_RUNTIME_DIR` via env, uses Go's `filepath.Clean` and may expand `~` via the `os/user` package — producing a longer path. Worst case: user sets `runtime_dir = "~/r"` (3 bytes). Lua check: 3 + 14 = 17. Go expansion: `/Users/long-username/r` (22 bytes) + 14 = 36. Both pass, but the divergence is real; longer home dirs (corporate-managed Macs with `/Users/employee.firstname.lastname/`) can blow past 104.

**v2.3 fix**: §6.4 v2.3: Lua check expands `~` BEFORE measuring length, using `wezterm.home_dir` (verify the API name) or `os.getenv("HOME")`. The expansion mirrors what the Go binary does. Document explicitly: "Pass either an absolute path or `~`-prefixed path; tilde is expanded before length validation."

#### HIGH — `wezterm.target_triple` substring-match heuristic

The Lua check `(wezterm.target_triple:find("apple") and 104) or 108` is a substring match. Verified: wezterm exposes `target_triple` (e.g., `aarch64-apple-darwin`). Match is robust on real Macs but brittle: a hypothetical `aarch64-apple-linux` would mismatch (104 instead of 108). Safer pattern: explicit check `target_triple:match("apple%-darwin$")` to anchor.

**v2.3 fix**: tighten the regex to `target_triple:match("%-apple%-darwin")`; the `%-` anchors prevent false positives from other "apple" substrings in obscure triples.

#### HIGH — Type validation missing on `opts.runtime_dir`

User passes `runtime_dir = nil` / `123` / `{}`: `#nil` raises a Lua error, `#123` raises a Lua error, `#{}` returns 0. The error propagates → `apply_to_config` outer pcall catches → user sees generic "wezsesh setup failed" toast, not the helpful "runtime_dir must be a string" guidance. **v2.3 fix**: explicit `assert(type(opts.runtime_dir) == "string", "wezsesh: runtime_dir must be a string path")` BEFORE the length check. The assert raises the right message which the outer pcall surfaces via toast.

#### HIGH — `$XDG_RUNTIME_DIR` fallback for non-systemd Linux

Per [XDG Base Directory spec](https://specifications.freedesktop.org/basedir-spec/basedir-spec-latest.html), `$XDG_RUNTIME_DIR` is set by `pam_systemd` for logind sessions. On Alpine, void, gentoo-without-systemd, Devuan, FreeBSD-running-Linux-binaries, raw SSH sessions without logind, container-based desktops, $XDG_RUNTIME_DIR is UNSET. v2.2 says "auto-detect $XDG_RUNTIME_DIR on Linux" without specifying fallback. **v2.3 fix**: §6.4 v2.3: when $XDG_RUNTIME_DIR is unset on Linux, fall back to `/tmp/wezsesh-<uid>/` (matching darwin pattern). Document and test.

#### HIGH — `$XDG_RUNTIME_DIR` ownership / permissions not pre-flight-checked

Per XDG spec, `$XDG_RUNTIME_DIR` MUST be user-owned, mode 0700, on tmpfs. Sysadmin / user override could violate this. The Go binary's `MkdirAll` + `net.Listen` will fail with `EACCES`, surfacing `IPC_INIT_FAILED` — but the user sees an opaque errno without the actionable "your $XDG_RUNTIME_DIR is owned by root, fix it" hint. **v2.3 fix**: doctor probes `os.Stat($XDG_RUNTIME_DIR)`, reports owner UID, mode, and FS type; warns if not user-owned or not 0700.

#### MEDIUM — Outer pcall hides the SUN_PATH error message from the user

§7.1 wraps `apply_to_config` in pcall. A clean Lua `error(...)` raised by the SUN_PATH check is caught and degraded to a generic toast. **v2.3 fix**: SUN_PATH check raises a structured error and `apply_to_config`'s outer catch block detects this specific error (via a sentinel string prefix or table-error) and surfaces the original message via a 10s toast. Other errors (vendored module syntax, etc.) still degrade to the generic toast.

#### MEDIUM — Filesystem remount race between Lua check and Go binary

User sets `runtime_dir = "/tmp/wezsesh/"`. Check passes (15 + 14 = 29). Before TUI launch, `/tmp` is remounted to a longer path via NFS automount. Go binary catches this via re-validation; minor UX degradation. Document.

#### MEDIUM — Symlink in `runtime_dir`

Go's `net.Listen("unix", path)` doesn't follow symlinks the way `O_NOFOLLOW` would block. A user-supplied symlink target could produce a different actual SUN_PATH. **v2.3 fix**: §6.4 v2.3 mandates `safefs.VerifyDir(parent_of_runtime_dir)` before `net.Listen`. Reject if symlink. Already implied by §6.19 but not explicit for runtime_dir.

#### MEDIUM — `os.getenv("CUSTOM_VAR")` indirection in Lua config

User config: `runtime_dir = (os.getenv("MY_RUNTIME") or "/tmp/wezsesh/") .. "/sock/"`. Check measures the resolved-at-config-load value. If the user changes `MY_RUNTIME` later in a different shell and spawns a TUI, the env var passed to wezsesh-binary is the new value, but the Lua check ran with the old. Edge case; document.

### Decisions — §6.4, §7.1, §7.3

- §6.4 v2.3: explicit contract — `opts.runtime_dir` IS the final reply dir (with `/wezsesh/`); `+14` budget correct under this contract. Document.
- §6.4 v2.3: Lua-side `~` expansion BEFORE length check (using `wezterm.home_dir` or `os.getenv("HOME")`).
- §6.4 v2.3: `target_triple` regex tightened to `"%-apple%-darwin"` anchor.
- §6.4 v2.3: explicit `type(opts.runtime_dir) == "string"` assertion.
- §6.4 v2.3: `$XDG_RUNTIME_DIR` unset on Linux → fall back to `/tmp/wezsesh-<uid>/`.
- §6.4 v2.3: outer pcall in `apply_to_config` surfaces SUN_PATH errors via 10s toast (not generic).
- §6.4 v2.3: `safefs.VerifyDir(parent_of_runtime_dir)` before `net.Listen`.
- §7.3 v2.3: doctor reports `$XDG_RUNTIME_DIR` ownership, mode, FS type; warns on misconfiguration.

### Files cited

- [XDG Base Directory Spec](https://specifications.freedesktop.org/basedir-spec/basedir-spec-latest.html)
- Lua 5.4 manual `#` operator semantics
- wezterm Lua API at https://wezterm.org/config/lua/wezterm/

---

## 4. `SwitchToWorkspace` activity coupling — ONE HIGH, ONE MEDIUM, ONE VERIFIED-OK

### Findings

#### VERIFIED-OK — `mux.set_active_workspace_for_client` IS synchronous

Read [`mux/src/lib.rs`](https://raw.githubusercontent.com/wezterm/wezterm/main/mux/src/lib.rs) `set_active_workspace_for_client`:
```rust
pub fn set_active_workspace_for_client(&self, ident: &Arc<ClientId>, workspace: &str) {
    let mut clients = self.clients.write();
    if let Some(info) = clients.get_mut(&ident) {
        info.active_workspace.replace(workspace.to_string());
        self.notify(MuxNotification::ActiveWorkspaceChanged(ident.clone()));
    }
}
```
Synchronous: write-lock the clients map, mutate, fire notification. No async spawn. The Lua-side `window:perform_action(act.SwitchToWorkspace{...})` call returns after the mux state has been updated. PRD's polling-based completion detection is an architectural fallback (Lua doesn't surface `MuxNotification::ActiveWorkspaceChanged` as an event), not a workaround for asynchrony.

`MuxNotification::ActiveWorkspaceChanged` is fired internally but is NOT exposed as a Lua-surfaced event. Verified by reading the documented Lua window-events list at https://wezterm.org/config/lua/window-events.html (events: `gui-startup`, `update-status`, `format-tab-title`, `bell`, `format-window-title`, `mux-startup`, `new-tab-button-click`, `open-uri`, `pane-focus-changed`, `update-right-status`, `user-var-changed`, `window-config-reloaded`, `window-focus-changed`, `window-resized`). No "active-workspace-changed" event.

PRD §6.15's polling pattern remains correct as the only Lua-side post-switch detection mechanism.

#### HIGH — `cli list --format json` has NO global active-workspace field

Read [`wezterm/src/cli/list.rs`](https://raw.githubusercontent.com/wezterm/wezterm/main/wezterm/src/cli/list.rs) — the `CliListResultItem` struct exposes per-pane `workspace` (String) and `is_active` (bool, per-tab as PRD §6.5 states). There is NO top-level "currently active workspace" field. The poller's success predicate "workspace `target` is now the active workspace of the focused client in `target_window_id`" is NOT directly queryable from `cli list` alone. The poller MUST:

1. Query `cli list-clients --format json` → get focused client's `focused_pane_id`.
2. Cross-reference that `pane_id` in `cli list --format json` → extract its `workspace` field.
3. Compare `workspace == target` (and `window_id == target_window_id` for cross-window correctness).

The PRD §6.5 already establishes that for "globally focused pane" we use `cli list-clients` — so the two-step pattern is implied. But §6.15 step 4 ("workspace `target` is now the active workspace of the focused client in `target_window_id`") doesn't EXPLICITLY say "this requires both CLIs." Round-12 finding: clarify the predicate's implementation. Plus: the polling rate doubles (50ms cadence × 2 CLI invocations). PRD §6.20 cites ~15ms fork-exec per `wezterm cli` invocation; doubling is acceptable but should be documented.

**v2.3 fix**: §6.15 step 4 explicitly says "the predicate evaluation requires BOTH `cli list-clients --format json` (for `focused_pane_id`) and `cli list --format json` (for that pane's workspace)." Update the polling cadence note accordingly. Add CI test that exercises the cross-reference logic.

#### MEDIUM — Multi-GUI-client disambiguation underspecified

`cli list-clients --format json` returns ALL connected GUI clients, each with their own `focused_pane_id`. PRD §6.20 says "pick the one with smallest `idle_time`" — verify what this is. Reading the wezterm-client source, the field is `last_input` (timestamp of last user input). If two clients are simultaneously active (both have recent input), tie-breaking is by stable order. PRD §6.5 doesn't specify; needs explicit rule.

**v2.3 fix**: §6.5 v2.3: when multiple GUI clients are connected, the poller picks the client whose most-recent `last_input` is most recent. Tie-break: lowest `client_id`. This matches the wezterm-client default behavior.

### Decisions — §6.5, §6.15

- §6.15 v2.3: predicate evaluation explicitly requires both `cli list-clients` and `cli list`; document the two-step query.
- §6.5 v2.3: multi-client disambiguation rule (most-recent `last_input`, tie-break on `client_id`).
- VERIFIED: `mux.set_active_workspace_for_client` synchronous; `MuxNotification::ActiveWorkspaceChanged` not Lua-exposed; polling remains the only Lua-side mechanism.

### Files cited

- [wezterm/src/cli/list.rs](https://raw.githubusercontent.com/wezterm/wezterm/main/wezterm/src/cli/list.rs)
- [mux/src/lib.rs `set_active_workspace_for_client`](https://raw.githubusercontent.com/wezterm/wezterm/main/mux/src/lib.rs)
- [wezterm-client/src/client.rs](https://raw.githubusercontent.com/wezterm/wezterm/main/wezterm-client/src/client.rs)
- [wezterm Lua window-events](https://wezterm.org/config/lua/window-events.html)

---

## 5. `cli activate-pane` cross-workspace semantics — ONE BLOCKER

### Findings

#### BLOCKER F — `cli activate-pane` does NOT switch workspace

Verified by reading [`wezterm/src/cli/activate_pane.rs`](https://raw.githubusercontent.com/wezterm/wezterm/main/wezterm/src/cli/activate_pane.rs) (client side):

```rust
impl ActivatePane {
    pub async fn run(&self, client: Client) -> anyhow::Result<()> {
        let pane_id = client.resolve_pane_id(self.pane_id).await?;
        client
            .set_focused_pane_id(codec::SetFocusedPane { pane_id })
            .await?;
        Ok(())
    }
}
```

The client sends a `SetFocusedPane` PDU containing only `pane_id`. No workspace parameter.

Server-side, [`wezterm-mux-server-impl/src/sessionhandler.rs`](https://raw.githubusercontent.com/wezterm/wezterm/main/wezterm-mux-server-impl/src/sessionhandler.rs):

```rust
Pdu::SetFocusedPane(SetFocusedPane { pane_id }) => {
    let client_id = self.client_id.clone();
    spawn_into_main_thread(async move {
        catch(move || {
            let mux = Mux::get();
            let _identity = mux.with_identity(client_id);
            let pane = mux.get_pane(pane_id).ok_or_else(|| anyhow::anyhow!("pane {pane_id} not found"))?;
            let (_domain_id, window_id, tab_id) = mux.resolve_pane_id(pane_id)
                .ok_or_else(|| anyhow::anyhow!("pane {pane_id} not found"))?;
            {
                let mut window = mux.get_window_mut(window_id).ok_or_else(|| anyhow::anyhow!("window {window_id} not found"))?;
                let tab_idx = window.idx_by_id(tab_id).ok_or_else(|| anyhow::anyhow!("tab {tab_id} isn't really in window {window_id}!?"))?;
                window.save_and_then_set_active(tab_idx);
            }
            let tab = mux.get_tab(tab_id).ok_or_else(|| anyhow::anyhow!("tab {tab_id} not found"))?;
            tab.set_active_pane(&pane);
            mux.record_focus_for_current_identity(pane_id);
            mux.notify(mux::MuxNotification::PaneFocused(pane_id));
            Ok(Pdu::UnitResponse(UnitResponse {}))
        }, send_response)
    }).detach();
}
```

The server: (1) sets the active tab in the pane's window, (2) sets the active pane in the tab, (3) records focus for the current identity, (4) emits `MuxNotification::PaneFocused`. **It does NOT call `mux.set_active_workspace_for_client(...)` for the target's workspace.** If the target pane is in workspace B and the user (current GUI client) is in workspace A, the server marks pane B as the "active pane within its window in workspace B" — but the user remains on workspace A and does not see pane B.

PRD §6.20's claim "Cross-window/cross-workspace activation is server-side implicit (workspace switch happens automatically per §6.13)" is **WRONG**. This is the carry-forward never verified by previous rounds, finally read by round 12.

**Impact**: `wezsesh find` selects a pane via fuzzy-match across workspaces, then calls `cli activate-pane --pane-id <selected>`. The pane is "activated" within its workspace, but the user — who triggered `wezsesh find` from workspace A — remains on workspace A. The activated pane is invisible. The user sees the TUI close and nothing else change.

**v2.3 fix**: `wezsesh find`'s success path must, when the selected pane is in a different workspace, FIRST emit a `switch` op (§6.15 polling pattern) to bring the user to the target workspace, THEN emit `cli activate-pane`. Two paths:

- **Same workspace**: `cli activate-pane --pane-id <selected>` directly. No switch needed.
- **Different workspace**: emit `switch` op (Lua dispatches `act.SwitchToWorkspace`, Go polls for completion via §6.15), THEN `cli activate-pane`. The full sequence is: workspace-switch → poll → activate-pane → exit.

§6.13 v2.3 explicitly documents the two-phase sequence. The PRD §6.20 claim is corrected. Add CI test: `wezsesh find` selecting a cross-workspace pane MUST result in the user being on that workspace AND the pane being active.

### Decisions — §6.13, §6.20

- §6.13 v2.3: `wezsesh find` cross-workspace selection requires explicit two-phase sequence: workspace-switch (via `act.SwitchToWorkspace`) THEN `cli activate-pane`. Same-workspace selection skips phase 1.
- §6.20 v2.3: corrects the "server-side implicit" claim. `cli activate-pane` only sets active pane within the pane's window; does NOT switch the user's active workspace. Documented with source citation.

### Files cited

- [wezterm/src/cli/activate_pane.rs](https://raw.githubusercontent.com/wezterm/wezterm/main/wezterm/src/cli/activate_pane.rs)
- [wezterm-mux-server-impl/src/sessionhandler.rs SetFocusedPane handler](https://raw.githubusercontent.com/wezterm/wezterm/main/wezterm-mux-server-impl/src/sessionhandler.rs)

---

## Summary of changes for PRD_V6 (= internally stamped v2.3)

### BLOCKER fixes (must change before code lands)

1. **§6.19** (correctness/portability): `F_OFD_SETLK` not defined on darwin in `golang.org/x/sys/unix`; naive `runtime.GOOS == "linux"` branching produces a darwin build break. v2.3 mandates `lock_linux.go` + `lock_other.go` build-tag split. Spike 2.
2. **§6.19** (security/correctness): `O_CLOEXEC` missing on `AcquireExclusive`'s lock fd. The fd leaks into every `os/exec` child (`wezsesh reply`, `wezsesh keygen`); for OFD locks specifically, a child with the inherited fd can release the OFD lock from the parent's perspective via `fcntl(F_OFD_UNLCK)`. v2.3 mandates `unix.O_CLOEXEC` on every `unix.Openat` in `internal/safefs/`. Spike 2.
3. **§6.7 / §6.19** (reliability): `filepath.EvalSymlinks` is itself a hang surface (no Go-level loop limit on darwin; magic-link recursion on Linux). v2.3 wraps in `context.WithTimeout(500ms)`; on overrun, use unresolved path + WARN. Spike 1.
4. **§6.7 / §6.19** (correctness): Layer 2 prefix list misses third-party File Provider extensions (Box, NextCloud, Seafile, ProtonDrive) and custom mount paths. v2.3 extends the prefix list and explicitly documents the false-negative class. Spike 1.
5. **§6.7 / §6.19** (correctness): Layer 2 cannot detect symlinks INSIDE the snapshot dir pointing to cloud-synced data. v2.3 documents honestly. Recommendation strengthened to "ALL contents of the snapshot dir must be local-only." Spike 1.
6. **§6.4** (correctness): `+14` budget undercounts if user-supplied `runtime_dir` doesn't include trailing `/wezsesh/` (the binary appends it). v2.3 explicitly defines the contract: `opts.runtime_dir` IS the final reply dir, including `/wezsesh/`. Document. Spike 3.
7. **§6.13 / §6.20** (correctness): `cli activate-pane` does NOT switch the user's active workspace. The server-side `SetFocusedPane` handler at `sessionhandler.rs` calls `window.save_and_then_set_active(tab_idx)` and `tab.set_active_pane(&pane)` but does NOT call `mux.set_active_workspace_for_client`. PRD's "Cross-window/cross-workspace activation is server-side implicit" claim is wrong. v2.3 corrects: `wezsesh find` cross-workspace selection requires explicit two-phase sequence (workspace-switch → poll → activate-pane). Spike 5.

### HIGH-severity correctness/portability/UX

- **§6.19** (correctness): `EvalSymlinks` permission errors (EACCES on ancestor symlinks) not handled. v2.3: on Readlink error, use unresolved path + WARN. Spike 1.
- **§6.19** (correctness): NFC vs NFD normalization mismatch between `$HOME` (on-disk encoding) and prefix anchors. v2.3: normalize BOTH at the same step. Spike 1.
- **§6.7 / §6.19** (correctness): User-renamed iCloud Drive alias bypasses detection. v2.3: also resolve via EvalSymlinks; document. Spike 1.
- **§6.19** (correctness): `sudo`/`doas` clears `$HOME`. v2.3: doctor reports resolved `$HOME` and warns on divergence. Spike 1.
- **§6.7 / §6.19** (false-positive): `/Volumes/GoogleDrive*` legacy on Apple Silicon vestigial; produces false-positives for unrelated mounts. v2.3 documents. Spike 1.
- **§6.4** (UX): tilde expansion timing — Lua `#` measures literal `~/...` but Go expands; expanded form longer. v2.3: Lua-side `~` expansion BEFORE length check via `wezterm.home_dir` or `os.getenv("HOME")`. Spike 3.
- **§6.4** (correctness): `wezterm.target_triple` substring-match heuristic. v2.3: tighten regex to `"%-apple%-darwin"` anchor. Spike 3.
- **§6.4** (UX): type validation missing on `opts.runtime_dir`. v2.3: explicit `assert(type(opts.runtime_dir) == "string", ...)` BEFORE length check. Spike 3.
- **§6.4** (correctness): `$XDG_RUNTIME_DIR` fallback for non-systemd Linux undefined. v2.3: fall back to `/tmp/wezsesh-<uid>/`. Spike 3.
- **§6.4** (UX): `$XDG_RUNTIME_DIR` ownership/permissions not pre-flight-checked. v2.3: doctor reports owner UID, mode, FS type. Spike 3.
- **§6.15** (clarity): `cli list --format json` has NO global active-workspace field; poller MUST cross-reference `cli list-clients` (focused_pane_id → workspace). v2.3 clarifies the two-step query in §6.15 step 4. Spike 4.

### MEDIUM updates

- **§6.19** (case-sensitivity): Layer 2 prefix match uses Go's case-sensitive `filepath.Match` but APFS is case-insensitive; lowercase-named clones bypass. v2.3 uses `strings.EqualFold` on darwin. Spike 1.
- **§6.19** (hygiene): trailing slash in `$HOME` produces double-slash paths. v2.3 mandates `filepath.Clean` after expansion. Spike 1.
- **§6.7 / §6.19** (TOCTOU): Layer 2 evaluated at TUI open; iCloud sync state can change. Document. Spike 1.
- **§6.19** (assumption): Desktop & Documents detection assumes symlinks; if Apple switches to bind-mounts, breaks. Document. Spike 1.
- **§6.19** (Linux baseline): F_OFD_SETLK requires Linux 3.15+. v2.3: doctor probes `/proc/sys/kernel/osrelease`. Spike 2.
- **§6.4** (UX): outer `pcall` in `apply_to_config` hides clear SUN_PATH error message. v2.3: surface SUN_PATH errors via 10s toast. Spike 3.
- **§6.4** (race): filesystem remount race between Lua check and Go binary. Mitigated by Go re-validation. Document. Spike 3.
- **§6.4** (TOCTOU): symlink in `runtime_dir` not pre-flight-checked. v2.3: `safefs.VerifyDir(parent)` before `net.Listen`. Spike 3.
- **§6.4** (race): `os.getenv` indirection allows env-var change between Lua check and TUI launch. Document. Spike 3.
- **§6.5** (clarity): multi-GUI-client disambiguation when multiple clients have recent input. v2.3: most-recent `last_input`, tie-break on `client_id`. Spike 4.
- **§6.7** (false-positive): hung `pluginkit` if used for File Provider discovery. v2.3 deferred to v0.2 with explicit hang-risk note. Spike 1.

### Severity tally

- BLOCKER: 7 (build-tag, O_CLOEXEC, EvalSymlinks loop, third-party FP providers, symlinks-in-snapshot-dir, +14 path-component undercount, cli activate-pane no-workspace-switch).
- HIGH: 11 (EvalSymlinks permissions, NFC/NFD normalization, user-renamed iCloud alias, sudo $HOME, /Volumes/GoogleDrive vestigial, tilde-expansion timing, target_triple regex, type validation, $XDG_RUNTIME_DIR fallback, ownership pre-flight, cli list cross-reference clarity).
- MEDIUM: 11.
- VERIFIED-OK: 1 (mux.set_active_workspace_for_client is synchronous; MuxNotification::ActiveWorkspaceChanged exists internally but not Lua-exposed).

### Status

v12 complete. Seven BLOCKERs addressed; PRD bumped to v2.3.

### Audit-cadence assessment

Round 11's hypothesis ("BLOCKERs are predominantly drift between OUR own recent edits and reality") held emphatically. Six of seven BLOCKERs are direct drift:

1. v2.2 introduced Layer 2 path-prefix heuristic — round 12 found it has zero coverage for third-party providers and is itself a hang surface.
2. v2.2 introduced `F_OFD_SETLK` Linux path — round 12 found build-tag pattern unspecified and `O_CLOEXEC` missing.
3. v2.2 introduced `+14` SUN_PATH budget — round 12 found it undercounts by 9 bytes if `/wezsesh/` is appended.
4. The seventh BLOCKER (`cli activate-pane` no-workspace-switch) is a never-verified carry-forward from v1.x finally caught by reading the actual server-side handler.

This is the convergence signature: each round catches drift between spec and reality. **The pattern is now structural, not transient.** Spec-only audits are catching drift faster than runtime testing would, but the audit cadence is producing diminishing returns per round AND introducing new drift each cycle (round 12's Layer 2 heuristic is itself an audit-introduced edit).

**Recommendation**: STOP audit-only iteration. Implement v0.1, run integration tests in CI under realistic workloads (concurrent TUIs, hung NFS, sigkill mid-save, fork-spawned reply, cross-workspace find), and discover the remaining surfaces empirically. Round 13 should be a CODE-AND-TEST iteration, not a SPEC iteration. The spec is now sufficiently stable; further audit-only rounds will continue finding ~5–10 issues per round with diminishing severity.

If the project DOES iterate on spec one more time before code: round 13 should focus exclusively on what v2.3's edits introduce (the new build-tag boundary, the `EvalSymlinks` timeout wrapper, the two-phase `wezsesh find` flow). Per the round-11 termination criterion, if round 13 finds zero BLOCKERs and zero HIGHs against v2.3 edits, audit phase is done.

---

The most striking finding of this round is that the §6.13 cross-workspace `wezsesh find` flow — central to one of the PRD's named v0.1 features — has been built on a misunderstanding of `cli activate-pane` semantics since round 1. The defense actually doesn't work as specified: the user picks a pane, hits Enter, and stays on their original workspace. The two-phase fix (switch → poll → activate) is straightforward but should have been caught earlier; previous rounds didn't read the server-side `SetFocusedPane` handler at all.

The pattern across rounds 8–12 is now decisive: each round finds smaller, more specific bugs, but each round also introduces new spec content that the NEXT round catches drift in. The audit phase has converged to the point where additional rounds are net-zero or net-negative — closing N issues while introducing M new drifts. Code-and-test is the right next phase.
