-- Busted-style spec for ct_eq.lua. Self-contained — runs under plain
-- `lua plugin/wezsesh/ct_eq_spec.lua` from the repo root, no busted
-- required.
--
-- Run:
--     cd <repo-root>
--     lua plugin/wezsesh/ct_eq_spec.lua
--
-- Exits 0 with `OK N/N` on success, 1 with FAIL lines on stderr
-- otherwise.

-- Make ct_eq.lua loadable regardless of CWD: derive this spec's
-- directory from arg[0] and prepend it.
local function script_dir()
    local src = arg and arg[0] or "plugin/wezsesh/ct_eq_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
package.path = script_dir() .. "/?.lua;" .. package.path

local ct = require("ct_eq")

-- ────────────────────────────────────────────────────────────────────
-- minimal busted-shaped harness (mirrors canonical_json_spec.lua /
-- hmac_spec.lua)
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

-- ────────────────────────────────────────────────────────────────────
-- Lua 5.3+ requirement
-- ────────────────────────────────────────────────────────────────────

describe("Lua 5.3+ load-time assertion", function()
    it("the running interpreter satisfies _VERSION >= \"Lua 5.3\"",
    function()
        -- The module already asserted this at require time; if we got
        -- here, the assert held. Re-state the invariant explicitly so
        -- a future regression in the assert wording doesn't quietly
        -- regress the gate.
        assert_true(_VERSION >= "Lua 5.3",
            "running under " .. tostring(_VERSION)
            .. " — ct_eq.lua's bitwise ops require Lua 5.3+")
    end)

    it("module exposes only `eq`; no leak of internal helpers",
    function()
        local keys = {}
        for k in pairs(ct) do keys[#keys + 1] = k end
        table.sort(keys)
        assert_eq(table.concat(keys, ","), "eq",
            "ct_eq module surface drift")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- equality semantics
-- ────────────────────────────────────────────────────────────────────

describe("eq — equality semantics", function()
    it("returns true for two empty strings", function()
        assert_true(ct.eq("", ""), "empty == empty")
    end)

    it("returns true for identical 1-byte strings", function()
        assert_true(ct.eq("a", "a"), "\"a\" == \"a\"")
    end)

    it("returns false for differing 1-byte strings", function()
        assert_false(ct.eq("a", "b"), "\"a\" should not == \"b\"")
    end)

    it("returns true for identical 64-char hex digests "
        .. "(HMAC hot path)", function()
        local d = string.rep("0123456789abcdef", 4)
        assert_true(ct.eq(d, d), "identical 64-char hex mismatch")
    end)

    it("returns false when a single byte at the end differs",
    function()
        local a = string.rep("a", 64)
        local b = string.rep("a", 63) .. "b"
        assert_false(ct.eq(a, b), "trailing-byte mismatch accepted")
    end)

    it("returns false when a single byte at the start differs",
    function()
        local a = string.rep("a", 64)
        local b = "b" .. string.rep("a", 63)
        assert_false(ct.eq(a, b), "leading-byte mismatch accepted")
    end)

    it("returns false when a single byte in the middle differs",
    function()
        local a = string.rep("a", 64)
        local b = string.rep("a", 31) .. "b" .. string.rep("a", 32)
        assert_false(ct.eq(a, b), "middle-byte mismatch accepted")
    end)

    it("returns false on length mismatch (any direction)", function()
        assert_false(ct.eq("", "a"), "empty vs 1-byte")
        assert_false(ct.eq("a", ""), "1-byte vs empty")
        assert_false(ct.eq("aa", "a"), "2 vs 1")
        assert_false(ct.eq(string.rep("a", 64), string.rep("a", 65)),
            "64 vs 65")
    end)

    it("treats binary bytes (NUL, 0xff) byte-exactly", function()
        local a = "\x00\xff\x10\xfe"
        local b = "\x00\xff\x10\xfe"
        assert_true(ct.eq(a, b), "binary bytes equal")
        local c = "\x00\xff\x10\xff"
        assert_false(ct.eq(a, c), "binary bytes one byte off")
    end)

    it("compares all 256 byte values across position 1..256",
    function()
        -- Build a 256-byte string covering every byte value 0..255
        -- in order; compare against itself, against a one-byte-diff
        -- copy at every index.
        local buf = {}
        for i = 0, 255 do buf[#buf + 1] = string.char(i) end
        local s = table.concat(buf)
        assert_eq(#s, 256, "fixture length")
        assert_true(ct.eq(s, s), "256-byte identity")
        for pos = 1, 256 do
            local copy = {}
            for i = 0, 255 do copy[#copy + 1] = string.char(i) end
            -- Flip the high bit at `pos` so the byte is guaranteed
            -- different.
            local orig = string.byte(s, pos)
            copy[pos] = string.char((orig ~ 0x80) & 0xff)
            local mut = table.concat(copy)
            assert_eq(#mut, 256,
                "mutated fixture length at pos " .. pos)
            assert_false(ct.eq(s, mut),
                "single-byte flip at pos " .. pos
                .. " not detected")
        end
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Constant-time / branchless property (acceptance gate)
-- ────────────────────────────────────────────────────────────────────
--
-- Strict wall-clock timing tests are flaky under shared CI hardware,
-- so we verify the structural property the spec actually demands:
-- for two equal-length inputs, `eq` must perform exactly the same
-- number of `string.byte` reads regardless of WHERE — or WHETHER —
-- the bytes diverge. This is the operational definition of "no
-- early-out" / "branchless on input length" up to 256 chars.
--
-- We instrument `string.byte` with a counting wrapper and re-`require`
-- the module under that override so its `a:byte(i)` / `b:byte(i)`
-- calls flow through the counter. The string-method dispatch
-- (`s:byte(i)`) goes through `getmetatable("").__index`, so
-- monkey-patching `string.byte` is sufficient.

describe("constant-time access pattern (acceptance gate: branchless "
    .. "on length up to 256 chars)", function()

    -- Re-load `ct_eq` against an instrumented `string.byte` so we can
    -- count reads. Save and restore the original to avoid bleeding
    -- the wrapper into other test groups.
    local function with_byte_counter(fn)
        local orig_byte = string.byte
        local count = 0
        string.byte = function(...)
            count = count + 1
            return orig_byte(...)
        end
        package.loaded["ct_eq"] = nil
        local mod = require("ct_eq")
        local function reset() count = 0 end
        local function get() return count end
        local ok, err = pcall(fn, mod, reset, get)
        string.byte = orig_byte
        package.loaded["ct_eq"] = nil
        require("ct_eq")  -- restore the canonical module table
        if not ok then error(err, 0) end
    end

    -- For each tested length, compare:
    --   • identical strings           (must do same byte work)
    --   • diff at first byte
    --   • diff at middle byte
    --   • diff at last byte
    -- All four runs MUST produce the same `string.byte` call count.
    -- That is the structural definition of "branchless on length".
    local LENS = {1, 2, 16, 32, 63, 64, 65, 128, 200, 255, 256}

    for _, n in ipairs(LENS) do
        it(string.format(
            "length=%d: byte-read count is identical across "
            .. "match / first-diff / mid-diff / last-diff", n),
        function()
            with_byte_counter(function(mod, reset, get)
                local base = string.rep("a", n)

                local function flip(s, pos)
                    if n == 0 then return s end
                    return s:sub(1, pos - 1) .. "b" .. s:sub(pos + 1)
                end

                reset()
                assert_true(mod.eq(base, base),
                    "match: equal-string compare returned false")
                local c_match = get()

                local first = flip(base, 1)
                reset()
                assert_false(mod.eq(base, first),
                    "first-diff: mismatch missed")
                local c_first = get()

                local mid = flip(base, math.max(1, (n + 1) // 2))
                reset()
                assert_false(mod.eq(base, mid),
                    "mid-diff: mismatch missed")
                local c_mid = get()

                local last = flip(base, n)
                reset()
                assert_false(mod.eq(base, last),
                    "last-diff: mismatch missed")
                local c_last = get()

                -- All four runs MUST inspect the same number of
                -- bytes. Any divergence implies an early-exit
                -- branch was taken (timing leak).
                assert_eq(c_first, c_match,
                    string.format(
                        "n=%d: first-diff inspected %d bytes vs "
                        .. "match's %d (early-exit detected)",
                        n, c_first, c_match))
                assert_eq(c_mid, c_match,
                    string.format(
                        "n=%d: mid-diff inspected %d bytes vs "
                        .. "match's %d (early-exit detected)",
                        n, c_mid, c_match))
                assert_eq(c_last, c_match,
                    string.format(
                        "n=%d: last-diff inspected %d bytes vs "
                        .. "match's %d (early-exit detected)",
                        n, c_last, c_match))

                -- Sanity on the absolute count: equal-length compare
                -- reads exactly 2*n bytes (one per side per index).
                -- The length check `#a ~= #b` uses `#`, not `:byte`,
                -- so it does not contribute to the counter.
                assert_eq(c_match, 2 * n, string.format(
                    "n=%d: expected exactly %d byte reads (2 per "
                    .. "iteration), got %d", n, 2 * n, c_match))
            end)
        end)
    end

    it("differing-length inputs short-circuit BEFORE any byte read "
        .. "(length leak is publicly acceptable)", function()
        with_byte_counter(function(mod, reset, get)
            reset()
            assert_false(mod.eq(string.rep("a", 64),
                                string.rep("a", 65)),
                "differing lengths must compare unequal")
            assert_eq(get(), 0,
                "length-mismatch path read bytes; expected 0")
        end)
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
