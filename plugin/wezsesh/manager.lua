-- Owns the wezsesh-binary lifecycle on the Lua side:
--
--   * resolve_binary       — pick the wezsesh executable (string).
--   * detect_version       — exec `<bin> version` and parse semver.
--   * compatible           — same-major semver gate.
--   * ensure_session_key   — generate or load the HMAC session key.
--   * validate_runtime_dir — SUN_PATH ceiling check.
--   * write_config_file    — JSON config to a temp file.
--   * spawn                — env-vector + mux/tab spawn.
--   * register_keybinding  — default keybinding registration.
--
-- mlua sandbox: acquired via `local wezterm = require("wezterm")`. The
-- standalone spec installs a harness double via
-- `package.preload["wezterm"]` BEFORE requiring this file.
--
-- The spawn path uses `wezterm.mux.spawn_window` and
-- `mux_window:spawn_tab`, NOT `wezterm.background_child_process`, so
-- this file does not contain any background_child_process call. The
-- spec includes a static-text assertion guarding that property
-- (any future bg-spawn here would also need the
-- `lua-bg-child-process-pcall` lint to keep enforcing pcall-wrap).

local wezterm = require("wezterm")
local globals = require("wezsesh.runtime.globals")

local M = {}

-- Plugin version. Single source of truth for the wire-protocol
-- `plugin_version` field and the `WEZSESH_PLUGIN_VERSION` env var.
M.VERSION = "0.1.0"

-- ────────────────────────────────────────────────────────────────────
-- helpers
-- ────────────────────────────────────────────────────────────────────

local function trim(s)
    if type(s) ~= "string" then return "" end
    return (s:gsub("^%s+", ""):gsub("%s+$", ""))
end

-- Parse a semver-shaped string into {major,minor,patch}. Returns nil
-- on malformed input. Trailing pre-release / build metadata accepted
-- (e.g. `1.2.3-rc1+abc`) but ignored beyond the leading three numbers.
local function parse_semver(s)
    if type(s) ~= "string" then return nil end
    local mj, mn, pt = s:match("^(%d+)%.(%d+)%.(%d+)")
    if mj == nil then return nil end
    return {
        major = tonumber(mj),
        minor = tonumber(mn),
        patch = tonumber(pt),
    }
end

-- Path join: trim a single trailing slash from `dir`, then concatenate.
-- Pure string ops; no filesystem syscalls.
local function path_join(dir, file)
    if type(dir) ~= "string" or dir == "" then return file end
    if dir:sub(-1) == "/" then return dir .. file end
    return dir .. "/" .. file
end

-- Tempdir picker. Lua's `os.tmpdir()` doesn't exist; emulate by
-- consulting `$TMPDIR` (POSIX), falling back to `/tmp`.
local function tmpdir()
    local t = os.getenv("TMPDIR")
    if type(t) == "string" and t ~= "" then
        -- strip trailing slash for consistency
        if t:sub(-1) == "/" then t = t:sub(1, -2) end
        return t
    end
    return "/tmp"
end

-- ────────────────────────────────────────────────────────────────────
-- resolve_binary
-- ────────────────────────────────────────────────────────────────────

function M.resolve_binary(opts)
    opts = opts or {}
    if type(opts.binary) == "string" and opts.binary ~= "" then
        return opts.binary
    end
    if type(opts.plugin_root) == "string" and opts.plugin_root ~= "" then
        return path_join(opts.plugin_root, "wezsesh")
    end
    -- Documented default: bare `"wezsesh"` and let PATH resolve at
    -- exec time.
    return "wezsesh"
end

-- ────────────────────────────────────────────────────────────────────
-- detect_version
-- ────────────────────────────────────────────────────────────────────

function M.detect_version(bin)
    if type(bin) ~= "string" or bin == "" then return "missing" end
    local ok, stdout, _stderr =
        wezterm.run_child_process({ bin, "version" })
    if not ok then return "missing" end
    local out = trim(stdout or "")
    if out == "" then return "unparseable" end
    -- Accept `M.m.p` or `M.m.p<extra>` (no whitespace inside).
    if not out:match("^%d+%.%d+%.%d+%S*$") then
        return "unparseable"
    end
    return out
