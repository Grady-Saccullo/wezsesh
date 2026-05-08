-- runtime/dir_providers.lua — typed accessor for the user-supplied list
-- of directory-row providers.
--
-- Same idiom as `runtime/resurrect_ref.lua`: the user passes a list of
-- callables at `apply_to_config` time, init.lua stashes them via
-- `set(list)`, and the `list_dirs` verb dispatcher pulls them out via
-- `invoke_all(query)` to materialise external rows for the picker.
--
-- A provider is `function(query: string) -> { {path: string, name: string}, ... }`.
-- Failures must not bubble: each provider call goes through `pcall`, and
-- a raise / malformed return is logged via `runtime/log` (warn level)
-- and dropped — the next provider's contribution still ships. Empty
-- default: `set` was never called → `get` returns `{}` → `invoke_all`
-- returns `{}`.
--
-- The provider list itself lives in module-local Lua state, NOT in
-- `wezterm.GLOBAL`. Providers are functions, which `wezterm.GLOBAL`
-- forbids in nested-table values; the list is only meaningful inside
-- the current Lua state anyway, so there's no reason to round-trip it
-- through a serialiser.
--
-- Cache-bust safety: init.lua's `package.loaded["wezsesh.*"]` wipe loop
-- re-requires this module on Ctrl+Shift+R reload, dropping any stashed
-- list. init.lua immediately re-stashes via `set(opts.dir_providers)`
-- on the same reload tick, so the gap is never observable.
--
-- The lualint rule `lua-dir-providers-only` enforces that only this
-- module and `verbs/list_dirs.lua` may `require("wezsesh.runtime.dir_providers")`.

local log = require("wezsesh.runtime.log")

local M = {}

-- The stashed provider list. Module-local; reset to `{}` on first
-- module load and on every Ctrl+Shift+R reload (init.lua busts the
-- cache and re-requires).
local providers = {}

-- Stash the user-supplied list. `nil` is normalised to `{}` so the
-- subsequent `invoke_all` is well-defined. A non-list value is also
-- normalised to `{}` and logged — the caller passed something we can't
-- iterate over, but we don't want to wedge `apply_to_config`.
function M.set(list)
    if list == nil then
        providers = {}
        return
    end
    if type(list) ~= "table" then
        log.warn("dir_providers.set: expected table, got " .. type(list)
            .. "; ignoring")
        providers = {}
        return
    end
    -- Snapshot the input; subsequent caller mutations to the passed-in
    -- table must not change what `invoke_all` will iterate.
    local out = {}
    for i = 1, #list do out[i] = list[i] end
    providers = out
end

-- Return the stashed list. The returned table is the live module-local
-- store; callers MUST NOT mutate it. `invoke_all` is the canonical read
-- path; this is exposed for tests and inspection.
function M.get()
    return providers
end

-- Validate a single provider's return: must be a table; every entry
-- must be a table with a string `path` field. `name` is optional —
-- callers (the binary's reply parser) derive `filepath.Base(path)` if
-- the provider didn't supply one. Returns the filtered list of valid
-- entries. Drops malformed entries with a logged warn.
local function sanitise_rows(rows, idx)
    if type(rows) ~= "table" then
        log.warn(string.format(
            "dir_providers: provider #%d returned %s, expected table; "
            .. "dropping", idx, type(rows)))
        return {}
    end
    local out = {}
    for i = 1, #rows do
        local row = rows[i]
        if type(row) == "table" and type(row.path) == "string" then
            local name = row.name
            if type(name) ~= "string" then name = nil end
            out[#out + 1] = { path = row.path, name = name }
        else
            log.warn(string.format(
                "dir_providers: provider #%d row %d malformed "
                .. "(expected {path = string, ...}); dropping",
                idx, i))
        end
    end
    return out
end

-- Invoke every stashed provider with `query` and concatenate the
-- results in registration order. Each provider runs inside `pcall`; a
-- raise is logged and that provider's contribution is skipped. The
-- return shape is `{ {path = string, name = string|nil}, ... }`.
function M.invoke_all(query)
    if type(query) ~= "string" then query = "" end
    local out = {}
    for i = 1, #providers do
        local fn = providers[i]
        if type(fn) == "function" then
            local ok, rows = pcall(fn, query)
            if ok then
                local sane = sanitise_rows(rows, i)
                for j = 1, #sane do
                    out[#out + 1] = sane[j]
                end
            else
                log.warn(string.format(
                    "dir_providers: provider #%d raised: %s",
                    i, tostring(rows)))
            end
        else
            log.warn(string.format(
                "dir_providers: provider #%d is %s, expected function; "
                .. "dropping", i, type(fn)))
        end
    end
    return out
end

-- Test seam: clear the stashed list. Used by spec teardown so a
-- previous test's `set` doesn't bleed into the next.
function M._reset()
    providers = {}
end

return M
