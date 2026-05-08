-- verbs/init.lua — verb registry + dispatch boundary.
--
-- Replaces ops.lua. Sits between `ipc.lua`'s authenticated step (i)
-- and the per-verb business logic that calls `resurrect.*` and emits
-- the wire reply via `result.lua`. Adding a verb is now a single-file
-- change: drop a new module under `verbs/`, register it here.
--
-- Verb catalog:
--   * bootstrap — config-fetch verb. Go binary calls this once at TUI
--                 startup to pull resolved opts (replaces the
--                 WEZSESH_CONFIG_JSON_BASE64 env transport).
--   * switch    — switch to workspace; covers live, saved-not-live,
--                 and brand-new (rename-current-window) targets.
--   * load      — restore `name` snapshot into the current workspace.
--   * save      — snapshot current workspace; dual-path detector.
--   * new       — spawn a fresh window into a new workspace (CLI-only).
--   * noop      — TUI cancellation marker. No-op.
--
-- Why save and load go through `resurrect_error.with_capture` (and not
-- a bare `pcall(state_manager.{save,load}_state, …)`): resurrect's
-- state manager swallows I/O and encryption failures into a
-- `wezterm.emit("resurrect.error", string)`, so a bare pcall observes
-- only synchronous Lua raises. The `with_capture` wrapper buffers
-- `resurrect.error` emissions for the duration of the call and returns
-- them alongside the pcall result so both failure paths are caught.
--
-- Unknown verbs: handled wire-silent at `ipc.lua` step (e); the
-- defensive UNKNOWN_VERB branch below is exercised only by unit tests.
--
-- Test seam: `_set_deps` / `_reset_deps` delegate to `verbs/_deps.lua`.
-- Tests written against `verbs._set_deps{ resurrect = …,
-- with_capture = … }` swap the resurrect lookup and capture wrapper
-- across all verb modules in one call.

local deps   = require("wezsesh.verbs._deps")
local log    = require("wezsesh.runtime.log")
local result = require("wezsesh.result")

local M = {}

-- ────────────────────────────────────────────────────────────────────
-- registry
-- ────────────────────────────────────────────────────────────────────

-- Internal registry: { verb_name = mod }. Each `mod` is a per-verb
-- table from `verbs/X.lua` with `args_shape` and `dispatch` fields.
-- Used to extract args_shape data; dispatch lookup goes through
-- `M.dispatch_table` so tests can monkey-patch a single arm without
-- replacing the whole module.
--
-- Underscore-prefixed files (verbs/_deps.lua, verbs/_restore.lua) are
-- helpers, not verbs, and are not registered here.
local registry = {}

-- Public dispatch table: { verb_name = mod.dispatch }. Tests mutate
-- arms of this directly; the repo-shape lint reads it to assert key
-- parity with canonical_json.verb_args_shape.
M.dispatch_table = {}

-- Register a verb module under its wire name. Populates both the
-- internal registry (full module ref, used by `args_shape`) and the
-- public dispatch table (function only). Idempotent.
--
-- The lualint rule `lua-verb-contract` asserts every registered
-- module exposes both `args_shape` and `dispatch`; this function does
-- NOT validate at runtime — a missing field surfaces as a clear error
-- at dispatch time.
function M.register(name, mod)
    registry[name] = mod
    M.dispatch_table[name] = mod and mod.dispatch or nil
end

-- Built-in verb registrations. Adding a verb means dropping a file in
-- `verbs/`, requiring it here, and adding one register call.
M.register("bootstrap", require("wezsesh.verbs.bootstrap"))
M.register("noop",      require("wezsesh.verbs.noop"))
M.register("save",      require("wezsesh.verbs.save"))
M.register("load",      require("wezsesh.verbs.load"))
M.register("switch",    require("wezsesh.verbs.switch"))
M.register("new",       require("wezsesh.verbs.new"))

-- Build `{ verb_name = mod.args_shape }` from the registry. Returns a
-- fresh table on each call; consumers cache the result themselves if
-- they need to. canonical_json.lua reads this at module load time to
-- populate its `verb_args_shape` field, and ipc.lua step (e) reads it
-- at handler-call time to resolve the per-verb shape for the canonical
-- re-encode + HMAC verify.
function M.shapes()
    local out = {}
    for name, mod in pairs(registry) do
        out[name] = mod and mod.args_shape or nil
    end
    return out
end

-- ────────────────────────────────────────────────────────────────────
-- test seam
-- ────────────────────────────────────────────────────────────────────

-- Proxy to verbs/_deps.lua. Production code never calls this; tests
-- inject `resurrect` / `with_capture` here.
M._deps = deps.deps
function M._set_deps(d)
    deps.set_deps(d)
    M._deps = deps.deps
end
function M._reset_deps()
    deps.reset_deps()
    M._deps = deps.deps
end

-- ────────────────────────────────────────────────────────────────────
-- dispatch
-- ────────────────────────────────────────────────────────────────────

-- Outer dispatch. pcall-wrapped at the boundary; emits
-- result.reply_error on caught error so a verb that raises out of
-- pcall cannot wedge the wezterm event loop.
function M.dispatch(payload, window, pane)
    if type(payload) ~= "table" or type(payload.op) ~= "string" then
        return result.reply_error(payload or {}, "UNKNOWN",
            "verbs.dispatch: invalid payload",
            { raw_error = "verbs.dispatch: invalid payload" })
    end

    local handler = M.dispatch_table[payload.op]
    if type(handler) ~= "function" then
        return result.reply_error(payload, "UNKNOWN_VERB",
            "unknown verb: " .. tostring(payload.op),
            {})
    end

    local ok, err = pcall(handler, payload, window, pane)
    if not ok then
        log.warn("verbs.dispatch: verb '" .. tostring(payload.op)
            .. "' raised: " .. tostring(err))
        return result.reply_error(payload, "UNKNOWN",
            tostring(err),
            { raw_error = tostring(err) })
    end
    return true
end

return M
