package canonicaljson

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenInputs maps each fixture name in testdata/golden/ to the Go
// value the encoder MUST encode to byte-equal the file's contents.
//
// The §17.1 rendering of the corpus in docs/design.md prints the raw
// bytes for U+0000, U+007F, U+2028, U+2029 in the "expected" column —
// that is a markdown rendering artifact. §4.1 rule 4 is normative and
// requires \u00xx / \uxxxx escapes; the on-disk fixtures here follow
// §4.1. See "Accepted findings" in the task report.
var goldenInputs = map[string]any{
	// §17.1 base corpus.
	"empty_object":   map[string]any{},
	"empty_array":    []any{},
	"empty_string":   "",
	"nul_in_string":  "\x00",
	"del_byte":       "\x7f",
	"ls_ps":          "  ",
	"multibyte_utf8": "café",
	"cjk":            "日本語",
	"emoji":          "\U0001f980",
	"nested_3deep": map[string]any{
		"a": map[string]any{
			"b": map[string]any{"c": int64(1)},
		},
	},
	"mixed_array":   []any{int64(1), "x", true, Null},
	"int_min":       int64(math.MinInt64),
	"int_max":       int64(math.MaxInt64),
	"int_zero":      int64(0),
	"neg_one":       int64(-1),
	"boolean_true":  true,
	"explicit_null": Null,
	"forward_slash": "a/b",

	// Per-verb fixtures.
	"verb_switch_args": map[string]any{"name": "work", "cwd": "/home/user/code"},
	"verb_load_args":   map[string]any{"name": "work"},
	"verb_save_args": map[string]any{
		"name": "work", "overwrite": false, "expected_hash": "sha256:dead",
	},
	"verb_save_args_first": map[string]any{
		"name": "work", "overwrite": false, "expected_hash": Null,
	},
	"verb_new_args": map[string]any{
		"name": "~/code", "cwd": "/home/user/code",
	},
	"verb_noop_args":      map[string]any{},
	"verb_bootstrap_args": map[string]any{},
	"verb_bootstrap_reply_data": map[string]any{
		"colors": map[string]any{
			"accent":   "#ff8800",
			"focus_bg": "#222222",
			"muted":    "#888888",
		},
		"columns":                       []any{"marker", "name", "tabs", "age", "tags"},
		"confirm_delete":                true,
		"confirm_overwrite":             false,
		"data_dir":                      "/var/data",
		"default_action":                "switch",
		"default_action_load_no_prompt": true,
		"dir_providers": []any{
			map[string]any{
				"type":       "command",
				"argv":       []any{"zoxide", "query", "-l"},
				"limit":      int64(200),
				"timeout_ms": int64(5000),
			},
			map[string]any{
				"type":           "directory",
				"path":           "/home/user/code",
				"depth":          int64(2),
				"limit":          int64(200),
				"include_hidden": false,
			},
			map[string]any{
				"type":  "static",
				"paths": []any{"/tmp", "/etc"},
			},
		},
		"exclude":                       []any{"^default$", "^skip$"},
		"hooks": map[string]any{
			"prompt_on_untrusted": false,
			"run_hooks":           true,
			"timeout_seconds":     int64(600),
		},
		"keys": map[string]any{
			"bottom": "G", "clear_marks": "c", "delete": "d",
			"down": "j", "filter": "/", "help": "?",
			"load": "l", "mark": "Tab", "mark_alt": "Space",
			"new": "n", "pin": "p", "quit": "q",
			"rename": "r", "save": "S", "switch": "s",
			"tag": "t", "top": "gg", "up": "k",
		},
		"log_level": "info",
		"markers": map[string]any{
			"active":  "▶",
			"live":    "●",
			"marked":  "✓",
			"pinned":  "[pinned]",
			"unsaved": "(unsaved)",
		},
		"name_truncate":         "middle",
		"new_workspace_command": "tmux new",
		"plugin_version":        "0.1.0",
		"preview": map[string]any{
			"enabled": true,
			"width":   int64(40),
		},
		"resurrect_argv_allowlist": []any{"bash", "zsh", "nvim"},
		"runtime_dir":              "/tmp/wezsesh",
		"snapshot_dir":             "/var/snap",
		"sort":                     "live_first",
		"state_dir":                "/var/state",
	},
	"verb_bootstrap_reply_data_empty": map[string]any{
		"colors":                        map[string]any{},
		"columns":                       []any{},
		"confirm_delete":                false,
		"confirm_overwrite":             false,
		"data_dir":                      "",
		"default_action":                "",
		"default_action_load_no_prompt": false,
		"dir_providers":                 []any{},
		"exclude":                       []any{},
		"hooks": map[string]any{
			"prompt_on_untrusted": false,
			"run_hooks":           false,
			"timeout_seconds":     int64(0),
		},
		"keys":                     map[string]any{},
		"log_level":                "",
		"markers":                  map[string]any{},
		"name_truncate":            "",
		"new_workspace_command":    Null,
		"plugin_version":           "",
		"preview": map[string]any{
			"enabled": false,
			"width":   int64(0),
		},
		"resurrect_argv_allowlist": []any{},
		"runtime_dir":              "",
		"snapshot_dir":             "",
		"sort":                     "",
		"state_dir":                "",
	},
	// Reply-shape fixtures. The `hmac` field is a stable 64-zero string —
	// the corpus tests encoder byte-equality for the reply shapes that
	// the Lua-side signer now emits, not HMAC correctness.
	//
	// `binary_session_id` and `plugin_session_id` are 26-char ULID
	// placeholders distinct from `id` so a human eyeballing the file
	// can tell them apart; they ride along under the canonical sorted-
	// key order.
	"reply_started": map[string]any{
		"v":                 int64(2),
		"id":                "01JABCDEFGHJKMNPQRSTVWXYZA",
		"status":            "started",
		"ok":                true,
		"binary_session_id": "01JABCDEFGHJKMNPQRSTVWXYZB",
		"plugin_session_id": "01JABCDEFGHJKMNPQRSTVWXYZC",
		"hmac":              "0000000000000000000000000000000000000000000000000000000000000000",
	},
	"reply_completed_ok": map[string]any{
		"v":                 int64(2),
		"id":                "01JABCDEFGHJKMNPQRSTVWXYZA",
		"status":            "completed",
		"ok":                true,
		"data":              map[string]any{"active_workspace": "main"},
		"binary_session_id": "01JABCDEFGHJKMNPQRSTVWXYZB",
		"plugin_session_id": "01JABCDEFGHJKMNPQRSTVWXYZC",
		"hmac":              "0000000000000000000000000000000000000000000000000000000000000000",
	},
	"reply_completed_error": map[string]any{
		"v":                 int64(2),
		"id":                "01JABCDEFGHJKMNPQRSTVWXYZA",
		"status":            "completed",
		"ok":                false,
		"binary_session_id": "01JABCDEFGHJKMNPQRSTVWXYZB",
		"plugin_session_id": "01JABCDEFGHJKMNPQRSTVWXYZC",
		"error": map[string]any{
			"code":    "SAVE_FAILED",
			"message": "disk full",
			"details": map[string]any{"raw_error": "ENOSPC"},
		},
		"hmac": "0000000000000000000000000000000000000000000000000000000000000000",
	},
	"reply_partial": map[string]any{
		"v":                 int64(2),
		"id":                "01JABCDEFGHJKMNPQRSTVWXYZA",
		"status":            "partial",
		"ok":                true,
		"data":              map[string]any{"name": "snap-1"},
		"binary_session_id": "01JABCDEFGHJKMNPQRSTVWXYZB",
		"plugin_session_id": "01JABCDEFGHJKMNPQRSTVWXYZC",
		"warnings": []any{
			map[string]any{
				"code":    "RESURRECT_PARTIAL",
				"message": "some panes failed",
				"details": map[string]any{"raw_error": "spawn failed"},
			},
		},
		"hmac": "0000000000000000000000000000000000000000000000000000000000000000",
	},

	// Full request envelope fixture. Pins the v=2 sorted-key shape end
	// to end so the byte-equality gate covers the request side with the
	// same fidelity it covers the reply side.
	"request_full": map[string]any{
		"v":                 int64(2),
		"id":                "01JABCDEFGHJKMNPQRSTVWXYZA",
		"ts":                int64(1700000000),
		"target_window_id":  int64(1),
		"reply_sock":        "/tmp/x.sock",
		"op":                "noop",
		"args":              map[string]any{},
		"binary_session_id": "01JABCDEFGHJKMNPQRSTVWXYZB",
		"hmac":              "0000000000000000000000000000000000000000000000000000000000000000",
	},
}

