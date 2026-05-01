# wezsesh — Technical Design v3

Reference spec. Contracts, APIs, schemas, state machines.

This is v3 of the technical design — incorporates the deep review applied
to v2 (DESIGN_V2.md). Major changes summarised in §0.1. Rationale and
audit history live in `PRD_V7.md`; this document does not duplicate them.
PRD section references are given as `(P §x.y)`.

> Conventions:
> - All Go package paths are relative to the module root.
> - All Lua module paths are relative to `plugin/wezsesh/`.
> - "MUST", "MUST NOT", "SHOULD" follow RFC 2119.
> - Type signatures are normative; surrounding prose explains semantics
>   only where the signature is insufficient.

---

## §0 — Reading guide

### §0.1 — Changes from v2

| # | Change | Rationale |
|---|---|---|
| 1 | Save flow no longer holds the OS file lock across the IPC roundtrip. Lock-acquire / hash-verify happen once; lock is released before OSC dispatch; an in-process per-name mutex serialises concurrent wezsesh saves. A second short lock is taken to re-hash after Lua returns. | v2-R1/P1 — locks held across async IPC are a smell, and the lock provides no protection against resurrect's own writer anyway. |
| 2 | `state.json.pins[]` removed. Sidecar `pinned: true\|false` is the single source of truth for saved workspaces; `state.json.live_pins` (renamed) holds pins for live-only workspaces only. The two stores are disjoint by construction; on save, any live pin migrates to the sidecar and is removed from `live_pins`. | v2-P3 — eliminates mirror drift; sidecars are read at startup anyway, so the cache is free. |
| 3 | `seen_ids` is now session-wide (single map keyed by ULID), not per-pane. ULIDs are globally unique; HMAC binds requests; per-pane bucketing added prune complexity with no security upside. | v2-P2 |
| 4 | New `internal/ipcdispatcher` package owns the concrete `Dispatcher`. `cmd/wezsesh` only does flag parsing, DI, and lifecycle. | v2-P4 |
| 5 | Reply payload carries `"v": <int>` (request-version echo) for forward-compat; reply invariants restated in lowercase JSON casing. | v2-#3, R6 |
| 6 | New error code `SAVE_FAILED` covers Lua-side `resurrect.state_manager.save_state` failures (disk full, encryption agent error, etc.). | v2-#9 |
| 7 | New error-code surface category `binary-only` for codes returned by binary-only operations (rename / delete / tag / pin) that never traverse IPC. `RENAME_COLLISION` reclassified. | v2-#6 |
| 8 | `wezsesh nuke` renamed to `wezsesh reset`; new `--include-snapshots` flag for true totality. The old name is kept as an alias with a deprecation toast for one release. | v2-P14/R14 |
| 9 | Hook env scrub narrowed: only the three sensitive vars (`WEZSESH_HMAC_KEY`, `WEZSESH_PROTO_VERSION`, `WEZSESH_CONFIG_FILE`) are dropped. User-tunables (`WEZSESH_LOG`, `WEZSESH_NO_HOOKS`, `WEZSESH_NERDFONT`) survive. | v2-R9 |
| 10 | Centralised symlink-defense policy in `safefs.SymlinkPolicy` enum + `safefs.Enforce` helper; replaces ad-hoc per-site reactions. | v2-P7 |
| 11 | Logger flushes immediately on `Warn` and `Error` (not just on the 1 s tick). | v2-P8 |
| 12 | OSC atomicity rationale rewritten — `/dev/tty` is not a pipe; PIPE_BUF does not apply. The 4 KiB ceiling derives from wezterm's OSC parser buffer + write(2) ergonomics. | v2-R2 |
| 13 | `expected_hash` is always `"sha256:<hex>"`. `snapshots.Repo.Hash` returns the prefixed form; helper `RawHashHex` exists for callers that need the bare hex. Save reply re-hashes after Lua returns. | v2-R7, #10 |
| 14 | Save flow gains a second timeout budget: `lockCtx` (5 s) and `ipcCtx` (5 s) are independent. | v2-R12 |
| 15 | `ct_eq.lua` documents the Lua 5.3+ requirement explicitly; CI gate asserts wezterm ships with mlua/Lua 5.4. | v2-R4 |
| 16 | Per-pane `wezsesh_state.<pid>.hmac_key` removed (was redundant with session-wide key). | v2-#4 |
| 17 | `state.SetPin` ctx-parameter inconsistency fixed; all callsites pass ctx. | v2-#2 |
| 18 | `find` Phase-1 drain protocol specified: ctx-cancel after poller success closes the listener; consumer drains the channel until it returns `ok=false`. The "switch to live target gets `started`" comment was wrong — live targets reply `completed`; updated accordingly. | v2-#7, R17 |
| 19 | Switch poller worst-case latency budget is documented (each tick may take up to 4 s on a slow mux); cadence becomes adaptive. | v2-R15 |
| 20 | New §13.5.2 trust rebind UX (`wezsesh trust --rebind <old> <new>`) for moved sidecars. | v2-R11 |
| 21 | New §13.13 unknown-verb behaviour: Lua's `ops.dispatch` replies with `error.code=UNKNOWN_VERB`, `ok=false`, terminal `completed`. The "treated as noop" wording is dropped. | v2-#5 |
| 22 | New §13.14 panic paths for non-TUI subcommands. | v2-R13 |
| 23 | New §17.6 end-to-end smoke test contract. | v2-P10 |
| 24 | `__wezsesh_canonical = "untagged"` outlawed; encoder shape table is the single tagging mechanism. Numbering note: v2's §0.1 referenced §13.11 / §13.12 / §13.13 for sort / pin / binary-only flows, but the actual numbers were §13.10 / §13.11 / §13.12. v3 keeps the in-document numbering and updates this changelog accordingly. | v2-#1 |
| 25 | Threat-model assumption made explicit (Appendix D): single-user host. `wezsesh keygen` exit path is unchanged; the assumption is now a documented precondition. | v2-P9 |
| 26 | Unicode sort caveat made explicit (§13.10): alphabetical sort is byte-order over NFC-normalised UTF-8 — locale-naive. Locale-aware ordering deferred. | v2-R10 |
| 27 | Config-vs-env precedence specified explicitly (§11.4) and documented as a single resolution table. | v2-R16 |

### §0.2 — Document map

| Section | Purpose |
|---|---|
| §1 | Scope |
| §2 | Repository layout |
| §3 | IPC wire protocol |
| §4 | Canonical JSON encoder |
| §5 | HMAC, freshness, replay |
| §6 | IPC verb catalog (5 verbs) |
| §7 | Error code catalog |
| §8 | Go package APIs |
| §9 | Lua module APIs |
| §10 | Persistent data schemas |
| §11 | Configuration schema |
| §12 | Filesystem contracts |
| §13 | State machines |
| §14 | Concurrency & timeouts |
| §15 | Validation rules |
| §16 | Build, dependencies, lint rules |
| §17 | Testing contracts |
| Appendix A | Spawn invocation |
| Appendix B | Encryption magic-byte sniff |
| Appendix C | Resurrect events subscribed |
| Appendix D | Threat-model assumptions |

---

## §1 — Scope

This document specifies the runtime contracts a developer needs to
build and ship wezsesh v0.1. It does NOT specify:
- Threat-model rationale (in PRD; assumptions summarised in Appendix D)
- Audit history (in PRD round-by-round findings)
- UI copy text (in TUI ticket spec)
- Iteration backlog (in `PROJECT.md` once tickets land)

---

## §2 — Repository layout

```
wezsesh/
├── plugin/
│   ├── init.lua                                 §9.1
│   └── wezsesh/
│       ├── manager.lua                          §9.2
│       ├── ipc.lua                              §9.3
│       ├── ops.lua                              §9.4
│       ├── result.lua                           §9.5
│       ├── state.lua                            §9.6
│       ├── canonical_json.lua                   §9.7
│       ├── hmac.lua                             §9.8
│       ├── ct_eq.lua                            §9.9
│       ├── b64.lua                              §9.10
│       ├── on_pane_restore.lua                  §9.11
│       ├── default_allowlist.lua                §9.12   (codegen'd, see §8.12)
│       └── vendor/
│           ├── sha2.lua                         pinned commit, MIT
│           └── SOURCES.lock                     upstream commit + sha256
├── cmd/
│   └── wezsesh/
│       ├── main.go                              §8.20
│       ├── reply.go                             §8.20
│       ├── doctor.go                            §8.20
│       ├── list.go                              §8.20
│       ├── find.go                              §8.20
│       ├── trust.go                             §8.20
│       ├── keygen.go                            §8.20
│       ├── reset.go                             §8.20  (was: nuke.go)
│       └── version.go                           §8.20
├── internal/
│   ├── safefs/                                  §8.1
│   │   ├── safefs.go
│   │   ├── lockedfile.go                        LockedFile type
│   │   ├── lock_linux.go                        //go:build linux
│   │   ├── lock_other.go                        //go:build !linux
│   │   ├── netfs.go                             IsNetworkFS
│   │   └── symlinkpolicy.go                     SymlinkPolicy + Enforce
│   ├── canonicaljson/                           §8.2
│   ├── hmac/                                    §8.3
│   ├── nameval/                                 §8.4
│   ├── ipc/                                     §8.5    Dispatcher interface
│   ├── ipcdispatcher/                           §8.6    concrete Dispatcher impl
│   ├── ipcsock/                                 §8.7
│   ├── uservar/                                 §8.8
│   ├── wezcli/                                  §8.9
│   ├── snapshots/                               §8.10
│   ├── state/                                   §8.11
│   ├── trust/                                   §8.12
│   ├── argvallow/                               §8.13
│   │   └── default.txt                          (//go:embed; source of truth)
│   ├── find/                                    §8.14
│   ├── pathpicker/                              §8.15
│   ├── tui/                                     §8.16
│   ├── doctor/                                  §8.17
│   ├── logger/                                  §8.18
│   ├── config/                                  §8.19
│   └── lualint/                                 (CI lint helper, §17.4)
├── flake.nix
├── go.mod                                       (Go 1.26.2 pinned)
└── LICENSE                                      MIT
```

Lint rules (CI):
- No Go file outside `internal/safefs/` may use `os.OpenFile` /
  `os.WriteFile` / `syscall.Open` for paths under wezsesh-managed dirs
  (state, data, snapshot, runtime).
- Direct `wezterm cli` invocation outside `internal/wezcli/` is forbidden.
- `unix.F_OFD_SETLK` outside `internal/safefs/lock_linux.go` fails the build.
- Concrete Dispatcher construction outside `internal/ipcdispatcher/`
  fails the build (grep `ipcsock.StartListener` callsites).

---

## §3 — IPC wire protocol

### §3.1 — Forward path: binary → Lua

Bytes written to `/dev/tty` (NOT `os.Stdout`), under
`internal/uservar.Writer.mu`:

```
ESC ] 1337 ; SetUserVar=wezsesh_op= <base64-payload> BEL
0x1B 0x5D "1337;SetUserVar=wezsesh_op=" <b64> 0x07
```

`<b64>` is `base64.StdEncoding` of the canonical-JSON request bytes
(§3.3). No line wrap, no padding tweaks.

**Atomicity rationale.** `/dev/tty` is a character device backed by the
controlling terminal driver; PIPE_BUF semantics do NOT apply (PIPE_BUF
governs `pipe(2)` and FIFOs only). wezterm's OSC parser buffers bytes
across reads until it sees the `ESC \` / `BEL` terminator, so partial
writes do not corrupt the sequence. The 4 KiB request ceiling (§3.5) is
self-imposed for two reasons:
1. `write(2)` to a non-blocking TTY rarely returns short for ≤ 4 KiB
   payloads in practice (the kernel TTY buffers are 4 KiB on Linux,
   ≥ 1 KiB on darwin), keeping the common-case write a single syscall;
2. it gives the OSC parser a hard upper bound for memory budgeting.

The `internal/uservar.Writer.mu` mutex serialises wezsesh's own writes;
it does NOT prevent interleaving with bubbletea's renderer (which also
writes to `/dev/tty`). Wezterm's OSC parser handles interleaved
non-OSC bytes correctly because OSC is delimited.

### §3.2 — Reverse path: Lua → binary

Per-request Unix-domain socket created by the binary BEFORE OSC
emission. Lua replies via `wezsesh reply <sock> <b64>` subcommand
(spawned with `wezterm.background_child_process`, wrapped in `pcall`).

**Socket path** — full path MUST satisfy `len(path) + 14 ≤ SUN_PATH`
(SUN_PATH = 104 darwin, 108 Linux):

```
Linux : $XDG_RUNTIME_DIR/wezsesh/<8-hex>.sock
        (fallback when $XDG_RUNTIME_DIR unset: /tmp/wezsesh-<uid>/<8-hex>.sock)
darwin: /tmp/wezsesh-<uid>/<8-hex>.sock
```

`<8-hex>` is the first 8 hex chars of the request ULID. `.sock` is
mandatory.

**Permissions.**
- Reply dir: mode 0700; `safefs.Enforce(SymlinkRefuse)` rejects symlink
  ancestors.
- Sock file: born 0600 via `unix.Umask(0077)` before `net.Listen`;
  `os.Chmod(sock, 0o600)` is a backstop.

### §3.3 — Request payload (canonical JSON before base64)

```jsonc
{
  "v": 1,
  "id": "<26-char Crockford-base32 ULID>",
  "ts": <int unix-seconds>,
  "target_window_id": <int>,
  "reply_sock": "<absolute-path>",
  "op": "<verb>",
  "args": { /* tagged object; verb-specific, §6 */ },
  "hmac": "<64 lowercase hex chars>"
}
```

All fields REQUIRED **on requests**. Field order in canonical JSON is
unsigned UTF-8 byte order
(`args`, `hmac`, `id`, `op`, `reply_sock`, `target_window_id`, `ts`,
`v`).

### §3.4 — Reply payload (Unix socket, JSON, no envelope)

Single JSON object, one `write` per reply, ≤ 1 MiB. Reader uses an
`io.LimitedReader`; oversize is dropped + logged.

```jsonc
{
  "v": 1,
  "id": "<request ULID>",
  "status": "completed" | "started" | "partial",
  "ok": true | false,
  "data"?: { /* verb-specific */ },
  "warnings"?: [ { "code": "<error-code>", "message": "<str>", "details"?: {} } ],
  "error"?: { "code": "<error-code>", "message": "<str>", "details"?: {} }
}
```

`v` echoes the request's `v`. `data`, `warnings`, and `error` are
OPTIONAL on replies; the others are REQUIRED.

**Invariants** (lowercase keys reference the wire shape):
- `ok == (error is absent)` for all status values.
- `status == "started"` ⇒ `ok = true`, no `data` / `warnings` / `error`.
- `status == "completed"`, `ok = true` ⇒ `data` MUST be present (may be `{}`).
- `status == "completed"`, `ok = false` ⇒ `error` MUST be present.
- `status == "partial"` ⇒ `ok = true`, `data` AND `warnings` MUST be present.

**Status semantics.**
- `completed` — terminal.
- `started` — non-terminal; emitted only by restore-class verbs (§6.1.1).
  TUI dismisses immediately. A `completed` or `partial` follow-up MUST
  arrive within 30 s additional (§14.1).
- `partial` — terminal; success-with-warnings (e.g., `RESURRECT_PARTIAL`).

### §3.5 — Hard ceilings

| Limit | Value | Source |
|---|---|---|
| Request canonical-JSON size | 4 KiB | self-imposed; TTY/parser ergonomics |
| Reply payload size | 1 MiB | `io.LimitedReader` cap |
| First-reply ceiling | 5 s | TUI surfaces `IPC_TIMEOUT` |
| Follow-up after `started` | 30 s | non-fatal toast on overrun |
| Single-retransmit at | 2 s | `tea.Tick` Cmd, idempotent guard |
| Reply dir cleanup mtime | 60 s | startup sweep |
| Reply channel buffer | 2 | exactly fits split-reply (started + terminal) |

(`delete` no longer goes over OSC; bulk size limit removed.)

The reply channel buffer reduction from v2's 4 to 2 is intentional —
sequential accept (§13.2) plus the at-most-2 messages per request bound
the steady-state queue depth at 2.

---

## §4 — Canonical-JSON spec (Go ⇄ Lua byte-equality)

Both encoders MUST produce byte-identical output for identical inputs.
CI gate: golden corpus (§17.1) under `LC_ALL=C`.

### §4.1 — Encoding rules

1. **Object keys** — sorted by unsigned UTF-8 byte order, recursively.
   - Go: `sort.Strings(keys)`.
   - Lua: `table.sort(keys, function(a, b) return a < b end)`.
2. **Whitespace** — none. No spaces, no newlines.
3. **Numbers** — integer only, range `[-2^63, 2^63-1]`. Decimal ASCII,
   optional leading `-`, no leading zeros except `"0"`.
   - Reject: floats, NaN, ±Inf, scientific notation.
   - Lua: `assert(math.type(n) == "integer")`.
   - Go: `strconv.FormatInt(n, 10)`; reject `float32`/`float64` reflectively.
4. **Strings** — UTF-8. Both sides MUST validate (`utf8.ValidString`
   on Go; pure-Lua validator on Lua).
   - Escape `\\` for U+005C; `\"` for U+0022.
   - Escape `\u00XX` (lowercase hex) for U+0000–U+001F, U+007F,
     U+0080–U+009F (when present as valid UTF-8).
   - Escape ` ` / ` ` for U+2028 / U+2029.
   - **FORBIDDEN**: short-form `\b \f \n \r \t`.
   - **NEVER ESCAPED**: forward slash `/` (U+002F).
   - All other code points ≥ U+0020 emitted as raw UTF-8 bytes.
5. **Booleans** — `true` / `false`. Lowercase.
6. **Null** — `null`. Lua side uses `canonical_json.NULL` sentinel
   (never emit for `nil`).
7. **Arrays vs objects (Lua disambiguation)** — wrapper-function
   metatables; untagged tables are an encoder error.
   ```lua
   M.array_mt  = { __wezsesh_canonical = "array" }
   M.object_mt = { __wezsesh_canonical = "object" }
   M.array(t)   -- setmetatable(t or {}, M.array_mt)
   M.object(t)  -- setmetatable(t or {}, M.object_mt)
   M.NULL       -- setmetatable({}, { __wezsesh_canonical = "null" })
   ```
   Empty object MUST emit `{}`. Empty array MUST emit `[]`.
   Untagged tables are a hard encoder error (`ENCODER_UNTAGGED_TABLE`);
   the only legitimate shape source is the verb-aware tagger in §4.2.
