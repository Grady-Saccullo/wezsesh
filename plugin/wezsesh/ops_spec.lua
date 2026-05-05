-- Busted-style spec for ops.lua. Self-contained — runs under plain
-- `lua plugin/wezsesh/ops_spec.lua` from the repo root, no busted
-- required.
--
-- Run:
--     cd <repo-root>
--     lua plugin/wezsesh/ops_spec.lua
--
-- Exits 0 with `OK N/N` on success, 1 with FAIL lines on stderr
-- otherwise. Mirrors the structure of result_spec.lua /
-- resurrect_error_spec.lua.
--
-- The spec installs a wezterm-shim via `package.preload["wezterm"]`
-- BEFORE requiring the module under test. The shim records every
-- `wezterm.background_child_process` invocation in `bg_calls` so the
-- spec can decode the b64 reply payload and assert on the §3.4
-- envelope shape. `wezterm.mux` is a stub the per-test setup mutates
-- to exercise the switch / new dispatch arms.
--
-- Dependency injection: `ops._set_deps{ resurrect = …, with_capture = … }`
-- swaps the resurrect global and capture wrapper for fakes per test.
-- The default `with_capture` lazy-requires the production
-- `wezsesh.resurrect_error`, so unless overridden the integration is
-- exercised end-to-end.

local function script_dir()
    local src = arg and arg[0] or "plugin/wezsesh/ops_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
package.path = script_dir() .. "/?.lua;"
            .. script_dir() .. "/../?.lua;"
            .. package.path

-- ────────────────────────────────────────────────────────────────────
-- wezterm shim — installed BEFORE require("ops")
-- ────────────────────────────────────────────────────────────────────

local function deepcopy(v)
    if type(v) ~= "table" then return v end
    local out = {}
    for k, vv in pairs(v) do out[k] = deepcopy(vv) end
    return out
end

-- Tiny pure-Lua JSON encode/parse — sufficient for the flat envelopes
-- result.lua emits. Mirrors the helpers in result_spec.lua so the
-- shapes can be asserted by round-tripping through the b64'd argv[4].

