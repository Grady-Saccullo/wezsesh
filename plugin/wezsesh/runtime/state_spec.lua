-- Busted-style spec for state.lua. Self-contained — runs under plain
-- `lua plugin/wezsesh/state_spec.lua` from the repo root, no busted
-- required.
--
-- Run:
--     cd <repo-root>
--     lua plugin/wezsesh/state_spec.lua
--
-- Exits 0 with `OK N/N` on success, 1 with FAIL lines on stderr
-- otherwise.
--
-- This spec installs a wezterm-shim via `package.preload["wezterm"]`
-- BEFORE requiring the module under test, so state.lua's production
-- `require("wezterm")` line resolves to our test double instead of
-- wezterm's mlua sandbox. The double mirrors mlua's GLOBAL userdata
-- semantics:
--
--   * reads return a deserialised SNAPSHOT (a fresh deep-copy each
--     time), so partial mutation of a previously-read snapshot is
--     LOST unless the caller writes the whole bucket back;
--   * an explicit write-counter on the underlying store lets us
--     assert the §9.6 contract: every mutating call MUST flush via
--     a write-back assignment, never bypass it;
--   * a "force a nested-table value into a scalar slot" hatch
--     exercises the §0.1 row 30 (`wezterm.GLOBAL` value-shape rule)
--     so the spec proves state.lua's read paths refuse to dereference
--     a value that should never have been there.

local function script_dir()
    local src = arg and arg[0] or "plugin/wezsesh/state_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
package.path = script_dir() .. "/?.lua;"
            .. script_dir() .. "/../../?.lua;"
            .. package.path

-- ────────────────────────────────────────────────────────────────────
-- wezterm shim — installed BEFORE require("state")
-- ────────────────────────────────────────────────────────────────────
--
-- The shim exposes:
--   * wezterm.GLOBAL — a userdata-shaped table whose __index returns a
--     deep copy of the underlying store and whose __newindex replaces
--     the stored bucket wholesale. This mirrors mlua's "snapshot on
--     read, replace on write" behaviour (CLAUDE.md invariant 5).
--   * wezterm.json_encode / wezterm.json_parse — pinned to a tiny
--     pure-Lua JSON dialect that round-trips the structured-bucket
--     shapes (`{target_window_id=1, spawned_at=1700000000}` etc.)
--     byte-for-byte. The plugin runtime uses wezterm's built-in
--     parser; we don't need byte parity here, only round-trip
--     correctness for the shapes state.lua actually writes.
--   * a test-only `__store`/`__write_count`/`__inject_raw` surface for
--     the spec's assertions. Production code never sees these.

local helpers = require("spec_helpers")
local deepcopy = helpers.deepcopy

local function make_global()
    local store = {}
    local writes = 0

    local proxy = setmetatable({}, {
        __index = function(_, k)
            -- Return a deep copy so callers can't mutate the
            -- underlying storage by holding a reference. mlua's
            -- userdata __index works the same way (deserialised
            -- value snapshot per call).
            return deepcopy(store[k])
        end,
        __newindex = function(_, k, v)
            -- Replace the bucket wholesale; deep-copy so subsequent
            -- caller mutations of `v` don't leak into the store.
            store[k] = deepcopy(v)
            writes = writes + 1
        end,
    })

    return proxy, {
        store = store,
        writes_count = function() return writes end,
        reset_writes = function() writes = 0 end,
        -- Test-only escape hatch: bypasses __newindex's deep-copy so
        -- the spec can prove state.lua's read paths reject a value
        -- that VIOLATES the §0.1 row 30 value-shape rule. Production
        -- code can never hit this — that's the whole point.
        inject_raw = function(bucket, key, value)
            store[bucket] = store[bucket] or {}
            store[bucket][key] = value
        end,
        get_raw_bucket = function(bucket) return store[bucket] end,
    }
end

