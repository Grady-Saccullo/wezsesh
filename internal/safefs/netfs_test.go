package safefs

import (
	"runtime"
	"testing"
)

// TestIsNetworkFSLocal — the §17.3 "safefs.IsNetworkFS detection" gate.
// On Linux, /tmp is typically tmpfs and must classify as ("tmpfs",
// false). On darwin, /tmp resolves to apfs underneath; the assertion
// degrades to "not network".
//
// The NFS branch is environmental — only assertable when an NFS mount
// is available in the test runner. We don't fail when it's absent.
func TestIsNetworkFSLocal(t *testing.T) {
	t.Parallel()
	fsType, layer, isNet, err := IsNetworkFS("/tmp")
	if err != nil {
		t.Fatalf("IsNetworkFS(/tmp): %v", err)
	}
	if isNet {
		t.Errorf("IsNetworkFS(/tmp): want isNet=false, got true (fsType=%q layer=%q)", fsType, layer)
	}
	if runtime.GOOS == "linux" && fsType != "tmpfs" && fsType != "" {
		// /tmp on linux is usually tmpfs but some distros use a regular
		// FS; accept both as long as not flagged network.
		t.Logf("note: /tmp on linux reports fsType=%q (expected tmpfs on most distros)", fsType)
	}
}

// TestIsNetworkFSResolvesPath — a path that doesn't exist still returns
// without panic; downstream callers can degrade.
func TestIsNetworkFSResolvesPath(t *testing.T) {
	t.Parallel()
	// nonexistent paths surface a Statfs error rather than crashing.
	_, _, _, err := IsNetworkFS("/this/does/not/exist/wezsesh-test")
	// We don't assert err == nil — the kernel will ENOENT and we surface
	// it. The contract is "no panic + sensible value".
	_ = err
}

// TestIsNetworkFSEmptyPath — empty string is a degenerate input; should
// return zero values without panic.
func TestIsNetworkFSEmptyPath(t *testing.T) {
	t.Parallel()
	fsType, layer, isNet, err := IsNetworkFS("")
	if fsType != "" || layer != "" || isNet || err != nil {
		t.Errorf("IsNetworkFS(\"\"): got (%q, %q, %v, %v)", fsType, layer, isNet, err)
	}
}

// TestExpandTilde — covers the ~/foo prefix expansion used by the
// File-Provider prefix matcher.
func TestExpandTilde(t *testing.T) {
	t.Setenv("HOME", "/home/test")
	cases := map[string]string{
		"~":         "/home/test",
		"~/":        "/home/test",
		"~/foo":     "/home/test/foo",
		"~/foo/bar": "/home/test/foo/bar",
		"/abs":      "/abs",
		"./rel":     "./rel",
		"~user/foo": "~user/foo", // ~user expansion intentionally NOT supported
	}
	for in, want := range cases {
		got := expandTilde(in)
		if got != want {
			t.Errorf("expandTilde(%q): got %q want %q", in, got, want)
		}
	}
}