8. **Recursion** — all rules apply at every nesting level.

### §4.2 — Verb-aware tagging on the verifier path (Lua)

`wezterm.json_parse` returns plain Lua tables with no shape metatables.
For an empty inner table the parser cannot tell whether it was `{}` or
`[]`. Therefore the verifier MUST tag the parsed payload using a
**static, verb-keyed shape table** before re-encoding for HMAC.

Why not a simpler scheme:
- "Forbid empty containers" — would force every reply schema to seed a
  placeholder key, and would not survive future verbs with optional
  array args.
- "Sentinel key (e.g., `__type: array`)" — pollutes the JSON shape and
  forces both sides to filter the sentinel.
- "Always object, never array" — loses ordering for verbs that may need
  arrays in the future (none today; this would be one-way door).

The shape-table approach keeps wire JSON clean, has zero runtime cost
on the happy path (one extra map lookup), and the verb count is small
and stable.

```lua
-- canonical_json.lua exports verb_args_shape (the canonical shape
-- declarations for every verb's args object).
local verb_args_shape = {
    switch = { _shape = "object", name = "string" },
    load   = { _shape = "object", name = "string" },
    save   = { _shape = "object",
               name = "string", overwrite = "bool",
               expected_hash = "string_or_null" },
    new    = { _shape = "object", name = "string", cwd = "string" },
    noop   = { _shape = "object" },  -- empty
}
```

The encoder exposes:

```lua
-- Walk t recursively, applying tags from shape. shape may be a string
-- ("string"|"int"|"bool"|"string_or_null"), an object spec, or an array
-- spec ({ _shape="array", _of=<spec> }). Returns t with tags attached.
function M.tag_in_place(t, root_shape, shape)  end
```

Step (e) of `ipc.lua` (§9.3) calls `tag_in_place(payload,
ROOT_PAYLOAD_SHAPE, verb_args_shape[op])` after step (d) field-shape
validation has populated `op`. Adding a verb requires a corresponding
shape entry; CI lint (§17.4) enforces parity between
`ops.dispatch_table` keys and `verb_args_shape` keys.

### §4.3 — HMAC field-removal sequence (both signer and verifier)

1. Construct payload struct WITHOUT `hmac`.
2. Canonical-encode (§4.1).
3. Compute `HMAC-SHA-256(bytes, key)`.
4. Hex-encode digest (lowercase).
5. Set `hmac` field.
6. Re-encode for wire.
7. Verifier: parse → REMOVE `hmac` key (do not zero) → verb-aware tag
   (§4.2) → canonical-encode → recompute HMAC → constant-time compare.

The byte sequence at step 2 MUST NOT contain `"hmac":""`. Forbidden
alternative ("set empty then encode") produces different bytes.

---

## §5 — HMAC, freshness, replay

### §5.1 — Key

- `WEZSESH_HMAC_KEY` is 64 lowercase hex chars (32 raw bytes / 256
  bits). Generated once per wezterm session via `wezsesh keygen` from
  Go's `crypto/rand`.
- Both sides hex-decode to 32 raw bytes BEFORE feeding to HMAC.
- 32 bytes < SHA-256 block size (64); HMAC's "long-key rehash" path is
  not exercised.

The key is stored in `wezterm.GLOBAL.wezsesh_session_key` (one location,
session-wide). Per-pane storage from v2 has been removed.

### §5.2 — Generation chain (plugin load)

```
1. wezterm.run_child_process({wezsesh_bin_path, "keygen"}) → 64 hex + "\n"
   (binary exits 0 on success; non-zero on /dev/urandom failure)
2. fallback: io.open("/dev/urandom", "rb") → read 32 bytes → hex-encode
3. fallback: hard-fail. Toast + log_error. wezsesh_session_key = nil.
   Listener early-returns on nil key.
```

The build matrix is POSIX-only (Linux + darwin); step 2's `/dev/urandom`
read is therefore always available on supported platforms.

The Lua side trims whitespace and validates shape `^%x+$` and length
`64` before storing in `wezterm.GLOBAL.wezsesh_session_key`.

### §5.3 — Freshness

`|os.time() - payload.ts| > 30` seconds → reject `STALE_PAYLOAD`. Check
runs AFTER HMAC verify (so attackers cannot use stale-ts to spam logs).

### §5.4 — Replay

Session-wide `seen_ids` keyed by request ULID:
`wezterm.GLOBAL.wezsesh_seen_ids[<ulid>] = { ts = os.time() }`.
If present → silent drop.

ULIDs are 128-bit; collision probability across a session is negligible.
Per-pane bucketing (v2) added prune complexity with no security benefit
because HMAC already binds a request to the session key.

### §5.5 — TTL prune

Triggered on `window-config-reloaded` and at end of every dispatch
(after `seen_ids` write-back, never before). Drop entries where
`ts < os.time() - 60`.

Applies to:
- `wezsesh_seen_ids[ulid]`
- `wezsesh_state[pid]` itself (drop if `spawned_at < now - 60`)
- `wezsesh_requests[id]` (drop if `started_at < now - 60`)

### §5.6 — Constant-time compare

`ct_eq.eq(a, b)` (§9.9). Lua's `==` short-circuits and leaks timing.
Used at HMAC compare ONLY; field-shape assertions use `==` normally.

---

## §6 — IPC verb catalog

Five verbs: `switch`, `load`, `save`, `new`, `noop`. Operations
that are pure filesystem (`rename`, `delete`, `tag`, `pin`) are
binary-only and NOT IPC verbs — see §13.12.

Per-verb args + reply spec. Reply envelope (§3.4) is identical across
verbs.

### §6.0 — Universal errors (any verb)

These can fire on any verb that traverses the wire and are listed once
here, not repeated in each verb's table:

| Code | Origin |
|---|---|
| `HMAC_MISMATCH` | (silent on the wire — see §7) |
| `STALE_PAYLOAD` | freshness check after HMAC |
| `REPLAY` | (silent on the wire) |
| `FOREIGN_PANE` | (silent on the wire) |
| `IPC_TIMEOUT` | TUI-side timer (5 s first reply) |
| `UNEXPECTED_EXIT` | Go panic-recover wrote sentinel reply |
| `IPC_INIT_FAILED` | `net.Listen` setup failed (incl. SUN_PATH) |
| `XDG_PATH_TIMEOUT` | state/trust read exceeded 2 s ctx |
| `UNKNOWN_VERB` | unknown `op` value (terminal `error.code`) |
| `UNKNOWN` | uncategorised |

### §6.1 — `switch`

Switches to `name`. If `name` is saved-not-live, also restores.

```jsonc
"args": { "name": "<workspace-name>" }
```

Reply:
- Live target: `completed`, `data: { active_workspace: "<name>" }`.
- Saved-not-live target: split reply — `started`, then
  `completed`/`partial` after restore. Same socket; accept loop
  continues (§13.2).
- Already on target: `completed` immediately (poller short-circuits via
  the `target == pre.ActiveWorkspace` clause).

Verb-specific errors (terminal): `MUX_UNREACHABLE`, `SNAPSHOT_MISSING`,
`SNAPSHOT_LOAD_FAILED`, `ILLEGAL_NAME`.

Verb-specific warnings (partial): `RESURRECT_PARTIAL`.

#### §6.1.1 — Restore-class verbs

`switch` (when target is saved-not-live) and `load` are the only verbs
that emit `started` replies. Detection: the Lua-side dispatch handler
in `ops.lua` checks the pre-state (live in mux vs saved-only on disk)
before invoking restore. TUI dismisses on `started`; binary stays alive
to receive the follow-up reply (§13.1).

### §6.2 — `load`

Restore `name` snapshot into the **current** workspace.

```jsonc
"args": { "name": "<workspace-name>" }
```

Reply: `started` immediately, then `completed`/`partial`.
On success: `data: { name: "<name>", workspace: "<active-after-restore>" }`.

Verb-specific errors (terminal): `SNAPSHOT_MISSING`,
`SNAPSHOT_LOAD_FAILED`, `ILLEGAL_NAME`.

Verb-specific warnings (partial): `RESURRECT_PARTIAL`.

### §6.3 — `save`

Snapshot current workspace.

```jsonc
"args": {
  "name":          "<workspace-name>",
  "overwrite":     false,
  "expected_hash": "sha256:<hex>" | null
}
```

Reply: `completed`. `data: { name: "<name>", hash: "sha256:<hex>" }`.
The `hash` returned is the SHA-256 of the file *as written by Lua*; the
binary re-reads under a brief second lock to compute it (§13.4).

Verb-specific errors (terminal): `SNAPSHOT_CHANGED`, `SNAPSHOT_LOCKED`,
`SAVE_FAILED`, `ILLEGAL_NAME`, `MUX_UNREACHABLE`.

`expected_hash`: SHA-256 over the on-disk file bytes (encrypted or
plaintext; we never look inside), prefixed `sha256:`. `null` = first
save (file does not yet exist); `safefs.AcquireExclusiveOrCreate` is
used (§13.4).

### §6.4 — `new`

Create new workspace from a directory.

```jsonc
"args": {
  "name": "<~-collapsed-display-name>",
  "cwd":  "<absolute-path>"
}
```

Reply: `completed`. `data: { name: "<name>", pane_id: <int> }`.

Verb-specific errors (terminal): `ILLEGAL_NAME`, `MUX_UNREACHABLE`.

After spawn, the binary triggers project-sidecar trust check (§13.5)
for `<cwd>/.wezsesh.json`. Hook execution is independent of this
verb's reply.

### §6.5 — `noop`

TUI cancellation marker. No-op on Lua side.

```jsonc
"args": {}
```

Reply: `completed`. `data: {}`.

Unknown verbs emit a terminal `completed` reply with
`ok=false, error.code=UNKNOWN_VERB` (§13.13). They do NOT degrade to
noop semantics; the wording in v2 was misleading.

---

## §7 — Error codes

Stable string identifiers. Additive — never renamed. UI copy is keyed
off the code.

The "Surface" column distinguishes:
- `error.code` — terminal failure on a wire reply (`ok=false`).
- `warnings[].code` — partial success on a wire reply (`ok=true`,
  `status="partial"`).
- `wire-silent` — no reply written (still logged).
- `tui-only` — surfaced by the TUI itself, never on the wire.
- `doctor-only` — emitted only by the doctor path.
- `binary-only` — returned by binary-only operations (rename / delete /
  tag / pin) directly to the TUI in-process; no wire involvement.

| Code | Trigger | Status | Surface | Recovery |
|---|---|---|---|---|
| `SNAPSHOT_CHANGED` | `save` `expected_hash` mismatch | `completed` | `error.code` | TUI re-prompts |
| `SNAPSHOT_MISSING` | snapshot file gone between picker open and op | `completed` | `error.code` / `binary-only` | TUI re-lists |
| `SNAPSHOT_LOCKED` | `safefs.AcquireExclusive` ctx timeout (5 s) | `completed` | `error.code` / `binary-only` | TUI offers retry |
| `SNAPSHOT_LOAD_FAILED` | resurrect `load_state` error / decryption rejected / parse error | `completed` | `error.code` | toast |
| `SAVE_FAILED` | resurrect `save_state` error (disk full, encryption agent error, etc.); error message in `details.raw_error` | `completed` | `error.code` | toast w/ details |
| `RESURRECT_PARTIAL` | restore `pcall` caught error mid-restore | `partial` | `warnings[].code` | toast w/ details |
| `ILLEGAL_NAME` | name/tag fails §15 validation | `completed` | `error.code` / `binary-only` | TUI re-prompts (with `details.field`) |
| `MUX_UNREACHABLE` | `wezterm cli` 2 s ctx timeout / exit 1 / invalid JSON / poller ceiling | `completed` | `error.code` / `binary-only` | toast; doctor hint |
| `HMAC_MISMATCH` | Lua HMAC verify failed | n/a | `wire-silent` | log_error; TUI hits `IPC_TIMEOUT` |
| `FOREIGN_PANE` | OSC from pane not in `wezsesh_state` | n/a | `wire-silent` | log_warn |
| `STALE_PAYLOAD` | `|now - ts| > 30` | `completed` | `error.code` | log_warn (only fired post-HMAC) |
| `REPLAY` | duplicate `id` in `seen_ids` | n/a | `wire-silent` | (deduplication) |
| `IPC_TIMEOUT` | first-reply 5 s ceiling exceeded | n/a | `tui-only` | toast |
| `UNKNOWN_VERB` | unknown `op` field | `completed` | `error.code` | log_warn |
| `UNEXPECTED_EXIT` | binary panic-recover wrote sentinel reply | `completed` | `error.code` | toast |
| `PANE_CLOSED_RACE` | `cli activate-pane` exit 1 twice | `completed` | `error.code` / `binary-only` | toast `target pane closed; refresh and retry` |
| `XDG_PATH_TIMEOUT` | 2 s ctx exceeded reading `state.json` / trust file | `completed` | `error.code` | toast w/ remediation |
| `IPC_INIT_FAILED` | `net.Listen` failed (incl. SUN_PATH overflow) | n/a | (binary exit) | toast on Lua side |
| `ENCRYPTION_AGENT_SLOW` | `gpg`/`age` `--version` doctor probe > 2 s | n/a | `doctor-only` | non-fatal hint |
| `PATH_PICKER_TIMEOUT` | path picker 15 s ctx exceeded | n/a | `tui-only` | toast |
| `PATH_PICKER_CMD_FAILED` | path picker tool exited non-zero | n/a | `tui-only` | toast w/ first 256 B of stderr |
| `NO_PATH_PROVIDER` | no zoxide/fd on PATH and no override | n/a | `tui-only` | toast w/ install hint |
| `RENAME_COLLISION` | rename target already exists (live or saved) | n/a | `binary-only` | TUI re-prompts |
| `TRUST_REBIND_MISSING` | `wezsesh trust --rebind <old> <new>` source absent | n/a | (CLI exit) | exit non-zero |
| `UNKNOWN` | uncategorised | `completed` | `error.code` | toast |

`details` field shapes:
- `ILLEGAL_NAME` — `{ field: "name"|"tags[i]", reason: "<human>" }`.
- `RESURRECT_PARTIAL` — `{ raw_error: "<lua-error-message>" }`.
- `SAVE_FAILED` — `{ raw_error: "<lua-error-message>" }`.
- `PATH_PICKER_CMD_FAILED` — `{ stderr_head: "<first 256 bytes>" }`.
- `RENAME_COLLISION` — `{ existing: "live"|"saved" }`.
- Others: `{}` or absent.

Adding a new code requires updating: this table, the relevant package's
emitter, the TUI copy table (`internal/tui/strings.go`), and the
canonical-JSON fixture corpus (§17.1) if surfaced via reply.

---

## §8 — Go package APIs

Each subsection lists exported symbols and their semantics. Internal
helpers are out of scope.

### §8.1 — `internal/safefs`

```go
package safefs

// LockedFile is the only public handle to a locked file. It deliberately
// does NOT expose Close — release via the closure returned by
// AcquireExclusive / AcquireExclusiveOrCreate is the only permitted path.
// Closing any other fd to the same file would silently drop the lock.
type LockedFile struct { /* unexported */ }
func (lf *LockedFile) ReadAt(p []byte, off int64) (int, error)
func (lf *LockedFile) WriteAt(p []byte, off int64) (int, error)
func (lf *LockedFile) Truncate(size int64) error
func (lf *LockedFile) Sync() error
func (lf *LockedFile) Stat() (os.FileInfo, error)
func (lf *LockedFile) Size() (int64, error)
// ReadAll reads the entire file via repeated ReadAt; respects ctx
// cancellation between syscalls.
func (lf *LockedFile) ReadAll(ctx context.Context) ([]byte, error)
// WriteAll truncates and writes; respects ctx between syscalls.
func (lf *LockedFile) WriteAll(ctx context.Context, p []byte) error

// AtomicWriteFile writes data to <parentDir>/<filename> via openat(2)
// + temp-then-rename on a verified parent dirfd. perm is the final mode.
func AtomicWriteFile(ctx context.Context, parentDir, filename string, data []byte, perm fs.FileMode) error

// VerifyDir opens parentDir with O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC.
// Returns dirfd + Stat. Errors if symlink or not a directory. Caller
// closes fd.
func VerifyDir(parentDir string) (fd int, info fs.FileInfo, err error)

// SafeOpenForRead opens path with O_NOFOLLOW|O_CLOEXEC. Errors with
// ELOOP if final component is a symlink.
func SafeOpenForRead(path string) (*os.File, error)

// AcquireExclusive opens path under a verified dirfd and acquires a
// POSIX advisory lock.
//   Linux: F_OFD_SETLK (lock_linux.go)
//   Other: F_SETLK with single-fd discipline (lock_other.go)
// Polls non-blocking with 10ms→100ms exponential backoff (capped) until
// ctx is Done. Logs WARN-at-1s and WARN-at-3s on contended waits.
// Returns ErrLockTimeout when ctx expires; ErrNotExist if file missing.
func AcquireExclusive(ctx context.Context, path string) (*LockedFile, func(), error)

// AcquireExclusiveOrCreate is like AcquireExclusive but creates the
// file with O_CREAT|O_RDWR|O_NOFOLLOW|O_CLOEXEC if missing. Used by
// first-save flow (§13.4). perm is the mode if the file is created
// (typically 0600).
func AcquireExclusiveOrCreate(ctx context.Context, parentDir, filename string, perm fs.FileMode) (*LockedFile, func(), error)

// IsNetworkFS classifies path as local vs network/cloud-sync.
func IsNetworkFS(path string) (fsType, fsLayer string, isNetwork bool, err error)

// Errors
var ErrIsSymlink   = errors.New("safefs: path is a symlink")
var ErrLockTimeout = errors.New("safefs: lock acquire timed out")
var ErrNotExist    = errors.New("safefs: file does not exist")
```

#### §8.1.1 — Symlink policy (centralised)

