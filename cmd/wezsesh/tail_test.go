package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// withTailEnv points WEZSESH_*_DIR overrides at a scratch tree so
// subcmdTail's config.LoadFromEnv (now AutoDetect + applyEnvOverrides
// for non-TUI subcommands) resolves to it. The two log files are
// pre-created (empty) so the readers don't bail out on ENOENT.
func withTailEnv(t *testing.T) (stateDir, runtimeDir string) {
	t.Helper()
	root := t.TempDir()
	stateDir = filepath.Join(root, "state")
	runtimeDir = filepath.Join(root, "runtime")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}
	t.Setenv("WEZSESH_SNAPSHOT_DIR", filepath.Join(root, "snap"))
	t.Setenv("WEZSESH_STATE_DIR", stateDir)
	t.Setenv("WEZSESH_RUNTIME_DIR", runtimeDir)
	t.Setenv("WEZSESH_DATA_DIR", filepath.Join(root, "data"))
	return stateDir, runtimeDir
}

// writeLog appends a sequence of pre-encoded JSON lines to path.
func writeLog(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

func mustJSON(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// ──────────────────────────────────────────────────────────────────────
// Parsing
// ──────────────────────────────────────────────────────────────────────

func TestParseTailLine_Binary(t *testing.T) {
	line := []byte(`{"time":"2026-05-08T10:00:00.123Z","level":"WARN","msg":"hmac mismatch","binary_session_id":"01J7PABC","trace_id":"01J7ZXYZ","reason":"replay"}`)
	rec, ok := parseTailLine("binary", line)
	if !ok {
		t.Fatalf("parse failed")
	}
	if rec.Source != "binary" {
		t.Fatalf("source = %q, want binary", rec.Source)
	}
	if rec.Msg != "hmac mismatch" {
		t.Fatalf("msg = %q", rec.Msg)
	}
	if rec.TraceID != "01J7ZXYZ" {
		t.Fatalf("trace_id = %q", rec.TraceID)
	}
	if rec.BinarySessionID != "01J7PABC" {
		t.Fatalf("binary_session_id = %q", rec.BinarySessionID)
	}
	if rec.Time.IsZero() {
		t.Fatalf("time not parsed")
	}
	if got := rec.Raw["reason"].(string); got != "replay" {
		t.Fatalf("raw.reason = %q", got)
	}
}

func TestParseTailLine_Plugin(t *testing.T) {
	line := []byte(`{"ts":1746700800,"level":"warn","msg":"REQ_POINTER_REJECTED","plugin_session_id":"01J8QABC","trace_id":"01J7ZXYZ","binary_session_id":"01J7PABC","pane_id":42}`)
	rec, ok := parseTailLine("plugin", line)
	if !ok {
		t.Fatalf("parse failed")
	}
	if rec.Source != "plugin" {
		t.Fatalf("source = %q, want plugin", rec.Source)
	}
	if rec.PluginSessionID != "01J8QABC" {
		t.Fatalf("plugin_session_id = %q", rec.PluginSessionID)
	}
	if rec.PaneID != "42" {
		t.Fatalf("pane_id = %q, want 42", rec.PaneID)
	}
	want := time.Unix(1746700800, 0)
	if !rec.Time.Equal(want) {
		t.Fatalf("time = %v, want %v", rec.Time, want)
	}
}

func TestParseTailLine_Malformed(t *testing.T) {
	if _, ok := parseTailLine("binary", []byte("not json")); ok {
		t.Fatalf("expected ok=false on malformed JSON")
	}
	// Binary missing time.
	if _, ok := parseTailLine("binary", []byte(`{"level":"info","msg":"x"}`)); ok {
		t.Fatalf("expected ok=false on missing time")
	}
	// Plugin missing ts.
	if _, ok := parseTailLine("plugin", []byte(`{"level":"info","msg":"x"}`)); ok {
		t.Fatalf("expected ok=false on missing ts")
	}
}

// ──────────────────────────────────────────────────────────────────────
// Filtering
// ──────────────────────────────────────────────────────────────────────

func TestTailRecordMatches_Trace(t *testing.T) {
	rec := tailRecord{TraceID: "01J7ZABCDEF"}
	if !tailRecordMatches(rec, tailFlags{Trace: "ZABC"}) {
		t.Fatalf("substring trace match failed")
	}
	if tailRecordMatches(rec, tailFlags{Trace: "NOPE"}) {
		t.Fatalf("non-matching trace passed")
	}
	// Empty filter is a no-op.
	if !tailRecordMatches(rec, tailFlags{}) {
		t.Fatalf("empty trace filter rejected record")
	}
}

func TestTailRecordMatches_Session(t *testing.T) {
	rec := tailRecord{
		BinarySessionID: "01J7PBINBIN",
		PluginSessionID: "01J8QPLUPLU",
	}
	if !tailRecordMatches(rec, tailFlags{Session: "BINBIN"}) {
		t.Fatalf("binary session substring match failed")
	}
	if !tailRecordMatches(rec, tailFlags{Session: "PLUPLU"}) {
		t.Fatalf("plugin session substring match failed")
	}
	if tailRecordMatches(rec, tailFlags{Session: "ZZZ"}) {
		t.Fatalf("non-matching session passed")
	}
}

func TestTailRecordMatches_Level(t *testing.T) {
	rec := tailRecord{Level: "info"}
	if tailRecordMatches(rec, tailFlags{Level: "warn"}) {
		t.Fatalf("info should not match level=warn")
	}
	if !tailRecordMatches(rec, tailFlags{Level: "info"}) {
		t.Fatalf("info should match level=info")
	}
	if !tailRecordMatches(rec, tailFlags{Level: "debug"}) {
		t.Fatalf("info should match level=debug (more permissive)")
	}
}

func TestTailRecordMatches_Since(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	prev := tailNow
	t.Cleanup(func() { tailNow = prev })
	tailNow = func() time.Time { return now }

	old := tailRecord{Time: now.Add(-10 * time.Minute)}
	fresh := tailRecord{Time: now.Add(-1 * time.Minute)}

	if tailRecordMatches(old, tailFlags{Since: 5 * time.Minute}) {
		t.Fatalf("old record should be filtered out")
	}
	if !tailRecordMatches(fresh, tailFlags{Since: 5 * time.Minute}) {
		t.Fatalf("fresh record should pass")
	}
}

// ──────────────────────────────────────────────────────────────────────
// Reader / interleaving
// ──────────────────────────────────────────────────────────────────────

// drainOnce stands up a reader on a single source in --no-follow mode,
// returning every parsed record (in source order). Used as the spine
// for the parsing/rotation tests below.
func drainOnce(t *testing.T, src tailSource) []tailRecord {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan tailRecord, 64)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tailReader(ctx, src, true /* noFollow */, ch, &bytes.Buffer{})
	}()
	go func() {
		wg.Wait()
		close(ch)
	}()
	var out []tailRecord
	for rec := range ch {
		out = append(out, rec)
	}
	return out
}

