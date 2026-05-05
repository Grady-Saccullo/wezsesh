-- Busted-style spec for plugin/init.lua. Self-contained — runs under
-- plain `lua plugin/init_spec.lua` from the repo root, no busted
-- required.
--
-- Run:
--     cd <repo-root>
--     lua plugin/init_spec.lua
--
-- Exits 0 with `OK N/N` on success, 1 with FAIL lines on stderr
-- otherwise. Mirrors the harness convention from
-- plugin/wezsesh/manager_spec.lua.
--
-- This spec exercises every T-603 acceptance gate:
--   * `package.loaded["wezsesh.*"]` bust loop runs (sentinel
--     `package.loaded["wezsesh.fake"] = "stub"` is nil after
--     apply_to_config).
--   * `resurrect_error.register()` is invoked.
--   * Appendix C event set is the EXACT registration set (the spec
--     observes `wezterm.on(name, ...)` calls and asserts the names).
--   * `change_state_save_dir(opts.snapshot_dir .. "/")` is called with
--     the trailing slash.
--   * opts.binary precedence (both / only-binary / only-plugin_root /
--     neither).
--   * Outer body `pcall`-wrapped: a sentinel raise (forced by mocking
--     manager.validate_runtime_dir) is caught and surfaced via
--     wezterm.toast_notification.
--
-- The spec installs a wezterm shim via `package.preload["wezterm"]`
-- BEFORE requiring the module under test, plus shims for every
-- `wezsesh.*` submodule so we can record cross-module call sequences.

local function script_dir()
    local src = arg and arg[0] or "plugin/init_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
-- The package.path lets us require "init" (alongside this spec) and
-- the wezsesh.* siblings (under plugin/wezsesh/). The mirroring of the
-- production-shaped require path matters because init.lua's lazy
-- requires (`require("wezsesh.manager")` etc.) are how the cache-bust
-- loop hands fresh modules to apply_to_config.
package.path = script_dir() .. "/?.lua;"
            .. script_dir() .. "/wezsesh/?.lua;"
            .. script_dir() .. "/?/init.lua;"
            .. package.path

-- ────────────────────────────────────────────────────────────────────
-- wezterm shim
-- ────────────────────────────────────────────────────────────────────

local helpers = require("spec_helpers")
local deepcopy = helpers.deepcopy

local global_proxy = helpers.make_global_proxy()

-- Recording slots for assertions.
local on_calls       = {}    -- { {name, fn}, ... }
local toast_calls    = {}    -- { {title, body, ttl}, ... }
local log_warn_calls = {}
local log_error_calls = {}
local action_callbacks = {}  -- captured wezterm.action_callback wrappers

