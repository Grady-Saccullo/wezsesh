package rules

import "testing"

// TestLoggerBanRule_Positive: log.Println in a restricted package fires.
func TestLoggerBanRule_Positive(t *testing.T) {
	src := []byte(`package state

import "log"

func warn() {
	log.Println("hi")
}
`)
	rule := LoggerBanRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/state/foo.go", src, fset, f)
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for log.Println, got 0")
	}
}

// TestLoggerBanRule_Positive_FprintlnStderr: fmt.Fprintln(os.Stderr,…)
// in a restricted package fires.
func TestLoggerBanRule_Positive_FprintlnStderr(t *testing.T) {
	src := []byte(`package state

import (
	"fmt"
	"os"
)

func warn() {
	fmt.Fprintln(os.Stderr, "hi")
}
`)
	rule := LoggerBanRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/state/foo.go", src, fset, f)
	if len(out) == 0 {
		t.Fatalf("want >=1 finding for fmt.Fprintln(os.Stderr,…), got 0")
	}
}

// TestLoggerBanRule_Negative_LoggerPkg: the same calls inside
// internal/logger are exempt.
func TestLoggerBanRule_Negative_LoggerPkg(t *testing.T) {
	src := []byte(`package logger

import "log"

func warn() {
	log.Println("hi")
}
`)
	rule := LoggerBanRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/logger/logger.go", src, fset, f)
	if len(out) != 0 {
		t.Errorf("want 0 findings inside logger pkg, got %d: %v", len(out), out)
	}
}

// TestLoggerBanRule_Negative_TestFile: tests are exempt.
func TestLoggerBanRule_Negative_TestFile(t *testing.T) {
	src := []byte(`package state

import (
	"fmt"
	"os"
	"testing"
)

func TestX(t *testing.T) {
	fmt.Fprintln(os.Stderr, "hi")
}
`)
	rule := LoggerBanRule()
	fset, f := mustParseGo(t, src)
	out := rule.Check("internal/state/state_test.go", src, fset, f)
	if len(out) != 0 {
		t.Errorf("want 0 findings on _test.go, got %d: %v", len(out), out)
	}
}
