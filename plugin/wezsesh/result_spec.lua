-- Busted-style spec for result.lua. Self-contained — runs under plain
-- `lua plugin/wezsesh/result_spec.lua` from the repo root, no busted
-- required.
--
-- Run:
--     cd <repo-root>
--     lua plugin/wezsesh/result_spec.lua
--
-- Exits 0 with `OK N/N` on success, 1 with FAIL lines on stderr
-- otherwise. Mirrors the structure of state_spec.lua / b64_spec.lua /
-- hmac_spec.lua / ct_eq_spec.lua / canonical_json_spec.lua.
--
-- The spec installs a wezterm-shim via `package.preload["wezterm"]`
-- BEFORE requiring the module under test, so result.lua's production
-- `require("wezterm")` line resolves to our test double. The shim
-- captures every `wezterm.background_child_process` invocation in
-- `bg_calls`, exposes a `bg_should_raise` toggle that lets us
-- exercise the pcall-wrap acceptance gate, and routes
-- `json_encode` / `GLOBAL` through pure-Lua replacements that are
-- shape-faithful enough for the four `reply_*` envelopes.

local function script_dir()
    local src = arg and arg[0] or "plugin/wezsesh/result_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
-- Two entries:
--   <script_dir>/?.lua            — bare requires for sibling modules
--                                    (`require("b64")`, etc.) used by the
--                                    spec harness only.
--   <script_dir>/../?.lua         — namespaced `require("wezsesh.b64")`
--                                    used by production result.lua per
--                                    cache-bust prefix rule.
--                                    Lua maps the dot to a slash, so
--                                    `wezsesh.b64` resolves to
--                                    `<script_dir>/../wezsesh/b64.lua`,
--                                    i.e. the same b64.lua sibling file.
package.path = script_dir() .. "/?.lua;"
            .. script_dir() .. "/../?.lua;"
            .. package.path

-- ────────────────────────────────────────────────────────────────────
-- wezterm shim — installed BEFORE require("result")
-- ────────────────────────────────────────────────────────────────────

local helpers = require("spec_helpers")
local deepcopy = helpers.deepcopy

-- Pure-Lua JSON encode sufficient for the flat envelopes result.lua
-- builds. We don't need byte-equality with Go here (the reply path is
-- not the canonical-JSON request wire). Round-trip via the
-- shim's parser is enough for the assertions below.
local function json_encode_shim(v)
    local function emit(x)
        local t = type(x)
        if t == "number" then return tostring(x) end
        if t == "boolean" then return x and "true" or "false" end
        if t == "string" then
            return '"' .. x:gsub("[\\\"]", "\\%0") .. '"'
        end
        if t == "table" then
            -- Detect array vs object by checking for a 1..n integer
            -- prefix. result.lua's `warnings` is an array; everything
            -- else is an object.
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
            -- Object: sorted keys for determinism in tests.
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

-- Tiny JSON parser sufficient for round-tripping the shim's output —
-- mirrors the one in state_spec.lua / canonical_json_spec.lua.
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

-- The shim's mutable surface. The spec tweaks `bg_should_raise` to
-- exercise the pcall-wrap acceptance gate; reads `bg_calls` to
-- assert the spawn argv shape; tweaks `GLOBAL.wezsesh_bin_path` to
-- exercise the missing-binary degraded path; tweaks
-- `GLOBAL.wezsesh_session_key` to drive the HMAC-signing path on
-- replies.
local bg_calls = {}
local bg_should_raise = false
-- A deterministic 64-hex test key matching internal/ipcdispatcher's
-- dispatcher_test.go so cross-language round-trip fixtures are
-- reproducible.
local FIXTURE_KEY_HEX =
    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
-- Deterministic plugin_session_id for the v=2 envelope correlation
-- assertions. Production mints this once per `apply_to_config` via
-- runtime/crypto/ulid.lua; the spec stubs it via GLOBAL so the
-- result.lua resolver picks it up the same way it does in production
-- (through `globals.plugin_session_id()`).
local FIXTURE_PLUGIN_SESSION_ID = "01JTESTPLUGIN_____________"
local global_store = {
    wezsesh_bin_path = "/usr/local/bin/wezsesh",
    wezsesh_session_key = FIXTURE_KEY_HEX,
    wezsesh_plugin_session_id = FIXTURE_PLUGIN_SESSION_ID,
}

local global_proxy = setmetatable({}, {
    __index = function(_, k) return deepcopy(global_store[k]) end,
    __newindex = function(_, k, v) global_store[k] = deepcopy(v) end,
})