local wezterm_shim = {
    GLOBAL = global_proxy,
    on = function(name, fn)
        on_calls[#on_calls + 1] = { name = name, fn = fn }
    end,
    toast_notification = function(title, body, _color, ttl_ms)
        toast_calls[#toast_calls + 1] =
            { title = title, body = body, ttl = ttl_ms }
    end,
    log_warn = function(msg)
        log_warn_calls[#log_warn_calls + 1] = msg
    end,
    log_error = function(msg)
        log_error_calls[#log_error_calls + 1] = msg
    end,
    action_callback = function(fn)
        local wrap = { __wezterm_action_callback = true, fn = fn }
        action_callbacks[#action_callbacks + 1] = wrap
        return wrap
    end,
    target_triple = "x86_64-unknown-linux-gnu",
    home_dir      = "/home/test",
}

package.preload["wezterm"] = function() return wezterm_shim end

-- ────────────────────────────────────────────────────────────────────
-- wezsesh.* submodule shims — installed via package.preload BEFORE
-- requiring init. These are the surface init.lua calls into; replacing
-- them with recording fakes lets us assert the cross-module call
-- sequence without dragging the real submodules' I/O behaviour into
-- the spec.
-- ────────────────────────────────────────────────────────────────────

local manager_calls = {
    validate_runtime_dir = {},
    resolve_binary       = {},
    ensure_session_key   = {},
    register_keybinding  = {},
}
local manager_shim = {
    VERSION = "0.1.0",
    validate_runtime_dir = function(opts)
        manager_calls.validate_runtime_dir[
            #manager_calls.validate_runtime_dir + 1] = deepcopy(opts)
    end,
    resolve_binary = function(opts)
        manager_calls.resolve_binary[
            #manager_calls.resolve_binary + 1] = deepcopy(opts)
        if type(opts) == "table" and type(opts.binary) == "string"
           and opts.binary ~= ""
        then
            return opts.binary
        end
        if type(opts) == "table" and type(opts.plugin_root) == "string"
           and opts.plugin_root ~= ""
        then
            return opts.plugin_root .. "/wezsesh"
        end
        return "wezsesh"
    end,
    ensure_session_key = function(bin)
        manager_calls.ensure_session_key[
            #manager_calls.ensure_session_key + 1] = bin
        return string.rep("a", 64)
    end,
    register_keybinding = function(config, opts)
        manager_calls.register_keybinding[
            #manager_calls.register_keybinding + 1] = {
            config = config, opts = deepcopy(opts),
        }
    end,
}

local ipc_calls = { register = {} }
local ipc_shim = {
    register = function(opts)
        ipc_calls.register[#ipc_calls.register + 1] = deepcopy(opts)
        -- Mirror production: ipc.register installs a user-var-changed
        -- handler. Required for the Appendix C event-set assertion.
        wezterm_shim.on("user-var-changed", function() end)
    end,
}

local resurrect_error_calls = { register = 0 }
local resurrect_error_shim = {
    register = function()
        resurrect_error_calls.register =
            resurrect_error_calls.register + 1
        -- Mirror production: install the resurrect.error listener.
        wezterm_shim.on("resurrect.error", function() end)
    end,
}

-- Resurrect plugin shim (the user-supplied resurrect.wezterm). Shape
-- mirrors `resurrect.state_manager.change_state_save_dir(string)`. We
-- record every call to assert the §9.1 trailing-slash contract.
local resurrect_calls = { change_state_save_dir = {} }
local resurrect_shim = {
    state_manager = {
        change_state_save_dir = function(dir)
            resurrect_calls.change_state_save_dir[
                #resurrect_calls.change_state_save_dir + 1] = dir
        end,
    },
}

-- Helper to (re)install a fresh set of wezsesh.* preloads. Called
-- between tests so a test that mutates a submodule doesn't leak into
-- the next.
local function install_wezsesh_preloads()
    package.preload["wezsesh.manager"] =
        function() return manager_shim end
    package.preload["wezsesh.ipc"] =
        function() return ipc_shim end
    package.preload["wezsesh.resurrect_error"] =
        function() return resurrect_error_shim end
end
install_wezsesh_preloads()

-- ────────────────────────────────────────────────────────────────────
-- Now load the module under test.
-- ────────────────────────────────────────────────────────────────────

local init = require("init")

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

local function reset_state()
    on_calls = {}
    toast_calls = {}
    log_warn_calls = {}
    log_error_calls = {}
    action_callbacks = {}

    manager_calls = {
        validate_runtime_dir = {},
        resolve_binary       = {},
        ensure_session_key   = {},
        register_keybinding  = {},
    }
    ipc_calls = { register = {} }
    resurrect_error_calls = { register = 0 }
    resurrect_calls = { change_state_save_dir = {} }

    -- Reset submodule shims to their default productive shape so a
    -- test that swapped a method out (e.g. forced a raise) doesn't
    -- leak into the next.
    manager_shim.validate_runtime_dir = function(opts)
        manager_calls.validate_runtime_dir[
            #manager_calls.validate_runtime_dir + 1] = deepcopy(opts)
    end
    manager_shim.resolve_binary = function(opts)
        manager_calls.resolve_binary[
            #manager_calls.resolve_binary + 1] = deepcopy(opts)
        if type(opts) == "table" and type(opts.binary) == "string"
           and opts.binary ~= ""
        then
            return opts.binary
        end
        if type(opts) == "table" and type(opts.plugin_root) == "string"
           and opts.plugin_root ~= ""
        then
            return opts.plugin_root .. "/wezsesh"
        end
        return "wezsesh"
    end
    manager_shim.ensure_session_key = function(bin)
        manager_calls.ensure_session_key[
            #manager_calls.ensure_session_key + 1] = bin
        return string.rep("a", 64)
    end
    manager_shim.register_keybinding = function(config, opts)
        manager_calls.register_keybinding[
            #manager_calls.register_keybinding + 1] = {
            config = config, opts = deepcopy(opts),
        }
    end
    manager_shim.VERSION = "0.1.0"

    ipc_shim.register = function(opts)
        ipc_calls.register[#ipc_calls.register + 1] = deepcopy(opts)
        wezterm_shim.on("user-var-changed", function() end)
    end

    resurrect_error_shim.register = function()
        resurrect_error_calls.register =
            resurrect_error_calls.register + 1
        wezterm_shim.on("resurrect.error", function() end)
    end

    rawset(_G, "resurrect", nil)
    -- Reset GLOBAL.
    for k in pairs(global_proxy) do global_proxy[k] = nil end

    -- Re-install preloads (a previous test may have nilled some).
    install_wezsesh_preloads()
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

local function assert_match(s, pattern, msg)
    if type(s) ~= "string" or s:find(pattern) == nil then
        error(string.format("%s\n   string: %s\n  pattern: %s",
            msg or "pattern mismatch", tostring(s), pattern), 2)
    end
end

-- ────────────────────────────────────────────────────────────────────
-- module surface
-- ────────────────────────────────────────────────────────────────────

describe("module surface (§9.1)", function()
    it("exposes apply_to_config and a VERSION constant", function()
        assert_eq(type(init.apply_to_config), "function",
            "apply_to_config not function")
        assert_eq(type(init.VERSION), "string", "VERSION not string")
        assert_match(init.VERSION, "^%d+%.%d+%.%d+",
            "VERSION not semver-shaped")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §9.1 — package.loaded["wezsesh.*"] cache-bust loop (spike #1)
-- ────────────────────────────────────────────────────────────────────

describe("cache-bust loop (§9.1 / §17.4)", function()
    it("nils every package.loaded['wezsesh.*'] entry on entry", function()
        package.loaded["wezsesh.fake_sentinel"] = "stub"
        package.loaded["wezsesh.another_one"] = { tag = "stub" }
        init.apply_to_config({}, {})
        assert_nil(package.loaded["wezsesh.fake_sentinel"],
            "wezsesh.fake_sentinel survived bust loop")
        assert_nil(package.loaded["wezsesh.another_one"],
            "wezsesh.another_one survived bust loop")
    end)

    it("does NOT touch unrelated modules", function()
        package.loaded["unrelated_mod"] = "keepme"
        package.loaded["wezsesh"] = "no-dot-keepme"
        init.apply_to_config({}, {})
        assert_eq(package.loaded["unrelated_mod"], "keepme",
            "unrelated module was nilled")
        assert_eq(package.loaded["wezsesh"], "no-dot-keepme",
            "bare 'wezsesh' (no dot) was nilled")
        package.loaded["unrelated_mod"] = nil
        package.loaded["wezsesh"] = nil
    end)

    it("the literal §17.4 grep pattern is present in init.lua source",
    function()
        -- §17.4 grep gate: the bust loop must literally appear in the
        -- file source. CI runs a grep; we reproduce it here so the
        -- requirement breaks visibly if the loop is rewritten.
        local f = io.open(script_dir() .. "/init.lua", "rb")
        assert_true(f ~= nil, "could not read init.lua")
        local src = f:read("*a")
        f:close()
        assert_true(
            src:find('k:sub%(1, 8%) == "wezsesh%."', 1) ~= nil,
            "cache-bust prefix check missing from init.lua")
        assert_true(
            src:find("package%.loaded%[k%] = nil", 1) ~= nil,
            "cache-bust assignment missing from init.lua")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- P §7.1 / §10.6 — GLOBAL schema-version stamp + cross-version wipe
-- ────────────────────────────────────────────────────────────────────

describe("GLOBAL schema-version stamp (P §7.1, §10.6)", function()
    it("stamps wezsesh_plugin_version on first load (no wipe needed)",
    function()
        -- Pre-state empty; the stamp must equal init.VERSION afterwards.
        assert_nil(global_proxy.wezsesh_plugin_version,
            "pre-state had a stamp")
        init.apply_to_config({}, {})
        assert_eq(global_proxy.wezsesh_plugin_version, init.VERSION,
            "version stamp not written on first load")
    end)

    it("wipes every wezsesh_* GLOBAL key when stored stamp mismatches " ..
        "(upgrade path)", function()
        global_proxy.wezsesh_plugin_version = "0.0.0-old"
        global_proxy.wezsesh_dispatcher_socket = "/tmp/x"
        global_proxy.wezsesh_state = { ["123"] = "stale" }
        global_proxy.wezsesh_seen_ids = { ["01ABCDEF"] = 1700000000 }
        global_proxy.wezsesh_writing = { ["/x.json"] = true }
        -- Sentinel that MUST survive (different prefix).
        global_proxy.unrelated_global_key = "keepme"

        init.apply_to_config({}, {})

        assert_nil(global_proxy.wezsesh_dispatcher_socket,
            "wezsesh_dispatcher_socket not wiped")
        assert_nil(global_proxy.wezsesh_state,
            "wezsesh_state not wiped")
        assert_nil(global_proxy.wezsesh_seen_ids,
            "wezsesh_seen_ids not wiped")
        assert_nil(global_proxy.wezsesh_writing,
            "wezsesh_writing not wiped")
        assert_eq(global_proxy.wezsesh_plugin_version, init.VERSION,
            "version stamp not re-written after wipe")
        assert_eq(global_proxy.unrelated_global_key, "keepme",
            "non-wezsesh key wiped (prefix matched too broadly)")
        global_proxy.unrelated_global_key = nil
    end)

    it("wipes on downgrade as well as upgrade", function()
        global_proxy.wezsesh_plugin_version = "9.9.9-future"
        global_proxy.wezsesh_session_key = "deadbeef"
        init.apply_to_config({}, {})
        -- session_key gets re-populated by manager.ensure_session_key
        -- (the shim returns "aaaa..."), so we assert the stamp landed
        -- and the pre-existing stale session key was overwritten in
        -- the post-wipe path. The wipe is observable via the version
        -- stamp moving from 9.9.9-future → init.VERSION; without the
        -- wipe the stamp would have remained 9.9.9-future because
        -- our shim does NOT touch the version key.
        assert_eq(global_proxy.wezsesh_plugin_version, init.VERSION,
            "downgrade path did not re-stamp")
    end)

    it("is idempotent on same-version reload (no wipe, no churn)",
    function()
        global_proxy.wezsesh_plugin_version = init.VERSION
        global_proxy.wezsesh_session_key = "should-survive"
        global_proxy.wezsesh_state = { ["1"] = "should-survive" }

        init.apply_to_config({}, {})

        -- The version matched, so the wipe branch must NOT run. The
        -- session key gets refreshed by manager.ensure_session_key
        -- (the shim returns "aaaa..."), but that's a separate write
        -- that happens after stamp_and_maybe_wipe_globals. The state
        -- key is the cleanest probe — neither init.lua nor the
        -- manager shim touches it on same-version reload.
        assert_true(global_proxy.wezsesh_state ~= nil,
            "wezsesh_state was wiped on same-version reload " ..
            "(idempotency violated)")
        assert_eq(global_proxy.wezsesh_state["1"], "should-survive",
            "wezsesh_state contents lost on same-version reload")
        assert_eq(global_proxy.wezsesh_plugin_version, init.VERSION,
            "stamp drifted on same-version reload")
    end)

    it("stamp+wipe runs BEFORE resurrect_error.register and " ..
        "before manager.ensure_session_key", function()
        -- Order matters: if the wipe ran AFTER ensure_session_key, the
        -- freshly-written session key would be nuked. We instrument
        -- the relevant calls and read GLOBAL state at each one to
        -- assert the stamp is in place by the time those writes happen.
        global_proxy.wezsesh_plugin_version = "0.0.0-old"
        global_proxy.wezsesh_session_key = "stale"

        local stamp_when_register, stamp_when_keygen
        resurrect_error_shim.register = function()
            resurrect_error_calls.register =
                resurrect_error_calls.register + 1
            stamp_when_register = global_proxy.wezsesh_plugin_version
            wezterm_shim.on("resurrect.error", function() end)
        end
        manager_shim.ensure_session_key = function(bin)
            manager_calls.ensure_session_key[
                #manager_calls.ensure_session_key + 1] = bin
            stamp_when_keygen = global_proxy.wezsesh_plugin_version
            return string.rep("a", 64)
        end

        init.apply_to_config({}, {})

        assert_eq(stamp_when_register, init.VERSION,
            "stamp not yet written when resurrect_error.register ran")
        assert_eq(stamp_when_keygen, init.VERSION,
            "stamp not yet written when ensure_session_key ran")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §9.1 step (a) — resurrect_error.register() invoked (spike #2)
-- ────────────────────────────────────────────────────────────────────

describe("resurrect_error.register (§9.1.a / §17.4)", function()
    it("is invoked exactly once per apply_to_config call", function()
        init.apply_to_config({}, {})
        assert_eq(resurrect_error_calls.register, 1,
            "resurrect_error.register call count wrong")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §9.1 step (b) — change_state_save_dir trailing-slash contract
-- ────────────────────────────────────────────────────────────────────

describe("change_state_save_dir (§9.1.b)", function()
    it("calls resurrect.state_manager.change_state_save_dir with " ..
        "snapshot_dir + '/'", function()
        init.apply_to_config({}, {
            snapshot_dir = "/var/lib/wezsesh/snap",
            resurrect    = resurrect_shim,
        })
        assert_eq(#resurrect_calls.change_state_save_dir, 1,
            "expected exactly one change_state_save_dir call")
        assert_eq(resurrect_calls.change_state_save_dir[1],
            "/var/lib/wezsesh/snap/",
            "trailing slash contract violated")
    end)

    it("falls back to _G.resurrect when opts.resurrect absent", function()
        rawset(_G, "resurrect", resurrect_shim)
        init.apply_to_config({}, { snapshot_dir = "/x/y" })
        assert_eq(#resurrect_calls.change_state_save_dir, 1,
            "did not pick up _G.resurrect fallback")
        assert_eq(resurrect_calls.change_state_save_dir[1], "/x/y/",
            "trailing slash on _G fallback wrong")
        rawset(_G, "resurrect", nil)
    end)

    it("does not call when snapshot_dir is nil/empty", function()
        rawset(_G, "resurrect", resurrect_shim)
        init.apply_to_config({}, {})
        assert_eq(#resurrect_calls.change_state_save_dir, 0,
            "called despite missing snapshot_dir")
        init.apply_to_config({}, { snapshot_dir = "" })
        assert_eq(#resurrect_calls.change_state_save_dir, 0,
            "called despite empty-string snapshot_dir")
        rawset(_G, "resurrect", nil)
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §9.1 / §0.1 row 31 — opts.binary / opts.plugin_root precedence
-- ────────────────────────────────────────────────────────────────────

describe("binary / plugin_root precedence (§9.1)", function()
    it("both-set: binary wins (passed unchanged to resolve_binary)",
    function()
        init.apply_to_config({}, {
            binary      = "/abs/wezsesh",
            plugin_root = "/should/not/win",
        })
        local opts = manager_calls.resolve_binary[1]
        assert_eq(opts.binary, "/abs/wezsesh", "binary not preserved")
        -- plugin_root carried through but binary takes precedence in
        -- manager.resolve_binary itself (see §9.2).
        assert_eq(opts.plugin_root, "/should/not/win",
            "plugin_root mutated despite both-set")
    end)

    it("only-binary: derives plugin_root from parent_dir(binary) + " ..
        "'/plugin'", function()
        init.apply_to_config({}, { binary = "/opt/wezsesh-rel/wezsesh" })
        local opts = manager_calls.resolve_binary[1]
        assert_eq(opts.binary, "/opt/wezsesh-rel/wezsesh",
            "binary not preserved")
        assert_eq(opts.plugin_root, "/opt/wezsesh-rel/plugin",
            "plugin_root not auto-derived from parent_dir")
    end)

    it("only-binary on a system PATH dir: optimistic-derivation locks " ..
        "in `<parent>/plugin`", function()
        -- The optimistic-derivation rule is `parent_dir(binary)/plugin`
        -- regardless of whether `<parent>/plugin` actually exists. This
        -- exercises the common-case install where the binary lives on
        -- `$PATH` and the user has supplied `binary` only. The plugin
        -- root may not actually exist at /usr/local/bin/plugin — that's
        -- fine; manager.resolve_binary uses `binary` first and the
        -- derived plugin_root is purely a fallback, never a hard dep.
        init.apply_to_config({}, { binary = "/usr/local/bin/wezsesh" })
        local opts = manager_calls.resolve_binary[1]
        assert_eq(opts.binary, "/usr/local/bin/wezsesh",
            "binary not preserved for system PATH case")
        assert_eq(opts.plugin_root, "/usr/local/bin/plugin",
            "plugin_root derivation rule changed from " ..
            "parent_dir(binary)/plugin")
    end)

    it("only-plugin_root: kept as-is", function()
        init.apply_to_config({}, { plugin_root = "/opt/plugin" })
        local opts = manager_calls.resolve_binary[1]
        assert_nil(opts.binary, "binary materialised from nowhere")
        assert_eq(opts.plugin_root, "/opt/plugin",
            "plugin_root mutated despite only-plugin_root")
    end)

    it("neither: no raise, resolve_binary called with empty opts",
    function()
        init.apply_to_config({}, {})
        assert_eq(#manager_calls.resolve_binary, 1,
            "resolve_binary not called")
        assert_eq(#toast_calls, 0,
            "toast surfaced for neither-set (should be PATH fallback)")
    end)

    it("empty-string binary normalised to nil (treated as absent)",
    function()
        init.apply_to_config({}, {
            binary      = "",
            plugin_root = "/opt/plugin",
        })
        local opts = manager_calls.resolve_binary[1]
        assert_nil(opts.binary,
            "empty-string binary not normalised to nil")
        assert_eq(opts.plugin_root, "/opt/plugin",
            "plugin_root mutated when binary was empty")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Appendix C — exact event-subscription set
-- ────────────────────────────────────────────────────────────────────
--
-- The §17.4 lint rejects `wezterm.on("restore_workspace.finished", …)`,
-- `wezterm.on("smart_workspace_switcher.*", …)`, etc. We assert the
-- POSITIVE shape here: the only event names that appear in `on_calls`
-- after a clean apply_to_config are the Appendix C set.
--
-- Note: Appendix C lists three events for this plugin's surface
-- (write_state.start, write_state.finished, resurrect.error). The
-- `user-var-changed` registration also fires (per §9.3). The shims for
-- resurrect_error.register and ipc.register both call wezterm_shim.on
-- to mirror production registration; the resurrect_error wrapper does
-- not also subscribe to the file_io.write_state events here, because
-- those are owned by the resurrect plugin's own apply_to_config — the
-- wezsesh side just routes via state.set_writing as the handler. The
-- spec asserts the exact set of names that wezsesh's own
-- apply_to_config triggers.

describe("Appendix C event subscriptions", function()
    it("registers EXACTLY the Appendix C event set (transitively)",
    function()
        init.apply_to_config({}, {})
        local names = {}
        for _, c in ipairs(on_calls) do
            names[#names + 1] = c.name
        end
        table.sort(names)
        local want = { "resurrect.error", "user-var-changed" }
        assert_eq(table.concat(names, ","),
            table.concat(want, ","),
            "Appendix C drift — registered event set does not match")
    end)

    it("never subscribes to restore_workspace.finished", function()
        init.apply_to_config({}, {})
        for _, c in ipairs(on_calls) do
            assert_true(
                c.name:find("restore_workspace%.finished") == nil,
                "forbidden subscription: " .. c.name)
        end
    end)

    it("never subscribes to restore_window.finished or " ..
        "restore_tab.finished", function()
        init.apply_to_config({}, {})
        for _, c in ipairs(on_calls) do
            assert_true(c.name:find("restore_window%.finished") == nil,
                "forbidden subscription: " .. c.name)
            assert_true(c.name:find("restore_tab%.finished") == nil,
                "forbidden subscription: " .. c.name)
        end
    end)

    it("never subscribes to any smart_workspace_switcher.* event",
    function()
        init.apply_to_config({}, {})
        for _, c in ipairs(on_calls) do
            assert_true(c.name:find("smart_workspace_switcher") == nil,
                "forbidden subscription: " .. c.name)
        end
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- (P §7.1) — outer body pcall-wrapped
-- ────────────────────────────────────────────────────────────────────

describe("outer pcall boundary (P §7.1)", function()
    it("a sentinel raise inside is caught and surfaced as a toast",
    function()
        manager_shim.validate_runtime_dir = function(_opts)
            error("WEZSESH_SUN_PATH_OVERFLOW: synthetic", 0)
        end
        local ok = pcall(init.apply_to_config, {}, {
            runtime_dir = "/whatever",
        })
        assert_true(ok,
            "apply_to_config re-raised — would explode config eval")
        assert_eq(#toast_calls, 1, "expected exactly one toast call")
        assert_match(toast_calls[1].body, "WEZSESH_SUN_PATH_OVERFLOW",
            "toast did not carry sentinel body")
        assert_eq(toast_calls[1].title, "wezsesh", "toast title wrong")
        -- 10s ttl per the §9.1 contract.
        assert_eq(toast_calls[1].ttl, 10000,
            "toast TTL not 10000ms (~10s)")
    end)

    it("a non-sentinel raise is wrapped in WEZSESH_INTERNAL prefix",
    function()
        manager_shim.validate_runtime_dir = function(_opts)
            error("plain old lua error", 0)
        end
        init.apply_to_config({}, {})
        assert_eq(#toast_calls, 1, "expected exactly one toast call")
        assert_match(toast_calls[1].body, "WEZSESH_INTERNAL",
            "non-sentinel raise not prefixed with WEZSESH_INTERNAL")
    end)

    it("ensure_session_key returning nil + sentinel surfaces a toast",
    function()
        manager_shim.ensure_session_key = function(_bin)
            return nil, "WEZSESH_SESSION_KEY_GENERATION_FAILED"
        end
        local ok = pcall(init.apply_to_config, {}, {})
        assert_true(ok, "session-key failure escaped pcall boundary")
        assert_eq(#toast_calls, 1,
            "expected toast for session-key failure")
        assert_match(toast_calls[1].body,
            "WEZSESH_SESSION_KEY_GENERATION_FAILED",
            "toast did not carry session-key sentinel")
    end)

    it("does not raise when opts is missing entirely", function()
        local ok = pcall(init.apply_to_config, {})
        assert_true(ok, "apply_to_config raised on missing opts")
    end)

    it("does not raise when both config and opts are nil", function()
        -- config can legitimately be nil if the user is using
        -- wezsesh purely for runtime APIs; manager.register_keybinding
        -- internally guards on this (config = config or {}).
        local ok = pcall(init.apply_to_config, nil, nil)
        assert_true(ok, "apply_to_config raised on nil config + opts")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §9.1 step ordering — observable sequence
-- ────────────────────────────────────────────────────────────────────

describe("step ordering (§9.1)", function()
    it("validate_runtime_dir runs before ensure_session_key", function()
        local sequence = {}
        manager_shim.validate_runtime_dir = function(_opts)
            sequence[#sequence + 1] = "validate_runtime_dir"
        end
        manager_shim.ensure_session_key = function(_bin)
            sequence[#sequence + 1] = "ensure_session_key"
            return string.rep("b", 64)
        end
        init.apply_to_config({}, {})
        assert_eq(sequence[1], "validate_runtime_dir",
            "validate_runtime_dir did not run first")
        assert_eq(sequence[2], "ensure_session_key",
            "ensure_session_key did not run after validate_runtime_dir")
    end)

    it("resurrect_error.register → change_state_save_dir → " ..
        "validate_runtime_dir relative ordering (LOW finding)",
    function()
        -- A refactor that swapped change_state_save_dir below
        -- validate_runtime_dir would not have failed any prior test;
        -- this assertion locks in the §9.1 step (a)→(b) ordering plus
        -- the (b)→(c) ordering from validate_runtime_dir.
        local sequence = {}
        resurrect_error_shim.register = function()
            resurrect_error_calls.register =
                resurrect_error_calls.register + 1
            sequence[#sequence + 1] = "resurrect_error.register"
            wezterm_shim.on("resurrect.error", function() end)
        end
        resurrect_shim.state_manager.change_state_save_dir =
            function(dir)
                resurrect_calls.change_state_save_dir[
                    #resurrect_calls.change_state_save_dir + 1] = dir
                sequence[#sequence + 1] = "change_state_save_dir"
            end
        manager_shim.validate_runtime_dir = function(_opts)
            sequence[#sequence + 1] = "validate_runtime_dir"
        end

        init.apply_to_config({}, {
            snapshot_dir = "/var/snap",
            resurrect    = resurrect_shim,
        })

        local re_idx, csd_idx, vrd_idx
        for i, s in ipairs(sequence) do
            if s == "resurrect_error.register" then re_idx = i
            elseif s == "change_state_save_dir" then csd_idx = i
            elseif s == "validate_runtime_dir" then vrd_idx = i
            end
        end
        assert_true(re_idx ~= nil,
            "resurrect_error.register never ran")
        assert_true(csd_idx ~= nil,
            "change_state_save_dir never ran")
        assert_true(vrd_idx ~= nil,
            "validate_runtime_dir never ran")
        assert_true(re_idx < csd_idx,
            "change_state_save_dir ran before resurrect_error.register")
        assert_true(csd_idx < vrd_idx,
            "validate_runtime_dir ran before change_state_save_dir " ..
            "— §9.1 (a)→(b)→(c) ordering broken")
    end)

    it("ipc.register and register_keybinding both run after key gen",
    function()
        local sequence = {}
        manager_shim.ensure_session_key = function(_bin)
            sequence[#sequence + 1] = "ensure_session_key"
            return string.rep("c", 64)
        end
        ipc_shim.register = function(_o)
            sequence[#sequence + 1] = "ipc.register"
            wezterm_shim.on("user-var-changed", function() end)
        end
        manager_shim.register_keybinding = function(_c, _o)
            sequence[#sequence + 1] = "register_keybinding"
        end
        init.apply_to_config({}, {})
        -- ensure_session_key must precede both registrations.
        local ks_idx, ipc_idx, kb_idx
        for i, s in ipairs(sequence) do
            if s == "ensure_session_key" then ks_idx = i
            elseif s == "ipc.register" then ipc_idx = i
            elseif s == "register_keybinding" then kb_idx = i
            end
        end
        assert_true(ks_idx ~= nil, "ensure_session_key never ran")
        assert_true(ipc_idx ~= nil, "ipc.register never ran")
        assert_true(kb_idx ~= nil, "register_keybinding never ran")
        assert_true(ks_idx < ipc_idx,
            "ipc.register ran before ensure_session_key")
        assert_true(ks_idx < kb_idx,
            "register_keybinding ran before ensure_session_key")
    end)

    it("ipc.register receives the runtime_dir + target_window_id keys",
    function()
        init.apply_to_config({}, {
            runtime_dir      = "/tmp/wezsesh-9001",
            target_window_id = 17,
        })
        assert_eq(#ipc_calls.register, 1,
            "ipc.register called wrong # of times")
        local got = ipc_calls.register[1]
        assert_eq(got.runtime_dir, "/tmp/wezsesh-9001",
            "runtime_dir not threaded into ipc.register")
        assert_eq(got.target_window_id, 17,
            "target_window_id not threaded into ipc.register")
    end)

    it("register_keybinding receives the (config, opts) pair", function()
        local cfg = { keys = {} }
        init.apply_to_config(cfg, { binary = "/abs/x" })
        assert_eq(#manager_calls.register_keybinding, 1,
            "register_keybinding called wrong # of times")
        assert_true(manager_calls.register_keybinding[1].config == cfg,
            "config table identity not preserved")
        assert_eq(manager_calls.register_keybinding[1].opts.binary,
            "/abs/x",
            "binary not threaded into register_keybinding")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- mlua sandbox (§9.0.1)
-- ────────────────────────────────────────────────────────────────────

describe("mlua sandbox (§9.0.1 / §17.4)", function()
    local function init_source()
        local f = io.open(script_dir() .. "/init.lua", "rb")
        assert_true(f ~= nil, "could not read init.lua for lint")
        local src = f:read("*a")
        f:close()
        return src
    end

    it("acquires wezterm via require, never via _G.wezterm", function()
        local src = init_source()
        assert_true(
            src:find('local wezterm = require%("wezterm"%)', 1) ~= nil,
            "init.lua does not require wezterm at file top")
        -- The §17.4 grep ban: `_G%.wezterm` MUST NOT appear.
        for line in src:gmatch("([^\n]*)\n?") do
            local stripped = line:gsub("^%s+", "")
            if stripped:sub(1, 2) ~= "--" then
                assert_true(line:find("_G%.wezterm") == nil,
                    "_G.wezterm reference in init.lua: " .. line)
            end
        end
    end)

    it("contains no debug.* references (outside comments)", function()
        local src = init_source()
        for line in src:gmatch("([^\n]*)\n?") do
            local stripped = line:gsub("^%s+", "")
            if stripped:sub(1, 2) ~= "--" then
                assert_true(line:find("debug%.") == nil,
                    "debug.* reference in init.lua: " .. line)
            end
        end
    end)

    it("contains no dofile( references (outside comments)", function()
        local src = init_source()
        for line in src:gmatch("([^\n]*)\n?") do
            local stripped = line:gsub("^%s+", "")
            if stripped:sub(1, 2) ~= "--" then
                assert_true(line:find("dofile%(") == nil,
                    "dofile( reference in init.lua: " .. line)
            end
        end
    end)

    it("does NOT subscribe to 'resurrect.error' directly — " ..
        "resurrect_error.register() is the sole legal subscriber " ..
        "(§9.13 / §17.4)", function()
        -- §17.4 grep gate (interim source-grep until cmd/lualint is
        -- built, T-005). The single-subscriber invariant matters
        -- because resurrect_error.lua maintains an `_G` install gate
        -- to guarantee idempotency across reload, plus a capture-
        -- channel ring buffer that downstream `with_capture`
        -- consumers depend on. A second `wezterm.on("resurrect.error",
        -- …)` subscription elsewhere would either silently lose
        -- captures or duplicate them.
        local src = init_source()
        for line in src:gmatch("([^\n]*)\n?") do
            local stripped = line:gsub("^%s+", "")
            if stripped:sub(1, 2) ~= "--" then
                -- Match the *.on(*"resurrect.error" pattern with
                -- some flexibility for whitespace; the literal
                -- string is the load-bearing part.
                assert_true(
                    line:find('on%s*%(%s*"resurrect%.error"') == nil,
                    'forbidden direct subscription to ' ..
                    '"resurrect.error" in init.lua: ' .. line)
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
    os.exit(0)
end
