-- runtime/bootstrap_body.lua — module-local stash for the resolved
-- bootstrap reply body.
--
-- Why a stash: `apply_to_config` runs once at wezterm startup with the
-- user's `opts` table in scope, but the `bootstrap` verb fires later
-- (when the binary's TUI dispatches it over IPC). By then `opts` is no
-- longer reachable. We compute the body once at apply_to_config time
-- via `manager.build_bootstrap_body(opts)` and stash the result here;
-- `verbs/bootstrap.lua`'s dispatcher reads it back with `get()` and
-- echoes it as the reply data.
--
-- Cache-bust safety: init.lua's `package.loaded["wezsesh.*"]` wipe
-- loop re-requires this module on Ctrl+Shift+R reload, dropping the
-- stashed body. init.lua immediately re-stashes on the same reload
-- tick (apply_to_config re-runs), so the gap is never observable.
--
-- Module-local Lua state, NOT `wezterm.GLOBAL`: the body is a nested
-- table, and `wezterm.GLOBAL` forbids nested-table values. Mirrors the
-- `runtime/dir_providers.lua` pattern.

local M = {}

-- The stashed body. `nil` until apply_to_config runs.
local body = nil

-- Stash the resolved body table. nil is permitted (and is the initial
-- state); the bootstrap dispatcher handles a nil body by replying with
-- an empty data object.
function M.set(b)
    body = b
end

-- Return the stashed body or nil if `set` has not been called yet.
function M.get()
    return body
end

-- Test seam: clear the stash. Used by spec teardown so a previous
-- test's set doesn't bleed into the next.
function M._reset()
    body = nil
end

return M