```go
// SymlinkPolicy is the single enum used at every site that touches a
// path that might be a symlink. Replaces v2's ad-hoc per-site reactions.
type SymlinkPolicy int

const (
    // SymlinkRefuse — error out. Used for top-level dirs (state, data,
    // runtime, snapshot, trust). Failure is hard.
    SymlinkRefuse SymlinkPolicy = iota
    // SymlinkSkipWarn — log_warn, treat as absent / skip. Used for
    // individual files inside managed dirs (sidecars, sock files,
    // log files, trust files) during sweeps and resets.
    SymlinkSkipWarn
    // SymlinkRejectOp — return ErrIsSymlink to the caller; caller
    // decides surfacing. Used by SafeOpenForRead.
    SymlinkRejectOp
)

// Enforce performs Lstat and applies policy. Returns:
//   ok=true,  err=nil      → not a symlink, proceed
//   ok=false, err=nil      → symlink, policy was SkipWarn (already logged)
//   ok=false, err=non-nil  → symlink, policy was Refuse or RejectOp
//
// Use this helper at every site that previously reasoned about
// symlinks ad-hoc.
func Enforce(path string, policy SymlinkPolicy, log *logger.Logger) (ok bool, err error)

// SafeRemove unlinks path after Enforce(SymlinkRejectOp).
func SafeRemove(path string) error

// SafeRemoveTree recursively removes path with Enforce at every step.
// File-level symlinks: SkipWarn (do not unlink). Directory symlinks
// inside the tree: SkipWarn. Top-level path symlink: Refuse.
func SafeRemoveTree(path string, log *logger.Logger) error
```

**Build-tag split — mandatory.**

```go
// internal/safefs/lock_linux.go
//go:build linux
// uses unix.F_OFD_SETLK directly

// internal/safefs/lock_other.go
//go:build !linux
// uses unix.F_SETLK with single-fd discipline
```

CI lint: any reference to `unix.F_OFD_SETLK` outside `lock_linux.go` is
a build error.

**Layer 2 prefix list (darwin only)** for `IsNetworkFS`. NFC-normalize
both the resolved path and the prefix anchors at the same step. Match
`strings.EqualFold` on darwin, case-sensitive on Linux.

```
~/Library/Mobile Documents/                (verified — Apple)
~/Library/CloudStorage/iCloud~*            (verified — Apple)
~/Library/CloudStorage/Dropbox*            (verified)
~/Library/CloudStorage/GoogleDrive*        (verified)
~/Library/CloudStorage/OneDrive*           (verified)
~/Library/CloudStorage/Box*                (verified — 2024+)
~/Library/CloudStorage/Nextcloud*          (community FP variants)
~/Library/CloudStorage/Proton*             (best-effort, unverified)
~/Library/CloudStorage/Seafile*            (best-effort, unverified)
~/iCloud Drive                             (also EvalSymlinks → resolve)
~/Dropbox                                  (legacy)
~/Nextcloud                                (standard NextCloud Desktop)
~/Google Drive  /Volumes/GoogleDrive*      (vestigial Apple Silicon)
~/Desktop, ~/Documents                     (only when iCloud "Desktop & Documents" enabled)
```

### §8.2 — `internal/canonicaljson`

```go
package canonicaljson

// Marshal encodes v per §4.1 rules. Returns ErrFloat for any float
// subtype, ErrInvalidUTF8 for invalid UTF-8 strings, ErrUnsupported
// for unsupported types.
func Marshal(v any) ([]byte, error)

var (
    ErrFloat       = errors.New("canonicaljson: float not allowed")
    ErrInvalidUTF8 = errors.New("canonicaljson: invalid UTF-8")
    ErrUnsupported = errors.New("canonicaljson: unsupported type")
)
```

There is no `Unmarshal`. Reply parsing uses `encoding/json` —
canonicality is a property of *outbound* bytes only.

### §8.3 — `internal/hmac`

```go
package hmac

// Signer caches the decoded key. Construct once per request;
// reusable across Sign/Verify calls.
type Signer struct { /* unexported */ }

// NewSigner hex-decodes key (must be 64 lowercase hex chars).
func NewSigner(hexKey string) (*Signer, error)

// Sign canonical-encodes payload (sans "hmac" key, per §4.3),
// computes HMAC-SHA-256, returns lowercase hex digest.
func (s *Signer) Sign(payload map[string]any) (string, error)

// Verify recomputes the digest and constant-time compares against
// payload["hmac"]. Removes "hmac" from a copy of payload before
// recomputing. Returns false for any error during recompute.
func (s *Signer) Verify(payload map[string]any) (bool, error)

var ErrBadKey = errors.New("hmac: key must be 64 lowercase hex chars")
```

### §8.4 — `internal/nameval`

```go
package nameval

// ValidateWorkspaceName runs §15.1 rules on a workspace name.
// (Renamed from v2's bare Validate for clarity.)
func ValidateWorkspaceName(name string) error

// ValidateTag runs §15.2 rules on a single tag.
func ValidateTag(tag string) error

// ValidateTags checks count + each tag.
func ValidateTags(tags []string) error

// SanitizeForDisplay replaces every byte in 0x00–0x1F (except \t)
// and 0x7F with U+FFFD. Also handles valid-UTF-8 representations
// of U+0080–U+009F. MUST be called on every disk-sourced string
// before lipgloss / fmt / log / toast / doctor output.
func SanitizeForDisplay(s string) string

// NormalizeNFC normalizes via golang.org/x/text/unicode/norm.NFC.String.
// Apply at every name ingestion site.
func NormalizeNFC(s string) string

type ValidationError struct {
    Reason string  // human-readable
    Field  string  // e.g., "name", "tags[2]"
    Code   string  // always "ILLEGAL_NAME"
}
func (e *ValidationError) Error() string
```

### §8.5 — `internal/ipc` (Dispatcher seam)

```go
package ipc

// Dispatcher abstracts the OSC-emit + reply-listen sequence. Used by
// internal/tui and internal/find so neither package depends directly
// on internal/uservar / internal/ipcsock.
type Dispatcher interface {
    // Dispatch sends one OSC for verb with args and returns a channel
    // of replies. Single-reply verbs deliver one Reply then close.
    // Restore-class verbs deliver "started" then a terminal reply,
    // then close. ctx cancellation closes the listener early; the
    // channel is then closed after any in-flight read drains.
    Dispatch(ctx context.Context, verb string, args map[string]any) (<-chan Reply, error)
}

// Reply mirrors the wire shape; consumed by Dispatcher clients.
type Reply struct {
    V        int
    ID       string
    Status   string  // "completed" | "started" | "partial"
    OK       bool
    Data     map[string]any
    Warnings []Warning
    Error    *ReplyError
}
type Warning struct {
    Code, Message string
    Details       map[string]any
}
type ReplyError struct {
    Code, Message string
    Details       map[string]any
}
```

The concrete implementation lives in `internal/ipcdispatcher` (§8.6).
Tests substitute a fake.

### §8.6 — `internal/ipcdispatcher` (concrete Dispatcher)

```go
package ipcdispatcher

// New constructs a Dispatcher backed by:
//   - uservar.Writer for the forward path
//   - ipcsock.StartListener for the reverse path
//   - hmac.Signer for request signing
//   - the per-request socket-path generator
//
// The deps argument bundles the live components so that cmd/wezsesh
// only has to build one struct rather than wire each callsite.
func New(deps Deps) (ipc.Dispatcher, func(), error)

type Deps struct {
    Writer        *uservar.Writer
    Signer        *hmac.Signer
    RuntimeDir    string
    TargetWindowID int
    Logger        *logger.Logger
}
```

Why an own package: keeps `cmd/wezsesh/main.go` to flag parsing + DI;
keeps the listener wiring with the OSC-write pairing in one place;
gives tests a clean construction seam (`ipcdispatcher.New` substituted
with a fake at the `ipc.Dispatcher` interface level).

### §8.7 — `internal/ipcsock`

```go
package ipcsock

// StartListener creates the reply socket at sockPath, starts an accept
// loop in a goroutine, and returns:
//   replies — buffered (cap 2) channel of parsed Reply payloads
//   cleanup — closes listener + os.Remove(sockPath); idempotent (sync.Once)
//
// MUST be called synchronously before the corresponding OSC is emitted
// (in bubbletea: from Update, NEVER from a tea.Cmd body).
//
// Accept loop: SEQUENTIAL — one connection at a time. Top-level
// defer recover() in the goroutine logs via the structured logger.
//
// Caller MUST `defer cleanup()` immediately after StartListener returns.
func StartListener(sockPath string) (replies <-chan ipc.Reply, cleanup func(), err error)

// InstallSignalHandler registers SIGINT/SIGTERM/SIGHUP. On signal:
// invoke cleanup(), then os.Exit(130).
func InstallSignalHandler(cleanup func())

// SweepStale removes *.sock files in dir whose mtime > 60 s.
// Called from main() before tea.Run(). Uses safefs.Enforce(SymlinkSkipWarn)
// per-file.
func SweepStale(dir string, log *logger.Logger) error
```

### §8.8 — `internal/uservar`

```go
package uservar

// Writer wraps /dev/tty under a mutex. SAFE to call from tea.Cmd
// bodies; wezterm's OSC parser tolerates interleaving with bubbletea's
// renderer because OSC sequences are delimited (§3.1).
type Writer struct { /* unexported */ }

// New opens /dev/tty (O_WRONLY|O_CLOEXEC).
func New() (*Writer, error)

// WriteOSC emits one OSC 1337 SetUserVar=wezsesh_op=<payload> sequence.
// payload MUST be base64'd canonical-JSON. Single write(2) syscall in
// the common case (≤ 4 KiB payloads on Linux, ≥ 1 KiB on darwin).
func (w *Writer) WriteOSC(ctx context.Context, payload []byte) error

// Close releases /dev/tty fd. Called once at process shutdown.
func (w *Writer) Close() error
```

### §8.9 — `internal/wezcli`

```go
package wezcli

// Client is a thin holder for the resolved wezterm path + logger.
// Methods take ctx; internal 2 s timeout always applies (the longer
// of caller ctx vs internal cap).
type Client struct { /* unexported */ }

// NewClient resolves `wezterm` via exec.LookPath. Returns
// ErrWeztermNotFound if absent.
func NewClient(log *logger.Logger) (*Client, error)

func (c *Client) List(ctx context.Context) ([]Pane, error)
func (c *Client) ListClients(ctx context.Context) ([]ClientInfo, error)

// RenameWorkspace performs a pre-collision check via List, then issues
// `cli rename-workspace <old> <new>`. Same-name short-circuits as
// no-op success. Returns ErrRenameCollision when <new> exists in mux.
func (c *Client) RenameWorkspace(ctx context.Context, old, new string) error

// ActivatePane / ActivateTab: on exit 1, re-list once and retry;
// second failure returns ErrPaneClosedRace.
func (c *Client) ActivatePane(ctx context.Context, paneID int) error
func (c *Client) ActivateTab(ctx context.Context, tabID int) error

// SpawnInWorkspace returns the spawned pane ID (parsed from
// `cli spawn`'s stdout, which prints the pane ID).
func (c *Client) SpawnInWorkspace(ctx context.Context, workspace, cwd string) (paneID int, err error)

// Probe runs `cli list` for doctor; reports observed latency.
func (c *Client) Probe(ctx context.Context) (latency time.Duration, err error)

// CapturePreSwitchState reads ListClients, picks the most-recent
// last_input client (tie-break on client_id), and returns the
// pre-state needed by StartSwitchPoller.
func (c *Client) CapturePreSwitchState(ctx context.Context, targetWindowID int) (SwitchPreState, error)

// StartSwitchPoller polls until pre.TargetClientID's focused pane
// is in workspace `target` AND in window pre.TargetWindowID,
// OR ctx expires.
//
// Cadence is ADAPTIVE: the loop dispatches to ticker that fires at
// 50 ms when prior tick latency was < 100 ms, dilating to 250 ms
// when prior tick latency was ≥ 1 s (slow mux). Worst-case per-tick
// latency is bounded by the internal 2 s ctx on each wezcli call;
// in the slow path two 2 s calls = 4 s per tick. Document this in
// the timeout table.
func (c *Client) StartSwitchPoller(
    ctx context.Context,
    pre SwitchPreState,
    target string,
    isRestoreFlow bool,
) error

type Pane struct {
    PaneID    int
    TabID     int
    WindowID  int
    Workspace string
    Title     string
    TabTitle  string
    WindowTitle string
    cwd       string  // file:// URL or "" — use CWDPath()
    Size      Size
    IsActive  bool    // per-tab, NOT global
    IsZoomed  bool
    TTYName   *string // nil on Windows / unreported
    CursorX   int
    CursorY   int
}

func (p Pane) CWDPath() (path string, ok bool)

type Size struct { Rows, Cols, PixelWidth, PixelHeight, DPI int }

type ClientInfo struct {
    ClientID       string
    Username       string
    Hostname       string
    PID            int
    FocusedPaneID  int
    LastInput      time.Time
    IdleTime       time.Duration
}

type SwitchPreState struct {
    TargetClientID  string
    TargetWindowID  int
    ActiveWorkspace string
}

var (
    ErrWeztermNotFound = errors.New("wezcli: wezterm not on PATH")
    ErrMuxUnreachable  = errors.New("wezcli: mux unreachable")
    ErrRenameCollision = errors.New("wezcli: rename collision")
    ErrPaneClosedRace  = errors.New("wezcli: pane closed race")
)
```

### §8.10 — `internal/snapshots`

```go
package snapshots

// Repo binds to <snapshotDir>/workspace/. NewRepo is bind-only —
// it verifies the dir but does NOT scan files. The first List call
// performs the directory scan.
type Repo struct { /* unexported */ }

func NewRepo(snapshotDir string) (*Repo, error)

func (r *Repo) SnapshotDir() string

// List returns one Entry per snapshot file. Hashes are LAZY — entries
// carry a HashLazy() closure rather than precomputed digests, so
// startup latency is O(file count) not O(total bytes). Per-file errors
// are surfaced via entry.ParseError; never propagated.
//
// ctx wraps the directory scan. Per-file size cap 10 MiB; depth cap 100.
func (r *Repo) List(ctx context.Context) ([]Entry, error)

func (r *Repo) Has(ctx context.Context, name string) (bool, error)

func (r *Repo) ReadAll(ctx context.Context, name string) ([]byte, error)

// Hash returns the prefixed digest "sha256:<hex>" of raw on-disk bytes.
// Cached for the life of the Entry's HashLazy; subsequent calls memoize.
func (r *Repo) Hash(ctx context.Context, name string) (string, error)

// RawHashHex returns the bare hex (no prefix) for callers that need it
// (e.g., trust hash preimages). Same memoisation as Hash.
func (r *Repo) RawHashHex(ctx context.Context, name string) (string, error)

func (r *Repo) Sniff(ctx context.Context, name string) (Encryption, error)

// Delete removes <encoded>.json + sidecar. AcquireExclusive on the
// snapshot file for the duration; sidecar handled separately.
func (r *Repo) Delete(ctx context.Context, name string) error

// Rename renames both files. AcquireExclusive on each.
func (r *Repo) Rename(ctx context.Context, old, new string) error

// EncodeName replaces "/" with "+" (resurrect's transform).
func EncodeName(name string) string
func DecodeName(encoded string) string

func (r *Repo) SidecarPath(name string) string

// ReadSidecar returns the parsed sidecar.
//   v == 0 (missing) → zero Sidecar, ok=false, nil err
//   v == 1           → parsed, ok=true, nil err
//   v >  1           → log_warn, rename to .wezsesh.json.v<N>.bak,
//                       zero Sidecar, ok=false, nil err
//
// Acquires nothing (read-only). Sidecar writes serialise via
// AcquireExclusive in WriteSidecar.
func (r *Repo) ReadSidecar(ctx context.Context, name string) (s Sidecar, ok bool, err error)

// WriteSidecar atomically writes under AcquireExclusive on the sidecar
// path. Sets s.Version = 1 if zero.
func (r *Repo) WriteSidecar(ctx context.Context, name string, s Sidecar) error

type Entry struct {
    Name        string         // decoded (NFC-normalised)
    Path        string         // absolute path to .json
    Mtime       time.Time
    Size        int64
    Encryption  Encryption
    State       *WorkspaceState  // nil if encrypted or parse failed
    SidecarOK   bool             // true iff sidecar read succeeded
    Sidecar     Sidecar          // zero if !SidecarOK
    ParseError  error            // populated when parse failed
    HashLazy    func(ctx context.Context) (string, error)  // memoised, prefixed form
}

type Encryption int
const (
    EncryptionPlaintext Encryption = iota
    EncryptionAge
    EncryptionOpenPGP
    EncryptionUnknown
)

type WorkspaceState struct {
    Workspace    *string
    WindowStates []WindowState
}
type WindowState struct {
    Title *string
    Tabs  []TabState
    Size  *Size
}

type Sidecar struct {
    Version    int
    Tags       []string
    Pinned     bool
    OnCreate   *string
    OnRestore  *string
    Notes      *string
}

const (
    MaxFileSize  = 10 * 1024 * 1024  // 10 MiB
    MaxJSONDepth = 100
)
```

### §8.11 — `internal/state`

```go
package state

// Store backs $XDG_STATE_HOME/wezsesh/state.json.
// Pins for SAVED workspaces live in the snapshot sidecar (single source
// of truth, §13.11). state.json holds usage stats and pins for
// LIVE-ONLY workspaces only.
type Store struct { /* unexported */ }

func Open(ctx context.Context) (*Store, error)

// RecordSwitch atomically increments switch_count and updates
// last_switched. ctx bounds the write.
func (s *Store) RecordSwitch(ctx context.Context, name string) error

// SetLivePin marks a live-only workspace as pinned. Used only when no
// snapshot exists for the name. On subsequent save, the pin is
// migrated to the sidecar (§13.11) and SetLivePin(name, false) is
// called to clean up.
func (s *Store) SetLivePin(ctx context.Context, name string, pinned bool) error

// IsLivePinned returns true iff name appears in live_pins. No disk I/O.
func (s *Store) IsLivePinned(name string) bool

func (s *Store) LastSwitched(name string) int64
func (s *Store) SwitchCount(name string) int

// LivePins returns a copy of the live_pins set. No disk I/O.
func (s *Store) LivePins() []string

type State struct {
    Version  int
    Usage    map[string]Usage
    LivePins []string
}
type Usage struct {
    LastSwitched int64
    SwitchCount  int
}
```

Last-writer-wins under concurrent TUIs (acceptable per §10.4).

### §8.12 — `internal/trust`

