-- verbs/load.lua — restore a saved workspace snapshot into the current
-- workspace. Restore-class verb: split-reply (started → completed/
-- partial/error) wrapped around two with_capture rounds. The shared
-- machinery lives in verbs/_restore.lua; this module just adapts the
-- wire shape.

local restore = require("wezsesh.verbs._restore")

local M = {}

M.args_shape = { _shape = "object", name = "string" }

function M.dispatch(payload, window, _pane)
    local name = (payload.args and payload.args.name) or ""
    -- Data shape on success: { name, workspace } — mirrors the verb
    -- catalog. The TUI reads `name` to position its cursor and
    -- `workspace` to update the active marker.
    --
    -- `window` is the GUI Window the user-var-changed handler fired
    -- in; passing it as `opts.window` lets resurrect adopt the
    -- caller's active tab/pane as the snapshot's first
    -- tab/pane (legacy `smart_workspace_switcher.workspace_switcher.created`
    -- behaviour). The workspace is NOT switched — load is "replay
    -- this snapshot into the current workspace", distinct from
    -- `switch`'s "make me be in workspace X."
    return restore.load_and_restore(payload, name, function(n)
        return { name = n, workspace = n }
    end, { window = window })
end

return M
