-- verbs/new.lua — spawn a fresh workspace from a cwd. Trust check +
-- hook execution is binary-side; this verb is purely the
-- `wezterm.mux.spawn_window` call plus a tidy reply with the new
-- workspace name and pane id so the TUI can position its cursor on
-- the new row.

local wezterm = require("wezterm")
local result  = require("wezsesh.result")

local M = {}

M.args_shape = { _shape = "object", name = "string", cwd = "string" }

function M.dispatch(payload, _window, _pane)
    local args = payload.args or {}
    local name = args.name or ""
    local cwd  = args.cwd  or ""

    local mux = wezterm.mux
    if type(mux) ~= "table" or type(mux.spawn_window) ~= "function" then
        return result.reply_error(payload, "MUX_UNREACHABLE",
            "wezterm.mux.spawn_window unavailable",
            { raw_error = "wezterm.mux.spawn_window unavailable" })
    end

    local ok, ret_or_err, pane_or_nil = pcall(mux.spawn_window, {
        workspace = name,
        cwd       = cwd,
    })
    if not ok then
        return result.reply_error(payload, "MUX_UNREACHABLE",
            tostring(ret_or_err),
            { raw_error = tostring(ret_or_err) })
    end

    -- spawn_window returns `(tab, pane, window)`. With pcall, success
    -- shapes as (true, tab, pane, window). Lua varargs through pcall
    -- only surface as the first three positional locals here; we
    -- received pane_or_nil = pane on the success path.
    local pane = pane_or_nil
    if type(pane) ~= "table" and type(pane) ~= "userdata" then
        -- Some wezterm builds return pane as a userdata; some return
        -- nil for a failed spawn that didn't raise. Treat absent pane
        -- as MUX_UNREACHABLE.
        return result.reply_error(payload, "MUX_UNREACHABLE",
            "wezterm.mux.spawn_window did not return a pane",
            {
                raw_error =
                "wezterm.mux.spawn_window did not return a pane",
            })
    end

    -- pane:pane_id() is immediately available after spawn_window
    -- returns. pcall the method call so a userdata-method raise
    -- doesn't wedge.
    local ok_id, pid = pcall(function() return pane:pane_id() end)
    if not ok_id or type(pid) ~= "number" then
        return result.reply_error(payload, "MUX_UNREACHABLE",
            "pane:pane_id() failed: " .. tostring(pid),
            { raw_error = "pane:pane_id() failed: " .. tostring(pid) })
    end

    return result.reply_completed(payload, { name = name, pane_id = pid })
end

return M
