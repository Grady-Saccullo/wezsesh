-- verbs/bootstrap.lua — config-fetch verb. The Go binary calls this
-- once at TUI startup (after the dispatcher is up, before the model
-- is built) to pull the user's resolved opts over IPC. The reply
-- carries the same body shape that flowed via
-- `WEZSESH_CONFIG_JSON_BASE64` historically; see the project plan and
-- `manager.lua::build_bootstrap_body` for the field list.
--
-- The body itself is computed once at apply_to_config time and
-- stashed via `runtime.bootstrap_body.set`; this dispatcher just
-- echoes whatever is stashed. A nil stash (no apply_to_config has
-- run yet, e.g. in a stripped spec harness) replies with `{}` so the
-- wire shape is still well-formed and the caller fails fast on
-- missing required fields rather than on a missing reply.
--
-- Wire shape: completed reply only. No `started`, no `partial` —
-- bootstrap is a single round-trip and the caller (Go's
-- `ipc.AwaitTerminal`) treats anything else as an init-protocol
-- violation.

local bootstrap_body = require("wezsesh.runtime.bootstrap_body")
local result         = require("wezsesh.result")

local M = {}

M.args_shape = { _shape = "object" }

function M.dispatch(payload, _window, _pane)
    local body = bootstrap_body.get()
    if type(body) ~= "table" then body = {} end
    return result.reply_completed(payload, body)
end

return M
