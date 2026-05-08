-- Busted-style spec for providers/zoxide.lua. Self-contained — runs
-- under plain `lua plugin/wezsesh/providers/zoxide_spec.lua` from the
-- repo root.
--
-- Run:
--     cd <repo-root>
--     lua plugin/wezsesh/providers/zoxide_spec.lua
--
-- Exits 0 with `OK N/N` on success, 1 with FAIL lines on stderr
-- otherwise.
--
-- The spec installs a wezterm-shim via `package.preload["wezterm"]`
-- BEFORE requiring the module under test. The shim exposes a settable
-- `run_child_process` slot and a recording log surface so the spec can
-- script the binary's behaviour per-test.

local function script_dir()
    local src = arg and arg[0]
        or "plugin/wezsesh/providers/zoxide_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
local function parent_dir(p)
    return p:match("^(.*)/[^/]+$") or "."
end
local SPEC_DIR = script_dir()                  -- plugin/wezsesh/providers
local PARENT_DIR = parent_dir(SPEC_DIR)        -- plugin/wezsesh
local GRANDPARENT_DIR = parent_dir(PARENT_DIR) -- plugin
package.path = SPEC_DIR .. "/?.lua;"
            .. PARENT_DIR .. "/?.lua;"
            .. GRANDPARENT_DIR .. "/?.lua;"
            .. GRANDPARENT_DIR .. "/?/init.lua;"
            .. package.path

local helpers = require("spec_helpers")
local deepcopy = helpers.deepcopy

-- Per-test mutable state.
local rcp_calls
local rcp_handler
local log_warns