```go
package trust

// ComputeHash uses length-prefixed concatenation (P §6.11):
//   sha256( uint32_be(len(path)) || path_bytes ||
//           uint32_be(len(cmd))  || cmd_bytes )
func ComputeHash(absSidecarPath string, commandBytes []byte) string

// Store backs $XDG_DATA_HOME/wezsesh/allow/.
type Store struct { /* unexported */ }

// Open MkdirAlls if missing (mode 0700), Enforce(SymlinkRefuse) on the
// trust dir.
func Open(ctx context.Context) (*Store, error)

func (s *Store) Approve(ctx context.Context, absSidecarPath string, commandBytes []byte) error
func (s *Store) IsApproved(ctx context.Context, absSidecarPath string, commandBytes []byte) bool
func (s *Store) Revoke(ctx context.Context, absSidecarPath string, commandBytes []byte) error
func (s *Store) List(ctx context.Context) ([]Entry, error)

// Prune removes entries whose recorded path no longer exists.
func (s *Store) Prune(ctx context.Context) (removed int, err error)

// Rebind transfers approval from oldPath → newPath WITHOUT re-prompting,
// when both paths resolve to the same on-disk command bytes. Used by
// `wezsesh trust --rebind` (§13.5.2).
//
//   oldHash := ComputeHash(oldPath, cmdBytes)
//   newHash := ComputeHash(newPath, cmdBytes)
//   if Lstat(oldHashFile) ENOENT → return ErrTrustRebindMissing
//   write newHash file with content {"path": newPath}
//   remove oldHash file
func (s *Store) Rebind(ctx context.Context, oldPath, newPath string, cmdBytes []byte) error

type Entry struct {
    Hash string
    Path string  // from file contents; advisory only
}

var ErrTrustRebindMissing = errors.New("trust: source approval not found")
```

### §8.13 — `internal/argvallow`

```go
package argvallow

import _ "embed"

//go:embed default.txt
var defaultListRaw string

// Default returns the v0.1 baseline list (program basenames).
// Source of truth: internal/argvallow/default.txt; the same file is
// codegen'd into plugin/wezsesh/default_allowlist.lua via
// `go run ./internal/argvallow/codegen` (a CI step).
func Default() []string

// Auditor scans snapshots and reports argv basenames that would be
// skipped by the active policy.
type Auditor struct { /* unexported */ }

// NewAuditor builds the active policy:
//   default + basename($SHELL) + userAdditions
// User additions cannot remove default entries.
func NewAuditor(shell string, userAdditions []string) *Auditor

func (a *Auditor) AuditSnapshots(ctx context.Context, repo *snapshots.Repo) (map[string][]string, error)
```

#### §8.13.1 — `default.txt` (source of truth)

```
sh
bash
zsh
fish
dash
ksh
nvim
vim
vi
emacs
nano
helix
hx
less
more
man
info
git
jj
lazygit
tig
python
python3
ipython
node
ruby
irb
lua
htop
btop
top
k9s
lazydocker
tmux
screen
```

`tmux` and `screen` are intentionally INCLUDED — users restoring
multiplexer panes are common; the inner-shell still gets its own
allowlist enforcement when its commands are restored.

### §8.14 — `internal/find`

```go
package find

func Search(ctx context.Context, c *wezcli.Client, pattern string, opts Options) ([]Match, error)

// Activate performs the two-phase sequence (P §6.13). Requires a
// Dispatcher because Phase 1 emits a `switch` verb. Drain protocol:
// after the switch poller returns success, Activate cancels the
// dispatch ctx, then drains the replies channel until it returns
// (channel closed by Dispatcher).
//
// progress fires synchronously at each phase transition.
func Activate(ctx context.Context, d ipc.Dispatcher, c *wezcli.Client, match Match, progress func(Phase)) error

type Options struct {
    Deep      bool
    CWDOnly   bool
    Workspace string
}

type Match struct {
    PaneID      int
    TabID       int
    WindowID    int
    Workspace   string
    Title       string
    CWD         string
    Score       int
    SourceField string  // "title"|"tab_title"|"window_title"|"cwd"|"ps"
}

type Phase string
const (
    PhaseSwitchStarted   Phase = "switch_started"
    PhaseSwitchSucceeded Phase = "switch_succeeded"
    PhaseSwitchTimeout   Phase = "switch_timeout"
    PhaseActivateStarted Phase = "activate_started"
    PhaseActivateDone    Phase = "activate_done"
)
```

#### §8.14.1 — `--deep` mode `ps -t` parsing

Portable subset (works on darwin BSD-ps and Linux procps):

```
ps -p $(pgrep -t <tty_basename>) -o stat=,comm=,args=
```

Parse format: `<stat-field> <comm> <args...>`. Match the row whose
`stat` field contains `+` (foreground process group). When multiple
processes share `+` (pipeline), prefer the rightmost in the pipe-tree
(largest PID heuristic). On parse failure: log_warn, skip that pane.

### §8.15 — `internal/pathpicker`

```go
package pathpicker

// Resolve runs the configured (or auto-detected) path provider and
// returns up to 10 000 absolute directory paths.
//
// Auto-detection (when userCmd == ""):
//   1. exec.LookPath("zoxide") → "zoxide query -l"
//   2. exec.LookPath("fd") → "fd -t d --max-depth 4 . ~"
//   3. return ErrNoPathProvider
//
// Exec model (P §5.6):
//   exec.CommandContext(ctx, shell, "-c", cmd)
//   shell from $SHELL, fallback /bin/sh
//   Stdin = nil
//   SysProcAttr.Setpgid = true (group SIGKILL on timeout)
//   Cmd.Env = os.Environ() filtered (drop the three sensitive
//             WEZSESH_ vars per §13.5.1)
//
// Caps: 1 MiB stdout (io.LimitedReader), 512 KiB scanner buffer,
// 10 000 lines (silent drop beyond), per-line UTF-8 + NUL validation.
//
// ctx SHOULD be context.WithTimeout(15s).
func Resolve(ctx context.Context, userCmd string) ([]string, error)

var (
    ErrNoPathProvider     = errors.New("pathpicker: no provider")
    ErrPathPickerTimeout  = errors.New("pathpicker: timeout")
    ErrPathPickerCmdFailed = errors.New("pathpicker: command failed")
)
```

### §8.16 — `internal/tui`

```go
package tui

// Model is the bubbletea model. cmd/wezsesh owns tea.NewProgram.
type Model struct { /* unexported */ }

// New constructs the model. Verb dispatch goes through `d`.
func New(cfg Config, initial Data, d ipc.Dispatcher) tea.Model

// ReplyReceived is exposed as a model field for the tea.Tick
// retransmit guard (§14.2). Update sets it on first reply; subsequent
// retransmitMsg invocations short-circuit.

type Config struct {
    Sort           SortMode
    DefaultAction  Action
    DefaultActionLoadNoPrompt bool
    PreviewEnabled bool
    PreviewWidth   float64
    Markers        Markers
    Columns        []Column
    NameTruncate   string  // "middle" only in v0.1
    Colors         Colors
    Keys           KeyMap
    ConfirmDelete    bool
    ConfirmOverwrite bool
}

type SortMode string
const (
    SortLiveFirst    SortMode = "live_first"
    SortRecent       SortMode = "recent"
    SortMtime        SortMode = "mtime"
    SortAlphabetical SortMode = "alphabetical"
)

type Action string
const (
    ActionSwitch Action = "switch"
    ActionLoad   Action = "load"
    ActionNone   Action = "none"
)

type Column string
const (
    ColMarker Column = "marker"
    ColName   Column = "name"
    ColTabs   Column = "tabs"
    ColAge    Column = "age"
    ColTags   Column = "tags"
)

type Markers struct {
    Active, Live, Marked, Unsaved, Pinned string
}
type Colors struct {
    Accent, Muted, Error, Success, FocusBG, MatchHighlight,
    LiveMarker, SavedMarker *string  // nil → terminal default
}
type KeyMap struct {
    Switch, Load, Rename, Delete, Save, New, Pin, Tag,
    Mark, MarkAlt, ClearMarks, Help, Filter, Quit,
    Up, Down, Top, Bottom string  // empty string = disabled
}

type Data struct {
    Workspaces       []WorkspaceRow
    State            state.State
    ActiveWorkspace  string
    ActiveWindowID   int
}

type WorkspaceRow struct {
    Name      string
    Live      bool
    Active    bool
    Saved     bool
    Tabs      int
    Mtime     time.Time
    Tags      []string
    Pinned    bool         // unioned: sidecar.Pinned for saved, state.LivePins for live-only
    Snapshot  *snapshots.Entry
}
```

### §8.17 — `internal/doctor`

```go
package doctor

func Run(ctx context.Context, env Env) Report

type Env struct {
    BinaryPath    string
    PluginVersion string
    SnapshotDir   string
    StateDir      string
    RuntimeDir    string
    TrustDir      string
    Cfg           *config.Config
}

type Report struct {
    Checks   []Check
    Critical bool
}

type Check struct {
    ID       string
    Status   Status
    Message  string
    Details  map[string]any
}

type Status string
const (
    StatusOK   Status = "ok"
    StatusWarn Status = "warn"
    StatusFail Status = "fail"
    StatusSkip Status = "skip"
)
```

#### §8.17.1 — Required check IDs

```
binary.version
binary.path
binary.fs.network                  ← IsNetworkFS on binary path
plugin.version
version.compatible
wezterm.version                    ← floor 20230408-112425-69ae8472
wezterm.lua_version                ← assert ≥ 5.3 (ct_eq.lua bitwise ops)
wezterm.cli.list
wezterm.cli.list-clients
wezterm.cli.tty_name
wezterm.pane.env                   ← WEZTERM_PANE set + resolves
snapshot.dir.exists
snapshot.dir.writable
snapshot.dir.fs.network
snapshot.count
snapshot.name.validation
snapshot.argv.allowlist.coverage
snapshot.encryption.detected
snapshot.pin.consistency           ← live_pins ∩ saved-names should be ∅
state.dir.exists
state.dir.writable
state.dir.fs.network
trust.dir.exists                   ← rejects symlink
trust.count
trust.orphans
runtime.dir.exists
runtime.dir.fs.network
runtime.dir.permissions
runtime.dir.sun_path_budget
home.consistency
linux.kernel.version
nerdfont.detected
pathpicker.zoxide
pathpicker.fd
encryption.agent.responsive
log.recent_errors
config.exclude.regex_validity     ← reports invalid regexes from cfg.Exclude
```

### §8.18 — `internal/logger`

```go
package logger

type Logger struct { /* unexported */ }

type Level int
const (
    LevelDebug Level = iota
    LevelInfo
    LevelWarn
    LevelError
)

// New opens (or rotates into) <stateDir>/wezsesh.log. Rotation policy:
// when current log exceeds 1 MiB on Write, atomically rename to
// wezsesh.log.1, shift older numbered logs (.1 → .2, .2 → .3, drop .3).
// Enforce(SymlinkRefuse) at every rotation step.
//
// Buffering policy:
//   - Debug/Info: line-buffered with periodic 1 s flush.
//   - Warn/Error: flushed synchronously on every call (no 1 s window
//     for crash-loss; tradeoff: slight overhead on warn-storms).
// Close drains the buffer.
func New(stateDir string, level Level) (*Logger, error)

func (l *Logger) Debug(msg string, kv ...any)
func (l *Logger) Info(msg string, kv ...any)
func (l *Logger) Warn(msg string, kv ...any)   // syncs after write
func (l *Logger) Error(msg string, kv ...any)  // syncs after write
func (l *Logger) Close() error

func ResolveLevel(optsLevel string, envLevel string) Level
```

Implementation: Go 1.26 `log/slog` JSON handler over a custom rotating
writer (in-tree, ~150 LOC).

### §8.19 — `internal/config`

```go
package config

// Config is the binary-side configuration loaded from $WEZSESH_CONFIG_FILE.
type Config struct {
    SnapshotDir    string
    StateDir       string
    RuntimeDir     string
    LogLevel       string
    Sort           string
    DefaultAction  string
    DefaultActionLoadNoPrompt bool
    ConfirmDelete    bool
    ConfirmOverwrite bool
    Exclude        []string  // RE2 regex strings as authored
    ExcludeCompiled []*regexp.Regexp // populated by Load; len matches Exclude with nil for invalid
    ExcludeErrors  []ExcludeError    // one per invalid element (index + reason)
    NewWorkspaceCommand string

    Preview struct {
        Enabled bool
        Width   float64
    }
    Markers     Markers
    Columns     []string
    NameTruncate string

    Colors Colors

    Hooks struct {
        RunHooks         bool
        PromptOnUntrusted bool
        TimeoutSeconds   int
    }

    ResurrectArgvAllowlist []string

    Keys KeyMap

    PluginVersion string
    ProtoVersion  int
}

type ExcludeError struct {
    Index  int
    Source string  // the regex string as authored
    Reason string  // err.Error() from regexp.Compile
}

// Load reads the config file at path (JSON), validates, returns Config.
// Exclude regex policy: each element compiled independently; failures
// recorded in cfg.ExcludeErrors; the corresponding ExcludeCompiled
// entry is nil and the element is treated as a no-op match (never
// excludes). Doctor reports cfg.ExcludeErrors via
// `config.exclude.regex_validity` (§8.17.1).
func Load(ctx context.Context, path string) (*Config, error)

func LoadFromEnv(ctx context.Context) (*Config, error)
func AutoDetect() (*Config, error)
```

### §8.20 — `cmd/wezsesh`

CLI surface:

```
wezsesh                          → runs TUI
wezsesh --version                → prints version, exit 0
wezsesh --pane-id <int>          → override $WEZTERM_PANE (test/CI)
wezsesh list [--format json]
wezsesh doctor [--format json]   → exit 0 on all-OK, !=0 otherwise
wezsesh find [PATTERN] [flags]
wezsesh trust <name>
wezsesh trust --revoke <name>
wezsesh trust --list
wezsesh trust --prune
wezsesh trust --show <name>
wezsesh trust --path <picked>
wezsesh trust --sidecar <abs>
wezsesh trust --rebind <old-abs> <new-abs>   (§13.5.2)
wezsesh keygen                   → 64 hex chars + "\n", exit 0
wezsesh reset                    → preview only (NO writes)
wezsesh reset --dry-run          → verbose preview
wezsesh reset --yes              → ACTUAL deletion (state/trust/log/sidecars)
wezsesh reset --yes --include-snapshots  → also remove resurrect snapshots
wezsesh nuke ...                 → deprecated alias for `reset` with toast
wezsesh reply <sock> <b64json>   → internal IPC reply
```

#### §8.20.1 — `main.go` startup sequence

1. Parse flags + subcommand.
2. `defer recover()` registered for the TUI path; subcommands have
   their own simpler error paths (§13.14).
3. Subcommand routing:
   - `--version`, `keygen`, `reply` — minimal env, no listener.
   - `doctor`, `list`, `trust`, `reset` — `config.LoadFromEnv` (falls
     back to `AutoDetect`); no listener.
   - `find` — config load; constructs an in-process Dispatcher iff
     invoked from inside wezterm (`WEZTERM_PANE` set + listener-init
     succeeds); otherwise prints results only.
   - default (TUI) — full setup below.
4. Full TUI setup:
   1. Validate `WEZTERM_PANE` set, `WEZSESH_HMAC_KEY` is 64 hex.
   2. `config.LoadFromEnv` (requires `WEZSESH_CONFIG_FILE`).
   3. `logger.New(cfg.StateDir, logger.ResolveLevel(...))`.
   4. `ipcsock.SweepStale(cfg.RuntimeDir, log)`.
   5. `safefs.Enforce(SymlinkRefuse)` on snapshot / state / runtime dirs.
   6. `wezcli.NewClient(log)`.
   7. `snapshots.NewRepo(cfg.SnapshotDir)` + `state.Open(ctx)` +
      `trust.Open(ctx)`.
   8. Build initial `tui.Data` (sidecar pin + state.LivePins union).
   9. `dispatcher, dispCleanup, _ := ipcdispatcher.New(deps)`.
  10. `model := tui.New(cfg, initial, dispatcher)`.
  11. `program := tea.NewProgram(model)` + `program.Run()`.
  12. After `Run()` returns: drive any deferred phases (find Phase 2
      poller, restore-class follow-up) to completion. Use a
      `sync.WaitGroup` shared with the dispatcher, NOT polling on
      "open channels".
  13. `dispCleanup()` then `cleanup()` (sock files, log flush,
      /dev/tty close).
5. Top-level panic-recover (TUI path only): write sentinel
   `UNEXPECTED_EXIT` reply to any open reply socket via dispatcher's
   `EmergencyReply` hook, log, `os.Exit(2)`.

---

## §9 — Lua module APIs

All modules return a single table `M`.

### §9.0 — Lua-version requirement

wezsesh's Lua modules require **Lua 5.3 or newer** for native bitwise
operators (`~` for binary XOR, `|` for bitwise OR, used in `ct_eq.lua`
§9.9). wezterm currently ships with mlua/Lua 5.4. CI gate:
`wezterm.lua_version` doctor check (§8.17.1) asserts `_VERSION` ≥ 5.3.

If wezterm ever swaps to LuaJIT (no native bitwise ops), `ct_eq.lua`
would need a `bit.bxor` / `bit.bor` rewrite. The CI check fails loudly
in that scenario, preventing silent breakage.

### §9.1 — `init.lua`

```lua
local M = {}
M.VERSION = "0.1.0"      -- bumped per tagged release; CI asserts match

-- Entry point. Body wrapped in pcall (P §7.1).
-- Sentinel-prefixed errors (WEZSESH_*) raised via error(msg, 0) are
-- detected by string.find substring match and surfaced via 10s toast.
function M.apply_to_config(config, opts)  end

-- Programmatic API
function M.open(window, pane)  end
function M.is_running(window)  end
function M.list()              end       -- table: { {name, live, saved, pinned, tags, mtime}, ... }
function M.tags(name)          end       -- string[]
function M.pinned(name)        end       -- bool

return M
```

### §9.2 — `manager.lua`

