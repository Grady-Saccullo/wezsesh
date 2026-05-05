package argvallow

import (
	"reflect"
	"strings"
	"testing"
)

// expectedDefault is the canonical 35-entry baseline (§8.13.1). The
// list is duplicated here intentionally — if default.txt drifts from
// the spec, this test catches it without trusting the embed. Order is
// load-bearing (codegen emits in source order).
var expectedDefault = []string{
	"sh", "bash", "zsh", "fish", "dash", "ksh",
	"nvim", "vim", "vi", "emacs", "nano", "helix", "hx",
	"less", "more", "man", "info",
	"git", "jj", "lazygit", "tig",
	"python", "python3", "ipython", "node", "ruby", "irb", "lua",
	"htop", "btop", "top", "k9s", "lazydocker",
	"tmux", "screen",
}

func TestDefault_MatchesSpec(t *testing.T) {
	got := Default()
	if !reflect.DeepEqual(got, expectedDefault) {
		t.Fatalf("Default() drift\n got: %v\nwant: %v", got, expectedDefault)
	}
}

func TestDefault_LengthAndUniqueness(t *testing.T) {
	got := Default()
	if len(got) != 35 {
		t.Fatalf("Default() length = %d, want 35 (§8.13.1)", len(got))
	}
	seen := make(map[string]bool, len(got))
	for _, name := range got {
		if seen[name] {
			t.Fatalf("Default() contains duplicate %q", name)
		}
		seen[name] = true
	}
}

// TestAuditor_AllowlistEnforcement is the §17.3 "Argv allowlist
// enforcement (Go side)" gate: argv[1]="rm" → no exec. The Auditor's
// Allows lookup is what gates the on-pane-restore decision flow on
// the Go-side audit/doctor surface.
func TestAuditor_AllowlistEnforcement(t *testing.T) {
	a := NewAuditor("/bin/bash", nil)
	if a.Allows("rm") {
		t.Fatal("Allows(\"rm\") = true; want false (§17.3 Argv allowlist enforcement)")
	}
	// A few obvious exec-bait names that must remain rejected even
	// when a user adds permissive entries elsewhere.
	for _, danger := range []string{"sh", "rm", "curl", "wget", "ssh", "sudo", "nc"} {
		// "sh" SHOULD be allowed (it is in the default list); the
		// rest must not be.
		if danger == "sh" {
			if !a.Allows(danger) {
				t.Errorf("Allows(%q) = false; want true (in default list)", danger)
			}
			continue
		}
		if a.Allows(danger) {
			t.Errorf("Allows(%q) = true; want false (not in default list)", danger)
		}
	}
}

func TestAuditor_DefaultsAllowed(t *testing.T) {
	a := NewAuditor("", nil)
	for _, name := range expectedDefault {
		if !a.Allows(name) {
			t.Errorf("Allows(%q) = false; want true (default entry)", name)
		}
	}
}

func TestAuditor_UserAdditionsExtend(t *testing.T) {
	a := NewAuditor("", []string{"git", "rg", "fd"})
	// "git" is already in defaults — adding it is a no-op.
	if !a.Allows("git") {
		t.Error("Allows(\"git\") = false; want true")
	}
	// New entries appear.
	for _, name := range []string{"rg", "fd"} {
		if !a.Allows(name) {
			t.Errorf("Allows(%q) = false; want true (user addition)", name)
		}
	}
	// User cannot REMOVE defaults — there is no API to do so;
	// confirm the entries we did not add remain.
	for _, name := range []string{"bash", "zsh", "vim"} {
		if !a.Allows(name) {
			t.Errorf("Allows(%q) = false; user additions must not remove defaults", name)
		}
	}
}

func TestAuditor_ShellBasename(t *testing.T) {
	// shell argument is a path; we should record only the basename.
	a := NewAuditor("/usr/local/bin/xonsh", nil)
	if !a.Allows("xonsh") {
		t.Error("Allows(\"xonsh\") = false; want true (basename($SHELL))")
	}
	if a.Allows("/usr/local/bin/xonsh") {
		t.Error("Allows() accepted full path; want basename-only lookup")
	}
}

func TestAuditor_UserAdditionPathStripped(t *testing.T) {
	a := NewAuditor("", []string{"/usr/bin/rg"})
	if !a.Allows("rg") {
		t.Error("Allows(\"rg\") = false; want true (user addition basename)")
	}
	if a.Allows("/usr/bin/rg") {
		t.Error("Allows() accepted full path on user addition; want basename-only lookup")
	}
}

func TestAuditor_EmptyAndNil(t *testing.T) {
	var nilAuditor *Auditor
	if nilAuditor.Allows("bash") {
		t.Error("nil Auditor.Allows() = true; want false")
	}
	a := NewAuditor("", nil)
	if a.Allows("") {
		t.Error("Allows(\"\") = true; want false")
	}
}

// TestAuditor_LookupConstantTime is a structural assertion: the
// allowed set is map-backed (O(1) average), not slice-scanned. We
// cannot directly observe complexity from a test — but we can confirm
// the unexported field is a map[string]struct{}, which is the load-
// bearing claim from the acceptance gate.
func TestAuditor_LookupConstantTime(t *testing.T) {
	a := NewAuditor("", nil)
	if a.allowed == nil {
		t.Fatal("Auditor.allowed is nil; want map-backed lookup for O(1)")
	}
	// Confirm the type is the expected struct{} value type — a
	// map[string]bool would technically work but the spec/PR comment
	// pins struct{} to make zero-allocation semantics explicit.
	if reflect.TypeOf(a.allowed).Elem().Size() != 0 {
		t.Errorf("Auditor.allowed value size = %d; want 0 (struct{})",
			reflect.TypeOf(a.allowed).Elem().Size())
	}
}

func TestAuditor_List_PreservesOrder(t *testing.T) {
	a := NewAuditor("/bin/bash", []string{"rg"})
	got := a.List()
	// First 35 entries must match defaults exactly.
	for i, name := range expectedDefault {
		if got[i] != name {
			t.Fatalf("List()[%d] = %q; want %q (default order)", i, got[i], name)
		}
	}
	// "bash" was already in defaults; shell add must not duplicate.
	if got[len(expectedDefault)] != "rg" {
		t.Errorf("List() after defaults = %q; want %q (rg, since bash dedup'd)",
			got[len(expectedDefault)], "rg")
	}
	// List returns a copy: mutating it must not affect the Auditor.
	got[0] = "tampered"
	if a.List()[0] != "sh" {
		t.Error("List() returned a shared slice; want defensive copy")
	}
}

// TestParseList_TolerantOfBlankLinesAndComments confirms the parser
// is tolerant. The codegen tool emits no comments / blank lines, so
// no Lua-side surprise is possible from this leniency.
func TestParseList_TolerantOfBlankLinesAndComments(t *testing.T) {
	raw := strings.Join([]string{
		"# leading comment",
		"",
		"sh",
		"  bash  ", // trimmed
		"",
		"# inline comment line",
		"zsh",
		"",
	}, "\n")
	got := parseList(raw)
	want := []string{"sh", "bash", "zsh"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseList tolerant case: got %v, want %v", got, want)
	}
}
