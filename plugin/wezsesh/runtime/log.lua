-- runtime/log.lua — structured logger for the Lua side of wezsesh.
--
-- The whole plugin's log surface goes through this module. Every call
-- builds a flat structured record:
--
--   { level, ts, msg, plugin_session_id, [trace_id, binary_session_id,
--     pane_id, ...caller fields] }
--
-- The record is JSON-encoded once and emitted to TWO sinks:
--
--   1. `wezterm.log_warn` / `log_error` / `log_info` with the literal
--      `[wezsesh] ` prefix so `wezsesh tail` can grep / parse the
--      record off wezterm's GUI log file.
--   2. `<runtime_dir>/plugin.log`, append-only, reopened per write.
--      The reopen-per-write pattern is load-bearing — init.lua's
--      `package.loaded["wezsesh.*"]` cache-bust loop on Ctrl+Shift+R
--      drops every cached module, including this one; a long-lived
--      file handle would leak descriptors across reload cycles. See
--      CLAUDE.md "Plugin layers" for the cache-bust rule.
--
-- ── Durability asymmetry (load-bearing) ────────────────────────────
-- Lua's `io` API has NO fsync binding (`fh:flush()` only flushes the
-- userspace buffer; it does NOT call fsync(2)). A wezterm crash within
-- the OS write-back window can lose the tail of `plugin.log`. The
-- Go-side `internal/logger/logger.go` issues a sync-flush on Warn /
-- Error specifically to survive crashes within the 1 s window; the Lua
-- side does not have that property. A future reader MUST NOT treat the
-- two log streams as durability-equivalent.
--
-- ── Truncation cap ────────────────────────────────────────────────
-- Each emitted JSON line is capped at 512 bytes. POSIX guarantees
-- atomic-append for writes ≤ PIPE_BUF; Linux PIPE_BUF is 4 KiB but
-- Darwin's is 512, so the smaller-of-the-two is the safe cap. On
-- overflow the `msg` field is truncated with a `…` marker (UTF-8
-- ellipsis, 3 bytes) and the record re-encoded once. We do NOT loop —
-- a single overflow → truncate → re-encode pass is sufficient because
-- the structural fields (`level`, `ts`, ids) are bounded; only `msg`
-- is unbounded.
--
-- ── pcall discipline ──────────────────────────────────────────────
-- `wezterm.log_*` is `pcall`-wrapped because a stripped-down host (no
-- logger registered, or one that raises) MUST NOT bubble out of an
-- event-loop callback (wezterm wedge per CLAUDE.md invariant 1). The
-- file-append leg is also `pcall`-wrapped: a filesystem error
-- (`runtime_dir` gone, ENOSPC, EACCES) MUST NOT crash the event loop;
-- the wezterm-log leg still gets the line.
--
-- ── Level threshold ───────────────────────────────────────────────
-- `init.lua` resolves the threshold locally at apply_to_config via
-- `M.resolve_level(opts.log_level, $WEZSESH_LOG)` — symmetric with
-- the Go side's `logger.ResolveLevel`. The result is stamped via
-- `globals.set_log_level`. This module reads `globals.log_level()`
-- lazily on every emit and drops records whose rank is below the
-- threshold. Symmetry with the Go side: a config of "warn" suppresses
-- Debug/Info on BOTH `<state_dir>/wezsesh.log` (slog filtering) and
-- `<runtime_dir>/plugin.log` + the wezterm-log leg (this filter).
-- Both sides see the same parent env at wezterm-launch time, so the
-- resolved level matches without any disk-sidecar coordination.
-- Unknown / nil threshold → "info" (matches the Go default).
--
-- ── Test seam ─────────────────────────────────────────────────────
-- `_set(warn_fn, error_fn, info_fn)` replaces the wezterm-log sinks.
-- The override receives the STRUCTURED RECORD (a Lua table) — not the
-- JSON string — so spec assertions can index `rec.level` /
-- `rec.plugin_session_id` directly without round-tripping through a
-- parser. `_reset()` restores the wezterm-backed defaults. The file-
-- append leg is unaffected by `_set`; specs that need to assert on
-- `plugin.log` contents stash a tmp `runtime_dir` via the globals
-- accessor and read the file after the call.
--
-- mlua sandbox: acquired via `local wezterm = require("wezterm")` per
-- the mlua-sandbox rule. `_G.wezterm` is forbidden; the lualint
-- `lua-g-wezterm` rule guards that.

