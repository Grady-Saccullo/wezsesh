-- verbs/switch.lua — "make workspace X the one I'm in". One verb,
-- four branches:
--
--   1. No-op: name == current workspace. Reply completed, do nothing.
--   2. Live target: workspace already exists in the mux. Plain
--      `wezterm.mux.set_active_workspace(name)`. `cwd` ignored — the
--      workspace already has whatever cwd it was created with.
--   3. Saved-not-live target: workspace exists as a snapshot but is
--      not in the live mux. Defer to verbs/_restore.lua's split-reply
--      restore path. `cwd` ignored — the snapshot encodes its own.
--   4. Brand new: the user picked a name that's neither live nor
--      saved (the zoxide-row UX). Rename the current window's
--      workspace to `name` so the active window comes along; if
--      `cwd` is non-empty, `pane:send_text("cd " .. shellescape(cwd)
--      .. "\r")` on the active pane to land us in the right
--      directory. This replaces the legacy
--      `act.SwitchToWorkspace { spawn = {...} }` flow that opened a
--      fresh window for every not-yet-live target.
--
-- The `new` verb is unchanged — it's the explicit "spawn a fresh
-- window" path the `wezsesh new` CLI still uses. Only TUI dispatch
-- routing changed: every "go to workspace X" picker action now goes
-- through `switch`.

local wezterm = require("wezterm")
local deps    = require("wezsesh.verbs._deps")
local restore = require("wezsesh.verbs._restore")
local result  = require("wezsesh.result")

local M = {}

M.args_shape = {
    _shape = "object",
    name   = "string",
    cwd    = "string",
}

-- Probe the live mux for `name`. wezterm.mux is a userdata; any
-- accessor missing or raising means the mux is unreachable, which
-- the dispatch handler maps to MUX_UNREACHABLE.
local function workspace_is_live(name)
    local mux = wezterm.mux
    if type(mux) ~= "table" and type(mux) ~= "userdata" then
        return false
    end
    if type(mux.get_workspace_names) ~= "function" then return false end
    local ok, names = pcall(mux.get_workspace_names)
    if not ok or type(names) ~= "table" then return false end
    for _, n in ipairs(names) do
        if n == name then return true end
    end
    return false
end

