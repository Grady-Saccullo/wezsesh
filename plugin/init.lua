-- §9.1 — `init.lua`. The plugin's entry point. Every wezterm config that
-- pulls in wezsesh calls into `M.apply_to_config(config, opts)` from its
-- top-level Lua module. This file MUST stay tiny and disciplined: every
-- raise inside `apply_to_config` is a config-eval explosion the user
-- sees as wezterm refusing to start. (P §7.1) — surface plugin failure
-- as a 10s toast, never a config-load explosion.
--
-- Responsibilities (in strict order):
--   1. Bust `package.loaded["wezsesh.*"]` so submodule edits land on
--      `Ctrl+Shift+R` reload (spike #1 — wezterm reloads init.lua via
--      `loadfile` but does NOT re-evaluate cached `require()` results).
--   2. GLOBAL schema-version stamp (P §7.1, design §10.6). Compare
--      `wezterm.GLOBAL.wezsesh_plugin_version` to `M.VERSION`; on
--      mismatch (including downgrade and first-load-nil) wipe every
--      `wezsesh_*` GLOBAL key and re-stamp. MUST run BEFORE any other
--      GLOBAL write or listener registration — otherwise re-init writes
--      get nuked. Handles `wezterm.plugin.update_all()` cleanly with
--      no migration logic.
--   3. Install the persistent `resurrect.error` listener via
--      `resurrect_error.register()` (spike #2 — backs §9.13 with_capture).
--   4. Lock resurrect's `save_state_dir` to wezsesh's `snapshot_dir`
--      (impossible-by-construction over the §8.17.1
--      `snapshot.dir.matches.resurrect` doctor check).
--   5. Validate `opts.runtime_dir` against the §13.9 SUN_PATH ceiling.
--   6. Generate / fetch the §5.2 HMAC session key into
--      `wezterm.GLOBAL.wezsesh_session_key`.
--   7. Register the `user-var-changed` handler via `ipc.register{...}`.
--   8. Register the §11.1 keybinding via `manager.register_keybinding`.
--
-- Steps 5–8 mutate global state. Each one MUST be inside the outer
-- pcall so a sentinel raise (`WEZSESH_RUNTIME_DIR_TYPE`,
-- `WEZSESH_SUN_PATH_OVERFLOW`, `WEZSESH_CONFIG_WRITE_FAILED`,
-- `WEZSESH_SESSION_KEY_GENERATION_FAILED`) becomes a toast, not a
-- traceback in `~/.wezterm.lua` eval.
--
-- mlua sandbox (§9.0.1):
--   * `local wezterm = require("wezterm")` at file top — never `_G.wezterm`.
--   * No `debug.*`. No `dofile(`. No nested-table values into
--     `wezterm.GLOBAL` (the submodules already enforce this; this file
--     doesn't touch GLOBAL directly).

local wezterm = require("wezterm")

local M = {}

-- §17.4 lint pin: bumped per tagged release in lockstep with
-- `manager.lua`'s `M.VERSION`. The `apply_to_config` body asserts
-- equality — a divergence between the two surfaces is a release-process
-- bug that would silently misreport `WEZSESH_PLUGIN_VERSION` over the
-- wire (Appendix A env vector).
M.VERSION = "0.1.0"

-- ────────────────────────────────────────────────────────────────────
-- helpers
-- ────────────────────────────────────────────────────────────────────

-- Strip the rightmost path component from `p` and return the parent.
-- Pure string ops; no filesystem syscalls. Trailing slash on the result
-- is dropped so the caller can append `/plugin` cleanly.
local function parent_dir(p)
    if type(p) ~= "string" or p == "" then return nil end
    local parent = p:match("^(.*)/[^/]+$")
    if parent == nil or parent == "" then return nil end
    return parent
end

-- Mutate `opts` so `binary` and `plugin_root` carry the §9.1 contract
-- precedence: `binary` non-empty wins; otherwise `plugin_root` carries.
-- When only `binary` is set, derive `plugin_root` as
-- `parent_dir(binary)/plugin` for §9.2.resolve_binary's downstream
-- consistency. Empty-string `binary` is treated as absent (matches
-- `manager.resolve_binary`'s `opts.binary ~= ""` precedence rule).
local function reconcile_binary_opts(opts)
    if type(opts) ~= "table" then return opts end

    local has_binary =
        type(opts.binary) == "string" and opts.binary ~= ""
    local has_plugin_root =
        type(opts.plugin_root) == "string" and opts.plugin_root ~= ""

    if has_binary and not has_plugin_root then
        local parent = parent_dir(opts.binary)
        if parent ~= nil then
            opts.plugin_root = parent .. "/plugin"
        end
    end

    -- Empty-string `binary` is normalised to nil so the precedence test
    -- in `manager.resolve_binary` works on a clean type.
    if type(opts.binary) == "string" and opts.binary == "" then
        opts.binary = nil
    end

    return opts
end

-- GLOBAL schema-version stamp + cross-version wipe (P §7.1, §10.6).
-- Compares `wezterm.GLOBAL.wezsesh_plugin_version` to `M.VERSION`. On
-- mismatch (nil first-load, downgrade, or any inequality) every key
-- whose name starts with `wezsesh_` is set to nil before writing the
-- fresh stamp. Without the wipe, a plugin update that changed the
-- shape of e.g. `wezsesh_state[pid]` would have new code reading old
-- entries and crashing on the indexing path.
--
-- MUST run before any other GLOBAL write or listener registration in
-- apply_to_config — otherwise this very function would clear the
-- writes and leave the plugin half-initialised.
--
-- `pairs(wezterm.GLOBAL)` is iterable in production wezterm (the
-- userdata implements __pairs); the spec harness installs a __pairs
-- metamethod on its proxy so this enumeration works under tests too.
local function stamp_and_maybe_wipe_globals(version)
    local stored = wezterm.GLOBAL.wezsesh_plugin_version
    if stored == version then return end
    -- Mismatch (or nil): wipe every wezsesh_* key, THEN stamp.
    local doomed = {}
    for k, _ in pairs(wezterm.GLOBAL) do
        if type(k) == "string" and k:sub(1, 8) == "wezsesh_" then
            doomed[#doomed + 1] = k
        end
    end
    for _, k in ipairs(doomed) do
        wezterm.GLOBAL[k] = nil
    end
    wezterm.GLOBAL.wezsesh_plugin_version = version
end

-- Surface a sentinel-prefixed error as a 10-second toast. Sentinels are
-- emitted by the submodules via `error("WEZSESH_*: …", 0)`. We match
-- on substring (the `error(msg, 0)` form suppresses the file:line
-- prefix, but we cannot rely on that everywhere).
local function maybe_toast(err)
    if err == nil then return end
    local msg = tostring(err)
    if not msg:find("WEZSESH_") then
        msg = "WEZSESH_INTERNAL: " .. msg
    end
    if type(wezterm.toast_notification) == "function" then
        pcall(wezterm.toast_notification,
            "wezsesh", msg, nil, 10000)
    end
    if type(wezterm.log_error) == "function" then
        pcall(wezterm.log_error, "wezsesh: " .. msg)
    end
end

-- ────────────────────────────────────────────────────────────────────
-- §9.1 — apply_to_config
-- ────────────────────────────────────────────────────────────────────
--
-- The outer body runs inside `pcall`. Any sentinel raise inside is
-- caught and surfaced via `wezterm.toast_notification`; the user's
-- wezterm continues to start. (P §7.1)
--
-- The §17.4 grep gate enforces the `package.loaded["wezsesh.*"]` bust
-- loop is present in this function literally. Keep the form below
-- byte-stable.

function M.apply_to_config(config, opts)
    opts = opts or {}

    -- Cache-bust §9.1 / spike #1. MUST stay literally as below — §17.4
    -- lint greps for this exact loop body. Sub-8-char prefixes would
    -- match user modules whose names happen to start with `wezsesh`
    -- (e.g. `wezsesh_extras.lua`); the `.` is part of the contract.
    for k in pairs(package.loaded) do
        if k:sub(1, 8) == "wezsesh." then package.loaded[k] = nil end
    end

    local ok, err = pcall(function()
        -- Reconcile binary/plugin_root precedence (§9.1 contract).
        opts = reconcile_binary_opts(opts)

        -- Step 2 — GLOBAL schema-version stamp + cross-version wipe
        -- (P §7.1, §10.6). MUST run BEFORE any other GLOBAL write or
        -- listener registration; the wipe set is `^wezsesh_` keys, so
        -- a stamp written after `ensure_session_key` would itself nuke
        -- `wezsesh_session_key`. Idempotent on same-version reloads
        -- (early-returns when the stored stamp equals M.VERSION).
        stamp_and_maybe_wipe_globals(M.VERSION)

        -- Step 3 — Install the persistent `resurrect.error` listener.
        -- Idempotent within a single Lua state via the `_G` install gate
        -- in `resurrect_error.lua`; cleanly re-armed on reload because
        -- wezterm rebuilds the Lua state.
        local resurrect_error = require("wezsesh.resurrect_error")
        resurrect_error.register()

        -- Step 4 — Lock resurrect's save_state_dir to opts.snapshot_dir.
        -- `resurrect` is the user-supplied resurrect.wezterm plugin
        -- module; the §9.1 contract names this call site explicitly.
        -- We resolve the table off the user's wezterm config: the user
        -- typically does `local resurrect = wezterm.plugin.require("…")`
        -- before calling us, then passes the table on opts.resurrect.
        -- Spec is silent on the resolution path; both `opts.resurrect`
        -- and a top-level `_G.resurrect` (set by the resurrect plugin's
        -- own apply path) are accepted. (Accepted finding.)
        local resurrect_mod = opts.resurrect
        if resurrect_mod == nil then
            resurrect_mod = rawget(_G, "resurrect")
        end

        -- Stash the resolved module on a plain Lua global so ops.lua
        -- can pick it up at dispatch time. wezterm.GLOBAL forbids
        -- nested-table values (CLAUDE.md mlua-sandbox invariant), so
        -- we cannot route the reference through there; the §11 cache-
        -- bust loop only wipes `package.loaded["wezsesh.*"]`, leaving
        -- `_G` keys intact. Without this stash, ops.lua's load/save
        -- handlers can't see the user-supplied resurrect plugin and
        -- every dispatch replies "resurrect plugin unavailable".
        if resurrect_mod ~= nil then
            rawset(_G, "wezsesh_resurrect", resurrect_mod)
        end

        -- When `opts.snapshot_dir` is nil/empty (the §11 default that
        -- delegates to §12.5 auto-detect), we deliberately do NOT call
        -- `change_state_save_dir`. The §9.1 "drift impossible by
        -- construction" guarantee only holds when the user has pinned
        -- the dir on both sides; otherwise resurrect's own §12.5
        -- auto-detect path is the canonical resolver, and the
        -- `snapshot.dir.matches.resurrect` doctor check (§8.17.1) is
        -- the load-bearing fallback that catches drift at runtime.
        -- Resolving auto-detect at apply_to_config time would
        -- re-introduce exactly the drift this guarantee is supposed
        -- to prevent.
        if type(opts.snapshot_dir) == "string" and opts.snapshot_dir ~= "" then
            if type(resurrect_mod) == "table"
               and type(resurrect_mod.state_manager) == "table"
               and type(resurrect_mod.state_manager.change_state_save_dir)
                   == "function"
            then
                resurrect_mod.state_manager.change_state_save_dir(
                    opts.snapshot_dir .. "/")
            end
        end

        -- Step 5 — Validate runtime_dir SUN_PATH budget. May raise
        -- WEZSESH_RUNTIME_DIR_TYPE / WEZSESH_SUN_PATH_OVERFLOW. Caught
        -- by the outer pcall.
        local manager = require("wezsesh.manager")
        manager.validate_runtime_dir(opts)

        -- Step 6 — Resolve the binary's absolute path and ensure the
        -- §5.2 session key is populated in `wezterm.GLOBAL`.
        local bin = manager.resolve_binary(opts)
        wezterm.GLOBAL.wezsesh_bin_path = bin
        local key, key_err = manager.ensure_session_key(bin)
        if key == nil then
            error(key_err
                  or "WEZSESH_SESSION_KEY_GENERATION_FAILED", 0)
        end

        -- Step 7 — Register the §9.3 user-var-changed handler. Requires
        -- a target_window_id; the binary's spawn (T-602's
        -- `manager.spawn`) captures it from the spawning window. At
        -- apply_to_config time we don't have a live window yet, so the
        -- handler runs with the §3.3 "any window" sentinel `-1` and
        -- relies on the per-pane `state.set_state` record to
        -- discriminate via `session.target_window_id` per
        -- §9.3.1.C / ipc.lua step (g). `-1` is durable: wezterm has
        -- never assigned negative window ids, so the sentinel cannot
        -- collide with a real window id. The earlier `0` placeholder
        -- was empirically wrong — wezterm's first window is `WINID = 0`,
        -- so a keybinding spawned from that window emitted a wire
        -- `target_window_id` of `0` that the §8.6 constructor rejected
        -- with `TargetWindowID must be positive`. See T-DOC-049 / T-905.
        local ipc = require("wezsesh.ipc")
        ipc.register({
            runtime_dir      = opts.runtime_dir or "/tmp/wezsesh",
            target_window_id = opts.target_window_id or -1,
        })

        -- Step 8 — Append the §11.1 keybinding to config.keys. Wraps
        -- M.spawn(window, opts) under pcall internally so a spawn
        -- raise can't wedge the wezterm event loop.
        manager.register_keybinding(config, opts)

        -- Plugin/manager VERSION drift guard. A divergence is a
        -- release-process bug; reporting it loud is cheaper than
        -- chasing a wire-version mismatch later.
        if M.VERSION ~= manager.VERSION then
            error(string.format(
                "WEZSESH_VERSION_DRIFT: init.lua VERSION=%s but " ..
                "manager.lua VERSION=%s — release pin out of sync",
                tostring(M.VERSION), tostring(manager.VERSION)), 0)
        end
    end)

    if not ok then
        maybe_toast(err)
    end
end

return M