// TestGoldenCorpus loads each fixture in testdata/golden/, encodes the
// declared Go input, and asserts byte-equality with the file. CI runs
// this under LC_ALL=C; sort.Strings on a Go string is byte-order so
// the gate is locale-independent.
func TestGoldenCorpus(t *testing.T) {
	dir := filepath.Join("testdata", "golden")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}

	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		base := strings.TrimSuffix(name, ".json")
		seen[base] = true

		input, ok := goldenInputs[base]
		if !ok {
			t.Errorf("golden file %q has no goldenInputs entry", name)
			continue
		}

		want, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("ReadFile(%q): %v", name, err)
			continue
		}

		got, err := Marshal(input)
		if err != nil {
			t.Errorf("Marshal(%q): %v", base, err)
			continue
		}

		if !bytes.Equal(got, want) {
			t.Errorf("golden mismatch for %q\n got: %q\nwant: %q", base, got, want)
		}
	}

	for name := range goldenInputs {
		if !seen[name] {
			t.Errorf("goldenInputs entry %q has no testdata/golden/%s.json", name, name)
		}
	}
}

// TestStability — encoding the same input twice produces the same bytes.
func TestStability(t *testing.T) {
	v := map[string]any{
		"z": int64(3), "a": int64(1), "m": int64(2),
		"nested": map[string]any{"y": int64(2), "b": int64(1)},
	}
	first, err := Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for i := 0; i < 16; i++ {
		got, err := Marshal(v)
		if err != nil {
			t.Fatalf("Marshal[%d]: %v", i, err)
		}
		if !bytes.Equal(first, got) {
			t.Fatalf("instability at iter %d:\nfirst=%q\n got =%q", i, first, got)
		}
	}
}

