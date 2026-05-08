-- Busted-style spec for runtime/bootstrap_body.lua. Self-contained.
--
-- Run:
--     cd <repo-root>
--     lua plugin/wezsesh/runtime/bootstrap_body_spec.lua

local function script_dir()
    local src = arg and arg[0]
        or "plugin/wezsesh/runtime/bootstrap_body_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
local function parent_dir(p) return p:match("^(.*)/[^/]+$") or "." end
local SPEC_DIR = script_dir()                             -- runtime
local PARENT_DIR = parent_dir(SPEC_DIR)                   -- wezsesh
local GRANDPARENT_DIR = parent_dir(PARENT_DIR)            -- plugin
package.path = SPEC_DIR .. "/?.lua;"
            .. PARENT_DIR .. "/?.lua;"
            .. GRANDPARENT_DIR .. "/?.lua;"
            .. GRANDPARENT_DIR .. "/?/init.lua;"
            .. package.path

-- runtime/bootstrap_body.lua has no external requires beyond plain
-- Lua, so no wezterm shim is needed here.

local bootstrap_body = require("wezsesh.runtime.bootstrap_body")

local failures, total = 0, 0
local current = "<top>"
local function describe(name, fn)
    local prev = current
    current = name
    fn()
    current = prev
end
local function it(name, fn)
    total = total + 1
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
local function assert_nil(v, msg)
    if v ~= nil then error(msg or "expected nil", 2) end
end

describe("bootstrap_body", function()
    it("returns nil before any set", function()
        bootstrap_body._reset()
        assert_nil(bootstrap_body.get(), "expected nil from fresh stash")
    end)

    it("set then get returns the same table reference", function()
        bootstrap_body._reset()
        local b = { snapshot_dir = "/sd" }
        bootstrap_body.set(b)
        assert_eq(bootstrap_body.get(), b,
            "get did not return the same table reference")
    end)

    it("set replaces the previous body", function()
        bootstrap_body._reset()
        bootstrap_body.set({ snapshot_dir = "/old" })
        bootstrap_body.set({ snapshot_dir = "/new" })
        local b = bootstrap_body.get()
        assert_eq(b.snapshot_dir, "/new",
            "second set did not replace first")
    end)

    it("_reset clears the stash to nil", function()
        bootstrap_body.set({ snapshot_dir = "/sd" })
        bootstrap_body._reset()
        assert_nil(bootstrap_body.get(), "stash not cleared")
    end)
end)

if failures == 0 then
    io.write(string.format("OK %d/%d\n", total, total))
    os.exit(0)
else
    io.stderr:write(string.format("FAILED %d/%d\n", failures, total))
    os.exit(1)
end
