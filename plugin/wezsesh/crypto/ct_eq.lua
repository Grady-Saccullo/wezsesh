-- §9.9 / §5.6 — constant-time string compare for the HMAC verify path
-- in ipc.lua step (f). Lua's `==` short-circuits at the first byte
-- mismatch and leaks the position of the divergence via wall-clock
-- timing. This module's `eq` walks both strings to completion,
-- accumulating the XOR of every byte pair into a single integer; the
-- comparison only inspects that accumulator at the end, so for
-- equal-length inputs the work performed is independent of where —
-- or whether — the bytes diverge.
--
-- §9.0 / §0.1 row 15 — requires Lua 5.3+ for native bitwise operators
-- (`~` binary XOR, `|` bitwise OR). wezterm currently embeds mlua/Lua
-- 5.4, which satisfies this. Two independent gates enforce the floor:
--
--   • Build-time floor — the §16.4 CI matrix row "Lua version
--     assertion" asserts `_VERSION` ≥ "Lua 5.3" against the wezterm
--     release the build matrix targets, so a downgrade to a Lua 5.2
--     (or LuaJIT) wezterm fails CI before shipping.
--   • Runtime guard — the `assert(_VERSION >= "Lua 5.3", ...)` at
--     module load below; `require("ct_eq")` raises immediately on
--     Lua 5.1/5.2 rather than producing the more confusing parse
--     error that `~` and `|` in `eq`'s body would emit on those
--     interpreters.
--
-- A future LuaJIT swap therefore fails loudly rather than silently
-- breaking HMAC verification (LuaJIT has no native bitwise operators
-- — would require a `bit.bxor`/`bit.bor` rewrite per §9.0).
assert(_VERSION >= "Lua 5.3",
       "ct_eq.lua requires Lua 5.3+ for native bitwise operators (got "
       .. tostring(_VERSION) .. ")")

local M = {}

-- §9.9 — constant-time byte-string equality.
--
-- For equal-length inputs the loop body is branch-free: every
-- iteration reads one byte from each side, XORs them, ORs the result
-- into the running accumulator, and increments the index. No
-- `if`/`break` sits inside the loop, so the cumulative work and
-- memory-access pattern depend only on `#a`, not on the byte values.
--
-- The `#a ~= #b` short-circuit at the top is a length check; that is
-- a public attribute of the wire format (HMAC digests are always 64
-- hex chars; §5.1) and is acceptable to leak per §5.6's caller
-- contract. The HMAC verify path in hmac.lua / ipc.lua only ever
-- compares two equal-length 64-char hex strings, so this branch is
-- never taken on the hot path.
function M.eq(a, b)
    if #a ~= #b then return false end
    local d = 0
    for i = 1, #a do
        d = d | (a:byte(i) ~ b:byte(i))
    end
    return d == 0
end

return M