local wezterm = require("wezterm")
local globals = require("wezsesh.runtime.globals")
local state   = require("wezsesh.runtime.state")

local M = {}

-- POSIX atomic-append cap: smaller of Linux 4 KiB / Darwin 512.
local MAX_LINE_BYTES = 512
-- 3-byte UTF-8 ellipsis used as the truncation marker.
local TRUNCATE_MARK = "\xE2\x80\xA6"

-- Numeric ranks for the level filter. Matches Go-side iota ordering
-- (Debug<Info<Warn<Error) so the two sides agree on what "≥ warn"
-- means. Unknown levels rank as info — emit-on-uncertainty so a typo
-- in a future caller doesn't silently drop the record.
local LEVEL_RANK = {
    debug = 0,
    info  = 1,
    warn  = 2,
    error = 3,
}

-- Resolve the threshold rank from the GLOBAL stamp, with fallback to
-- "info" on nil / unknown (matches the Go-side default in ResolveLevel).
local function threshold_rank()
    local s = globals.log_level()
    if type(s) ~= "string" then return LEVEL_RANK.info end
    local r = LEVEL_RANK[s]
    if r == nil then return LEVEL_RANK.info end
    return r
end

-- resolve_level picks the more verbose (lower-rank) of two candidate
-- levels — `opts_level` from `apply_to_config` and `env_level` from
-- `$WEZSESH_LOG`. Mirrors Go's `internal/logger.ResolveLevel`:
--
--   * Both nil/empty/invalid → "info" (the canonical fallback).
--   * One valid → that one.
--   * Both valid → whichever has the lower numeric rank (more
--     verbose). Env can only make logging noisier, never quieter,
--     which matches the Go-side semantics — `WEZSESH_LOG=debug`
--     surfaces debug records even when the user's opts say "warn".
--
-- Returns one of "debug" / "info" / "warn" / "error".
function M.resolve_level(opts_level, env_level)
    local function parse(s)
        if type(s) ~= "string" or s == "" then return nil end
        local lc = s:lower()
        if LEVEL_RANK[lc] ~= nil then return lc end
        return nil
    end
    local a = parse(opts_level)
    local b = parse(env_level)
    if a == nil and b == nil then return "info" end
    if a == nil then return b end
    if b == nil then return a end
    if LEVEL_RANK[a] <= LEVEL_RANK[b] then return a end
    return b
end

-- emit_at returns true when a record at `level` should reach the sinks.
local function emit_at(level)
    local r = LEVEL_RANK[level]
    if r == nil then r = LEVEL_RANK.info end
    return r >= threshold_rank()
end

-- ────────────────────────────────────────────────────────────────────
-- record builder
-- ────────────────────────────────────────────────────────────────────

