-- runtime/log.lua — single point of contact with wezterm's log surface.
--
-- Production wraps `wezterm.log_warn` / `wezterm.log_error` in `pcall` so
-- a stripped-down host (no logger registered, or one that raises) can
-- never bubble out of an event-loop callback and wedge the wezterm event
-- loop. Specs install a capture pair via `_set` to assert on log content
-- without each consumer module re-implementing its own log seam.
--
-- mlua sandbox: acquired via `local wezterm = require("wezterm")` per the
-- mlua-sandbox rule. `_G.wezterm` is forbidden; the lualint
-- `lua-g-wezterm` rule guards that.
--
-- Cache-bust safety: when init.lua wipes `package.loaded["wezsesh.*"]`
-- on Ctrl+Shift+R, the consumers are also re-required and pick up the
-- fresh module instance. Spec captures installed via `_set` are wiped
-- alongside, but specs re-install their capture between tests.

local wezterm = require("wezterm")

local M = {}

local function default_warn(msg)
    if type(wezterm.log_warn) == "function" then
        pcall(wezterm.log_warn, msg)
    end
end

local function default_error(msg)
    if type(wezterm.log_error) == "function" then
        pcall(wezterm.log_error, msg)
    end
end

local sink_warn  = default_warn
local sink_error = default_error

function M.warn(msg)
    sink_warn(msg)
end

function M.error(msg)
    sink_error(msg)
end

-- Test seam. Replace the warn/error sinks. Pass nil for either arg to
-- leave that sink unchanged. Returns the previous (warn, error) pair so
-- a test can restore on teardown.
function M._set(warn_fn, error_fn)
    local prev_warn, prev_error = sink_warn, sink_error
    if warn_fn ~= nil then sink_warn = warn_fn end
    if error_fn ~= nil then sink_error = error_fn end
    return prev_warn, prev_error
end

-- Reset to the wezterm-backed defaults. Used by spec teardown and by any
-- code path that wants to drop a previously-installed capture without
-- caring about the exact previous values.
function M._reset()
    sink_warn  = default_warn
    sink_error = default_error
end

return M
