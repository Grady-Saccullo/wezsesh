-- §9.3 — `user-var-changed` handler state machine for the Lua side of
-- the §3.1 two-phase forward-dispatch IPC protocol.
--
-- This module owns ONE wezterm event handler (`user-var-changed`) for
-- the `wezsesh_op` user variable. The handler walks pre-steps (1)–(4)
-- (the spike-#3 pointer-recovery sequence) followed by the existing
-- step-machine (a)–(i) — pane-id match, HMAC key availability, payload
-- field-shape validate, verb-aware tag + canonical re-encode, HMAC
-- verify (constant-time), freshness window, replay (seen_ids) lookup,
-- target-window scope check, dedup write-back + state record, dispatch.
--
-- ┌─ Pre-steps (spike-#3 §3.1 / §C.5) ─────────────────────────────────┐
-- │ (1) wezterm.json_parse → pointer  (pcall). wezterm pre-decodes the │
-- │     base64 OSC value before firing `user-var-changed`, so `value`  │
-- │     is already the raw pointer JSON bytes; a second b64 decode     │
-- │     would always fail.                                             │
-- │ (2) Pointer field-shape (§9.3.1.A): v == 1, id 26 chars, path with │
-- │     <runtime_dir>/req/ prefix.                                     │
-- │ (3) io.open + stat: regular file, mode 0600, owner-self, NOT a     │
-- │     symlink. Failure → silent-drop + log_warn REQ_POINTER_REJECTED.│
-- │ (4) io.read("*a") → bytes; os.remove(path) (best-effort);          │
-- │     wezterm.json_parse → payload (pcall);                          │
-- │     cross-check pointer.id == payload.id.                          │
-- └────────────────────────────────────────────────────────────────────┘
-- ┌─ Steps (a)–(i) — synchronous-only (CI lint: §17.4) ────────────────┐
-- │ (a) Pane-ID match against state.get_state(pane_id_str).            │
-- │ (b) HMAC key availability check; nil → silent-drop.                │
-- │ (c) [folded into pre-step 4 — the parser already ran]              │
-- │ (d) Payload field-shape validator (§9.3.1.B).                      │
-- │ (e) canonical_json.tag_in_place(payload, …) + canonical_json.encode│
-- │     of copy_without(payload, "hmac"). pcall — encoder raises on    │
-- │     UTF-8 / float / untagged table.                                │
-- │ (f) HMAC verify with ct_eq.eq (NEVER raw ==).                      │
-- │ (g) Freshness (|now-ts| > 30 → reject); replay (seen_ids[id] →     │
-- │     reject); target_window_id match.                               │
-- │ (h) seen_ids write-back + state.prune_seen_ids + state.set_state.  │
-- │ (i) pcall(ops.dispatch, payload, window, pane).                    │
-- └────────────────────────────────────────────────────────────────────┘
--
-- §17.4 invariant: steps (a)–(h) MUST be synchronous Lua bytecode
-- (zero `wezterm.run_child_process` / `wezterm.sleep_ms` / any
-- add_async_function-exposed API). The lint walks the AST between
-- markers `(a)` and `(h)` of this file. Step (i)'s
-- `wezterm.background_child_process` (inside ops.dispatch via
-- result.lua) is fire-and-forget and exempt.
--
-- mlua sandbox: every external require is namespaced (`wezsesh.b64`,
-- etc.) so the §11 / §16.5 cache-bust loop in init.lua picks up edits
-- on Ctrl+Shift+R reload. The standalone spec (`ipc_spec.lua`)
-- installs harness doubles via `package.preload[...]` BEFORE requiring
-- this file.

local wezterm        = require("wezterm")
local canonical_json = require("wezsesh.canonical_json")
local ct_eq          = require("wezsesh.ct_eq")
local hmac           = require("wezsesh.hmac")
local state          = require("wezsesh.state")

local M = {}

-- Module constants. Public so the spec can assert on them.
M.FRESHNESS_WINDOW_SECONDS = 30
M.SEEN_IDS_TTL_SECONDS     = 60
M.USER_VAR_NAME            = "wezsesh_op"

-- Per-platform AF_UNIX SUN_PATH ceiling. darwin = 104, Linux = 108.
-- Reply-socket basename is "<8hex>.sock" (14 chars including the slash);
-- §13.9 budgets `len(runtime_dir) + 14 ≤ ceiling`.
local SUN_PATH_TAIL_BYTES  = 14   -- "/<8hex>.sock"

-- Pluggable seam for the §C.5 pre-step (3) stat() check. Production
-- wezterm has no `lstat` API — wezterm.glob/read_dir do not return
-- mode bits. The default implementation attempts `io.open` only; the
-- spec installs a stricter stat double that returns mode/symlink/
-- owner-self info from the harness's in-memory filesystem.
--
-- Returns either nil (stat unavailable; caller falls back to io.open
-- only) or a table { mode = octal_int, is_symlink = bool, is_regular = bool, owner_self = bool }.
function M._default_stat_path(_path)
    return nil
end

-- Internal deps table. Replaced wholesale by the spec's harness via
-- `M._set_deps{...}`; production callers MAY override individual fields
-- through that surface (e.g., to inject an lfs binding the host has
-- vendored), but the default-equipped table is what `M.register` uses
-- when no override is set.
M._deps = {
    log_warn  = function(msg) wezterm.log_warn(msg) end,
    log_error = function(msg) wezterm.log_error(msg) end,
    now       = os.time,
    stat_path = M._default_stat_path,
    -- ops.dispatch is required lazily inside the handler so this module
    -- can be loaded in a session where ops.lua is not yet available
    -- (e.g., the spec; T-601 builds ops). The lazy require is wrapped
    -- in pcall — a missing module degrades to "no dispatch" rather than
    -- wedging the event loop.
    dispatch  = nil,
}

-- Test seam: replace deps wholesale. Used by `ipc_spec.lua` to inject
-- a stat double, a clock, and a recording log_warn. Production code
-- never calls this.
function M._set_deps(d)
    if type(d) ~= "table" then return end
    for k, v in pairs(d) do
        M._deps[k] = v
    end
end

-- Reset deps to default. Used by the spec between tests.
function M._reset_deps()
    M._deps = {
        log_warn  = function(msg) wezterm.log_warn(msg) end,
        log_error = function(msg) wezterm.log_error(msg) end,
        now       = os.time,
        stat_path = M._default_stat_path,
        dispatch  = nil,
    }
end

local function log_warn(msg)
    local fn = M._deps.log_warn
    if type(fn) == "function" then
        pcall(fn, msg)
    end
end

-- ────────────────────────────────────────────────────────────────────
-- §9.3.1.A — pointer field-shape validator (pre-step 2)
-- ────────────────────────────────────────────────────────────────────
--
-- req_dir_prefix MUST end with `/` so the prefix check is unambiguous
-- against partial-prefix attacks (e.g. `<runtime_dir>/req2/` matching
-- `<runtime_dir>/req/`).

function M.validate_pointer(pointer, req_dir_prefix)
    if type(pointer) ~= "table" then return false end
    if type(pointer.v) ~= "number" or pointer.v ~= 1 then return false end
    if type(pointer.id) ~= "string" or #pointer.id ~= 26 then
        return false
    end
    if type(pointer.path) ~= "string" then return false end
    if type(req_dir_prefix) ~= "string" or #req_dir_prefix == 0 then
        return false
    end
    if pointer.path:sub(1, #req_dir_prefix) ~= req_dir_prefix then
        return false
    end
    return true
end

-- ────────────────────────────────────────────────────────────────────
-- §9.3.1.B — payload field-shape validator (step (d))
-- ────────────────────────────────────────────────────────────────────
--
-- Strict types-and-bounds. The verb-aware encoder runs AFTER this
-- validator (step (e)), so downstream code is nil-safe.

function M.validate_payload(payload)
    if type(payload) ~= "table" then return false end
    if type(payload.v) ~= "number" or payload.v ~= 1 then return false end
    if type(payload.id) ~= "string" or #payload.id ~= 26 then
        return false
    end
    if type(payload.ts) ~= "number" then return false end
    if type(payload.op) ~= "string"
       or #payload.op == 0 or #payload.op > 32
    then
        return false
    end
    if type(payload.args) ~= "table" then return false end
    if type(payload.reply_sock) ~= "string"
       or #payload.reply_sock == 0 or #payload.reply_sock > 104
    then
        return false
    end
    if type(payload.target_window_id) ~= "number" then return false end
    if type(payload.hmac) ~= "string" or #payload.hmac ~= 64 then
        return false
    end
    return true
end

-- ────────────────────────────────────────────────────────────────────
-- §13.9 — SUN_PATH validation (Lua side)
-- ────────────────────────────────────────────────────────────────────
--
-- Raises a sentinel error so `init.lua`'s `apply_to_config` pcall-
-- boundary can substring-match `WEZSESH_SUN_PATH_OVERFLOW` /
-- `WEZSESH_RUNTIME_DIR_TYPE` and surface a 10s toast.
--
-- The HOME expansion mirrors §13.9 verbatim: `~/` prefix is replaced
-- with `wezterm.home_dir` (preferred) or `$HOME` (fallback). On
-- expansion failure the original string is used; the SUN_PATH math
-- then has the same effect.

local function expand_home(path)
    if type(path) ~= "string" then return path end
    if path:sub(1, 2) ~= "~/" then return path end
    local home = (wezterm.home_dir or os.getenv("HOME") or "")
    return home .. path:sub(2)
end

local function platform_sun_path_ceiling()
    -- §13.9: darwin = 104; Linux/everything else = 108. The match
    -- pattern mirrors the spec's `wezterm.target_triple` discriminator
    -- so a future BSD addition still picks the safe (108) ceiling.
    local triple = wezterm.target_triple
    if type(triple) == "string" and triple:match("%-apple%-darwin") then
        return 104
    end
    return 108
end

function M.validate_runtime_dir(runtime_dir)
    if type(runtime_dir) ~= "string" then
        error(
            "WEZSESH_RUNTIME_DIR_TYPE: opts.runtime_dir must be a string path",
            0
        )
    end
    local expanded = expand_home(runtime_dir)
    local ceiling = platform_sun_path_ceiling()
    local needed = #expanded + SUN_PATH_TAIL_BYTES
    if needed > ceiling then
        error(string.format(
            "WEZSESH_SUN_PATH_OVERFLOW: runtime_dir too long for AF_UNIX SUN_PATH "
            .. "(needed=%d, ceiling=%d, path=%q). Shorten or use the default.",
            needed, ceiling, expanded), 0)
    end
    return expanded
end

-- ────────────────────────────────────────────────────────────────────
-- Pre-step (3) — file mode + symlink + owner-self guard
-- ────────────────────────────────────────────────────────────────────
--
-- Returns true iff the file at `path` passes the §3.1 guards:
--   * regular file (not a directory, FIFO, sock),
--   * mode bits == 0600 exactly,
--   * NOT a symlink,
--   * owned by the current uid (best-effort).
--
-- If the deps.stat_path returns nil (no stat available — production
-- on a wezterm without an `lfs` binding), the path-prefix check from
-- pre-step (2) plus the parent dir's `safefs.Enforce(SymlinkRefuse)`
-- guarantee carry the safety burden — see §3.3 prose. The function
-- returns true so the handler proceeds to io.open + io.read (which
-- will fail loudly if the file vanished or is unreadable).
local function stat_guard_ok(path)
    local fn = M._deps.stat_path
    if type(fn) ~= "function" then return true end
    local ok, info = pcall(fn, path)
    if not ok then return false end
    if info == nil then return true end           -- stat unavailable
    if type(info) ~= "table" then return false end
    if info.is_symlink then return false end
    if info.is_regular == false then return false end
    if info.owner_self == false then return false end
    if info.mode ~= nil and info.mode ~= 0x180 then
        -- 0x180 == octal 0600. Some lfs bindings strip the type bits;
        -- compare the perm-bit subset.
        if (info.mode & 0x1ff) ~= 0x180 then return false end
    end
    return true
end

-- ────────────────────────────────────────────────────────────────────
-- Pre-step (4) — read file + parse + cross-check
-- ────────────────────────────────────────────────────────────────────

local function read_request_file(path)
    local fh, err = io.open(path, "rb")
    if fh == nil then return nil, err end
    local bytes = fh:read("*a")
    fh:close()
    -- Best-effort unlink; orphans are reaped by §12.4 startup sweep.
    os.remove(path)
    return bytes
end

-- ────────────────────────────────────────────────────────────────────
-- Reply emission (used by step (g) / (h) drop paths and (i)).
-- ────────────────────────────────────────────────────────────────────
--
-- The handler MUST be silent on the wire for HMAC mismatch / freshness
-- failure / replay failure / target-window mismatch / pointer rejects:
-- all of those land in `log_warn` only and the binary observes
-- IPC_TIMEOUT. ops.dispatch (step (i)) owns the success-path replies.

-- ────────────────────────────────────────────────────────────────────
-- The handler. Pure function; the wezterm-event registration in
-- M.register is a thin closure over this.
-- ────────────────────────────────────────────────────────────────────

function M.handle_user_var(window, pane, name, value, opts)
    -- `name` is the user-var key; we filter to ours up front so a
    -- foreign OSC (some other plugin emitting `SetUserVar=other_var=…`)
    -- exits in O(1) without any further parsing.
    if name ~= M.USER_VAR_NAME then return end

    -- opts is the per-register frozen config: { req_dir_prefix,
    -- target_window_id }. Both come from `apply_to_config`'s opts
    -- (validated up there).
    if type(opts) ~= "table" then return end
    local req_dir_prefix   = opts.req_dir_prefix
    local target_window_id = opts.target_window_id

    -- ── Pre-step (1): JSON-parse → pointer ───────────────────────────
    -- wezterm's SetUserVar parser base64-decodes the OSC value before
    -- firing `user-var-changed`, so `value` here is already the raw
    -- pointer JSON bytes. A separate b64.decode would (and historically
    -- did) double-decode and reject every live pointer.
    if type(value) ~= "string" or #value == 0 then
        log_warn("REQ_POINTER_REJECTED: empty or non-string value")
        return
    end

    local ok_p, pointer = pcall(wezterm.json_parse, value)
    if not ok_p or type(pointer) ~= "table" then
        log_warn("REQ_POINTER_REJECTED: pointer JSON parse failed")
        return
    end

    -- ── Pre-step (2): pointer field-shape validate ───────────────────
    if not M.validate_pointer(pointer, req_dir_prefix) then
        log_warn("REQ_POINTER_REJECTED: pointer shape invalid")
        return
    end

    -- ── Pre-step (3): stat (regular, 0600, non-symlink, owner-self) ──
    if not stat_guard_ok(pointer.path) then
        log_warn("REQ_POINTER_REJECTED: file stat guard failed")
        return
    end

    -- ── Pre-step (4): read + parse + cross-check id ──────────────────
    local file_bytes, read_err = read_request_file(pointer.path)
    if file_bytes == nil then
        log_warn("REQ_POINTER_REJECTED: read failed: " .. tostring(read_err))
        return
    end
    if #file_bytes == 0 then
        log_warn("REQ_POINTER_REJECTED: empty request file")
        return
    end

    local ok_pl, payload = pcall(wezterm.json_parse, file_bytes)
    if not ok_pl or type(payload) ~= "table" then
        log_warn("REQ_POINTER_REJECTED: payload JSON parse failed")
        return
    end

    if payload.id ~= pointer.id then
        log_warn("REQ_POINTER_REJECTED: pointer.id != payload.id")
        return
    end

    -- ── Step (a): pane-id match ──────────────────────────────────────
    -- The pane that emitted the OSC — by §12.1 / §10.6 — is the binary
    -- spawn this handler is responsible for. `state.set_state(pane_id, …)`
    -- recorded the {target_window_id, spawned_at} association at spawn
    -- time. Foreign panes (other plugins emitting wezsesh_op?) fall out
    -- here; the cost is one stringify + one GLOBAL read.
    local pane_id = pane and pane:pane_id()
    if pane_id == nil then
        log_warn("ipc: handler received nil pane")
        return
    end
    local session = state.get_state(pane_id)
    if session == nil then
        -- No matching spawn record. Either a stale OSC after
        -- window-config-reloaded blew the bucket, or a foreign emitter.
        return
    end

    -- ── Step (b): HMAC key availability ──────────────────────────────
    local hex_key = wezterm.GLOBAL.wezsesh_session_key
    if type(hex_key) ~= "string" or #hex_key ~= 64 then
        -- Generation chain (§5.2) failed at apply_to_config; toast was
        -- already shown there. Drop silently.
        return
    end

    -- ── Step (c) is folded into pre-step (4) — payload already parsed.
    -- ── Step (d): payload field-shape validate ───────────────────────
    if not M.validate_payload(payload) then
        log_warn("ipc: payload shape invalid")
        return
    end

    -- ── Step (e): verb-aware tag + canonical re-encode (sans hmac) ───
    -- §4.2 + §4.3: the verifier MUST drop the `hmac` field, tag the
    -- remaining tree by the verb-keyed shape, and canonical-encode the
    -- result. Encoder raises on non-canonical input (untagged tables,
    -- floats, invalid UTF-8). pcall keeps the raise from wedging the
    -- event loop; we silently drop and log_warn for observability.
    local verb_args_shape = canonical_json.verb_args_shape[payload.op]
    if verb_args_shape == nil then
        -- Unknown verb during encode — the canonical_json shape walker
        -- requires a verb-keyed args spec. ops.dispatch will reply
        -- UNKNOWN_VERB downstream, but the verifier path can't proceed
        -- without a shape entry. Defer to ops.dispatch by skipping the
        -- HMAC check would be a security bug; instead we silently drop
        -- and log_warn (matches the §4.2 lint that blocks adding
        -- dispatch entries without a shape).
        log_warn("ipc: no shape registered for op=" .. tostring(payload.op))
        return
    end

    -- §4.2 / §4.3: tag the FULL parsed payload (with `hmac` still in
    -- place) using the verb-keyed shape, then drop `hmac` from a copy
    -- and encode. canonical_json.ROOT_PAYLOAD_SHAPE declares `hmac` as
    -- a root key (it IS part of the on-the-wire envelope per §3.3); the
    -- verifier therefore must tag-with-hmac, copy_without-hmac, encode.
    -- Reversing the order (copy_without then tag) trips
    -- `CANONICAL_SHAPE_MISMATCH: missing required key: hmac`.
    local ok_tag = pcall(canonical_json.tag_in_place, payload,
                         canonical_json.ROOT_PAYLOAD_SHAPE,
                         verb_args_shape)
    if not ok_tag then
        log_warn("ipc: canonical tag failed")
        return
    end

    local sans_hmac = canonical_json.copy_without(payload, "hmac")
    local ok_enc, payload_bytes = pcall(canonical_json.encode, sans_hmac)
    if not ok_enc or type(payload_bytes) ~= "string" then
        log_warn("ipc: canonical encode failed")
        return
    end

    -- ── Step (f): HMAC verify (constant-time) ────────────────────────
    -- Contract: hmac.compute returns a lowercase hex string; ct_eq.eq
    -- does a length-equal byte-string compare. NEVER raw `==` here.
    local ok_h, computed = pcall(hmac.compute, payload_bytes, hex_key)
    if not ok_h or type(computed) ~= "string" then
        -- Unexpected error in HMAC primitive — log and drop. Wire-silent.
        return
    end
    if not ct_eq.eq(payload.hmac, computed) then
        -- §17.3: HMAC mismatch is silent on the wire; binary observes
        -- IPC_TIMEOUT. Logging is INTERNAL only.
        log_warn("ipc: HMAC mismatch")
        return
    end

    -- ── Step (g): freshness + replay + target_window_id ──────────────
    -- AFTER HMAC verify so attackers can't spam STALE_PAYLOAD logs.
    local now = M._deps.now()
    local skew = math.abs(now - payload.ts)
    if skew > M.FRESHNESS_WINDOW_SECONDS then
        log_warn(string.format(
            "ipc: STALE_PAYLOAD (skew=%ds, ts=%d, now=%d)",
            skew, payload.ts, now))
        return
    end

    if state.seen(payload.id) then
        -- Replay. Authentic but already processed.
        return
    end

    -- §9.3.1.C: wire `-1` is the "any window" sentinel — skip the
    -- window-match check (the per-pane state.set_state record is the
    -- gating signal in that branch). Any other value is a real
    -- wezterm window id and is matched STRICTLY against the live
    -- pane's recorded session.target_window_id; `0` is wezterm's
    -- first-window id and matches `session.target_window_id == 0`
    -- (no sentinel collision because the sentinel is `-1`).
    if payload.target_window_id ~= -1
       and payload.target_window_id ~= session.target_window_id
    then
        -- Multi-window broadcast (#3524): wezterm fires user-var-changed
        -- in EVERY listening window. Only the window matching the
        -- request's target_window_id should dispatch.
        return
    end

    -- ── Step (h): seen_ids write-back + prune + state record ─────────
    state.mark_seen(payload.id)
    state.prune_seen_ids(M.SEEN_IDS_TTL_SECONDS)
    -- Preserve the spawn record's spawned_at while re-asserting the
    -- session entry: GLOBAL reads return deserialised snapshots (CLAUDE.md
    -- invariant 5), so explicit set_state write-back is required after
    -- any sub-table mutation. spawned_at is intentionally NOT re-stamped
    -- — its value gates the §12.4 startup-sweep age window.
    state.set_state(pane_id, {
        target_window_id = session.target_window_id,
        spawned_at       = session.spawned_at,
    })

    -- ── Step (i): dispatch (pcall — fire-and-forget reply spawn lives
    --             inside ops.dispatch via result.lua) ─────────────────
    local dispatch_fn = M._deps.dispatch
    if dispatch_fn == nil then
        local ok_req, ops = pcall(require, "wezsesh.ops")
        if ok_req and type(ops) == "table"
           and type(ops.dispatch) == "function"
        then
            dispatch_fn = ops.dispatch
        end
    end
    if type(dispatch_fn) ~= "function" then
        log_warn("ipc: ops.dispatch unavailable")
        return
    end

    local ok_d, err_d = pcall(dispatch_fn, payload, window, pane)
    if not ok_d then
        -- Verbs raising out of pcall is a programmer error; we still
        -- swallow it — CLAUDE.md invariant 1 (uncaught raise wedges the
        -- event loop). Logging gives the next operator a thread to pull.
        log_warn("ipc: dispatch raised: " .. tostring(err_d))
    end
end

-- ────────────────────────────────────────────────────────────────────
-- Registration. Called from `init.lua`'s `apply_to_config` after
-- `manager.validate_runtime_dir` confirmed the SUN_PATH budget.
-- ────────────────────────────────────────────────────────────────────
--
-- opts MUST carry:
--   * runtime_dir       — already-validated absolute path; this fn
--                         derives `<runtime_dir>/req/` as the
--                         per-pointer file-prefix gate.
--   * target_window_id  — int; the wezterm window id this binary is
--                         scoped to.
--
-- The `wezterm.on(...)` body is `pcall`-wrapped at the boundary —
-- any uncaught raise inside the handler would wedge the wezterm event
-- loop (CLAUDE.md invariant 1).
function M.register(opts)
    if type(opts) ~= "table" then
        error("ipc.register: opts table required", 0)
    end
    -- Idempotency-sensitive: re-registering on Ctrl+Shift+R reload would
    -- compound handlers. wezterm wipes registered handlers when the Lua
    -- state is rebuilt, so a single register-per-load matches the cache.
    local expanded = M.validate_runtime_dir(opts.runtime_dir)
    local req_dir_prefix = expanded
    if req_dir_prefix:sub(-1) ~= "/" then
        req_dir_prefix = req_dir_prefix .. "/"
    end
    req_dir_prefix = req_dir_prefix .. "req/"

    if type(opts.target_window_id) ~= "number" then
        error("ipc.register: opts.target_window_id must be a number", 0)
    end
    local frozen = {
        req_dir_prefix   = req_dir_prefix,
        target_window_id = opts.target_window_id,
    }

    wezterm.on("user-var-changed", function(window, pane, name, value)
        local ok, err = pcall(M.handle_user_var,
                              window, pane, name, value, frozen)
        if not ok then
            -- Last-resort safety net: even with the per-step pcalls
            -- inside handle_user_var, a programmer error in this file
            -- (e.g. a typo causing a nil-deref) MUST NOT wedge the
            -- wezterm event loop. CLAUDE.md invariant 1.
            log_warn("ipc: handler raised: " .. tostring(err))
        end
    end)
end

return M