end

-- ────────────────────────────────────────────────────────────────────
-- compatible
-- ────────────────────────────────────────────────────────────────────
--
-- Spec is silent on the exact rule. Choice (documented): same-major
-- semver match. Both inputs MUST parse; missing / unparseable arms
-- return false so the apply_to_config caller surfaces the doctor toast.

function M.compatible(plugin_v, bin_v)
    local p = parse_semver(plugin_v)
    local b = parse_semver(bin_v)
    if p == nil or b == nil then return false end
    return p.major == b.major
end

-- ────────────────────────────────────────────────────────────────────
-- ensure_session_key
-- ────────────────────────────────────────────────────────────────────
--
-- Chain:
--   1. exec `<bin> keygen` → 64 hex on stdout.
--   2. fallback: read 32 bytes from /dev/urandom, hex-encode.
--   3. fallback: return nil (caller logs + early-returns; the listener
--      no-ops on a nil session_key).
--
-- Validation: trimmed output MUST match `^%x+$` and have length 64.
-- Stores in `wezterm.GLOBAL.wezsesh_session_key` on success. Never
-- raises — a raise here would wedge `apply_to_config`.

local function valid_hex_64(s)
    if type(s) ~= "string" then return false end
    if #s ~= 64 then return false end
    return s:match("^%x+$") ~= nil
end

local function bytes_to_hex(bytes)
    if type(bytes) ~= "string" then return nil end
    local parts = {}
    for i = 1, #bytes do
        parts[i] = string.format("%02x", bytes:byte(i))
    end
    return table.concat(parts)
end

function M.ensure_session_key(bin)
    -- Reuse the existing key if one is already stored in GLOBAL. The
    -- binary's WEZSESH_HMAC_KEY env is frozen at spawn-time, so any
    -- regeneration here invalidates every in-flight TUI's HMAC and
    -- causes silent IPC drops at step (f). apply_to_config can re-run
    -- (config auto-reload, plugin updates, user calls); only the very
    -- first call should mint a key.
    local existing = globals.session_key()
    if valid_hex_64(existing) then
        return existing
    end

    -- Step 1: exec the binary's keygen subcommand.
    if type(bin) == "string" and bin ~= "" then
        local ok, stdout, _stderr =
            wezterm.run_child_process({ bin, "keygen" })
        if ok then
            local hex = trim(stdout or "")
            if valid_hex_64(hex) then
                globals.set_session_key(hex)
                return hex
            end
        end
    end

    -- Step 2: fallback to /dev/urandom (POSIX-only build matrix).
    -- io.open returns nil + errmsg on failure.
    local f = io.open("/dev/urandom", "rb")
    if f ~= nil then
        local raw = f:read(32)
        f:close()
        if type(raw) == "string" and #raw == 32 then
            local hex = bytes_to_hex(raw)
            if valid_hex_64(hex) then
                globals.set_session_key(hex)
                return hex
            end
        end
    end

    -- Step 3: hard fail. Caller toasts + early-returns.
    return nil, "WEZSESH_SESSION_KEY_GENERATION_FAILED"
end

-- ────────────────────────────────────────────────────────────────────
-- validate_runtime_dir (Lua side SUN_PATH ceiling check)
-- ────────────────────────────────────────────────────────────────────
--
-- Sentinel errors are raised via `error(msg, 0)` so the file:line
-- prefix is suppressed and the caller can match the sentinel
-- substring directly.
--
-- When `opts.runtime_dir` is nil the auto-detect path applies; there's
-- nothing to validate. Return silently.

function M.validate_runtime_dir(opts)
    opts = opts or {}
    if opts.runtime_dir == nil then return end

    if type(opts.runtime_dir) ~= "string" then
        error("WEZSESH_RUNTIME_DIR_TYPE: opts.runtime_dir must be a string path", 0)
    end

    local expanded = opts.runtime_dir
    if expanded:sub(1, 2) == "~/" then
        expanded = (wezterm.home_dir or os.getenv("HOME") or "") .. expanded:sub(2)
    end

    local triple = wezterm.target_triple or ""
    local ceiling = (triple:match("%-apple%-darwin") and 104) or 108
    local needed = #expanded + 14   -- "/<8hex>.sock"

    if needed > ceiling then
        error(string.format(
            "WEZSESH_SUN_PATH_OVERFLOW: runtime_dir too long for AF_UNIX SUN_PATH " ..
            "(needed=%d, ceiling=%d, path=%q). Shorten or use the default.",
            needed, ceiling, expanded), 0)
    end
