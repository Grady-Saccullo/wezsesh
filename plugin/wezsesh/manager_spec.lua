-- Busted-style spec for manager.lua. Self-contained — runs under plain
-- `lua plugin/wezsesh/manager_spec.lua` from the repo root, no busted
-- required.
--
-- Run:
--     cd <repo-root>
--     lua plugin/wezsesh/manager_spec.lua
--
-- Exits 0 with `OK N/N` on success, 1 with FAIL lines on stderr
-- otherwise. Mirrors the structure of state_spec.lua / ops_spec.lua.
--
-- The spec installs a wezterm-shim via `package.preload["wezterm"]`
-- BEFORE requiring the module under test. The shim covers:
--   * wezterm.GLOBAL — snapshot-on-read userdata-shaped table.
--   * wezterm.run_child_process — programmable per-test response.
--   * wezterm.json_encode — pure-Lua JSON emitter (round-trip only).
--   * wezterm.target_triple / wezterm.home_dir — string fields used by
--     SUN_PATH ceiling math.
--   * wezterm.mux.spawn_window — captures invocation for assertions.
--   * wezterm.action_callback — wraps the callback in a tagged table
--     that the spec can identify.
--   * wezterm.log_error / wezterm.log_warn — recorded for assertions.
--
-- T-903 added `require("wezsesh.default_allowlist")` to manager.lua's
-- write_config_file fallback path. The spec runs with package.path
-- rooted at plugin/wezsesh/, which would resolve to a non-existent
-- `plugin/wezsesh/wezsesh/default_allowlist.lua`. Install a
-- package.preload shim so the require lands on the real list at
-- plugin/wezsesh/default_allowlist.lua.

local function script_dir()
    local src = arg and arg[0] or "plugin/wezsesh/manager_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
-- script_dir() lets bare `require("manager")` resolve to the file next
-- to this spec; script_dir()/../ lets dotted requires
-- (e.g. wezsesh.runtime.globals) resolve via plugin/wezsesh/runtime/...
package.path = script_dir() .. "/?.lua;"
            .. script_dir() .. "/../?.lua;"
            .. package.path

-- ────────────────────────────────────────────────────────────────────
-- pure-Lua JSON encode/decode for the shim and for the spec body
-- ────────────────────────────────────────────────────────────────────

-- Sentinel matching wezterm's `wezterm.json_array_metatable`. Any Lua
-- table whose metatable is this sentinel is encoded as a JSON array,
-- even when it has zero entries — mirroring the production wezterm
-- behaviour the round-2 T-903 fix relies on.
local JSON_ARRAY_METATABLE = { __wezterm_json_array = true }

local function json_encode_shim(v)
    local function emit(x)
        local t = type(x)
        if t == "number" then return tostring(x) end
        if t == "boolean" then return x and "true" or "false" end
        if t == "string" then
            return '"' .. x:gsub("[\\\"]", "\\%0") .. '"'
        end
        if t == "table" then
            local tagged_array =
                getmetatable(x) == JSON_ARRAY_METATABLE
            -- treat as array if all keys are positive integers 1..n
            local n = 0
            local is_array = true
            for k in pairs(x) do
                if type(k) ~= "number" or k ~= math.floor(k) or k < 1 then
                    is_array = false; break
                end
                n = n + 1
            end
            if tagged_array or (is_array and n > 0) then
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
    local function parse_array()
        pos = pos + 1
        skip_ws()
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
    local function parse_object()
        pos = pos + 1
        skip_ws()
        local out = {}
        if s:sub(pos, pos) == "}" then pos = pos + 1; return out end
        while true do
            skip_ws()
            local k = parse_string()
            skip_ws()
            if s:sub(pos, pos) ~= ":" then err("expected ':'") end
            pos = pos + 1
            skip_ws()
            out[k] = parse_value()
            skip_ws()
            local c = s:sub(pos, pos)
            if c == "}" then pos = pos + 1; return out end
            if c ~= "," then err("expected ',' or '}'") end
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

-- ────────────────────────────────────────────────────────────────────
-- wezterm shim
-- ────────────────────────────────────────────────────────────────────

local function deepcopy(v)
    if type(v) ~= "table" then return v end
    local out = {}
    for k, vv in pairs(v) do out[k] = deepcopy(vv) end
    return out
end

local function make_global()
    local store = {}
    local proxy = setmetatable({}, {
        __index = function(_, k) return deepcopy(store[k]) end,
        __newindex = function(_, k, v) store[k] = deepcopy(v) end,
    })
    return proxy, {
        get = function(k) return store[k] end,
        set = function(k, v) store[k] = v end,
        clear = function() for k in pairs(store) do store[k] = nil end end,
    }
end

local global, gctrl = make_global()

