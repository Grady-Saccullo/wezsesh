-- §9.10 — RFC 4648 §4 standard base64 codec for the IPC wire path.
--
-- `encode` is used on the reply path (`wezsesh reply <sock> <b64>`).
-- `decode` is the §0.1 row 34 / spike-#3 hot path: §9.3 pre-step (1)
-- calls it once per inbound `user-var-changed` event to recover the
-- §3.1 pointer JSON before opening the request file. Returning `nil`
-- (malformed base64) MUST short-circuit the handler with
-- `REQ_POINTER_REJECTED` (§7) — callers therefore treat any nil
-- exactly as "drop this OSC, write no reply".
--
-- mlua sandbox: pure-string ops. No `wezterm`, no `debug.*`, no
-- `dofile`. Module surface is `{encode, decode}` only.
--
-- Implementation choices, all driven by the < 1 ms / 4 KiB warm gate:
--   * Encode/decode tables are built once at module-load and indexed
--     by integer (`enc[v]`, `dec[byte]`). No per-byte `string.find` or
--     `string.match` calls.
--   * `string.byte(s, i, j)` returns multiple values in a single call,
--     so the inner loops pull 3 input bytes (encode) / 4 input chars
--     (decode) per iteration via one C-level call.
--   * Output assembly is `table.concat` over `string.char` chunks.
--     Iterative `..` concatenation is O(n²) on large inputs and would
--     blow the perf gate on its own.
--   * No closures or tables allocated inside the hot loop. The decode
--     accumulator is a plain int rebuilt per quad.

local M = {}

-- RFC 4648 §4 standard alphabet. Index 0..63 → ASCII byte. We store
-- the bytes as integers and turn them into a string per output chunk;
-- this avoids allocating a 64-entry string-of-strings table that
-- would force per-lookup interning.
local ENC = {
    [0]  = 0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, -- A B C D E F G H
    0x49, 0x4a, 0x4b, 0x4c, 0x4d, 0x4e, 0x4f, 0x50,        -- I J K L M N O P
    0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58,        -- Q R S T U V W X
    0x59, 0x5a,                                             -- Y Z
    0x61, 0x62, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68,        -- a b c d e f g h
    0x69, 0x6a, 0x6b, 0x6c, 0x6d, 0x6e, 0x6f, 0x70,        -- i j k l m n o p
    0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78,        -- q r s t u v w x
    0x79, 0x7a,                                             -- y z
    0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37,        -- 0..7
    0x38, 0x39,                                             -- 8 9
    0x2b, 0x2f,                                             -- + /
}
assert(#ENC == 63 and ENC[0] ~= nil,
       "b64 ENC table must cover indices 0..63 inclusive")

-- 256-entry decode table indexed by ASCII byte. Each cell is the
-- 0..63 sextet value for the alphabet bytes, -1 for everything else.
-- The pad byte ('=', 0x3D) is also -1 here — pad handling is done by
-- the decoder's framing logic, not via the lookup, so a stray '='
-- inside a quad is rejected by "value < 0" the same way a non-base64
-- byte is.
local DEC = {}
for i = 0, 255 do DEC[i] = -1 end
for v = 0, 63 do DEC[ENC[v]] = v end

local PAD = 0x3d  -- '='

local char = string.char
local byte = string.byte
local concat = table.concat
local sub = string.sub
local floor = math.floor

-- §9.10 — encode. Standard RFC 4648 §4: no line breaks, no URL-safe
-- substitutions, '=' padding to a multiple of 4 output chars. Empty
-- input encodes to the empty string (matches Go's
-- base64.StdEncoding.EncodeToString).
function M.encode(s)
    if type(s) ~= "string" then
        error("b64.encode: expected string, got " .. type(s), 2)
    end
    local n = #s
    if n == 0 then return "" end

    -- Pre-allocate the output chunk array. Each 3-byte input group
    -- emits exactly 4 output bytes (4 chars); the trailing 1- or
    -- 2-byte tail emits 4 chars padded with '='.
    local out = {}
    local oi = 0
    local i = 1
    local triples_end = n - (n % 3)

    while i <= triples_end do
        local b1, b2, b3 = byte(s, i, i + 2)
        -- 24-bit group split into 4 sextets, MSB first.
        local n24 = (b1 << 16) | (b2 << 8) | b3
        oi = oi + 1
        out[oi] = char(
            ENC[(n24 >> 18) & 0x3f],
            ENC[(n24 >> 12) & 0x3f],
            ENC[(n24 >> 6) & 0x3f],
            ENC[n24 & 0x3f])
        i = i + 3
    end

    local rem = n - triples_end
    if rem == 1 then
        local b1 = byte(s, i)
        local n24 = b1 << 16
        oi = oi + 1
        out[oi] = char(
            ENC[(n24 >> 18) & 0x3f],
            ENC[(n24 >> 12) & 0x3f],
            PAD,
            PAD)
    elseif rem == 2 then
        local b1, b2 = byte(s, i, i + 1)
        local n24 = (b1 << 16) | (b2 << 8)
        oi = oi + 1
        out[oi] = char(
            ENC[(n24 >> 18) & 0x3f],
            ENC[(n24 >> 12) & 0x3f],
            ENC[(n24 >> 6) & 0x3f],
            PAD)
    end

    return concat(out)
