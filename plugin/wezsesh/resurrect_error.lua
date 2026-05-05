-- §9.13 — `resurrect.error` capture for the dual-path save/load detector.
--
-- This module owns the persistent `wezterm.on("resurrect.error", …)`
-- listener and the per-call capture buffer that backs §9.4.1 / §9.4.2's
-- dual-path error detection. New in spike #2; full empirical basis lives
-- in `docs/issues/2.md` (TL;DR: `state_manager.save_state` swallows I/O
-- and encryption errors into a `resurrect.error` event, so a bare
-- `pcall(save_state, …)` is not enough — a side-channel capture buffer
-- has to be inspected before replying success).
--
-- Why the listener is module-level and persistent, not per-call:
-- `wezterm.on` has no de-register API (per the wezterm Lua docs). A
-- per-call subscribe-then-unsubscribe pattern would either leak
-- handlers (unbounded growth) or rely on a boolean filter inside the
-- handler that we'd have to reset around every save/load — equivalent
-- to the buffer + capture flag this module already keeps.
--
-- Why install via a `_G` gate (and NOT `wezterm.GLOBAL`):
-- `wezterm.GLOBAL` persists across config reloads, but the wezterm
-- runtime rebuilds the Lua state on reload — so the listener registered
-- by the previous reload is gone, while a `wezterm.GLOBAL`-backed gate
-- would still report "installed" and skip re-installation, leaving the
-- plugin without a listener. `_G` survives the §9.1 cache-bust loop
-- (which only nils `package.loaded["wezsesh.*"]`) but is reset on
-- reload — exactly the scope we need.
--
-- mlua sandbox: acquired via `local wezterm = require("wezterm")` per
-- §9.0.1. The standalone spec installs a wezterm shim via
-- `package.preload["wezterm"]` BEFORE requiring this file so the
-- production-shaped require() path is exercised end-to-end.
--
-- Errors are intentionally NOT raised here. The handler runs on every
-- `resurrect.error` emission, which can fire during periodic save (no
-- in-flight wezsesh request to surface a raise to). A raise out of the
-- handler would wedge the wezterm event loop (CLAUDE.md invariant 1).

local wezterm = require("wezterm")

local M = {}

-- Per-call capture buffer. `nil` when no save/load is in flight; a
-- (mutable) array when `with_capture` is wrapping a call. The handler
-- below appends to this array on every `resurrect.error` emission while
-- it is non-nil; emissions outside of a `with_capture` window land in
-- the diagnostic ring instead.
--
-- Re-entrancy: this is a module-level single slot, not a stack. The
-- `with_capture` assert pins the invariant that no save/load handler
-- nests another save/load handler — a nested call would silently
-- overwrite the outer's capture and the outer would lose its events.
local current_capture = nil

-- Diagnostic ring buffer for `resurrect.error` events that fire when
-- no `with_capture` is active (e.g., resurrect's `periodic_save`
-- background tick errors). Bounded so a noisy background failure mode
-- doesn't grow unbounded; FIFO-pruned on overflow. Surfaced by
-- `wezsesh doctor` and the recent-errors log check (§8.17.1
-- `log.recent_errors`) via `recent_uncaptured()` / `clear_uncaptured()`.
local UNCAPTURED_RING_MAX = 32
local uncaptured_ring = {}

-- The single `resurrect.error` handler. Routes the emission into the
-- in-flight capture buffer when there is one, otherwise into the
-- diagnostic ring. Returning nothing (NOT `false`) is deliberate:
-- returning `false` from a `wezterm.on` handler short-circuits the
-- emission chain, suppressing any user-installed `resurrect.error`
-- handler downstream (e.g., a tabline plugin's toast). See spike #2 V2:
-- multiple handlers fire in registration order.
local function on_resurrect_error(msg)
    local s = tostring(msg)
    if current_capture ~= nil then
        current_capture[#current_capture + 1] = s
    else
        wezterm.log_warn("resurrect.error (uncaptured): " .. s)
        uncaptured_ring[#uncaptured_ring + 1] =
            { ts = os.time(), msg = s }
        while #uncaptured_ring > UNCAPTURED_RING_MAX do
            table.remove(uncaptured_ring, 1)
        end
    end
    -- DO NOT `return false` — see header comment.
end

-- Install the persistent `resurrect.error` listener. Called from §9.1
-- `apply_to_config` AFTER the `package.loaded["wezsesh.*"]` cache-bust
-- loop. Idempotent within a single Lua state via a `_G` install gate.
--
-- The gate name is namespaced (`_wezsesh_…_installed`) to make it
-- obvious in any `_G` dump and to avoid colliding with other plugins
-- that might also touch `_G`. The leading underscore matches Lua's
-- conventional "internal / do not touch" namespace.
--
-- `wezterm.on` has no de-register API. Without this gate, every
-- `apply_to_config` call (which the user can trigger by editing
-- `wezterm.lua` and triggering `Ctrl+Shift+R` reload) would register a
-- fresh handler, and every `resurrect.error` emission would fan out
-- N copies into the capture buffer.
function M.register()
    if _G._wezsesh_resurrect_error_listener_installed then
        return
    end
    wezterm.on("resurrect.error", on_resurrect_error)
    _G._wezsesh_resurrect_error_listener_installed = true
end

-- Public API consumed by ops.lua (§9.4.1, §9.4.2).
--
-- Wraps `fn` in `pcall` and a fresh capture buffer. Returns three
-- values:
--   pcall_ok   bool     — false iff fn raised a Lua error
--                          (e.g., `wezterm.json_encode` choked on a
--                          function value, per spike #2 V4a)
--   pcall_ret  any      — pcall's second return: the value fn returned
--                          on success, or the error value on failure
--   captured   string[] — every `resurrect.error` string emitted
--                          between fn entry and fn return; empty
--                          table if none
--
-- The dual-path detector in §9.4.2 inspects BOTH pcall_ok and
-- #captured; either signal alone misses half of the failure modes
-- (json_encode raises vs. swallowed I/O / encryption errors).
--
-- Re-entrancy guard: `with_capture` MUST NOT nest. `wezterm.emit`
-- runs handlers synchronously on the calling thread (spike #2 V1
-- confirmed), so the only way to nest is for a save/load handler to
-- call back into another save/load — currently no path does this. The
-- assert pins the invariant; the captured buffer of an in-flight
-- outer call MUST NOT be silently clobbered by an inner one.
--
-- The capture buffer is reset (back to `nil`) before returning, even
-- when fn raises. This MUST happen via a function-level `pcall` of fn
-- — a `xpcall` with a finalizer would also work but is more
-- machinery than needed here.
function M.with_capture(fn)
    assert(current_capture == nil,
        "wezsesh.resurrect_error: with_capture nested")
    current_capture = {}
    local ok, ret = pcall(fn)
    local captured = current_capture
    current_capture = nil
    return ok, ret, captured
end

-- Returns a copy of the diagnostic ring (uncaptured `resurrect.error`
-- events). Surfaced by `wezsesh doctor` (§8.17.1 `log.recent_errors`)
-- and the runtime `recent_errors` log check. The copy is intentional:
-- callers should not be able to mutate the ring through the returned
-- handle.
function M.recent_uncaptured()
    local out = {}
    for i = 1, #uncaptured_ring do
        local e = uncaptured_ring[i]
        out[i] = { ts = e.ts, msg = e.msg }
    end
    return out
end

-- Clears the diagnostic ring. Called by doctor after surfacing the
-- contents to the user, and exposed for the spec's reset semantics.
function M.clear_uncaptured()
    uncaptured_ring = {}
end

return M
