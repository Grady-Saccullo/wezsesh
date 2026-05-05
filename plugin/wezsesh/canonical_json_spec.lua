-- Busted-style spec for canonical_json.lua. Self-contained — runs
-- under plain `lua plugin/wezsesh/canonical_json_spec.lua` from the
-- repo root, no busted required.
--
-- Run:
--     cd <repo-root>
--     lua plugin/wezsesh/canonical_json_spec.lua
--
-- Exits 0 with `OK N/N` on success, 1 with FAIL lines on stderr
-- otherwise.

-- Make canonical_json.lua loadable regardless of CWD: derive this
-- spec's directory from arg[0] and prepend it.
local function script_dir()
    local src = arg and arg[0] or "plugin/wezsesh/canonical_json_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
local function parent_dir(p) return p:match("^(.*)/[^/]+$") or "." end
-- Three roots: script_dir for bare requires (canonical_json), parent
-- for dotted requires (wezsesh.verbs, wezsesh.runtime.*), and the
-- /?/init.lua suffix for verbs/init.lua.
package.path = script_dir() .. "/?.lua;"
            .. parent_dir(script_dir()) .. "/?.lua;"
            .. parent_dir(script_dir()) .. "/?/init.lua;"
            .. package.path

-- canonical_json's verb_args_shape now sources from wezsesh.verbs at
-- module load. Loading verbs pulls in result.lua and runtime/globals.lua,
-- which both `require("wezterm")` at the top. The encoder itself is
-- pure-Lua and never calls a wezterm function, so the minimal shim is
-- enough to satisfy the indirect requires.
local helpers = require("spec_helpers")
package.preload["wezterm"] = function() return helpers.minimal_wezterm() end

local cj = require("canonical_json")

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

local function it(name, fn)
    total = total + 1
    local ok, err = pcall(fn)
    if not ok then
        failures = failures + 1
        io.stderr:write(string.format("FAIL [%s] %s\n  %s\n",
            current_describe, name, tostring(err)))
    end
end