local function json_encode_shim(v)
    local function emit(x)
        local t = type(x)
        if t == "number" then return tostring(x) end
        if t == "boolean" then return x and "true" or "false" end
        if t == "string" then
            return '"' .. x:gsub("[\\\"]", "\\%0") .. '"'
        end
        if t == "table" then
            local n = 0
            local is_array = true
            for k in pairs(x) do
                if type(k) ~= "number" then is_array = false; break end
                n = n + 1
            end
            if is_array and n > 0 then
                local parts = {}
                for i = 1, n do parts[i] = emit(x[i]) end
                return "[" .. table.concat(parts, ",") .. "]"
            end
            local keys = {}
            for k in pairs(x) do keys[#keys + 1] = k end
            table.sort(keys, function(a, b)
                return tostring(a) < tostring(b)
            end)
            local parts = {}
            for _, k in ipairs(keys) do
                parts[#parts + 1] = '"' .. tostring(k) .. '":' .. emit(x[k])
            end
            return "{" .. table.concat(parts, ",") .. "}"
        end
        if t == "nil" then return "null" end
        error("json_encode_shim: unsupported type " .. t)
    end
    return emit(v)
end

local function json_parse_shim(s)
    local pos = 1
    local function err(msg)
        error("json_parse_shim: " .. msg .. " at " .. pos, 0)
    end
    local function skip_ws()
        while pos <= #s do
            local c = s:sub(pos, pos)
            if c == " " or c == "\t" or c == "\n" or c == "\r" then
                pos = pos + 1
            else return end
        end
    end
    local parse_value
    local function parse_string()
        if s:sub(pos, pos) ~= '"' then err("expected string") end
        pos = pos + 1
        local out = {}
        while pos <= #s do
            local c = s:sub(pos, pos)
            if c == '"' then pos = pos + 1; return table.concat(out) end
            if c == "\\" then
                pos = pos + 1
                out[#out + 1] = s:sub(pos, pos)
                pos = pos + 1
            else
                out[#out + 1] = c
                pos = pos + 1
            end
        end
        err("unterminated string")
    end
    local function parse_number()
        local s2 = s:sub(pos)
        local num_str = s2:match("^%-?%d+%.?%d*[eE]?[%-+]?%d*")
        if not num_str or num_str == "" then err("bad number") end
        pos = pos + #num_str
        local n = tonumber(num_str)
        if not num_str:find("[.eE]") then
            n = math.tointeger(n) or n
        end
        return n
    end
    local function parse_object()
        pos = pos + 1; skip_ws()
        local out = {}
        if s:sub(pos, pos) == "}" then pos = pos + 1; return out end
        while true do
            skip_ws()
            local k = parse_string()
            skip_ws()
            if s:sub(pos, pos) ~= ":" then err("expected ':'") end
            pos = pos + 1; skip_ws()
            out[k] = parse_value()
            skip_ws()
            local c = s:sub(pos, pos)
            if c == "}" then pos = pos + 1; return out end
            if c ~= "," then err("expected ',' or '}'") end
            pos = pos + 1
        end
    end
    local function parse_array()
        pos = pos + 1; skip_ws()
        local out = {}
        if s:sub(pos, pos) == "]" then pos = pos + 1; return out end
        while true do
            skip_ws()
            out[#out + 1] = parse_value()
            skip_ws()
            local c = s:sub(pos, pos)
            if c == "]" then pos = pos + 1; return out end
            if c ~= "," then err("expected ',' or ']'") end
            pos = pos + 1
        end
    end
    parse_value = function()
        skip_ws()
        local c = s:sub(pos, pos)
        if c == "{" then return parse_object() end
        if c == "[" then return parse_array() end
        if c == '"' then return parse_string() end
        if c == "t" then
            if s:sub(pos, pos + 3) == "true" then pos = pos + 4; return true end
            err("bad literal")
        end
        if c == "f" then
            if s:sub(pos, pos + 4) == "false" then pos = pos + 5; return false end
            err("bad literal")
        end
        if c == "n" then
            if s:sub(pos, pos + 3) == "null" then pos = pos + 4; return nil end
            err("bad literal")
        end
        return parse_number()
    end
    skip_ws()
    return parse_value()
end

-- Mutable shim surface — reset between tests by `reset_state()`.
local bg_calls
local global_store
local mux_stub
local log_warns

local global_proxy = setmetatable({}, {
    __index = function(_, k) return deepcopy(global_store[k]) end,
    __newindex = function(_, k, v) global_store[k] = deepcopy(v) end,
})

local wezterm_shim = {
    GLOBAL = global_proxy,
    target_triple = "x86_64-unknown-linux-gnu",
    json_encode = json_encode_shim,
    background_child_process = function(argv)
        bg_calls[#bg_calls + 1] = deepcopy(argv)
        return true
    end,
    log_warn = function(msg)
        log_warns[#log_warns + 1] = tostring(msg)
    end,
    log_error = function(msg)
        log_warns[#log_warns + 1] = "ERR: " .. tostring(msg)
    end,
    -- mux is a per-test stub. Production wezterm.mux methods used by
    -- ops.lua: get_workspace_names, set_active_workspace, spawn_window.
    mux = setmetatable({}, {
        __index = function(_, k) return mux_stub[k] end,
    }),
}

-- `wezterm.on` shim — captures the resurrect.error handler the
-- production resurrect_error module installs at register() time. The
-- spec exposes `emit("resurrect.error", msg)` for fakes that want to
-- synthesise an error during a save / load.
local on_handlers = {}
function wezterm_shim.on(event, handler)
    on_handlers[event] = on_handlers[event] or {}
    table.insert(on_handlers[event], handler)
end

local function emit(event, ...)
    local hs = on_handlers[event] or {}
    for i = 1, #hs do hs[i](...) end
end

package.preload["wezterm"] = function() return wezterm_shim end

-- Now load the modules under test.
local b64 = require("b64")
local resurrect_error = require("resurrect_error")
local canonical_json = require("canonical_json")
local ops = require("ops")

-- Install the persistent resurrect.error listener once (matches the
-- production `apply_to_config` flow). The _G install gate keeps this
-- idempotent across reset_state() rounds.
resurrect_error.register()

-- ────────────────────────────────────────────────────────────────────
-- minimal busted-shaped harness
-- ────────────────────────────────────────────────────────────────────

local failures, total = 0, 0
local current_describe = "<top>"

local function describe(name, fn)
    local prev = current_describe
    current_describe = name
    fn()
    current_describe = prev
end

local function reset_state()
    bg_calls = {}
    log_warns = {}
    global_store = {
        wezsesh_bin_path = "/usr/local/bin/wezsesh",
    }
    mux_stub = {}
    -- Drain any leftover diagnostic-ring entries so a previous test's
    -- uncaptured emissions don't bleed in.
    if resurrect_error.clear_uncaptured then
        resurrect_error.clear_uncaptured()
    end
    ops._reset_deps()
end

local function it(name, fn)
    total = total + 1
    reset_state()
    local ok, err = pcall(fn)
    if not ok then
        failures = failures + 1
        io.stderr:write(string.format("FAIL [%s] %s\n  %s\n",
            current_describe, name, tostring(err)))
    end
end

local function assert_eq(got, want, msg)
    if got ~= want then
        error(string.format("%s\n   got: %s\n  want: %s",
            msg or "values differ", tostring(got), tostring(want)), 2)
    end
end

local function assert_true(cond, msg)
    if not cond then error(msg or "expected truthy", 2) end
end

local function assert_nil(v, msg)
    if v ~= nil then
        error((msg or "expected nil") .. ", got: " .. tostring(v), 2)
    end
end

local function decode_envelope(idx)
    idx = idx or #bg_calls
    assert_true(#bg_calls >= idx,
        "expected at least " .. idx .. " spawn calls, got " .. #bg_calls)
    local argv = bg_calls[idx]
    assert_eq(argv[2], "reply", "argv[2] not 'reply'")
    local json = b64.decode(argv[4])
    assert_true(json ~= nil, "argv[4] was not valid b64")
    return json_parse_shim(json), argv
end

local function fixture_payload(op, args)
    return {
        v          = 1,
        id         = "01JABCDEFGHJKMNPQRSTVWXYZA",
        ts         = 1700000000,
        op         = op or "noop",
        reply_sock = "/tmp/wezsesh-1000/abcdef01.sock",
        args       = args or {},
    }
end

-- ────────────────────────────────────────────────────────────────────
-- Module surface (T-601 done-when)
-- ────────────────────────────────────────────────────────────────────

describe("module surface (§9.4)", function()
    it("exposes dispatch_table, dispatch, _set_deps, _reset_deps",
    function()
        assert_true(type(ops.dispatch_table) == "table",
            "M.dispatch_table missing")
        assert_true(type(ops.dispatch) == "function",
            "M.dispatch missing")
        assert_true(type(ops._set_deps) == "function",
            "M._set_deps missing")
        assert_true(type(ops._reset_deps) == "function",
            "M._reset_deps missing")
    end)

    it("dispatch_table has exactly the five §6 verbs", function()
        local want = { "load", "new", "noop", "save", "switch" }
        local keys = {}
        for k in pairs(ops.dispatch_table) do keys[#keys + 1] = k end
        table.sort(keys)
        assert_eq(table.concat(keys, ","), table.concat(want, ","),
            "dispatch_table verb set drift")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §17.4 — verb / shape parity (runtime mirror of CI lint)
-- ────────────────────────────────────────────────────────────────────

describe("verb / shape parity (§17.4)", function()
    it("verb_args_shape keys equal dispatch_table keys", function()
        local sk, dk = {}, {}
        for k in pairs(canonical_json.verb_args_shape) do
            sk[#sk + 1] = k
        end
        for k in pairs(ops.dispatch_table) do
            dk[#dk + 1] = k
        end
        table.sort(sk); table.sort(dk)
        assert_eq(table.concat(sk, ","), table.concat(dk, ","),
            "parity drift: shape={" .. table.concat(sk, ",")
            .. "} dispatch={" .. table.concat(dk, ",") .. "}")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §13.13 — Unknown verb (defensive branch unreachable in production)
-- ────────────────────────────────────────────────────────────────────

describe("unknown verb (§13.13 / §9.4 defensive branch)", function()
    it("dispatching `op=bogus` replies UNKNOWN_VERB, ok=false, "
        .. "status=completed", function()
        local p = fixture_payload("bogus")
        ops.dispatch(p, nil, nil)
        local env = decode_envelope()
        assert_eq(env.status, "completed", "unknown: status wrong")
        assert_eq(env.ok, false, "unknown: ok wrong")
        assert_eq(env.error.code, "UNKNOWN_VERB",
            "unknown: error.code wrong")
        assert_true(env.error.message:find("bogus", 1, true) ~= nil,
            "unknown: error.message must mention the op name; got: "
            .. tostring(env.error.message))
        assert_nil(env.data, "unknown: data must NOT be present")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §6.5 — noop
-- ────────────────────────────────────────────────────────────────────

describe("§6.5 — noop", function()
    it("replies completed + empty data", function()
        local p = fixture_payload("noop")
        ops.dispatch(p, nil, nil)
        local env = decode_envelope()
        assert_eq(env.status, "completed", "noop: status wrong")
        assert_eq(env.ok, true, "noop: ok wrong")
        assert_true(env.data ~= nil, "noop: data must be present")
        local n = 0
        for _ in pairs(env.data) do n = n + 1 end
        assert_eq(n, 0, "noop: data must be empty")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §9.4.2 — save (dual-path detector)
-- ────────────────────────────────────────────────────────────────────

describe("§9.4.2 — save", function()
    it("Lua-side I/O failure (capture non-empty) → SAVE_FAILED via "
        .. "with_capture", function()
        ops._set_deps{
            resurrect = {
                workspace_state = {
                    get_workspace_state = function()
                        return { window_states = {} }
                    end,
                },
                state_manager = {
                    save_state = function(_state)
                        -- Spike #2 V6: save_state silently emits
                        -- resurrect.error and returns nil on I/O failure
                        -- (chmod-0500 dir, ENOSPC, …). Synthesise via
                        -- the persistent listener.
                        emit("resurrect.error",
                            "Failed to write state: Could not open file: "
                            .. "/var/data/snap.json")
                        return nil
                    end,
                },
            },
            with_capture = resurrect_error.with_capture,
        }
        local p = fixture_payload("save", { name = "snap-1" })
        ops.dispatch(p, nil, nil)
        local env = decode_envelope()
        assert_eq(env.status, "completed", "save I/O: status wrong")
        assert_eq(env.ok, false, "save I/O: ok must be false")
        assert_eq(env.error.code, "SAVE_FAILED",
            "save I/O: error.code wrong")
        assert_true(
            env.error.details.raw_error:find("Failed to write state",
                1, true) ~= nil,
            "save I/O: details.raw_error missing the captured error; got: "
            .. tostring(env.error.details.raw_error))
    end)

    it("Lua-side encode failure (pcall raised) → SAVE_FAILED via pcall",
    function()
        local raised_msg =
            "error converting Lua function to value (JsonValue)"
        ops._set_deps{
            resurrect = {
                workspace_state = {
                    get_workspace_state = function()
                        return { window_states = {} }
                    end,
                },
                state_manager = {
                    save_state = function(_state)
                        -- Spike #2 V4a: wezterm.json_encode raises on
                        -- non-encodable inputs (function values,
                        -- userdata). Emulate the raise.
                        error(raised_msg, 0)
                    end,
                },
            },
            with_capture = resurrect_error.with_capture,
        }
        local p = fixture_payload("save", { name = "snap-1" })
        ops.dispatch(p, nil, nil)
        local env = decode_envelope()
        assert_eq(env.status, "completed", "save encode: status wrong")
        assert_eq(env.ok, false, "save encode: ok must be false")
        assert_eq(env.error.code, "SAVE_FAILED",
            "save encode: error.code wrong")
        assert_true(
            env.error.details.raw_error:find(
                "error converting Lua function", 1, true) ~= nil,
            "save encode: raw_error must include serde_json msg; got: "
            .. tostring(env.error.details.raw_error))
    end)

    it("success → completed + data: { name }; capture stays empty; "
        .. "Lua does NOT add `hash`", function()
        local save_called = false
        ops._set_deps{
            resurrect = {
                workspace_state = {
                    get_workspace_state = function()
                        return { window_states = {} }
                    end,
                },
                state_manager = {
                    save_state = function(_state)
                        save_called = true
                        return nil
                    end,
                },
            },
            with_capture = resurrect_error.with_capture,
        }
        local p = fixture_payload("save", { name = "snap-1" })
        ops.dispatch(p, nil, nil)
        local env = decode_envelope()
        assert_true(save_called, "save: state_manager.save_state not called")
        assert_eq(env.status, "completed", "save success: status wrong")
        assert_eq(env.ok, true, "save success: ok wrong")
        assert_eq(env.data.name, "snap-1", "save success: data.name wrong")
        assert_nil(env.data.hash,
            "save success: Lua MUST NOT set `hash`; binary fills it "
            .. "in Phase C (§13.4)")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §9.4.1 — load (split-reply restore-class)
-- ────────────────────────────────────────────────────────────────────

describe("§9.4.1 — load", function()
    it("torn JSON (pcall raised) → started preamble + "
        .. "SNAPSHOT_LOAD_FAILED; restore_workspace NEVER called",
    function()
        local restore_called = false
        ops._set_deps{
            resurrect = {
                state_manager = {
                    load_state = function(_name, _kind)
                        -- Spike #2 V3: torn JSON arrives as a
                        -- wezterm.json_parse throw. Emulate.
                        error(
                            "EOF while parsing a value at line 1 column 5",
                            0)
                    end,
                },
                workspace_state = {
                    restore_workspace = function(_state, _opts)
                        restore_called = true
                    end,
                },
            },
            with_capture = resurrect_error.with_capture,
        }
        local p = fixture_payload("load", { name = "snap-1" })
        ops.dispatch(p, nil, nil)
        assert_eq(#bg_calls, 2,
            "load torn-JSON: expected 2 spawns (started + error), got "
            .. #bg_calls)
        local started = decode_envelope(1)
        assert_eq(started.status, "started",
            "load torn-JSON: first reply must be `started`")
        local err_env = decode_envelope(2)
        assert_eq(err_env.status, "completed",
            "load torn-JSON: terminal reply not `completed`")
        assert_eq(err_env.ok, false, "load torn-JSON: ok must be false")
        assert_eq(err_env.error.code, "SNAPSHOT_LOAD_FAILED",
            "load torn-JSON: error.code wrong")
        assert_true(
            err_env.error.details.raw_error:find("EOF while parsing",
                1, true) ~= nil,
            "load torn-JSON: raw_error lost; got: "
            .. tostring(err_env.error.details.raw_error))
        assert_true(not restore_called,
            "load torn-JSON: restore_workspace MUST NOT run on a "
            .. "failed load (§9.4.1 step 2 guard)")
    end)

    it("silent decrypt failure ({} return + capture) → "
        .. "SNAPSHOT_LOAD_FAILED; restore_workspace NEVER called",
    function()
        local restore_called = false
        ops._set_deps{
            resurrect = {
                state_manager = {
                    load_state = function(_name, _kind)
                        -- Spike #2 V5: decrypt failure path. resurrect's
                        -- state_manager returns `{}` after emitting a
                        -- resurrect.error.
                        emit("resurrect.error",
                            "Decryption failed: bad key")
                        return {}
                    end,
                },
                workspace_state = {
                    restore_workspace = function(_state, _opts)
                        restore_called = true
                    end,
                },
            },
            with_capture = resurrect_error.with_capture,
        }
        local p = fixture_payload("load", { name = "snap-1" })
        ops.dispatch(p, nil, nil)
        assert_eq(#bg_calls, 2,
            "load decrypt: expected 2 spawns (started + error), got "
            .. #bg_calls)
        local started = decode_envelope(1)
        assert_eq(started.status, "started",
            "load decrypt: first reply must be `started`")
        local err_env = decode_envelope(2)
        assert_eq(err_env.status, "completed",
            "load decrypt: terminal reply not `completed`")
        assert_eq(err_env.ok, false, "load decrypt: ok must be false")
        assert_eq(err_env.error.code, "SNAPSHOT_LOAD_FAILED",
            "load decrypt: error.code wrong")
        assert_true(
            err_env.error.details.raw_error:find("Decryption failed",
                1, true) ~= nil,
            "load decrypt: raw_error lost; got: "
            .. tostring(err_env.error.details.raw_error))
        assert_true(not restore_called,
            "load decrypt: restore_workspace MUST NOT run when the "
            .. "load has no .window_states (§9.4.1 step 2 guard)")
    end)

    it("success → started + completed; data: { name, workspace }",
    function()
        local restore_called = false
        ops._set_deps{
            resurrect = {
                state_manager = {
                    load_state = function(_name, _kind)
                        return { window_states = { { dummy = true } } }
                    end,
                },
                workspace_state = {
                    restore_workspace = function(_state, _opts)
                        restore_called = true
                    end,
                },
            },
            with_capture = resurrect_error.with_capture,
        }
        local p = fixture_payload("load", { name = "snap-1" })
        ops.dispatch(p, nil, nil)
        assert_eq(#bg_calls, 2, "load success: expected 2 spawns")
        local started = decode_envelope(1)
        assert_eq(started.status, "started", "load success: started missing")
        local env = decode_envelope(2)
        assert_eq(env.status, "completed",
            "load success: terminal status wrong")
        assert_eq(env.ok, true, "load success: ok wrong")
        assert_eq(env.data.name, "snap-1",
            "load success: data.name wrong")
        assert_eq(env.data.workspace, "snap-1",
            "load success: data.workspace wrong")
        assert_true(restore_called,
            "load success: restore_workspace must run")
    end)

    it("restore-side partial → started + partial + warnings", function()
        ops._set_deps{
            resurrect = {
                state_manager = {
                    load_state = function(_name, _kind)
                        return { window_states = { { dummy = true } } }
                    end,
                },
                workspace_state = {
                    restore_workspace = function(_state, _opts)
                        emit("resurrect.error",
                            "Domain X is not spawnable")
                    end,
                },
            },
            with_capture = resurrect_error.with_capture,
        }
        local p = fixture_payload("load", { name = "snap-1" })
        ops.dispatch(p, nil, nil)
        assert_eq(#bg_calls, 2, "load partial: expected 2 spawns")
        local env = decode_envelope(2)
        assert_eq(env.status, "partial",
            "load partial: terminal status wrong")
        assert_eq(env.ok, true, "load partial: ok wrong")
        assert_eq(env.warnings[1].code, "RESURRECT_PARTIAL",
            "load partial: warning code wrong")
        assert_true(
            env.warnings[1].details.raw_error:find(
                "Domain X is not spawnable", 1, true) ~= nil,
            "load partial: raw_error lost; got: "
            .. tostring(env.warnings[1].details.raw_error))
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §6.1 — switch (live target vs saved-not-live)
-- ────────────────────────────────────────────────────────────────────

describe("§6.1 — switch", function()
    it("live target → completed + data: { active_workspace }; "
        .. "set_active_workspace called; load/restore NEVER", function()
        local set_called_with = nil
        local load_called = false
        mux_stub.get_workspace_names = function()
            return { "main", "alt" }
        end
        mux_stub.set_active_workspace = function(name)
            set_called_with = name
        end
        ops._set_deps{
            resurrect = {
                state_manager = {
                    load_state = function() load_called = true end,
                },
                workspace_state = {
                    restore_workspace = function() end,
                },
            },
            with_capture = resurrect_error.with_capture,
        }
        local p = fixture_payload("switch", { name = "main" })
        ops.dispatch(p, nil, nil)
        assert_eq(set_called_with, "main",
            "switch live: set_active_workspace not invoked with `main`")
        assert_true(not load_called,
            "switch live: load_state MUST NOT run on a live target")
        assert_eq(#bg_calls, 1,
            "switch live: expected 1 spawn (no started preamble)")
        local env = decode_envelope()
        assert_eq(env.status, "completed",
            "switch live: status wrong")
        assert_eq(env.ok, true, "switch live: ok wrong")
        assert_eq(env.data.active_workspace, "main",
            "switch live: data.active_workspace wrong")
    end)

    it("saved-not-live target → started + completed restore; "
        .. "load_state and restore_workspace called", function()
        local load_called = false
        local restore_called = false
        mux_stub.get_workspace_names = function()
            return { "alt" }
        end
        ops._set_deps{
            resurrect = {
                state_manager = {
                    load_state = function(_name, _kind)
                        load_called = true
                        return { window_states = { { dummy = true } } }
                    end,
                },
                workspace_state = {
                    restore_workspace = function(_state, _opts)
                        restore_called = true
                    end,
                },
            },
            with_capture = resurrect_error.with_capture,
        }
        local p = fixture_payload("switch", { name = "main" })
        ops.dispatch(p, nil, nil)
        assert_true(load_called,
            "switch saved-not-live: load_state must run")
        assert_true(restore_called,
            "switch saved-not-live: restore_workspace must run")
        assert_eq(#bg_calls, 2,
            "switch saved-not-live: expected 2 spawns "
            .. "(started + terminal)")
        local started = decode_envelope(1)
        assert_eq(started.status, "started",
            "switch saved-not-live: started missing")
        local env = decode_envelope(2)
        assert_eq(env.status, "completed",
            "switch saved-not-live: terminal status wrong")
        assert_eq(env.data.active_workspace, "main",
            "switch saved-not-live: data.active_workspace wrong")
    end)

    it("MUX_UNREACHABLE when set_active_workspace raises", function()
        mux_stub.get_workspace_names = function()
            return { "main" }
        end
        mux_stub.set_active_workspace = function()
            error("mux gone", 0)
        end
        local p = fixture_payload("switch", { name = "main" })
        ops.dispatch(p, nil, nil)
        local env = decode_envelope()
        assert_eq(env.error.code, "MUX_UNREACHABLE",
            "switch mux-unreachable: error.code wrong")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §6.4 — new
-- ────────────────────────────────────────────────────────────────────

describe("§6.4 — new", function()
    it("success → completed + data: { name, pane_id }", function()
        local fake_pane = { pane_id = function(_self) return 42 end }
        mux_stub.spawn_window = function(_opts)
            return {}, fake_pane, {}
        end
        local p = fixture_payload("new",
            { name = "~/proj", cwd = "/home/u/proj" })
        ops.dispatch(p, nil, nil)
        local env = decode_envelope()
        assert_eq(env.status, "completed", "new: status wrong")
        assert_eq(env.ok, true, "new: ok wrong")
        assert_eq(env.data.name, "~/proj", "new: data.name wrong")
        assert_eq(env.data.pane_id, 42, "new: data.pane_id wrong")
    end)

    it("MUX_UNREACHABLE when spawn_window raises", function()
        mux_stub.spawn_window = function(_opts)
            error("spawn_window failed", 0)
        end
        local p = fixture_payload("new",
            { name = "~/proj", cwd = "/home/u/proj" })
        ops.dispatch(p, nil, nil)
        local env = decode_envelope()
        assert_eq(env.error.code, "MUX_UNREACHABLE",
            "new mux-unreachable: error.code wrong")
        assert_true(
            env.error.details.raw_error:find("spawn_window failed",
                1, true) ~= nil,
            "new mux-unreachable: raw_error lost; got: "
            .. tostring(env.error.details.raw_error))
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Outer dispatch — pcall boundary swallows verb raises
-- ────────────────────────────────────────────────────────────────────

describe("dispatch outer pcall boundary (§9.4)", function()
    it("a verb raise does NOT propagate; replies UNKNOWN error",
    function()
        -- Inject a bogus dispatch arm that raises immediately. We use
        -- `noop` since it has no resurrect dependencies — the spec
        -- dispatch_table is mutable per the public contract, but we
        -- restore it after the test to keep the parity gate happy.
        local saved = ops.dispatch_table.noop
        ops.dispatch_table.noop = function()
            error("synthetic verb raise", 0)
        end
        local p = fixture_payload("noop")
        local ok = pcall(ops.dispatch, p, nil, nil)
        ops.dispatch_table.noop = saved
        assert_true(ok,
            "ops.dispatch must NOT raise out of a verb pcall boundary")
        local env = decode_envelope()
        assert_eq(env.error.code, "UNKNOWN",
            "verb raise: error.code wrong (expected sentinel UNKNOWN)")
        assert_true(
            env.error.details.raw_error:find("synthetic verb raise",
                1, true) ~= nil,
            "verb raise: raw_error must include the raised string; got: "
            .. tostring(env.error.details.raw_error))
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- summary
-- ────────────────────────────────────────────────────────────────────

if failures > 0 then
    io.stderr:write(string.format("FAILED %d/%d\n", failures, total))
    os.exit(1)
else
    io.stdout:write(string.format("OK %d/%d\n", total, total))
end
