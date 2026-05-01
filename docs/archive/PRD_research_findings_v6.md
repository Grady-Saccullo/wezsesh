# PRD Research Findings — Round 6 (RCE / filesystem / hook / lifecycle audit)

Goal of this round: hunt for security / architecture / process missteps that survived rounds 1–5. Four parallel research spikes targeting areas not yet examined: snapshot file as RCE vector, filesystem TOCTOU and symlinks on our owned writes, hook execution path safety, and plugin lifecycle edges (first-run, update, uninstall, error surfaces).

**Three BLOCKERs found**, each a real security bug. Plus a substantial lifecycle gap list that addresses the "first-month bug reports" surface.

---

## 1. Snapshot `process.argv` is an RCE vector — wezsesh-level allowlist required

### Findings

Source: `MLFlexer/resurrect.wezterm/plugin/resurrect/tab_state.lua` (recent main).

The function `pub.default_on_pane_restore` at line ~127:
```lua
if pane_tree.alt_screen_active then
    pane:send_text(wezterm.shell_join_args(pane_tree.process.argv) .. "\r\n")
end
```

`pane:send_text` writes directly into the pane's PTY input buffer — **equivalent to the user typing the command and pressing Enter**. There is no validation, no allowlist, no trust check anywhere in this path. The trigger is `alt_screen_active = true` in the snapshot — true for any pane that had an editor / pager / TUI app running at save time.

An attacker who places a crafted snapshot file in the user's resurrect snapshot directory (with `pane_tree.alt_screen_active = true` and `pane_tree.process.argv = ["rm", "-rf", "~"]`) achieves RCE on next restore. Threat surfaces:
- Dotfiles sync of resurrect's snapshot directory.
- Same-UID trap (npm postinstall, downloaded binary, MCP server) writing into `~/.local/share/wezterm/state/resurrect/workspace/`.
- Shared / multi-user machine with loose permissions.
- Snapshot received from another user (deliberate sharing).

**§6.11 hook trust covers `on_create`/`on_restore` only.** The argv vector is independent and was uncovered.

### Comparison with prior art

- **tmux-resurrect** ships an allowlist by default (`@resurrect-processes`); `:all:` is opt-in. Industry precedent for this exact problem.
- **direnv `.envrc`**: hash-based fail-closed (the §6.11 model). Right for one-shot user-authored commands; wrong for per-pane argv (per-pane prompts unusable).
- **Vim sessions**: known RCE class; mitigated via `'secure'` mode.
- **Shell history**: never auto-executes; only displays.

### Decision (BLOCKER) — §6.18 added

Adopt tmux-resurrect's allowlist model. wezsesh installs a custom `on_pane_restore` callback over resurrect's default. Behavior:
1. Match `basename(pane_tree.process.argv[1])` against allowlist.
2. **Match** → call resurrect's default (full argv send_text).
3. **No match** → `pane:send_text("cd " .. shell_quote(cwd) .. "\r\n")` (clean shell at right cwd) + WARN log.
4. **Empty argv / no `process` field** → no-op.

Default allowlist: `$SHELL`, `sh`/`bash`/`zsh`/`fish`/`dash`/`ksh`, `nvim`/`vim`/`vi`/`emacs`/`nano`/`helix`/`hx`, `less`/`more`/`man`/`info`, `git`/`jj`/`lazygit`/`tig`, `python`/`python3`/`ipython`/`node`/`ruby`/`irb`/`lua`, `htop`/`btop`/`top`/`k9s`/`lazydocker`.

