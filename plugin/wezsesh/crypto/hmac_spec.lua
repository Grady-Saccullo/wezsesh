-- Busted-style spec for hmac.lua. Self-contained — runs under plain
-- `lua plugin/wezsesh/hmac_spec.lua` from the repo root, no busted
-- required.
--
-- Run:
--     cd <repo-root>
--     lua plugin/wezsesh/hmac_spec.lua
--
-- Exits 0 with `OK N/N` on success, 1 with FAIL lines on stderr
-- otherwise.

-- Make hmac.lua, canonical_json.lua, and vendor/sha2.lua loadable
-- regardless of CWD: derive this spec's directory from arg[0] and
-- prepend it.
local function script_dir()
    local src = arg and arg[0] or "plugin/wezsesh/crypto/hmac_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
-- Three roots so this spec runs both bare and under wezterm's
-- `wezterm.plugin.require` (which adds `<plugin_root>/plugin/?.lua`):
--
--   * SPEC_DIR     — `plugin/wezsesh/crypto/?.lua`. Lets bare
--                    `require("hmac")` find this directory's hmac.lua.
--   * SPEC_DIR/..  — `plugin/wezsesh/?.lua`. Lets bare
--                    `require("canonical_json")` find the sibling
--                    plugin/wezsesh/canonical_json.lua.
--   * SPEC_DIR/../.. — `plugin/?.lua`. Lets the dotted form
--                    `require("wezsesh.vendor.sha2")` (used inside
--                    hmac.lua) resolve to plugin/wezsesh/vendor/sha2.lua.
local function parent_dir(p)
    return p:match("^(.*)/[^/]+$") or "."
end
local SPEC_DIR = script_dir()
local PARENT_DIR = parent_dir(SPEC_DIR)
local GRANDPARENT_DIR = parent_dir(PARENT_DIR)
package.path = SPEC_DIR .. "/?.lua;"
            .. SPEC_DIR .. "/?/init.lua;"
            .. PARENT_DIR .. "/?.lua;"
            .. PARENT_DIR .. "/?/init.lua;"
            .. GRANDPARENT_DIR .. "/?.lua;"
            .. GRANDPARENT_DIR .. "/?/init.lua;"
            .. package.path

-- canonical_json's verb_args_shape sources from wezsesh.verbs at module
-- load. The verb modules indirectly require wezterm (via result.lua and
-- runtime/globals.lua); the minimal-wezterm shim is enough to satisfy
-- the load — none of those module bodies actually call into wezterm at
-- load time.
local helpers = require("spec_helpers")
package.preload["wezterm"] = function() return helpers.minimal_wezterm() end

local hm = require("hmac")
local cj = require("canonical_json")

-- ────────────────────────────────────────────────────────────────────
-- minimal busted-shaped harness (mirrors canonical_json_spec.lua)
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

local function assert_false(cond, msg)
    if cond then error(msg or "expected falsy", 2) end
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

-- ────────────────────────────────────────────────────────────────────
-- §17.2 fixture (mirrors internal/hmac/testdata/roundtrip.json)
-- ────────────────────────────────────────────────────────────────────

local FIXTURE_KEY_HEX =
    "a0b1c2d3e4f5a0b1c2d3e4f5a0b1c2d3e4f5a0b1c2d3e4f5a0b1c2d3e4f5a0b1"

local FIXTURE_CANONICAL_SANS_HMAC =
    '{"args":{},"id":"01JABCDEFGHJKMNPQRSTVWXYZA","op":"noop",'
    .. '"reply_sock":"/tmp/x.sock","target_window_id":1,'
    .. '"ts":1700000000,"v":1}'

local FIXTURE_EXPECTED_HMAC =
    "52d0003484acc868ce5762d065e2360f98b37b777009306b3cec8e7177dd14b5"

-- The §17.2 payload as a Lua table, identical in shape to the Go
-- fixture's `payload` map. `args` is the parser-style empty table; the
-- canonical_json verb tagger upgrades it to an object below.
local function fixture_payload()
    return {
        v                = 1,
        id               = "01JABCDEFGHJKMNPQRSTVWXYZA",
        ts               = 1700000000,
        target_window_id = 1,
        reply_sock       = "/tmp/x.sock",
        op               = "noop",
        args             = {},
    }
