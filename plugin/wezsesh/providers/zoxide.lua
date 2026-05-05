-- providers/zoxide.lua — bundled directory-row provider that shells
-- out to `zoxide query -l` and feeds the resulting paths into
-- `list_dirs`'s reply row stream. This is the only provider wezsesh
-- ships out of the box; users wanting `fd`, project files, or ad-hoc
-- enumerators write their own callable and pass it via
-- `opts.dir_providers` at `apply_to_config` time.
--
-- Usage:
--     local zoxide = require("wezsesh.providers.zoxide")
--     wezsesh.apply_to_config(config, {
--         dir_providers = { zoxide() },
--     })
--
-- Module shape: a constructor `function(opts) -> provider_fn`. The
-- returned `provider_fn(query)` is the actual thing that
-- `runtime.dir_providers.invoke_all` calls. Constructor opts:
--
--   opts.binary  (default "zoxide")  — executable name; the user can
--                                      point at an absolute path or a
--                                      different binary if they have a
--                                      drop-in replacement.
--   opts.limit   (default 200)       — cap on the number of rows
--                                      returned. zoxide's database
--                                      can grow into the thousands;
--                                      the picker doesn't need them
--                                      all at once.
--
-- Failure modes: if `wezterm.run_child_process` fails (binary missing,
-- non-zero exit, anything raised by the wezterm side), we log a warn
-- via `runtime.log` and return `{}`. Every failure is non-fatal — the
-- picker continues to render the live + saved rows it already has.
--
-- The lualint rule `lua-providers-vendor-only` enforces that this
-- module may only `require("wezterm")` and
-- `require("wezsesh.runtime.log")`. Anything else (state, result,
-- runtime/globals, ...) is structurally forbidden — providers are a
-- thin extension surface, not an entry into plugin internals.

local wezterm = require("wezterm")
local log     = require("wezsesh.runtime.log")

local DEFAULT_BINARY = "zoxide"
local DEFAULT_LIMIT  = 200

-- Derive the basename of an absolute path. Pure string ops; mirrors
-- Go's `filepath.Base` for forward-slash paths. Returns the input
-- unchanged when the path has no slash (already a bare name).
local function basename(p)
    if type(p) ~= "string" or #p == 0 then return "" end
    -- Strip a trailing slash so `/foo/bar/` → "bar", not "".
    local trimmed = p:gsub("/+$", "")
    if trimmed == "" then return "/" end
    local base = trimmed:match("([^/]+)$")
    return base or trimmed
end

-- Split `output` (the captured stdout from zoxide) into per-line
-- entries. Trims the trailing CR if zoxide ever emits CRLF (it
-- doesn't on Unix today, but the cost is minimal). Empty lines are
-- skipped — a blank line in the middle of zoxide's output would be a
-- bug we don't want to surface as a row with `path = ""`.
local function parse_lines(output)
    if type(output) ~= "string" or #output == 0 then return {} end
    local out = {}
    -- The trailing newline causes the final segment to be empty; the
    -- skip-empty filter handles it.
    for line in (output .. "\n"):gmatch("([^\n]*)\n") do
        local trimmed = line:gsub("\r$", "")
        if #trimmed > 0 then
            out[#out + 1] = trimmed
        end
    end
    return out
end

local M = {}

-- Construct a zoxide-backed provider. Returns a `function(query)`
-- suitable for direct inclusion in `opts.dir_providers`. The query is
-- currently ignored — zoxide's own `query` subcommand is for ranked
-- single-best-match lookup, not list-with-filter; the TUI does its own
-- substring filter over the unioned row set, so we feed it the full
-- `query -l` dump and let the picker handle it.
function M.__call(_self, opts)
    return M.new(opts)
end

-- Public alias for the constructor; the module is callable as both
-- `zoxide()` and `zoxide.new()` so users who store the require result
-- as a local can pick whichever spelling they prefer. Tests use the
-- explicit `.new` form to avoid metatable shenanigans.
function M.new(opts)
    opts = opts or {}
    local binary = opts.binary
    if type(binary) ~= "string" or binary == "" then
        binary = DEFAULT_BINARY
    end
    local limit = opts.limit
    if type(limit) ~= "number" or limit < 0 then
        limit = DEFAULT_LIMIT
    end

    return function(_query)
        if type(wezterm.run_child_process) ~= "function" then
            log.warn("providers.zoxide: wezterm.run_child_process " ..
                "unavailable; degrading to empty")
            return {}
        end

        -- Spawn through `$SHELL -c` rather than execing `binary` bare.
        -- This is the same pattern `internal/pathpicker.Resolve` uses on
        -- the Go side: macOS GUI wezterm.app inherits launchd's minimal
        -- PATH (`/usr/bin:/bin:/usr/sbin:/sbin`), so a direct
        -- `wezterm.run_child_process({"zoxide", ...})` fails with
        -- ENOENT for users who installed zoxide via Nix profile,
        -- Homebrew, Cargo, etc. Going through the user's login shell
        -- means `/etc/zshenv` (Nix-darwin) /  `/etc/zprofile` /
        -- `~/.zshenv` are sourced first, and `zoxide` resolves from the
        -- enriched PATH the user already configured.
        --
        -- Falling back to `/bin/sh` for the rare case where SHELL is
        -- unset matches `pathpicker.Resolve`'s behaviour and avoids
        -- hard-coding an absolute path to the binary (Nix store paths
        -- rotate; Homebrew paths differ on Intel vs Apple Silicon).
        local shell = os.getenv("SHELL")
        if type(shell) ~= "string" or shell == "" then
            shell = "/bin/sh"
        end
        local cmdline = binary .. " query -l"

        -- Production wezterm.run_child_process returns
        -- `(success, stdout, stderr)`. Pre-mlua-7 builds raised on a
        -- non-zero exit; modern builds return `success=false`. We pcall
        -- to be safe and treat any failure mode as "degrade to empty".
        local ok, success, stdout, stderr =
            pcall(wezterm.run_child_process, { shell, "-c", cmdline })
        if not ok then
            log.warn("providers.zoxide: run_child_process raised: "
                .. tostring(success))
            return {}
        end
        if success == false then
            log.warn("providers.zoxide: " .. shell .. " -c '" .. cmdline
                .. "' exited non-zero: "
                .. tostring(stderr or "<no stderr>"))
            return {}
        end

        local lines = parse_lines(stdout)
        local out = {}
        for i = 1, #lines do
            if #out >= limit then break end
            local p = lines[i]
            out[#out + 1] = { path = p, name = basename(p) }
        end
        return out
    end
end

-- Make the module table itself callable so `zoxide()` works without
-- the user reaching for `.new`. Keeps the docs example one line
-- shorter.
setmetatable(M, { __call = M.__call })

return M
