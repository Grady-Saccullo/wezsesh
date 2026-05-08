-- Canonical-JSON encoder for the Lua side of the IPC wire protocol.
-- Produces byte-identical output to the Go encoder
-- (internal/canonicaljson/encoder.go) for every shape in the golden
-- corpus. Pure Lua; no wezterm calls, no globals, no I/O.
--
-- Side-effect free except for the verb-aware tagger, which mutates
-- its input in place — but only by setting metatables / replacing
-- tables of size 0 with the NULL sentinel where the shape mandates
-- `string_or_null` and the value is the parser's empty table.
--
-- Errors raised via error(msg, 0) so the message has no file:line
-- prefix; callers (ipc.lua step (e)) substring-match the sentinel
-- prefix (ENCODER_*, CANONICAL_SHAPE_MISMATCH).

local M = {}

M.array_mt  = { __wezsesh_canonical = "array"  }
M.object_mt = { __wezsesh_canonical = "object" }
M.NULL      = setmetatable({}, { __wezsesh_canonical = "null" })

function M.array(t)  return setmetatable(t or {}, M.array_mt)  end
function M.object(t) return setmetatable(t or {}, M.object_mt) end

-- Verb args shape declarations. Source of truth for each shape is the
-- per-verb module (plugin/wezsesh/verbs/X.lua); we populate this field
-- at module load time by asking the verbs registry. Wrapped in pcall
-- so spec environments that don't have the verbs module on
-- package.path degrade to an empty shape table — those specs that
-- need shape data add the parent dir to package.path so the require
-- resolves.
do
    local ok, verbs = pcall(require, "wezsesh.verbs")
    if ok and type(verbs) == "table" and type(verbs.shapes) == "function" then
        M.verb_args_shape = verbs.shapes()
    else
        M.verb_args_shape = {}
    end
end

-- Root payload envelope. Used by ipc.lua step (e):
--   tag_in_place(payload, ROOT_PAYLOAD_SHAPE, verb_args_shape[op]).
-- The args subspec is filled in dynamically by the caller per op.
--
-- Wire version: every key declared here is REQUIRED by tag_in_place
-- (the walker raises CANONICAL_SHAPE_MISMATCH on a missing key).
-- `binary_session_id` was added at v=2 alongside the wire version
-- bump from 1 to 2; an old binary speaking v=1 cannot satisfy this
-- shape and is rejected loudly by ipc.validate_payload before the
-- tagger ever sees it.
M.ROOT_PAYLOAD_SHAPE = {
    _shape            = "object",
    v                 = "int",
    id                = "string",
    ts                = "int",
    target_window_id  = "int",
    reply_sock        = "string",
    op                = "string",
    hmac              = "string",
    binary_session_id = "string",
    -- args is not declared here; the verifier merges in the verb-keyed
    -- subspec when calling tag_in_place.
}

-- ────────────────────────────────────────────────────────────────────
-- internal helpers
-- ────────────────────────────────────────────────────────────────────

local function fail(code, msg)
    error(code .. ": " .. msg, 0)
end

local function tag_of(t)
    local mt = getmetatable(t)
    if mt == nil then return nil end
    return mt.__wezsesh_canonical
end

-- Lua 5.4's utf8.len returns nil + byte position on invalid input.
local utf8_len = utf8.len

local function validate_utf8(s)
    if utf8_len(s) == nil then
        fail("ENCODER_INVALID_UTF8", "string is not valid UTF-8")
    end
end

local hex_digits = "0123456789abcdef"

local function hex2(b)
    return "\\u00"
        .. hex_digits:sub(((b >> 4) & 0xf) + 1, ((b >> 4) & 0xf) + 1)
        .. hex_digits:sub((b & 0xf) + 1, (b & 0xf) + 1)
end

local function hex4(cp)
    return "\\u"
        .. hex_digits:sub(((cp >> 12) & 0xf) + 1, ((cp >> 12) & 0xf) + 1)
        .. hex_digits:sub(((cp >>  8) & 0xf) + 1, ((cp >>  8) & 0xf) + 1)
        .. hex_digits:sub(((cp >>  4) & 0xf) + 1, ((cp >>  4) & 0xf) + 1)
        .. hex_digits:sub(( cp        & 0xf) + 1, ( cp        & 0xf) + 1)
