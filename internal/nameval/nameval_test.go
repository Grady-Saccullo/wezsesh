package nameval

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// ----------------------------------------------------------------------
// §15.4 — Render-time sanitization
// ----------------------------------------------------------------------

// TestSanitize_SnapshotNamedCSI is the §17.3 "Render-time sanitization"
// gate: a snapshot named "\x1b[2J" must not clear the terminal — its
// rendered form must contain no ESC byte.
func TestSanitize_SnapshotNamedCSI(t *testing.T) {
	in := "\x1b[2J"
	out := SanitizeForDisplay(in)
	if strings.ContainsRune(out, '\x1b') {
		t.Fatalf("ESC byte survived sanitization: %q", out)
	}
	if !utf8.ValidString(out) {
		t.Fatalf("sanitized string is not valid UTF-8: %q", out)
	}
	// ESC must have become U+FFFD; the rest ("[2J") is printable and
	// passes through.
	if !strings.HasPrefix(out, "�") {
		t.Fatalf("expected U+FFFD prefix, got %q", out)
	}
	if !strings.HasSuffix(out, "[2J") {
		t.Fatalf("expected printable suffix [2J, got %q", out)
	}
}

func TestSanitize_StripsAllC0ExceptTab(t *testing.T) {
	for c := 0; c < 0x20; c++ {
		if c == '\t' {
			continue
		}
		in := string(rune(c))
		out := SanitizeForDisplay(in)
		if out != "�" {
			t.Errorf("byte 0x%02X: expected U+FFFD, got %q", c, out)
		}
	}
}

func TestSanitize_PreservesTab(t *testing.T) {
	in := "a\tb"
	if got := SanitizeForDisplay(in); got != in {
		t.Fatalf("tab was modified: in=%q out=%q", in, got)
	}
}

func TestSanitize_StripsDEL(t *testing.T) {
	if got := SanitizeForDisplay("\x7f"); got != "�" {
		t.Fatalf("DEL not replaced: %q", got)
	}
}

func TestSanitize_StripsC1Controls(t *testing.T) {
	for r := rune(0x80); r <= 0x9F; r++ {
		in := string(r)
		out := SanitizeForDisplay(in)
		if out != "�" {
			t.Errorf("U+%04X: expected U+FFFD, got %q", r, out)
		}
	}
}

func TestSanitize_StripsLineSeparators(t *testing.T) {
	for _, r := range []rune{0x2028, 0x2029} {
		in := string(r)
		out := SanitizeForDisplay(in)
		if out != "�" {
			t.Errorf("U+%04X: expected U+FFFD, got %q", r, out)
		}
	}
}

func TestSanitize_ReplacesInvalidUTF8(t *testing.T) {
	in := "abc\xffdef"
	out := SanitizeForDisplay(in)
	if !utf8.ValidString(out) {
		t.Fatalf("output not valid UTF-8: %q", out)
	}
	if out != "abc�def" {
		t.Fatalf("got %q", out)
	}
}

func TestSanitize_PassthroughClean(t *testing.T) {
	in := "hello world — αβγ"
	if SanitizeForDisplay(in) != in {
		t.Fatalf("clean input was modified")
	}
}

func TestSanitize_Deterministic(t *testing.T) {
	in := "\x1b[2J\x07\x7f abc \u0085 \u2028 done"
	a := SanitizeForDisplay(in)
	b := SanitizeForDisplay(in)
	if a != b {
		t.Fatalf("non-deterministic output: %q vs %q", a, b)
	}
}

// ----------------------------------------------------------------------
// §17.3 — Control-char cwd/argv
// ----------------------------------------------------------------------