// TestKeyOrdering — sort.Strings yields unsigned UTF-8 byte order.
func TestKeyOrdering(t *testing.T) {
	cases := []struct {
		in   map[string]any
		want string
	}{
		{
			map[string]any{"b": int64(1), "a": int64(2), "z": int64(3)},
			`{"a":2,"b":1,"z":3}`,
		},
		// ASCII < non-ASCII: any 7-bit byte sorts before a UTF-8 lead
		// byte in 0xC2..0xF4. "é" is 0xc3 0xa9.
		{
			map[string]any{"é": int64(1), "a": int64(2)},
			"{\"a\":2,\"\xc3\xa9\":1}",
		},
		// Empty string key sorts first.
		{
			map[string]any{"a": int64(1), "": int64(2)},
			`{"":2,"a":1}`,
		},
	}
	for _, c := range cases {
		got, err := Marshal(c.in)
		if err != nil {
			t.Errorf("Marshal(%v): %v", c.in, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("got %q; want %q", got, c.want)
		}
	}
}

// TestFloatRejected — every Go float kind must be rejected.
func TestFloatRejected(t *testing.T) {
	cases := []any{
		float32(0),
		float32(1.5),
		float64(0),
		float64(1.0),
		math.NaN(),
		math.Inf(1),
		math.Inf(-1),
		[]any{int64(1), float64(2.0)}, // nested
		map[string]any{"x": float64(1.0)},
	}
	for _, v := range cases {
		_, err := Marshal(v)
		if !errors.Is(err, ErrFloat) {
			t.Errorf("Marshal(%v): want ErrFloat, got %v", v, err)
		}
	}
}

// TestInvalidUTF8 — strings must be valid UTF-8.
func TestInvalidUTF8(t *testing.T) {
	cases := []any{
		"\xff\xfe",       // bare bytes
		"\xc3\x28",       // bad continuation
		"a\xed\xa0\x80b", // unpaired surrogate
		map[string]any{"k": "\xff"},
		[]any{"\xff"},
		// invalid in a key:
		map[string]any{"\xff": int64(1)},
	}
	for _, v := range cases {
		_, err := Marshal(v)
		if !errors.Is(err, ErrInvalidUTF8) {
			t.Errorf("Marshal(%v): want ErrInvalidUTF8, got %v", v, err)
		}
	}
}

// TestEscapeRules — every escape decision in §4.1 rule 4. All `want`
// values use Go double-quoted strings with explicit \xNN bytes, so the
// expected literal bytes survive any source-file rewrite.
func TestEscapeRules(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Literal escapes.
		{"\\", "\"\\\\\""},
		{"\"", "\"\\\"\""},
		{"\\\\\"", "\"\\\\\\\\\\\"\""},

		// Forward slash NEVER escaped (§4.1 rule 4 last bullet).
		{"/", "\"/\""},
		{"a/b/c", "\"a/b/c\""},

		// Forbidden short-form chars must use \u00xx (lowercase).
		{"\x08", "\"\\u0008\""}, // backspace (forbidden \b)
		{"\x09", "\"\\u0009\""}, // tab (forbidden \t)
		{"\x0a", "\"\\u000a\""}, // newline (forbidden \n)
		{"\x0b", "\"\\u000b\""},
		{"\x0c", "\"\\u000c\""}, // form feed (forbidden \f)
		{"\x0d", "\"\\u000d\""}, // CR (forbidden \r)
		{"\x00", "\"\\u0000\""},
		{"\x1f", "\"\\u001f\""},
		{"\x7f", "\"\\u007f\""}, // DEL
		{"\u0080", "\"\\u0080\""},
		{"\u009f", "\"\\u009f\""},

		// LS / PS — full four-hex form (lowercase).
		{" ", "\"\\u2028\""},
		{" ", "\"\\u2029\""},

		// Mixed control + plain.
		{"a\nb", "\"a\\u000ab\""},
		{"a\tb", "\"a\\u0009b\""},

		// Plain ASCII at and just above U+0020 is raw.
		{" ", "\" \""},
		{"!", "\"!\""},

		// 0x20 boundary: 0x1F escapes, 0x20 raw.
		{"\x1f ", "\"\\u001f \""},

		// U+00A0 is above the C1 control range — raw two-byte UTF-8.
		{" ", "\"\xc2\xa0\""},
	}
	for _, c := range cases {
		got, err := Marshal(c.in)
		if err != nil {
			t.Errorf("Marshal(%q): %v", c.in, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("Marshal(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

// TestNumberBoundaries — int64 endpoints and unsigned overflow.
func TestNumberBoundaries(t *testing.T) {
	type tc struct {
		in   any
		want string
		err  error
	}
	cases := []tc{
		{int64(math.MinInt64), "-9223372036854775808", nil},
		{int64(math.MaxInt64), "9223372036854775807", nil},
		{int(0), "0", nil},
		{int8(-128), "-128", nil},
		{int16(32767), "32767", nil},
		{int32(-1), "-1", nil},
		{uint(0), "0", nil},
		{uint64(math.MaxInt64), "9223372036854775807", nil},
		// uint64 above MaxInt64 must error: the wire is signed-only.
		{uint64(math.MaxInt64) + 1, "", ErrIntOverflow},
		{^uint64(0), "", ErrIntOverflow},
	}
	for _, c := range cases {
		got, err := Marshal(c.in)
		if c.err != nil {
			if !errors.Is(err, c.err) {
				t.Errorf("Marshal(%v): want %v, got %v", c.in, c.err, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("Marshal(%v): %v", c.in, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("Marshal(%v) = %s; want %s", c.in, got, c.want)
		}
	}
}

// TestEmptyContainerShape — empty map → {}, empty slice → [], even when
// nil. This is the Go-side analogue of §9.7 verb-aware tagging: shape
// is encoded by Go type.
func TestEmptyContainerShape(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{map[string]any{}, "{}"},
		{[]any{}, "[]"},
		{map[string]any(nil), "{}"},
		{[]any(nil), "[]"},
		{map[string]any{"a": []any{}}, `{"a":[]}`},
		{map[string]any{"a": map[string]any{}}, `{"a":{}}`},
		{[]any{map[string]any{}, []any{}}, `[{},[]]`},
	}
	for _, c := range cases {
		got, err := Marshal(c.in)
		if err != nil {
			t.Errorf("Marshal(%v): %v", c.in, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("Marshal(%v) = %s; want %s", c.in, got, c.want)
		}
	}
}

// TestNullSentinel — Null emits null; bare nil is rejected.
func TestNullSentinel(t *testing.T) {
	got, err := Marshal(Null)
	if err != nil {
		t.Fatalf("Marshal(Null): %v", err)
	}
	if string(got) != "null" {
		t.Fatalf("Marshal(Null) = %s; want null", got)
	}

	if _, err := Marshal(nil); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Marshal(nil): want ErrUnsupported, got %v", err)
	}
}

// TestUnsupported — types we cannot represent.
func TestUnsupported(t *testing.T) {
	type S struct{ X int }
	cases := []any{
		make(chan int),
		func() {},
		complex64(0),
		complex128(0),
		S{X: 1}, // no struct support
		map[int]any{1: "x"},
	}
	for _, v := range cases {
		_, err := Marshal(v)
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("Marshal(%T): want ErrUnsupported, got %v", v, err)
		}
	}
}

// TestRequestEnvelopeFieldOrder — §3.3 mandates the exact key order
// (args, binary_session_id, hmac, id, op, reply_sock, target_window_id,
// ts, v). This test exercises the canonical encoder against a realistic
// request envelope at v=2.
func TestRequestEnvelopeFieldOrder(t *testing.T) {
	req := map[string]any{
		"v":                 int64(2),
		"id":                "01JABCDEFGHJKMNPQRSTVWXYZA",
		"ts":                int64(1700000000),
		"target_window_id":  int64(1),
		"reply_sock":        "/tmp/x.sock",
		"op":                "noop",
		"args":              map[string]any{},
		"binary_session_id": "01JABCDEFGHJKMNPQRSTVWXYZB",
		"hmac":              "deadbeef",
	}
	got, err := Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"args":{},"binary_session_id":"01JABCDEFGHJKMNPQRSTVWXYZB","hmac":"deadbeef","id":"01JABCDEFGHJKMNPQRSTVWXYZA","op":"noop","reply_sock":"/tmp/x.sock","target_window_id":1,"ts":1700000000,"v":2}`
	if string(got) != want {
		t.Fatalf("envelope canonical order wrong\n got: %s\nwant: %s", got, want)
	}
}
