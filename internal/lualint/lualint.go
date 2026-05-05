package lualint

import (
	"fmt"
	"os"
)

// Severity classifies a Finding. Concrete rules choose the level; the
// Lint runner does not interpret it. The CI driver maps Error to a
// non-zero exit status (§17.4 — "PR fail" / "build error" both surface
// as a failed lint run).
type Severity int

const (
	SevWarning Severity = iota
	SevError
)

// String returns "warning" / "error". Used by CLI output.
func (s Severity) String() string {
	switch s {
	case SevError:
		return "error"
	case SevWarning:
		return "warning"
	}
	return "unknown"
}

// Finding is a single lint diagnostic. Path/Line/Col mirror the source
// position the rule wants to flag (1-based, identical to gopls /
// staticcheck conventions). RuleID is a stable short string the rule
// owns; Message is human-readable and may include rule-specific detail.
type Finding struct {
	Path     string
	Line     int
	Col      int
	RuleID   string
	Severity Severity
	Message  string
}

// String formats the finding in a `path:line:col: [rule] message` form
// compatible with Vim/quickfix and most editor problem matchers.
func (f Finding) String() string {
	return fmt.Sprintf("%s:%d:%d: %s: [%s] %s", f.Path, f.Line, f.Col, f.Severity, f.RuleID, f.Message)
}

// Rule is the contract every concrete lint implements. ID is a stable
// short identifier (e.g. "lua-handler-async") used in Findings and CLI
// suppression flags. Check is invoked once per file with the tokenised
// source; it MUST NOT mutate Tokens. Returning a nil/empty slice means
// "this file is clean for this rule".
type Rule interface {
	ID() string
	Check(t *Tokens) []Finding
}

// RuleFunc adapts a function to the Rule interface for callers that
// don't want to declare a struct (notably tests).
type RuleFunc struct {
	RuleID string
	Fn     func(t *Tokens) []Finding
}

// ID returns the rule's identifier.
func (r RuleFunc) ID() string { return r.RuleID }

// Check delegates to the wrapped function.
func (r RuleFunc) Check(t *Tokens) []Finding {
	if r.Fn == nil {
		return nil
	}
	return r.Fn(t)
}

// Lint reads the file at path, tokenises it, and runs each rule against
// the resulting token stream. Findings from every rule are concatenated
// in (rule-order, source-order) and returned. The error return is
// reserved for I/O failures reading path; rule-side failures surface as
// Findings with SevError (rules don't return errors — they're meant to
// be infallible over a tokenised stream).
//
// The signature is fixed by T-003's acceptance gate. Future rule
// packages plug into this entry point unchanged.
func Lint(path string, rules []Rule) ([]Finding, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("lualint: read %s: %w", path, err)
	}
	toks := Tokenise(path, src)
	var out []Finding
	for _, r := range rules {
		if r == nil {
			continue
		}
		out = append(out, r.Check(toks)...)
	}
	return out, nil
}

// LintBytes is the same as Lint but operates on an in-memory source.
// The package's test suite uses it; the production CLI uses Lint. path
// is recorded in any returned Findings.
func LintBytes(path string, src []byte, rules []Rule) []Finding {
	toks := Tokenise(path, src)
	var out []Finding
	for _, r := range rules {
		if r == nil {
			continue
		}
		out = append(out, r.Check(toks)...)
	}
	return out
}