// TestControlCharCwd is the §17.3 "Control-char `cwd`/argv" byte-clean
// primitive gate. The decision (downgrade-to-no-op) lives in
// argvallow / on_pane_restore, but the test here asserts that
// nameval flags such a cwd as invalid — i.e., the byte-clean primitive
// rejects it, which is what argvallow keys off.
func TestControlCharCwd(t *testing.T) {
	cwd := "/tmp/foo\nrm -rf ~"
	err := ValidateWorkspaceName(cwd)
	if err == nil {
		t.Fatalf("expected ValidationError for cwd=%q, got nil", cwd)
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if ve.Code != CodeIllegalName {
		t.Fatalf("expected code %s, got %s", CodeIllegalName, ve.Code)
	}
}

// ----------------------------------------------------------------------
// §15.1 — Workspace name
// ----------------------------------------------------------------------

func TestValidateWorkspaceName_Length(t *testing.T) {
	// Too short.
	if err := ValidateWorkspaceName(""); err == nil {
		t.Fatal("empty name should fail")
	}
	// At max length (200 bytes).
	if err := ValidateWorkspaceName(strings.Repeat("a", 200)); err != nil {
		t.Fatalf("200-byte name should pass: %v", err)
	}
	// Over max length.
	if err := ValidateWorkspaceName(strings.Repeat("a", 201)); err == nil {
		t.Fatal("201-byte name should fail")
	}
}

func TestValidateWorkspaceName_RejectsControls(t *testing.T) {
	for c := 0; c < 0x20; c++ {
		// §15.1 forbids ALL of NUL/LF/CR/TAB/C0 — every byte 0x00..0x1F.
		in := "ws" + string(rune(c)) + "name"
		if err := ValidateWorkspaceName(in); err == nil {
			t.Errorf("byte 0x%02X: expected rejection", c)
		}
	}
}

func TestValidateWorkspaceName_RejectsDEL(t *testing.T) {
	if err := ValidateWorkspaceName("ws\x7fname"); err == nil {
		t.Fatal("DEL should be rejected")
	}
}

func TestValidateWorkspaceName_RejectsC1(t *testing.T) {
	for r := rune(0x80); r <= 0x9F; r++ {
		in := "ws" + string(r) + "name"
		if err := ValidateWorkspaceName(in); err == nil {
			t.Errorf("U+%04X: expected rejection", r)
		}
	}
}

func TestValidateWorkspaceName_RejectsLineParaSeparator(t *testing.T) {
	for _, r := range []rune{0x2028, 0x2029} {
		in := "ws" + string(r) + "name"
		if err := ValidateWorkspaceName(in); err == nil {
			t.Errorf("U+%04X: expected rejection", r)
		}
	}
}

func TestValidateWorkspaceName_RejectsLeadingTrailingWS(t *testing.T) {
	for _, in := range []string{" foo", "foo ", " foo", "foo "} {
		if err := ValidateWorkspaceName(in); err == nil {
			t.Errorf("%q: expected rejection", in)
		}
	}
}

func TestValidateWorkspaceName_RejectsAllWhitespace(t *testing.T) {
	for _, in := range []string{" ", "   ", " ", "　"} {
		if err := ValidateWorkspaceName(in); err == nil {
			t.Errorf("%q: expected rejection", in)
		}
	}
}

func TestValidateWorkspaceName_RejectsDotAndDoubleDot(t *testing.T) {
	for _, in := range []string{".", ".."} {
		if err := ValidateWorkspaceName(in); err == nil {
			t.Errorf("%q: expected rejection", in)
		}
	}
	// Triple-dot is fine.
	if err := ValidateWorkspaceName("..."); err != nil {
		t.Fatalf("'...' should pass: %v", err)
	}
}

func TestValidateWorkspaceName_RejectsBackslash(t *testing.T) {
	if err := ValidateWorkspaceName(`foo\bar`); err == nil {
		t.Fatal("backslash should be rejected")
	}
}

func TestValidateWorkspaceName_AcceptsPlus(t *testing.T) {
	// '+' is allowed; warning is the caller's job.
	if err := ValidateWorkspaceName("foo+bar"); err != nil {
		t.Fatalf("'+' should be accepted: %v", err)
	}
	if !HasPlusWarning("foo+bar") {
		t.Fatal("HasPlusWarning should be true")
	}
	if HasPlusWarning("foo bar") {
		t.Fatal("HasPlusWarning should be false")
	}
}

func TestValidateWorkspaceName_AcceptsCommonNames(t *testing.T) {
	for _, in := range []string{"main", "foo-bar", "café", "α-project", "项目"} {
		if err := ValidateWorkspaceName(in); err != nil {
			t.Errorf("%q rejected: %v", in, err)
		}
	}
}

func TestValidateWorkspaceName_RejectsInvalidUTF8(t *testing.T) {
	if err := ValidateWorkspaceName("foo\xffbar"); err == nil {
		t.Fatal("invalid UTF-8 should be rejected")
	}
}

func TestValidateWorkspaceName_ErrorFields(t *testing.T) {
	err := ValidateWorkspaceName("")
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if ve.Code != CodeIllegalName {
		t.Errorf("Code = %q, want %q", ve.Code, CodeIllegalName)
	}
	if ve.Field != "name" {
		t.Errorf("Field = %q, want %q", ve.Field, "name")
	}
	if ve.Reason == "" {
		t.Error("Reason is empty")
	}
	// Error() includes all three.
	msg := ve.Error()
	if !strings.Contains(msg, CodeIllegalName) || !strings.Contains(msg, "name") {
		t.Errorf("Error() missing context: %q", msg)
	}
}

// ----------------------------------------------------------------------
// §15.1 — NFC normalisation
// ----------------------------------------------------------------------

func TestNormalizeNFC(t *testing.T) {
	// "café" composed (U+00E9) vs decomposed (U+0065 + U+0301).
	composed := "café"
	decomposed := "café"
	if NormalizeNFC(composed) != composed {
		t.Errorf("composed should be unchanged")
	}
	if NormalizeNFC(decomposed) != composed {
		t.Errorf("decomposed should normalise to composed: got %q", NormalizeNFC(decomposed))
	}
	// NFC is idempotent.
	if NormalizeNFC(NormalizeNFC(decomposed)) != composed {
		t.Error("NFC not idempotent")
	}
}

// ----------------------------------------------------------------------
// §15.2 — Tag rules
// ----------------------------------------------------------------------

func TestValidateTag_Length(t *testing.T) {
	if err := ValidateTag(""); err == nil {
		t.Fatal("empty tag should fail")
	}
	if err := ValidateTag(strings.Repeat("a", 50)); err != nil {
		t.Fatalf("50-byte tag should pass: %v", err)
	}
	if err := ValidateTag(strings.Repeat("a", 51)); err == nil {
		t.Fatal("51-byte tag should fail")
	}
}

func TestValidateTag_RejectsControls(t *testing.T) {
	if err := ValidateTag("foo\nbar"); err == nil {
		t.Fatal("LF tag should fail")
	}
	if err := ValidateTag("foo\tbar"); err == nil {
		t.Fatal("TAB tag should fail")
	}
	if err := ValidateTag("foo\x7fbar"); err == nil {
		t.Fatal("DEL tag should fail")
	}
}

func TestValidateTag_AllowsBackslash(t *testing.T) {
	// §15.2 lists byte rules but not backslash; the workspace-only
	// backslash rule does not apply to tags.
	if err := ValidateTag(`a\b`); err != nil {
		t.Errorf("backslash should be allowed in tags: %v", err)
	}
}

func TestValidateTag_AllowsDotAndDoubleDot(t *testing.T) {
	// §15.2 doesn't list "."/".." rejection — workspace-only.
	for _, in := range []string{".", ".."} {
		if err := ValidateTag(in); err != nil {
			t.Errorf("%q tag rejected: %v", in, err)
		}
	}
}

func TestValidateTags_Count(t *testing.T) {
	if err := ValidateTags(nil); err == nil {
		t.Fatal("empty tag list should fail (min count 1)")
	}
	if err := ValidateTags([]string{}); err == nil {
		t.Fatal("empty tag list should fail")
	}
	tags := make([]string, 10)
	for i := range tags {
		tags[i] = "t"
	}
	if err := ValidateTags(tags); err != nil {
		t.Fatalf("10 tags should pass: %v", err)
	}
	tags = append(tags, "extra")
	if err := ValidateTags(tags); err == nil {
		t.Fatal("11 tags should fail")
	}
}

func TestValidateTags_FieldIndexing(t *testing.T) {
	tags := []string{"ok1", "ok2", "ba\nd"}
	err := ValidateTags(tags)
	if err == nil {
		t.Fatal("bad tag at index 2 should fail")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if ve.Field != "tags[2]" {
		t.Errorf("Field = %q, want %q", ve.Field, "tags[2]")
	}
}

// ----------------------------------------------------------------------
// §15.5 — Name truncate algorithm (middle mode)
// ----------------------------------------------------------------------

func TestTruncateMiddle_NoTruncationNeeded(t *testing.T) {
	in := "abc"
	if got := TruncateMiddle(in, 10); got != in {
		t.Errorf("got %q, want %q", got, in)
	}
}

func TestTruncateMiddle_BasicMiddle(t *testing.T) {
	// 11 ASCII chars; width 7 → ellipsis (1) + 6 cells split 3/3.
	in := "abcdefghijk"
	got := TruncateMiddle(in, 7)
	want := "abc…ijk"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTruncateMiddle_OddBudget(t *testing.T) {
	// budget=5 → prefix=2, suffix=3 (suffix gets the extra).
	in := "abcdefghij"
	got := TruncateMiddle(in, 6)
	want := "ab…hij"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTruncateMiddle_Deterministic(t *testing.T) {
	in := "the quick brown fox jumps over"
	a := TruncateMiddle(in, 12)
	b := TruncateMiddle(in, 12)
	if a != b {
		t.Fatalf("non-deterministic: %q vs %q", a, b)
	}
}

func TestTruncateMiddle_WidthSmallerThanEllipsis(t *testing.T) {
	if got := TruncateMiddle("abcdef", 1); got != "a" {
		t.Errorf("got %q, want %q", got, "a")
	}
	if got := TruncateMiddle("abcdef", 0); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestTruncateMiddle_WideRunes(t *testing.T) {
	// Each CJK rune is 2 cells. "项目设置文件" = 12 cells.
	in := "项目设置文件"
	if got := TruncateMiddle(in, 12); got != in {
		t.Errorf("no-trunc case: got %q, want %q", got, in)
	}
	// width=7 → ellipsis(1) + budget(6) → prefix=3 cells, suffix=3 cells.
	// Each rune is 2 cells, so 3 cells means 1 rune.
	got := TruncateMiddle(in, 7)
	want := "项…件"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTruncateMiddle_BoundaryCellAccounting(t *testing.T) {
	// Cell width of result must never exceed width.
	cases := []struct {
		in    string
		width int
	}{
		{"abcdefghij", 5},
		{"abcdefghij", 6},
		{"abcdefghij", 7},
		{"项目设置文件", 5},
		{"项目设置文件", 8},
		{"项目设置文件", 11},
		{"a项b目c", 4},
	}
	for _, tc := range cases {
		got := TruncateMiddle(tc.in, tc.width)
		gotW := stringCellWidth(got)
		if gotW > tc.width {
			t.Errorf("TruncateMiddle(%q, %d) = %q (width=%d), exceeds %d",
				tc.in, tc.width, got, gotW, tc.width)
		}
	}
}

// stringCellWidth measures cell width via go-runewidth, matching the
// production implementation's measurement basis.
func stringCellWidth(s string) int { return runewidth.StringWidth(s) }