```lua
local M = {}

function M.resolve_binary(opts)  end       -- string | raises

-- Returns "missing" | "unparseable" | "<semver>"
function M.detect_version(binary_abs_path)  end

function M.compatible(plugin_v, bin_v)  end

-- HMAC key generation chain (§5.2). Trims whitespace, validates
-- ^%x+$ length 64. Stores in wezterm.GLOBAL.wezsesh_session_key.
function M.ensure_session_key(binary_abs_path)  end

-- SUN_PATH validation (§13.9). Raises sentinel WEZSESH_RUNTIME_DIR_TYPE
-- or WEZSESH_SUN_PATH_OVERFLOW on failure.
function M.validate_runtime_dir(opts)  end

-- Write opts (filtered to binary-relevant fields) to a temp JSON file
-- and return its absolute path. Caller passes via WEZSESH_CONFIG_FILE
-- env var. Schema: §10.7.
function M.write_config_file(opts)  end

function M.spawn(window, opts)  end
function M.register_keybinding(config, opts)  end

return M
```

### §9.3 — `ipc.lua` (handler step state machine)

```lua
local M = {}

-- The user-var-changed handler. MUST execute steps (a)–(h)
-- synchronously (zero .await). CI lint enforces.
local function handler(window, pane, name, value)
    -- (a) Pane-ID match
    -- (b) HMAC key availability
    -- (c) JSON parse (pcall-wrapped)
    -- (d) Field-shape validator (§9.3.1)
    -- (e) Verb-aware tagging + canonical re-encode (§4.2; pcall)
    -- (f) HMAC verify with ct_eq.eq
    -- (g) Freshness + replay + target_window_id
    -- (h) seen_ids write-back + state.prune + state.set_state
    -- (i) Dispatch with pcall
end

function M.validate_payload(payload)  end
function M.register()  end

return M
```

#### §9.3.1 — Field-shape validator (step (d))

```lua
return type(payload.v) == "number" and payload.v == 1
   and type(payload.id) == "string" and #payload.id == 26
   and type(payload.ts) == "number"
   and type(payload.op) == "string" and #payload.op > 0 and #payload.op <= 32
   and type(payload.args) == "table"
   and type(payload.reply_sock) == "string"
        and #payload.reply_sock > 0 and #payload.reply_sock <= 104
   and type(payload.target_window_id) == "number"
   and type(payload.hmac) == "string" and #payload.hmac == 64
```

### §9.4 — `ops.lua` (verb dispatch table)

```lua
local M = {}

-- Five verbs only (§6).
M.dispatch_table = {
    switch = function(payload, window, pane) ... end,
    load   = function(payload, window, pane) ... end,
    save   = function(payload, window, pane) ... end,
    new    = function(payload, window, pane) ... end,
    noop   = function(payload, window, pane) ... end,
}

-- Outer dispatch. pcall-wrapped at the boundary; emits
-- result.reply_error on caught error. UNKNOWN VERB HANDLING:
--   if dispatch_table[payload.op] is nil:
--     log_warn("UNKNOWN_VERB op=" .. payload.op)
--     result.reply_error(payload, "UNKNOWN_VERB",
--                        "unknown verb: " .. payload.op, {})
--     return
-- (Replies terminal `completed`, ok=false; does NOT degrade to noop.)
function M.dispatch(payload, window, pane)  end

return M
```

#### §9.4.1 — Restore-class verbs (split-reply)

`switch` (when target is saved-not-live) and `load` emit:
1. `result.reply_started(payload)` BEFORE calling
   `resurrect.workspace_state.restore_workspace(...)` (pcall-wrapped).
2. Then, AFTER restore returns: `result.reply_completed(payload, data)`
   on success or `result.reply_partial(payload, data, warnings)` on
   pcall-caught error.

#### §9.4.2 — Save handler

`save`'s Lua-side handler:
1. Receive payload. Lua does NOT enforce `expected_hash` — that has
   already been checked binary-side (§13.4) before the OSC was emitted.
2. `pcall(resurrect.state_manager.save_state, current_state)`.
3. On success: `result.reply_completed(payload, { name = name })`.
   The binary fills `hash` after re-reading the file under a brief
   second lock (§13.4 step 7) and surfaces the final value to TUI.
4. On error: `result.reply_error(payload, "SAVE_FAILED",
   tostring(err), { raw_error = tostring(err) })`.

### §9.5 — `result.lua`

```lua
local M = {}

function M.reply_started(payload)  end
function M.reply_completed(payload, data)  end
function M.reply_partial(payload, data, warnings)  end
function M.reply_error(payload, code, message, details)  end

function M.toast(window, message, ms)  end

return M
```

All reply emitters set `v = payload.v` on the outbound JSON.

### §9.6 — `state.lua`

```lua
local M = {}

-- Centralises wezterm.GLOBAL access; coerces pane IDs to strings
-- (GLOBAL Object nodes reject integer keys).

function M.set_state(pane_id, state)  end
function M.get_state(pane_id)         end
function M.delete_state(pane_id)      end

function M.set_request(id, info)  end
function M.get_request(id)        end
function M.delete_request(id)     end

function M.set_writing(path, b)   end
function M.is_writing(path)       end

-- Session-wide seen_ids (§5.4); no per-pane bucketing.
function M.seen(id)               end       -- bool
function M.mark_seen(id)          end       -- record at now

function M.prune_seen_ids(ttl_seconds)  end   -- session-wide
function M.prune_states(now, ttl_seconds)        end
function M.prune_requests(now, ttl_seconds)      end

function M.wipe_all()  end

return M
```

### §9.7 — `canonical_json.lua`

```lua
local M = {}

M.array_mt  = { __wezsesh_canonical = "array" }
M.object_mt = { __wezsesh_canonical = "object" }
M.NULL      = setmetatable({}, { __wezsesh_canonical = "null" })

function M.array(t)   return setmetatable(t or {}, M.array_mt)  end
function M.object(t)  return setmetatable(t or {}, M.object_mt) end

-- Serialize per §4.1. Untagged tables raise ENCODER_UNTAGGED_TABLE.
function M.encode(v)  end

-- Walk t recursively, applying tags from shape (verb-aware tagging
-- per §4.2). Raises CANONICAL_SHAPE_MISMATCH on type incompatibility.
function M.tag_in_place(t, root_shape, args_shape)  end

-- Verb args shape declarations.
M.verb_args_shape = {
    switch = { _shape = "object", name = "string" },
    load   = { _shape = "object", name = "string" },
    save   = { _shape = "object",
               name = "string", overwrite = "bool",
               expected_hash = "string_or_null" },
    new    = { _shape = "object", name = "string", cwd = "string" },
    noop   = { _shape = "object" },
}

function M.copy_without(t, k)  end

return M
```

### §9.8 — `hmac.lua`

```lua
local M = {}

function M.compute(payload_bytes, hex_key)  end
function M.verify(payload_bytes, hex_key, expected_hex)  end

return M
```

### §9.9 — `ct_eq.lua`

Requires Lua 5.3+ (see §9.0). Native bitwise operators are used.

```lua
local M = {}

function M.eq(a, b)
    if #a ~= #b then return false end
    local d = 0
    for i = 1, #a do d = d | (a:byte(i) ~ b:byte(i)) end
    return d == 0
end

return M
```

### §9.10 — `b64.lua`

```lua
local M = {}

function M.encode(s)  end
function M.decode(s)  end  -- returns string or nil

return M
```

`encode` is used on the reply path (`wezsesh reply <sock> <b64>`).
`decode` is reserved for future use; not currently exercised in the
hot path.

### §9.11 — `on_pane_restore.lua`

```lua
local M = {}

-- The wezsesh-installed callback wrapping resurrect's default.
-- SIGNATURE IS SINGLE-ARG (P §6.18):
--    function(pane_tree)
--        local pane = pane_tree.pane
--        ...
--    end
function M.callback(pane_tree)  end

function M.configure(opts)  end

-- Byte-cleanliness check. Rejects bytes 0x00–0x1F and 0x7F.
function M.bytes_clean(s)  end

return M
```

#### §9.11.1 — Decision flow

```
1. argv = pane_tree.process and pane_tree.process.argv
2. if not argv or #argv == 0:
       resurrect.default_on_pane_restore(pane_tree); return
3. prog = basename(argv[1])
4. if not policy.allows(prog):
       send_cwd_or_newline(pane_tree)
       log_warn("skipped argv restore for <prog>; cwd <clean|dirty>")
       return
5. for each elem in argv: if not bytes_clean(elem) → goto step 4
6. if pane_tree.cwd and not bytes_clean(pane_tree.cwd) → goto step 4
7. resurrect.default_on_pane_restore(pane_tree)

On any uncaught error (pcall-wrapped at outer boundary):
    pane:send_text("\r\n")
    log_warn("hook crash; failed CLOSED")
    -- MUST NOT call resurrect.default_on_pane_restore
```

### §9.12 — `default_allowlist.lua` (codegen'd)

```lua
-- AUTOGENERATED by `go run ./internal/argvallow/codegen`.
-- Source: internal/argvallow/default.txt
-- DO NOT EDIT BY HAND.
return {
    "sh", "bash", "zsh", "fish", "dash", "ksh",
    "nvim", "vim", "vi", "emacs", "nano", "helix", "hx",
    "less", "more", "man", "info",
    "git", "jj", "lazygit", "tig",
    "python", "python3", "ipython", "node", "ruby", "irb", "lua",
    "htop", "btop", "top", "k9s", "lazydocker",
    "tmux", "screen",
}
```

CI gate: regenerate, diff against the committed file; mismatch fails
the build.

---

## §10 — Persistent data schemas

### §10.1 — Snapshot file (resurrect-owned; we parse tolerantly)

Schema mirrors P §6.6. Every field optional (Go pointers). `process`
parsed via custom unmarshaler accepting both string-shape (legacy)
and object-shape (current).

### §10.2 — Snapshot sidecar (`<encoded>.wezsesh.json`)

```jsonc
{
  "version":    1,
  "tags":       ["api", "backend"],
  "pinned":     false,
  "on_create":  null | "<shell-command>",
  "on_restore": null | "<shell-command>",
  "notes":      null | "<freeform>"
}
```

Sidecar is the **single source of truth** for `pinned` on saved
workspaces (§13.11).

### §10.3 — Project sidecar (`<picked_path>/.wezsesh.json`)

Same schema as §10.2. Trust hash binds the absolute project sidecar
path (§13.5). Hooks fire in this order:
- `on_create` runs once after `new` verb spawn (§6.4 / §13.5).
- `on_restore` runs after `load` and after `switch`-with-restore
  completes (§9.11 hooks the per-pane restore; project-sidecar hook
  is a per-workspace one-shot triggered from the binary).

### §10.4 — `state.json`

```jsonc
{
  "version": 1,
  "usage": {
    "<workspace-name>": {
      "last_switched": <unix-seconds>,
      "switch_count":  <int>
    }
  },
  "live_pins": ["<workspace-name>", ...]
}
```

`live_pins` holds pins for live-only workspaces (no snapshot exists).
On save of a live-only-pinned workspace, the pin migrates to
`<encoded>.wezsesh.json.pinned = true` and is removed from `live_pins`
(§13.11). v2's `pins[]` field is removed; migration on read renames
`pins` → `live_pins` and drops any entry that has a corresponding
snapshot file (the sidecar wins).

Atomic write via `safefs.AtomicWriteFile`. No locking; last-writer-wins
under concurrent TUIs. v > 1 → back up to `.v<N>.bak` and reinitialise.

### §10.5 — Trust file (`<sha256>`)

JSON content:

```jsonc
{ "path": "/Users/grady/snapshots/workspace/code+foo.wezsesh.json" }
```

The file *name* (the SHA-256 hash) is the truth. Content is advisory
(used by `wezsesh trust --list`).

### §10.6 — `wezterm.GLOBAL` keys

All keys are JSON-shaped (string keys only at Object nodes). Pane IDs
are stringified via `tostring(...)` at the boundary.

```
wezsesh_session_key       string  (64 hex chars; the only HMAC-key store)
wezsesh_plugin_version    string  ("0.1.0")
wezsesh_bin_path          string  (absolute path)
wezsesh_state             object  → keyed by pane_id_str:
    {
      target_window_id : number,
      spawned_at       : number
    }
wezsesh_seen_ids          object  → keyed by ULID string:
                                    { ts = number }     (session-wide)
wezsesh_requests          object  → keyed by request ULID:
    { spawned_pane_id = number, started_at = number }
wezsesh_writing           object  → keyed by absolute path: bool
```

(Note vs v2: `hmac_key` is no longer per-pane; `seen_ids` is now
session-wide rather than nested under `wezsesh_state[pid]`.)

### §10.7 — Binary config file (`$WEZSESH_CONFIG_FILE`)

JSON file written by Lua at plugin load (§9.2 `manager.write_config_file`)
and read by the binary (§8.19 `config.Load`). One file per spawn; the
binary deletes after reading.

```jsonc
{
  "version": 1,
  "snapshot_dir": "<absolute>",
  "state_dir":    "<absolute>",
  "runtime_dir":  "<absolute>",
  "log_level":    "info",
  "sort":         "live_first",
  "default_action": "switch",
  "default_action_load_no_prompt": false,
  "confirm_delete":    true,
  "confirm_overwrite": true,
  "exclude":      ["^default$"],
  "new_workspace_command": null,
  "preview":      { "enabled": true, "width": 0.4 },
  "markers":      { "active": "▶", "live": "●", "marked": "✓",
                    "unsaved": "(unsaved)", "pinned": "[pinned]" },
  "columns":      ["marker", "name", "tabs", "age", "tags"],
  "name_truncate": "middle",
  "colors":       { "accent": null, "muted": null, "error": null,
                    "success": null, "focus_bg": null,
                    "match_highlight": null, "live_marker": null,
                    "saved_marker": null },
  "hooks":        { "run_hooks": true, "prompt_on_untrusted": false,
                    "timeout_seconds": 600 },
  "resurrect_argv_allowlist": [],
  "keys":         { /* §11.1 default key map */ },
  "plugin_version": "0.1.0",
  "proto_version":  1
}
```

The HMAC key is NOT in this file — it travels via `WEZSESH_HMAC_KEY`
env var (Appendix A). Config file has wider on-disk exposure than env.

---

## §11 — Configuration schema (`apply_to_config(config, opts)`)

| Key | Type | Default | Validation |
|---|---|---|---|
| `binary` | string | `"wezsesh"` | non-empty |
| `keybinding` | `{key, mods}` | `{"W", "LEADER\|SHIFT"}` | type check both fields |
| `spawn_mode` | string | `"tab"` | enum: `"tab"`, `"window"` |
| `snapshot_dir` | string\|nil | nil | nil → §12.5 auto-detect |
| `state_dir` | string\|nil | nil | nil → §12.5 auto-detect |
| `runtime_dir` | string\|nil | nil | nil → §12.5; if string, §13.9 SUN_PATH |
| `force_close` | bool | `false` | — |
| `sort` | string | `"live_first"` | enum |
| `default_action` | string | `"switch"` | enum |
| `default_action_load_no_prompt` | bool | `false` | — |
| `confirm_delete` | bool | `true` | — |
| `confirm_overwrite` | bool | `true` | — |
| `exclude` | string[] | `["^default$"]` | each compiles as Go RE2 (in binary; doctor reports invalid) |
| `new_workspace_command` | string\|nil | nil | nil → pathpicker auto-detect (§8.15) |
| `preview.enabled` | bool | `true` | — |
| `preview.width` | float | `0.4` | (0.0, 1.0) |
| `markers.{active,live,marked,unsaved,pinned}` | string | (defaults) | — |
| `columns` | string[] | `["marker","name","tabs","age","tags"]` | each in `Column` enum (§8.16) |
| `name_truncate` | string | `"middle"` | enum: `"middle"` (only in v0.1) |
| `colors.*` | string\|nil | nil | hex (`#rrggbb` / `#rrggbbaa`) or named (lipgloss-compatible); doctor validates |
| `hooks.run_hooks` | bool | `true` | — |
| `hooks.prompt_on_untrusted` | bool | `false` | — |
| `hooks.timeout_seconds` | int | `600` | min 1; max 86400 |
| `resurrect_argv_allowlist` | string[] | `[]` | each is a basename |
| `log_level` | string | `"info"` | enum |
| `keys.*` | string\|false | (per §11.1) | string key spec or `false` to disable |
| `on_before_op` | function\|nil | nil | pcall-wrapped at dispatch |
| `on_after_op` | function\|nil | nil | pcall-wrapped at dispatch |

Unknown keys log a warning but do not fail.

### §11.1 — Default `keys` table

```lua
keys = {
    switch = "s", load = "l", rename = "r", delete = "d",
    save = "S", new = "n", pin = "p", tag = "t",
    mark = "Tab", mark_alt = "Space", clear_marks = "c",
    help = "?", filter = "/", quit = "q",
    up = "k", down = "j", top = "gg", bottom = "G",
}
```

`mark_alt = "Space"` (the bubbletea-canonical spelling, replacing v2's
literal `" "`).

`"gg"` is a multi-key sequence; the TUI implements a vim-style
two-key state machine for `g_` prefixes.

### §11.2 — Configuration precedence

1. Plugin defaults.
2. Values passed to `apply_to_config`.
3. Direct field assignment (`wezsesh.colors = { ... }` post-call) —
   only affects in-Lua state. Post-call assignments do NOT propagate to
   the binary's config file (which is written at `apply_to_config`
   time). Document this caveat to users.

### §11.3 — Override env vars

| Var | Purpose | Beats config? |
|---|---|---|
| `WEZSESH_LOG` | log level | yes if more verbose |
| `WEZSESH_NO_HOOKS` | `=1` disables hooks entirely | yes |
| `WEZSESH_NERDFONT` | hint for doctor + TUI | n/a |

### §11.4 — Resolution table (env vs config file)

For the binary's runtime configuration, fields resolve in this order
(first non-empty wins, except `log_level` which uses min):

| Field | Env var | Config file | Auto-detect |
|---|---|---|---|
| `snapshot_dir` | `WEZSESH_SNAPSHOT_DIR` | `snapshot_dir` | §12.5 |
| `state_dir` | `WEZSESH_STATE_DIR` | `state_dir` | §12.5 |
| `runtime_dir` | `WEZSESH_RUNTIME_DIR` | `runtime_dir` | §12.5 |
| `log_level` | `WEZSESH_LOG` | `log_level` | `"info"` |
| `hooks.run_hooks` | `WEZSESH_NO_HOOKS=1` ⇒ false | `hooks.run_hooks` | true |

For `log_level`, `ResolveLevel(envLevel, optsLevel)` returns the
**more verbose** of the two (lower numeric value); env can only make
logging noisier, never quieter. Other fields: env wins outright when
set.

The binary reads env vars at startup; the config file is read once
during `LoadFromEnv`. Direct in-Lua post-call assignments (§11.2)
cannot reach the binary because the file is already written.

