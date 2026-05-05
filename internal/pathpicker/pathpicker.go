// Package pathpicker resolves up to 10 000 absolute directory paths via a
// user-configured (or auto-detected) shell command, validating every line per
// the §15.3 picker output rules. The TUI's `n` flow consumes the result; this
// package itself does no rendering, so callers MUST sanitize for display via
// nameval.SanitizeForDisplay before showing any returned path on screen.
//
// See §8.15 for the surface contract and §15.3 for the per-line validation
// table.
package pathpicker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

// Sentinel errors returned by Resolve. The §8.15 contract names all three.
var (
	ErrNoPathProvider      = errors.New("pathpicker: no provider")
	ErrPathPickerTimeout   = errors.New("pathpicker: timeout")
	ErrPathPickerCmdFailed = errors.New("pathpicker: command failed")
)

// Caps from §8.15. Lines beyond maxAcceptedLines are silently dropped;
// skipped lines (failed validation) do NOT count against the cap.
const (
	maxStdoutBytes    = 1 << 20  // 1 MiB
	scannerBufferSize = 512 << 10 // 512 KiB
	maxAcceptedLines  = 10_000
)

// sensitiveEnvKeys are the §13.5.1 keys filtered out of the child process
// environment. Every other key from os.Environ() (including WEZSESH_LOG,
// WEZSESH_NO_HOOKS, WEZSESH_NERDFONT) is preserved verbatim.
var sensitiveEnvKeys = map[string]struct{}{
	"WEZSESH_HMAC_KEY":      {},
	"WEZSESH_PROTO_VERSION": {},
	"WEZSESH_CONFIG_FILE":   {},
}

// Resolve runs the configured (or auto-detected) path provider and returns
// up to 10 000 validated absolute directory paths. See package doc.
func Resolve(ctx context.Context, userCmd string) ([]string, error) {
	cmdLine := userCmd
	if cmdLine == "" {
		detected, err := autoDetect()
		if err != nil {
			return nil, err
		}
		cmdLine = detected
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}

	cmd := exec.CommandContext(ctx, shell, "-c", cmdLine)
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = filterEnv(os.Environ())

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPathPickerCmdFailed, err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPathPickerCmdFailed, err)
	}

	// Group SIGKILL on timeout (§8.15). exec.CommandContext SIGKILLs the
	// leader on ctx.Done; the deferred kill ensures any orphaned children
	// in the new process group are reaped too.
	defer func() {
		if ctx.Err() != nil && cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()

	limited := &io.LimitedReader{R: stdout, N: maxStdoutBytes}
	out := scanLines(limited)

	waitErr := cmd.Wait()

	if ctxErr := ctx.Err(); ctxErr == context.DeadlineExceeded {
		return nil, fmt.Errorf("%w: %v", ErrPathPickerTimeout, ctxErr)
	}
	if waitErr != nil {
		return nil, fmt.Errorf("%w: %v", ErrPathPickerCmdFailed, waitErr)
	}
	return out, nil
}

// autoDetect picks the first available provider per §8.15. The shell expands
// `~` for fd at exec time; we do not pre-expand on the command line.
func autoDetect() (string, error) {
	if _, err := exec.LookPath("zoxide"); err == nil {
		return "zoxide query -l", nil
	}
	if _, err := exec.LookPath("fd"); err == nil {
		return "fd -t d --max-depth 4 . ~", nil
	}
	return "", ErrNoPathProvider
}

// filterEnv returns os.Environ() with the §13.5.1 sensitive WEZSESH_ keys
// removed. Match is exact on the key name preceding `=`.
func filterEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			out = append(out, kv)
			continue
		}
		if _, drop := sensitiveEnvKeys[kv[:eq]]; drop {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// scanLines reads validated absolute directory paths from r. Lines failing
// any §15.3 rule are logged at WARN and skipped; only successfully-validated
// paths count against maxAcceptedLines.
func scanLines(r io.Reader) []string {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), scannerBufferSize)

	out := make([]string, 0, 64)
	for scanner.Scan() {
		if len(out) >= maxAcceptedLines {
			break
		}
		raw := scanner.Text()
		path, ok := validateLine(raw)
		if !ok {
			continue
		}
		out = append(out, path)
	}
	return out
}

// validateLine applies the §15.3 rules in order. The returned path is the
// tilde-expanded, absolute, directory-confirmed canonical form. ok=false
// means the line was logged and should be skipped; the caller does not
// distinguish reasons.
func validateLine(raw string) (string, bool) {
	line := strings.TrimRight(raw, "\r")
	if strings.TrimSpace(line) == "" {
		// Empty-after-trim is a benign blank line (e.g. trailing newline
		// quirks); skipping is the spec behaviour but not worth a warn.
		return "", false
	}
	if !utf8.ValidString(line) {
		slog.Warn("pathpicker: skipping line", "line", line, "reason", "invalid utf-8")
		return "", false
	}
	if strings.IndexByte(line, 0) >= 0 {
		slog.Warn("pathpicker: skipping line", "line", line, "reason", "nul byte")
		return "", false
	}

	expanded, ok := expandTilde(line)
	if !ok {
		slog.Warn("pathpicker: skipping line", "line", line, "reason", "tilde expansion")
		return "", false
	}

	if !filepath.IsAbs(expanded) {
		slog.Warn("pathpicker: skipping line", "line", line, "reason", "not absolute")
		return "", false
	}

	info, err := os.Stat(expanded)
	if err != nil {
		slog.Warn("pathpicker: skipping line", "line", line, "reason", "stat failed")
		return "", false
	}
	if !info.IsDir() {
		slog.Warn("pathpicker: skipping line", "line", line, "reason", "not a directory")
		return "", false
	}
	return expanded, true
}

// expandTilde handles the §15.3 tilde rule: `~` and `~/...` expand via
// os.UserHomeDir(); `~user/...` is explicitly unsupported and rejected.
// A line with no leading tilde is returned unchanged.
func expandTilde(line string) (string, bool) {
	if line == "" || line[0] != '~' {
		return line, true
	}
	if line == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		return home, true
	}
	if line[1] != '/' {
		// `~user/...` form is not supported per §15.3.
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, line[2:]), true
}
