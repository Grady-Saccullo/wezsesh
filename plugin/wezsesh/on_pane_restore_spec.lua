-- Busted-style spec for on_pane_restore.lua. Self-contained — runs
-- under plain `lua plugin/wezsesh/on_pane_restore_spec.lua` from the
-- repo root, no busted required.
--
-- Run:
--     cd <repo-root>
--     lua plugin/wezsesh/on_pane_restore_spec.lua
--
-- Exits 0 with `OK N/N` on success, 1 with FAIL lines on stderr
-- otherwise. Mirrors the structure of manager_spec.lua / ops_spec.lua.
--
-- Acceptance gates exercised:
--   * Argv hook fail-CLOSED — forced exception → no
--     default_on_pane_restore invocation; exactly one `\r\n` send.
--   * Argv allowlist enforcement (Lua side) — argv[1]="rm" → no exec;
--     `cd '<cwd>'\r\n` if cwd clean.
--   * Control-char cwd / argv — `cwd="/tmp/foo\nrm -rf ~"` → no
--     injection; downgrade to no-op fallback. Same with control char
--     inside argv + clean cwd → fallback uses cd <cwd>.
--   * Single-arg callback shape, 1-based argv indexing.
--   * bytes_clean applied to BOTH every argv element AND cwd.

local function script_dir()
    local src = arg and arg[0] or "plugin/wezsesh/on_pane_restore_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
-- Two roots: script_dir() lets `require("on_pane_restore")` resolve to
-- the file next to this spec, and script_dir()/../ lets the dotted form
-- `require("wezsesh.runtime.log")` resolve via plugin/wezsesh/runtime/log.lua.
package.path = script_dir() .. "/?.lua;"
            .. script_dir() .. "/../?.lua;"
            .. package.path

-- ────────────────────────────────────────────────────────────────────
-- wezterm shim — installed BEFORE require("on_pane_restore")
-- ────────────────────────────────────────────────────────────────────

local log_calls = { warn = {}, error = {} }

-- Snapshot-on-read GLOBAL proxy. runtime/log.lua now requires
-- runtime/state.lua and runtime/globals.lua, both of which touch
-- wezterm.GLOBAL on every log emission. A missing GLOBAL surface here
-- would crash the require chain.
local helpers_for_shim = require("spec_helpers")
local global_proxy = helpers_for_shim.make_global_proxy()

