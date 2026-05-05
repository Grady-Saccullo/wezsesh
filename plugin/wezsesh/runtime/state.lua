-- Centralised access to `wezterm.GLOBAL.*` on the Lua side of the
-- IPC handler.
--
-- This module is the ONLY place in the plugin that touches the GLOBAL
-- userdata for the in-flight state buckets. It owns three responsibilities
-- the rest of the codebase cannot get right by itself:
--
--   1. Pane-ID coercion. Pane IDs come out of wezterm as Lua numbers
--      (integer subtype). `wezterm.GLOBAL.<bucket>` is an Object node
--      and rejects integer keys at runtime ("can only index objects
--      using string values"). Every entry point coerces via tostring(...).
--
--   2. Read-then-write-back discipline. mlua's GLOBAL userdata returns
--      a deserialised SNAPSHOT on every read (CLAUDE.md invariant 5) —
--      sub-table mutation requires an explicit assignment of the whole
--      bucket back to GLOBAL or the change is local and lost. Every
--      mutating function in this module reads the bucket, edits the
--      snapshot, and writes the snapshot back in one atomic-looking
--      operation.
--
--   3. Value-shape normalisation. All sub-table values MUST be flat
--      scalars (string, integer, boolean) — NEVER nested Lua tables.
--      mlua's GLOBAL accepts nested-table writes silently but throws
--      "can only index array or object values" on the next read of
--      the inner field. The two structured buckets
--      (`wezsesh_state[pid]`, `wezsesh_requests[id]`) pack their
--      fields into a JSON-encoded STRING via `wezterm.json_encode`;
--      the read-side decodes via `wezterm.json_parse`. The flat
--      buckets (`wezsesh_seen_ids[ulid]` → int, `wezsesh_writing[path]`
--      → bool) never see a table value at all.
--
-- mlua sandbox: acquired via `local wezterm = require("wezterm")`. The
-- standalone spec (`state_spec.lua`) installs a harness double via
-- `package.preload["wezterm"]` BEFORE requiring this file so the
-- production-shaped require() path is exercised end-to-end.
--
-- Errors are intentionally NOT raised here. The mutating helpers all
-- run inside the `user-var-changed` handler and a raise would wedge
-- the wezterm event loop (CLAUDE.md invariant 1). Bad shapes returned
-- by `wezterm.json_parse` (eg. on a corrupted GLOBAL) degrade to nil
-- so the handler fails closed at the verifier instead of crashing.

local wezterm = require("wezterm")

local M = {}

-- Bucket key names. Single source of truth — every other module
-- reaching into GLOBAL for these buckets goes through this file.
local KEY_STATE     = "wezsesh_state"
local KEY_SEEN_IDS  = "wezsesh_seen_ids"
local KEY_REQUESTS  = "wezsesh_requests"
local KEY_WRITING   = "wezsesh_writing"

-- Stringify a pane id (or request id, or path). Pane ids reach us as
-- Lua integers via `pane:pane_id()`; request ids and absolute paths
-- are already strings. Coerce uniformly so callers cannot accidentally
-- write an integer-keyed entry the next reader can't index.
local function key_str(k)
    if type(k) == "string" then return k end
    return tostring(k)
end

-- Read a top-level GLOBAL bucket as a plain Lua table. mlua's GLOBAL
-- userdata may return either a deserialised Lua table OR a userdata
-- proxy depending on the wezterm build; both forms support `pairs(...)`
-- and direct `b[k]` indexing, but only Lua tables can be mutated and
-- written back via `wezterm.GLOBAL[name] = bucket` and have the
-- mutations land. We always rebuild a fresh Lua table by iterating
-- so subsequent `bucket[k] = v` writes are local-only and the
-- write_bucket helper persists them atomically.
-- Direct single-key lookup on a GLOBAL bucket. wezterm's userdata
-- proxy responds correctly to `b[k]` indexing even though `pairs(b)`
-- can return a different keyspace than the one the writer used (we
-- write `b["18"]` but pairs may yield numeric `18` whose value is a
-- different / nil entry in the proxy). Bypassing pairs for reads
-- avoids that asymmetry entirely. Returns nil when the bucket is
-- absent or the key is missing.
local function lookup_bucket(name, key)
    local b = wezterm.GLOBAL[name]
    local bt = type(b)
    if bt ~= "table" and bt ~= "userdata" then return nil end
    local ok, v = pcall(function() return b[key] end)
    if not ok then return nil end
    return v
end

-- Read a top-level GLOBAL bucket as a fresh Lua table for the
-- mutate-then-write-back path. mlua's GLOBAL may return either a
-- deserialised Lua table OR a userdata proxy depending on the wezterm
-- build; only Lua tables can be mutated and written back. We rebuild
-- a fresh Lua table by iterating so subsequent `bucket[k] = v` writes
-- are local-only and the write_bucket helper persists them atomically.
-- Keys are coerced to strings: numeric-looking string keys can come
-- back as integers from pairs() on the userdata proxy.
local function read_bucket(name)
    local b = wezterm.GLOBAL[name]
    local bt = type(b)
    if bt ~= "table" and bt ~= "userdata" then return {} end
    local out = {}
    local ok = pcall(function()
        for k, v in pairs(b) do
            local sk = (type(k) == "string") and k or tostring(k)
            out[sk] = v
        end
    end)
    if not ok then return {} end
    return out
end

-- Write a bucket back to GLOBAL. The whole table is replaced — the
-- mlua quirk of "snapshot on read" means partial mutation of the
-- previously-read snapshot would otherwise be lost.
local function write_bucket(name, t)
    wezterm.GLOBAL[name] = t
end

-- Round-trip a structured value through JSON so the GLOBAL bucket
-- only ever stores a flat string scalar (the value-shape rule above).
-- Returns the encoded string or nil on failure.
local function encode_struct(v)
    if type(v) ~= "table" then return nil end
    local ok, s = pcall(wezterm.json_encode, v)
    if not ok or type(s) ~= "string" then return nil end
    return s
end

-- Inverse of encode_struct. nil-safe — feeding nil or a non-string
-- (which should never happen if we own all writes, but might if a
-- user reload mid-session left a stale value behind) returns nil and
-- the caller treats the entry as absent.
local function decode_struct(s)
    if type(s) ~= "string" then return nil end
    local ok, v = pcall(wezterm.json_parse, s)
    if not ok or type(v) ~= "table" then return nil end
    return v
end

-- ────────────────────────────────────────────────────────────────────
-- wezsesh_state[pane_id_str] : JSON-encoded
--                                     {target_window_id, spawned_at}
-- ────────────────────────────────────────────────────────────────────

function M.set_state(pane_id, state)
    local k = key_str(pane_id)
    local enc = encode_struct(state)
    if enc == nil then return end
    local bucket = read_bucket(KEY_STATE)
    bucket[k] = enc
    write_bucket(KEY_STATE, bucket)
end

function M.get_state(pane_id)
    local k = key_str(pane_id)
    return decode_struct(lookup_bucket(KEY_STATE, k))
end

function M.delete_state(pane_id)
    local k = key_str(pane_id)
    local bucket = read_bucket(KEY_STATE)
    if bucket[k] == nil then return end
    bucket[k] = nil
    write_bucket(KEY_STATE, bucket)
end

-- ────────────────────────────────────────────────────────────────────
-- wezsesh_requests[request_id] : JSON-encoded
--                                       {spawned_pane_id, started_at}
-- ────────────────────────────────────────────────────────────────────

function M.set_request(id, info)
    local k = key_str(id)
    local enc = encode_struct(info)
    if enc == nil then return end
    local bucket = read_bucket(KEY_REQUESTS)
    bucket[k] = enc
    write_bucket(KEY_REQUESTS, bucket)
end

function M.get_request(id)
    local k = key_str(id)
    return decode_struct(lookup_bucket(KEY_REQUESTS, k))
end

function M.delete_request(id)
    local k = key_str(id)
    local bucket = read_bucket(KEY_REQUESTS)
    if bucket[k] == nil then return end
    bucket[k] = nil
    write_bucket(KEY_REQUESTS, bucket)
end

-- ────────────────────────────────────────────────────────────────────
-- wezsesh_writing[abs_path] : bool (flat scalar)
-- ────────────────────────────────────────────────────────────────────

function M.set_writing(path, b)
    local k = key_str(path)
    local bucket = read_bucket(KEY_WRITING)
    if b then
        bucket[k] = true
    else
        if bucket[k] == nil then return end
        bucket[k] = nil
    end
    write_bucket(KEY_WRITING, bucket)
end

function M.is_writing(path)
    local k = key_str(path)
    return lookup_bucket(KEY_WRITING, k) == true
end

-- ────────────────────────────────────────────────────────────────────
-- wezsesh_seen_ids[ulid] : int unix-seconds (session-wide)
-- ────────────────────────────────────────────────────────────────────
--
-- Storage shape is FLAT INT (unix-seconds), not a
-- nested {ts = N} table. The v2 draft used a nested table; mlua's
-- GLOBAL silently accepted the write but threw "can only index array
-- or object values" on the next read of `entry.ts`. Fix is to keep
-- this bucket scalar-only.

function M.seen(id)
    local k = key_str(id)
    return lookup_bucket(KEY_SEEN_IDS, k) ~= nil
end

function M.mark_seen(id)
    local k = key_str(id)
    local bucket = read_bucket(KEY_SEEN_IDS)
    bucket[k] = os.time()
    write_bucket(KEY_SEEN_IDS, bucket)
end

-- ────────────────────────────────────────────────────────────────────
-- TTL prune. Triggered on `window-config-reloaded` and at end
-- of every dispatch (after seen_ids write-back, never before).
-- ────────────────────────────────────────────────────────────────────

function M.prune_seen_ids(ttl_seconds)
    local cutoff = os.time() - (ttl_seconds or 60)
    local bucket = read_bucket(KEY_SEEN_IDS)
    local changed = false
    for k, ts in pairs(bucket) do
        if type(ts) ~= "number" or ts < cutoff then
            bucket[k] = nil
            changed = true
        end
    end
    if changed then write_bucket(KEY_SEEN_IDS, bucket) end
end

function M.prune_states(now, ttl_seconds)
    local cutoff = (now or os.time()) - (ttl_seconds or 60)
    local bucket = read_bucket(KEY_STATE)
    local changed = false
    for k, enc in pairs(bucket) do
        local entry = decode_struct(enc)
        if entry == nil
            or type(entry.spawned_at) ~= "number"
            or entry.spawned_at < cutoff
        then
            bucket[k] = nil
            changed = true
        end
    end
    if changed then write_bucket(KEY_STATE, bucket) end
end

function M.prune_requests(now, ttl_seconds)
    local cutoff = (now or os.time()) - (ttl_seconds or 60)
    local bucket = read_bucket(KEY_REQUESTS)
    local changed = false
    for k, enc in pairs(bucket) do
        local entry = decode_struct(enc)
        if entry == nil
            or type(entry.started_at) ~= "number"
            or entry.started_at < cutoff
        then
            bucket[k] = nil
            changed = true
        end
    end
    if changed then write_bucket(KEY_REQUESTS, bucket) end
end

-- ────────────────────────────────────────────────────────────────────
-- wipe_all: reset every bucket this module owns. Used by
-- `wezsesh reset` callbacks and by config-reload teardown.
-- ────────────────────────────────────────────────────────────────────

function M.wipe_all()
    write_bucket(KEY_STATE,    {})
    write_bucket(KEY_SEEN_IDS, {})
    write_bucket(KEY_REQUESTS, {})
    write_bucket(KEY_WRITING,  {})
end

return M
