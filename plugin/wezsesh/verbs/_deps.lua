-- verbs/_deps.lua — internal deps state shared by every verb module.
--
-- Two seams live here:
--
--   resurrect — the resurrect.wezterm plugin module. Production resolves
--               via runtime.resurrect_ref.get(); tests inject a fake.
--
--   with_capture — wraps a Lua function in a `pcall` that ALSO collects
--                  any `resurrect.error` events emitted during the
--                  call. Production lazy-requires
--                  `wezsesh.resurrect_error.with_capture`; tests inject
--                  a fake or use the real one.
--
-- Why a separate module: per-verb modules import this module directly,
-- and `verbs/init.lua` exposes `_set_deps` / `_reset_deps` that
-- delegate here. Co-locating the state in init.lua would force every
-- verb module to require its parent (a circular import for verbs that
-- get registered at init.lua's load time).
--
-- The underscore prefix marks this as an internal helper; the
-- `lua-verb-contract` lint rule walks `verbs/*.lua` and skips any
-- file whose basename starts with `_`.

local resurrect_ref = require("wezsesh.runtime.resurrect_ref")

local M = {}

-- Default `with_capture` lazy-requires the production capture module
-- on every call. The lazy require survives init.lua's cache-bust loop
-- (a fresh `require` after `package.loaded["wezsesh.*"] = nil` picks
-- up edits on Ctrl+Shift+R). Tests replace `M.deps.with_capture` via
-- `verbs._set_deps`.
local function default_with_capture(fn)
    local ok, mod = pcall(require, "wezsesh.resurrect_error")
    if not ok or type(mod) ~= "table"
       or type(mod.with_capture) ~= "function"
    then
        -- Degraded: run fn under bare pcall and return an empty
        -- captured array. The dispatch handlers then take the no-
        -- capture path. Purely defensive — the standard wiring always
        -- has resurrect_error available.
        local pok, ret = pcall(fn)
        return pok, ret, {}
    end
    return mod.with_capture(fn)
end

-- Default `resurrect` resolver delegates to the centralised module.
-- Tests inject a fake by calling `_set_deps{ resurrect = … }`; the
-- override may be a table (the resurrect module directly) or a
-- function returning one.
local function default_resurrect()
    return resurrect_ref.get()
end

M.deps = {
    resurrect    = default_resurrect,
    with_capture = default_with_capture,
}

-- Resolve the resurrect module. Honours both function and table
-- overrides so tests can hand in either shape.
function M.resurrect()
    local r = M.deps.resurrect
    if type(r) == "function" then return r() end
    return r
end

-- Run `fn` under the configured capture wrapper. Returns
-- `(ok, value_or_error, captured)` matching `resurrect_error.with_capture`'s
-- contract.
function M.with_capture(fn)
    local wc = M.deps.with_capture
    if type(wc) ~= "function" then
        local pok, ret = pcall(fn)
        return pok, ret, {}
    end
    return wc(fn)
end

-- Test seam: replace deps wholesale.
function M.set_deps(d)
    if type(d) ~= "table" then return end
    for k, v in pairs(d) do
        M.deps[k] = v
    end
end

-- Reset to the production-equipped defaults.
function M.reset_deps()
    M.deps = {
        resurrect    = default_resurrect,
        with_capture = default_with_capture,
    }
end

return M