end

-- ────────────────────────────────────────────────────────────────────
-- tests
-- ────────────────────────────────────────────────────────────────────

describe("§17.2 round-trip fixture", function()
    it("compute over the pinned canonical sans-hmac bytes "
        .. "= expected_hmac", function()
        local got = hm.compute(FIXTURE_CANONICAL_SANS_HMAC, FIXTURE_KEY_HEX)
        assert_eq(got, FIXTURE_EXPECTED_HMAC,
            "compute(sans_hmac_bytes, key) digest mismatch")
    end)

    it("end-to-end: tag → copy_without(hmac) → encode → compute "
        .. "= expected_hmac", function()
        -- Build the payload exactly as ipc.lua step (e) would: the
        -- on-the-wire payload carries `hmac` (any string — the tagger
        -- requires the key be present per ROOT_PAYLOAD_SHAPE), tag it,
        -- then drop the hmac key for the sans-hmac re-encode.
        local payload = fixture_payload()
        payload.hmac = "00"  -- placeholder; removed before encode
        cj.tag_in_place(payload, cj.ROOT_PAYLOAD_SHAPE,
            cj.verb_args_shape.noop)
        local sans = cj.copy_without(payload, "hmac")
        local sans_bytes = cj.encode(sans)
        assert_eq(sans_bytes, FIXTURE_CANONICAL_SANS_HMAC,
            "encoded sans-hmac bytes diverge from §17.2 fixture")
        local got = hm.compute(sans_bytes, FIXTURE_KEY_HEX)
        assert_eq(got, FIXTURE_EXPECTED_HMAC,
            "round-trip digest diverges from §17.2 fixture")
    end)

    it("verify accepts the freshly-signed payload (Lua sign → "
        .. "Lua verify)", function()
        local digest = hm.compute(FIXTURE_CANONICAL_SANS_HMAC,
            FIXTURE_KEY_HEX)
        assert_true(hm.verify(FIXTURE_CANONICAL_SANS_HMAC,
            FIXTURE_KEY_HEX, digest),
            "verify(sans_hmac_bytes, key, digest) returned false")
    end)

    it("verify accepts the pinned expected_hmac (Go sign → Lua "
        .. "verify, modeled by the pinned literal)", function()
        assert_true(hm.verify(FIXTURE_CANONICAL_SANS_HMAC,
            FIXTURE_KEY_HEX, FIXTURE_EXPECTED_HMAC),
            "verify against pinned expected_hmac returned false")
    end)
end)

describe("§4.3 field-removal sequence (no hmac=\"\" set-then-encode)",
function()
    it("the canonical sans-hmac bytes contain no \"hmac\":"
        .. " fragment", function()
        local payload = fixture_payload()
        payload.hmac = "00"  -- placeholder; removed before encode
        cj.tag_in_place(payload, cj.ROOT_PAYLOAD_SHAPE,
            cj.verb_args_shape.noop)
        local sans = cj.copy_without(payload, "hmac")
        local sans_bytes = cj.encode(sans)
        assert_true(sans_bytes:find('"hmac":', 1, true) == nil,
            "sans-hmac bytes leak hmac key: " .. sans_bytes)
    end)

    it("removing hmac vs zeroing hmac produce DIFFERENT digests "
        .. "(forbidden alternative would silently match)", function()
        -- Correct: payload tagged with a placeholder hmac so the
        -- shape-checker accepts it, then key dropped before encode.
        local right_payload = fixture_payload()
        right_payload.hmac = "00"
        cj.tag_in_place(right_payload, cj.ROOT_PAYLOAD_SHAPE,
            cj.verb_args_shape.noop)
        local right_sans = cj.copy_without(right_payload, "hmac")
        local right_bytes = cj.encode(right_sans)
        local right_digest = hm.compute(right_bytes, FIXTURE_KEY_HEX)

        -- Forbidden: payload constructed with hmac = "" and encoded.
        -- Used here as the negative control — its bytes contain
        -- `"hmac":""` and the digest MUST diverge from the §17.2
        -- expected. If these matched, the §4.3 invariant would be
        -- silently violable.
        local wrong_payload = fixture_payload()
        wrong_payload.hmac = ""
        cj.tag_in_place(wrong_payload, cj.ROOT_PAYLOAD_SHAPE,
            cj.verb_args_shape.noop)
        local wrong_bytes = cj.encode(wrong_payload)
        assert_true(wrong_bytes:find('"hmac":""', 1, true) ~= nil,
            "negative control: empty hmac was not encoded as expected")
        local wrong_digest = hm.compute(wrong_bytes, FIXTURE_KEY_HEX)

        assert_true(right_digest ~= wrong_digest,
            "removal vs zeroing collided: both produced " .. right_digest)
        assert_eq(right_digest, FIXTURE_EXPECTED_HMAC,
            "removal-form digest diverges from §17.2 expected")
    end)
end)

