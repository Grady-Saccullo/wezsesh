-- §9.8 / §4.3 / §5 — HMAC-SHA-256 primitive for the Lua side of the IPC
-- wire protocol. Mirrors the Go `internal/hmac` package's primitive
-- contract: given the canonical sans-hmac payload bytes, return a
-- lowercase hex digest that equals the Go signer's output for the §17.2
-- fixture.
--
-- This module is intentionally narrow. It does NOT canonical-encode and
-- it does NOT remove the `hmac` key from a payload. Callers (ipc.lua
-- step (e), result.lua signer path) MUST:
--
--   1. Construct the payload table without the `hmac` key
--      (canonical_json.copy_without on a tagged copy).
--   2. canonical_json.encode that table.
--   3. Pass the resulting bytes to compute(...) — the bytes feed into
--      HMAC verbatim.
--
-- The forbidden alternative — set payload.hmac = "" then encode — would
-- emit `"hmac":""` into step 2's bytes and produce a different digest.
-- This module cannot detect that misuse; the field-removal sequence is
-- a §4.3 caller-side invariant, exercised by the round-trip fixture
-- test in hmac_spec.lua.
--
-- Errors are raised via error(msg, 0) so the message has no file:line
-- prefix; ipc.lua substring-matches HMAC_* sentinels.

local sha = require("wezsesh.vendor.sha2")

local M = {}

local function fail(code, msg)
    error(code .. ": " .. msg, 0)
end

-- Reject non-conforming hex keys mirrors `internal/hmac.NewSigner`'s
-- ErrBadKey: 64 lowercase hex chars exactly. Both sides agree on
-- lowercase per §5.1 and the §17.2 fixture.
local function decode_hex_key(hex_key)
    if type(hex_key) ~= "string" then
        fail("HMAC_BAD_KEY",
             "key must be string, got " .. type(hex_key))
    end
    if #hex_key ~= 64 then
        fail("HMAC_BAD_KEY",
             "key must be 64 lowercase hex chars, got length " .. #hex_key)
    end
    -- Constrain to lowercase hex; uppercase is rejected per §5.1.
    if hex_key:find("[^0-9a-f]") then
        fail("HMAC_BAD_KEY",
             "key must be lowercase hex chars only")
    end
    -- sha.hex_to_bin accepts mixed case; we've already rejected
    -- non-lowercase above. Result is the 32-byte raw key (CLAUDE.md
    -- invariant 7): hex-decoded BEFORE feeding HMAC.
    return sha.hex_to_bin(hex_key)
end

-- Constant-time compare on equal-length byte strings. Inlined here so
-- this module has no plugin-internal dependency beyond vendor/sha2.lua.
-- Uses Lua 5.3+ bitwise operators (CLAUDE.md / §9.0 — wezterm ships
-- Lua 5.4). If lengths differ the function returns false in constant
-- time relative to the shorter string (length leak is acceptable: the
-- supplied digest length is publicly bounded by the wire format).
local function ct_eq_bytes(a, b)
    if #a ~= #b then return false end
    local d = 0
    for i = 1, #a do
        d = d | (a:byte(i) ~ b:byte(i))
    end
    return d == 0
end

-- Validate that a string is N lowercase hex chars. Used on the
-- caller-supplied digest in verify(). Both sides agree on lowercase.
local function is_lower_hex(s, n)
    if type(s) ~= "string" then return false end
    if #s ~= n then return false end
    if s:find("[^0-9a-f]") then return false end
    return true
end

-- §9.8 — compute HMAC-SHA-256 over payload_bytes with a hex key.
-- Returns the lowercase hex digest (64 chars).
function M.compute(payload_bytes, hex_key)
    if type(payload_bytes) ~= "string" then
        fail("HMAC_BAD_INPUT",
             "payload_bytes must be string, got " .. type(payload_bytes))
    end
    local key_bin = decode_hex_key(hex_key)
    -- sha.hmac returns the digest as lowercase hex (the underlying
    -- hash_func returns hex per the sha2.lua API).
    return sha.hmac(sha.sha256, key_bin, payload_bytes)
end

-- §9.8 — recompute HMAC-SHA-256 over payload_bytes and constant-time
-- compare to the caller-supplied lowercase-hex expected digest. Returns
-- true on match, false otherwise. Bad-shape inputs (non-string, wrong
-- length, non-hex digest) return false rather than raising — the
-- verifier path on the wire treats every failure as silent-drop, and a
-- raise would propagate out of the user-var-changed handler.
function M.verify(payload_bytes, hex_key, expected_hex)
    if type(payload_bytes) ~= "string" then return false end
    if not is_lower_hex(expected_hex, 64) then return false end
    local ok, computed = pcall(M.compute, payload_bytes, hex_key)
    if not ok or type(computed) ~= "string" then return false end
    return ct_eq_bytes(computed, expected_hex)
end

return M