end

-- string serializer.
local function append_string(out, s)
    if type(s) ~= "string" then
        fail("ENCODER_UNSUPPORTED", "expected string, got " .. type(s))
    end
    validate_utf8(s)

    out[#out + 1] = '"'

    local i, n = 1, #s
    while i <= n do
        local b = s:byte(i)
        -- Fast path for plain ASCII >= 0x20 < 0x7f, not " or \.
        if b >= 0x20 and b < 0x7f and b ~= 0x22 and b ~= 0x5c then
            out[#out + 1] = s:sub(i, i)
            i = i + 1
        elseif b == 0x5c then
            out[#out + 1] = "\\\\"
            i = i + 1
        elseif b == 0x22 then
            out[#out + 1] = "\\\""
            i = i + 1
        elseif b < 0x20 or b == 0x7f then
            -- C0 controls + DEL → \u00xx (lowercase).
            out[#out + 1] = hex2(b)
            i = i + 1
        elseif b >= 0xc2 and b <= 0xdf then
            -- 2-byte UTF-8: U+0080..U+07FF.
            local b2 = s:byte(i + 1)
            local cp = ((b & 0x1f) << 6) | (b2 & 0x3f)
            if cp >= 0x80 and cp <= 0x9f then
                out[#out + 1] = hex2(cp)
            else
                out[#out + 1] = s:sub(i, i + 1)
            end
            i = i + 2
        elseif b >= 0xe0 and b <= 0xef then
            -- 3-byte UTF-8: U+0800..U+FFFF.
            local b2 = s:byte(i + 1)
            local b3 = s:byte(i + 2)
            local cp = ((b & 0x0f) << 12) | ((b2 & 0x3f) << 6) | (b3 & 0x3f)
            if cp == 0x2028 or cp == 0x2029 then
                out[#out + 1] = hex4(cp)
            else
                out[#out + 1] = s:sub(i, i + 2)
            end
            i = i + 3
        elseif b >= 0xf0 and b <= 0xf4 then
            -- 4-byte UTF-8: U+10000..U+10FFFF (no escape rule applies).
            out[#out + 1] = s:sub(i, i + 3)
            i = i + 4
        else
            -- Should be unreachable: utf8.len already validated input.
            fail("ENCODER_INVALID_UTF8", "invalid UTF-8 lead byte at offset " .. i)
        end
    end

    out[#out + 1] = '"'
end

local append_value -- forward decl

-- object serializer.
local function append_object(out, t)
    -- Collect string keys; reject non-string keys.
    local keys = {}
    for k, _ in pairs(t) do
        if type(k) ~= "string" then
            fail("ENCODER_UNSUPPORTED",
                 "object key must be string, got " .. type(k))
        end
        keys[#keys + 1] = k
    end
    -- Lua < on strings is bytewise; matches Go sort.Strings under
    -- LC_ALL=C. The explicit comparator mirrors that bytewise rule.
    table.sort(keys, function(a, b) return a < b end)

    out[#out + 1] = "{"
    for i = 1, #keys do
        if i > 1 then out[#out + 1] = "," end
        local k = keys[i]
        append_string(out, k)
        out[#out + 1] = ":"
        local v = t[k]
        if v == nil then
            -- An untagged-object value can in principle be nil after
            -- tagging quirks; reject to mirror Go's "no untyped nil".
            fail("ENCODER_NIL_ELEMENT",
                 "nil value for object key " .. k)
        end
        append_value(out, v)
    end
    out[#out + 1] = "}"
end

-- array serializer.
local function append_array(out, t)
    local n = #t
    -- Holes in arrays are illegal: Go has no analogous shape and the
    -- wire would silently lose elements. Probe by counting non-nil
    -- entries (pairs) vs #t.
    local count = 0
    for k, _ in pairs(t) do
        if type(k) ~= "number" then
            fail("ENCODER_UNSUPPORTED",
                 "array contains non-integer key: " .. tostring(k))
        end
        count = count + 1
    end
    if count ~= n then
        fail("ENCODER_NIL_ELEMENT",
             "array has holes (#t=" .. n .. ", count=" .. count .. ")")
    end

    out[#out + 1] = "["
    for i = 1, n do
        if i > 1 then out[#out + 1] = "," end
        local v = t[i]
        if v == nil then
            fail("ENCODER_NIL_ELEMENT",
                 "nil at array index " .. i)
        end
        append_value(out, v)
    end
    out[#out + 1] = "]"
end

-- type dispatch.
append_value = function(out, v)
    local tv = type(v)

    if tv == "string" then
        append_string(out, v)
        return
    elseif tv == "boolean" then
        out[#out + 1] = v and "true" or "false"
        return
    elseif tv == "number" then
        if math.type(v) ~= "integer" then
            fail("ENCODER_FLOAT_REJECTED",
                 "float not allowed: " .. tostring(v))
        end
        out[#out + 1] = string.format("%d", v)
        return
    elseif tv == "table" then
        local tag = tag_of(v)
        if tag == "null" then
            -- M.NULL sentinel, or any custom-tagged null table.
            out[#out + 1] = "null"
            return
        elseif tag == "object" then
            append_object(out, v)
            return
        elseif tag == "array" then
            append_array(out, v)
            return
        elseif tag == nil then
            fail("ENCODER_UNTAGGED_TABLE",
                 "table has no __wezsesh_canonical metatag; "
                 .. "use canonical_json.array{...} / .object{...} / .NULL")
        else
            -- Anything other than the three legitimate tags is an
            -- error: only the three legitimate tags are allowed.
            fail("ENCODER_UNTAGGED_TABLE",
                 "unknown __wezsesh_canonical tag: " .. tostring(tag))
        end
    elseif tv == "nil" then
        fail("ENCODER_NIL_ELEMENT", "bare nil; use canonical_json.NULL")
    end

    fail("ENCODER_UNSUPPORTED", "unsupported value type: " .. tv)
end

-- ────────────────────────────────────────────────────────────────────
-- public API
-- ────────────────────────────────────────────────────────────────────

function M.encode(v)
    local out = {}
    append_value(out, v)
    return table.concat(out)
end

-- Verb-aware tagger. Walks t in place applying tags from the
-- shape spec. Container shape mismatches and leaf-type violations
-- raise CANONICAL_SHAPE_MISMATCH. Returns t.
--
-- shape grammar:
--   "string" | "int" | "bool" | "string_or_null"
--   { _shape = "object", <key> = <subspec>, ... }
--   { _shape = "array",  _of = <subspec> }
--
-- For an object subspec, every declared key is recursed into; keys
-- present on t but not in shape are left untouched (they will fail the
-- field-shape validator earlier in step (d) anyway, but tag_in_place
-- is conservative — it only enforces shapes for declared keys).

local function is_array_table(t)
    local mt = getmetatable(t)
    return mt and mt.__wezsesh_canonical == "array"
end

local function is_object_table(t)
    local mt = getmetatable(t)
    return mt and mt.__wezsesh_canonical == "object"
end

local function is_null(v)
    if v == M.NULL then return true end
    if type(v) ~= "table" then return false end
    local mt = getmetatable(v)
    return mt and mt.__wezsesh_canonical == "null" or false
end

local function tag_walk(t, spec)
    if type(spec) == "string" then
        if spec == "string" then
            if type(t) ~= "string" then
                fail("CANONICAL_SHAPE_MISMATCH",
                     "expected string, got " .. type(t))
            end
        elseif spec == "int" then
            if type(t) ~= "number" or math.type(t) ~= "integer" then
                fail("CANONICAL_SHAPE_MISMATCH",
                     "expected int, got " .. type(t))
            end
        elseif spec == "bool" then
            if type(t) ~= "boolean" then
                fail("CANONICAL_SHAPE_MISMATCH",
                     "expected bool, got " .. type(t))
            end
        elseif spec == "string_or_null" then
            if not (type(t) == "string" or is_null(t)) then
                fail("CANONICAL_SHAPE_MISMATCH",
                     "expected string or null, got " .. type(t))
            end
        else
            fail("CANONICAL_SHAPE_MISMATCH",
                 "unknown leaf shape: " .. spec)
        end
        return t
    end

    if type(spec) ~= "table" then
        fail("CANONICAL_SHAPE_MISMATCH",
             "shape spec must be string or table, got " .. type(spec))
    end

    local s = spec._shape
    if s == "object" then
        if type(t) ~= "table" then
            fail("CANONICAL_SHAPE_MISMATCH",
                 "expected object table, got " .. type(t))
        end
        if is_array_table(t) then
            fail("CANONICAL_SHAPE_MISMATCH",
                 "expected object, got array-tagged table")
        end
        if not is_object_table(t) then
            setmetatable(t, M.object_mt)
        end
        for k, sub in pairs(spec) do
            if k ~= "_shape" then
                local v = t[k]
                if v == nil then
                    fail("CANONICAL_SHAPE_MISMATCH",
                         "missing required key: " .. tostring(k))
                end
                tag_walk(v, sub)
            end
        end
        return t
    elseif s == "array" then
        if type(t) ~= "table" then
            fail("CANONICAL_SHAPE_MISMATCH",
                 "expected array table, got " .. type(t))
        end
        if is_object_table(t) then
            fail("CANONICAL_SHAPE_MISMATCH",
                 "expected array, got object-tagged table")
        end
        if not is_array_table(t) then
            setmetatable(t, M.array_mt)
        end
        local of = spec._of
        if of == nil then
            fail("CANONICAL_SHAPE_MISMATCH",
                 "array spec missing _of")
        end
        for i = 1, #t do
            tag_walk(t[i], of)
        end
        return t
    else
        fail("CANONICAL_SHAPE_MISMATCH",
             "unknown _shape: " .. tostring(s))
    end
end

function M.tag_in_place(t, root_shape, args_shape)
    if type(t) ~= "table" then
        fail("CANONICAL_SHAPE_MISMATCH",
             "tag_in_place root must be a table, got " .. type(t))
    end

    -- The verifier wants to merge args_shape into root_shape's `args`
    -- slot. Build a shallow merged copy of root_shape so we don't
    -- mutate the caller's static table.
    if root_shape == nil then
        fail("CANONICAL_SHAPE_MISMATCH", "root_shape is nil")
    end

    local merged = { _shape = root_shape._shape or "object" }
    for k, sub in pairs(root_shape) do
        if k ~= "_shape" then merged[k] = sub end
    end
    if args_shape ~= nil then
        merged.args = args_shape
    end

    return tag_walk(t, merged)
end

-- Rehydrate `string_or_null` args slots that the JSON parser dropped.
--
-- Why this exists: `wezterm.json_parse` (and the spec_helpers shim)
-- decodes JSON `null` as Lua `nil`, and a Lua table key set to `nil`
-- is indistinguishable from a missing key. Without rehydration, every
-- `string_or_null` slot the binary signed as `null` would (a) trip
-- the tag-walk's "missing required key" raise at step (e), and (b)
-- even if the walk passed, the sans-hmac re-encode would emit bytes
-- *without* the slot, breaking HMAC parity with the binary's pre-sign
-- bytes.
--
-- The fix is narrow: only `string_or_null` slots declared in the verb's
-- args_shape get re-hydrated. Other parser-as-nil values still fall
-- through to the tag-walker's CANONICAL_SHAPE_MISMATCH so genuine
-- shape violations stay loud.
--
-- Mutates `payload.args` in place; returns nothing. Safe to call with
-- a missing/non-table args (e.g. envelope-shape failure caught later).
function M.rehydrate_nullable_args(payload, args_shape)
    if type(payload) ~= "table" then return end
    if type(args_shape) ~= "table" then return end
    local args = payload.args
    if type(args) ~= "table" then return end
    for k, sub in pairs(args_shape) do
        if k ~= "_shape" and sub == "string_or_null" then
            if args[k] == nil then
                args[k] = M.NULL
            end
        end
    end
end

-- Shallow copy minus key k. Preserves metatable so a tagged container
-- stays tagged after the copy. Used by the HMAC verifier to drop the
-- `hmac` field before re-encoding for HMAC compute.
function M.copy_without(t, k)
    if type(t) ~= "table" then
        fail("ENCODER_UNSUPPORTED",
             "copy_without expects a table, got " .. type(t))
    end
    local out = {}
    for kk, v in pairs(t) do
        if kk ~= k then out[kk] = v end
    end
    local mt = getmetatable(t)
    if mt then setmetatable(out, mt) end
    return out
end

return M