func TestTailReader_NoFollowDrain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wezsesh.log")
	writeLog(t, path,
		mustJSON(t, map[string]any{"time": "2026-05-08T10:00:00.000Z", "level": "INFO", "msg": "first", "binary_session_id": "BS1"}),
		mustJSON(t, map[string]any{"time": "2026-05-08T10:00:01.000Z", "level": "INFO", "msg": "second", "binary_session_id": "BS1"}),
	)
	got := drainOnce(t, tailSource{Name: "binary", Path: path})
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
	if got[0].Msg != "first" || got[1].Msg != "second" {
		t.Fatalf("messages = %v", []string{got[0].Msg, got[1].Msg})
	}
}

// TestTailReader_RotationDetected: writer writes to file A, we open A,
// drain it, then `os.Rename` A → A.1, create a fresh A with new content,
// and the reader picks up the new file via inode change. Runs in
// follow mode with a context that is cancelled once we've seen both
// the pre-rotation and post-rotation lines.
func TestTailReader_RotationDetected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wezsesh.log")
	writeLog(t, path,
		mustJSON(t, map[string]any{"time": "2026-05-08T10:00:00.000Z", "level": "INFO", "msg": "before", "binary_session_id": "BS1"}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan tailRecord, 16)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tailReader(ctx, tailSource{Name: "binary", Path: path}, false /* follow */, ch, &bytes.Buffer{})
	}()

	// Receive the pre-rotation record.
	first := readWithTimeout(t, ch, 2*time.Second)
	if first.Msg != "before" {
		t.Fatalf("first msg = %q, want before", first.Msg)
	}

	// Simulate rotation: rename → create.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	writeLog(t, path,
		mustJSON(t, map[string]any{"time": "2026-05-08T10:00:01.000Z", "level": "INFO", "msg": "after", "binary_session_id": "BS1"}),
	)

	second := readWithTimeout(t, ch, 5*time.Second)
	if second.Msg != "after" {
		t.Fatalf("second msg = %q, want after", second.Msg)
	}
	cancel()
	wg.Wait()
}