local function quote(s)
    if type(s) ~= "string" then return tostring(s) end
    -- Render bytes for diagnostic prints.
    local out = {}
    for i = 1, #s do
        local b = s:byte(i)
        if b >= 0x20 and b < 0x7f then
            out[#out + 1] = string.char(b)
        else
            out[#out + 1] = string.format("\\x%02x", b)
        end
    end
    return '"' .. table.concat(out) .. '"'
end

local function assert_eq(got, want, msg)
    if got ~= want then
        error(string.format("%s\n   got: %s\n  want: %s",
            msg or "values differ", quote(got), quote(want)), 2)
    end
end

local function assert_true(cond, msg)
    if not cond then error(msg or "expected truthy", 2) end
end

local function assert_raises(fn, sentinel)
    local ok, err = pcall(fn)
    if ok then
        error("expected error containing " .. tostring(sentinel)
              .. ", but call succeeded", 2)
    end
    if type(err) ~= "string" or not err:find(sentinel, 1, true) then
        error("expected error containing " .. tostring(sentinel)
              .. ", got: " .. tostring(err), 2)
    end
end

local function read_file(path)
    local f, err = io.open(path, "rb")
    if not f then return nil, err end
    local data = f:read("*a")
    f:close()
    return data
end

-- ────────────────────────────────────────────────────────────────────
-- §17.1 golden-corpus inputs (mirrors goldenInputs in encoder_test.go)
-- ────────────────────────────────────────────────────────────────────
--
-- IMPORTANT: empty Lua tables MUST be tagged with cj.array / cj.object;
-- the encoder errors on untagged tables. Discriminating fixtures
-- (int_min, int_max, explicit_null, boolean_true) carry typed literals
-- — Lua 5.4 stores all integer literals as the integer subtype.

local golden_inputs = {
    -- §17.1 base corpus.
    empty_object   = cj.object{},
    empty_array    = cj.array{},
    empty_string   = "",
    nul_in_string  = "\x00",
    del_byte       = "\x7f",
    ls_ps          = "\xe2\x80\xa8\xe2\x80\xa9", -- U+2028, U+2029
    multibyte_utf8 = "café",
    cjk            = "日本語",
    emoji          = "\xf0\x9f\xa6\x80",          -- 🦀 U+1F980
    nested_3deep   = cj.object{
        a = cj.object{
            b = cj.object{ c = 1 },
        },
    },
    mixed_array    = cj.array{ 1, "x", true, cj.NULL },
    int_min        = math.mininteger,
    int_max        = math.maxinteger,
    int_zero       = 0,
    neg_one        = -1,
    boolean_true   = true,
    explicit_null  = cj.NULL,
    forward_slash  = "a/b",

    -- Per-verb fixtures (§6).
    verb_switch_args     = cj.object{ name = "work" },
    verb_load_args       = cj.object{ name = "work" },
    verb_save_args       = cj.object{
        name = "work", overwrite = false,
        expected_hash = "sha256:dead",
    },
    verb_save_args_first = cj.object{
        name = "work", overwrite = false,
        expected_hash = cj.NULL,
    },
    verb_new_args        = cj.object{
        name = "~/code", cwd = "/home/user/code",
    },
    verb_noop_args       = cj.object{},
}

-- Locate the golden corpus relative to either the repo root (CI/normal
-- run) or the script's own directory (dev convenience: `lua spec.lua`
-- from inside plugin/wezsesh/).
local function locate_golden_dir()
    local candidates = {
        "internal/canonicaljson/testdata/golden",
        script_dir() .. "/../../internal/canonicaljson/testdata/golden",
    }
    for _, c in ipairs(candidates) do
        local f = io.open(c .. "/empty_object.json", "rb")
        if f then f:close(); return c end
    end
    return candidates[1]  -- fall back; tests will fail loudly
end

local golden_dir = locate_golden_dir()

-- Discover fixtures by attempting to read each declared name; fall back
-- to listing via `ls` when available. Pure-Lua dir listing isn't in
-- 5.4's stdlib without LFS. We require io.popen("ls ...") here, which
-- works under plain Lua (this spec is a dev harness, not plugin code,
-- and never runs inside wezterm's mlua sandbox).
local function list_golden_files()
    local p = io.popen("ls " .. golden_dir .. " 2>/dev/null")
    if not p then return nil end
    local names = {}
    for line in p:lines() do
        if line:match("%.json$") then
            names[#names + 1] = line:gsub("%.json$", "")
        end
    end
    p:close()
    return names
end

-- ────────────────────────────────────────────────────────────────────
-- tests
-- ────────────────────────────────────────────────────────────────────

describe("golden corpus (§17.1)", function()
    local fixtures = list_golden_files()

    it("can list fixtures", function()
        assert_true(fixtures and #fixtures > 0,
            "no fixtures found under " .. golden_dir)
    end)

    if fixtures then
        -- Symmetry: every fixture must have an input entry.
        for _, name in ipairs(fixtures) do
            it("fixture " .. name .. " has goldenInputs entry", function()
                assert_true(golden_inputs[name] ~= nil
                    or name == "explicit_null",  -- NULL is a real value
                    "no input for " .. name)
            end)
        end

        -- Symmetry: every input must have a fixture file.
        for name, _ in pairs(golden_inputs) do
            it("input " .. name .. " has matching .json", function()
                local seen = false
                for _, f in ipairs(fixtures) do
                    if f == name then seen = true; break end
                end
                assert_true(seen, "no fixture file for " .. name)
            end)
        end

        for _, name in ipairs(fixtures) do
            it("encodes " .. name .. " byte-equal to golden", function()
                local want, err = read_file(golden_dir .. "/" .. name .. ".json")
                assert_true(want, "read fixture: " .. tostring(err))
                local input = golden_inputs[name]
                assert_true(input ~= nil
                    or name == "explicit_null",
                    "no input declared for " .. name)
                local got = cj.encode(input)
                assert_eq(got, want, "byte mismatch for " .. name)
            end)
        end
    end
end)

describe("verb-aware tagging round-trip (§4.2 / §17.2)", function()
    it("noop: parser-style empty {} re-tags to object and encodes correctly",
    function()
        -- Simulate wezterm.json_parse output: plain (untagged) tables.
        local payload = {
            v                = 1,
            id               = "01JABCDEFGHJKMNPQRSTVWXYZA",
            ts               = 1700000000,
            target_window_id = 1,
            reply_sock       = "/tmp/x.sock",
            op               = "noop",
            hmac             = "deadbeef",  -- present; would be removed
                                            -- before re-encode in real
                                            -- verifier flow, but the
                                            -- canonical-WITH-hmac form
                                            -- is what §17.2 documents.
            args             = {},
        }

        cj.tag_in_place(payload, cj.ROOT_PAYLOAD_SHAPE,
            cj.verb_args_shape.noop)

        local got = cj.encode(payload)
        local want_with_hmac = '{"args":{},"hmac":"deadbeef",'
            .. '"id":"01JABCDEFGHJKMNPQRSTVWXYZA","op":"noop",'
            .. '"reply_sock":"/tmp/x.sock","target_window_id":1,'
            .. '"ts":1700000000,"v":1}'
        assert_eq(got, want_with_hmac,
            "tagged-payload encode (with hmac) wrong")

        -- canonical sans-hmac form (§17.2 normative).
        local sans = cj.copy_without(payload, "hmac")
        local got_sans = cj.encode(sans)
        local want_sans = '{"args":{},'
            .. '"id":"01JABCDEFGHJKMNPQRSTVWXYZA","op":"noop",'
            .. '"reply_sock":"/tmp/x.sock","target_window_id":1,'
            .. '"ts":1700000000,"v":1}'
        assert_eq(got_sans, want_sans,
            "canonical sans-hmac form mismatch (§17.2)")
    end)

    it("save: full envelope tags correctly with verb args", function()
        local payload = {
            v                = 1,
            id               = "01JABCDEFGHJKMNPQRSTVWXYZA",
            ts               = 1700000000,
            target_window_id = 1,
            reply_sock       = "/tmp/x.sock",
            op               = "save",
            hmac             = "deadbeef",
            args = {
                name = "work", overwrite = false,
                expected_hash = "sha256:dead",
            },
        }
        cj.tag_in_place(payload, cj.ROOT_PAYLOAD_SHAPE,
            cj.verb_args_shape.save)
        local got = cj.encode(payload)
        -- args order: expected_hash < name < overwrite (byte order).
        assert_eq(got,
            '{"args":{"expected_hash":"sha256:dead","name":"work","overwrite":false},'
            .. '"hmac":"deadbeef","id":"01JABCDEFGHJKMNPQRSTVWXYZA",'
            .. '"op":"save","reply_sock":"/tmp/x.sock",'
            .. '"target_window_id":1,"ts":1700000000,"v":1}')
    end)

    it("save: NULL expected_hash round-trips", function()
        local payload = {
            v = 1, id = "01JABCDEFGHJKMNPQRSTVWXYZA",
            ts = 1700000000, target_window_id = 1,
            reply_sock = "/tmp/x.sock", op = "save",
            hmac = "deadbeef",
            args = {
                name = "work", overwrite = false,
                expected_hash = cj.NULL,  -- already a sentinel
            },
        }
        cj.tag_in_place(payload, cj.ROOT_PAYLOAD_SHAPE,
            cj.verb_args_shape.save)
        local got = cj.encode(payload)
        assert_true(got:find('"expected_hash":null', 1, true) ~= nil,
            "expected null in output, got: " .. got)
    end)

    it("rejects shape mismatch on leaf (name = 42)", function()
        local payload = {
            v = 1, id = "01JABCDEFGHJKMNPQRSTVWXYZA",
            ts = 1700000000, target_window_id = 1,
            reply_sock = "/tmp/x.sock", op = "switch",
            hmac = "deadbeef",
            args = { name = 42 },  -- wrong type
        }
        assert_raises(function()
            cj.tag_in_place(payload, cj.ROOT_PAYLOAD_SHAPE,
                cj.verb_args_shape.switch)
        end, "CANONICAL_SHAPE_MISMATCH")
    end)

    it("rejects missing required key", function()
        local payload = {
            v = 1, id = "01JABCDEFGHJKMNPQRSTVWXYZA",
            ts = 1700000000, target_window_id = 1,
            reply_sock = "/tmp/x.sock", op = "switch",
            hmac = "deadbeef",
            args = {},  -- missing name
        }
        assert_raises(function()
            cj.tag_in_place(payload, cj.ROOT_PAYLOAD_SHAPE,
                cj.verb_args_shape.switch)
        end, "CANONICAL_SHAPE_MISMATCH")
    end)
end)

describe("untagged table rejection (§4.1 rule 7)", function()
    it("plain {} raises ENCODER_UNTAGGED_TABLE", function()
        assert_raises(function() cj.encode({}) end,
            "ENCODER_UNTAGGED_TABLE")
    end)

    it("nested untagged {} inside tagged object raises", function()
        local t = cj.object{ inner = {} }  -- inner is untagged
        assert_raises(function() cj.encode(t) end,
            "ENCODER_UNTAGGED_TABLE")
    end)

    it("__wezsesh_canonical = 'untagged' is outlawed (§0.1 row 24)",
    function()
        local bad = setmetatable({}, { __wezsesh_canonical = "untagged" })
        assert_raises(function() cj.encode(bad) end,
            "ENCODER_UNTAGGED_TABLE")
    end)

    it("__wezsesh_canonical = 'banana' (any other tag) raises", function()
        local bad = setmetatable({}, { __wezsesh_canonical = "banana" })
        assert_raises(function() cj.encode(bad) end,
            "ENCODER_UNTAGGED_TABLE")
    end)
end)

describe("number rules (§4.1 rule 3)", function()
    it("int_min and int_max round-trip", function()
        assert_eq(cj.encode(math.mininteger), "-9223372036854775808")
        assert_eq(cj.encode(math.maxinteger), "9223372036854775807")
    end)

    it("zero and negative one", function()
        assert_eq(cj.encode(0), "0")
        assert_eq(cj.encode(-1), "-1")
    end)

    it("rejects floats (top-level)", function()
        assert_raises(function() cj.encode(1.5) end,
            "ENCODER_FLOAT_REJECTED")
    end)

    it("rejects floats (1.0 — integer-valued but float subtype)",
    function()
        assert_raises(function() cj.encode(1.0) end,
            "ENCODER_FLOAT_REJECTED")
    end)

    it("rejects nested float in object value", function()
        local t = cj.object{ x = 1.5 }
        assert_raises(function() cj.encode(t) end,
            "ENCODER_FLOAT_REJECTED")
    end)

    it("rejects nested float in array element", function()
        local t = cj.array{ 1, 2.0 }
        assert_raises(function() cj.encode(t) end,
            "ENCODER_FLOAT_REJECTED")
    end)

    it("rejects NaN and Infinity", function()
        assert_raises(function() cj.encode(0/0) end,
            "ENCODER_FLOAT_REJECTED")
        assert_raises(function() cj.encode(math.huge) end,
            "ENCODER_FLOAT_REJECTED")
        assert_raises(function() cj.encode(-math.huge) end,
            "ENCODER_FLOAT_REJECTED")
    end)
end)

describe("string escape rules (§4.1 rule 4)", function()
    it("forward slash NEVER escaped", function()
        assert_eq(cj.encode("/"), '"/"')
        assert_eq(cj.encode("a/b/c"), '"a/b/c"')
    end)

    it("U+2028 / U+2029 escaped (lowercase 4-hex)", function()
        assert_eq(cj.encode("\xe2\x80\xa8"), '"\\u2028"')
        assert_eq(cj.encode("\xe2\x80\xa9"), '"\\u2029"')
    end)

    it("DEL (0x7f) escaped", function()
        assert_eq(cj.encode("\x7f"), '"\\u007f"')
    end)

    it("C1 controls 0x80 / 0x9F escaped (lowercase \\u00xx)", function()
        assert_eq(cj.encode("\xc2\x80"), '"\\u0080"')
        assert_eq(cj.encode("\xc2\x9f"), '"\\u009f"')
    end)

    it("U+00A0 above C1 — raw two-byte UTF-8", function()
        assert_eq(cj.encode("\xc2\xa0"), '"\xc2\xa0"')
    end)

    it("backslash and quote escaped", function()
        assert_eq(cj.encode("\\"), '"\\\\"')
        assert_eq(cj.encode("\""), '"\\\""')
    end)

    it("forbidden short-form chars use \\u00xx", function()
        assert_eq(cj.encode("\x08"), '"\\u0008"')
        assert_eq(cj.encode("\x09"), '"\\u0009"')
        assert_eq(cj.encode("\x0a"), '"\\u000a"')
        assert_eq(cj.encode("\x0b"), '"\\u000b"')
        assert_eq(cj.encode("\x0c"), '"\\u000c"')
        assert_eq(cj.encode("\x0d"), '"\\u000d"')
    end)

    it("NUL escaped", function()
        assert_eq(cj.encode("\x00"), '"\\u0000"')
    end)

    it("0x20 boundary: 0x1F escapes, 0x20 raw", function()
        assert_eq(cj.encode("\x1f "), '"\\u001f "')
    end)

    it("plain ASCII at and just above U+0020 raw", function()
        assert_eq(cj.encode(" "), '" "')
        assert_eq(cj.encode("!"), '"!"')
    end)

    it("mixed control + plain", function()
        assert_eq(cj.encode("a\nb"), '"a\\u000ab"')
    end)
end)

describe("UTF-8 validation (§4.1 rule 4)", function()
    it("rejects bare invalid bytes", function()
        assert_raises(function() cj.encode("\xff\xfe") end,
            "ENCODER_INVALID_UTF8")
    end)

    it("rejects bad continuation", function()
        assert_raises(function() cj.encode("\xc3\x28") end,
            "ENCODER_INVALID_UTF8")
    end)

    it("rejects invalid UTF-8 inside object value", function()
        local t = cj.object{ k = "\xff" }
        assert_raises(function() cj.encode(t) end,
            "ENCODER_INVALID_UTF8")
    end)

    it("rejects invalid UTF-8 in object key", function()
        local t = cj.object{ ["\xff"] = 1 }
        assert_raises(function() cj.encode(t) end,
            "ENCODER_INVALID_UTF8")
    end)

    it("rejects invalid UTF-8 inside array", function()
        local t = cj.array{ "\xff" }
        assert_raises(function() cj.encode(t) end,
            "ENCODER_INVALID_UTF8")
    end)
end)

describe("key ordering (§4.1 rule 1, byte order)", function()
    it("ASCII keys sorted bytewise", function()
        assert_eq(cj.encode(cj.object{ b = 1, a = 2, z = 3 }),
            '{"a":2,"b":1,"z":3}')
    end)

    it("é (0xc3 0xa9) sorts after ASCII 'a'", function()
        local t = cj.object{}
        t["a"] = 2
        t["é"] = 1
        assert_eq(cj.encode(t), '{"a":2,"\xc3\xa9":1}')
    end)

    it("empty-string key sorts first", function()
        local t = cj.object{}
        t["a"] = 1
        t[""]  = 2
        assert_eq(cj.encode(t), '{"":2,"a":1}')
    end)
end)

describe("stability", function()
    it("encoding the same input 16 times yields identical bytes",
    function()
        local v = cj.object{
            z = 3, a = 1, m = 2,
            nested = cj.object{ y = 2, b = 1 },
        }
        local first = cj.encode(v)
        for _ = 1, 16 do
            assert_eq(cj.encode(v), first, "encode unstable")
        end
    end)
end)

describe("copy_without (§9.7)", function()
    it("returns new table without key k, preserving metatable",
    function()
        local t = cj.object{ a = 1, hmac = "deadbeef", b = 2 }
        local copy = cj.copy_without(t, "hmac")
        assert_eq(copy.hmac, nil, "hmac should be absent")
        assert_eq(copy.a, 1)
        assert_eq(copy.b, 2)
        assert_eq(t.hmac, "deadbeef", "original must not be mutated")
        local mt_t, mt_c = getmetatable(t), getmetatable(copy)
        assert_true(mt_c == mt_t,
            "metatable not preserved on copy")
    end)

    it("copy is encodable as object", function()
        local t = cj.object{ a = 1, hmac = "x" }
        local c = cj.copy_without(t, "hmac")
        assert_eq(cj.encode(c), '{"a":1}')
    end)
end)

describe("array constraints", function()
    it("rejects array with hole", function()
        local a = cj.array{ 1, nil, 3 }
        -- This will be rejected for either #a inconsistency or nil
        -- entry — both raise ENCODER_NIL_ELEMENT.
        assert_raises(function() cj.encode(a) end,
            "ENCODER_NIL_ELEMENT")
    end)

    it("encodes mixed_array correctly", function()
        local a = cj.array{ 1, "x", true, cj.NULL }
        assert_eq(cj.encode(a), '[1,"x",true,null]')
    end)
end)

describe("explicit null", function()
    it("M.NULL emits null", function()
        assert_eq(cj.encode(cj.NULL), "null")
    end)

    it("nil at top level raises", function()
        assert_raises(function() cj.encode(nil) end,
            "ENCODER_NIL_ELEMENT")
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
