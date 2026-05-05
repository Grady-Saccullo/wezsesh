-- Busted-style spec for resurrect_error.lua. Self-contained — runs
-- under plain `lua plugin/wezsesh/resurrect_error_spec.lua` from the
-- repo root, no busted required.
--
-- Run:
--     cd <repo-root>
--     lua plugin/wezsesh/resurrect_error_spec.lua
--
-- Exits 0 with `OK N/N` on success, 1 with FAIL lines on stderr
-- otherwise. Mirrors the structure of state_spec.lua / result_spec.lua.
--
-- The spec installs a wezterm-shim via `package.preload["wezterm"]`
-- BEFORE requiring the module under test, so resurrect_error.lua's
-- production `require("wezterm")` line resolves to our test double.
-- The shim provides:
--
--   * `wezterm.on(event, handler)` — records the (event, handler) pair
--     in `wezterm_shim.__on_calls` and stashes the handler under
--     `wezterm_shim.__handlers[event]` so the spec can synthesise
--     emissions via `emit(...)`. Mirrors mlua's append-only
--     registration semantics (no de-register API; multiple handlers
--     for the same event fan out in registration order, per spike #2
--     V2 PASS).
--   * `wezterm.log_warn` — captures messages into `log_warns` so the
--     spec can assert the "uncaptured" warn is emitted.
--   * `wezterm.GLOBAL` — empty stub; resurrect_error.lua does not
--     touch it, but other modules pulled into the package.preload
--     namespace might. Kept for forward-compat with shared shims.
--
-- The spec does NOT depend on any sibling wezsesh module — the file
-- under test only imports `wezterm`. `package.path` is set up the same
-- way as result_spec.lua so the production `require("wezterm")` (no
-- prefix) and any future `require("wezsesh.*")` resolve.

local function script_dir()
    local src = arg and arg[0] or "plugin/wezsesh/resurrect_error_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
package.path = script_dir() .. "/?.lua;"
            .. script_dir() .. "/../?.lua;"
            .. package.path

-- ────────────────────────────────────────────────────────────────────
-- wezterm shim — installed BEFORE require("resurrect_error")
-- ────────────────────────────────────────────────────────────────────

-- Per-test mutable surface. `reset_state()` re-initialises these; the
-- shim's tables are *replaced*, not cleared, so existing references
-- captured by an in-flight test stay isolated from the next test.
local on_calls
local handlers
local log_warns
local global_store

local wezterm_shim = {}