describe("verify shape negatives", function()
    it("returns false on tampered payload bytes (single-byte edit)",
    function()
        -- Flip "noop" → "swop" inside the canonical bytes.
        local tampered = FIXTURE_CANONICAL_SANS_HMAC
            :gsub('"op":"noop"', '"op":"swop"')
        assert_false(hm.verify(tampered, FIXTURE_KEY_HEX,
            FIXTURE_EXPECTED_HMAC),
            "verify accepted tampered payload bytes")
    end)

    it("returns false on missing/short/non-hex/uppercase digest",
    function()
        assert_false(hm.verify(FIXTURE_CANONICAL_SANS_HMAC,
            FIXTURE_KEY_HEX, ""), "empty digest accepted")
        assert_false(hm.verify(FIXTURE_CANONICAL_SANS_HMAC,
            FIXTURE_KEY_HEX, "deadbeef"), "short digest accepted")
        assert_false(hm.verify(FIXTURE_CANONICAL_SANS_HMAC,
            FIXTURE_KEY_HEX, "zz" .. string.rep("0", 62)),
            "non-hex digest accepted")
        assert_false(hm.verify(FIXTURE_CANONICAL_SANS_HMAC,
            FIXTURE_KEY_HEX, FIXTURE_EXPECTED_HMAC:upper()),
            "uppercase-hex digest accepted")
        assert_false(hm.verify(FIXTURE_CANONICAL_SANS_HMAC,
            FIXTURE_KEY_HEX, nil), "nil digest accepted")
    end)

    it("returns false on non-string payload_bytes", function()
        assert_false(hm.verify(nil, FIXTURE_KEY_HEX, FIXTURE_EXPECTED_HMAC),
            "nil payload_bytes accepted")
        assert_false(hm.verify(42, FIXTURE_KEY_HEX, FIXTURE_EXPECTED_HMAC),
            "number payload_bytes accepted")
    end)

    it("returns false on bad key shape (caught compute raise)",
    function()
        assert_false(hm.verify(FIXTURE_CANONICAL_SANS_HMAC,
            "not-a-hex-key", FIXTURE_EXPECTED_HMAC),
            "non-hex key accepted")
        assert_false(hm.verify(FIXTURE_CANONICAL_SANS_HMAC,
            string.rep("a", 63), FIXTURE_EXPECTED_HMAC),
            "short key accepted")
    end)
end)

describe("compute key-shape enforcement (§5.1)", function()
    it("rejects non-string key", function()
        assert_raises(function()
            hm.compute(FIXTURE_CANONICAL_SANS_HMAC, nil)
        end, "HMAC_BAD_KEY")
        assert_raises(function()
            hm.compute(FIXTURE_CANONICAL_SANS_HMAC, 42)
        end, "HMAC_BAD_KEY")
    end)

    it("rejects wrong-length key", function()
        assert_raises(function()
            hm.compute(FIXTURE_CANONICAL_SANS_HMAC, "")
        end, "HMAC_BAD_KEY")
        assert_raises(function()
            hm.compute(FIXTURE_CANONICAL_SANS_HMAC, string.rep("a", 63))
        end, "HMAC_BAD_KEY")
        assert_raises(function()
            hm.compute(FIXTURE_CANONICAL_SANS_HMAC, string.rep("a", 65))
        end, "HMAC_BAD_KEY")
    end)

    it("rejects uppercase hex (lowercase-only per §5.1)", function()
        assert_raises(function()
            hm.compute(FIXTURE_CANONICAL_SANS_HMAC,
                FIXTURE_KEY_HEX:upper())
        end, "HMAC_BAD_KEY")
    end)

    it("rejects non-hex characters in key", function()
        assert_raises(function()
            hm.compute(FIXTURE_CANONICAL_SANS_HMAC, string.rep("g", 64))
        end, "HMAC_BAD_KEY")
    end)

    it("rejects non-string payload_bytes", function()
        assert_raises(function()
            hm.compute(nil, FIXTURE_KEY_HEX)
        end, "HMAC_BAD_INPUT")
        assert_raises(function()
            hm.compute(42, FIXTURE_KEY_HEX)
        end, "HMAC_BAD_INPUT")
    end)
end)