---

## §12 — Filesystem contracts

### §12.1 — Path table

| Path | Purpose | Mode | Created by |
|---|---|---|---|
| `$XDG_STATE_HOME/wezsesh/` | state dir parent | 0700 | `state.Open` (MkdirAll) |
| `$XDG_STATE_HOME/wezsesh/state.json` | usage + live_pins | 0600 | `safefs.AtomicWriteFile` |
| `$XDG_STATE_HOME/wezsesh/wezsesh.log` | rotated log | 0600 | `internal/logger` (Enforce(SymlinkRefuse)) |
| `$XDG_DATA_HOME/wezsesh/` | data dir parent | 0700 | `trust.Open` (MkdirAll) |
| `$XDG_DATA_HOME/wezsesh/allow/` | trust store | 0700 | `trust.Open`; Enforce(SymlinkRefuse) |
| `$XDG_DATA_HOME/wezsesh/allow/<sha256>` | trust file | 0600 | `safefs.AtomicWriteFile` |
| `<snapshot_dir>/workspace/<encoded>.json` | resurrect-owned | resurrect | resurrect |
| `<snapshot_dir>/workspace/<encoded>.wezsesh.json` | snapshot sidecar | 0600 | `safefs.AtomicWriteFile` |
| `<picked_path>/.wezsesh.json` | project sidecar | user-authored | user |
| `<reply_dir>/` | reply socket parent | 0700 | binary, with umask 0077 |
| `<reply_dir>/<8-hex>.sock` | reply socket | 0600 | `net.Listen` after umask |
| `<temp>/wezsesh-<pid>-config.json` | binary config (per-spawn) | 0600 | Lua `manager.write_config_file` |

### §12.2 — Reply directory selection

```
opts.runtime_dir set (string)? → use as-is (after SUN_PATH check)
$XDG_RUNTIME_DIR set on Linux? → "$XDG_RUNTIME_DIR/wezsesh/"
darwin                         → "/tmp/wezsesh-<uid>/"
Linux without $XDG_RUNTIME_DIR → "/tmp/wezsesh-<uid>/"
```

### §12.3 — Filename encoding

Workspace name → snapshot filename: `name:gsub("/", "+")`. Reverse for
display. Not bijective for names containing literal `+` (P §5.5
warning surfaced to the user). Both Go and Lua MUST agree on the
transform.

### §12.4 — Cleanup rules

- Reply socket: `defer cleanup()` after `StartListener`
  (close + remove, `sync.Once`); `InstallSignalHandler` runs the same
  on SIGINT/SIGTERM/SIGHUP.
- Startup sweep: `ipcsock.SweepStale(reply_dir, log)` removes `*.sock`
  files with mtime > 60 s. Per-file `safefs.Enforce(SymlinkSkipWarn)`.
- `wezsesh reset --yes` removes:
  - state dir (all contents + dir itself if empty post-cleanup)
  - trust store (all contents + `allow/` dir + `wezsesh/` parent if empty)
  - reply sock dir (all `*.sock` + dir if empty)
  - log files (in state dir, before state-dir removal)
  - `*.wezsesh.json` in `<snapshot_dir>/workspace/` (sidecars only)
  - **Does NOT touch resurrect snapshots** unless
    `--include-snapshots` is also passed.
- `wezsesh reset --yes --include-snapshots` additionally removes
  `<snapshot_dir>/workspace/*.json` (the resurrect-owned files).
  Confirmation prompt is double-stage: prints absolute paths,
  refuses unless stdin is a TTY (or `--yes-i-really-mean-it` is also
  passed for non-TTY use).
- **Symlink defense** (centralised via `safefs.Enforce`):
  - Top-level dirs (snapshot, state, data, runtime): `SymlinkRefuse`
    → ABORT entire run.
  - Individual files inside (sidecars, sock files, log files, trust
    files): `SymlinkSkipWarn` → log_warn, do not unlink.

`wezsesh nuke ...` is a deprecated alias that prints a one-time toast
("nuke renamed to reset; this alias removed in v0.2") and then runs
the same code path.

### §12.5 — Auto-detection rules (no env vars set)

When invoked outside wezterm spawn (`wezsesh doctor` from a shell,
etc.), the binary auto-detects:

