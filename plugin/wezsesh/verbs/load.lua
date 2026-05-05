-- verbs/load.lua — restore a saved workspace snapshot into the current
-- workspace. Restore-class verb: split-reply (started → completed/
-- partial/error) wrapped around two with_capture rounds. The shared
-- machinery lives in verbs/_restore.lua; this module just adapts the
-- wire shape.

local restore = require("wezsesh.verbs._restore")

local M = {}

M.args_shape = { _shape = "object", name = "string" }

function M.dispatch(payload, _window, _pane)
    local name = (payload.args and payload.args.name) or ""
    -- Data shape on success: { name, workspace } — mirrors the verb
    -- catalog. The TUI reads `name` to position its cursor and
    -- `workspace` to update the active marker.
    return restore.load_and_restore(payload, name, function(n)
        return { name = n, workspace = n }
    end)
end

return M
