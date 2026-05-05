-- verbs/switch.lua — switch to a workspace by name. Two paths:
--
--   * Live target (workspace already exists in the mux):
--     direct `wezterm.mux.set_active_workspace(name)` + completed
--     reply. Cheapest path; no restore needed.
--
--   * Saved-not-live target (workspace exists as a snapshot but is
--     not in the live mux): defer to verbs/_restore.lua's split-reply
--     restore path. Same wire shape as `load` but with
--     `data: { active_workspace = name }`.

local wezterm = require("wezterm")
local restore = require("wezsesh.verbs._restore")
local result  = require("wezsesh.result")

local M = {}

M.args_shape = { _shape = "object", name = "string" }

-- Probe the live mux for `name`. wezterm.mux is a userdata; any
-- accessor missing or raising means the mux is unreachable, which
-- the dispatch handler maps to MUX_UNREACHABLE.
local function workspace_is_live(name)
    local mux = wezterm.mux
    if type(mux) ~= "table" then return false end
    if type(mux.get_workspace_names) ~= "function" then return false end
    local ok, names = pcall(mux.get_workspace_names)
    if not ok or type(names) ~= "table" then return false end
    for _, n in ipairs(names) do
        if n == name then return true end
    end
    return false
end

function M.dispatch(payload, _window, _pane)
    local name = (payload.args and payload.args.name) or ""

    if workspace_is_live(name) then
        local mux = wezterm.mux
        if type(mux) ~= "table"
           or type(mux.set_active_workspace) ~= "function"
        then
            return result.reply_error(payload, "MUX_UNREACHABLE",
                "wezterm.mux.set_active_workspace unavailable",
                {
                    raw_error =
                    "wezterm.mux.set_active_workspace unavailable",
                })
        end
        local ok, err = pcall(mux.set_active_workspace, name)
        if not ok then
            return result.reply_error(payload, "MUX_UNREACHABLE",
                tostring(err),
                { raw_error = tostring(err) })
        end
        return result.reply_completed(payload,
            { active_workspace = name })
    end

    -- Saved-not-live: split-reply restore path. Same machinery as
    -- `load`, just a different terminal data shape.
    return restore.load_and_restore(payload, name, function(n)
        return { active_workspace = n }
    end)
end

return M
