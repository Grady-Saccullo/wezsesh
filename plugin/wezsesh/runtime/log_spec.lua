-- Busted-style spec for runtime/log.lua. Self-contained — runs under
-- plain `lua plugin/wezsesh/runtime/log_spec.lua` from the repo root,
-- no busted required.
--
-- Run:
--     cd <repo-root>
--     lua plugin/wezsesh/runtime/log_spec.lua
--
-- Exits 0 with `OK N/N` on success, 1 with FAIL lines on stderr
-- otherwise.
--
-- The spec installs a wezterm-shim via `package.preload["wezterm"]`
-- BEFORE requiring the module under test. The shim covers the surface
-- runtime/log.lua actually touches:
--   * wezterm.GLOBAL — snapshot-on-read userdata-shaped table.
--     runtime/log requires runtime/globals (for plugin_session_id /
--     runtime_dir) and runtime/state (for the active-trace lookup),
--     both of which read GLOBAL on every emit.
--   * wezterm.json_encode — used to encode the structured record.
--   * wezterm.json_parse — used by runtime/state to decode the
--     active-trace bucket.
--   * wezterm.log_warn / log_error / log_info — the wezterm-side log
--     sinks runtime/log writes to.
--
-- The structural-record assertions DON'T install a `_set` capture —
-- we use the public API and inspect the recorded wezterm-log output
-- AND the tmp-runtime_dir's plugin.log file. That way the test exercises
-- the real two-leg path (wezterm log + file append).

local function script_dir()
    local src = arg and arg[0] or "plugin/wezsesh/runtime/log_spec.lua"
    return src:match("^(.*)/[^/]+$") or "."
end
-- Three roots:
--   <script_dir>/?.lua            — bare requires for sibling modules
--   <script_dir>/../?.lua         — namespaced wezsesh.runtime.<m>
--   <script_dir>/../../?.lua      — namespaced wezsesh.<m> (above
--                                    runtime/) for crypto / canonical_json /
--                                    spec_helpers used by transitive deps.
package.path = script_dir() .. "/?.lua;"
            .. script_dir() .. "/../?.lua;"
            .. script_dir() .. "/../../?.lua;"
            .. package.path

-- ────────────────────────────────────────────────────────────────────
-- wezterm shim — installed BEFORE require("log")
-- ────────────────────────────────────────────────────────────────────

local helpers = require("spec_helpers")
local global_proxy = helpers.make_global_proxy()
local codec = helpers.make_json_codec()

-- Captured wezterm-log emissions. Each entry is the raw string the
-- module passed to wezterm.log_<level>. The "[wezsesh] " prefix is
-- preserved so prefix assertions stay byte-faithful.
local wezterm_log = { warn = {}, error = {}, info = {} }

