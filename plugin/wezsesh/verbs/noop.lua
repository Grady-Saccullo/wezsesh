-- verbs/noop.lua — TUI cancellation marker. The TUI emits a `noop`
-- when the user dismisses the picker without acting; the binary
-- expects a terminal reply on the wire and noop is the cheapest
-- "I saw your request, nothing to do" envelope we can return.

local result = require("wezsesh.result")

local M = {}

-- Empty-args shape. canonical_json's tagger walks the args table by
-- reference to the verb-keyed shape; an empty `_shape = "object"` is
-- enough to round-trip an empty `args = {}` object byte-identically.
M.args_shape = { _shape = "object" }

-- Empty-data completed reply. result.lua normalises nil to `{}` so we
-- pass an explicit empty table for clarity.
function M.dispatch(payload, _window, _pane)
    return result.reply_completed(payload, {})
end

return M
