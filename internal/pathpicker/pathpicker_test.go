package pathpicker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// shellPrintf returns a sh -c command line that prints the given lines
// verbatim (each followed by a newline). Bytes that have shell-syntax
// significance must not appear in the lines unless intended.
func shellPrintf(lines ...string) string {
	// Use printf %s to avoid backslash interpretation issues; we feed it
	// pre-built strings that already include the desired byte patterns.
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = "'" + strings.ReplaceAll(l, "'", "'\\''") + "'"
	}
	return "printf '%s\\n' " + strings.Join(parts, " ")
}

func TestResolve_HappyPath_SingleValidDir(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := Resolve(ctx, shellPrintf(tmp))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 || got[0] != tmp {
		t.Fatalf("got %v, want [%q]", got, tmp)
	}
}

func TestResolve_EmptyOutput_ReturnsZeroResults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := Resolve(ctx, "true")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected zero results, got %v", got)
	}
}

func TestResolve_MixedValidAndInvalid_DropsBadLines(t *testing.T) {
	tmp := t.TempDir()
	otherDir := filepath.Join(tmp, "other")
	if err := os.Mkdir(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(tmp, "regular-file")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Build the §15.3 sad-path corpus on disk to avoid argv encoding
	// issues (NUL bytes can't go through fork/exec argv) and to keep the
	// test agnostic to the host shell's printf escape semantics.
	corpus := strings.Join([]string{
		"",                       // empty (blank)
		tmp,                      // valid: directory
		"relative/path",          // not absolute
		"/definitely/does/not/exist/T700-pathpicker", // stat fails
		filePath,                 // exists but is a file
		"~bogususer/foo",         // ~user form unsupported
		"\xff\xfe/bad",           // invalid UTF-8
		otherDir,                 // valid: directory
		"prefix\x00/with-nul",    // NUL byte present
		"valid-with-cr-stripped", // not absolute (after \r strip)
		tmp + "\r",               // trailing \r → strips back to a valid dir
	}, "\n") + "\n"

	corpusPath := filepath.Join(tmp, "corpus.txt")
	if err := os.WriteFile(corpusPath, []byte(corpus), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := Resolve(ctx, "cat "+corpusPath)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Expect: tmp, otherDir, tmp (the trailing-\r case strips back to tmp).
	want := []string{tmp, otherDir, tmp}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d]=%q want %q (full got=%v)", i, got[i], want[i], got)
		}
	}
}

func TestResolve_TildeExpansion_HappyPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir available: %v", err)
	}
	if _, err := os.Stat(home); err != nil {
		t.Skipf("home dir not statable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := Resolve(ctx, shellPrintf("~"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 || got[0] != home {
		t.Fatalf("got %v, want [%q]", got, home)
	}
}

func TestResolve_SymlinkedDir_Accepted(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := Resolve(ctx, shellPrintf(link))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 || got[0] != link {
		t.Fatalf("got %v, want [%q]", got, link)
	}
}

func TestResolve_NoPathProvider(t *testing.T) {
	// Strip PATH so neither zoxide nor fd can be located.
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Resolve(ctx, "")
	if !errors.Is(err, ErrNoPathProvider) {
		t.Fatalf("got %v, want ErrNoPathProvider", err)
	}
}

func TestResolve_Timeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Resolve(ctx, "sleep 5")
	elapsed := time.Since(start)
	if !errors.Is(err, ErrPathPickerTimeout) {
		t.Fatalf("got %v, want ErrPathPickerTimeout", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Resolve did not honor timeout (took %v)", elapsed)
	}
}

func TestResolve_CommandFailed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Resolve(ctx, "false")
	if !errors.Is(err, ErrPathPickerCmdFailed) {
		t.Fatalf("got %v, want ErrPathPickerCmdFailed", err)
	}
}

func TestResolve_EnvFilter_DropsSensitive_KeepsOthers(t *testing.T) {
	t.Setenv("WEZSESH_HMAC_KEY", "should-be-dropped")
	t.Setenv("WEZSESH_LOG", "should-survive")
	// Non-sensitive vars (WEZSESH_RUNTIME_DIR / WEZSESH_PLUGIN_VERSION
	// after C4) MUST flow through to the hook child — they're paths /
	// metadata, not secrets.
	t.Setenv("WEZSESH_RUNTIME_DIR", "/tmp/wezsesh-test")
	t.Setenv("WEZSESH_PLUGIN_VERSION", "0.1.0")

	tmp := t.TempDir()
	envOut := filepath.Join(tmp, "env.txt")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Dump the environment to a file, then emit the tmp dir so Resolve
	// returns at least one valid path (so we can assert err == nil cleanly).
	cmd := "env > " + envOut + "; printf '%s\\n' " + tmp
	got, err := Resolve(ctx, cmd)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 || got[0] != tmp {
		t.Fatalf("got %v, want [%q]", got, tmp)
	}

	envBytes, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	envStr := string(envBytes)

	if strings.Contains(envStr, "WEZSESH_HMAC_KEY=") {
		t.Fatalf("sensitive key WEZSESH_HMAC_KEY leaked into child env:\n%s", envStr)
	}
	for _, want := range []string{
		"WEZSESH_LOG=should-survive",
		"WEZSESH_RUNTIME_DIR=/tmp/wezsesh-test",
		"WEZSESH_PLUGIN_VERSION=0.1.0",
	} {
		if !strings.Contains(envStr, want) {
			t.Fatalf("expected non-sensitive %q in child env; env dump:\n%s", want, envStr)
		}
	}
}

func TestFilterEnv_PreservesNonEqualsEntries(t *testing.T) {
	in := []string{
		"FOO=1",
		"WEZSESH_HMAC_KEY=secret",
		"NO_EQUALS_SIGN", // pathological; preserve verbatim
		"WEZSESH_LOG=info",
	}
	out := filterEnv(in)
	want := []string{"FOO=1", "NO_EQUALS_SIGN", "WEZSESH_LOG=info"}
	if len(out) != len(want) {
		t.Fatalf("got %v, want %v", out, want)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("out[%d]=%q want %q", i, out[i], want[i])
		}
	}
}

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir available: %v", err)
	}
	cases := []struct {
		in        string
		wantOK    bool
		wantValue string
	}{
		{"", true, ""},
		{"/abs/path", true, "/abs/path"},
		{"~", true, home},
		{"~/sub/dir", true, filepath.Join(home, "sub/dir")},
		{"~user/foo", false, ""},
		{"~bogus", false, ""},
	}
	for _, tc := range cases {
		got, ok := expandTilde(tc.in)
		if ok != tc.wantOK {
			t.Errorf("expandTilde(%q): ok=%v want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if ok && got != tc.wantValue {
			t.Errorf("expandTilde(%q): got %q want %q", tc.in, got, tc.wantValue)
		}
	}
}

// Sanity that we never accidentally reach for the bubbletea API or any
// platform we don't support — pathpicker is pure Go + unix syscall.
func TestPlatform_UnixOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Fatal("pathpicker assumes unix-style /bin/sh and Setpgid; windows path not supported")
	}
}
