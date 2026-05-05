-- verbs/_restore.lua — shared restore-class helper used by `load` and
-- the saved-not-live arm of `switch`. Both verbs need a split-reply
-- (`started` then `completed`/`partial`/`error`) wrapped around two
-- with_capture rounds (state_manager.load_state, then
-- workspace_state.restore_workspace).
--
-- The underscore prefix marks this as a not-a-verb helper; the
-- `lua-verb-contract` lint walks `verbs/*.lua` and skips files whose
-- basename starts with `_`.

local deps   = require("wezsesh.verbs._deps")
local result = require("wezsesh.result")

local M = {}

-- load_and_restore drives the split-reply path:
--
--   1. Validate `resurrect` is wired up. Without it, no work to do —
--      reply SNAPSHOT_LOAD_FAILED and return.
--   2. Emit `started` so the TUI dismisses immediately.
--   3. Run state_manager.load_state under capture. A pcall raise or a
--      `resurrect.error` emission is the same failure: reply
--      SNAPSHOT_LOAD_FAILED with the captured error string.
--   4. Empty-state guard: load_state returns `{}` on json_parse-
--      returned-nil or decrypt failure. Without this guard,
--      restore_workspace would raise `ipairs(nil)` from inside the
--      spawn loop and the failure would be misclassified as
--      RESURRECT_PARTIAL.
--   5. Run workspace_state.restore_workspace under capture. Failures
--      here are RESURRECT_PARTIAL (the restore made progress before
--      the error fired); the TUI surfaces the warning while the
--      partially-restored state stays visible.
--   6. Success → completed reply with the verb-specific data shape.
--
-- `data_factory(name)` returns the data table for the terminal reply;
-- callers parameterise the shape (load returns `{ name, workspace }`,
-- switch returns `{ active_workspace }`).
function M.load_and_restore(payload, name, data_factory)
    local resurrect = deps.resurrect()
    if resurrect == nil
       or type(resurrect.workspace_state) ~= "table"
       or type(resurrect.state_manager) ~= "table"
    then
        return result.reply_error(payload, "SNAPSHOT_LOAD_FAILED",
            "resurrect plugin unavailable",
            { raw_error = "resurrect plugin unavailable" })
    end

    -- Split-reply step 1.
    result.reply_started(payload)

    -- Step 2 — load_state under capture.
    local ok_load, state, captured_load = deps.with_capture(function()
        return resurrect.state_manager.load_state(name, "workspace")
    end)

    -- pcall caught: wezterm.json_parse threw on torn JSON, etc.
    if not ok_load then
        return result.reply_error(payload, "SNAPSHOT_LOAD_FAILED",
            tostring(state),
            { raw_error = tostring(state) })
    end

    -- Empty-state guard: see header comment item 4.
    if (type(captured_load) == "table" and #captured_load > 0)
       or type(state) ~= "table"
       or state.window_states == nil
    then
        local raw
        if type(captured_load) == "table" and #captured_load > 0 then
            raw = table.concat(captured_load, " | ")
        else
            raw = "load returned empty state"
        end
        return result.reply_error(payload, "SNAPSHOT_LOAD_FAILED", raw,
            { raw_error = raw })
    end

    -- Step 3 — restore_workspace under capture.
    local ok_restore, _, captured_restore = deps.with_capture(function()
        resurrect.workspace_state.restore_workspace(state, {})
    end)

    local data = data_factory(name)

    if not ok_restore then
        local msg = tostring(
            (type(captured_restore) == "table" and captured_restore[1])
            or "<no message>")
        return result.reply_partial(payload, data, {{
            code    = "RESURRECT_PARTIAL",
            message = msg,
            details = { raw_error = msg },
        }})
    end

    if type(captured_restore) == "table" and #captured_restore > 0 then
        local raw = table.concat(captured_restore, " | ")
        return result.reply_partial(payload, data, {{
            code    = "RESURRECT_PARTIAL",
            message = raw,
            details = { raw_error = raw },
        }})
    end

    return result.reply_completed(payload, data)
end

return M
