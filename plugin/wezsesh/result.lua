-- Reply emitter for the Lua side of the IPC protocol.
--
-- This module owns the second half of the request/reply round-trip:
-- after the verb dispatcher decides what happened, it calls one of
-- the four `reply_*` functions here to spit a JSON envelope back to
-- the binary's reply socket. Every reply (started / completed /
-- partial / error) lands on the wire via the same path:
--
--   1. Build a flat Lua table matching the reply envelope:
--      `v`, `id`, `status`, `ok`, plus the verb-shape's `data`,
--      `warnings`, or `error` fields per the kind invariants.
--   2. JSON-encode it via the local `json_encode` defined below. The
--      reply channel is NOT the canonical-JSON wire (that's
--      request-only) — Go's reply parser feeds the bytes through
--      encoding/json which has no byte-equality contract. We avoid
--      `wezterm.json_encode` here because production mlua serialises
--      an empty Lua table as `[]`, which the Go reply decoder cannot
--      unmarshal into `Data map[string]any` for the noop / nil-details
--      paths. The local encoder defaults empty to `{}` and honours
--      explicit `as_object` / `as_array` tags.
--   3. Base64-encode the JSON via `b64.encode` — the `wezsesh reply`
--      subcommand takes its payload as a single argv to dodge
--      shell-quoting concerns.
--   4. Spawn `wezsesh reply <sock> <b64>` via
--      `wezterm.background_child_process`. The
--      `lua-bg-child-process-pcall` lint requires this call to be
--      `pcall`-wrapped: a failed spawn (binary missing, sock
--      vanished, fork ENOMEM) MUST NOT propagate up to the
--      `user-var-changed` handler, where an uncaught raise wedges the
--      wezterm event loop.
--
-- The reply is fire-and-forget. We never block on the binary's
-- consumption of it; the TUI side has its own 5 s / 30 s reply-budget
-- timers and the binary runs `io.LimitedReader` over the socket. If
-- the binary is gone, the spawn is a noop.
--
-- Every reply carries `v: 1`. The `v` we emit is sourced from
-- `payload.v` so a future protocol bump only needs to round-trip the
-- request's `v` correctly; today payload.v is always 1.
--
-- Reply-kind invariants encoded structurally:
--   * `started`  — ok=true; NO data, warnings, or error.
--   * `completed`+ok=true — data present (may be `{}`).
--   * `completed`+ok=false (error) — error present, no data.
--   * `partial`  — ok=true; data AND warnings present.
--
-- mlua sandbox: acquired via `local wezterm = require("wezterm")`. The
-- standalone spec (`result_spec.lua`) installs a wezterm shim via
-- `package.preload["wezterm"]` BEFORE requiring this file.

local wezterm = require("wezterm")
local b64     = require("wezsesh.crypto.b64")
local globals = require("wezsesh.runtime.globals")

local M = {}

-- Reply-envelope JSON encoder. Production mlua's `wezterm.json_encode`
-- serialises empty Lua tables as `[]` (mlua-serde's empty-table
-- heuristic), which the Go reply decoder cannot unmarshal into
-- `Data map[string]any` / `Error.Details map[string]any`. We need an
-- empty `data`/`details` to land on the wire as `{}` while `warnings`
-- stays as `[]`. Since the reply path is NOT the canonical-JSON wire
-- (see header comment above), byte-equality with Go is not
-- required; only valid JSON with the right shape distinction.
--
-- The encoder honours an explicit metatable-tag set by `as_object` /
-- `as_array` (the reply_* call sites tag the relevant containers), and
-- falls back to a deterministic heuristic: a non-empty table whose
-- keys are all positive integers is an array; everything else
-- (including the empty default) is an object.
local OBJECT_MT = { __wezsesh_reply_kind = "object" }
local ARRAY_MT  = { __wezsesh_reply_kind = "array"  }