func readWithTimeout(t *testing.T, ch <-chan tailRecord, d time.Duration) tailRecord {
	t.Helper()
	select {
	case rec, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed before record arrived")
		}
		return rec
	case <-time.After(d):
		t.Fatalf("timed out after %s waiting for record", d)
	}
	return tailRecord{}
}

// TestRunTail_NoFollow_Interleaved: the two readers emit records with
// overlapping times; the no-follow drain sorts by time and produces a
// single ordered stream. Force colour off so the output is grep-able.
func TestRunTail_NoFollow_Interleaved(t *testing.T) {
	stateDir, runtimeDir := withTailEnv(t)
	prev := tailColorize
	t.Cleanup(func() { tailColorize = prev })
	tailColorize = func() bool { return false }

	binPath := filepath.Join(stateDir, "wezsesh.log")
	luaPath := filepath.Join(runtimeDir, "plugin.log")

	// Binary at t=10:00:00.500, plugin at t=10:00:00.250 (earlier),
	// binary at t=10:00:01.000, plugin at t=10:00:01.750.
	// Expected order by time: lua@250, bin@500, bin@1000, lua@1750.
	writeLog(t, binPath,
		mustJSON(t, map[string]any{"time": "2026-05-08T10:00:00.500Z", "level": "INFO", "msg": "bin-A", "binary_session_id": "BS1"}),
		mustJSON(t, map[string]any{"time": "2026-05-08T10:00:01.000Z", "level": "INFO", "msg": "bin-B", "binary_session_id": "BS1"}),
	)
	// Plugin uses unix-second granularity; pick distinct seconds so the
	// ordering is deterministic across the two sources.
	luaT1, _ := time.Parse(time.RFC3339, "2026-05-08T10:00:00Z")
	luaT2, _ := time.Parse(time.RFC3339, "2026-05-08T10:00:02Z")
	writeLog(t, luaPath,
		mustJSON(t, map[string]any{"ts": luaT1.Unix(), "level": "info", "msg": "lua-A", "plugin_session_id": "PS1"}),
		mustJSON(t, map[string]any{"ts": luaT2.Unix(), "level": "info", "msg": "lua-B", "plugin_session_id": "PS1"}),
	)

	var out, errBuf bytes.Buffer
	rc := subcmdTail([]string{"--no-follow"}, &out, &errBuf)
	if rc != exitOK {
		t.Fatalf("rc = %d, want %d (stderr=%s)", rc, exitOK, errBuf.String())
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4: %q", len(lines), out.String())
	}
	// Expected order: lua-A (10:00:00), bin-A (10:00:00.500),
	// bin-B (10:00:01), lua-B (10:00:02).
	expectMsg := []string{"lua-A", "bin-A", "bin-B", "lua-B"}
	for i, want := range expectMsg {
		if !strings.Contains(lines[i], want) {
			t.Fatalf("line %d = %q, want substring %q", i, lines[i], want)
		}
	}
}