User extension via `resurrect_argv_allowlist` config (additions only — defaults can't be disabled). Per-snapshot opt-out is NOT offered (would weaken the defense without meaningful UX gain).

`wezsesh doctor` audits all snapshots and reports unique `argv[0]` not in the active allowlist.

### Files cited
- `plugin/resurrect/tab_state.lua:127` (the `pane:send_text(shell_join_args(argv))` call)

---

## 2. Filesystem safety — symlink hijack on owned writes

### Findings

Audit of every file-write site in wezsesh:

| Path | Default Go behavior |
|---|---|
| `state.json` (state dir) | `os.OpenFile(path, O_WRONLY\|O_CREATE\|O_TRUNC, 0600)` follows symlinks |
| `*.wezsesh.json` sidecars | Same. Plus: Lua's `io.open` has no `O_NOFOLLOW` equivalent. |
| `<sha256>` trust files | Same. Symlink to anywhere clobbers target. |
| Reply socket (`<8-hex>.sock`) | `MkdirAll` does NOT chmod existing dirs. Pre-existing `/tmp/wezsesh-<uid>/` with mode 0755 stays 0755. |
| Log file | `lumberjack` opens with `O_APPEND`, follows symlinks. |

A same-UID attacker (npm postinstall, downloaded binary, MCP server) can pre-place symlinks under predictable paths. Standard Unix multiuser threat is out of scope (already game-over), but less-privileged-process-under-user is realistic.

### Decision (BLOCKER) — §6.19 added; `internal/safefs/` package

```go
// AtomicWriteFile — O_NOFOLLOW + openat(2) + Renameat(2). Mandatory for
// state.json, sidecars, trust files. Lint rule: os.WriteFile/os.OpenFile
// forbidden in internal/snapshots, internal/state, internal/trust.
func AtomicWriteFile(parentDir, filename string, data []byte, perm fs.FileMode) error

// VerifyDir — opens parentDir with O_DIRECTORY|O_NOFOLLOW; errors if symlink.
func VerifyDir(parentDir string) (fd int, info fs.FileInfo, err error)

// SafeOpenForRead — O_NOFOLLOW on read. Defends snapshot reads from symlink hijack.
func SafeOpenForRead(path string) (*os.File, error)
```

Implementation uses `golang.org/x/sys/unix` (Openat, Renameat, O_NOFOLLOW). Standard library `os` doesn't expose these.

**Critical constraint**: sidecar writes are **Go-only**. Lua dispatches tag/pin updates via IPC; Go performs the file write through `safefs.AtomicWriteFile`. Lua's `io.open` is unsafe for this purpose.

**Reply socket setup**:
1. `MkdirAll(replyDir, 0700)`.
2. `os.Lstat(replyDir)` — error if symlink or non-dir.
3. `os.Chmod(replyDir, 0700)` — in case it pre-existed loose.
4. `oldUmask := unix.Umask(0077)` before `net.Listen`; restore after. Sock is born `0600`.

**Log open**: `os.Lstat` before each rotation; if symlink, refuse to rotate and log to stderr instead.

**Trust file race is benign**: content-addressed by hash. Swapping file contents gains nothing — hash is recomputed from in-memory `(path, command)` at restore time and compared against the filename. On-disk contents are read only by `--list` for display; mismatch is cosmetic.

### Files cited
- Go stdlib: `syscall.O_NOFOLLOW` (defined Linux + darwin)
- `golang.org/x/sys/unix.Openat`, `Renameat`, `Umask`
- `os.OpenFile`, `os.Lstat`, `os.Rename` semantics

---

## 3. Hook execution path — eight specific gaps

### Findings

§6.11's previous spec was high-level ("`$SHELL -c '<command>'`, output to stderr, run after the workspace op"). Eight concrete gaps:

1. **TOCTOU between hash-check and exec.** Sequence: read sidecar → hash check → exec. If a re-read happens between check and exec, an attacker swaps the sidecar in the microseconds gap. **Fix**: read once, hash and exec from the same in-memory `command_bytes`.

2. **`$SHELL` empty / invalid.** `exec.LookPath("")` returns `ErrNotFound`; user sees silent fail. **Fix**: fallback to `/bin/sh`, log WARN.

3. **Stale `cwd` for `on_restore`.** First-pane cwd from snapshot may be deleted. **Fix**: `os.Stat` first; if missing, `$HOME` fallback + WARN.

4. **Stdin inherited.** Hook reading stdin blocks forever on the dead terminal. **Fix**: `Cmd.Stdin = nil` (Go maps to `/dev/null`).

5. **No timeout.** Hung hook (`sleep infinity`, network call) blocks binary exit. **Fix**: `context.WithTimeout(10 * time.Minute)`; SIGTERM the group, wait 5s, SIGKILL. Configurable via `wezsesh.hooks.timeout_seconds`.

6. **Children survive parent.** Default Go exec doesn't create a process group; `make dev`-spawned grandchildren orphan on timeout. **Fix**: `Cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}`. Kill via `syscall.Kill(-cmd.Process.Pid, SIGTERM)`.

7. **HMAC key leaks.** Hook inherits `WEZSESH_HMAC_KEY` via env. `printenv > /tmp/leak` exfiltrates it. **Fix**: `Cmd.Env = filtered(os.Environ())` removing all `WEZSESH_*` vars. Defense-in-depth (the hook is user-trusted, but secrets shouldn't propagate).

8. **Symlink trust bypass.** `os.Stat` on the trust file follows symlinks. Attacker symlinks `<sha256>` to any existing file → trust check passes for unapproved command. Same for the trust *directory*. **Fix**: `os.Lstat` (not `os.Stat`); reject `ModeSymlink`. At startup, `Lstat` the trust directory; abort hard if symlink.

### Decision (BLOCKER) — §6.11 expanded

§6.11 "Hook execution environment" rewritten with all eight contracts as numbered list items. Implementable directly without further security debate.

### Files cited
- Go stdlib `os/exec`, `syscall` package docs
- direnv as prior art for `.envrc`-style trust

---

## 4. Plugin lifecycle — eight gaps; one critical, several "will surface in week 1"

### Findings

| # | Gap | Severity |
|---|---|---|
| 1a | Binary-not-found shows "version mismatch" toast (`detect_binary_version` returns nil for both) | First-run UX broken |
| 1b | Listener invoked with `wezsesh_session_key == nil` (after keygen failure) | Undefined behavior |
| 1c | `apply_to_config` body NOT `pcall`-wrapped — vendored module syntax error breaks user's wezterm config | Critical reliability |
| 1d | `wezterm.plugin.require()` itself fails on network/syntax error — outside our control, document only | Docs |
| 2a | `wezterm.GLOBAL` survives reload but has no schema version stamp — old version's state shape may break new code after `update_all()` | Update reliability |
| 2c/2d | No state.json or sidecar schema migration policy | Latent — bites future versions |
| 3 | No clean-uninstall path; sidecars left in resurrect's snapshot dir confuse users | Will generate support issues |
| 4 | Reply sockets in `/tmp/wezsesh-<uid>/` survive across wezterm restarts on darwin (no tmpfs); startup sweep handles, but PRD should confirm ordering | Confirmed correct |
| 5 | `on_before_op` / `on_after_op` user hooks not explicitly `pcall`-wrapped at dispatch boundary | Edge correctness |
| 6 | Coexistence with smart_workspace_switcher / tabline / etc — no actual conflicts (pane-ID + HMAC filter) | Confirmed safe |
| 7a | Binary panic surfaces as `IPC_TIMEOUT` (5s wait) instead of `UNEXPECTED_EXIT` (immediate, via pane-closed event) | UX confusion |
| 7c | FS timeout / hang on NFS / cloud-sync mounts blocks TUI startup | Real-world hang |

### Decisions

- **§7.1**: distinct toasts for `"missing"` / `"unparseable"` / `compatible()=false`. `apply_to_config` body wrapped in top-level `pcall`. GLOBAL schema version stamp + wipe-on-mismatch.
- **§7.2**: `wezsesh nuke [--dry-run]` subcommand.
- **§6.14 failure-mode matrix**: explicit rows for each above, including listener-with-nil-key (silent log_warn + drop), binary unexpected exit (pane-closed wired to UNEXPECTED_EXIT toast), apply_to_config raise (pcall + stub config), user hook raise (pcall at dispatch + log_warn + continue), FS timeout (5s context deadline + partial list).
- **§8.1**: state.json schema migration documented (back up to `state.json.v<N>.bak` and reinitialize on version mismatch — pragmatic, loses usage history but acceptable).
- **§6.6 / §8.1**: `context.WithTimeout(5 * time.Second)` wrapping all FS ops in `internal/snapshots/`.

### Files cited
- PRD §7.1 version handshake (existing)
- Go stdlib `os/exec`, `context.WithTimeout`

---

## Summary of changes for PRD_V1

### BLOCKER fixes (must change before code lands)

- **§6.18 (new)**: snapshot argv allowlist defending against RCE via crafted resurrect snapshots. Hook trust (§6.11) was insufficient — it covers `on_*` hooks, NOT `process.argv` which resurrect blindly send_texts. (Spike 1.)
- **§6.19 (new)**: filesystem safety — `internal/safefs/` package using `O_NOFOLLOW` + `openat(2)` + `Renameat`. Mandatory for all owned writes. Sidecar writes are Go-only. (Spike 2.)
- **§6.11 (expanded)**: hook execution path made implementable with eight concrete gaps closed: read-once-exec, $SHELL fallback, cwd Stat, Stdin nil, timeout, process group, env scrub, Lstat for symlinks. (Spike 3.)

### Updates

- **§6.10 plugin layout**: added `internal/safefs/`, `internal/argvallow/`, `internal/nameval/`.
- **§6.14 failure mode matrix**: 7 new rows for listener-with-nil-key, binary unexpected exit, binary not on PATH, apply_to_config raise, user hook raise, FS timeout, distinct first-run vs version-mismatch toasts.
- **§7.1**: GLOBAL schema versioning; distinct binary-missing / unparseable / mismatch toasts; `apply_to_config` body wrapped in top-level pcall.
- **§7.2**: `wezsesh nuke [--dry-run]` subcommand.
- **§8.1**: storage/IPC list expanded with safefs requirements, signal handler spec, FS context deadline, env scrubbing on hooks. State.json migration policy noted.
- **§9 risks**: 13 new rows covering all v1.7 BLOCKERs and the lifecycle gaps. Risk table now serves as audit trail of every exploitable surface.

### Status
v6 complete. Three security BLOCKERs addressed. Eight lifecycle gaps closed. PRD bumped to v1.7.
