-- verbs/list_dirs.lua — read-side path enumeration. The TUI dispatches
-- this verb at startup (and on demand) to merge external directory
-- rows (zoxide, fd, ad-hoc tables, ...) into the picker alongside live
-- workspaces and saved snapshots.
--
-- The provider list is set by the user at `apply_to_config` time via
-- `opts.dir_providers = { ... }`; init.lua stashes it through
-- `runtime.dir_providers.set` and we pull it back out here. Each
-- provider is invoked synchronously inside its own `pcall`, so a
-- raising or misbehaving provider never wedges the verb dispatcher;
-- runtime/dir_providers handles all the validation + log surfacing.
--
-- Reply data shape: `{ dirs = { { path = string, name = string|nil }, ... } }`.
-- The binary's reply parser falls back to `filepath.Base(path)` if
-- `name` is absent, so providers MAY omit it.

local dir_providers = require("wezsesh.runtime.dir_providers")
local result        = require("wezsesh.result")

local M = {}

M.args_shape = { _shape = "object", query = "string" }

function M.dispatch(payload, _window, _pane)
    local args = payload.args or {}
    local query = args.query
    if type(query) ~= "string" then query = "" end

    local rows = dir_providers.invoke_all(query)
    return result.reply_completed(payload, { dirs = rows })
end

return M
