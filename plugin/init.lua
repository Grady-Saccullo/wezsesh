-- The plugin's entry point. Every wezterm config that pulls in wezsesh
-- calls into `M.apply_to_config(config, opts)` from its top-level Lua
-- module. This file MUST stay tiny and disciplined: every raise inside
-- `apply_to_config` is a config-eval explosion the user sees as
-- wezterm refusing to start. We surface plugin failure as a 10s toast,
-- never a config-load explosion.
--
-- Responsibilities (in strict order):
--   1. Bust `package.loaded["wezsesh.*"]` so submodule edits land on
--      `Ctrl+Shift+R` reload — wezterm reloads init.lua via `loadfile`
--      but does NOT re-evaluate cached `require()` results.
--   2. GLOBAL schema-version stamp. Compare
--      `wezterm.GLOBAL.wezsesh_plugin_version` to `M.VERSION`; on
--      mismatch (including downgrade and first-load-nil) wipe every
--      `wezsesh_*` GLOBAL key and re-stamp. MUST run BEFORE any other
--      GLOBAL write or listener registration — otherwise re-init writes
--      get nuked. Handles `wezterm.plugin.update_all()` cleanly with
--      no migration logic.
--   2b. Mint `plugin_session_id` (26-char ULID-shaped string) into
--      `wezterm.GLOBAL.wezsesh_plugin_session_id`. Runs immediately
--      after the wipe so the first log line in this boot already
--      carries the id; re-minted on Ctrl+Shift+R reload (intentional —
--      a reload IS a new logical session for tracing purposes).
--   3. Install the persistent `resurrect.error` listener via
--      `resurrect_error.register()` (backs `with_capture`'s side
--      channel).
--   4. Lock resurrect's `save_state_dir` to wezsesh's `snapshot_dir`
--      (impossible-by-construction over the
--      `snapshot.dir.matches.resurrect` doctor check).
--   5. Validate `opts.runtime_dir` against the SUN_PATH ceiling.
--   5b. Stash `runtime_dir` in `wezterm.GLOBAL.wezsesh_runtime_dir`
--      and `state_dir` in `wezterm.GLOBAL.wezsesh_state_dir` so
--      `runtime/log.lua` can append structured records to
--      `<state_dir>/plugin.log` (next to the binary's wezsesh.log)
--      without each caller threading the path through.
--   5c. Resolve the log level locally (symmetric with Go-side
--      `logger.ResolveLevel`) via
--      `runtime.log.resolve_level(opts.log_level, $WEZSESH_LOG)` and
--      stamp it via `globals.set_log_level` so `runtime/log.lua` can
--      gate record emission at the same threshold the Go side uses.
--      Default "info" when both inputs are nil / invalid.
--   6. Generate / fetch the HMAC session key into
--      `wezterm.GLOBAL.wezsesh_session_key`.
--   7. Register the `user-var-changed` handler via `ipc.register{...}`.
--   8. Register the default keybinding via
--      `manager.register_keybinding`.
--
-- Steps 5–8 mutate global state. Each one MUST be inside the outer
-- pcall so a sentinel raise (`WEZSESH_RUNTIME_DIR_TYPE`,
-- `WEZSESH_SUN_PATH_OVERFLOW`, `WEZSESH_CONFIG_WRITE_FAILED`,
-- `WEZSESH_SESSION_KEY_GENERATION_FAILED`) becomes a toast, not a
-- traceback in `~/.wezterm.lua` eval.
--
-- mlua sandbox:
--   * `local wezterm = require("wezterm")` at file top — never `_G.wezterm`.
--   * No `debug.*`. No `dofile(`. No nested-table values into
--     `wezterm.GLOBAL` (the submodules already enforce this; this file
--     doesn't touch GLOBAL directly).

local wezterm = require("wezterm")

local M = {}

-- Lint pin: bumped per tagged release in lockstep with
-- `manager.lua`'s `M.VERSION`. The `apply_to_config` body asserts
-- equality — a divergence between the two surfaces is a release-process
-- bug that would silently misreport `WEZSESH_PLUGIN_VERSION` over the
-- wire.
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

-- Mutate `opts` so `binary` and `plugin_root` carry the documented
-- precedence: `binary` non-empty wins; otherwise `plugin_root` carries.
-- When only `binary` is set, derive `plugin_root` as
-- `parent_dir(binary)/plugin` so manager.resolve_binary's downstream
-- consistency holds. Empty-string `binary` is treated as absent (matches
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

-- The schema-version stamp + cross-version wipe loop lives in
-- runtime.globals.wipe_on_version_mismatch; init.lua's apply_to_config
-- calls it as step 2 below. Centralising it there keeps every
-- `wezterm.GLOBAL.wezsesh_*` access in one of the two authorised
-- modules (runtime/globals.lua, runtime/state.lua), which the lualint
-- rule `lua-globals-only` enforces.

-- Surface a sentinel-prefixed error as a 10-second toast. Sentinels are
-- emitted by the submodules via `error("WEZSESH_*: …", 0)`. We match
-- on substring (the `error(msg, 0)` form suppresses the file:line
-- prefix, but we cannot rely on that everywhere).
--
-- The error record routes through runtime/log.lua so it lands in
-- plugin.log alongside the wezterm GUI log emission — `wezsesh tail`
-- consumers see apply_to_config sentinel failures without grepping the
-- wezterm log themselves. The require is pcall-wrapped because
-- maybe_toast runs OUTSIDE apply_to_config's pcall body: a load error
-- on runtime/log here must not propagate and crash the wezterm config
-- eval. On require failure we fall back to wezterm.log_error so the
-- toast machinery still preserves at least one structured emission.
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
    local ok_log, log = pcall(require, "wezsesh.runtime.log")
    if ok_log and type(log) == "table" and type(log.error) == "function" then
        pcall(log.error, "apply_to_config: " .. msg)
    elseif type(wezterm.log_error) == "function" then
        pcall(wezterm.log_error, "wezsesh: " .. msg)
    end
end

-- ────────────────────────────────────────────────────────────────────
-- apply_to_config
-- ────────────────────────────────────────────────────────────────────
--
-- The outer body runs inside `pcall`. Any sentinel raise inside is
-- caught and surfaced via `wezterm.toast_notification`; the user's
-- wezterm continues to start.
--
-- The `lua-package-loaded-bust-loop` lint enforces the
-- `package.loaded["wezsesh.*"]` bust loop is present in this function
-- literally. Keep the form below byte-stable.

function M.apply_to_config(config, opts)
    opts = opts or {}

    -- Cache-bust on reload. MUST stay literally as below — the
    -- lualint rule greps for this exact loop body. Sub-8-char prefixes
    -- would match user modules whose names happen to start with
    -- `wezsesh` (e.g. `wezsesh_extras.lua`); the `.` is part of the
    -- contract.
    for k in pairs(package.loaded) do
        if k:sub(1, 8) == "wezsesh." then package.loaded[k] = nil end
    end

    local globals = require("wezsesh.runtime.globals")

    local ok, err = pcall(function()
        -- Reconcile binary/plugin_root precedence.
        opts = reconcile_binary_opts(opts)

        -- Step 2 — GLOBAL schema-version stamp + cross-version wipe.
        -- MUST run BEFORE any other GLOBAL write or listener
        -- registration; the wipe set is `^wezsesh_` keys, so a stamp
        -- written after `ensure_session_key` would itself nuke
        -- `wezsesh_session_key`. Idempotent on same-version reloads
        -- (the wipe loop early-returns when the stored stamp equals
        -- M.VERSION).
        globals.wipe_on_version_mismatch(M.VERSION)

        -- Step 2b — Mint the plugin_session_id. Runs immediately after
        -- the wipe so the very first log line (any module loaded
        -- below) already carries the id. Re-minted on Ctrl+Shift+R
        -- reload because the cache-bust loop above wiped this module
        -- AND the cross-version wipe nuked the previous stamp; that's
        -- the intended semantics — a hot reload IS a new logical
        -- session. Best-effort uniqueness: ulid.mint is NOT
        -- cryptographic, just shaped like a ULID for grep-friendliness.
        local ulid = require("wezsesh.crypto.ulid")
        globals.set_plugin_session_id(ulid.mint())

        -- Step 3 — Install the persistent `resurrect.error` listener.
        -- Idempotent within a single Lua state via the `_G` install gate
        -- in `resurrect_error.lua`; cleanly re-armed on reload because
        -- wezterm rebuilds the Lua state.
        local resurrect_error = require("wezsesh.resurrect_error")
        resurrect_error.register()

        -- Step 4 — Resolve the user's resurrect plugin module and lock
        -- its save_state_dir to opts.snapshot_dir.
        --
        -- The user typically does
        --   `local resurrect = wezterm.plugin.require("…")`
        -- in their wezterm config and passes the resulting table via
        -- `opts.resurrect`. `runtime.resurrect_ref` owns both the stash
        -- and the legacy `_G.resurrect` fallback lookup; calling `set`
        -- with `opts.resurrect` (which may be nil) and then `get` here
        -- lets the verb dispatchers later recover whichever module the
        -- user wired without each consumer re-running the resolution.
        local resurrect_ref = require("wezsesh.runtime.resurrect_ref")
        resurrect_ref.set(opts.resurrect)
        local resurrect_mod = resurrect_ref.get()

        -- When `opts.snapshot_dir` is nil/empty we deliberately do NOT
        -- call `change_state_save_dir`. The "no drift between resurrect
        -- and wezsesh's snapshot directories" guarantee only holds when
        -- the user has pinned the dir on both sides; otherwise
        -- resurrect's own auto-detect path is the canonical resolver,
        -- and `wezsesh doctor`'s snapshot-dir-matches check is the
        -- runtime fallback that catches drift. Resolving auto-detect
        -- at apply_to_config time would re-introduce exactly the drift
        -- this guarantee is meant to prevent.
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

        -- Step 4b — Stash the user-supplied directory-row providers.
        -- Same pattern as resurrect_ref.set: typed runtime accessor,
        -- linter-enforced ownership boundary
        -- (`lua-dir-providers-only`), nil-tolerant. Each provider is
        -- a callable invoked synchronously by the `list_dirs` verb at
        -- TUI startup.
        local dir_providers = require("wezsesh.runtime.dir_providers")
        dir_providers.set(opts.dir_providers or {})

        -- Step 4d — Compute and stash the bootstrap reply body. The
        -- `bootstrap` IPC verb (called by the Go binary at TUI startup)
        -- reads this stash and echoes it as the reply data. We compute
        -- the body once here (when `opts` is in scope) so the dispatch
        -- is a constant-return; mirrors the dir_providers stash idiom.
        -- Hoisted `local manager` to here (step 5 reuses it).
        local manager = require("wezsesh.manager")
        local bootstrap_body = require("wezsesh.runtime.bootstrap_body")
        bootstrap_body.set(manager.build_bootstrap_body(opts))

        -- Step 4c — Build the on_pane_restore argv-allowlist policy and
        -- install it via `on_pane_restore.configure`. Until this runs,
        -- the module's default policy denies every program (fail-CLOSED),
        -- so a snapshot restore would land in the cwd-only / newline
        -- branch — no scrollback, no respawn. The policy union is:
        --
        --   default_allowlist  (codegen'd from internal/argvallow/default.txt)
        --   ∪ basename($SHELL)  (login shell almost always present, but
        --                        belt-and-braces in case the user's shell
        --                        was renamed off the default list)
        --   ∪ opts.resurrect_argv_allowlist  (user additions; passes
        --                        through to the binary too)
        --
        -- The lookup is a hashed set; `allows` returns the boolean
        -- presence flag. This module is required directly (not through
        -- a runtime/ accessor) because it owns its own deps state and
        -- is exempt from the runtime/ ownership rules — same shape as
        -- `manager.lua` and `ipc.lua`.
        local on_pane_restore = require("wezsesh.on_pane_restore")
        local policy_set = {}
        local default_allowlist = require("wezsesh.default_allowlist")
        for _, name in ipairs(default_allowlist) do
            policy_set[name] = true
        end
        local shell = os.getenv("SHELL")
        if type(shell) == "string" and shell ~= "" then
            local last = shell:match("([^/]+)$") or shell
            policy_set[last] = true
        end
        if type(opts.resurrect_argv_allowlist) == "table" then
            for _, name in ipairs(opts.resurrect_argv_allowlist) do
                if type(name) == "string" and name ~= "" then
                    policy_set[name] = true
                end
            end
        end
        on_pane_restore.configure({
            resurrect = function() return resurrect_ref.get() end,
            policy = {
                allows = function(prog)
                    return policy_set[prog] == true
                end,
            },
        })

        -- Step 5 — Validate runtime_dir SUN_PATH budget. May raise
        -- WEZSESH_RUNTIME_DIR_TYPE / WEZSESH_SUN_PATH_OVERFLOW. Caught
        -- by the outer pcall. (`manager` was hoisted in step 4d above.)
        manager.validate_runtime_dir(opts)

        -- Step 5b — Stash the runtime_dir for downstream consumers
        -- (IPC sockets + req/ sidecars). The path has just been
        -- validated against the SUN_PATH ceiling AND the binary
        -- side runs `manager.validate_runtime_dir` / `safefs.Enforce`
        -- on it before the binary spawns, so it is safe to write
        -- through without re-running symlink checks here. Falls
        -- through to the `/tmp/wezsesh` default when the user didn't
        -- pin a path — same default as ipc.register below.
        globals.set_runtime_dir(opts.runtime_dir or "/tmp/wezsesh")

        -- Stash the state_dir alongside, mirroring the binary's
        -- autoStateDir (`internal/config/autodetect.go`): XDG-style
        -- on both Linux and macOS, since the wezsesh state files
        -- (state.json, the rotated wezsesh.log set, and now
        -- plugin.log) live in one well-known location that
        -- `wezsesh tail` resolves the same way from a fresh shell —
        -- no env coordination required.
        local function default_state_dir()
            local xdg = os.getenv("XDG_STATE_HOME")
            if type(xdg) == "string" and xdg ~= "" then
                return xdg .. "/wezsesh"
            end
            local home = (wezterm.home_dir or os.getenv("HOME") or "")
            return home .. "/.local/state/wezsesh"
        end
        local state_dir = opts.state_dir
        if type(state_dir) ~= "string" or state_dir == "" then
            state_dir = default_state_dir()
        end
        globals.set_state_dir(state_dir)

        -- Step 5c — Resolve the log level locally (symmetric with the
        -- Go side's `logger.ResolveLevel`). Both sides see the same
        -- parent env at wezterm-launch time, so the resolved level
        -- matches without any disk-sidecar coordination. Algorithm:
        -- the more verbose (lower-numeric) of `opts.log_level` and
        -- `$WEZSESH_LOG`. Unrecognised inputs fall through to "info"
        -- — same default as the Go side's parseLevel.
        local log_runtime = require("wezsesh.runtime.log")
        globals.set_log_level(
            log_runtime.resolve_level(opts.log_level, os.getenv("WEZSESH_LOG"))
        )

        -- Step 6 — Resolve the binary's absolute path and ensure the
        -- HMAC session key is populated in wezterm.GLOBAL via
        -- runtime/globals.
        local bin = manager.resolve_binary(opts)
        globals.set_bin_path(bin)
        local key, key_err = manager.ensure_session_key(bin)
        if key == nil then
            error(key_err
                  or "WEZSESH_SESSION_KEY_GENERATION_FAILED", 0)
        end

        -- Step 7 — Register the user-var-changed handler. Requires
        -- a target_window_id; `manager.spawn` captures it from the
        -- spawning window. At apply_to_config time we don't have a
        -- live window yet, so the handler runs with the "any window"
        -- sentinel `-1` and relies on the per-pane `state.set_state`
        -- record to discriminate via `session.target_window_id`. `-1`
        -- is durable: wezterm has never assigned negative window ids,
        -- so the sentinel cannot collide with a real window id. The
        -- earlier `0` placeholder was empirically wrong — wezterm's
        -- first window is `WINID = 0`, so a keybinding spawned from
        -- that window emitted a wire `target_window_id` of `0` that
        -- the binary's payload constructor rejected with
        -- `TargetWindowID must be positive`.
        local ipc = require("wezsesh.ipc")
        ipc.register({
            runtime_dir      = opts.runtime_dir or "/tmp/wezsesh",
            target_window_id = opts.target_window_id or -1,
        })

        -- Step 8 — Append the default keybinding to config.keys. Wraps
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
