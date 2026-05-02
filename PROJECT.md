# PROJECT — wezsesh build ledger

The iteration backlog for shipping wezsesh v0.1, as nominated by `docs/design.md` §1.
This file is the **single source of truth** for what's done, what's next, and what's
blocked. Status changes ship in the same commit as the implementation.

## Status legend

- **`blocked`** — a `depends-on` task is not yet `done`.
- **`ready`** — all dependencies are `done`; can be picked next.
- **`in-progress`** — currently being implemented (a single task at a time, set by
  `/next-task` so a fresh session knows what was interrupted).
- **`needs-review`** — implementation landed, but a gate failed or
  `design-conformance-reviewer` flagged something. Body of the task lists the
  outstanding items; the next `/next-task` invocation picks this up before any
  `ready` task.
- **`done`** — all gates green, conformance review clean (or findings explicitly
  accepted in the task body), commit landed on `main`.

## Working agreement

- One task = one commit. Commit message: `<type>(<scope>): T-XXX <title>`.
  Examples: `feat(safefs): T-101 atomic write + locks`, `feat(plugin): T-600 ipc.lua handler`.
- A task is `done` only when **every** acceptance gate passes locally AND
  `design-conformance-reviewer` reports no `CRITICAL`/`HIGH` findings against the
  diff. `MEDIUM`/`LOW` findings can be accepted with a one-line note in the task body.
- `Spec` refs point at `docs/design.md` headings; § numbers may drift, headings are
  durable. `(P §x.y)` refs live in `docs/prd.md`.
- Acceptance gates name §17.3 / §17.4 tests verbatim. Don't paraphrase — the test
  name is the contract.
- A task should NOT introduce code outside its listed `Files`. If you find you
  need to, stop and either (a) extend the file list with a one-line note, or
  (b) create a new task. Scope creep is what `design-conformance-reviewer` exists
  to catch.
- Tasks within the same phase that share no `depends-on` MAY run in parallel
  across separate sessions (different worktrees), but each task is still a single
  commit.

## How to advance the build

Run `/next-task` in a fresh `claude` session. The skill reads this file, picks the
first `ready` task (preferring `needs-review` if any exist), dispatches the listed
`Owner` agent, runs the gates, runs the conformance reviewer, commits, and updates
this file. See `.claude/skills/next-task/SKILL.md`.

## Index

| Phase | Range | Theme |
|---|---|---|
| 0 | T-000…T-005 | Bootstrap (module, deps, vendored Lua, CI) |
| 1 | T-100…T-105 | Foundation Go packages (no inter-deps) |
| 2 | T-200…T-203 | State primitives |
| 3 | T-300…T-303 | IPC plumbing |
| 4 | T-400…T-401 | Wezterm interop |
| 5 | T-500…T-506 | Lua primitives |
| 6 | T-600…T-605 | Lua handler & plugin entry |
| 7 | T-700…T-702 | TUI + doctor + pathpicker |
| 8 | T-800…T-808 | CLI subcommands |
| 9 | T-900…T-901 | Integration: e2e smoke + fuzz |

---

## Phase 0 — Bootstrap

### T-000 · Repo skeleton + go.mod
**Status:** in-progress
**Owner:** general-purpose
**Depends-on:** —
**Spec:** §2 (repo layout), §16.1 (Go version)
**Files:** `go.mod`, `cmd/wezsesh/.keep`, `internal/<each-pkg>/.keep`, `plugin/wezsesh/.keep`, `plugin/wezsesh/vendor/.keep`
**Acceptance gates:**
- `go.mod` has `go 1.26.2` and module path matching the import paths used in §8.
- All directories under `internal/` listed in §2 exist (empty `.keep` files OK).
- `go vet ./...` succeeds (no Go files yet, but the command must run cleanly).
**Done when:** `go mod tidy` is a no-op and `git ls-files` matches the §2 layout.

### T-001 · Pin Go dependencies
**Status:** blocked
**Owner:** general-purpose
**Depends-on:** T-000
**Spec:** §16.2 (pinned dependencies)
**Files:** `go.mod`, `go.sum`
**Acceptance gates:**
- Every module in §16.2 listed at exactly the named version (latest where unpinned).
- `go mod verify` passes.
- `govulncheck ./...` passes (warnings on unused deps OK at this stage).
**Done when:** `go mod tidy && go mod verify` are both clean.

### T-002 · Vendor `sha2.lua` + `SOURCES.lock`
**Status:** blocked
**Owner:** lua-plugin-engineer
**Depends-on:** T-000
**Spec:** §16.3 (vendored Lua)
**Files:** `plugin/wezsesh/vendor/sha2.lua`, `plugin/wezsesh/vendor/SOURCES.lock`
**Acceptance gates:**
- `sha2.lua` matches upstream `Egor-Skriptunoff/pure_lua_SHA` at commit `6adac177c16c3496899f69d220dfb20bc31c03df`.
- `SOURCES.lock` records upstream URL, commit, and `sha256` of the file.
- `sha256sum -c plugin/wezsesh/vendor/SOURCES.lock` exits 0.
**Done when:** the integrity gate is part of CI and passes.