// TestRunTail_NoFollow_TraceFilter: filter by trace_id substring.
func TestRunTail_NoFollow_TraceFilter(t *testing.T) {
	stateDir, runtimeDir := withTailEnv(t)
	prev := tailColorize
	t.Cleanup(func() { tailColorize = prev })
	tailColorize = func() bool { return false }

	writeLog(t, filepath.Join(stateDir, "wezsesh.log"),
		mustJSON(t, map[string]any{"time": "2026-05-08T10:00:00Z", "level": "INFO", "msg": "matches", "binary_session_id": "BS1", "trace_id": "01J7ZTRACEME"}),
		mustJSON(t, map[string]any{"time": "2026-05-08T10:00:01Z", "level": "INFO", "msg": "skipped", "binary_session_id": "BS1", "trace_id": "01J7ZOTHER"}),
	)
	writeLog(t, filepath.Join(runtimeDir, "plugin.log"),
		mustJSON(t, map[string]any{"ts": 1746700802, "level": "info", "msg": "lua-match", "plugin_session_id": "PS1", "trace_id": "01J7ZTRACEME"}),
	)

	var out, errBuf bytes.Buffer
	rc := subcmdTail([]string{"--no-follow", "--trace", "TRACEME"}, &out, &errBuf)
	if rc != exitOK {
		t.Fatalf("rc = %d (stderr=%s)", rc, errBuf.String())
	}
	if strings.Contains(out.String(), "skipped") {
		t.Fatalf("trace filter let through skipped record: %q", out.String())
	}
	if !strings.Contains(out.String(), "matches") || !strings.Contains(out.String(), "lua-match") {
		t.Fatalf("expected both matching records: %q", out.String())
	}
}

// TestRunTail_NoFollow_JSON: --json emits the original raw map plus a
// `_source` discriminator.
func TestRunTail_NoFollow_JSON(t *testing.T) {
	stateDir, runtimeDir := withTailEnv(t)
	prev := tailColorize
	t.Cleanup(func() { tailColorize = prev })
	tailColorize = func() bool { return false }

	writeLog(t, filepath.Join(stateDir, "wezsesh.log"),
		mustJSON(t, map[string]any{"time": "2026-05-08T10:00:00Z", "level": "INFO", "msg": "x", "binary_session_id": "BS1"}),
	)
	writeLog(t, filepath.Join(runtimeDir, "plugin.log"),
		mustJSON(t, map[string]any{"ts": 1746700801, "level": "info", "msg": "y", "plugin_session_id": "PS1"}),
	)

	var out, errBuf bytes.Buffer
	rc := subcmdTail([]string{"--no-follow", "--json"}, &out, &errBuf)
	if rc != exitOK {
		t.Fatalf("rc = %d (stderr=%s)", rc, errBuf.String())
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), out.String())
	}
	for _, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("invalid JSON: %v (line=%q)", err, line)
		}
		src, ok := m["_source"].(string)
		if !ok || (src != "binary" && src != "plugin") {
			t.Fatalf("missing or bad _source: %q", line)
		}
	}
}

// TestRunTail_NoFollow_NoBinary: --no-binary skips the wezsesh.log
// source; the only output should be the plugin record.
func TestRunTail_NoFollow_NoBinary(t *testing.T) {
	stateDir, runtimeDir := withTailEnv(t)
	prev := tailColorize
	t.Cleanup(func() { tailColorize = prev })
	tailColorize = func() bool { return false }

	writeLog(t, filepath.Join(stateDir, "wezsesh.log"),
		mustJSON(t, map[string]any{"time": "2026-05-08T10:00:00Z", "level": "INFO", "msg": "bin-skipped", "binary_session_id": "BS1"}),
	)
	writeLog(t, filepath.Join(runtimeDir, "plugin.log"),
		mustJSON(t, map[string]any{"ts": 1746700801, "level": "info", "msg": "lua-kept", "plugin_session_id": "PS1"}),
	)

	var out, errBuf bytes.Buffer
	rc := subcmdTail([]string{"--no-follow", "--no-binary"}, &out, &errBuf)
	if rc != exitOK {
		t.Fatalf("rc = %d (stderr=%s)", rc, errBuf.String())
	}
	if strings.Contains(out.String(), "bin-skipped") {
		t.Fatalf("--no-binary leaked binary record: %q", out.String())
	}
	if !strings.Contains(out.String(), "lua-kept") {
		t.Fatalf("expected plugin record: %q", out.String())
	}
}

// TestRunTail_NoBothSources_Errors: --no-binary + --no-plugin together
// is a usage error.
func TestRunTail_NoBothSources_Errors(t *testing.T) {
	withTailEnv(t)
	prev := tailColorize
	t.Cleanup(func() { tailColorize = prev })
	tailColorize = func() bool { return false }

	var out, errBuf bytes.Buffer
	rc := subcmdTail([]string{"--no-follow", "--no-binary", "--no-plugin"}, &out, &errBuf)
	if rc != exitDoctorOrSubcmd {
		t.Fatalf("rc = %d, want %d", rc, exitDoctorOrSubcmd)
	}
	if !strings.Contains(errBuf.String(), "no sources") {
		t.Fatalf("stderr missing 'no sources': %q", errBuf.String())
	}
}

