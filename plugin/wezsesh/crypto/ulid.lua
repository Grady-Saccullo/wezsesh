-- crypto/ulid.lua — best-effort ULID-shaped 26-char string.
--
-- NOT cryptographic. The plugin needs a stable per-`apply_to_config`
-- correlation id (`plugin_session_id`) shared between Lua log records
-- and the reply envelope; the plan's only constraints are "26 chars,
-- string". A real ULID demands a CSPRNG, which Lua's `math.random` is
-- not — but for log correlation that's not load-bearing. Treat the
-- output here as a pseudo-random debug id, never as a security token.
--
-- Shape: [10 chars timestamp Crockford-base32][16 chars random
-- Crockford-base32]. Same alphabet as the Go ULID encoder
-- (oklog/ulid) so a shared eyeball-grep over `plugin.log` and
-- `wezsesh.log` for `01J…` works across both streams.
--
-- Cache-bust safety: this module is reloaded on Ctrl+Shift+R; that's
-- intended — `apply_to_config` mints a fresh id on every reload, and
-- the reload IS a new logical session.
--
-- mlua sandbox: lives under `crypto/` to satisfy the
-- `lua-crypto-vendor-only` rule. The rule allows `crypto/*.lua` to
-- import `wezsesh.vendor.*` and nothing else; this module imports
-- nothing — it's pure stdlib.

local M = {}

-- Crockford base-32 alphabet (same as oklog/ulid). Lowercase mapped to
-- uppercase by the lookup; ambiguous letters (I, L, O, U) are absent.
local ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

-- Encode `n` as `width` Crockford-base32 characters. `n` is treated as
-- an unsigned integer; bits beyond `width * 5` are silently dropped.
-- Pure integer ops; no allocation beyond the result string concat.
local function encode_int(n, width)
    if n == nil or n < 0 then n = 0 end
    local out = {}
    for i = width, 1, -1 do
        local idx = (n & 0x1f) + 1
        out[i] = ALPHABET:sub(idx, idx)
        n = n >> 5
    end
    return table.concat(out)
end

-- One-time PRNG seed. Lua's `math.random` is process-deterministic
-- without a seed call; we mix in `os.time()`, the address of a fresh
-- table (via `tostring`'s pointer suffix), and `os.clock()` for sub-
-- second jitter. Best-effort: a wezterm GUI-process restart that lands
-- on the exact same `os.time()` second AND the same Lua heap layout
-- AND the same pre-seed `math.random` state would collide. We treat
-- that as not-a-real-failure for a log-correlation id.
local seeded = false
local function ensure_seeded()
    if seeded then return end
    seeded = true
    local addr = tostring({}):match("0x(%x+)") or "0"
    local addr_n = tonumber(addr, 16) or 0
    local clock_n = math.floor((os.clock() or 0) * 1e6)
    math.randomseed(os.time() + addr_n + clock_n)
end

-- Mint a 26-char Crockford-base32 string. The leading 10 chars encode
-- a 48-bit unix-millisecond timestamp (close enough to a real ULID's
-- time component that `01J…`-prefixed ids show up in 2024+); the
-- trailing 16 chars are 80 bits of `math.random` output split across
-- four 20-bit chunks.
function M.mint()
    ensure_seeded()
    local now_ms = math.floor((os.time() or 0) * 1000)
    local time_part = encode_int(now_ms, 10)

    -- 80 bits of randomness across four 20-bit chunks. `math.random()`
    -- with no args returns a float in [0, 1); scaling and flooring is
    -- the documented integer-extraction idiom.
    local rand_parts = {}
    for i = 1, 4 do
        local chunk = math.floor(math.random() * 0x100000)  -- 20 bits
        rand_parts[i] = encode_int(chunk, 4)
    end
    local rand_part = table.concat(rand_parts)

    return time_part .. rand_part
end

return M
