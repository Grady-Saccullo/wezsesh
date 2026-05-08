-- Constant-time string compare for the HMAC verify path. Lua's `==`
-- short-circuits at the first byte mismatch and leaks the position of
-- the divergence via wall-clock timing. This module's `eq` walks both
-- strings to completion, accumulating the XOR of every byte pair into
-- a single integer; the comparison only inspects that accumulator at
-- the end, so for equal-length inputs the work performed is
-- independent of where — or whether — the bytes diverge.
--
-- Requires Lua 5.3+ for native bitwise operators (`~` binary XOR,
-- `|` bitwise OR). wezterm currently embeds mlua/Lua 5.4, which
-- satisfies this. The runtime guard below makes `require("ct_eq")`
-- raise immediately on Lua 5.1/5.2 rather than emitting the more
-- confusing parse error `~` and `|` would produce in `eq`'s body. A
-- future LuaJIT swap (no native bitwise) would also fail loudly here
-- rather than silently breaking HMAC verification.
assert(_VERSION >= "Lua 5.3",
       "ct_eq.lua requires Lua 5.3+ for native bitwise operators (got "
       .. tostring(_VERSION) .. ")")

local M = {}

-- Constant-time byte-string equality.
--
-- For equal-length inputs the loop body is branch-free: every
-- iteration reads one byte from each side, XORs them, ORs the result
-- into the running accumulator, and increments the index. No
-- `if`/`break` sits inside the loop, so the cumulative work and
-- memory-access pattern depend only on `#a`, not on the byte values.
--
-- The `#a ~= #b` short-circuit at the top is a length check. That's
-- a public attribute of the wire format — HMAC digests are always 64
-- hex chars — and is acceptable to leak. The hot path only ever
-- compares two equal-length 64-char hex strings, so this branch is
-- never taken in practice.
function M.eq(a, b)
    if #a ~= #b then return false end
    local d = 0
    for i = 1, #a do
        d = d | (a:byte(i) ~ b:byte(i))
    end
    return d == 0
end

return M