local wezterm_shim = {
    GLOBAL = setmetatable({}, {
        __index = function() return nil end,
        __newindex = function() end,
    }),
    run_child_process = function(argv)
        rcp_calls[#rcp_calls + 1] = deepcopy(argv)
        if rcp_handler then return rcp_handler(argv) end
        return true, "", ""
    end,
    log_warn = function(msg)
        log_warns[#log_warns + 1] = tostring(msg)
    end,
    log_error = function(msg)
        log_warns[#log_warns + 1] = "ERR: " .. tostring(msg)
    end,
}
package.preload["wezterm"] = function() return wezterm_shim end

local zoxide = require("wezsesh.providers.zoxide")

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
    rcp_calls = {}
    rcp_handler = nil
    log_warns = {}
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
    it("is callable as zoxide() and as zoxide.new()", function()
        local p1 = zoxide()
        local p2 = zoxide.new()
        assert_eq(type(p1), "function", "zoxide() did not return fn")
        assert_eq(type(p2), "function", "zoxide.new() did not return fn")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- happy path: parses zoxide output into rows
-- ────────────────────────────────────────────────────────────────────

describe("parses zoxide output", function()
    it("turns each line into a {path, name} row, name = basename(path)",
    function()
        rcp_handler = function(_argv)
            return true,
                "/home/u/proj\n/srv\n/var/data/work\n",
                ""
        end
        local p = zoxide.new()
        local rows = p("")
        assert_eq(#rows, 3, "row count")
        assert_eq(rows[1].path, "/home/u/proj", "row 1 path")
        assert_eq(rows[1].name, "proj", "row 1 basename")
        assert_eq(rows[2].path, "/srv", "row 2 path")
        assert_eq(rows[2].name, "srv", "row 2 basename")
        assert_eq(rows[3].path, "/var/data/work", "row 3 path")
        assert_eq(rows[3].name, "work", "row 3 basename")
    end)

    it("strips a trailing slash before computing the basename",
    function()
        rcp_handler = function(_argv)
            return true, "/home/u/proj/\n", ""
        end
        local rows = zoxide.new()("")
        assert_eq(#rows, 1, "row count")
        assert_eq(rows[1].name, "proj",
            "trailing slash leaked into basename")
    end)

    it("skips empty lines", function()
        rcp_handler = function(_argv)
            return true, "\n\n/a\n\n/b\n\n", ""
        end
        local rows = zoxide.new()("")
        assert_eq(#rows, 2, "blank lines leaked through")
        assert_eq(rows[1].path, "/a", "row 1")
        assert_eq(rows[2].path, "/b", "row 2")
    end)

    it("invokes zoxide via $SHELL -c so the user's PATH is honoured",
    function()
        -- Going through `$SHELL -c` is load-bearing: macOS GUI
        -- wezterm.app inherits launchd's minimal PATH, and the user's
        -- zoxide install (Nix profile, Homebrew, …) lives off it.
        -- `$SHELL -c` sources the user's shell init (e.g. Nix-darwin's
        -- `/etc/zshenv`) which enriches PATH before the command runs.
        os.getenv = function(k)  -- luacheck: ignore
            if k == "SHELL" then return "/usr/bin/zsh" end
            return nil
        end
        rcp_handler = function(_argv)
            return true, "/x\n", ""
        end
        zoxide.new()("anything")
        assert_eq(#rcp_calls, 1, "rcp call count")
        local argv = rcp_calls[1]
        assert_eq(argv[1], "/usr/bin/zsh", "shell")
        assert_eq(argv[2], "-c", "shell -c flag")
        assert_eq(argv[3], "zoxide query -l", "command line")
    end)

    it("falls back to /bin/sh when SHELL is unset", function()
        os.getenv = function(_k) return nil end  -- luacheck: ignore
        rcp_handler = function(_argv) return true, "", "" end
        zoxide.new()("")
        assert_eq(rcp_calls[1][1], "/bin/sh",
            "SHELL-unset fallback")
    end)

    it("opts.binary is interpolated into the shell command line",
    function()
        os.getenv = function(k)  -- luacheck: ignore
            if k == "SHELL" then return "/bin/zsh" end
            return nil
        end
        rcp_handler = function(_argv) return true, "", "" end
        zoxide.new({ binary = "/opt/zo/bin/zoxide" })("")
        assert_eq(rcp_calls[1][3], "/opt/zo/bin/zoxide query -l",
            "binary override not honoured")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- limit cap
-- ────────────────────────────────────────────────────────────────────

describe("limit cap", function()
    it("default limit is 200; rows beyond are dropped", function()
        local lines = {}
        for i = 1, 250 do lines[i] = "/p" .. i end
        rcp_handler = function(_argv)
            return true, table.concat(lines, "\n") .. "\n", ""
        end
        local rows = zoxide.new()("")
        assert_eq(#rows, 200, "default limit not applied")
    end)

    it("opts.limit overrides the default", function()
        local lines = {}
        for i = 1, 50 do lines[i] = "/p" .. i end
        rcp_handler = function(_argv)
            return true, table.concat(lines, "\n") .. "\n", ""
        end
        local rows = zoxide.new({ limit = 10 })("")
        assert_eq(#rows, 10, "explicit limit not applied")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- failure modes degrade cleanly
-- ────────────────────────────────────────────────────────────────────

describe("failure modes degrade cleanly", function()
    it("missing-binary (success=false) returns {} and logs a warn",
    function()
        rcp_handler = function(_argv)
            return false, "", "zoxide: command not found"
        end
        local rows = zoxide.new()("")
        assert_eq(#rows, 0, "expected empty rows on failure")
        assert_true(any_warn_contains("exited non-zero"),
            "missing-binary warn not logged")
    end)

    it("run_child_process raise returns {} and logs a warn", function()
        rcp_handler = function(_argv)
            error("synthetic raise from rcp", 0)
        end
        local rows = zoxide.new()("")
        assert_eq(#rows, 0, "expected empty rows on raise")
        assert_true(any_warn_contains("raised"),
            "raise was not logged")
    end)

    it("absent run_child_process API returns {} and logs a warn",
    function()
        local saved = wezterm_shim.run_child_process
        wezterm_shim.run_child_process = nil
        local rows = zoxide.new()("")
        assert_eq(#rows, 0, "expected empty rows when API missing")
        assert_true(any_warn_contains("run_child_process"),
            "missing-API warn not logged")
        wezterm_shim.run_child_process = saved
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
