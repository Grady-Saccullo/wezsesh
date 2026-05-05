package lualint

import "testing"

// TestIsAsync is the table-driven gate for the §14.3 async-function
// registry. Adding/removing entries from asyncFuncs MUST land alongside
// a row update here.
func TestIsAsync(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// §14.3 explicit enumeration.
		{"wezterm.run_child_process", true},
		{"wezterm.sleep_ms", true},

		// Unknown / unrelated names return false. (The broader
		// add_async_function-exposed surface is enumerated by T-600.)
		{"wezterm.read_dir", false},
		{"wezterm.glob", false},
		{"wezterm.format", false},
		{"print", false},
		{"", false},
	}
	for _, tc := range cases {
		got := IsAsync(tc.name)
		if got != tc.want {
			t.Errorf("IsAsync(%q) = %v want %v", tc.name, got, tc.want)
		}
	}
}

// TestAsyncFuncsConsistent ensures the slice form matches the map.
// Tooling that prints the registry depends on this.
func TestAsyncFuncsConsistent(t *testing.T) {
	got := AsyncFuncs()
	if len(got) != len(asyncFuncs) {
		t.Fatalf("AsyncFuncs len %d, asyncFuncs len %d", len(got), len(asyncFuncs))
	}
	for _, name := range got {
		if !asyncFuncs[name] {
			t.Errorf("AsyncFuncs returned %q, but it is not in asyncFuncs", name)
		}
	}
}
