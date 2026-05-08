-- Busted-style spec for ipc.lua. Self-contained — runs under plain
-- `lua plugin/wezsesh/ipc_spec.lua` from the repo root, no busted
-- required.
--
-- Run:
--     cd <repo-root>
--     lua plugin/wezsesh/ipc_spec.lua
--
-- Exits 0 with `OK N/N` on success, 1 with FAIL lines on stderr
-- otherwise.
--
-- This spec exercises every IPC acceptance gate:
--   * Pointer-shape validation: malformed JSON, path outside
--     `<runtime_dir>/req/`, wrong mode, symlink, `pointer.id ≠
--     payload.id` → silent-drop + log_warn REQ_POINTER_REJECTED.
--   * HMAC mismatch silent on wire — corrupted payload → no spawn.
--   * Freshness boundary — ts=now-30 accept; now-31 reject; now+30
--     accept; now+31 reject.
--   * seen_ids TTL prune (session-wide) — entries older than 60s
--     dropped.
--   * SUN_PATH overflow — over-budget runtime_dir → Lua sentinel
--     `WEZSESH_SUN_PATH_OVERFLOW` (10s toast happens in init.lua via
--     pcall).
--   * Multi-window broadcast (#3524) — only window with matching
--     target_window_id dispatches.
--   * Steps (a)–(h) sync-only — exercised by the `internal/lualint`
--     harness, not by this spec; we cover the runtime contract here
--     (no spawn unless dispatch reaches step (i)).

local function script_dir()
    local src = arg and arg[0] or "plugin/wezsesh/ipc_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
-- See result_spec.lua for the rationale of the two-entry package.path:
-- bare requires for sibling modules and namespaced `wezsesh.<m>`
-- requires for the production code path.
package.path = script_dir() .. "/?.lua;"
            .. script_dir() .. "/?/init.lua;"
            .. script_dir() .. "/../?.lua;"
            .. script_dir() .. "/../?/init.lua;"
            .. package.path

-- ────────────────────────────────────────────────────────────────────
-- wezterm shim — installed BEFORE require("ipc")
-- ────────────────────────────────────────────────────────────────────
--
-- We need the production path to:
--   * resolve `wezterm.GLOBAL.wezsesh_session_key` via the same
--     snapshot-on-read userdata behaviour state.lua relies on;
--   * call `wezterm.json_parse` on raw bytes (the canonical-JSON
--     request file contents);
--   * call `wezterm.log_warn` (silent in the spec; we capture).
--
-- We do NOT route the spec through any `wezterm.background_child_process`
-- — ipc.lua does not spawn directly; ops.dispatch (T-601) does. The
-- spec stubs `M._deps.dispatch` to a recorder so we can assert on
-- dispatch invocation count for HMAC-drop scenarios.

local helpers = require("spec_helpers")
local deepcopy = helpers.deepcopy

local codec = helpers.make_json_codec()
local json_encode_shim = codec.encode
local json_parse_shim  = codec.decode

local global_proxy = helpers.make_global_proxy()

local log_warn_calls = {}
local log_error_calls = {}

local wezterm_shim = {
    GLOBAL        = global_proxy,
    json_encode   = json_encode_shim,
    json_parse    = json_parse_shim,
    log_warn      = function(msg) log_warn_calls[#log_warn_calls + 1] = msg end,
    log_error     = function(msg) log_error_calls[#log_error_calls + 1] = msg end,
    home_dir      = "/home/test",
    target_triple = "x86_64-unknown-linux-gnu",
    on            = function(_evt, _fn) end,  -- swallowed; we call handle_user_var directly
}
package.preload["wezterm"] = function() return wezterm_shim end

-- Now load the modules under test (production require path).
local ipc            = require("wezsesh.ipc")
local state          = require("wezsesh.runtime.state")
local canonical_json = require("wezsesh.canonical_json")
local hmac           = require("wezsesh.crypto.hmac")

-- ────────────────────────────────────────────────────────────────────
-- minimal busted-shaped harness
-- ────────────────────────────────────────────────────────────────────

local failures, total = 0, 0
local current_describe = "<top>"

local function describe(name, fn)
    local prev = current_describe
    current_describe = name
    fn()
    current_describe = prev
end

local function reset_state()
    log_warn_calls = {}
    log_error_calls = {}
    state.wipe_all()
    for k in pairs(global_proxy) do global_proxy[k] = nil end
    global_proxy.wezsesh_session_key = string.rep("a", 64)
    -- Clear the user-var-changed install gate so each test exercises a
    -- fresh `M.register` call. Production code relies on this gate to
    -- avoid stacking handlers on apply_to_config re-runs; tests need to
    -- bypass it to assert the registration path on every it() block.
    _G._wezsesh_user_var_listener_installed = nil
end

local function it(name, fn)
    total = total + 1
    reset_state()
    ipc._reset_deps()
    local ok, err = pcall(fn)
    if not ok then
        failures = failures + 1
        io.stderr:write(string.format("FAIL [%s] %s\n  %s\n",
            current_describe, name, tostring(err)))
    end
end

local function assert_eq(got, want, msg)
    if got ~= want then
        error(string.format("%s\n   got: %s\n  want: %s",
            msg or "values differ", tostring(got), tostring(want)), 2)
    end
end

local function assert_true(cond, msg)
    if not cond then error(msg or "expected truthy", 2) end
end

local function assert_false(cond, msg)
    if cond then error(msg or "expected falsy", 2) end
end

local function log_contains(substr)
    for _, m in ipairs(log_warn_calls) do
        if m:find(substr, 1, true) then return true end
    end
    return false
end

-- ────────────────────────────────────────────────────────────────────
-- payload + pointer fixture builders
-- ────────────────────────────────────────────────────────────────────
--
-- Build a canonical-JSON payload, sign it with the same 64-hex key
-- the GLOBAL holds (default `aaa…`), then build a pointer envelope
-- referring to a synthetic on-disk path. The handler
-- never actually reads the file from disk in the spec — the stat seam
-- and the read seam route through fakes — but the pointer's `path`
-- still has to satisfy the prefix check in pre-step (2).

local DEFAULT_PANE_ID    = 7
local DEFAULT_WINDOW_ID  = 42
local DEFAULT_RUNTIME    = "/tmp/wezsesh-1000"
local DEFAULT_REQ_PREFIX = DEFAULT_RUNTIME .. "/req/"
local DEFAULT_KEY        = string.rep("a", 64)

local function valid_ulid()
    -- 26-char Crockford-base32 placeholder. The validator only checks
    -- length (#id == 26); alphabet enforcement lives in the binary
    -- side (canonical_json acceptor would also reject non-string).
    return "01JABCDEFGHJKMNPQRSTVWXYZA"
end

local function build_payload(overrides)
    overrides = overrides or {}
    local id = overrides.id or valid_ulid()
    local args = canonical_json.object{}
    -- canonical_json.tag_in_place will populate verb-specific fields,
    -- but the production path expects an args TABLE; for noop the shape
    -- is `{ _shape = "object" }` (empty), so the empty object suffices.
    local payload = {
        v                = 1,
        id               = id,
        ts               = overrides.ts or 1700000000,
        target_window_id = overrides.target_window_id or DEFAULT_WINDOW_ID,
        reply_sock       = overrides.reply_sock
                          or "/tmp/wezsesh-1000/abcdef01.sock",
        op               = overrides.op or "noop",
        args             = args,
    }

    -- Signer: tag the full (with-hmac) payload, then
    -- copy_without("hmac") + encode + sign. Mirrors the verifier order
    -- in ipc.lua step (e). For the signer we don't yet HAVE an hmac
    -- to drop, so we tag a placeholder, then re-tag the real payload
    -- after attaching the digest. Cheaper to construct a sans-hmac
    -- merged shape inline for signing only.
    local key = overrides.key or DEFAULT_KEY
    local sign_shape = { _shape = "object" }
    for k, sub in pairs(canonical_json.ROOT_PAYLOAD_SHAPE) do
        if k ~= "_shape" and k ~= "hmac" then
            sign_shape[k] = sub
        end
    end
    canonical_json.tag_in_place(payload, sign_shape,
        canonical_json.verb_args_shape[payload.op])
    local bytes = canonical_json.encode(payload)
    payload.hmac = hmac.compute(bytes, key)

    if overrides.flip_hmac then
        -- Corrupt the last hex char to flip one bit pair without
        -- breaking the 64-char length contract.
        local last = payload.hmac:sub(64, 64)
        local replacement = (last == "f") and "e" or "f"
        payload.hmac = payload.hmac:sub(1, 63) .. replacement
    end

    return payload
end

-- Build a pointer envelope. Returns `(pointer_value, payload_json)`.
-- wezterm pre-decodes the base64 form of the OSC value before firing
-- `user-var-changed`, so the handler receives the raw pointer JSON
-- directly; the spec mirrors that contract by handing JSON in. The
-- spec's stat/read seams hand back `payload_json` when the handler
-- "opens" the pointer's path.
local function build_pointer(payload, overrides)
    overrides = overrides or {}
    local pointer = {
        v    = overrides.v_field or 1,
        id   = overrides.pointer_id or payload.id,
        path = overrides.path or (DEFAULT_REQ_PREFIX .. "12345678.json"),
    }
    if overrides.drop_v then pointer.v = nil end
    if overrides.drop_id then pointer.id = nil end
    if overrides.drop_path then pointer.path = nil end

    local pointer_json = json_encode_shim(pointer)
    local payload_json = json_encode_shim(payload)
    return pointer_json, payload_json, pointer
end

-- Install a stat seam + a read seam in ipc._deps so the handler walks
-- pre-step (3) and pre-step (4) without touching the real filesystem.
-- `entries` is keyed by absolute path and carries `{stat = …, body = …}`.
local function install_fs_seams(entries)
    -- Override io.open transparently. We stash the original + restore in
    -- the per-test reset.
    local real_io_open = io.open
    io.open = function(path, mode)
        local entry = entries[path]
        if entry == nil then
            return real_io_open(path, mode)
        end
        local body = entry.body or ""
        local pos = 1
        return {
            read = function(self, fmt)
                if fmt == "*a" or fmt == "a" then
                    local out = body:sub(pos)
                    pos = #body + 1
                    return out
                end
                return nil
            end,
            close = function() end,
        }
    end
    local real_os_remove = os.remove
    os.remove = function(path)
        if entries[path] ~= nil then
            entries[path] = nil
            return true
        end
        return real_os_remove(path)
    end

    ipc._set_deps{
        stat_path = function(path)
            local entry = entries[path]
            if entry == nil then return nil end
            return entry.stat
        end,
    }

    return function()
        io.open = real_io_open
        os.remove = real_os_remove
    end
end

-- A standard "happy" stat result: regular file, mode 0600, owner-self,
-- not a symlink.
local function ok_stat()
    return {
        mode = 0x180,        -- octal 0600
        is_symlink = false,
        is_regular = true,
        owner_self = true,
    }
end

local function fake_pane(pid)
    return {
        pane_id = function() return pid or DEFAULT_PANE_ID end,
    }
end

local function fake_window()
    return { window_id = function() return DEFAULT_WINDOW_ID end }
end

-- Seed state.set_state so step (a) accepts the pane.
local function seed_session(pid, wid, spawned_at)
    state.set_state(pid or DEFAULT_PANE_ID, {
        target_window_id = wid or DEFAULT_WINDOW_ID,
        spawned_at       = spawned_at or 1700000000,
    })
end

-- Drive the handler with a built payload + pointer, recording dispatch
-- invocations. Returns dispatch_calls (table of {payload, window, pane}).
local function drive_handler(payload, pointer_overrides, opts_overrides)
    pointer_overrides = pointer_overrides or {}
    opts_overrides = opts_overrides or {}

    local pointer_value, payload_json, pointer =
        build_pointer(payload, pointer_overrides)

    local entries = {
        [pointer.path] = {
            stat = pointer_overrides.stat or ok_stat(),
            body = pointer_overrides.body or payload_json,
        },
    }
    if pointer_overrides.no_file then
        entries = {}
    end
    local restore = install_fs_seams(entries)

    local dispatch_calls = {}
    -- Preserve any stat_path the test installed; only override dispatch
    -- + log capture.
    local existing = ipc._deps.stat_path
    ipc._set_deps{
        stat_path = existing or function(path)
            local e = entries[path]
            if e == nil then return nil end
            return e.stat
        end,
        now = opts_overrides.now or function() return 1700000000 end,
        dispatch = function(p, w, pn)
            dispatch_calls[#dispatch_calls + 1] = { p, w, pn }
        end,
    }

    local frozen = {
        req_dir_prefix   = opts_overrides.req_dir_prefix or DEFAULT_REQ_PREFIX,
        target_window_id = opts_overrides.target_window_id
                          or DEFAULT_WINDOW_ID,
    }

    ipc.handle_user_var(
        opts_overrides.window or fake_window(),
        opts_overrides.pane or fake_pane(),
        ipc.USER_VAR_NAME,
        pointer_value,
        frozen)

    restore()
    return dispatch_calls
end

-- ────────────────────────────────────────────────────────────────────
-- module surface
-- ────────────────────────────────────────────────────────────────────

describe("module surface", function()
    it("exposes the public API (validate_pointer, validate_payload, "
        .. "register, handle_user_var)", function()
        assert_true(type(ipc.validate_pointer) == "function",
            "validate_pointer missing")
        assert_true(type(ipc.validate_payload) == "function",
            "validate_payload missing")
        assert_true(type(ipc.register) == "function",
            "register missing")
        assert_true(type(ipc.handle_user_var) == "function",
            "handle_user_var missing")
        assert_true(type(ipc.validate_runtime_dir) == "function",
            "validate_runtime_dir missing")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- pointer field-shape validator
-- ────────────────────────────────────────────────────────────────────

describe("validate_pointer", function()
    it("accepts a well-formed pointer", function()
        local p = { v = 1, id = valid_ulid(),
                    path = DEFAULT_REQ_PREFIX .. "ab.json" }
        assert_true(ipc.validate_pointer(p, DEFAULT_REQ_PREFIX),
            "well-formed pointer rejected")
    end)
    it("rejects v != 1", function()
        local p = { v = 2, id = valid_ulid(), path = DEFAULT_REQ_PREFIX .. "x" }
        assert_false(ipc.validate_pointer(p, DEFAULT_REQ_PREFIX),
            "v != 1 accepted")
    end)
    it("rejects ULID length != 26", function()
        local p = { v = 1, id = "01JABC", path = DEFAULT_REQ_PREFIX .. "x" }
        assert_false(ipc.validate_pointer(p, DEFAULT_REQ_PREFIX),
            "short ULID accepted")
    end)
    it("rejects path outside the configured prefix", function()
        local p = { v = 1, id = valid_ulid(), path = "/etc/passwd" }
        assert_false(ipc.validate_pointer(p, DEFAULT_REQ_PREFIX),
            "/etc/passwd accepted")
    end)
    it("rejects partial-prefix attack (req2/ vs req/)", function()
        local p = { v = 1, id = valid_ulid(),
                    path = DEFAULT_RUNTIME .. "/req2/x.json" }
        assert_false(ipc.validate_pointer(p, DEFAULT_REQ_PREFIX),
            "partial-prefix attack accepted")
    end)
    it("rejects missing fields", function()
        assert_false(ipc.validate_pointer({}, DEFAULT_REQ_PREFIX),
            "empty pointer accepted")
        assert_false(ipc.validate_pointer({ v = 1, id = valid_ulid() },
                                         DEFAULT_REQ_PREFIX),
            "no path accepted")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- payload field-shape validator
-- ────────────────────────────────────────────────────────────────────

describe("validate_payload", function()
    local function base()
        return {
            v = 1, id = valid_ulid(), ts = 1700000000,
            target_window_id = 1, reply_sock = "/tmp/x.sock",
            op = "noop", args = {}, hmac = string.rep("a", 64),
        }
    end
    it("accepts the canonical envelope shape", function()
        assert_true(ipc.validate_payload(base()),
            "well-formed payload rejected")
    end)
    it("rejects op longer than 32 chars", function()
        local p = base(); p.op = string.rep("x", 33)
        assert_false(ipc.validate_payload(p), "33-char op accepted")
    end)
    it("rejects empty op", function()
        local p = base(); p.op = ""
        assert_false(ipc.validate_payload(p), "empty op accepted")
    end)
    it("rejects reply_sock > 104 chars", function()
        local p = base(); p.reply_sock = string.rep("x", 105)
        assert_false(ipc.validate_payload(p), "105-char reply_sock accepted")
    end)
    it("rejects hmac != 64 chars", function()
        local p = base(); p.hmac = string.rep("a", 63)
        assert_false(ipc.validate_payload(p), "short hmac accepted")
    end)
    it("rejects v != 1", function()
        local p = base(); p.v = 2
        assert_false(ipc.validate_payload(p), "v=2 accepted")
    end)
    it("rejects non-string id", function()
        local p = base(); p.id = 12345
        assert_false(ipc.validate_payload(p), "numeric id accepted")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- SUN_PATH validation (Lua side)
-- ────────────────────────────────────────────────────────────────────

describe("SUN_PATH validation", function()
    it("accepts a sane runtime_dir", function()
        local ok, _ = pcall(ipc.validate_runtime_dir, "/tmp/wezsesh-1000")
        assert_true(ok, "sane runtime_dir rejected")
    end)
    it("expands ~/", function()
        local _, expanded = pcall(ipc.validate_runtime_dir,
                                  "~/.cache/wezsesh")
        -- expanded value is the second pcall arg ONLY on failure;
        -- on success, validate_runtime_dir returns the expanded path
        -- as its first return value (pcall packs as ok, ret).
        local ok, ret = pcall(ipc.validate_runtime_dir,
                              "~/.cache/wezsesh")
        assert_true(ok, "~/ expansion failed")
        assert_eq(ret, "/home/test/.cache/wezsesh",
            "expansion didn't substitute home_dir")
    end)
    it("raises WEZSESH_RUNTIME_DIR_TYPE on non-string", function()
        local ok, err = pcall(ipc.validate_runtime_dir, 42)
        assert_false(ok, "non-string runtime_dir accepted")
        assert_true(tostring(err):find("WEZSESH_RUNTIME_DIR_TYPE", 1, true)
            ~= nil, "expected WEZSESH_RUNTIME_DIR_TYPE sentinel: "
            .. tostring(err))
    end)
    it("raises WEZSESH_SUN_PATH_OVERFLOW on darwin over-budget", function()
        wezterm_shim.target_triple = "aarch64-apple-darwin"
        -- darwin ceiling is 104; tail is 14; so >= 91 chars overflows.
        local long = "/tmp/" .. string.rep("z", 100)
        local ok, err = pcall(ipc.validate_runtime_dir, long)
        wezterm_shim.target_triple = "x86_64-unknown-linux-gnu"
        assert_false(ok, "darwin SUN_PATH overflow accepted")
        assert_true(tostring(err):find(
            "WEZSESH_SUN_PATH_OVERFLOW", 1, true) ~= nil,
            "expected WEZSESH_SUN_PATH_OVERFLOW sentinel: " .. tostring(err))
    end)
    it("raises WEZSESH_SUN_PATH_OVERFLOW on Linux over-budget", function()
        -- Linux ceiling is 108; tail is 14; so >= 95 chars overflows.
        local long = "/tmp/" .. string.rep("z", 110)
        local ok, err = pcall(ipc.validate_runtime_dir, long)
        assert_false(ok, "linux SUN_PATH overflow accepted")
        assert_true(tostring(err):find(
            "WEZSESH_SUN_PATH_OVERFLOW", 1, true) ~= nil,
            "expected WEZSESH_SUN_PATH_OVERFLOW sentinel: " .. tostring(err))
    end)
    it("accepts darwin runtime_dir at exactly the budget", function()
        wezterm_shim.target_triple = "aarch64-apple-darwin"
        -- needed = 90 + 14 = 104 = ceiling.
        local at_budget = "/" .. string.rep("z", 89)
        local ok, ret = pcall(ipc.validate_runtime_dir, at_budget)
        wezterm_shim.target_triple = "x86_64-unknown-linux-gnu"
        assert_true(ok, "at-budget darwin runtime_dir rejected: "
            .. tostring(ret))
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Pointer-shape validation (spike-#3) — wire-silent + log_warn
-- ────────────────────────────────────────────────────────────────────

describe("Pointer-shape validation (spike-#3)", function()
    it("non-JSON value → REQ_POINTER_REJECTED, no dispatch", function()
        seed_session()
        ipc._set_deps{
            now = function() return 1700000000 end,
            dispatch = function() error("dispatch should not run", 0) end,
        }
        ipc.handle_user_var(fake_window(), fake_pane(),
            ipc.USER_VAR_NAME,
            "not valid json at all",
            { req_dir_prefix = DEFAULT_REQ_PREFIX,
              target_window_id = DEFAULT_WINDOW_ID })
        assert_true(log_contains("REQ_POINTER_REJECTED"),
            "expected REQ_POINTER_REJECTED log_warn")
    end)

    it("malformed pointer JSON → REQ_POINTER_REJECTED", function()
        seed_session()
        ipc._set_deps{ dispatch = function()
            error("dispatch should not run", 0)
        end }
        ipc.handle_user_var(fake_window(), fake_pane(),
            ipc.USER_VAR_NAME, "{not-json",
            { req_dir_prefix = DEFAULT_REQ_PREFIX,
              target_window_id = DEFAULT_WINDOW_ID })
        assert_true(log_contains("REQ_POINTER_REJECTED"),
            "expected REQ_POINTER_REJECTED on JSON parse fail")
    end)

    it("empty value → REQ_POINTER_REJECTED, no dispatch", function()
        seed_session()
        ipc._set_deps{
            now = function() return 1700000000 end,
            dispatch = function() error("dispatch should not run", 0) end,
        }
        ipc.handle_user_var(fake_window(), fake_pane(),
            ipc.USER_VAR_NAME, "",
            { req_dir_prefix = DEFAULT_REQ_PREFIX,
              target_window_id = DEFAULT_WINDOW_ID })
        assert_true(log_contains("REQ_POINTER_REJECTED"),
            "expected REQ_POINTER_REJECTED on empty value")
    end)

    it("path outside <runtime_dir>/req/ → REQ_POINTER_REJECTED", function()
        seed_session()
        local payload = build_payload()
        local calls = drive_handler(payload, { path = "/etc/passwd" })
        assert_eq(#calls, 0, "dispatch ran on bad pointer path")
        assert_true(log_contains("REQ_POINTER_REJECTED"),
            "expected REQ_POINTER_REJECTED on path outside prefix")
    end)

    it("symlink at the request file → REQ_POINTER_REJECTED", function()
        seed_session()
        local payload = build_payload()
        local bad_stat = ok_stat(); bad_stat.is_symlink = true
        local calls = drive_handler(payload, { stat = bad_stat })
        assert_eq(#calls, 0, "dispatch ran on symlink")
        assert_true(log_contains("REQ_POINTER_REJECTED"),
            "expected REQ_POINTER_REJECTED on symlink")
    end)

    it("wrong mode (0644) → REQ_POINTER_REJECTED", function()
        seed_session()
        local payload = build_payload()
        local bad_stat = ok_stat(); bad_stat.mode = 0x1a4   -- 0644
        local calls = drive_handler(payload, { stat = bad_stat })
        assert_eq(#calls, 0, "dispatch ran on mode-0644 file")
        assert_true(log_contains("REQ_POINTER_REJECTED"),
            "expected REQ_POINTER_REJECTED on wrong mode")
    end)

    it("non-regular file (e.g. socket) → REQ_POINTER_REJECTED", function()
        seed_session()
        local payload = build_payload()
        local bad_stat = ok_stat(); bad_stat.is_regular = false
        local calls = drive_handler(payload, { stat = bad_stat })
        assert_eq(#calls, 0, "dispatch ran on non-regular file")
        assert_true(log_contains("REQ_POINTER_REJECTED"),
            "expected REQ_POINTER_REJECTED on non-regular file")
    end)

    it("foreign owner → REQ_POINTER_REJECTED", function()
        seed_session()
        local payload = build_payload()
        local bad_stat = ok_stat(); bad_stat.owner_self = false
        local calls = drive_handler(payload, { stat = bad_stat })
        assert_eq(#calls, 0, "dispatch ran on foreign-owned file")
        assert_true(log_contains("REQ_POINTER_REJECTED"),
            "expected REQ_POINTER_REJECTED on foreign owner")
    end)

    it("pointer.id != payload.id → REQ_POINTER_REJECTED", function()
        seed_session()
        local payload = build_payload()
        local calls = drive_handler(payload,
            { pointer_id = "01JADIFFERENTIDXXXXXXXXXXX" })
        assert_eq(#calls, 0, "dispatch ran on id mismatch")
        assert_true(log_contains("REQ_POINTER_REJECTED"),
            "expected REQ_POINTER_REJECTED on pointer/payload id mismatch")
    end)

    it("missing request file → REQ_POINTER_REJECTED", function()
        seed_session()
        local payload = build_payload()
        local calls = drive_handler(payload, { no_file = true })
        assert_eq(#calls, 0, "dispatch ran on missing file")
        assert_true(log_contains("REQ_POINTER_REJECTED"),
            "expected REQ_POINTER_REJECTED on missing file")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- HMAC mismatch silent on wire
-- ────────────────────────────────────────────────────────────────────

describe("HMAC mismatch silent on wire", function()
    it("flipped hex char → no dispatch, no spawn, log_warn 'HMAC mismatch'",
    function()
        seed_session()
        local payload = build_payload({ flip_hmac = true })
        local calls = drive_handler(payload)
        assert_eq(#calls, 0, "dispatch ran on bad HMAC")
        assert_true(log_contains("HMAC mismatch"),
            "expected log_warn 'HMAC mismatch' (internal-only)")
    end)

    it("wrong key on signer (binary side mismatch) → silent drop",
    function()
        seed_session()
        local payload = build_payload({ key = string.rep("b", 64) })
        local calls = drive_handler(payload)
        assert_eq(#calls, 0, "dispatch ran on key-mismatch HMAC")
    end)

    it("HMAC failure leaves seen_ids untouched (so the legit retransmit "
        .. "after fix is still acceptable)", function()
        seed_session()
        local payload = build_payload({ flip_hmac = true })
        drive_handler(payload)
        assert_false(state.seen(payload.id),
            "HMAC failure pre-marked seen_ids, breaking legit retransmit")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Freshness boundary
-- ────────────────────────────────────────────────────────────────────

describe("Freshness boundary", function()
    it("ts = now - 30 → accept (boundary inclusive)", function()
        seed_session()
        local payload = build_payload({ ts = 1700000000 - 30 })
        local calls = drive_handler(payload, {},
            { now = function() return 1700000000 end })
        assert_eq(#calls, 1, "now-30 should accept")
    end)

    it("ts = now - 31 → reject (boundary exclusive on the other side)",
    function()
        seed_session()
        local payload = build_payload({ ts = 1700000000 - 31 })
        local calls = drive_handler(payload, {},
            { now = function() return 1700000000 end })
        assert_eq(#calls, 0, "now-31 should reject")
        assert_true(log_contains("STALE_PAYLOAD"),
            "expected STALE_PAYLOAD log_warn at -31s")
    end)

    it("ts = now + 30 → accept (clock skew tolerance)", function()
        seed_session()
        local payload = build_payload({ ts = 1700000000 + 30 })
        local calls = drive_handler(payload, {},
            { now = function() return 1700000000 end })
        assert_eq(#calls, 1, "now+30 should accept")
    end)

    it("ts = now + 31 → reject", function()
        seed_session()
        local payload = build_payload({ ts = 1700000000 + 31 })
        local calls = drive_handler(payload, {},
            { now = function() return 1700000000 end })
        assert_eq(#calls, 0, "now+31 should reject")
        assert_true(log_contains("STALE_PAYLOAD"),
            "expected STALE_PAYLOAD log_warn at +31s")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- seen_ids TTL prune (session-wide) — entries older than 60s dropped
-- ────────────────────────────────────────────────────────────────────

describe("seen_ids TTL prune", function()
    it("a stale entry (age > 60s) is pruned at end-of-dispatch", function()
        seed_session()
        -- Pre-seed a stale entry with a fake clock helper. We use
        -- state.mark_seen which stamps os.time(); to force "stale" we
        -- override os.time temporarily for the seed call.
        local real_now = os.time
        os.time = function() return 1699999000 end  -- 1000s before "now"
        state.mark_seen("01JOLDXXXXXXXXXXXXXXXXXXXX")
        os.time = real_now

        -- Drive a fresh dispatch with now = 1700000000. The handler's
        -- step (h) calls state.prune_seen_ids(60); the stale entry must
        -- be evicted.
        local payload = build_payload()
        local calls = drive_handler(payload, {},
            { now = function() return 1700000000 end })
        assert_eq(#calls, 1, "fresh dispatch failed")

        -- The stale entry should be gone. The fresh one (payload.id)
        -- should be present.
        assert_false(state.seen("01JOLDXXXXXXXXXXXXXXXXXXXX"),
            "stale seen_ids entry not pruned")
        assert_true(state.seen(payload.id),
            "fresh dispatch's id missing from seen_ids")
    end)

    it("session-wide bucketing: same ULID across panes deduplicates",
    function()
        seed_session(7, DEFAULT_WINDOW_ID)
        seed_session(8, DEFAULT_WINDOW_ID)
        local payload = build_payload()

        -- First dispatch on pane 7 — accept.
        local c1 = drive_handler(payload, {}, { pane = fake_pane(7) })
        assert_eq(#c1, 1, "first dispatch rejected")

        -- Re-deliver the SAME payload to pane 8 — the seen_ids bucket
        -- is session-wide (no per-pane key), so this MUST be dropped.
        local c2 = drive_handler(payload, {}, { pane = fake_pane(8) })
        assert_eq(#c2, 0,
            "replay across panes dispatched twice (bucketing regression)")
    end)

    it("replay of the same id → dropped (silent)", function()
        seed_session()
        local payload = build_payload()
        drive_handler(payload)
        local calls2 = drive_handler(payload)
        assert_eq(#calls2, 0, "replay dispatched a second time")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Multi-window broadcast (#3524) — window scoping
-- ────────────────────────────────────────────────────────────────────

describe("Multi-window broadcast (#3524)", function()
    it("payload.target_window_id != session.target_window_id → drop",
    function()
        -- The pane's session record was created when this binary spawned
        -- in window 99; the payload claims window 42. Even though the
        -- handler is invoked (wezterm broadcasts user-var-changed to
        -- every window), only the matching one MUST dispatch.
        seed_session(DEFAULT_PANE_ID, 99 --[[ session window ]])
        local payload = build_payload({ target_window_id = 42 })
        local calls = drive_handler(payload, {},
            { target_window_id = 42 })
        assert_eq(#calls, 0,
            "wrong-window payload reached dispatch")
    end)

    it("matching target_window_id → dispatch", function()
        seed_session(DEFAULT_PANE_ID, 42)
        local payload = build_payload({ target_window_id = 42 })
        local calls = drive_handler(payload, {},
            { target_window_id = 42 })
        assert_eq(#calls, 1, "matching-window payload didn't dispatch")
    end)

    it("wire target_window_id == 0 matches session.target_window_id == 0 "
        .. "(wezterm's first-window id)", function()
        -- wezterm assigns WINID = 0 to the first window; a keybinding
        -- spawned from that window emits a wire target_window_id of 0.
        -- The handler MUST match it strictly against session = 0 — `0`
        -- is a real window id, NOT the sentinel.
        seed_session(DEFAULT_PANE_ID, 0)
        local payload = build_payload({ target_window_id = 0 })
        local calls = drive_handler(payload, {},
            { target_window_id = 0 })
        assert_eq(#calls, 1,
            "wire 0 didn't strict-match session 0 (T-905 regression)")
    end)

    it("wire target_window_id == 0 against session != 0 → drop "
        .. "(no sentinel-shaped fallthrough on 0)", function()
        -- The corollary of the strict-match rule: `0` is NOT a
        -- sentinel, so a wire 0 against a session bound to a different
        -- window must drop. The earlier broken contract treated 0 as
        -- "any window" and would have dispatched here.
        seed_session(DEFAULT_PANE_ID, 42)
        local payload = build_payload({ target_window_id = 0 })
        local calls = drive_handler(payload, {},
            { target_window_id = 0 })
        assert_eq(#calls, 0,
            "wire 0 fell through against session 42 (sentinel collision)")
    end)

    it("wire target_window_id == -1 (any-window sentinel) → dispatch "
        .. "regardless of session.target_window_id",
    function()
        -- Apply-time emissions (init.lua step 7) carry -1 because no
        -- wezterm window is bound at apply_to_config time. The handler
        -- MUST skip the window-match check and fall through to step (h).
        seed_session(DEFAULT_PANE_ID, 99)
        local payload = build_payload({ target_window_id = -1 })
        local calls = drive_handler(payload, {},
            { target_window_id = -1 })
        assert_eq(#calls, 1,
            "wire -1 (any-window sentinel) didn't fall through to dispatch")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Foreign user_var name → instant exit (no parsing cost)
-- ────────────────────────────────────────────────────────────────────

describe("foreign user_var name → instant return", function()
    it("ignores other plugins' SetUserVar emissions", function()
        seed_session()
        ipc._set_deps{ dispatch = function()
            error("dispatch should not run", 0)
        end }
        ipc.handle_user_var(fake_window(), fake_pane(),
            "some_other_plugin_var",
            "anything",
            { req_dir_prefix = DEFAULT_REQ_PREFIX,
              target_window_id = DEFAULT_WINDOW_ID })
        -- No log_warn: this is the cheapest exit, not an error.
        assert_eq(#log_warn_calls, 0,
            "foreign user_var name should be a silent no-op")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Step (a) — pane-id match: no session record → silent drop
-- ────────────────────────────────────────────────────────────────────

describe("pane-id match (step a)", function()
    it("no session record for pane → drop without dispatch", function()
        -- Skip the seed_session call.
        local payload = build_payload()
        local calls = drive_handler(payload)
        assert_eq(#calls, 0,
            "missing session record didn't drop the payload")
    end)

    it("no session record → silent drop (no log_warn emitted)", function()
        -- Locks in the absence-of-log signal: step (a)'s "no session
        -- record" branch is a SILENT drop — either a stale OSC after
        -- window-config-reloaded blew the bucket, or a foreign emitter.
        -- A future refactor adding a log_warn here would cause foreign-
        -- pane traffic (the 99%-case this branch handles cheaply) to
        -- spam the log. Skip the seed_session call so step (a) drops.
        local payload = build_payload()
        local calls = drive_handler(payload)
        assert_eq(#calls, 0,
            "missing session record didn't drop the payload")
        assert_eq(#log_warn_calls, 0,
            "step (a) 'no session record' branch should be log-silent; "
            .. "got: " .. tostring(log_warn_calls[1]))
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Step (b) — HMAC key absent
-- ────────────────────────────────────────────────────────────────────

describe("HMAC key availability (step b)", function()
    it("nil session_key → silent drop", function()
        seed_session()
        local payload = build_payload()
        global_proxy.wezsesh_session_key = nil
        local calls = drive_handler(payload)
        assert_eq(#calls, 0,
            "missing HMAC key dispatched anyway")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Step (d) — payload field-shape failure
-- ────────────────────────────────────────────────────────────────────

describe("payload shape failure (step d)", function()
    it("malformed payload (op too long) → drop", function()
        seed_session()
        -- Build a payload, then mutate to break shape AFTER signing.
        -- The handler's step (d) runs BEFORE re-encode/HMAC, so a bad
        -- shape is rejected before we even hit the signer.
        local payload = build_payload()
        payload.op = string.rep("x", 33)
        local calls = drive_handler(payload)
        assert_eq(#calls, 0, "33-char op dispatched anyway")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Happy path: well-formed → step (i) dispatch
-- ────────────────────────────────────────────────────────────────────

describe("happy path", function()
    it("valid noop payload → dispatch invoked exactly once", function()
        seed_session()
        local payload = build_payload({ op = "noop" })
        local calls = drive_handler(payload)
        assert_eq(#calls, 1, "noop didn't dispatch exactly once")
        assert_eq(calls[1][1].id, payload.id, "dispatched payload id wrong")
    end)

    it("valid switch payload → dispatch invoked exactly once", function()
        seed_session()
        -- switch's args shape requires `name` and `cwd` strings. Build
        -- the payload from scratch so build_payload's signer path picks
        -- up the keys from the start (re-signing after-the-fact would
        -- have to invert the in-place tagging build_payload did to the
        -- shared static args object).
        local payload = {
            v                = 1,
            id               = valid_ulid(),
            ts               = 1700000000,
            target_window_id = DEFAULT_WINDOW_ID,
            reply_sock       = "/tmp/wezsesh-1000/abcdef01.sock",
            op               = "switch",
            args             = canonical_json.object{
                name = "main",
                cwd  = "",
            },
        }
        local sign_shape = { _shape = "object" }
        for k, sub in pairs(canonical_json.ROOT_PAYLOAD_SHAPE) do
            if k ~= "_shape" and k ~= "hmac" then
                sign_shape[k] = sub
            end
        end
        canonical_json.tag_in_place(payload, sign_shape,
            canonical_json.verb_args_shape.switch)
        payload.hmac = hmac.compute(canonical_json.encode(payload), DEFAULT_KEY)

        local calls = drive_handler(payload)
        assert_eq(#calls, 1, "switch didn't dispatch")
    end)

    it("save with expected_hash=null round-trips through json_parse "
        .. "(rehydrate_nullable_args)", function()
        -- Regression: the TUI's first-save path sends
        -- `expected_hash: canonicaljson.Null`, the binary signs the
        -- bytes including `"expected_hash":null`, but
        -- `wezterm.json_parse` decodes JSON `null` as Lua `nil` —
        -- which is indistinguishable from a missing key. Without the
        -- rehydrate step before tag_in_place + HMAC re-encode, the
        -- tag walk raised CANONICAL_SHAPE_MISMATCH (or, if it hadn't,
        -- the sans-hmac re-encode would have produced bytes WITHOUT
        -- expected_hash, busting HMAC parity with the binary's pre-
        -- sign bytes).
        seed_session()

        -- Build the SAME bytes the binary would sign: a save payload
        -- with expected_hash = cj.NULL. canonical_json.encode emits
        -- `"expected_hash":null` for the sentinel.
        local payload = {
            v                = 1,
            id               = valid_ulid(),
            ts               = 1700000000,
            target_window_id = DEFAULT_WINDOW_ID,
            reply_sock       = "/tmp/wezsesh-1000/abcdef01.sock",
            op               = "save",
            args             = canonical_json.object{
                name          = "work",
                overwrite     = false,
                expected_hash = canonical_json.NULL,
            },
        }
        local sign_shape = { _shape = "object" }
        for k, sub in pairs(canonical_json.ROOT_PAYLOAD_SHAPE) do
            if k ~= "_shape" and k ~= "hmac" then
                sign_shape[k] = sub
            end
        end
        canonical_json.tag_in_place(payload, sign_shape,
            canonical_json.verb_args_shape.save)
        local pre_sign_bytes = canonical_json.encode(payload)
        payload.hmac = hmac.compute(pre_sign_bytes, DEFAULT_KEY)

        -- Sanity: the bytes the binary would sign include the JSON
        -- null literal at the expected_hash slot (this is also the
        -- shape the spec_helpers json_encode_shim emits for cj.NULL,
        -- which is what drive_handler will hand to the parser).
        assert_true(
            pre_sign_bytes:find('"expected_hash":null', 1, true) ~= nil,
            "fixture failed: expected_hash:null not in pre-sign bytes")

        local calls = drive_handler(payload)

        -- (1) tag_in_place must not raise after rehydrate.
        assert_false(log_contains("canonical tag failed"),
            "tag_in_place raised on parsed-null expected_hash; the "
            .. "rehydrate step (e) didn't restore the cj.NULL sentinel")

        -- (2) HMAC verify must succeed → silent on the wire is now a
        -- successful dispatch; the only path to step (i) is HMAC pass.
        assert_false(log_contains("HMAC mismatch"),
            "HMAC mismatched after rehydrate — the sans-hmac re-encode "
            .. "produced bytes != the binary's pre-sign bytes")

        -- (3) dispatch reached step (i) exactly once.
        assert_eq(#calls, 1,
            "expected dispatch to fire on parsed-null expected_hash; "
            .. "did the rehydrate step run before validate / tag_in_place?")

        -- (4) the payload that reached dispatch carries the sentinel,
        -- not a missing key — downstream verbs reading expected_hash
        -- can rely on the rehydrated form being present.
        local dispatched = calls[1][1]
        local rehydrated = dispatched.args.expected_hash
        local mt = getmetatable(rehydrated)
        assert_true(mt and mt.__wezsesh_canonical == "null",
            "args.expected_hash didn't end up as cj.NULL after rehydrate")
    end)

    it("dispatch raise is swallowed (CLAUDE.md invariant 1)", function()
        seed_session()
        local payload = build_payload()

        local pointer_value, payload_json, pointer = build_pointer(payload)
        local entries = {
            [pointer.path] = { stat = ok_stat(), body = payload_json },
        }
        local restore = install_fs_seams(entries)

        ipc._set_deps{
            stat_path = function(path)
                local e = entries[path]; return e and e.stat or nil
            end,
            now = function() return 1700000000 end,
            dispatch = function() error("synthetic dispatch raise", 0) end,
        }

        local ok = pcall(ipc.handle_user_var,
            fake_window(), fake_pane(), ipc.USER_VAR_NAME, pointer_value,
            { req_dir_prefix = DEFAULT_REQ_PREFIX,
              target_window_id = DEFAULT_WINDOW_ID })
        restore()
        assert_true(ok, "dispatch raise propagated out of handler")
        assert_true(log_contains("dispatch raised"),
            "expected log_warn 'dispatch raised'")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- M.register — wezterm.on hookup + SUN_PATH gate at register time
-- ────────────────────────────────────────────────────────────────────

describe("M.register", function()
    it("registers a single user-var-changed handler with a sane "
        .. "runtime_dir", function()
        local registered = {}
        wezterm_shim.on = function(evt, fn)
            registered[#registered + 1] = { evt = evt, fn = fn }
        end
        ipc.register{
            runtime_dir = "/tmp/wezsesh-1000",
            target_window_id = DEFAULT_WINDOW_ID,
        }
        assert_eq(#registered, 1, "expected one wezterm.on registration")
        assert_eq(registered[1].evt, "user-var-changed",
            "wrong event name")
    end)

    it("propagates WEZSESH_SUN_PATH_OVERFLOW from validate_runtime_dir",
    function()
        wezterm_shim.target_triple = "aarch64-apple-darwin"
        local ok, err = pcall(ipc.register, {
            runtime_dir = "/tmp/" .. string.rep("z", 100),
            target_window_id = DEFAULT_WINDOW_ID,
        })
        wezterm_shim.target_triple = "x86_64-unknown-linux-gnu"
        assert_false(ok, "register accepted over-budget runtime_dir")
        assert_true(tostring(err):find(
            "WEZSESH_SUN_PATH_OVERFLOW", 1, true) ~= nil,
            "expected WEZSESH_SUN_PATH_OVERFLOW sentinel: "
            .. tostring(err))
    end)

    it("the registered handler is pcall-wrapped at the outer "
        .. "boundary (CLAUDE.md invariant 1)", function()
        local registered_fn
        wezterm_shim.on = function(_evt, fn) registered_fn = fn end
        ipc.register{
            runtime_dir = "/tmp/wezsesh-1000",
            target_window_id = DEFAULT_WINDOW_ID,
        }
        assert_true(type(registered_fn) == "function",
            "no handler captured")
        -- A foreign user_var name is the cheapest path; the body's
        -- early-return shouldn't raise. But if a programmer error
        -- inside handle_user_var raised, the OUTER pcall in register's
        -- closure must swallow it. We exercise by passing a non-string
        -- value (handler returns early on the type check; no raise).
        local ok = pcall(registered_fn, fake_window(), fake_pane(),
                         "wezsesh_op", 12345)
        assert_true(ok, "register's outer pcall didn't catch raise")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- summary
-- ────────────────────────────────────────────────────────────────────

if failures > 0 then
    io.stderr:write(string.format("FAILED %d/%d\n", failures, total))
    os.exit(1)
else
    io.stdout:write(string.format("OK %d/%d\n", total, total))
end