end

-- ────────────────────────────────────────────────────────────────────
-- write_config_file
-- ────────────────────────────────────────────────────────────────────
--
-- Builds the JSON config shape and writes it under
-- `<tmp>/wezsesh-<pid>-config.json`. Returns the absolute path.
--
-- pid sourcing: the wezterm mlua sandbox does not expose `os.getpid`,
-- and the mode-0600 contract is aspirational from Lua (we cannot
-- guarantee 0600 in pure Lua; the file is written via `io.open` which
-- honours the process umask). We use `wezterm.procinfo.pid` when
-- available, else a process-wide monotonic counter combined with
-- `os.time()` for collision avoidance.

local _config_seq = 0

local function next_config_filename()
    _config_seq = _config_seq + 1
    local pid_like
    local ok, pi = pcall(function() return wezterm.procinfo end)
    if ok and type(pi) == "table" and type(pi.pid) == "function" then
        local pok, p = pcall(pi.pid)
        if pok and type(p) == "number" then
            pid_like = tostring(p)
        end
    end
    if pid_like == nil then
        pid_like = tostring(os.time()) .. "-" .. tostring(_config_seq)
    end
    return string.format("wezsesh-%s-config.json", pid_like)
end

-- Default keys table — copied here so write_config_file is a pure
-- function of `opts`. apply_to_config merges user overrides into this
-- table before calling us.
local DEFAULT_KEYS = {
    switch = "s", load = "l", rename = "r", delete = "d",
    save = "S", new = "n", pin = "p", tag = "t",
    mark = "Tab", mark_alt = "Space", clear_marks = "c",
    help = "?", filter = "/", quit = "q",
    up = "k", down = "j", top = "gg", bottom = "G",
}

local function copy_keys(t)
    local out = {}
    for k, v in pairs(t) do out[k] = v end
    return out
end

