# PRD Research Findings — Round 7 (secondary defenses + supply-chain + goroutine lifecycle)

Goal of this round: surface the categories not yet examined after rounds 1–6. Four parallel research spikes targeting realistic next-pass blind spots: path-picker exec contract, shell-quoting in the §6.18 cwd-degradation path, dependency CVE / supply-chain audit, goroutine lifecycle.

**One BLOCKER (security)**, **two HIGH-severity correctness leaks**, **one major dependency upgrade**, plus a useful PRD ambiguity caught (sidecar location conflation).

---

## 1. Path picker / `new_workspace_command` exec contract

### Findings

§5.6 had skeleton-only spec ("zoxide → fd → error; one path per line"). Five gaps:
- Exec mechanism unspecified (sh -c vs argv).
- No timeout — hung zoxide / runaway fd blocks the binary.
- Output unbounded — pathological tool output exhausts memory.
- No path validation post-pick (existence, dir-ness).
- "Sidecar" referenced ambiguously — `<picked_path>/.wezsesh.json` for `on_create` is a DIFFERENT file from `<snapshot_dir>/workspace/<name>.wezsesh.json` for `on_restore`. PRD had been conflating these.

### Decisions — §5.6 fully respec'd

- **Exec**: `exec.CommandContext(ctx, shell, "-c", cmd)` matching §6.11's `$SHELL` → `/bin/sh` resolution. Stdin closed; `WEZSESH_*` env vars scrubbed; `SysProcAttr.Setpgid` for group-kill.
- **Timeout**: 15s hard. Distinct from the 10-min hook timeout because the user is staring at an empty picker. Not configurable in v0.1.
- **Output caps**: 1 MiB total stdout (`io.LimitedReader`), 10 000 lines max, 512 KiB scanner buffer per line.
- **Per-line validation**: trim trailing `\r`; skip blank; reject non-UTF-8; reject lines containing `\x00`. Tilde-expansion; `filepath.Clean`; must be `IsAbs` + existing directory.
- **Distinct error codes**: `NO_PATH_PROVIDER` (no zoxide / fd / override), `PATH_PICKER_TIMEOUT`, `PATH_PICKER_CMD_FAILED` (tool found but exited non-zero — surface stderr).
- **Sidecar disambiguation**: PRD now uses "project sidecar" for `<picked_path>/.wezsesh.json` (carries `on_create`, lives in project, travels with git) vs "snapshot sidecar" for `<snapshot_dir>/workspace/<name>.wezsesh.json` (carries `on_restore`/tags/pinned, lives next to resurrect's snapshot, travels with dotfiles sync). Same JSON schema, different locations and lifecycles. Trust hashes computed independently.

---

## 2. `pane:send_text` control-char injection — BLOCKER

### Findings

Source: `wezterm/config/src/lua.rs:414-422`, `wezterm/lua-api-crates/mux/src/pane.rs:118-124`.

`wezterm.shell_quote_arg` delegates to Rust `shlex::try_quote` v1.3.0. `shlex` errors **only** on NUL bytes — it accepts `\n` (LF) and `\r` (CR) inside the quoted string, including them as literal bytes. `pane:send_text` writes raw bytes directly to the pane's PTY (bare `pane.writer().write_all(text.as_bytes())`).

So an attacker-controlled `cwd = "/tmp/foo\nrm -rf ~"` flowing through the v1.7 §6.18 spec:
```lua
pane:send_text("cd " .. shell_quote(cwd) .. "\r\n")
```
produces:
```
cd '/tmp/foo
rm -rf ~'
```
The shell parses three commands: `cd '/tmp/foo` (incomplete syntax error), `rm -rf ~` (executed), `/tmp'` (syntax error). **Command injection.**

Same risk in the ALLOWED case if any `argv` element contains `\n`/`\r`: `argv = ["vim", "/etc/passwd\nrm -rf ~"]` would inject through `shell_join_args`.

The v1.7 §6.18 argv allowlist alone was insufficient — it covered `argv[0]` basename matching but didn't sanitize `cwd` or other argv elements.

### Decision — §6.18 control-char defense (BLOCKER)

Lua-side `bytes_clean` regex `[%z\1-\31\127]` rejects all C0 controls + DEL + NUL. Apply before `wezterm.shell_quote_arg` in BOTH branches:
- **Match branch**: validate every `argv` element AND `cwd`. Any dirty element → fall through to no-match path.
- **No-match branch**: validate `cwd`. Clean → `pane:send_text("cd " .. shell_quote_arg(cwd) .. "\r\n")`. Dirty → `pane:send_text("\r\n")` only (Option C: user lands at shell's default cwd). WARN-log either way.

`wezsesh doctor` extended: list snapshots whose `pane_tree.cwd` or any `pane_tree.process.argv[*]` element contains control characters; these would hit the dirty-input downgrade at restore time.

### Files cited
- `wezterm/config/src/lua.rs:414-422` (`shell_join_args` / `shell_quote_arg` thin wrappers over `shlex`)
- `wezterm/lua-api-crates/mux/src/pane.rs:118-124` (raw `write_all` to PTY)
- Rust `shlex` v1.3.0 docs (control char "cannot be quoted portably")

---

## 3. Dependency CVE / supply-chain audit

### Findings

GitHub Advisory Database + `pkg.go.dev/vuln` queried 2026-04-29. **No CVEs in any pinned dep.** But the entire charmbracelet stack is now stale — round 3's "avoid v2, RC, API churn" decision is resolved.

| Dep | v1.7 pin | Latest | Status |
|---|---|---|---|
| `bubbletea` | v1.3.5 | **v2.0.6** (2025-04) | Upgrade to v2 |
| `bubbles` | v0.21.0 | **v2.1.0** (2025-03) | Upgrade to v2 |
| `lipgloss` | v1.1.0 | **v2.0.3** (2025-04) | Upgrade to v2 |
| `x/ansi` | v0.9.3 | **v0.11.7** (2026-04) | Upgrade |
| `huh` | "latest" | **v2.0.3** (2025-03) | Pin to v2 |
| `sahilm/fuzzy` | v0.1.1 | v0.1.1 (last 2025-08-02) | Keep |
| `pure_lua_SHA` | unpinned | commit `6adac177` (2022-03) | **Pin commit + sha256 verify** |

Two application-level concerns surfaced:
1. **lipgloss does NOT sanitize input.** A snapshot-named `\x1b[2J` would clear the terminal at picker render. Round-7 finding flowed back into §6.17 as render-time sanitization.
2. **bubbletea v2 stdin parsing** routes through `charmbracelet/x/input` (ECMA-48 parser). A malicious terminal host injecting fabricated CSI/OSC into stdin could confuse key events. Out of scope: `WEZTERM_PANE` guard already enforces local-PTY-only.

### Decisions — §8.1 dependency block + CI checks

- Upgrade entire charmbracelet stack to v2. Module path changes (`/v2` suffix).
- Pin `go 1.26.2` (current stable; includes `crypto/tls` CVE-2025-68121 patch).
- Pin `pure_lua_SHA` to `6adac177c16c3496899f69d220dfb20bc31c03df` with `plugin/wezsesh/vendor/SOURCES.lock` (commit + sha256). CI runs `sha256sum -c`.
- Required CI: `go mod verify`, `govulncheck ./...`, sha256 vendor verify, `-trimpath -ldflags="-s -w"` reproducible builds, `staticcheck`, `go vet`. Release workflow gated on these.
- Add `internal/nameval.SanitizeForDisplay` for render-time C0/C1+DEL stripping (§6.17).

### Files cited
- GitHub Advisory Database (`https://github.com/advisories`) per-dep queries
- Go vuln DB (`https://pkg.go.dev/vuln/list`)
- charmbracelet release pages

---

## 4. Goroutine lifecycle audit

### Findings

Goroutines wezsesh spawns: G1 reply listener, G2 retransmit timer, G3 switch poller, G4 signal handler, G5–G7 bubbletea internals.

Two HIGH-severity latent leaks:

**G1 (HIGH) — Listener orphaned on early TUI quit.** PRD §6.4 specified `StartListener` returns `(chan, cleanup, error)`, but cleanup contract was incomplete: it called `os.Remove(sockPath)` but NOT `listener.Close()`. Without `Close()`, `Accept()` doesn't unblock; goroutine stays alive holding the socket fd until process exit. If `tea.Run()` returned before any reply arrived (user pressed `q`), the goroutine dangled.

**G3 (HIGH) — Switch poller has no cancellation.** §6.15 said "50ms cadence × 5s ceiling." If workspace appeared at 60ms, no mechanism stopped the goroutine — it kept ticking 50ms intervals until 5s. `goleak.VerifyNone(t)` at T+100ms would fail.

Plus medium-severity:
- G2 retransmit if implemented as raw goroutine fires at 2s regardless of early reply — leaks goroutine until fire.
- No `defer recover()` in any spawned goroutine. A panic on malformed JSON / CLI output crashes the binary, leaving the wezsesh pane visibly dead with no diagnostic.
- `cleanup()` not `sync.Once`-wrapped; double-call from defer + signal handler would double-close listener (panic).

### Decisions — §6.4, §6.15, §8.1

- `cleanup()` calls `listener.Close()` + `os.Remove(sockPath)` under `sync.Once`. `defer cleanup()` placed immediately after `StartListener`; same ref passed to `InstallSignalHandler`. Listener loop exits on `net.ErrClosed`.
- `StartSwitchPoller(ctx, name, interval)` accepts cancellable context. Caller wraps in `context.WithTimeout(parent, 5*time.Second)`. Poller checks `<-ctx.Done()` at iteration boundary.
- Retransmit MUST be `tea.After(2*time.Second, retransmitMsg{})` Cmd. Idempotent guard: `if model.replyReceived { return model, nil }`. Bubbletea handles cancellation on program exit; no goroutine to track.
- Mandatory `defer func() { if r := recover(); r != nil { log.Error(...) } }()` in every goroutine in `internal/ipcsock/`, `internal/wezcli/`, `internal/argvallow/`, `internal/tui/`. Spec'd in §8.1 testing invariants.
- Tests use `goleak.VerifyNone(t, goleak.IgnoreTopFunction("internal/ipcsock.installSignalHandlerWorker"))`. Signal handler is the only intentional survivor.

### Files cited
- bubbletea v2 `tea.go:737-742` (program.Send safe drop)
- Go stdlib `context.WithTimeout`, `sync.Once`, `net.ErrClosed`
- `go.uber.org/goleak v1.3+`

---

## Summary of changes for PRD_V1

### BLOCKER fixes (must change before code lands)

- **§6.18 (security)**: `pane:send_text` control-char defense. Lua `bytes_clean` regex applied to `cwd` and every `argv` element before `shell_quote_arg`. Dirty inputs downgrade to no-op send.

### HIGH-severity correctness

- **§6.4**: `cleanup()` contract — `listener.Close()` + `os.Remove` under `sync.Once`; `defer cleanup()` immediately after `StartListener`. Listener loop exits on `net.ErrClosed`.
- **§6.15**: `StartSwitchPoller(ctx, ...)` accepts context; cancellation propagates.
- **§6.2 / §6.4**: retransmit must be `tea.After` Cmd, not raw goroutine.

### Updates

- **§5.6**: full path-picker contract (exec, timeout, caps, validation, error codes); sidecar disambiguation (project vs snapshot).
- **§6.17**: render-time sanitization helper for disk-sourced strings; lipgloss doesn't sanitize.
- **§8.1**: charmbracelet v1 → v2 upgrade; `go 1.26.2` pin; `pure_lua_SHA` commit pin; CI supply-chain checks (`go mod verify`, `govulncheck`, sha256 verify, `-trimpath`); test invariants (`goleak`, `defer recover`, golden-JSON tests, HMAC round-trip, race tests, broadcast test).
- **§9 risks**: 11 new rows covering v1.8 BLOCKERs and the supply-chain / lifecycle / sanitization hardening.

### Status
v7 complete. One security BLOCKER (control-char injection) and two HIGH-severity correctness leaks (G1 listener, G3 poller) addressed. PRD bumped to v1.8.

The "we keep finding more" pattern is converging — round 7 found one BLOCKER (vs three in round 6, three in round 5). The remaining unknowns are increasingly runtime-only: cold-start measurement, restore-latency reality-check on real shells, end-to-end IPC dry-run with real wezterm + resurrect installs. Those are next-phase work that needs actual code, not more spec audit.
