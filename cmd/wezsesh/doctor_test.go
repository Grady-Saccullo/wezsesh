package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doctorTestEnv builds a self-contained config tree under t.TempDir()
// and pins WEZSESH_CONFIG_FILE so subcmdDoctor's config.LoadFromEnv
// path resolves to it. Mirrors resetTestEnv in style.
type doctorTestEnv struct {
	dir         string
	stateDir    string
	dataDir     string
	trustDir    string
	runtimeDir  string
	snapshotDir string
}

func newDoctorTestEnv(t *testing.T) *doctorTestEnv {
	t.Helper()
	dir := t.TempDir()
	env := &doctorTestEnv{
		dir:         dir,
		stateDir:    filepath.Join(dir, "state"),
		dataDir:     filepath.Join(dir, "data"),
		trustDir:    filepath.Join(dir, "data", "allow"),
		runtimeDir:  filepath.Join(dir, "rt"),
		snapshotDir: filepath.Join(dir, "snap"),
	}
	for _, d := range []string{env.stateDir, env.trustDir, env.runtimeDir, env.snapshotDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	cfgPath := filepath.Join(dir, "wezsesh.json")
	body := []byte(`{
"version":1,
"snapshot_dir":"` + env.snapshotDir + `",
"state_dir":"` + env.stateDir + `",
"runtime_dir":"` + env.runtimeDir + `",
"data_dir":"` + env.dataDir + `"
}`)
	if err := os.WriteFile(cfgPath, body, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("WEZSESH_CONFIG_FILE", cfgPath)
	return env
}

// TestParseDoctorFlags_Defaults pins the default-format contract
// (text) and the round-trip parse for a valid --format value.
func TestParseDoctorFlags_Defaults(t *testing.T) {
	got, err := parseDoctorFlags(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != doctorFormatText {
		t.Fatalf("default format = %q, want %q", got, doctorFormatText)
	}
}

func TestParseDoctorFlags_AcceptsJSON(t *testing.T) {
	got, err := parseDoctorFlags([]string{"--format", "json"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != doctorFormatJSON {
		t.Fatalf("got %q, want %q", got, doctorFormatJSON)
	}
}

// TestParseDoctorFlags_RejectsUnknown asserts an unknown --format value
// surfaces as a parse error (the cmd-layer translates that to a
// stderr message + exit 2).
func TestParseDoctorFlags_RejectsUnknown(t *testing.T) {
	_, err := parseDoctorFlags([]string{"--format", "yaml"})
	if err == nil {
		t.Fatalf("expected error for unknown format")
	}
	if !strings.Contains(err.Error(), "unknown --format") {
		t.Fatalf("error missing format marker: %v", err)
	}
}

// TestParseDoctorFlags_RejectsPositional rejects trailing positional
// args. §8.20 lists no positional args for `doctor`.
func TestParseDoctorFlags_RejectsPositional(t *testing.T) {
	_, err := parseDoctorFlags([]string{"extra"})
	if err == nil {
		t.Fatalf("expected error for positional arg")
	}
}

// TestSubcmdDoctor_UnknownFormat asserts an unknown --format returns
// non-zero and writes to stderr (the §8.20 surface rule).
func TestSubcmdDoctor_UnknownFormat(t *testing.T) {
	newDoctorTestEnv(t)
	var stdout, stderr bytes.Buffer
	rc := subcmdDoctor([]string{"--format", "yaml"}, &stdout, &stderr)
	if rc != exitDoctorOrSubcmd {
		t.Fatalf("rc = %d, want %d", rc, exitDoctorOrSubcmd)
	}
	if stderr.Len() == 0 {
		t.Fatalf("expected non-empty stderr")
	}
	if !strings.Contains(stderr.String(), "wezsesh doctor:") {
		t.Fatalf("stderr missing prefix: %q", stderr.String())
	}
}

// TestSubcmdDoctor_ConfigLoadFails asserts that a missing
// WEZSESH_CONFIG_FILE returns a non-zero exit code with a clear
// stderr message — doctor is a diagnostic, so config resolution
// failure is reported rather than silently coerced into a synthetic
// empty Env.
func TestSubcmdDoctor_ConfigLoadFails(t *testing.T) {
	t.Setenv("WEZSESH_CONFIG_FILE", filepath.Join(t.TempDir(), "missing.json"))
	var stdout, stderr bytes.Buffer
	rc := subcmdDoctor(nil, &stdout, &stderr)
	if rc != exitDoctorOrSubcmd {
		t.Fatalf("rc = %d, want %d", rc, exitDoctorOrSubcmd)
	}
	if !strings.Contains(stderr.String(), "wezsesh doctor: config:") {
		t.Fatalf("stderr missing config-error marker: %q", stderr.String())
	}
}

// TestSubcmdDoctor_JSONParseable asserts that --format json produces
// JSON that parses back into the doctor.Report shape (Checks,
// Critical, Warnings).
//
// The test does NOT force a green run (the cmd-layer cannot reach
// internal/doctor's package-private seams); instead we run against a
// real but stub config tree and assert the output's STRUCTURE is
// correct. The exit code follows the report: rc=0 iff !Critical &&
// !Warnings, rc=2 otherwise — both branches are valid signals here
// and the rc-vs-report invariant is checked below.
func TestSubcmdDoctor_JSONParseable(t *testing.T) {
	newDoctorTestEnv(t)
	var stdout, stderr bytes.Buffer
	rc := subcmdDoctor([]string{"--format", "json"}, &stdout, &stderr)

	// Report structure: parse it back into a struct shaped like
	// doctor.Report. Using inline anonymous structs keeps the test
	// independent of any future field additions to the real struct
	// (a forward-compat check would otherwise fail spuriously).
	type checkLike struct {
		ID      string         `json:"ID"`
		Status  string         `json:"Status"`
		Message string         `json:"Message"`
		Details map[string]any `json:"Details"`
	}
	type reportLike struct {
		Checks   []checkLike `json:"Checks"`
		Critical bool        `json:"Critical"`
		Warnings bool        `json:"Warnings"`
	}
	var got reportLike
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal report: %v\nstdout: %s\nstderr: %s",
			err, stdout.String(), stderr.String())
	}
	if len(got.Checks) == 0 {
		t.Fatalf("report has no checks; stdout=%s", stdout.String())
	}
	for i, c := range got.Checks {
		if c.ID == "" {
			t.Errorf("check[%d].ID empty: %+v", i, c)
		}
		switch c.Status {
		case "ok", "warn", "fail", "skip":
		default:
			t.Errorf("check[%d].Status = %q; want one of ok/warn/fail/skip", i, c.Status)
		}
	}

	// Exit-code rule: rc==0 iff every check is OK (no FAIL, no WARN).
	// Compute the expected rc from the parsed report and assert.
	wantRC := exitOK
	if got.Critical || got.Warnings {
		wantRC = exitDoctorOrSubcmd
	}
	if rc != wantRC {
		t.Fatalf("rc = %d, want %d (Critical=%v Warnings=%v)",
			rc, wantRC, got.Critical, got.Warnings)
	}

	// Cross-check Critical / Warnings against the per-check statuses.
	var sawFail, sawWarn bool
	for _, c := range got.Checks {
		if c.Status == "fail" {
			sawFail = true
		}
		if c.Status == "warn" {
			sawWarn = true
		}
	}
	if sawFail != got.Critical {
		t.Errorf("Critical = %v but sawFail = %v", got.Critical, sawFail)
	}
	if sawWarn != got.Warnings {
		t.Errorf("Warnings = %v but sawWarn = %v", got.Warnings, sawWarn)
	}
}

// TestSubcmdDoctor_TextFormat asserts --format text (the default)
// produces non-empty stdout with the expected per-check shape and a
// trailing summary line. Exit code follows the same Critical||Warnings
// rule.
func TestSubcmdDoctor_TextFormat(t *testing.T) {
	newDoctorTestEnv(t)
	var stdout, stderr bytes.Buffer
	rc := subcmdDoctor(nil, &stdout, &stderr)
	if stdout.Len() == 0 {
		t.Fatalf("expected non-empty stdout; stderr: %s", stderr.String())
	}
	out := stdout.String()
	// Trailing summary line ("N checks, …") MUST be present.
	if !strings.Contains(out, "checks,") {
		t.Fatalf("stdout missing summary line: %q", out)
	}
	// Each line should start with one of OK/WARN/FAIL/SKIP (uppercase).
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least one check line + summary; got %d lines", len(lines))
	}
	for _, ln := range lines[:len(lines)-1] {
		ok := false
		for _, p := range []string{"OK", "WARN", "FAIL", "SKIP"} {
			if strings.HasPrefix(ln, p) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("line missing status prefix: %q", ln)
		}
	}
	// rc is the Critical||Warnings exit; we don't pin a specific value
	// here because the underlying environment may legitimately produce
	// any of the four statuses. The JSON test above already asserts
	// the rc-vs-report cross-check.
	_ = rc
}