describe("hex-decoded key length (§5.1: 32 raw bytes)", function()
    it("compute produces a 64-char lowercase-hex digest", function()
        local d = hm.compute("", FIXTURE_KEY_HEX)
        assert_eq(#d, 64, "digest length")
        assert_true(d:find("[^0-9a-f]") == nil,
            "digest contains non-lowercase-hex char: " .. d)
    end)
end)

describe("§16.3 require-name resolves under wezterm.plugin.require "
    .. "search root", function()
    -- Regression pin. wezterm.plugin.require adds
    -- `<plugin_root>/plugin/?.lua` to package.path. With that single
    -- entry, `require("wezsesh.vendor.sha2")` MUST resolve to
    -- `<plugin_root>/plugin/wezsesh/vendor/sha2.lua`. If a future change
    -- reverts hmac.lua to `require("vendor.sha2")`, the spec would
    -- still pass under one of the local fallbacks, but the production
    -- wezterm plugin load would BREAK with "module 'vendor.sha2' not
    -- found". This test exercises ONLY the production-equivalent
    -- search root, isolating the require-name contract.
    it("package.searchpath('wezsesh.vendor.sha2', '<plugin>/?.lua') "
        .. "resolves to plugin/wezsesh/vendor/sha2.lua", function()
        local production_search = GRANDPARENT_DIR .. "/?.lua"
        local found = package.searchpath("wezsesh.vendor.sha2",
            production_search)
        assert_true(found ~= nil,
            "package.searchpath returned nil for wezsesh.vendor.sha2 "
            .. "under " .. production_search)
        -- Suffix check is invariant to whether the spec is invoked with
        -- an absolute or relative arg[0]. Both
        -- `plugin/wezsesh/vendor/sha2.lua` (relative) and
        -- `/abs/path/plugin/wezsesh/vendor/sha2.lua` (absolute) match.
        local want_suffix = "wezsesh/vendor/sha2.lua"
        assert_true(found:sub(-#want_suffix) == want_suffix,
            "expected suffix " .. want_suffix
            .. ", got " .. tostring(found))
    end)

    it("package.searchpath('vendor.sha2', '<plugin>/?.lua') does NOT "
        .. "resolve (proves the rename is load-bearing)", function()
        -- Negative control: under the production search root, the old
        -- `vendor.sha2` name MUST NOT resolve. If it did, reverting
        -- hmac.lua's require would silently still work in production
        -- and the rename would be cosmetic.
        local production_search = GRANDPARENT_DIR .. "/?.lua"
        local found = package.searchpath("vendor.sha2",
            production_search)
        assert_true(found == nil,
            "expected nil for vendor.sha2 under production search root, "
            .. "got " .. tostring(found))
    end)

    it("hmac.lua loaded successfully under the renamed require "
        .. "(smoke check on the top-of-file require)", function()
        -- The `local hm = require("hmac")` at the top of this file
        -- already exercises the rename indirectly: if
        -- `require("wezsesh.vendor.sha2")` had failed, `hm` would not
        -- be a table. This is a redundant explicit assertion to catch
        -- silent-fallback regressions where someone re-introduces a
        -- module-local stub.
        assert_true(type(hm) == "table",
            "hmac module did not load: " .. type(hm))
        assert_true(type(hm.compute) == "function",
            "hmac.compute is not a function")
        assert_true(type(hm.verify) == "function",
            "hmac.verify is not a function")
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
