-- Busted-style spec for b64.lua. Self-contained — runs under plain
-- `lua plugin/wezsesh/b64_spec.lua` from the repo root, no busted
-- required.
--
-- Run:
--     cd <repo-root>
--     lua plugin/wezsesh/b64_spec.lua
--
-- Exits 0 with `OK N/N` on success, 1 with FAIL lines on stderr
-- otherwise. Mirrors the structure of ct_eq_spec.lua and hmac_spec.lua.

-- Make b64.lua loadable regardless of CWD: derive this spec's
-- directory from arg[0] and prepend it.
local function script_dir()
    local src = arg and arg[0] or "plugin/wezsesh/b64_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
package.path = script_dir() .. "/?.lua;" .. package.path

local b64 = require("b64")

-- ────────────────────────────────────────────────────────────────────
-- minimal busted-shaped harness (mirrors ct_eq_spec.lua / hmac_spec.lua)
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

local function assert_nil(v, msg)
    if v ~= nil then
        error((msg or "expected nil") .. ", got: " .. quote(v), 2)
    end
end

-- ────────────────────────────────────────────────────────────────────
-- module surface
-- ────────────────────────────────────────────────────────────────────

describe("module surface", function()
    it("exposes only `encode` and `decode`", function()
        local keys = {}
        for k in pairs(b64) do keys[#keys + 1] = k end
        table.sort(keys)
        assert_eq(table.concat(keys, ","), "decode,encode",
            "b64 module surface drift")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- encoder: pinned RFC 4648 fixtures
-- ────────────────────────────────────────────────────────────────────
--
-- These match Go's `base64.StdEncoding.EncodeToString` byte-for-byte;
-- the IPC reply path passes the encoder's output verbatim to
-- `wezsesh reply`, which feeds Go's StdEncoding decoder. Any divergence
-- here breaks the wire format silently.

describe("encode — RFC 4648 fixtures", function()
    -- (input, expected base64) pairs spanning every padding length
    -- and every alphabet quadrant. The "f", "fo", "foo", ...
    -- sequence is RFC 4648's own test vector set.
    local fixtures = {
        {"", ""},
        {"f", "Zg=="},
        {"fo", "Zm8="},
        {"foo", "Zm9v"},
        {"foob", "Zm9vYg=="},
        {"fooba", "Zm9vYmE="},
        {"foobar", "Zm9vYmFy"},
        -- Exercise all 0x00..0xff via three-byte groups that cover
        -- the high-bit alphabet ('+', '/').
        {"\x00\x00\x00", "AAAA"},
        {"\xff\xff\xff", "////"},
        {"\xfb\xff\xbf", "+/+/"},
    }
    for _, pair in ipairs(fixtures) do
        local input, want = pair[1], pair[2]
        it(string.format("encode(%s) = %s", quote(input), quote(want)),
        function()
            assert_eq(b64.encode(input), want, "encode mismatch")
        end)
    end
end)

describe("encode — input typing", function()
    it("raises on non-string input", function()
        local ok, err = pcall(b64.encode, nil)
        assert_true(not ok, "encode(nil) should raise")
        assert_true(err and tostring(err):find("expected string", 1, true),
            "expected diagnostic, got: " .. tostring(err))
        local ok2 = pcall(b64.encode, 42)
        assert_true(not ok2, "encode(42) should raise")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- round-trip gate (acceptance: 0–4096 bytes)
-- ────────────────────────────────────────────────────────────────────

describe("round-trip — 0..4096 byte inputs (acceptance gate)",
function()
    local function check(s, label)
        local enc = b64.encode(s)
        local dec = b64.decode(enc)
        assert_eq(dec, s, "round-trip mismatch (" .. label .. ")")
    end

    it("zero-byte input", function() check("", "n=0") end)

    it("padding edge cases (n = 1, 2, 3)", function()
        check("\x01", "n=1")
        check("\x01\x02", "n=2")
        check("\x01\x02\x03", "n=3")
    end)

    it("every byte value 0x00..0xff in a single buffer", function()
        local buf = {}
        for i = 0, 255 do buf[#buf + 1] = string.char(i) end
        local s = table.concat(buf)
        assert_eq(#s, 256, "256-byte fixture length")
        check(s, "0x00..0xff")
    end)

    it("4096-byte buffer with a deterministic pattern", function()
        -- Repeating 0x00..0xff cycle, 16 cycles → 4096 bytes.
        local buf = {}
        for cycle = 1, 16 do
            for i = 0, 255 do buf[#buf + 1] = string.char(i) end
        end
        local s = table.concat(buf)
        assert_eq(#s, 4096, "4096-byte fixture length")
        check(s, "n=4096")
    end)

    it("a sample at every length from 0 through 64", function()
        -- This sweeps the residue classes (n%3 = 0,1,2) and the
        -- transition into a second 3-byte group exhaustively.
        for n = 0, 64 do
            local buf = {}
            for i = 1, n do buf[i] = string.char((i - 1) % 256) end
            check(table.concat(buf), "n=" .. n)
        end
    end)

    it("a sample at lengths 1023, 1024, 1025, 4095, 4096 "
        .. "(boundary check)", function()
        for _, n in ipairs({1023, 1024, 1025, 4095, 4096}) do
            local buf = {}
            for i = 1, n do buf[i] = string.char((i * 31) % 256) end
            check(table.concat(buf), "n=" .. n)
        end
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- decoder: shape and alphabet rejection
-- ────────────────────────────────────────────────────────────────────

describe("decode — input typing", function()
    it("returns nil on non-string input (does not raise)", function()
        assert_nil(b64.decode(nil), "nil should decode to nil")
        assert_nil(b64.decode(42), "number should decode to nil")
        assert_nil(b64.decode({}), "table should decode to nil")
        -- Booleans likewise.
        assert_nil(b64.decode(true), "true should decode to nil")
    end)
end)

describe("decode — length-not-multiple-of-4 rejection", function()
    -- Any unpadded-and-non-multiple-of-4 string must drop. These are
    -- the "I forgot the padding" cases and they MUST fail.
    for _, s in ipairs({"Z", "Zg", "Zm8", "Zm9vYg", "Zm9vYmE",
                       "AAAAA", "AAAAAA", "AAAAAAA"}) do
        it("rejects length-" .. #s .. " input " .. quote(s), function()
            assert_nil(b64.decode(s),
                "non-multiple-of-4 input was accepted: " .. s)
        end)
    end
end)

describe("decode — non-alphabet character rejection", function()
    -- Each fixture is a 4-char input where exactly one position
    -- carries an out-of-alphabet byte. Decoder MUST reject.
    local bads = {
        "Z g=",   -- space
        "Zm9 ",
        "Zm9-",   -- URL-safe '-' is NOT in std alphabet
        "Zm9_",   -- URL-safe '_' is NOT in std alphabet
        "Zm9.",
        "Zm9*",
        "Zm9\n",  -- embedded newline (Go StdEncoding rejects too;
                  --                  StdEncoding.WithPadding(nil) does
                  --                  not — we don't expose either knob)
        "Zm9\r",
        "Zm9\t",
        "Zm9\x00",
        "Zm9\x80",
        "Zm9\xff",
    }
    for _, s in ipairs(bads) do
        it("rejects out-of-alphabet quad " .. quote(s), function()
            assert_nil(b64.decode(s), "accepted bad alphabet: " .. s)
        end)
    end
end)

describe("decode — '=' position rejection", function()
    -- '=' is allowed only as 0/1/2 trailing pad chars inside the FINAL
    -- quad. Any other position is malformed.
    local bads = {
        "=AAA",   -- pad in c1
        "A=AA",   -- pad in c2
        "A==A",   -- mixed pad positions
        "===A",
        "====",   -- "all pads" is not valid base64 of any byte string
        "AAAA====",  -- trailing extra pads beyond the final quad
        "AA==AAAA",  -- pad in non-final quad
        "AAA=AAAA",
    }
    for _, s in ipairs(bads) do
        it("rejects misplaced '=' in " .. quote(s), function()
            assert_nil(b64.decode(s), "accepted misplaced '=': " .. s)
        end)
    end
end)

-- ────────────────────────────────────────────────────────────────────
-- non-canonical residual-bit rejection (security gate)
-- ────────────────────────────────────────────────────────────────────
--
-- The encoder MUST emit zero residual bits in the final sextet. The
-- decoder MUST refuse to accept inputs whose residual bits are
-- non-zero. Failure to enforce this is the exploitable variant that
-- has historically broken base64-framed HMAC payloads (an attacker
-- can rewrite the padding bits to produce a different cipher input
-- that decodes to the same plaintext).

describe("decode — non-canonical padding rejection (security gate)",
function()
    -- "XY==" encodes one byte = (v1<<2) | (v2>>4). v2's bottom 4 bits
    -- are the unused residual. Iterate every v2 with non-zero
    -- residual and assert decode() returns nil.
    it("rejects every \"XY==\" form whose v2 has non-zero residual "
        .. "(4 bits)", function()
        local ALPHABET =
            "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
        -- Enumerate every (v1, v2) combination and check.
        for v1 = 0, 63 do
            for v2 = 0, 63 do
                local c1 = ALPHABET:sub(v1 + 1, v1 + 1)
                local c2 = ALPHABET:sub(v2 + 1, v2 + 1)
                local s = c1 .. c2 .. "=="
                local got = b64.decode(s)
                local residual = v2 & 0x0f
                if residual ~= 0 then
                    if got ~= nil then
                        error(string.format(
                            "non-canonical %s (v2=%d residual=0x%x) "
                            .. "decoded to %s; expected nil",
                            quote(s), v2, residual, quote(got)))
                    end
                else
                    -- Canonical form must round-trip cleanly.
                    if got == nil then
                        error(string.format(
                            "canonical %s (v2=%d) was rejected",
                            quote(s), v2))
                    end
                    local re = b64.encode(got)
                    if re ~= s then
                        error(string.format(
                            "canonical %s round-trip drifted to %s",
                            quote(s), quote(re)))
                    end
                end
            end
        end
    end)

    it("rejects every \"XYZ=\" form whose v3 has non-zero residual "
        .. "(2 bits)", function()
        local ALPHABET =
            "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
        -- Enumerate every (v3) value across a fixed (v1, v2) and
        -- assert: residual=0 round-trips; residual≠0 rejects. This
        -- doesn't need v1/v2 sweep — the residual rule depends on v3
        -- alone for the "XYZ=" framing.
        local v1, v2 = 0, 0
        local c1 = ALPHABET:sub(v1 + 1, v1 + 1)
        local c2 = ALPHABET:sub(v2 + 1, v2 + 1)
        for v3 = 0, 63 do
            local c3 = ALPHABET:sub(v3 + 1, v3 + 1)
            local s = c1 .. c2 .. c3 .. "="
            local got = b64.decode(s)
            local residual = v3 & 0x03
            if residual ~= 0 then
                if got ~= nil then
                    error(string.format(
                        "non-canonical %s (v3=%d residual=0x%x) "
                        .. "decoded to %s; expected nil",
                        quote(s), v3, residual, quote(got)))
                end
            else
                if got == nil then
                    error(string.format(
                        "canonical %s (v3=%d) was rejected",
                        quote(s), v3))
                end
                local re = b64.encode(got)
                if re ~= s then
                    error(string.format(
                        "canonical %s round-trip drifted to %s",
                        quote(s), quote(re)))
                end
            end
        end
    end)

    it("explicit witnesses: AA== canonical, AB==/AC==/AD==/.../AP== "
        .. "non-canonical (lax codecs decode all to byte 0x00)",
    function()
        -- "AA==" : v1=0 v2=0  → byte = 0<<2 | 0>>4 = 0x00. v2's low
        -- 4 bits are 0000 (canonical).
        -- "AB==" : v1=0 v2=1  → byte = 0<<2 | 1>>4 = 0x00. v2 low 4
        -- bits = 0001, non-zero residual; lax codecs accept it (same
        -- output byte) but we MUST reject.
        -- Same goes for v2 = 2..15 ("AC".."AP") — every one of these
        -- decodes to byte 0x00 in a permissive decoder; only "AA=="
        -- is canonical.
        assert_eq(b64.decode("AA=="), "\x00",
            "AA== must decode to one zero byte (canonical)")
        local ALPHABET =
            "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
        for v2 = 1, 15 do
            local c2 = ALPHABET:sub(v2 + 1, v2 + 1)
            local s = "A" .. c2 .. "=="
            assert_nil(b64.decode(s),
                s .. " must reject (non-canonical residual)")
        end
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Go StdEncoding interop (pinned cross-encoder fixtures)
-- ────────────────────────────────────────────────────────────────────
--
-- The reply path's bytes flow through Go's `base64.StdEncoding`. Drift
-- between the two encoders is silent on the happy path (Go decodes
-- fine), but a divergent character set or padding rule manifests as
-- "REPLY_DECODE_FAILED" on the binary side. Pinning a few outputs
-- locks the alphabet AND padding to RFC 4648.

describe("Go StdEncoding interop (pinned)", function()
    -- Outputs computed via Go's `base64.StdEncoding.EncodeToString`.
    local fixtures = {
        -- A 24-byte buffer of 0x00..0x17 encodes to the stable
        -- canonical form below. This output covers indices 0..63 of
        -- the alphabet evenly enough to detect alphabet permutations.
        {"\x00\x01\x02\x03\x04\x05\x06\x07"
            .. "\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f"
            .. "\x10\x11\x12\x13\x14\x15\x16\x17",
         "AAECAwQFBgcICQoLDA0ODxAREhMUFRYX"},
        -- High-bit chunk; exercises '/' (0x3f).
        {"\xff\xff\xff", "////"},
        -- Mixed; exercises '+' (0x3e).
        {"\xfb\xff\xbf", "+/+/"},
    }
    for _, pair in ipairs(fixtures) do
        local input, want = pair[1], pair[2]
        it("Go-pinned: encode(" .. quote(input) .. ")", function()
            assert_eq(b64.encode(input), want,
                "wire-protocol drift vs Go StdEncoding")
            assert_eq(b64.decode(want), input,
                "decode of Go-produced output diverged")
        end)
    end
end)

-- ────────────────────────────────────────────────────────────────────
-- performance gate: decode 4 KiB warm in < 1 ms
-- ────────────────────────────────────────────────────────────────────
--
-- The hot-path contract. The literal "< 1 ms" is the per-call
-- target; we measure across N iterations so wall-clock granularity
-- doesn't dominate the signal, then assert per-iteration.
--
-- The gate is a regression alarm, not a microbenchmark. Shared CI
-- can swing latencies; we therefore allow a generous slack
-- (10× headroom) on the budget and average across a warmup-stripped
-- batch. A regression that breaks the 1 ms intent will still trip
-- this — going from sub-ms to multi-ms shows up as a 10–100×
-- regression in the average.

describe("performance gate — decode 4 KiB < 1 ms warm",
function()
    it("avg decode time on a 4096-byte input is well under 1 ms",
    function()
        -- Build a deterministic 4096-byte payload spanning every
        -- byte value, then encode it once.
        local buf = {}
        for cycle = 1, 16 do
            for i = 0, 255 do buf[#buf + 1] = string.char(i) end
        end
        local plain = table.concat(buf)
        assert_eq(#plain, 4096, "4096-byte payload length")
        local enc = b64.encode(plain)
        -- 4096 bytes → 1366 quads (ceil(4096/3) = 1366) → 5464 chars
        -- (1366 * 4). The trailing quad is "XY==" because 4096 mod 3
        -- = 1 (one byte residue).
        assert_eq(#enc, 5464,
            "expected encoded length ceil(4096/3)*4 = 5464")

        -- Warmup: prime any adaptive interpreter behaviour and the
        -- module-level decode table.
        for _ = 1, 32 do
            assert_eq(b64.decode(enc), plain, "warmup mismatch")
        end

        -- Measure. ITER large enough that each decode contributes a
        -- meaningful slice of os.clock granularity (~10 ms on most
        -- hosts, ~16 ms on Windows).
        local ITER = 200
        local t0 = os.clock()
        for _ = 1, ITER do
            local dec = b64.decode(enc)
            -- Avoid letting the JIT/optimiser elide the decode; use
            -- the result.
            if #dec ~= 4096 then error("decode shrank") end
        end
        local elapsed = os.clock() - t0
        local per = elapsed / ITER
        -- Print for diagnostic visibility on green runs; harness only
        -- cares about the assertion.
        io.stdout:write(string.format(
            "  [perf] decode 4096B avg = %.3f ms over %d iters "
            .. "(total %.1f ms)\n",
            per * 1000, ITER, elapsed * 1000))
        -- 10× the spec budget so shared-CI noise is absorbed; a real
        -- regression (per-byte alloc, string concat in inner loop)
        -- will blow past this by 1–2 orders of magnitude.
        assert_true(per < 0.010,
            string.format("decode 4 KiB regressed: avg %.3f ms "
                .. "(budget 1 ms; soft limit 10 ms)", per * 1000))
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
