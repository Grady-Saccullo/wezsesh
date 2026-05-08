-- Shared scaffolding for the plugin's *_spec.lua files. The patterns
-- in here were duplicated across half the spec files before this module
-- existed; centralising them here keeps the per-spec setup focused on
-- what's actually unique about that spec (its recording hooks and
-- assertions).
--
-- Specs that load this:
--   * deepcopy           — every spec that snapshots tables across
--                          calls
--   * make_json_codec    — verbs, ipc, fuzz (canonical_json_spec uses
--                          cj's own encoder for byte-equality and is
--                          intentionally NOT a consumer)
--   * make_global_proxy  — init, ipc, fuzz (runtime/state_spec keeps
--                          its own (proxy, control) factory because
--                          it needs test-only escape hatches the
--                          other three callers don't want)
--   * minimal_wezterm    — canonical_json, crypto/hmac (specs that
--                          only need wezterm.GLOBAL to satisfy a
--                          transitive require)

local M = {}

-- Recursive deep copy. Tables only — leaf values are returned as-is.
function M.deepcopy(v)
    if type(v) ~= "table" then return v end
    local out = {}
    for k, vv in pairs(v) do out[k] = M.deepcopy(vv) end
    return out
end

-- Pure-Lua JSON encoder/decoder honouring the __wezsesh_canonical
-- metatag the production code uses to discriminate empty arrays from
-- empty objects ("array" / "object") and the cj.NULL sentinel
-- ("null"). Lifted from fuzz_spec — the most permissive variant in
-- the tree, a strict superset of what the other consumers need.
function M.make_json_codec()
    local function encode(v)
        local function emit(x)
            local t = type(x)
            if t == "number" then
                if math.type and math.type(x) == "integer" then
                    return tostring(x)
                end
                return string.format("%.17g", x)
            end
            if t == "boolean" then return x and "true" or "false" end
            if t == "string" then
                local out = { '"' }
                for i = 1, #x do
                    local b = x:byte(i)
                    if b == 0x22 then out[#out + 1] = '\\"'
                    elseif b == 0x5c then out[#out + 1] = "\\\\"
                    elseif b < 0x20 then
                        out[#out + 1] = string.format("\\u%04x", b)
                    else out[#out + 1] = string.char(b)
                    end
                end
                out[#out + 1] = '"'
                return table.concat(out)
            end
            if t == "table" then
                local mt = getmetatable(x)
                local kind = mt and mt.__wezsesh_canonical
                if kind == "null" then return "null" end
                if kind == "array" then
                    local parts = {}
                    for i = 1, #x do parts[i] = emit(x[i]) end
                    return "[" .. table.concat(parts, ",") .. "]"
                end
                local n = 0
                local is_array = true
                for k in pairs(x) do
                    if type(k) ~= "number" then is_array = false; break end
                    n = n + 1
                end
                if is_array and n > 0 and kind ~= "object" then
                    local parts = {}
                    for i = 1, n do parts[i] = emit(x[i]) end
                    return "[" .. table.concat(parts, ",") .. "]"
                end
                local keys = {}
                for k in pairs(x) do keys[#keys + 1] = k end
                table.sort(keys, function(a, b)
                    return tostring(a) < tostring(b)
                end)
                local parts = {}
                for _, k in ipairs(keys) do
                    parts[#parts + 1] = emit(tostring(k)) .. ":" .. emit(x[k])
                end
                return "{" .. table.concat(parts, ",") .. "}"
            end
            if t == "nil" then return "null" end
            error("json_encode_shim: unsupported type " .. t)
        end
        return emit(v)
    end

    local function decode(s)
        if type(s) ~= "string" then
            error("json_parse_shim: input must be string", 0)
        end
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
                    if esc == "u" then
                        out[#out + 1] = s:sub(pos + 1, pos + 4)
                        pos = pos + 5
                    else
                        out[#out + 1] = esc
                        pos = pos + 1
                    end
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
            if not num_str:find("[.eE]") then
                n = math.tointeger(n) or n
            end
            return n
        end
        local function parse_object()
            pos = pos + 1; skip_ws()
            local out = {}
            if s:sub(pos, pos) == "}" then pos = pos + 1; return out end
            while true do
                skip_ws()
                local k = parse_string()
                skip_ws()
                if s:sub(pos, pos) ~= ":" then err("expected ':'") end
                pos = pos + 1; skip_ws()
                out[k] = parse_value()
                skip_ws()
                local c = s:sub(pos, pos)
                if c == "}" then pos = pos + 1; return out end
                if c ~= "," then err("expected ',' or '}'") end
                pos = pos + 1
            end
        end
        local function parse_array()
            pos = pos + 1; skip_ws()
            local out = {}
            if s:sub(pos, pos) == "]" then pos = pos + 1; return out end
            while true do
                skip_ws()
                out[#out + 1] = parse_value()
                skip_ws()
                local c = s:sub(pos, pos)
                if c == "]" then pos = pos + 1; return out end
                if c ~= "," then err("expected ',' or ']'") end
                pos = pos + 1
            end
        end
        parse_value = function()
            skip_ws()
            local c = s:sub(pos, pos)
            if c == "{" then return parse_object() end
            if c == "[" then return parse_array() end
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

    return { encode = encode, decode = decode }
end

-- Snapshot-on-read GLOBAL proxy. Reads emit a deepcopy so callers
-- can't mutate the underlying store; writes deepcopy on entry. The
-- __pairs metamethod is load-bearing for init.lua's cross-version
-- wipe loop (`for k in pairs(GLOBAL) ...`) — wezterm's real GLOBAL
-- userdata is iterable; this mirror has to match. Non-iterating
-- consumers ignore __pairs for free.
function M.make_global_proxy()
    local store = {}
    return setmetatable({}, {
        __index = function(_, k) return M.deepcopy(store[k]) end,
        __newindex = function(_, k, v) store[k] = M.deepcopy(v) end,
        __pairs = function(_)
            local function iter(_, prev)
                local k, v = next(store, prev)
                if k == nil then return nil end
                return k, M.deepcopy(v)
            end
            return iter, nil, nil
        end,
    })
end

-- Minimal wezterm shim with an empty, no-op GLOBAL. For specs that
-- don't drive wezterm directly but transitively require modules
-- (runtime/globals.lua, result.lua, etc.) that do `require("wezterm")`
-- at module load. The empty __index/__newindex prevents
-- runtime/globals.lua from blowing up when its accessors run during
-- the indirect require chain.
function M.minimal_wezterm()
    return {
        GLOBAL = setmetatable({}, {
            __index    = function() return nil end,
            __newindex = function() end,
        }),
    }
end

-- Self-test: when this file is invoked directly (`lua
-- plugin/spec_helpers.lua`), exercise each helper end-to-end and
-- exit non-zero on the first failure. Cheap smoke test that the
-- module itself isn't broken before specs start consuming it.
if arg and arg[0] and arg[0]:match("spec_helpers%.lua$") then
    local failures = {}
    local function check(name, ok, msg)
        if not ok then failures[#failures + 1] = name .. ": " .. (msg or "fail") end
    end

    do
        local src = { a = 1, b = { c = 2, d = { e = 3 } } }
        local copy = M.deepcopy(src)
        copy.b.d.e = 999
        check("deepcopy/independent", src.b.d.e == 3,
              "mutation leaked into source")
        check("deepcopy/leaf", M.deepcopy(7) == 7, "leaf scalar not preserved")
        check("deepcopy/string", M.deepcopy("x") == "x", "leaf string not preserved")
    end

    do
        local codec = M.make_json_codec()
        check("codec/integer", codec.encode(7) == "7", "integer encode")
        check("codec/string", codec.encode("hi") == '"hi"', "string encode")
        check("codec/bool", codec.encode(true) == "true", "bool encode")
        check("codec/null/nil", codec.encode(nil) == "null", "nil → null")
        local null_sentinel = setmetatable({}, { __wezsesh_canonical = "null" })
        check("codec/null/sentinel", codec.encode(null_sentinel) == "null",
              "metatag null sentinel")
        local arr = setmetatable({}, { __wezsesh_canonical = "array" })
        check("codec/empty-array", codec.encode(arr) == "[]",
              "metatag empty array → []")
        local obj = setmetatable({}, { __wezsesh_canonical = "object" })
        check("codec/empty-object", codec.encode(obj) == "{}",
              "metatag empty object → {}")
        check("codec/sorted-keys",
              codec.encode({ b = 1, a = 2 }) == '{"a":2,"b":1}',
              "object keys must sort")
        check("codec/array",
              codec.encode({ 1, 2, 3 }) == "[1,2,3]", "natural array")
        check("codec/string-escape/dquote",
              codec.encode('a"b') == '"a\\"b"', "dquote escape")
        check("codec/string-escape/control",
              codec.encode("\1") == '"\\u0001"', "control char escape")
        local round = codec.decode(codec.encode({ k = "v", n = 42 }))
        check("codec/roundtrip", round.k == "v" and round.n == 42, "roundtrip")
    end

    do
        local g = M.make_global_proxy()
        g.foo = { count = 1 }
        local read = g.foo
        read.count = 999
        check("proxy/snapshot-on-read", g.foo.count == 1,
              "mutation via read leaked into store")
        local written = { tag = "live" }
        g.bar = written
        written.tag = "mutated"
        check("proxy/snapshot-on-write", g.bar.tag == "live",
              "mutation via write leaked into store")
        local seen = {}
        for k, v in pairs(g) do seen[k] = v end
        check("proxy/__pairs/foo", seen.foo and seen.foo.count == 1,
              "__pairs missed foo")
        check("proxy/__pairs/bar", seen.bar and seen.bar.tag == "live",
              "__pairs missed bar")
    end

    do
        local w = M.minimal_wezterm()
        check("minimal/global-table", type(w.GLOBAL) == "table",
              "GLOBAL not a table")
        check("minimal/index-nil", w.GLOBAL.anything == nil,
              "__index did not return nil")
        w.GLOBAL.x = "y"
        check("minimal/newindex-noop", w.GLOBAL.x == nil,
              "__newindex was not a no-op")
    end

    if #failures == 0 then
        io.write("OK\n")
        os.exit(0)
    end
    for _, f in ipairs(failures) do
        io.stderr:write("FAIL " .. f .. "\n")
    end
    os.exit(1)
end

return M