-- Build the structured record. Order of precedence for trace context:
--   1. Caller-supplied `fields.trace_id` / `fields.binary_session_id`
--      win — explicit threading from async callsites that captured the
--      ids before crossing the event-loop boundary.
--   2. If absent and `fields.pane_id` is present, look up
--      `state.get_active_trace(pane_id)` and merge.
--   3. Otherwise the record carries `plugin_session_id` only — that's
--      the documented "no trace context handy" fallback.
--
-- `plugin_session_id` is always stamped (even on the empty string when
-- the mint at apply_to_config hasn't run yet) so the record schema is
-- stable across the boot window.
local function build_record(level, msg, fields)
    local rec = {
        level             = level,
        ts                = os.time(),
        msg               = tostring(msg or ""),
        plugin_session_id = globals.plugin_session_id() or "",
    }
    if type(fields) == "table" then
        for k, v in pairs(fields) do
            -- Don't let a caller silently overwrite the structural
            -- fields above; the ts / level / plugin_session_id wins.
            -- `msg` is overwritable only by passing `fields.msg`
            -- explicitly, which is unusual but not forbidden.
            if k ~= "level" and k ~= "ts" and k ~= "plugin_session_id" then
                rec[k] = v
            end
        end
        -- pane_id-driven trace lookup. Only fills in slots the caller
        -- didn't override, so an async callsite passing a captured
        -- `trace_id` keeps it.
        if fields.pane_id ~= nil then
            local ctx = state.get_active_trace(fields.pane_id)
            if type(ctx) == "table" then
                if rec.trace_id == nil and ctx.trace_id ~= nil then
                    rec.trace_id = ctx.trace_id
                end
                if rec.binary_session_id == nil
                   and ctx.binary_session_id ~= nil
                then
                    rec.binary_session_id = ctx.binary_session_id
                end
            end
        end
    end
    return rec
end

-- ────────────────────────────────────────────────────────────────────
-- JSON-encode + cap to MAX_LINE_BYTES
-- ────────────────────────────────────────────────────────────────────

local function encode_record(rec)
    local ok, json = pcall(wezterm.json_encode, rec)
    if not ok or type(json) ~= "string" then return nil end
    return json
end

-- Single-pass truncation. If the encoded record exceeds the cap, slice
-- the message to a budget that leaves headroom for the marker, the
-- closing brace, and the surrounding JSON scaffolding, then re-encode.
-- The structural fields are bounded (level ≤ 5 chars, ts ≤ 10 digits,
-- ids 26 chars), so the only field that can drive overflow is `msg`.
local function encode_capped(rec)
    local json = encode_record(rec)
    if json == nil then return nil end
    if #json <= MAX_LINE_BYTES then return json end

    -- Excess bytes = #json - MAX_LINE_BYTES. The msg field carries
    -- those excess bytes plus the ellipsis we want to add. Slice it to
    -- a length that leaves the budget intact: original_msg_bytes -
    -- excess - #TRUNCATE_MARK. If that drops to zero or below, fall
    -- back to the marker alone.
    local excess = #json - MAX_LINE_BYTES + #TRUNCATE_MARK
    local original = rec.msg or ""
    local new_len = #original - excess
    if new_len < 0 then new_len = 0 end
    -- Avoid splitting a UTF-8 multi-byte sequence: walk back until we
    -- land on a continuation-byte boundary (0xxxxxxx or 11xxxxxx start).
    while new_len > 0 do
        local b = string.byte(original, new_len + 1)
        if b == nil or b < 0x80 or b >= 0xC0 then break end
        new_len = new_len - 1
    end
    rec.msg = original:sub(1, new_len) .. TRUNCATE_MARK
    json = encode_record(rec)
    -- One-shot. If even the truncated form somehow still exceeds the
    -- cap (a structural field grew unexpectedly), return what we have;
    -- the wezterm sink doesn't enforce the cap and the file-append
    -- atomic-append guarantee silently degrades. Better than dropping
    -- the line.
    return json
end

-- ────────────────────────────────────────────────────────────────────
-- wezterm log sinks (test-seamed)
-- ────────────────────────────────────────────────────────────────────

local function default_warn_sink(rec)
    local json = encode_capped(rec)
    if json == nil then return end
    if type(wezterm.log_warn) == "function" then
        pcall(wezterm.log_warn, "[wezsesh] " .. json)
    end
end

local function default_error_sink(rec)
    local json = encode_capped(rec)
    if json == nil then return end
    if type(wezterm.log_error) == "function" then
        pcall(wezterm.log_error, "[wezsesh] " .. json)
    end
end

-- info sink: prefer wezterm.log_info; fall back to wezterm.log_warn so
-- info records still surface. Some wezterm builds don't expose
-- log_info; the fallback keeps the structured record alive.
local function default_info_sink(rec)
    local json = encode_capped(rec)
    if json == nil then return end
    local fn = wezterm.log_info
    if type(fn) ~= "function" then fn = wezterm.log_warn end
    if type(fn) == "function" then
        pcall(fn, "[wezsesh] " .. json)
    end
end

local sink_warn  = default_warn_sink
local sink_error = default_error_sink
local sink_info  = default_info_sink

-- ────────────────────────────────────────────────────────────────────
-- file-append leg (NOT test-seamed — specs use a tmp runtime_dir)
-- ────────────────────────────────────────────────────────────────────

local function append_to_plugin_log(rec)
    local dir = globals.runtime_dir()
    if type(dir) ~= "string" or #dir == 0 then return end
    local path = dir
    if path:sub(-1) ~= "/" then path = path .. "/" end
    path = path .. "plugin.log"

    local json = encode_capped(rec)
    if json == nil then return end

    -- Reopen-per-write. A long-lived handle would leak fds across the
    -- cache-bust loop in init.lua. Wrap in pcall so a filesystem error
    -- (runtime_dir vanished, ENOSPC, EACCES, etc.) never propagates
    -- out of an event-loop callback.
    pcall(function()
        local fh = io.open(path, "a")
        if fh == nil then return end
        fh:write(json)
        fh:write("\n")
        fh:close()
    end)
end

-- ────────────────────────────────────────────────────────────────────
-- public API
-- ────────────────────────────────────────────────────────────────────

function M.warn(msg, fields)
    if not emit_at("warn") then return end
    local rec = build_record("warn", msg, fields)
    sink_warn(rec)
    append_to_plugin_log(rec)
end

function M.error(msg, fields)
    if not emit_at("error") then return end
    local rec = build_record("error", msg, fields)
    sink_error(rec)
    append_to_plugin_log(rec)
end

function M.info(msg, fields)
    if not emit_at("info") then return end
    local rec = build_record("info", msg, fields)
    sink_info(rec)
    append_to_plugin_log(rec)
end

function M.debug(msg, fields)
    if not emit_at("debug") then return end
    local rec = build_record("debug", msg, fields)
    -- No wezterm.log_debug surface; debug records reach only the
    -- file-append leg so they don't pollute the wezterm GUI log when
    -- a user opts into the verbose threshold.
    append_to_plugin_log(rec)
end

-- Test seam. Replace the sinks. Pass nil for any arg to leave that
-- sink unchanged. Returns the previous (warn, error, info) triple so
-- a test can restore on teardown. The override receives the STRUCTURED
-- RECORD (a Lua table) — not the JSON string — so spec assertions can
-- index fields directly without round-tripping through a parser.
function M._set(warn_fn, error_fn, info_fn)
    local prev_warn, prev_error, prev_info = sink_warn, sink_error, sink_info
    if warn_fn  ~= nil then sink_warn  = warn_fn  end
    if error_fn ~= nil then sink_error = error_fn end
    if info_fn  ~= nil then sink_info  = info_fn  end
    return prev_warn, prev_error, prev_info
end

-- Reset to the wezterm-backed defaults. Used by spec teardown and by
-- any code path that wants to drop a previously-installed capture
-- without caring about the exact previous values.
function M._reset()
    sink_warn  = default_warn_sink
    sink_error = default_error_sink
    sink_info  = default_info_sink
end

-- Test-only: build the structured record without emitting. Used by
-- log_spec to assert on the record shape without running through the
-- (capped + JSON-encoded) sink path.
function M._build_record(level, msg, fields)
    return build_record(level, msg, fields)
end

-- Test-only: encode a record with the truncation cap applied.
function M._encode_capped(rec)
    return encode_capped(rec)
end

-- Test-only: expose the level-filter decision so specs can pin the
-- threshold semantics (debug/info/warn/error rank ordering, default
-- to "info" on unknown / nil) without round-tripping through the
-- whole emit pipeline.
function M._emit_at(level)
    return emit_at(level)
end

return M
