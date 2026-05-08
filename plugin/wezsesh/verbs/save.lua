-- verbs/save.lua — snapshot the current workspace to disk via
-- resurrect's state_manager. Dual-path failure detector:
--
--   * pcall catches `wezterm.json_encode` raises (a workspace state
--     containing function-typed values, the most common form).
--   * with_capture catches I/O / encryption-agent failures that
--     resurrect's state_manager swallows into a
--     `wezterm.emit("resurrect.error", string)` — a bare pcall would
--     never observe these.
--
-- Wire contract:
--
--   * Lua does NOT enforce expected_hash; the binary already
--     validated it before forward-dispatch.
--   * Lua does NOT add `hash` to the reply; the binary's Phase C
--     re-hash fills it after the file lands on disk.

local deps   = require("wezsesh.verbs._deps")
local result = require("wezsesh.result")

local M = {}

M.args_shape = {
    _shape        = "object",
    name          = "string",
    overwrite     = "bool",
    expected_hash = "string_or_null",
}

function M.dispatch(payload, _window, _pane)
    local resurrect = deps.resurrect()
    if resurrect == nil
       or type(resurrect.workspace_state) ~= "table"
       or type(resurrect.state_manager) ~= "table"
    then
        return result.reply_error(payload, "SAVE_FAILED",
            "resurrect plugin unavailable",
            { raw_error = "resurrect plugin unavailable" })
    end

    -- Snapshot the current workspace state. Resurrect builds the
    -- {window_states, ...} tree from wezterm.mux state on the
    -- caller's behalf.
    local current_state =
        resurrect.workspace_state.get_workspace_state()

    -- Run save_state under the dual-path detector.
    local pok, perr, captured = deps.with_capture(function()
        return resurrect.state_manager.save_state(current_state)
    end)

    -- pcall caught a Lua error (typically wezterm.json_encode on a
    -- non-encodable value).
    if not pok then
        return result.reply_error(payload, "SAVE_FAILED",
            tostring(perr),
            { raw_error = tostring(perr) })
    end

    -- save_state returned cleanly but resurrect.error fired during
    -- the call (I/O / encryption-agent failure).
    if type(captured) == "table" and #captured > 0 then
        local raw = table.concat(captured, " | ")
        return result.reply_error(payload, "SAVE_FAILED", raw,
            { raw_error = raw })
    end

    -- Success. Phase C (binary side) fills `hash`.
    local name = (payload.args and payload.args.name) or ""
    return result.reply_completed(payload, { name = name })
end

return M