-- run_child_process queue: each test pushes a {ok, stdout, stderr}
-- response which the next call consumes. Calls also append a record
-- to `rcp_calls` for argv assertions.
local rcp_responses = {}
local rcp_calls = {}
local function run_child_process_shim(argv)
    rcp_calls[#rcp_calls + 1] = deepcopy(argv)
    if #rcp_responses == 0 then
        return false, "", "rcp_shim: no response queued"
    end
    local r = table.remove(rcp_responses, 1)
    return r.ok, r.stdout or "", r.stderr or ""
end

-- spawn_window / spawn_tab capture
local spawn_calls = {}
local mux_shim = {
    spawn_window = function(arg)
        spawn_calls[#spawn_calls + 1] = { mode = "window", arg = deepcopy(arg) }
        return { mock = "window" }, { mock = "pane" }, { mock = "win" }
    end,
}

-- Window/pane stubs for spawn_tab path. The mode = "tab" branch goes
-- through `window:mux_window():spawn_tab(...)`; the GUI Window
-- userdata exposes `:mux_window()` for that resolution.
-- `Pane:spawn_tab` does NOT exist in current wezterm builds.
local function make_window_stub()
    return setmetatable({}, {
        __index = function(_, k)
            if k == "mux_window" then
                return function(_self)
                    return setmetatable({}, {
                        __index = function(__, kk)
                            if kk == "spawn_tab" then
                                return function(_p, arg)
                                    spawn_calls[#spawn_calls + 1] = {
                                        mode = "tab", arg = deepcopy(arg)
                                    }
                                    return { mock = "tab" },
                                           { mock = "pane" },
                                           { mock = "win" }
                                end
                            end
                        end,
                    })
                end
            end
        end,
    })
end

-- action_callback — wraps fn in a tagged record for spec inspection.
local function action_callback_shim(fn)
    return { __wezterm_action_callback = true, fn = fn }
end

local log_calls = { warn = {}, error = {} }

local wezterm_shim = {
    GLOBAL                = global,
    json_encode           = json_encode_shim,
    json_parse            = json_parse_shim,
    json_array_metatable  = JSON_ARRAY_METATABLE,
    run_child_process     = run_child_process_shim,
    target_triple         = "x86_64-unknown-linux-gnu",
    home_dir              = "/home/tester",
    mux                   = mux_shim,
    action_callback       = action_callback_shim,
    log_warn              = function(msg) log_calls.warn[#log_calls.warn + 1] = msg end,
    log_error             = function(msg) log_calls.error[#log_calls.error + 1] = msg end,
    procinfo              = nil,  -- absent by default → seq fallback
}

package.preload["wezterm"] = function() return wezterm_shim end

-- Bridge `wezsesh.default_allowlist` (the wezterm plugin loader's
-- canonical name) to the plain `default_allowlist` module on the
-- spec-local package.path. Without this, manager.lua's
-- `require("wezsesh.default_allowlist")` fallback in write_config_file
-- raises a module-not-found error.
package.preload["wezsesh.default_allowlist"] = function()
    return require("default_allowlist")
end

-- Now load the module under test.
local manager = require("manager")

-- ────────────────────────────────────────────────────────────────────
-- harness
-- ────────────────────────────────────────────────────────────────────

local failures, total = 0, 0
local current_describe = "<top>"

local function describe(name, fn)
    local prev = current_describe
    current_describe = name
    fn()
    current_describe = prev
end

local function it(name, fn)
    total = total + 1
    -- Reset shim state between tests.
    rcp_responses = {}
    rcp_calls = {}
    spawn_calls = {}
    log_calls.warn = {}
    log_calls.error = {}
    gctrl.clear()
    wezterm_shim.target_triple = "x86_64-unknown-linux-gnu"
    wezterm_shim.home_dir = "/home/tester"
    wezterm_shim.json_array_metatable = JSON_ARRAY_METATABLE
    wezterm_shim.executable_dir = nil
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

local function assert_match(s, pattern, msg)
    if type(s) ~= "string" or s:find(pattern) == nil then
        error(string.format("%s\n   string: %s\n  pattern: %s",
            msg or "pattern mismatch", tostring(s), pattern), 2)
    end
end

-- ────────────────────────────────────────────────────────────────────
-- module surface
-- ────────────────────────────────────────────────────────────────────

describe("module surface", function()
    it("exposes the public API and a VERSION constant", function()
        local want = {
            "VERSION", "compatible", "detect_version",
            "ensure_session_key", "register_keybinding",
            "resolve_binary", "spawn", "validate_runtime_dir",
            "write_config_file",
        }
        local keys = {}
        for k in pairs(manager) do keys[#keys + 1] = k end
        table.sort(keys)
        assert_eq(table.concat(keys, ","), table.concat(want, ","),
            "manager module surface drift")
    end)

    it("exports a non-empty version string", function()
        assert_eq(type(manager.VERSION), "string", "VERSION not string")
        assert_match(manager.VERSION, "^%d+%.%d+%.%d+",
            "VERSION not semver-shaped")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- resolve_binary
-- ────────────────────────────────────────────────────────────────────

describe("resolve_binary", function()
    it("honours opts.binary when set", function()
        assert_eq(manager.resolve_binary({ binary = "/usr/bin/wezsesh" }),
            "/usr/bin/wezsesh", "explicit binary not honoured")
    end)

    it("falls back to plugin_root + /wezsesh when binary unset", function()
        assert_eq(manager.resolve_binary({ plugin_root = "/opt/wezsesh" }),
            "/opt/wezsesh/wezsesh", "plugin_root path-join wrong")
    end)

    it("strips a single trailing slash on plugin_root", function()
        assert_eq(manager.resolve_binary({ plugin_root = "/opt/wezsesh/" }),
            "/opt/wezsesh/wezsesh",
            "trailing-slash plugin_root path-join wrong")
    end)

    it("falls back to bare \"wezsesh\" when neither is set", function()
        assert_eq(manager.resolve_binary({}), "wezsesh",
            "bare PATH default not used")
        assert_eq(manager.resolve_binary(nil), "wezsesh",
            "nil opts didn't degrade to bare default")
    end)

    it("binary wins when both binary and plugin_root are set", function()
        assert_eq(manager.resolve_binary({
            binary = "/x", plugin_root = "/y",
        }), "/x", "opts.binary did not take precedence over plugin_root")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- detect_version
-- ────────────────────────────────────────────────────────────────────

describe("detect_version", function()
    it("returns 'missing' when run_child_process reports failure", function()
        rcp_responses[#rcp_responses + 1] =
            { ok = false, stdout = "", stderr = "exec failed" }
        assert_eq(manager.detect_version("/bin/wezsesh"), "missing",
            "exec-failure path didn't report 'missing'")
    end)

    it("returns 'unparseable' for non-semver output", function()
        rcp_responses[#rcp_responses + 1] =
            { ok = true, stdout = "definitely not semver\n" }
        assert_eq(manager.detect_version("/bin/wezsesh"), "unparseable",
            "non-semver didn't report 'unparseable'")
    end)

    it("returns 'unparseable' for empty output", function()
        rcp_responses[#rcp_responses + 1] =
            { ok = true, stdout = "   \n\t\n" }
        assert_eq(manager.detect_version("/bin/wezsesh"), "unparseable",
            "empty stdout didn't report 'unparseable'")
    end)

    it("returns trimmed semver for valid output", function()
        rcp_responses[#rcp_responses + 1] =
            { ok = true, stdout = "  0.1.0\n" }
        assert_eq(manager.detect_version("/bin/wezsesh"), "0.1.0",
            "trim or parse wrong")
    end)

    it("accepts pre-release / build suffixes (no inner whitespace)", function()
        rcp_responses[#rcp_responses + 1] =
            { ok = true, stdout = "1.2.3-rc1+sha.abcd\n" }
        assert_eq(manager.detect_version("/bin/wezsesh"),
            "1.2.3-rc1+sha.abcd",
            "semver-with-extras rejected")
    end)

    it("returns 'missing' for empty/nil binary path", function()
        assert_eq(manager.detect_version(""), "missing",
            "empty bin not 'missing'")
        assert_eq(manager.detect_version(nil), "missing",
            "nil bin not 'missing'")
    end)

    it("calls the binary with argv {bin, 'version'}", function()
        rcp_responses[#rcp_responses + 1] =
            { ok = true, stdout = "0.1.0" }
        manager.detect_version("/x/wezsesh")
        assert_eq(#rcp_calls, 1, "expected exactly one rcp call")
        assert_eq(rcp_calls[1][1], "/x/wezsesh", "argv[1] wrong")
        assert_eq(rcp_calls[1][2], "version", "argv[2] wrong")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- compatible
-- ────────────────────────────────────────────────────────────────────

describe("compatible", function()
    it("returns true on equal-major", function()
        assert_true(manager.compatible("0.1.0", "0.1.0"),
            "same version not compatible")
        assert_true(manager.compatible("0.1.0", "0.5.7"),
            "same major (0.x) not compatible")
        assert_true(manager.compatible("1.2.3", "1.0.0"),
            "same major (1.x) not compatible")
    end)

    it("returns false on mismatched-major", function()
        assert_false(manager.compatible("0.1.0", "1.0.0"),
            "0 vs 1 reported compatible")
        assert_false(manager.compatible("2.0.0", "1.0.0"),
            "2 vs 1 reported compatible")
    end)

    it("returns false on unparseable inputs", function()
        assert_false(manager.compatible("missing", "0.1.0"),
            "missing/0.1.0 reported compatible")
        assert_false(manager.compatible("0.1.0", "unparseable"),
            "0.1.0/unparseable reported compatible")
        assert_false(manager.compatible(nil, nil),
            "nil/nil reported compatible")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- ensure_session_key
-- ────────────────────────────────────────────────────────────────────

describe("ensure_session_key", function()
    it("calls `<bin> keygen` and stores 64hex in GLOBAL on success",
    function()
        local hex = string.rep("a", 64)
        rcp_responses[#rcp_responses + 1] =
            { ok = true, stdout = hex .. "\n" }
        local got = manager.ensure_session_key("/bin/wezsesh")
        assert_eq(got, hex, "returned key wrong")
        assert_eq(gctrl.get("wezsesh_session_key"), hex,
            "GLOBAL.wezsesh_session_key not set")
        -- argv was {bin, "keygen"}.
        assert_eq(rcp_calls[1][1], "/bin/wezsesh", "argv[1] wrong")
        assert_eq(rcp_calls[1][2], "keygen", "argv[2] wrong")
    end)

    it("rejects non-hex / wrong-length keygen output", function()
        -- 63 chars: too short → falls through to /dev/urandom.
        rcp_responses[#rcp_responses + 1] =
            { ok = true, stdout = string.rep("a", 63) .. "\n" }
        local got = manager.ensure_session_key("/bin/wezsesh")
        -- /dev/urandom is real on the host running the spec; we accept
        -- either a 64hex fallback or nil if the spec environment
        -- happens to lack /dev/urandom (unlikely on POSIX).
        assert_true(got == nil
            or (type(got) == "string" and #got == 64
                and got:match("^%x+$")),
            "non-hex keygen didn't fall back cleanly: " .. tostring(got))
    end)

    it("falls back to /dev/urandom when keygen reports failure",
    function()
        rcp_responses[#rcp_responses + 1] =
            { ok = false, stdout = "", stderr = "no /dev/urandom in container" }
        local got = manager.ensure_session_key("/bin/wezsesh")
        -- /dev/urandom is universally available on POSIX hosts the
        -- build matrix targets. Skip cleanly only if io.open fails.
        if got ~= nil then
            assert_eq(type(got), "string", "fallback type wrong")
            assert_eq(#got, 64, "fallback length wrong")
            assert_true(got:match("^%x+$") ~= nil,
                "fallback shape not hex")
            assert_eq(gctrl.get("wezsesh_session_key"), got,
                "GLOBAL not stored from fallback")
        end
    end)

    it("does NOT raise on total failure; returns nil + sentinel",
    function()
        -- Force keygen failure and patch io.open to return nil so the
        -- urandom fallback also fails. Restore on exit.
        rcp_responses[#rcp_responses + 1] =
            { ok = false, stdout = "", stderr = "boom" }
        local real_io_open = io.open
        io.open = function(_path, _mode)
            return nil, "synthetic test failure"
        end
        local ok, got, sentinel = pcall(
            manager.ensure_session_key, "/bin/wezsesh")
        io.open = real_io_open
        assert_true(ok,
            "ensure_session_key raised — would wedge apply_to_config")
        assert_nil(got, "expected nil on total failure")
        assert_eq(sentinel, "WEZSESH_SESSION_KEY_GENERATION_FAILED",
            "sentinel wrong on total failure")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- validate_runtime_dir
-- ────────────────────────────────────────────────────────────────────

describe("validate_runtime_dir", function()
    it("raises WEZSESH_RUNTIME_DIR_TYPE for non-string input", function()
        local ok, err = pcall(manager.validate_runtime_dir,
            { runtime_dir = 42 })
        assert_false(ok, "expected raise on numeric runtime_dir")
        assert_match(tostring(err), "WEZSESH_RUNTIME_DIR_TYPE",
            "wrong sentinel for type error")
    end)

    it("raises WEZSESH_SUN_PATH_OVERFLOW when needed > ceiling",
    function()
        -- Linux ceiling 108, header overhead 14 → max path length 94.
        local long = "/tmp/" .. string.rep("a", 200)
        local ok, err = pcall(manager.validate_runtime_dir,
            { runtime_dir = long })
        assert_false(ok, "expected raise on overflowed runtime_dir")
        assert_match(tostring(err), "WEZSESH_SUN_PATH_OVERFLOW",
            "wrong sentinel for overflow")
    end)

    it("uses 104 ceiling on darwin", function()
        wezterm_shim.target_triple = "x86_64-apple-darwin"
        -- A 95-char path overflows on darwin (95+14=109 > 104) but not
        -- on linux (95+14=109 > 108 → also fails on linux). Use 91:
        -- 91+14=105 → overflows darwin (>104) but fits linux (<108).
        local p = "/tmp/" .. string.rep("a", 86)  -- 5+86=91
        assert_eq(#p, 91, "path-builder bug in spec")
        local ok, err = pcall(manager.validate_runtime_dir,
            { runtime_dir = p })
        assert_false(ok, "darwin should reject 91-char path")
        assert_match(tostring(err), "WEZSESH_SUN_PATH_OVERFLOW",
            "darwin overflow sentinel wrong")

        wezterm_shim.target_triple = "x86_64-unknown-linux-gnu"
        local ok2 = pcall(manager.validate_runtime_dir,
            { runtime_dir = p })
        assert_true(ok2, "linux should accept 91-char path "
            .. "(91+14=105 < 108)")
    end)

    it("expands a leading ~/ via wezterm.home_dir", function()
        wezterm_shim.home_dir = "/home/" .. string.rep("x", 100)
        local ok, err = pcall(manager.validate_runtime_dir,
            { runtime_dir = "~/wezsesh" })
        -- /home/<100x>/wezsesh = 6+100+8 = 114; +14 = 128 > 108.
        assert_false(ok, "expected ~/-expanded overflow")
        assert_match(tostring(err), "WEZSESH_SUN_PATH_OVERFLOW",
            "expansion failed to influence ceiling math")
    end)

    it("accepts a normal short path", function()
        local ok = pcall(manager.validate_runtime_dir,
            { runtime_dir = "/tmp/wezsesh-1000" })
        assert_true(ok, "short path was rejected")
    end)

    it("returns silently when runtime_dir is nil (auto-detect path)",
    function()
        local ok, err = pcall(manager.validate_runtime_dir, {})
        assert_true(ok,
            "nil runtime_dir should not raise: " .. tostring(err))
        local ok2 = pcall(manager.validate_runtime_dir, nil)
        assert_true(ok2, "nil opts should not raise")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- write_config_file
-- ────────────────────────────────────────────────────────────────────

describe("write_config_file", function()
    it("writes JSON containing the dirs and version fields", function()
        local path = manager.write_config_file({
            snapshot_dir = "/sd",
            state_dir    = "/st",
            runtime_dir  = "/rd",
            data_dir     = "/dd",
        })
        assert_true(type(path) == "string" and path ~= "",
            "no path returned")
        local f = io.open(path, "rb")
        assert_true(f ~= nil, "config file not created at " .. path)
        local body = f:read("*a")
        f:close()
        os.remove(path)
        local parsed = json_parse_shim(body)
        assert_eq(parsed.version, 1, "version not 1")
        assert_eq(parsed.proto_version, 1, "proto_version not 1")
        assert_eq(parsed.plugin_version, manager.VERSION,
            "plugin_version mismatch")
        assert_eq(parsed.snapshot_dir, "/sd", "snapshot_dir wrong")
        assert_eq(parsed.state_dir, "/st", "state_dir wrong")
        assert_eq(parsed.runtime_dir, "/rd", "runtime_dir wrong")
        assert_eq(parsed.data_dir, "/dd", "data_dir wrong")
    end)

    it("returns an absolute path", function()
        local path = manager.write_config_file({})
        assert_true(path:sub(1, 1) == "/",
            "config path not absolute: " .. path)
        os.remove(path)
    end)

    it("emits ALL config-schema top-level keys", function()
        local path = manager.write_config_file({})
        local f = io.open(path, "rb"); local body = f:read("*a")
        f:close(); os.remove(path)
        local parsed = json_parse_shim(body)
        local want_keys = {
            "version", "snapshot_dir", "state_dir", "runtime_dir",
            "data_dir", "log_level", "sort", "default_action",
            "default_action_load_no_prompt", "confirm_delete",
            "confirm_overwrite", "exclude",
            -- new_workspace_command is nil-by-default → omitted by
            -- json_encode (the binary's parser accepts null here).
            "preview", "markers", "columns", "name_truncate",
            "colors", "hooks", "resurrect_argv_allowlist",
            "keys", "plugin_version", "proto_version",
        }
        for _, k in ipairs(want_keys) do
            assert_true(parsed[k] ~= nil,
                "config file missing schema key: " .. k)
        end
    end)

    it("honours user overrides for log_level / sort / hooks", function()
        local path = manager.write_config_file({
            log_level = "debug",
            sort      = "alphabetical",
            hooks     = { run_hooks = false,
                          prompt_on_untrusted = true,
                          timeout_seconds = 30 },
        })
        local f = io.open(path, "rb"); local body = f:read("*a")
        f:close(); os.remove(path)
        local parsed = json_parse_shim(body)
        assert_eq(parsed.log_level, "debug", "log_level not honoured")
        assert_eq(parsed.sort, "alphabetical", "sort not honoured")
        assert_eq(parsed.hooks.run_hooks, false,
            "hooks.run_hooks not honoured")
        assert_eq(parsed.hooks.timeout_seconds, 30,
            "hooks.timeout_seconds not honoured")
    end)

    -- T-903 case 1: nil opts.resurrect_argv_allowlist falls back to the
    -- default_allowlist module so the `[]string` schema receives a
    -- non-empty array (avoiding wezterm's empty-Lua-table → `{}` quirk).
    it("nil resurrect_argv_allowlist falls back to default_allowlist",
    function()
        local default_list = require("default_allowlist")
        assert_true(#default_list > 0,
            "default_allowlist module is empty — fixture broken")
        local path = manager.write_config_file({})
        local f = io.open(path, "rb"); local body = f:read("*a")
        f:close(); os.remove(path)
        local parsed = json_parse_shim(body)
        assert_eq(type(parsed.resurrect_argv_allowlist), "table",
            "resurrect_argv_allowlist not a table")
        assert_true(#parsed.resurrect_argv_allowlist > 0,
            "resurrect_argv_allowlist empty (would emit {} on wire)")
        -- Sanity: the first element matches the first default entry.
        assert_eq(parsed.resurrect_argv_allowlist[1], default_list[1],
            "default_allowlist contents not echoed in config file")
    end)

    -- T-903 case 2: explicit non-empty array round-trips as a JSON array.
    -- Asserts the file body literally contains `["sh"]` (not `{...}`),
    -- not just that the parsed form is correct.
    it("explicit one-element array emits JSON array form", function()
        local path = manager.write_config_file({
            resurrect_argv_allowlist = { "sh" },
        })
        local f = io.open(path, "rb"); local body = f:read("*a")
        f:close(); os.remove(path)
        assert_match(body,
            '"resurrect_argv_allowlist":%[%s*"sh"%s*%]',
            "explicit { 'sh' } not emitted as JSON array")
        local parsed = json_parse_shim(body)
        assert_eq(type(parsed.resurrect_argv_allowlist), "table",
            "parsed allowlist not a table")
        assert_eq(#parsed.resurrect_argv_allowlist, 1,
            "parsed allowlist not length 1")
        assert_eq(parsed.resurrect_argv_allowlist[1], "sh",
            "parsed allowlist[1] not 'sh'")
    end)

    -- T-903 case 3 (load-bearing): explicit empty `{}` emits `[]` via
    -- the json_array_metatable tag path — mirroring production wezterm
    -- where the metatable accessor is installed. The shim's
    -- `wezterm.json_array_metatable` sentinel is reset by the harness
    -- before each test, so this exercise hits the round-2 fix.
    it("explicit empty {} emits JSON `[]` via json_array_metatable",
    function()
        local path = manager.write_config_file({
            resurrect_argv_allowlist = {},
        })
        local f = io.open(path, "rb"); local body = f:read("*a")
        f:close(); os.remove(path)
        assert_match(body,
            '"resurrect_argv_allowlist":%[%s*%]',
            "empty {} not emitted as JSON `[]` (would break config schema)")
    end)

    -- T-903 fallback: when the wezterm build does NOT expose
    -- `json_array_metatable` (older wezterm; the spec shim with the
    -- accessor stripped), an explicitly-empty `{}` must NOT round-trip
    -- as `{}` — manager.lua substitutes the non-empty default_allowlist
    -- so the config schema still receives a JSON array.
    it("empty {} substitutes default_allowlist when accessor absent",
    function()
        wezterm_shim.json_array_metatable = nil
        local path = manager.write_config_file({
            resurrect_argv_allowlist = {},
        })
        local f = io.open(path, "rb"); local body = f:read("*a")
        f:close(); os.remove(path)
        assert_true(body:find('"resurrect_argv_allowlist":{}', 1, true)
            == nil,
            "empty allowlist leaked through as `{}` — schema violation")
        local parsed = json_parse_shim(body)
        assert_eq(type(parsed.resurrect_argv_allowlist), "table",
            "fallback allowlist not a table")
        assert_true(#parsed.resurrect_argv_allowlist > 0,
            "fallback allowlist empty (would emit {} on wire)")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- spawn
-- ────────────────────────────────────────────────────────────────────
--
-- Acceptance gate: env vector contains EXACTLY the four contract
-- keys (WEZSESH_HMAC_KEY, WEZSESH_PROTO_VERSION, WEZSESH_CONFIG_FILE,
-- WEZSESH_PLUGIN_VERSION) — no extras, no missing. The dirs travel
-- inside WEZSESH_CONFIG_FILE.

describe("spawn", function()
    local function seed_session_key()
        gctrl.set("wezsesh_session_key", string.rep("b", 64))
    end

    local function appendix_a_keys()
        return {
            WEZSESH_HMAC_KEY       = true,
            WEZSESH_PROTO_VERSION  = true,
            WEZSESH_CONFIG_FILE    = true,
            WEZSESH_PLUGIN_VERSION = true,
        }
    end

    it("constructs the env vector with the four contract keys (PATH "
        .. "permitted per T-906 launchd workaround)",
    function()
        seed_session_key()
        local win = make_window_stub()
        manager.spawn(win, { spawn_mode = "tab" })
        assert_eq(#spawn_calls, 1, "expected one spawn call")
        local env = spawn_calls[1].arg.set_environment_variables
        -- exact value checks.
        assert_eq(env.WEZSESH_HMAC_KEY, string.rep("b", 64),
            "HMAC key not threaded from GLOBAL")
        assert_eq(env.WEZSESH_PROTO_VERSION, "1",
            "PROTO_VERSION not '1'")
        assert_eq(env.WEZSESH_PLUGIN_VERSION, manager.VERSION,
            "PLUGIN_VERSION not M.VERSION")
        assert_true(type(env.WEZSESH_CONFIG_FILE) == "string"
            and env.WEZSESH_CONFIG_FILE ~= "",
            "CONFIG_FILE empty / wrong type")
        -- The four contract keys MUST be present. We also inject
        -- PATH on macOS launchd children (documented choice in
        -- manager.lua); permit it but reject any other keys.
        local allowed = {
            WEZSESH_HMAC_KEY = true,
            WEZSESH_PROTO_VERSION = true,
            WEZSESH_CONFIG_FILE = true,
            WEZSESH_PLUGIN_VERSION = true,
            PATH = true,
        }
        for k in pairs(env) do
            assert_true(allowed[k] == true,
                "unexpected env key: " .. tostring(k))
        end
        -- Cleanup the temp config file.
        os.remove(env.WEZSESH_CONFIG_FILE)
        -- Reference appendix_a_keys to silence unused-local warnings.
        appendix_a_keys()
    end)

    it("argv is exactly { resolved_binary }", function()
        seed_session_key()
        local win = make_window_stub()
        manager.spawn(win, { spawn_mode = "window",
                             binary = "/abs/wezsesh" })
        local args = spawn_calls[1].arg.args
        assert_eq(#args, 1, "argv length not 1")
        assert_eq(args[1], "/abs/wezsesh", "argv[1] wrong")
        os.remove(spawn_calls[1].arg.set_environment_variables
            .WEZSESH_CONFIG_FILE)
    end)

    it("uses wezterm.mux.spawn_window for spawn_mode='window'", function()
        seed_session_key()
        manager.spawn(nil, { spawn_mode = "window" })
        assert_eq(#spawn_calls, 1, "expected one spawn call")
        assert_eq(spawn_calls[1].mode, "window",
            "did not route through mux.spawn_window")
        os.remove(spawn_calls[1].arg.set_environment_variables
            .WEZSESH_CONFIG_FILE)
    end)

    it("uses mux_window:spawn_tab for spawn_mode='tab' (the default)",
    function()
        seed_session_key()
        local win = make_window_stub()
        manager.spawn(win, {})
        assert_eq(#spawn_calls, 1, "expected one spawn call")
        assert_eq(spawn_calls[1].mode, "tab",
            "did not route through window:mux_window():spawn_tab")
        os.remove(spawn_calls[1].arg.set_environment_variables
            .WEZSESH_CONFIG_FILE)
    end)

    it("returns nil + sentinel when session_key missing", function()
        -- No seed_session_key: GLOBAL is clean.
        local win = make_window_stub()
        local got, sentinel = manager.spawn(win, {})
        assert_nil(got, "expected nil return on missing key")
        assert_eq(sentinel, "WEZSESH_SESSION_KEY_MISSING",
            "wrong sentinel on missing key")
        assert_eq(#spawn_calls, 0,
            "spawn was called despite missing key")
    end)

    -- T-906: macOS GUI launchd hands wezterm.app a minimal PATH that
    -- omits the wezterm CLI itself. manager.spawn must prepend
    -- wezterm.executable_dir so the binary's exec.LookPath("wezterm")
    -- resolves; when the accessor is absent (older builds) the parent's
    -- PATH is inherited unmodified — never clobbered with a fixed string.
    it("PATH is prepended with wezterm.executable_dir when available",
    function()
        seed_session_key()
        wezterm_shim.executable_dir = "/fake/wezterm/bin"
        local saved_getenv = os.getenv
        os.getenv = function(name)
            if name == "PATH" then return "/usr/bin:/bin" end
            return saved_getenv(name)
        end
        local win = make_window_stub()
        local ok, err = pcall(manager.spawn, win, {})
        os.getenv = saved_getenv
        assert_true(ok, "spawn raised: " .. tostring(err))
        local env = spawn_calls[1].arg.set_environment_variables
        assert_eq(env.PATH, "/fake/wezterm/bin:/usr/bin:/bin",
            "PATH not prepended with executable_dir")
        os.remove(env.WEZSESH_CONFIG_FILE)
    end)

    it("PATH inherits parent unmodified when executable_dir absent",
    function()
        seed_session_key()
        wezterm_shim.executable_dir = nil
        local saved_getenv = os.getenv
        os.getenv = function(name)
            if name == "PATH" then return "/usr/bin:/bin" end
            return saved_getenv(name)
        end
        local win = make_window_stub()
        local ok, err = pcall(manager.spawn, win, {})
        os.getenv = saved_getenv
        assert_true(ok, "spawn raised: " .. tostring(err))
        local env = spawn_calls[1].arg.set_environment_variables
        -- Inherited unmodified: no executable_dir prefix, no clobber to a
        -- fixed string different from what the parent supplied.
        assert_eq(env.PATH, "/usr/bin:/bin",
            "PATH not inherited unmodified when executable_dir absent")
        assert_true(env.PATH:find("/fake/wezterm/bin:", 1, true) == nil,
            "stale executable_dir prefix leaked despite accessor absent")
        os.remove(env.WEZSESH_CONFIG_FILE)
    end)

    it("PATH inherits parent unmodified when executable_dir is empty "
        .. "string", function()
        -- Defensive: an empty-string accessor result must NOT produce a
        -- leading ":" in PATH. Treated equivalently to absent.
        seed_session_key()
        wezterm_shim.executable_dir = ""
        local saved_getenv = os.getenv
        os.getenv = function(name)
            if name == "PATH" then return "/usr/bin:/bin" end
            return saved_getenv(name)
        end
        local win = make_window_stub()
        local ok, err = pcall(manager.spawn, win, {})
        os.getenv = saved_getenv
        assert_true(ok, "spawn raised: " .. tostring(err))
        local env = spawn_calls[1].arg.set_environment_variables
        assert_eq(env.PATH, "/usr/bin:/bin",
            "empty-string executable_dir produced a leading ':' in PATH")
        os.remove(env.WEZSESH_CONFIG_FILE)
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- register_keybinding
-- ────────────────────────────────────────────────────────────────────

describe("register_keybinding", function()
    it("appends a {key, mods, action} entry to config.keys", function()
        local config = {}
        manager.register_keybinding(config, {
            keybinding = { key = "W", mods = "LEADER|SHIFT" },
        })
        assert_eq(type(config.keys), "table", "config.keys not table")
        assert_eq(#config.keys, 1, "did not append exactly one entry")
        local entry = config.keys[1]
        assert_eq(entry.key, "W", "entry.key wrong")
        assert_eq(entry.mods, "LEADER|SHIFT", "entry.mods wrong")
        assert_true(type(entry.action) == "table"
            and entry.action.__wezterm_action_callback == true,
            "entry.action is not a wezterm.action_callback wrapper")
        assert_eq(type(entry.action.fn), "function",
            "action callback fn not captured")
    end)

    it("preserves existing config.keys entries", function()
        local config = { keys = { { key = "X", mods = "CTRL" } } }
        manager.register_keybinding(config, {})
        assert_eq(#config.keys, 2, "did not append (lost prior entry)")
        assert_eq(config.keys[1].key, "X", "prior entry mutated")
    end)

    it("uses default keybinding when opts.keybinding nil", function()
        local config = {}
        manager.register_keybinding(config, {})
        assert_eq(config.keys[1].key, "W", "default key wrong")
        assert_eq(config.keys[1].mods, "LEADER|SHIFT",
            "default mods wrong")
    end)

    it("the callback is pcall-wrapped so a spawn raise can't wedge "
        .. "the wezterm event loop", function()
        -- Force spawn to raise inside the callback. The callback's
        -- internal pcall should swallow it without re-raising.
        local config = {}
        manager.register_keybinding(config, {})
        local entry = config.keys[1]
        local saved_spawn = manager.spawn
        manager.spawn = function() error("synthetic raise from spawn") end
        local ok = pcall(entry.action.fn, nil, nil)
        manager.spawn = saved_spawn
        assert_true(ok,
            "callback re-raised — would wedge the wezterm event loop")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- pcall-wrap gate (background_child_process)
-- ────────────────────────────────────────────────────────────────────
--
-- The spawn path uses mux.spawn_window / mux_window:spawn_tab —
-- `background_child_process` is not invoked. The strongest assertion
-- here is "the file contains no unwrapped background_child_process
-- call". A future change that introduces one without `pcall(...)` on
-- the same line will trip this gate.

describe("background_child_process pcall-wrap gate", function()
    it("contains no unwrapped wezterm.background_child_process call",
    function()
        local f = io.open(script_dir() .. "/manager.lua", "rb")
        assert_true(f ~= nil, "could not read manager.lua for lint")
        local source = f:read("*a")
        f:close()

        for line_num, line in
            (function()
                local lines = {}
                local n = 0
                for ln in source:gmatch("([^\n]*)\n?") do
                    n = n + 1
                    lines[#lines + 1] = { n, ln }
                end
                local i = 0
                return function()
                    i = i + 1
                    if i > #lines then return nil end
                    return lines[i][1], lines[i][2]
                end
            end)()
        do
            if line:find("wezterm%.background_child_process") then
                -- A comment line is fine; treat any line whose stripped
                -- form starts with `--` as a comment.
                local stripped = line:gsub("^%s+", "")
                if stripped:sub(1, 2) ~= "--"
                   and not line:find("pcall%(") then
                    error(string.format(
                        "manager.lua:%d uses wezterm.background_child_process "
                        .. "without a pcall wrapper on the same line: %s",
                        line_num, line), 0)
                end
            end
        end
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