-- Tag a Lua table so wezterm.json_encode emits a JSON array. wezterm's
-- encoder treats an empty Lua table as `{}` (object) by default; the
-- config schema requires `[]` (array) for `resurrect_argv_allowlist`.
-- The accessor `wezterm.json_array_metatable` is the documented hook.
-- When it isn't installed (older wezterm builds, the spec shim with
-- json_array_metatable absent), an explicitly-empty caller table would
-- still encode as `{}`; the caller code below handles that case by
-- substituting the non-empty default_allowlist (which encodes as `[]`
-- via the encoder's array-detection on positive-integer keys).
local function as_json_array(t)
    if type(t) ~= "table" then return t end
    local mt = wezterm.json_array_metatable
    if mt ~= nil then
        local copy = {}
        for i = 1, #t do copy[i] = t[i] end
        return setmetatable(copy, mt)
    end
    return t
end

-- `resurrect_argv_allowlist` resolver. Returns a table that will
-- encode as a JSON array `[]` (never `{}`).
--
--   * nil  → default_allowlist (non-empty array; encodes as `[…]`).
--   * `{}` → tagged via json_array_metatable when available, else
--            default_allowlist (callers wanting truly-empty must run on
--            a wezterm build that exposes json_array_metatable).
--   * non-empty table → tagged when available; passes through
--            otherwise (encoder's positive-integer-keys detection
--            handles it).
local function resolve_argv_allowlist(v)
    if v == nil then
        return require("wezsesh.default_allowlist")
    end
    if type(v) == "table" then
        local n = 0
        for _ in pairs(v) do n = n + 1 end
        if n == 0 and wezterm.json_array_metatable == nil then
            return require("wezsesh.default_allowlist")
        end
        return as_json_array(v)
    end
    return v
end

function M.write_config_file(opts)
    opts = opts or {}

    -- Build the config body. Every top-level key the binary expects is
    -- emitted unconditionally so the binary's `config.Load` can rely
    -- on a stable shape even when the user hasn't set the field.
    local body = {
        version          = 1,
        snapshot_dir     = opts.snapshot_dir or "",
        state_dir        = opts.state_dir or "",
        runtime_dir      = opts.runtime_dir or "",
        data_dir         = opts.data_dir or "",
        log_level        = opts.log_level or "info",
        sort             = opts.sort or "live_first",
        default_action   = opts.default_action or "switch",
        default_action_load_no_prompt =
            opts.default_action_load_no_prompt == true,
        confirm_delete    = opts.confirm_delete ~= false,
        confirm_overwrite = opts.confirm_overwrite ~= false,
        exclude          = opts.exclude or { "^default$" },
        new_workspace_command = opts.new_workspace_command,
        preview          = opts.preview or { enabled = true, width = 0.4 },
        markers          = opts.markers or {
            active = "▶", live = "●", marked = "✓",
            unsaved = "(unsaved)", pinned = "[pinned]",
        },
        columns          = opts.columns or
            { "marker", "name", "tabs", "age", "tags" },
        name_truncate    = opts.name_truncate or "middle",
        colors           = opts.colors or {
            accent = nil, muted = nil, error = nil,
            success = nil, focus_bg = nil,
            match_highlight = nil, live_marker = nil,
            saved_marker = nil,
        },
        hooks            = opts.hooks or {
            run_hooks = true,
            prompt_on_untrusted = false,
            timeout_seconds = 600,
        },
        resurrect_argv_allowlist = resolve_argv_allowlist(
            opts.resurrect_argv_allowlist),
        keys             = opts.keys or copy_keys(DEFAULT_KEYS),
        plugin_version   = M.VERSION,
        proto_version    = 1,
    }

    local path = path_join(tmpdir(), next_config_filename())
    local encoded = wezterm.json_encode(body)
    local f, err = io.open(path, "wb")
    if f == nil then
        error(string.format(
            "WEZSESH_CONFIG_WRITE_FAILED: %s (%s)",
            tostring(err), path), 0)
    end
    f:write(encoded)
    f:close()
    return path
end

-- ────────────────────────────────────────────────────────────────────
-- spawn
-- ────────────────────────────────────────────────────────────────────
--
-- Builds the env vector EXACTLY:
--   WEZSESH_HMAC_KEY, WEZSESH_PROTO_VERSION, WEZSESH_CONFIG_FILE,
--   WEZSESH_PLUGIN_VERSION
-- — no more, no less. Dirs travel inside `WEZSESH_CONFIG_FILE`.
--
-- spawn_mode dispatch:
--   "window" → wezterm.mux.spawn_window{ args=…, set_environment_variables=… }
--   "tab"   → window:mux_window():spawn_tab{ … } (default; the GUI
--             Window userdata exposes `:mux_window()` for that
--             resolution)
--
-- The HMAC key is read from `wezterm.GLOBAL.wezsesh_session_key`;
-- ensure_session_key MUST have populated it before spawn. If absent,
-- we early-return without spawning so the caller can surface a doctor
-- toast (a missing key would mean every subsequent OSC is silently
-- dropped at HMAC verify).

function M.spawn(window, opts)
    opts = opts or {}

    local key = globals.session_key()
    if type(key) ~= "string" or #key ~= 64 then
        if type(wezterm.log_error) == "function" then
            wezterm.log_error(
                "wezsesh: spawn aborted — wezsesh_session_key missing or malformed")
        end
        return nil, "WEZSESH_SESSION_KEY_MISSING"
    end

    local config_path = M.write_config_file(opts)
    local bin = M.resolve_binary(opts)

    local env = {
        WEZSESH_HMAC_KEY       = key,
        WEZSESH_PROTO_VERSION  = "1",
        WEZSESH_CONFIG_FILE    = config_path,
        WEZSESH_PLUGIN_VERSION = M.VERSION,
    }

    -- macOS GUI launch contexts hand spawned children a minimal launchd
    -- PATH (/usr/bin:/bin:/usr/sbin:/sbin), which doesn't include the
    -- wezterm CLI itself even though wezterm.app *is* the parent. Inject
    -- wezterm.executable_dir so the binary's `exec.LookPath("wezterm")`
    -- resolves. Fall back to inheriting the parent's PATH if the
    -- accessor isn't available.
    local exe_dir = type(wezterm.executable_dir) == "string"
        and wezterm.executable_dir or nil
    local parent_path = os.getenv("PATH") or "/usr/bin:/bin:/usr/sbin:/sbin"
    if exe_dir and exe_dir ~= "" then
        env.PATH = exe_dir .. ":" .. parent_path
    else
        env.PATH = parent_path
    end

    local args = { bin }
    local mode = opts.spawn_mode or "tab"

    -- Record the spawn so the ipc handler's first step recognises the
    -- binary's pane and accepts its OSCs. Without this, every fresh OSC
    -- silently drops at `state.get_state(pane_id) == nil` and the binary
    -- observes IPC_TIMEOUT.
    local function record_spawn(spawned_pane, spawned_window)
        if spawned_pane == nil then return end
        local ok_pid, pid = pcall(function() return spawned_pane:pane_id() end)
        if not ok_pid or type(pid) ~= "number" then return end
        local wid = -1
        if spawned_window ~= nil then
            local ok_w, w = pcall(function()
                return spawned_window:window_id()
            end)
            if ok_w and type(w) == "number" then wid = w end
        end
        local state = require("wezsesh.runtime.state")
        state.set_state(pid, {
            target_window_id = wid,
            spawned_at       = os.time(),
        })
    end

    if mode == "window" then
        local tab, pane, mux_win = wezterm.mux.spawn_window {
            args = args,
            set_environment_variables = env,
            cwd = opts.cwd,
            workspace = opts.workspace,
        }
        record_spawn(pane, mux_win)
        return tab, pane, mux_win
    end

    -- "tab" (default). Calls `current_window:spawn_tab{...}` where
    -- `current_window` is the MUX window. The GUI `Window` userdata
    -- the keybinding receives exposes `:mux_window()` for that
    -- resolution; `Pane:spawn_tab` does NOT exist in current wezterm
    -- builds.
    if window == nil then
        return nil, "WEZSESH_SPAWN_NO_WINDOW"
    end
    local mux_window = window:mux_window()
    local tab, pane, mux_win = mux_window:spawn_tab {
        args = args,
        set_environment_variables = env,
        cwd = opts.cwd,
    }
    record_spawn(pane, mux_win or mux_window)
    return tab, pane, mux_win
end

-- ────────────────────────────────────────────────────────────────────
-- register_keybinding
-- ────────────────────────────────────────────────────────────────────
--
-- Append a `{key, mods, action}` entry to `config.keys`, initialising
-- the array as an empty table when absent. The action is a
-- `wezterm.action_callback` wrapper that calls `M.spawn(window, opts)`
-- under `pcall` — `apply_to_config` is responsible for catching the
-- pcall return and toasting if needed; here we keep the binding
-- crash-isolated from the wezterm event loop.

function M.register_keybinding(config, opts)
    opts = opts or {}
    config = config or {}
    if type(config.keys) ~= "table" then config.keys = {} end

    local kb = opts.keybinding or { key = "W", mods = "LEADER|SHIFT" }

    -- Idempotency. apply_to_config can re-run (config auto-reload,
    -- plugin updates, user code calling it again) and table.insert
    -- without a presence check would stack N copies of the binding,
    -- each spawning its own TUI on a single LEADER+SHIFT+W. Scan the
    -- existing entries and skip if our (key, mods) tuple is already
    -- registered. We only own that tuple — the user owns the rest of
    -- config.keys.
    for _, entry in ipairs(config.keys) do
        if type(entry) == "table"
           and entry.key == kb.key
           and entry.mods == kb.mods
        then
            return config
        end
    end

    table.insert(config.keys, {
        key    = kb.key,
        mods   = kb.mods,
        action = wezterm.action_callback(function(window, _pane)
            local ok, err = pcall(M.spawn, window, opts)
            if not ok and type(wezterm.log_error) == "function" then
                wezterm.log_error(
                    "wezsesh: spawn keybinding failed: " .. tostring(err))
            end
        end),
    })
    return config
end

return M