local wezterm_shim = {
    GLOBAL = global_proxy,
    -- runtime/state.lua and runtime/log.lua call json_encode/json_parse
    -- to round-trip the active-trace bucket and to encode the structured
    -- log record. The codec from spec_helpers is shape-faithful.
    json_encode = helpers_for_shim.make_json_codec().encode,
    json_parse  = helpers_for_shim.make_json_codec().decode,
    log_warn = function(msg)
        log_calls.warn[#log_calls.warn + 1] = tostring(msg)
    end,
    log_error = function(msg)
        log_calls.error[#log_calls.error + 1] = tostring(msg)
    end,
    log_info = function(msg)
        -- Some tests below assert against log_calls.warn for messages
        -- that may now be emitted at info level if a future contributor
        -- moves them. Capture into the same warn bucket so substring
        -- matching keeps working.
        log_calls.warn[#log_calls.warn + 1] = tostring(msg)
    end,
    -- Shim approximating wezterm.shell_quote_arg (which delegates to
    -- Rust's shlex::try_quote). Production uses wezterm's real impl;
    -- this POSIX single-quote escape is sufficient for the clean ASCII
    -- inputs these tests exercise (every input has already passed
    -- bytes_clean, so no NUL or controls).
    shell_quote_arg = function(s)
        return "'" .. tostring(s):gsub("'", "'\\''") .. "'"
    end,
}

package.preload["wezterm"] = function() return wezterm_shim end

local on_pane_restore = require("on_pane_restore")
local default_allowlist = require("default_allowlist")

-- Build a hashed lookup from the codegen'd default list. Every test
-- uses this as the production policy unless it injects its own.
local function build_default_policy()
    local set = {}
    for _, name in ipairs(default_allowlist) do set[name] = true end
    return {
        allows = function(prog) return set[prog] == true end,
    }
end

-- ────────────────────────────────────────────────────────────────────
-- harness
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
    log_calls.warn = {}
    log_calls.error = {}
    on_pane_restore._reset_deps()
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

local function assert_match(s, pattern, msg)
    if type(s) ~= "string" or s:find(pattern) == nil then
        error(string.format("%s\n   string: %s\n  pattern: %s",
            msg or "pattern mismatch", tostring(s), pattern), 2)
    end
end

-- Fake pane: records every send_text call.
local function make_pane()
    local sent = {}
    local pane = {}
    function pane:send_text(s)  -- luacheck: ignore self
        sent[#sent + 1] = s
    end
    return pane, sent
end

-- Fake resurrect with an invocation counter on default_on_pane_restore.
-- The real resurrect plugin nests this under `tab_state` (NOT at the
-- top level); keeping the mock shape honest is the only thing that
-- catches the kind of typo where on_pane_restore.lua used to read
-- `resurrect.default_on_pane_restore` directly and silently no-op'd
-- because the type-check guard masked it.
local function make_resurrect()
    local calls = {}
    return {
        tab_state = {
            default_on_pane_restore = function(pane_tree)
                calls[#calls + 1] = pane_tree
            end,
        },
    }, calls
end

-- ────────────────────────────────────────────────────────────────────
-- module surface
-- ────────────────────────────────────────────────────────────────────

describe("module surface", function()
    it("exposes callback, configure, bytes_clean", function()
        assert_eq(type(on_pane_restore.callback), "function",
            "M.callback missing")
        assert_eq(type(on_pane_restore.configure), "function",
            "M.configure missing")
        assert_eq(type(on_pane_restore.bytes_clean), "function",
            "M.bytes_clean missing")
    end)

    it("exposes a _reset_deps test seam", function()
        assert_eq(type(on_pane_restore._reset_deps), "function",
            "M._reset_deps missing — spec needs it for inter-test isolation")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- bytes_clean
-- ────────────────────────────────────────────────────────────────────

describe("bytes_clean", function()
    it("rejects non-string and empty input", function()
        assert_false(on_pane_restore.bytes_clean(nil),
            "nil should be dirty")
        assert_false(on_pane_restore.bytes_clean(42),
            "number should be dirty")
        assert_false(on_pane_restore.bytes_clean(""),
            "empty string should be dirty")
    end)

    it("rejects every byte in 0x00–0x1F and 0x7F", function()
        for b = 0, 31 do
            assert_false(
                on_pane_restore.bytes_clean("a" .. string.char(b) .. "z"),
                string.format("byte 0x%02x should be dirty", b))
        end
        assert_false(
            on_pane_restore.bytes_clean("a" .. string.char(0x7F) .. "z"),
            "byte 0x7F (DEL) should be dirty")
    end)

    it("accepts printable ASCII and high bytes", function()
        assert_true(on_pane_restore.bytes_clean("/tmp/foo bar"),
            "spaces should be clean")
        assert_true(on_pane_restore.bytes_clean("/tmp/$dollar`tick"),
            "shell metas (handled by quoter) should be clean")
        -- High-bit bytes (UTF-8 leading bytes etc.) MUST pass — the
        -- byte-by-byte check should not reject them.
        assert_true(on_pane_restore.bytes_clean("/tmp/" .. string.char(0xE2, 0x9C, 0x93)),
            "high-bit / UTF-8 bytes should be clean")
    end)

    it("rejects NUL specifically", function()
        assert_false(on_pane_restore.bytes_clean("a\0b"),
            "NUL byte should be dirty")
    end)

    it("rejects \\n and \\r (the injection bytes)", function()
        assert_false(on_pane_restore.bytes_clean("a\nb"),
            "LF should be dirty")
        assert_false(on_pane_restore.bytes_clean("a\rb"),
            "CR should be dirty")
    end)

    it("rejects U+2028 / U+2029 (terminal line-separator codepoints)",
    function()
        -- U+2028 LINE SEPARATOR = \xE2\x80\xA8. Modern terminals
        -- interpret it as a line break, splitting `cd '...<U+2028>...'`
        -- into two commands and executing the injected payload.
        assert_false(
            on_pane_restore.bytes_clean("/tmp/foo\xE2\x80\xA8rm -rf ~"),
            "U+2028 (LINE SEPARATOR) should be dirty")
        -- U+2029 PARAGRAPH SEPARATOR = \xE2\x80\xA9, same exec class.
        assert_false(
            on_pane_restore.bytes_clean("/tmp/foo\xE2\x80\xA9rm -rf ~"),
            "U+2029 (PARAGRAPH SEPARATOR) should be dirty")
        -- The other E2 80 xx codepoints are NOT exec-injection vectors
        -- — they're typographic. Confirm we don't over-reject.
        assert_true(
            on_pane_restore.bytes_clean("/tmp/foo\xE2\x80\xA0bar"),
            "non-line-separator E2 80 xx should be clean")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- empty / missing argv
-- ────────────────────────────────────────────────────────────────────

describe("step 2 — empty/missing argv falls through to default", function()
    it("missing process field → default_on_pane_restore called", function()
        local resurrect, calls = make_resurrect()
        local pane = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        on_pane_restore.callback({ pane = pane })
        assert_eq(#calls, 1, "default_on_pane_restore not called")
    end)

    it("process.argv missing → default_on_pane_restore called", function()
        local resurrect, calls = make_resurrect()
        local pane = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        on_pane_restore.callback({ pane = pane, process = {} })
        assert_eq(#calls, 1, "default_on_pane_restore not called")
    end)

    it("argv length 0 → default_on_pane_restore called", function()
        local resurrect, calls = make_resurrect()
        local pane = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        on_pane_restore.callback({
            pane = pane,
            process = { argv = {} },
        })
        assert_eq(#calls, 1, "default_on_pane_restore not called")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- basename + 1-based indexing
-- ────────────────────────────────────────────────────────────────────

describe("step 3 — argv[1] is the program (1-based)", function()
    it("strips a leading path on argv[1]", function()
        -- /bin/bash basenames to "bash", which is in the default
        -- allowlist → success path; default_on_pane_restore called.
        local resurrect, calls = make_resurrect()
        local pane = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        on_pane_restore.callback({
            pane = pane,
            process = { argv = { "/bin/bash", "-l" } },
            cwd = "/home/user",
        })
        assert_eq(#calls, 1,
            "/bin/bash should basename to 'bash' (allowlisted) → default fired")
    end)

    it("uses argv[1] (NOT argv[0]) — Lua 1-based", function()
        -- argv = {"rm", "-rf"}: argv[1] = "rm" (not allowlisted).
        -- A buggy 0-based reader might index argv[0] (=nil) and fall
        -- through to step 2 → default fired. We assert the opposite.
        local resurrect, calls = make_resurrect()
        local pane, sent = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        on_pane_restore.callback({
            pane = pane,
            process = { argv = { "rm", "-rf" } },
            cwd = "/home/user",
        })
        assert_eq(#calls, 0,
            "argv[1]='rm' should be denied; default MUST NOT fire")
        assert_eq(#sent, 1, "expected exactly one send_text")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Argv allowlist enforcement (Lua side)
-- ────────────────────────────────────────────────────────────────────

describe("Argv allowlist enforcement (Lua side)", function()
    it("argv[1]='rm' → no exec; cd '<cwd>'\\r\\n if cwd clean", function()
        local resurrect, calls = make_resurrect()
        local pane, sent = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        on_pane_restore.callback({
            pane = pane,
            process = { argv = { "rm", "-rf", "/tmp" } },
            cwd = "/home/user",
        })
        assert_eq(#calls, 0,
            "default_on_pane_restore was invoked despite denied argv[1]")
        assert_eq(#sent, 1, "expected exactly one send_text")
        assert_eq(sent[1], "cd '/home/user'\r\n",
            "cwd-cd send_text shape wrong")
        assert_true(#log_calls.warn >= 1,
            "expected a log_warn for the skipped argv restore")
        assert_match(log_calls.warn[1], "skipped argv restore",
            "log message did not mention skipped argv restore")
        assert_match(log_calls.warn[1], "cwd clean",
            "log message did not mention cwd clean")
    end)

    it("missing cwd → \\r\\n only", function()
        local resurrect, calls = make_resurrect()
        local pane, sent = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        on_pane_restore.callback({
            pane = pane,
            process = { argv = { "rm" } },
        })
        assert_eq(#calls, 0, "default fired despite denied argv")
        assert_eq(sent[1], "\r\n",
            "missing-cwd path should send '\\r\\n' only")
        assert_match(log_calls.warn[1], "cwd dirty",
            "missing cwd should log as 'cwd dirty'")
    end)

    it("allowlisted argv with clean cwd → default fires", function()
        local resurrect, calls = make_resurrect()
        local pane, sent = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        local pt = {
            pane = pane,
            process = { argv = { "vim", "/etc/hosts" } },
            cwd = "/home/user",
        }
        on_pane_restore.callback(pt)
        assert_eq(#calls, 1, "default_on_pane_restore not invoked")
        assert_true(calls[1] == pt,
            "default_on_pane_restore got a different pane_tree object")
        assert_eq(#sent, 0, "no send_text expected on success path")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Control-char cwd / argv (the injection-byte gate)
-- ────────────────────────────────────────────────────────────────────

describe("Control-char cwd/argv defense", function()
    it("cwd='/tmp/foo\\nrm -rf ~' → no injection; \\r\\n only", function()
        -- argv is allowlisted (vim) but cwd contains LF. Step 6
        -- detects the control byte and routes to step 4. The cwd
        -- itself is dirty so send_cwd_or_newline falls back to '\r\n'.
        -- CRITICAL: the cwd MUST NOT appear in any sent_text.
        local resurrect, calls = make_resurrect()
        local pane, sent = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        on_pane_restore.callback({
            pane = pane,
            process = { argv = { "vim" } },
            cwd = "/tmp/foo\nrm -rf ~",
        })
        assert_eq(#calls, 0, "default fired despite dirty cwd")
        assert_eq(#sent, 1, "expected exactly one send_text")
        assert_eq(sent[1], "\r\n",
            "dirty cwd must downgrade to bare '\\r\\n'")
        for _, s in ipairs(sent) do
            assert_true(s:find("rm", 1, true) == nil,
                "injected 'rm' leaked into sent text: " .. s)
            assert_true(s:find("/tmp/foo", 1, true) == nil,
                "dirty cwd leaked into sent text: " .. s)
        end
    end)

    it("argv element with NUL + clean cwd → cd <cwd>", function()
        -- argv[1] is allowlisted ("vim") but argv[2] contains a NUL.
        -- Step 5 detects the dirty argv element and routes to step 4.
        -- Because cwd is clean, send_cwd_or_newline emits cd <cwd>.
        local resurrect, calls = make_resurrect()
        local pane, sent = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        on_pane_restore.callback({
            pane = pane,
            process = { argv = { "vim", "innocent\0; rm -rf ~" } },
            cwd = "/home/user",
        })
        assert_eq(#calls, 0, "default fired despite dirty argv element")
        assert_eq(#sent, 1, "expected exactly one send_text")
        assert_eq(sent[1], "cd '/home/user'\r\n",
            "clean cwd path expected")
        for _, s in ipairs(sent) do
            assert_true(s:find("rm", 1, true) == nil,
                "argv-injected 'rm' leaked into sent text")
        end
    end)

    it("cwd containing U+2028 → no injection; \\r\\n only", function()
        -- bytes_clean rejects \xE2\x80\xA8 (LINE SEPARATOR). Step 7
        -- routes to fallback; cwd is dirty so send_cwd_or_newline emits
        -- a bare '\r\n' — the cwd MUST NOT appear in any send_text.
        local resurrect, calls = make_resurrect()
        local pane, sent = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        on_pane_restore.callback({
            pane = pane,
            process = { argv = { "vim" } },
            cwd = "/tmp/foo\xE2\x80\xA8rm -rf ~",
        })
        assert_eq(#calls, 0,
            "default fired despite U+2028 in cwd")
        assert_eq(#sent, 1, "expected exactly one send_text")
        assert_eq(sent[1], "\r\n",
            "U+2028-tainted cwd must downgrade to bare '\\r\\n'")
        for _, s in ipairs(sent) do
            assert_true(s:find("rm", 1, true) == nil,
                "U+2028-injected 'rm' leaked into sent text: " .. s)
        end
    end)

    it("argv element containing U+2029 + clean cwd → cd <cwd>", function()
        local resurrect, calls = make_resurrect()
        local pane, sent = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        on_pane_restore.callback({
            pane = pane,
            process = { argv = { "vim", "innocent\xE2\x80\xA9rm -rf ~" } },
            cwd = "/home/user",
        })
        assert_eq(#calls, 0,
            "default fired despite U+2029 in argv element")
        assert_eq(#sent, 1, "expected exactly one send_text")
        assert_eq(sent[1], "cd '/home/user'\r\n",
            "clean cwd path expected")
        for _, s in ipairs(sent) do
            assert_true(s:find("rm", 1, true) == nil,
                "argv-injected 'rm' leaked into sent text")
        end
    end)

    it("argv[1] (the program path itself) with control char is rejected",
    function()
        -- A malicious snapshot that crafts argv[1]="bash\nrm -rf ~"
        -- might basename to "bash\nrm -rf ~". The basename is then
        -- matched against the allowlist (will not match — controls in
        -- the basename) → step 4. Even if it DID match, step 5's
        -- bytes_clean over each argv element catches it.
        local resurrect, calls = make_resurrect()
        local pane, sent = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        on_pane_restore.callback({
            pane = pane,
            process = { argv = { "bash\nrm -rf ~" } },
            cwd = "/home/user",
        })
        assert_eq(#calls, 0, "default fired despite dirty argv[1]")
        for _, s in ipairs(sent) do
            assert_true(s:find("rm", 1, true) == nil,
                "leak detected in sent text")
        end
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Argv size / count caps (Fix #4 — DoS defense-in-depth)
-- ────────────────────────────────────────────────────────────────────

describe("Argv size / count caps", function()
    it("argv element > 64 KB → fallback fires", function()
        -- A 100 KB argv element exceeds MAX_ARGV_ELEMENT_BYTES.
        -- argv[1]="vim" is allowlisted, so step 5 passes, but step 6's
        -- per-element cap rejects → fallback to cd <cwd>.
        local resurrect, calls = make_resurrect()
        local pane, sent = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        on_pane_restore.callback({
            pane = pane,
            process = { argv = { "vim", string.rep("A", 100 * 1024) } },
            cwd = "/home/user",
        })
        assert_eq(#calls, 0,
            "default fired despite oversized argv element")
        assert_eq(#sent, 1, "expected exactly one send_text")
        assert_eq(sent[1], "cd '/home/user'\r\n",
            "fallback should send cd <cwd>")
    end)

    it("argv count > 256 → fallback fires", function()
        local resurrect, calls = make_resurrect()
        local pane, sent = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        local argv = { "vim" }
        for i = 2, 257 do argv[i] = "x" end
        assert_eq(#argv, 257, "test fixture argv count wrong")
        on_pane_restore.callback({
            pane = pane,
            process = { argv = argv },
            cwd = "/home/user",
        })
        assert_eq(#calls, 0,
            "default fired despite argv count > 256")
        assert_eq(#sent, 1, "expected exactly one send_text")
        assert_eq(sent[1], "cd '/home/user'\r\n",
            "fallback should send cd <cwd>")
    end)

    it("argv total > 256 KB → fallback fires", function()
        -- Each element ≤ 64 KB (per-element cap not tripped) but the
        -- running total exceeds MAX_ARGV_TOTAL_BYTES.
        local resurrect, calls = make_resurrect()
        local pane, sent = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        local argv = { "vim" }
        for i = 2, 6 do argv[i] = string.rep("B", 60 * 1024) end
        -- Total = 3 + 5 * 60 KB = 300 KB + change > 256 KB.
        on_pane_restore.callback({
            pane = pane,
            process = { argv = argv },
            cwd = "/home/user",
        })
        assert_eq(#calls, 0,
            "default fired despite argv total > 256 KB")
        assert_eq(#sent, 1, "expected exactly one send_text")
        assert_eq(sent[1], "cd '/home/user'\r\n",
            "fallback should send cd <cwd>")
    end)

    it("argv just under all caps → default fires", function()
        -- Sanity: confirm we don't over-reject. argv with 256 elements,
        -- each tiny, total well under 256 KB → success path.
        local resurrect, calls = make_resurrect()
        local pane = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        local argv = { "vim" }
        for i = 2, 256 do argv[i] = "x" end
        assert_eq(#argv, 256, "test fixture argv count wrong")
        on_pane_restore.callback({
            pane = pane,
            process = { argv = argv },
            cwd = "/home/user",
        })
        assert_eq(#calls, 1,
            "default should fire when caps are not tripped")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Relative / empty argv[1] (Fix #5 — defense-in-depth)
-- ────────────────────────────────────────────────────────────────────

describe("argv[1] relative-path / empty rejection", function()
    it("argv[1]='./evil' → fallback fires", function()
        -- basename("./evil") == "evil"; if we relied on the allowlist
        -- alone, an attacker-planted ./evil binary in cwd would be
        -- spawned. Step 3 rejects before the basename strips the path.
        local resurrect, calls = make_resurrect()
        local pane, sent = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            -- Inject a permissive policy to ensure the rejection happens
            -- in step 3 (relative path) and not via the allowlist.
            policy = { allows = function() return true end },
        })
        on_pane_restore.callback({
            pane = pane,
            process = { argv = { "./evil", "-rf" } },
            cwd = "/home/user",
        })
        assert_eq(#calls, 0,
            "default fired despite relative argv[1] './evil'")
        assert_eq(#sent, 1, "expected exactly one send_text")
        assert_eq(sent[1], "cd '/home/user'\r\n",
            "fallback should send cd <cwd>")
    end)

    it("argv[1]='../evil' → fallback fires", function()
        local resurrect, calls = make_resurrect()
        local pane, sent = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = { allows = function() return true end },
        })
        on_pane_restore.callback({
            pane = pane,
            process = { argv = { "../evil" } },
            cwd = "/home/user",
        })
        assert_eq(#calls, 0,
            "default fired despite relative argv[1] '../evil'")
        assert_eq(#sent, 1, "expected exactly one send_text")
        assert_eq(sent[1], "cd '/home/user'\r\n",
            "fallback should send cd <cwd>")
    end)

    it("argv = {''} → fallback fires (NOT step 2 default)", function()
        -- argv = {} would route to step 2 (resurrect default). The new
        -- case is argv = {""} — non-empty argv with an empty argv[1].
        -- Step 3's empty-string guard rejects.
        local resurrect, calls = make_resurrect()
        local pane, sent = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = { allows = function() return true end },
        })
        on_pane_restore.callback({
            pane = pane,
            process = { argv = { "" } },
            cwd = "/home/user",
        })
        assert_eq(#calls, 0,
            "default fired despite empty argv[1]")
        assert_eq(#sent, 1, "expected exactly one send_text")
        assert_eq(sent[1], "cd '/home/user'\r\n",
            "fallback should send cd <cwd>")
    end)

    it("argv[1]='/usr/bin/zsh' → still allowed (don't over-reject)",
    function()
        -- Absolute paths must continue to work. zsh is in the default
        -- allowlist; basename("/usr/bin/zsh") == "zsh" → success path.
        local resurrect, calls = make_resurrect()
        local pane = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        on_pane_restore.callback({
            pane = pane,
            process = { argv = { "/usr/bin/zsh", "-l" } },
            cwd = "/home/user",
        })
        assert_eq(#calls, 1,
            "absolute-path argv[1] should still hit the success path")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- Argv hook fail-CLOSED (the load-bearing gate)
-- ────────────────────────────────────────────────────────────────────

describe("Argv hook fail-CLOSED", function()
    it("policy.allows raises → no default_on_pane_restore call; "
        .. "exactly one '\\r\\n' send", function()
        local resurrect, calls = make_resurrect()
        local pane, sent = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = {
                allows = function()
                    error("synthetic raise from policy.allows")
                end,
            },
        })
        on_pane_restore.callback({
            pane = pane,
            process = { argv = { "bash" } },
            cwd = "/home/user",
        })
        assert_eq(#calls, 0,
            "default_on_pane_restore MUST NOT be called on hook crash")
        assert_eq(#sent, 1,
            "expected exactly one '\\r\\n' send on hook crash")
        assert_eq(sent[1], "\r\n",
            "fail-CLOSED send_text shape wrong")
        assert_true(#log_calls.warn >= 1,
            "expected a log_warn for the hook crash")
        local saw_crash_log = false
        for _, m in ipairs(log_calls.warn) do
            if m:find("hook crash", 1, true) then
                saw_crash_log = true; break
            end
        end
        assert_true(saw_crash_log,
            "no 'hook crash' WARN in: "
            .. table.concat(log_calls.warn, " | "))
    end)

    it("bytes_clean raise also fails CLOSED", function()
        local resurrect, calls = make_resurrect()
        local pane, sent = make_pane()
        -- Inject a policy that raises only on the second call so step 4
        -- passes but step 5's per-elem loop blows up. (Equivalent to a
        -- rogue bytes_clean.) Easiest version: monkey-patch the
        -- module's bytes_clean for the duration of the test.
        local saved = on_pane_restore.bytes_clean
        on_pane_restore.bytes_clean = function(_s)
            error("synthetic raise from bytes_clean")
        end
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        local ok = pcall(on_pane_restore.callback, {
            pane = pane,
            process = { argv = { "vim", "x" } },
            cwd = "/home/user",
        })
        on_pane_restore.bytes_clean = saved
        assert_true(ok,
            "callback re-raised — outer pcall boundary violated")
        assert_eq(#calls, 0,
            "default_on_pane_restore fired despite hook crash")
        assert_eq(#sent, 1,
            "expected exactly one send_text on hook crash")
        assert_eq(sent[1], "\r\n",
            "hook crash send shape wrong")
    end)

    it("hook crash with no pane available does NOT raise", function()
        local resurrect, calls = make_resurrect()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = {
                allows = function() error("boom") end,
            },
        })
        local ok = pcall(on_pane_restore.callback, {
            -- No `pane` field. The recover arm should still log_warn
            -- and return without raising.
            process = { argv = { "bash" } },
        })
        assert_true(ok, "callback re-raised on missing pane")
        assert_eq(#calls, 0, "default fired despite hook crash")
    end)

    it("CI assertion: hook that raises MUST NOT result in "
        .. "send_text(shell_join_args(argv))", function()
        -- The argv joined would contain "rm" and "-rf"; assert that
        -- neither appears in any send_text payload after a forced
        -- raise. This is the load-bearing fail-closed gate.
        local resurrect, calls = make_resurrect()
        local pane, sent = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = {
                allows = function() error("synthetic raise") end,
            },
        })
        on_pane_restore.callback({
            pane = pane,
            process = { argv = { "rm", "-rf", "/tmp" } },
            cwd = "/home/user",
        })
        assert_eq(#calls, 0,
            "default fired — RCE surface re-introduced")
        for _, s in ipairs(sent) do
            assert_true(s:find("rm", 1, true) == nil,
                "argv leaked into sent text on hook crash: " .. s)
            assert_true(s:find("-rf", 1, true) == nil,
                "argv leaked into sent text on hook crash: " .. s)
            assert_true(s:find("/tmp", 1, true) == nil,
                "argv leaked into sent text on hook crash: " .. s)
        end
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- single-arg callback shape
-- ────────────────────────────────────────────────────────────────────

describe("single-arg callback shape", function()
    it("M.callback accepts exactly one argument (pane_tree)", function()
        -- A callable that's invoked with `(pane_tree)` MUST work; one
        -- with `(pane, pane_tree)` would mean argv[1] etc. are looked
        -- up on the wrong table. We assert the single-arg shape by
        -- threading a known pane_tree through and checking that the
        -- success-path default_on_pane_restore receives it as its sole
        -- argument.
        local resurrect, calls = make_resurrect()
        local pane = make_pane()
        on_pane_restore.configure({
            resurrect = resurrect,
            policy = build_default_policy(),
        })
        local pt = {
            pane = pane,
            process = { argv = { "bash" } },
            cwd = "/home/user",
        }
        on_pane_restore.callback(pt)
        assert_eq(#calls, 1, "default not invoked")
        assert_true(calls[1] == pt,
            "single-arg shape violated; pane_tree not threaded "
            .. "through to default_on_pane_restore")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- default_allowlist integration
-- ────────────────────────────────────────────────────────────────────

describe("default_allowlist integration", function()
    it("default list contains shells (sh/bash/zsh) and editors", function()
        local set = {}
        for _, n in ipairs(default_allowlist) do set[n] = true end
        for _, want in ipairs({ "sh", "bash", "zsh", "vim", "nvim",
                                 "git", "tmux", "screen" }) do
            assert_true(set[want],
                "default_allowlist missing expected entry: " .. want)
        end
    end)

    it("denies a non-allowlisted program (`rm`)", function()
        local set = {}
        for _, n in ipairs(default_allowlist) do set[n] = true end
        assert_false(set["rm"],
            "default_allowlist unexpectedly contains 'rm'")
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
