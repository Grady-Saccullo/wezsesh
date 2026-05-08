-- Reply emitter for the Lua side of the IPC protocol.
--
-- This module owns the second half of the request/reply round-trip:
-- after the verb dispatcher decides what happened, it calls one of
-- the four `reply_*` functions here to spit a JSON envelope back to
-- the binary's reply socket. Every reply (started / completed /
-- partial / error) lands on the wire via the same path:
--
--   1. Build a flat Lua table matching the reply envelope:
--      `v`, `id`, `status`, `ok`, plus the verb-shape's `data`,
--      `warnings`, or `error` fields per the kind invariants.
--   2. JSON-encode it via `canonical_json.encode`, HMAC-sign with the
--      session key (encode-sans-hmac → digest → set hmac → re-encode),
--      and emit the signed bytes. The reply path is now byte-equality-
--      bound symmetric with the request path: Go's parseReply runs
--      Signer.Verify and synthesises a REPLY_HMAC_MISMATCH terminal
--      reply on a verify failure. Verb-supplied containers are passed
--      through `deep_tag` first to satisfy the canonical encoder's
--      "every table must be tagged" contract.
--   3. Base64-encode the JSON via `b64.encode` — the `wezsesh reply`
--      subcommand takes its payload as a single argv to dodge
--      shell-quoting concerns.
--   4. Spawn `wezsesh reply <sock> <b64>` via
--      `wezterm.background_child_process`. The
--      `lua-bg-child-process-pcall` lint requires this call to be
--      `pcall`-wrapped: a failed spawn (binary missing, sock
--      vanished, fork ENOMEM) MUST NOT propagate up to the
--      `user-var-changed` handler, where an uncaught raise wedges the
--      wezterm event loop.
--
-- The reply is fire-and-forget. We never block on the binary's
-- consumption of it; the TUI side has its own 5 s / 30 s reply-budget
-- timers and the binary runs `io.LimitedReader` over the socket. If
-- the binary is gone, the spawn is a noop.
--
-- Every reply carries `v: 1`. The `v` we emit is sourced from
-- `payload.v` so a future protocol bump only needs to round-trip the
-- request's `v` correctly; today payload.v is always 1.
--
-- Reply-kind invariants encoded structurally:
--   * `started`  — ok=true; NO data, warnings, or error.
--   * `completed`+ok=true — data present (may be `{}`).
--   * `completed`+ok=false (error) — error present, no data.
--   * `partial`  — ok=true; data AND warnings present.
--
-- mlua sandbox: acquired via `local wezterm = require("wezterm")`. The
-- standalone spec (`result_spec.lua`) installs a wezterm shim via
-- `package.preload["wezterm"]` BEFORE requiring this file.

local wezterm        = require("wezterm")
local b64            = require("wezsesh.crypto.b64")
local globals        = require("wezsesh.runtime.globals")
local hmac           = require("wezsesh.crypto.hmac")
local canonical_json = require("wezsesh.canonical_json")

local M = {}

-- Recursive auto-tagger for verb-supplied containers. The canonical
-- encoder rejects untagged tables (ENCODER_UNTAGGED_TABLE) — verbs
-- like `list_dirs` hand us `{ dirs = { { name, path }, ... } }` with
-- bare untagged subtables, so we walk and tag in place. Empty tables
-- default to object: empty `data` and `details` MUST land on the wire
-- as `{}` (Go's reply parser cannot unmarshal `[]` into
-- `map[string]any`); empty `warnings` is wrapped as `[]` explicitly at
-- the call site. Idempotent: an already-tagged container's tag wins
-- and we still recurse into its children.
local function deep_tag(t)
    if type(t) ~= "table" then return t end

    local mt = getmetatable(t)
    local tag = mt and mt.__wezsesh_canonical
    if tag == "null" then return t end

    if tag == nil then
        local n = 0
        local all_int_keys = true
        for k in pairs(t) do
            if type(k) ~= "number" or k ~= math.floor(k) or k < 1 then
                all_int_keys = false
                break
            end
            n = n + 1
        end
        if all_int_keys and n > 0 then
            setmetatable(t, canonical_json.array_mt)
            tag = "array"
        else
            setmetatable(t, canonical_json.object_mt)
            tag = "object"
        end
    end

    if tag == "array" then
        for i = 1, #t do deep_tag(t[i]) end
    elseif tag == "object" then
        for _, v in pairs(t) do
            if type(v) == "table" then deep_tag(v) end
        end
    end
    return t
