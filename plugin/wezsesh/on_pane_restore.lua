-- on_pane_restore.lua — the wezsesh-installed callback that wraps
-- resurrect's `default_on_pane_restore`. Resurrect calls `cb(pane_tree)`
-- with a single argument; the pane is accessed as `pane_tree.pane`.
--
-- Decision flow:
--
--   1. argv = pane_tree.process and pane_tree.process.argv
--   2. if not argv or #argv == 0:
--          resurrect.default_on_pane_restore(pane_tree); return
--   3. prog = basename(argv[1])
--   4. if not policy.allows(prog):
--          send_cwd_or_newline(pane_tree); log; return
--   5. for each elem in argv: if not bytes_clean(elem) → goto step 4
--   6. if pane_tree.cwd and not bytes_clean(pane_tree.cwd) → goto step 4
--   7. resurrect.default_on_pane_restore(pane_tree)
--
-- The entire decision-flow body is `pcall`-wrapped. Fail-CLOSED on any
-- uncaught error: `pane:send_text("\r\n")` only, log the crash, MUST
-- NOT call `resurrect.default_on_pane_restore` (which would honour the
-- attacker-controlled argv).
--
-- argv indexing: 1-based. `argv[1]` IS the program (NOT the first arg),
-- opposite of C-convention `argv[0]`.
--
-- Test seam: `M.configure({ resurrect = …, policy = … })` swaps the
-- resolved deps. Production resolution defers to `runtime.resurrect_ref`
-- (which owns the `opts.resurrect` / `_G.resurrect` lookup rule) and
-- builds `policy` from `default_allowlist.lua` + `basename($SHELL)` +
-- `opts.resurrect_argv_allowlist` at apply_to_config time.
--
-- Logging routes through `runtime.log` so capture in tests goes through
-- a single seam rather than this module's own `_deps.logger`.

local wezterm       = require("wezterm")
local log           = require("wezsesh.runtime.log")
local resurrect_ref = require("wezsesh.runtime.resurrect_ref")

local M = {}

-- ────────────────────────────────────────────────────────────────────
-- bytes_clean
-- ────────────────────────────────────────────────────────────────────
--
-- Rejects empty, non-string, and any string containing a byte in
-- 0x00–0x1F or 0x7F. The pattern uses `%z` for NUL and `\1-\31` for
-- the rest of the C0 range, plus `\127` for DEL.
--
-- Operates byte-by-byte; does NOT assume valid UTF-8. (Snapshot bytes
-- are attacker-controlled.)

function M.bytes_clean(s)
    if type(s) ~= "string" or #s == 0 then return false end
    return s:find("[%z\1-\31\127]") == nil
end

-- ────────────────────────────────────────────────────────────────────
-- helpers
-- ────────────────────────────────────────────────────────────────────

-- Last segment after the final `/`. Returns the input unchanged when
-- there is no `/`. Pure string op; no syscalls.
local function basename(p)
    if type(p) ~= "string" or p == "" then return "" end
    local last = p:match("([^/]+)$")
    return last or p
end

-- ────────────────────────────────────────────────────────────────────
-- default deps + configure
-- ────────────────────────────────────────────────────────────────────
--
-- The defaults lazy-resolve at call time. Production wiring populates
-- `policy` via apply_to_config; until that happens, the default policy
-- denies everything (fail-CLOSED) — a snapshot restore before
-- configure() runs lands in the cwd-only / newline branch rather than
-- falling through to resurrect's default.

local function default_resurrect()
    -- runtime.resurrect_ref owns the lookup rule (opts.resurrect
    -- stashed by init.lua, fallback to _G.resurrect for legacy
    -- wiring). Routing through it here keeps the resolution logic in
    -- exactly one place.
    return resurrect_ref.get()
end

local function default_policy()
    return { allows = function(_prog) return false end }
end

M._deps = {
    resurrect = default_resurrect,
    policy    = default_policy(),
}

-- Configure: install the resurrect reference and the policy. Called
-- from `apply_to_config` once `policy` has been built from
-- `default_allowlist.lua` + `basename($SHELL)` + user additions.
--
-- `policy` MUST expose an `allows(prog)` predicate.
function M.configure(opts)
    opts = opts or {}
    if opts.resurrect ~= nil then
        M._deps.resurrect = opts.resurrect
    end
    if opts.policy ~= nil then
        M._deps.policy = opts.policy
    end