-- Tag the caller's table in place. Mutation is fine — the only callers
-- are inside this module, the tagged tables are constructed (or `or {}`'d)
-- at the reply_* call sites, and they never leave the module after
-- encoding.
local function as_object(t) return setmetatable(t or {}, OBJECT_MT) end
local function as_array(t)  return setmetatable(t or {}, ARRAY_MT)  end

local json_encode

local function escape_string(s)
    -- Standard JSON escapes for the reply path. ASCII passthrough is
    -- fine — UTF-8 validation isn't required here (the canonical-JSON
    -- canonical-JSON request wire is what enforces UTF-8; this is the
    -- reply channel).
    local out = { '"' }
    for i = 1, #s do
        local b = s:byte(i)
        if b == 0x22 then       out[#out + 1] = '\\"'
        elseif b == 0x5C then   out[#out + 1] = "\\\\"
        elseif b == 0x08 then   out[#out + 1] = "\\b"
        elseif b == 0x09 then   out[#out + 1] = "\\t"
        elseif b == 0x0A then   out[#out + 1] = "\\n"
        elseif b == 0x0C then   out[#out + 1] = "\\f"
        elseif b == 0x0D then   out[#out + 1] = "\\r"
        elseif b < 0x20 then
            out[#out + 1] = string.format("\\u%04x", b)
        else
            out[#out + 1] = string.char(b)
        end
    end
    out[#out + 1] = '"'
    return table.concat(out)
end

local function detect_kind(t)
    local mt = getmetatable(t)
    if mt and mt.__wezsesh_reply_kind == "array"  then return "array"  end
    if mt and mt.__wezsesh_reply_kind == "object" then return "object" end
    -- Untagged heuristic: non-empty + all-integer-keys → array;
    -- otherwise object (so empty `{}` defaults to `{}` on the wire,
    -- which is what the Go decoder wants for `data` / `details`).
    local n = 0
    for k in pairs(t) do
        if type(k) ~= "number" or k ~= math.floor(k) or k < 1 then
            return "object"
        end
        n = n + 1
    end
    if n == 0 then return "object" end
    return "array"
end

json_encode = function(v)
    local t = type(v)
    if t == "nil"     then return "null" end
    if t == "boolean" then return v and "true" or "false" end
    if t == "string"  then return escape_string(v) end
    if t == "number" then
        if v ~= v or v == math.huge or v == -math.huge then
            error("json_encode: non-finite number not encodable", 0)
        end
        if math.type and math.type(v) == "integer" then
            return tostring(v)
        end
        if math.floor(v) == v and math.abs(v) < 1e16 then
            return string.format("%d", v)
        end
        -- Floats aren't expected on this path; %.17g preserves
        -- round-trip without raising.
        return string.format("%.17g", v)
    end
    if t == "table" then
        local kind = detect_kind(v)
        if kind == "array" then
            local parts = {}
            for i = 1, #v do parts[i] = json_encode(v[i]) end
            return "[" .. table.concat(parts, ",") .. "]"
        end
        -- Object: sorted-key iteration for deterministic output. The
        -- result_spec asserts on substring matches over the JSON
        -- bytes; determinism keeps those stable.
        local keys = {}
        for k in pairs(v) do
            if type(k) ~= "string" then
                error("json_encode: object key must be string, got "
                    .. type(k), 0)
            end
            keys[#keys + 1] = k
        end
        table.sort(keys)
        local parts = {}
        for _, k in ipairs(keys) do
            parts[#parts + 1] = escape_string(k) .. ":" .. json_encode(v[k])
        end
        return "{" .. table.concat(parts, ",") .. "}"
    end
    error("json_encode: unsupported type " .. t, 0)
end

-- Resolve the wezsesh binary path. apply_to_config writes it via
-- `runtime.globals.set_bin_path` at load time; an unset value (mis-
-- configured plugin) makes the spawn a noop and the binary on the
-- other end hits IPC_TIMEOUT, which the TUI surfaces.
local function bin_path()
    local p = globals.bin_path()
    if type(p) == "string" and #p > 0 then return p end
    return nil
end

-- Spawn `wezsesh reply <sock> <b64>` fire-and-forget. The
-- `lua-bg-child-process-pcall` lint verifies this single call to
-- `wezterm.background_child_process` is wrapped in `pcall` — a failed
-- spawn MUST NOT bubble out of the user-var-changed handler.
--
-- Returns true if the spawn was attempted; false if we couldn't even
-- get to the spawn (missing binary path, missing reply_sock, encode
-- failure, etc.). Production callers ignore the return — failures are
-- structurally indistinguishable from "binary already exited" and the
-- TUI's reply-timeout is the only safety net we need.
local function spawn_reply(reply_sock, envelope)
    local bin = bin_path()
    if bin == nil then return false end
    if type(reply_sock) ~= "string" or #reply_sock == 0 then
        return false
    end

    -- JSON-encode the reply envelope via the local encoder above.
    -- Production mlua's `wezterm.json_encode` would emit `[]` for an
    -- empty Lua table (mlua-serde heuristic), which would break the
    -- Go reply decoder's `Data map[string]any` / `Error.Details
    -- map[string]any` shapes for noop / UNKNOWN_VERB / nil-details
    -- paths. The local encoder honours `as_object` / `as_array` tags
    -- and defaults empty to `{}`. pcall-wrap anyway: a programmer-
    -- error-shaped value (function, userdata) would raise.
    local enc_ok, json = pcall(json_encode, envelope)
    if not enc_ok or type(json) ~= "string" then return false end

    local b64s = b64.encode(json)

    -- pcall-wrap the spawn. The argv is the public CLI contract:
    -- `wezsesh reply <sock> <b64json>`. Argv form (not a shell string)
    -- sidesteps every shell-escaping concern.
    local ok = pcall(wezterm.background_child_process,
                     { bin, "reply", reply_sock, b64s })
    return ok
end

-- `started` reply. Restore-class verbs (`switch` to saved-not-live,
-- `load`) emit this BEFORE running the actual restore. The TUI
-- dismisses immediately on receipt; a `completed` or `partial`
-- follow-up is required within 30 s.
--
-- Invariant: started ⇒ ok=true, NO data / warnings / error.
-- We deliberately do NOT thread through any optional kwargs here —
-- the structural shape is fixed and a future call site that wants
-- to attach data to a `started` reply is a bug we want to catch
-- via the spec.
function M.reply_started(payload)
    return spawn_reply(payload.reply_sock, {
        v      = payload.v,
        id     = payload.id,
        status = "started",
        ok     = true,
    })
end

-- `completed` + ok=true reply. The verb-specific `data` shape is the
-- caller's responsibility (per-verb shapes live in the verb modules).
-- A nil `data` is normalised to an empty table — the wire mandates
-- `data` be present (may be `{}`) for completed+ok=true.
function M.reply_completed(payload, data)
    return spawn_reply(payload.reply_sock, {
        v      = payload.v,
        id     = payload.id,
        status = "completed",
        ok     = true,
        -- Tag as object so an empty `data` lands on the wire as `{}`
        -- (Go's `Data map[string]any` cannot unmarshal `[]`).
        data   = as_object(data),
    })
end

-- `partial` reply. Terminal: success-with-warnings (e.g.,
-- `RESURRECT_PARTIAL` after a mid-restore Lua error). The wire
-- mandates BOTH `data` AND `warnings` present; we normalise nils to
-- empty containers so a caller passing nil for either still produces
-- a conforming envelope.
function M.reply_partial(payload, data, warnings)
    -- Per-warning `details` tagging: the heuristic already classifies
    -- a populated `details` table as an object (string keys), but a
    -- caller passing an explicitly-empty `details = {}` would land as
    -- `[]` without an explicit tag. Cheap to tag every entry.
    if type(warnings) == "table" then
        for _, w in ipairs(warnings) do
            if type(w) == "table" and type(w.details) == "table" then
                as_object(w.details)
            end
        end
    end
    return spawn_reply(payload.reply_sock, {
        v        = payload.v,
        id       = payload.id,
        status   = "partial",
        ok       = true,
        -- data + warnings BOTH present. Object for data, array
        -- for warnings — empty containers must serialise to `{}` and
        -- `[]` respectively to match the Go reply decoder's shape.
        data     = as_object(data),
        warnings = as_array(warnings),
    })
end

-- `completed` + ok=false reply. Terminal failure with a structured
-- error. `code` is the wire-stable error-code identifier, e.g.
-- `UNKNOWN_VERB`, `SNAPSHOT_LOAD_FAILED`. `message` is human-readable;
-- `details` is the per-code shape (e.g., `{raw_error = "..."}` for
-- SAVE_FAILED).
function M.reply_error(payload, code, message, details)
    return spawn_reply(payload.reply_sock, {
        v      = payload.v,
        id     = payload.id,
        status = "completed",
        ok     = false,
        error  = {
            code    = tostring(code or "UNKNOWN"),
            message = tostring(message or ""),
            -- Tag as object: Go's `Error.Details map[string]any`
            -- cannot unmarshal an `[]` empty-table serialisation.
            details = as_object(details),
        },
    })
end

-- Toast helper. Surfaces a wezterm overlay toast for non-wire
-- failure modes (HMAC mismatch logged + IPC_INIT_FAILED on the Lua
-- side, etc.). pcall-wrapped: `window:toast_notification(...)` is a
-- userdata method call on a wezterm-side object and a stale `window`
-- handle (e.g., closed window) raises.
function M.toast(window, message, ms)
    if window == nil or type(message) ~= "string" then return false end
    local ok = pcall(function()
        window:toast_notification("wezsesh", message, nil, ms or 4000)
    end)
    return ok
end

return M
