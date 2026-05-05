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
--
-- `restore_opts` (optional) is the table forwarded into
-- `restore_workspace`. Callers thread `window` (the MuxWindow whose
-- first tab/pane the snapshot adopts) and any extra knobs they need
-- (`spawn_in_workspace`, `close_open_tabs`, etc.). The fields filled
-- in by this helper are the ones the wire-protocol contract owns:
-- `relative = true`, `restore_text = true`, and
-- `on_pane_restore = wezsesh's argv-allowlisted callback`.
-- Without these, resurrect splits panes but never re-runs the saved
-- process inside them — which is what the user observed as "the
-- workspace switches but the tabs come back empty".
--
-- `after_restore` (optional) is a callable fired SYNCHRONOUSLY after
-- a successful restore_workspace, before reply_completed. The switch
-- verb uses it to `mux.set_active_workspace(name)` once resurrect's
-- spawned windows exist — calling set_active_workspace before the
-- workspace has any panes raises, and calling it after the verb has
-- replied is too late (the TUI has already exited). Wrapped in pcall;
-- a raise here logs and falls through to a still-successful reply
-- (better to land in the new workspace partially activated than to
-- regress the whole restore).
function M.load_and_restore(payload, name, data_factory, restore_opts, after_restore)
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
    --
    -- Build the opts table. Caller-supplied fields take precedence; the
    -- defaults set below are the ones the wire-protocol contract owns
    -- (the on_pane_restore callback is the wezsesh argv-allowlisted
    -- wrapper, NOT resurrect's `default_on_pane_restore` directly —
    -- §load-bearing-invariants). `relative`/`restore_text` mirror what
    -- the legacy `smart_workspace_switcher.workspace_switcher.created`
    -- listener passed; without `restore_text` the scrollback is empty,
    -- without `on_pane_restore` panes are split but the saved process
    -- never re-runs.
    local on_pane_restore_mod = require("wezsesh.on_pane_restore")
    local opts = {}
    if type(restore_opts) == "table" then
        for k, v in pairs(restore_opts) do opts[k] = v end
    end
    if opts.on_pane_restore == nil then
        opts.on_pane_restore = on_pane_restore_mod.callback
    end
    if opts.relative == nil then opts.relative = true end
    if opts.restore_text == nil then opts.restore_text = true end
    -- resize_window default false: a snapshot saved on monitor A
    -- carries that monitor's pixel dimensions; replaying them on a
    -- different display (different DPI, different geometry, headless
    -- mux) lands `:set_inner_size` in cases where wezterm raises
    -- before any window/tab spawns finish — the symptom is "the
    -- restore got as far as the spawn artifact and stopped." The
    -- saved cols/rows still flow into spawn_window via
    -- workspace_state.lua's spawn_window_args, so the new window
    -- still picks up the snapshot's terminal geometry; we just skip
    -- the GUI pixel resize.
    if opts.resize_window == nil then opts.resize_window = false end

    local log = require("wezsesh.runtime.log")
    log.warn(string.format(
        "_restore: restore_workspace name=%q window=%s spawn_in_workspace=%s "
        .. "close_open_tabs=%s resize_window=%s "
        .. "on_pane_restore=%s relative=%s restore_text=%s window_states=%d",
        tostring(name),
        tostring(opts.window),
        tostring(opts.spawn_in_workspace),
        tostring(opts.close_open_tabs),
        tostring(opts.resize_window),
        tostring(opts.on_pane_restore),
        tostring(opts.relative),
        tostring(opts.restore_text),
        (type(state) == "table" and type(state.window_states) == "table")
            and #state.window_states or -1))

    local ok_restore, _, captured_restore = deps.with_capture(function()
        resurrect.workspace_state.restore_workspace(state, opts)
    end)
    log.warn(string.format(
        "_restore: restore_workspace returned ok=%s captured_count=%d "
        .. "captured_first=%s",
        tostring(ok_restore),
        (type(captured_restore) == "table") and #captured_restore or -1,
        (type(captured_restore) == "table" and captured_restore[1])
            and tostring(captured_restore[1]):sub(1, 200)
            or "<none>"))

    -- after_restore fires on any non-error path (full success OR
    -- captured emissions = RESURRECT_PARTIAL). The intent — switch
    -- the active workspace — is just as important when only some
    -- windows came back as when they all did. Skipped only on the
    -- raised-out-of-restore path below, where the workspace may not
    -- exist at all.
    if ok_restore and type(after_restore) == "function" then
        local ok_after, after_err = pcall(after_restore)
        if not ok_after then
            log.warn("_restore: after_restore callback raised: "
                .. tostring(after_err))
        end
    end

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