-- Probe the live mux for the active workspace name. Returns nil if
-- the accessor is missing or raises — the rename branch then falls
-- back to skipping the rename step (we still send `cd` so the user
-- isn't stranded with an unrelated cwd).
local function active_workspace(window)
    if window ~= nil then
        local ok, name = pcall(function()
            return window:active_workspace()
        end)
        if ok and type(name) == "string" then return name end
    end
    local mux = wezterm.mux
    if type(mux) == "table" or type(mux) == "userdata" then
        if type(mux.get_active_workspace) == "function" then
            local ok, name = pcall(mux.get_active_workspace)
            if ok and type(name) == "string" then return name end
        end
    end
    return nil
end

-- Single-quote shell-escape: wrap in single quotes, escape any
-- embedded `'` as `'\''`. The active pane's shell receives this as
-- typed input, so the contract is bash/zsh/sh single-quote-string
-- semantics — every byte inside the quotes is literal. `string.format
-- ("%q", ...)` would emit Lua-string semantics (\n escapes, etc.),
-- which is wrong for shell input.
local function shellescape(s)
    if type(s) ~= "string" then s = tostring(s) end
    return "'" .. s:gsub("'", "'\\''") .. "'"
end

function M.dispatch(payload, window, _pane)
    local args = payload.args or {}
    local name = args.name or ""
    local cwd  = args.cwd  or ""

    -- Branch 1 — no-op: target is already the active workspace.
    local current = active_workspace(window)
    if current ~= nil and current == name then
        return result.reply_completed(payload,
            { active_workspace = name })
    end

    -- Branch 2 — live target: plain mux switch. `cwd` ignored.
    if workspace_is_live(name) then
        local mux = wezterm.mux
        if (type(mux) ~= "table" and type(mux) ~= "userdata")
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

    -- Branch 3 — saved-not-live: split-reply restore path. Same
    -- machinery as `load`, just a different terminal data shape.
    --
    -- Discriminator: `cwd == ""`. The Go TUI passes `cwd=""` for
    -- live and saved rows (the snapshot file owns the per-pane cwds)
    -- and the provider's path for SourceExternal rows. We've already
    -- ruled out live by Branch 2 above, so reaching here with an
    -- empty cwd means "saved snapshot" (or a dead name; load_state
    -- will surface SNAPSHOT_LOAD_FAILED via the empty-state guard).
    --
    -- Mux model invariant we rely on (per wezterm's official docs at
    -- https://wezterm.org/workspaces.html):
    --
    --   "Every MuxWindow is associated with a workspace, which is
    --    just a label."
    --   "The wezterm GUI is focused on the active workspace, which
    --    means that it will present a GUI window for each MuxWindow
    --    that is present in that workspace."
    --   "You can spawn windows into differently named workspaces
    --    and they won't become visible until you set the active
    --    workspace to that name."
    --   "When switching the active workspace, wezterm will swap the
    --    contents of the GUI windows with the MuxWindows that
    --    belong to the now-focused workspace."
    --
    -- That's the whole story for visibility: GUI windows are
    -- viewports, the mux owns workspace identity globally, and
    -- `set_active_workspace` is the "swap viewports" operation.
    -- Wezterm hides the source workspace's MuxWindows for us.
    --
    -- So the entire flow is:
    --   1. Pass `spawn_in_workspace = true` + no `opts.window` to
    --      restore_workspace. Resurrect calls `mux.spawn_window
    --      { workspace = target, cwd = saved_cwd }` for every
    --      window_state, so each MuxWindow is born under the target
    --      label at its saved cwd. They remain hidden until step 2.
    --   2. `mux.set_active_workspace(target)` in after_restore.
    --      Wezterm swaps the GUI viewports — source's MuxWindows
    --      (including the wezsesh TUI's window) become hidden, the
    --      newly-spawned target MuxWindows become visible.
    --
    -- Everything I tried earlier — pre-spawning a window, renaming
    -- the source workspace, killing source-workspace panes via
    -- `wezterm cli kill-pane`, `close_open_tabs=true`, manually
    -- closing the wezsesh TUI's window — was fighting wezterm's
    -- model. The rename approach in particular re-labelled every
    -- source MuxWindow to the target, which made wezterm correctly
    -- show all of them — that was Grady's "windows aren't closing"
    -- bug. Source workspace state is preserved on disk and in the
    -- mux (just hidden); switching back via `act.SwitchToWorkspace`
    -- or `set_active_workspace` reveals it.
    if cwd == "" then
        local log = require("wezsesh.runtime.log")
        log.warn(string.format(
            "switch: branch3 saved-not-live name=%q current=%q",
            tostring(name), tostring(current)))

        local mux_for_switch = wezterm.mux

        return restore.load_and_restore(payload, name, function(n)
            return { active_workspace = n }
        end, {
            spawn_in_workspace = true,
        }, function()
            -- after_restore: flip wezterm's active workspace.
            -- `set_active_workspace` errors if the workspace doesn't
            -- exist in the mux yet — restore_workspace just spawned
            -- the snapshot's window_states under that label so it
            -- DOES exist now. wezterm hides the source workspace's
            -- MuxWindows automatically via the viewport swap.
            if (type(mux_for_switch) == "table"
                or type(mux_for_switch) == "userdata")
               and type(mux_for_switch.set_active_workspace) == "function"
            then
                local ok, err = pcall(mux_for_switch.set_active_workspace,
                                      name)
                if not ok then
                    log.warn("switch: post-restore set_active_workspace "
                        .. "failed: " .. tostring(err))
                else
                    log.warn(string.format(
                        "switch: set_active_workspace -> %q (wezterm "
                        .. "hides source-workspace windows automatically)",
                        tostring(name)))
                end
            end
        end)
    end

    -- Branch 4 — brand new: rename the current workspace into `name`
    -- so the active window comes along, then optionally `cd` the
    -- active pane into the requested directory.
    local mux = wezterm.mux
    if (type(mux) ~= "table" and type(mux) ~= "userdata")
       or type(mux.rename_workspace) ~= "function"
    then
        return result.reply_error(payload, "MUX_UNREACHABLE",
            "wezterm.mux.rename_workspace unavailable",
            {
                raw_error =
                "wezterm.mux.rename_workspace unavailable",
            })
    end
    if type(current) == "string" and current ~= "" and current ~= name then
        local ok, err = pcall(mux.rename_workspace, current, name)
        if not ok then
            return result.reply_error(payload, "MUX_UNREACHABLE",
                tostring(err),
                { raw_error = tostring(err) })
        end
    end
    if type(cwd) == "string" and cwd ~= "" then
        local active_pane
        if window ~= nil then
            local ok, p = pcall(function()
                return window:active_pane()
            end)
            if ok then active_pane = p end
        end
        if active_pane ~= nil then
            pcall(function()
                active_pane:send_text("cd " .. shellescape(cwd) .. "\r")
            end)
        end
    end
    return result.reply_completed(payload,
        { active_workspace = name })
end

return M