end

-- Replace any non-UTF-8 byte sequence in `s` with U+FFFD. Applied
-- ONLY to fields originating from `tostring(err)` / resurrect's
-- `with_capture` (error.message, error.details.raw_error,
-- warning.message, warning.details.raw_error). Verb-controlled
-- identifiers (error.code, data.name, etc.) are not sanitised — a bug
-- that produces non-UTF-8 in those should surface, not be papered
-- over.
local function sanitize_utf8(s)
    if type(s) ~= "string" then return s end
    if utf8.len(s) ~= nil then return s end

    local out = {}
    local i, n = 1, #s
    while i <= n do
        local ok_len, bad = utf8.len(s, i, n)
        if ok_len ~= nil then
            out[#out + 1] = s:sub(i, n)
            break
        end
        if bad > i then
            out[#out + 1] = s:sub(i, bad - 1)
        end
        out[#out + 1] = "\xEF\xBF\xBD"
        i = bad + 1
    end
    return table.concat(out)
end

-- Resolve the wezsesh binary path. apply_to_config writes it via
-- `runtime.globals.set_bin_path` at load time; an unset value (mis-
-- configured plugin) makes the spawn a noop and the binary on the
-- other end hits IPC_TIMEOUT, which the TUI surfaces.
local function bin_path()
    local p = globals.bin_path()
    if type(p) == "string" and #p > 0 then return p end
    return nil
end

-- Resolve the plugin-side session id stamped onto every reply.
-- `init.lua`'s apply_to_config mints the id and stashes it via
-- `globals.set_plugin_session_id`; on a brief window before that runs
-- (or in a stripped-down spec harness) the accessor returns nil and
-- we fall back to "" so the field is still present on the wire (the
-- canonical-JSON shape walker treats it as required).
local function plugin_session_id()
    local id = globals.plugin_session_id()
    if type(id) == "string" and #id > 0 then return id end
    return ""
end

-- Build the recovery envelope used when the primary encode raises.
-- Guaranteed-clean ASCII so the recovery encode itself can't fail
-- under normal conditions; a sanitised error message lets the TUI
-- surface what actually went wrong instead of silently hanging.
local function build_recovery_envelope(payload, err_text)
    return canonical_json.object{
        v                 = payload.v,
        id                = payload.id,
        status            = "completed",
        ok                = false,
        binary_session_id = payload.binary_session_id or "",
        plugin_session_id = plugin_session_id(),
        error             = canonical_json.object{
            code    = "REPLY_ENCODE_FAILED",
            message = sanitize_utf8(tostring(err_text or "")),
            details = canonical_json.object{},
        },
    }
end

-- Sign an envelope and return its canonical-JSON bytes with the `hmac`
-- field attached. Mirrors Go's `Signer.Sign` field-removal sequence:
-- encode-sans-hmac → HMAC-SHA-256 → set envelope.hmac → re-encode.
-- NEVER set envelope.hmac = "" before encoding (the forbidden alt
-- emits `,"hmac":"",` into the signed bytes and breaks symmetry with
-- the Go verifier).
--
-- Returns (json, nil) on success; (nil, sentinel) on encode failure
-- (ENCODER_*, etc.) or missing/invalid session key. The caller routes
-- the sentinel through the recovery envelope path.
local function sign_envelope(envelope)
    local enc_ok, bytes_no_hmac = pcall(canonical_json.encode, envelope)
    if not enc_ok or type(bytes_no_hmac) ~= "string" then
        return nil, bytes_no_hmac
    end
    local key = globals.session_key()
    if type(key) ~= "string" or #key ~= 64 then
        return nil, "WEZSESH_SESSION_KEY_MISSING: session key absent or wrong length"
    end
    local hm_ok, digest = pcall(hmac.compute, bytes_no_hmac, key)
    if not hm_ok or type(digest) ~= "string" then
        return nil, digest
    end
    envelope.hmac = digest
    local final_ok, json = pcall(canonical_json.encode, envelope)
    if not final_ok or type(json) ~= "string" then
        return nil, json
    end
    return json, nil
end

-- Spawn `wezsesh reply <sock> <b64>` fire-and-forget. The
-- `lua-bg-child-process-pcall` lint verifies this single call to
-- `wezterm.background_child_process` is wrapped in `pcall` — a failed
-- spawn MUST NOT bubble out of the user-var-changed handler.
--
-- Returns true if the spawn was attempted; false if we couldn't even
-- get to the spawn (missing binary path, missing reply_sock, encode
-- failure of both primary and recovery envelopes). On a primary
-- encode failure we synthesise a REPLY_ENCODE_FAILED envelope so the
-- TUI sees a structured error instead of waiting for the IPC
-- timeout; both primary and recovery envelopes are HMAC-signed via
-- sign_envelope so the TUI accepts them at parseReply.
--
-- §3.3 v=2 trace correlation: the binary_session_id from the
-- originating request envelope is propagated into the spawned
-- `wezsesh reply` child via `WEZSESH_BINARY_SESSION_ID`. Wezterm's
-- public `background_child_process(argv)` API takes only an argv
-- list — there is no `set_environment_variables` option on the
-- background variant — so the env var is injected via `/usr/bin/env`,
-- which is in POSIX and present at the same path on Linux and Darwin.
-- The bsid is a 26-char Crockford-base32 ULID (alphabet `[0-9A-HJKMNP-TV-Z]`),
-- so it never contains shell-meta or `=` and is safe to splice
-- inline into env's `KEY=VALUE` token. We still defensively replace
-- any non-conforming character with the empty string before splicing,
-- and skip the env-prefix entirely when the bsid is missing — better
-- to spawn without trace correlation than to hand env(1) a malformed
-- token and have the spawn ENOENT.
local function spawn_reply(reply_sock, payload, envelope)
    local bin = bin_path()
    if bin == nil then return false end
    if type(reply_sock) ~= "string" or #reply_sock == 0 then
        return false
    end

    local json, sign_err = sign_envelope(envelope)
    if json == nil then
        if type(payload) ~= "table" then return false end
        local rec = build_recovery_envelope(payload, sign_err)
        local rec_json
        rec_json, _ = sign_envelope(rec)
        if rec_json == nil then return false end
        json = rec_json
    end

    local b64s = b64.encode(json)

    local argv
    local bsid = payload and payload.binary_session_id
    if type(bsid) == "string" and #bsid == 26
       and bsid:match("^[0-9A-Z]+$") ~= nil
    then
        -- env(1) form: env -i would clear the inherited environment;
        -- we want the child to inherit wezterm's env (PATH, HOME, etc.)
        -- and just add WEZSESH_BINARY_SESSION_ID on top. Plain
        -- `env KEY=VALUE prog args...` is exactly that.
        argv = {
            "/usr/bin/env",
            "WEZSESH_BINARY_SESSION_ID=" .. bsid,
            bin, "reply", reply_sock, b64s,
        }
    else
        -- bsid missing or malformed (legacy v=1 fallback / fixture
        -- harness without payload.binary_session_id). Fall back to the
        -- bare argv shape so the reply still ships; the spawned
        -- `wezsesh reply` simply has no bsid in its env.
        argv = { bin, "reply", reply_sock, b64s }
    end

    local ok = pcall(wezterm.background_child_process, argv)
    return ok
end

-- `started` reply. Restore-class verbs (`switch` to saved-not-live,
-- `load`) emit this BEFORE running the actual restore. The TUI
-- dismisses immediately on receipt; a `completed` or `partial`
-- follow-up is required within 30 s.
--
-- Invariant: started ⇒ ok=true, NO data / warnings / error.
-- We deliberately do NOT thread through any optional kwargs here —
-- the structural shape is fixed and a future call site that wants
-- to attach data to a `started` reply is a bug we want to catch
-- via the spec.
function M.reply_started(payload)
    return spawn_reply(payload.reply_sock, payload, canonical_json.object{
        v                 = payload.v,
        id                = payload.id,
        status            = "started",
        ok                = true,
        binary_session_id = payload.binary_session_id or "",
        plugin_session_id = plugin_session_id(),
    })
end

-- `completed` + ok=true reply. The verb-specific `data` shape is the
-- caller's responsibility (per-verb shapes live in the verb modules).
-- A nil `data` is normalised to an empty table — the wire mandates
-- `data` be present (may be `{}`) for completed+ok=true.
function M.reply_completed(payload, data)
    return spawn_reply(payload.reply_sock, payload, canonical_json.object{
        v                 = payload.v,
        id                = payload.id,
        status            = "completed",
        ok                = true,
        data              = deep_tag(data or {}),
        binary_session_id = payload.binary_session_id or "",
        plugin_session_id = plugin_session_id(),
    })
end

-- `partial` reply. Terminal: success-with-warnings (e.g.,
-- `RESURRECT_PARTIAL` after a mid-restore Lua error). The wire
-- mandates BOTH `data` AND `warnings` present; we normalise nils to
-- empty containers so a caller passing nil for either still produces
-- a conforming envelope.
function M.reply_partial(payload, data, warnings)
    local rebuilt = {}
    if type(warnings) == "table" then
        for i, w in ipairs(warnings) do
            if type(w) == "table" then
                rebuilt[i] = canonical_json.object{
                    code    = tostring(w.code or "UNKNOWN"),
                    message = sanitize_utf8(tostring(w.message or "")),
                    details = deep_tag(w.details or {}),
                }
            end
        end
    end
    return spawn_reply(payload.reply_sock, payload, canonical_json.object{
        v                 = payload.v,
        id                = payload.id,
        status            = "partial",
        ok                = true,
        data              = deep_tag(data or {}),
        warnings          = canonical_json.array(rebuilt),
        binary_session_id = payload.binary_session_id or "",
        plugin_session_id = plugin_session_id(),
    })
end

-- `completed` + ok=false reply. Terminal failure with a structured
-- error. `code` is the wire-stable error-code identifier, e.g.
-- `UNKNOWN_VERB`, `SNAPSHOT_LOAD_FAILED`. `message` is human-readable;
-- `details` is the per-code shape (e.g., `{raw_error = "..."}` for
-- SAVE_FAILED).
function M.reply_error(payload, code, message, details)
    return spawn_reply(payload.reply_sock, payload, canonical_json.object{
        v                 = payload.v,
        id                = payload.id,
        status            = "completed",
        ok                = false,
        binary_session_id = payload.binary_session_id or "",
        plugin_session_id = plugin_session_id(),
        error             = canonical_json.object{
            code    = tostring(code or "UNKNOWN"),
            message = sanitize_utf8(tostring(message or "")),
            details = deep_tag(details or {}),
        },
    })
end

-- Toast helper. Surfaces a wezterm overlay toast for non-wire
-- failure modes (HMAC mismatch logged + IPC_INIT_FAILED on the Lua
-- side, etc.). pcall-wrapped: `window:toast_notification(...)` is a
-- userdata method call on a wezterm-side object and a stale `window`
-- handle (e.g., closed window) raises.
function M.toast(window, message, ms)
    if window == nil or type(message) ~= "string" then return false end
    local ok = pcall(function()
        window:toast_notification("wezsesh", message, nil, ms or 4000)
    end)
    return ok
end

return M
