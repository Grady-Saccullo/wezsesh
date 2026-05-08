-- Busted-style spec for verbs/bootstrap.lua. Self-contained.
--
-- Run:
--     cd <repo-root>
--     lua plugin/wezsesh/verbs/bootstrap_spec.lua

local function script_dir()
    local src = arg and arg[0] or "plugin/wezsesh/verbs/bootstrap_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
local function parent_dir(p) return p:match("^(.*)/[^/]+$") or "." end
local SPEC_DIR = script_dir()                          -- verbs
local PARENT_DIR = parent_dir(SPEC_DIR)                -- wezsesh
local GRANDPARENT_DIR = parent_dir(PARENT_DIR)         -- plugin
package.path = SPEC_DIR .. "/?.lua;"
            .. PARENT_DIR .. "/?.lua;"
            .. GRANDPARENT_DIR .. "/?.lua;"
            .. GRANDPARENT_DIR .. "/?/init.lua;"
            .. package.path

-- ────────────────────────────────────────────────────────────────────
-- wezterm shim — installed BEFORE require("wezsesh.verbs.bootstrap").
-- bootstrap.lua transitively requires result.lua → canonical_json.lua,
-- both of which need a wezterm shim. We capture every
-- background_child_process call so we can decode the b64 reply payload
-- and assert on the envelope shape.
-- ────────────────────────────────────────────────────────────────────

local function deepcopy(v)
    if type(v) ~= "table" then return v end
    local out = {}
    for k, val in pairs(v) do out[k] = deepcopy(val) end
    return setmetatable(out, getmetatable(v))
end

local FIXTURE_KEY_HEX =
    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
local FIXTURE_PLUGIN_SESSION_ID = "01JTESTPLUGIN_____________"

local bg_calls = {}
local global_store = {
    wezsesh_bin_path          = "/usr/local/bin/wezsesh",
    wezsesh_session_key       = FIXTURE_KEY_HEX,
    wezsesh_plugin_session_id = FIXTURE_PLUGIN_SESSION_ID,
}
local global_proxy = setmetatable({}, {
    __index    = function(_, k) return deepcopy(global_store[k]) end,
    __newindex = function(_, k, v) global_store[k] = deepcopy(v) end,
})

local wezterm_shim = {
    GLOBAL = global_proxy,
    background_child_process = function(argv)
        bg_calls[#bg_calls + 1] = deepcopy(argv)
        return true
    end,
}
package.preload["wezterm"] = function() return wezterm_shim end

local b64            = require("wezsesh.crypto.b64")
local cj             = require("wezsesh.canonical_json")
local bootstrap      = require("wezsesh.verbs.bootstrap")
local bootstrap_body = require("wezsesh.runtime.bootstrap_body")

-- ────────────────────────────────────────────────────────────────────
-- minimal harness
-- ────────────────────────────────────────────────────────────────────

local failures, total = 0, 0
local current = "<top>"
local function describe(name, fn)
    local prev = current
    current = name
    fn()
    current = prev
end
local function reset_state()
    bg_calls = {}
    global_store.wezsesh_bin_path          = "/usr/local/bin/wezsesh"
    global_store.wezsesh_session_key       = FIXTURE_KEY_HEX
    global_store.wezsesh_plugin_session_id = FIXTURE_PLUGIN_SESSION_ID
    bootstrap_body._reset()
end
local function it(name, fn)
    total = total + 1
    reset_state()
    local ok, err = pcall(fn)
    if not ok then
        failures = failures + 1
        io.stderr:write(string.format("FAIL [%s] %s\n  %s\n",
            current, name, tostring(err)))
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

local function fixture_payload()
    return {
        v                 = 2,
        id                = "01JABCDEFGHJKMNPQRSTVWXYZA",
        ts                = 1700000000,
        target_window_id  = -1,
        reply_sock        = "/tmp/x.sock",
        op                = "bootstrap",
        binary_session_id = "01JABCDEFGHJKMNPQRSTVWXYZB",
        args              = {},
        hmac              = string.rep("0", 64),
    }
end

local function decode_envelope()
    assert_true(#bg_calls >= 1,
        "expected at least one background_child_process call")
    local argv = bg_calls[#bg_calls]
    local b64s = argv[#argv]
    local bytes = b64.decode(b64s)
    assert_true(type(bytes) == "string" and #bytes > 0,
        "reply b64 did not decode")
    return bytes
end

-- ────────────────────────────────────────────────────────────────────
-- module surface
-- ────────────────────────────────────────────────────────────────────

describe("bootstrap module", function()
    it("exposes args_shape and dispatch", function()
        assert_eq(type(bootstrap.args_shape), "table",
            "args_shape missing or not a table")
        assert_eq(bootstrap.args_shape._shape, "object",
            "args_shape._shape != \"object\"")
        assert_eq(type(bootstrap.dispatch), "function",
            "dispatch missing or not a function")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- dispatch behaviour
-- ────────────────────────────────────────────────────────────────────

describe("dispatch with stashed body", function()
    it("echoes the stashed body as completed reply data", function()
        bootstrap_body.set({
            snapshot_dir  = "/sd",
            log_level     = "info",
            dir_providers = cj.array({}),
        })
        bootstrap.dispatch(fixture_payload(), nil, nil)
        local bytes = decode_envelope()
        assert_true(bytes:find('"status":"completed"', 1, true) ~= nil,
            "reply status not completed")
        assert_true(bytes:find('"ok":true', 1, true) ~= nil,
            "reply ok not true")
        assert_true(bytes:find('"snapshot_dir":"/sd"', 1, true) ~= nil,
            "snapshot_dir missing from reply data: " .. bytes)
        assert_true(bytes:find('"dir_providers":%[%]') ~= nil,
            "dir_providers not emitted as empty array: " .. bytes)
    end)
end)

describe("dispatch with empty stash", function()
    it("replies completed with empty data", function()
        bootstrap_body._reset()
        bootstrap.dispatch(fixture_payload(), nil, nil)
        local bytes = decode_envelope()
        assert_true(bytes:find('"status":"completed"', 1, true) ~= nil,
            "reply status not completed")
        assert_true(bytes:find('"data":{}', 1, true) ~= nil,
            "reply data not empty object: " .. bytes)
    end)
end)

if failures == 0 then
    io.write(string.format("OK %d/%d\n", total, total))
    os.exit(0)
else
    io.stderr:write(string.format("FAILED %d/%d\n", failures, total))
    os.exit(1)
end