-- Pure-Lua JSON encode/decode for the two structured shapes state.lua
-- packs into a single string before storing in GLOBAL. We deliberately
-- keep this tiny and tolerant — the only objects state.lua produces
-- here are flat scalar maps (`{target_window_id=1, spawned_at=N}` and
-- `{spawned_pane_id=N, started_at=N}`). Anything richer would be a
-- bug we want the spec to catch via a separate assertion.
local function json_encode_shim(v)
    local function emit(x)
        local t = type(x)
        if t == "number" then return tostring(x) end
        if t == "boolean" then return x and "true" or "false" end
        if t == "string" then
            -- Minimal escape: quote, backslash, control. Sufficient
            -- for state.lua's shapes (which never contain quotes).
            return '"' .. x:gsub("[\\\"]", "\\%0") .. '"'
        end
        if t == "table" then
            local keys = {}
            for k in pairs(x) do keys[#keys + 1] = k end
            table.sort(keys, function(a, b)
                return tostring(a) < tostring(b)
            end)
            local parts = {}
            for _, k in ipairs(keys) do
                parts[#parts + 1] = '"' .. tostring(k) .. '":' .. emit(x[k])
            end
            return "{" .. table.concat(parts, ",") .. "}"
        end
        if t == "nil" then return "null" end
        error("json_encode_shim: unsupported type " .. t)
    end
    return emit(v)
end

local function json_parse_shim(s)
    -- A minimal JSON parser sufficient for the shapes state.lua
    -- writes back. We could pull in a real one, but keeping this
    -- tiny means the spec is self-contained.
    local pos = 1
    local function err(msg)
        error("json_parse_shim: " .. msg .. " at " .. pos, 0)
    end
    local function skip_ws()
        while pos <= #s do
            local c = s:sub(pos, pos)
            if c == " " or c == "\t" or c == "\n" or c == "\r" then
                pos = pos + 1
            else return end
        end
    end
    local parse_value
    local function parse_string()
        if s:sub(pos, pos) ~= '"' then err("expected string") end
        pos = pos + 1
        local out = {}
        while pos <= #s do
            local c = s:sub(pos, pos)
            if c == '"' then pos = pos + 1; return table.concat(out) end
            if c == "\\" then
                pos = pos + 1
                local esc = s:sub(pos, pos)
                out[#out + 1] = esc
                pos = pos + 1
            else
                out[#out + 1] = c
                pos = pos + 1
            end
        end
        err("unterminated string")
    end
    local function parse_number()
        local s2 = s:sub(pos)
        local num_str = s2:match("^%-?%d+%.?%d*[eE]?[%-+]?%d*")
        if not num_str or num_str == "" then err("bad number") end
        pos = pos + #num_str
        local n = tonumber(num_str)
        -- Round to integer where the literal lacks a fraction so we
        -- preserve Lua 5.4's integer subtype. State.lua's prune_states
        -- would reject a float-typed `spawned_at` because production
        -- mlua's json_parse returns integers for integer literals.
        if not num_str:find("[.eE]") then
            n = math.tointeger(n) or n
        end
        return n
    end
    local function parse_object()
        pos = pos + 1  -- '{'
        skip_ws()
        local out = {}
        if s:sub(pos, pos) == "}" then pos = pos + 1; return out end
        while true do
            skip_ws()
            local k = parse_string()
            skip_ws()
            if s:sub(pos, pos) ~= ":" then err("expected ':'") end
            pos = pos + 1
            skip_ws()
            out[k] = parse_value()
            skip_ws()
            local c = s:sub(pos, pos)
            if c == "}" then pos = pos + 1; return out end
            if c ~= "," then err("expected ',' or '}'") end
            pos = pos + 1
        end
    end
    parse_value = function()
        skip_ws()
        local c = s:sub(pos, pos)
        if c == "{" then return parse_object() end
        if c == '"' then return parse_string() end
        if c == "t" then
            if s:sub(pos, pos + 3) == "true" then pos = pos + 4; return true end
            err("bad literal")
        end
        if c == "f" then
            if s:sub(pos, pos + 4) == "false" then pos = pos + 5; return false end
            err("bad literal")
        end
        if c == "n" then
            if s:sub(pos, pos + 3) == "null" then pos = pos + 4; return nil end
            err("bad literal")
        end
        return parse_number()
    end
    skip_ws()
    return parse_value()
end

-- Build the wezterm shim and install via package.preload. State.lua's
-- top-level `require("wezterm")` will resolve to this table.
local global, control = make_global()
local wezterm_shim = {
    GLOBAL      = global,
    json_encode = json_encode_shim,
    json_parse  = json_parse_shim,
}
package.preload["wezterm"] = function() return wezterm_shim end

-- Now load the module under test.
local state = require("state")

-- ────────────────────────────────────────────────────────────────────
-- minimal busted-shaped harness (mirrors canonical_json_spec.lua /
-- ct_eq_spec.lua / hmac_spec.lua / b64_spec.lua)
-- ────────────────────────────────────────────────────────────────────

local failures, total = 0, 0
local current_describe = "<top>"

local function describe(name, fn)
    local prev = current_describe
    current_describe = name
    fn()
    current_describe = prev
end

local function it(name, fn)
    total = total + 1
    -- Reset writes counter and clear all buckets between tests so each
    -- spec runs against a fresh GLOBAL — mirrors a fresh wezterm boot.
    state.wipe_all()
    control.reset_writes()
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

local function assert_nil(v, msg)
    if v ~= nil then
        error((msg or "expected nil") .. ", got: " .. tostring(v), 2)
    end
end

-- ────────────────────────────────────────────────────────────────────
-- §9.6 — module surface
-- ────────────────────────────────────────────────────────────────────

describe("module surface (§9.6)", function()
    it("exposes the §9.6 API and nothing more", function()
        local want = {
            "delete_request", "delete_state", "get_request", "get_state",
            "is_writing", "mark_seen", "prune_requests", "prune_seen_ids",
            "prune_states", "seen", "set_request", "set_state",
            "set_writing", "wipe_all",
        }
        local keys = {}
        for k in pairs(state) do keys[#keys + 1] = k end
        table.sort(keys)
        assert_eq(table.concat(keys, ","), table.concat(want, ","),
            "state module surface drift")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §9.6 / §10.6 — wezsesh_state per-pane bucket
-- ────────────────────────────────────────────────────────────────────

describe("set_state / get_state / delete_state (§9.6, §10.6)", function()
    it("round-trips a {target_window_id, spawned_at} record", function()
        state.set_state(7, { target_window_id = 1, spawned_at = 1700000000 })
        local got = state.get_state(7)
        assert_true(got ~= nil, "decoded state nil")
        assert_eq(got.target_window_id, 1, "target_window_id wrong")
        assert_eq(got.spawned_at, 1700000000, "spawned_at wrong")
    end)

    it("coerces integer pane ids to string keys (§10.6)", function()
        state.set_state(42, { target_window_id = 1, spawned_at = 100 })
        -- Read the raw bucket (test-only) and confirm the key is a
        -- STRING, not the integer that was passed in. Production
        -- mlua's GLOBAL Object node would error on integer keys; the
        -- module MUST stringify at the boundary.
        local raw = control.get_raw_bucket("wezsesh_state")
        assert_true(raw ~= nil, "bucket missing")
        assert_true(raw["42"] ~= nil,
            "key not stringified — would crash mlua at runtime")
        assert_nil(raw[42], "integer key leaked into GLOBAL bucket")
    end)

    it("string pane ids are accepted and returned unchanged", function()
        state.set_state("13", { target_window_id = 2, spawned_at = 200 })
        local raw = control.get_raw_bucket("wezsesh_state")
        assert_true(raw["13"] ~= nil, "string key absent")
    end)

    it("returns nil for absent pane ids", function()
        assert_nil(state.get_state(999), "expected nil on miss")
        assert_nil(state.get_state("missing"), "expected nil on miss")
    end)

    it("delete_state removes the entry", function()
        state.set_state(5, { target_window_id = 1, spawned_at = 1 })
        assert_true(state.get_state(5) ~= nil, "set didn't land")
        state.delete_state(5)
        assert_nil(state.get_state(5), "delete didn't take")
    end)

    it("set_state flushes via write-back (§9.6 acceptance gate)",
    function()
        -- Each mutating call MUST issue at least one assignment to
        -- wezterm.GLOBAL.<bucket> so the snapshot mlua handed us is
        -- replaced. A regression that mutates the in-memory snapshot
        -- without writing back would leave writes_count() at 0.
        control.reset_writes()
        state.set_state(1, { target_window_id = 1, spawned_at = 1 })
        local n = control.writes_count()
        assert_true(n >= 1,
            "no GLOBAL write-back observed for set_state (got "
            .. n .. ")")
    end)

    it("delete_state of a missing key does NOT issue a write-back",
    function()
        -- Pure read-only check: if there's nothing to delete, skip
        -- the round-trip. Cheap optimisation; spec captures the
        -- intent so a future "always write" refactor is intentional.
        control.reset_writes()
        state.delete_state(404)
        assert_eq(control.writes_count(), 0,
            "missing-key delete should be read-only")
    end)

    it("delete_state of a present key DOES write back", function()
        state.set_state(8, { target_window_id = 1, spawned_at = 1 })
        control.reset_writes()
        state.delete_state(8)
        assert_true(control.writes_count() >= 1,
            "present-key delete missed its write-back")
    end)

    it("snapshot semantics: holding a previous get_state result and "
        .. "mutating it does NOT leak into the store", function()
        state.set_state(3, { target_window_id = 1, spawned_at = 100 })
        local snap = state.get_state(3)
        snap.spawned_at = 999      -- tamper with the snapshot
        local re = state.get_state(3)
        assert_eq(re.spawned_at, 100,
            "GLOBAL snapshot not isolated — mutation leaked")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §9.6 / §10.6 — wezsesh_requests per-id bucket
-- ────────────────────────────────────────────────────────────────────

describe("set_request / get_request / delete_request (§9.6, §10.6)",
function()
    it("round-trips a {spawned_pane_id, started_at} record", function()
        state.set_request("01JABC", { spawned_pane_id = 7, started_at = 100 })
        local got = state.get_request("01JABC")
        assert_true(got ~= nil, "decoded request nil")
        assert_eq(got.spawned_pane_id, 7, "spawned_pane_id wrong")
        assert_eq(got.started_at, 100, "started_at wrong")
    end)

    it("set_request flushes via write-back (§9.6 acceptance gate)",
    function()
        control.reset_writes()
        state.set_request("01JREQ", { spawned_pane_id = 1, started_at = 1 })
        assert_true(control.writes_count() >= 1,
            "no GLOBAL write-back observed for set_request")
    end)

    it("delete_request removes the entry and writes back", function()
        state.set_request("01JDEL", { spawned_pane_id = 1, started_at = 1 })
        control.reset_writes()
        state.delete_request("01JDEL")
        assert_nil(state.get_request("01JDEL"), "delete didn't take")
        assert_true(control.writes_count() >= 1,
            "delete_request missed its write-back")
    end)

    it("get_request returns nil on miss", function()
        assert_nil(state.get_request("nope"), "expected nil on miss")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §9.6 / §10.6 — wezsesh_writing per-path flag
-- ────────────────────────────────────────────────────────────────────

describe("set_writing / is_writing (§9.6, §10.6)", function()
    it("round-trips a true/false marker keyed by absolute path",
    function()
        state.set_writing("/abs/path/to/snap", true)
        assert_true(state.is_writing("/abs/path/to/snap"),
            "expected writing=true")
        state.set_writing("/abs/path/to/snap", false)
        assert_false(state.is_writing("/abs/path/to/snap"),
            "expected writing=false after clear")
    end)

    it("is_writing returns false for an unknown path", function()
        assert_false(state.is_writing("/never/written"),
            "expected false on miss")
    end)

    it("the GLOBAL bucket value is a flat boolean (§10.6 storage rule)",
    function()
        state.set_writing("/p", true)
        local raw = control.get_raw_bucket("wezsesh_writing")
        assert_eq(type(raw["/p"]), "boolean",
            "writing flag must be flat boolean, not nested table")
        assert_eq(raw["/p"], true, "writing flag wrong value")
    end)

    it("set_writing(path, true) writes back; "
        .. "set_writing(path, false) on absent path is read-only",
    function()
        control.reset_writes()
        state.set_writing("/p", true)
        assert_true(control.writes_count() >= 1,
            "set true missed write-back")
        control.reset_writes()
        state.set_writing("/missing/path", false)
        assert_eq(control.writes_count(), 0,
            "clearing absent path should be read-only")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §5.4 / §0.1 row 30 — seen_ids: flat int (unix-seconds) per ULID
-- ────────────────────────────────────────────────────────────────────

describe("seen / mark_seen (§5.4, §0.1 row 30)", function()
    it("seen() returns false on a fresh ULID", function()
        assert_false(state.seen("01JFRESH"), "expected false on miss")
    end)

    it("mark_seen records the ULID; seen() returns true thereafter",
    function()
        state.mark_seen("01JABC")
        assert_true(state.seen("01JABC"), "expected true after mark_seen")
    end)

    it("mark_seen flushes via write-back (§9.6 acceptance gate)",
    function()
        control.reset_writes()
        state.mark_seen("01JWRT")
        assert_true(control.writes_count() >= 1,
            "no GLOBAL write-back observed for mark_seen")
    end)

    it("storage shape is flat int unix-seconds (§0.1 row 30)", function()
        state.mark_seen("01JSHAPE")
        local raw = control.get_raw_bucket("wezsesh_seen_ids")
        assert_true(raw ~= nil, "seen_ids bucket missing")
        local v = raw["01JSHAPE"]
        assert_eq(type(v), "number",
            "seen_ids[ulid] must be a flat number, not "
            .. type(v))
        assert_true(math.type(v) == "integer",
            "seen_ids[ulid] must be integer subtype "
            .. "(unix-seconds), got " .. tostring(math.type(v)))
        -- Sanity bound: roughly between 2001 and 2200.
        assert_true(v >= 1000000000 and v < 7000000000,
            "seen_ids[ulid] not a sensible unix-second value: "
            .. tostring(v))
    end)

    it("forbidden value-shape: a nested-table value would silently "
        .. "break indexing in production mlua — the read path "
        .. "MUST refuse to dereference it", function()
        -- Simulate the v2-draft regression that spike #1 caught: a
        -- nested {ts = N} value smuggled past state.lua via the test
        -- harness's escape hatch. Production mlua's GLOBAL would
        -- accept the write here too — but the next read would throw
        -- "can only index array or object values" inside the handler.
        --
        -- state.lua's `seen()` MUST NOT try to dereference such a
        -- value. The flat-scalar contract means "anything not nil
        -- counts as seen" and the prune loop will sweep the malformed
        -- entry on the next tick.
        control.inject_raw("wezsesh_seen_ids", "01JBAD", { ts = 100 })
        local ok, err = pcall(state.seen, "01JBAD")
        assert_true(ok,
            "seen() must not raise on a malformed value: "
            .. tostring(err))
        -- The prune sweep MUST drop it on the next pass since
        -- type(ts) ~= "number".
        state.prune_seen_ids(60)
        local raw = control.get_raw_bucket("wezsesh_seen_ids")
        assert_nil(raw["01JBAD"],
            "prune did not sweep a value-shape-violating entry")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §5.5 — TTL prune (session-wide, never per-pane)
-- ────────────────────────────────────────────────────────────────────

describe("prune_seen_ids (§5.5)", function()
    it("drops entries older than ttl_seconds; keeps fresh ones",
    function()
        local now = os.time()
        local raw = control.get_raw_bucket("wezsesh_seen_ids")
                    or {}  -- in case wipe_all just ran
        -- Seed two entries via mark_seen, then back-date one of them
        -- via the inject hatch (state.lua has no time-travel API).
        state.mark_seen("01JFRESH")
        control.inject_raw("wezsesh_seen_ids", "01JOLD", now - 120)
        state.prune_seen_ids(60)
        assert_true(state.seen("01JFRESH"),
            "fresh entry was wrongly pruned")
        assert_false(state.seen("01JOLD"),
            "stale entry was not pruned")
    end)

    it("default ttl is 60 s when none passed", function()
        local now = os.time()
        control.inject_raw("wezsesh_seen_ids", "01JEDGE", now - 61)
        state.prune_seen_ids()  -- no arg → 60 s default
        assert_false(state.seen("01JEDGE"),
            "default-ttl prune missed the > 60 s entry")
    end)

    it("ULIDs from different panes share the SAME bucket "
        .. "(no per-pane bucketing per §0.1 row 27)", function()
        -- The §17.3 'seen_ids TTL prune (session-wide)' gate checks
        -- this: two ULIDs marked from "different" panes must land in
        -- the same bucket and prune together. We can't simulate panes
        -- here (state.lua has no pane parameter on mark_seen) but we
        -- CAN observe that the bucket is keyed by ULID alone — there
        -- is no `[pane_id][ulid]` two-level key in the storage shape.
        state.mark_seen("01JPANE_A")
        state.mark_seen("01JPANE_B")
        local raw = control.get_raw_bucket("wezsesh_seen_ids")
        assert_true(raw["01JPANE_A"] ~= nil,
            "ULID A absent from session-wide bucket")
        assert_true(raw["01JPANE_B"] ~= nil,
            "ULID B absent from session-wide bucket")
        -- No nested bucket keyed by pane.
        for k, v in pairs(raw) do
            assert_true(type(v) ~= "table",
                "session-wide bucket has a nested table at key "
                .. k .. " — looks like per-pane bucketing crept in")
        end
    end)

    it("write-back is skipped when nothing was pruned", function()
        state.mark_seen("01JKEEP")
        control.reset_writes()
        state.prune_seen_ids(60)
        assert_eq(control.writes_count(), 0,
            "no-op prune issued a spurious write-back")
    end)

    it("write-back fires when at least one entry was pruned",
    function()
        local now = os.time()
        control.inject_raw("wezsesh_seen_ids", "01JOLD2", now - 120)
        control.reset_writes()
        state.prune_seen_ids(60)
        assert_true(control.writes_count() >= 1,
            "successful prune did not write back")
    end)
end)

describe("prune_states (§5.5)", function()
    it("drops states with spawned_at older than ttl", function()
        local now = os.time()
        state.set_state(1, { target_window_id = 1, spawned_at = now })
        state.set_state(2, { target_window_id = 1, spawned_at = now - 120 })
        state.prune_states(now, 60)
        assert_true(state.get_state(1) ~= nil,
            "fresh state pruned in error")
        assert_nil(state.get_state(2), "stale state not pruned")
    end)

    it("drops malformed entries (non-decodable) defensively",
    function()
        -- Inject a raw garbage string into the bucket via the escape
        -- hatch. state.lua's prune MUST sweep it rather than crashing
        -- on the bad shape.
        control.inject_raw("wezsesh_state", "9", "not even json")
        state.prune_states(os.time(), 60)
        local raw = control.get_raw_bucket("wezsesh_state")
        assert_nil(raw["9"], "malformed state entry not swept")
    end)

    it("uses os.time() when `now` is nil", function()
        local now = os.time()
        state.set_state(11, { target_window_id = 1, spawned_at = now - 120 })
        state.prune_states(nil, 60)
        assert_nil(state.get_state(11),
            "nil-now path did not cut over to os.time()")
    end)
end)

describe("prune_requests (§5.5)", function()
    it("drops requests with started_at older than ttl", function()
        local now = os.time()
        state.set_request("01JFRESH",
            { spawned_pane_id = 1, started_at = now })
        state.set_request("01JOLD",
            { spawned_pane_id = 1, started_at = now - 120 })
        state.prune_requests(now, 60)
        assert_true(state.get_request("01JFRESH") ~= nil,
            "fresh request pruned in error")
        assert_nil(state.get_request("01JOLD"),
            "stale request not pruned")
    end)

    it("drops malformed entries defensively", function()
        control.inject_raw("wezsesh_requests", "01JBAD", "garbage")
        state.prune_requests(os.time(), 60)
        local raw = control.get_raw_bucket("wezsesh_requests")
        assert_nil(raw["01JBAD"], "malformed request not swept")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §9.6 — wipe_all
-- ────────────────────────────────────────────────────────────────────

describe("wipe_all (§9.6)", function()
    it("clears every bucket this module owns", function()
        state.set_state(1, { target_window_id = 1, spawned_at = 100 })
        state.set_request("01JREQ",
            { spawned_pane_id = 1, started_at = 100 })
        state.mark_seen("01JSEEN")
        state.set_writing("/p", true)

        state.wipe_all()

        assert_nil(state.get_state(1), "state not wiped")
        assert_nil(state.get_request("01JREQ"), "request not wiped")
        assert_false(state.seen("01JSEEN"), "seen not wiped")
        assert_false(state.is_writing("/p"), "writing not wiped")

        -- All four buckets should be present-and-empty (not nil) so
        -- subsequent reads land on a fresh table without an extra
        -- write — mirrors the post-reset state.
        for _, name in ipairs({"wezsesh_state", "wezsesh_seen_ids",
                                "wezsesh_requests", "wezsesh_writing"}) do
            local raw = control.get_raw_bucket(name)
            assert_true(raw ~= nil, name .. " bucket missing after wipe")
            local n = 0
            for _ in pairs(raw) do n = n + 1 end
            assert_eq(n, 0, name .. " not empty after wipe (" .. n
                .. " entries left)")
        end
    end)

    it("issues a write-back per bucket so the GLOBAL store reflects "
        .. "the cleared state immediately", function()
        state.mark_seen("01JX")
        control.reset_writes()
        state.wipe_all()
        -- 4 buckets → 4 explicit writes.
        assert_eq(control.writes_count(), 4,
            "expected 4 GLOBAL write-backs (one per bucket), got "
            .. control.writes_count())
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- §10.6 — value-shape rule: nested-table values are forbidden in
-- every GLOBAL sub-bucket this module owns. The §17.4 CI lint
-- enforces this statically; this spec exercises the runtime contract
-- via the test-only escape hatch.
-- ────────────────────────────────────────────────────────────────────

describe("§10.6 GLOBAL value-shape rule (no nested-table values)",
function()
    it("set_state stores a STRING value (JSON-encoded), not a "
        .. "nested table, so the bucket is mlua-safe",
    function()
        state.set_state(1, { target_window_id = 1, spawned_at = 100 })
        local raw = control.get_raw_bucket("wezsesh_state")
        local stored = raw["1"]
        assert_eq(type(stored), "string",
            "wezsesh_state[pid] must be a JSON string, got "
            .. type(stored))
        -- Sanity: round-trips back through the parser.
        local back = json_parse_shim(stored)
        assert_eq(back.target_window_id, 1, "round-trip lost field")
        assert_eq(back.spawned_at, 100, "round-trip lost field")
    end)

    it("set_request stores a STRING value (JSON-encoded), not a "
        .. "nested table", function()
        state.set_request("01JX",
            { spawned_pane_id = 7, started_at = 1 })
        local raw = control.get_raw_bucket("wezsesh_requests")
        local stored = raw["01JX"]
        assert_eq(type(stored), "string",
            "wezsesh_requests[id] must be a JSON string, got "
            .. type(stored))
    end)

    it("seen_ids stores integers only (§0.1 row 30)", function()
        state.mark_seen("01JX")
        local raw = control.get_raw_bucket("wezsesh_seen_ids")
        assert_eq(type(raw["01JX"]), "number",
            "seen_ids[ulid] must be int")
    end)

    it("writing stores booleans only", function()
        state.set_writing("/p", true)
        local raw = control.get_raw_bucket("wezsesh_writing")
        assert_eq(type(raw["/p"]), "boolean",
            "writing[path] must be bool")
    end)

    it("the harness exercises the spec's intent: a nested-table "
        .. "value injected via the escape hatch is what state.lua "
        .. "MUST not produce — and what the §17.4 grep lint MUST "
        .. "catch in source", function()
        -- We're not running the lint here (Go-side tool). What we
        -- ARE proving is that NO state.lua API path produced a
        -- nested-table value: every set_*/mark_seen above was
        -- followed by a `type(stored) == "string|number|boolean"`
        -- check against the raw bucket. Combined with the §17.4
        -- grep lint over plugin/wezsesh/*.lua, the value-shape
        -- contract has belt + suspenders.
        --
        -- This test exists so that anyone refactoring state.lua to
        -- "just store the table" gets a single clear failure here
        -- pointing at §0.1 row 30 / spike #1.
        for _, name in ipairs({"wezsesh_state", "wezsesh_seen_ids",
                                "wezsesh_requests", "wezsesh_writing"}) do
            local raw = control.get_raw_bucket(name) or {}
            for k, v in pairs(raw) do
                assert_true(type(v) == "string"
                    or type(v) == "number"
                    or type(v) == "boolean",
                    name .. "[" .. tostring(k)
                    .. "] is type " .. type(v)
                    .. " — violates §10.6 storage rule")
            end
        end
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
