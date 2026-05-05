-- §9.4 — `ops.lua` (verb dispatch table) for the Lua side of the IPC
-- protocol. Sits between `ipc.lua` step (i) (which has just authenticated
-- and validated a request) and the per-verb business logic that calls
-- `resurrect.*` and emits the wire reply via `result.lua`.
--
-- Verb catalog (§6):
--   * switch — switch to workspace; restore-class when saved-not-live.
--   * load   — restore `name` snapshot into the current workspace.
--   * save   — snapshot current workspace; dual-path detector (spike #2).
--   * new    — spawn a new workspace from a cwd.
--   * noop   — TUI cancellation marker. No-op.
--
-- §9.4.1 (restore-class split-reply) and §9.4.2 (save dual-path) are
-- implemented verbatim against the design.md pseudocode. Both load and
-- save go through `resurrect_error.with_capture` (§9.13) — the bare
-- `pcall(state_manager.{save,load}_state, …)` form is forbidden by
-- §17.4 lint because `save_state` swallows I/O / encryption failures
-- into a `wezterm.emit("resurrect.error", string)` and a bare pcall
-- never observes them (spike #2).
--
-- Unknown verbs: §13.13 mandates wire-silent at `ipc.lua` step (e)
-- (verb-keyed shape lookup misses → log_warn + return). ops.dispatch
-- still carries a defensive UNKNOWN_VERB branch — the §17.4 verb / shape
-- parity lint asserts every dispatch_table key has a verb_args_shape
-- entry, so this branch is unreachable in production. Unit tests
-- exercise it directly.
--
-- mlua sandbox: `local wezterm = require("wezterm")` per §9.0.1; every
-- sibling require is namespaced (`wezsesh.*`) so the §11 cache-bust
-- loop in init.lua picks up edits on Ctrl+Shift+R reload. The standalone
-- spec installs a wezterm shim via `package.preload["wezterm"]` BEFORE
-- requiring this file.
--
-- `wezterm.background_child_process` is NOT called directly here — the
-- four `result.reply_*` emitters wrap it (§9.5). All `pcall`-wrapping
-- of the spawn lives in result.lua per §16.5 lint expectations.

local wezterm = require("wezterm")
local result  = require("wezsesh.result")

local M = {}

-- ────────────────────────────────────────────────────────────────────
-- Test seam: replace deps wholesale. The spec swaps `resurrect` and
-- `with_capture` for fakes; production code never calls _set_deps. The
-- defaults lazy-resolve at call time (so a missing `resurrect` global
-- in a stripped-down host degrades gracefully — the dispatch handler
-- replies SAVE_FAILED / SNAPSHOT_LOAD_FAILED / MUX_UNREACHABLE rather
-- than wedging the event loop).
-- ────────────────────────────────────────────────────────────────────

local function default_resurrect()
    -- `resurrect.wezterm` installs itself as the global `resurrect`
    -- table at apply_to_config time (its plugin entry point). We do
    -- NOT require() it from here — it is delivered out-of-band by
    -- the user's wezterm config and may not be a Lua module.
    return rawget(_G, "resurrect")
end

-- Default `with_capture` is a thunk that lazy-requires the production
-- module on every call. The lazy require survives the §11 cache-bust
-- loop (a fresh require after `package.loaded["wezsesh.*"] = nil` picks
-- up edits on Ctrl+Shift+R). The spec replaces _deps.with_capture
-- wholesale via _set_deps.
local function default_with_capture(fn)
    local ok, mod = pcall(require, "wezsesh.resurrect_error")
    if not ok or type(mod) ~= "table"
       or type(mod.with_capture) ~= "function"
    then
        -- Degraded: run fn under bare pcall and return an empty
        -- captured array. The dispatch handlers then take the no-
        -- capture path; downstream §17.4 lint should have caught a
        -- missing module at registration time, so this is purely
        -- defensive.
        local pok, ret = pcall(fn)
        return pok, ret, {}
    end
    return mod.with_capture(fn)
end

local function default_log_warn(msg)
    wezterm.log_warn(msg)
end

M._deps = {
    resurrect    = default_resurrect,
    with_capture = default_with_capture,
    log_warn     = default_log_warn,
}

function M._set_deps(d)
    if type(d) ~= "table" then return end
    for k, v in pairs(d) do
        M._deps[k] = v
    end
end

function M._reset_deps()
    M._deps = {
        resurrect    = default_resurrect,
        with_capture = default_with_capture,
        log_warn     = default_log_warn,
    }
end

local function resolve_resurrect()
    local r = M._deps.resurrect
    if type(r) == "function" then return r() end
    return r
end

local function resolve_with_capture()
    local wc = M._deps.with_capture
    if type(wc) == "function" then return wc end
    return nil
end

local function log_warn(msg)
    local fn = M._deps.log_warn
    if type(fn) == "function" then
        pcall(fn, msg)
    end
end

-- ────────────────────────────────────────────────────────────────────
-- §6.5 — noop. Empty-data completed reply. result.lua normalises nil
-- to `{}` so we pass an explicit empty table for clarity.
-- ────────────────────────────────────────────────────────────────────

local function dispatch_noop(payload, _window, _pane)
    return result.reply_completed(payload, {})
end

-- ────────────────────────────────────────────────────────────────────
-- §6.3 / §9.4.2 — save. Dual-path detector: pcall catches
-- wezterm.json_encode raises (spike #2 V4a — function-value workspace
-- state); with_capture catches I/O / encryption failures swallowed
-- into resurrect.error.
--
-- Lua does NOT enforce expected_hash — already validated binary-side
-- before the §3.1 forward-dispatch (§13.4). Lua does NOT add `hash`
-- to the reply — the binary's Phase C re-hash fills it.
-- ────────────────────────────────────────────────────────────────────

local function dispatch_save(payload, _window, _pane)
    local resurrect = resolve_resurrect()
    if resurrect == nil
       or type(resurrect.workspace_state) ~= "table"
       or type(resurrect.state_manager) ~= "table"
    then
        return result.reply_error(payload, "SAVE_FAILED",
            "resurrect plugin unavailable",
            { raw_error = "resurrect plugin unavailable" })
    end

    local with_capture = resolve_with_capture()
    if with_capture == nil then
        return result.reply_error(payload, "SAVE_FAILED",
            "resurrect_error.with_capture unavailable",
            { raw_error = "resurrect_error.with_capture unavailable" })
    end

    -- §9.4.2 step 2.
    local current_state =
        resurrect.workspace_state.get_workspace_state()

    -- §9.4.2 step 3.
    local pok, perr, captured = with_capture(function()
        return resurrect.state_manager.save_state(current_state)
    end)

    -- §9.4.2 step 4 — pcall caught a Lua error (typically
    -- wezterm.json_encode on a non-encodable value).
    if not pok then
        return result.reply_error(payload, "SAVE_FAILED",
            tostring(perr),
            { raw_error = tostring(perr) })
    end

    -- §9.4.2 step 5 — save_state returned cleanly but resurrect.error
    -- fired during the call (I/O / encryption-agent failure).
    if type(captured) == "table" and #captured > 0 then
        local raw = table.concat(captured, " | ")
        return result.reply_error(payload, "SAVE_FAILED", raw,
            { raw_error = raw })
    end

    -- §9.4.2 step 6 — success. Phase C (binary side) fills `hash`.
    local name = (payload.args and payload.args.name) or ""
    return result.reply_completed(payload, { name = name })
end

-- ────────────────────────────────────────────────────────────────────
-- §6.2 / §9.4.1 — load. Restore-class split-reply with two
-- with_capture rounds (load_state, then restore_workspace).
-- ────────────────────────────────────────────────────────────────────

local function load_and_restore(payload, name, data_factory)
    local resurrect = resolve_resurrect()
    if resurrect == nil
       or type(resurrect.workspace_state) ~= "table"
       or type(resurrect.state_manager) ~= "table"
    then
        return result.reply_error(payload, "SNAPSHOT_LOAD_FAILED",
            "resurrect plugin unavailable",
            { raw_error = "resurrect plugin unavailable" })
    end

    local with_capture = resolve_with_capture()
    if with_capture == nil then
        return result.reply_error(payload, "SNAPSHOT_LOAD_FAILED",
            "resurrect_error.with_capture unavailable",
            { raw_error = "resurrect_error.with_capture unavailable" })
    end

    -- §9.4.1 step 1.
    result.reply_started(payload)

    -- §9.4.1 step 2 — load_state under capture.
    local ok_load, state, captured_load = with_capture(function()
        return resurrect.state_manager.load_state(name, "workspace")
    end)

    -- pcall caught: wezterm.json_parse threw on torn JSON, etc.
    if not ok_load then
        return result.reply_error(payload, "SNAPSHOT_LOAD_FAILED",
            tostring(state),
            { raw_error = tostring(state) })
    end

    -- Empty-state guard: load_state returns `{}` on json_parse-
    -- returned-nil or decrypt failure (resurrect's
    -- state_manager.lua:43–46). Without this guard, restore_workspace
    -- would raise `ipairs(nil)` from inside the spawn loop and the
    -- failure would be misclassified as RESURRECT_PARTIAL.
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

    -- §9.4.1 step 3 — restore_workspace under capture.
    local ok_restore, _, captured_restore = with_capture(function()
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

local function dispatch_load(payload, _window, _pane)
    local name = (payload.args and payload.args.name) or ""
    return load_and_restore(payload, name, function(n)
        return { name = n, workspace = n }
    end)
end

-- ────────────────────────────────────────────────────────────────────
-- §6.1 / §6.1.1 — switch. Live target → set_active_workspace +
-- completed. Saved-not-live → restore-class split-reply (same as
-- load, but with `data: { active_workspace }`).
-- ────────────────────────────────────────────────────────────────────

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

local function dispatch_switch(payload, _window, _pane)
    local name = (payload.args and payload.args.name) or ""

    if workspace_is_live(name) then
        -- §6.1: live target. Direct mux call. wezterm.mux exposes
        -- `set_active_workspace(name)`; on raise (mux unreachable),
        -- reply MUX_UNREACHABLE per §6.1 verb-specific errors.
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

    -- §6.1.1 saved-not-live: split-reply restore path.
    return load_and_restore(payload, name, function(n)
        return { active_workspace = n }
    end)
end

-- ────────────────────────────────────────────────────────────────────
-- §6.4 — new. Spawn a new workspace from `args.cwd` and reply with
-- `data: { name, pane_id }`. Trust check + hook execution is binary-
-- side (§13.5); this verb is purely the wezterm.mux.spawn_window call.
-- ────────────────────────────────────────────────────────────────────

local function dispatch_new(payload, _window, _pane)
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
    -- returns (CLAUDE.md invariant 17). pcall the method call so a
    -- userdata-method raise doesn't wedge.
    local ok_id, pid = pcall(function() return pane:pane_id() end)
    if not ok_id or type(pid) ~= "number" then
        return result.reply_error(payload, "MUX_UNREACHABLE",
            "pane:pane_id() failed: " .. tostring(pid),
            { raw_error = "pane:pane_id() failed: " .. tostring(pid) })
    end

    return result.reply_completed(payload, { name = name, pane_id = pid })
end

-- ────────────────────────────────────────────────────────────────────
-- §9.4 — dispatch table + outer dispatch. Five verbs only (§6).
-- ────────────────────────────────────────────────────────────────────

M.dispatch_table = {
    switch = dispatch_switch,
    load   = dispatch_load,
    save   = dispatch_save,
    new    = dispatch_new,
    noop   = dispatch_noop,
}

-- Outer dispatch. pcall-wrapped at the boundary; emits
-- result.reply_error on caught error.
--
-- UNKNOWN VERB HANDLING: §4.2's verb-keyed verifier in ipc.lua step (e)
-- short-circuits before this function is called, so the UNKNOWN_VERB
-- branch is unreachable in production (§13.13 / §17.4 verb-shape
-- parity lint). The branch is exercised by unit tests directly.
function M.dispatch(payload, window, pane)
    if type(payload) ~= "table" or type(payload.op) ~= "string" then
        return result.reply_error(payload or {}, "UNKNOWN",
            "ops.dispatch: invalid payload",
            { raw_error = "ops.dispatch: invalid payload" })
    end

    local handler = M.dispatch_table[payload.op]
    if handler == nil then
        return result.reply_error(payload, "UNKNOWN_VERB",
            "unknown verb: " .. tostring(payload.op),
            {})
    end

    local ok, err = pcall(handler, payload, window, pane)
    if not ok then
        log_warn("ops.dispatch: verb '" .. tostring(payload.op)
            .. "' raised: " .. tostring(err))
        return result.reply_error(payload, "UNKNOWN",
            tostring(err),
            { raw_error = tostring(err) })
    end
    return true
end

return M