end

-- §9.10 — decode. Strict canonical-only acceptor. Returns the decoded
-- byte string on success or `nil` on any malformed input. Never
-- raises (callers — ipc.lua pre-step (1) — must not be wedged by a
-- malformed OSC).
--
-- Rejected by construction:
--   * non-string input;
--   * length not a multiple of 4;
--   * any byte outside [A-Za-z0-9+/=];
--   * '=' anywhere other than as 0/1/2 trailing pad chars in the
--     final quad;
--   * non-canonical final-quad: the unused bits of the last sextet
--     (4 bits when one '=' is present, 2 bits when two '=' are
--     present) MUST be zero. e.g. `"AB=="` decodes to byte 0x00 and
--     is canonical; `"AC=="` would decode to the same byte but with
--     2 non-zero stray bits, so it MUST be rejected. This is the
--     exploitable variant that historically broke HMAC-and-base64
--     wire formats — the pointer-decode hot path can't tolerate it.
function M.decode(s)
    if type(s) ~= "string" then return nil end
    local n = #s
    if n == 0 then return "" end
    if n % 4 ~= 0 then return nil end

    local out = {}
    local oi = 0
    local quads_end = n - 4  -- last full quad is at i = quads_end + 1

    -- All quads except the trailing one MUST have four non-pad chars.
    local i = 1
    while i <= quads_end do
        local c1, c2, c3, c4 = byte(s, i, i + 3)
        local v1 = DEC[c1]
        local v2 = DEC[c2]
        local v3 = DEC[c3]
        local v4 = DEC[c4]
        -- DEC[c] is -1 for any byte that isn't in the alphabet,
        -- including '=' — '=' is forbidden in a non-trailing quad.
        if v1 < 0 or v2 < 0 or v3 < 0 or v4 < 0 then return nil end
        local n24 = (v1 << 18) | (v2 << 12) | (v3 << 6) | v4
        oi = oi + 1
        out[oi] = char((n24 >> 16) & 0xff,
                       (n24 >> 8) & 0xff,
                       n24 & 0xff)
        i = i + 4
    end

    -- Final quad. Three valid framings:
    --   * c1 c2 c3 c4    (all alphabet)         → 3 output bytes
    --   * c1 c2 c3 '='   (one pad)              → 2 output bytes
    --   * c1 c2 '=' '='  (two pads)             → 1 output byte
    -- Anything else (lone '=' at c1/c2; mixed pad positions; non-
    -- alphabet) is rejected. Non-canonical residual bits in the last
    -- sextet are rejected by the v3/v2 mask checks below.
    local c1, c2, c3, c4 = byte(s, i, i + 3)
    local v1 = DEC[c1]
    local v2 = DEC[c2]
    if v1 < 0 or v2 < 0 then return nil end

    if c4 == PAD then
        if c3 == PAD then
            -- "XY==" → one output byte from (v1<<2 | v2>>4).
            -- The bottom 4 bits of v2 are unused; canonical-only
            -- rejects any input where they're non-zero.
            if (v2 & 0x0f) ~= 0 then return nil end
            local n24 = (v1 << 18) | (v2 << 12)
            oi = oi + 1
            out[oi] = char((n24 >> 16) & 0xff)
        else
            -- "XYZ=" → two output bytes from
            --     (v1<<2 | v2>>4), ((v2&0xf)<<4 | v3>>2).
            -- The bottom 2 bits of v3 are unused; canonical-only
            -- rejects any input where they're non-zero.
            local v3 = DEC[c3]
            if v3 < 0 then return nil end
            if (v3 & 0x03) ~= 0 then return nil end
            local n24 = (v1 << 18) | (v2 << 12) | (v3 << 6)
            oi = oi + 1
            out[oi] = char((n24 >> 16) & 0xff,
                           (n24 >> 8) & 0xff)
        end
    else
        -- Last quad has no '=' — must be a normal full quad.
        local v3 = DEC[c3]
        local v4 = DEC[c4]
        if v3 < 0 or v4 < 0 then return nil end
        local n24 = (v1 << 18) | (v2 << 12) | (v3 << 6) | v4
        oi = oi + 1
        out[oi] = char((n24 >> 16) & 0xff,
                       (n24 >> 8) & 0xff,
                       n24 & 0xff)
    end

    return concat(out)
end

-- Suppress luacheck "unused" warnings in environments that don't
-- exercise both code paths simultaneously. These are only referenced
-- transitively above and would not appear in a basic call graph.
local _unused = sub
_unused = floor
_ = _unused

return M