### T-003 · `internal/lualint` package skeleton
**Status:** blocked
**Owner:** general-purpose
**Depends-on:** T-001
**Spec:** §17.4 (CI lint suite), §16.5 (custom lints), §9.0.1.1 (Lua expression-call ambiguity)
**Files:** `internal/lualint/lualint.go`, `internal/lualint/tokeniser.go`, `internal/lualint/async_funcs.go`
**Acceptance gates:**
- A minimal Lua tokeniser that handles strings, comments, and the line-leading-`(` lookback (per §16.5 — regex-based won't pass).
- Exported `Lint(path string, rules []Rule) ([]Finding, error)` API stub callable from `cmd/lualint`.
- `go test ./internal/lualint/...` passes against a tiny golden corpus committed alongside.
**Done when:** the package compiles and exports a stable API that other lint tasks can plug rules into. Concrete rules land in their owning tasks (T-600 ships the `.await`-free check, T-605 ships the codegen-sync check, etc.).

### T-004 · CI workflow with §16.4 gates
**Status:** blocked
**Owner:** general-purpose
**Depends-on:** T-001, T-002
**Spec:** §16.4 (required CI gates), §16.1 (build matrix)
**Files:** `.github/workflows/ci.yml` (or platform-equivalent), `Makefile` or `tasks` script for local parity
**Acceptance gates:**
- Every gate in §16.4 invoked: module verify, govulncheck, staticcheck, vet,
  vendored-crypto integrity, codegen freshness (placeholder until T-605),
  reproducible build, `LC_ALL=C` canonical-JSON tests (placeholder until T-102),
  Lua version assertion (deferred until plugin lands), verb/shape parity
  (placeholder until T-601).
- Build matrix: `linux-amd64`, `linux-arm64`, `darwin-amd64`, `darwin-arm64`,
  with macOS runners pinned to `macos-13` and `macos-14`.
- CI passes on a fresh PR.
**Done when:** a no-op PR shows green across the matrix.

### T-005 · Custom CI lints from §16.5
**Status:** blocked
**Owner:** general-purpose
**Depends-on:** T-003, T-004
**Spec:** §16.5 (custom CI lints)
**Files:** `internal/lualint/rules/*.go`, `cmd/lualint/main.go`, `.github/workflows/ci.yml` (extend)
**Acceptance gates:**
- The Go-side grep/AST gates: `unix.F_OFD_SETLK` outside `lock_linux.go`,
  `os.WriteFile`/`os.OpenFile`/`syscall.Open` in restricted packages, direct
  `wezterm cli` exec outside `internal/wezcli/`, concrete Dispatcher outside
  `internal/ipcdispatcher/`, `tea.After` references, `pcall`-wrap on async
  spawns, `defer recover()` in goroutines (restricted packages),
  `log.Println`/`fmt.Fprintln(os.Stderr, ...)` ban, `verb_args_shape` parity.
- The Lua-side rules wired into `cmd/lualint`: `_G.wezterm` ban, `debug.*` ban,
  `dofile(` ban, line-leading-`(` after expression-call statement,
  `package.loaded["wezsesh.*"] = nil` bust loop presence in `init.lua`,
  nested-table value into `wezterm.GLOBAL`, the four spike-#2 grep lints.
- A test corpus under `internal/lualint/testdata/` proving each rule fires on a
  positive sample and stays silent on a negative sample.
**Done when:** all lints run in CI as build errors / PR-fail per the §16.5 column.

---

## Phase 1 — Foundation Go packages

### T-100 · `internal/logger`
**Status:** blocked
**Owner:** general-purpose
**Depends-on:** T-001
**Spec:** §8.18 (logger), §17.3 row "Logger Warn/Error sync flush"
**Files:** `internal/logger/logger.go`, `internal/logger/logger_test.go`
**Acceptance gates:**
- §17.3: **Logger Warn/Error sync flush** — crash-after-Warn → log file contains the Warn line on disk.
- 1 s tick flush for Info; immediate flush on Warn/Error.
- `ResolveLevel` honours `WEZSESH_LOG`.
**Done when:** test passes; `staticcheck ./internal/logger/...` clean.

### T-101 · `internal/safefs`
**Status:** blocked
**Owner:** safefs-engineer
**Depends-on:** T-001
**Spec:** §8.1 (safefs), §13.4 (save flow), §16.5 (lints)
**Files:** `internal/safefs/safefs.go`, `internal/safefs/lockedfile.go`, `internal/safefs/lock_linux.go`, `internal/safefs/lock_other.go`, `internal/safefs/netfs.go`, `internal/safefs/symlinkpolicy.go`, plus `_test.go` siblings
**Acceptance gates (from §17.3):**
- **F_OFD_SETLK build-tag** — reference outside `lock_linux.go` fails build.
- **O_CLOEXEC inheritance** — lock fd NOT in fork-spawned child's fd table.
- **F_SETLK polling fairness** — 3 contending binaries, lock held 100 ms → others acquire within 5 s; WARN fires at 1 s and 3 s.
- **`safefs.Enforce` SkipWarn vs Refuse** — top-level dir symlink → Refuse error; file inside → SkipWarn returns ok=false, no err.
- **`safefs.IsNetworkFS` detection** — tmpfs → `("tmpfs", false)`; NFS (when available) → `("nfs", true)`.
- **Save flock serialisation (Phase A)** — one succeeds, other gets `SNAPSHOT_LOCKED`.
- **Save first-write (no expected_hash)** — `AcquireExclusiveOrCreate` creates, locks, releases; concurrent first-saves serialise via per-name in-process mutex (mutex itself ships with T-303 / T-800; this gate covers the safefs primitive).
- **Request-file atomic write (spike-#3)** — concurrent `AtomicWriteFile` produces disjoint files; tmp+rename observably atomic; `O_EXCL` rejects collisions.
**Done when:** all listed tests pass under `go test -race`; conformance review §2 clean.

### T-102 · `internal/canonicaljson`
**Status:** blocked
**Owner:** wire-protocol-guardian
**Depends-on:** T-001
**Spec:** §8.2 (canonicaljson), §4 (canonical-JSON spec), §17.1 (golden corpus)
**Files:** `internal/canonicaljson/encoder.go`, `internal/canonicaljson/encoder_test.go`, `internal/canonicaljson/testdata/golden/*.json`
**Acceptance gates:**
- §17.1 golden corpus committed (the table in §17.1 is the source of truth).
- Round-trip tests for every shape in §4.1 (empty container shape, integer/float handling, key ordering, escape rules).
- `LC_ALL=C go test ./internal/canonicaljson/...` passes (CI gate).
**Done when:** golden corpus is byte-stable across re-runs; conformance review §1 (wire) clean.

### T-103 · `internal/hmac`
**Status:** blocked
**Owner:** wire-protocol-guardian
**Depends-on:** T-102
**Spec:** §8.3 (hmac), §5 (HMAC, freshness, replay), §17.2 (HMAC fixture); spike #1 in `docs/issues/1.md` for fixture-ID rationale
**Files:** `internal/hmac/hmac.go`, `internal/hmac/hmac_test.go`, `internal/hmac/testdata/roundtrip.json`
**Acceptance gates:**
- §17.2 round-trip fixture committed verbatim (the corrected ID per §0.1 row 28; pinned `expected_hmac`).
- HMAC field-removal sequence per §4.3 (no `hmac=""` set-then-encode pattern).
- Constant-time compare delegated to `crypto/subtle`.
**Done when:** fixture round-trips; openssl cross-check command in test docstring works.

### T-104 · `internal/nameval`
**Status:** blocked
**Owner:** general-purpose
**Depends-on:** T-001
**Spec:** §8.4 (nameval), §15 (validation rules)
**Files:** `internal/nameval/nameval.go`, `internal/nameval/sanitize.go`, `internal/nameval/nameval_test.go`
**Acceptance gates (from §17.3):**
- **Render-time sanitization** — snapshot named `\x1b[2J` does not clear terminal.
- **Control-char `cwd`/argv** — `cwd="/tmp/foo\nrm -rf ~"` → no injection (downgrade to no-op). (The decision happens in argvallow / on_pane_restore but the byte-clean primitive lives here.)
- Workspace-name validation per §15.1 (length, character class, NFC normalise).
- Tag validation per §15.2 (count + per-tag rules).
- **Name truncate algorithm** per §15.5.
**Done when:** every §15 rule has a test; `nameval.SanitizeForDisplay` returns deterministic output.

### T-105 · `internal/config`
**Status:** blocked
**Owner:** general-purpose
**Depends-on:** T-001
**Spec:** §8.19 (config), §11 (configuration schema), §11.4 (env vs config resolution table)
**Files:** `internal/config/config.go`, `internal/config/loader.go`, `internal/config/autodetect.go`, `internal/config/config_test.go`
**Acceptance gates (from §17.3):**
- **Config Exclude invalid regex** — bad regex → `ExcludeErrors` populated; runtime treats element as no-op.
- **Auto-detect** rules per §12.5 covered by tests.
- **Env override** rules per §11.3 honoured; precedence per §11.4 verified.
**Done when:** loader handles each row of §11.4 with explicit tests.

---

## Phase 2 — State primitives

### T-200 · `internal/state`
**Status:** blocked
**Owner:** safefs-engineer
**Depends-on:** T-101
**Spec:** §8.11 (state), §10.4 (state.json), §13.11 (pin storage)
**Files:** `internal/state/state.go`, `internal/state/migrate.go`, `internal/state/state_test.go`
**Acceptance gates (from §17.3):**
- **Schema migration `state.json` v=1 → live_pins** — v=1 file with old `pins` key → migrated to `live_pins`; entries with corresponding snapshot are dropped.
- **Schema migration `state.json` v>1** — v=2 file → backed up to `.v2.bak` + reinitialised; no error.
- All disk writes go through `safefs.AtomicWriteFile`.
- `state.SetPin` accepts ctx (§0.1 row 17).
**Done when:** migration tests pass; conformance review §2 clean.

### T-201 · `internal/trust`
**Status:** blocked
**Owner:** trust-and-hooks-engineer
**Depends-on:** T-101
**Spec:** §8.12 (trust), §10.5 (trust file), §13.5 (hook trust check), §13.5.2 (rebind UX)
**Files:** `internal/trust/trust.go`, `internal/trust/hash.go`, `internal/trust/rebind.go`, `internal/trust/trust_test.go`
**Acceptance gates (from §17.3):**
- **Trust rebind happy path** — identical command_bytes at new path → rebind succeeds; old hash file removed.
- **Trust rebind diverged command** — new path has different command_bytes → rebind refuses, old approval intact.
- Trust hash is length-prefixed (`uint32_be(len) || bytes || uint32_be(len) || bytes`); any `\n` separator is a CVE.
- Trust file `os.Lstat` (not `os.Stat`); trust dir `safefs.Enforce(SymlinkRefuse)` at startup.
- Sidecar bytes read once; same in-memory bytes used for both hash AND `exec.Command`.
**Done when:** all hash construction is unit-tested; conformance review §6 (trust+hooks) clean.

### T-202 · `internal/argvallow`
**Status:** blocked
**Owner:** resurrect-interop-engineer
**Depends-on:** T-101
**Spec:** §8.13 (argvallow)
**Files:** `internal/argvallow/argvallow.go`, `internal/argvallow/default.txt`, `internal/argvallow/codegen/main.go`, `internal/argvallow/argvallow_test.go`
**Acceptance gates (from §17.3):**
- **Argv allowlist enforcement** (Go side) — `argv[1]="rm"` → no exec.
- `default.txt` embedded via `//go:embed`; lookup is `O(1)` against the embedded set.
- Codegen tool emits `default_allowlist.lua` with byte-equal contents (T-605 wires the freshness lint).
**Done when:** Go side enforcement tests pass; the codegen tool runs and produces a deterministic `.lua` (used by T-605).

### T-203 · `internal/snapshots`
**Status:** blocked
**Owner:** resurrect-interop-engineer
**Depends-on:** T-101, T-102
**Spec:** §8.10 (snapshots), §10.1 (snapshot file), §10.2 (sidecar), Appendix B (encryption sniff)
**Files:** `internal/snapshots/repo.go`, `internal/snapshots/sidecar.go`, `internal/snapshots/encryption.go`, `internal/snapshots/snapshots_test.go`
**Acceptance gates (from §17.3):**
- **Resurrect race** — mid-write parse failure recovers via 3× retry.
- **Schema migration sidecar** — v=2 sidecar → backed up to `.v2.bak` + `ReadSidecar` returns ok=false.
- Encryption magic-byte sniff per Appendix B.
- 10 MiB / depth 100 caps (per conformance §7).
- `Hash` returns `"sha256:<hex>"`; helper `RawHashHex` exists (§0.1 row 13).
**Done when:** parse tolerance covered (per-file errors → `Entry.ParseError`, never abort `Repo.List`); conformance review §7 clean.

---

## Phase 3 — IPC plumbing

### T-300 · `internal/uservar`
**Status:** blocked
**Owner:** wire-protocol-guardian
**Depends-on:** T-101
**Spec:** §8.8 (uservar), §3.1 (forward path, sidecar pattern), §0.1 row 34 (spike-#3); full spike-#3 rationale in `docs/issues/3.md` (renderer-OSC interleave race; 256 B ceiling derivation)
**Files:** `internal/uservar/writer.go`, `internal/uservar/writer_test.go`
**Acceptance gates (from §17.3, §16.5):**
- **OSC ≤ 256 B contract (spike-#3)** — `WriteOSC` rejects payloads whose on-the-wire OSC envelope > 256 B with an explicit error rather than emit a multi-syscall write.
- Writes go to `/dev/tty`, NOT `os.Stdout`.
- Pointer payload contains `{v, id, path}` only; no inline canonical-JSON request.
**Done when:** size-ceiling test passes; bubbletea integration uses this writer (verified in T-701 / T-800).

### T-301 · `internal/ipcsock`
**Status:** blocked
**Owner:** wire-protocol-guardian
**Depends-on:** T-101
**Spec:** §8.7 (ipcsock), §3.2 (reverse path), §13.2 (reply socket lifecycle)
**Files:** `internal/ipcsock/listener.go`, `internal/ipcsock/sweep.go`, `internal/ipcsock/listener_test.go`
**Acceptance gates (from §17.3):**
- **Reply socket lifecycle** — listener exits via `net.ErrClosed`; cleanup is `sync.Once`.
- **Reply socket sequential accept** — second connection waits for first to close.
- **Reply channel buffer** — producer blocks at cap 2; never panics.
- **SUN_PATH overflow** (Go side) — `IPC_INIT_FAILED` returned for over-budget runtime_dir.
**Done when:** `go test -race ./internal/ipcsock/...` clean; goroutines leak-checked via `goleak`.

### T-302 · `internal/ipc` (Dispatcher interface)
**Status:** blocked
**Owner:** wire-protocol-guardian
**Depends-on:** T-001
**Spec:** §8.5 (ipc)
**Files:** `internal/ipc/dispatcher.go`
**Acceptance gates:**
- Pure interface declaration; no implementation.
- The §16.5 lint "Concrete Dispatcher construction outside `internal/ipcdispatcher/`" passes when applied to all callsites in the repo.
**Done when:** `go vet` clean; the interface compiles; no concrete impl present.

### T-303 · `internal/ipcdispatcher` (concrete dispatcher)
**Status:** blocked
**Owner:** wire-protocol-guardian
**Depends-on:** T-101, T-102, T-103, T-300, T-301, T-302
**Spec:** §8.6 (ipcdispatcher), §3.1 (forward path), §3.5 (hard ceilings), §13.1 (request lifecycle), §0.1 row 34 (spike-#3 sidecar); full spike-#3 rationale in `docs/issues/3.md` (Phase 1 atomic write semantics, request-file lifecycle)
**Files:** `internal/ipcdispatcher/dispatcher.go`, `internal/ipcdispatcher/phases.go`, `internal/ipcdispatcher/dispatcher_test.go`
**Acceptance gates (from §17.3):**
- **Request-file atomic write (spike-#3)** — concurrent `Dispatch` produces disjoint `<8-hex>.json` files.
- **Save in-process serialisation** — two concurrent same-name saves in one binary run sequentially via `nameMutex`; no races.
- **Save Phase C re-hash** — reply.data.hash matches sha256 of file as written by Lua. (Phase C hooked here; harness shipped from T-800.)
- Phase 1 calls `safefs.AtomicWriteFile` per §0.1 row 34.
- Reply parser rejects missing `v` field per §0.1 row 5.
- `lockCtx` (5 s) and `ipcCtx` (5 s) are independent per §0.1 row 14.
**Done when:** dispatcher tests pass under `-race`; conformance review §1 wire clean.

---

## Phase 4 — Wezterm interop

### T-400 · `internal/wezcli`
**Status:** blocked
**Owner:** wezterm-interop-engineer
**Depends-on:** T-100
**Spec:** §8.9 (wezcli), §13.3 (switch poller), §0.1 rows 18–19
**Files:** `internal/wezcli/client.go`, `internal/wezcli/list.go`, `internal/wezcli/listclients.go`, `internal/wezcli/switchpoller.go`, `internal/wezcli/wezcli_test.go`
**Acceptance gates (from §17.3):**
- **Switch-poller false-positive** — `switch` to active short-circuits in 1 tick; `switch+restore` bypasses via `isRestoreFlow`.
- **Switch poller adaptive cadence** — slow `ListClients` (1.5 s tick) → cadence dilates to 250 ms.
- Pinned `TargetClientID` captured at Phase 1 start, NOT re-evaluated per tick (conformance §5).
- Predicate scopes by `TargetWindowID` AND workspace.
- `cwd` parsing guards for `""` (NOT `null`) before URL-decoding.
- All `wezterm cli` invocations live in this package (CI lint covers).
**Done when:** test suite passes; conformance review §5 clean.

### T-401 · `internal/find`
**Status:** blocked
**Owner:** wezterm-interop-engineer
**Depends-on:** T-400
**Spec:** §8.14 (find), §13.7 (two-phase find), §0.1 row 18 (drain protocol)
**Files:** `internal/find/find.go`, `internal/find/twophase.go`, `internal/find/find_test.go`
**Acceptance gates (from §17.3):**
- **Two-phase find drain** — post-poller dispCancel + drain → channel closes within 100 ms; goroutines exit cleanly.
- **Two-phase find client pinning** — second client gaining "most recent" mid-poll does NOT flip predicate.
- **Two-phase find window scoping** — closing wezterm window mid-Phase-1 → `MUX_UNREACHABLE`.
- Phase 2 NEVER runs without Phase 1 when `match.Workspace != currentActiveWorkspace`.
**Done when:** all listed tests pass; `goleak` confirms no leaked goroutines.

---

## Phase 5 — Lua primitives

### T-500 · `plugin/wezsesh/canonical_json.lua`
**Status:** blocked
**Owner:** lua-plugin-engineer
**Depends-on:** T-000, T-102
**Spec:** §9.7 (canonical_json), §4 (encoder spec), §17.1 (golden corpus)
**Files:** `plugin/wezsesh/canonical_json.lua`, `plugin/wezsesh/canonical_json_spec.lua` (busted-style harness)
**Acceptance gates (from §17.3):**
- **Verb-aware tagging round-trip** — empty `args = {}` for `noop` verifies; the same shape parsed and re-encoded matches Go's canonical bytes (golden corpus shared with T-102).
- Encoder shape table is the single tagging mechanism; `__wezsesh_canonical = "untagged"` is outlawed (§0.1 row 24).
- Byte-equality with Go encoder verified across the entire §17.1 corpus.
**Done when:** harness invokes Lua against the same fixtures Go uses and matches byte-for-byte.

### T-501 · `plugin/wezsesh/hmac.lua`
**Status:** blocked
**Owner:** lua-plugin-engineer
**Depends-on:** T-002, T-103, T-500
**Spec:** §9.8 (hmac.lua), §5 (HMAC), §17.2 (round-trip fixture)
**Files:** `plugin/wezsesh/hmac.lua`, `plugin/wezsesh/hmac_spec.lua`
**Acceptance gates:**
- §17.2 fixture round-trips with byte-equal `hmac` value.
- HMAC field-removal sequence per §4.3 (no `hmac=""` set-then-encode).
**Done when:** fixture verified; `_G.wezterm` not referenced (§16.5 lint).

### T-502 · `plugin/wezsesh/ct_eq.lua`
**Status:** blocked
**Owner:** lua-plugin-engineer
**Depends-on:** T-000
**Spec:** §9.9 (ct_eq), §5.6 (constant-time compare), §0.1 row 15 (Lua 5.3+ requirement)
**Files:** `plugin/wezsesh/ct_eq.lua`, `plugin/wezsesh/ct_eq_spec.lua`
**Acceptance gates:**
- Bitwise `~`/`|` in source → require Lua ≥ 5.3 (asserted at module load).
- Constant-time property: branchless on input length up to 256 chars.
**Done when:** spec passes under both Lua 5.3 and 5.4.

### T-503 · `plugin/wezsesh/b64.lua`
**Status:** blocked
**Owner:** lua-plugin-engineer
**Depends-on:** T-000
**Spec:** §9.10 (b64), §0.1 row 34 (`b64.decode` is hot-path post-spike-#3)
**Files:** `plugin/wezsesh/b64.lua`, `plugin/wezsesh/b64_spec.lua`
**Acceptance gates:**
- `encode`/`decode` round-trip on 0–4096 byte inputs; rejects non-canonical padding.
- Performance: `decode` of 4 KiB completes in < 1 ms (warm).
**Done when:** spec passes; `decode` is allocation-conscious enough to be invoked per request.

### T-504 · `plugin/wezsesh/state.lua`
**Status:** blocked
**Owner:** lua-plugin-engineer
**Depends-on:** T-000
**Spec:** §9.6 (state), §10.6 (`wezterm.GLOBAL` keys), §0.1 row 30 (GLOBAL value-shape rule); full spike-#1 rationale in `docs/issues/1.md` (mlua GLOBAL userdata silent-break on nested-table values)
**Files:** `plugin/wezsesh/state.lua`, `plugin/wezsesh/state_spec.lua`
**Acceptance gates:**
- Every `wezterm.GLOBAL` write flushes back via `set_state`.
- Forbidden: nested-table values; CI lint catches but spec exercises a harness double.
- `seen_ids` storage shape is `flat int (unix-seconds) per ULID` per §0.1 row 30.
**Done when:** spec covers each GLOBAL key in §10.6; conformance §3 clean.

### T-505 · `plugin/wezsesh/result.lua`
**Status:** blocked
**Owner:** lua-plugin-engineer
**Depends-on:** T-503
**Spec:** §9.5 (result)
**Files:** `plugin/wezsesh/result.lua`, `plugin/wezsesh/result_spec.lua`
**Acceptance gates:**
- Reply emitter wraps `wezterm.background_child_process` in `pcall` (§16.5).
- `started` reply has no `data` / `warnings` / `error`.
- Every reply carries `v: 1` (§0.1 row 5).
**Done when:** spec covers each verb's reply shape from §6.

### T-506 · `plugin/wezsesh/resurrect_error.lua`
**Status:** blocked
**Owner:** resurrect-interop-engineer
**Depends-on:** T-000
**Spec:** §9.13 (resurrect.error capture), §0.1 row 33 (spike-#2); full spike-#2 rationale in `docs/issues/2.md` (why `pcall(state_manager.save_state)` is empirically broken; dual-path detection scheme)
**Files:** `plugin/wezsesh/resurrect_error.lua`, `plugin/wezsesh/resurrect_error_spec.lua`
**Acceptance gates (from §17.3):**
- **`with_capture` re-entrancy guard (spike-#2)** — nested `with_capture` raises the assert; outer call's capture is preserved.
- **`resurrect_error.register()` is idempotent (spike-#2)** — calling `apply_to_config` twice in one Lua state → exactly one `wezterm.on("resurrect.error", …)` registration via the `_G` install gate.
- The §16.5 lints for `resurrect.error` registration site pass.
**Done when:** spec verifies the per-call buffer interleaved with the persistent listener.

---

## Phase 6 — Lua handler & plugin

### T-600 · `plugin/wezsesh/ipc.lua` (handler state machine)
**Status:** blocked
**Owner:** lua-plugin-engineer
**Depends-on:** T-303, T-500, T-501, T-502, T-503, T-504, T-505
**Spec:** §9.3 (handler step state machine), §13.1 (request lifecycle), §13.9 (SUN_PATH validation), §3.1 (forward path two-phase decode); full spike-#3 rationale in `docs/issues/3.md` (pointer pre-step validation; payload-vs-pointer field-shape split)
**Files:** `plugin/wezsesh/ipc.lua`, `plugin/wezsesh/ipc_spec.lua`
**Acceptance gates (from §17.3):**
- **Pointer-shape validation (spike-#3)** — malformed pointer JSON / path outside `<runtime_dir>/req/` / wrong mode / symlink / `pointer.id ≠ payload.id` → silent-drop + `log_warn REQ_POINTER_REJECTED`. Plugin does not write a reply.
- **HMAC mismatch silent on wire** — corrupted payload → no reply on socket.
- **Freshness boundary** — `ts=now-30` accept; `ts=now-31` reject; `ts=now+30` accept; `ts=now+31` reject.
- **`seen_ids` TTL prune (session-wide)** — entries older than 60 s dropped.
- **SUN_PATH overflow** (Lua side) — over-budget runtime_dir → Lua sentinel + 10s toast.
- **Multi-window broadcast (#3524)** — only window with matching `target_window_id` dispatches.
- Steps (a)–(h) are sync-only (CI lint catches; spec exercises with `internal/lualint` harness).
**Done when:** all listed tests pass under fuzz harness preview; conformance §3 clean.

### T-601 · `plugin/wezsesh/ops.lua` (verb dispatch)
**Status:** blocked
**Owner:** lua-plugin-engineer
**Depends-on:** T-600, T-506
**Spec:** §9.4 (verb dispatch table), §6 (verb catalog), §13.13 (unknown-verb); full spike-#2 rationale in `docs/issues/2.md` (save / load dual-path detection; SNAPSHOT_LOAD_FAILED vs RESURRECT_PARTIAL split)
**Files:** `plugin/wezsesh/ops.lua`, `plugin/wezsesh/ops_spec.lua`
**Acceptance gates (from §17.3):**
- **Unknown verb reply** — `op="bogus"` → reply `error.code=UNKNOWN_VERB`, `ok=false`, `status=completed`.
- **Save Lua-side I/O failure (spike-#2)** — chmod-0500 snapshot dir → `with_capture` returns `(true, nil, [resurrect.error msg])` → `SAVE_FAILED`. Phase C MUST be skipped.
- **Save Lua-side encode failure (spike-#2)** — workspace state polluted with a function value → `SAVE_FAILED` with serde_json error string.
- **Save Lua-side success leaves capture empty (spike-#2)** — `#captured == 0`; `completed`.
- **Load: torn JSON → SNAPSHOT_LOAD_FAILED via pcall (spike-#2)** — corrupted plaintext snapshot.
- **Load: silent decrypt failure → SNAPSHOT_LOAD_FAILED via capture (spike-#2)** — wrong-key encrypted snapshot.
- `verb_args_shape` keys equal `dispatch_table` keys (parity gate).
**Done when:** every verb in §6 has a dispatch arm; spike-#2 dual-path detection coverage complete.

### T-602 · `plugin/wezsesh/manager.lua`
**Status:** blocked
**Owner:** lua-plugin-engineer
**Depends-on:** T-601
**Spec:** §9.2 (manager)
**Files:** `plugin/wezsesh/manager.lua`, `plugin/wezsesh/manager_spec.lua`
**Acceptance gates:**
- `wezterm.background_child_process` calls are `pcall`-wrapped (§16.5).
- Spawn invocation matches Appendix A.
**Done when:** spec verifies command construction and env scrub for the binary spawn path.

### T-603 · `plugin/init.lua` (apply_to_config)
**Status:** blocked
**Owner:** lua-plugin-engineer
**Depends-on:** T-600, T-601, T-602, T-506
**Spec:** §9.1 (init.lua), §0.1 rows 29 + 31 (mlua sandbox + cache-bust); full spike-#1 rationale in `docs/issues/1.md` (Ctrl+Shift+R doesn't re-evaluate cached `require()`; `package.loaded` bust loop derivation)
**Files:** `plugin/init.lua`, `plugin/init_spec.lua`
**Acceptance gates:**
- `apply_to_config(config, opts)` enforces `opts.binary` (or `plugin_root`) per §0.1 row 31.
- `package.loaded["wezsesh.*"] = nil` bust loop present (CI lint).
- `resurrect_error.register()` invoked (CI lint).
- Listener subscriptions match Appendix C (no `restore_workspace.finished`; no `smart_workspace_switcher.*`).
- Outer body `pcall`-wrapped.
**Done when:** spec drives `apply_to_config` and verifies sandbox compliance.

### T-604 · `plugin/wezsesh/on_pane_restore.lua`
**Status:** blocked
**Owner:** resurrect-interop-engineer
**Depends-on:** T-202, T-605
**Spec:** §9.11 (on_pane_restore), §13.5 (hook trust), §0.1 row 22 (panic paths)
**Files:** `plugin/wezsesh/on_pane_restore.lua`, `plugin/wezsesh/on_pane_restore_spec.lua`
**Acceptance gates (from §17.3):**
- **Argv hook fail-CLOSED** — forced exception → no `default_on_pane_restore` invocation.
- **Argv allowlist enforcement (Lua side)** — `argv[1]="rm"` → no exec; `cd <cwd>` if cwd clean.
- **Control-char `cwd`/argv** — `cwd="/tmp/foo\nrm -rf ~"` → no injection (downgrade to no-op).
- Single-arg callback shape (`function(pane_tree)`); 1-based argv indexing.
- `bytes_clean(s)` applied to BOTH every argv element AND `cwd`.
**Done when:** all listed gates pass; conformance §7 clean.

### T-605 · codegen `default_allowlist.lua`
**Status:** blocked
**Owner:** resurrect-interop-engineer
**Depends-on:** T-202, T-005
**Spec:** §9.12 (codegen'd allowlist), §17.4 (default-allowlist sync lint)
**Files:** `plugin/wezsesh/default_allowlist.lua` (generated), `internal/argvallow/codegen/main.go` (consumed; lives in T-202)
**Acceptance gates (from §17.3):**
- **Argv default list sync** — `internal/argvallow/default.txt` ↔ `default_allowlist.lua` byte-equal under codegen.
- §16.4 "default_allowlist.lua codegen freshness" gate is wired (`go run ./internal/argvallow/codegen --check`).
**Done when:** CI fails when one is edited without regenerating the other.

---

## Phase 7 — TUI + doctor + pathpicker

### T-700 · `internal/pathpicker`
**Status:** blocked
**Owner:** bubbletea-tui-engineer
**Depends-on:** T-104, T-105
**Spec:** §8.15 (pathpicker), §15.3 (path picker output line)
**Files:** `internal/pathpicker/pathpicker.go`, `internal/pathpicker/pathpicker_test.go`
**Acceptance gates:**
- Picker output validation per §15.3 (rejects malformed lines).
- No direct `wezterm cli` invocation (lint covers).
**Done when:** unit tests cover §15.3 happy + sad paths.

### T-701 · `internal/tui`
**Status:** blocked
**Owner:** bubbletea-tui-engineer
**Depends-on:** T-104, T-300, T-302, T-700, T-203, T-200, T-201, T-401
**Spec:** §8.16 (tui), §13 (state machines), §13.8 (quit-mid-op)
**Files:** `internal/tui/model.go`, `internal/tui/update.go`, `internal/tui/view.go`, `internal/tui/keys.go`, `internal/tui/modal.go`, `internal/tui/preview.go`, `internal/tui/tui_test.go`
**Acceptance gates (from §17.3):**
- **Render-time sanitization** — snapshot named `\x1b[2J` does not clear terminal (verified inside the TUI render path).
- **`tea.Tick` retransmit cancellation** — timer goroutine exits within 100 ms of `tea.Run` return.
- OSC writes go through `internal/uservar.Writer` (conformance §4).
- `StartListener` called from `Update` synchronously, with `defer cleanup()` immediately following.
- Quit-mid-op uses inline status (NOT modal); `op_in_flight` flag tracked.
- No `tea.After` references (CI lint covers).
**Done when:** TUI compiles, `tea.NewProgram(model).Run()` terminates cleanly under each test scenario; conformance §4 clean.

### T-702 · `internal/doctor`
**Status:** blocked
**Owner:** general-purpose
**Depends-on:** T-101, T-105, T-203, T-200, T-201, T-303
**Spec:** §8.17 (doctor); spike-#1 (`docs/issues/1.md`) for the `WEZSESH_UNDER_MULTIPLEXER` derivation; spike-#3 (`docs/issues/3.md`) for `runtime.dir.req_orphans`
**Files:** `internal/doctor/doctor.go`, `internal/doctor/checks.go`, `internal/doctor/doctor_test.go`
**Acceptance gates (from §17.3):**
- **Pin doctor consistency** — `live_pins ∩ saved-names ≠ ∅` → warn.
- **Config Exclude invalid regex** — bad regex → reported.
- `runtime.dir.req_orphans` check from §0.1 row 34 wired.
- `WEZSESH_UNDER_MULTIPLEXER` check from §0.1 row 32.
**Done when:** every check in §8.17 has a positive + negative test.

---

## Phase 8 — CLI subcommands

### T-800 · `cmd/wezsesh/main.go` (startup + routing)
**Status:** blocked
**Owner:** general-purpose
**Depends-on:** T-100, T-101, T-105, T-200, T-201, T-203, T-300, T-301, T-303, T-400, T-701, T-702
**Spec:** §8.20 (cmd/wezsesh), §8.20.1 (startup sequence), §13.14 (panic paths)
**Files:** `cmd/wezsesh/main.go`, `cmd/wezsesh/main_test.go`
**Acceptance gates (from §17.3):**
- **Save Phase C re-hash** — reply.data.hash matches sha256 of file as written by Lua.
- **Save in-process serialisation** — two concurrent same-name saves run sequentially.
- **Save with stale hash (Phase A reject)** — mismatch → `SNAPSHOT_CHANGED`.
- **Save first-write (no expected_hash)** — concurrent first-saves serialise.
- **Save flock serialisation (Phase A)** — `SNAPSHOT_LOCKED` returned to one concurrent caller.
- **Pin sync on save (live → saved)** — sidecar.Pinned=true; state.LivePins removes the entry.
- **Reply `v` field echo** — request `v=1` → reply has `v=1`.
- **Hook env: WEZSESH_LOG survives** — hook sees `$WEZSESH_LOG`; not `$WEZSESH_HMAC_KEY` / `$WEZSESH_PROTO_VERSION` / `$WEZSESH_CONFIG_FILE`.
- **SUN_PATH overflow (Go)** — `IPC_INIT_FAILED`.
- §8.20.1 startup sequence implemented in order; top-level `defer recover()` writes `UNEXPECTED_EXIT` reply via `EmergencyReply`.
**Done when:** TUI path runs end-to-end against a stub plugin; conformance review across every area clean.

### T-801 · `cmd/wezsesh/version.go`
**Status:** blocked
**Owner:** general-purpose
**Depends-on:** T-800
**Spec:** §8.20 (CLI surface)
**Files:** `cmd/wezsesh/version.go`, `cmd/wezsesh/version_test.go`
**Acceptance gates:**
- Prints `main.version` (set by `-ldflags`); exits 0.
**Done when:** `wezsesh --version` produces the linker-set string.

### T-802 · `cmd/wezsesh/keygen.go`
**Status:** blocked
**Owner:** general-purpose
**Depends-on:** T-800
**Spec:** §8.20 (CLI surface)
**Files:** `cmd/wezsesh/keygen.go`, `cmd/wezsesh/keygen_test.go`
**Acceptance gates (from §17.3):**
- **`wezsesh keygen` output** — exits 0; stdout is exactly 65 bytes (64 hex + `\n`); 64-hex matches `^[a-f0-9]{64}$`.
**Done when:** test passes; uses `crypto/rand`.

### T-803 · `cmd/wezsesh/reply.go`
**Status:** blocked
**Owner:** wire-protocol-guardian
**Depends-on:** T-800, T-301
**Spec:** §8.20 (CLI surface), §3.4 (reply payload)
**Files:** `cmd/wezsesh/reply.go`, `cmd/wezsesh/reply_test.go`
**Acceptance gates:**
- Decodes b64 JSON, dials the socket, writes payload, exits.
- Refuses any non-canonical reply shape.
**Done when:** integration with T-301's listener succeeds.

### T-804 · `cmd/wezsesh/list.go`
**Status:** blocked
**Owner:** general-purpose
**Depends-on:** T-800, T-203, T-200
**Spec:** §8.20 (CLI surface)
**Files:** `cmd/wezsesh/list.go`, `cmd/wezsesh/list_test.go`
**Acceptance gates:**
- `--format json` produces stable, machine-parseable output.
- Live + saved + pinned views match the union semantics §13.11 describes.
**Done when:** golden-file tests cover both formats.

### T-805 · `cmd/wezsesh/find.go`
**Status:** blocked
**Owner:** wezterm-interop-engineer
**Depends-on:** T-800, T-401
**Spec:** §8.20 (CLI surface), §13.7 (two-phase find)
**Files:** `cmd/wezsesh/find.go`, `cmd/wezsesh/find_test.go`
**Acceptance gates:**
- Outside-wezterm: prints results only.
- Inside-wezterm: constructs in-process Dispatcher; Phase 1 + Phase 2 sequencing per §13.7.
**Done when:** behavioural tests cover both invocation contexts.

### T-806 · `cmd/wezsesh/trust.go`
**Status:** blocked
**Owner:** trust-and-hooks-engineer
**Depends-on:** T-800, T-201
**Spec:** §8.20 (CLI surface), §13.5 (trust check), §13.5.2 (rebind)
**Files:** `cmd/wezsesh/trust.go`, `cmd/wezsesh/trust_test.go`
**Acceptance gates (from §17.3):**
- **Project sidecar trust enforcement** — untrusted `.wezsesh.json` → no exec; toast surfaces; `wezsesh trust` approves.
- All flags from §8.20 (`--revoke`, `--list`, `--prune`, `--show`, `--path`, `--sidecar`, `--rebind`) implemented and tested.
**Done when:** every flag has a happy + sad path; conformance §6 clean.

### T-807 · `cmd/wezsesh/reset.go` (with `nuke` alias)
**Status:** blocked
**Owner:** safefs-engineer
**Depends-on:** T-800, T-101
**Spec:** §8.20 (CLI surface), §0.1 row 8 (rename + alias)
**Files:** `cmd/wezsesh/reset.go`, `cmd/wezsesh/reset_test.go`
**Acceptance gates (from §17.3):**
- **`wezsesh reset` symlink defense** — pre-placed symlink at state dir → ABORT; pre-placed symlink at `<state>/state.json` → SKIP+WARN.
- **`wezsesh nuke` deprecation alias** — invoking `nuke` runs `reset` and prints deprecation toast.
- **`wezsesh reset --include-snapshots`** — confirmation gate enforced; only on `--yes` does it remove resurrect files.
- `--dry-run` previews everything without writes.
**Done when:** all listed tests pass; symlink defense exercised end-to-end.

### T-808 · `cmd/wezsesh/doctor.go`
**Status:** blocked
**Owner:** general-purpose
**Depends-on:** T-800, T-702
**Spec:** §8.20 (CLI surface)
**Files:** `cmd/wezsesh/doctor.go`, `cmd/wezsesh/doctor_test.go`
**Acceptance gates:**
- `--format json` is parseable; exit code 0 iff all checks PASS.
**Done when:** matches the `internal/doctor` invariants from T-702.

---

## Phase 9 — Integration

### T-900 · End-to-end smoke test (§17.6)
**Status:** blocked
**Owner:** general-purpose
**Depends-on:** T-603, T-800, T-806, T-805
**Spec:** §17.6 (end-to-end smoke)
**Files:** `e2e/smoke_test.go` (`//go:build e2e`), `e2e/Makefile` or `tasks` target
**Acceptance gates:**
- All scenarios 1–6 from §17.6 pass against a real wezterm binary.
- Sidecar gate (scenario 6) sweeps 13 buckets × 100 reps; asserts 0 % loss + 0 orphans.
- `runtime_dir/req/*.json` empty after teardown.
- No panics in either binary; no Lua errors in `wezterm.log`.
**Done when:** dedicated CI job is green.

### T-901 · Lua handler fuzz harness (§17.5)
**Status:** blocked
**Owner:** lua-plugin-engineer
**Depends-on:** T-600, T-601
**Spec:** §17.5 (fuzz mutation classes)
**Files:** `plugin/wezsesh/fuzz/fuzz_spec.lua`, `cmd/lua-fuzzer/main.go`
**Acceptance gates:**
- All 14 mutation classes covered.
- 10 000 mutated bytes per run; no Lua error escapes the handler.
- `ops.dispatch` invocation count = 0 for unauthenticated inputs.
- No reply on socket on HMAC mismatch.
- Frame paint < 50 ms throughout.
**Done when:** harness runs as a CI job, with seeds checked in for regression coverage.
