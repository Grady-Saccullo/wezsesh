-- Busted-style spec for verbs/list_dirs.lua. Self-contained — runs
-- under plain `lua plugin/wezsesh/verbs/list_dirs_spec.lua` from the
-- repo root.
--
-- Run:
--     cd <repo-root>
--     lua plugin/wezsesh/verbs/list_dirs_spec.lua
--
-- Exits 0 with `OK N/N` on success, 1 with FAIL lines on stderr
-- otherwise.
--
-- The spec installs a wezterm-shim via `package.preload["wezterm"]`
-- BEFORE requiring the module under test. The shim records every
-- `wezterm.background_child_process` invocation in `bg_calls` so the
-- spec can decode the b64 reply payload and assert on the reply
-- envelope shape.

local function script_dir()
    local src = arg and arg[0] or "plugin/wezsesh/verbs/list_dirs_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
local function parent_dir(p)
    return p:match("^(.*)/[^/]+$") or "."
end
local SPEC_DIR = script_dir()                  -- plugin/wezsesh/verbs
local PARENT_DIR = parent_dir(SPEC_DIR)        -- plugin/wezsesh
local GRANDPARENT_DIR = parent_dir(PARENT_DIR) -- plugin
package.path = SPEC_DIR .. "/?.lua;"
            .. PARENT_DIR .. "/?.lua;"
            .. GRANDPARENT_DIR .. "/?.lua;"
            .. GRANDPARENT_DIR .. "/?/init.lua;"
            .. package.path

local helpers = require("spec_helpers")
local deepcopy = helpers.deepcopy
local codec = helpers.make_json_codec()

local bg_calls
local global_store
local log_warns

local global_proxy = setmetatable({}, {
    __index = function(_, k) return deepcopy(global_store[k]) end,
    __newindex = function(_, k, v) global_store[k] = deepcopy(v) end,
})

local wezterm_shim = {
    GLOBAL = global_proxy,
    target_triple = "x86_64-unknown-linux-gnu",
    json_encode = codec.encode,
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
}
package.preload["wezterm"] = function() return wezterm_shim end

local b64           = require("wezsesh.crypto.b64")
local list_dirs     = require("wezsesh.verbs.list_dirs")
local dir_providers = require("wezsesh.runtime.dir_providers")

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
        -- result.lua now signs reply envelopes; the spec needs a
        -- session key so spawn_reply does not fall through to the
        -- silent-noop floor.
        wezsesh_session_key =
            "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    }
    dir_providers._reset()
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

local function decode_envelope(idx)
    idx = idx or #bg_calls
    assert_true(#bg_calls >= idx,
        "expected at least " .. idx .. " spawn calls, got " .. #bg_calls)
    local argv = bg_calls[idx]
    assert_eq(argv[2], "reply", "argv[2] not 'reply'")
    local json = b64.decode(argv[4])
    assert_true(json ~= nil, "argv[4] was not valid b64")
    return codec.decode(json), argv
end

local function fixture_payload(args)
    return {
        v          = 1,
        id         = "01JABCDEFGHJKMNPQRSTVWXYZA",
        ts         = 1700000000,
        op         = "list_dirs",
        reply_sock = "/tmp/wezsesh-1000/abcdef01.sock",
        args       = args or {},
    }
end

-- ────────────────────────────────────────────────────────────────────
-- module surface
-- ────────────────────────────────────────────────────────────────────

describe("module surface", function()
    it("exports {args_shape, dispatch}", function()
        assert_eq(type(list_dirs.args_shape), "table",
            "args_shape missing")
        assert_eq(type(list_dirs.dispatch), "function",
            "dispatch missing")
    end)

    it("args_shape declares { _shape = 'object', query = 'string' }",
    function()
        assert_eq(list_dirs.args_shape._shape, "object",
            "_shape not object")
        assert_eq(list_dirs.args_shape.query, "string",
            "query type wrong")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- zero providers → empty dirs
-- ────────────────────────────────────────────────────────────────────

describe("zero providers", function()
    it("replies completed + data: { dirs = {} }", function()
        list_dirs.dispatch(fixture_payload({ query = "" }))
        local env = decode_envelope()
        assert_eq(env.status, "completed", "status wrong")
        assert_eq(env.ok, true, "ok wrong")
        assert_true(env.data ~= nil, "data missing")
        assert_true(env.data.dirs ~= nil, "dirs missing")
        assert_eq(#env.data.dirs, 0, "dirs not empty")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- one provider → its rows
-- ────────────────────────────────────────────────────────────────────

describe("one provider", function()
    it("passes the provider's rows through to data.dirs", function()
        local p = function(_q)
            return {
                { path = "/srv", name = "srv" },
                { path = "/home/u/proj", name = "proj" },
            }
        end
        dir_providers.set({ p })
        list_dirs.dispatch(fixture_payload({ query = "" }))
        local env = decode_envelope()
        assert_eq(#env.data.dirs, 2, "row count wrong")
        assert_eq(env.data.dirs[1].path, "/srv", "row 1 path")
        assert_eq(env.data.dirs[1].name, "srv", "row 1 name")
        assert_eq(env.data.dirs[2].path, "/home/u/proj", "row 2 path")
        assert_eq(env.data.dirs[2].name, "proj", "row 2 name")
    end)

    it("threads the query string through to the provider", function()
        local seen
        local p = function(q)
            seen = q
            return { { path = "/x", name = "x" } }
        end
        dir_providers.set({ p })
        list_dirs.dispatch(fixture_payload({ query = "needle" }))
        assert_eq(seen, "needle", "query not threaded")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- error isolation: one bad provider leaves others intact
-- ────────────────────────────────────────────────────────────────────

describe("provider error isolation", function()
    it("a raising provider does not take down the others' rows",
    function()
        local bad = function(_q) error("boom", 0) end
        local good = function(_q)
            return { { path = "/g", name = "g" } }
        end
        dir_providers.set({ bad, good })
        list_dirs.dispatch(fixture_payload({ query = "" }))
        local env = decode_envelope()
        assert_eq(env.status, "completed", "status wrong on partial")
        assert_eq(env.ok, true,
            "ok wrong: list_dirs is best-effort, never errors")
        assert_eq(#env.data.dirs, 1, "good rows lost")
        assert_eq(env.data.dirs[1].path, "/g", "wrong row")
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
