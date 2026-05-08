-- Busted-style spec for runtime/dir_providers.lua. Self-contained.
--
-- Run:
--     cd <repo-root>
--     lua plugin/wezsesh/runtime/dir_providers_spec.lua

local function script_dir()
    local src = arg and arg[0]
        or "plugin/wezsesh/runtime/dir_providers_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
local function parent_dir(p)
    return p:match("^(.*)/[^/]+$") or "."
end
local SPEC_DIR = script_dir()                  -- plugin/wezsesh/runtime
local PARENT_DIR = parent_dir(SPEC_DIR)        -- plugin/wezsesh
local GRANDPARENT_DIR = parent_dir(PARENT_DIR) -- plugin
package.path = SPEC_DIR .. "/?.lua;"
            .. PARENT_DIR .. "/?.lua;"
            .. GRANDPARENT_DIR .. "/?.lua;"
            .. GRANDPARENT_DIR .. "/?/init.lua;"
            .. package.path

local helpers = require("spec_helpers")

-- Capture log output so we can assert that bad entries are surfaced
-- with a logged warn rather than silently swallowed.
local log_warns
local log_errors

local wezterm_shim = helpers.minimal_wezterm()
-- runtime/log.lua JSON-encodes a structured record before emitting.
local _codec = helpers.make_json_codec()
wezterm_shim.json_encode = _codec.encode
wezterm_shim.json_parse  = _codec.decode
wezterm_shim.log_warn = function(msg)
    log_warns[#log_warns + 1] = tostring(msg)
end
wezterm_shim.log_error = function(msg)
    log_errors[#log_errors + 1] = tostring(msg)
end
package.preload["wezterm"] = function() return wezterm_shim end

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
    log_warns = {}
    log_errors = {}
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

local function any_warn_contains(needle)
    for _, m in ipairs(log_warns) do
        if m:find(needle, 1, true) ~= nil then return true end
    end
    return false
end

-- ────────────────────────────────────────────────────────────────────
-- module surface
-- ────────────────────────────────────────────────────────────────────

describe("module surface", function()
    it("exposes set, get, _reset", function()
        assert_eq(type(dir_providers.set), "function", "set missing")
        assert_eq(type(dir_providers.get), "function", "get missing")
        assert_eq(type(dir_providers._reset), "function",
            "_reset missing")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- empty default
-- ────────────────────────────────────────────────────────────────────

describe("empty default", function()
    it("get returns an empty list when set was never called", function()
        local list = dir_providers.get()
        assert_eq(type(list), "table", "get return type")
        assert_eq(#list, 0, "default empty list")
    end)

    it("set(nil) normalises to empty list", function()
        dir_providers.set(nil)
        assert_eq(#dir_providers.get(), 0, "nil not normalised")
    end)

    it("set(non-table) logs a warn and clears", function()
        dir_providers.set("not a list")
        assert_eq(#dir_providers.get(), 0, "non-table not normalised")
        assert_true(any_warn_contains("expected table"),
            "non-table set did not log a warn")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- set + get round-trip (declarative configs)
-- ────────────────────────────────────────────────────────────────────

describe("set + get round-trip", function()
    it("preserves the list of configs in registration order", function()
        local cfg1 = { type = "command", argv = { "zoxide", "query", "-l" } }
        local cfg2 = { type = "directory", path = "~/code", depth = 2 }
        dir_providers.set({ cfg1, cfg2 })
        local got = dir_providers.get()
        assert_eq(#got, 2, "list length")
        assert_eq(got[1].type, "command", "first slot type")
        assert_eq(got[1].argv[1], "zoxide", "first slot argv")
        assert_eq(got[2].type, "directory", "second slot type")
        assert_eq(got[2].path, "~/code", "second slot path")
    end)

    it("subsequent caller mutations to the passed-in list do not "
        .. "leak into the stashed snapshot", function()
        local list = { { type = "static", paths = { "/a" } } }
        dir_providers.set(list)
        list[2] = { type = "static", paths = { "/b" } }
        assert_eq(#dir_providers.get(), 1,
            "post-set caller mutation leaked into stash")
    end)

    it("subsequent mutation of a nested table (argv / paths) does NOT "
        .. "leak into the stashed snapshot", function()
        local argv = { "zoxide", "query", "-l" }
        dir_providers.set({ { type = "command", argv = argv } })
        argv[1] = "MUTATED"
        local got = dir_providers.get()
        assert_eq(got[1].argv[1], "zoxide",
            "nested-array mutation leaked into stash")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- entry validation
-- ────────────────────────────────────────────────────────────────────

describe("entry validation", function()
    it("non-table entry is logged and dropped", function()
        dir_providers.set({
            "not a table",
            { type = "static", paths = { "/keep" } },
        })
        local got = dir_providers.get()
        assert_eq(#got, 1, "good entry dropped")
        assert_eq(got[1].type, "static", "good entry mis-routed")
        assert_true(any_warn_contains("expected table"),
            "non-table entry was not logged")
    end)

    it("entry missing string `type` is logged and dropped", function()
        dir_providers.set({
            { argv = { "zoxide" } }, -- missing type
            { type = "static", paths = { "/keep" } },
        })
        local got = dir_providers.get()
        assert_eq(#got, 1, "good entry dropped")
        assert_eq(got[1].type, "static", "good entry mis-routed")
        assert_true(any_warn_contains("missing string `type`"),
            "missing-type entry was not logged")
    end)

    it("empty-string `type` is rejected", function()
        dir_providers.set({
            { type = "" },
            { type = "static", paths = { "/keep" } },
        })
        local got = dir_providers.get()
        assert_eq(#got, 1, "good entry dropped")
    end)

    it("non-string `type` is rejected", function()
        dir_providers.set({
            { type = 42 },
            { type = "static", paths = { "/keep" } },
        })
        local got = dir_providers.get()
        assert_eq(#got, 1, "good entry dropped")
    end)
end)

if failures == 0 then
    io.write(string.format("OK %d/%d\n", total, total))
    os.exit(0)
else
    io.stderr:write(string.format("FAILED %d/%d\n", failures, total))
    os.exit(1)
end