end

-- Reset to factory defaults. Called by the spec between tests so a
-- previous test's `policy` injection does not bleed across.
function M._reset_deps()
    M._deps = {
        resurrect = default_resurrect,
        policy    = default_policy(),
    }
end

local function resolve_resurrect()
    local r = M._deps.resurrect
    if type(r) == "function" then return r() end
    return r
end

local function resolve_policy()
    return M._deps.policy
end

-- ────────────────────────────────────────────────────────────────────
-- send_cwd_or_newline (step 4 action)
-- ────────────────────────────────────────────────────────────────────
--
-- Line terminator is `\r\n` (CRLF — what wezterm injects into a real
-- pane on the user pressing Enter). The quoter is
-- `wezterm.shell_quote_arg`; its only failure mode (embedded NUL) is
-- precluded by step 6 / the per-elem bytes_clean check above.

local function send_cwd_or_newline(pane_tree)
    local pane = pane_tree and pane_tree.pane
    local cwd = pane_tree and pane_tree.cwd
    if pane == nil then
        return "dirty"
    end
    if type(cwd) == "string" and M.bytes_clean(cwd) then
        pane:send_text("cd " .. wezterm.shell_quote_arg(cwd) .. "\r\n")
        return "clean"
    end
    pane:send_text("\r\n")
    return "dirty"
end

-- ────────────────────────────────────────────────────────────────────
-- impl — the decision flow body
-- ────────────────────────────────────────────────────────────────────

local function impl(pane_tree)
    -- Step 1: argv lookup.
    local argv = pane_tree and pane_tree.process and pane_tree.process.argv

    -- Step 2: empty/missing argv → resurrect's default.
    if argv == nil or #argv == 0 then
        local resurrect = resolve_resurrect()
        if resurrect ~= nil
           and type(resurrect.default_on_pane_restore) == "function"
        then
            resurrect.default_on_pane_restore(pane_tree)
        end
        return
    end

    -- Step 3: basename(argv[1]). 1-based: argv[1] IS the program.
    local prog = basename(tostring(argv[1] or ""))

    -- Step 4 action — extracted so steps 5/6 can re-enter it.
    local function fallback(reason_prog)
        local cwd_state = send_cwd_or_newline(pane_tree)
        log.warn(string.format(
            "wezsesh: skipped argv restore for %q; cwd %s",
            tostring(reason_prog), cwd_state))
    end

    -- Step 4: allowlist check.
    local policy = resolve_policy()
    if policy == nil or type(policy.allows) ~= "function"
       or not policy.allows(prog)
    then
        fallback(prog)
        return
    end

    -- Step 5: every argv element must be bytes_clean.
    for i = 1, #argv do
        if not M.bytes_clean(tostring(argv[i] or "")) then
            fallback(prog)
            return
        end
    end

    -- Step 6: pane_tree.cwd, if present, must be bytes_clean.
    if pane_tree.cwd ~= nil and not M.bytes_clean(pane_tree.cwd) then
        fallback(prog)
        return
    end

    -- Step 7: resurrect's default (the only success-path call site).
    local resurrect = resolve_resurrect()
    if resurrect ~= nil
       and type(resurrect.default_on_pane_restore) == "function"
    then
        resurrect.default_on_pane_restore(pane_tree)
    end
end

-- ────────────────────────────────────────────────────────────────────
-- public entry point. pcall-wrapped boundary.
-- ────────────────────────────────────────────────────────────────────
--
-- On any uncaught error in impl:
--   * pane:send_text("\r\n") if pane is available
--   * log the crash
--   * MUST NOT call resurrect.default_on_pane_restore
--
-- The default-call lives only at step 7 inside impl()'s success path.
-- A raise from inside impl unwinds before step 7 fires, so the recover
-- arm here never re-invokes it.

function M.callback(pane_tree)
    local ok, err = pcall(impl, pane_tree)
    if not ok then
        local pane = pane_tree and pane_tree.pane
        if type(pane) == "table" or type(pane) == "userdata" then
            pcall(function() pane:send_text("\r\n") end)
        end
        log.warn("wezsesh: on_pane_restore hook crash; failed CLOSED: "
            .. tostring(err))
    end
end

return M