- `snapshot_dir`:
  - Linux: `$XDG_STATE_HOME/wezterm/resurrect/` (default
    `~/.local/state/wezterm/resurrect/`).
  - darwin: `~/Library/Application Support/wezterm/resurrect/`
    (resurrect's macOS default).
  - Override via `WEZSESH_SNAPSHOT_DIR` or `--snapshot-dir`.
- `state_dir`:
  - Linux: `$XDG_STATE_HOME/wezsesh/` (default
    `~/.local/state/wezsesh/`).
  - darwin: `~/.local/state/wezsesh/` (XDG semantics on darwin too,
    matching PRD §6.8).
  - Override via `WEZSESH_STATE_DIR` or `--state-dir`.
- `runtime_dir`: per §12.2.

Doctor reports the resolved paths and how each was determined
(env-var / auto / `--flag`).

---

## §13 — State machines

### §13.1 — Request lifecycle (binary)

```
                      [TUI dispatch]
                           │
                           ▼
                  build canonical payload
                           │
                           ▼
                 ipcsock.StartListener(sock)        ◀── synchronous (in Update)
                           │
                           ▼ defer cleanup()
                  uservar.WriteOSC(b64(payload))
                           │
                           ▼
                  tea.Tick(2s) — retransmit Cmd
                           │
              ┌────────────┼────────────────────┐
              │            │                    │
       reply received    2s elapses     5s elapses (first)
              │       no reply yet              │
              │            │                    ▼
              │     uservar.WriteOSC(again)  IPC_TIMEOUT
              │     (replay-guard suppresses)
              ▼
   parse Reply.status
        │
        ├── "completed" → terminal; cleanup()
        ├── "started"   → TUI dismisses; binary stays alive
        │                 to receive 30s-budgeted follow-up
        └── "partial"   → terminal; cleanup()

PANIC PATH (TUI subcommand):
   defer recover() (top of TUI startup)
   → dispatcher.EmergencyReply({ status:"completed", ok:false,
                                  error:{code:"UNEXPECTED_EXIT"} })
     to any open reply socket
   → log
   → os.Exit(2)

POST-TUI PATH:
   tea.Run returns → main.go drives any deferred phases
   (find Phase 2; restore-class follow-up reply consumption) to
   completion via the dispatcher's WaitGroup.
   dispCleanup() runs first; then global cleanup().
```

### §13.2 — Reply socket lifecycle

```
StartListener (synchronous, in Update):
  1. unix.Umask(0077)
  2. listener, _ = net.Listen("unix", sockPath)
  3. unix.Umask(prev)
  4. go acceptLoop(listener, replies)

acceptLoop (sequential — one connection at a time):
  defer recover() at top
  for {
    conn, err := listener.Accept()
    if errors.Is(err, net.ErrClosed) { return }
    if err != nil { log_warn; continue }
    func() {
      defer conn.Close()
      conn.SetReadDeadline(now + 2s)
      bytes, _ := io.ReadAll(io.LimitReader(conn, 1<<20))
      reply := parseReply(bytes)
      replies <- reply         // buffered, cap 2
    }()
  }

cleanup (sync.Once):
  listener.Close()        // unblocks Accept with net.ErrClosed
  os.Remove(sockPath)
```

Channel buffer cap 2: tight fit for split-reply (started + terminal).
Senders block if buffer fills (defensive — should never happen in
normal flow because the consumer always reads).

### §13.3 — Switch poller (`wezcli.StartSwitchPoller`)

```
inputs: ctx (5s parent), pre {TargetClientID, TargetWindowID, ActiveWorkspace},
        target, isRestoreFlow

cadence_ms := 50    -- starts fast; dilates if we observe slow ticks

for ctx.Err() == nil:
  tick_start := now
  clients, err := wezcli.ListClients(ctx)  // 2s sub-ctx per call
  if err != nil { time.Sleep(cadence_ms); continue }
  client := find(clients, ClientID == pre.TargetClientID)
  if client == nil { time.Sleep(cadence_ms); continue }
  panes, err := wezcli.List(ctx)
  if err != nil { time.Sleep(cadence_ms); continue }
  pane := find(panes, PaneID == client.FocusedPaneID)
  if pane == nil { time.Sleep(cadence_ms); continue }

  succeeded :=
    pane.Workspace == target
    AND pane.WindowID == pre.TargetWindowID
    AND ((target != pre.ActiveWorkspace) OR isRestoreFlow)

  if succeeded { return nil }

  tick_elapsed := now - tick_start
  if tick_elapsed < 100ms: cadence_ms = 50
  elif tick_elapsed >= 1s:  cadence_ms = 250    -- slow mux; back off
  time.Sleep(cadence_ms)

on ctx.Done(): return ErrMuxUnreachable
```

**Worst-case latency.** Each tick performs two `wezcli` calls, each
capped at 2 s. On a maximally-slow mux, a single tick can consume up
to 4 s. With a 5 s parent ctx this means 1–2 effective ticks in the
worst case. The 5 s ceiling is therefore the polling budget, not a
true polling interval — document explicitly.

### §13.4 — Save flow (lock-briefly + in-process serialisation)

This is the largest behavioural change vs v2. The lock is no longer
held across the IPC roundtrip.

```
TUI dispatch save{ name, overwrite, expected_hash }
  │
  ▼
  in-process: nameMutex(name).Lock()  defer Unlock()
                                                    ◀── serialises concurrent
                                                        wezsesh saves of the
                                                        same name within this
                                                        binary
  ▼
  lockCtx := WithTimeout(parent, 5s)        // verify-hash budget
  ipcCtx  := WithTimeout(parent, 5s)        // IPC roundtrip budget
                                            // (independent — see §14.1)
  ▼
  PHASE A — verify hash (brief lock):
  if expected_hash == nil:                    // FIRST SAVE
      fd, release, err := safefs.AcquireExclusiveOrCreate(
                              lockCtx, snapshotDir, encodedName, 0600)
      err == ErrLockTimeout → reply SNAPSHOT_LOCKED
      release()                          ◀── release immediately;
                                              file exists empty + 0600
  else:                                       // OVERWRITE
      fd, release, err := safefs.AcquireExclusive(lockCtx, snapshotPath)
      err == ErrLockTimeout → reply SNAPSHOT_LOCKED
      err == ErrNotExist    → reply SNAPSHOT_MISSING
      bytes, err := fd.ReadAll(lockCtx)
      hash := "sha256:" + sha256(bytes)
      release()                          ◀── release BEFORE OSC dispatch
      if hash != expected_hash:
          reply SNAPSHOT_CHANGED
          return
  ▼
  PHASE B — emit save OSC (no lock held):
  dispatcher.Dispatch(ipcCtx, "save", { name, overwrite, expected_hash })
  Lua resurrect.state_manager.save_state(...)
  Lua reply:
    "completed" ok=true       → proceed
    "completed" ok=false      → propagate SAVE_FAILED to TUI; return
                                (Lua includes details.raw_error)
  ▼
  PHASE C — re-hash under brief second lock:
  fd2, release2, err := safefs.AcquireExclusive(
                            WithTimeout(parent, 2s), snapshotPath)
  err == ErrLockTimeout → reply SNAPSHOT_LOCKED  (rare; Lua just wrote)
  bytes2, _ := fd2.ReadAll(...)
  newHash := "sha256:" + sha256(bytes2)
  release2()
  ▼
  state.RecordSwitch(parent, name)  (fire-and-forget)
  if state.IsLivePinned(name):           // §13.11 pin migration
      sidecar.Pinned = true
      r.WriteSidecar(parent, name, sidecar)
      state.SetLivePin(parent, name, false)
  ▼
  surface to TUI: { name, hash: newHash }
```

**Caveats.**
- The in-process per-name mutex (`nameMutex(name)`) guards against
  concurrent saves *within this binary*. Cross-binary concurrency is
  bounded by `AcquireExclusive`, but the lock is held only during
  Phase A and Phase C — not across the full flow.
- Race window: between Phase A's release and Phase B's OSC dispatch,
  another writer could mutate the file. The `expected_hash` semantics
  no longer give the user a watertight guarantee; they give a
  best-effort "file matched what I last saw". This trade-off is
  acceptable because:
  1. The race window is sub-millisecond in practice.
  2. The watertight alternative (lock-during-IPC) is broken anyway —
     resurrect uses `io.open` and ignores POSIX locks.
- The new hash returned to the TUI is computed AFTER Lua finishes,
  so it accurately reflects the on-disk file as Lua wrote it.

**Concurrency model summary.** Same-name concurrent saves within one
binary serialise on `nameMutex(name)`. Cross-binary concurrent saves
serialise during Phase A (the first to get the file lock wins the
hash check; the second sees `SNAPSHOT_CHANGED`). Concurrent
*non-wezsesh* writes (resurrect-direct) clobber freely; documented
v0.1 limitation.

### §13.5 — Hook trust check

```
inputs: sidecar_abs_path  (already verified non-symlink at read time
                           via safefs.SafeOpenForRead),
        command_bytes     (read from sidecar in memory; never re-read)

hash := trust.ComputeHash(sidecar_abs_path, command_bytes)
trustfile := <trust_dir>/<hash>

safefs.Enforce(trustfile, SymlinkSkipWarn, log)  →
  ENOENT or symlink → untrusted
  ok=true, regular file → trusted

if trusted:
    exec hook (per §13.5.1)
else if hooks.prompt_on_untrusted:
    prompt user; on approve → trust.Approve + exec
else:
    log_warn
    toast(`wezsesh: on_<verb> not trusted for "<name>". Run 'wezsesh trust <name>' to approve.`)
    no exec
```

#### §13.5.1 — Hook exec environment

```
exec.CommandContext(ctx, shell, "-c", command_bytes)
ctx = context.WithTimeout(parent, hooks.timeout_seconds * time.Second)

shell := os.Getenv("SHELL"); fall back to "/bin/sh"
Cmd.Dir := primaryCwd; if !exists, fall back to os.UserHomeDir()
                       ("primaryCwd" is the workspace's preferred cwd:
                        for `new`, the picked path; for `load`/`switch`,
                        the active pane's cwd at restore time.)
Cmd.Stdin = nil
Cmd.Stdout = os.Stderr (inherit)
Cmd.Stderr = os.Stderr (inherit)
Cmd.SysProcAttr = &syscall.SysProcAttr{ Setpgid: true }

Cmd.Env = os.Environ() filtered: drop ONLY these three sensitive keys:
            WEZSESH_HMAC_KEY
            WEZSESH_PROTO_VERSION
            WEZSESH_CONFIG_FILE
          User-tunable WEZSESH_LOG / WEZSESH_NO_HOOKS /
          WEZSESH_NERDFONT survive.

on ctx.Done():
    syscall.Kill(-cmd.Process.Pid, SIGTERM)
    wait min(5s, hooks.timeout_seconds / 10)     // proportional grace
    syscall.Kill(-cmd.Process.Pid, SIGKILL)
log_warn("hook timed out after %ds")
```

#### §13.5.2 — Trust rebind

When a project sidecar moves to a new path, its trust hash changes
(hash binds path + command). `wezsesh trust --rebind <old-abs>
<new-abs>` transfers the approval without re-prompting:

```
read newPath/.wezsesh.json (or fail if absent / untrusted-shape)
read oldPath/.wezsesh.json (must exist and have identical command_bytes)
oldHash := ComputeHash(oldPath, cmdBytes)
newHash := ComputeHash(newPath, cmdBytes)

if !trust.IsApproved(ctx, oldPath, cmdBytes):
    return TRUST_REBIND_MISSING
trust.Approve(ctx, newPath, cmdBytes)        // write new hash file
trust.Revoke(ctx, oldPath, cmdBytes)         // remove old hash file
```

If the new path's command bytes differ from the old, the rebind
refuses (would be a silent uplift of approval scope) — the user must
run `wezsesh trust <new>` manually.

### §13.6 — Encrypted snapshot operations

```
operation         | works on encrypted? | mechanism
------------------|---------------------|---------------------------------------------
switch (live)     | yes                 | mux only; file unread
load / restore    | yes                 | resurrect.state_manager.load_state decrypts
save (overwrite)  | yes                 | hash over raw bytes; resurrect rewrites
rename            | yes                 | os.Rename of .json + sidecar
delete            | yes                 | filesystem op
tag / pin         | yes                 | sidecar is plaintext, separate
preview           | DEGRADED            | "(encrypted snapshot — preview unavailable)"
```

### §13.7 — Find two-phase

```
match := pick from search results
preCtx := wezcli.CapturePreSwitchState(ctx, currentWindowID)

if match.Workspace == preCtx.ActiveWorkspace AND match.WindowID == preCtx.TargetWindowID:
    skip Phase 1
else:
    progress(PhaseSwitchStarted)
    dispCtx, dispCancel := context.WithCancel(ctx)
    replies, err := dispatcher.Dispatch(dispCtx, "switch",
                                        { "name": match.Workspace })
    // For a live target (always the case for find — match comes from
    // a live pane), Lua replies "completed" with ok=true. Find
    // ignores the reply contents and polls the mux instead.
    err := wezcli.StartSwitchPoller(WithTimeout(5s), preCtx,
                                    match.Workspace, false)
    if err == ErrMuxUnreachable:
        dispCancel(); for range replies {}     // drain
        progress(PhaseSwitchTimeout)
        return MUX_UNREACHABLE
    progress(PhaseSwitchSucceeded)

    dispCancel()                  // close listener; Dispatcher closes channel
    for range replies {}          // drain remaining replies (one or zero)

progress(PhaseActivateStarted)
err := wezcli.ActivatePane(WithTimeout(2s), match.PaneID)
if err == ErrPaneClosedRace:
    return PANE_CLOSED_RACE
progress(PhaseActivateDone)
return nil
```

TUI MUST render a one-line progress status during Phase 1
(`Switching workspace...` → `Activating pane...`).

The drain protocol (`dispCancel()` + `for range replies {}`) is
mandatory: it ensures the listener goroutine exits and the reply
socket is unlinked before Phase 2 begins.

### §13.8 — Quit-mid-op

```
state := { op_in_flight: bool }
on dispatch:        op_in_flight = true
on terminal reply:  op_in_flight = false
on IPC_TIMEOUT:     op_in_flight = false

key q | Esc:
  if not op_in_flight:
      tea.Quit
  else:
      render inline status: "op in progress, quit anyway? [y/N]"
      key y → tea.Quit (orphan reply socket; reaped by next launch)
      any other → dismiss, stay
```

### §13.9 — SUN_PATH validation (Lua side)

```lua
if type(opts.runtime_dir) ~= "string" then
    error("WEZSESH_RUNTIME_DIR_TYPE: opts.runtime_dir must be a string path", 0)
end

local expanded = opts.runtime_dir
if expanded:sub(1, 2) == "~/" then
    expanded = (wezterm.home_dir or os.getenv("HOME") or "") .. expanded:sub(2)
end

local ceiling = (wezterm.target_triple:match("%-apple%-darwin") and 104) or 108
local needed = #expanded + 14   -- "/<8hex>.sock"

if needed > ceiling then
    error(string.format(
        "WEZSESH_SUN_PATH_OVERFLOW: runtime_dir too long for AF_UNIX SUN_PATH " ..
        "(needed=%d, ceiling=%d, path=%q). Shorten or use the default.",
        needed, ceiling, expanded), 0)
end
```

Go re-validates at runtime: returns `IPC_INIT_FAILED` if violated.

### §13.10 — Sort comparators

Each `SortMode` produces a strict total order. Comparator chains
(returning `<0` / `0` / `>0` for `a` vs `b`):

**`live_first` (default):**
1. Pinned-first: `a.Pinned > b.Pinned`.
2. Within pinned: `a.Live > b.Live`. Within live: `a.Active > b.Active`.
3. Within saved or non-pinned-non-live: by `Mtime` descending
   (newest first; missing `Mtime` sorts last).
4. Tie-break: `a.Name < b.Name` (NFC byte order).

**`recent`:**
1. Pinned-first.
2. By `state.LastSwitched` descending. Workspaces with no record
   (`0`) fall through to `Mtime` descending.
3. Tie-break: `Name` (NFC byte order).

**`mtime`:**
1. By `Mtime` descending. Live-unsaved sorts at the end.
2. Tie-break: `Name` (NFC byte order).

**`alphabetical`:**
1. By `Name` ascending — **byte order over NFC-normalised UTF-8**.
   This is locale-naive: ASCII A–Z sort intuitively, but multibyte
   ranges (Cyrillic, CJK, Greek, accented Latin) sort by code point,
   which does NOT match locale-aware expectations (e.g., Spanish "ñ"
   sorts after "z", Swedish "å" sorts after "z"). Locale-aware
   ordering is a v0.2+ candidate — the cost is pulling in `x/text`
   collation tables (~500 KB) and threading a locale through the
   sort path.

### §13.11 — Pin storage (single source of truth)

Two storage locations, **disjoint by construction**:
- Snapshot sidecar `pinned: true|false` — authoritative for SAVED
  workspaces. Travels with the snapshot (portable).
- `state.json.live_pins[]` — pins for LIVE-ONLY workspaces (no
  snapshot). Local to this machine.

On startup, the picker builds an in-memory pin set as the union of
both sources. They cannot disagree about a single workspace because
they cover disjoint domains:

```
for entry in repo.List():
    if entry.SidecarOK and entry.Sidecar.Pinned: pinned.add(entry.Name)
for name in state.LivePins():
    if not repo.Has(name):                     // sanity prune
        pinned.add(name)
```

Doctor check `snapshot.pin.consistency` warns if `live_pins ∩
saved-names ≠ ∅` (would indicate a stale `live_pins` entry; the
doctor offers `wezsesh trust --prune`-style reconciliation).

Operations (all signatures take `ctx`):

```
pin(ctx, name=L, true)   live-only (no sidecar):
   state.SetLivePin(ctx, name, true)

pin(ctx, name=S, true)   saved (sidecar exists):
   sidecar.Pinned = true
   r.WriteSidecar(ctx, name, sidecar)
   // state.json untouched — sidecar is authoritative for saved

pin(ctx, name=S, false)  saved:
   sidecar.Pinned = false
   r.WriteSidecar(ctx, name, sidecar)

migration on save (live-pinned → newly-saved):
   on save success of name where state.IsLivePinned(name):
       sidecar.Pinned = true
       r.WriteSidecar(ctx, name, sidecar)
       state.SetLivePin(ctx, name, false)        // remove live-only entry
```

### §13.12 — Binary-only operation flows

These are NOT IPC verbs; the binary executes them directly without
OSC roundtrip. Errors return via in-process channels to the TUI;
surface category is `binary-only` (§7).

#### §13.12.1 — `rename` (live and/or saved)

```
inputs: old, new, overwrite
  │
  ▼
nameval.ValidateWorkspaceName(old) and ValidateWorkspaceName(new)
                                                  → ILLEGAL_NAME

if old == new:
    return success (no-op)

isLive := wezcli.List(ctx) contains old
isSaved := snapshots.Repo.Has(ctx, old)

// Collision check
if (live: wezcli.List contains new) or
   (saved: Repo.Has(new) and !overwrite):
    return RENAME_COLLISION

// Live rename (if applicable)
if isLive:
    err := wezcli.RenameWorkspace(ctx, old, new)
    if ErrRenameCollision:
        return RENAME_COLLISION
    if err != nil:
        return MUX_UNREACHABLE

// Saved rename (if applicable)
if isSaved:
    err := snapshots.Repo.Rename(ctx, old, new)   // locks both files
    err == ErrLockTimeout → return SNAPSHOT_LOCKED
    err == ErrNotExist    → return SNAPSHOT_MISSING

return success
```

#### §13.12.2 — `delete` (saved snapshots, possibly bulk)

```
inputs: names []string
results := []
for each name in names:
    err := snapshots.Repo.Delete(ctx, name)
    classify err → SNAPSHOT_MISSING / SNAPSHOT_LOCKED / nil
    append (name, err) to results

return results       // best-effort; not transactional
```

TUI displays a single combined progress + final summary:
"Deleted X of N; Y errors".

Confirmation modals are TUI-side:
- Single delete: `Delete '<name>' [y/N]`.
- Bulk delete: `Delete N marked workspaces? [y/N]`. List names if N≤5.

#### §13.12.3 — `tag`

```
inputs: name, tags []string

nameval.ValidateWorkspaceName(name)        → ILLEGAL_NAME (field=name)
nameval.ValidateTags(tags)                 → ILLEGAL_NAME (field=tags[i])

sidecar, ok, err := r.ReadSidecar(ctx, name)
if !ok: start with zero Sidecar (Version=1)
sidecar.Tags = tags
err := r.WriteSidecar(ctx, name, sidecar)   // locks
err == ErrLockTimeout → SNAPSHOT_LOCKED

return success
```

#### §13.12.4 — `pin`

```
inputs: ctx, name, pinned bool

nameval.ValidateWorkspaceName(name)        → ILLEGAL_NAME

isSaved := r.Has(ctx, name)
if isSaved:
    sidecar, _, _ := r.ReadSidecar(ctx, name)
    sidecar.Pinned = pinned
    err := r.WriteSidecar(ctx, name, sidecar)
    err == ErrLockTimeout → SNAPSHOT_LOCKED
else:
    err := state.SetLivePin(ctx, name, pinned)
    err != nil → XDG_PATH_TIMEOUT

return success
```

### §13.13 — Unknown-verb handling

Lua's `ops.dispatch` (§9.4) checks `dispatch_table[payload.op]`. When
nil:

```
log_warn("UNKNOWN_VERB op=" .. payload.op .. " id=" .. payload.id)
result.reply_error(payload, "UNKNOWN_VERB",
                   "unknown verb: " .. payload.op,
                   { received_op = payload.op })
return
```

This is a terminal `completed` reply with `ok=false`. The reply
satisfies the TUI's first-reply ceiling (5 s) so the user sees a
specific error rather than a generic `IPC_TIMEOUT`.

### §13.14 — Non-TUI subcommand panic paths

Each subcommand has a thin top-level recover that:
1. Logs the panic with stack at LevelError (the synchronous flush
   guarantees crash-loss is bounded by the kernel write barrier).
2. Prints a one-line error to stderr in the form
   `wezsesh <subcommand>: panic: <err>`.
3. Exits with status:
   - `2` for `doctor`, `list`, `find`, `trust`, `reset`.
   - `3` for `keygen` (so the Lua `ensure_session_key` chain falls
     through to step 2 of §5.2).
   - `2` for `reply` (the Lua side has no synchronous wait on this;
     the TUI just hits `IPC_TIMEOUT`).

No sentinel reply is emitted for these subcommands because they
don't hold a reply socket. The TUI subcommand's panic path (§13.1)
is the only one that emits a wire sentinel.

---

## §14 — Concurrency & timeouts

### §14.1 — Timeout table

| Surface | Ceiling | Mechanism | Failure code |
|---|---|---|---|
| Single `wezterm cli` invocation | 2 s | `context.WithTimeout` | `MUX_UNREACHABLE` |
| Snapshot dir scan (`Repo.List`) | 5 s | `context.WithTimeout` | partial list + non-fatal toast |
| `state.json` / trust file read | 2 s | `context.WithTimeout` | `XDG_PATH_TIMEOUT` |
| Path picker exec | 15 s | `context.WithTimeout` | `PATH_PICKER_TIMEOUT` |
| Hook exec | 600 s (configurable; max 86400) | `context.WithTimeout` + group SIGTERM/KILL | (logged; no IPC code) |
| `AcquireExclusive` lock — save Phase A | 5 s | poll loop under `lockCtx` | `SNAPSHOT_LOCKED` |
| `AcquireExclusive` lock — save Phase C re-hash | 2 s | poll loop | `SNAPSHOT_LOCKED` (rare) |
| Save IPC roundtrip | 5 s | `ipcCtx` (independent of lockCtx) | `IPC_TIMEOUT` / `SAVE_FAILED` |
| First IPC reply | 5 s | TUI-side timer | `IPC_TIMEOUT` |
| Follow-up after `started` | 30 s | TUI-side timer | non-fatal toast |
| OSC retransmit | 2 s | `tea.Tick` once | suppressed via replay guard |
| `Statfs` inside `IsNetworkFS` | 2 s | goroutine + ctx | classify as network |
| `EvalSymlinks` inside `IsNetworkFS` | 500 ms | goroutine + ctx | use unresolved + WARN |
| Switch poller parent | 5 s | `context.WithTimeout` | `MUX_UNREACHABLE` |
| Switch poller cadence | 50–250 ms (adaptive) | `time.Sleep` | n/a |
| Switch poller worst-case per-tick | 4 s | two 2 s sub-ctx calls | (caps total ticks at 1–2) |
| Encryption agent doctor probe | 2 s | `context.WithTimeout` | `ENCRYPTION_AGENT_SLOW` |
| Per-connection reply read | 2 s | `SetReadDeadline` | conn close, log |

### §14.2 — Goroutine hygiene rules

- Every goroutine in `internal/ipcsock`, `internal/ipcdispatcher`,
  `internal/wezcli`, `internal/argvallow`, `internal/find`,
  `internal/tui` MUST have a top-level `defer recover()` that logs and
  exits cleanly. Lint-checked (§17.4).
- All tests exercising `StartListener`, `StartSwitchPoller`,
  `InstallSignalHandler`, `ipcdispatcher.New`, or `tea.Run` MUST end
  with:
  ```go
  defer goleak.VerifyNone(t,
      goleak.IgnoreTopFunction("internal/ipcsock.installSignalHandlerWorker"))
  ```
- Cancellation primitive: `context.Context` only. Never raw
  `time.AfterFunc`. `tea.Tick` is the only timer in `tea.Cmd` bodies
  (`tea.After` does not exist in any released bubbletea version).
- The `tui.Model` MUST track a `replyReceived bool` field; `Update`
  ignores `retransmitMsg` when set.
- `ipcdispatcher` keeps a `sync.WaitGroup` over outstanding requests;
  `main.go` waits on it post-`tea.Run` before invoking cleanup
  (replaces v2's "open replies channels" check).

### §14.3 — Lua handler synchrony rule

Steps (a)–(h) of `ipc.lua` (§9.3) MUST be synchronous Lua bytecode.
CI lint (§17.4) parses the AST and fails on any call to a known-async
wezterm API in that range:
- `wezterm.run_child_process`
- `wezterm.sleep_ms`
- any `add_async_function`-exposed API enumerated in
  `internal/lualint/async_funcs.go`

`wezterm.background_child_process` is fire-and-forget; permitted in
step (i) only.

---

## §15 — Validation rules

Single source of truth: `internal/nameval`.

### §15.1 — Workspace name

| Rule | Check |
|---|---|
| Length | `1 ≤ len(name_utf8_bytes) ≤ 200` |
| No NUL, LF, CR, TAB, other C0 (0x01–0x1F) | byte scan |
| No DEL (U+007F) | byte/codepoint scan |
| No C1 controls (U+0080–U+009F as valid UTF-8) | codepoint scan |
| No U+2028, U+2029 | codepoint scan |
| No leading/trailing whitespace; no all-whitespace | trim check |
| Not exactly `"."` or `".."` | exact match |
| No backslash `\` | byte scan |
| `+` allowed but warned at save/rename time (UI hint) | post-validation |
| NFC-normalize before storing/comparing | always |

The `+` warning fires in the TUI's save and rename modals when the
input contains a literal `+` character; copy is "Note: '+' will
collide with '/'-encoded names on disk." Validation does not fail.

Failure → `ILLEGAL_NAME` with `details.field = "name"`,
`details.reason = "<human>"`.

### §15.2 — Tag

| Rule | Check |
|---|---|
| Tag count per workspace | `1 ≤ N ≤ 10` |
| Tag length | `1 ≤ len(tag_utf8_bytes) ≤ 50` |
| Same byte rules as workspace name | (NUL/C0/TAB/DEL/C1/LS/PS/leading-trailing-WS forbidden) |
| NFC-normalize | always |

Failure → `ILLEGAL_NAME` with `details.field = "tags[i]"`.

### §15.3 — Path picker output line

| Rule | Check |
|---|---|
| UTF-8 valid | `utf8.ValidString` |
| No NUL byte | byte scan |
| Strip trailing `\r` | preprocess |
| Non-empty after trim | check |
| Tilde-expandable (`~/...` only; `~user/...` not supported) | `os.UserHomeDir()` |
| `filepath.IsAbs` after expand | check |
| `os.Stat`.IsDir | check |
| Symlinked dirs accepted | (no rejection) |

Failure → log and skip the line; do not abort the picker.

### §15.4 — Render-time sanitization

`SanitizeForDisplay(s)` replaces every byte in `0x00`–`0x1F` (except
`\t`) and `0x7F` with U+FFFD. Also handles valid-UTF-8 representations
of `U+0080`–`U+009F`. Apply at:
- Picker row render (lipgloss)
- Preview pane render (lipgloss)
- Modal labels
- Toast messages
- `internal/logger` — log lines containing user-controlled strings
- Doctor output

### §15.5 — Name truncate algorithm

`name_truncate = "middle"` (only mode in v0.1):

```
input:   "<prefix>...<suffix>"
target:  width W (cells, lipgloss-measured)

if cellWidth(input) <= W: return input
ellipsis := "…"  (1 cell)
budget := W - cellWidth(ellipsis)
prefix_cells := budget / 2
suffix_cells := budget - prefix_cells
return runesFromLeft(input, prefix_cells) + ellipsis + runesFromRight(input, suffix_cells)
```

Implementation: `lipgloss/v2`'s `truncate.StringWithTail` with middle
mode, or in-house if not available. Cell width via
`github.com/mattn/go-runewidth`.

---

## §16 — Build, dependencies, lint rules

### §16.1 — Go version & flags

```
go.mod          : go 1.26.2
release build   : go build -trimpath \
                          -ldflags="-s -w -X main.version=v$(git describe --tags --always)"
```

### §16.2 — Pinned dependencies (Go)

| Module | Version | Notes |
|---|---|---|
| `github.com/charmbracelet/bubbletea/v2` | `v2.0.6` | module path `/v2` |
| `github.com/charmbracelet/bubbles/v2` | `v2.1.0` | |
| `github.com/charmbracelet/lipgloss/v2` | `v2.0.3` | |
| `github.com/charmbracelet/x/ansi` | `v0.11.7` | |
| `github.com/charmbracelet/huh/v2` | `v2.0.3` | modal forms |
| `github.com/sahilm/fuzzy` | `v0.1.1` | NUL-byte panic fix included |
| `github.com/mattn/go-runewidth` | latest | display-width measurement |
| `golang.org/x/sys/unix` | latest | `O_NOFOLLOW`, `Openat`, `Renameat`, `Umask`, `F_OFD_SETLK` (Linux) |
| `golang.org/x/text/unicode/norm` | latest | NFC |
| `go.uber.org/goleak` | `v1.3+` | test-only |

### §16.3 — Vendored Lua (supply chain)

```
plugin/wezsesh/vendor/sha2.lua            ← Egor-Skriptunoff/pure_lua_SHA
                                          ← pinned commit 6adac177c16c3496899f69d220dfb20bc31c03df
plugin/wezsesh/vendor/SOURCES.lock        ← upstream commit + sha256 of file
```

### §16.4 — Required CI gates

| Gate | Command |
|---|---|
| Module verify | `go mod verify` |
| Vulnerability | `govulncheck ./...` |
| Static analysis | `staticcheck ./...` |
| Vet | `go vet ./...` |
| Vendored crypto integrity | `sha256sum -c plugin/wezsesh/vendor/SOURCES.lock` |
| `default_allowlist.lua` codegen freshness | `go run ./internal/argvallow/codegen --check` |
| Reproducible build | `go build -trimpath -ldflags='-s -w -X main.version=v...'` |
| Canonical-JSON locale | `LC_ALL=C go test ./internal/canonicaljson/... ./plugin/...` |
| Build matrix | `linux-amd64`, `linux-arm64`, `darwin-amd64`, `darwin-arm64`; CI runners pin macos-13 and macos-14 |
| Lua version assertion | wezterm shipped Lua `_VERSION` ≥ "Lua 5.3" |
| Verb / shape parity | `verb_args_shape` keys == `dispatch_table` keys |

### §16.5 — Custom CI lints

| Lint | Implementation | Failure |
|---|---|---|
| `unix.F_OFD_SETLK` outside `internal/safefs/lock_linux.go` | grep + AST walk | build error |
| `os.WriteFile`/`os.OpenFile`/`syscall.Open` in `internal/{snapshots,state,trust}` etc | AST walk | build error |
| Direct `wezterm cli` invocation outside `internal/wezcli/` | grep `exec.Command` + `"wezterm"` | build error |
| Concrete Dispatcher construction outside `internal/ipcdispatcher/` | grep `ipcsock.StartListener` callsites | build error |
| Lua handler sync-only between markers (a)–(h) | `internal/lualint` parser | PR fail |
| `tea.After` references | grep | build error |
| `pcall`-wrap on `wezterm.background_child_process` calls | AST walk in `result.lua`, `ipc.lua` | PR fail |
| `defer recover()` presence in goroutines in restricted packages | AST walk for `go func` and goroutine-bodies | PR fail |
| `log.Println`/`fmt.Fprintln(os.Stderr, ...)` in restricted packages | AST walk; must use `internal/logger` | PR fail |
| `verb_args_shape` parity | reflective check: keyset(dispatch_table) == keyset(verb_args_shape) | PR fail |

---

## §17 — Testing contracts

### §17.1 — Canonical-JSON golden corpus

Fixture file format (committed):
```
testdata/canonical_json/<name>.lua_input    -- expression that produces the value (e.g., M.object{x = 1})
testdata/canonical_json/<name>.go_input     -- Go literal expression
testdata/canonical_json/<name>.expected     -- raw expected canonical bytes
```

Required vectors (`<name>` and what it tests):

```
empty_object              M.object{}                             → {}
empty_array               M.array{}                              → []
empty_string              ""                                     → ""
nul_in_string             " "                               → " "
del_byte                  ""                               → ""
ls_ps                     "  "                         → "  "
multibyte_utf8            "café"                                 → "café"
cjk                       "日本語"                               → "日本語"
emoji                     "🦀"                                   → "🦀"
nested_3deep              { a = { b = { c = 1 } } } as object   → {"a":{"b":{"c":1}}}
mixed_array               [1, "x", true, M.NULL]                 → [1,"x",true,null]
int_min                   -9223372036854775808                   → -9223372036854775808
int_max                   9223372036854775807                    → 9223372036854775807
int_zero                  0                                      → 0
neg_one                   -1                                     → -1
boolean_true              true                                   → true
explicit_null             M.NULL                                 → null
forward_slash             "a/b"                                  → "a/b"   (NEVER escaped)
```

Plus per-verb fixtures: one per verb in §6 with realistic + edge args.

CI runner: both encoders run on the same fixture corpus; bytes are
diff'd. Any divergence fails the build. CI runs under `LC_ALL=C`.

### §17.2 — HMAC round-trip fixture

```
key_hex = "a0b1c2d3e4f5a0b1c2d3e4f5a0b1c2d3e4f5a0b1c2d3e4f5a0b1c2d3e4f5a0b1"
canonical_json (noop fixture, including v):
    {"args":{},"hmac":"<computed>","id":"01JABCDEFGHIJKLMNPQRSTUVWXY",
     "op":"noop","reply_sock":"/tmp/x.sock","target_window_id":1,
     "ts":1700000000,"v":1}
expected_hmac = <pre-computed via reference RFC-4231 tooling, committed>
```

Both encoders MUST emit `expected_hmac` for the canonical sans-hmac
form. Both directions tested:
- Lua signs → Go verifies
- Go signs → Lua verifies

### §17.3 — Required tests by surface

| Test | Package | Asserts |
|---|---|---|
| Multi-window broadcast (#3524) | `plugin` integration | only window with matching `target_window_id` dispatches |
| Save flock serialisation (Phase A) | `internal/safefs`, `cmd/wezsesh` | one succeeds, other gets `SNAPSHOT_LOCKED` during Phase A |
| Save first-write (no expected_hash) | `internal/safefs`, `cmd/wezsesh` | `AcquireExclusiveOrCreate` creates, locks, releases; concurrent first-saves serialise via the per-name in-process mutex |
| Save with stale hash (Phase A reject) | `cmd/wezsesh` | mismatch → `SNAPSHOT_CHANGED`; user re-confirm with refreshed hash succeeds |
| Save Lua-side failure | `cmd/wezsesh`, `plugin` | resurrect.save_state errors → reply with `ok=false, error.code=SAVE_FAILED, details.raw_error` |
| Save Phase C re-hash | `cmd/wezsesh` | reply.data.hash matches sha256 of file as written by Lua |
| Save in-process serialisation | `cmd/wezsesh` | two concurrent same-name saves in one binary run sequentially via nameMutex; no races |
| Switch-poller false-positive | `internal/wezcli` | `switch` to active short-circuits in 1 tick; `switch+restore` bypasses via `isRestoreFlow` |
| Switch poller adaptive cadence | `internal/wezcli` | slow ListClients (1.5 s tick) → cadence dilates to 250 ms |
| Two-phase find drain | `internal/find` | post-poller dispCancel + drain → channel closes within 100 ms; goroutines exit cleanly |
| Two-phase find client pinning | `internal/find` | second client gaining "most recent" mid-poll does NOT flip predicate |
| Two-phase find window scoping | `internal/find` | closing wezterm window mid-Phase-1 → `MUX_UNREACHABLE` |
| Resurrect race | `internal/snapshots` | mid-write parse failure recovers via 3× retry |
| Reply socket lifecycle | `internal/ipcsock` | listener exits via `net.ErrClosed`; cleanup is `sync.Once` |
| Reply socket sequential accept | `internal/ipcsock` | second connection waits for first to close |
| Reply channel buffer | `internal/ipcsock` | producer blocks at cap 2; never panics |
| `tea.Tick` retransmit cancellation | `cmd/wezsesh` | timer goroutine exits within 100 ms of `tea.Run` return |
| F_OFD_SETLK build-tag | CI | reference outside `lock_linux.go` fails build |
| O_CLOEXEC inheritance | `internal/safefs` | lock fd NOT in fork-spawned child's fd table |
| F_SETLK polling fairness | `internal/safefs` | 3 contending binaries, lock held 100 ms → others acquire within 5 s; WARN fires at 1 s and 3 s |
| `safefs.Enforce` SkipWarn vs Refuse | `internal/safefs` | top-level dir symlink → Refuse error; file inside → SkipWarn returns ok=false, no err |
| Project sidecar trust enforcement | `cmd/wezsesh`, `internal/trust` | untrusted `.wezsesh.json` → no exec; toast surfaces; `wezsesh trust` approves |
| Trust rebind happy path | `internal/trust` | identical command_bytes at new path → rebind succeeds; old hash file removed |
| Trust rebind diverged command | `internal/trust` | new path has different command_bytes → rebind refuses, old approval intact |
| `wezsesh reset` symlink defense | `cmd/wezsesh` | pre-placed symlink at state dir → ABORT; pre-placed symlink at `<state>/state.json` → SKIP+WARN |
| `wezsesh nuke` deprecation alias | `cmd/wezsesh` | invoking `nuke` runs `reset` and prints deprecation toast |
| `wezsesh reset --include-snapshots` | `cmd/wezsesh` | confirmation gate enforced; only on `--yes` does it remove resurrect files |
| Argv allowlist enforcement | `internal/argvallow`, `plugin` | `argv[1]="rm"` → no exec; `cd <cwd>` if cwd clean |
| Argv hook fail-CLOSED | `plugin` | forced exception → no `default_on_pane_restore` invocation |
| Argv default list sync | CI | `internal/argvallow/default.txt` ↔ `default_allowlist.lua` byte-equal under codegen |
| Control-char `cwd`/argv | `plugin` | `cwd="/tmp/foo\nrm -rf ~"` → no injection (downgrade to no-op) |
| Render-time sanitization | `internal/nameval`, `internal/tui` | snapshot named `\x1b[2J` does not clear terminal |
| `safefs.IsNetworkFS` detection | `internal/safefs` | tmpfs → `("tmpfs", false)`; NFS (when available) → `("nfs", true)` |
| Lua handler fuzz | `plugin` integration | 10 000 mutated bytes; no Lua error escapes; `ops.dispatch` invocations = 0 for non-authenticated; wezterm GUI paint < 50 ms |
| Verb-aware tagging round-trip | `plugin` | empty `args = {}` for `noop` verifies; the same shape parsed and re-encoded matches Go's canonical bytes |
| HMAC mismatch silent on wire | `plugin` | corrupted payload → no reply on socket; binary hits IPC_TIMEOUT; toast surfaces in wezterm |
| Freshness boundary | `plugin` | `ts=now-30` accept; `ts=now-31` reject; `ts=now+30` accept; `ts=now+31` reject |
| `seen_ids` TTL prune (session-wide) | `plugin` | entries older than 60 s dropped on `window-config-reloaded` and end-of-dispatch; same ULID across panes deduplicated |
| Schema migration `state.json` v=1 → live_pins | `internal/state` | v=1 file with old `pins` key → migrated to `live_pins`; entries with corresponding snapshot are dropped |
| Schema migration `state.json` v>1 | `internal/state` | v=2 file → backed up to `.v2.bak` + reinitialised; no error |
| Schema migration sidecar | `internal/snapshots` | v=2 sidecar → backed up to `.v2.bak` + ReadSidecar returns ok=false |
| Pin sync on save (live → saved) | `cmd/wezsesh` | live-pinned workspace → save → sidecar.Pinned=true; state.LivePins removes the entry |
| Pin doctor consistency | `internal/doctor` | `live_pins ∩ saved-names ≠ ∅` → warn |
| SUN_PATH overflow | `plugin` + `cmd/wezsesh` | over-budget runtime_dir → Lua sentinel + 10s toast; Go `IPC_INIT_FAILED` |
| `wezsesh keygen` output | `cmd/wezsesh` | exits 0; stdout is exactly 65 bytes (64 hex + `\n`); 64-hex matches `^[a-f0-9]{64}$` |
| Reply `v` field echo | `cmd/wezsesh`, `plugin` | request `v=1` → reply has `v=1`; reply with missing `v` is rejected at Reply parse |
| Unknown verb reply | `plugin` | `op="bogus"` → reply `error.code=UNKNOWN_VERB`, `ok=false`, `status=completed` |
| Hook env: WEZSESH_LOG survives | `cmd/wezsesh` | hook sees `$WEZSESH_LOG`; does NOT see `$WEZSESH_HMAC_KEY` / `$WEZSESH_PROTO_VERSION` / `$WEZSESH_CONFIG_FILE` |
| Logger Warn/Error sync flush | `internal/logger` | crash-after-Warn → log file contains the Warn line on disk |
| Config Exclude invalid regex | `internal/config`, `internal/doctor` | bad regex → ExcludeErrors populated; doctor reports it; runtime treats element as no-op |

### §17.4 — CI lint suite

| Lint | Tool | Trigger |
|---|---|---|
| Lua handler `.await`-free | `internal/lualint` AST walker | call to known-async fn between markers `(a)`–`(h)` in `ipc.lua` |
| `os.WriteFile`/`os.OpenFile`/`syscall.Open` ban | AST walker | usage in restricted packages |
| `unix.F_OFD_SETLK` outside `lock_linux.go` | grep | any reference |
| `tea.After` reference | grep | any reference |
| `pcall`-wrap on async spawns | AST walker | unwrapped `wezterm.background_child_process` |
| `defer recover()` in goroutines | AST walker | bare `go func() { ... }` without top-level recover in restricted packages |
| Direct `wezterm cli` exec outside `internal/wezcli/` | grep | bare `exec.Command("wezterm", ...)` outside the package |
| Concrete Dispatcher outside `internal/ipcdispatcher/` | grep | `ipcsock.StartListener` callsite outside the package |
| Vendored SHA tampering | `sha256sum -c` | mismatch |
| Default-allowlist sync | codegen tool | source `default.txt` ↔ generated `default_allowlist.lua` mismatch |
| Verb / shape parity | reflective check | `dispatch_table` keys ≠ `verb_args_shape` keys |
| Locale | run `LC_ALL=C` | test diff failure |

### §17.5 — Fuzz test mutation classes (Lua handler)

```
random_bytes              raw bytes 0–4096 length
b64_garbage               valid base64 of random bytes
b64_malformed_json        valid base64 of malformed JSON
field_missing             valid JSON missing each required field one at a time
type_swapped              ts="string", args=42, target_window_id="x"
float_subtype             ts=1.5, target_window_id=2.0
untagged_table            args = bare lua {} (no metatable)
oversized_string          id = string.rep("X", 1<<20)
nested_deep               args = 200-deep nested object
control_char_field        name = "\x00\x01\x1b[2J"
empty_args_per_verb       args = {} for each verb in §6
hmac_corrupted            valid payload, last hex char flipped
ts_boundary               ts in {now-31, now-30, now+30, now+31}
unknown_verb              op = "bogus"  (asserts UNKNOWN_VERB reply, not panic)
v_field_swap              v="1", v=2, v=null  (asserts strict numeric == 1)
```

Assertions per fuzz iteration:
- No Lua error escapes the `user-var-changed` handler.
- `ops.dispatch` invocation count remains zero unless the input passes
  HMAC verify (impossible for random mutations against a known key).
- No reply written on HMAC mismatch (silent-drop verification).
- Frame paint time stays < 50 ms throughout.

### §17.6 — End-to-end smoke test

A minimal integration test that exercises the full live stack
(wezterm + plugin + binary). Marked `//go:build e2e` and executed in
a dedicated CI job with a real wezterm binary.

```
Setup:
  - spawn wezterm in headless mode (`wezterm start --always-new-process
    -- /bin/sh -c "sleep 60"`)
  - install plugin via temp wezterm.lua pointing at the local checkout
  - wait for the picker keybinding to register

Scenarios:
  1. open picker, observe at least one row (the live workspace)
  2. press 's' on first row → picker closes; binary reply consumed
  3. invoke `save` via keybinding → snapshot file appears on disk;
     sidecar created; reply.data.hash matches file sha256
  4. invoke `delete` on the saved snapshot → file disappears;
     `wezsesh list` no longer shows it
  5. spawn second instance via the keybinding → both panes coexist;
     no listener-port collisions

Failure modes captured:
  - any panic in either binary is asserted (logs scanned)
  - any Lua error in the wezterm.log
  - any orphaned `.sock` file in runtime_dir after teardown

Coverage caveat: e2e cannot assert visual fidelity (no screenshot
diffing in v0.1). Manual QA covers picker rendering, marker glyphs,
and color theming.
```

---

## Appendix A — Spawn invocation (binary)

```
wezterm cli spawn --cwd <project-cwd> -- \
  env WEZSESH_HMAC_KEY=<64hex> \
      WEZSESH_PROTO_VERSION=1 \
      WEZSESH_CONFIG_FILE=<absolute-path>     \
      WEZSESH_PLUGIN_VERSION=<plugin-M.VERSION> \
      wezsesh
```

Lua plugin uses `wezterm.mux.spawn_window {...}` (when
`spawn_mode == "window"`) or `current_window:spawn_tab {...}` (when
`spawn_mode == "tab"`). The CLI form above is shown for env-vector
clarity.

The binary's pane ID is read from `WEZTERM_PANE` (auto-injected by
wezterm) and resolved via `wezterm cli list --format json`.

`WEZSESH_CONFIG_FILE` points to the temp JSON file written by
`manager.write_config_file` (§9.2). The binary reads, validates, and
deletes it during startup.

The HMAC key is intentionally an env var, NOT inside the config file:
config-file-on-disk has a wider exposure surface than env (which only
inherits to direct children).

`WEZSESH_SNAPSHOT_DIR`, `WEZSESH_STATE_DIR`, `WEZSESH_RUNTIME_DIR` are
NOT set on the spawn invocation — they live in `WEZSESH_CONFIG_FILE`
(§10.7). The binary's auto-detect path (§12.5) is used only when
invoked outside spawn (e.g., `wezsesh doctor` from a shell).

---

## Appendix B — Snapshot encryption magic-byte sniff

First 32 bytes of `<encoded>.json`:

| Bytes | Encryption |
|---|---|
| ASCII `age-encryption.org/v1\n` | `age` / `rage` |
| First byte high bit set (OpenPGP packet tag) | `gpg` |
| `{`, `[`, or whitespace | plaintext JSON |
| Anything else | `EncryptionUnknown` (treat as encrypted-opaque) |

Operations work without decrypting (§13.6). Doctor warns when any
snapshot is non-JSON.

---

## Appendix C — Resurrect events subscribed

| Event | Handler | Purpose |
|---|---|---|
| `resurrect.file_io.write_state.start` | `state.set_writing(path, true)` | snapshot-write gate |
| `resurrect.file_io.write_state.finished` | `state.set_writing(path, nil)` | unblock TUI open |
| `resurrect.error` | `log_warn` + best-effort correlate to in-flight requests | save-failure observability |

Note: `resurrect.workspace_state.restore_workspace.finished` is NOT
subscribed; it only fires on the success path and cannot be used as a
completion signal.

---

## Appendix D — Threat-model assumptions

wezsesh v0.1 assumes:

1. **Single-user host.** The HMAC key transport via env vars and the
   `wezsesh keygen` stdout are safe under the assumption that no
   other user account on the same machine can read this user's
   process environment or stdout. Multi-user hosts are out of scope
   for v0.1; a future hardening pass would move to anonymous-pipe
   transport for the key.
2. **Trusted wezterm process.** wezterm's OSC parser, mlua runtime,
   and plugin loader are part of the TCB. Compromise of wezterm
   compromises wezsesh.
3. **Cooperating user.** Users can craft their own malicious sidecar
   commands; the trust system gates *first* execution but cannot
   prevent self-harm. Hooks run with the user's full shell privileges
   by design.
4. **Untrusted snapshot contents.** Snapshot bytes (titles, cwd,
   process info) flow through `nameval.SanitizeForDisplay` before
   reaching any TTY render path. argv-restore is gated by the
   allowlist + byte-clean check.
5. **POSIX-only deployment.** Build matrix is Linux + darwin. Windows
   is out of scope; `/dev/urandom` and `/dev/tty` are assumed to
   exist.

Violations of (1) — e.g., attempting to run wezsesh on a shared
research cluster — should be surfaced by doctor with a critical
warning. v0.1 ships without that check; tracking ticket TBD.

---

**Status.** Design v3.0 (technical spec, deep-review applied), derived
from PRD v2.4 (`PRD_V7.md`, 2026-04-30). Section IDs are stable;
tickets reference `(D §x.y)` for design contracts and `(P §x.y)` for
PRD rationale.