local wezterm_shim = {
    GLOBAL = global_proxy,
    json_encode = json_encode_shim,
    background_child_process = function(argv)
        if bg_should_raise then
            error("synthetic spawn failure", 0)
        end
        bg_calls[#bg_calls + 1] = deepcopy(argv)
        return true
    end,
}
package.preload["wezterm"] = function() return wezterm_shim end

-- Now load the module under test.
local result = require("result")

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
    bg_should_raise = false
    global_store.wezsesh_bin_path = "/usr/local/bin/wezsesh"
    global_store.wezsesh_session_key = FIXTURE_KEY_HEX
    global_store.wezsesh_plugin_session_id = FIXTURE_PLUGIN_SESSION_ID
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

local function assert_false(cond, msg)
    if cond then error(msg or "expected falsy", 2) end
end

local function assert_nil(v, msg)
    if v ~= nil then
        error((msg or "expected nil") .. ", got: " .. tostring(v), 2)
    end
end

-- Decode the b64 payload of the most-recent spawn call into a Lua
-- table for shape assertions. Mirrors what `wezsesh reply` does on
-- the binary side.
local b64 = require("wezsesh.crypto.b64")

-- The spawn argv shape depends on whether `payload.binary_session_id`
-- is present (§3.3 v=2): with bsid we shell out via `/usr/bin/env
-- WEZSESH_BINARY_SESSION_ID=<bsid> <bin> reply <sock> <b64>` so the
-- spawned `wezsesh reply` child inherits the trace-correlation env
-- var; without bsid we fall back to the bare-argv `<bin> reply <sock>
-- <b64>` shape. parse_argv normalises both into a flat record so the
-- assertions below don't have to repeat the shape detection.
local function parse_argv(argv)
    if argv[1] == "/usr/bin/env" then
        local env_pair = argv[2]
        local env_key, env_val = env_pair:match("^([^=]+)=(.*)$")
        return {
            env_form          = true,
            env_key           = env_key,
            env_val           = env_val,
            bin               = argv[3],
            verb              = argv[4],
            reply_sock        = argv[5],
            b64               = argv[6],
        }
    end
    return {
        env_form   = false,
        bin        = argv[1],
        verb       = argv[2],
        reply_sock = argv[3],
        b64        = argv[4],
    }
end

local function last_envelope()
    assert_true(#bg_calls > 0,
        "expected at least one wezterm.background_child_process call")
    local argv = bg_calls[#bg_calls]
    local p = parse_argv(argv)
    assert_eq(p.bin, "/usr/local/bin/wezsesh",
        "argv: bin not the wezsesh binary path")
    assert_eq(p.verb, "reply", "argv: verb not 'reply'")
    assert_true(type(p.reply_sock) == "string" and #p.reply_sock > 0,
        "argv: reply_sock missing")
    local json = b64.decode(p.b64)
    assert_true(json ~= nil, "argv: b64 payload was not valid b64")
    return json_parse_shim(json), argv
end

-- Return the raw wire JSON bytes of the most-recent spawn call. Used
-- by tests that assert on the literal bytes (e.g. round-trip HMAC
-- verification, or substring checks on canonical-form deserialisation).
local function last_wire_json()
    assert_true(#bg_calls > 0,
        "expected at least one wezterm.background_child_process call")
    local argv = bg_calls[#bg_calls]
    local p = parse_argv(argv)
    local json = b64.decode(p.b64)
    assert_true(type(json) == "string" and #json > 0,
        "argv: b64 payload did not decode to non-empty string")
    return json
end

-- Assert the wire bytes carry an `"hmac":"<64-lower-hex>"` field.
-- The substring match is intentional — round-tripping through
-- json_parse_shim would lose any byte-level distinction we care about
-- (e.g. uppercase hex, wrong length).
local function assert_hmac_present(json)
    local hex = json:match('"hmac":"(%x+)"')
    assert_true(hex ~= nil,
        "expected an \"hmac\" field on the wire, got: " .. tostring(json))
    assert_eq(#hex, 64,
        "hmac field must be 64 hex chars, got " .. tostring(#hex)
        .. " (" .. tostring(hex) .. ")")
    -- Lowercase only; uppercase would slip through %x.
    if hex:find("[A-F]") then
        error("hmac must be lowercase hex, got: " .. hex, 2)
    end
    return hex
end

-- A canonical "valid request payload" stub. Each reply_* takes a
-- payload and reads `v`, `id`, `reply_sock`, `binary_session_id` from
-- it. The verb-specific tests below mutate `op` and the data shape
-- per verb.
local function fixture_payload(op)
    return {
        v                 = 2,
        id                = "01JABCDEFGHJKMNPQRSTVWXYZA",
        ts                = 1700000000,
        op                = op or "noop",
        reply_sock        = "/tmp/wezsesh-1000/abcdef01.sock",
        binary_session_id = "01JABCDEFGHJKMNPQRSTVWXYZB",
    }
end

-- ────────────────────────────────────────────────────────────────────
-- module surface
-- ────────────────────────────────────────────────────────────────────

describe("module surface", function()
    it("exposes the public API and nothing more", function()
        local want = {
            "reply_completed", "reply_error", "reply_partial",
            "reply_started", "toast",
        }
        local keys = {}
        for k in pairs(result) do keys[#keys + 1] = k end
        table.sort(keys)
        assert_eq(table.concat(keys, ","), table.concat(want, ","),
            "result module surface drift")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- every reply echoes `v` from the request
-- ────────────────────────────────────────────────────────────────────

describe("every reply echoes payload.v", function()
    it("reply_started echoes payload.v", function()
        result.reply_started(fixture_payload())
        local env = last_envelope()
        assert_eq(env.v, 2, "started: v != 2")
    end)

    it("reply_completed echoes payload.v", function()
        result.reply_completed(fixture_payload(), { foo = "bar" })
        local env = last_envelope()
        assert_eq(env.v, 2, "completed: v != 2")
    end)

    it("reply_partial echoes payload.v", function()
        result.reply_partial(fixture_payload(), { foo = "bar" },
            {{ code = "RESURRECT_PARTIAL", message = "x", details = {} }})
        local env = last_envelope()
        assert_eq(env.v, 2, "partial: v != 2")
    end)

    it("reply_error echoes payload.v", function()
        result.reply_error(fixture_payload(), "UNKNOWN_VERB", "msg", {})
        local env = last_envelope()
        assert_eq(env.v, 2, "error: v != 2")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- v=2 envelope correlation fields
-- ────────────────────────────────────────────────────────────────────

describe("session-id correlation fields (v=2)", function()
    it("reply_started echoes binary_session_id and stamps "
        .. "plugin_session_id", function()
        result.reply_started(fixture_payload())
        local env = last_envelope()
        assert_eq(env.binary_session_id, "01JABCDEFGHJKMNPQRSTVWXYZB",
            "binary_session_id not echoed")
        -- plugin_session_id is read from globals.plugin_session_id();
        -- the spec seeds wezsesh_plugin_session_id to a deterministic
        -- fixture so the resolver returns it.
        assert_eq(env.plugin_session_id, FIXTURE_PLUGIN_SESSION_ID,
            "plugin_session_id should be the GLOBAL-seeded fixture")
    end)

    it("reply_completed echoes both session ids", function()
        result.reply_completed(fixture_payload(), { foo = "bar" })
        local env = last_envelope()
        assert_eq(env.binary_session_id, "01JABCDEFGHJKMNPQRSTVWXYZB",
            "completed: binary_session_id not echoed")
        assert_eq(env.plugin_session_id, FIXTURE_PLUGIN_SESSION_ID,
            "completed: plugin_session_id wrong")
    end)

    it("reply_partial echoes both session ids", function()
        result.reply_partial(fixture_payload(), { foo = "bar" },
            {{ code = "RESURRECT_PARTIAL", message = "x", details = {} }})
        local env = last_envelope()
        assert_eq(env.binary_session_id, "01JABCDEFGHJKMNPQRSTVWXYZB",
            "partial: binary_session_id not echoed")
        assert_eq(env.plugin_session_id, FIXTURE_PLUGIN_SESSION_ID,
            "partial: plugin_session_id wrong")
    end)

    it("reply_error echoes both session ids", function()
        result.reply_error(fixture_payload(), "UNKNOWN_VERB", "msg", {})
        local env = last_envelope()
        assert_eq(env.binary_session_id, "01JABCDEFGHJKMNPQRSTVWXYZB",
            "error: binary_session_id not echoed")
        assert_eq(env.plugin_session_id, FIXTURE_PLUGIN_SESSION_ID,
            "error: plugin_session_id wrong")
    end)

    it("a payload missing binary_session_id falls back to '' "
        .. "(degraded but signed)", function()
        local p = fixture_payload()
        p.binary_session_id = nil
        result.reply_started(p)
        local env = last_envelope()
        assert_eq(env.binary_session_id, "",
            "missing payload bsid did not degrade to empty string")
    end)

    it("absent plugin_session_id (mint hadn't run yet) degrades to ''",
    function()
        -- Pre-`apply_to_config` window: globals.plugin_session_id()
        -- returns nil because init.lua hasn't minted yet. The reply
        -- builder MUST still emit the field as "" so the canonical-
        -- shape walker accepts the envelope.
        global_store.wezsesh_plugin_session_id = nil
        result.reply_started(fixture_payload())
        local env = last_envelope()
        assert_eq(env.plugin_session_id, "",
            "nil plugin_session_id should degrade to empty string")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- `started` reply has no data / warnings / error
-- ────────────────────────────────────────────────────────────────────

describe("started reply: invariants", function()
    it("status='started', ok=true, NO data / warnings / error", function()
        result.reply_started(fixture_payload())
        local env = last_envelope()
        assert_eq(env.status, "started", "status wrong")
        assert_eq(env.ok, true, "ok must be true on started")
        assert_nil(env.data, "started must NOT carry data")
        assert_nil(env.warnings, "started must NOT carry warnings")
        assert_nil(env.error, "started must NOT carry error")
    end)

    it("started carries v=2 envelope keys exactly, nothing else",
    function()
        result.reply_started(fixture_payload())
        local env = last_envelope()
        local keys = {}
        for k in pairs(env) do keys[#keys + 1] = k end
        table.sort(keys)
        assert_eq(table.concat(keys, ","),
            "binary_session_id,hmac,id,ok,plugin_session_id,status,v",
            "started envelope has unexpected fields: "
            .. table.concat(keys, ","))
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- completed/ok=true ⇒ data present (may be `{}`)
-- ────────────────────────────────────────────────────────────────────

describe("completed reply: invariants", function()
    it("status='completed', ok=true, data present", function()
        result.reply_completed(fixture_payload(), { active_workspace = "x" })
        local env = last_envelope()
        assert_eq(env.status, "completed", "status wrong")
        assert_eq(env.ok, true, "ok wrong")
        assert_true(env.data ~= nil, "completed+ok=true must carry data")
        assert_eq(env.data.active_workspace, "x", "data passthrough lost")
        assert_nil(env.error, "completed+ok=true must NOT carry error")
    end)

    it("nil data normalises to empty object {}", function()
        result.reply_completed(fixture_payload(), nil)
        local env = last_envelope()
        assert_true(env.data ~= nil, "nil data not normalised")
        local n = 0
        for _ in pairs(env.data) do n = n + 1 end
        assert_eq(n, 0, "data should be empty object")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- partial: ok=true, data AND warnings present
-- ────────────────────────────────────────────────────────────────────

describe("partial reply: invariants", function()
    it("status='partial', ok=true, both data AND warnings present",
    function()
        local warnings = {{
            code = "RESURRECT_PARTIAL", message = "mid-restore",
            details = { raw_error = "spawn failed" },
        }}
        result.reply_partial(fixture_payload(), { name = "x" }, warnings)
        local env = last_envelope()
        assert_eq(env.status, "partial", "status wrong")
        assert_eq(env.ok, true, "ok wrong")
        assert_true(env.data ~= nil, "partial must carry data")
        assert_true(env.warnings ~= nil, "partial must carry warnings")
        assert_eq(env.warnings[1].code, "RESURRECT_PARTIAL",
            "warning code lost")
        assert_nil(env.error, "partial must NOT carry error")
    end)

    it("nil data and warnings normalise to empty containers", function()
        result.reply_partial(fixture_payload(), nil, nil)
        local env = last_envelope()
        assert_true(env.data ~= nil, "nil data not normalised")
        assert_true(env.warnings ~= nil, "nil warnings not normalised")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- error: completed+ok=false ⇒ error present
-- ────────────────────────────────────────────────────────────────────

describe("error reply: invariants", function()
    it("status='completed', ok=false, error present, NO data", function()
        result.reply_error(fixture_payload(), "UNKNOWN_VERB",
            "unknown verb: bogus", { hint = "see verbs/" })
        local env = last_envelope()
        assert_eq(env.status, "completed", "status wrong")
        assert_eq(env.ok, false, "ok must be false on error")
        assert_true(env.error ~= nil, "error reply must carry error")
        assert_eq(env.error.code, "UNKNOWN_VERB", "error.code lost")
        assert_eq(env.error.message, "unknown verb: bogus",
            "error.message lost")
        assert_eq(env.error.details.hint, "see verbs/", "error.details lost")
        assert_nil(env.data, "error must NOT carry data")
    end)

    it("nil details normalises to empty object", function()
        result.reply_error(fixture_payload(), "UNKNOWN", "msg", nil)
        local env = last_envelope()
        assert_true(env.error.details ~= nil,
            "nil details not normalised")
    end)

    it("non-string code coerces to string via tostring", function()
        result.reply_error(fixture_payload(), nil, nil, nil)
        local env = last_envelope()
        assert_eq(env.error.code, "UNKNOWN",
            "nil code didn't degrade to UNKNOWN sentinel")
        assert_eq(env.error.message, "",
            "nil message didn't degrade to empty string")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- `pcall`-wrap on `wezterm.background_child_process` calls
-- ────────────────────────────────────────────────────────────────────

describe("pcall-wrap acceptance gate", function()
    it("a synthetic spawn failure does NOT propagate out of reply_*",
    function()
        bg_should_raise = true
        -- All four reply_* fns must swallow the spawn failure. If any
        -- of these raise, the user-var-changed handler that calls
        -- them would wedge the wezterm event loop — CLAUDE.md
        -- the lualint rule catches the static case; this
        -- spec exercises the runtime contract.
        local payload = fixture_payload()
        local ok1 = pcall(result.reply_started, payload)
        local ok2 = pcall(result.reply_completed, payload, {})
        local ok3 = pcall(result.reply_partial, payload, {}, {})
        local ok4 = pcall(result.reply_error, payload, "X", "msg", {})
        assert_true(ok1, "reply_started leaked spawn raise")
        assert_true(ok2, "reply_completed leaked spawn raise")
        assert_true(ok3, "reply_partial leaked spawn raise")
        assert_true(ok4, "reply_error leaked spawn raise")
    end)

    it("returns false on caught spawn failure (informational)",
    function()
        bg_should_raise = true
        local r = result.reply_started(fixture_payload())
        assert_eq(r, false,
            "spawn-failed call should return false (caller may log)")
    end)

    it("missing wezsesh_bin_path is a degraded noop, not a raise",
    function()
        global_store.wezsesh_bin_path = nil
        local ok, _ = pcall(result.reply_started, fixture_payload())
        assert_true(ok, "missing bin_path should not raise")
        assert_eq(#bg_calls, 0,
            "missing bin_path must not spawn a child process")
    end)

    it("missing reply_sock is a degraded noop, not a raise",
    function()
        local payload = fixture_payload()
        payload.reply_sock = nil
        local ok = pcall(result.reply_completed, payload, {})
        assert_true(ok, "missing reply_sock should not raise")
        assert_eq(#bg_calls, 0,
            "missing reply_sock must not spawn a child process")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- spawn argv shape — `wezsesh reply <sock> <b64>`
-- ────────────────────────────────────────────────────────────────────

describe("spawn argv: `/usr/bin/env WEZSESH_BINARY_SESSION_ID=… wezsesh "
    .. "reply <sock> <b64>`",
function()
    it("env-form argv: env, KEY=VAL, bin, 'reply', sock, b64 with "
        .. "binary_session_id threaded into the spawn env", function()
        result.reply_started(fixture_payload())
        local _, argv = last_envelope()
        assert_eq(#argv, 6, "argv (env-form) must have exactly 6 elements")
        assert_eq(argv[1], "/usr/bin/env",
            "argv[1] must be /usr/bin/env (POSIX env(1))")
        local p = parse_argv(argv)
        assert_true(p.env_form, "expected env-form argv")
        assert_eq(p.env_key, "WEZSESH_BINARY_SESSION_ID",
            "env-pair key must be WEZSESH_BINARY_SESSION_ID")
        assert_eq(p.env_val, fixture_payload().binary_session_id,
            "env-pair value must echo payload.binary_session_id")
        assert_eq(p.bin, "/usr/local/bin/wezsesh",
            "bin path must be the wezsesh binary")
        assert_eq(p.verb, "reply", "verb must be 'reply'")
        assert_eq(p.reply_sock, "/tmp/wezsesh-1000/abcdef01.sock",
            "reply_sock must be the request's reply_sock")
        assert_true(#p.b64 > 0, "b64 payload must be non-empty")
    end)

    it("falls back to bare argv when payload.binary_session_id is "
        .. "missing or malformed (legacy / fixture harness path)",
    function()
        local p = fixture_payload()
        p.binary_session_id = nil
        result.reply_started(p)
        local _, argv = last_envelope()
        assert_eq(#argv, 4, "argv (bare-form) must have exactly 4 elements")
        local parsed = parse_argv(argv)
        assert_false(parsed.env_form, "expected bare-form argv")
        assert_eq(parsed.bin, "/usr/local/bin/wezsesh",
            "bin path must be the wezsesh binary")
        assert_eq(parsed.verb, "reply", "verb must be 'reply'")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Per-verb reply shapes — the "Done when" contract.
-- Each verb's terminal completed-reply data is the verb's published
-- shape; restore-class verbs (switch saved-not-live, load) also emit
-- a started preamble.
-- ────────────────────────────────────────────────────────────────────

describe("`switch` verb reply shapes", function()
    it("live target: completed + data: { active_workspace }", function()
        local p = fixture_payload("switch")
        result.reply_completed(p, { active_workspace = "main" })
        local env = last_envelope()
        assert_eq(env.status, "completed", "switch live: status wrong")
        assert_eq(env.ok, true, "switch live: ok wrong")
        assert_eq(env.data.active_workspace, "main",
            "switch live: data shape wrong")
    end)

    it("saved-not-live target: started preamble", function()
        local p = fixture_payload("switch")
        result.reply_started(p)
        local env = last_envelope()
        assert_eq(env.status, "started", "switch restore: started missing")
        assert_nil(env.data, "switch started: data must be absent")
    end)

    it("saved-not-live target: terminal completed/partial follow-up",
    function()
        local p = fixture_payload("switch")
        result.reply_partial(p, { active_workspace = "main" }, {{
            code = "RESURRECT_PARTIAL", message = "x",
            details = { raw_error = "spawn failed" },
        }})
        local env = last_envelope()
        assert_eq(env.status, "partial", "switch partial: status wrong")
        assert_eq(env.warnings[1].code, "RESURRECT_PARTIAL",
            "switch partial: warnings shape wrong")
    end)

    it("verb-specific error: SNAPSHOT_MISSING", function()
        local p = fixture_payload("switch")
        result.reply_error(p, "SNAPSHOT_MISSING",
            "snapshot gone for 'main'", {})
        local env = last_envelope()
        assert_eq(env.error.code, "SNAPSHOT_MISSING",
            "switch error: code wrong")
    end)
end)

describe("`load` verb reply shapes", function()
    it("started preamble (load is restore-class)", function()
        local p = fixture_payload("load")
        result.reply_started(p)
        local env = last_envelope()
        assert_eq(env.status, "started", "load: started missing")
    end)

    it("success: completed + data: { name, workspace }", function()
        local p = fixture_payload("load")
        result.reply_completed(p, { name = "snap-1", workspace = "main" })
        local env = last_envelope()
        assert_eq(env.status, "completed", "load: status wrong")
        assert_eq(env.data.name, "snap-1", "load: data.name wrong")
        assert_eq(env.data.workspace, "main",
            "load: data.workspace wrong")
    end)

    it("partial success: RESURRECT_PARTIAL warning shape", function()
        local p = fixture_payload("load")
        result.reply_partial(p,
            { name = "snap-1", workspace = "main" },
            {{ code = "RESURRECT_PARTIAL", message = "spawn failed",
               details = { raw_error = "Domain X is not spawnable" } }})
        local env = last_envelope()
        assert_eq(env.status, "partial", "load partial: status wrong")
        assert_eq(env.warnings[1].details.raw_error,
            "Domain X is not spawnable",
            "load partial: raw_error lost")
    end)

    it("verb-specific error: SNAPSHOT_LOAD_FAILED with raw_error",
    function()
        local p = fixture_payload("load")
        result.reply_error(p, "SNAPSHOT_LOAD_FAILED",
            "EOF while parsing", { raw_error = "EOF while parsing" })
        local env = last_envelope()
        assert_eq(env.error.code, "SNAPSHOT_LOAD_FAILED",
            "load error: code wrong")
        assert_eq(env.error.details.raw_error, "EOF while parsing",
            "load error: raw_error lost")
    end)
end)

describe("`save` verb reply shapes", function()
    it("success: completed + data: { name } (binary fills hash later)",
    function()
        local p = fixture_payload("save")
        -- Lua side replies with `name` only; the binary's Phase C
        -- re-hash adds the `hash` field before forwarding to the TUI.
        result.reply_completed(p, { name = "snap-1" })
        local env = last_envelope()
        assert_eq(env.status, "completed", "save: status wrong")
        assert_eq(env.data.name, "snap-1", "save: data.name wrong")
    end)

    it("verb-specific error: SAVE_FAILED with details.raw_error",
    function()
        local p = fixture_payload("save")
        result.reply_error(p, "SAVE_FAILED",
            "Failed to write state: ENOSPC",
            { raw_error = "Failed to write state: ENOSPC" })
        local env = last_envelope()
        assert_eq(env.error.code, "SAVE_FAILED",
            "save error: code wrong")
        assert_eq(env.error.details.raw_error,
            "Failed to write state: ENOSPC",
            "save error: raw_error lost")
    end)

    it("save does NOT emit a started preamble (not restore-class)",
    function()
        -- This is a structural assertion: the test catalogue here
        -- never calls reply_started for `save`; if a future caller
        -- did, it would still produce a conforming envelope —
        -- but the per-verb catalogue explicitly excludes save from
        -- restore-class. Caught by the dispatcher, not by result.lua.
        local p = fixture_payload("save")
        result.reply_completed(p, { name = "snap-1" })
        -- One spawn = one reply; no started preamble.
        assert_eq(#bg_calls, 1, "save reply produced > 1 spawn")
    end)
end)

describe("`new` verb reply shapes", function()
    it("success: completed + data: { name, pane_id }", function()
        local p = fixture_payload("new")
        result.reply_completed(p, { name = "~/proj", pane_id = 42 })
        local env = last_envelope()
        assert_eq(env.status, "completed", "new: status wrong")
        assert_eq(env.data.name, "~/proj", "new: data.name wrong")
        assert_eq(env.data.pane_id, 42, "new: data.pane_id wrong")
    end)

    it("verb-specific error: ILLEGAL_NAME with details.field", function()
        local p = fixture_payload("new")
        result.reply_error(p, "ILLEGAL_NAME",
            "name contains control char",
            { field = "name", reason = "control char" })
        local env = last_envelope()
        assert_eq(env.error.code, "ILLEGAL_NAME",
            "new error: code wrong")
        assert_eq(env.error.details.field, "name",
            "new error: details.field wrong")
    end)
end)

describe("`noop` verb reply shapes", function()
    it("success: completed + data: {} (empty)", function()
        local p = fixture_payload("noop")
        -- For noop, the data is an empty object; result.lua normalises
        -- nil to {} so callers don't have to be careful.
        result.reply_completed(p, nil)
        local env = last_envelope()
        assert_eq(env.status, "completed", "noop: status wrong")
        assert_true(env.data ~= nil, "noop: data must be present (may be {})")
        local n = 0
        for _ in pairs(env.data) do n = n + 1 end
        assert_eq(n, 0, "noop: data must be empty object")
    end)
end)

describe("unknown verb reply shape", function()
    it("UNKNOWN_VERB → terminal completed + ok=false + error.code",
    function()
        local p = fixture_payload("bogus")
        result.reply_error(p, "UNKNOWN_VERB",
            "unknown verb: bogus", {})
        local env = last_envelope()
        -- Unknown verb: terminal `completed` reply with ok=false; does
        -- NOT degrade to noop semantics.
        assert_eq(env.status, "completed", "unknown verb: status wrong")
        assert_eq(env.ok, false, "unknown verb: ok must be false")
        assert_eq(env.error.code, "UNKNOWN_VERB",
            "unknown verb: code wrong")
        assert_nil(env.data, "unknown verb: data must NOT be present")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- HIGH fix — empty `data` / `details` must serialise as `{}` (object),
-- empty `warnings` must serialise as `[]` (array). Production mlua's
-- `wezterm.json_encode` would emit `[]` for any empty Lua table,
-- which the Go reply decoder cannot unmarshal into
-- `Data map[string]any` / `Error.Details map[string]any`. Substring
-- matching on the raw JSON bytes is intentional — the existing
-- `json_parse_shim` round-trip would obscure the `{}` vs `[]`
-- distinction we are asserting at the wire level.
-- ────────────────────────────────────────────────────────────────────

describe("empty data/details serialise as object {}",
function()
    it("noop's empty data is '{}', not '[]'", function()
        result.reply_completed(fixture_payload("noop"), nil)
        local _, argv = last_envelope()
        local json = b64.decode(parse_argv(argv).b64)
        assert_true(json:find('"data":{}', 1, true) ~= nil,
            "expected '\"data\":{}' in envelope, got: " .. json)
        assert_false(json:find('"data":[]', 1, true),
            "must NOT serialise empty data as '[]'")
    end)

    it("error's empty details is '{}', not '[]'", function()
        result.reply_error(fixture_payload(), "X", "msg", nil)
        local _, argv = last_envelope()
        local json = b64.decode(parse_argv(argv).b64)
        assert_true(json:find('"details":{}', 1, true) ~= nil,
            "expected '\"details\":{}' in envelope, got: " .. json)
        assert_false(json:find('"details":[]', 1, true),
            "must NOT serialise empty details as '[]'")
    end)

    it("partial's empty warnings is '[]', not '{}'", function()
        result.reply_partial(fixture_payload(), nil, nil)
        local _, argv = last_envelope()
        local json = b64.decode(parse_argv(argv).b64)
        assert_true(json:find('"warnings":%[%]') ~= nil,
            "expected '\"warnings\":[]' in envelope, got: " .. json)
        assert_false(json:find('"warnings":{}', 1, true),
            "must NOT serialise empty warnings as '{}'")
    end)

    it("partial's empty data is '{}', not '[]'", function()
        result.reply_partial(fixture_payload(), nil, nil)
        local _, argv = last_envelope()
        local json = b64.decode(parse_argv(argv).b64)
        assert_true(json:find('"data":{}', 1, true) ~= nil,
            "expected '\"data\":{}' in envelope, got: " .. json)
    end)

    it("UNKNOWN_VERB error has '\"details\":{}' on the wire", function()
        local p = fixture_payload("bogus")
        result.reply_error(p, "UNKNOWN_VERB", "unknown verb: bogus", nil)
        local _, argv = last_envelope()
        local json = b64.decode(parse_argv(argv).b64)
        assert_true(json:find('"details":{}', 1, true) ~= nil,
            "UNKNOWN_VERB must emit '\"details\":{}', got: " .. json)
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Production result.lua requires `wezsesh.crypto.b64` (the cache-bust
-- loop in init.lua keys on the `wezsesh.` prefix). Confirm the
-- namespaced require resolves and produces a working module instance.
-- ────────────────────────────────────────────────────────────────────

describe("namespaced require('wezsesh.crypto.b64') resolves", function()
    it("require('wezsesh.crypto.b64') exposes encode + decode",
    function()
        local namespaced = require("wezsesh.crypto.b64")
        assert_true(type(namespaced) == "table",
            "wezsesh.crypto.b64 did not load")
        assert_true(type(namespaced.encode) == "function",
            "wezsesh.crypto.b64.encode missing")
        assert_true(type(namespaced.decode) == "function",
            "wezsesh.crypto.b64.decode missing")
        -- Round-trip parity sanity check.
        local s = "hello, world"
        assert_eq(namespaced.decode(namespaced.encode(s)), s,
            "wezsesh.crypto.b64 round-trip failed")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- M.toast — non-wire surface helper. pcall-wrap is the contract.
-- ────────────────────────────────────────────────────────────────────

describe("M.toast", function()
    it("nil window is a no-op (does not raise)", function()
        local ok = pcall(result.toast, nil, "hi", 1000)
        assert_true(ok, "nil window must not raise")
    end)

    it("non-string message is a no-op (does not raise)", function()
        local ok = pcall(result.toast, {}, nil, 1000)
        assert_true(ok, "nil message must not raise")
    end)

    it("a window:toast_notification raise does NOT propagate "
        .. "(pcall-wrapped)", function()
        local fake_window = {
            toast_notification = function()
                error("synthetic toast failure", 0)
            end,
        }
        local ok = pcall(result.toast, fake_window, "msg", 1000)
        assert_true(ok,
            "toast_notification raise must be swallowed")
    end)

    it("happy path: invokes window:toast_notification with the message",
    function()
        local seen = {}
        local fake_window = {
            toast_notification = function(_self, app, msg, _url, ms)
                seen.app = app
                seen.msg = msg
                seen.ms  = ms
            end,
        }
        local r = result.toast(fake_window, "hello", 2500)
        assert_eq(r, true, "toast happy path should return true")
        assert_eq(seen.app, "wezsesh", "toast app id wrong")
        assert_eq(seen.msg, "hello", "toast message lost")
        assert_eq(seen.ms, 2500, "toast ms lost")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- deep_tag covers verb-supplied untagged subtables. Verbs that hand
-- result.reply_completed a `{ outer = { { ...row... }, ... } }` shape
-- with bare untagged subtables (e.g. bootstrap's `dir_providers =
-- [{type, argv}, ...]`) trip the canonical encoder's
-- ENCODER_UNTAGGED_TABLE check unless the rebuilder walks through
-- nested containers and tags them first.
-- ────────────────────────────────────────────────────────────────────

describe("deep_tag covers nested untagged tables", function()
    it("bootstrap-style nested rows encode without raising", function()
        local p = fixture_payload("bootstrap")
        local rows = {
            { name = "alpha", path = "/tmp/alpha" },
            { name = "beta",  path = "/tmp/beta"  },
        }
        local ok = pcall(result.reply_completed, p, { dirs = rows })
        assert_true(ok,
            "nested untagged rows must not trip the canonical encoder")
        local env = last_envelope()
        assert_eq(env.status, "completed", "status wrong")
        assert_eq(env.data.dirs[1].name, "alpha",
            "row 1 name lost")
        assert_eq(env.data.dirs[2].path, "/tmp/beta",
            "row 2 path lost")
    end)

    it("deeply nested empty subtables default to object on the wire",
    function()
        local p = fixture_payload("noop")
        local ok = pcall(result.reply_completed, p,
            { outer = { inner = {} } })
        assert_true(ok, "deep empty subtables must encode")
        local _, argv = last_envelope()
        local json = b64.decode(parse_argv(argv).b64)
        assert_true(json:find('"inner":{}', 1, true) ~= nil,
            "expected '\"inner\":{}' in envelope, got: " .. json)
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Encoder-failure recovery — a programmer-error value (e.g., a float
-- in `data`) makes the canonical encoder raise. The recovery path
-- builds a synthetic REPLY_ENCODE_FAILED envelope so the TUI sees a
-- structured error instead of waiting out the IPC timeout.
-- ────────────────────────────────────────────────────────────────────

describe("encoder failure triggers REPLY_ENCODE_FAILED fallback",
function()
    it("a float in data falls back to REPLY_ENCODE_FAILED", function()
        local p = fixture_payload("noop")
        -- Lua 5.4 splits ints from floats; an explicit float (3.14)
        -- is rejected by the canonical encoder via
        -- ENCODER_FLOAT_REJECTED, which deep_tag won't catch (it
        -- doesn't recurse into number leaves).
        local ok = pcall(result.reply_completed, p,
            { ratio = 3.14 })
        assert_true(ok,
            "encoder failure must NOT propagate out of reply_completed")
        local env = last_envelope()
        assert_eq(env.status, "completed",
            "fallback envelope: status wrong")
        assert_eq(env.ok, false,
            "fallback envelope: ok must be false")
        assert_true(env.error ~= nil,
            "fallback envelope: error must be present")
        assert_eq(env.error.code, "REPLY_ENCODE_FAILED",
            "fallback envelope: code wrong")
        -- v + id are echoed from the request so the binary's reply
        -- correlator can match it back to the in-flight call.
        assert_eq(env.v, 2, "fallback envelope: v not echoed")
        assert_eq(env.id, p.id, "fallback envelope: id not echoed")
    end)

    it("recovery envelope itself is a single spawn (no double-reply)",
    function()
        local p = fixture_payload("noop")
        result.reply_completed(p, { ratio = 3.14 })
        assert_eq(#bg_calls, 1,
            "recovery path must not produce more than one spawn")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- HMAC signing — the reply path is now authenticated symmetrically
-- with the forward path. Every reply_* must emit a top-level `"hmac"`
-- field carrying the lowercase-hex HMAC-SHA-256 of the canonical-JSON
-- sans-hmac form, computed against the session key.
--
-- The Go-side parseReply (internal/ipcdispatcher/dispatcher.go) calls
-- Signer.Verify on the reply map; a mismatch produces a synthesised
-- REPLY_HMAC_MISMATCH terminal reply on the consumer channel. The
-- locked-step round-trip below (extract → strip → re-encode → compute)
-- is exactly what Verify does on the Go side; byte-equality of the
-- digest is the load-bearing contract.
-- ────────────────────────────────────────────────────────────────────

describe("HMAC signing on the reply path", function()
    local hmac_module = require("wezsesh.crypto.hmac")
    local cj = require("wezsesh.canonical_json")

    it("reply_started emits an hmac field", function()
        result.reply_started(fixture_payload())
        local json = last_wire_json()
        assert_hmac_present(json)
    end)

    it("reply_completed emits an hmac field", function()
        result.reply_completed(fixture_payload(), { foo = "bar" })
        local json = last_wire_json()
        assert_hmac_present(json)
    end)

    it("reply_partial emits an hmac field", function()
        result.reply_partial(fixture_payload(), { name = "x" }, {{
            code = "RESURRECT_PARTIAL", message = "y", details = {},
        }})
        local json = last_wire_json()
        assert_hmac_present(json)
    end)

    it("reply_error emits an hmac field", function()
        result.reply_error(fixture_payload(), "X", "msg", {})
        local json = last_wire_json()
        assert_hmac_present(json)
    end)

    it("REPLY_ENCODE_FAILED recovery envelope is also signed", function()
        -- A float in data triggers the recovery path. The recovery
        -- envelope MUST be signed too, otherwise the TUI's HMAC verify
        -- would reject the recovery and turn one bad-encoder bug into
        -- a silent IPC timeout instead of a clear error.
        result.reply_completed(fixture_payload("noop"), { ratio = 3.14 })
        local json = last_wire_json()
        assert_hmac_present(json)
    end)

    it("emitted hmac verifies against the signing key", function()
        -- Round-trip: extract the wire hmac, drop it, re-encode the
        -- envelope canonically sans-hmac, recompute via hmac.compute,
        -- and assert byte-equality. This pins the contract that the
        -- Go-side Signer.Verify will accept the same bytes.
        result.reply_completed(fixture_payload("noop"),
            { active_workspace = "main" })
        local json = last_wire_json()
        local supplied = assert_hmac_present(json)

        -- Reconstruct a tagged Lua envelope from the wire bytes. The
        -- json_parse_shim returns plain tables; re-tag via the same
        -- deep_tag idiom result.lua uses so canonical_json.encode
        -- accepts them.
        local parsed = json_parse_shim(json)
        local function deep_tag(t)
            if type(t) ~= "table" then return t end
            local mt = getmetatable(t)
            local tag = mt and mt.__wezsesh_canonical
            if tag == "null" then return t end
            if tag == nil then
                local n = 0
                local all_int = true
                for k in pairs(t) do
                    if type(k) ~= "number" or k ~= math.floor(k) or k < 1 then
                        all_int = false
                        break
                    end
                    n = n + 1
                end
                if all_int and n > 0 then
                    setmetatable(t, cj.array_mt)
                    tag = "array"
                else
                    setmetatable(t, cj.object_mt)
                    tag = "object"
                end
            end
            if tag == "array" then
                for i = 1, #t do deep_tag(t[i]) end
            else
                for _, v in pairs(t) do
                    if type(v) == "table" then deep_tag(v) end
                end
            end
            return t
        end
        deep_tag(parsed)

        -- Drop the hmac key (preserves metatable) and re-encode.
        local sans = cj.copy_without(parsed, "hmac")
        local sans_bytes = cj.encode(sans)
        local recomputed = hmac_module.compute(sans_bytes, FIXTURE_KEY_HEX)
        assert_eq(supplied, recomputed,
            "wire hmac does not match recompute over canonical sans-hmac form")

        -- Verify symmetrically too — hmac.verify is the inverse, and
        -- both Lua and Go agree on constant-time compare.
        assert_true(
            hmac_module.verify(sans_bytes, FIXTURE_KEY_HEX, supplied),
            "hmac.verify rejected the round-tripped digest")
    end)

    it("missing session key triggers the recovery envelope path",
    function()
        global_store.wezsesh_session_key = nil
        -- With no key, sign_envelope returns nil and routes to the
        -- recovery envelope; the recovery envelope itself also wants
        -- a session key, so the spawn is the silent-noop floor.
        local ok = pcall(result.reply_started, fixture_payload())
        assert_true(ok, "missing session key must not propagate")
        assert_eq(#bg_calls, 0,
            "missing session key must not produce a signed spawn")
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
