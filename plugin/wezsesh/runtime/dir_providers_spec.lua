-- Busted-style spec for runtime/dir_providers.lua. Self-contained —
-- runs under plain `lua plugin/wezsesh/runtime/dir_providers_spec.lua`
-- from the repo root, no busted required.
--
-- Run:
--     cd <repo-root>
--     lua plugin/wezsesh/runtime/dir_providers_spec.lua
--
-- Exits 0 with `OK N/N` on success, 1 with FAIL lines on stderr
-- otherwise.
--
-- The spec installs a wezterm-shim via `package.preload["wezterm"]`
-- BEFORE requiring the module under test. dir_providers itself doesn't
-- touch wezterm, but it requires `runtime.log` which does.

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

-- Capture log output so we can assert that bad providers / rows are
-- surfaced with a logged warn rather than silently swallowed.
local log_warns
local log_errors

local wezterm_shim = helpers.minimal_wezterm()
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
    it("exposes set, get, invoke_all, _reset", function()
        assert_eq(type(dir_providers.set), "function", "set missing")
        assert_eq(type(dir_providers.get), "function", "get missing")
        assert_eq(type(dir_providers.invoke_all), "function",
            "invoke_all missing")
        assert_eq(type(dir_providers._reset), "function",
            "_reset missing")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- empty default
-- ────────────────────────────────────────────────────────────────────

describe("empty default", function()
    it("invoke_all returns {} when set was never called", function()
        local rows = dir_providers.invoke_all("")
        assert_eq(type(rows), "table", "invoke_all return type")
        assert_eq(#rows, 0, "default empty contract")
    end)

    it("get returns an empty list when set was never called", function()
        local list = dir_providers.get()
        assert_eq(type(list), "table", "get return type")
        assert_eq(#list, 0, "default empty list")
    end)

    it("set(nil) normalises to empty list", function()
        dir_providers.set(nil)
        assert_eq(#dir_providers.get(), 0, "nil not normalised")
        assert_eq(#dir_providers.invoke_all(""), 0,
            "invoke_all after set(nil)")
    end)

    it("set(non-table) logs a warn and clears", function()
        dir_providers.set("not a list")
        assert_eq(#dir_providers.get(), 0, "non-table not normalised")
        assert_true(any_warn_contains("expected table"),
            "non-table set did not log a warn")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- set + get round-trip
-- ────────────────────────────────────────────────────────────────────

describe("set + get round-trip", function()
    it("preserves the list of providers in registration order",
    function()
        local function p1() return {} end
        local function p2() return {} end
        dir_providers.set({ p1, p2 })
        local got = dir_providers.get()
        assert_eq(#got, 2, "list length")
        assert_true(got[1] == p1, "first slot identity")
        assert_true(got[2] == p2, "second slot identity")
    end)

    it("subsequent caller mutations to the passed-in list do not "
        .. "leak into the stashed snapshot", function()
        local list = { function() return {} end }
        dir_providers.set(list)
        list[2] = function() return {} end
        assert_eq(#dir_providers.get(), 1,
            "post-set caller mutation leaked into stash")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- invoke_all concatenation
-- ────────────────────────────────────────────────────────────────────

describe("invoke_all concatenation", function()
    it("concatenates rows across providers in registration order",
    function()
        local p1 = function(_q)
            return { { path = "/a", name = "a" } }
        end
        local p2 = function(_q)
            return {
                { path = "/b", name = "b" },
                { path = "/c", name = "c" },
            }
        end
        dir_providers.set({ p1, p2 })
        local rows = dir_providers.invoke_all("")
        assert_eq(#rows, 3, "concat length")
        assert_eq(rows[1].path, "/a", "row[1].path")
        assert_eq(rows[2].path, "/b", "row[2].path")
        assert_eq(rows[3].path, "/c", "row[3].path")
    end)

    it("threads the query string through to each provider", function()
        local seen
        local p = function(q)
            seen = q
            return {}
        end
        dir_providers.set({ p })
        dir_providers.invoke_all("hello")
        assert_eq(seen, "hello", "query not threaded")
    end)

    it("non-string query is normalised to empty string", function()
        local seen
        local p = function(q)
            seen = q
            return {}
        end
        dir_providers.set({ p })
        dir_providers.invoke_all(nil)
        assert_eq(seen, "", "nil query not normalised")
        dir_providers.invoke_all(42)
        assert_eq(seen, "", "non-string query not normalised")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- pcall isolation
-- ────────────────────────────────────────────────────────────────────

describe("pcall isolation", function()
    it("a raising provider does not tank the others", function()
        local bad = function(_q) error("boom", 0) end
        local good = function(_q)
            return { { path = "/g", name = "g" } }
        end
        dir_providers.set({ bad, good })
        local rows = dir_providers.invoke_all("")
        assert_eq(#rows, 1, "good provider's row dropped")
        assert_eq(rows[1].path, "/g", "good provider's row missing")
        assert_true(any_warn_contains("provider #1 raised"),
            "raise was not logged")
    end)

    it("a non-function entry is logged and skipped", function()
        local good = function(_q)
            return { { path = "/g", name = "g" } }
        end
        dir_providers.set({ "not a function", good })
        local rows = dir_providers.invoke_all("")
        assert_eq(#rows, 1, "good provider's row missing")
        assert_eq(rows[1].path, "/g", "good provider's row identity")
        assert_true(any_warn_contains("expected function"),
            "non-function entry was not logged")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- malformed return
-- ────────────────────────────────────────────────────────────────────

describe("malformed return shape", function()
    it("provider returning non-table is logged and dropped", function()
        local bad = function(_q) return "nope" end
        local good = function(_q)
            return { { path = "/g", name = "g" } }
        end
        dir_providers.set({ bad, good })
        local rows = dir_providers.invoke_all("")
        assert_eq(#rows, 1, "good provider tanked")
        assert_eq(rows[1].path, "/g", "good provider mis-routed")
        assert_true(any_warn_contains("expected table"),
            "non-table return was not logged")
    end)

    it("provider returning a row missing `path` drops THAT row only",
    function()
        local p = function(_q)
            return {
                { path = "/keep", name = "keep" },
                { name = "missing path" },
                { path = "/also-keep" },
            }
        end
        dir_providers.set({ p })
        local rows = dir_providers.invoke_all("")
        assert_eq(#rows, 2, "expected 2 valid rows; got " .. #rows)
        assert_eq(rows[1].path, "/keep", "first kept row wrong")
        assert_eq(rows[2].path, "/also-keep", "second kept row wrong")
        assert_true(any_warn_contains("malformed"),
            "malformed row was not logged")
    end)

    it("non-string `name` is normalised to nil (binary derives base)",
    function()
        local p = function(_q)
            return { { path = "/p", name = 42 } }
        end
        dir_providers.set({ p })
        local rows = dir_providers.invoke_all("")
        assert_eq(#rows, 1, "row dropped despite valid path")
        assert_eq(rows[1].path, "/p", "path lost")
        assert_eq(rows[1].name, nil,
            "non-string name not normalised; got "
            .. tostring(rows[1].name))
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