// TestRunTail_TextFormat_Chips: default text output contains the cyan/
// magenta source chips (when colour is on) or the plain `[bin …]` /
// `[lua …]` tags (when colour is off), plus trace/session chips.
func TestRunTail_TextFormat_Chips(t *testing.T) {
	stateDir, runtimeDir := withTailEnv(t)
	prev := tailColorize
	t.Cleanup(func() { tailColorize = prev })
	tailColorize = func() bool { return false }

	writeLog(t, filepath.Join(stateDir, "wezsesh.log"),
		mustJSON(t, map[string]any{
			"time":              "2026-05-08T10:00:00Z",
			"level":             "WARN",
			"msg":               "HMAC mismatch",
			"binary_session_id": "01J7PBINARYID",
			"trace_id":          "01J7ZTRACEABC",
			"reason":            "replay",
		}),
	)
	writeLog(t, filepath.Join(runtimeDir, "plugin.log"),
		mustJSON(t, map[string]any{
			"ts":                int64(1746700801),
			"level":             "warn",
			"msg":               "REQ_POINTER_REJECTED",
			"plugin_session_id": "01J8QPLUGID",
			"trace_id":          "01J7ZTRACEABC",
			"binary_session_id": "01J7PBINARYID",
		}),
	)

	var out, errBuf bytes.Buffer
	rc := subcmdTail([]string{"--no-follow"}, &out, &errBuf)
	if rc != exitOK {
		t.Fatalf("rc = %d (stderr=%s)", rc, errBuf.String())
	}
	got := out.String()
	wantSubs := []string{
		"[bin ", "[lua ", " W]", "[t:", "bs:", "ps:",
		"HMAC mismatch", "REQ_POINTER_REJECTED",
		"reason=replay",
	}
	for _, s := range wantSubs {
		if !strings.Contains(got, s) {
			t.Fatalf("output missing %q:\n%s", s, got)
		}
	}
	// Colour off → no ANSI escapes leak into the buffer.
	if strings.Contains(got, ansiCyan) || strings.Contains(got, ansiMagenta) {
		t.Fatalf("colour leaked into non-TTY output:\n%s", got)
	}
}

// TestParseTailFlags: thin coverage of the flag parser. Exercises the
// success path, the unknown-level path, and the bad --since path.
func TestParseTailFlags(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		got, err := parseTailFlags([]string{"--trace", "abc", "--session", "def", "--level", "warn", "--since", "5m", "--no-follow", "--json"})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got.Trace != "abc" || got.Session != "def" || got.Level != "warn" {
			t.Fatalf("parsed wrong: %+v", got)
		}
		if got.Since != 5*time.Minute {
			t.Fatalf("since = %v", got.Since)
		}
		if !got.NoFollow || !got.JSON {
			t.Fatalf("flags not set: %+v", got)
		}
	})
	t.Run("unknown level", func(t *testing.T) {
		if _, err := parseTailFlags([]string{"--level", "lol"}); err == nil {
			t.Fatalf("expected error")
		}
	})
	t.Run("bad since", func(t *testing.T) {
		if _, err := parseTailFlags([]string{"--since", "five-minutes"}); err == nil {
			t.Fatalf("expected error")
		}
	})
	t.Run("negative since", func(t *testing.T) {
		if _, err := parseTailFlags([]string{"--since", "-5m"}); err == nil {
			t.Fatalf("expected error")
		}
	})
	t.Run("trailing args rejected", func(t *testing.T) {
		if _, err := parseTailFlags([]string{"extra"}); err == nil {
			t.Fatalf("expected error")
		}
	})
}

// TestRun_TailRouteReaches: the dispatcher in run() routes "tail" to
// subcmdTail. We point withTailEnv at empty log files; --no-follow
// returns rc=0 with empty stdout — the routing aliveness check.
func TestRun_TailRouteReaches(t *testing.T) {
	withTailEnv(t)
	var out, errBuf bytes.Buffer
	rc := run([]string{"tail", "--no-follow"}, &out, &errBuf, testBinarySessionID)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0; stderr=%q", rc, errBuf.String())
	}
}