local wezterm_shim = {
    GLOBAL = global_proxy,
    json_encode = codec.encode,
    json_parse = codec.decode,
    log_warn = function(msg)
        wezterm_log.warn[#wezterm_log.warn + 1] = msg
    end,
    log_error = function(msg)
        wezterm_log.error[#wezterm_log.error + 1] = msg
    end,
    log_info = function(msg)
        wezterm_log.info[#wezterm_log.info + 1] = msg
    end,
}
package.preload["wezterm"] = function() return wezterm_shim end

-- Now load the modules under test.
local log     = require("wezsesh.runtime.log")
local globals = require("wezsesh.runtime.globals")
local state   = require("wezsesh.runtime.state")

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

-- Tmp dir for the file-append leg. Created lazily on first need;
-- reaped after the run via the OS's tmpdir cleanup (we don't
-- aggressively `os.remove` because the spec wants to inspect the file
-- after each test).
local tmp_runtime_dir
local function ensure_tmp_dir()
    if tmp_runtime_dir ~= nil then return tmp_runtime_dir end
    -- Use mktemp -d so the path is unique. Fallback to /tmp/wezsesh-spec-<pid>
    -- if mktemp isn't available.
    local p
    local fh = io.popen("mktemp -d 2>/dev/null")
    if fh ~= nil then
        p = fh:read("*l")
        fh:close()
    end
    if p == nil or p == "" then
        p = "/tmp/wezsesh-log-spec-" .. tostring(os.time())
        os.execute("mkdir -p " .. p)
    end
    tmp_runtime_dir = p
    return p
end

-- Read the plugin.log file from the tmp runtime_dir. Returns the file
-- contents as a string, or nil if the file doesn't exist yet.
local function read_plugin_log()
    local dir = ensure_tmp_dir()
    local path = dir .. "/plugin.log"
    local fh = io.open(path, "rb")
    if fh == nil then return nil end
    local s = fh:read("*a")
    fh:close()
    return s
end

local function unlink_plugin_log()
    local dir = ensure_tmp_dir()
    local path = dir .. "/plugin.log"
    os.remove(path)
end

local function it(name, fn)
    total = total + 1
    -- Reset capture buckets and GLOBAL between tests so each spec
    -- runs against a fresh module state.
    wezterm_log.warn = {}
    wezterm_log.error = {}
    wezterm_log.info = {}
    log._reset()
    -- Wipe the plugin_session_id / runtime_dir / active-trace keys so
    -- a previous test's seed doesn't bleed.
    for k in pairs(global_proxy) do
        if type(k) == "string" and k:sub(1, 8) == "wezsesh_" then
            global_proxy[k] = nil
        end
    end
    unlink_plugin_log()

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

local function assert_nil(v, msg)
    if v ~= nil then
        error((msg or "expected nil") .. ", got: " .. tostring(v), 2)
    end
end

local function assert_match(s, pattern, msg)
    if type(s) ~= "string" or s:find(pattern) == nil then
        error(string.format("%s\n   string: %s\n  pattern: %s",
            msg or "pattern mismatch", tostring(s), pattern), 2)
    end
end

-- Strip the literal `[wezsesh] ` prefix and decode the JSON record.
local function decode_emitted(line)
    assert_true(type(line) == "string", "expected string emission")
    local prefix = "[wezsesh] "
    assert_eq(line:sub(1, #prefix), prefix,
        "expected '[wezsesh] ' prefix on the wezterm-log line")
    return codec.decode(line:sub(#prefix + 1))
end

-- ────────────────────────────────────────────────────────────────────
-- module surface
-- ────────────────────────────────────────────────────────────────────

describe("module surface", function()
    it("exposes warn/error/info/debug/_set/_reset", function()
        assert_eq(type(log.warn), "function", "log.warn missing")
        assert_eq(type(log.error), "function", "log.error missing")
        assert_eq(type(log.info), "function", "log.info missing")
        assert_eq(type(log.debug), "function", "log.debug missing")
        assert_eq(type(log._set), "function", "log._set missing")
        assert_eq(type(log._reset), "function", "log._reset missing")
        assert_eq(type(log._emit_at), "function", "log._emit_at missing")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- level threshold (mirrors Go-side slog filtering)
-- ────────────────────────────────────────────────────────────────────

describe("level threshold", function()
    it("defaults to 'info' when wezsesh_log_level is nil", function()
        global_proxy.wezsesh_log_level = nil
        assert_eq(log._emit_at("debug"), false, "debug should be gated")
        assert_eq(log._emit_at("info"),  true,  "info should pass")
        assert_eq(log._emit_at("warn"),  true,  "warn should pass")
        assert_eq(log._emit_at("error"), true,  "error should pass")
    end)

    it("'debug' threshold passes everything", function()
        global_proxy.wezsesh_log_level = "debug"
        assert_eq(log._emit_at("debug"), true)
        assert_eq(log._emit_at("info"),  true)
        assert_eq(log._emit_at("warn"),  true)
        assert_eq(log._emit_at("error"), true)
    end)

    it("'warn' threshold drops debug + info", function()
        global_proxy.wezsesh_log_level = "warn"
        assert_eq(log._emit_at("debug"), false)
        assert_eq(log._emit_at("info"),  false)
        assert_eq(log._emit_at("warn"),  true)
        assert_eq(log._emit_at("error"), true)
    end)

    it("'error' threshold drops everything below error", function()
        global_proxy.wezsesh_log_level = "error"
        assert_eq(log._emit_at("debug"), false)
        assert_eq(log._emit_at("info"),  false)
        assert_eq(log._emit_at("warn"),  false)
        assert_eq(log._emit_at("error"), true)
    end)

    it("unknown threshold falls through to 'info'", function()
        global_proxy.wezsesh_log_level = "loud"
        assert_eq(log._emit_at("debug"), false)
        assert_eq(log._emit_at("info"),  true)
    end)

    it("unknown record level emits at info-rank", function()
        global_proxy.wezsesh_log_level = "warn"
        -- Unknown levels are treated as info → dropped at warn threshold.
        assert_eq(log._emit_at("verbose"), false)
        global_proxy.wezsesh_log_level = "info"
        -- At info threshold, unknown still emits (info-rank passes info threshold).
        assert_eq(log._emit_at("verbose"), true)
    end)

    it("warn() at error threshold does NOT reach the wezterm sink", function()
        global_proxy.wezsesh_log_level = "error"
        log.warn("dropped")
        assert_eq(#wezterm_log.warn, 0,
            "warn record should be filtered out before sink emit")
    end)

    it("error() always reaches the sink at every threshold", function()
        for _, lvl in ipairs({ "debug", "info", "warn", "error" }) do
            global_proxy.wezsesh_log_level = lvl
            wezterm_log.error = {}
            log.error("kept")
            assert_eq(#wezterm_log.error, 1,
                "error record should pass at threshold " .. lvl)
        end
    end)

    it("debug() routes only to plugin.log, never wezterm sink", function()
        global_proxy.wezsesh_log_level = "debug"
        global_proxy.wezsesh_runtime_dir = ensure_tmp_dir()
        log.debug("only-file")
        assert_eq(#wezterm_log.warn, 0, "debug must not hit log_warn")
        assert_eq(#wezterm_log.error, 0, "debug must not hit log_error")
        assert_eq(#wezterm_log.info, 0, "debug must not hit log_info")
        local content = read_plugin_log()
        assert_match(content, '"level":"debug"', "debug record should land in plugin.log")
    end)

    it("debug() at default threshold writes nothing to plugin.log", function()
        global_proxy.wezsesh_log_level = "info"
        global_proxy.wezsesh_runtime_dir = ensure_tmp_dir()
        log.debug("dropped")
        -- The file is unlinked between tests; a gated debug call should
        -- leave it untouched, so the helper returns nil (file absent).
        assert_nil(read_plugin_log(),
            "debug record must be gated before plugin.log append")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- structured record shape
-- ────────────────────────────────────────────────────────────────────

describe("structured record shape", function()
    it("records carry level, ts, msg, plugin_session_id", function()
        globals.set_plugin_session_id("01JTESTPLUGIN_____________")
        log.warn("hello world")
        assert_eq(#wezterm_log.warn, 1, "expected one warn emission")
        local rec = decode_emitted(wezterm_log.warn[1])
        assert_eq(rec.level, "warn", "level wrong")
        assert_eq(type(rec.ts), "number", "ts must be a number")
        assert_eq(rec.msg, "hello world", "msg lost")
        assert_eq(rec.plugin_session_id, "01JTESTPLUGIN_____________",
            "plugin_session_id not stamped")
    end)

    it("absent plugin_session_id stamps an empty string", function()
        -- Pre-apply_to_config window: the mint hasn't run.
        log.warn("boot-time message")
        local rec = decode_emitted(wezterm_log.warn[1])
        assert_eq(rec.plugin_session_id, "",
            "absent plugin_session_id should degrade to empty string")
    end)

    it("caller fields merge on top of the structural record", function()
        log.warn("hi", { extra_field = "value", count = 7 })
        local rec = decode_emitted(wezterm_log.warn[1])
        assert_eq(rec.extra_field, "value",
            "caller extra_field not merged")
        assert_eq(rec.count, 7, "caller count not merged")
    end)

    it("caller fields cannot overwrite structural fields", function()
        log.warn("hi",
            { level = "evil", ts = -1,
              plugin_session_id = "evil_id" })
        local rec = decode_emitted(wezterm_log.warn[1])
        assert_eq(rec.level, "warn",
            "caller overrode structural level")
        assert_true(rec.ts ~= -1,
            "caller overrode structural ts")
        assert_eq(rec.plugin_session_id, "",
            "caller overrode structural plugin_session_id")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- pane_id-driven trace context lookup
-- ────────────────────────────────────────────────────────────────────

describe("pane_id-driven trace context lookup", function()
    it("merges trace_id / binary_session_id from active-trace bucket",
    function()
        state.set_active_trace(42, {
            trace_id = "01JTRACE0000000000000000AA",
            binary_session_id = "01JBSESS0000000000000000BB",
        })
        log.warn("dispatching", { pane_id = 42 })
        local rec = decode_emitted(wezterm_log.warn[1])
        assert_eq(rec.trace_id, "01JTRACE0000000000000000AA",
            "trace_id not pulled from active-trace")
        assert_eq(rec.binary_session_id, "01JBSESS0000000000000000BB",
            "binary_session_id not pulled from active-trace")
        assert_eq(rec.pane_id, 42, "pane_id not preserved as a field")
    end)

    it("caller-supplied trace_id wins over active-trace lookup",
    function()
        state.set_active_trace(99, {
            trace_id = "01JFROMSTATE______________",
            binary_session_id = "01JBSESS_FROM_STATE_______",
        })
        log.warn("async callsite",
            { pane_id = 99, trace_id = "01JCAPTURED_______________" })
        local rec = decode_emitted(wezterm_log.warn[1])
        assert_eq(rec.trace_id, "01JCAPTURED_______________",
            "caller trace_id was overridden by lookup")
        -- binary_session_id has no caller override → state lookup wins.
        assert_eq(rec.binary_session_id, "01JBSESS_FROM_STATE_______",
            "binary_session_id should have been pulled from active-trace")
    end)

    it("missing pane_id active-trace skips the lookup quietly",
    function()
        log.warn("no trace context", { pane_id = 12345 })
        local rec = decode_emitted(wezterm_log.warn[1])
        assert_nil(rec.trace_id, "spurious trace_id without active-trace")
        assert_eq(rec.pane_id, 12345, "pane_id field still preserved")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- truncation cap
-- ────────────────────────────────────────────────────────────────────

describe("truncation cap (512 bytes)", function()
    it("an oversized message is truncated with the ellipsis marker",
    function()
        local big = string.rep("X", 1000)
        log.warn(big)
        assert_eq(#wezterm_log.warn, 1, "expected one warn emission")
        local line = wezterm_log.warn[1]
        -- The cap applies to the JSON record (what lands in
        -- plugin.log atomically); the wezterm-log leg prepends
        -- a 10-byte `[wezsesh] ` prefix, which is intentionally
        -- outside the cap because the wezterm GUI log file isn't
        -- write-atomicity-critical. Strip the prefix before
        -- asserting on the cap.
        local prefix = "[wezsesh] "
        assert_eq(line:sub(1, #prefix), prefix,
            "missing [wezsesh] prefix on the wezterm-log line")
        local json = line:sub(#prefix + 1)
        assert_true(#json <= 512,
            "encoded JSON record exceeds 512-byte cap (got "
            .. #json .. ")")
        local rec = decode_emitted(line)
        assert_match(rec.msg, "\xE2\x80\xA6$",
            "truncated msg missing ellipsis marker")
    end)

    it("plugin.log lines respect the 512-byte cap (POSIX atomic-append)",
    function()
        globals.set_runtime_dir(ensure_tmp_dir())
        local big = string.rep("Z", 2000)
        log.warn(big)
        local body = read_plugin_log()
        assert_true(body ~= nil, "plugin.log not created")
        for line in body:gmatch("([^\n]+)") do
            assert_true(#line <= 512,
                "plugin.log line exceeds 512-byte atomic-append cap "
                .. "(got " .. #line .. ")")
        end
    end)

    it("a small message is NOT truncated", function()
        log.warn("short")
        local line = wezterm_log.warn[1]
        local rec = decode_emitted(line)
        assert_eq(rec.msg, "short", "short msg was unexpectedly mutated")
    end)

    it("the cap doesn't drop required structural fields", function()
        local big = string.rep("Y", 2000)
        log.warn(big, { extra = "kept" })
        local rec = decode_emitted(wezterm_log.warn[1])
        assert_eq(rec.level, "warn", "truncate dropped level")
        assert_true(type(rec.ts) == "number", "truncate dropped ts")
        assert_eq(rec.extra, "kept", "truncate dropped caller field")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- wezterm-log prefix per level
-- ────────────────────────────────────────────────────────────────────

describe("wezterm-log sink routing", function()
    it("warn routes to wezterm.log_warn with [wezsesh] prefix",
    function()
        log.warn("warn-line")
        assert_eq(#wezterm_log.warn, 1, "warn did not hit log_warn")
        assert_eq(#wezterm_log.error, 0, "warn leaked to log_error")
        assert_match(wezterm_log.warn[1], "^%[wezsesh%] ",
            "warn line missing [wezsesh] prefix")
    end)

    it("error routes to wezterm.log_error with [wezsesh] prefix",
    function()
        log.error("err-line")
        assert_eq(#wezterm_log.error, 1, "error did not hit log_error")
        assert_eq(#wezterm_log.warn, 0, "error leaked to log_warn")
        assert_match(wezterm_log.error[1], "^%[wezsesh%] ",
            "error line missing [wezsesh] prefix")
    end)

    it("info routes to wezterm.log_info with [wezsesh] prefix",
    function()
        log.info("info-line")
        assert_eq(#wezterm_log.info, 1, "info did not hit log_info")
        assert_match(wezterm_log.info[1], "^%[wezsesh%] ",
            "info line missing [wezsesh] prefix")
    end)

    it("info falls back to wezterm.log_warn when log_info absent",
    function()
        local saved = wezterm_shim.log_info
        wezterm_shim.log_info = nil
        log.info("info-fallback")
        wezterm_shim.log_info = saved
        -- The fallback emits via log_warn so the structured record
        -- still surfaces.
        assert_eq(#wezterm_log.warn, 1,
            "info fallback did not hit log_warn")
        assert_match(wezterm_log.warn[1], "^%[wezsesh%] ",
            "info fallback line missing [wezsesh] prefix")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- file-append leg — <runtime_dir>/plugin.log
-- ────────────────────────────────────────────────────────────────────

describe("plugin.log file-append leg", function()
    it("appends a JSON line to <runtime_dir>/plugin.log on every emit",
    function()
        globals.set_runtime_dir(ensure_tmp_dir())
        globals.set_plugin_session_id("01JFILELEG_______________")
        log.warn("file-1")
        log.error("file-2")
        log.info("file-3")
        local body = read_plugin_log()
        assert_true(body ~= nil, "plugin.log not created")
        -- Three lines (one per emit). Trailing newline counts.
        local lines = {}
        for line in body:gmatch("([^\n]+)") do lines[#lines + 1] = line end
        assert_eq(#lines, 3,
            "expected 3 lines in plugin.log, got " .. #lines)
        local rec1 = codec.decode(lines[1])
        assert_eq(rec1.msg, "file-1", "line 1 msg lost")
        assert_eq(rec1.level, "warn", "line 1 level wrong")
        assert_eq(rec1.plugin_session_id, "01JFILELEG_______________",
            "line 1 plugin_session_id wrong")
        local rec2 = codec.decode(lines[2])
        assert_eq(rec2.level, "error", "line 2 level wrong")
        local rec3 = codec.decode(lines[3])
        assert_eq(rec3.level, "info", "line 3 level wrong")
    end)

    it("plugin.log lines do NOT carry the [wezsesh] prefix",
    function()
        globals.set_runtime_dir(ensure_tmp_dir())
        log.warn("file-prefix-test")
        local body = read_plugin_log()
        assert_true(body ~= nil, "plugin.log not created")
        assert_true(body:find("^%[wezsesh%]") == nil,
            "plugin.log carried the [wezsesh] prefix; "
            .. "tail tool would double-strip")
    end)

    it("absent runtime_dir is a noop on the file leg "
        .. "(wezterm log still gets the line)", function()
        -- Don't set runtime_dir. The wezterm-log leg still fires.
        log.warn("no-runtime-dir")
        assert_eq(#wezterm_log.warn, 1,
            "wezterm log leg dropped on absent runtime_dir")
    end)

    it("a write error on the file leg does NOT propagate", function()
        -- Point runtime_dir at a path we can't write to. The pcall in
        -- the file leg must swallow the error.
        globals.set_runtime_dir("/this/path/should/not/exist/wezsesh")
        local ok = pcall(log.warn, "fs-error")
        assert_true(ok, "log.warn raised on filesystem error")
        assert_eq(#wezterm_log.warn, 1,
            "wezterm log leg dropped despite file leg failing")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- _set / _reset test seam
-- ────────────────────────────────────────────────────────────────────

describe("_set / _reset test seam", function()
    it("_set replaces sinks; the override receives the structured record",
    function()
        local captured = {}
        log._set(
            function(rec) captured.warn = rec end,
            function(rec) captured.error = rec end,
            function(rec) captured.info = rec end)
        log.warn("via-seam-warn", { extra = "w" })
        log.error("via-seam-error")
        log.info("via-seam-info")
        log._reset()

        assert_eq(captured.warn.level, "warn",
            "warn capture missed the structured record")
        assert_eq(captured.warn.msg, "via-seam-warn",
            "warn msg lost in capture")
        assert_eq(captured.warn.extra, "w",
            "warn fields lost in capture")
        assert_eq(captured.error.msg, "via-seam-error",
            "error capture lost")
        assert_eq(captured.info.msg, "via-seam-info",
            "info capture lost")
        -- Captures are STRUCTURED records — the override didn't see
        -- a `[wezsesh] ` string. Substring checks are caller's call.
        assert_eq(type(captured.warn), "table",
            "override should see a Lua table, not a JSON string")
    end)

    it("_reset restores the wezterm-backed defaults", function()
        log._set(
            function() error("must not be called after reset") end,
            nil, nil)
        log._reset()
        log.warn("post-reset")
        assert_eq(#wezterm_log.warn, 1,
            "post-reset emit did not reach wezterm.log_warn")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- pcall discipline — wezterm-log raises must NOT propagate
-- ────────────────────────────────────────────────────────────────────

describe("pcall discipline", function()
    it("a wezterm.log_warn raise is swallowed", function()
        local saved = wezterm_shim.log_warn
        wezterm_shim.log_warn = function() error("synthetic raise") end
        local ok = pcall(log.warn, "swallow-me")
        wezterm_shim.log_warn = saved
        assert_true(ok,
            "log.warn re-raised — would wedge the wezterm event loop")
    end)

    it("a wezterm.log_error raise is swallowed", function()
        local saved = wezterm_shim.log_error
        wezterm_shim.log_error = function() error("synthetic raise") end
        local ok = pcall(log.error, "swallow-me")
        wezterm_shim.log_error = saved
        assert_true(ok, "log.error re-raised")
    end)
end)

-- ────────────────────────────────────────────────────────────────────
-- resolve_level (mirrors Go-side logger.ResolveLevel)
-- ────────────────────────────────────────────────────────────────────

describe("resolve_level", function()
    it("both nil → info", function()
        assert_eq(log.resolve_level(nil, nil), "info")
    end)

    it("both empty → info", function()
        assert_eq(log.resolve_level("", ""), "info")
    end)

    it("opts only → opts", function()
        assert_eq(log.resolve_level("debug", nil), "debug")
        assert_eq(log.resolve_level("warn", ""), "warn")
    end)

    it("env only → env", function()
        assert_eq(log.resolve_level(nil, "debug"), "debug")
        assert_eq(log.resolve_level("", "error"), "error")
    end)

    it("env wins when more verbose than opts", function()
        -- WEZSESH_LOG=debug overrides opts.log_level=warn — env can
        -- only make logging noisier, never quieter.
        assert_eq(log.resolve_level("warn", "debug"), "debug")
        assert_eq(log.resolve_level("error", "info"), "info")
    end)

    it("opts wins when more verbose than env", function()
        assert_eq(log.resolve_level("debug", "warn"), "debug")
    end)

    it("equal levels → that level", function()
        assert_eq(log.resolve_level("info", "info"), "info")
        assert_eq(log.resolve_level("warn", "warn"), "warn")
    end)

    it("case-insensitive matching", function()
        assert_eq(log.resolve_level("DEBUG", nil), "debug")
        assert_eq(log.resolve_level(nil, "Warn"), "warn")
    end)

    it("invalid input falls through", function()
        assert_eq(log.resolve_level("verbose", nil), "info")
        assert_eq(log.resolve_level(nil, "loud"), "info")
        assert_eq(log.resolve_level("verbose", "loud"), "info")
        -- One valid + one invalid → the valid one wins.
        assert_eq(log.resolve_level("warn", "loud"), "warn")
        assert_eq(log.resolve_level("loud", "warn"), "warn")
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
