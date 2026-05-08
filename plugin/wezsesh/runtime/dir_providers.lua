-- runtime/dir_providers.lua — typed accessor for the user-supplied
-- list of declarative directory-row provider configs.
--
-- Pattern mirrors `runtime/resurrect_ref.lua`: the user passes a list
-- of typed config tables at `apply_to_config` time, init.lua stashes
-- them via `set(list)`, and `manager.build_bootstrap_body` pulls them
-- back out via `get()` to embed in the bootstrap reply. The Go
-- binary's `internal/dirproviders` package executes them natively.
--
-- A provider config is one of three shapes:
--   { type = "command",   argv = { "zoxide", "query", "-l" }, limit = 200, timeout_ms = 5000 }
--   { type = "directory", path = "~/code", depth = 2, limit = 200, include_hidden = false }
--   { type = "static",    paths = { "/foo", "/bar" } }
--
-- Validation here is intentionally lightweight: we drop entries that
-- aren't tables or that lack a `type` string. Per-type field
-- validation lives on the Go side (see `internal/dirproviders/Config.Validate`)
-- so the Lua plugin doesn't have to maintain a parallel validator.
--
-- The provider list lives in module-local Lua state. Cache-bust
-- safety: init.lua's `package.loaded["wezsesh.*"]` wipe loop
-- re-requires this module on Ctrl+Shift+R reload, dropping the
-- stashed list. init.lua immediately re-stashes via
-- `set(opts.dir_providers)` on the same reload tick.
--
-- The lualint rule `lua-dir-providers-only` enforces that only this
-- module, `manager.lua` (via `build_bootstrap_body`), and `init.lua`
-- (via `set`) may `require("wezsesh.runtime.dir_providers")`.

local log = require("wezsesh.runtime.log")

local M = {}

-- The stashed config list. Module-local; reset to `{}` on first
-- module load and on every Ctrl+Shift+R reload.
local configs = {}

-- Stash the user-supplied list of provider configs. nil → empty list.
-- Non-table input → empty list with a warn log. Per-entry validation
-- (drop non-table entries, missing `type` field) happens here so
-- malformed user opts never reach the bootstrap reply.
function M.set(list)
    if list == nil then
        configs = {}
        return
    end
    if type(list) ~= "table" then
        log.warn("dir_providers.set: expected table, got "
            .. type(list) .. "; ignoring")
        configs = {}
        return
    end
    local out = {}
    for i = 1, #list do
        local entry = list[i]
        if type(entry) ~= "table" then
            log.warn(string.format(
                "dir_providers: entry #%d is %s, expected table; "
                .. "dropping", i, type(entry)))
        elseif type(entry.type) ~= "string" or entry.type == "" then
            log.warn(string.format(
                "dir_providers: entry #%d missing string `type`; "
                .. "dropping", i))
        else
            -- Snapshot the entry so subsequent caller mutations of
            -- the input table cannot change what we ship in the
            -- bootstrap reply. Shallow copy is enough — Lua tables
            -- of strings / numbers / arrays cover every supported
            -- field shape and have no upstream-mutation aliasing.
            local copy = {}
            for k, v in pairs(entry) do
                if type(v) == "table" then
                    local sub = {}
                    for kk, vv in pairs(v) do sub[kk] = vv end
                    copy[k] = sub
                else
                    copy[k] = v
                end
            end
            out[#out + 1] = copy
        end
    end
    configs = out
end

-- Return the stashed config list. The returned table is the live
-- module-local store; callers MUST NOT mutate it. The bootstrap-body
-- builder is the canonical read path.
function M.get()
    return configs
end

-- Test seam: clear the stash. Used by spec teardown.
function M._reset()
    configs = {}
end

return M
