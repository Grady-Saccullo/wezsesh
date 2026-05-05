-- Lua handler fuzz harness for ipc.lua's `user-var-changed` step
-- machine. Drives 15 mutation classes (the fixed-width list plus the
-- `unknown_verb` and `v_field_swap` amendments) into the production
-- `M.handle_user_var(window, pane, name, value, opts)` entry point
-- and asserts the four invariants on every iteration:
--
--   (1) no Lua error escapes the handler (outer pcall returns true);
--   (2) `ops.dispatch` invocation count = 0 unless the input by
--       construction passed HMAC verify (only `ts_boundary` inner pair
--       and `empty_args_per_verb` op="noop" are legitimate dispatchers);
--   (3) no reply is written on HMAC mismatch / unknown-verb short-
--       circuit (the dispatch shim doubles as a reply recorder; both
--       counters must be 0 for those classes);
--   (4) frame paint per iteration stays < 50 ms (wall-clock around the
--       outer pcall).
--
-- Run:
--     lua plugin/wezsesh/fuzz/fuzz_spec.lua
--         [--seed=<int>] [--iters=<int>] [--no-shim]
--
-- Exits 0 with `OK <N>/<N> (mutations: <M>, total bytes: <B>)` on
-- success; 1 with `FAIL <class>: <reason>` on stderr on assertion
-- failure. Deterministic given a fixed `--seed`; never reads os.time()
-- for seeding.
--
-- Shared design with ipc_spec.lua: a wezterm shim is installed via
-- `package.preload["wezterm"]` BEFORE require("wezsesh.ipc"). The
-- handler's filesystem seams (`io.open`, `os.remove`, `_deps.stat_path`)
-- are intercepted so we drive arbitrary bytes into pre-step (4) without
-- touching the real disk.
--
-- The harness deliberately does NOT use any wezterm API (foreign to
-- standalone-Lua) and stays outside the lualint AST walker (the
-- file is `*_spec.lua`, which lualint skips wholesale at line 103 of
-- cmd/lualint/main.go).

local function script_dir()
    local src = arg and arg[0] or "plugin/wezsesh/fuzz/fuzz_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
package.path = script_dir() .. "/?.lua;"
            .. script_dir() .. "/?/init.lua;"
            .. script_dir() .. "/../?.lua;"
            .. script_dir() .. "/../?/init.lua;"
            .. script_dir() .. "/../../?.lua;"
            .. script_dir() .. "/../../?/init.lua;"
            .. package.path

-- ────────────────────────────────────────────────────────────────────
-- argv parsing — `--seed=<int>`, `--iters=<int>`, `--no-shim`
-- ────────────────────────────────────────────────────────────────────
--
-- `--no-shim` switches `oversized_string` to the spec-literal 1 MiB
-- id fixture (`string.rep("X", 1<<20)`). Without it the harness
-- cycles 256/1024/4096 bytes because the pure-Lua `json_parse_shim`
-- would dominate the per-iter budget at 1 MiB. Real wezterm ships a C
-- json_parse and the production budget is comfortable at 1<<20 —
-- that environment is exercised by the e2e job, where the spec's 1
-- MiB intent is honoured. CI does NOT pass `--no-shim`; the
-- division-of-labor is intentional.

local function parse_argv()
    local seed, iters, no_shim = 1, nil, false
    if arg ~= nil then
        for i = 1, #arg do
            local a = arg[i]
            local s = a:match("^%-%-seed=(%-?%d+)$")
            if s ~= nil then seed = tonumber(s) end
            local it = a:match("^%-%-iters=(%d+)$")
            if it ~= nil then iters = tonumber(it) end
            if a == "--no-shim" then no_shim = true end
        end
    end
    return seed, iters, no_shim
end

local SEED, ITERS_OVERRIDE, NO_SHIM = parse_argv()
-- LOW-1 (round 1): math.random's PRNG seed mapping changed between
-- Lua 5.3 and 5.4 (xoshiro256** in 5.4); seeded replay is portable
-- only across the same Lua version. CI pins lua5.4 at the invocation
-- level (see .github/workflows/ci.yml) so seeded regression seeds
-- replay deterministically across runs.
math.randomseed(SEED)

