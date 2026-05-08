package dirproviders

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Grady-Saccullo/wezsesh/internal/logger"
)

// commandStdoutMax bounds the bytes the command provider will read
// from a child process's stdout. Mirrors pathpicker.maxStdoutBytes —
// 1 MiB is the documented ceiling for picker output, well above any
// realistic zoxide / fd / ghq dump.
const commandStdoutMax int64 = 1 << 20

// commandScannerBufSize bounds the per-line scan buffer.
const commandScannerBufSize = 512 << 10

// runCommand spawns `$SHELL -c "<argv joined by space>"` (falling
// back to /bin/sh when SHELL is unset), parses stdout one line per
// row, validates each line via validatePath, and caps the result at
// cfg.Limit.
//
// Why $SHELL -c instead of direct exec: macOS GUI launchd hands
// wezterm.app a minimal PATH (/usr/bin:/bin:/usr/sbin:/sbin) that
// omits Nix / Homebrew / Cargo bin dirs. Going through the user's
// login shell sources /etc/zshenv, ~/.zshenv, etc., so the user's
// enriched PATH is available — same pattern pathpicker.Resolve
// uses. The argv is space-joined and shell-quoted by the caller's
// responsibility (today every wired provider passes hardcoded argv
// elements, so quoting is a non-issue; if a future config carries
// user-controlled paths this needs revisiting).
func runCommand(ctx context.Context, cfg *Config, log *logger.Logger) ([]ExternalRow, error) {
	if cfg.Type != TypeCommand {
		return nil, fmt.Errorf("dirproviders: runCommand: type %q", cfg.Type)
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmdline := strings.Join(cfg.Argv, " ")

	cctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutMs)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(cctx, shell, "-c", cmdline)
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = filterChildEnv(os.Environ())

	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 100 * time.Millisecond

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	limited := &io.LimitedReader{R: stdout, N: commandStdoutMax}
	rows := scanCommandLines(limited, cfg.Limit, log)

	waitErr := cmd.Wait()
	if waitErr != nil {
		// Non-zero exit / killed-by-pgroup: log warn but still return
		// whatever rows we got. zoxide on a fresh install often exits
		// non-zero with empty output; that's not a hard failure.
		if log != nil {
			log.Warn("dirproviders: command provider non-zero exit",
				"argv", cmdline, "err", waitErr.Error(),
				"rows", len(rows))
		}
	}

	return rows, nil
}

// scanCommandLines reads validated rows from `r` until either
// `limit` rows accumulate or the reader closes. Lines that fail
// validatePath are logged at warn and skipped; only successfully-
// validated paths count against `limit`.
func scanCommandLines(r io.Reader, limit int, log *logger.Logger) []ExternalRow {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), commandScannerBufSize)

	out := make([]ExternalRow, 0, 64)
	for scanner.Scan() {
		if limit > 0 && len(out) >= limit {
			break
		}
		raw := scanner.Text()
		path, reason, ok := validatePath(raw)
		if !ok {
			if log != nil && reason != "blank line" {
				log.Warn("dirproviders: skipping line",
					"line", raw, "reason", reason)
			}
			continue
		}
		out = append(out, ExternalRow{
			Path: path,
			Name: filepath.Base(path),
		})
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) && log != nil {
		log.Warn("dirproviders: command scan error", "err", err.Error())
	}
	return out
}

// filterChildEnv removes the WEZSESH_HMAC_KEY secret from the
// inherited env before handing it to the shelled-out command. A
// command provider running in the user's shell context has no
// business seeing the IPC HMAC key. Mirrors pathpicker.filterEnv
// (extracting the helper there is a follow-up — keeping this local
// for C5 isolation).
func filterChildEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			out = append(out, kv)
			continue
		}
		if kv[:eq] == "WEZSESH_HMAC_KEY" {
			continue
		}
		out = append(out, kv)
	}
	return out
}
