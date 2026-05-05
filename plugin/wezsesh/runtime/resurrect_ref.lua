-- runtime/resurrect_ref.lua — single resolver for the user-supplied
-- resurrect plugin module.
--
-- The user holds resurrect as a local in their wezterm config:
--
--     local resurrect = wezterm.plugin.require("https://.../resurrect.wezterm")
--     wezsesh.apply_to_config(config, { resurrect = resurrect, ... })
--
-- We can't reach into that local from anywhere except `apply_to_config`,
-- so init.lua calls `set(opts.resurrect)` once at config-eval time and
-- the value is parked on `_G.wezsesh_resurrect` (a plain Lua global).
-- The reason it goes on `_G` rather than `wezterm.GLOBAL`:
-- `wezterm.GLOBAL` round-trips through wezterm's serialiser and rejects
-- function-typed / nested-table values, both of which the resurrect
-- module table contains. Plain `_G` survives the cache-bust loop in
-- init.lua (which only wipes `package.loaded["wezsesh.*"]`), so a
-- Ctrl+Shift+R reload preserves the reference until init.lua re-stamps it.
--
-- Consumers (`wezsesh.verbs._deps`, `wezsesh.on_pane_restore`) call
-- `get()` — they never touch `_G` directly. The lualint rule
-- `lua-resurrect-ref-only` enforces that.
--
-- Fallback: `_G.resurrect` is checked when `_G.wezsesh_resurrect` is
-- nil. Some users wire resurrect.wezterm by assigning it to `_G.resurrect`
-- directly (older docs); honouring that lookup keeps those configs
-- working without forcing them through `apply_to_config`'s `resurrect=`
-- option.

local M = {}

-- Stash the resolved resurrect module so consumers can pick it up at
-- dispatch time. Idempotent: nil input is a no-op (preserves any
-- previously-stashed reference). init.lua calls this once per
-- `apply_to_config`; tests call it directly to inject a fake.
function M.set(mod)
    if mod == nil then return end
    rawset(_G, "wezsesh_resurrect", mod)
end

-- Return the stashed resurrect module, or nil if neither
-- `_G.wezsesh_resurrect` (the apply_to_config path) nor `_G.resurrect`
-- (the legacy path) has been populated.
function M.get()
    local r = rawget(_G, "wezsesh_resurrect")
    if r ~= nil then return r end
    return rawget(_G, "resurrect")
end

-- Test seam: clear the stashed reference. Used by spec teardown so a
-- previous test's `set` doesn't bleed into the next one.
function M._reset()
    rawset(_G, "wezsesh_resurrect", nil)
end

return M