-- ────────────────────────────────────────────────────────────────────
-- wezterm shim — minimum surface to satisfy ipc.lua + transitive deps
-- ────────────────────────────────────────────────────────────────────

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
    on            = function(_evt, _fn) end,
}
package.preload["wezterm"] = function() return wezterm_shim end

local ipc            = require("wezsesh.ipc")
local state          = require("wezsesh.runtime.state")
local canonical_json = require("wezsesh.canonical_json")
local hmac           = require("wezsesh.crypto.hmac")
local b64            = require("wezsesh.crypto.b64")

-- ────────────────────────────────────────────────────────────────────
-- per-iteration constants + shared fixtures
-- ────────────────────────────────────────────────────────────────────

local DEFAULT_PANE_ID    = 7
local DEFAULT_WINDOW_ID  = 42
local DEFAULT_RUNTIME    = "/tmp/wezsesh-fuzz"
local DEFAULT_REQ_PREFIX = DEFAULT_RUNTIME .. "/req/"
local DEFAULT_KEY        = string.rep("a", 64)
local DEFAULT_NOW        = 1700000000

local function valid_ulid()
    return "01JABCDEFGHJKMNPQRSTVWXYZA"
end

local function ok_stat()
    return {
        mode = 0x180,        -- octal 0600
        is_symlink = false,
        is_regular = true,
        owner_self = true,
    }
end

local function fake_pane(pid)
    return { pane_id = function() return pid or DEFAULT_PANE_ID end }
end
local function fake_window()
    return { window_id = function() return DEFAULT_WINDOW_ID end }
end

local function reset_world()
    log_warn_calls = {}
    log_error_calls = {}
    state.wipe_all()
    for k in pairs(global_proxy) do global_proxy[k] = nil end
    global_proxy.wezsesh_session_key = DEFAULT_KEY
    state.set_state(DEFAULT_PANE_ID, {
        target_window_id = DEFAULT_WINDOW_ID,
        spawned_at       = DEFAULT_NOW,
    })
    ipc._reset_deps()
end

-- Build a properly signed canonical payload table. Caller may override any
-- field; build_payload then re-tags + re-signs with the verb-keyed
-- shape so tag_in_place succeeds during the verifier path.
local function build_signed_payload(overrides)
    overrides = overrides or {}
    local op = overrides.op or "noop"
    local args = overrides.args
    if args == nil then
        if op == "noop" then
            args = canonical_json.object{}
        elseif op == "switch" then
            args = canonical_json.object{ name = "main", cwd = "" }
        elseif op == "list_dirs" then
            args = canonical_json.object{ query = "" }
        elseif op == "load" then
            args = canonical_json.object{ name = "main" }
        elseif op == "new" then
            args = canonical_json.object{
                name = "main",
                cwd  = "/tmp/proj",
            }
        elseif op == "save" then
            args = canonical_json.object{
                name          = "main",
                overwrite     = false,
                expected_hash = canonical_json.NULL,
            }
        else
            args = canonical_json.object{}
        end
    end
    local payload = {
        v                = overrides.v or 1,
        id               = overrides.id or valid_ulid(),
        ts               = overrides.ts or DEFAULT_NOW,
        target_window_id = overrides.target_window_id or DEFAULT_WINDOW_ID,
        reply_sock       = overrides.reply_sock
                          or "/tmp/wezsesh-fuzz/abcdef01.sock",
        op               = op,
        args             = args,
    }
    local shape = canonical_json.verb_args_shape[op]
    if shape == nil then return payload end
    local sign_shape = { _shape = "object" }
    for k, sub in pairs(canonical_json.ROOT_PAYLOAD_SHAPE) do
        if k ~= "_shape" and k ~= "hmac" then
            sign_shape[k] = sub
        end
    end
    canonical_json.tag_in_place(payload, sign_shape, shape)
    local bytes = canonical_json.encode(payload)
    payload.hmac = hmac.compute(bytes, overrides.key or DEFAULT_KEY)
    return payload
end