-- Mirror mlua's `wezterm.on(event, handler)`: append-only registration.
-- The fan-out (calling each registered handler when `wezterm.emit`
-- fires) is synthesised by the spec's local `emit(...)` helper below
-- since we never invoke `wezterm.emit` from production code in this
-- module — only the `resurrect.error` event matters here, and the
-- handler is the one being unit-tested.
function wezterm_shim.on(event, handler)
    on_calls[#on_calls + 1] = { event = event, handler = handler }
    handlers[event] = handlers[event] or {}
    handlers[event][#handlers[event] + 1] = handler
end

function wezterm_shim.log_warn(msg)
    log_warns[#log_warns + 1] = tostring(msg)
end

-- Synthesise a `wezterm.emit("resurrect.error", msg)` by calling each
-- handler registered against `event` in registration order. mlua's
-- semantics: handlers run synchronously on the emitter's thread
-- (spike #2 V1 confirmed) and return values are not used by us here.
local function emit(event, ...)
    local hs = handlers[event] or {}
    for i = 1, #hs do hs[i](...) end
end

wezterm_shim.GLOBAL = setmetatable({}, {
    __index = function(_, k) return global_store[k] end,
    __newindex = function(_, k, v) global_store[k] = v end,
})

package.preload["wezterm"] = function() return wezterm_shim end

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

-- Reset every piece of mutable state the spec touches between tests:
--   * the wezterm shim's `on_calls` / `handlers` / `log_warns` /
--     GLOBAL store;
--   * the `_G` install gate that resurrect_error.register() sets;
--   * the production module's diagnostic ring (drained via
--     `clear_uncaptured()`);
--   * the production module's package cache, so a subsequent
--     `require("resurrect_error")` re-loads a clean copy. This
--     mirrors what §9.1's `package.loaded["wezsesh.*"] = nil` cache-
--     bust loop does on every config reload — an idempotency test
--     that doesn't reset this would only ever exercise the fast-path
--     after the very first call.
local function reset_state()
    on_calls = {}
    handlers = {}
    log_warns = {}
    global_store = {}
    _G._wezsesh_resurrect_error_listener_installed = nil
    -- Drop the cached module so each test gets a pristine instance
    -- (current_capture nil, ring empty). The bare `resurrect_error`
    -- key matches the bare-require form used inside the spec; the
    -- namespaced `wezsesh.resurrect_error` form is dropped too in
    -- case any test pulls it explicitly.
    package.loaded["resurrect_error"] = nil
    package.loaded["wezsesh.resurrect_error"] = nil
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

-- Re-load the module under test. Tests that need a fresh instance
-- (everything in the idempotency suite) call this after `reset_state`
-- has cleared the package cache + install gate.
local function fresh_module()
    return require("resurrect_error")
end

-- ────────────────────────────────────────────────────────────────────
-- §9.13 — module surface
-- ────────────────────────────────────────────────────────────────────

describe("module surface (§9.13)", function()
    it("exposes the §9.13 API and nothing more", function()
        local M = fresh_module()
        local want = {
            "clear_uncaptured", "recent_uncaptured",
            "register", "with_capture",
        }
        local keys = {}
        for k in pairs(M) do keys[#keys + 1] = k end
        table.sort(keys)
        assert_eq(table.concat(keys, ","), table.concat(want, ","),
            "resurrect_error module surface drift")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- ACCEPTANCE GATE 1 — `with_capture` re-entrancy guard (spike-#2)
-- "nested with_capture raises the assert; outer call's capture is
--  preserved"
-- ────────────────────────────────────────────────────────────────────

describe("with_capture re-entrancy guard (spike-#2)", function()
    it("nested with_capture raises the assert", function()
        local M = fresh_module()
        M.register()
        local inner_pcall_ok = nil
        local inner_err = nil
        local _, _, _ = M.with_capture(function()
            -- Synthesise an in-flight save: while the outer capture is
            -- live, an inner with_capture MUST raise. The assert
            -- propagates through the inner with_capture's own machinery
            -- (the assert fires before pcall is set up around fn).
            local ok, err = pcall(M.with_capture, function() end)
            inner_pcall_ok = ok
            inner_err = err
        end)
        assert_eq(inner_pcall_ok, false,
            "nested with_capture must raise (assert), not return")
        assert_true(
            tostring(inner_err):find("with_capture nested", 1, true) ~= nil,
            "expected the wezsesh.resurrect_error: with_capture nested "
            .. "assert message; got: " .. tostring(inner_err))
    end)

    it("outer call's capture is preserved across a nested-raise attempt",
    function()
        local M = fresh_module()
        M.register()
        local pcall_ok, ret, captured = M.with_capture(function()
            -- Emit one resurrect.error via the production handler
            -- chain so the capture buffer has an event to lose if the
            -- nested call clobbered current_capture.
            emit("resurrect.error", "outer-event-1")
            -- Attempt the nested call; pcall the assert so the outer
            -- with_capture can finish cleanly.
            pcall(M.with_capture, function() end)
            -- Emit another so we can verify the buffer is still wired
            -- to the outer capture (i.e., the assert fired BEFORE the
            -- nested call replaced current_capture with a new {}).
            emit("resurrect.error", "outer-event-2")
            return "outer-ret"
        end)
        assert_eq(pcall_ok, true,
            "outer with_capture must complete cleanly")
        assert_eq(ret, "outer-ret",
            "outer fn's return value must propagate")
        assert_eq(#captured, 2,
            "outer's capture buffer lost events; got "
            .. tostring(#captured) .. " expected 2")
        assert_eq(captured[1], "outer-event-1",
            "outer-event-1 missing from outer capture")
        assert_eq(captured[2], "outer-event-2",
            "outer-event-2 missing — nested call clobbered the buffer")
    end)

    it("after the outer with_capture returns, current_capture clears "
        .. "and a fresh with_capture works", function()
        local M = fresh_module()
        M.register()
        -- Outer round.
        local _, _, c1 = M.with_capture(function()
            emit("resurrect.error", "round-1")
        end)
        assert_eq(#c1, 1, "round-1 capture missing event")
        -- A second, sequential with_capture must work — the outer
        -- already cleared current_capture on return, so this is NOT
        -- a re-entrancy violation.
        local _, _, c2 = M.with_capture(function()
            emit("resurrect.error", "round-2")
        end)
        assert_eq(#c2, 1, "round-2 capture missing event")
        assert_eq(c2[1], "round-2", "round-2 saw the wrong event")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- ACCEPTANCE GATE 2 — register() is idempotent (spike-#2)
-- "calling apply_to_config twice in one Lua state → exactly one
--  wezterm.on('resurrect.error', …) registration via the _G install
--  gate"
-- ────────────────────────────────────────────────────────────────────

describe("register() is idempotent (spike-#2)", function()
    it("calling register() twice → exactly one wezterm.on registration",
    function()
        local M = fresh_module()
        M.register()
        M.register()
        -- Filter on_calls down to the single event we care about; the
        -- shim records every wezterm.on(event, handler) regardless of
        -- event name, but resurrect_error.lua only registers the one.
        local n = 0
        for _, c in ipairs(on_calls) do
            if c.event == "resurrect.error" then n = n + 1 end
        end
        assert_eq(n, 1,
            "expected exactly one wezterm.on('resurrect.error', …) "
            .. "registration after two register() calls; got " .. n)
    end)

    it("the second register() is a no-op even if the install gate is "
        .. "set but the wezterm shim has been re-shimmed", function()
        -- Simulates the §9.1 cache-bust loop: package.loaded is nilled
        -- but `_G` (and therefore the install gate) survives. After a
        -- subsequent `require("resurrect_error")`, register() must
        -- still be a no-op — the gate's purpose is exactly this.
        local M = fresh_module()
        M.register()
        local first_count = #on_calls
        -- Drop the module from the cache, leave _G gate in place. Do
        -- NOT reset on_calls — this round is observing additions.
        package.loaded["resurrect_error"] = nil
        package.loaded["wezsesh.resurrect_error"] = nil
        local M2 = fresh_module()
        M2.register()
        assert_eq(#on_calls, first_count,
            "second register() after cache-bust still added a handler "
            .. "— _G install gate failed")
    end)

    it("after _G gate is cleared (simulating wezterm reload), "
        .. "register() registers fresh", function()
        -- A real wezterm reload rebuilds the Lua state, which clears
        -- both `_G` and `package.loaded`. Simulate by clearing the
        -- gate explicitly and re-loading the module. The new register()
        -- MUST install a fresh handler — otherwise a reload would
        -- leave the plugin without a listener.
        local M = fresh_module()
        M.register()
        local first_count = #on_calls
        -- Wezterm-reload simulation: clear _G + package.loaded.
        _G._wezsesh_resurrect_error_listener_installed = nil
        package.loaded["resurrect_error"] = nil
        package.loaded["wezsesh.resurrect_error"] = nil
        local M2 = fresh_module()
        M2.register()
        assert_eq(#on_calls, first_count + 1,
            "register() after a reload-equivalent gate clear failed "
            .. "to install a fresh handler")
    end)

    it("apply_to_config-style double-call (cache-bust + register) → "
        .. "exactly one registration in one Lua state", function()
        -- §9.1 apply_to_config:
        --   1. nil out package.loaded["wezsesh.*"]
        --   2. require + register every wezsesh.* module
        -- A user editing wezterm.lua and triggering Ctrl+Shift+R does
        -- NOT rebuild the Lua state — apply_to_config runs again on
        -- the same _G. Verify the gate keeps the registration count
        -- pinned at 1.
        local function apply_to_config()
            package.loaded["resurrect_error"] = nil
            package.loaded["wezsesh.resurrect_error"] = nil
            local M = require("resurrect_error")
            M.register()
        end
        apply_to_config()
        apply_to_config()
        apply_to_config()
        local n = 0
        for _, c in ipairs(on_calls) do
            if c.event == "resurrect.error" then n = n + 1 end
        end
        assert_eq(n, 1,
            "three apply_to_config calls produced " .. n
            .. " registrations; expected 1")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §9.13 — capture buffer interleaved with the persistent listener
-- ("Done when: spec verifies the per-call buffer interleaved with the
-- persistent listener.")
-- ────────────────────────────────────────────────────────────────────

describe("per-call buffer interleaved with the persistent listener",
function()
    it("emissions outside with_capture land in the diagnostic ring",
    function()
        local M = fresh_module()
        M.register()
        emit("resurrect.error", "uncaptured-1")
        emit("resurrect.error", "uncaptured-2")
        local recent = M.recent_uncaptured()
        assert_eq(#recent, 2, "ring missing entries")
        assert_eq(recent[1].msg, "uncaptured-1",
            "uncaptured-1 missing or out of order")
        assert_eq(recent[2].msg, "uncaptured-2",
            "uncaptured-2 missing or out of order")
        assert_true(type(recent[1].ts) == "number",
            "ring entry missing ts (number)")
    end)

    it("emissions outside with_capture trigger a log_warn", function()
        local M = fresh_module()
        M.register()
        emit("resurrect.error", "noisy-bg-error")
        assert_eq(#log_warns, 1,
            "expected one log_warn for an uncaptured error")
        assert_true(log_warns[1]:find("uncaptured", 1, true) ~= nil,
            "log_warn message must mention 'uncaptured'; got: "
            .. log_warns[1])
        assert_true(log_warns[1]:find("noisy-bg-error", 1, true) ~= nil,
            "log_warn must include the original error message")
    end)

    it("emissions inside with_capture do NOT touch the ring or "
        .. "trigger log_warn", function()
        local M = fresh_module()
        M.register()
        local _, _, captured = M.with_capture(function()
            emit("resurrect.error", "in-flight-1")
            emit("resurrect.error", "in-flight-2")
        end)
        assert_eq(#captured, 2, "captured buffer wrong length")
        assert_eq(#M.recent_uncaptured(), 0,
            "in-flight events leaked into the diagnostic ring")
        assert_eq(#log_warns, 0,
            "in-flight events triggered log_warn (should be silent)")
    end)

    it("interleaved: uncaptured → captured → uncaptured", function()
        local M = fresh_module()
        M.register()
        -- Phase 1: background error before any save/load is in flight.
        emit("resurrect.error", "bg-before")
        -- Phase 2: a save runs; the only events the capture sees are
        -- the ones emitted between with_capture entry and exit.
        local _, _, captured = M.with_capture(function()
            emit("resurrect.error", "during-save")
        end)
        -- Phase 3: another background error after the save returned.
        emit("resurrect.error", "bg-after")
        assert_eq(#captured, 1, "wrong capture length")
        assert_eq(captured[1], "during-save",
            "capture saw the wrong event during save")
        local recent = M.recent_uncaptured()
        assert_eq(#recent, 2, "ring should hold bg-before + bg-after")
        assert_eq(recent[1].msg, "bg-before", "ring[1] wrong")
        assert_eq(recent[2].msg, "bg-after", "ring[2] wrong")
    end)

    it("with_capture returns (true, ret, []) on a clean fn run",
    function()
        local M = fresh_module()
        M.register()
        local ok, ret, captured = M.with_capture(function()
            return "happy"
        end)
        assert_eq(ok, true, "pcall_ok should be true on clean run")
        assert_eq(ret, "happy", "fn return value lost")
        assert_eq(#captured, 0,
            "no resurrect.error emitted; capture should be empty")
    end)

    it("with_capture returns (false, err, [...]) when fn raises BUT "
        .. "still surfaces the captured events emitted before the raise",
    function()
        -- Spike #2 V4a: `wezterm.json_encode` raises on a function
        -- value, while `state_manager.save_state` on a clean payload
        -- with a missing-dir would emit a resurrect.error and return
        -- nil. The dual-path detector inspects #captured even on
        -- pcall-ok=false so a fn that managed to emit before raising
        -- doesn't lose its diagnostic.
        local M = fresh_module()
        M.register()
        local ok, ret, captured = M.with_capture(function()
            emit("resurrect.error", "pre-raise-event")
            error("synthetic-raise", 0)
        end)
        assert_eq(ok, false, "pcall_ok should be false when fn raised")
        assert_true(tostring(ret):find("synthetic-raise", 1, true) ~= nil,
            "raised error string lost; got: " .. tostring(ret))
        assert_eq(#captured, 1,
            "captured events emitted before raise must still surface")
        assert_eq(captured[1], "pre-raise-event",
            "wrong event in capture buffer")
    end)

    it("the persistent handler does NOT re-register itself per "
        .. "with_capture call — it is module-level", function()
        local M = fresh_module()
        M.register()
        local first = #on_calls
        M.with_capture(function() end)
        M.with_capture(function() end)
        M.with_capture(function() end)
        assert_eq(#on_calls, first,
            "with_capture should not register additional handlers")
    end)

    it("the listener does NOT return false (would short-circuit "
        .. "downstream user-installed handlers)", function()
        -- Spike #2 V2: multiple wezterm.on handlers fire in
        -- registration order. If our handler returned false, mlua's
        -- chain semantics would short-circuit downstream handlers
        -- (e.g., a tabline plugin's resurrect.error toast). Verify
        -- the handler is a void return.
        local M = fresh_module()
        M.register()
        local our_handler = handlers["resurrect.error"][1]
        local r = our_handler("anything")
        assert_nil(r,
            "handler must return nil/void, NOT false (would suppress "
            .. "downstream user handlers)")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- diagnostic ring bounds + reset semantics
-- ────────────────────────────────────────────────────────────────────

describe("diagnostic ring bounds + reset semantics", function()
    it("ring is bounded to 32 entries (FIFO eviction)", function()
        local M = fresh_module()
        M.register()
        for i = 1, 40 do
            emit("resurrect.error", "evt-" .. i)
        end
        local recent = M.recent_uncaptured()
        assert_eq(#recent, 32, "ring should cap at 32; got " .. #recent)
        -- The first 8 events should have been evicted; the surviving
        -- entries are evt-9 through evt-40 in order.
        assert_eq(recent[1].msg, "evt-9",
            "FIFO eviction wrong; oldest survivor should be evt-9, got "
            .. recent[1].msg)
        assert_eq(recent[#recent].msg, "evt-40",
            "newest entry should be evt-40, got " .. recent[#recent].msg)
    end)

    it("recent_uncaptured returns a copy — caller mutation does NOT "
        .. "affect the ring", function()
        local M = fresh_module()
        M.register()
        emit("resurrect.error", "evt-x")
        local r1 = M.recent_uncaptured()
        r1[1].msg = "TAMPERED"
        r1[#r1 + 1] = { ts = 0, msg = "INJECTED" }
        local r2 = M.recent_uncaptured()
        assert_eq(#r2, 1,
            "ring should be unaffected by caller mutation; got "
            .. #r2 .. " entries")
        assert_eq(r2[1].msg, "evt-x",
            "ring entry should be unchanged; got " .. r2[1].msg)
    end)

    it("clear_uncaptured drains the ring", function()
        local M = fresh_module()
        M.register()
        emit("resurrect.error", "drain-me")
        assert_eq(#M.recent_uncaptured(), 1, "pre-drain length wrong")
        M.clear_uncaptured()
        assert_eq(#M.recent_uncaptured(), 0,
            "ring not drained by clear_uncaptured")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- non-string error payload tolerance — mlua emits raw Lua values
-- through wezterm.emit; tostring() coerces them at the boundary.
-- ────────────────────────────────────────────────────────────────────

describe("non-string error payload tolerance", function()
    it("a numeric error payload coerces to a string in capture",
    function()
        local M = fresh_module()
        M.register()
        local _, _, captured = M.with_capture(function()
            emit("resurrect.error", 42)
        end)
        assert_eq(#captured, 1, "captured wrong length")
        assert_eq(captured[1], "42",
            "numeric payload should coerce to string '42'; got: "
            .. tostring(captured[1]))
    end)

    it("a numeric error payload coerces to a string in the ring",
    function()
        local M = fresh_module()
        M.register()
        emit("resurrect.error", 42)
        local recent = M.recent_uncaptured()
        assert_eq(recent[1].msg, "42",
            "numeric payload should coerce in ring; got: "
            .. tostring(recent[1].msg))
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