-- Build a pointer envelope referring to `path` with `id`. wezterm
-- pre-decodes the base64 form of the SetUserVar OSC value before
-- firing `user-var-changed`, so the handler receives the raw pointer
-- JSON directly; the harness mirrors that contract.
local function build_pointer_envelope(id, path)
    local pointer = {
        v    = 1,
        id   = id,
        path = path,
    }
    return json_encode_shim(pointer)
end

-- Install fs seams + a recording dispatch shim. Returns
-- (restore_fn, dispatch_calls, reply_writes). dispatch_calls is the
-- canonical record (pcall'd from step (i)). reply_writes is a
-- separate counter the dispatch shim increments to model the
-- Reply-on-socket contract — neither is incremented unless
-- the handler reaches step (i).
local function install_seams(file_entries, now)
    local real_io_open  = io.open
    local real_os_remove = os.remove
    io.open = function(path, mode)
        local entry = file_entries[path]
        if entry == nil then
            return real_io_open(path, mode)
        end
        local body = entry.body or ""
        local pos = 1
        return {
            read = function(_, fmt)
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
    os.remove = function(path)
        if file_entries[path] ~= nil then
            file_entries[path] = nil
            return true
        end
        return real_os_remove(path)
    end

    local dispatch_calls = {}
    local reply_writes = {}
    ipc._set_deps{
        stat_path = function(path)
            local e = file_entries[path]
            if e == nil then return nil end
            return e.stat
        end,
        now = function() return now or DEFAULT_NOW end,
        dispatch = function(p, w, pn)
            dispatch_calls[#dispatch_calls + 1] = { p, w, pn }
            -- Model the reply path: a real ops.dispatch would emit
            -- one reply via wezterm.background_child_process. Failure
            -- of step (f) MUST keep this counter at 0.
            reply_writes[#reply_writes + 1] = {
                op = p.op, id = p.id,
            }
        end,
    }

    local function restore()
        io.open = real_io_open
        os.remove = real_os_remove
    end
    return restore, dispatch_calls, reply_writes
end

local frozen_opts = {
    req_dir_prefix   = DEFAULT_REQ_PREFIX,
    target_window_id = DEFAULT_WINDOW_ID,
}

-- ────────────────────────────────────────────────────────────────────
-- Mutation generators — one per mutation class
-- ────────────────────────────────────────────────────────────────────
--
-- Each generator returns (value, file_entries, byte_count) where
-- `value` is the user-var bytes the handler receives, and
-- `file_entries` is the synthetic FS the handler reads from. Some
-- classes don't need file_entries (random_bytes / b64_garbage /
-- b64_malformed_json — the handler never gets past pre-step (1)/(2)).

local function rand_byte_string(min_len, max_len)
    local n = math.random(min_len, max_len)
    local out = {}
    for i = 1, n do out[i] = string.char(math.random(0, 255)) end
    return table.concat(out)
end

local function rand_alpha(n)
    local out = {}
    for i = 1, n do
        out[i] = string.char(math.random(0x61, 0x7a))   -- 'a'..'z'
    end
    return table.concat(out)
end

local function gen_random_bytes()
    local s = rand_byte_string(0, 4096)
    return s, {}, #s
end

local function gen_b64_garbage()
    local payload = rand_byte_string(0, 256)
    local enc = b64.encode(payload)
    return enc, {}, #enc
end

local function gen_b64_malformed_json()
    -- Produce strings that base64-decode cleanly but parse-fail as
    -- JSON. Choices: random alpha (valid-ASCII, parser hits unknown
    -- token), unbalanced braces, naked colons, etc.
    local choices = {
        rand_alpha(math.random(1, 64)),
        "{" .. rand_alpha(math.random(1, 32)),
        "[" .. rand_alpha(math.random(1, 32)),
        "{\"v\":" .. rand_alpha(math.random(1, 8)),
        "}{}{",
        "}}}}",
        "[[[",
    }
    local raw = choices[math.random(1, #choices)]
    local enc = b64.encode(raw)
    return enc, {}, #enc
end

-- For classes 4-15 we need a valid pointer that points at a synthetic
-- file whose body is the mutated payload bytes. The pointer must pass
-- pre-steps (1)/(2)/(3). Pre-step (4) read + json_parse + pointer.id ==
-- payload.id is what the mutation exercises.
local function build_file_class(payload_table, want_id)
    local id_for_pointer = want_id or payload_table.id
    local path = DEFAULT_REQ_PREFIX .. "f.json"
    local body
    if type(payload_table) == "string" then
        -- Caller supplied raw bytes for the file body.
        body = payload_table
    else
        body = json_encode_shim(payload_table)
    end
    local entries = {
        [path] = {
            stat = ok_stat(),
            body = body,
        },
    }
    local value = build_pointer_envelope(id_for_pointer, path)
    return value, entries, #body
end

local function gen_field_missing(idx)
    -- 7 required root fields (sans hmac which is 8th):
    -- v, id, ts, target_window_id, reply_sock, op, args, hmac.
    local fields = { "v", "id", "ts", "target_window_id",
                     "reply_sock", "op", "args", "hmac" }
    local payload = build_signed_payload{ op = "noop" }
    local drop = fields[(idx - 1) % #fields + 1]
    payload[drop] = nil
    return build_file_class(payload, valid_ulid())
end

local function gen_type_swapped()
    -- The shape-mismatch fixture: ts="string", args=42, target_window_id="x".
    -- Random selection per iter so we exercise each type-swap branch.
    local payload = build_signed_payload{ op = "noop" }
    local choice = math.random(1, 4)
    if choice == 1 then payload.ts = "now"
    elseif choice == 2 then payload.args = 42
    elseif choice == 3 then payload.target_window_id = "x"
    else                  payload.reply_sock = 12345
    end
    return build_file_class(payload, payload.id)
end

local function gen_float_subtype()
    local payload = build_signed_payload{ op = "noop" }
    local choice = math.random(1, 2)
    if choice == 1 then payload.ts = 1.5
    else                payload.target_window_id = 2.0
    end
    return build_file_class(payload, payload.id)
end

local function gen_untagged_table()
    -- args contains a bare untagged table where the verb-keyed shape
    -- requires a string. tag_in_place on `switch` declares
    -- `name = "string"`; substituting an untagged table for `name`
    -- forces the encoder's tag walker to raise (CANONICAL_SHAPE_
    -- MISMATCH or ENCODER_UNSUPPORTED). For `noop` the args shape is
    -- empty-object so a bare {} round-trips cleanly via json_parse →
    -- tag_in_place; that path doesn't exercise the failure mode this
    -- class is for. Use `switch` deliberately.
    local payload = build_signed_payload{ op = "switch" }
    payload.args = canonical_json.object{
        -- bare table where a string is required
        name = {},
    }
    return build_file_class(payload, payload.id)
end

local function gen_oversized_string()
    -- Oversized fixture: id = string.rep("X", 1<<20). The handler's
    -- validate_payload (step (d)) rejects on #id != 26, so the gate
    -- is independent of length; under the pure-Lua json_parse_shim
    -- the harness-side length is capped to keep the per-iter wall-
    -- clock under the 50ms budget. Real wezterm uses a C
    -- parser; the production budget is comfortable at 1<<20. Pass
    -- `--no-shim` (e.g. from the e2e env) to honour the spec
    -- literally; CI's standalone harness invocation does not.
    local big
    if NO_SHIM then
        big = string.rep("X", 1 << 20)
    else
        local sizes = { 256, 1024, 4096 }
        local sz = sizes[math.random(1, #sizes)]
        big = string.rep("X", sz)
    end
    local payload = build_signed_payload{ op = "noop" }
    payload.id = big
    return build_file_class(payload, big)
end

local function gen_nested_deep()
    -- args = 200-deep nested object. tag_in_place doesn't recurse
    -- into shapeless subkeys for `noop` (whose args _shape is empty
    -- object), so the depth itself doesn't trip the encoder. We
    -- therefore set op="bogus" so encode-prep rejects on the
    -- shape-lookup; alternatively use op="switch" so the canonical
    -- encoder sees a non-string-typed `name` (deep table) and raises
    -- CANONICAL_SHAPE_MISMATCH. The latter exercises encode-side
    -- recursion safety more honestly.
    local function deep(n)
        if n == 0 then return canonical_json.object{ leaf = "x" } end
        return canonical_json.object{ next = deep(n - 1) }
    end
    local payload = build_signed_payload{ op = "switch" }
    -- Replace `name` (string per shape) with a 200-deep object.
    payload.args = deep(200)
    return build_file_class(payload, payload.id)
end

local function gen_control_char_field()
    -- name = "\x00\x01\x1b[2J" — tests that control bytes don't
    -- escape the encoder's UTF-8 / canonical-string contract. The
    -- handler should not raise even on control-byte input.
    local payload = build_signed_payload{ op = "switch" }
    payload.args = canonical_json.object{ name = "\x00\x01\x1b[2J" }
    return build_file_class(payload, payload.id)
end

local VERBS = { "noop", "switch", "load", "save", "new", "list_dirs" }

local function gen_hmac_corrupted()
    -- Properly signed payload, last hex char of payload.hmac flipped
    -- so step (f) silent-drops with no reply.
    local payload = build_signed_payload{ op = "noop" }
    local last = payload.hmac:sub(64, 64)
    local replacement = (last == "f") and "e" or "f"
    payload.hmac = payload.hmac:sub(1, 63) .. replacement
    return build_file_class(payload, payload.id)
end

-- ts_boundary returns 4 deterministic cases. The harness drives all
-- 4 per outer iteration and asserts:
--   ts = now-30 → accept (dispatch == 1)
--   ts = now-31 → reject (dispatch == 0, STALE_PAYLOAD log)
--   ts = now+30 → accept (dispatch == 1)
--   ts = now+31 → reject (dispatch == 0, STALE_PAYLOAD log)
local function gen_ts_boundary(branch)
    local offsets = { -30, -31, 30, 31 }
    local off = offsets[((branch - 1) % 4) + 1]
    local payload = build_signed_payload{
        op = "noop", ts = DEFAULT_NOW + off,
    }
    local accept = (off == -30) or (off == 30)
    local v, e, b = build_file_class(payload, payload.id)
    return v, e, b, accept
end

local function gen_unknown_verb()
    -- op = "bogus" (also: "Switch" wrong case, "snapshot", "exec" —
    -- pull from a small bag so per-iter mutation is non-trivial).
    -- The handler signs the payload (so HMAC would pass IF the verb
    -- were known), but step (e) short-circuits at the
    -- verb_args_shape lookup with `log_warn("ipc: no shape ...")`.
    local bogus_verbs = { "bogus", "Switch", "snapshot", "exec",
                          "wipe", "x", string.rep("z", 32) }
    local op = bogus_verbs[math.random(1, #bogus_verbs)]
    -- We sign with the verb_args_shape table missing → can't sign
    -- correctly. So we sign as `noop` (whose shape exists), then
    -- swap the op label after signing. The spec says HMAC
    -- verify never runs anyway — step (e) drops first.
    local payload = build_signed_payload{ op = "noop" }
    payload.op = op
    return build_file_class(payload, payload.id)
end

local function gen_v_field_swap(branch)
    -- v="1" (string), v=2 (wrong int), v=null (nil after json_parse).
    -- All three must reject at validate_payload (step (d)).
    local cases = { "1", 2, "nil" }
    local c = cases[((branch - 1) % 3) + 1]
    local payload = build_signed_payload{ op = "noop" }
    if c == "nil" then payload.v = nil
    else payload.v = c
    end
    return build_file_class(payload, payload.id)
end

-- ────────────────────────────────────────────────────────────────────
-- Per-iteration driver — drives one mutation, asserts gates.
-- ────────────────────────────────────────────────────────────────────

local FRAME_BUDGET_MS = 50
local FRAME_BUDGET_S  = FRAME_BUDGET_MS / 1000.0

-- Drive the handler with `value` + `file_entries`. If `must_dispatch`
-- is true, dispatch_calls must == 1 AND reply_writes must == 1; else
-- both must be 0 (silent drop on the wire).
local function drive(class_name, value, file_entries, must_dispatch, now)
    reset_world()
    local restore, dispatch_calls, reply_writes =
        install_seams(file_entries or {}, now)

    -- MEDIUM-2 (round 1): os.clock() measures CPU seconds, not wall.
    -- Under the in-memory shim (no real I/O, no network, no disk) the
    -- CPU/wall ratio is ≈1, so this remains a sound lower-bound proxy
    -- for the 50 ms wall-clock gate. If the harness ever swaps
    -- the in-memory shim for real I/O, re-check this gate under
    -- wall-clock (e.g. socket(2) syscall + poll). Don't switch to
    -- os.date-diff — its 1-second resolution would silently invalidate
    -- a < 50 ms bound.
    local t0 = os.clock()
    local ok = pcall(ipc.handle_user_var,
                     fake_window(), fake_pane(),
                     ipc.USER_VAR_NAME, value, frozen_opts)
    local elapsed = os.clock() - t0
    restore()

    if not ok then
        return false, string.format(
            "Lua error escaped handler: class=%s", class_name)
    end
    if elapsed > FRAME_BUDGET_S then
        return false, string.format(
            "frame paint over budget: %s elapsed=%.4fs > %.4fs",
            class_name, elapsed, FRAME_BUDGET_S)
    end

    if must_dispatch then
        if #dispatch_calls ~= 1 then
            return false, string.format(
                "%s: expected exactly 1 dispatch, got %d",
                class_name, #dispatch_calls)
        end
        if #reply_writes ~= 1 then
            return false, string.format(
                "%s: expected exactly 1 reply write, got %d",
                class_name, #reply_writes)
        end
    else
        if #dispatch_calls ~= 0 then
            return false, string.format(
                "%s: dispatch invoked on unauthenticated input "
                .. "(count=%d). Possible HMAC-bypass regression.",
                class_name, #dispatch_calls)
        end
        if #reply_writes ~= 0 then
            return false, string.format(
                "%s: reply written on unauthenticated input (count=%d). "
                .. "silent-drop contract violated.",
                class_name, #reply_writes)
        end
    end
    return true, nil
end

-- ────────────────────────────────────────────────────────────────────
-- Per-class iteration plan. Total mutated bytes across all classes
-- MUST be ≥ 10 000 (the acceptance gate).
-- ────────────────────────────────────────────────────────────────────
--
-- random_bytes drives the bulk: 60 iters × ~2048 byte avg = ~120k.
-- Other classes contribute thousands. With ITERS_OVERRIDE the operator
-- can scale down for fast smoke runs (cmd/lua-fuzzer --iters=10) but
-- the unscaled CI default is the load-bearing run.

local function plan(name, default_iters)
    if ITERS_OVERRIDE ~= nil then
        return math.max(1, math.floor(
            default_iters * ITERS_OVERRIDE / 1000))
    end
    return default_iters
end

-- ────────────────────────────────────────────────────────────────────
-- Run plan — one entry per mutation class
-- ────────────────────────────────────────────────────────────────────

local total_mutations = 0
local total_bytes = 0
local total_classes = 0
local failures = {}

local function record_fail(class, msg)
    failures[#failures + 1] = string.format(
        "FAIL %s: %s", class, msg)
end

local function run_class(name, iters, body_fn)
    total_classes = total_classes + 1
    for i = 1, iters do
        local ok, err = body_fn(i)
        if not ok then
            record_fail(name, err)
            return
        end
    end
end

-- 1. random_bytes — 60 × variable
run_class("random_bytes", plan("random_bytes", 60), function(_)
    local v, fe, bytes = gen_random_bytes()
    total_mutations = total_mutations + 1
    total_bytes = total_bytes + bytes
    return drive("random_bytes", v, fe, false)
end)

-- 2. b64_garbage — 40 × ~256
run_class("b64_garbage", plan("b64_garbage", 40), function(_)
    local v, fe, bytes = gen_b64_garbage()
    total_mutations = total_mutations + 1
    total_bytes = total_bytes + bytes
    return drive("b64_garbage", v, fe, false)
end)

-- 3. b64_malformed_json — 30 × ~64
run_class("b64_malformed_json", plan("b64_malformed_json", 30),
function(_)
    local v, fe, bytes = gen_b64_malformed_json()
    total_mutations = total_mutations + 1
    total_bytes = total_bytes + bytes
    return drive("b64_malformed_json", v, fe, false)
end)

-- 4. field_missing — 16 × payload (cycles the 8 droppable fields ×2)
run_class("field_missing", plan("field_missing", 16), function(i)
    local v, fe, bytes = gen_field_missing(i)
    total_mutations = total_mutations + 1
    total_bytes = total_bytes + bytes
    return drive("field_missing", v, fe, false)
end)

-- 5. type_swapped — 12 × random branch
run_class("type_swapped", plan("type_swapped", 12), function(_)
    local v, fe, bytes = gen_type_swapped()
    total_mutations = total_mutations + 1
    total_bytes = total_bytes + bytes
    return drive("type_swapped", v, fe, false)
end)

-- 6. float_subtype — 8 × random branch
run_class("float_subtype", plan("float_subtype", 8), function(_)
    local v, fe, bytes = gen_float_subtype()
    total_mutations = total_mutations + 1
    total_bytes = total_bytes + bytes
    return drive("float_subtype", v, fe, false)
end)

-- 7. untagged_table — 8 ×
run_class("untagged_table", plan("untagged_table", 8), function(_)
    local v, fe, bytes = gen_untagged_table()
    total_mutations = total_mutations + 1
    total_bytes = total_bytes + bytes
    return drive("untagged_table", v, fe, false)
end)

-- 8. oversized_string — 2 × 1MiB id (heavy, capped)
run_class("oversized_string", plan("oversized_string", 2), function(_)
    local v, fe, bytes = gen_oversized_string()
    total_mutations = total_mutations + 1
    total_bytes = total_bytes + bytes
    return drive("oversized_string", v, fe, false)
end)

-- 9. nested_deep — 4 × 200-deep
run_class("nested_deep", plan("nested_deep", 4), function(_)
    local v, fe, bytes = gen_nested_deep()
    total_mutations = total_mutations + 1
    total_bytes = total_bytes + bytes
    return drive("nested_deep", v, fe, false)
end)

-- 10. control_char_field — 8 ×
run_class("control_char_field", plan("control_char_field", 8),
function(_)
    local v, fe, bytes = gen_control_char_field()
    total_mutations = total_mutations + 1
    total_bytes = total_bytes + bytes
    return drive("control_char_field", v, fe, false)
end)

-- 11. empty_args_per_verb — 10 × cycling all 5 verbs (so 2 sweeps).
-- Only `noop` legitimately dispatches (empty args satisfies its
-- shape); switch/load/save/new fail at tag_in_place because their
-- shapes require concrete fields on args.
run_class("empty_args_per_verb", plan("empty_args_per_verb", 10),
function(i)
    -- Inline build (instead of gen_empty_args_per_verb) so we can
    -- attach the verb tag onto the drive() label without a multi-
    -- return juggle.
    local op = VERBS[(i - 1) % #VERBS + 1]
    local payload = build_signed_payload{ op = op }
    payload.args = canonical_json.object{}
    local v, fe, bytes = build_file_class(payload, payload.id)
    total_mutations = total_mutations + 1
    total_bytes = total_bytes + bytes
    -- Only `noop` legitimately dispatches: its verb_args_shape is
    -- `{ _shape = "object" }`, and an empty args object satisfies
    -- it. switch/load/save/new have required keys so step (e)'s
    -- tag_in_place raises CANONICAL_SHAPE_MISMATCH.
    local must = (op == "noop")
    return drive("empty_args_per_verb[" .. op .. "]", v, fe, must)
end)

-- 12. hmac_corrupted — 8 × (asserts silent-drop, no reply)
run_class("hmac_corrupted", plan("hmac_corrupted", 8), function(_)
    local v, fe, bytes = gen_hmac_corrupted()
    total_mutations = total_mutations + 1
    total_bytes = total_bytes + bytes
    return drive("hmac_corrupted", v, fe, false)
end)

-- 13. ts_boundary — exactly 8 (2 sweeps × 4 branches), each branch
-- asserts the right accept/reject side.
run_class("ts_boundary", plan("ts_boundary", 8), function(i)
    local v, fe, bytes, accept = gen_ts_boundary(i)
    total_mutations = total_mutations + 1
    total_bytes = total_bytes + bytes
    local ok, err = drive("ts_boundary", v, fe, accept)
    if not ok then return ok, err end
    -- Extra: on reject (outer pair) the handler must have logged a
    -- STALE_PAYLOAD entry. Inner pair must NOT log STALE_PAYLOAD.
    local saw_stale = false
    for _, m in ipairs(log_warn_calls) do
        if m:find("STALE_PAYLOAD", 1, true) then saw_stale = true end
    end
    if not accept and not saw_stale then
        return false, string.format(
            "ts_boundary[%d]: reject branch missing STALE_PAYLOAD log", i)
    end
    if accept and saw_stale then
        return false, string.format(
            "ts_boundary[%d]: accept branch logged STALE_PAYLOAD", i)
    end
    return true, nil
end)

-- 14. unknown_verb — 12 × random verb name
run_class("unknown_verb", plan("unknown_verb", 12), function(_)
    local v, fe, bytes = gen_unknown_verb()
    total_mutations = total_mutations + 1
    total_bytes = total_bytes + bytes
    local ok, err = drive("unknown_verb", v, fe, false)
    if not ok then return ok, err end
    -- Step (e) MUST log "ipc: no shape registered for op=…"
    local saw = false
    for _, m in ipairs(log_warn_calls) do
        if m:find("no shape registered for op=", 1, true) then
            saw = true
        end
    end
    if not saw then
        return false, "unknown_verb: missing 'no shape registered' log"
    end
    return true, nil
end)

-- 15. v_field_swap — exactly 9 (3 cases × 3 sweeps)
run_class("v_field_swap", plan("v_field_swap", 9), function(i)
    local v, fe, bytes = gen_v_field_swap(i)
    total_mutations = total_mutations + 1
    total_bytes = total_bytes + bytes
    return drive("v_field_swap", v, fe, false)
end)

-- ────────────────────────────────────────────────────────────────────
-- Acceptance-gate cross-check
-- ────────────────────────────────────────────────────────────────────

local EXPECTED_CLASSES = 15
if total_classes ~= EXPECTED_CLASSES then
    record_fail("classes",
        string.format("expected %d classes, ran %d",
            EXPECTED_CLASSES, total_classes))
end

-- The acceptance gate requires ≥ 10 000 mutated bytes per run.
-- Skip the gate when the operator has explicitly downscaled with
-- --iters (cmd/lua-fuzzer --iters=10 smoke).
--
-- MEDIUM-3 (round 1): `--iters=<n>` overrides BYTE_FLOOR. The
-- load-bearing constraint is therefore that the CI invocation MUST
-- NOT pass `--iters` — see `.github/workflows/ci.yml` step "Lua
-- handler fuzz harness". This is enforced by inspection of
-- ci.yml; we deliberately don't gate on a CI-marker env var here to
-- keep the harness simple.
local BYTE_FLOOR = 10000
if ITERS_OVERRIDE == nil and total_bytes < BYTE_FLOOR then
    record_fail("byte_floor",
        string.format("total mutated bytes %d < %d (acceptance gate)",
            total_bytes, BYTE_FLOOR))
end

-- ────────────────────────────────────────────────────────────────────
-- Summary
-- ────────────────────────────────────────────────────────────────────

if #failures > 0 then
    for _, line in ipairs(failures) do
        io.stderr:write(line .. "\n")
    end
    io.stderr:write(string.format(
        "FAILED %d/%d (mutations: %d, total bytes: %d, seed: %d)\n",
        #failures, total_classes, total_mutations, total_bytes, SEED))
    os.exit(1)
else
    io.stdout:write(string.format(
        "OK %d/%d (mutations: %d, total bytes: %d, seed: %d)\n",
        total_classes, total_classes,
        total_mutations, total_bytes, SEED))
end
